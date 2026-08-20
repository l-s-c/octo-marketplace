package plugin

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestPluginIDUsesOpaqueStorageContract(t *testing.T) {
	if got := PluginID("skill", "same", 1); got != "same" {
		t.Fatalf("unique ID = %q", got)
	}
	a := PluginID("skill", "same", 2)
	b := PluginID("expert", "same", 2)
	if a == "same" || a == b || a != PluginID("skill", "same", 2) {
		t.Fatalf("prefixed IDs not stable/distinct: %q %q", a, b)
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

func TestPackageJSONIncludesCanonicalManifestAndAttachmentMetadata(t *testing.T) {
	manifestJSON, err := canonical(newPluginManifest("Example", "expert", "Example", "description", []string{"tag"}, []string{"try it"}))
	if err != nil {
		t.Fatal(err)
	}
	pkgJSON, err := packageJSON(manifestJSON,
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
	if pkg.Schema != "cowork-plugin-package-1.0.json" || len(pkg.Attachments) != 3 {
		t.Fatalf("package = %#v", pkg)
	}
	paths := []string{pkg.Attachments[0].Path, pkg.Attachments[1].Path, pkg.Attachments[2].Path}
	if strings.Join(paths, ",") != "a.txt,manifest.json,z.txt" {
		t.Fatalf("attachment paths = %#v", paths)
	}
	manifestAttachment := pkg.Attachments[1]
	if manifestAttachment.RawContent != string(manifestJSON) || manifestAttachment.ContentSize != len(manifestJSON) || manifestAttachment.ContentHash != hashJSON(manifestJSON) {
		t.Fatalf("manifest attachment = %#v", manifestAttachment)
	}
	for _, attachment := range pkg.Attachments {
		if attachment.ContentSize != len([]byte(attachment.RawContent)) || attachment.ContentHash != hashJSON([]byte(attachment.RawContent)) {
			t.Fatalf("bad attachment metadata: %#v", attachment)
		}
	}
}

func TestPackageJSONRejectsUnsafeAndConflictingPaths(t *testing.T) {
	manifestJSON, _ := canonical(newPluginManifest("Example", "skill", "example", "", nil, nil))
	for _, attachmentPath := range []string{"../secret", "/absolute", `bad\\path`} {
		if _, err := packageJSON(manifestJSON, rawAttachment{path: attachmentPath, mimeType: "text/plain", content: "x"}); err == nil {
			t.Fatalf("unsafe path accepted: %q", attachmentPath)
		}
	}
	if _, err := packageJSON(manifestJSON,
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
	pkgJSON, err := packageJSON(manifestJSON)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(append(append(append([]byte{}, manifestJSON...), '\n'), pkgJSON...))
	want := "sha256:" + hex.EncodeToString(sum[:])
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
