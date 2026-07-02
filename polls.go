package socialkit

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// polls is the standalone site-wide poll module: admin-authored questions with
// options, anon-capable one-vote-per-(poll,user)/(poll,ip) tallying. Unlike the
// engagement modules it is NOT tied to a host entity, so it never gates on the
// EntityResolver — writes gate on the PollWrite perm; reads/votes are public.
type polls struct {
	rt *Runtime
	s  *store
}

func newPolls(rt *Runtime) *polls {
	return &polls{rt: rt, s: rt.store}
}

// pollOption is one choice with its denormalized vote_count.
type pollOption struct {
	ID        string `json:"id"`
	Label     string `json:"label"`
	ImageURL  string `json:"image_url,omitempty"`
	Position  int    `json:"position"`
	VoteCount int    `json:"vote_count"`
}

// pollView is a question plus its options and the caller's own vote (if any).
type pollView struct {
	ID       string       `json:"id"`
	Question string       `json:"question"`
	Language string       `json:"language"`
	IsActive bool         `json:"is_active"`
	Options  []pollOption `json:"options"`
	Voted    bool         `json:"voted"`
	MyOption string       `json:"my_option,omitempty"`
}

type createPollInput struct {
	Question string              `json:"question"`
	Language string              `json:"language"`
	Options  []createOptionInput `json:"options"`
}

// createOptionInput takes image_url directly (the MediaStore upload path is the
// host's job before calling — the kit stores the resolved url).
type createOptionInput struct {
	Label    string `json:"label"`
	ImageURL string `json:"image_url"`
	Position int    `json:"position"`
}

// updatePollInput uses pointers so absent fields are left untouched (COALESCE).
type updatePollInput struct {
	Question *string `json:"question"`
	IsActive *bool   `json:"is_active"`
}

// --- admin (PollWrite-gated) ---

// create inserts a question and its options atomically. Fail-closed on perm.
func (p *polls) create(ctx context.Context, actor Actor, in createPollInput) (pollView, error) {
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PollWrite); err != nil {
		return pollView{}, err
	}
	in.Question = strings.TrimSpace(in.Question)
	if in.Question == "" {
		return pollView{}, badRequest("question is required")
	}
	if len(in.Options) < 2 {
		return pollView{}, badRequest("a poll needs at least 2 options")
	}
	for i := range in.Options {
		in.Options[i].Label = strings.TrimSpace(in.Options[i].Label)
		if in.Options[i].Label == "" {
			return pollView{}, badRequest("option %d label is required", i)
		}
	}

	tx, err := p.s.pool.Begin(ctx)
	if err != nil {
		return pollView{}, err
	}
	defer tx.Rollback(ctx)

	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO `+p.s.t.pollQuestions+`
		(question, language) VALUES ($1, $2) RETURNING id::text`, in.Question, in.Language).Scan(&id); err != nil {
		return pollView{}, err
	}
	for _, o := range in.Options {
		if _, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.pollOptions+`
			(question_id, label, image_url, position) VALUES ($1, $2, $3, $4)`,
			id, o.Label, nullIf(o.ImageURL), o.Position); err != nil {
			return pollView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pollView{}, err
	}
	return p.get(ctx, actor, id)
}

// update mutates question/is_active; nil fields are left as-is via COALESCE.
func (p *polls) update(ctx context.Context, actor Actor, id string, in updatePollInput) (pollView, error) {
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PollWrite); err != nil {
		return pollView{}, err
	}
	if in.Question != nil {
		q := strings.TrimSpace(*in.Question)
		if q == "" {
			return pollView{}, badRequest("question cannot be blank")
		}
		in.Question = &q
	}
	tag, err := p.s.pool.Exec(ctx, `UPDATE `+p.s.t.pollQuestions+`
		SET question = COALESCE($2, question), is_active = COALESCE($3, is_active), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id, in.Question, in.IsActive)
	if err != nil {
		return pollView{}, err
	}
	if tag.RowsAffected() == 0 {
		return pollView{}, ErrNotFound
	}
	return p.get(ctx, actor, id)
}

// softDelete flags deleted_at; the row (and its votes) stay for history.
func (p *polls) softDelete(ctx context.Context, actor Actor, id string) error {
	if err := p.rt.requirePerm(ctx, actor, p.rt.perms.PollWrite); err != nil {
		return err
	}
	tag, err := p.s.pool.Exec(ctx, `UPDATE `+p.s.t.pollQuestions+`
		SET deleted_at = now(), updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// --- public read ---

// list returns active, non-deleted polls (optionally language-filtered) with
// options and the caller's vote. Empty language means "all languages".
func (p *polls) list(ctx context.Context, actor Actor, language string) ([]pollView, error) {
	sql := `SELECT id::text, question, language, is_active FROM ` + p.s.t.pollQuestions + `
		WHERE deleted_at IS NULL AND is_active = true`
	var args []any
	if language != "" {
		sql += ` AND language = $1`
		args = append(args, language)
	}
	sql += ` ORDER BY created_at DESC`

	rows, err := p.s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []pollView
	var ids []string
	idx := map[string]int{}
	for rows.Next() {
		v := pollView{Options: []pollOption{}}
		if err := rows.Scan(&v.ID, &v.Question, &v.Language, &v.IsActive); err != nil {
			return nil, err
		}
		idx[v.ID] = len(views)
		views = append(views, v)
		ids = append(ids, v.ID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return []pollView{}, nil
	}

	opts, err := p.optionsFor(ctx, p.s.pool, ids)
	if err != nil {
		return nil, err
	}
	votes, err := p.votesFor(ctx, p.s.pool, actor, ids)
	if err != nil {
		return nil, err
	}
	for qid, i := range idx {
		if o := opts[qid]; o != nil {
			views[i].Options = o
		}
		if oid, ok := votes[qid]; ok {
			views[i].Voted, views[i].MyOption = true, oid
		}
	}
	return views, nil
}

// get returns one non-deleted poll (active or not) with options + caller vote.
func (p *polls) get(ctx context.Context, actor Actor, id string) (pollView, error) {
	if id == "" {
		return pollView{}, ErrNotFound
	}
	v := pollView{Options: []pollOption{}}
	err := p.s.pool.QueryRow(ctx, `SELECT id::text, question, language, is_active
		FROM `+p.s.t.pollQuestions+` WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&v.ID, &v.Question, &v.Language, &v.IsActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return pollView{}, ErrNotFound
	}
	if err != nil {
		return pollView{}, err
	}
	opts, err := p.optionsFor(ctx, p.s.pool, []string{id})
	if err != nil {
		return pollView{}, err
	}
	if o := opts[id]; o != nil {
		v.Options = o
	}
	votes, err := p.votesFor(ctx, p.s.pool, actor, []string{id})
	if err != nil {
		return pollView{}, err
	}
	if oid, ok := votes[id]; ok {
		v.Voted, v.MyOption = true, oid
	}
	return v, nil
}

// optionsFor batch-loads options keyed by question id (one array-param query).
func (p *polls) optionsFor(ctx context.Context, q querier, ids []string) (map[string][]pollOption, error) {
	rows, err := q.Query(ctx, `SELECT question_id::text, id::text, label, coalesce(image_url, ''), position, vote_count
		FROM `+p.s.t.pollOptions+` WHERE question_id = ANY($1::uuid[]) ORDER BY question_id, position`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]pollOption{}
	for rows.Next() {
		var qid string
		var o pollOption
		if err := rows.Scan(&qid, &o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount); err != nil {
			return nil, err
		}
		out[qid] = append(out[qid], o)
	}
	return out, rows.Err()
}

// votesFor batch-loads the caller's chosen option per question (qid -> optionID).
func (p *polls) votesFor(ctx context.Context, q querier, actor Actor, ids []string) (map[string]string, error) {
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return map[string]string{}, nil
	}
	rows, err := q.Query(ctx, `SELECT question_id::text, option_id::text FROM `+p.s.t.pollVotes+`
		WHERE question_id = ANY($1::uuid[]) AND `+actorPred(userID, 2), ids, actorArg(userID, ip))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var qid, oid string
		if err := rows.Scan(&qid, &oid); err != nil {
			return nil, err
		}
		out[qid] = oid
	}
	return out, rows.Err()
}

// vote records one vote and bumps the option's counter — but only the winning
// insert (RowsAffected==1) counts, so concurrent duplicates never double-count.
func (p *polls) vote(ctx context.Context, actor Actor, pollID, optionID string) (pollView, error) {
	if optionID == "" {
		return pollView{}, badRequest("option_id is required")
	}
	userID, ip, ok := reactionKey(actor)
	if !ok {
		return pollView{}, badRequest("cannot identify voter (no user id or ip)")
	}

	tx, err := p.s.pool.Begin(ctx)
	if err != nil {
		return pollView{}, err
	}
	defer tx.Rollback(ctx)

	// Poll must exist, be live and not soft-deleted (hide both as 404).
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND deleted_at IS NULL AND is_active = true`, pollID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return pollView{}, ErrNotFound
	}
	if err != nil {
		return pollView{}, err
	}
	// Option must belong to this poll.
	err = tx.QueryRow(ctx, `SELECT true FROM `+p.s.t.pollOptions+`
		WHERE id = $1 AND question_id = $2`, optionID, pollID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return pollView{}, badRequest("option does not belong to poll")
	}
	if err != nil {
		return pollView{}, err
	}

	// ON CONFLICT DO NOTHING makes a duplicate voter BLOCK then no-op (0 rows) —
	// a bare INSERT would raise 23505, aborting the whole tx (25P02).
	tag, err := tx.Exec(ctx, `INSERT INTO `+p.s.t.pollVotes+`
		(question_id, option_id, user_id, ip) VALUES ($1, $2, $3, $4)`+pollVoteConflict(userID),
		pollID, optionID, nullIf(userID), nullIf(ip))
	if err != nil {
		return pollView{}, err
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `UPDATE `+p.s.t.pollOptions+`
			SET vote_count = vote_count + 1 WHERE id = $1`, optionID); err != nil {
			return pollView{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return pollView{}, err
	}
	return p.get(ctx, actor, pollID)
}

// pollVoteConflict targets the partial unique index for the voter's dedup key.
func pollVoteConflict(userID string) string {
	if userID != "" {
		return ` ON CONFLICT (question_id, user_id) WHERE user_id IS NOT NULL DO NOTHING`
	}
	return ` ON CONFLICT (question_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL DO NOTHING`
}

// --- HTTP ---

func (p *polls) mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /polls", p.handleList)
	mux.HandleFunc("GET /polls/{id}", p.handleGet)
	mux.HandleFunc("POST /polls", p.handleCreate)
	mux.HandleFunc("PATCH /polls/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /polls/{id}", p.handleDelete)
	mux.HandleFunc("POST /polls/{id}/vote", p.handleVote)
	mux.HandleFunc("POST /polls/options/{oid}/image", p.handleOptionImage)
}

// handleOptionImage uploads an option image to socialkit's media store and
// stores the resulting public URL on the option. PollWrite-gated.
func (p *polls) handleOptionImage(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	oid := req.PathValue("oid")
	if !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	data, ct, ext, err := readUpload(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	url, err := p.rt.media.Put(req.Context(), "polls/options/"+oid+"."+ext, data, ct)
	if err != nil {
		writeErr(w, err)
		return
	}
	tag, err := p.s.pool.Exec(req.Context(), `UPDATE `+p.s.t.pollOptions+` SET image_url = $2 WHERE id = $1`, oid, url)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"image_url": url})
}

func (p *polls) handleList(w http.ResponseWriter, req *http.Request) {
	views, err := p.list(req.Context(), p.rt.actor(req.Context()), req.URL.Query().Get("language"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (p *polls) handleGet(w http.ResponseWriter, req *http.Request) {
	v, err := p.get(req.Context(), p.rt.actor(req.Context()), req.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *polls) handleCreate(w http.ResponseWriter, req *http.Request) {
	var in createPollInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.create(req.Context(), p.rt.actor(req.Context()), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (p *polls) handleUpdate(w http.ResponseWriter, req *http.Request) {
	var in updatePollInput
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.update(req.Context(), p.rt.actor(req.Context()), req.PathValue("id"), in)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (p *polls) handleDelete(w http.ResponseWriter, req *http.Request) {
	if err := p.softDelete(req.Context(), p.rt.actor(req.Context()), req.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (p *polls) handleVote(w http.ResponseWriter, req *http.Request) {
	var in struct {
		OptionID string `json:"option_id"`
	}
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	v, err := p.vote(req.Context(), p.rt.actor(req.Context()), req.PathValue("id"), in.OptionID)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}
