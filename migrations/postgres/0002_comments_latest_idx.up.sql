-- Index for the cross-entity latest-comments feed (GET /comments/latest and
-- Runtime.LatestComments): it lists ALL live comments newest-first, but the
-- only created_at index (social_comments_toplevel_idx) leads on
-- (entity_type, entity_id), so the global feed was a full scan + top-N sort
-- per page. This partial index matches the feed's predicate exactly
-- (deleted_at IS NULL, ordered created_at DESC) so each page is an index scan.
--
-- The admin queue (GET /comments/admin) intentionally stays unindexed: it also
-- reads tombstoned rows (deleted_at IS NOT NULL), is moderator-only traffic,
-- and does not warrant a second, non-partial copy of this index.
--
-- Plain CREATE INDEX (not CONCURRENTLY: migratekit applies migrations inside a
-- transaction); comment tables are small enough that the write lock is a
-- non-issue at current sizes.
CREATE INDEX social_comments_latest_idx
    ON social_comments (created_at DESC)
    WHERE deleted_at IS NULL;
