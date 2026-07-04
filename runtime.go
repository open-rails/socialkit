package socialkit

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/open-rails/migratekit"
)

// MigratekitApp is the migratekit tracker label for socialkit's migrations. A
// host running a central migrate step applies socialkit.PostgresMigrations under
// THIS label (+ WithSchema(hostSchema)) and passes Options.SkipMigrate, so the
// runtime and the migrate step agree. One label is safe across hosts that share
// a database with different schemas: migratekit v1.2.0+ folds the target schema
// into its tracker identity, so doujins.* and hentai0.* track independently.
const MigratekitApp = "socialkit"

// Options configures a Runtime. Pool, Schema, Identity, Authz and Entities are
// mandatory; the rest fall back to documented defaults.
type Options struct {
	// Pool is the host's shared pgx pool. socialkit does not own its lifecycle.
	Pool *pgxpool.Pool
	// Schema is the host schema every table + query is qualified to ("doujins").
	Schema string

	// Mandatory ports.
	Identity Identity
	Authz    Authorizer
	Entities EntityResolver

	// Optional ports (nil -> default).
	Users      UserEnricher     // default: no enrichment (ids only)
	Moderation Moderation       // default: DefaultModeration
	Recorder   Recorder         // default: no-op
	Media      MediaStore       // explicit override; usually leave nil and set Storage
	Content    ContentProcessor // default: strip tags

	// Storage configures socialkit's built-in S3-backed media store (poll/post
	// image upload to a public bucket). When set and Media is nil, socialkit owns
	// file upload itself. See StorageConfig.
	Storage *StorageConfig

	// Perms are the host-supplied, opaque permission strings gating privileged
	// writes (see Perms). An unset perm on a privileged action fails closed.
	Perms Perms

	// EntityTypes are the commentable/reactable/favoritable types the host
	// registers (e.g. "gallery", "video", "post"). Reads/writes on an
	// unregistered type return 404.
	EntityTypes []string

	// SkipMigrate skips the self-migration at construction (host runs a central
	// migrate step). Runtime still validates the schema is present.
	SkipMigrate bool

	// Logger receives socialkit's access log: each request at DEBUG, and a 500 at
	// ERROR with its cause (4xx are debug-only, not failures). nil -> slog.Default().
	Logger *slog.Logger
}

// Runtime is an embedded socialkit instance: shared deps + the module services,
// exposing one mountable http.Handler.
type Runtime struct {
	store    *store
	schema   string
	identity Identity
	authz    Authorizer
	entities EntityResolver
	users    UserEnricher
	mod      Moderation
	rec      Recorder
	media    MediaStore
	content  ContentProcessor
	perms    Perms
	log      *slog.Logger
	types    map[string]struct{}
	// mediaBase absolutizes stored relative media paths (legacy/backfilled rows)
	// against the public bucket origin; empty = serve values verbatim.
	mediaBase string

	reactions *reactions
	polls     *polls
	comments  *comments
	posts     *posts
	favorites *favorites
}

// New constructs a Runtime, self-migrates socialkit's schema (unless
// SkipMigrate), and wires the module services.
func New(ctx context.Context, opts Options) (*Runtime, error) {
	if opts.Pool == nil {
		return nil, fmt.Errorf("socialkit: Pool is required")
	}
	if opts.Schema == "" {
		return nil, fmt.Errorf("socialkit: Schema is required")
	}
	if opts.Identity == nil || opts.Authz == nil || opts.Entities == nil {
		return nil, fmt.Errorf("socialkit: Identity, Authz and Entities ports are required")
	}

	media, err := resolveMedia(opts)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{
		store:    newStore(opts.Pool, opts.Schema),
		schema:   opts.Schema,
		identity: opts.Identity,
		authz:    opts.Authz,
		entities: opts.Entities,
		users:    orDefault[UserEnricher](opts.Users, noopEnricher{}),
		mod:      orDefault[Moderation](opts.Moderation, &DefaultModeration{}),
		rec:      orDefault[Recorder](opts.Recorder, noopRecorder{}),
		media:    media,
		content:  orDefault[ContentProcessor](opts.Content, stripProcessor{}),
		perms:    opts.Perms,
		log:      orDefault[*slog.Logger](opts.Logger, slog.Default()),
		types:    make(map[string]struct{}, len(opts.EntityTypes)),
	}
	if opts.Storage != nil {
		rt.mediaBase = strings.TrimRight(opts.Storage.PublicBaseURL, "/")
	}
	for _, t := range opts.EntityTypes {
		rt.types[t] = struct{}{}
	}

	if !opts.SkipMigrate {
		if err := rt.migrate(ctx); err != nil {
			return nil, err
		}
	}

	// reactions first — comments and posts reuse its applyTx primitive.
	rt.reactions = newReactions(rt)
	rt.polls = newPolls(rt)
	rt.comments = newComments(rt)
	rt.posts = newPosts(rt)
	rt.favorites = newFavorites(rt)
	return rt, nil
}

// migrate applies socialkit's migrations into the host schema, idempotently.
func (rt *Runtime) migrate(ctx context.Context) error {
	migrations, err := migratekit.LoadFromFS(PostgresMigrations)
	if err != nil {
		return fmt.Errorf("socialkit: load migrations: %w", err)
	}
	db := stdlib.OpenDBFromPool(rt.store.pool)
	defer db.Close() // wraps the pool; does not close the host's pool
	// migratekit's WithSchema sets search_path but does not create the schema;
	// ensure it exists (no-op for hosts that already own it).
	if _, err := db.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+pgx.Identifier{rt.schema}.Sanitize()); err != nil {
		return fmt.Errorf("socialkit: ensure schema: %w", err)
	}
	if err := migratekit.NewPostgres(db, MigratekitApp).WithSchema(rt.schema).ApplyMigrations(ctx, migrations); err != nil {
		return fmt.Errorf("socialkit: apply migrations: %w", err)
	}
	return nil
}

// Handler returns the mountable http.Handler. The host mounts it under a prefix
// (e.g. "/api/social/") after its own auth middleware has populated identity
// into the request context. socialkit ships no middleware.
func (rt *Runtime) Handler() http.Handler {
	mux := http.NewServeMux()
	rt.reactions.mount(mux)
	rt.polls.mount(mux)
	rt.comments.mount(mux)
	rt.posts.mount(mux)
	rt.favorites.mount(mux)
	return rt.accessLog(mux)
}

// accessLog logs each request at DEBUG, and a 500 at ERROR with its cause. 4xx are
// expected client outcomes, logged only at debug.
func (rt *Runtime) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		start := time.Now()
		next.ServeHTTP(sw, r)
		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", sw.status,
			"duration", time.Since(start),
		}
		if sw.status >= http.StatusInternalServerError {
			if sw.internalErr != nil {
				attrs = append(attrs, "err", sw.internalErr.Error())
			}
			rt.log.Error("socialkit request failed", attrs...)
			return
		}
		rt.log.Debug("socialkit request", attrs...)
	})
}

// --- shared handler helpers used by every module ---

var entityTypeRe = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

func (rt *Runtime) isRegistered(entityType string) bool {
	_, ok := rt.types[entityType]
	return ok
}

// gate resolves a target and enforces the required access level. It returns a
// sentinel error (ErrNotFound/ErrNotVisible/ErrForbidden) that writeErr maps to
// status, or the resolver's own error. The returned ref carries the resolver's
// CANONICAL identity (e.g. "slug-123" -> "123:en"); callers MUST store/query by
// ref.Type/ref.ID, never the caller-supplied key, so aliases can't fragment
// rows. A resolver that leaves Type/ID empty keeps the caller-supplied key.
func (rt *Runtime) gate(ctx context.Context, entityType, entityID string, actor Actor, needAccessible bool) (EntityRef, error) {
	if !entityTypeRe.MatchString(entityType) || !rt.isRegistered(entityType) {
		return EntityRef{}, ErrNotFound
	}
	if entityID == "" {
		return EntityRef{}, ErrNotFound
	}
	ref, err := rt.entities.Resolve(ctx, entityType, entityID, actor)
	if err != nil {
		return EntityRef{}, err
	}
	if ref.Type == "" {
		ref.Type = entityType
	}
	if ref.ID == "" {
		ref.ID = entityID
	}
	if !ref.Visible {
		return EntityRef{}, ErrNotVisible
	}
	if needAccessible && !ref.Accessible {
		return EntityRef{}, ErrForbidden
	}
	return ref, nil
}

// canonical maps a caller-supplied key to the resolver's canonical one for
// paths that must succeed even when the target is hidden (e.g. un-wishlisting
// deleted content): a resolve failure falls back to the raw key.
func (rt *Runtime) canonical(ctx context.Context, entityType, entityID string, actor Actor) (string, string) {
	if !entityTypeRe.MatchString(entityType) || !rt.isRegistered(entityType) || entityID == "" {
		return entityType, entityID
	}
	ref, err := rt.entities.Resolve(ctx, entityType, entityID, actor)
	if err != nil || ref.ID == "" {
		return entityType, entityID
	}
	if ref.Type == "" {
		ref.Type = entityType
	}
	return ref.Type, ref.ID
}

// requirePerm is fail-closed: an unset perm, a denied check, or a check error
// all deny. authkit resolves a human's authority server-side and that can fail;
// an error must never become "allowed".
func (rt *Runtime) requirePerm(ctx context.Context, actor Actor, perm string) error {
	if perm == "" {
		return errForbidden
	}
	ok, err := rt.authz.Can(ctx, actor, perm)
	if err != nil || !ok {
		return errForbidden
	}
	return nil
}

// absMediaURL absolutizes a stored relative media path (legacy/backfilled data)
// against the public bucket origin; absolute URLs and unset values pass through.
func (rt *Runtime) absMediaURL(u string) string {
	if u == "" || rt.mediaBase == "" || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return u
	}
	return rt.mediaBase + "/" + strings.TrimLeft(u, "/")
}

// actor reads the (possibly anonymous) authenticated actor from context.
func (rt *Runtime) actor(ctx context.Context) Actor {
	a, ok := rt.identity.Actor(ctx)
	if !ok {
		return Actor{Anonymous: true}
	}
	return a
}

// requireActor demands a non-anonymous authenticated actor.
func (rt *Runtime) requireActor(ctx context.Context) (Actor, error) {
	a, ok := rt.identity.Actor(ctx)
	if !ok || a.Anonymous || a.ID == "" {
		return Actor{}, errUnauthorized
	}
	return a, nil
}

func orDefault[T comparable](v, def T) T {
	var zero T
	if v == zero {
		return def
	}
	return v
}
