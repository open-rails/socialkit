# socialkit

An embeddable Go library for **content + engagement**: posts, comments,
reactions/votes, polls, and favorites over an opaque polymorphic entity key.

Like openrails / authkit / searchkit, socialkit embeds into a host binary: it
shares the host's `*pgxpool.Pool`, creates its own `social_`-prefixed tables
**inside the host's schema** (given at construction) via migratekit, and exposes
one mountable `net/http` handler. doujins and hentai0 are its first two
consumers.

## Design

- **Generic.** Everything host-specific lives behind ports (`ports.go`).
  socialkit imports **no** sibling kit — not authkit, openrails, searchkit, or
  storage. Entity types are host-registered; access is an opaque host verdict;
  ids are opaque text. Any app that implements the ports can embed it.
- **One polymorphic key everywhere:** `(entity_type text, entity_id text)`. No
  foreign keys into host tables.
- **Content, not discovery.** socialkit owns posts/comments/reactions/polls.
  Grouping, ranking, topics, and feeds are a discovery system's job (searchkit);
  socialkit indexes posts out via the `Recorder` port and stops there. SPLIT
  up/down counts and comment materialized paths are kept so a ranker can work
  later with no recount.
- **Auth is two small ports.** `Identity.Actor(ctx)` reads an
  already-authenticated actor (the host mounts authn upstream); `Authorizer.Can`
  answers opaque permission strings and is **fail-closed** on error. socialkit
  ships no route middleware (middleware is framework-coupled).
- **Access gating via `EntityResolver`.** The host maps a `(type,id)` to a
  three-level verdict — exists / visible (published, not soft-deleted) /
  accessible (opaque; the host computes it however it likes, e.g. an entitlement
  or purchase). react/comment require *accessible*; favorite requires only
  *visible* (wishlist-friendly).

## Usage

```go
rt, err := socialkit.New(ctx, socialkit.Options{
    Pool:        pool,          // host's pgx pool
    Schema:      "doujins",     // host schema; tables land here
    Identity:    identityAdapter,
    Authz:       authzAdapter,
    Entities:    resolverAdapter,
    Recorder:    searchkitRecorder, // optional
    Content:     blogSanitizer,     // optional
    Perms:       socialkit.Perms{PostWrite: "root:post:update"},
    EntityTypes: []string{"gallery", "post"},
})
mux.Handle("/api/social/", http.StripPrefix("/api/social", rt.Handler()))
```

`New` self-migrates idempotently (or pass `SkipMigrate: true` if the host runs a
central migrate step and register `socialkit.PostgresMigrations` with migratekit).

## Status

v1 in progress. See `../open-rails-tracker/socialkit/progress.md`. Reactions + the host boundary +
schema + embed runtime + default moderation are built; comments, polls, posts,
and favorites are landing next.

## Tests

Integration tests use a Postgres testcontainer (Docker required); pure-logic
tests (moderation) run without it.

```
go test ./...
```
