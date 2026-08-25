// Package plugincontract is the product-neutral Cowork Plugin contract:
// canonical JSON, the plugin_hash formula, schema/per-type-file validation, the
// connector descriptor rules, and the relation endpoint matrix.
//
// It is an inlined copy of github.com/Mininglamp-OSS/octo-marketplace/internal/plugincontract@v1.0.1.
// octo-marketplace is the only Go consumer, so the contract lives here directly
// rather than as an external module (see .octospec divergence record). The
// byte-exact CanonicalJSON/ComputePluginHash must stay in lockstep with the
// TypeScript reimplementation in octo-web (goCanonicalJSON); change both
// together.
package plugincontract
