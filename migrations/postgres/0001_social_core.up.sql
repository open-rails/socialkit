-- socialkit core schema (single migration): content + engagement primitives.
--
-- Tables are created UNQUALIFIED here; migratekit applies them into the host's
-- schema via NewPostgres(db,"socialkit").WithSchema(<hostSchema>). doujins and
-- hentai0 share one database but separate schemas, so per-app content is
-- physically isolated by schema (no `site` discriminator). NO foreign keys
-- reach into host content tables — the target is the opaque (entity_type,
-- entity_id) polymorphic key everywhere.

-- ----------------------------------------------------------------------------
-- Reactions: 3-state (-1 dislike / 0 neutral / 1 like) over any entity.
-- One per (entity,user) and one per (entity,anon-ip). 3-state so a recommender
-- "mute"/neutral signal survives without delete-to-clear semantics.
-- ----------------------------------------------------------------------------
CREATE TABLE social_reactions (
    entity_type text        NOT NULL,
    entity_id   text        NOT NULL,
    user_id     text,               -- NULL for anonymous
    ip          text,               -- anonymous key
    value       smallint    NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT social_reactions_value_ck CHECK (value IN (-1, 0, 1)),
    CONSTRAINT social_reactions_actor_ck CHECK (user_id IS NOT NULL OR ip IS NOT NULL)
);
CREATE UNIQUE INDEX social_reactions_user_uq
    ON social_reactions (entity_type, entity_id, user_id)
    WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX social_reactions_ip_uq
    ON social_reactions (entity_type, entity_id, ip)
    WHERE user_id IS NULL AND ip IS NOT NULL;
CREATE INDEX social_reactions_entity_idx
    ON social_reactions (entity_type, entity_id);

-- ----------------------------------------------------------------------------
-- Comments: YouTube-style two-level threading via an adjacency `parent_id`.
-- Top-level comments carry a denormalized `reply_count`; replies are ONE level
-- deep and fetched lazily per parent (no full-tree materialization). SPLIT
-- like/dislike counters (not a net score) so a discovery layer can rank later
-- with no recount. Soft-delete tombstones a row so a thread stays navigable.
-- ----------------------------------------------------------------------------
CREATE TABLE social_comments (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    entity_type text        NOT NULL,
    entity_id   text        NOT NULL,
    parent_id   uuid        REFERENCES social_comments (id) ON DELETE CASCADE,
    user_id     text,
    anon_name   text,
    body        text        NOT NULL,
    likes       int         NOT NULL DEFAULT 0,
    dislikes    int         NOT NULL DEFAULT 0,
    reply_count int         NOT NULL DEFAULT 0,   -- top-level only; replies are one level deep
    deleted_at  timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT social_comments_actor_ck CHECK (user_id IS NOT NULL OR anon_name IS NOT NULL)
);
-- Top-level list per entity, newest-first.
CREATE INDEX social_comments_toplevel_idx
    ON social_comments (entity_type, entity_id, created_at DESC) WHERE parent_id IS NULL;
-- A parent's replies, oldest-first (lazy reply fetch).
CREATE INDEX social_comments_parent_idx
    ON social_comments (parent_id, created_at) WHERE parent_id IS NOT NULL;

-- ----------------------------------------------------------------------------
-- Polls: site-wide questions with options; anon-capable voting, one vote per
-- (poll,user) and per (poll,ip); vote_count denormalized on the option.
-- ----------------------------------------------------------------------------
CREATE TABLE social_poll_questions (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    question   text        NOT NULL,
    language   text        NOT NULL DEFAULT '',
    is_active  boolean     NOT NULL DEFAULT true,
    image_url  text,
    live_at    timestamptz NOT NULL DEFAULT now(),  -- scheduled publish; public reads gate on live_at <= now()
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz
);
-- Live/active selection + month-archive windows, newest-live-first.
CREATE INDEX social_poll_questions_live_idx
    ON social_poll_questions (language, live_at DESC) WHERE deleted_at IS NULL AND is_active;

CREATE TABLE social_poll_options (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    question_id uuid NOT NULL REFERENCES social_poll_questions (id) ON DELETE CASCADE,
    label       text NOT NULL,
    image_url   text,
    position    int  NOT NULL DEFAULT 0,
    vote_count  int  NOT NULL DEFAULT 0
);
CREATE INDEX social_poll_options_question_idx
    ON social_poll_options (question_id, position);

CREATE TABLE social_poll_votes (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    question_id uuid        NOT NULL REFERENCES social_poll_questions (id) ON DELETE CASCADE,
    option_id   uuid        NOT NULL REFERENCES social_poll_options (id) ON DELETE CASCADE,
    user_id     text,
    ip          text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT social_poll_votes_actor_ck CHECK (user_id IS NOT NULL OR ip IS NOT NULL)
);
CREATE UNIQUE INDEX social_poll_votes_user_uq
    ON social_poll_votes (question_id, user_id) WHERE user_id IS NOT NULL;
CREATE UNIQUE INDEX social_poll_votes_ip_uq
    ON social_poll_votes (question_id, ip) WHERE user_id IS NULL AND ip IS NOT NULL;

-- ----------------------------------------------------------------------------
-- Posts: the generic authored-content primitive (a "blog post" is just a post
-- whose write-permission is held only by the root group). SPLIT counters, same
-- reasoning as comments. NOT `blog_posts` — the name is the one real lock-in.
-- ----------------------------------------------------------------------------
CREATE TABLE social_posts (
    id             text        PRIMARY KEY DEFAULT gen_random_uuid()::text,  -- opaque; hosts may backfill legacy numeric ids
    author_id      text        NOT NULL,
    title          text        NOT NULL,
    slug           text,
    body           text        NOT NULL,
    excerpt        text,
    cover_url      text,
    language       text        NOT NULL DEFAULT '',
    is_draft       boolean     NOT NULL DEFAULT true,
    live_at        timestamptz,
    total_likes    int         NOT NULL DEFAULT 0,
    total_dislikes int         NOT NULL DEFAULT 0,
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    deleted_at     timestamptz
);
CREATE INDEX social_posts_published_idx
    ON social_posts (language, live_at DESC)
    WHERE deleted_at IS NULL AND is_draft = false;
CREATE UNIQUE INDEX social_posts_slug_uq
    ON social_posts (slug) WHERE slug IS NOT NULL AND deleted_at IS NULL;

-- ----------------------------------------------------------------------------
-- Per-entity aggregate rollup: one row per (entity_type, entity_id) with the
-- denormalized counts, maintained IN-TX by the reaction / favorite / comment
-- write paths. Lets any host read an item's counts O(1) and sort many items by
-- them (galleries by likes/favorites/comments). Source tables stay authoritative
-- so a one-shot recompute can always rebuild this (see #20).
-- ----------------------------------------------------------------------------
CREATE TABLE social_entity_counts (
    entity_type   text        NOT NULL,
    entity_id     text        NOT NULL,
    likes         int         NOT NULL DEFAULT 0,
    dislikes      int         NOT NULL DEFAULT 0,
    favorites     int         NOT NULL DEFAULT 0,
    comment_count int         NOT NULL DEFAULT 0,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (entity_type, entity_id)
);
-- Sort a type's items by a count (DESC), only over rows that have any.
CREATE INDEX social_entity_counts_likes_idx     ON social_entity_counts (entity_type, likes DESC)         WHERE likes > 0;
CREATE INDEX social_entity_counts_favorites_idx ON social_entity_counts (entity_type, favorites DESC)     WHERE favorites > 0;
CREATE INDEX social_entity_counts_comments_idx  ON social_entity_counts (entity_type, comment_count DESC) WHERE comment_count > 0;
-- "Best" = Wilson lower bound on likes vs (likes+dislikes): ranks a 900/1000
-- above a 1/1, cheaply, via an immutable expression index. 0 when no votes.
CREATE INDEX social_entity_counts_best_idx ON social_entity_counts (
    entity_type,
    (CASE WHEN likes + dislikes = 0 THEN 0::float8 ELSE
        ((likes + 1.9208) / (likes + dislikes)
         - 1.96 * sqrt((likes::float8 * dislikes) / (likes + dislikes) + 0.9604) / (likes + dislikes))
        / (1 + 3.8416 / (likes + dislikes))
     END) DESC
) WHERE likes + dislikes > 0;

-- ----------------------------------------------------------------------------
-- Favorites: user-only bookmark (no anon). Separate from reactions (favorites
-- = unsigned presence; reactions = signed ±1, anon-capable). No denormalized
-- count column — the host decides denormalization.
-- ----------------------------------------------------------------------------
CREATE TABLE social_favorites (
    user_id     text        NOT NULL,
    entity_type text        NOT NULL,
    entity_id   text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, entity_type, entity_id)
);
CREATE INDEX social_favorites_entity_idx
    ON social_favorites (entity_type, entity_id);
CREATE INDEX social_favorites_user_created_idx
    ON social_favorites (user_id, created_at DESC);
