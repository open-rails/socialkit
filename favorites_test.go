package socialkit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// favIsFavorited is the batch check narrowed to one target for terse assertions.
func favIsFavorited(t *testing.T, f *favorites, userID, typ, id string) bool {
	t.Helper()
	key := EntityKey{Type: typ, ID: id}
	m, err := f.IsFavorited(context.Background(), userID, []EntityKey{key})
	if err != nil {
		t.Fatalf("IsFavorited: %v", err)
	}
	return m[key]
}

// favLastKind returns the Kind of the most recent recorder signal ("" if none).
func favLastKind(rec *recordingRecorder) string {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reactions) == 0 {
		return ""
	}
	return rec.reactions[len(rec.reactions)-1].Kind
}

func TestFavorites_AddRemoveStatusRecorder(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rec := &recordingRecorder{}
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}, Recorder: rec})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	// add -> favorited, exactly one row, recorder emits "favorite".
	if err := f.add(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if !favIsFavorited(t, f, "u1", "widget", "1") {
		t.Fatal("status after add: want favorited")
	}
	if k := favLastKind(rec); k != "favorite" {
		t.Fatalf("recorder kind after add = %q, want favorite", k)
	}
	if got := rec.reactionSignals()[0].Delta; got != 0 {
		t.Fatalf("favorite delta = %d, want 0", got)
	}
	if c, err := rt.Counts(ctx, "widget", "1"); err != nil || c.Favorites != 1 {
		t.Fatalf("favorites count after add = %d err=%v, want 1", c.Favorites, err)
	}

	// re-add is idempotent: no error, still a single row.
	if err := f.add(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("re-add: %v", err)
	}
	if got := rec.reactionCount(); got != 1 {
		t.Fatalf("recorder signals after re-add = %d, want 1", got)
	}
	if c, err := rt.Counts(ctx, "widget", "1"); err != nil || c.Favorites != 1 {
		t.Fatalf("favorites count after re-add = %d err=%v, want 1 (idempotent)", c.Favorites, err)
	}

	// remove -> not favorited, recorder emits "unfavorite".
	if err := f.remove(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if favIsFavorited(t, f, "u1", "widget", "1") {
		t.Fatal("status after remove: want not favorited")
	}
	if k := favLastKind(rec); k != "unfavorite" {
		t.Fatalf("recorder kind after remove = %q, want unfavorite", k)
	}
	if signals := rec.reactionSignals(); len(signals) != 2 || signals[1].Delta != 0 {
		t.Fatalf("recorder signals after remove = %+v, want unfavorite delta 0", signals)
	}
	if c, err := rt.Counts(ctx, "widget", "1"); err != nil || c.Favorites != 0 {
		t.Fatalf("favorites count after remove = %d err=%v, want 0", c.Favorites, err)
	}

	// remove again is idempotent (no row) -> no error.
	if err := f.remove(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	if got := rec.reactionCount(); got != 2 {
		t.Fatalf("recorder signals after repeated remove = %d, want 2", got)
	}
}

func TestFavorites_RecorderObservesCommittedState(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	recorder := &committedStateRecorder{}
	rt, pool := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}, Recorder: recorder})
	recorder.pool = pool

	if err := rt.favorites.add(context.Background(), Actor{ID: "u1", Kind: "user"}, "widget", "1"); err != nil {
		t.Fatalf("favorite: %v", err)
	}
	recorder.assertVisible(t, 1)
	if err := rt.favorites.remove(context.Background(), Actor{ID: "u1", Kind: "user"}, "widget", "1"); err != nil {
		t.Fatalf("unfavorite: %v", err)
	}
	recorder.assertVisible(t, 2)
}

func TestFavorites_RecorderSkipsTransactionError(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	recorder := &recordingRecorder{}
	rt, pool := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}, Recorder: recorder})
	if _, err := pool.Exec(context.Background(), `DROP TABLE hostapp.social_entity_counts`); err != nil {
		t.Fatalf("drop counts table: %v", err)
	}

	if err := rt.favorites.add(context.Background(), Actor{ID: "u1", Kind: "user"}, "widget", "1"); err == nil {
		t.Fatal("favorite error = nil, want transaction failure")
	}
	if got := recorder.reactionCount(); got != 0 {
		t.Fatalf("recorder signals = %d, want 0 after rollback", got)
	}
}

func TestFavorites_BatchIsFavorited(t *testing.T) {
	res := &fakeResolver{}
	for _, id := range []string{"1", "2", "3"} {
		res.set("widget", id, true, true)
	}
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	// favorite 1 and 3 only.
	if err := f.add(ctx, actor, "widget", "1"); err != nil {
		t.Fatalf("add 1: %v", err)
	}
	if err := f.add(ctx, actor, "widget", "3"); err != nil {
		t.Fatalf("add 3: %v", err)
	}

	targets := []EntityKey{
		{Type: "widget", ID: "1"},
		{Type: "widget", ID: "2"},
		{Type: "widget", ID: "3"},
		{Type: "widget", ID: "4"},
	}
	got, err := f.IsFavorited(ctx, "u1", targets)
	if err != nil {
		t.Fatalf("IsFavorited: %v", err)
	}
	want := map[EntityKey]bool{
		{Type: "widget", ID: "1"}: true,
		{Type: "widget", ID: "2"}: false,
		{Type: "widget", ID: "3"}: true,
		{Type: "widget", ID: "4"}: false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("IsFavorited = %v, want %v", got, want)
	}
}

// TestFavorites_WishlistVisibleNotAccessible is the whole point: a visible but
// premium-locked target can be favorited, unlike a reaction which requires
// accessibility.
func TestFavorites_WishlistVisibleNotAccessible(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "premium", true, false) // visible but not accessible (locked)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	if err := f.add(ctx, actor, "widget", "premium"); err != nil {
		t.Fatalf("favorite premium-locked: want success, got %v", err)
	}
	if !favIsFavorited(t, f, "u1", "widget", "premium") {
		t.Fatal("status after favoriting premium-locked: want favorited")
	}
	// Contrast: reactions gate on accessibility, so the same target is rejected.
	if err := reactErr(rt.reactions.react(ctx, actor, "widget", "premium", 1)); !errors.Is(err, ErrForbidden) {
		t.Fatalf("react on premium-locked: want ErrForbidden, got %v", err)
	}
}

func TestFavorites_GatingHiddenMissing(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "hidden", false, false) // unpublished/soft-deleted
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	actor := Actor{ID: "u1", Kind: "user"}

	if err := f.add(ctx, actor, "widget", "hidden"); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("favorite hidden: want ErrNotVisible, got %v", err)
	}
	if err := f.add(ctx, actor, "widget", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("favorite missing: want ErrNotFound, got %v", err)
	}
	if err := f.add(ctx, actor, "unregistered", "1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("favorite unregistered type: want ErrNotFound, got %v", err)
	}
}

func TestFavorites_AnonymousRejected(t *testing.T) {
	res := &fakeResolver{}
	res.set("widget", "1", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	mux := http.NewServeMux()
	newFavorites(rt).mount(mux)

	// every route requires a non-anonymous actor -> 401.
	anon := Actor{Anonymous: true, IP: "10.0.0.9"}
	for _, c := range []struct{ method, path string }{
		{"POST", "/widget/1/favorite"},
		{"DELETE", "/widget/1/favorite"},
		{"GET", "/widget/1/favorite"},
		{"GET", "/favorites"},
	} {
		req := httptest.NewRequest(c.method, c.path, nil)
		req = req.WithContext(withActor(req.Context(), anon))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s: status %d, want 401 (body %s)", c.method, c.path, w.Code, w.Body.String())
		}
	}

	// sanity: an authenticated actor succeeds via the mounted route.
	req := httptest.NewRequest("POST", "/widget/1/favorite", nil)
	req = req.WithContext(withActor(req.Context(), Actor{ID: "u1", Kind: "user"}))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated favorite: status %d, body %s", w.Code, w.Body.String())
	}
}

func TestFavorites_ListAndCounts(t *testing.T) {
	res := &fakeResolver{}
	for _, id := range []string{"1", "2", "3"} {
		res.set("widget", id, true, true)
	}
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"widget"}})
	f := newFavorites(rt)
	ctx := context.Background()
	u1 := Actor{ID: "u1", Kind: "user"}
	u2 := Actor{ID: "u2", Kind: "user"}

	// u1 favorites 1,2,3 in order; newest-first list => 3,2,1.
	for _, id := range []string{"1", "2", "3"} {
		if err := f.add(ctx, u1, "widget", id); err != nil {
			t.Fatalf("add %s: %v", id, err)
		}
	}
	items, err := f.list(ctx, "u1", 20, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var order []string
	for _, it := range items {
		order = append(order, it.EntityID)
	}
	if want := []string{"3", "2", "1"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("list order = %v, want %v (newest-first)", order, want)
	}

	// a second user favorites widget/1 -> the rollup reflects both users.
	if err := f.add(ctx, u2, "widget", "1"); err != nil {
		t.Fatalf("u2 add: %v", err)
	}
	if c, err := rt.Counts(ctx, "widget", "1"); err != nil || c.Favorites != 2 {
		t.Fatalf("favorites count(widget,1) = %d err=%v, want 2", c.Favorites, err)
	}
	counts, err := rt.CountsByEntity(ctx, "widget", []string{"1", "2", "3", "4"})
	if err != nil {
		t.Fatalf("CountsByEntity: %v", err)
	}
	if counts["1"].Favorites != 2 || counts["2"].Favorites != 1 || counts["3"].Favorites != 1 || counts["4"].Favorites != 0 {
		t.Fatalf("CountsByEntity favorites = %v, want 1:2 2:1 3:1 4:0", counts)
	}

	// pagination: limit 2 returns the two newest, offset walks the window.
	page, err := f.list(ctx, "u1", 2, 0)
	if err != nil || len(page) != 2 || page[0].EntityID != "3" || page[1].EntityID != "2" {
		t.Fatalf("paged list = %v err=%v, want [3 2]", page, err)
	}
}
