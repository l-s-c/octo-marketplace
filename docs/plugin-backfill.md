# Unified Plugin historical backfill

`plugin-backfill` is an explicit operations command. It is not run by API startup or by schema migration, does not alter legacy rows, does not dual-write, and does not create checkpoint tables.

## Safety model

- Run only after `20260819-00-unified-plugin.sql` has created the seven confirmed unified tables.
- Source IDs are preserved only when globally unique across Skills, Experts, Expert Squads, and MCP servers. Collisions use deterministic family-prefixed IDs.
- Skill and Expert category rows receive deterministic family-prefixed IDs, so the two legacy taxonomies cannot collide.
- Skill tags are resolved only through `skill_tags`; Expert and Expert Team tags are resolved separately through `expert_tags`. Unknown IDs reject the owning legacy row.
- Experts and Expert Teams are imported as top-level Plugins. A source ID is preserved only when globally unique; collisions use deterministic family IDs. Legacy Expert `public` visibility becomes unified `space`, preserving its original Space-scoped visibility semantics.
- Embedded Expert skills, Squad member snapshots, and each member's embedded skills become deterministic standalone Plugins. Stable persisted `member_key`, `object_key`, or `zip_object_key` values are used when present; otherwise identity includes the parent and source array position. Names, filenames, commands, and URLs are never used to infer catalog identity.
- Relations are deterministic and retain source array order: `expert_team_member` for Team→member and `expert_skill` for Expert/member→Skill. Every version snapshots its sorted outgoing relations.
- Generated relation graphs must be acyclic, no deeper than 16 nodes, and contain no more than 500 nodes per connected component. A violation aborts planning before any write.
- Top-level, member, and Connector MCP configurations are strictly sanitized. Exactly one JSON object is accepted; `env` and `headers` string values are blanked; non-empty or non-string secret-shaped fields reject the entire owning graph. Secret values are never written to Plugin, version, relation, audit, hash, or report data.
- Placements are intentionally not imported because no legacy-to-`placement_code` mapping is confirmed.
- Skills, Experts, Expert Teams, generated member/skill snapshots, and MCP servers receive immutable version rows, `current_version_id`, and deterministic `import` audits. Existing Skill version history is retained; a deterministic snapshot is created only when no version row exists.
- Apply preflights every deterministic primary key against the complete expected row. An exact match is counted as existing; any mismatch aborts and rolls back instead of being hidden by `INSERT IGNORE`. Re-running resumes without a checkpoint table. Verify covers generated Plugins, relations, relation-bearing versions, and audits and reports missing or conflicting rows.

## Usage

```bash
export MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/octo_marketplace?parseTime=true'

go run ./cmd/plugin-backfill -mode dry-run
go run ./cmd/plugin-backfill -mode apply
go run ./cmd/plugin-backfill -mode verify
```

The default mode is `dry-run`. Output is structured JSON containing expected and observed counts, deterministic hashes, missing/conflicting counts, and explicit skipped-field/source issues. `apply` is transactional for each execution. `verify` performs no writes. The command exits with status 2 when verification finds missing or conflicting rows.

Review every `issues` entry before cutover. A skipped field or row requires a product/data decision rather than inferred semantics.
