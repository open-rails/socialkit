package socialkit

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	ID         string       `json:"id"`
	Question   string       `json:"question"`
	Language   string       `json:"language"`
	IsActive   bool         `json:"is_active"`
	ImageURL   string       `json:"image_url,omitempty"`
	LiveAt     time.Time    `json:"live_at"`
	TotalVotes int          `json:"total_votes"`
	Options    []pollOption `json:"options"`
	Voted      bool         `json:"voted"`
	MyOption   string       `json:"my_option,omitempty"`
}

type createPollInput struct {
	Question string              `json:"question"`
	Language string              `json:"language"`
	ImageURL string              `json:"image_url,omitempty"`
	LiveAt   *time.Time          `json:"live_at,omitempty"` // nil = live now
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
	Question *string    `json:"question"`
	IsActive *bool      `json:"is_active"`
	LiveAt   *time.Time `json:"live_at"`
	ImageURL *string    `json:"image_url"`
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
		(question, language, image_url, live_at) VALUES ($1, $2, $3, COALESCE($4, now()))
		RETURNING id::text`, in.Question, in.Language, nullIf(in.ImageURL), in.LiveAt).Scan(&id); err != nil {
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
		SET question = COALESCE($2, question), is_active = COALESCE($3, is_active),
		    live_at = COALESCE($4, live_at), image_url = COALESCE($5, image_url), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id, in.Question, in.IsActive, in.LiveAt, in.ImageURL)
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

// listFilter narrows list: language, a month ("2006-01") or day ("2006-01-02")
// live_at window, and admin (include future/inactive polls; PollWrite-gated by
// the caller).
type listFilter struct {
	language, month, date string
	admin                 bool
	limit, offset         int
}

// list returns non-deleted polls newest-live-first with options + caller vote.
// Public mode never leaks future polls (live_at <= now). The default view (no
// month/date window) serves only the active poll(s); browsing a specific
// month/date archive returns all live polls in that window regardless of
// is_active, so historical polls stay readable once a newer poll becomes active.
// Voting remains gated on is_active elsewhere (see vote()).
func (p *polls) list(ctx context.Context, actor Actor, f listFilter) ([]pollView, error) {
	from, to, hasWindow := parseWindow(f.month, f.date)
	sql := `SELECT id::text, question, language, is_active, coalesce(image_url,''), live_at
		FROM ` + p.s.t.pollQuestions + ` WHERE deleted_at IS NULL`
	if !f.admin {
		sql += ` AND live_at <= now()`
		// Only the default (windowless) view is restricted to the active poll;
		// an explicit month/date archive shows all live polls in that window.
		if !hasWindow {
			sql += ` AND is_active = true`
		}
	}
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if f.language != "" {
		sql += ` AND language = ` + arg(f.language)
	}
	if hasWindow {
		sql += ` AND live_at >= ` + arg(from) + ` AND live_at < ` + arg(to)
	}
	sql += ` ORDER BY live_at DESC LIMIT ` + arg(f.limit) + ` OFFSET ` + arg(f.offset)

	rows, err := p.s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []pollView
	for rows.Next() {
		v := pollView{Options: []pollOption{}}
		if err := rows.Scan(&v.ID, &v.Question, &v.Language, &v.IsActive, &v.ImageURL, &v.LiveAt); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := p.attach(ctx, actor, views); err != nil {
		return nil, err
	}
	if views == nil {
		views = []pollView{}
	}
	return views, nil
}

// parseWindow converts a month ("2006-01") or day ("2006-01-02") into a
// [from, to) UTC range; date wins when both are given.
func parseWindow(month, date string) (from, to time.Time, ok bool) {
	if date != "" {
		if d, err := time.Parse("2006-01-02", date); err == nil {
			return d, d.AddDate(0, 0, 1), true
		}
	}
	if month != "" {
		if m, err := time.Parse("2006-01", month); err == nil {
			return m, m.AddDate(0, 1, 0), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// get returns one non-deleted poll (any state) with options + caller vote; the
// HTTP layer live-gates public access.
func (p *polls) get(ctx context.Context, actor Actor, id string) (pollView, error) {
	if id == "" {
		return pollView{}, ErrNotFound
	}
	v := pollView{Options: []pollOption{}}
	err := p.s.pool.QueryRow(ctx, `SELECT id::text, question, language, is_active, coalesce(image_url,''), live_at
		FROM `+p.s.t.pollQuestions+` WHERE id = $1 AND deleted_at IS NULL`, id).
		Scan(&v.ID, &v.Question, &v.Language, &v.IsActive, &v.ImageURL, &v.LiveAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return pollView{}, ErrNotFound
	}
	if err != nil {
		return pollView{}, err
	}
	views := []pollView{v}
	if err := p.attach(ctx, actor, views); err != nil {
		return pollView{}, err
	}
	return views[0], nil
}

// attach batch-loads options + the caller's votes into views, sums total_votes,
// and absolutizes stored-relative image paths (backfilled legacy rows).
func (p *polls) attach(ctx context.Context, actor Actor, views []pollView) error {
	if len(views) == 0 {
		return nil
	}
	ids := make([]string, len(views))
	for i := range views {
		ids[i] = views[i].ID
	}
	opts, err := p.optionsFor(ctx, p.s.pool, ids)
	if err != nil {
		return err
	}
	votes, err := p.votesFor(ctx, p.s.pool, actor, ids)
	if err != nil {
		return err
	}
	for i := range views {
		views[i].ImageURL = p.rt.absMediaURL(views[i].ImageURL)
		if o := opts[views[i].ID]; o != nil {
			views[i].Options = o
		}
		total := 0
		for j := range views[i].Options {
			views[i].Options[j].ImageURL = p.rt.absMediaURL(views[i].Options[j].ImageURL)
			total += views[i].Options[j].VoteCount
		}
		views[i].TotalVotes = total
		if oid, ok := votes[views[i].ID]; ok {
			views[i].Voted, views[i].MyOption = true, oid
		}
	}
	return nil
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

	// Poll must exist, be live (live_at reached), active, and not soft-deleted
	// (hide all as 404 — a future poll must not leak via the vote path).
	var exists bool
	err = tx.QueryRow(ctx, `SELECT true FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND deleted_at IS NULL AND is_active = true AND live_at <= now()`, pollID).Scan(&exists)
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
	mux.HandleFunc("GET /polls/admin", p.handleAdminList) // literal beats {id}
	mux.HandleFunc("GET /polls/{id}", p.handleGet)
	mux.HandleFunc("POST /polls", p.handleCreate)
	mux.HandleFunc("PATCH /polls/{id}", p.handleUpdate)
	mux.HandleFunc("DELETE /polls/{id}", p.handleDelete)
	mux.HandleFunc("POST /polls/{id}/vote", p.handleVote)
	mux.HandleFunc("POST /polls/{id}/image", p.handleQuestionImage)
	mux.HandleFunc("POST /polls/{id}/options", p.handleAddOption)
	// Nested under the poll: a bare /polls/options/{oid} DELETE would ambiguously
	// overlap reactions' generic DELETE /{type}/{id}/reaction in ServeMux.
	mux.HandleFunc("PATCH /polls/{id}/options/{oid}", p.handleUpdateOption)
	mux.HandleFunc("DELETE /polls/{id}/options/{oid}", p.handleDeleteOption)
	mux.HandleFunc("POST /polls/options/{oid}/image", p.handleOptionImage)
}

// optionPatch is the add/update body; pointers so an absent field is untouched.
type optionPatch struct {
	Label    *string `json:"label"`
	ImageURL *string `json:"image_url"`
	Position *int    `json:"position"`
}

// handleAddOption appends an option to an existing poll. Position defaults to
// end-of-list. PollWrite-gated.
func (p *polls) handleAddOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeErr(w, ErrNotFound)
		return
	}
	var in optionPatch
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Label == nil || strings.TrimSpace(*in.Label) == "" {
		writeErr(w, badRequest("label is required"))
		return
	}
	var o pollOption
	err := p.s.pool.QueryRow(req.Context(), `INSERT INTO `+p.s.t.pollOptions+`
		(question_id, label, image_url, position)
		SELECT q.id, $2, $3, COALESCE($4, (SELECT COALESCE(MAX(position)+1, 0) FROM `+p.s.t.pollOptions+` WHERE question_id = q.id))
		FROM `+p.s.t.pollQuestions+` q WHERE q.id = $1 AND q.deleted_at IS NULL
		RETURNING id::text, label, coalesce(image_url,''), position, vote_count`,
		id, strings.TrimSpace(*in.Label), in.ImageURL, in.Position).
		Scan(&o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount)
	if errors.Is(err, pgx.ErrNoRows) { // poll missing/deleted
		writeErr(w, ErrNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	o.ImageURL = p.rt.absMediaURL(o.ImageURL)
	writeJSON(w, http.StatusCreated, o)
}

// handleUpdateOption edits an option's label/image/position. PollWrite-gated.
func (p *polls) handleUpdateOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	pollID, oid := req.PathValue("id"), req.PathValue("oid")
	if !uuidRe.MatchString(pollID) || !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	var in optionPatch
	if err := decodeJSON(req, &in); err != nil {
		writeErr(w, err)
		return
	}
	if in.Label != nil {
		l := strings.TrimSpace(*in.Label)
		if l == "" {
			writeErr(w, badRequest("label cannot be blank"))
			return
		}
		in.Label = &l
	}
	var o pollOption
	err := p.s.pool.QueryRow(req.Context(), `UPDATE `+p.s.t.pollOptions+`
		SET label = COALESCE($2, label), image_url = COALESCE($3, image_url), position = COALESCE($4, position)
		WHERE id = $1 AND question_id = $5
		RETURNING id::text, label, coalesce(image_url,''), position, vote_count`,
		oid, in.Label, in.ImageURL, in.Position, pollID).
		Scan(&o.ID, &o.Label, &o.ImageURL, &o.Position, &o.VoteCount)
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, ErrNotFound)
		return
	}
	if err != nil {
		writeErr(w, err)
		return
	}
	o.ImageURL = p.rt.absMediaURL(o.ImageURL)
	writeJSON(w, http.StatusOK, o)
}

// handleDeleteOption removes an option (its votes cascade). Refuses to shrink a
// poll below 2 options — that would break voting. PollWrite-gated.
func (p *polls) handleDeleteOption(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	pollID, oid := req.PathValue("id"), req.PathValue("oid")
	if !uuidRe.MatchString(pollID) || !uuidRe.MatchString(oid) {
		writeErr(w, ErrNotFound)
		return
	}
	// Single statement: delete only when >= 3 siblings exist, so a poll never
	// shrinks below 2 votable options.
	tag, err := p.s.pool.Exec(req.Context(), `DELETE FROM `+p.s.t.pollOptions+` o
		WHERE o.id = $1 AND o.question_id = $2 AND (
			SELECT count(*) FROM `+p.s.t.pollOptions+` s WHERE s.question_id = $2) >= 3`, oid, pollID)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		// Missing option and too-few-options both land here; disambiguate.
		var exists bool
		if err := p.s.pool.QueryRow(req.Context(), `SELECT true FROM `+p.s.t.pollOptions+`
			WHERE id = $1 AND question_id = $2`, oid, pollID).Scan(&exists); err == nil && exists {
			writeErr(w, badRequest("a poll needs at least 2 options"))
			return
		}
		writeErr(w, ErrNotFound)
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

// handleQuestionImage uploads a question-level image and stores its public URL.
// PollWrite-gated.
func (p *polls) handleQuestionImage(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	id := req.PathValue("id")
	if !uuidRe.MatchString(id) {
		writeErr(w, ErrNotFound)
		return
	}
	data, ct, ext, err := readUpload(req)
	if err != nil {
		writeErr(w, err)
		return
	}
	url, err := p.rt.media.Put(req.Context(), "polls/"+id+"."+ext, data, ct)
	if err != nil {
		writeErr(w, err)
		return
	}
	// Remember the previous image so a replace under a different key (extension
	// changed) can drop the old object instead of orphaning it. Best-effort.
	var prev *string
	_ = p.s.pool.QueryRow(req.Context(), `SELECT image_url FROM `+p.s.t.pollQuestions+`
		WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&prev)
	tag, err := p.s.pool.Exec(req.Context(), `UPDATE `+p.s.t.pollQuestions+`
		SET image_url = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, id, url)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	if prev != nil && *prev != url {
		p.rt.deleteMediaByURL(req.Context(), *prev)
	}
	writeJSON(w, http.StatusOK, map[string]string{"image_url": url})
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
	// Best-effort old-object cleanup on a key-changing replace (see question image).
	var prev *string
	_ = p.s.pool.QueryRow(req.Context(), `SELECT image_url FROM `+p.s.t.pollOptions+` WHERE id = $1`, oid).Scan(&prev)
	tag, err := p.s.pool.Exec(req.Context(), `UPDATE `+p.s.t.pollOptions+` SET image_url = $2 WHERE id = $1`, oid, url)
	if err != nil {
		writeErr(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		writeErr(w, ErrNotFound)
		return
	}
	if prev != nil && *prev != url {
		p.rt.deleteMediaByURL(req.Context(), *prev)
	}
	writeJSON(w, http.StatusOK, map[string]string{"image_url": url})
}

func (p *polls) handleList(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	limit, offset := parsePage(req)
	views, err := p.list(req.Context(), p.rt.actor(req.Context()), listFilter{
		language: q.Get("language"), month: q.Get("month"), date: q.Get("date"),
		limit: limit, offset: offset,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

// handleAdminList returns ALL non-deleted polls (future/inactive included) for
// the poll-management UI. PollWrite-gated.
func (p *polls) handleAdminList(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
		writeErr(w, err)
		return
	}
	q := req.URL.Query()
	limit, offset := parsePage(req)
	views, err := p.list(req.Context(), actor, listFilter{
		language: q.Get("language"), month: q.Get("month"), date: q.Get("date"),
		admin: true, limit: limit, offset: offset,
	})
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, views)
}

func (p *polls) handleGet(w http.ResponseWriter, req *http.Request) {
	actor := p.rt.actor(req.Context())
	v, err := p.get(req.Context(), actor, req.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	// Future/inactive polls are admin-only; hide as 404 (don't leak schedules).
	if !v.IsActive || v.LiveAt.After(time.Now()) {
		if err := p.rt.requirePerm(req.Context(), actor, p.rt.perms.PollWrite); err != nil {
			writeErr(w, ErrNotFound)
			return
		}
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
