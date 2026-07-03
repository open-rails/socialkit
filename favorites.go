package socialkit

import (
	"context"
	"net/http"
	"time"
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

// FavoriteItem is one row of the caller's wishlist (newest-first on list).
type FavoriteItem struct {
	EntityType string    `json:"entity_type"`
	EntityID   string    `json:"entity_id"`
	CreatedAt  time.Time `json:"created_at"`
}

// add gates on visibility ONLY (needAccessible=false) so a premium-locked but
// visible entity can be wishlisted, then idempotently inserts and emits the
// discovery signal. Re-favoriting is a no-op success (ON CONFLICT DO NOTHING).
func (f *favorites) add(ctx context.Context, actor Actor, entityType, entityID string) error {
	ref, err := f.rt.gate(ctx, entityType, entityID, actor, false)
	if err != nil {
		return err
	}
	entityType, entityID = ref.Type, ref.ID // store under the canonical key
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
	// Canonicalize when possible; falls back to the raw key so un-wishlisting
	// content the resolver can no longer see still works.
	entityType, entityID = f.rt.canonical(ctx, entityType, entityID, actor)
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

// list returns the caller's favorites, most recent first, paginated.
func (f *favorites) list(ctx context.Context, userID string, limit, offset int) ([]FavoriteItem, error) {
	rows, err := f.s.pool.Query(ctx, `SELECT entity_type, entity_id, created_at
		FROM `+f.s.t.favorites+`
		WHERE user_id = $1
		ORDER BY created_at DESC, entity_type, entity_id
		LIMIT $2 OFFSET $3`, userID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]FavoriteItem, 0, min(limit, 128)) // limit can be MaxInt32 ("all")
	for rows.Next() {
		var it FavoriteItem
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
	entityType, entityID := f.rt.canonical(req.Context(), req.PathValue("type"), req.PathValue("id"), actor)
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
