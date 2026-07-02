package socialkit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// commentsEnricher is a fake UserEnricher for the enrichment assertion.
type commentsEnricher struct{}

func (commentsEnricher) UsersByIDs(_ context.Context, ids []string) (map[string]PublicUser, error) {
	out := make(map[string]PublicUser, len(ids))
	for _, id := range ids {
		out[id] = PublicUser{ID: id, Username: "name-" + id}
	}
	return out, nil
}

func commentsRuntime(t *testing.T, opts Options) *Runtime {
	res := &fakeResolver{}
	res.set("gallery", "1", true, true)
	if opts.Entities == nil {
		opts.Entities = res
	}
	if opts.EntityTypes == nil {
		opts.EntityTypes = []string{"gallery"}
	}
	rt, _ := newTestRuntime(t, opts)
	return rt
}

func mustComment(t *testing.T, rt *Runtime, actor Actor, entityType, id string, in createInput) Comment {
	t.Helper()
	cm, err := rt.comments.create(context.Background(), actor, entityType, id, in)
	if err != nil {
		t.Fatalf("create comment: %v", err)
	}
	return cm
}

func TestComments_ThreadingReplyCountAndOrder(t *testing.T) {
	rt := commentsRuntime(t, Options{Users: commentsEnricher{}})
	ctx := context.Background()
	author := Actor{ID: "author"}

	a := mustComment(t, rt, author, "gallery", "1", createInput{Body: "root A"})
	r := mustComment(t, rt, author, "gallery", "1", createInput{Body: "reply to A", ParentID: a.ID})
	_ = mustComment(t, rt, author, "gallery", "1", createInput{Body: "root B"})

	if a.Depth != 0 || r.Depth != 1 {
		t.Fatalf("depths: root=%d reply=%d, want 0 and 1", a.Depth, r.Depth)
	}
	if r.ParentID != a.ID {
		t.Fatalf("reply parent = %q, want %q", r.ParentID, a.ID)
	}

	list, err := rt.comments.list(ctx, author, "gallery", "1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Fatalf("list len = %d, want 3", len(list))
	}
	// The reply must immediately follow its parent (thread grouping).
	ai := indexOfComment(list, a.ID)
	if ai < 0 || ai+1 >= len(list) || list[ai+1].ID != r.ID {
		t.Fatalf("reply does not immediately follow parent: %+v", commentIDs(list))
	}
	// Parent's reply_count was bumped, and the author was enriched.
	if list[ai].ReplyCount != 1 {
		t.Fatalf("parent reply_count = %d, want 1", list[ai].ReplyCount)
	}
	if list[ai].Author == nil || list[ai].Author.Username != "name-author" {
		t.Fatalf("author not enriched: %+v", list[ai].Author)
	}
}

func TestComments_ExactlyOneTargetAndDepthCap(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "1", true, true)
	res.set("gallery", "2", true, true)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"gallery"}})
	ctx := context.Background()
	author := Actor{ID: "author"}

	onE1 := mustComment(t, rt, author, "gallery", "1", createInput{Body: "on entity 1"})
	// A reply on entity 2 whose parent lives on entity 1 is rejected.
	if _, err := rt.comments.create(ctx, author, "gallery", "2", createInput{Body: "cross", ParentID: onE1.ID}); err == nil {
		t.Fatal("expected rejection: parent belongs to a different entity")
	}

	// Nest to the depth cap; the level beyond it is rejected.
	parent := onE1
	for d := 1; d <= maxCommentDepth; d++ {
		child, err := rt.comments.create(ctx, author, "gallery", "1", createInput{Body: fmt.Sprintf("d%d", d), ParentID: parent.ID})
		if err != nil {
			t.Fatalf("reply at depth %d rejected early: %v", d, err)
		}
		parent = child
	}
	if _, err := rt.comments.create(ctx, author, "gallery", "1", createInput{Body: "too deep", ParentID: parent.ID}); err == nil {
		t.Fatalf("expected depth-cap rejection beyond depth %d", maxCommentDepth)
	}
}

func TestComments_AccessGating(t *testing.T) {
	res := &fakeResolver{}
	res.set("gallery", "locked", true, false)
	res.set("gallery", "hidden", false, false)
	rt, _ := newTestRuntime(t, Options{Entities: res, EntityTypes: []string{"gallery"}})
	ctx := context.Background()
	author := Actor{ID: "author"}

	if _, err := rt.comments.create(ctx, author, "gallery", "locked", createInput{Body: "x"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("premium-locked: want ErrForbidden, got %v", err)
	}
	if _, err := rt.comments.create(ctx, author, "gallery", "hidden", createInput{Body: "x"}); !errors.Is(err, ErrNotVisible) {
		t.Fatalf("hidden: want ErrNotVisible, got %v", err)
	}
	if _, err := rt.comments.create(ctx, author, "gallery", "ghost", createInput{Body: "x"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: want ErrNotFound, got %v", err)
	}
}

func TestComments_ModerationRejectsLinks(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	if _, err := rt.comments.create(context.Background(), Actor{ID: "u"}, "gallery", "1", createInput{Body: "visit https://spam.example"}); err == nil {
		t.Fatal("expected default moderation to reject a link")
	}
}

func TestComments_AnonRequiresName(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	anon := Actor{IP: "1.2.3.4", Anonymous: true}
	if _, err := rt.comments.create(ctx, anon, "gallery", "1", createInput{Body: "hi"}); err == nil {
		t.Fatal("anon without a name should be rejected")
	}
	if _, err := rt.comments.create(ctx, anon, "gallery", "1", createInput{Body: "hi", AnonName: "Guest"}); err != nil {
		t.Fatalf("named anon should be allowed: %v", err)
	}
}

func TestComments_SoftDeleteKeepsThread(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	author := Actor{ID: "author"}
	parent := mustComment(t, rt, author, "gallery", "1", createInput{Body: "parent"})
	reply := mustComment(t, rt, author, "gallery", "1", createInput{Body: "reply", ParentID: parent.ID})

	if err := rt.comments.softDelete(ctx, author, parent.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	list, err := rt.comments.list(ctx, author, "gallery", "1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2 (tombstone kept)", len(list))
	}
	pi := indexOfComment(list, parent.ID)
	if !list[pi].Deleted || list[pi].Body != commentTombstone {
		t.Fatalf("parent not tombstoned: %+v", list[pi])
	}
	if indexOfComment(list, reply.ID) < 0 {
		t.Fatal("reply disappeared after parent soft-delete")
	}
}

func TestComments_DeleteOwnerAndModerator(t *testing.T) {
	// Non-owner without the moderator permission is denied.
	denyRt, _ := newTestRuntime(t, Options{
		Entities: resolverWith("gallery", "1"), EntityTypes: []string{"gallery"},
		Authz: denyAll{}, Perms: Perms{CommentModerate: "root:comment:moderate"},
	})
	ctx := context.Background()
	c1 := mustComment(t, denyRt, Actor{ID: "author"}, "gallery", "1", createInput{Body: "mine"})
	if err := denyRt.comments.softDelete(ctx, Actor{ID: "author"}, c1.ID); err != nil {
		t.Fatalf("owner delete should succeed: %v", err)
	}
	c2 := mustComment(t, denyRt, Actor{ID: "author"}, "gallery", "1", createInput{Body: "mine2"})
	if err := denyRt.comments.softDelete(ctx, Actor{ID: "intruder"}, c2.ID); !errors.Is(err, errForbidden) {
		t.Fatalf("non-owner without perm: want forbidden, got %v", err)
	}

	// A moderator (Authz allows the perm) may delete another's comment.
	modRt, _ := newTestRuntime(t, Options{
		Entities: resolverWith("gallery", "1"), EntityTypes: []string{"gallery"},
		Authz: allowAll{}, Perms: Perms{CommentModerate: "root:comment:moderate"},
	})
	c3 := mustComment(t, modRt, Actor{ID: "author"}, "gallery", "1", createInput{Body: "theirs"})
	if err := modRt.comments.softDelete(ctx, Actor{ID: "mod"}, c3.ID); err != nil {
		t.Fatalf("moderator delete should succeed: %v", err)
	}
}

func TestComments_ReactionCountersExact(t *testing.T) {
	rt := commentsRuntime(t, Options{})
	ctx := context.Background()
	author := Actor{ID: "author"}
	cm := mustComment(t, rt, author, "gallery", "1", createInput{Body: "react to me"})
	reactor := Actor{ID: "reactor"}

	// 15 concurrent identical likes from one actor => exactly one like.
	var wg sync.WaitGroup
	for i := 0; i < 15; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = rt.comments.reactTx(context.Background(), reactor, cm.ID, 1)
		}()
	}
	wg.Wait()

	list, err := rt.comments.list(ctx, reactor, "gallery", "1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	i := indexOfComment(list, cm.ID)
	if list[i].Likes != 1 || list[i].Dislikes != 0 {
		t.Fatalf("comment counters = likes %d dislikes %d, want 1/0", list[i].Likes, list[i].Dislikes)
	}
	if list[i].Mine != 1 {
		t.Fatalf("caller's own reaction = %d, want 1", list[i].Mine)
	}
	// Switch to dislike: split counters move, no double count.
	if _, err := rt.comments.reactTx(ctx, reactor, cm.ID, -1); err != nil {
		t.Fatalf("switch reaction: %v", err)
	}
	list, _ = rt.comments.list(ctx, reactor, "gallery", "1")
	i = indexOfComment(list, cm.ID)
	if list[i].Likes != 0 || list[i].Dislikes != 1 {
		t.Fatalf("after switch: likes %d dislikes %d, want 0/1", list[i].Likes, list[i].Dislikes)
	}
}

// --- helpers ---

func resolverWith(entityType, id string) *fakeResolver {
	r := &fakeResolver{}
	r.set(entityType, id, true, true)
	return r
}

func indexOfComment(list []Comment, id string) int {
	for i := range list {
		if list[i].ID == id {
			return i
		}
	}
	return -1
}

func commentIDs(list []Comment) []string {
	ids := make([]string, len(list))
	for i := range list {
		ids[i] = list[i].ID
	}
	return ids
}
