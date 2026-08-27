-- +migrate Up
-- Unify plugin visibility onto the three-value enum system/space/private.
--
-- The legacy per-type visibility vocabularies meant different things, and the
-- old-table backfill preserved that split:
--   * mcp/expert/expert_team: legacy `public` meant space-scoped (mapped to
--     `space`), legacy `system` meant platform-global (kept `system`).
--   * skill: legacy `public` meant platform-global.
-- So every `public` row that reached the unified `plugins` table is a
-- platform-global one — admin-created rows (the old CREATE convention stamped
-- `public` for skill/expert/expert_team) and backfilled global skills. All of
-- them carry an empty global Space, so folding `public` into the unified global
-- value `system` preserves their reach without leaking any space-scoped row.
UPDATE `plugins` SET `visibility` = 'system' WHERE `visibility` = 'public';

-- +migrate Down
-- Best-effort inverse: restore `public` for the global rows this could have
-- touched. `system` connectors predate this change, so scope the revert to the
-- types whose global rows were `public` before the unification.
UPDATE `plugins` SET `visibility` = 'public'
  WHERE `visibility` = 'system'
    AND `plugin_type` IN ('skill', 'expert', 'expert_team')
    AND (`space_id` IS NULL OR `space_id` = '');
