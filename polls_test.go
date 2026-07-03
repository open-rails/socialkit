package socialkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// pollAdmin is the gated writer; pollWritePerm must be non-empty or requirePerm
// fails closed even under allowAll.
var pollAdmin = Actor{ID: "admin", Kind: "user"}

const pollWritePerm = "poll:write"

// newPollTest builds a Runtime with PollWrite wired and returns the polls service.
func newPollTest(t *testing.T, opts Options) (*Runtime, *polls) {
	t.Helper()
	if opts.Perms.PollWrite == "" {
		opts.Perms.PollWrite = pollWritePerm
	}
	rt, _ := newTestRuntime(t, opts)
	return rt, newPolls(rt)
}

func twoOptionPoll(language string) createPollInput {
	return createPollInput{
		Question: "Best girl?",
		Language: language,
		Options: []createOptionInput{
			{Label: "Rei", Position: 0},
			{Label: "Asuka", Position: 1},
		},
	}
}

func optVoteCount(v pollView, optID string) int {
	for _, o := range v.Options {
		if o.ID == optID {
			return o.VoteCount
		}
	}
	return -1
}

func totalVotes(v pollView) int {
	n := 0
	for _, o := range v.Options {
		n += o.VoteCount
	}
	return n
}

func TestPolls_LifecycleCreateListVoteTally(t *testing.T) {
	_, p := newPollTest(t, Options{})
	ctx := context.Background()

	created, err := p.create(ctx, pollAdmin, twoOptionPoll(""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.ID == "" || len(created.Options) != 2 {
		t.Fatalf("created poll malformed: %+v", created)
	}

	views, err := p.list(ctx, pollAdmin, listFilter{limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(views) != 1 || views[0].ID != created.ID || len(views[0].Options) != 2 {
		t.Fatalf("list = %+v, want the one created poll with 2 options", views)
	}

	voter := Actor{ID: "v1", Kind: "user"}
	opt := created.Options[0].ID
	view, err := p.vote(ctx, voter, created.ID, opt)
	if err != nil {
		t.Fatalf("vote: %v", err)
	}
	if optVoteCount(view, opt) != 1 || !view.Voted || view.MyOption != opt {
		t.Fatalf("post-vote view = %+v, want opt count 1 and voted=true", view)
	}
	// tally persists on a fresh read
	got, err := p.get(ctx, voter, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if optVoteCount(got, opt) != 1 || totalVotes(got) != 1 {
		t.Fatalf("get tally = %+v, want exactly one vote on opt", got)
	}
}

func TestPolls_ConcurrentDuplicateVoteIsExact(t *testing.T) {
	_, p := newPollTest(t, Options{})
	ctx := context.Background()
	created, err := p.create(ctx, pollAdmin, twoOptionPoll(""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	racer := Actor{ID: "racer", Kind: "user"}
	opt := created.Options[0].ID

	var wg sync.WaitGroup
	errs := make(chan error, 15)
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.vote(context.Background(), racer, created.ID, opt)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent vote: %v", err)
		}
	}
	// 15 identical votes from one actor => exactly one vote counted.
	got, err := p.get(ctx, racer, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if optVoteCount(got, opt) != 1 || totalVotes(got) != 1 {
		t.Fatalf("tally after race = %+v, want exactly 1", got)
	}
}

func TestPolls_AnonymousDedupByIP(t *testing.T) {
	_, p := newPollTest(t, Options{})
	ctx := context.Background()
	created, err := p.create(ctx, pollAdmin, twoOptionPoll(""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	opt := created.Options[0].ID

	anon1 := Actor{IP: "10.0.0.1", Anonymous: true}
	if _, err := p.vote(ctx, anon1, created.ID, opt); err != nil {
		t.Fatalf("anon1 vote: %v", err)
	}
	if _, err := p.vote(ctx, anon1, created.ID, opt); err != nil {
		t.Fatalf("anon1 re-vote: %v", err)
	}
	// distinct IP counts separately
	anon2 := Actor{IP: "10.0.0.2", Anonymous: true}
	if _, err := p.vote(ctx, anon2, created.ID, opt); err != nil {
		t.Fatalf("anon2 vote: %v", err)
	}
	got, err := p.get(ctx, anon1, created.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if optVoteCount(got, opt) != 2 || totalVotes(got) != 2 {
		t.Fatalf("tally = %+v, want 2 (one per distinct IP)", got)
	}

	// unidentifiable actor (no id, no ip) is rejected
	if _, err := p.vote(ctx, Actor{Anonymous: true}, created.ID, opt); err == nil {
		t.Fatal("expected rejection for unidentifiable voter")
	}
}

func TestPolls_LanguageSlicing(t *testing.T) {
	_, p := newPollTest(t, Options{})
	ctx := context.Background()
	if _, err := p.create(ctx, pollAdmin, twoOptionPoll("ja")); err != nil {
		t.Fatalf("create ja: %v", err)
	}
	en, err := p.create(ctx, pollAdmin, twoOptionPoll("en"))
	if err != nil {
		t.Fatalf("create en: %v", err)
	}

	views, err := p.list(ctx, pollAdmin, listFilter{language: "en", limit: 20})
	if err != nil {
		t.Fatalf("list en: %v", err)
	}
	if len(views) != 1 || views[0].ID != en.ID || views[0].Language != "en" {
		t.Fatalf("list en = %+v, want only the en poll", views)
	}
}

func TestPolls_AdminGateDeniedReturns403(t *testing.T) {
	rt, p := newPollTest(t, Options{Authz: denyAll{}})
	ctx := context.Background()

	_, err := p.create(ctx, pollAdmin, twoOptionPoll(""))
	var he httpError
	if !errors.As(err, &he) || he.status != http.StatusForbidden {
		t.Fatalf("create under denyAll: want 403 httpError, got %v", err)
	}

	// same gate over HTTP
	mux := http.NewServeMux()
	p.mount(mux)
	rec := doPollReq(t, mux, "POST", "/polls", twoOptionPoll(""), pollAdmin)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("POST /polls under denyAll: status %d, body %s", rec.Code, rec.Body.String())
	}
	_ = rt
}

func TestPolls_HasVotedReflectedPerCaller(t *testing.T) {
	_, p := newPollTest(t, Options{})
	ctx := context.Background()
	created, err := p.create(ctx, pollAdmin, twoOptionPoll(""))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	opt := created.Options[1].ID
	voter := Actor{ID: "voter", Kind: "user"}
	if _, err := p.vote(ctx, voter, created.ID, opt); err != nil {
		t.Fatalf("vote: %v", err)
	}

	// the caller who voted sees voted + chosen option, in both get and list
	gv, err := p.get(ctx, voter, created.ID)
	if err != nil {
		t.Fatalf("get voter: %v", err)
	}
	if !gv.Voted || gv.MyOption != opt {
		t.Fatalf("get for voter = %+v, want voted=true my_option=%s", gv, opt)
	}
	lv, err := p.list(ctx, voter, listFilter{limit: 20})
	if err != nil {
		t.Fatalf("list voter: %v", err)
	}
	if len(lv) != 1 || !lv[0].Voted || lv[0].MyOption != opt {
		t.Fatalf("list for voter = %+v, want voted=true my_option=%s", lv, opt)
	}

	// a different caller has not voted
	other := Actor{ID: "other", Kind: "user"}
	ov, err := p.get(ctx, other, created.ID)
	if err != nil {
		t.Fatalf("get other: %v", err)
	}
	if ov.Voted || ov.MyOption != "" {
		t.Fatalf("get for non-voter = %+v, want voted=false", ov)
	}
}

func TestPolls_HTTPRoutesEndToEnd(t *testing.T) {
	_, p := newPollTest(t, Options{})
	mux := http.NewServeMux()
	p.mount(mux)

	rec := doPollReq(t, mux, "POST", "/polls", twoOptionPoll(""), pollAdmin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("POST /polls: status %d, body %s", rec.Code, rec.Body.String())
	}
	var created pollView
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create resp: %v", err)
	}

	rec = doPollReq(t, mux, "GET", "/polls", nil, Actor{Anonymous: true, IP: "9.9.9.9"})
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /polls: status %d", rec.Code)
	}

	vote := map[string]string{"option_id": created.Options[0].ID}
	rec = doPollReq(t, mux, "POST", "/polls/"+created.ID+"/vote", vote, Actor{Anonymous: true, IP: "9.9.9.9"})
	if rec.Code != http.StatusOK {
		t.Fatalf("POST vote: status %d, body %s", rec.Code, rec.Body.String())
	}
	var voted pollView
	if err := json.Unmarshal(rec.Body.Bytes(), &voted); err != nil {
		t.Fatalf("decode vote resp: %v", err)
	}
	if optVoteCount(voted, created.Options[0].ID) != 1 {
		t.Fatalf("vote resp tally = %+v, want 1", voted)
	}
}

func doPollReq(t *testing.T, h http.Handler, method, target string, body any, actor Actor) *httptest.ResponseRecorder {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		r = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, r)
	req = req.WithContext(withActor(req.Context(), actor))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// pollAdminOnly authorizes only the admin actor — lets one runtime test both
// admin success and public denial paths.
type pollAdminOnly struct{}

func (pollAdminOnly) Can(_ context.Context, a Actor, _ string) (bool, error) {
	return a.ID == "admin", nil
}

func TestPolls_LiveGatingAndAdminList(t *testing.T) {
	rt, p := newPollTest(t, Options{Authz: pollAdminOnly{}})
	ctx := context.Background()
	future := time.Now().Add(time.Hour)

	live, err := p.create(ctx, pollAdmin, twoOptionPoll("en"))
	if err != nil {
		t.Fatalf("create live: %v", err)
	}
	sched := twoOptionPoll("en")
	sched.LiveAt = &future
	scheduled, err := p.create(ctx, pollAdmin, sched)
	if err != nil {
		t.Fatalf("create scheduled: %v", err)
	}

	// Public list: only the live poll; admin list: both, newest-live-first.
	pub, err := p.list(ctx, Actor{ID: "user1"}, listFilter{limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(pub) != 1 || pub[0].ID != live.ID {
		t.Fatalf("public list leaked a future poll: %+v", pollIDs(pub))
	}
	adm, err := p.list(ctx, pollAdmin, listFilter{admin: true, limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(adm) != 2 || adm[0].ID != scheduled.ID {
		t.Fatalf("admin list = %v, want [scheduled, live]", pollIDs(adm))
	}

	// HTTP: a future poll 404s for the public but serves for the admin; the
	// admin list endpoint 403s for the public.
	get := func(actor Actor, path string) int {
		req := httptest.NewRequest("GET", path, nil)
		req = req.WithContext(withActor(req.Context(), actor))
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := get(Actor{ID: "user1"}, "/polls/"+scheduled.ID); code != http.StatusNotFound {
		t.Fatalf("public GET future poll = %d, want 404", code)
	}
	if code := get(pollAdmin, "/polls/"+scheduled.ID); code != http.StatusOK {
		t.Fatalf("admin GET future poll = %d, want 200", code)
	}
	if code := get(Actor{ID: "user1"}, "/polls/admin"); code != http.StatusForbidden {
		t.Fatalf("public GET /polls/admin = %d, want 403", code)
	}

	// Voting on a not-yet-live poll is a 404 (no schedule leak).
	if _, err := p.vote(ctx, Actor{ID: "user1"}, scheduled.ID, scheduled.Options[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("vote on future poll: want ErrNotFound, got %v", err)
	}
	// Rescheduling it into the past makes it publicly live.
	past := time.Now().Add(-time.Minute)
	if _, err := p.update(ctx, pollAdmin, scheduled.ID, updatePollInput{LiveAt: &past}); err != nil {
		t.Fatal(err)
	}
	pub, _ = p.list(ctx, Actor{ID: "user1"}, listFilter{limit: 20})
	if len(pub) != 2 {
		t.Fatalf("after reschedule, public list = %v, want both", pollIDs(pub))
	}
}

func TestPolls_MonthWindowsTotalsAndImageAbsolutization(t *testing.T) {
	_, p := newPollTest(t, Options{Storage: &StorageConfig{Bucket: "b", PublicBaseURL: "https://cdn.test/"}})
	ctx := context.Background()

	mk := func(lang, live string) pollView {
		t.Helper()
		at, _ := time.Parse("2006-01-02", live)
		in := twoOptionPoll(lang)
		in.LiveAt = &at
		v, err := p.create(ctx, pollAdmin, in)
		if err != nil {
			t.Fatalf("create %s: %v", live, err)
		}
		return v
	}
	may := mk("en", "2024-05-15")
	jun := mk("en", "2024-06-15")

	got, err := p.list(ctx, pollAdmin, listFilter{month: "2024-05", limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != may.ID {
		t.Fatalf("month=2024-05 -> %v, want [may]", pollIDs(got))
	}
	got, err = p.list(ctx, pollAdmin, listFilter{date: "2024-06-15", limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != jun.ID {
		t.Fatalf("date=2024-06-15 -> %v, want [jun]", pollIDs(got))
	}

	// TotalVotes sums option counters.
	if _, err := p.vote(ctx, Actor{ID: "v1"}, may.ID, may.Options[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := p.vote(ctx, Actor{ID: "v2"}, may.ID, may.Options[1].ID); err != nil {
		t.Fatal(err)
	}
	v, _ := p.get(ctx, pollAdmin, may.ID)
	if v.TotalVotes != 2 || v.TotalVotes != totalVotes(v) {
		t.Fatalf("TotalVotes = %d (options sum %d), want 2", v.TotalVotes, totalVotes(v))
	}

	// A stored RELATIVE path (backfilled legacy row) absolutizes against the
	// public bucket origin; an absolute URL passes through untouched.
	rel := "assets/poll-images/legacy.webp"
	if _, err := p.update(ctx, pollAdmin, may.ID, updatePollInput{ImageURL: &rel}); err != nil {
		t.Fatal(err)
	}
	abs := "https://elsewhere.example/x.png"
	if _, err := p.update(ctx, pollAdmin, jun.ID, updatePollInput{ImageURL: &abs}); err != nil {
		t.Fatal(err)
	}
	v, _ = p.get(ctx, pollAdmin, may.ID)
	if v.ImageURL != "https://cdn.test/assets/poll-images/legacy.webp" {
		t.Fatalf("relative image not absolutized: %q", v.ImageURL)
	}
	v, _ = p.get(ctx, pollAdmin, jun.ID)
	if v.ImageURL != abs {
		t.Fatalf("absolute image mangled: %q", v.ImageURL)
	}
}

func pollIDs(vs []pollView) []string {
	ids := make([]string, len(vs))
	for i := range vs {
		ids[i] = vs[i].ID
	}
	return ids
}

func TestPolls_OptionCRUD(t *testing.T) {
	rt, p := newPollTest(t, Options{Authz: pollAdminOnly{}})
	ctx := context.Background()
	v, err := p.create(ctx, pollAdmin, twoOptionPoll("en"))
	if err != nil {
		t.Fatal(err)
	}

	do := func(actor Actor, method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var rdr io.Reader
		if body != "" {
			rdr = bytes.NewBufferString(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req = req.WithContext(withActor(req.Context(), actor))
		rec := httptest.NewRecorder()
		rt.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Add a third option; position auto-appends after the existing two.
	rec := do(pollAdmin, "POST", "/polls/"+v.ID+"/options", `{"label":"Misato"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add option: %d %s", rec.Code, rec.Body.String())
	}
	var added pollOption
	if err := json.Unmarshal(rec.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Position != 2 {
		t.Fatalf("added position = %d, want 2 (auto end-of-list)", added.Position)
	}

	// Rename it + move it to the front.
	rec = do(pollAdmin, "PATCH", "/polls/"+v.ID+"/options/"+added.ID, `{"label":"Kaji","position":0}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update option: %d %s", rec.Code, rec.Body.String())
	}
	var upd pollOption
	_ = json.Unmarshal(rec.Body.Bytes(), &upd)
	if upd.Label != "Kaji" || upd.Position != 0 {
		t.Fatalf("update result = %+v", upd)
	}

	// Delete works at 3 options; the next delete would leave 1 -> refused.
	if rec = do(pollAdmin, "DELETE", "/polls/"+v.ID+"/options/"+added.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete option: %d %s", rec.Code, rec.Body.String())
	}
	if rec = do(pollAdmin, "DELETE", "/polls/"+v.ID+"/options/"+v.Options[0].ID, ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("delete below 2 options: %d, want 400", rec.Code)
	}

	// Non-admin is denied on all three.
	user := Actor{ID: "user1"}
	if rec = do(user, "POST", "/polls/"+v.ID+"/options", `{"label":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin add = %d, want 403", rec.Code)
	}
	if rec = do(user, "PATCH", "/polls/"+v.ID+"/options/"+v.Options[0].ID, `{"label":"x"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin update = %d, want 403", rec.Code)
	}
	if rec = do(user, "DELETE", "/polls/"+v.ID+"/options/"+v.Options[0].ID, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("non-admin delete = %d, want 403", rec.Code)
	}
}
