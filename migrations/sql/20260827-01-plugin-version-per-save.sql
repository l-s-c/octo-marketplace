-- +migrate Up
-- Every save (create / edit / container reupload) now appends a full
-- plugin_versions snapshot carrying a per-plugin auto-increment version label, so
-- the (plugin_id, version) pair is no longer unique. Relax the unique key to a
-- plain index: the version_id primary key still guarantees row uniqueness, and
-- idx_plugin_versions_plugin_created preserves list-page ordering.
ALTER TABLE `plugin_versions`
  DROP INDEX `uq_plugin_versions_plugin_version`,
  ADD INDEX `idx_plugin_versions_plugin_version` (`plugin_id`, `version`);

-- +migrate Down
-- Restoring the unique constraint requires the table to hold no duplicate
-- (plugin_id, version) rows. Per-save snapshots use an auto-increment label so
-- they do not collide, but a manual re-publish could; deduplicate before rolling
-- back if this ALTER fails with error 1062.
ALTER TABLE `plugin_versions`
  DROP INDEX `idx_plugin_versions_plugin_version`,
  ADD UNIQUE KEY `uq_plugin_versions_plugin_version` (`plugin_id`, `version`);
