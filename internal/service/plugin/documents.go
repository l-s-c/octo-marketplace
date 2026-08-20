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
	p, err := normalizePackage(pkg, m, spaceID)
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
