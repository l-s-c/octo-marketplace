-- +migrate Up
-- listing_state is the LISTING lifecycle axis, independent of review state.
-- Before this column `visibility='private'` doubled as "draft", so a save and a
-- publish were indistinguishable: the moment an author created a plugin it was
-- already in their own marketplace grid. visibility now means DECLARED INTENT
-- (who should see it once listed) and listing_state means WHETHER it is listed.
--
-- This is NOT a review-status column. The unified-plugin review flow deliberately
-- keeps review state on plugin_review_requests so a listed v1 and an in-review v2
-- can coexist (see .octospec/tasks/plugin-space-review/brief.md item 26). This
-- enum holds no review vocabulary, no reviewer, no reason, and no frozen
-- snapshot; "审核中" is DERIVED from listing_state plus a pending request rather
-- than stored. Never add 'pending' or 'rejected' to these values — that is
-- exactly the collapse item 26 forbids, and a migration test asserts the enum
-- has these three values and no others.
--
-- The DEFAULT is deliberately the fail-closed value. RebuildGraph re-inserts a
-- container's embedded children, and those children inherit the parent's row
-- shape; a 'published' default would silently list a draft container's children.
-- Every Go write path threads ListingState explicitly, so the default is only
-- reachable from hand-written SQL and test fixtures, where invisible is the safe
-- outcome.
ALTER TABLE `plugins`
  ADD COLUMN `listing_state` ENUM('draft','published','delisted') NOT NULL DEFAULT 'draft'
    COMMENT 'Listing lifecycle, independent of review state. Never add review values.'
    AFTER `visibility`;

-- Grandfathering: every live row keeps exactly the reach it has today. Existing
-- `space` rows were listed under the pre-listing_state rules and are treated as
-- approved with no retroactive requests (brief.md:119-121); existing
-- `private`/`system` rows are unaffected by listing_state under the read
-- predicate either way. 'draft' is therefore a purely NEW state that no existing
-- row can be in.
--
-- Soft-deleted rows are deliberately left 'draft' so an accidental undelete does
-- not republish a plugin the org had already lost sight of.
UPDATE `plugins` SET `listing_state` = 'published' WHERE `deleted_at` IS NULL;

-- The catalog read predicate now filters (visibility, space_id, listing_state)
-- together, so listing_state joins the existing scope index rather than getting
-- one of its own.
ALTER TABLE `plugins`
  DROP INDEX `idx_plugins_scope_category_created`,
  ADD INDEX `idx_plugins_scope_category_created` (`visibility`, `space_id`, `listing_state`, `category_id`, `created_at`);

-- Backs the latest-review-per-plugin lookup that derives the displayed status on
-- the "my publishes" list. The pending EXISTS is already served by
-- idx_review_plugin_status_version; this one serves the ORDER BY submitted_at.
ALTER TABLE `plugin_review_requests`
  ADD INDEX `idx_review_plugin_submitted` (`plugin_id`, `submitted_at`);

-- +migrate Down
ALTER TABLE `plugin_review_requests` DROP INDEX `idx_review_plugin_submitted`;
ALTER TABLE `plugins`
  DROP INDEX `idx_plugins_scope_category_created`,
  ADD INDEX `idx_plugins_scope_category_created` (`visibility`, `space_id`, `category_id`, `created_at`);
ALTER TABLE `plugins` DROP COLUMN `listing_state`;
