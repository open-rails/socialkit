<!-- socialkit FUTURE issues + design north-star — NOT active v1 work -->

> Future directions, declined scope, and the design principles that keep v1 extensible.
> Active v1 issues live in `.agents/progress.md`. IDs share ONE space across progress / future / completed.

## Design principles (apply to ALL v1 issues — this is HOW we build)

Stolen from the best-in-class survey (Discourse, Coral, Sanity, Stream, PocketBase). These keep the v1 data model clean AND ready for permission-gated UGC posts + searchkit-driven discovery:
- **Polymorphic subject + polymorphic actor**, opaque ids, no FK into host tables — one `(subject_type, subject_id)` shape for comment / reaction / vote / flag / bookmark.
- **Structured rich text, not HTML** — bodies as sanitized markdown-AST / Portable-Text JSON (mentions / links / embeds as tokens). XSS-safe; renders in-page + (future) email.
- **Soft-delete + edit history** — tombstones preserve threads; revisions table; time-boxed anon edit/delete window.
- **Threading** via `parent_id` + materialized path (ltree/text) + depth cap + denormalized `reply_count`.
- **Denormalized counters via `INSERT … ON CONFLICT` upsert**; never `COUNT(*)` on read.
- **Reactions**: stable shortcode not raw emoji; curated set per subject-type (config); single-select-vs-multi-select flag (votes = single/replace, emoji = multi/add).
- **Trust levels** — an integer reputation per actor gating rate / links / images / flag weight (Discourse): rate-limiter + spam defense + progressive permission in one. (Also the enabler for future UGC — #13.)
- **Flag → queue → consensus moderation** — flags are reactions with `type=flag`; weighted threshold auto-hides + enqueues; moderation queue is a first-class table with a pluggable classifier hook (Coral).
- **Host-schema isolation (NOT a discriminator)** — socialkit's tables live in each app's own schema (`doujins.*`/`hentai0.*`, `social_`-prefixed), so per-app content is physically isolated. doujins + hentai0 share one DB but separate schemas. (A `site` discriminator would only be needed if hosts ever shared a schema — not our case.)
- **Config-as-code** — hosts register subject types / reaction sets / policy in Go (PocketBase proves the Go form factor).
- **Real-time (if ever)** via SSE + Postgres `LISTEN/NOTIFY`, NOT WebSockets.

## THE design boundary: content in socialkit, discovery in searchkit

The vision is twitter-shaped: users create posts; a generic mechanism groups / orders / surfaces them (e.g. auto-topic grouping via vector search). Split of concerns:
- **socialkit** owns CONTENT + engagement: posts, comments, reactions/votes, polls. Writes gated by pluggable auth permissions (authkit `root:post:update` etc.). It indexes posts into searchkit and stops there.
- **searchkit** owns DISCOVERY: grouping, ordering, topic-clustering, feeds. A "community" is just one discovery lens, not a socialkit table.

So v1 keeps only the hedges expensive to retrofit (progress.md decision #7): `social_posts` naming, SPLIT up/down counts (so searchkit can rank), comment materialized path, permission-gated writes via a generic auth port. NO community/feed/ranking code in socialkit — ever.

---

# #13: (FUTURE) Wider authorship — twitter-style UGC

Not v1, and mostly NOT a socialkit code change: authorship is the post-write permission (`root:post:update`). Going UGC = grant it more widely in authkit (+ optionally a trust-level gate + author-owns-edit/delete + the flag→queue moderation path). The post store already supports it.

**Tasks:**
- [ ] Grant post-write beyond the root group (authkit config); optional trust gate.
- [ ] Author-owned edit/delete + flag→queue moderation.

---

# #14: (FUTURE, searchkit — NOT socialkit) Post discovery / grouping

Not a socialkit primitive. Grouping / ordering / surfacing posts — twitter-style topic auto-grouping via vector search, a "community" as a saved lens, "for you" — all live in **searchkit** over the posts socialkit indexes. socialkit gets NO community table. Placeholder pointing at searchkit; do the work there when wanted.

**Tasks:**
- [ ] (searchkit) Index posts; topic/vector grouping + ranking lenses over the corpus.

---

# #15: (FUTURE, searchkit) Vote-based ranking

Not v1, and it belongs in **searchkit** (it already ranks galleries/videos). socialkit stores SPLIT up/down counts and feeds them to searchkit as signals; searchkit computes hot / best (Wilson) / controversial. socialkit exposes no ranking of its own.

**Tasks:**
- [ ] (searchkit) Ranking functions over post/comment vote signals; expose as sort lenses.

---

# #16: (DECLINED) Feeds / follow-graph timelines

NOT planned (owner-decided 2026-07-02). Stream is best-in-class but SaaS + off-platform data = privacy problem for adult content. v1 serves **simple LISTS of blog posts + polls** (plain sorted queries) — all doujins/hentai0 need — which is NOT a feed. Revisit only if real social timelines are ever required.

---

# #17: (DECLINED for now) Notifications

NOT needed now (owner-decided 2026-07-02); neither app has any — leave it that way. If a concrete need ever arises: self-hosted **Novu** (keeps data in-house) or a tiny `notifykit` (fan-out-on-write + write-time aggregation keys + keyset pagination), NOT Stream. Not on the roadmap; do NOT fold into socialkit.
