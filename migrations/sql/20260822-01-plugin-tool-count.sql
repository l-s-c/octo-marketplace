-- +migrate Up
-- Materialized count of entries in the connector/tools.json package attachment.
-- List queries deliberately never load plugin_json, and MySQL cannot parse a
-- JSON document embedded in an attachment string, so the count is computed on
-- the single write path (CanonicalizeDocuments callers) and stored here.
ALTER TABLE `plugins`
  ADD COLUMN `tool_count` INT NOT NULL DEFAULT 0 AFTER `icon`;

-- +migrate Down
ALTER TABLE `plugins` DROP COLUMN `tool_count`;
