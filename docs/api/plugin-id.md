# Unified Plugin ID boundary

The cowork-v3 unified Plugin API uses typed, prefixed IDs. Encoding and strict
parsing live in `internal/service/plugin/pluginid`; callers must not concatenate
or split these values themselves.

## Wire forms

```text
expert:<experts.id>
expert_team:<expert_squads.id>
skill:<skills.id>
connector:<mcp_servers.id>
expert_member:<expert_squads.id>:<member_key>
expert_skill:<experts.id>:<skill_key>
expert_member_skill:<expert_squads.id>:<member_key>:<skill_key>
```

There are no compatibility prefixes or aliases.

## Unified database boundary

The current unified `plugins.plugin_id` and relation columns store opaque IDs in
`VARCHAR(64)`. They must remain unprefixed until a schema and data migration is
explicitly performed. Prefixed IDs can be longer than 64 bytes, especially for
embedded assets, and must not be written to those columns implicitly.

Use this conversion at the service/API boundary:

1. Parse an incoming wire ID with `pluginid.Parse`.
2. For a top-level ID, use `StorageID()` to obtain the exact opaque DB ID. The
   prefix selects and must agree with the resource type; the suffix is not
   normalized or regenerated.
3. For an embedded ID, use `ResourceID` to load the parent and locate the nested
   item by `MemberKey` and/or `SkillKey`. Embedded IDs do not map to an
   independent `plugins` row and `StorageID()` deliberately returns no mapping.
4. Encode outbound top-level records with `pluginid.NewTopLevel`; encode nested
   addresses with the matching embedded constructor.

This boundary is intentionally exposed without changing handlers, existing
service methods, or backfill behavior. Integration should happen atomically at
the unified API facade so repository methods continue receiving opaque DB IDs
and wire responses consistently receive prefixed IDs.

## Validation

Every segment is non-empty ASCII and at most 64 bytes, restricted to letters,
digits, `.`, `_`, and `-`. Stable `member_key` and `skill_key` segments also
reject `..`. Parsing rejects unsupported prefixes, whitespace, missing or extra
segments, and overlong values. Skill keys are stable generated or persisted
keys; array indexes and display names are never valid substitutes.
