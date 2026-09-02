-- +migrate Up
-- Split out of 20260902-02 so each file carries a SINGLE DDL statement (see
-- 20260902-00's header for the implicit-commit hazard a multi-DDL file carries).
-- 20260902-02 rebuilt an index on `plugins`; this one adds an index on
-- `plugin_review_requests`. Two DDLs in one file are avoidable and reintroduce
-- the exact hazard the split exists to close: MySQL implicitly commits each
-- ALTER, so if the process dies between the two ALTERs and the migration-record
-- insert, the next boot replays the file — the first ALTER is replay-safe, but a
-- bare ADD INDEX is not (ERROR 1061 duplicate key name), stranding the boot.
--
-- NOTE: Earlier heads of this branch (pre-split 20260902-00 and the round-8
-- 20260902-02) ALREADY PHYSICALLY CREATED an index named
-- `idx_review_plugin_submitted` on environments that ran those heads. Creating
-- a fresh migration id is NOT sufficient on its own — replay against such an
-- environment would throw ERROR 1061 (duplicate key name) and brick every
-- subsequent boot. The index is therefore given a fresh name
-- (`idx_review_plugin_submitted_at`) so this step is idempotent against both a
-- clean schema and any environment that ran an earlier head. Code and the
-- optimizer reference the index by shape, not by name; the leftover old-named
-- index on upgraded envs is a harmless duplicate of the same key (same
-- columns, same order) and costs one extra index write per insert — acceptable
-- versus forcing a DROP+ADD rename on the hot path here. A follow-up cleanup
-- migration can drop the stale name once all envs are past this point.
--
-- Backs the latest-review-per-plugin lookup that derives the displayed status on
-- the "my publishes" list. The pending EXISTS is already served by
-- idx_review_plugin_status_version; this one serves the ORDER BY submitted_at.
ALTER TABLE `plugin_review_requests`
  ADD INDEX `idx_review_plugin_submitted_at` (`plugin_id`, `submitted_at`),
  ALGORITHM=INPLACE, LOCK=NONE;

-- +migrate Down
ALTER TABLE `plugin_review_requests`
  DROP INDEX `idx_review_plugin_submitted_at`,
  ALGORITHM=INPLACE, LOCK=NONE;
