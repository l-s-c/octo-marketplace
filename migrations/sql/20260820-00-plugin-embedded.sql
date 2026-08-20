-- +migrate Up
-- Embedded plugins are standalone rows promoted from a parent's persisted JSON
-- (legacy expert embedded skills, squad member snapshots). They stay reachable
-- through relations and detail, but list surfaces exclude them: they are parts
-- of a parent asset, not independently authored catalog entries.
ALTER TABLE `plugins`
  ADD COLUMN `is_embedded` TINYINT(1) NOT NULL DEFAULT 0 AFTER `plugin_type`;

-- +migrate Down
ALTER TABLE `plugins` DROP COLUMN `is_embedded`;
