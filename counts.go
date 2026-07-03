package socialkit

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// EntityCounts is the denormalized per-entity aggregate (social_entity_counts).
type EntityCounts struct {
	Likes        int `json:"likes"`
	Dislikes     int `json:"dislikes"`
	Favorites    int `json:"favorites"`
	CommentCount int `json:"comment_count"`
}

// bumpCounts upserts the per-entity rollup by the given deltas inside the
// caller's tx. Deltas may be negative (a reaction switch, an unfavorite, a
// delete); GREATEST clamps each count at 0 so it never goes negative.
func bumpCounts(ctx context.Context, tx pgx.Tx, s *store, entityType, entityID string, dLikes, dDislikes, dFav, dComments int) error {
	if dLikes == 0 && dDislikes == 0 && dFav == 0 && dComments == 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `INSERT INTO `+s.t.entityCounts+`
		(entity_type, entity_id, likes, dislikes, favorites, comment_count)
		VALUES ($1, $2, GREATEST($3,0), GREATEST($4,0), GREATEST($5,0), GREATEST($6,0))
		ON CONFLICT (entity_type, entity_id) DO UPDATE SET
			likes         = GREATEST(`+s.t.entityCounts+`.likes + $3, 0),
			dislikes      = GREATEST(`+s.t.entityCounts+`.dislikes + $4, 0),
			favorites     = GREATEST(`+s.t.entityCounts+`.favorites + $5, 0),
			comment_count = GREATEST(`+s.t.entityCounts+`.comment_count + $6, 0),
			updated_at    = now()`,
		entityType, entityID, dLikes, dDislikes, dFav, dComments)
	return err
}

// orderBy builds an ORDER BY clause for a count-sortable list from a `sort`
// query value. "likes" = most likes; "best" = Wilson lower bound (quality over
// raw volume, so 900/1000 outranks 1/1); anything else = newest. Column names
// are trusted (kit-internal), never user input.
func orderBy(sort, likes, dislikes, created string) string {
	switch sort {
	case "likes":
		return "ORDER BY " + likes + " DESC, " + created + " DESC"
	case "best":
		l, d := likes, dislikes
		n := "(" + l + "+" + d + ")"
		return "ORDER BY (CASE WHEN " + n + "=0 THEN 0::float8 ELSE " +
			"((" + l + "+1.9208)/" + n + " - 1.96*sqrt((" + l + "::float8*" + d + ")/" + n + "+0.9604)/" + n + ")/(1+3.8416/" + n + ") END) DESC, " + created + " DESC"
	default:
		return "ORDER BY " + created + " DESC"
	}
}

// Counts returns the aggregate counts for one entity (O(1) rollup read). The
// zero value is returned when the entity has no engagement yet.
func (rt *Runtime) Counts(ctx context.Context, entityType, entityID string) (EntityCounts, error) {
	var c EntityCounts
	err := rt.store.pool.QueryRow(ctx, `SELECT likes, dislikes, favorites, comment_count
		FROM `+rt.store.t.entityCounts+` WHERE entity_type = $1 AND entity_id = $2`,
		entityType, entityID).Scan(&c.Likes, &c.Dislikes, &c.Favorites, &c.CommentCount)
	if errors.Is(err, pgx.ErrNoRows) {
		return EntityCounts{}, nil
	}
	return c, err
}

// ListFavorites returns userID's bookmarks newest-first — the host-facing Go
// API a hydrated host list route reads its ids from. limit <= 0 means all.
func (rt *Runtime) ListFavorites(ctx context.Context, userID string, limit, offset int) ([]FavoriteItem, error) {
	if limit <= 0 {
		limit = 1<<31 - 1
	}
	return rt.favorites.list(ctx, userID, limit, offset)
}

// LatestComments is the host-facing feed API (see comments.latest): newest
// comments across all entities the given actor may see, with canonical entity
// keys for host-side title/cover enrichment.
func (rt *Runtime) LatestComments(ctx context.Context, actor Actor, limit, offset int) ([]FeedItem, error) {
	if limit <= 0 {
		limit = 20
	}
	return rt.comments.latest(ctx, actor, limit, offset)
}

// MyReactions batch-reads the actor's own reaction (-1/0/1) for many targets —
// the host-facing hydration read for list/detail responses (doujins-style
// `user_reaction` enrichment). Only nonzero reactions appear in the map.
func (rt *Runtime) MyReactions(ctx context.Context, actor Actor, targets []EntityKey) (map[EntityKey]int16, error) {
	out := make(map[EntityKey]int16, len(targets))
	userID, ip, ok := reactionKey(actor)
	if !ok || len(targets) == 0 {
		return out, nil
	}
	types := make([]string, len(targets))
	ids := make([]string, len(targets))
	for i, k := range targets {
		types[i], ids[i] = k.Type, k.ID
	}
	// unnest pairs type/id positionally (no cross-matching, one round-trip).
	rows, err := rt.store.pool.Query(ctx, `SELECT entity_type, entity_id, value
		FROM `+rt.store.t.reactions+`
		WHERE `+actorPred(userID, 1)+` AND value <> 0 AND (entity_type, entity_id) IN (
			SELECT * FROM unnest($2::text[], $3::text[]))`,
		actorArg(userID, ip), types, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k EntityKey
		var v int16
		if err := rows.Scan(&k.Type, &k.ID, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ActorReaction is one row of an actor's reaction history for one entity type.
type ActorReaction struct {
	EntityID  string    `json:"entity_id"`
	Value     int16     `json:"value"` // -1 or 1 (neutral rows are excluded)
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ReactionsByActor lists the actor's nonzero reactions of one entity type,
// newest-first (host-facing; e.g. a "my tag preferences" page). limit <= 0
// means all.
func (rt *Runtime) ReactionsByActor(ctx context.Context, actor Actor, entityType string, limit, offset int) ([]ActorReaction, error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return nil, nil
	}
	if limit <= 0 {
		limit = 1<<31 - 1
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT entity_id, value, created_at, updated_at
		FROM `+rt.store.t.reactions+`
		WHERE entity_type = $1 AND `+actorPred(userID, 2)+` AND value <> 0
		ORDER BY updated_at DESC LIMIT $3 OFFSET $4`,
		entityType, actorArg(userID, ip), limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ActorReaction, 0, min(limit, 128))
	for rows.Next() {
		var r ActorReaction
		if err := rows.Scan(&r.EntityID, &r.Value, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// IsFavorited batch-checks bookmarks for a user (host-facing; every requested
// key is present in the map, absent bookmarks => false).
func (rt *Runtime) IsFavorited(ctx context.Context, userID string, targets []EntityKey) (map[EntityKey]bool, error) {
	return rt.favorites.IsFavorited(ctx, userID, targets)
}

// CountsByEntity batch-reads aggregate counts for many ids of one entity type
// (missing ids are absent from the map). For host list hydration + sorting.
func (rt *Runtime) CountsByEntity(ctx context.Context, entityType string, ids []string) (map[string]EntityCounts, error) {
	out := make(map[string]EntityCounts, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := rt.store.pool.Query(ctx, `SELECT entity_id, likes, dislikes, favorites, comment_count
		FROM `+rt.store.t.entityCounts+` WHERE entity_type = $1 AND entity_id = ANY($2)`, entityType, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var c EntityCounts
		if err := rows.Scan(&id, &c.Likes, &c.Dislikes, &c.Favorites, &c.CommentCount); err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, rows.Err()
}
