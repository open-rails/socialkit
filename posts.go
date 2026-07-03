package socialkit

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// posts is the generic authored-content primitive (a "blog post" is just a post
// whose write-permission is held only by the root group). socialkit owns the
// store + CRUD + a simple published list/get; discovery/ranking/feeds live in a
// host discovery layer, not here.
//
// Writes are gated by the opaque host permission Perms.PostWrite (fail-closed).
// Post like/dislike reuses reactions.applyTx so the SPLIT counter logic lives in
// the reactions module once; posts only denormalizes total_likes/total_dislikes.
type posts struct {
	rt   *Runtime
	s    *store
	cols string // shared SELECT column list incl. the comment_count subquery
}

func newPosts(rt *Runtime) *posts {
	s := rt.store
	// One column list for get + list. comment_count is a correlated subquery over
	// social_comments on the opaque ("post", id) key; qualified to p.id since
	// social_comments also has an `id` column (bare `id` would bind to the wrong one).
	cols := `p.id, p.author_id, p.title, p.slug, p.body, p.excerpt, p.cover_url,
		p.language, p.is_draft, p.live_at, p.total_likes, p.total_dislikes,
		p.created_at, p.updated_at,
		(SELECT count(*) FROM ` + s.t.comments + ` c
			WHERE c.entity_type = 'post' AND c.entity_id = p.id
			AND c.deleted_at IS NULL) AS comment_count`
	return &posts{rt: rt, s: s, cols: cols}
}

// postView is the JSON shape returned by get/list/create/update.
type postView struct {
	ID            string     `json:"id"`
	AuthorID      string     `json:"author_id"`
	Title         string     `json:"title"`
	Slug          *string    `json:"slug,omitempty"`
	Body          string     `json:"body"`
	Excerpt       *string    `json:"excerpt,omitempty"`
	CoverURL      *string    `json:"cover_url,omitempty"`
	Language      string     `json:"language"`
	IsDraft       bool       `json:"is_draft"`
	LiveAt        *time.Time `json:"live_at,omitempty"`
	TotalLikes    int        `json:"total_likes"`
	TotalDislikes int        `json:"total_dislikes"`
	CommentCount  int        `json:"comment_count"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// postWriteReq is the create/update body. All-pointer so PATCH is partial (nil =
// leave unchanged); create requires title+body.
type postWriteReq struct {
	Title    *string    `json:"title"`
	Body     *string    `json:"body"`
	Excerpt  *string    `json:"excerpt"`
	Language *string    `json:"language"`
	Slug     *string    `json:"slug"`
	IsDraft  *bool      `json:"is_draft"`
	LiveAt   *time.Time `json:"live_at"`
	CoverURL *string    `json:"cover_url"`
}

// --- HTTP ---

func (p *posts) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /posts", p.handleList)
	mux.HandleFunc("GET /posts/{id}", p.handleGet)
	mux.HandleFunc("POST /posts", p.handleCreate)
	mux.HandleFunc("PATCH /posts/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /posts/{id}", p.handleDelete)
	// More specific than reactions' /{type}/{id}/like, so no ServeMux conflict.
	mux.HandleFunc("POST /posts/{id}/like", p.handleReact(1))
	mux.HandleFunc("POST /posts/{id}/dislike", p.handleReact(-1))
	mux.HandleFunc("POST /posts/{id}/neutral", p.handleReact(0))
	mux.HandleFunc("POST /posts/{id}/cover", p.handleCover)
	mux.HandleFunc("POST /posts/media", p.handleMedia)
}

// handleMedia uploads an inline post image (editor drops the returned URL into
// the body). Not post-scoped — the editor uploads while composing a draft.
// PostWrite-gated.
func (p *posts) handleMedia(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PostWrite); err != nil {
		writeErr(w, err)
		return
	}
	data, ct, ext, err := readUpload(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	url, err := p.rt.media.Put(req.Context(), "posts/media/"+uuid.NewString()+"."+ext, data, ct)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url})
}

// handleCover uploads a post cover to socialkit's media store and stores the
// resulting public URL on the post. PostWrite-gated.
func (p *posts) handleCover(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PostWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if id == "" || len(id) > 64 { // post ids are opaque text (uuid or legacy numeric)
		writeErr(w, ErrNotFound)
		return
	}
	data, ct, ext, err := readUpload(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	url, err := p.rt.media.Put(req.Context(), "posts/"+id+"/cover."+ext, data, ct)
	if err != nil {
		writeErr(w, err)
		return
	}
	tag, err := p.s.pool.Exec(req.Context(), `UPDATE `+p.s.t.posts+` SET cover_url = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, id, url)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"cover_url": url})
}

func (p *posts) handleCreate(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	actor := p.rt.actor(ctx)
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PostWrite); err != nil {
		writeErr(w, err)
		return
	}
	var in postWriteReq
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Title == nil || strings.TrimSpace(*in.Title) == "" {
		writeErr(w, badRequest("title is required"))
		return
	}
	if in.Body == nil {
		writeErr(w, badRequest("body is required"))
		return
	}
	body, err := p.rt.content.Sanitize(ctx, *in.Body)
	if err != nil {
		writeErr(w, err)
		return
	}
	excerpt, err := p.sanitizePtr(ctx, in.Excerpt)
	if err != nil {
		writeErr(w, err)
		return
	}
	var id string
	err = p.s.pool.QueryRow(ctx, `INSERT INTO `+p.s.t.posts+`
		(author_id, title, slug, body, excerpt, cover_url, language, is_draft, live_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		actor.ID, *in.Title, in.Slug, body, excerpt, in.CoverURL,
		derefStr(in.Language), derefBool(in.IsDraft), in.LiveAt).Scan(&id)
	if err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.loadByID(ctx, p.s.pool, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	p.emitPost(ctx, v, false)
	writeJSON(w, http.StatusCreated, v)
}

func (p *posts) handleUpdate(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	actor := p.rt.actor(ctx)
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PostWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	var in postWriteReq
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	// Re-sanitize provided rich text; nil stays nil so COALESCE keeps the old value.
	var body *string
	if in.Body != nil {
		b, err := p.rt.content.Sanitize(ctx, *in.Body)
		if err != nil {
			writeErr(w, err)
			return
		}
		body = &b
	}
	excerpt, err := p.sanitizePtr(ctx, in.Excerpt)
	if err != nil {
		writeErr(w, err)
		return
	}
	tag, err := p.s.pool.Exec(ctx, `UPDATE `+p.s.t.posts+` SET
		title = COALESCE($2, title), body = COALESCE($3, body),
		excerpt = COALESCE($4, excerpt), slug = COALESCE($5, slug),
		language = COALESCE($6, language), cover_url = COALESCE($7, cover_url),
		is_draft = COALESCE($8, is_draft), live_at = COALESCE($9, live_at),
		updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`,
		id, in.Title, body, excerpt, in.Slug, in.Language, in.CoverURL, in.IsDraft, in.LiveAt)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	v, err := p.loadByID(ctx, p.s.pool, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	p.emitPost(ctx, v, false)
	writeJSON(w, http.StatusOK, v)
}

func (p *posts) handleDelete(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	actor := p.rt.actor(ctx)
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PostWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	// Soft delete; RETURNING feeds the deletion signal.
	var title, body, language string
	err := p.s.pool.QueryRow(ctx, `UPDATE `+p.s.t.posts+`
		SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING title, body, language`, id).Scan(&title, &body, &language)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, ErrNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	p.rt.rec.Post(ctx, PostSignal{PostID: id, Deleted: true, Title: title, Body: body, Language: language})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "deleted": true})
}

func (p *posts) handleGet(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	actor := p.rt.actor(ctx)
	id := req.PathValue("id")
	v, err := p.loadByID(ctx, p.s.pool, id)
	if err != nil {
		writeErr(w, err)
		return
	}
	// A published post is public; a draft/unpublished one is visible only to a
	// PostWrite holder, else hidden as 404 (don't leak its existence).
	if !isPublished(v) {
		if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PostWrite); err != nil {
			writeErr(w, ErrNotFound)
			return
		}
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *posts) handleList(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	q := req.URL.Query()
	language, limit, offset := q.Get("language"), parseLimit(q.Get("limit")), parseOffset(q.Get("offset"))
	rows, err := p.s.pool.Query(ctx, `SELECT `+p.cols+` FROM `+p.s.t.posts+` p
		WHERE p.deleted_at IS NULL AND p.is_draft = false
		AND (p.live_at IS NULL OR p.live_at <= now())
		AND ($1 = '' OR p.language = $1)
		`+orderBy(q.Get("sort"), "p.total_likes", "p.total_dislikes", "COALESCE(p.live_at, p.created_at)")+`
		LIMIT $2 OFFSET $3`, language, limit, offset)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer rows.Close()
	out := []postView{}
	for rows.Next() {
		v, err := scanPost(rows)
		if err != nil {
			writeErr(w, err)
			return
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (p *posts) handleReact(value int16) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ctx := req.Context()
		actor := p.rt.actor(ctx)
		id := req.PathValue("id")
		if err := p.react(ctx, actor, id, value); err != nil {
			writeErr(w, err)
			return
		}
		v, err := p.loadByID(ctx, p.s.pool, id)
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}
}

// --- data ---

// react applies a like/dislike/neutral to a post. The ("post", id) target is
// socialkit-internal (no host gate), so it verifies the post is published inside
// the tx, then reuses reactions.applyTx and bumps the SPLIT counter by the exact
// returned deltas — atomic and concurrency-exact via applyTx's row lock.
func (p *posts) react(ctx context.Context, actor Actor, id string, value int16) error {
	tx, err := p.s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := p.requirePublished(ctx, tx, id); err != nil {
		return err
	}
	dLikes, dDislikes, err := p.rt.reactions.applyTx(ctx, tx, actor, "post", id, value)
	if err != nil {
		return err
	}
	if dLikes != 0 || dDislikes != 0 {
		if _, err := tx.Exec(ctx, `UPDATE `+p.s.t.posts+`
			SET total_likes = total_likes + $1, total_dislikes = total_dislikes + $2, updated_at = now()
			WHERE id = $3`, dLikes, dDislikes, id); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// loadByID returns a single non-deleted post (draft or published). id is matched
// as text so a non-uuid path 404s instead of erroring on the uuid cast.
func (p *posts) loadByID(ctx context.Context, q querier, id string) (postView, error) {
	row := q.QueryRow(ctx, `SELECT `+p.cols+` FROM `+p.s.t.posts+` p
		WHERE p.id = $1 AND p.deleted_at IS NULL`, id)
	v, err := scanPost(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return postView{}, ErrNotFound
	}
	return v, err
}

// requirePublished asserts the post exists and is publicly published; ErrNotFound
// otherwise (hides drafts/deleted from reactors).
func (p *posts) requirePublished(ctx context.Context, q querier, id string) error {
	var ok bool
	err := q.QueryRow(ctx, `SELECT true FROM `+p.s.t.posts+`
		WHERE id = $1 AND deleted_at IS NULL AND is_draft = false
		AND (live_at IS NULL OR live_at <= now())`, id).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (p *posts) sanitizePtr(ctx context.Context, s *string) (*string, error) {
	if s == nil {
		return nil, nil
	}
	out, err := p.rt.content.Sanitize(ctx, *s)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *posts) emitPost(ctx context.Context, v postView, deleted bool) {
	p.rt.rec.Post(ctx, PostSignal{PostID: v.ID, Deleted: deleted, Title: v.Title, Body: v.Body, Language: v.Language})
}

// scanPost scans the p.cols column order (pgx.Rows satisfies pgx.Row, so list reuses it).
func scanPost(row pgx.Row) (postView, error) {
	var v postView
	err := row.Scan(&v.ID, &v.AuthorID, &v.Title, &v.Slug, &v.Body, &v.Excerpt,
		&v.CoverURL, &v.Language, &v.IsDraft, &v.LiveAt, &v.TotalLikes,
		&v.TotalDislikes, &v.CreatedAt, &v.UpdatedAt, &v.CommentCount)
	return v, err
}

// isPublished mirrors the list predicate for a loaded row (deleted already excluded).
func isPublished(v postView) bool {
	return !v.IsDraft && (v.LiveAt == nil || !v.LiveAt.After(time.Now()))
}

const (
	defaultListLimit = 20
	maxListLimit     = 100
)

func parseLimit(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 {
		return defaultListLimit
	}
	if n > maxListLimit {
		return maxListLimit
	}
	return n
}

func parseOffset(s string) int {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n
	}
	return 0
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefBool(b *bool) bool {
	return b != nil && *b
}
