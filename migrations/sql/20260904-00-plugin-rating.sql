-- +migrate Up
ALTER TABLE `plugins`
  ADD COLUMN `rating` TINYINT UNSIGNED NULL AFTER `tool_count`,
  ADD CONSTRAINT `chk_plugins_rating_range`
    CHECK (`rating` IS NULL OR `rating` BETWEEN 1 AND 5);

-- +migrate Down
ALTER TABLE `plugins`
  DROP CHECK `chk_plugins_rating_range`,
  DROP COLUMN `rating`;
