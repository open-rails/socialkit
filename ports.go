// Package socialkit is an embeddable Go library for content + engagement:
// posts, comments, reactions/votes, polls, and favorites over an opaque
// polymorphic entity key. It creates its own `social_`-prefixed tables inside
// the HOST application's schema (given at construction) via migratekit, shares
// the host pgx pool, and mounts its own net/http routes.
//
// socialkit is generic: everything host-specific lives behind the ports in this
// file. It imports NO sibling kit (not authkit/openrails/searchkit/storage) and
// bakes in no doujins/hentai0 assumptions — entity types are host-registered,
// access is an opaque host verdict, ids are opaque text. Any app that implements
// the mandatory ports can embed it.
package socialkit

import "context"

// Actor is the already-authenticated caller, read from context by the Identity
// port. socialkit never authenticates — the host mounts authn upstream and puts
// identity into the request context.
type Actor struct {
	ID        string // stable subject id (uuid text); empty when Anonymous
	Kind      string // opaque: "user" | "service" | "delegated" | ...
	IP        string // anon fallback key for reactions / poll votes
	Anonymous bool
}

// PublicUser is display enrichment for an author/actor id.
type PublicUser struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Avatar   string `json:"avatar,omitempty"`
}

// EntityRef is the host's verdict about a polymorphic target, returned by
// EntityResolver.Resolve. socialkit treats Accessible as opaque.
type EntityRef struct {
	Type string
	ID   string
	// Visible = published (live_at reached) AND not soft-deleted.
	Visible bool
	// Accessible = the actor may consume it. An OPAQUE host verdict computed by
	// any means (entitlement / purchase / ACL / flag). socialkit imposes no
	// access model.
	Accessible bool
}

// --- mandatory ports ---

// Identity reads the authenticated actor from context. The host's auth
// middleware populated it upstream; socialkit is a consumer of identity, never
// a producer (no token parsing, no authn).
type Identity interface {
	Actor(ctx context.Context) (Actor, bool)
}

// Authorizer answers whether an actor holds a host-supplied, opaque permission
// string. Callers are FAIL-CLOSED on error: the check is a server-side lookup
// that can fail, and an error must never be treated as "allowed".
type Authorizer interface {
	Can(ctx context.Context, actor Actor, perm string) (bool, error)
}

// EntityResolver is the one mandatory content hook and the entire content-gating
// surface. It maps an opaque (type,id) to a visibility/accessibility verdict.
//
// Report absence/gating EITHER via the sentinel errors (ErrNotFound /
// ErrNotVisible / ErrForbidden) OR via the EntityRef flags — socialkit accepts
// both: a returned error short-circuits; otherwise the Visible/Accessible flags
// are enforced per action.
type EntityResolver interface {
	Resolve(ctx context.Context, entityType, entityID string, actor Actor) (EntityRef, error)
}

// UserEnricher batch-loads display data for author/actor ids.
type UserEnricher interface {
	UsersByIDs(ctx context.Context, ids []string) (map[string]PublicUser, error)
}

// --- optional ports (nil -> documented default) ---

// Moderation screens a pending text write; return a non-nil error to reject it
// (surfaced to the caller as 422). Default: DefaultModeration (issue #9).
type Moderation interface {
	Check(ctx context.Context, in ModerationInput) error
}

// ModerationInput is the pending write handed to Moderation.Check.
type ModerationInput struct {
	Actor      Actor
	EntityType string
	EntityID   string
	Text       string
}

// Recorder emits engagement signals to a discovery system (e.g. searchkit).
// Signals are best-effort and fired after the committing write. Default: no-op.
type Recorder interface {
	Reaction(ctx context.Context, sig ReactionSignal)
	Post(ctx context.Context, sig PostSignal)
}

// ReactionSignal is a like/dislike/neutral/favorite engagement event.
type ReactionSignal struct {
	EntityType string
	EntityID   string
	ActorID    string
	// EventID uniquely identifies one real committed state transition.
	EventID string
	Kind    string // "like" | "dislike" | "neutral" | "favorite" | "unfavorite"
	Delta   int16  // authoritative signed reaction change; zero for favorites
}

// PostSignal indexes a post write for discovery (searchkit owns grouping/ranking).
type PostSignal struct {
	PostID   string
	Deleted  bool
	Title    string
	Body     string
	Language string
}

// MediaStore stores option/cover images. Default: errUnsupportedMedia.
type MediaStore interface {
	Put(ctx context.Context, key string, data []byte, contentType string) (url string, err error)
}

// ContentProcessor sanitizes rich text on write. Default: stripTags (a safe
// plain-text fallback); hosts plug in their own (doujins `blogcontent`).
type ContentProcessor interface {
	Sanitize(ctx context.Context, raw string) (string, error)
}

// Perms carries the host-supplied, opaque permission strings socialkit checks
// via Authorizer.Can before privileged writes. An empty string does NOT mean
// allowed-for-anyone: an unset gate on a privileged action fails closed (see
// runtime.requirePerm).
type Perms struct {
	PostWrite       string // create/update/delete posts (doujins/hentai0: "root:post:update")
	PollWrite       string // create/update/delete polls + options
	CommentModerate string // moderator delete of another actor's comment
}
