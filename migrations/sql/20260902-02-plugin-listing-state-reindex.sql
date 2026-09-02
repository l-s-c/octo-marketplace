-- +migrate Up
-- Reindex, split out of 20260902-00 so each file is a single DDL unit (see
-- 20260902-00's header for the implicit-commit hazard a DDL→UPDATE→DDL file
-- carried). Both index rebuilds are pure DDL and safe to co-locate here: neither
-- is separated by a DML step, so no intermediate implicit commit can strand a
-- half-applied UPDATE.
--
-- The catalog read predicate now filters (visibility, space_id, listing_state)
-- together, so listing_state joins the existing scope index rather than getting
-- one of its own. DROP+ADD in one ALTER lets InnoDB do a single index rebuild.
--
-- LOCK=NONE keeps the table writable during the rebuild; ALGORITHM=INPLACE
-- forbids a silent copy-table fallback. An explicit clause turns "this index
-- change would block writes / rewrite the whole table" into an error an operator
-- sees at deploy time rather than a surprise stall on the largest table.
ALTER TABLE `plugins`
  DROP INDEX `idx_plugins_scope_category_created`,
  ADD INDEX `idx_plugins_scope_category_created` (`visibility`, `space_id`, `listing_state`, `category_id`, `created_at`),
  ALGORITHM=INPLACE, LOCK=NONE;

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
ALTER TABLE `plugins`
  DROP INDEX `idx_plugins_scope_category_created`,
  ADD INDEX `idx_plugins_scope_category_created` (`visibility`, `space_id`, `category_id`, `created_at`),
  ALGORITHM=INPLACE, LOCK=NONE;
