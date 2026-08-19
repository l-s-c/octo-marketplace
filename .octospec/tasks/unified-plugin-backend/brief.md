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
- Point placement is represented by `placement_code`, not separate scene/slot
  columns. Category and Plugin placement records carry visibility and order.
- Connector package/version/audit data must not persist or log secret values.
- Existing legacy routes and tables continue to operate during backend rollout;
  no long-term dual-write or compatibility layer is introduced.
- API success and error envelopes follow the repository OpenAPI standard.

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

- Detail, upsert, duplicate, audit list, archive, attachment upload/download.
- Placement-aware category list and Plugin list.
- Publish immutable version and placement configuration.
- Delete, Connector probe, and Plugin version list.

## Out of scope

- Frontend implementation or compatibility adapters for old page DTOs.
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
