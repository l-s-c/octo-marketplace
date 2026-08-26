-- +migrate Up
-- Backfill is_embedded for plugins that are parts of a parent asset but were
-- created through the live container-import path before insertPlugin wrote the
-- is_embedded column. Any plugin that is the TARGET of an expert_skill (a
-- bundled skill) or expert_team_expert (a team member) relation is an embedded
-- part: reachable via the parent's detail and relations, never a standalone
-- catalog entry. The backfill already stamps these `1`; this repairs the rows
-- the live import left at `0` so they drop out of the skill/expert lists.
--
-- Guard: only flip rows that carry NO market placement. Container/backfill
-- graphs mint dedicated embedded children with no placement, whereas an
-- admin/backfilled standalone catalog plugin always carries a default
-- placement. This spares those standalone rows, since the tenant upsert path
-- lets a tenant expert/team reference a standalone plugin (e.g. a system skill)
-- via these relation types. NOTE: the guard is a heuristic, not an invariant —
-- an UNPUBLISHED tenant-created plugin (Service.Create, no placement until
-- Publish) that is referenced this way would still be flipped. That residual
-- case is intentionally accepted: such a row is already absent from every
-- placement-scoped market list, and the current product flow bundles embedded
-- skill/member copies rather than referencing standalone rows. Idempotent:
-- rows already at `1` are unaffected.
UPDATE `plugins` p
  JOIN `plugin_relations` r
    ON r.target_plugin_id = p.plugin_id AND r.deleted_at IS NULL
  SET p.is_embedded = 1
  WHERE r.relation_type IN ('expert_skill', 'expert_team_expert')
    AND NOT EXISTS (
      SELECT 1 FROM `plugin_placements` pp WHERE pp.plugin_id = p.plugin_id
    );

-- +migrate Down
-- No safe inverse: this migration cannot distinguish rows it flipped from the
-- backfill's already-embedded parts, and un-embedding a genuine part would
-- wrongly surface it as a standalone catalog entry. Intentionally a no-op.
SELECT 1;
