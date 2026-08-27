-- +migrate Up
-- The octo-plugin-lib 2.0 contract forbids any host-private field inside a
-- package attachment: DecodePackage rejects unknown keys, so a storage
-- attachment can no longer carry its object key as `storage_uri` in plugin_json.
-- The key moves to this host-private sidecar column (a JSON object mapping the
-- attachment path to its managed object key). It is deliberately OUTSIDE the
-- hashed/validated package document, so plugin_hash covers only contract
-- content and the stored package stays a valid 2.0 document. Only skill rows
-- with spilled storage attachments populate it; every other row leaves it NULL.
ALTER TABLE `plugins`
  ADD COLUMN `attachment_keys_json` JSON NULL AFTER `plugin_json`;

-- Version snapshots carry their own immutable package copy, so they need the
-- same sidecar to resolve a historical version's storage attachments.
ALTER TABLE `plugin_versions`
  ADD COLUMN `attachment_keys_json` JSON NULL AFTER `plugin_json`;

-- +migrate Down
ALTER TABLE `plugin_versions` DROP COLUMN `attachment_keys_json`;
ALTER TABLE `plugins` DROP COLUMN `attachment_keys_json`;
