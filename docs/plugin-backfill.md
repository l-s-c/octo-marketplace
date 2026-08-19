# Unified Plugin historical backfill

`plugin-backfill` is an explicit operations command. It is not run by API startup or by schema migration, does not alter legacy rows, does not dual-write, and does not create checkpoint tables.

## Safety model

- Run only after `20260819-00-unified-plugin.sql` has created the seven confirmed unified tables.
- Source IDs are preserved only when globally unique across Skills, Experts, Expert Squads, and MCP servers. Collisions use deterministic family-prefixed IDs.
- Skill and Expert category rows receive deterministic family-prefixed IDs, so the two legacy taxonomies cannot collide. Expert categories are prepared for later expert import.
- Skill tag dictionary IDs are resolved back to names for `tags_json`.
- This safe first command deliberately skips Expert and Expert Squad rows and emits structured `expert_graph_not_migrated` issues. Squad members are embedded snapshots rather than proven Expert references, so the command does not invent member or relation semantics.
- Connector `env` and `headers` values are always blanked. Any other non-empty secret-shaped field rejects that source record and produces a structured skip report. Secret values are never written to unified rows or audit snapshots.
- Placements are intentionally not imported because no legacy-to-`placement_code` mapping is confirmed.
- Skills and MCP servers are imported. Each imported Plugin receives immutable version rows, `current_version_id`, and a deterministic `import` audit entry. Existing Skill version history is retained; a deterministic snapshot is created only when no version row exists.
- Inserts use deterministic primary keys with idempotent insert behavior. Re-running resumes without a checkpoint table; verify detects conflicting existing rows by type/hash.

## Usage

```bash
export MYSQL_DSN='user:password@tcp(127.0.0.1:3306)/octo_marketplace?parseTime=true'

go run ./cmd/plugin-backfill -mode dry-run
go run ./cmd/plugin-backfill -mode apply
go run ./cmd/plugin-backfill -mode verify
```

The default mode is `dry-run`. Output is structured JSON containing expected and observed counts, deterministic hashes, missing/conflicting counts, and explicit skipped-field/source issues. `apply` is transactional for each execution. `verify` performs no writes. The command exits with status 2 when verification finds missing or conflicting rows.

Review every `issues` entry before cutover. A skipped field or row requires a product/data decision rather than inferred semantics.
