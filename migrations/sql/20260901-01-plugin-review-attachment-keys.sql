-- +migrate Up
-- Freeze the storage-attachment sidecar on a review request so a zip-submitted
-- skill upgrade (which spills binary/oversize files to the managed prefix at
-- submit time) can carry its complete snapshot to approval. The column mirrors
-- plugins.attachment_keys_json exactly: JSON NULL, a path -> managed-object-key
-- map for any storage attachments the frozen package references. Without it,
-- approving a zip-submitted review would apply a package whose storage_uri
-- paths the live row's sidecar had no entry for.
ALTER TABLE `plugin_review_requests`
  ADD COLUMN `attachment_keys_json` JSON NULL AFTER `plugin_json`;

-- +migrate Down
ALTER TABLE `plugin_review_requests` DROP COLUMN `attachment_keys_json`;
