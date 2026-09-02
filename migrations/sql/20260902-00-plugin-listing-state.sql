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
-- SPLIT INTO THREE SINGLE-DDL FILES (add-column / backfill / reindex). MySQL
-- implicitly commits every DDL, so the sql-migrate transaction protects nothing:
-- a single file that ran DDL → long UPDATE → DDL would, on a transient failure of
-- the UPDATE (innodb_lock_wait_timeout during a rolling deploy, an operator or
-- pt-kill killing a long query), leave the ADD COLUMN committed but the migration
-- record unwritten — the next boot replays the file and dies on ERROR 1060
-- (duplicate column), so the service cannot start until a human edits
-- gorp_migrations by hand. Each statement now lives in its own file so a failure
-- leaves a whole, re-appliable step behind. Same hazard 20260722-00:3-6 reasons
-- about; this restores that discipline.
--
-- ADD COLUMN with DEFAULT 'published' + ALGORITHM=INSTANT is a metadata-only
-- change that touches ZERO rows on 8.0.29+: an instant-add column reads its
-- default for every pre-existing row without rewriting any of them. That default
-- grandfathers every live row to 'published' — the reach it had under the
-- pre-listing_state rules (existing `space` rows were listed and are treated as
-- approved, brief.md:119-121) — at no row cost. 20260902-01 then flips the column
-- default to the fail-closed 'draft' for future inserts; SET DEFAULT does not
-- rewrite the instant-add default, so the two defaults coexist and the grandfather
-- value survives. The soft-deleted correction also lives in 20260902-01.
--
-- ALGORITHM=INSTANT makes the "only instant from 8.0.29" hazard LOUD: `AFTER
-- visibility` silently degrades to a full clustered-index rebuild under the
-- migration lock on 8.0.12–8.0.28. An explicit clause turns that into an error an
-- operator sees rather than a surprise table rebuild on the largest table.
ALTER TABLE `plugins`
  ADD COLUMN `listing_state` ENUM('draft','published','delisted') NOT NULL DEFAULT 'published'
    COMMENT 'Listing lifecycle, independent of review state. Never add review values.'
    AFTER `visibility`,
  ALGORITHM=INSTANT;

-- +migrate Down
ALTER TABLE `plugins` DROP COLUMN `listing_state`;
