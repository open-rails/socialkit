package socialkit

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// comments is the threaded comment module over the polymorphic (entity_type,
// entity_id) key. Threading is YouTube-style: a list returns TOP-LEVEL comments
// with a reply_count, and replies (one level deep) are fetched lazily per parent
// — no full-tree materialization. SPLIT like/dislike counters live on the row
// and are bumped via the shared reactions.applyTx primitive. Soft-delete
// tombstones a row so a thread stays navigable.
type comments struct {
	rt *Runtime
	s  *store
}

func newComments(rt *Runtime) *comments {
	return &comments{rt: rt, s: rt.store}
}

// commentTombstone is the body shown for a soft-deleted comment.
const commentTombstone = "[deleted]"

// uuidRe validates a comment/parent id before it reaches a uuid column, so a
// malformed id is a clean 404/400 instead of a Postgres cast error (500).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// createInput is the POST body for a new comment.
type createInput struct {
	Body     string `json:"body"`
	ParentID string `json:"parent_id,omitempty"`
	AnonName string `json:"anon_name,omitempty"`
}

// editInput is the PATCH body for an edit.
type editInput struct {
	Body string `json:"body"`
}

// Comment is the API view of a social_comments row.
type Comment struct {
	ID         string      `json:"id"`
	ParentID   string      `json:"parent_id,omitempty"`
	UserID     string      `json:"user_id,omitempty"`
	AnonName   string      `json:"anon_name,omitempty"`
	Body       string      `json:"body"`
	Deleted    bool        `json:"deleted"`
	Likes      int         `json:"likes"`
	Dislikes   int         `json:"dislikes"`
	Mine       int16       `json:"mine"` // caller's own reaction: -1/0/1
	ReplyCount int         `json:"reply_count"`
	Author     *PublicUser `json:"author,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	UpdatedAt  time.Time   `json:"updated_at"`
}

// create gates on accessibility, moderates + sanitizes the body, and inserts.
// A reply (parent_id set) must target a TOP-LEVEL comment on the SAME entity;
// replies are one level deep (a reply-to-a-reply re-parents to the top-level on
// the client, e.g. via an @mention). The parent's reply_count is bumped in tx.
func (c *comments) create(ctx context.Context, actor Actor, entityType, entityID string, in createInput) (Comment, error) {
	if _, err := c.rt.gate(ctx, entityType, entityID, actor, true); err != nil {
		return Comment{}, err
	}

	// Author identity: a logged-in actor sets user_id; an anon MUST name itself.
	loggedIn := actor.ID != "" && !actor.Anonymous
	var userID, anonName any
	if loggedIn {
		userID = actor.ID
	} else {
		name := strings.TrimSpace(in.AnonName)
		if name == "" {
			return Comment{}, badRequest("anon_name is required for an anonymous comment")
		}
		anonName = name
	}

	body := strings.TrimSpace(in.Body)
	if body == "" {
		return Comment{}, badRequest("body is required")
	}
	if err := c.rt.mod.Check(ctx, ModerationInput{Actor: actor, EntityType: entityType, EntityID: entityID, Text: body}); err != nil {
		return Comment{}, err
	}
	clean, err := c.rt.content.Sanitize(ctx, body)
	if err != nil {
		return Comment{}, err
	}
	if clean = strings.TrimSpace(clean); clean == "" {
		return Comment{}, badRequest("body is required")
	}

	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return Comment{}, err
	}
	defer tx.Rollback(ctx)

	var parentArg any
	if in.ParentID != "" {
		if !uuidRe.MatchString(in.ParentID) {
			return Comment{}, badRequest("invalid parent_id")
		}
		var pType, pID string
		var pParent *string
		var pDeleted *time.Time
		row := tx.QueryRow(ctx, `SELECT entity_type, entity_id, parent_id::text, deleted_at FROM `+c.s.t.comments+` WHERE id = $1`, in.ParentID)
		if err := row.Scan(&pType, &pID, &pParent, &pDeleted); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return Comment{}, badRequest("parent comment not found")
			}
			return Comment{}, err
		}
		if pType != entityType || pID != entityID { // exactly-one-target
			return Comment{}, badRequest("parent belongs to a different entity")
		}
		if pDeleted != nil {
			return Comment{}, badRequest("cannot reply to a deleted comment")
		}
		if pParent != nil { // single-level: replies attach to a top-level comment
			return Comment{}, badRequest("cannot reply to a reply")
		}
		parentArg = in.ParentID
	}

	out := Comment{ParentID: in.ParentID, Body: clean}
	row := tx.QueryRow(ctx, `INSERT INTO `+c.s.t.comments+`
		(entity_type, entity_id, parent_id, user_id, anon_name, body)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text, created_at, updated_at`,
		entityType, entityID, parentArg, userID, anonName, clean)
	if err := row.Scan(&out.ID, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return Comment{}, err
	}
	if parentArg != nil {
		if _, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+` SET reply_count = reply_count + 1, updated_at = now() WHERE id = $1`, parentArg); err != nil {
			return Comment{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Comment{}, err
	}

	if loggedIn {
		out.UserID = actor.ID
	} else {
		out.AnonName, _ = anonName.(string)
	}
	return out, nil
}

// list returns the entity's TOP-LEVEL comments, newest-first and paginated, each
// with reply_count so the client can lazily fetch replies. Requires the entity
// be visible (not accessible — reading is allowed on premium-locked targets).
func (c *comments) list(ctx context.Context, actor Actor, entityType, entityID string, limit, offset int) ([]Comment, error) {
	if _, err := c.rt.gate(ctx, entityType, entityID, actor, false); err != nil {
		return nil, err
	}
	rows, err := c.s.pool.Query(ctx, `SELECT id::text, parent_id::text, user_id, anon_name, body, likes, dislikes, reply_count, deleted_at, created_at, updated_at
		FROM `+c.s.t.comments+`
		WHERE entity_type = $1 AND entity_id = $2 AND parent_id IS NULL
		ORDER BY created_at DESC
		LIMIT $3 OFFSET $4`, entityType, entityID, limit, offset)
	if err != nil {
		return nil, err
	}
	return c.hydrate(ctx, actor, rows)
}

// replies returns a parent comment's direct replies, oldest-first + paginated.
// Gated on the parent's entity visibility.
func (c *comments) replies(ctx context.Context, actor Actor, parentID string, limit, offset int) ([]Comment, error) {
	if !uuidRe.MatchString(parentID) {
		return nil, ErrNotFound
	}
	var entityType, entityID string
	if err := c.s.pool.QueryRow(ctx, `SELECT entity_type, entity_id FROM `+c.s.t.comments+` WHERE id = $1`, parentID).Scan(&entityType, &entityID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if _, err := c.rt.gate(ctx, entityType, entityID, actor, false); err != nil {
		return nil, err
	}
	rows, err := c.s.pool.Query(ctx, `SELECT id::text, parent_id::text, user_id, anon_name, body, likes, dislikes, reply_count, deleted_at, created_at, updated_at
		FROM `+c.s.t.comments+`
		WHERE parent_id = $1
		ORDER BY created_at ASC
		LIMIT $2 OFFSET $3`, parentID, limit, offset)
	if err != nil {
		return nil, err
	}
	return c.hydrate(ctx, actor, rows)
}

// hydrate scans comment rows (tombstoning soft-deleted ones), then batch-attaches
// authors + the caller's own reaction.
func (c *comments) hydrate(ctx context.Context, actor Actor, rows pgx.Rows) ([]Comment, error) {
	defer rows.Close()
	var out []Comment
	var authorIDs []string
	for rows.Next() {
		var cm Comment
		var parentID, userID, anonName *string
		var deletedAt *time.Time
		if err := rows.Scan(&cm.ID, &parentID, &userID, &anonName, &cm.Body, &cm.Likes, &cm.Dislikes, &cm.ReplyCount, &deletedAt, &cm.CreatedAt, &cm.UpdatedAt); err != nil {
			return nil, err
		}
		if parentID != nil {
			cm.ParentID = *parentID
		}
		if deletedAt != nil { // tombstone: keep the row, hide content + author
			cm.Deleted = true
			cm.Body = commentTombstone
		} else {
			if userID != nil {
				cm.UserID = *userID
				authorIDs = append(authorIDs, *userID)
			}
			if anonName != nil {
				cm.AnonName = *anonName
			}
		}
		out = append(out, cm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := c.enrichAuthors(ctx, out, authorIDs); err != nil {
		return nil, err
	}
	if err := c.attachMine(ctx, actor, out); err != nil {
		return nil, err
	}
	return out, nil
}

// enrichAuthors batch-loads display data for the non-tombstoned authors.
func (c *comments) enrichAuthors(ctx context.Context, list []Comment, authorIDs []string) error {
	if len(authorIDs) == 0 {
		return nil
	}
	users, err := c.rt.users.UsersByIDs(ctx, dedup(authorIDs))
	if err != nil {
		return err
	}
	for i := range list {
		if list[i].Deleted || list[i].UserID == "" {
			continue
		}
		if u, ok := users[list[i].UserID]; ok {
			pu := u
			list[i].Author = &pu
		}
	}
	return nil
}

// attachMine sets each comment's Mine via one query over social_reactions for
// the "comment" entity type and the listed ids.
func (c *comments) attachMine(ctx context.Context, actor Actor, list []Comment) error {
	if len(list) == 0 {
		return nil
	}
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return nil
	}
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	rows, err := c.s.pool.Query(ctx, `SELECT entity_id, value FROM `+c.s.t.reactions+`
		WHERE entity_type = 'comment' AND entity_id = ANY($1) AND `+actorPred(userID, 2),
		ids, actorArg(userID, ip))
	if err != nil {
		return err
	}
	defer rows.Close()
	mine := make(map[string]int16, len(list))
	for rows.Next() {
		var id string
		var v int16
		if err := rows.Scan(&id, &v); err != nil {
			return err
		}
		mine[id] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range list {
		list[i].Mine = mine[list[i].ID]
	}
	return nil
}

// edit re-sanitizes and updates a comment's body. Allowed for the owner or a
// moderator (CommentModerate). 404 if missing or soft-deleted.
func (c *comments) edit(ctx context.Context, actor Actor, cid, rawBody string) (Comment, error) {
	if _, err := c.loadForWrite(ctx, actor, cid); err != nil {
		return Comment{}, err
	}

	body := strings.TrimSpace(rawBody)
	if body == "" {
		return Comment{}, badRequest("body is required")
	}
	clean, err := c.rt.content.Sanitize(ctx, body)
	if err != nil {
		return Comment{}, err
	}
	if clean = strings.TrimSpace(clean); clean == "" {
		return Comment{}, badRequest("body is required")
	}

	var cm Comment
	var parentID, userID, anonName *string
	row := c.s.pool.QueryRow(ctx, `UPDATE `+c.s.t.comments+`
		SET body = $2, updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING id::text, parent_id::text, user_id, anon_name, body, likes, dislikes, reply_count, created_at, updated_at`, cid, clean)
	if err := row.Scan(&cm.ID, &parentID, &userID, &anonName, &cm.Body, &cm.Likes, &cm.Dislikes, &cm.ReplyCount, &cm.CreatedAt, &cm.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Comment{}, ErrNotFound
		}
		return Comment{}, err
	}
	if parentID != nil {
		cm.ParentID = *parentID
	}
	if userID != nil {
		cm.UserID = *userID
	}
	if anonName != nil {
		cm.AnonName = *anonName
	}
	return cm, nil
}

// softDelete tombstones a comment (keeps the row for thread integrity). Allowed
// for the owner or a moderator (CommentModerate). Decrements the parent's
// reply_count when a reply is deleted so the count never drifts high.
func (c *comments) softDelete(ctx context.Context, actor Actor, cid string) error {
	if _, err := c.loadForWrite(ctx, actor, cid); err != nil {
		return err
	}
	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var parentID *string
	err = tx.QueryRow(ctx, `UPDATE `+c.s.t.comments+`
		SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING parent_id::text`, cid).Scan(&parentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // already deleted; nothing to do
	}
	if err != nil {
		return err
	}
	if parentID != nil {
		if _, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+`
			SET reply_count = GREATEST(reply_count - 1, 0), updated_at = now() WHERE id = $1`, *parentID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// loadForWrite resolves a live comment and authorizes actor as owner-or-moderator.
func (c *comments) loadForWrite(ctx context.Context, actor Actor, cid string) (ownerID *string, err error) {
	if !uuidRe.MatchString(cid) {
		return nil, ErrNotFound
	}
	var deletedAt *time.Time
	row := c.s.pool.QueryRow(ctx, `SELECT user_id, deleted_at FROM `+c.s.t.comments+` WHERE id = $1`, cid)
	if err := row.Scan(&ownerID, &deletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if deletedAt != nil {
		return nil, ErrNotFound
	}
	// Owner may write their own; otherwise require the moderator permission.
	if ownerID != nil && actor.ID != "" && !actor.Anonymous && actor.ID == *ownerID {
		return ownerID, nil
	}
	if err := c.rt.requirePerm(ctx, actor, c.rt.perms.CommentModerate); err != nil {
		return nil, err
	}
	return ownerID, nil
}

// reactTx writes the caller's reaction to a comment and denormalizes the split
// counter on the comment row in the same tx. The ("comment", cid) target is
// socialkit-internal, so no rt.gate — just a liveness check.
func (c *comments) reactTx(ctx context.Context, actor Actor, cid string, value int16) (reactionCounts, error) {
	if !uuidRe.MatchString(cid) {
		return reactionCounts{}, ErrNotFound
	}
	tx, err := c.s.pool.Begin(ctx)
	if err != nil {
		return reactionCounts{}, err
	}
	defer tx.Rollback(ctx)

	var deletedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT deleted_at FROM `+c.s.t.comments+` WHERE id = $1`, cid).Scan(&deletedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return reactionCounts{}, ErrNotFound
		}
		return reactionCounts{}, err
	}
	if deletedAt != nil {
		return reactionCounts{}, ErrNotFound
	}

	dLikes, dDislikes, err := c.rt.reactions.applyTx(ctx, tx, actor, "comment", cid, value)
	if err != nil {
		return reactionCounts{}, err
	}
	if dLikes != 0 || dDislikes != 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+c.s.t.comments+`
			SET likes = likes + $2, dislikes = dislikes + $3, updated_at = now()
			WHERE id = $1`, cid, dLikes, dDislikes); err != nil {
			return reactionCounts{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return reactionCounts{}, err
	}
	return c.rt.reactions.counts(ctx, c.s.pool, actor, "comment", cid)
}

// --- HTTP ---

func (c *comments) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /{type}/{id}/comments", c.handleList)
	mux.HandleFunc("POST /{type}/{id}/comments", c.handleCreate)
	mux.HandleFunc("GET /comments/{cid}/replies", c.handleReplies)
	mux.HandleFunc("PATCH /comments/{cid}", c.handleEdit)
	mux.HandleFunc("DELETE /comments/{cid}", c.handleDelete)
	mux.HandleFunc("POST /comments/{cid}/like", c.handleReact(1))
	mux.HandleFunc("POST /comments/{cid}/dislike", c.handleReact(-1))
	mux.HandleFunc("POST /comments/{cid}/neutral", c.handleReact(0))
}

func (c *comments) handleList(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	list, err := c.list(req.Context(), actor, req.PathValue("type"), req.PathValue("id"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (c *comments) handleReplies(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	limit, offset := parsePage(req)
	list, err := c.replies(req.Context(), actor, req.PathValue("cid"), limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, list)
}

func (c *comments) handleCreate(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	var in createInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	cm, err := c.create(req.Context(), actor, req.PathValue("type"), req.PathValue("id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, cm)
}

func (c *comments) handleEdit(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	var in editInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	cm, err := c.edit(req.Context(), actor, req.PathValue("cid"), in.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, cm)
}

func (c *comments) handleDelete(w http.ResponseWriter, req *http.Request) {
	actor := c.rt.actor(req.Context())
	if err := c.softDelete(req.Context(), actor, req.PathValue("cid")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (c *comments) handleReact(value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		actor := c.rt.actor(req.Context())
		cnt, err := c.reactTx(req.Context(), actor, req.PathValue("cid"), value)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, cnt)
	}
}

// dedup returns the input with duplicate ids removed, preserving order.
func dedup(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}
