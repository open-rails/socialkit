package socialkit

import (
	"context"
	"testing"
)

func TestCounts_RollupAggregates(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "g1", true, true)
	res.set("gallery", "g2", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"gallery"}})
	ctx := context.Background()

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u1"}, "gallery", "g1", 1)))  // like
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u2"}, "gallery", "g1", -1))) // dislike
	must(rt.favorites.add(ctx, Actor{ID: "u1"}, "gallery", "g1"))                 // favorite
	if _, err := rt.comments.create(ctx, Actor{ID: "u1"}, "gallery", "g1", createInput{Body: "hi"}); err != nil {
		t.Fatal(err)
	}
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u3"}, "gallery", "g2", 1)))

	c, err := rt.Counts(ctx, "gallery", "g1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Likes != 1 || c.Dislikes != 1 || c.Favorites != 1 || c.CommentCount != 1 {
		t.Fatalf("g1 counts = %+v, want likes/dislikes/favorites/comments = 1/1/1/1", c)
	}

	m, err := rt.CountsByEntity(ctx, "gallery", []string{"g1", "g2", "g3"})
	if err != nil {
		t.Fatal(err)
	}
	if m["g1"].Likes != 1 || m["g2"].Likes != 1 {
		t.Fatalf("batch counts = %+v", m)
	}
	if _, ok := m["g3"]; ok {
		t.Fatal("g3 has no engagement; it should be absent from the batch map")
	}

	// Unfavorite decrements; switching a reaction moves the split.
	must(rt.favorites.remove(ctx, Actor{ID: "u1"}, "gallery", "g1"))
	must(reactErr(rt.reactions.react(ctx, Actor{ID: "u2"}, "gallery", "g1", 1))) // dislike -> like
	c, _ = rt.Counts(ctx, "gallery", "g1")
	if c.Favorites != 0 || c.Likes != 2 || c.Dislikes != 0 {
		t.Fatalf("after unfavorite + switch: %+v, want favorites=0 likes=2 dislikes=0", c)
	}
}

func TestCounts_CommentCountLifecycle(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	a := Actor{ID: "u1"}

	top := mustComment(t, rt, a, "gallery", "1", createInput{Body: "top"})
	mustComment(t, rt, a, "gallery", "1", createInput{Body: "reply", ParentID: top.ID}) // reply: no entity bump
	if c, _ := rt.Counts(ctx, "gallery", "1"); c.CommentCount != 1 {
		t.Fatalf("comment_count = %d, want 1 (reply excluded)", c.CommentCount)
	}
	top2 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "top2"})
	if c, _ := rt.Counts(ctx, "gallery", "1"); c.CommentCount != 2 {
		t.Fatalf("comment_count = %d, want 2", c.CommentCount)
	}
	if err := rt.comments.softDelete(ctx, a, top2.ID); err != nil {
		t.Fatal(err)
	}
	if c, _ := rt.Counts(ctx, "gallery", "1"); c.CommentCount != 1 {
		t.Fatalf("comment_count after delete = %d, want 1", c.CommentCount)
	}
}

// Wilson "best": 9-likes/1-dislike must outrank 1-like/0-dislike (quality with
// volume beats a single vote), and both outrank a no-vote comment.
func TestComments_SortByBest(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	a := Actor{ID: "author"}
	small := mustComment(t, rt, a, "gallery", "1", createInput{Body: "1/0"})
	big := mustComment(t, rt, a, "gallery", "1", createInput{Body: "9/1"})
	none := mustComment(t, rt, a, "gallery", "1", createInput{Body: "0/0"})

	if _, err := rt.comments.reactTx(ctx, Actor{ID: "v0"}, small.ID, 1); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 9; i++ {
		if _, err := rt.comments.reactTx(ctx, Actor{ID: "u" + string(rune('a'+i))}, big.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.comments.reactTx(ctx, Actor{ID: "hater"}, big.ID, -1); err != nil {
		t.Fatal(err)
	}

	top, err := rt.comments.list(ctx, a, "gallery", "1", "best", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top[0].ID != big.ID || top[1].ID != small.ID || top[2].ID != none.ID {
		t.Fatalf("sort=best order = %v, want [9/1, 1/0, 0/0]", commentIDs(top))
	}
}

func TestComments_SortByLikes(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	a := Actor{ID: "author"}
	c1 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "c1"})
	c2 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "c2"})
	c3 := mustComment(t, rt, a, "gallery", "1", createInput{Body: "c3"})

	// c2 gets 2 likes, c1 gets 1, c3 gets 0.
	for _, actor := range []Actor{{ID: "x1"}, {ID: "x2"}} {
		if _, err := rt.comments.reactTx(ctx, actor, c2.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rt.comments.reactTx(ctx, Actor{ID: "x1"}, c1.ID, 1); err != nil {
		t.Fatal(err)
	}

	top, err := rt.comments.list(ctx, a, "gallery", "1", "likes", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 3 || top[0].ID != c2.ID || top[1].ID != c1.ID || top[2].ID != c3.ID {
		t.Fatalf("sort=likes order = %v, want [c2 c1 c3]", commentIDs(top))
	}
}
