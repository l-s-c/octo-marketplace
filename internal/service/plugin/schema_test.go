package plugin

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestNormalizeManifestDefaultsAndSynchronizesOuterFields(t *testing.T) {
	raw := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Plugin","plugin_type":"skill","name":"internal","description":"","extra":{"kept":true}}`)
	got, tags, err := normalizeManifest(raw, "Plugin", model.PluginTypeSkill, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"$schema":"cowork-plugin-manifest-1.0.json","description":"","examples":[],"extra":{"kept":true},"labels":[],"name":"internal","plugin_name":"Plugin","plugin_type":"skill"}`
	if string(got) != want || string(tags) != `[]` {
		t.Fatalf("manifest=%s tags=%s", got, tags)
	}
}

func TestNormalizeManifestRejectsSchemaAndOuterMismatches(t *testing.T) {
	base := `{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Plugin","plugin_type":"expert","name":"internal","description":"desc","labels":["a"],"examples":[]}`
	tests := []struct {
		name, raw, outerName string
		outerType            model.PluginType
		tags                 json.RawMessage
	}{
		{"schema", strings.Replace(base, manifestSchema, "other", 1), "Plugin", model.PluginTypeExpert, json.RawMessage(`["a"]`)},
		{"plugin name", strings.Replace(base, `"Plugin"`, `"Other"`, 1), "Plugin", model.PluginTypeExpert, json.RawMessage(`["a"]`)},
		{"plugin type", strings.Replace(base, `"expert"`, `"skill"`, 1), "Plugin", model.PluginTypeExpert, json.RawMessage(`["a"]`)},
		{"labels", base, "Plugin", model.PluginTypeExpert, json.RawMessage(`["b"]`)},
		{"missing description", strings.Replace(base, `,"description":"desc"`, "", 1), "Plugin", model.PluginTypeExpert, json.RawMessage(`["a"]`)},
		{"bad example", strings.Replace(base, `[]}`, `[{"title":"x","input":"y","extra":1}]}`, 1), "Plugin", model.PluginTypeExpert, json.RawMessage(`["a"]`)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := normalizeManifest(json.RawMessage(tt.raw), tt.outerName, tt.outerType, tt.tags); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestNormalizePackageSortsAndRequiresExactCanonicalManifest(t *testing.T) {
	manifest, _, err := normalizeManifest(json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Plugin","plugin_type":"expert","name":"internal","description":"desc"}`), "Plugin", model.PluginTypeExpert, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"attachments":[{"path":"z.txt","content_type":"raw","mime_type":"text/plain","raw_content":"z"},{"path":"manifest.json","content_type":"raw","mime_type":"application/json","raw_content":` + quoted(string(manifest)) + `}],"$schema":"cowork-plugin-package-1.0.json"}`)
	got, err := normalizePackage(raw, manifest, "space-a")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(got), `"path":"manifest.json"`) > strings.Index(string(got), `"path":"z.txt"`) {
		t.Fatalf("attachments not sorted: %s", got)
	}
}

func TestNormalizePackageRejectsInvalidSchemaAttachmentsAndManifest(t *testing.T) {
	manifest := json.RawMessage(`{"canonical":true}`)
	entry := `{"path":"manifest.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"canonical\":true}"}`
	tests := []string{
		`{"$schema":"other","attachments":[` + entry + `]}`,
		`{"$schema":"cowork-plugin-package-1.0.json"}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[]}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` + entry + `,` + entry + `]}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"../manifest.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"canonical\":true}"}]}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"manifest.json","content_type":"storage","mime_type":"application/json","storage_uri":"key"}]}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"manifest.json","content_type":"raw","mime_type":"application/json","raw_content":"{ \"canonical\": true }"}]}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` + entry + `,{"path":"x","content_type":"raw","mime_type":"text/plain","raw_content":"x","storage_uri":"key"}]}`,
		`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` + entry + `,{"path":"x","content_type":"storage","mime_type":"text/plain","storage_uri":"key","extra":true}]}`,
	}
	for _, raw := range tests {
		if _, err := normalizePackage(json.RawMessage(raw), manifest, "space-a"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("accepted %s: %v", raw, err)
		}
	}
}

func TestNormalizePackageStorageURIMustMatchArchiveObjectKeyRule(t *testing.T) {
	manifest, _, err := normalizeManifest(json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Plugin","plugin_type":"expert","name":"internal","description":"desc"}`), "Plugin", model.PluginTypeExpert, nil)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(uri string) json.RawMessage {
		return json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
			`{"path":"assets/icon.png","content_type":"storage","mime_type":"image/png","storage_uri":` + quoted(uri) + `},` +
			`{"path":"manifest.json","content_type":"raw","mime_type":"application/json","raw_content":` + quoted(string(manifest)) + `}]}`)
	}
	if _, err := normalizePackage(mk("plugins/space-a/attachments/icon-1.png"), manifest, "space-a"); err != nil {
		t.Fatalf("approved key rejected: %v", err)
	}
	for _, uri := range []string{
		"s3://octo-plugin-assets/plugins/prd-outline/assets/icon.png",
		"plugins/space-b/attachments/icon-1.png",
		"/plugins/space-a/attachments/icon-1.png",
		"plugins/space-a/attachments/../escape.png",
		"plugins/space-a/attachments/",
		"key",
	} {
		if _, err := normalizePackage(mk(uri), manifest, "space-a"); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("uri %q accepted: %v", uri, err)
		}
	}
}

func quoted(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}
