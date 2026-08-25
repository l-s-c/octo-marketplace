# Unified plugin admin surface + octo-admin repoint

## Goal

Restore the unified marketplace-admin surface (`/api/v1/admin/plugins*` +
`/api/v1/admin/plugin_categories`) that was split out of PR #67 (commit
`84620e9`), rebuilt on the current clean baseline (octo-plugin-lib inlined, no
`secretscan`, connector attachment-tree package shape, backfill CAS fixes), and
repoint the octo-admin console's MCP / Skill / Expert / Expert-Team admin CRUD
onto it. This unblocks octo-admin PR #145, whose `d53496b` already repointed MCP
and Skill CRUD to `/admin/plugins` — endpoints that do not exist in #67.

This work lives on branch `feat/unified-plugin-admin-surface` (marketplace) and
`feat/unified-plugin-admin` (octo-admin). **PR #67 (`feat/unified-plugin-backend`)
must not be touched.** The marketplace branch is cut from #67's head and rebases
onto `main` once #67 merges.

## Load-bearing behavior

- The admin surface is authorized by the route-level admin role gate
  (`RoleMarketAdmin`), operating cross-Space with no ownership check via repo
  `Scope.Admin` (already present and dormant on the baseline). The service never
  re-derives a Space from the caller on this path.
- Type conventions: admin CREATE stamps `visibility=system` + NULL Space for
  connectors, `visibility=public` + empty global Space for skill/expert/
  expert_team. Admin UPDATE **preserves** the row's existing visibility, Space,
  owner, and creator provenance — a metadata edit must never force-publish a
  tenant-private plugin, and owner/creator are immutable.
- Storage-attachment keys are namespaced under the row's real Space, so admin
  UPDATE canonicalizes against `old.SpaceID` (not the empty global Space),
  otherwise every storage-backed attachment is rejected.
- Relation targets on an admin write resolve under the cross-Space admin scope
  (`writeScope(c, admin=true)`), matching the repo's admin-aware
  `lockRelationTargets`; the tenant scope would hide a space-scoped target.
- Decision A — retained supporting endpoints, data source swapped to `plugins`:
  the legacy per-type supporting endpoints stay mounted but read/write the
  unified `plugins`/`plugin_*` tables, not the old per-type tables:
  - skill `skill_md` / `download` read the `plugins.plugin_json` attachment tree
  - category writes target `plugin_categories`; tag listing aggregates
    `plugins.tags_json`
  - icon-upload result attaches to `plugins.icon`
  - the skill upload/parse/create/reupload pipeline persists into `plugins` via
    the unified import path (new skills land in `plugins`, not the old skills
    table)
  - `_probe` is a stateless network probe — no data source, unchanged
- Decision B — the legacy per-type admin CRUD routes (`/admin/mcps`,
  `/admin/experts`, `/admin/squads`, `/admin/skills`) are **retained this round**
  (not deleted). octo-admin stops calling their CRUD; they read the old tables
  and therefore show stale data, which is acceptable transitional state because
  no client calls them. Their removal is a separate follow-up.

## In scope

Marketplace (`feat/unified-plugin-admin-surface`):
- `internal/service/plugin/admin.go` — AdminList/Detail/Create/Update/Delete for
  all four plugin types + admin category listing.
- `internal/repository/plugin/admin_category.go` — `ListAdminCategories`.
- `internal/service/plugin/categories.go` — re-add `AdminListCategories`.
- `internal/service/plugin/service.go` — re-add `writeScope` + the `admin`
  parameter on `buildWrite`/`buildRelations` (keeping the current secretscan-free
  body); `createWithID`/`Update` pass `admin=false`.
- `internal/api/handler/plugin/admin.go` — `/api/v1/admin/plugins*` +
  `/api/v1/admin/plugin_categories`.
- `internal/api/router/router.go`, `cmd/marketplace-api/main.go` — wiring.
- Supporting-endpoint data-source swap (Decision A).
- Tests (service + handler, negative cross-Space/role) + regenerated OpenAPI
  (`admin_plugin` tag).

octo-admin (`feat/unified-plugin-admin`):
- Repoint Expert (专家) and Expert-Team (专家团) admin CRUD to `/admin/plugins`
  (`plugin_type=expert`/`expert_team`), matching the MCP/Skill pattern in
  `d53496b`; verify MCP/Skill; keep A-class supporting calls.

## Out of scope

- Deleting the legacy per-type admin CRUD routes/service/repo (deferred, per B).
- Any change to PR #67 / `feat/unified-plugin-backend`.
- The tenant `/api/v1/plugins/*` surface.
- Placement lifecycle redesign for admin-created rows (the original split
  rationale); admin rows follow the existing convention visibilities.

## Acceptance

- `go build ./...`, `go vet ./...`, `gofmt -l` clean, `go test ./...` green;
  `make openapi-check` four-gate passes with the `admin_plugin` paths present.
- Negative tests: non-admin caller is rejected at the gate; admin UPDATE does
  not change a row's visibility/Space/owner; cross-Space admin read/write works
  under `Scope.Admin`.
- octo-admin: `tsc --noEmit` clean, vitest green; MCP/Skill/Expert/Expert-Team
  admin CRUD all hit `/admin/plugins`; A-class supporting calls still function.
- Ship order documented: marketplace admin surface ships before octo-admin.
- `feat/unified-plugin-backend` ref is byte-unchanged throughout.
