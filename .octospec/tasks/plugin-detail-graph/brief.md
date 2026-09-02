---
type: Task
title: "Plugin: detail_graph aggregated endpoint"
description: Add `GET /plugins/detail_graph` returning a Plugin root together with the flat, deduplicated transitive closure of its relation graph, collapsing the frontend's 1+N+N×M `/plugins/detail` fan-out into one call.
tags: ["plugin", "api", "performance"]
timestamp: 2026-09-02T11:30:00+08:00
slug: plugin-detail-graph
source: self
---

# Plugin: detail_graph aggregated endpoint

## Goal

Rendering an Expert Team detail page on octo-web today costs `1 + N + N×M` calls
to `GET /plugins/detail` (root → N members → M skills/connectors per member).
Each response carries the heavy `plugin_json` blob and full-normalizes every
JSON field, forcing the frontend to stitch the tree client-side. This task
adds a new endpoint `GET /plugins/detail_graph?plugin_id=…` that returns the
root (full projection, byte-identical to today's `/plugins/detail`) plus every
edge in the transitive closure and a deduplicated list of related plugins in
light projection (manifest only, no `plugin_json`). The existing
`/plugins/detail` endpoint is preserved byte-for-byte; no migration, no model
change, no admin surface change in this task.

## Load-bearing behavior

- New endpoint is **additive**: `GET /plugins/detail_graph`,
  `@ID plugin.graph.get`, mounted on the public plugin router (tenant-scoped,
  Bearer auth). Route naming follows the existing verb-suffix, query-param-id
  convention on the public surface.
- Response is a **flat** (not nested) envelope:
  ```
  { data: {
      plugin:         pluginResponse,         // full, same as /plugins/detail
      relations:      relationResponse[],     // closure of all edges, data intact
      related_plugins: listItemResponse[]     // light, deduped, no plugin_json
  } }
  ```
  No `graph`/`node_count`/`is_partial`/`truncated` meta object is emitted.
- Traversal depth is fixed by the relation matrix and derived from the root
  plugin's type — no `depth` query param:
  - `skill` / `connector` (leaves): zero edge queries beyond the root fetch.
  - `expert` (container of skills/connectors): one level of edges.
  - `expert_team` (container of experts): up to two levels (members + their
    skills/connectors).
- The endpoint performs a fixed **3–4 SQL round-trips** regardless of fan-out:
  root fetch → level-1 edges → (optional) level-2 edges → one batch payload
  fetch for all related nodes using `pluginSummaryColumns` (which deliberately
  omits `plugin_json`, reusing an existing column list).
- **Node cap 500** (matching `maxInstallRelationTargets`). Exceeding returns
  HTTP 413 `PAYLOAD_TOO_LARGE` with `details.max_nodes` populated; the endpoint
  fails closed rather than silently truncating, so the UI never renders a
  partial squad.
- **Hidden related plugins are silently omitted** — edge and node both —
  matching the existing `/plugins/detail` behavior. No truncation flag, no
  error. This is deliberately different from the install path's fail-closed
  `ErrDependencyHidden`; install needs to refuse a partial provision, but a
  read-only detail page should degrade gracefully when cross-space targets
  exist.
- **Embedded (`is_embedded=1`) children resolve under their container's
  visibility**: if you can read a plugin you can read its bundled parts.
  Standalone (`is_embedded=0`) children remain strictly caller-scoped via the
  existing `visibilitySQL`. Defense-in-depth is provided by the batch read's
  embedded-ID whitelist, so the read can never widen to arbitrary embedded
  IDs outside the edge closure just derived from an authorized root.
- Icons are resolved per-request with memoization by raw icon key (no extra
  allocations for shared icons). Member counts for team-typed nodes are filled
  in-memory from the edge slice rather than issuing an extra
  `CountMemberRelations` query (which would over-count hidden members).
- View/install/download counters are read-only projections on the existing
  read path; detail reads never write metrics, so fanning out to related
  plugins cannot bump their view counts.
- The edge-query SQL for embedded children does NOT additionally require a
  `space_id` match: admin-created system-visibility containers own their
  embedded children in the global (NULL) space, and the write-path gate
  `lockRelationTargets` already rejects cross-container embedded edges, so
  imposing a space_id predicate would incorrectly hide legitimate embedded
  members from a caller who can see their system-visible parent. The node
  payload query's defense-in-depth predicate relies on the embedded-ID
  whitelist derived from the authorized edge set, not on a space check.
- The new repo primitive is not generalized to an arbitrary-ID batch read;
  `GetGraphClosure(ctx, scope, rootID string)` takes a single root ID so the
  embedded-child visibility relaxation cannot be driven by caller-supplied ID
  lists. The admin route for the same shape is intentionally deferred (repo
  works under `scope.Admin = true` as-is; handler veneer can be added when the
  admin console needs it). The install path is deliberately not refactored in
  this task — it is fail-closed and needs full `plugin_json` per node; sharing
  the primitive would require a visibility-mode and package-blob toggle that
  are separate concerns.
- API envelope and error codes use the fixed OCTO enum; swaggo annotations
  follow the project house style; `make openapi-check` must pass.

## Out of scope

- Admin surface (`/admin/plugins/:id/detail_graph`) — left for follow-up.
- Install-path refactor.
- Any new migration, model field, plugin type, or relation type.
- Changing `/plugins/detail` shape or behavior.
- Re-adding any backend-side secret value scanning.

## Acceptance

- New endpoint `GET /api/v1/plugins/detail_graph?plugin_id=…` returns 200 with
  the flat graph envelope.
- Root plugin byte-matches `/plugins/detail`'s plugin projection (including
  `plugin_json`). Related plugins do not carry `plugin_json` (verified by
  wire-JSON assertion in handler tests).
- For a system team in the local dev DB, the endpoint returns its 4 embedded
  members plus reachable embedded skills with a single HTTP call; children do
  not carry `plugin_json`.
- Leaf (skill/connector) roots return empty edges/related with no edge query
  issued; expert roots issue one edge query; expert_team roots issue two.
- Cross-space standalone children are silently omitted (edge + node both);
  embedded children of an authorized root are returned regardless of their
  stored `space_id`.
- Node cap at 500 enforced before the payload query fires; over-cap returns
  413 with `details.max_nodes`.
- Missing `plugin_id` → 400; unknown id → 404; unauthorized → 401 (exercised
  by handler tests).
- `go vet ./...`, `gofmt`, `make build`, `go test ./internal/...`,
  `make openapi-check` (with regenerated `docs/openapi/swagger.yaml`
  committed) all pass.
- No regression: existing `/plugins/detail` returns unchanged shape and
  relation counts; full `go test ./...` green.
