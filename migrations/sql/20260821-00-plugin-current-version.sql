-- +migrate Up
-- Denormalized copy of the published version string (plugin_versions.version)
-- referenced by current_version_id, so list and detail responses can render a
-- human-readable version without a join. Written in the same transaction that
-- moves current_version_id; NULL while a plugin has never been published.
ALTER TABLE `plugins`
  ADD COLUMN `current_version` VARCHAR(64) NULL DEFAULT NULL AFTER `current_version_id`;

-- +migrate Down
ALTER TABLE `plugins` DROP COLUMN `current_version`;
