---
type: Task
title: "Task: Space review auto approval policy"
slug: space-review-auto-approve
---

# Space review auto approval policy

## Goal

Let each Space owner choose whether organization-visible plugin submissions are
approved automatically. The effective default is enabled, so Spaces without a
stored override publish immediately while retaining an approval audit record.

## Load-bearing behavior

- The policy is owned and persisted by marketplace and scoped by authenticated
  `space_id`; request bodies never carry a Space identifier.
- A missing policy row resolves to `is_auto_approve_enabled=true`.
- Any authenticated Space member may read the effective policy; only the Space
  owner (`space_member.role=2`) may update it.
- When enabled, publishing a Space-visible plugin still freezes a review request
  and then approves it with `decision_source=policy`; no approval card is sent.
- When disabled, the existing pending-review and notification-card flow is used.
- Policy lookup failures fail closed and do not publish.
- Changing the policy does not mutate existing pending review requests.

## Out of scope

- Platform/system plugin policy.
- Retroactively deciding pending requests.
- Moving Space membership or role authority out of octo-server.

## Acceptance criteria

- GET/PATCH endpoints use authenticated Space context and standard envelopes.
- The PATCH endpoint rejects admins and members with `FORBIDDEN`.
- Default-enabled, disabled, lookup-failure, and automatic audit-source paths
  have tests.
- OpenAPI validation and compatibility checks pass.
