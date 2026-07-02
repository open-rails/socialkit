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

	views, err := p.list(ctx, pollAdmin, "")
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

	views, err := p.list(ctx, pollAdmin, "en")
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
	lv, err := p.list(ctx, voter, "")
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
