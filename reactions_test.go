package socialkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestReactions_TransitionsAndCounts(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("react: %v", err)
		}
	}
	must(reactErr(rt.reactions.react(ctx, actor, "widget", "1", 1))) // like
	assertCounts(t, rt, actor, "widget", "1", 1, 0, 1)
	must(reactErr(rt.reactions.react(ctx, actor, "widget", "1", -1))) // switch to dislike
	assertCounts(t, rt, actor, "widget", "1", 0, 1, -1)
	must(reactErr(rt.reactions.react(ctx, actor, "widget", "1", 0))) // neutral (not delete)
	assertCounts(t, rt, actor, "widget", "1", 0, 0, 0)

	// second distinct user likes -> independent row
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u2", Kind: "user"}, "widget", "1", 1)))
	assertCounts(t, rt, actor, "widget", "1", 1, 0, 0) // u1 still neutral, one like total
}

func TestReactions_ConcurrentDoubleLikeIsExact(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "42", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	actor := Actor{ID: "racer", Kind: "user"}

	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- reactErr(rt.reactions.react(context.Background(), actor, "widget", "42", 1))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent react: %v", err)
		}
	}
	// 20 concurrent identical likes from one actor => exactly one like.
	assertCounts(t, rt, actor, "widget", "42", 1, 0, 1)
}

func TestReactions_GatingRejectsInaccessibleAndMissing(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "locked", true, false)  // visible but premium-locked
	res.set("widget", "hidden", false, false) // unpublished/deleted
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	if err := reactErr(rt.reactions.react(ctx, actor, "widget", "locked", 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react on premium-locked: want ErrForbidden, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "widget", "hidden", 1)); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("react on hidden: want ErrNotVisible, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "widget", "ghost", 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("react on missing: want ErrNotFound, got %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, actor, "unregistered", "1", 1)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("react on unregistered type: want ErrNotFound, got %v", err)
	}
}

func TestReactions_AnonymousDedupByIP(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	ctx := context.Background()
	anon := Actor{IP: "10.0.0.1", Anonymous: true}

	if err := reactErr(rt.reactions.react(ctx, anon, "widget", "1", 1)); err != nil {
		t.Fatalf("anon like: %v", err)
	}
	if err := reactErr(rt.reactions.react(ctx, anon, "widget", "1", 1)); err != nil {
		t.Fatalf("anon re-like: %v", err)
	}
	assertCounts(t, rt, anon, "widget", "1", 1, 0, 1) // one like from the IP

	// unidentifiable actor (no id, no ip) is rejected
	if err := reactErr(rt.reactions.react(ctx, Actor{Anonymous: true}, "widget", "1", 1)); err == nil {
		t.Fatal("expected rejection for unidentifiable actor")
	}
}

func TestReactions_RecorderSignalEmitted(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rec := &recordingRecorder{}
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}, Recorder: rec})
	if err := reactErr(rt.reactions.react(context.Background(), Actor{ID: "u1"}, "widget", "1", 1)); err != nil {
		t.Fatalf("react: %v", err)
	}
	if rec.reactionCount() != 1 {
		t.Fatalf("recorder signals = %d, want 1", rec.reactionCount())
	}
}

func TestReactions_HTTPRoute(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	h := rt.Handler()

	req := httptest.NewRequest("POST", "/widget/1/like", nil)
	req = req.WithContext(withActor(req.Context(), Actor{ID: "u1", Kind: "user"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST like: status %d, body %s", rec.Code, rec.Body.String())
	}
}

func assertCounts(t *testing.T, rt *Runtime, actor Actor, entityType, id string, wantLikes, wantDislikes int, wantMine int16) {
	t.Helper()
	c, err := rt.reactions.counts(context.Background(), rt.store.pool, actor, entityType, id)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if c.Likes != wantLikes || c.Dislikes != wantDislikes || c.Mine != wantMine {
		t.Fatalf("counts = %+v, want likes=%d dislikes=%d mine=%d", c, wantLikes, wantDislikes, wantMine)
	}
}
