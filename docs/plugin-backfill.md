# Unified Plugin historical backfill

`plugin-backfill` is an explicit operations command. It is not run by API startup or by schema migration, does not alter legacy rows, does not dual-write, and does not create checkpoint tables.

## Safety model

- Run only after `20260819-00-unified-plugin.sql` has created the seven confirmed unified tables.
- Every backfilled row receives a deterministic UUID (RFC 9562 version 8, derived from a family-namespaced hash of the source ID), matching the UUID format the service generates at runtime (version 7). Legacy source IDs are not preserved; the legacy→unified mapping stays recomputable from the deterministic function.
- Skill and Expert category rows use the same deterministic family-namespaced derivation, so the two legacy taxonomies cannot collide.
- Skill tags are resolved only through `skill_tags`; Expert and Expert Team tags are resolved separately through `expert_tags`. Unknown IDs reject the owning legacy row.
- Experts and Expert Teams are imported as top-level Plugins with deterministic UUIDs. Legacy Expert `public` visibility becomes unified `space`, preserving its original Space-scoped visibility semantics.
- Embedded Expert skills, Squad member snapshots, and each member's embedded skills become deterministic standalone Plugins; parent packages keep only their own files and never embed copies of related-plugin content, which lives solely in the standalone Plugin reachable through the relation edge. Stable persisted `member_key`, `object_key`, or `zip_object_key` values are used when present; otherwise identity includes the parent and source array position. Names, filenames, commands, and URLs are never used to infer catalog identity.
- Relations are deterministic and retain source array order: `expert_team_member` for Team→member and `expert_skill` for Expert/member→Skill. Every version snapshots its sorted outgoing relations.
- Generated relation graphs must be acyclic, no deeper than 16 nodes, and contain no more than 500 nodes per connected component. A violation aborts planning before any write.
- Top-level, member, and Connector MCP configurations are strictly sanitized. Exactly one JSON object is accepted; `env` and `headers` string values are blanked; non-empty or non-string secret-shaped fields reject the entire owning graph. Secret values are never written to Plugin, version, relation, audit, hash, or report data.
- Placements are migrated to the confirmed `default` placement: every active category registers for each of its plugin types (ordered by its legacy `sort_order`), and every active top-level (non-embedded) Plugin is placed under its own category. Embedded plugins are never placed.
- Skills, Experts, Expert Teams, generated member/skill snapshots, and MCP servers receive immutable version rows, `current_version_id`, and deterministic `import` audits. Existing Skill version history is retained; a deterministic snapshot is created only when no version row exists.
- Planning validates every generated category, relation source/target, version owner, and `current_version_id` reference before writes; the command does not rely on database foreign keys.
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

## Phases

The command carries three idempotent phases (all support `dry-run` / `apply` / `verify`):

| Phase | What it does | When to run |
| --- | --- | --- |
| `plan` (default) | Deterministic historical backfill from the legacy tables (skills / mcp_servers / experts / expert_squads) into the unified plugin tables, emitting the contracts/v1 package layout. | Environments whose unified tables are empty while legacy data exists. |
| `enrich` | Display-data fill: legacy icons, connector category registration, materialized tool counts, one-time resource-metrics copy onto plugin rows. | After `plan`, or on environments that predate these columns. |
| `repackage` | Migrates already-stored documents to the contracts/v1 layout: strips embedded manifest.json, collapses expert_team packages to a single AGENTS.md, converts first-generation layouts, renames `expert_team_member` relations to `expert_team_expert` (re-deriving deterministic relation IDs), and recomputes every `plugin_hash` with the octo-plugin-lib formula across plugins, version snapshots, and audit chains. | Environments that ran a pre-contracts/v1 build of the unified plugin backend. |
| `expand-skills` | **STORAGE-AWARE.** Expands skill packages from the legacy pointer layout (SKILL.md stub + `skill/ref.json`, or a `skill/package.zip` storage attachment) into the flat attachment tree — one attachment per file, text inlined and binary/oversize files re-uploaded to the Space's managed prefix — then recomputes `plugin_hash` across plugins, version snapshots, and audit chains. Unlike every other phase this one fetches each skill's stored zip and re-uploads files, so it **requires object-storage credentials** (`STORAGE_DRIVER` + `OSS_*` / `LOCAL_STORAGE_DIR`, the same variables marketplace-api reads). Empty-pointer snapshot skills collapse to a single inline SKILL.md. Idempotent: already-expanded skills carry no pointer and are skipped. | Environments that ran a pre-attachment-tree build of the unified skill backend (after `repackage`). |

## Deployment runbook

Schema migrations (`migrations/sql/`) run automatically at marketplace-api startup; the
data phases above do NOT — run them explicitly right after deploying a new backend build:

1. Back up `plugins`, `plugin_versions`, `plugin_audit_logs`, `plugin_relations`
   (`repackage` rewrites hashes one-way).
2. Make sure the schema is current. Either deploy the new marketplace-api first
   (it auto-migrates at startup), or run the standalone image with `-migrate`,
   which applies the same embedded migration set under the same database lock —
   this lets the migration image go out and be verified BEFORE the service.
3. Run the data phases against the same database:
   - fresh environment: `plan` apply → verify, then `enrich` apply → verify;
   - previously-deployed environment: `enrich` apply → verify (if pending), then
     `repackage` apply → verify, then `expand-skills` apply → verify
     (each `remaining` must be all zeros; exit code 2 otherwise).
   `expand-skills` additionally needs object-storage env (`STORAGE_DRIVER` +
   `OSS_*` / `LOCAL_STORAGE_DIR`); the other phases run DB-only.
4. Release the matching octo-web build in the same window — old and new frontends read
   different package layouts, so the migration and the frontend must not drift apart.

Keep the gap between steps 2 and 3 short: the new binary reads the contracts/v1 layout,
so pre-migration rows are served degraded until `repackage` completes.

## Standalone image

`Dockerfile.backfill` builds a self-contained migration image (dependencies are vendored,
so no git credentials are needed at build time):

```bash
docker build -f Dockerfile.backfill -t plugin-backfill:<tag> .

# Read-only default (repackage dry-run):
docker run --rm -e MYSQL_DSN='<dsn>' plugin-backfill:<tag>

# Explicit phases (add -migrate on the first call to bring the schema current
# when the new marketplace-api has not been deployed yet):
docker run --rm -e MYSQL_DSN='<dsn>' plugin-backfill:<tag> -migrate -phase=enrich -mode=apply
docker run --rm -e MYSQL_DSN='<dsn>' plugin-backfill:<tag> -phase=repackage -mode=apply
docker run --rm -e MYSQL_DSN='<dsn>' plugin-backfill:<tag> -phase=repackage -mode=verify
```

In Kubernetes, run it as a one-shot Job (or a pre-release hook) mounting the same
`MYSQL_DSN` secret the marketplace-api deployment uses — the tool reads the decrypted
environment exactly like the service and never needs the plaintext DSN handled manually.
