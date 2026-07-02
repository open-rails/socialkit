<!-- socialkit issue tracker — ACTIVE issues -->

> One `# #<id>: <name>` section per issue, separated by `---` lines; anchor is a line starting with `# #`.
> IDs are stable for an issue's lifecycle; new issues take `next_id` and bump it.
> Tick tasks `- [x]` as they're completed; set an issue's `Completed:` to yes when all tasks done + acceptance met.

next_id: 21

## What socialkit is

A shared Go library (embedded like openrails/authkit/searchkit) that owns the generic
**content + engagement** primitives duplicated across doujins + hentai0: **posts,
comments, reactions/votes, polls, favorites**. Embedded into both binaries, it creates its tables
(`social_`-prefixed) inside the HOST application's schema (`doujins.*` / `hentai0.*`, given
at construction) via migratekit, shares the host pgx pool, mounts its own routes,
delegates identity + write-permissions to a host-provided **auth port** (authkit is the
adapter doujins/hentai0 plug in — socialkit does NOT depend on authkit). It does NOT own
discovery: grouping / ordering / topics / feeds live in **searchkit** over indexed posts.
It is a **generic** library — doujins + hentai0 are its first two consumers, but any app
that implements the ports can embed it; nothing doujins/hentai0-specific is baked in.

## Build-vs-buy (RESOLVED 2026-07-02 → BUILD)

Evaluated off-the-shelf first. No embeddable Go+Postgres library covers comments +
reactions + polls + blog with pluggable identity + a generic entity model. Every real
option is a STANDALONE SERVICE (own process, own DB, own auth, JS widget) or wrong
DB/license: Comentario (MIT, Go, PG — but a separate server + JS web-component, keyed by
page URL not an opaque entity, comments-only), Remark42 (BoltDB, own OAuth), Fider/Talkyard
(AGPL), Cusdis (Node/GPL), isso (Python), PocketBase (SQLite, owns the stack), Ponzu
(BoltDB, standalone). No embeddable reactions/polls/blog libraries exist at all (gopolls =
in-memory tally algorithms only). All four primitives are thin CRUD over Postgres keyed by
an opaque entity id + injected invoker — the same shape as our existing kits — so building
this ourselves is justified.

## Settled design decisions (from the design review)

1. **v1 scope:** posts + comments + reactions/votes + polls + favorites (content + engagement).
   Discovery (grouping/ordering/topics/feeds) is searchkit, NOT socialkit. Primary-content
   reactions (doujins gallery / hentai0 video — denorm-on-content-table + searchkit-recorder,
   heavily media/version-coupled) are DEFERRED to a later phase (#12).
2. **One opaque polymorphic key everywhere:** `(entity_type text, entity_id text)`.
   entity_id is TEXT so it fits uuid (gallery_i18n/video_i18n) and bigint (blog/video) ids.
   NO FKs into host tables. Existence + visibility come from a host-provided resolver.
3. **Reaction value is 3-state** (like / dislike / neutral) so hentai0's recommender
   "mute" signal survives; no separate delete-to-clear semantics.
4. **Posts (not "blog"):** the primitive is a generic `social_posts` table — a blog post
   is just a post whose write-permission (`root:post:update`) is held only by the root
   group. Kit owns store + CRUD + simple list/get; the HOST keeps routing / SEO / slug /
   theming; searchkit owns discovery over indexed posts.
5. **Generic, self-contained library — works for ANY host, not just doujins/hentai0.**
   Everything host-specific is behind ports (see #2): identity, entity-resolution +
   access, user-enrichment, authorization, moderation, analytics recorder, media store,
   content processor. socialkit's `go.mod` imports **NO sibling kit** — not authkit, not
   openrails, not searchkit, not storage; those are HOST-side adapters wired into the
   ports. NO doujins/hentai0 assumptions baked in: entity types are host-registered
   (not "gallery"/"video"), access is an opaque host verdict (not "openrails entitlement"),
   ids are opaque text. Any app that can implement the ports can embed it. Kit ships sane
   defaults where a port is optional. doujins + hentai0 are the first two consumers, not
   the only possible ones.
6. **Adoption order:** build kit from doujins' reference impl → adopt in doujins
   (backfill + cutover per system) → adopt in hentai0 (mostly deletes its inline code;
   gains polls it never had).
7. **socialkit owns CONTENT + engagement; DISCOVERY is searchkit's job.** socialkit =
   posts, comments, reactions/votes, polls. Grouping / ordering / topic-clustering /
   feeds — including a "community" as one lens, or twitter-style auto-grouping by topic
   via vector search — is **searchkit's** job, over the posts socialkit indexes into it
   (via the Recorder/signal port, #5/#8). So NO `community` table/column, NO feed/ranking
   code in socialkit; a "community" is just one saved discovery lens in searchkit.
   Only cheap-but-expensive-to-retrofit hedges live here (bake into #2/#3/#5/#7/#8):
   - `social_posts` (NOT `blog_posts`) — a blog post is just a post; the table NAME is
     the one real lock-in (renaming later touches every reference).
   - Votable subjects (posts + comments) keep per-direction counts SPLIT (`up`/`down`,
     i.e. likes/dislikes), NOT a net score — so searchkit can rank on them later with no
     recount. (Both apps already store split; just don't collapse it. Zero cost.)
   - Comments use a **materialized path** (ltree, depth-capped) from v1 — O(1)
     collapsible threads; justified for read perf regardless; avoids a tree-walk backfill.
   - Writes are gated by an opaque permission string via the auth port (decision #8);
     doujins/hentai0 supply their `root:...:update`, granted to the root group only.
     Wider/UGC authorship later = granting the permission more widely (+ maybe a trust gate).
   No `community_id`/`kind` columns, no feed/grouping/ranking in socialkit.
8. **Auth port — generic, authkit as one adapter (socialkit imports NO authkit)** — from the
   authkit study:
   - Two small ports: `Identity.Actor(ctx) (Actor, bool)` + `Authorizer.Can(ctx, actor,
     perm string) (bool, error)`. `Can` returns an **error** and callers are **fail-closed**
     (authkit resolves a human's authority server-side; that can fail — never treat an error
     as allow).
   - **Guarding split:** the HOST mounts authentication upstream (populates identity into
     ctx); socialkit does AUTHORIZATION in-handler via `Can`. socialkit ships NO route
     middleware — middleware is framework-coupled (authkit needs separate net/http + Gin
     adapters for that reason), so an embedded lib must not force host-side per-route wiring.
   - **Flow:** authkit middleware authenticates → puts identity (and, hentai0-style, maybe
     pre-resolved roles) in ctx; socialkit READS the actor via `Identity(ctx)` and asks
     `Can(perm)`. socialkit is a CONSUMER of identity, never a producer — it does no authn,
     no token parsing. The adapter resolves `Can` either server-side (doujins) or from
     perms the middleware pre-loaded into ctx (hentai0); socialkit is agnostic.
   - socialkit needs identity for: authz on writes; attribution (`author_id`/`user_id`/
     reaction actor); caller's own state (my-reaction, have-I-voted); moderation/ownership
     (suspended? author-owns-edit?) + `actor.IP` as the anon fallback (reactions/poll votes).
   - Perm strings are **host-supplied + opaque**, passed to `Runtime` at construction. The
     authkit adapter derives persona from the string's first segment (`<persona>:<res>:<action>`)
     and instance from ctx (root today), so future group-scoped perms (`community:posts:update`
     within group N) need NO signature change. authkit is one adapter (host-side); hentai0's
     local role→perm catalog satisfies the same `Authorizer` interface without calling authkit.

## Design north-star + future (see `.agents/future.md`)

The **design principles** that govern HOW we build (polymorphic subject/actor, structured rich text, soft-delete + history, upsert counters, trust levels, flag→queue moderation, multi-tenant, config-as-code), the **future directions** (#13–#15: UGC-by-permission + discovery-in-searchkit), and the **declined scope** (feeds #16, notifications #17) live in `.agents/future.md` — read it before building.

**Center of gravity (decision #7):** socialkit = posts + comments + votes + polls, writes gated by authkit permissions (`root:post:update` etc.); **DISCOVERY** (grouping / ordering / topics / feeds, incl. twitter-style vector grouping) is **searchkit** over posts indexed via the Recorder signal. socialkit grows no community / feed / ranking code. Baked into #2 (ports) / #3 (schema) / #5 (split counts + signal) / #7 (materialized path) / #8 (posts + permission gate + index).

**v1 read surface:** simple **LIST views for blog posts + polls** (plain sorted queries — #6, #8). NOT feeds/timelines. "We don't need much else."

---

# #1: Scaffold the module

**Completed:** yes
Status: DONE — flat root package `socialkit` (avoids the ports<->impl import cycle a `social/`+`internal/` split would force); module `github.com/open-rails/socialkit`, go 1.26.4.

Foundation: an empty, building Go module with the agreed layout + this design recorded.

**Tasks:**
- [ ] `go mod init github.com/doujins-org/socialkit`; pick the Go version doujins/hentai0 use.
- [ ] Package layout: `social/` (public API), `internal/{reactions,comments,polls,blog,store}`, `migrations/postgres/`, `ports.go`.
- [ ] Add `embed`-style `Runtime` skeleton (constructor over a host `*pgxpool.Pool`, no logic yet) mirroring openrails' embed shape.
- [ ] README with the "what/decisions" summary; `.gitignore`; empty `go build ./...` green.

Acceptance: `go build ./...` passes on an otherwise-empty module with the layout in place.

---

# #2: Host-provider ports + shared types

**Completed:** yes
Status: DONE — `ports.go` (Identity/Authorizer/EntityResolver mandatory; UserEnricher/Moderation/Recorder/MediaStore/ContentProcessor optional w/ defaults). In-memory fakes in `testsupport_test.go`.

The whole host boundary — the interfaces each app implements. Kit code depends only on these, never on host tables.

**Tasks:**
- [ ] `Actor{ID string /*subject uuid text*/; Kind string /*user|service|delegated, opaque*/; IP string; Anonymous bool}` + `PublicUser`, `EntityRef` types.
- [ ] `EntityResolver.Resolve(ctx, entityType, entityID string, actor Actor) (EntityRef, error)` — the one mandatory hook AND the entire content-gating surface. The host computes (opaque to socialkit) an access verdict over three levels: **exists**; **visible** = published (`live_at` reached) AND not soft-deleted; **accessible** = the actor may consume it — an OPAQUE host verdict; socialkit imposes NO access model and imports NO openrails. The host computes it by ANY means (for doujins/hentai0: openrails entitlements + purchase logs; another host: a flag, an ACL, whatever). Report via sentinels: `ErrNotFound`, `ErrNotVisible` (unpublished/soft-deleted), `ErrForbidden` (visible but premium-locked). socialkit applies **per-action policy** over these: comment/react require **accessible**; favorite requires only **visible** (wishlist — you can favorite premium you don't own, confirmed by the favorites eval). New access mechanisms (individual purchase, gifting, time passes) live entirely in the resolver — socialkit never changes; today premium is **account-wide** (one openrails `"premium"` entitlement), so the doujins/hentai0 adapters compute `accessible = !isPremium || actor holds "premium"`. **NB (verified):** today NEITHER app gates react/comment on entitlement (they check published-visibility only; premium is enforced only on read/stream/download) — so requiring `accessible` here CLOSES a latent gap by construction (non-premium users can currently like/comment on premium content).
- [ ] **Auth = two small ports, NO authkit import:** `Identity.Actor(ctx) (Actor, bool)` (socialkit reads an already-authenticated actor from ctx — the host mounts authentication upstream) + `Authorizer.Can(ctx, actor, perm string) (bool, error)` — **fail-closed on error** (a human JWT carries no perms; the check is a server-side lookup that can fail, so a bare bool would swallow an outage into "allowed"). Perm strings are **host-supplied + opaque** (host gives socialkit its gated-action perms at construction); the authkit adapter derives persona from the string's first segment + instance from ctx, so future group-scoped perms need no signature change. authkit is ONE adapter (host-side); hentai0's local role→perm catalog satisfies the same interface.
- [ ] `UserEnricher.UsersByIDs(...)` for username/avatar display enrichment.
- [ ] Optional ports: `Moderation.Check`, `Recorder{Reaction,View}`, `MediaStore`, `ContentProcessor.Sanitize` — each with a documented no-op/default.
- [ ] Entity-type registration API (host declares its commentable/reactable types).

Acceptance: ports compile with godoc on each; a trivial in-memory fake implements them for tests.

---

# #3: schema (tables in the host schema) + migratekit migrations

**Completed:** yes
Status: DONE — `migrations/postgres/001_social_core.up.sql` (all tables in one migration), applied via migratekit `WithSchema(host)`. Comment threading uses a dot-joined **text** path (no ltree extension dep — portability). Verified idempotent on a testcontainer.

The kit-owned schema; polymorphic key throughout; counters kit-maintained.

**Tasks:**
- [ ] `social_reactions(entity_type, entity_id, user_id, ip, value smallint /*-1,0,1*/, ...)` with partial-unique on (entity,user) and (entity,ip).
- [ ] `social_comments(id uuid, entity_type, entity_id, parent_id, path /*materialized path, ltree, depth-capped*/, user_id, anon_name, text, likes, dislikes /*SPLIT up/down for ranking*/, deleted_at, ...)` + exactly-one-actor CHECK + threading self-FK.
- [ ] `social_poll_questions / social_poll_options / social_poll_votes` (anon IP voting, unique per (poll,user)/(poll,ip), vote_count counter).
- [ ] `social_posts(id, author_id, title, content, language, is_draft, live_at, total_likes, total_dislikes /*SPLIT*/, deleted_at)` — NOT `blog_posts` (the name is the lock-in). No `community_id`/`kind` — cheap metadata-only `ALTER` later if ever.
- [ ] `social_favorites(user_id, entity_type, entity_id, created_at)` — PK `(user_id, entity_type, entity_id)`, user-only (no anon). Simplification of doujins' `user_entity_reactions` shape; separate from `social_reactions` (favorites = unsigned presence, reactions = signed ±1 anon-capable). No denormalized count in the table — expose `Count()`; host decides denormalization.
- [ ] migratekit source (tracking key `socialkit`), idempotent + lock/statement timeouts, per the doujins SQL migration conventions.
- [ ] **Schema placement:** tables live INSIDE the host's schema (`doujins.*`/`hentai0.*`), `social_`-prefixed and given the schema name at construction — NOT a dedicated `social.*` schema. Rationale: doujins + hentai0 share ONE database but separate schemas, so per-app content is physically isolated by schema (no `site` discriminator, no cross-app id collision). This differs from openrails/authkit, which own shared schemas because their data IS shared (one merchant, one user pool); socialkit content is per-app. socialkit still owns its DDL via its own migratekit source (tracking key `socialkit`) targeting the host schema. NO FKs into host content tables (opaque `(entity_type,entity_id)` key).

Acceptance: migrations apply cleanly on a fresh Postgres (testcontainer) and are idempotent on re-run.

---

# #4: Embed runtime + route mounting + entity registration

**Completed:** yes
Status: DONE — `runtime.go`: `New(ctx, Options)` self-migrates (idempotent, `SkipMigrate` escape hatch) + ensures the host schema exists; `Handler()` returns a stdlib `*http.ServeMux`; shared `gate()`/`requirePerm()` (fail-closed)/`actor()` helpers. Denied-Can → 403 asserted in the posts tests (#8).

Wire the kit into a host, mirroring the openrails embed surface.

**Tasks:**
- [ ] `Runtime` construction: takes host pgx pool + **the target schema name** (`doujins`/`hentai0` — every query + migration is schema-qualified to it) + the port implementations + the host-supplied gated-action perm strings + registered entity types.
- [ ] Mountable HTTP handler (host router mounts it, e.g. `/api/social/v1/...` or inline). **Guarding split:** the HOST mounts authentication upstream (populates identity into ctx); socialkit reads it via the `Identity` port and does AUTHORIZATION in-handler via `Authorizer.Can` — socialkit ships NO per-route middleware (middleware is framework-coupled; authkit itself needs a separate net/http vs Gin adapter for exactly this reason) and does NOT reimplement authn.
- [ ] Schema-version validation at init (like openrails: refuse to run against an unmigrated schema).
- [ ] Integration test: construct the runtime against a testcontainer + fake ports (fake Identity/Authorizer); assert routes respond + a denied `Can` yields 403.

Acceptance: a test host can embed the runtime, mount routes, and serve a health/no-op endpoint; auth is entirely port-driven (no authkit import).

---

# #5: Reactions system

**Completed:** yes
Status: DONE — `reactions.go`. 3-state upsert via SELECT FOR UPDATE + `INSERT ... ON CONFLICT DO NOTHING` (a bare INSERT's 23505 aborts the tx/25P02 — the losing racer must block+no-op instead). `applyTx` is the shared in-tx primitive comments/posts reuse to bump their SPLIT counter. Counts-on-read for generic entities. Integration-tested: transitions, concurrent double-like exactness, gating (locked/hidden/missing), anon-by-IP, recorder signal, HTTP route.

Generic reactions over the polymorphic key — the most-duplicated, cleanest-to-extract system. Build first.

**Tasks:**
- [ ] Reaction store: upsert/clear with `SELECT ... FOR UPDATE` + 23505 idempotency guard; 3-state value; denorm the SPLIT per-direction counts (up/down) on the subject in one tx — NOT just net (port the doujins #737/#183 pattern — fix lives here once). Keep the write-path seam clean so a future trust-weighted vote / trust-gated action slots in without touching it (no v1 build).
- [ ] Generic `Reactor`/`Handler{resources}` HTTP surface (adopt doujins' abstraction): `POST /<type>/:id/like|dislike`, `DELETE|neutral /<type>/:id/reaction`, `GET /<type>/:id/reaction`.
- [ ] Call `EntityResolver` before write and require **accessible** (reject `ErrNotFound`/`ErrNotVisible`/`ErrForbidden` — no reacting on deleted, unpublished, or premium-locked content); call `Recorder.Reaction` after (searchkit signal).
- [ ] Unit + integration tests incl. the concurrent double-like / like↔dislike-switch race, and reject-on-premium-locked.

Acceptance: reactions on any registered entity type; blocked on inaccessible targets; counters exact under concurrency; searchkit signal emitted.

---

# #6: Polls system

**Completed:** yes
Status: DONE — `polls.go`. Admin CRUD gated by `PollWrite`; public list (by language) + get + vote. Anon-by-IP, dup-guard via partial-unique + `INSERT ... ON CONFLICT DO NOTHING` (increment `vote_count` only when RowsAffected==1). Integration-tested incl. concurrent duplicate-vote exactness, per-language slicing, admin gate.

Standalone site-wide polls (doujins-only today → hentai0 gains it free). Lowest coupling.

**Tasks:**
- [ ] Poll question/option CRUD (admin, gated via `Authorizer`) + option images via `MediaStore`.
- [ ] Public list/active + vote; anon IP voting with dup-guard (unique indexes + has-voted check); `vote_count` maintenance.
- [ ] `language` slicing; API read/vote handlers.
- [ ] Tests: duplicate-vote race → clean 409, anon vs authed, per-language.

Acceptance: full poll lifecycle (create → vote → tally) works; no double-count under retry.

---

# #7: Comments system

**Completed:** yes
Status: DONE — `comments.go`. YouTube-style two-level threading: `list` returns TOP-LEVEL comments (newest-first, paginated) with `reply_count`; replies (one level deep) fetched lazily via `GET /comments/{cid}/replies`. No full-tree materialization (dropped the materialized path/depth — simpler, matches doujins). A reply must target a top-level comment on the same entity (reply-to-a-reply rejected; client re-parents to top-level via @mention). SPLIT counters via shared `reactions.applyTx`, soft-delete tombstones, create gates ACCESSIBLE + moderates + sanitizes, list gates VISIBLE + batch author enrichment + caller's reaction. Integration-tested: top-level/replies/reply_count, reply constraints, gating, moderation, anon, soft-delete-keeps-thread, owner-vs-moderator delete, concurrent reaction exactness. NB: socialkit stays opaque — the HOST adapter decides what an "item" is (e.g. doujins keys galleries on gallery-id+language, collapsing versions).

Threaded comments keyed on the polymorphic entity; the biggest rewrite for hentai0 (it has no comment service today).

**Tasks:**
- [ ] Comment store/service: create/update/soft-delete, `parent_id` + materialized-path (ltree, depth-capped) threading with same-entity parent check, reply-count on read, denorm SPLIT like/dislike counters (for future best/controversial sort).
- [ ] Create calls `EntityResolver` and requires **accessible** (reject deleted/unpublished/premium-locked — replaces per-app gallery-i18n / video-HLS SQL) + `Moderation.Check`. Feed hides comments whose target is no longer visible.
- [ ] User enrichment via `UserEnricher`; moderator delete via `Authorizer`.
- [ ] Feed + per-entity list endpoints; caller's own `user_reaction` included (batched).
- [ ] Tests: threading, exactly-one-target, access gating (deleted/unpublished/premium), moderation.

Acceptance: comment CRUD + threaded feed on any registered entity; commenting on deleted/unpublished/inaccessible targets rejected once, in the kit.

---

# #8: Posts (blog) system

**Completed:** yes
Status: DONE — `posts.go`. CRUD gated by `PostWrite` (fail-closed — an authz error is 403, verified); sanitize on write; published-only list (sorted, paginated, comment_count computed) + drafts visible only to permission holders; post like/dislike via shared `reactions.applyTx` bumping `total_likes/total_dislikes`; emits `Recorder.Post` for searchkit indexing. This also satisfies #4's denied-Can→403 acceptance.

The generic `social_posts` primitive — authored content, write-gated by an authkit permission. Kit owns store + CRUD + simple list/get; **discovery (grouping/ordering/topics/feeds) is searchkit** over posts indexed via the Recorder signal. Host keeps routing/theming.

**Tasks:**
- [ ] `social_posts` store/service: CRUD, `language`, `is_draft`, `live_at` scheduled publish, soft-delete, split like/dislike counters (via #5 reactions on `("post", id)`).
- [ ] Create/update/delete gated in-handler by `Authorizer.Can(ctx, actor, cfg.PostWritePerm)` (fail-closed; both apps supply their `root:...:update` string, granted to the root group only). Author-owns applies once the perm is granted more widely.
- [ ] `ContentProcessor.Sanitize` on write (host supplies doujins `blogcontent`; kit default strips); cover/media via `MediaStore`.
- [ ] Index posts into searchkit via `Recorder` (so discovery/grouping/ranking is searchkit's, not socialkit's); `comment_count` computed on read.
- [ ] Read API — simple list/get (plain sorted queries, NOT a feed; feeds/topics/trending are searchkit reads the host wires).
- [ ] Tests with fake ContentProcessor/MediaStore/Recorder.

Acceptance: post CRUD + simple list/get via the kit; writes permission-gated; posts indexed into searchkit for discovery; no feed/grouping code in socialkit.

---

# #9: Default moderation policy

**Completed:** yes
Status: DONE — `moderation.go` `DefaultModeration`: link block (overridable), duplicate-submission reject (in-memory TTL, one mutex — host swaps in Redis-backed for multi-replica), tiny censor set. Each rule individually overridable. Unit-tested (no container).

Ship a sane `Moderation` default so hosts get protection without wiring (doujins #722 reduced this to hardcoded rules — ideal library default).

**Tasks:**
- [ ] Default policy: URL/link block-list allowance, duplicate-submission rejection (short TTL), tiny censor list, per-actor rate-limit hooks.
- [ ] Make each rule individually overridable; document how a host swaps in its own.

Acceptance: a host with zero moderation config still gets link-block + dup-reject + censor on comments.

---

# #18: Favorites system

**Completed:** yes
Status: DONE — `favorites.go`. User-only bookmark; add gates VISIBLE-only (wishlist premium-you-don't-own — the key distinction from reactions); idempotent add/remove; batch `IsFavorited`/`Count`/`CountsByEntity` (no denorm column imposed); emits `Recorder.Reaction` kind favorite/unfavorite. Integration-tested incl. the wishlist-a-locked-entity case and anon-rejection.

Extraction confirmed (favorites eval): both apps have near-identical favorites — per-entity FK, user-only, plain bookmark, ZERO entitlement coupling (favoriting premium-you-don't-own is a wishlist, works today). Separate table from reactions; emitted as a reaction-KIND signal to searchkit.

**Tasks:**
- [ ] `social_favorites` store/service over `(actor, entity_type, entity_id)`: add / remove / list (paginated, `created_at DESC`) / is-favorited (batch) / `Count()` + `CountsByEntity()`.
- [ ] Gate on `EntityResolver`: favorite requires **visible** only (NOT accessible) — you can wishlist premium content you don't own; reject deleted/unpublished.
- [ ] Emit `Recorder.Reaction(kind="favorite"/"unfavorite")` to searchkit (hentai0 already does this).
- [ ] HTTP: `GET /favorites`, `POST /<type>/:id/favorite`, `DELETE /<type>/:id/favorite`, `GET /<type>/:id/favorite` (status).
- [ ] Tests: add/remove idempotency, batch is-favorited, wishlist-a-premium-entity allowed, favorite-deleted-entity rejected.

Acceptance: favorites on any registered entity type; wishlist-friendly (visible-not-accessible); searchkit signal emitted; no denormalized count imposed on the host.

---

# #10: Adopt socialkit in doujins

**Completed:** no
Status: PLANNED

Doujins is the reference/lead. Per system: backfill → cut over reads/writes → DELETE the old code → (separate, post-verification migration) drop old tables. Ship system-by-system. Each cutover = green build/vet/test + e2e before the drop. (All `social_*` backfill targets below are socialkit tables IN the `doujins.*` schema.)

**Ports (do first):**
- [ ] Implement the doujins port set: `EntityResolver` over galleries/gallery_i18n/tags/artists/series/characters/blog — its `accessible` verdict = the existing premium gate `!gallery.IsPremium || userCtx.HasEntitlement("premium")` (openrails entitlement + purchase logs; reuse `GalleryPermissionService`/`validateGalleryAccess`); `Identity`+`Authorizer` adapters over authkit (`verify.ClaimsFromContext` + `verify.Allow`/`Can`); `Recorder` over searchkit; `ContentProcessor` over `internal/blogcontent`; `MediaStore` over `storage`; `UserEnricher` over `UsersRepo`→authkit. (These adapters live in doujins and call openrails/authkit/searchkit — socialkit itself imports none of them.)

**Reactions:**
- [ ] Backfill `doujins.{comment,blog_post,user_entity}_reactions` → `social_reactions`; cut reads/writes to the kit.
- [ ] DELETE: `internal/services/entity_reaction/`; `internal/database/repo/entity_reactions.go`; `internal/api/reaction/adapters.go` (entity/blogPost/comment reactors) + the reaction wiring in `internal/api/handlers/registry.go`; `BlogPostService.AddReaction` + `BlogPostRepo.UpsertReaction` + `applyBlogReactionDeltas` (`blog_post.go`); `CommentRepo.UpsertCommentReaction`/`RemoveCommentReaction` + `applyCommentReactionDeltas` (`comment.go`).
- [ ] Drop tables `comment_reactions`, `blog_post_reactions`, `user_entity_reactions`. **Do NOT touch `gallery_reactions` / the gallery-service reaction path — deferred to #12.** Decide: keep the `reaction.Reactor`/`Handler` HTTP shell fronting socialkit, or replace with the kit's.

**Polls:**
- [ ] Backfill `doujins.poll_*` → `social_poll_*`; cut over.
- [ ] DELETE: `internal/services/poll/`; `internal/database/repo/question.go`; `internal/database/models/poll.go`; `internal/api/polls/`; `internal/api/admin/poll/`; `internal/utils/pollutil` (if unused elsewhere); poll routes in `internal/server/routes/content.go`.
- [ ] Drop tables `poll_questions`, `poll_options`, `poll_votes`.

**Comments:**
- [ ] Backfill `doujins.comments` (map `gallery_i18n_id`→`("gallery",uuid)`, `blog_post_id`→`("blog_post",id)`) → `social_comments`; cut over.
- [ ] DELETE: `internal/services/comment/`; `internal/database/repo/comment.go`; `internal/database/models/comment.go`; `internal/api/comment/`; `internal/api/admin/comment/`; comment routes in `content.go`; `internal/services/moderation` (its rules become socialkit's default #9 — verify no other caller first).
- [ ] Drop table `comments`.

**Blog (posts):**
- [ ] Backfill `doujins.blog_posts` → `social_posts`; cut over.
- [ ] DELETE the store/CRUD that socialkit replaces: `internal/services/blog_post/` (CRUD parts; the HTML/cover logic survives only as the `ContentProcessor`/`MediaStore` adapters), `internal/database/repo/blog_post.go`, model, `internal/api/blog_post/` read handlers, `internal/api/admin/blog_post/` CRUD. Keep doujins routing/theming as a thin layer over the kit; view counts stay via the searchkit reader.
- [ ] Drop table `blog_posts`.

**Favorites:**
- [ ] Backfill `doujins.gallery_favorites` → `social_favorites` (entity_type `"gallery"`); cut over.
- [ ] DELETE: `internal/services/gallery/favorites.go`, `internal/database/repo/gallery_favorites.go`, `models.FavoriteGallery`, the favorites routes in `content.go`.
- [ ] Drop table `gallery_favorites` + the `trg_gallery_favorite_count` trigger + `galleries.favorites_count` column — read the count from socialkit's `Count()` (or keep a host trigger fed by `social_favorites` if a denormalized column is still wanted).

**Cutover-wide:**
- [ ] Frontend: reconcile callers to socialkit's API shape — either mount the kit at doujins' existing route paths with matching response shapes, or update the frontend. (This is where API-contract drift bites; do it per system.)
- [ ] Verify build/vet/test + affected e2e per system BEFORE each table drop; drop tables in a separate follow-up migration so cutover is rollback-able.

Acceptance: doujins comments/reactions/polls/blog/favorites run entirely on socialkit; the listed code + tables removed; gallery reactions untouched (#12); green.

---

# #11: Adopt socialkit in hentai0

**Completed:** no
Status: PLANNED

Mostly deletion of hentai0's inline code + implementing the ports; hentai0 GAINS polls (net new). Same sequence as #10 (backfill → cut over → delete → separate drop migration). (All `social_*` backfill targets below are socialkit tables IN the `hentai0.*` schema.)

**Ports (do first):**
- [ ] Implement the hentai0 port set: `EntityResolver` over videos/video_i18n/tags/creators/franchises/characters/blog — the video→i18n + HLS-visibility chain (`resolveVideoI18nIDForComments`, `ensurePublicVideoVersionVisible`) moves BEHIND the resolver, and its `accessible` verdict = `!video.IsPremium || currentUserHasPremiumAccess` (openrails `"premium"` entitlement, `auth_helpers.go:131`); `Identity`/`Authorizer` over hentai0's authkit provider (local role→perm catalog, `currentUserHasPermission`); `Recorder` over searchkit; `ContentProcessor`/`MediaStore`/`UserEnricher` as in #10. (Adapters live in hentai0 and call openrails/authkit/searchkit; socialkit imports none.)

**Reactions:**
- [ ] Backfill `hentai0.{comment,blog_post,user_entity}_reactions` → `social_reactions`; cut over.
- [ ] DELETE: `PostgresCommentReactionRepository`, `PostgresPostReactionRepository`, `PostgresUserEntityReactionRepository` (`internal/repository/reaction.go`); `CommentReactionService`, `PostReactionService`, `UserEntityReactionService` (`internal/services/reaction.go`); post/entity handlers in `internal/api/reactions.go` + comment like/dislike in `internal/api/comments.go`.
- [ ] Drop tables `comment_reactions`, `blog_post_reactions`, `user_entity_reactions`. **Do NOT touch `video_reactions` / `VideoReactionService` / the `repository/video.go` reaction path — deferred to #12.**

**Comments:**
- [ ] Backfill `hentai0.comments` (map `video_i18n_id`→`("video",uuid)`, `blog_post_id`→`("blog_post",id)`) → `social_comments`; cut over.
- [ ] DELETE the inline CRUD in `internal/api/comments.go` (create/update/delete/getVideoComments/listCommentFeed/getLatestComments/feed handlers + `commentListItem`/`commentFeedResponse` DTOs); `getPostComments` in `internal/api/posts.go`; admin moderation `internal/api/admin_comments.go`; `internal/domain/comment_model.go`.
- [ ] Drop table `comments`.

**Blog (posts):**
- [ ] Backfill `hentai0.blog_posts` → `social_posts`; cut over.
- [ ] DELETE: `internal/services/blog_posts.go` (CRUD/read parts); blog read handlers in `internal/api/posts.go`; admin CRUD in `internal/api/admin.go` (`createAdminPost`/`updateAdminPost`/`deleteAdminPost`/`uploadPostThumbnail`). Keep routing/theming as a thin host layer; view counts via searchkit.
- [ ] Drop table `blog_posts`.

**Polls (NEW capability — no removal):**
- [ ] Enable the kit's poll surface in hentai0 + minimal admin + frontend (hentai0 never had polls).

**Favorites:**
- [ ] Backfill `hentai0.favorites` → `social_favorites` (entity_type `"video"`); cut over.
- [ ] DELETE: `internal/repository/favorite.go`, `internal/services/favorite.go`, `internal/api/favorites.go`, `internal/domain/favorite.go`, favorites routes. (hentai0 already emits `recorder.Reaction(kind="favorite")` — socialkit's favorites takes that over.)
- [ ] Drop table `favorites`.

**Cutover-wide:**
- [ ] Frontend: reconcile callers to socialkit's API shape (pnpm build); mount the kit at existing paths or update the frontend, per system.
- [ ] Verify build/vet/test + pnpm build BEFORE each table drop; drop tables in a separate follow-up migration.

Acceptance: hentai0 comments/reactions/blog/favorites run on socialkit; inline comment code + the 3 duplicate reaction repos/services removed; polls available; video reactions untouched (#12); green.

---

# #12: (DEFERRED) Move primary-content reactions into socialkit

**Completed:** no
Status: DEFERRED — do after v1 is proven on comments/reactions/polls/blog in both apps.

The gallery (doujins) / video (hentai0) reaction is the most host-coupled: denorm counter on the content table, version/i18n resolution, searchkit recorder, DB-trigger (doujins) vs Go path in `repository/video.go` (hentai0). Bring it into the kit as `("gallery"/"video", id)` reactions once the boundary is battle-tested.

**Tasks:**
- [ ] Represent gallery/video reactions as socialkit reactions on `("gallery"|"video", id)`.
- [ ] Replace the doujins `gallery_reactions` trigger + hentai0 `videos.likes_count` path; host reads counts from the kit; keep the searchkit recorder via `Recorder`.
- [ ] Backfill + cutover + drop the old per-content reaction tables/columns in both apps.

Acceptance: all reactions (incl. primary content) run through socialkit; no bespoke per-content reaction paths remain.

---

# #19: Adapt doujins + hentai0 `migrate legacy` to write into socialkit tables

**Completed:** no
Status: TODO (investigate first) — pairs with #10 (doujins) and #11 (hentai0) per-system cutovers.

BOTH apps have a `migrate legacy` importer that pulls legacy MySQL data into Postgres, and both write engagement into the OLD tables — so once socialkit owns those, both importers must be re-targeted to `social_*` (each in its own host schema), using the SAME opaque-item mapping each system's cutover settles. This is separate from the one-time backfill of already-migrated prod rows: the importer is ongoing/re-runnable, so it must target `social_*` going forward or a re-run repopulates dead tables.

**doujins (`internal/legacy_migrate/`):**
- [ ] Inventory every handler/engine touching comments / reactions / favorites / polls / blog (engine registry + `handlers/user_engagement.go` & friends), and the exact old-table writes + id shapes (how it derives `gallery_i18n_id` / `blog_post_id` / gallery id).
- [ ] Re-target each to the matching `social_*` table via socialkit's opaque `(entity_type, entity_id)`, reusing the doujins adapter's id-mapping so importer + live writes agree.

**hentai0 (its own `migrate legacy`):**
- [ ] Inventory hentai0's importer handlers touching video comments / reactions / favorites / blog (its engagement path), + id shapes (video → `video_i18n` uuid, `blog_post_id`, etc.).
- [ ] Re-target to `social_*` in the `hentai0` schema via the hentai0 adapter's id-mapping. NB [[hentai0-recreates-openrails-river]]: watch for hardcoded schema/migrate quirks.

**Both:**
- [ ] Reconcile with the per-system backfill (#10/#11): backfill existing rows + re-point the importer in the SAME cutover — one source of truth, no drift.
- [ ] Verify a `migrate legacy --new-only` re-run populates `social_*` (not the dropped/old tables), is idempotent, and the migration_* observability tables still report progress.
- [ ] Order it so the importer change lands WITH (not before) each system's table drop, else a re-run recreates a dropped table.

Acceptance: both importers write comments/reactions/favorites/polls/blog into `social_*` with the correct per-system, per-schema entity_id mapping; no writes to retired tables; idempotent re-runs; landed per-system alongside #10/#11.

---

# #20: Per-entity aggregate counts + count-based sorting

**Completed:** no
Status: TODO — important for doujins (sort galleries/videos by likes/favorites/comments; sort posts/comments by best). Decisions settled with the owner.

**Problem:** today socialkit denormalizes like/dislike counts on the owning row (`social_comments.likes/dislikes`, `social_posts.total_*`) transactionally + exact, and `reply_count` on the comment — BUT there is NO per-*entity* aggregate, so "likes/favorites/comment_count on gallery X" is computed on read via COUNT/GROUP BY. That is fine for one item's detail page but cannot efficiently **sort many items** by those counts, and there's no index to rank on. Two known miscounts to fix too: `reply_count` isn't decremented on reply soft-delete (drifts high); generic-entity reaction counts are on-read GROUP BY.

**Owner decisions:**
- **Transactional upkeep** (not async): upsert the rollup in the SAME tx as each reaction/favorite/comment write — exact + always fresh + no worker (matches doujins' current triggers). Build it behind a seam so a specific hot counter can move to a dirty-set + background recompute later; source tables stay authoritative so a one-shot recompute can always rebuild.
- **socialkit sorts its own lists too**: posts + comments list endpoints accept `sort=newest|likes|best|comments` ordering off the rollup; host-entity lists (galleries/videos) sort host-side by JOINing the rollup.

**Design:**
- New table `social_entity_counts(entity_type, entity_id, likes, dislikes, favorites, comment_count, updated_at, PK(entity_type,entity_id))`, indexed for sort: `(entity_type, likes DESC)`, `(entity_type, favorites DESC)`, `(entity_type, comment_count DESC)`, and a "best" ranking — a Wilson lower-bound (or `likes::float/NULLIF(likes+dislikes,0)`) expression index so a 900/1000 outranks a 1/1.
- Maintain in-tx: `reactions.applyTx` upserts likes/dislikes deltas for ANY reacted entity (uniform across gallery/video/comment/post); `favorites` add/remove upserts favorites ±1; top-level `comments` create/soft-delete upserts comment_count ±1. Fix: `reply_count` decrements on reply soft-delete.
- Reads switch to O(1) rollup: generic reaction counts, `favorites.Count/CountsByEntity`, and a public `Counts(entityType, entityID)` / `CountsByEntity(entityType, ids)` API so hosts read + sort gallery/video counts.
- Keep the owning-row like/dislike columns for now (additive, lower risk) OR unify to the rollup as the single denorm source — decide during impl (unify preferred if the read-path refactor stays clean).

**Tasks:**
- [ ] `social_entity_counts` table + sort indexes (incl. the best/Wilson expression index).
- [ ] In-tx maintenance across reactions / favorites / comments; `reply_count`-on-delete fix.
- [ ] Rollup-backed reads + public `Counts`/`CountsByEntity`.
- [ ] `sort=` on posts + comments lists (newest|likes|best|comments).
- [ ] Integration tests: exact under concurrency, sort correctness (incl. like-percentage), reply_count decrement, self-heal recompute.

Acceptance: any item's likes/dislikes/favorites/comment_count read O(1) and sort efficiently (incl. like-ratio); counts stay exact under concurrency; `reply_count` never drifts; a recompute-from-source can rebuild the rollup.

---

# #21: Inline post media upload

**Completed:** no
Status: TODO (small) — needed for blog: the editor uploads an image mid-article and gets a URL to embed in the Quill Delta body.

socialkit's media store already does poll option images + post covers; add a general `POST /posts/media` (PostWrite-gated, multipart) that uploads to the media store under `posts/media/{uuid}.{ext}` and returns `{url}`. The editor drops that URL into the Delta, so inline images carry their final socialkit-bucket URL at write time (kills doujins' read-time image-URL rewrite for new posts).

**Tasks:**
- [ ] `POST /posts/media` handler + route; test with the fake media store + perm gate.

Acceptance: an authorized editor uploads an inline image and gets a stable public URL; unauthorized → 403.
