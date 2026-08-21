-- +migrate Up
-- Display icon for marketplace cards: either a persistent public URL (the
-- legacy MCP icon upload flow) or a managed storage object key resolved to a
-- presigned URL at read time (the legacy skill icon flow). Kept out of
-- manifest_json so changing an icon never alters the content-addressed
-- manifest hash or version snapshots.
ALTER TABLE `plugins`
  ADD COLUMN `icon` VARCHAR(512) NOT NULL DEFAULT '' AFTER `created_by_bot_name`;

-- +migrate Down
ALTER TABLE `plugins` DROP COLUMN `icon`;
