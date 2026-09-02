-- +migrate Up
-- Split out of 20260902-02 so each file carries a SINGLE DDL statement (see
-- 20260902-00's header for the implicit-commit hazard a multi-DDL file carries).
-- 20260902-02 rebuilt an index on `plugins`; this one adds an index on
-- `plugin_review_requests`. Two DDLs in one file are avoidable and reintroduce
-- the exact hazard the split exists to close: MySQL implicitly commits each
-- ALTER, so if the process dies between the two ALTERs and the migration-record
-- insert, the next boot replays the file — the first ALTER is replay-safe, but a
-- bare ADD INDEX is not (ERROR 1061 duplicate key name), stranding the boot.
-- A fresh migration id also gives environments that ran a pre-split head a
-- never-recorded step, so this index is applied exactly once there too.
--
-- Backs the latest-review-per-plugin lookup that derives the displayed status on
-- the "my publishes" list. The pending EXISTS is already served by
-- idx_review_plugin_status_version; this one serves the ORDER BY submitted_at.
ALTER TABLE `plugin_review_requests`
  ADD INDEX `idx_review_plugin_submitted` (`plugin_id`, `submitted_at`),
  ALGORITHM=INPLACE, LOCK=NONE;

-- +migrate Down
ALTER TABLE `plugin_review_requests`
  DROP INDEX `idx_review_plugin_submitted`,
  ALGORITHM=INPLACE, LOCK=NONE;
