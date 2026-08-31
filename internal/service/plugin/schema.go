package plugin

// The manifest/package schema, canonical form, per-type file rules, and the
// plugin_hash formula are all owned by octo-plugin-lib; see documents.go for
// the integration seam. Only the schema identifiers used by host-side document
// builders remain here. They must match the ids the linked lib enforces
// (DecodeManifest/DecodePackage hard-assert `$schema`), so they move in lockstep
// with the contract generation — currently 2.0.
const (
	manifestSchema = "cowork-plugin-manifest-2.0.json"
	packageSchema  = "cowork-plugin-package-2.0.json"
)
