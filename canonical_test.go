package socialkit

import (
	"context"
	"testing"
)

// aliasResolver canonicalizes any alias of gallery 123 ("slug-123", "123") to
// the composite key "123:en" — the doujins/hentai0 model (id+language).
type aliasResolver struct{}

func (aliasResolver) Resolve(_ context.Context, entityType, id string, _ Actor) (EntityRef, error) {
	switch id {
	case "slug-123", "123", "123:en":
		return EntityRef{Type: entityType, ID: "123:en", Visible: true, Accessible: true}, nil
	}
	return EntityRef{}, ErrNotFound
}

// Writes through ANY alias must land on ONE canonical row; reads through any
// alias must see it. This is what keeps slug/id/composite forms from
// fragmenting engagement data.
func TestCanonicalKey_UnifiesAliases(t *testing.T) {
	rt, _ := newTestRuntime(t, Options{Entities: aliasResolver{}, EntityTypes: []string{"gallery"}})
	ctx := context.Background()
	u := Actor{ID: "u1"}

	// Favorite via the slug alias -> stored under "123:en".
	if err := rt.favorites.add(ctx, u, "gallery", "slug-123"); err != nil {
		t.Fatalf("add via alias: %v", err)
	}
	fav, err := rt.IsFavorited(ctx, "u1", []EntityKey{{Type: "gallery", ID: "123:en"}})
	if err != nil {
		t.Fatal(err)
	}
	if !fav[EntityKey{Type: "gallery", ID: "123:en"}] {
		t.Fatal("favorite via alias not stored under the canonical key")
	}

	// React via the bare id -> same canonical row feeds the rollup.
	if _, err := rt.reactions.react(ctx, u, "gallery", "123", 1); err != nil {
		t.Fatalf("react via alias: %v", err)
	}
	// Comment via the canonical form; list via the slug alias must see it.
	if _, err := rt.comments.create(ctx, u, "gallery", "123:en", createInput{Body: "hi"}); err != nil {
		t.Fatalf("comment: %v", err)
	}
	list, err := rt.comments.list(ctx, u, "gallery", "slug-123", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("comment list via alias = %d rows, want 1", len(list))
	}

	// One rollup row under the canonical key aggregates everything.
	c, err := rt.Counts(ctx, "gallery", "123:en")
	if err != nil {
		t.Fatal(err)
	}
	if c.Likes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("canonical rollup = %+v, want likes/favorites/comments = 1/1/1", c)
	}
	if alias, _ := rt.Counts(ctx, "gallery", "slug-123"); alias != (EntityCounts{}) {
		t.Fatalf("alias key leaked into the rollup: %+v", alias)
	}

	// Unfavorite via a different alias removes the canonical row.
	if err := rt.favorites.remove(ctx, u, "gallery", "123"); err != nil {
		t.Fatal(err)
	}
	c, _ = rt.Counts(ctx, "gallery", "123:en")
	if c.Favorites != 0 {
		t.Fatalf("favorites after aliased remove = %d, want 0", c.Favorites)
	}
}

func TestRuntime_ListFavoritesExported(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "a", true, true)
	res.set("gallery", "b", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"gallery"}})
	ctx := context.Background()
	u := Actor{ID: "u1"}

	if err := rt.favorites.add(ctx, u, "gallery", "a"); err != nil {
		t.Fatal(err)
	}
	if err := rt.favorites.add(ctx, u, "gallery", "b"); err != nil {
		t.Fatal(err)
	}
	items, err := rt.ListFavorites(ctx, "u1", 0, 0) // limit <= 0 = all
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].EntityID != "b" { // newest-first
		t.Fatalf("ListFavorites = %+v, want [b, a]", items)
	}
	one, err := rt.ListFavorites(ctx, "u1", 1, 0)
	if err != nil || len(one) != 1 {
		t.Fatalf("paged ListFavorites = %+v (%v), want 1 row", one, err)
	}
}
