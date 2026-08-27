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
- Type conventions: admin CREATE stamps `visibility=system` for every plugin
  type — connectors also get a NULL Space; skill/expert/expert_team stay in the
  empty global Space. (Correction, owner directive 2026-08: an earlier draft said
  skill/expert/expert_team CREATE stamped `visibility=public`; the code stamps
  `system` via `conventionVisibility` — the unified "全平台可见" value. The
  `model.PluginVisibilityPublic` constant is kept only for back-compat.) Admin
  UPDATE **preserves** the row's existing visibility, Space, owner, and creator
  provenance — a metadata edit must never force-publish a tenant-private plugin,
  and owner/creator are immutable.
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

> **Decision A amended (2026-08, 3rd review round): legacy skill supporting
> endpoints are FROZEN, not swapped — the unified replacements are NEW paths.**
> The brief above overstated the swap. The code (and this record) reconcile as:
> - The retained legacy admin skill helper `/api/v1/admin/skills/{id}/skill_md`
>   (and the tenant-side skill download helper) are **frozen against the legacy
>   `skills` table**: they serve legacy rows only and are not repointed at
>   `plugins`. They exist purely so pre-unification skill IDs keep resolving.
> - The unified replacement is the **NEW** `/api/v1/admin/plugins/{id}/skill_md`
>   and `/api/v1/admin/plugins/{id}/download`, served by the plugin admin handler
>   from `plugins.plugin_json` (the flat attachment tree). octo-admin points at
>   these for unified rows. Router-admission tests now cover both new routes.
> - Category writes / tag listing / icon-upload / the skill import pipeline ARE on
>   `plugins`/`plugin_*` as stated above — only the two skill read helpers are
>   frozen-legacy rather than swapped.
> - Container import/reupload dedupe (P2-3): a bundled skill referenced by several
>   refs under the SAME (file,name) collapses onto one embedded node; the SAME file
>   under DIFFERENT display names mints a distinct per-parent-copy node per name
>   (each with its own id/name/object namespace), and the shared archive's
>   decompression budget is charged once per distinct file. The dedupe is a pure
>   resource optimization, never a cross-parent shared node.
- Decision B — the legacy per-type admin CRUD routes (`/admin/mcps`,
  `/admin/experts`, `/admin/squads`, `/admin/skills`) are **retained this round**
  (not deleted). octo-admin stops calling their CRUD; they read the old tables
  and therefore show stale data, which is acceptable transitional state because
  no client calls them. Their removal is a separate follow-up.

> **Decision B superseded (owner directive, 2026-08): legacy per-type admin CRUD
> retired this round.** The per-type admin CRUD (`/admin/experts`,
> `/admin/squads`, `/admin/expert_categories`, `/admin/expert_tags`,
> `/admin/expert_skill_uploads`, `/admin/skill_categories`, and the MCP
> `/admin/mcps` CRUD) has been deleted in this PR, not deferred — an explicit
> owner directive supersedes Decision B above. Only the unified
> `/admin/plugins*` + `/admin/plugin_categories` surface remains, plus the
> retained MCP `_probe`/icon routes and the skill `skill_md` / upload-parse /
> download helpers. Contract docs (mcp-v1 §9, expert-v1 §9, README,
> CONFIGURATION) were updated to match.

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
