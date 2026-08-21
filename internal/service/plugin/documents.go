package plugin

import (
	"encoding/json"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// CanonicalDocuments carries API-canonical plugin documents and their hashes.
type CanonicalDocuments struct {
	Manifest     json.RawMessage
	Package      json.RawMessage
	Tags         json.RawMessage
	ManifestHash string
	PluginHash   string
}

// CanonicalizeManifest validates and canonicalizes a manifest with the exact
// rules the write API applies, returning the canonical manifest and tags.
// Exposed so out-of-band writers (the legacy backfill) share one validator
// instead of reimplementing it and drifting.
func CanonicalizeManifest(name string, typ model.PluginType, tags, manifest json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	trimmed := strings.TrimSpace(name)
	if !validName(trimmed) || !validPluginType(typ) {
		return nil, nil, ErrInvalidRequest
	}
	return normalizeManifest(manifest, trimmed, typ, tags)
}

// CanonicalizeDocuments validates a manifest/package pair, enforces the secret
// invariant for every plugin type, and returns canonical bytes plus the API
// hash formulas (plugin_hash = sha256(manifest + "\n" + package)).
func CanonicalizeDocuments(name string, typ model.PluginType, tags, manifest, pkg json.RawMessage, spaceID string) (*CanonicalDocuments, error) {
	m, t, err := CanonicalizeManifest(name, typ, tags, manifest)
	if err != nil {
		return nil, err
	}
	p, err := normalizePackage(pkg, m, spaceID, typ)
	if err != nil {
		return nil, err
	}
	if err := rejectSecretValues(m, p); err != nil {
		return nil, err
	}
	return &CanonicalDocuments{
		Manifest:     m,
		Package:      p,
		Tags:         t,
		ManifestHash: hashJSON(m),
		PluginHash:   hashJSON(append(append(cloneJSON(m), '\n'), p...)),
	}, nil
}

// RejectSecretValues runs the write-path secret scan over raw documents.
// Exported so out-of-band writers (the repackage migration) apply the same
// invariant before persisting rewritten packages via direct SQL.
func RejectSecretValues(values ...json.RawMessage) error {
	return rejectSecretValues(values...)
}

// rawAttachmentContent returns the raw_content of one inline package
// attachment; storage attachments and missing paths report false.
func rawAttachmentContent(pkg json.RawMessage, path string) (string, bool) {
	var doc struct {
		Attachments []struct {
			Path        string `json:"path"`
			ContentType string `json:"content_type"`
			RawContent  string `json:"raw_content"`
		} `json:"attachments"`
	}
	if json.Unmarshal(pkg, &doc) != nil {
		return "", false
	}
	for _, attachment := range doc.Attachments {
		if attachment.Path == path && attachment.ContentType == "raw" {
			return attachment.RawContent, true
		}
	}
	return "", false
}

// storageAttachmentKey returns the storage_uri of one storage-backed package
// attachment.
func storageAttachmentKey(pkg json.RawMessage, path string) (string, bool) {
	var doc struct {
		Attachments []struct {
			Path        string `json:"path"`
			ContentType string `json:"content_type"`
			StorageURI  string `json:"storage_uri"`
		} `json:"attachments"`
	}
	if json.Unmarshal(pkg, &doc) != nil {
		return "", false
	}
	for _, attachment := range doc.Attachments {
		if attachment.Path == path && attachment.ContentType == "storage" {
			return attachment.StorageURI, true
		}
	}
	return "", false
}

// ConnectorToolCount counts the entries of the canonical connector/tools.json
// raw attachment. Tools metadata is optional display data, so a missing or
// malformed attachment counts as zero instead of failing the write. Exported
// so the backfill enrichment recomputes counts through the same rule.
func ConnectorToolCount(pkg json.RawMessage) int {
	raw, ok := rawAttachmentContent(pkg, "connector/tools.json")
	if !ok {
		return 0
	}
	var tools []json.RawMessage
	if json.Unmarshal([]byte(raw), &tools) != nil {
		return 0
	}
	return len(tools)
}
