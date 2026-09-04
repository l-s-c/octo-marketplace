-- +migrate Up
-- MySQL DDL commits independently of sql-migrate's bookkeeping transaction. If
-- the process exits after ALTER commits but before gorp_migrations is updated,
-- replay must converge instead of failing with duplicate-column error 1060.
-- Do not position this column with AFTER/FIRST: CynosDB cannot instant-add a
-- positioned column and may reject it or rebuild the plugins table.
SET @ddl := IF(
  (SELECT COUNT(*) FROM information_schema.COLUMNS
     WHERE table_schema = DATABASE()
       AND table_name = 'plugins'
       AND column_name = 'rating') > 0,
  'DO 0',
  'ALTER TABLE `plugins` ADD COLUMN `rating` TINYINT UNSIGNED NULL, ADD CONSTRAINT `chk_plugins_rating_range` CHECK (`rating` IS NULL OR `rating` BETWEEN 1 AND 5)');
PREPARE add_plugin_rating FROM @ddl;
EXECUTE add_plugin_rating;
DEALLOCATE PREPARE add_plugin_rating;

-- +migrate Down
ALTER TABLE `plugins`
  DROP CHECK `chk_plugins_rating_range`,
  DROP COLUMN `rating`;
