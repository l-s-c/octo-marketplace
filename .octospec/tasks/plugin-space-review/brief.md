---
type: Task
title: "Task: plugin space review"
description: Space-level review workflow — members create private plugins freely, submit for Space visibility, Space admins approve (via web or IM notification card) before the plugin is listed in the org market.
tags: ["plugin", "marketplace", "review", "approval", "visibility", "im", "card-action"]
timestamp: 2026-08-31T12:00:00+08:00
slug: plugin-space-review
source: self
---

# Task: plugin space review

## Goal

Introduce an organization (Space) level review workflow on the unified plugin
surface. Members create and self-test `private` plugins without friction; the
act of requesting Space-wide visibility creates a review request that a Space
admin (owner/admin) processes either through a new "组织管理" page in the
market **or directly inside an approval card delivered to their DM by the
"通知助手" bot**. Only an approved request makes the plugin (or a new version
of an already-listed plugin) visible in the org market. Platform-wide
(`system`) publication and its octo-admin review queue are explicitly out of
scope for this task, but the model reserves room for them.

Confirmed product decisions (owner, 2026-08-31):

- **Trigger**: `private` create/self-test is free and unreviewed; submitting
  for Space visibility is what enters the review queue.
- **Version re-review**: a new version of an already-listed plugin must be
  re-reviewed; the currently listed version stays live during review.
- **Coverage**: all plugin types (`skill`, `connector`, `expert`,
  `expert_team`).
- **Version history is release history, not edit history**: personal edits are
  a mutable draft and never mint versions; a version is frozen at submit time
  and enters the org-facing version history only on approval. Rejected or
  canceled submissions never enter version history. Author edit history stays
  in `plugin_audit_logs`.
- **Manual version label**: the applicant types the version label in the
  submit form (pre-filled with a suggested bump, editable).
- **IM approval (2026-08-31 追加)**: on submit, the marketplace notifies all
  Space owners/admins via 通知助手 using an `approval_card` over
  `OCTO_CARD_ACTION_ROUTES`; the card carries 默认 approve/deny 双按钮
  (positive/destructive 配色). IM "拒绝" uses a hard-coded default reason
  (`管理员在消息卡片中拒绝,未填写原因`) and stamps the audit log with
  `decision_source=im`. 多管理员并发点击依赖接收方 CAS,第一个有效决策胜出,
  其他卡片各自渲染权威终态。

## Load-bearing behavior

### Review request model (new table `plugin_review_requests`)

Review state lives on an independent request entity, NOT on a plugin status
column. Rationale: version iteration stays coherent (listed v1 and in-review
v2 coexist), the same model later serves platform-wide review via
`target_scope`, and it cannot violate the existing invariant that an admin
metadata edit never changes a tenant plugin's visibility.

Columns (uuidv7 id, standard timestamps, soft delete):

- `plugin_id`, `space_id`
- `target_scope` — fixed `space` in this task; `system` reserved.
- `status` — `pending` / `approved` / `rejected` / `canceled`.
- `kind` — `first` (initial listing) / `upgrade` (new version of a listed
  plugin); derived server-side, never client-supplied.
- **Frozen submission snapshot**: `version` (applicant-typed label),
  `changelog`, `manifest_json`, `plugin_json`, `manifest_hash`, `plugin_hash`.
  The snapshot is stored on the request, NOT appended to `plugin_versions` at
  submit time — `plugin_versions` remains exactly the org-visible release
  history, and a rejected label never occupies the `(plugin_id, version)`
  unique key (so a fixed resubmission may reuse the same label).
- `applicant_uid/name`, `reviewer_uid/name`, `reason` (required on reject),
  `decision_source` (`web` default / `im` when the decision came from the
  notification card), `submitted_at`, `reviewed_at`.

Constraints: at most one `pending` request per `plugin_id` (enforced by a
database partial unique index or transactional `SELECT ... FOR UPDATE` +
1062→`ErrConflict`, following the existing `20260720-01-category-live-name-unique.sql`
pattern for soft uniqueness); version label is required, format-checked, and
must not collide with an already-published version of that plugin.

### State machine

- **Upload lands private.** The tenant import path no longer force-publishes
  Space-visible: `importConsumedTask`
  (`internal/service/plugin/import.go:193-224`) currently forces
  `visibility=space` + a visible default placement immediately — this changes
  to land as `private` (owner-visible draft, self-testable). The reviewed
  submit is the only path from `private` to `space`.
- **Submit** (owner-only): freezes the current draft content plus the typed
  version/changelog into a `pending` request. Draft edits after submit do not
  mutate the frozen snapshot; the admin reviews exactly what will be listed.
  After the submit transaction commits, marketplace **best-effort** dispatches
  an IM approval card to Space admins (see IM section below); notification
  failure is logged and never rolls back the submit.
- **Approve** (source ∈ {web, im}): in one transaction — append the frozen
  snapshot as a `plugin_versions` row, move `current_version_id`, apply the
  snapshot to the `plugins` row, and for `kind=first` flip `visibility`
  `private→space` and attach the visible `default` placement. For
  `kind=upgrade` the listed content/version swaps atomically;
  visibility/placement are already in place. Writes a `plugin_audit_logs` row
  with action `review_approve` and `remark` carrying `decision_source=<src>`.
  On IM-sourced approve the callback response sets `requester_uid` so
  octo-server's `StandardActionFinalizer` DMs an applicant result card.
- **Reject** (source ∈ {web, im}): `reason` required on web; on IM reject the
  server fills in `defaultIMDenyReason` (`"管理员在消息卡片中拒绝,未填写原因"`)
  — the card surface does not support text input. The plugin stays as-is
  (private draft, or the old version stays listed). The author may edit and
  resubmit. Writes `plugin_audit_logs` with action `review_reject` and
  `decision_source=<src>`.
- **Cancel**: the applicant may cancel a `pending` request. Cancel is web-only
  (no IM cancellation button on the card).
- **Space-admin self-submit auto-approves** but still creates the request row
  (reviewer = self, `decision_source=web` unless the self-approve happens via
  IM) so the audit trail and lists stay uniform.
- **Grandfathering**: existing Space-visible plugins are treated as approved;
  no retroactive requests, no backfill.
- **Containers**: `expert`/`expert_team` review at the top container; embedded
  children follow the container and are never independently reviewable.
- **Concurrency**: two admins approving/rejecting concurrently (web or IM)
  produces at most one winning decision. The repository uses
  `SELECT ... FOR UPDATE` on the request row plus a state-CAS predicate
  (`WHERE status='pending'`) so the second committer hits `ErrConflict` and
  surfaces the already-committed outcome; for IM, the card-action receipt
  table (`card_action_receipt`) provides per-`event_id` idempotency per the
  octo-server consumer contract.

### Authorization prerequisite (cross-repo)

The marketplace does not know per-Space roles today; this task requires it.

- **octo-server `space_roles` on verify** (already in scope): extend
  `POST /v1/auth/verify?include=context` (handler `authVerifyToken`,
  `modules/user/api.go:4728`) with a `space_roles` map.
  `queryUserSpaceContext` (`modules/user/api.go:4838`) already reads
  `space_member sm` and only needs to also select `sm.role`. Wire encoding:
  `space_roles: { "<space_id>": <0|1|2> }`.
- **octo-server NEW internal Space-admin list endpoint**:
  `GET /v1/internal/spaces/:space_id/admins` (recommended location:
  `modules/space/`, NOT `modules/internal_resolve/` which has route-shape
  source-string assertions). Wraps existing `db.queryAdminsAndOwner`
  (`modules/space/db.go:672`, `status=1 AND role>=1`), returns
  `{admins:[{uid,name,role}]}`. Auth uses a dedicated
  `X-Internal-Token` bound to new env `OCTO_MARKETPLACE_ADMIN_LIST_TOKEN`
  (≥32 bytes), constant-time compare, per-IP rate limit, registered into
  `ValidateNotifyTokenExclusions` in `main.go` so it cannot collide with
  notify/docs/bot-mention/drive tokens.
- **octo-marketplace**:
  - `model.Identity` gains `SpaceRoles map[string]int` to consume the
    `space_roles` map from the verify response.
  - Reviewer-side endpoints enforce server-side that the caller's role in the
    request's Space is admin-or-owner (`role >= 1`). Encoding is normalized
    to the server's `space_member.role` values — 0=member, 1=admin, 2=owner;
    reviewer check is `role >= 1`. (octo-web displays the inverse
    1=owner/2=admin/3=member; the frontend gate is cosmetic only.)
  - Cross-Space access to a request returns `NOT_FOUND` without revealing
    existence, per the unified-plugin-backend brief.

### IM notification & approval card (new)

**Topology**: marketplace calls octo-server `POST /v1/internal/notify` with
`approval_card`; octo-server delivers DMs from the global `notification` bot
to every admin and, when a decision lands, its `StandardActionFinalizer`
mutates that admin's card to the terminal state and sends a separate
octo/v1 result card to the applicant (free — no marketplace work). All
traffic goes through the existing octo-server card-action dispatch pipeline
(`OCTO_CARD_ACTION_ROUTES`); marketplace hosts the callback endpoint.

**octo-server deployment config (no code change, route-only)**:
`OCTO_CARD_MESSAGE_ENABLED=true` plus one entry in `OCTO_CARD_ACTION_ROUTES`:

```json
{
  "sender_uid": "notification",
  "owner": "marketplace",
  "action_type": "marketplace.plugin_review.decision",
  "url": "http://<marketplace-internal>/v1/card-actions/decide",
  "secret_env": "OCTO_MARKETPLACE_CARD_ACTION_SECRET",
  "notify_token_env": "OCTO_MARKETPLACE_NOTIFY_TOKEN",
  "timeout_ms": 3000,
  "max_attempts": 5,
  "base_backoff_ms": 1000,
  "max_backoff_ms": 60000,
  "max_in_flight": 8
}
```

Secrets (`OCTO_MARKETPLACE_NOTIFY_TOKEN`, `OCTO_MARKETPLACE_CARD_ACTION_SECRET`,
`OCTO_MARKETPLACE_ADMIN_LIST_TOKEN`) are ≥32 bytes, pairwise distinct, and
distinct from `NOTIFY_INTERNAL_TOKEN`/`OCTO_DOCS_NOTIFY_TOKEN`/
`OCTO_DOCS_BOT_MENTION_TOKEN`/`OCTO_DRIVE_INTERNAL_TOKEN`; startup fails on
collision (uses existing `ValidateNotifyTokenExclusions`).

**marketplace outgoing client** (`internal/notify/`, pattern copied from
`internal/fleet/client.go`): 3s `http.Client`, `CheckRedirect` rejection,
`io.LimitReader` response bound, typed `APIError`, no retries. Two methods:
`ListSpaceAdmins(ctx, spaceID) ([]AdminInfo, error)` against the new
`/v1/internal/spaces/:space_id/admins` endpoint; `NotifyReviewCard(ctx, req)`
against `/v1/internal/notify`. Call sites MUST inspect the response
`delivered[]` AND `filtered{uid:reason}`; logging is mandatory but never
bubbles up to fail a submit.

**Dispatch timing & reliability**: notification fires **after** the submit
transaction commits, using the same best-effort shape as
`internal/service/expert/install.go:109-131` (trackInstall):
`context.WithoutCancel(ctx)` + tight (2s) deadline, failure is Warn-logged
and dropped. No outbox, no retry queue, no queue table in v1 — if the
best-effort send fails, admins still see the request in the "组织管理" web
queue.

**Card payload** (server template owned by octo-server `pkg/cardtmpl`;
marketplace only supplies bounded fields):

- `space_id`, `service="marketplace"`, `targets=<admins from ListSpaceAdmins>`,
  `actor_uid=<applicant_uid>`
- `approval_card`:
  - `action_type`: `"marketplace.plugin_review.decision"`
  - `title`: `"插件上架申请 · {plugin_name}"` (≤200 runes after truncation)
  - `description`: assembled as
    `"类型：{type} · 申请人：{applicant_name}\n版本：{version}（{first: 新上架 | upgrade: 当前 v{current_version}}）\n{changelog 前缀}"`
    (≤300 runes)
  - `actions: null` (use the server-default localized 允许/拒绝 buttons with
    `style: positive` / `style: destructive`; never construct custom actions
    because they drop style per `approval_request.go:194-199`)
  - `data`: `{ "review_id": "<uuid>", "plugin_id": "<id>" }` only; keys
    lower-case, ≤32 entries, values ≤500 runes. `owner`/`action_type`/
    `decision` are reserved and injected server-side.
- Content rules: all strings go through the server's `escapeMarkdown`; no
  markdown, no URLs, no images (the approval template body is only two
  `TextBlock`s, there is no FactSet/Image/OpenUrl — no "查看详情" link is
  possible on this card).
- marketplace does NOT inspect or construct `type=17` AdaptiveCard JSON
  (doing so is rejected as `err.server.notify.card_not_allowed`).

**Callback endpoint** (`POST /v1/card-actions/decide`, mounted OUTSIDE the
tenant `Authenticator`):

- Raw body middleware (mirror octo-server consumer doc §"Run a raw-body
  middleware before any JSON parser"); HMAC-SHA256 verification mirrors
  `internal/cardactiondispatch/signature.go` verbatim (33 lines). Canonical
  string: `"v1\n{POST}\n{escapedPath}\n{X-Octo-Timestamp}\n{X-Octo-Event-ID}\n{sha256hex(rawBody)}"`;
  signature header `X-Octo-Signature: v1=<hex>`; timestamp freshness
  ±300s; `body.event_id` must equal `X-Octo-Event-ID`; verify first, parse
  second; constant-time compare.
- Request body unmarshals as octo-server `DecisionRequest` (`event_id` uses
  `json:"event_id,string"`). `operator_uid` is an identity assertion, NOT an
  authorization grant — marketplace re-checks that the operator has role≥1
  in `space_id` via the resolver (cached by `auth.CachedResolver`),
  otherwise return `{"disposition":"forbidden","state":"pending"}`
  (HTTP 200, per protocol).
- Idempotency + ordering follows the consumer doc transaction template
  exactly:
  1. `BEGIN`
  2. `SELECT stored_response FROM card_action_receipt WHERE event_id=?`; if
     found, `COMMIT` and replay that stored_response verbatim.
  3. Validate `decision` is `"approve"` or `"deny"`; reject other values
     with `{"disposition":"forbidden","state":"pending"}` (the reserved
     decisions approve/deny are produced by the default button set).
  4. `SELECT ... FROM plugin_review_requests WHERE id=? AND status='pending' FOR UPDATE`;
     if not found or not pending → `disposition:"conflict"` (already decided)
     with the actual final `state` and `requester_uid`.
  5. Re-verify operator is still a reviewer of that Space.
  6. Apply the approve/reject service logic (reuse the same tx path as the
     web handlers; reject fills `defaultIMDenyReason` and stamps
     `decision_source=im`).
  7. `INSERT INTO card_action_receipt(event_id, stored_response_json, created_at) VALUES (...)`.
  8. `COMMIT`.
  9. Return the stored response. UNIQUE-key collisions must re-read and
     replay the winner's response.
- Response is exactly the 4-field `DecisionResult`
  (`disposition`/`state`/`requester_uid`/`display`), `DisallowUnknownFields`;
  `state ∈ {approved,denied}` requires `requester_uid` set to
  `applicant_uid` so the octo-server finalizer can DM them; `display.title`
  carries the plugin name for the terminal card header.
- HTTP status semantics per protocol: 2xx with typed body for all handled
  outcomes (including business rejection); 408/429/5xx for transient failure
  (octo-server retries with backoff); other 4xx goes to DLQ (avoid except
  for truly permanent errors).
- The `card_action_receipt` table stores `event_id` as a decimal-string
  `VARCHAR(32) PRIMARY KEY` (int64 over JS Number.MAX_SAFE_INTEGER), plus
  `stored_response_json`, `created_at`; no replay UI, ops use octo-server's
  `tools/card-action-dlq` for replay.

**Not in v1 (called out explicitly)**:

- Card "查看详情" / Action.OpenUrl button — not supported by
  `BuildApprovalRequestCard`, which only emits `Action.Submit`
  (`pkg/cardtmpl/approval_request.go:194-199`); admins use the web "组织管理"
  queue for deep review.
- Card-wide auto-invalidation when one admin decides — each admin gets an
  independent DM card; later clicks hit CAS conflict and render "该申请已由
  {first-decider} 处理: {approved|denied}" as the terminal state.
- Proactive "you were approved/rejected" push on web-side decisions — only
  IM decisions trigger the automatic applicant result card (for free via
  `StandardActionFinalizer`). Web decisions do not send an extra IM in v1.
- Per-admin delivery failure alerts beyond the server log; the web queue is
  the source of truth.
- Inline reject-reason input — approval cards only support
  `Action.Submit`, not `Input.Text`; the brief for docs approval used
  inputs, but the generic approval-card template for non-docs owners does
  not.

### API surface (tenant, `/api/v1/plugins`, existing `Authenticator`)

- `POST /plugins/review_requests` — submit `{plugin_id, version, changelog}`;
  snapshot and `kind` derived server-side.
- `GET /plugins/review_requests?mode=mine|space&status=&page=&page_size=` —
  `mine` is applicant-scoped; `space` requires the Space reviewer role.
- `GET /plugins/review_requests/:id` — detail incl. plugin/version summary
  and snapshot preview (e.g. SKILL.md).
- `POST /plugins/review_requests/:id/approve`
- `POST /plugins/review_requests/:id/reject` — `reason` required for web
  calls; IM callback injects the default.
- `POST /plugins/review_requests/:id/cancel` — applicant only.

Internal card-action endpoint (separate route group, NO `Authenticator`):

- `POST /v1/card-actions/decide` — HMAC-signed callback from octo-server
  (see IM section above).

Standard `{data}` / `{error}` envelopes, fixed OCTO error codes, `$octo-api`
workflow (`make openapi-check`, `make openapi-diff`) apply.

### Frontend (octo-web, implemented in the `dmworkskillmarket` package)

- **Navigation / Tabs**: `SkillListPage` (`packages/dmworkskillmarket/src/pages/SkillListPage.tsx`)
  extends `TabId` from `"skills" | "mine"` to `"skills" | "mine" | "org"`;
  the "org" tab renders a new review queue and is shown only when the
  current user's role in the active Space is owner/admin (read from the
  existing space-role model; note that `dmworkbase` uses the inverse
  encoding `role=1 → owner, role=2 → admin`; this is a cosmetic gate —
  server-side enforcement is authoritative). A pending-count badge is shown
  on the tab. (No `dmworkbase` sidebar/`MarketSidebar` change required in
  v1; the tab lives inside the existing market module.)
- **Skill card states** (`components/SkillCard.tsx`): render status badges
  derived from the join of `visibility` + the latest pending/rejected
  review: 私有 / 审核中 / 已拒绝·展示理由 / 已上架 / 已上架·新版审核中;
  action buttons 「提交组织审核」/「撤回申请」/「重新提交」/「发布新版本」
  are gated by the derived state.
- **New-Skill / Upload flow** (`components/NewSkillModal.tsx`): replace the
  hardcoded `visibility: "space"` final step with an explicit publish-scope
  choice (「仅自己可见(私有)」/「提交组织审核,通过后组织内可见」); the latter
  reveals editable `version` and `changelog` fields pre-filled from the
  parsed metadata and a version-bump suggestion. Submit in the second case
  calls `POST /plugins/review_requests`; success copy states "已提交,待审核".
- **"我的插件" tab**: shows the status badges above; rejected entries show
  the reject reason inline with a resubmit entry; pending entries show a
  cancel entry.
- **"组织管理" tab** (new, in `src/components/ReviewQueue.tsx`): two sub-tabs
  待审核 / 已处理; list rows show icon, name, type tag, 首次上架/版本更新
  tag, applicant + submitted time, rejected entries inline the reason; row
  actions: 「通过」(direct), 「拒绝」(opens a reason-required modal matching
  the current `DeleteConfirmModal`/`BotPublishModal` pattern); clicking the
  row opens a detail drawer with metadata, SKILL.md snapshot preview,
  upgrade callout ("当前在架 vX → 申请上架 vY"), and approve/reject-with-reason
  footer actions.
- **API client** (`api/skillApiReal.ts`): add functions for the six
  review endpoints (list/get/submit/approve/reject/cancel), plus a
  `useReviewRequests` hook in `hooks/` following the `useSkills` pattern.
- **Types** (`types/skill.ts`): add `ReviewStatus`, `ReviewKind`,
  `ReviewRequest` (camelCase) plus `RawReviewRequest` (snake_case wire
  form); no new visibility enum values; `Skill` does not gain a review
  status column (review is a separate entity, derived at query time).
- **i18n**: new keys under `skillMarket.review.*`; copy uses `t()` via the
  existing `useI18n()` in `SkillListPage.tsx:12`.
- **Identity**: read `space_roles` from the existing auth context; the
  role gate is a display hint only (server 404/403 on unauthorized
  accesses).

## In scope

- Migration for `plugin_review_requests` (+ down step), plus
  `card_action_receipt` for IM callback idempotency.
- Repository + service + handlers for the seven endpoints above (six
  tenant + one card-action callback), with the approve transaction
  described in the state machine and the CAS/idempotency behavior.
- Import-path change: tenant uploads land `private`, no auto-placement.
- Identity extension consuming octo-server `space_roles` (marketplace side)
  and the new internal admin-list endpoint (octo-server side).
- IM notification dispatch on submit (fail-open, best-effort) and the
  card-action callback endpoint with HMAC verification and transactional
  CAS.
- The octo-web market pages listed in the Frontend section above.
- Configuration on both sides for the three new marketplace secrets
  (`OCTO_MARKETPLACE_NOTIFY_TOKEN`, `OCTO_MARKETPLACE_CARD_ACTION_SECRET`,
  `OCTO_MARKETPLACE_ADMIN_LIST_TOKEN`) and the `OCTO_CARD_ACTION_ROUTES`
  entry.
- OpenAPI regeneration.

## Out of scope

- Platform-wide (`system`) publication requests and the octo-admin review
  queue (`target_scope=system` reserved; octo-admin untouched).
- Proactive IM notification to the applicant on web-side decisions (only
  IM decisions auto-notify via `StandardActionFinalizer`; follow-up to add
  a symmetric push when acting from the web).
- A custom terminal-card visual for marketplace reviews — uses the
  `StandardActionFinalizer` localized 申请已允许/已拒绝 copy; extending
  the finalizer registry for `owner=marketplace` is an octo-server change
  and explicitly deferred.
- Card auto-invalidation across recipients (one admin deciding does not
  mutate other admins' cards; later clicks return the CAS conflict
  terminal state via the callback).
- Card "查看详情" deep-link / `Action.OpenUrl` / reject-reason input on the
  card (not supported by `BuildApprovalRequestCard`).
- An org-wide "manage all Space plugins" console (delist/transfer) beyond
  the review queue.
- Backfilling review requests for existing listed plugins.
- Any change to the admin (`/api/v1/admin/*`) surface semantics.
- New per-message delivery guarantees beyond best-effort + server log; the
  web queue is the source of truth.

## Acceptance criteria

- Migration up/down tests; `go build ./...`, `go vet`, `gofmt` clean,
  `go test ./...` green.
- State-machine tests: submit freezes snapshot; post-submit draft edits do
  not leak into the reviewed snapshot; approve(first) flips
  visibility+placement and mints the version row; approve(upgrade) swaps
  listed content atomically while the old version was live until then;
  reject requires reason (web) or fills default (IM) and changes nothing
  else; cancel by applicant only; single-pending constraint; version-label
  collision with a published version rejected, reuse of a rejected label
  accepted.
- Authorization negative tests: non-member 404, member-but-not-admin cannot
  list `mode=space` or approve/reject (403/404 per enumeration rules),
  cross-Space request access 404, applicant-only cancel; card-action
  callback with an `operator_uid` that is no longer a reviewer returns
  `forbidden/pending` without mutating state.
- Tenant upload no longer auto-lists: fresh import is `private` with no
  visible placement.
- Card-action protocol tests: signature verification using the fixed test
  vector in `docs/card-action-callback-consumer.md:178-187` must pass in
  Go; request body without valid signature is rejected; stale timestamp
  (>300s skew) rejected; `body.event_id` vs `X-Octo-Event-ID` mismatch
  rejected; unknown fields in the response JSON cause validation failure
  (mirrors `DecodeDecisionResult` rules); concurrent callbacks with
  distinct `event_id` for the same `review_id` produce exactly one
  committed decision and the other returns the authoritative terminal
  result; duplicate `event_id` replays the stored response verbatim
  (including on a different process/instance); approved/denied responses
  set `requester_uid=applicant_uid`.
- Notification fail-open: submit succeeds and commits even when octo-server
  is unreachable, returns 5xx, or returns partial `filtered`; the failure
  path logs and increments a metric; successful submit with a reachable
  octo-server records delivered/filtered counts via logs/metrics.
- IM-reject audit: IM reject rows have `reason = defaultIMDenyReason` and
  audit `remark` contains `decision_source=im`; web reject keeps the
  caller-supplied reason and `decision_source=web`.
- `make openapi-check` and `make openapi-diff` pass with regenerated spec
  committed.
- Frontend smoke (manual or via the existing `market-browser-test.cjs`
  flow): tab visibility gated by role; submit from upload modal creates a
  pending request; "组织管理" queue shows pending count; approve/reject
  transitions propagate to the card badges on "我的插件".

## Prototype

`prototype.html` in this directory — self-contained mock covering upload
scope choice, submit with manual version + changelog, review queue (web),
reject-with-reason (web), resubmit, upgrade re-review with old version
staying live, **plus** a 右下角 "通知助手" floating panel simulating the IM
approval-card surface with 通过/拒绝 buttons, default reject reason,
applicant result receipt, and a cross-admin conflict state when another
admin has already decided.

## Implementation divergence record (marketplace backend, rebuilt 2026-09-01)

The sections above were written against an older version/placement model and an
octo-server contract that has since changed. Where they disagree with what
shipped, **what shipped wins**. Each item below is a deliberate decision, not an
oversight — do not "restore" any of them without revisiting the reasoning.

### 1. The gate is VISIBILITY ONLY. Placements are untouched.

The brief has approve "attach the visible `default` placement" and has the import
land a *hidden* placement. Neither happens.

The market list already applies two independent filters: an INNER JOIN on a
`visible=1` placement, AND
`(visibility IN ('public','system') OR (space_id=? AND (visibility='space' OR owner_uid=?)))`.
The second filter alone is a complete review gate: a `private` plugin is readable
by its owner and invisible to everyone else in the Space. Hiding the placement
adds nothing and breaks something — the author's own "我的插件" list uses the same
INNER JOIN, so a hidden placement would hide the draft from the person who
created it.

So: `defaultMarketPlacement` and `syncDefaultPlacement` are byte-for-byte
unchanged, every create still attaches a visible default placement, and
`ApproveReview` touches no placement row. The one thing approve changes is
`plugins.visibility`.

Consequence worth knowing: a plugin that somehow has NO default placement row
(pre-auto-placement legacy) will not become visible on approval. Main's update
path self-heals that on the next save; approve deliberately does not, to keep
"approve writes exactly one column on the plugin row" true.

### 2. `kind` derives from visibility, never from whether a version exists.

`InsertReviewRequest` classifies `first` as `visibility == 'private'`. It does
NOT look at `current_version_id`.

This is load-bearing. Main's import path snapshots a version as part of the
upload, so a private draft normally HAS a `current_version_id`. Gating on its
absence classifies every real upload as an `upgrade`, and the `isFirst` branch of
approve — the only code that flips visibility — never runs: the request reaches
`approved`, a version is minted, and the plugin stays invisible forever, with no
error anywhere. A fixture built through the plugin-write API hides the bug
because that path also leaves `current_version_id` NULL and lands on the correct
branch by accident, which is why
`TestInsertReviewRequestDerivesKindFromVisibility` seeds the state a real import
produces.

### 3. The applicant's version label is NEVER written to `plugin_versions.version`.

The brief says approve should "append the frozen snapshot as a `plugin_versions`
row" carrying the submitted version. That column is not a label — it is a
per-plugin auto-increment counter (`"1"`, `"2"`, …) computed by
`snapshotVersion`. Writing a semver string into it corrupts the sequence for
every later save.

Approve therefore calls main's own `snapshotVersion`, which mints the next
counter value and writes the applicant's label to `plugins.current_version` (the
only place a caller-declared label has ever lived).

The knock-on: `plugin_versions` no longer records published labels at all, so
label uniqueness cannot be a DB unique key. `publishedVersionLabels` reconstructs
it from the review table — the labels of every `approved` request for that plugin
— plus the plugin's own `current_version` when the plugin is already listed
(grandfathering rows that were Space-visible before this feature, with no
backfill). A private draft's `current_version` is deliberately EXCLUDED: it is a
draft label, and a first listing at the import default `1.0.0` is the normal
case. Labels from `rejected`/`canceled` requests stay reusable.

### 4. The frozen snapshot includes the RELATION GRAPH.

`plugin_review_requests` carries `relations_json` alongside
`manifest_json`/`plugin_json`. For `expert`/`expert_team` the membership graph IS
the reviewable content; freezing only the documents ships the reviewed manifest
next to whatever the live membership happened to be at approve time, and records
zero relations on the minted version.

Approve applies the frozen graph through main's `syncRelations`, after
`reconcileFrozenRelations` clears any frozen relation_id that is no longer live
(so an edge the author deleted post-submit is re-created rather than failing the
approval) and normalizes a JSON-`null` `Data` back to nil — `json.RawMessage`
round-trips nil as the four bytes `null`, which
`chk_plugin_relations_json_object` rejects.

Relation targets are re-locked under the **plugin owner's** scope, not the
reviewer's: an expert's bundled skills are `private` rows owned by the applicant,
and resolving them as the reviewer 404s every container approval.

**Not frozen: `attachment_keys_json`.** The storage sidecar stays whatever the
live row has. A reupload between submit and approve therefore pairs the frozen
package with the new object keys. Same family as item 7; closing it needs the
draft/live split.

### 5. Approving a container promotes its embedded children.

Not in the brief. An `expert`/`expert_team` whose embedded children stay
`private` is uninstallable by anyone but its author: `resolveInstallDetail`
refuses an install whose declared relation count exceeds the targets the caller
can see. `promoteEmbeddedChildren` lifts exactly the `is_embedded`, same-Space,
still-`private` children of the reviewed graph (derived AFTER the relations are
synced, so a member dropped from the frozen graph is not promoted).

### 6. Two tenant write paths are clamped, and only two.

- `createWithID` (tenant create + fresh import) forces `private`.
- `Service.update` (tenant update + reupload) refuses to RAISE visibility;
  keeping it or lowering to `private` (self-delisting) stays allowed.

Both VALIDATE the requested value before overriding it, so `visibility:"public"`
or a garbage value is still a 400 rather than a silent downgrade — the existing
tenant contract for those inputs is preserved.

Both exempt `IsSystemAdmin`. A platform operator reaches every one of these
transitions through `/api/v1/admin/*` anyway, and `system` rows are not
tenant-owned. `AdminCreate`/`AdminUpdate`, container import/reupload and the
admin skill import are untouched.

### 7. A re-import still replaces LIVE content without re-review.

`resolveImportFields` preserves an already-listed plugin's visibility on
reupload, because demoting it mid-edit would silently delist it. That means the
content of a listed plugin can be replaced without a new review. The brief's
"personal edits are a mutable draft" model implies otherwise, but honouring it
needs a real draft/live content split. Out of scope; follow-up.

### 8. Single-pending is a STORED generated column, not a partial index.

MySQL has no partial indexes — `CREATE UNIQUE INDEX ... WHERE` is PostgreSQL
syntax and fails outright. `pending_plugin_id` projects NULL for every
non-pending or soft-deleted row, and MySQL treats NULLs in a UNIQUE index as
distinct. Same trick as `20260720-01-category-live-name-unique.sql`. Both
mechanisms are in place: the index is the hard guarantee, and the transactional
`SELECT ... FOR UPDATE` gives the loser a typed `ErrConflict`.

### 9. Losing a decision race is `ErrConflict`, and cancel-after-decision is 409.

`classifyMissingPending` distinguishes "already decided" (`ErrConflict` → 409,
carrying the committed outcome) from "not visible to this Space"
(`ErrNotFound` → 404 that does not confirm the id exists). `CancelReview` does
the same: a 404 would tell an applicant whose request was just approved that it
never existed.

### 10. Web authorization reads `Caller.SpaceRole`, never the notifier.

`Caller.SpaceRole` is populated by the HTTP layer from
`identity.SpaceRoles[spaceID]` — the Space THIS request is for, so an admin of
another Space arrives as a member. System admins are treated as owner.

It is deliberately independent of the IM wiring. An earlier draft resolved the
web role through the notifier, which meant nobody could approve anything unless
IM notification happened to be configured.

`writeServiceError` gained a 403 branch (`ErrReviewForbidden` →
`errcode.PermissionDenied`); before this, a permission error fell through to 500.

`adminCaller` (`internal/service/plugin/admin.go`) builds a Caller with
`SpaceID: ""` and `IsSystemAdmin: true`. The admin surface does not route into
review, but `isReviewer` short-circuits on `IsSystemAdmin` first so such a Caller
can never be demoted by the empty-Space comparison.

### 11. octo-server's contract changed: no roster, role lookup + `target_role`.

`GET /v1/internal/spaces/:space_id/admins` has been **deleted** (it leaked
verified legal names cross-tenant, and empty-vs-non-empty was a Space-existence
oracle). Marketplace therefore makes **no roster call at all**:

- Submit dispatches with `target_role: "space_admin"`; octo-server resolves the
  recipients, excludes robots, and reports them in `delivered`. With
  `target_role` the caller never learns the roster, so `delivered` is the only
  delivery record there is — an empty one means the Space has no active human
  admin (a legitimate state, logged as a warning, not an error).
- The card callback re-verifies the operator with
  `GET /v1/internal/spaces/{space_id}/members/{uid}/role`, whose `role` is a
  nullable int (0 is a real role) and whose "absent" answer is byte-identical for
  non-member, removed member, unknown Space and disbanded Space.
- The shared token env is **`OCTO_MARKETPLACE_INTERNAL_TOKEN`** (renamed from
  `OCTO_MARKETPLACE_ADMIN_LIST_TOKEN`). The brief's three-secret list is now two:
  the internal token and `OCTO_MARKETPLACE_CARD_ACTION_SECRET`.

### 12. Card dispatch is entirely post-commit. Nothing runs before the response.

`dispatchReviewCard` builds nothing and calls nothing on the request goroutine:
the whole payload assembly and the octo-server call happen inside
`notify.BestEffort`, on a detached context with a tight deadline. An earlier
design fetched the roster synchronously, which put a 3-second remote timeout in
front of every submit and lost the card entirely when the client disconnected
right after commit.

### 13. A role-lookup failure is a retryable 5xx, NOT `forbidden`.

`disposition:"forbidden"` is a 200 that octo-server acks and never redelivers.
Reporting "octo-server is unreachable" that way silently discards a real admin's
click. `operatorRole` returns `(nil, nil)` for "not a member" and an ERROR for
"could not find out"; the handler turns the error into 503. An unconfigured
internal token is also a fault, not a refusal — a deployment that mounted the
callback without it cannot authorize anyone, and answering `forbidden` would burn
the event.

### 14. The card-protocol enums are octo-server's vocabulary, not ours.

Our `ReviewStatus` is `pending/approved/rejected/canceled`; the callback protocol
is `pending/approved/denied/cancelled`. `DecodeDecisionResult` validates the
enum, so emitting our spelling makes octo-server reject the entire response.
`cardState()` in `internal/service/plugin/review.go` is the single translation
point; never hand it `string(status)`.

### 15. `readme_content` and `plugin_icon` are actually populated.

- `readme_content` is extracted from the FROZEN package (`SKILL.md`, then
  `README.md`/`AGENTS.md`, falling back to the manifest description so connectors
  are not blank). Storage-backed attachments are not fetched on this read path.
  The previous build never assigned the field, so reviewers approved a blank body.
- `plugin_icon` goes through the same `resolveIcon` path the plugin list uses. An
  uploaded icon is stored as an object key; emitting it raw 404s in the browser.
- The list query does not select `manifest_json`/`plugin_json`/`relations_json`;
  only the detail read does.

### 16. Wiring, spec, and lint notes.

- `router.Public` gained a `ReviewConfig` parameter (octo-server URL, internal
  token, card-action secret, timeouts) rather than a package-level global. With
  an empty value the six review endpoints work identically and simply dispatch no
  card; the callback route is mounted but permanently closed.
- The callback is mounted on the ROOT engine at `/v1/card-actions/decide`,
  outside the tenant `Authenticator`, and is deliberately ABSENT from the OpenAPI
  spec: it is not a client contract. octo-server's
  `docs/card-action-callback-consumer.md` is authoritative, and its published
  signature vector is pinned in a test so a canonical-string drift fails a test
  instead of 401ing every callback in production.
- `internal/api/handler/plugin/review.go` is registered in `OPENAPI_SCAN_DIRS`.
- Two spectral WARNINGS remain on the new endpoints (`octo-auth-has-403` on
  `GET /plugins/review_requests/{review_id}` and on `.../cancel`). Left as-is on
  purpose: neither operation can return 403 — the detail read folds "not yours"
  into 404 so it cannot confirm existence, and cancel is applicant-only with no
  role gate. Declaring an unreachable status would make clients write dead code.
- Review request bodies use a size-bounded but unknown-field-TOLERANT decoder,
  unlike `/plugins/upsert`'s strict one: the octo-web client is already built and
  shipped against this contract.
