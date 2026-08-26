package plugin

// The manifest/package schema, canonical form, per-type file rules, and the
// plugin_hash formula are all owned by octo-plugin-lib (contracts/v1); see
// documents.go for the integration seam. Only the schema identifiers used by
// host-side document builders remain here.

const (
	manifestSchema = "cowork-plugin-manifest-1.0.json"
	packageSchema  = "cowork-plugin-package-1.0.json"
)
