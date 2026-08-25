---
type: Task
title: "Task: unified plugin backend"
description: Introduce the unified Plugin persistence model and API as the new Marketplace authority.
tags: ["plugin", "marketplace", "migration", "api"]
timestamp: 2026-08-19T16:26:00+08:00
slug: unified-plugin-backend
source: self
---

# Task: unified plugin backend

## Goal

Implement the confirmed unified Plugin database model and backend API so Expert,
Expert Team, Skill, and Connector pages can consume one new contract. The new
Plugin tables become the authoritative storage after historical data migration;
the frontend will use the new data directly and does not require old URL or DTO
compatibility after cutover.

## Load-bearing behavior

- Identity and Space come only from verified request context; request bodies
  cannot choose owner, operator, or Space.
- Every tenant-owned query includes explicit caller and Space visibility scope.
- Cross-Space and inaccessible resource lookups return `NOT_FOUND` without
  revealing existence.
- `plugins` stores the current state for `expert`, `expert_team`, `skill`, and
  `connector`.
- Team members and embedded Expert/Member Skills are ordinary Plugin rows;
  composition is represented by `plugin_relations`.
- Published Plugin versions are immutable and audit records are append-only.
- The confirmed audit schema stores one action snapshot pair, not separate
  before/after JSON documents: create, update, publish, duplicate, and import
  store the resulting state; delete stores the last state before deletion.
  `before_hash` and `after_hash` describe the transition and do not imply that
  two JSON snapshot pairs exist in the row.
- Point placement is represented by `placement_code`, not separate scene/slot
  columns. Category and Plugin placement records carry visibility and order.
- Connector package/version/audit data must not persist or log secret values;
  this invariant is enforced client-side (see the secret-handling divergence
  record), not by a backend value scanner.
- Existing legacy routes and tables continue to operate during backend rollout;
  no long-term dual-write or compatibility layer is introduced.
- API success and error envelopes follow the repository OpenAPI standard.
- The confirmed cowork-v3 design contract is authoritative for Plugin wire DTOs:
  `plugin_name`, `plugin_type`, `manifest_json`, `plugin_json`, `relation_type`,
  and the `{data:{plugin,relations}}` detail shape. The generated OpenAPI must
  implement and document that contract exactly; the former shortened REST field
  spellings are not retained as aliases.

## In scope

### New authoritative tables

- `plugins`
- `plugin_relations`
- `plugin_versions`
- `plugin_audit_logs`
- `plugin_categories`
- `plugin_category_placements`
- `plugin_placements`

### Retained runtime tables

- `parse_tasks`
- `resource_metrics`
- `resource_metric_flushes`
- `gorp_migrations`

### Plugin API

The confirmed cowork-v3 design artifacts are the authoritative new client
contract. Implement their `/internal/plugins/*` facade routes and DTO names
without adding shortened-field compatibility aliases. In particular:

- Detail uses `GET /internal/plugins/detail?plugin_id=...&include_relations=true`.
- Create/update uses `POST /internal/plugins/upsert` with `{plugin,relations}`.
- Actions/history use `/internal/plugins/duplicate`, `/publish`, `/audit_logs`,
  and `/archive`.
- Attachments use `/internal/plugins/attachment/upload` and
  `/internal/plugins/attachment/download` with server-derived identity.
- Connector probing remains `POST /connectors/_probe`.
- Placement-aware discovery remains `GET /plugins` and
  `GET /plugin_categories`, using `scene_code`, `plugin_type`, `q`, `page`, and
  `page_size` as designed.
- Wire fields use `plugin_name`, `plugin_type`, `manifest_json`, `plugin_json`,
  and `relation_type`.
- Manifest and Package conform to `cowork-plugin-manifest-1.0.json` and
  `cowork-plugin-package-1.0.json`; each Package includes a canonical
  `manifest.json` attachment identical to the outer Manifest.

## Out of scope

- Frontend implementation or aliases for the superseded shortened REST DTOs.
- Preserving old Expert, Squad, Skill, or MCP backend URLs after frontend cutover.
- Removing legacy routes or dropping legacy tables in the initial backend rollout.
- Long-term new/legacy dual-write.
- New publication, secret-binding, tag-dictionary, or display-config tables.
- A Plugin-specific install-to-Loop API; the frontend may orchestrate existing
  Fleet capabilities.
- A separate tag suggestion or Plugin parse-task API added only to mimic old UI.

## Acceptance criteria

- Ordered embedded MySQL migration creates the seven confirmed tables with
  appropriate keys, indexes, soft-delete semantics, and reversible down steps.
- Repository methods implement scoped list/get/upsert/delete, relation graph,
  immutable versions, audit history, categories, and placements.
- Service validation enforces Plugin types, JSON size/shape, ownership,
  visibility, immutable version rules, and Connector secret redaction.
- New handlers are registered under `/api/v1`, use standard envelopes and fixed
  error codes, and include complete swag annotations.
- Cross-Space negative tests, ownership tests, relation tests, version
  immutability tests, and migration tests pass.
- `make openapi-check`, `make openapi-diff`, and `go test ./...` pass, or any
  pre-existing/tooling blocker is documented precisely.

## Implementation divergences (recorded post-implementation)

These deliberate deviations from the pre-implementation brief above ship in
PR #67; this note is the decision record the brief's earlier wording predates.

- **Routes are public, not `/internal/`.** Handlers mount `/api/v1/plugins/*`
  (not `/internal/plugins/*`). The `duplicate`, `audit_logs`, `archive`, and
  `attachment/upload|download` actions are out of scope for this PR; only
  `publish` shipped.
- **No `manifest.json` package attachment.** The package carries the outer
  Manifest via the row's `manifest_json` column, not a duplicated in-package
  attachment; the backfill/repackage path strips any such attachment.
- **Skill package layout is a flat attachment tree** (one attachment per file;
  text inline as `raw`, binary/oversize as own-Space `storage`), replacing the
  earlier `skill/ref.json` + `skill/package.zip` shape. Legacy pointers survive
  only on backfilled rows until `expand-skills` rewrites them.
- **Admin surface split to a follow-up.** An `/api/v1/admin/plugins*` surface
  was prototyped but removed from this PR (commit `84620e9`) and moved to the
  `feat/unified-plugin-admin-backend` branch, to land as a focused follow-up
  once the placement lifecycle for admin-created rows is designed. This PR ships
  the tenant catalog + migration only; the repo-layer `Scope.Admin` mechanism is
  retained for that follow-up.
- **IDs are opaque UUIDv7**, not the ULID/prefixed forms some older docs
  describe; see `internal/service/plugin/id_boundary.go` and the banner in
  `docs/api/plugin-id.md`.
- **Secret handling is client-side, not a backend scanner.** An earlier
  iteration ran a heuristic secret-value scanner (`internal/secretscan`, wired
  into the service write path and a repository persistence guard) that rejected
  connector writes carrying credential-shaped values. It was removed: string
  heuristics cannot reliably separate a secret from ordinary configuration
  (regions, timezones, locale codes, version tags), and placing that
  classification on a hard write gate produced oscillating false
  positives/negatives. The connector form's "需要用户自行配置" (user-supplied)
  toggle is now authoritative — toggled keys are written as `${PLACEHOLDER}`
  references so no real secret reaches persistence, and the backend makes no
  value judgment. The `must not persist secret values` invariant above still
  holds; it is enforced at the point of entry (the frontend) rather than by a
  backend guess.
