package socialkit

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// favorites is the user-only bookmark (wishlist): an unsigned presence over the
// polymorphic key. KEY DISTINCTION vs reactions: a favorite requires the target
// be VISIBLE only, NOT accessible — you can wishlist premium content you don't
// own yet. No anonymous favorites (the row is keyed on user_id alone).
type favorites struct {
	rt *Runtime
	s  *store
}

func newFavorites(rt *Runtime) *favorites {
	return &favorites{rt: rt, s: rt.store}
}

// EntityKey is a polymorphic target for the batch favorite lookup.
type EntityKey struct {
	Type string `json:"entity_type"`
	ID   string `json:"entity_id"`
}

// favoriteItem is one row of the caller's wishlist (newest-first on list).
type favoriteItem struct {
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// add gates on visibility ONLY (needAccessible=false) so a premium-locked but
// visible entity can be wishlisted, then idempotently inserts and emits the
// discovery signal. Re-favoriting is a no-op success (ON CONFLICT DO NOTHING).
func (f *favorites) add(ctx context.Context, actor Actor, entityType, entityID string) error {
	if _, err := f.rt.gate(ctx, entityType, entityID, actor, false); err != nil {
		return err
	}
	tx, err := f.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO `+f.s.t.favorites+`
		(user_id, entity_type, entity_id) VALUES ($1, $2, $3)
		ON CONFLICT (user_id, entity_type, entity_id) DO NOTHING`,
		actor.ID, entityType, entityID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 { // only a real new favorite bumps the rollup
		if err := bumpCounts(ctx, tx, f.s, entityType, entityID, 0, 0, 1, 0); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	f.rt.rec.Reaction(ctx, ReactionSignal{
		EntityType: entityType, EntityID: entityID, ActorID: actor.ID, Kind: "favorite",
	})
	return nil
}

// remove deletes the caller's bookmark (idempotent — no row is a no-op success)
// and emits the unfavorite signal. No visibility gate: un-wishlisting content
// that later became hidden must still work.
func (f *favorites) remove(ctx context.Context, actor Actor, entityType, entityID string) error {
	tx, err := f.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM `+f.s.t.favorites+`
		WHERE user_id = $1 AND entity_type = $2 AND entity_id = $3`,
		actor.ID, entityType, entityID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 { // only a real removal decrements the rollup
		if err := bumpCounts(ctx, tx, f.s, entityType, entityID, 0, 0, -1, 0); err != nil {
			return err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	f.rt.rec.Reaction(ctx, ReactionSignal{
		EntityType: entityType, EntityID: entityID, ActorID: actor.ID, Kind: "unfavorite",
	})
	return nil
}

// IsFavorited batch-reports which of targets the user has bookmarked. Every
// requested key is present in the map (absent bookmarks => false). Batch-shaped:
// the status endpoint is just a slice of one.
func (f *favorites) IsFavorited(ctx context.Context, userID string, targets []EntityKey) (map[EntityKey]bool, error) {
	out := make(map[EntityKey]bool, len(targets))
	for _, k := range targets {
		out[k] = false
	}
	if userID == "" || len(targets) == 0 {
		return out, nil
	}
	types := make([]string, len(targets))
	ids := make([]string, len(targets))
	for i, k := range targets {
		types[i], ids[i] = k.Type, k.ID
	}
	// unnest pairs the type/id arrays positionally so one round-trip checks all
	// (avoids cross-matching type_a with id_b that a plain IN would allow).
	rows, err := f.s.pool.Query(ctx, `SELECT entity_type, entity_id FROM `+f.s.t.favorites+`
		WHERE user_id = $1 AND (entity_type, entity_id) IN (
			SELECT * FROM unnest($2::text[], $3::text[]))`,
		userID, types, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k EntityKey
		if err := rows.Scan(&k.Type, &k.ID); err != nil {
			return nil, err
		}
		out[k] = true
	}
	return out, rows.Err()
}

// Count returns how many users have favorited one entity — an O(1) read of the
// per-entity rollup (maintained in-tx on add/remove).
func (f *favorites) Count(ctx context.Context, entityType, entityID string) (int, error) {
	var n int
	err := f.s.pool.QueryRow(ctx, `SELECT favorites FROM `+f.s.t.entityCounts+`
		WHERE entity_type = $1 AND entity_id = $2`, entityType, entityID).Scan(&n)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// CountsByEntity batch-tallies favorites for several ids of one type. Ids with
// zero favorites are omitted (host reads missing as 0).
func (f *favorites) CountsByEntity(ctx context.Context, entityType string, ids []string) (map[string]int, error) {
	out := make(map[string]int, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := f.s.pool.Query(ctx, `SELECT entity_id, favorites FROM `+f.s.t.entityCounts+`
		WHERE entity_type = $1 AND entity_id = ANY($2) AND favorites > 0`,
		entityType, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

// list returns the caller's favorites, most recent first, paginated.
func (f *favorites) list(ctx context.Context, userID string, limit, offset int) ([]favoriteItem, error) {
	rows, err := f.s.pool.Query(ctx, `SELECT entity_type, entity_id, created_at
		FROM `+f.s.t.favorites+`
		WHERE user_id = $1
		ORDER BY created_at DESC, entity_type, entity_id
		LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]favoriteItem, 0, limit)
	for rows.Next() {
		var it favoriteItem
		if err := rows.Scan(&it.EntityType, &it.EntityID, &it.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, rows.Err()
}

// --- HTTP ---

func (f *favorites) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /favorites", f.handleList)
	mux.HandleFunc("POST /{type}/{id}/favorite", f.handleAdd)
	mux.HandleFunc("DELETE /{type}/{id}/favorite", f.handleRemove)
	mux.HandleFunc("GET /{type}/{id}/favorite", f.handleStatus)
}

func (f *favorites) handleAdd(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	entityType, entityID := req.PathValue("type"), req.PathValue("id")
	if err := f.add(req.Context(), actor, entityType, entityID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorited": true})
}

func (f *favorites) handleRemove(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	entityType, entityID := req.PathValue("type"), req.PathValue("id")
	if err := f.remove(req.Context(), actor, entityType, entityID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorited": false})
}

func (f *favorites) handleStatus(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	entityType, entityID := req.PathValue("type"), req.PathValue("id")
	key := EntityKey{Type: entityType, ID: entityID}
	m, err := f.IsFavorited(req.Context(), actor.ID, []EntityKey{key})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"favorited": m[key]})
}

func (f *favorites) handleList(w http.ResponseWriter, req *http.Request) {
	actor, err := f.rt.requireActor(req.Context())
	if err != nil {
		writeErr(w, err)
		return
	}
	limit, offset := parsePage(req)
	items, err := f.list(req.Context(), actor.ID, limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, items)
}

// parsePage reads limit/offset with a default limit of 20 and a hard cap of 100.
func parsePage(req *http.Request) (limit, offset int) {
	limit, offset = 20, 0
	if v, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 100 {
		limit = 100
	}
	if v, err := strconv.Atoi(req.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}
