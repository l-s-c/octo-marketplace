# Brief: switch plugin contract from inlined copy to external octo-plugin-lib (2.0)

## Goal

Replace the inlined `internal/plugincontract` package (a byte copy of
`codex.mlamp.cn/dmwork/octo-plugin-lib/plugin@v1.0.1`) with a direct dependency
on the public module `github.com/Mininglamp-OSS/octo-plugin-lib/plugin`, and
adopt its current contract generation (schema 2.0). One authoritative contract,
no hand-maintained copy that can silently drift from upstream.

This branch also lands the coordinated version / placement / publish model
changes that the 2.0 adoption enabled — see "Additional scope landed with this
migration" below; they are declared, not incidental.

## Why the public module is a contract *upgrade*, not a mirror swap

Three sources of the same library exist with different generations:

| source | tag | schema gen | canonical/hash | go-get |
| --- | --- | --- | --- | --- |
| inlined `internal/plugincontract` | from codex v1.0.1 | 1.0 | baseline | n/a |
| codex.mlamp.cn (internal) | v1.3.2 | 1.1 | changed | needs GOPRIVATE + intranet |
| **github Mininglamp-OSS** | v0.1.0 | **2.0** | **changed** | public proxy, no auth |

The GitHub `v0.1.0` tag is the public repo's own tag number; its *content* is
the most advanced 2.0 contract (committed 2026-08-27). Adopting it is a 1.0 ->
2.0 contract upgrade with data + cross-repo implications, listed below.

## Load-bearing behavior (what must stay true)

- `plugin_hash` remains the content identity used for version immutability and
  digest verification. It is computed ONLY by the backend (octo-web never
  computes it — confirmed: no sha256/hash in octo-web).
- The stored `plugin_json` / `manifest_json` remain valid contract documents so
  `contract_sweep_test` (MySQL-gated) can validate every live row through the
  lib.
- Existing skill storage objects are NOT re-keyed (owner decision below): their
  managed object keys stay byte-identical; only where the key is *recorded*
  changes.

## Confirmed breaking points (github 2.0 vs inlined 1.0)

Hash-value changes (data migration):
- A1: `ComputePluginHash` sorts `attachments` by `path` before hashing (the
  inlined 1.0 did not) -> hash differs for any multi-attachment package not
  already path-sorted.
- A2: canonical number length cap 128 -> 10240 (inputs the old code rejected now
  pass; number canonicalization for >128-char numbers differs). No practical
  effect on current content but part of the contract.

Schema generation:
- B1: every schema id bumps `*-1.0.json` -> `*-2.0.json`. `DecodeManifest`/
  `DecodePackage` HARD-ASSERT the `$schema` value (validate.go:201, 284) — a
  1.0 `$schema` is rejected with `CodeInvalidField`.

Type / decoder changes (code):
- C1: `Status` is `type Status string` ("ACTIVE"/"ARCHIVED"); the inlined type
  was `uint8`. Contained to backend Go + read DTO (octo-web sends no status).
- C2: 2.0 `Relation` has no `RelationID`/`Status`/timestamps. The endpoint-probe
  in validation.go must drop those fields (DONE).
- C3: 2.0 `Attachment` has no `storage_uri`, and `DecodePackage` uses
  `DisallowUnknownFields` + a top-level key allowlist (`$schema`/`connector`/
  `attachments`). Any package carrying `storage_uri` is REJECTED. This is the
  central design problem — see Storage design.

## Storage design (owner-selected: row-level sidecar mapping)

The current skill flat-attachment tree spills binary/oversize files to managed
storage under a content-mixed key
`deterministicSkillObjectKey = sha256(pluginID \0 path \0 content)[:16]`, and
records that key as `storage_uri` INSIDE the package attachment. 2.0 forbids
that field, and the key is not reconstructible from 2.0 attachment fields
(path + content_hash) without the content, so the location must move out of the
package.

Chosen approach (no blob movement): keep the object key in a host-private
sidecar column, OUTSIDE the hashed/validated package.

- New column `attachment_keys_json JSON NULL` on `plugins` and `plugin_versions`
  (migration `20260827-00-plugin-attachment-keys.sql`, DONE), a JSON object
  mapping attachment path -> managed object key. NULL for rows with no spilled
  storage attachments.
- Write chokepoint `canonicalizeDocuments` splits the incoming package: extract
  each storage attachment's `storage_uri` into the sidecar map (validating
  managed-prefix scope), strip `storage_uri` from the package, then run
  CanonicalJSON / DecodePackage / ComputePluginHash over the 2.0-legal package.
  `buildSkillAttachmentTree` is unchanged (still emits an in-memory storage_uri
  that the chokepoint consumes).
- Read paths (`skill_artifact.go` StreamSkillPackage + legacy skill/package.zip,
  `install.go` supporting-file fetch + legacy zip) resolve the object key from
  the row's `AttachmentKeys` map by path instead of the package's storage_uri.
- Existing object keys are preserved -> no blob re-key/copy.

## Combined backfill (single pass over existing rows, both tables)

For every live `plugins` row and `plugin_versions` snapshot:
1. Extract storage_uri -> `attachment_keys_json`, strip storage_uri from the
   package (same split as the write chokepoint).
2. Rewrite `$schema` `*-1.0.json` -> `*-2.0.json` in manifest_json/plugin_json.
3. Recompute `plugin_hash` with the 2.0 formula (A1 path-sort).
4. plan/verify must be byte-identical to a fresh canonicalize; dry-run/apply/
   verify, idempotent. No object bytes are moved.

## Cross-repo lockstep (octo-web) — narrow but mandatory

octo-web does NOT compute plugin_hash and its `goCanonicalJSON` needs NO change
for A1/A2 (it neither sorts attachments nor normalizes numbers, and is used only
to serialize a few connector attachment contents). The ONLY required frontend
change is the hard-coded `$schema` constants, which must bump in the SAME deploy
window as the backend (2.0 rejects 1.0 `$schema`):
- `packages/dmworkmcp/src/api/mcpWireParams.ts` lines 63, 115
- `packages/dmworkskillmarket/src/api/skillApiReal.ts` lines 593, 622
Status is not sent by octo-web, so C1 does not touch the frontend.

## Additional scope landed with this migration (declared)

Beyond the pure contract swap above, this branch also lands the version /
placement / publish model changes below. They are in scope for this PR and
disclosed in its description's "Scope & deploy coordination" section; they are
recorded here so the Linked Spec and the shipped change agree.

- **`POST /plugins/publish` removed.** A save IS a version — every create/edit
  snapshots a `plugin_versions` row and, for tenant/admin create, auto-attaches
  the default market placement — so the explicit publish step (and its import
  rollback / `restoreWriteRequest` / `injectStorageKeys`) is gone. This is a
  knowing, coordinated OpenAPI breaking change (documented exception in
  `docs/openapi/breaking-ignore.txt` + `.oasdiff.yaml`).
- **Per-save version history + `version` wire field.** `/plugins/upsert` accepts
  an optional `version` label stored on `plugins.current_version` (default
  `1.0.0`; a version-less update inherits the stored label; import create stamps
  the parsed package version). `plugin_versions.version` is a per-plugin
  auto-increment ordinal by design, NOT the caller label. The snapshot dedup skips
  only a byte-identical no-op — content hashes AND label AND relations AND sidecar
  AND changelog all unchanged — so a client cannot bloat history with identical
  resubmits while every real save is recorded.
- **`GET /plugins/versions`** returns per-save history gated by ordinary plugin
  visibility, projected to metadata only (label / hashes / changelog / timestamp);
  full manifest/package bytes are not returned per history row.
- **Create-time + self-healing default placement.** Create attaches the default
  visible placement; `Update`/`RebuildGraph` insert it when missing so a plugin
  created before auto-placement stays market-listable. Placement `visible` /
  `sort_order` / multi-scene control retired with publish.
- **`POST /admin/skill_icon_uploads`** admin twin route (mirrors
  `/admin/mcp_icon_uploads`); connector category names localized into
  `plugin_categories.name`.
- **Backfill** additionally attaches default placements (`plan`) and fills
  categories / icons / tool-counts / metrics (`enrich`); `expand-skills` fails
  closed on an unresolvable live archive/object (gating `skip`) and repairs the
  audit hash chain across expanded→skipped boundaries.

## Out of scope

- Re-keying / moving stored skill blobs (explicitly rejected in favor of the
  sidecar).
- Adopting the upstream `pluginstore`/`pluginservice`/`pluginconformance` layers
  or the `RevisionContent` contract; the marketplace keeps its own persistence.
- octo-web / octo-admin changes beyond the coordinated set that ships in the same
  deploy window (the four `$schema` constants, the `version` wire field, and
  reliance on tenant create-time auto-placement) — see the paired PRs
  octo-web #1584 / octo-admin #145.

## Acceptance criteria

Closed in CI / this branch:
- `go build ./...`, `gofmt -l` clean, `go test ./...` green.
- `make openapi-check` passes; `make openapi-diff` breaking is documented — the
  `POST /plugins/publish` removal is a knowing exception, and the added
  `/admin/skill_icon_uploads` route + metadata-only `GET /plugins/versions` are
  regenerated into the spec.
- CI's MySQL 8.0 service verifies the migration up/down test.
- The version/placement/publish model is covered by unit + sqlmock tests:
  per-save snapshot, no-op dedup over every recorded component, version-label
  contract (create vs update), self-healing default placement, and the
  expand-skills truncation guard + audit-chain repair.

Deploy-time runbook gates (require a live MySQL + object store — cannot be closed
in the worktree or by CI, which does not run migrations before the sweep):
- Run the combined backfill (`plan` → `enrich`, or `enrich` → `repackage` →
  `expand-skills` for a previously-deployed env) against the real DB: hash
  recompute + `$schema` rewrite + storage_uri split byte-identical to a fresh
  canonicalize, idempotent, `remaining` all zero, no `error`/`skip` issue.
- `contract_sweep_test` (gated on `TEST_MYSQL_DSN`) validates every live row
  through the 2.0 lib against a migrated, populated database.
- Storage read paths (install + download) resolve keys from the sidecar.
- Backend backfill runs before or with the octo-web / octo-admin releases; all
  ship in one coordinated window (#72 deploys first or atomically).

## Post-review fixes (adversarial self-review, 3 confirmed correctness bugs)

- **Read fallback:** read paths (`writeSkillZip`, install supporting files, legacy
  zip via `storageAttachmentKey`) now fall back to the inline `storage_uri` when a
  row's sidecar is NULL, so un-migrated storage-bearing rows keep working in the
  window between deploying this code and running the backfill.
- **Repackage migrates storage rows:** `transformPackage` now splits `storage_uri`
  out of storage attachments into a returned sidecar map (previously only raw
  size/hash + `$schema` were normalized, so a flat-tree-with-storage row was
  rejected by DecodePackage and left unmigrated with a NULL sidecar). Normalization
  moved BEFORE the no-op short-circuit so a row needing only normalization still
  migrates. `repackagePlugins`/`repackageVersions` persist `attachment_keys_json`
  only when keys were split (never overwriting an expand-populated sidecar).
- **Restore path:** retired with the publish flow. The import rollback
  (`restoreWriteRequest`) and its `injectStorageKeys` re-injection helper were
  removed along with `Service.Publish`; a re-import is now just another save
  revision with no half-applied state to restore, so there is no stored-package
  re-canonicalization to guard.
- New regression tests: `TestTransformPackageSplitsStorageURI`,
  `TestSkillRefFallsBackToInlineStorageURI`,
  `TestAttachmentKeysMigrationAddsAndDropsColumn`.

## Progress (this branch `feat/octo-plugin-lib-external`)

Done + build/test-verified (`go build ./...`, `go vet ./...`, `gofmt -l` clean,
`go test ./...` = 35/35 packages green, `make openapi-check` passes):

- External dep added (`github.com/Mininglamp-OSS/octo-plugin-lib v0.1.0`),
  `internal/plugincontract` removed, all importers repointed (alias `libplugin`).
- C2: validation.go relation endpoint probe adapted to the 2.0 `Relation`.
- Migration `20260827-00-plugin-attachment-keys.sql` (plugins + plugin_versions).
- `model.Plugin/PluginVersion.AttachmentKeys`; repo INSERT/UPDATE/SELECT/scan +
  version snapshot persist the column; read.go loads it.
- Write chokepoint `splitStorageKeys` in `canonicalizeDocuments`: strips
  `storage_uri` into the sidecar, hashes the 2.0-legal package;
  `CanonicalDocuments.AttachmentKeys` threaded to the row in `buildWrite` (covers
  create/update/container-graph).
- Read paths resolve the object key from the sidecar: `skill_artifact.go`
  (`writeSkillZip`, legacy `skill/package.zip`), `install.go` (supporting files,
  legacy zip). `storageAttachmentKey` now reads `AttachmentKeys`, not the package.
- Raw-attachment 2.0 fix (raw forbids content_size/content_hash): `skill_tree.go`
  (`buildSkillAttachmentTree` + expand stub), `container.go` (`rawAttachmentMap`),
  backfill `mapping.go` + `repackage.go`.
- Host schema-id constants bumped 1.0->2.0 (`service/plugin/schema.go`,
  `backfill/plugin/mapping.go`); `transformPackage` normalizes legacy rows
  (drops raw size/hash, bumps `$schema`).
- `ExpandSkillPackage` now splits storage keys and returns the sidecar; the
  expand-skills backfill (`expand.go`) persists `attachment_keys_json` for
  plugins + plugin_versions. New test `TestExpandSkillPackageSplitsStorageKeys`.
- All affected unit tests + sqlmock fixtures modernized to the 2.0 contract.

Remaining (cannot be closed inside this worktree — need a live MySQL + object
store, and the paired repo):
- Run the combined backfill (repackage + expand-skills phases) against a real DB:
  verify hash recompute + `$schema` rewrite + storage_uri split are byte-identical
  to a fresh canonicalize, idempotent, and that `contract_sweep_test` (MYSQL_DSN
  gated) validates every live row through the 2.0 lib.
- octo-web: bump the four `$schema` constants (mcpWireParams.ts 63/115,
  skillApiReal.ts 593/622) 1.0->2.0 and ship in the SAME deploy window as the
  backend backfill.

