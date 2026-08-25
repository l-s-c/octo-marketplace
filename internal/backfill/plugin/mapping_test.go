package plugin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	libplugin "github.com/Mininglamp-OSS/octo-marketplace/internal/plugincontract"
)

var canonicalUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-8[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestPluginIDIsDeterministicUUID(t *testing.T) {
	a := PluginID("skill", "same")
	b := PluginID("expert", "same")
	if a == "same" || a == b || a != PluginID("skill", "same") {
		t.Fatalf("IDs not stable/distinct: %q %q", a, b)
	}
	for _, v := range []string{a, b, DeterministicID("relation", "x")} {
		if !canonicalUUID.MatchString(v) {
			t.Fatalf("not a canonical derived UUID: %q", v)
		}
	}
}

func TestSanitizeConnectorJSONBlanksEnvAndHeaders(t *testing.T) {
	got, err := SanitizeConnectorJSON([]byte(`{"url":"https://example.invalid","Env":{"TOKEN":"actual"},"HEADERS":{"Authorization":"Bearer actual"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(got, &value); err != nil {
		t.Fatal(err)
	}
	if value["Env"].(map[string]any)["TOKEN"] != "" || value["HEADERS"].(map[string]any)["Authorization"] != "" {
		t.Fatalf("secret maps were not blanked: %s", got)
	}
	if strings.Contains(string(got), "actual") {
		t.Fatalf("secret leaked: %s", got)
	}
}

func TestSanitizeConnectorJSONRejectsSecretShapedValue(t *testing.T) {
	for _, raw := range []string{
		`{"nested":{"api_key":"actual"}}`,
		`{"clientSecret":"actual"}`,
		`{"accessToken":"actual"}`,
		`{"privateKeyValue":"actual"}`,
		`{"token":{"nested":"actual"}}`,
		`{"ok":true} {"second":true}`,
		`[]`,
		`{"mcpServers":[]}`,
	} {
		if _, err := SanitizeConnectorJSON([]byte(raw)); err == nil {
			t.Fatalf("expected rejection for %s", raw)
		}
	}
}

func TestSanitizeConnectorJSONAllowsEmptyAndPlaceholder(t *testing.T) {
	got, err := SanitizeConnectorJSON([]byte(`{"api_key":"__OCTO_SECRET_PLACEHOLDER__"}`))
	if err != nil || string(got) != `{"api_key":""}` {
		t.Fatalf("got %s, %v", got, err)
	}
	got, err = SanitizeConnectorJSON(nil)
	if err != nil || string(got) != `{}` {
		t.Fatalf("empty got %s, %v", got, err)
	}
}

func TestPackageJSONSortsAttachmentsWithMetadataAndNoManifestEmbed(t *testing.T) {
	pkgJSON, err := packageJSON(
		rawAttachment{path: "z.txt", mimeType: "text/plain", content: "z"},
		rawAttachment{path: "a.txt", mimeType: "text/plain", content: "hello"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var pkg pluginPackage
	if err := json.Unmarshal(pkgJSON, &pkg); err != nil {
		t.Fatal(err)
	}
	if pkg.Schema != "cowork-plugin-package-1.0.json" || len(pkg.Attachments) != 2 {
		t.Fatalf("package = %#v", pkg)
	}
	paths := []string{pkg.Attachments[0].Path, pkg.Attachments[1].Path}
	// The contract layout carries the manifest only in the manifest_json
	// column; no manifest.json attachment is embedded.
	if strings.Join(paths, ",") != "a.txt,z.txt" {
		t.Fatalf("attachment paths = %#v", paths)
	}
	for _, attachment := range pkg.Attachments {
		if attachment.ContentSize != len([]byte(attachment.RawContent)) || attachment.ContentHash != hashJSON([]byte(attachment.RawContent)) {
			t.Fatalf("bad attachment metadata: %#v", attachment)
		}
	}
}

func TestPackageJSONRejectsUnsafeAndConflictingPaths(t *testing.T) {
	for _, attachmentPath := range []string{"../secret", "/absolute", `bad\\path`} {
		if _, err := packageJSON(rawAttachment{path: attachmentPath, mimeType: "text/plain", content: "x"}); err == nil {
			t.Fatalf("unsafe path accepted: %q", attachmentPath)
		}
	}
	if _, err := packageJSON(
		rawAttachment{path: "same", mimeType: "text/plain", content: "one"},
		rawAttachment{path: "same", mimeType: "text/plain", content: "two"},
	); err == nil {
		t.Fatal("conflicting duplicate path accepted")
	}
}

func TestManifestExamplesAndPluginHashFollowDesign(t *testing.T) {
	manifestJSON, err := canonical(newPluginManifest("Connector", "connector", "connector", "desc", nil, []string{"first", "second"}))
	if err != nil {
		t.Fatal(err)
	}
	var got pluginManifest
	if err := json.Unmarshal(manifestJSON, &got); err != nil {
		t.Fatal(err)
	}
	if got.Labels == nil || len(got.Examples) != 2 || got.Examples[1].Title != "使用示例 2" || got.Examples[1].Input != "second" {
		t.Fatalf("manifest = %#v", got)
	}
	pkgJSON, err := packageJSON(rawAttachment{path: "SKILL.md", mimeType: "text/markdown", content: "# doc"})
	if err != nil {
		t.Fatal(err)
	}
	// plugin_hash follows the lib's frozen formula.
	want, err := libplugin.ComputePluginHash(manifestJSON, pkgJSON)
	if err != nil {
		t.Fatal(err)
	}
	if got := both(manifestJSON, pkgJSON); got != want {
		t.Fatalf("plugin hash = %q want %q", got, want)
	}
}

func TestValidateGraphLimits(t *testing.T) {
	now := time.Unix(1, 0)
	edge := func(a, b string) relRow {
		return relation(a, b, "expert_skill", 0, map[string]any{}, "owner", now, now, sql.NullTime{})
	}
	if err := validateGraph([]relRow{edge("a", "b"), edge("b", "a")}, 16, 500); err == nil {
		t.Fatal("cycle accepted")
	}
	var deep []relRow
	for i := 0; i < 16; i++ {
		deep = append(deep, edge(fmt.Sprint(i), fmt.Sprint(i+1)))
	}
	if err := validateGraph(deep, 16, 500); err == nil {
		t.Fatal("depth 17 accepted")
	}
	if err := validateGraph([]relRow{edge("a", "b"), edge("a", "c")}, 16, 2); err == nil {
		t.Fatal("node cap ignored")
	}
	if err := validateGraph([]relRow{edge("a", "b"), edge("b", "c")}, 3, 3); err != nil {
		t.Fatalf("valid graph rejected: %v", err)
	}
}

func TestNamesFromTagIDs(t *testing.T) {
	got, err := namesFromTagIDs([]byte(`[2,1,2]`), map[int64]string{1: "one", 2: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "two,one" {
		t.Fatalf("names = %#v", got)
	}
	if _, err := namesFromTagIDs([]byte(`[3]`), map[int64]string{}); err == nil {
		t.Fatal("unknown ID accepted")
	}
}
