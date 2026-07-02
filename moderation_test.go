package socialkit

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestDefaultModeration_Rules(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	m := &DefaultModeration{nowFn: func() time.Time { return now }}
	ctx := context.Background()
	in := func(actor, text string) ModerationInput {
		return ModerationInput{Actor: Actor{ID: actor}, Text: text}
	}

	if err := m.Check(ctx, in("u1", "a normal comment")); err != nil {
		t.Fatalf("clean text rejected: %v", err)
	}
	if err := m.Check(ctx, in("u1", "visit https://spam.example")); err == nil {
		t.Fatal("expected link rejection")
	}
	if err := m.Check(ctx, in("u2", "buy CP now")); err == nil {
		t.Fatal("expected censor rejection")
	}
	// duplicate within window rejected; distinct actor unaffected.
	if err := m.Check(ctx, in("u3", "hello there")); err != nil {
		t.Fatalf("first submission: %v", err)
	}
	if err := m.Check(ctx, in("u3", "hello there")); err == nil {
		t.Fatal("expected duplicate rejection")
	}
	if err := m.Check(ctx, in("u4", "hello there")); err != nil {
		t.Fatalf("different actor same text rejected: %v", err)
	}
	// past the window, the same text is allowed again.
	now = now.Add(time.Minute)
	if err := m.Check(ctx, in("u3", "hello there")); err != nil {
		t.Fatalf("post-window resubmission rejected: %v", err)
	}

	// AllowLinks opt-out.
	permissive := &DefaultModeration{AllowLinks: true, nowFn: func() time.Time { return now }}
	if err := permissive.Check(ctx, in("u5", "see http://ok.example")); err != nil {
		t.Fatalf("AllowLinks should permit links: %v", err)
	}

	var me moderationError
	if err := m.Check(ctx, in("u6", "go to www.bad.example")); !errors.As(err, &me) {
		t.Fatalf("want moderationError, got %v", err)
	}
}
