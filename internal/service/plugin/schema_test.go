package plugin

import (
	"encoding/json"
	"errors"
	"testing"

	libplugin "codex.mlamp.cn/dmwork/octo-plugin-lib/plugin"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func manifestFor(pluginName, pluginType, name string, labels string) json.RawMessage {
	return json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":` + quoted(pluginName) +
		`,"plugin_type":"` + pluginType + `","name":` + quoted(name) + `,"description":"desc","labels":` + labels + `}`)
}

func TestCanonicalizeManifestAgreesWithOuterFieldsAndTags(t *testing.T) {
	manifest := manifestFor("Plugin", "skill", "internal", `["a","b"]`)
	got, tags, err := CanonicalizeManifest("Plugin", model.PluginTypeSkill, json.RawMessage(`["a","b"]`), manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(tags) != `["a","b"]` {
		t.Fatalf("tags=%s", tags)
	}
	// Canonical bytes come from the lib encoder: sorted keys, compact.
	want, err := libplugin.CanonicalJSON(manifest)
	if err != nil || string(got) != string(want) {
		t.Fatalf("canonical=%s want=%s err=%v", got, want, err)
	}
}

func TestCanonicalizeManifestRejectsContractViolations(t *testing.T) {
	tests := []struct {
		name      string
		outerName string
		outerType model.PluginType
		tags      json.RawMessage
		manifest  json.RawMessage
	}{
		{"schema", "Plugin", model.PluginTypeExpert, json.RawMessage(`[]`), json.RawMessage(`{"$schema":"other","plugin_name":"Plugin","plugin_type":"expert","name":"x","description":"d"}`)},
		{"plugin name mismatch", "Plugin", model.PluginTypeExpert, json.RawMessage(`[]`), manifestFor("Other", "expert", "x", `[]`)},
		{"plugin type mismatch", "Plugin", model.PluginTypeExpert, json.RawMessage(`[]`), manifestFor("Plugin", "skill", "x", `[]`)},
		{"labels vs tags mismatch", "Plugin", model.PluginTypeExpert, json.RawMessage(`["b"]`), manifestFor("Plugin", "expert", "x", `["a"]`)},
		{"duplicate labels", "Plugin", model.PluginTypeExpert, json.RawMessage(`["a"]`), manifestFor("Plugin", "expert", "x", `["a","a"]`)},
		{"missing description", "Plugin", model.PluginTypeExpert, json.RawMessage(`[]`), json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Plugin","plugin_type":"expert","name":"x"}`)},
		{"bad example", "Plugin", model.PluginTypeExpert, json.RawMessage(`[]`), json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Plugin","plugin_type":"expert","name":"x","description":"d","examples":[{"title":"t","input":"i","extra":1}]}`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := CanonicalizeManifest(tt.outerName, tt.outerType, tt.tags, tt.manifest); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func docsFixture(t *testing.T, typ model.PluginType, pkg string) (*CanonicalDocuments, error) {
	t.Helper()
	manifest := manifestFor("Plugin", string(typ), "example-plugin", `[]`)
	return CanonicalizeDocuments("Plugin", typ, json.RawMessage(`[]`), manifest, json.RawMessage(pkg), "space-a")
}

func TestCanonicalizeDocumentsEnforcesPerTypeFileRules(t *testing.T) {
	agents := `{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"}`
	skill := `{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"}`
	mcp := `{"path":"mcp.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"mcpServers\":{}}"}`

	if _, err := docsFixture(t, model.PluginTypeExpert, `{"$schema":"cowork-plugin-package-1.0.json","attachments":[`+agents+`]}`); err != nil {
		t.Fatalf("expert with AGENTS.md rejected: %v", err)
	}
	if _, err := docsFixture(t, model.PluginTypeExpert, `{"$schema":"cowork-plugin-package-1.0.json","attachments":[`+skill+`]}`); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expert without AGENTS.md accepted: %v", err)
	}
	if _, err := docsFixture(t, model.PluginTypeSkill, `{"$schema":"cowork-plugin-package-1.0.json","attachments":[`+skill+`]}`); err != nil {
		t.Fatalf("skill with SKILL.md rejected: %v", err)
	}
	// expert_team: exactly one AGENTS.md, nothing else.
	if _, err := docsFixture(t, model.PluginTypeExpertTeam, `{"$schema":"cowork-plugin-package-1.0.json","attachments":[`+agents+`]}`); err != nil {
		t.Fatalf("single-file team rejected: %v", err)
	}
	if _, err := docsFixture(t, model.PluginTypeExpertTeam, `{"$schema":"cowork-plugin-package-1.0.json","attachments":[`+agents+`,`+skill+`]}`); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("multi-file team accepted: %v", err)
	}
	// connector: descriptor required (with source), forbidden on other types.
	if _, err := docsFixture(t, model.PluginTypeConnector, `{"$schema":"cowork-plugin-package-1.0.json","connector":{"type":"mcp","source":"connector.x"},"attachments":[`+mcp+`]}`); err != nil {
		t.Fatalf("valid connector rejected: %v", err)
	}
	if _, err := docsFixture(t, model.PluginTypeConnector, `{"$schema":"cowork-plugin-package-1.0.json","attachments":[`+mcp+`]}`); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("connector without descriptor accepted: %v", err)
	}
	if _, err := docsFixture(t, model.PluginTypeConnector, `{"$schema":"cowork-plugin-package-1.0.json","connector":{"type":"mcp"},"attachments":[`+mcp+`]}`); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("connector without source accepted: %v", err)
	}
	if _, err := docsFixture(t, model.PluginTypeSkill, `{"$schema":"cowork-plugin-package-1.0.json","connector":{"type":"mcp","source":"x"},"attachments":[`+skill+`]}`); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("skill with descriptor accepted: %v", err)
	}
}

func TestCanonicalizeDocumentsScopesStorageKeysToSpace(t *testing.T) {
	mk := func(uri string) string {
		return `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
			`{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"},` +
			`{"path":"assets/icon.png","content_type":"storage","mime_type":"image/png","storage_uri":` + quoted(uri) + `}]}`
	}
	if _, err := docsFixture(t, model.PluginTypeExpert, mk("plugins/space-a/attachments/icon-1.png")); err != nil {
		t.Fatalf("approved key rejected: %v", err)
	}
	for _, uri := range []string{
		"s3://octo-plugin-assets/plugins/x/icon.png",
		"plugins/space-b/attachments/icon-1.png",
		"plugins/space-a/attachments/../escape.png",
		"key",
	} {
		if _, err := docsFixture(t, model.PluginTypeExpert, mk(uri)); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("uri %q accepted: %v", uri, err)
		}
	}
}

func TestCanonicalizeDocumentsUsesLibHashFormula(t *testing.T) {
	docs, err := docsFixture(t, model.PluginTypeExpert,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	want, err := libplugin.ComputePluginHash(docs.Manifest, docs.Package)
	if err != nil || docs.PluginHash != want {
		t.Fatalf("hash=%s want=%s err=%v", docs.PluginHash, want, err)
	}
	if docs.ManifestHash != hashJSON(docs.Manifest) {
		t.Fatalf("manifest_hash=%s", docs.ManifestHash)
	}
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// TestCanonicalizeDocumentsScopesSkillRefKeys is the Q0 provenance gate: on the
// caller write path a legacy skill/ref.json pointer may reference only this
// Space's managed prefix, so a caller cannot plant a legacy-root or cross-Space
// pointer that the expand-skills migration would later dereference with service
// credentials. The trusted backfill variant admits those keys.
func TestCanonicalizeDocumentsScopesSkillRefKeys(t *testing.T) {
	mk := func(refBody string) string {
		return `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
			`{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# stub"},` +
			`{"path":"skill/ref.json","content_type":"raw","mime_type":"application/json","raw_content":` + quoted(refBody) + `}]}`
	}
	manifest := manifestFor("Plugin", "skill", "example-plugin", `[]`)

	// Forged legacy-root and cross-Space pointers are rejected on the caller path.
	for _, ref := range []string{
		`{"zip_object_key":"skills/victim-id/versions/v1/package.zip"}`,
		`{"object_key":"experts/victim/skill.md"}`,
		`{"file_url":"squads/victim/pkg.zip"}`,
		`{"object_key":"plugins/space-b/attachments/skill-1.md"}`,
	} {
		if _, err := CanonicalizeDocuments("Plugin", model.PluginTypeSkill, json.RawMessage(`[]`), manifest, json.RawMessage(mk(ref)), "space-a"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("forged ref %q accepted on caller path: %v", ref, err)
		}
	}

	// An own-Space pointer is allowed on the caller path.
	ownRef := `{"object_key":"plugins/space-a/attachments/skill-1.md"}`
	if _, err := CanonicalizeDocuments("Plugin", model.PluginTypeSkill, json.RawMessage(`[]`), manifest, json.RawMessage(mk(ownRef)), "space-a"); err != nil {
		t.Fatalf("own-Space ref rejected: %v", err)
	}

	// The trusted backfill variant admits the legacy-root pointer it migrated.
	legacyRef := `{"zip_object_key":"skills/legit-id/versions/v1/package.zip"}`
	if _, err := CanonicalizeMigratedDocuments("Plugin", model.PluginTypeSkill, json.RawMessage(`[]`), manifest, json.RawMessage(mk(legacyRef)), "space-a"); err != nil {
		t.Fatalf("backfill legacy-root ref rejected: %v", err)
	}
}
