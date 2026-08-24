package plugin

import (
	"bytes"
	"encoding/json"
	"strings"

	libplugin "codex.mlamp.cn/dmwork/octo-plugin-lib/plugin"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// CanonicalDocuments carries contract-canonical plugin documents and their
// hashes. Canonicalization, schema validation, per-type file rules, and the
// plugin_hash formula all come from octo-plugin-lib; the marketplace adds only
// host-owned rules on top (secret scan, storage-key scoping, tags==labels).
type CanonicalDocuments struct {
	Manifest     json.RawMessage
	Package      json.RawMessage
	Tags         json.RawMessage
	ManifestHash string
	PluginHash   string
}

// CanonicalizeManifest validates a manifest through the octo-plugin-lib
// contract and returns its canonical bytes plus the canonical tags derived
// from the host tags==labels invariant. Exposed so out-of-band writers (the
// legacy backfill, import) share one validator instead of drifting.
func CanonicalizeManifest(name string, typ model.PluginType, tags, manifest json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	trimmed := strings.TrimSpace(name)
	if !validName(trimmed) || !validPluginType(typ) {
		return nil, nil, ErrInvalidRequest
	}
	if len(manifest) == 0 || len(manifest) > maxJSONBytes {
		return nil, nil, ErrInvalidRequest
	}
	decoded, err := libplugin.DecodeManifest(manifest)
	if err != nil {
		return nil, nil, ErrInvalidRequest
	}
	// The outer row fields are authoritative; the manifest must agree with
	// them exactly (same rule libplugin.ValidatePlugin applies to full rows).
	if decoded.PluginName != trimmed || string(decoded.PluginType) != string(typ) {
		return nil, nil, ErrInvalidRequest
	}
	canonical, err := libplugin.CanonicalJSON(manifest)
	if err != nil {
		return nil, nil, ErrInvalidRequest
	}
	// Host invariant: the plugins.tags_json column mirrors manifest labels.
	normalizedTags, err := normalizeTags(tags)
	if err != nil {
		return nil, nil, err
	}
	labels := decoded.Labels
	if labels == nil {
		labels = []string{}
	}
	canonicalLabels, err := canonicalJSONValue(labels)
	if err != nil || !bytes.Equal(canonicalLabels, normalizedTags) {
		return nil, nil, ErrInvalidRequest
	}
	return canonical, normalizedTags, nil
}

// CanonicalizeDocuments validates a manifest/package pair through the
// octo-plugin-lib contract (canonical form, per-type file rules, connector
// descriptor, hash), then applies the host-only rules: secret scan, approved
// storage-key scoping, and size caps. plugin_hash uses the lib formula
// sha256(canonicalManifest || canonicalPackage); manifest_hash stays a
// host-only column over the canonical manifest.
//
// This is the CALLER write path: a legacy skill/ref.json pointer may reference
// only this Space's own managed prefix, so a caller cannot persist a legacy-root
// or cross-Space pointer through upsert.
func CanonicalizeDocuments(name string, typ model.PluginType, tags, manifest, pkg json.RawMessage, spaceID string) (*CanonicalDocuments, error) {
	return canonicalizeDocuments(name, typ, tags, manifest, pkg, spaceID, false)
}

// CanonicalizeMigratedDocuments is the trusted out-of-band variant used only by
// the deterministic backfill, which legitimately embeds legacy-root skill/ref.json
// pointers (skills/, experts/, squads/) it migrated from the source catalogs.
// Because only this variant admits those pointers, every legacy-root pointer in
// the plugin catalog is provably backfill-written — never planted by a caller —
// which is what lets the expand-skills migration trust them.
func CanonicalizeMigratedDocuments(name string, typ model.PluginType, tags, manifest, pkg json.RawMessage, spaceID string) (*CanonicalDocuments, error) {
	return canonicalizeDocuments(name, typ, tags, manifest, pkg, spaceID, true)
}

func canonicalizeDocuments(name string, typ model.PluginType, tags, manifest, pkg json.RawMessage, spaceID string, trustLegacyRefs bool) (*CanonicalDocuments, error) {
	m, t, err := CanonicalizeManifest(name, typ, tags, manifest)
	if err != nil {
		return nil, err
	}
	if len(pkg) == 0 || len(pkg) > maxJSONBytes {
		return nil, ErrInvalidRequest
	}
	p, err := libplugin.CanonicalJSON(pkg)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	decoded, err := libplugin.DecodePackage(libplugin.Type(typ), p)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	// Host rule: storage attachments may only reference this Space's managed
	// prefix; the lib treats storage_uri as an opaque host-private string.
	for _, attachment := range decoded.Attachments {
		if attachment.ContentType != libplugin.ContentStorage {
			continue
		}
		if !safeObjectSegment.MatchString(spaceID) || !validReferencedObjectKey(attachment.StorageURI, spaceID) {
			return nil, ErrInvalidRequest
		}
	}
	// Host rule (caller path only): a legacy skill/ref.json pointer is a raw
	// attachment whose JSON body carries object keys the lib never inspects. Scope
	// those keys to this Space's managed prefix so a caller cannot persist a
	// forged legacy-root/cross-Space pointer for the expand-skills migration to
	// later dereference with service credentials. The trusted backfill bypasses
	// this gate (trustLegacyRefs) because it writes those pointers deliberately.
	if !trustLegacyRefs && !skillRefKeysScoped(p, spaceID) {
		return nil, ErrInvalidRequest
	}
	if err := rejectSecretValues(m, p); err != nil {
		return nil, err
	}
	hash, err := libplugin.ComputePluginHash(m, p)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	return &CanonicalDocuments{
		Manifest:     m,
		Package:      p,
		Tags:         t,
		ManifestHash: hashJSON(m),
		PluginHash:   hash,
	}, nil
}

// skillRefKeysScoped reports whether a package's legacy skill/ref.json pointer
// (if any) references only this Space's own managed prefix. A package without a
// skill/ref.json passes trivially. Every object key the pointer can resolve to
// (object_key, zip_object_key, file_url) must clear trustedArtifactKey; a
// legacy-root or cross-Space key fails closed. This is the provenance gate on
// the caller write path — see canonicalizeDocuments.
func skillRefKeysScoped(pkg json.RawMessage, spaceID string) bool {
	raw, ok := rawAttachmentContent(pkg, "skill/ref.json")
	if !ok {
		return true
	}
	var ref skillRefDocument
	if json.Unmarshal([]byte(raw), &ref) != nil {
		return false
	}
	sp := &spaceID
	for _, key := range []string{ref.ObjectKey, ref.ZipObjectKey, ref.FileURL} {
		if key == "" {
			continue
		}
		if !trustedArtifactKey(key, sp) {
			return false
		}
	}
	return true
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

// RejectSecretValues runs the write-path secret scan over raw documents.
// Exported so out-of-band writers (the repackage migration) apply the same
// invariant before persisting rewritten packages via direct SQL.
func RejectSecretValues(values ...json.RawMessage) error {
	return rejectSecretValues(values...)
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
