-- +migrate Up
-- (Intentional no-op.) An earlier draft of this migration flipped is_embedded=1
-- for every unplaced expert_skill / expert_team_expert relation target, to
-- repair rows the live container-import path created before insertPlugin wrote
-- the is_embedded column. That heuristic is unsafe: the tenant upsert path lets
-- a tenant expert/team reference a STANDALONE catalog plugin (e.g. a system
-- skill, or an unpublished tenant skill with no placement yet) via those same
-- relation types, and "absence of a placement" cannot reliably tell an embedded
-- per-parent child apart from such a referenced standalone. Flipping the latter
-- would hide it from the catalog (list queries filter is_embedded=0) with a
-- no-op Down.
--
-- The correct fix is at the write path, not in a bulk backfill: insertPlugin now
-- persists is_embedded, container import marks bundled skills / squad members
-- embedded, and the container reupload only tears down genuinely-embedded
-- children (collectContainerChildren filters on is_embedded). The live
-- container-import endpoint is new in this change set, so no production rows
-- predate the write-path fix and need repair. This migration is kept as a no-op
-- to preserve the applied-migration record where the earlier draft already ran.
SELECT 1;

-- +migrate Down
SELECT 1;
