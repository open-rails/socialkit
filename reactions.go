package socialkit

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"
)

// reactions is the 3-state (like/dislike/neutral) reaction system over the
// polymorphic key — the reference module every other engagement type mirrors.
//
// Its applyTx is the shared concurrency-safe upsert primitive: the comments and
// posts modules reuse it inside their own transactions to write a reaction AND
// bump their own SPLIT counter atomically, so the counter logic lives here once.
type reactions struct {
	rt *Runtime
	s  *store
}

func newReactions(rt *Runtime) *reactions {
	return &reactions{rt: rt, s: rt.store}
}

// reactionCounts is the SPLIT tally for an entity plus the caller's own value.
type reactionCounts struct {
	Likes    int   `json:"likes"`
	Dislikes int   `json:"dislikes"`
	Mine     int16 `json:"mine"` // -1, 0, or 1; 0 also means "no reaction"
}

// applyTx performs the 3-state upsert for (entityType, entityID, actor) inside
// tx and returns the count deltas (dLikes/dDislikes, each -1/0/+1) so the caller
// can denormalize a SPLIT counter in the same transaction. Neutral (0) is a
// stored state, not a delete, so a recommender "mute" signal survives.
//
// Concurrency: SELECT ... FOR UPDATE locks the existing row; a lost insert race
// (23505) re-selects and updates. Exact under concurrent double-like and
// like<->dislike switches.
func (r *reactions) applyTx(ctx context.Context, tx pgx.Tx, actor Actor, entityType, entityID string, value int16) (dLikes, dDislikes int, err error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return 0, 0, badRequest("cannot identify reactor (no user id or ip)")
	}

	prev, found, err := r.lockExisting(ctx, tx, userID, ip, entityType, entityID)
	if err != nil {
		return 0, 0, err
	}
	if found {
		if prev == value {
			return 0, 0, nil
		}
		if _, err = tx.Exec(ctx, `UPDATE `+r.s.t.reactions+`
			SET value = $1, updated_at = now()
			WHERE entity_type = $2 AND entity_id = $3 AND `+actorPred(userID, 4), value, entityType, entityID, actorArg(userID, ip)); err != nil {
			return 0, 0, err
		}
		dLikes, dDislikes = delta(prev, value)
		return r.bumpAndReturn(ctx, tx, entityType, entityID, dLikes, dDislikes)
	}

	// No existing row: insert, tolerating a concurrent insert. ON CONFLICT DO
	// NOTHING makes the losing racer BLOCK on the other tx then no-op (0 rows) —
	// a bare INSERT would raise 23505, which aborts the whole tx (25P02) and
	// poisons the re-select. RowsAffected distinguishes the two outcomes.
	tag, err := tx.Exec(ctx, `INSERT INTO `+r.s.t.reactions+`
		(entity_type, entity_id, user_id, ip, value) VALUES ($1, $2, $3, $4, $5)`+onConflict(userID),
		entityType, entityID, nullIf(userID), nullIf(ip), value)
	if err != nil {
		return 0, 0, err
	}
	if tag.RowsAffected() == 1 {
		dLikes, dDislikes = delta(0, value)
		return r.bumpAndReturn(ctx, tx, entityType, entityID, dLikes, dDislikes)
	}
	// Lost the insert race: the row now exists (committed) — lock + update it.
	prev, found, err = r.lockExisting(ctx, tx, userID, ip, entityType, entityID)
	if err != nil || !found {
		return 0, 0, err
	}
	if prev == value {
		return 0, 0, nil
	}
	if _, err = tx.Exec(ctx, `UPDATE `+r.s.t.reactions+`
		SET value = $1, updated_at = now()
		WHERE entity_type = $2 AND entity_id = $3 AND `+actorPred(userID, 4), value, entityType, entityID, actorArg(userID, ip)); err != nil {
		return 0, 0, err
	}
	dLikes, dDislikes = delta(prev, value)
	return r.bumpAndReturn(ctx, tx, entityType, entityID, dLikes, dDislikes)
}

// bumpAndReturn denormalizes the reaction delta into the per-entity rollup
// (same tx) and returns the deltas for callers that also keep their own counter.
func (r *reactions) bumpAndReturn(ctx context.Context, tx pgx.Tx, entityType, entityID string, dLikes, dDislikes int) (int, int, error) {
	if err := bumpCounts(ctx, tx, r.s, entityType, entityID, dLikes, dDislikes, 0, 0); err != nil {
		return 0, 0, err
	}
	return dLikes, dDislikes, nil
}

// lockExisting selects+locks the caller's current reaction row, if any.
func (r *reactions) lockExisting(ctx context.Context, tx pgx.Tx, userID, ip, entityType, entityID string) (value int16, found bool, err error) {
	row := tx.QueryRow(ctx, `SELECT value FROM `+r.s.t.reactions+`
		WHERE entity_type = $1 AND entity_id = $2 AND `+actorPred(userID, 3)+` FOR UPDATE`,
		entityType, entityID, actorArg(userID, ip))
	err = row.Scan(&value)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return value, true, nil
}

// react is the generic entry point for host-registered entity types: it gates
// on accessibility (no reacting on deleted/unpublished/premium-locked targets),
// applies the reaction, and emits the discovery signal.
func (r *reactions) react(ctx context.Context, actor Actor, entityType, entityID string, value int16) error {
	if _, err := r.rt.gate(ctx, entityType, entityID, actor, true); err != nil {
		return err
	}
	tx, err := r.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, _, err := r.applyTx(ctx, tx, actor, entityType, entityID, value); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	r.rt.rec.Reaction(ctx, ReactionSignal{
		EntityType: entityType, EntityID: entityID, ActorID: actor.ID, Kind: reactionKind(value),
	})
	return nil
}

// counts returns the SPLIT tally plus the caller's own reaction, on a querier
// (pool or tx). The tally is an O(1) read of the per-entity rollup, which
// applyTx maintains in-tx.
func (r *reactions) counts(ctx context.Context, q querier, actor Actor, entityType, entityID string) (reactionCounts, error) {
	var out reactionCounts
	if err := q.QueryRow(ctx, `SELECT likes, dislikes FROM `+r.s.t.entityCounts+`
		WHERE entity_type = $1 AND entity_id = $2`, entityType, entityID).Scan(&out.Likes, &out.Dislikes); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	if userID, ip, ok := reactionKey(actor); ok {
		row := q.QueryRow(ctx, `SELECT value FROM `+r.s.t.reactions+`
			WHERE entity_type = $1 AND entity_id = $2 AND `+actorPred(userID, 3),
			entityType, entityID, actorArg(userID, ip))
		var mine int16
		if err := row.Scan(&mine); err == nil {
			out.Mine = mine
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return out, err
		}
	}
	return out, nil
}

// --- HTTP ---

func (r *reactions) mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /{type}/{id}/like", r.handleSet(1))
	mux.HandleFunc("POST /{type}/{id}/dislike", r.handleSet(-1))
	mux.HandleFunc("POST /{type}/{id}/neutral", r.handleSet(0))
	mux.HandleFunc("DELETE /{type}/{id}/reaction", r.handleSet(0))
	mux.HandleFunc("GET /{type}/{id}/reaction", r.handleGet)
}

func (r *reactions) handleSet(value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := r.rt.actor(req.Context())
		entityType, entityID := req.PathValue("type"), req.PathValue("id")
		if err := r.react(req.Context(), actor, entityType, entityID, value); err != nil {
			writeErr(w, err)
			return
		}
		cnt, err := r.counts(req.Context(), r.s.pool, actor, entityType, entityID)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cnt)
	}
}

func (r *reactions) handleGet(w http.ResponseWriter, req *http.Request) {
	actor := r.rt.actor(req.Context())
	entityType, entityID := req.PathValue("type"), req.PathValue("id")
	if _, err := r.rt.gate(req.Context(), entityType, entityID, actor, false); err != nil {
		writeErr(w, err)
		return
	}
	cnt, err := r.counts(req.Context(), r.s.pool, actor, entityType, entityID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cnt)
}

// --- reaction key + delta helpers (shared by comments/posts via applyTx) ---

// reactionKey returns the dedup identity: a user id when present, else the IP.
// ok=false when the actor is fully unidentifiable (no id, no ip).
func reactionKey(a Actor) (userID, ip string, ok bool) {
	if a.ID != "" && !a.Anonymous {
		return a.ID, a.IP, true
	}
	if a.IP != "" {
		return "", a.IP, true
	}
	return "", "", false
}

// actorPred/actorArg build the WHERE clause + bound arg selecting the caller's
// row: by user_id when identified, else by (user_id IS NULL AND ip = ...). n is
// the 1-based placeholder index for the actor argument in the surrounding query.
func actorPred(userID string, n int) string {
	if userID != "" {
		return "user_id = $" + strconv.Itoa(n)
	}
	return "user_id IS NULL AND ip = $" + strconv.Itoa(n)
}

func actorArg(userID, ip string) string {
	if userID != "" {
		return userID
	}
	return ip
}

// onConflict targets the partial unique index for the actor's dedup key so a
// concurrent insert no-ops instead of aborting the transaction.
func onConflict(userID string) string {
	if userID != "" {
		return ` ON CONFLICT (entity_type, entity_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`
	}
	return ` ON CONFLICT (entity_type, entity_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL DO NOTHING`
}

func reactionKind(value int16) string {
	switch value {
	case 1:
		return "like"
	case -1:
		return "dislike"
	default:
		return "neutral"
	}
}

func delta(prev, next int16) (dLikes, dDislikes int) {
	return b2i(next == 1) - b2i(prev == 1), b2i(next == -1) - b2i(prev == -1)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIf(s string) any {
	if s == "" {
		return nil
	}
	return s
}
