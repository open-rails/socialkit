package socialkit

import (
	"context"
	"regexp"
	"strings"
	"sync"
	"time"
)

// moderationError is a rejection reason from Moderation.Check; mapped to 422.
type moderationError struct{ reason string }

func (e moderationError) Error() string { return e.reason }

// rejectModeration is the helper a Moderation implementation returns to reject.
func rejectModeration(reason string) error { return moderationError{reason: reason} }

// DefaultModeration is the zero-config policy every host gets unless it wires
// its own Moderation port: block links, reject near-instant duplicate posts
// from the same actor, and censor a tiny profanity set. Each rule is
// independently disable-able via its field.
//
// ponytail: dup-guard is an in-memory map behind one mutex with a TTL sweep —
// fine for a single process; a host running many replicas that needs global
// dedup wires its own Moderation backed by Redis.
type DefaultModeration struct {
	AllowLinks   bool          // default false: reject bodies containing URLs
	DupWindow    time.Duration // default 30s: reject identical (actor,text) within window; 0 disables
	CensorWords  []string      // default a tiny set; matched case-insensitively as whole words
	linkRe       *regexp.Regexp
	censorRe     *regexp.Regexp
	initOnce     sync.Once
	mu           sync.Mutex
	recent       map[string]time.Time // key: actor|hash(text)
	lastSweep    time.Time
	nowFn        func() time.Time // injectable clock for tests
}

var defaultCensor = []string{"childporn", "cp"}

var urlPattern = regexp.MustCompile(`(?i)\b(?:https?://|www\.)\S+`)

func (m *DefaultModeration) init() {
	m.initOnce.Do(func() {
		if m.DupWindow == 0 {
			m.DupWindow = 30 * time.Second
		}
		if m.nowFn == nil {
			m.nowFn = time.Now
		}
		m.linkRe = urlPattern
		words := m.CensorWords
		if words == nil {
			words = defaultCensor
		}
		if len(words) > 0 {
			quoted := make([]string, len(words))
			for i, w := range words {
				quoted[i] = regexp.QuoteMeta(w)
			}
			m.censorRe = regexp.MustCompile(`(?i)\b(` + strings.Join(quoted, "|") + `)\b`)
		}
		m.recent = make(map[string]time.Time)
	})
}

// Check enforces the default rules. Empty text passes (callers validate emptiness).
func (m *DefaultModeration) Check(_ context.Context, in ModerationInput) error {
	m.init()
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil
	}
	if !m.AllowLinks && m.linkRe.MatchString(text) {
		return rejectModeration("links are not allowed")
	}
	if m.censorRe != nil && m.censorRe.MatchString(text) {
		return rejectModeration("content violates policy")
	}
	if m.DupWindow > 0 {
		actor := in.Actor.ID
		if actor == "" {
			actor = in.Actor.IP
		}
		key := actor + "|" + text
		now := m.nowFn()
		m.mu.Lock()
		defer m.mu.Unlock()
		m.sweepLocked(now)
		if t, ok := m.recent[key]; ok && now.Sub(t) < m.DupWindow {
			return rejectModeration("duplicate submission, slow down")
		}
		m.recent[key] = now
	}
	return nil
}

// sweepLocked drops expired entries at most once per window to bound memory.
func (m *DefaultModeration) sweepLocked(now time.Time) {
	if now.Sub(m.lastSweep) < m.DupWindow {
		return
	}
	m.lastSweep = now
	for k, t := range m.recent {
		if now.Sub(t) >= m.DupWindow {
			delete(m.recent, k)
		}
	}
}
