package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func attachmentJSON(path, mime, content string) string {
	raw, _ := json.Marshal(map[string]any{
		"path": path, "content_type": "raw", "mime_type": mime,
		"raw_content": content, "content_size": len(content), "content_hash": hashJSON([]byte(content)),
	})
	return string(raw)
}

func TestTransformPackageRenamesExpertEntries(t *testing.T) {
	manifest := []byte(`{"plugin_name":"专家","name":"expert-a","description":"d"}`)
	pkg := []byte(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("expert/instruction.md", "text/markdown", "do work") + `,` +
		attachmentJSON("expert/mcp.json", "application/json", `{"mcpServers":{}}`) + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `]}`)
	out, changed, err := transformPackage(pkg, "expert", manifest)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	text := string(out)
	if !strings.Contains(text, `"path":"AGENTS.md"`) || !strings.Contains(text, `"path":"mcp.json"`) ||
		strings.Contains(text, "expert/instruction.md") || strings.Contains(text, "expert/mcp.json") {
		t.Fatalf("out=%s", text)
	}
	// Attachments stay path-sorted and the pass is idempotent.
	if strings.Index(text, `"path":"AGENTS.md"`) > strings.Index(text, `"path":"manifest.json"`) {
		t.Fatalf("not sorted: %s", text)
	}
	if _, again, err := transformPackage(out, "expert", manifest); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
	}
}

func TestTransformPackageSynthesizesTeamAgentsEntry(t *testing.T) {
	manifest := []byte(`{"plugin_name":"产品研发专家团","name":"team-a","description":"跨职能协作"}`)
	pkg := []byte(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("manifest.json", "application/json", "{}") + `,` +
		attachmentJSON("team/config.json", "application/json", `{"leader":"Alice","strategies":["先澄清目标","再评估风险"],"permission":"open"}`) + `]}`)
	out, changed, err := transformPackage(pkg, "expert_team", manifest)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	var doc struct {
		Attachments []struct {
			Path       string `json:"path"`
			RawContent string `json:"raw_content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	var agents string
	for _, a := range doc.Attachments {
		if a.Path == "AGENTS.md" {
			agents = a.RawContent
		}
	}
	want := teamAgentsMarkdown("产品研发专家团", "跨职能协作", "Alice", []any{"先澄清目标", "再评估风险"})
	if agents != want {
		t.Fatalf("agents=%q want=%q", agents, want)
	}
	if !strings.Contains(string(out), `"path":"team/config.json"`) {
		t.Fatalf("team config dropped: %s", out)
	}
	if _, again, err := transformPackage(out, "expert_team", manifest); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
	}
}

func TestTransformPackageConvertsConnectorConfig(t *testing.T) {
	manifest := []byte(`{"plugin_name":"Jira","name":"jira","description":"d"}`)
	config := `{"config":{"url":"https://mcp.example.com/mcp","env":{"REGION":""},"envUserSupplied":["TOKEN"],"authType":"bearer","serverName":"jira"},"transport":"streamable-http"}`
	pkg := []byte(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("connector/config.json", "application/json", config) + `,` +
		attachmentJSON("connector/tools.json", "application/json", `[{"name":"a"}]`) + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `]}`)
	out, changed, err := transformPackage(pkg, "connector", manifest)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	text := string(out)
	if !strings.Contains(text, `"connector":{"source":"connector.jira","type":"mcp"}`) {
		t.Fatalf("descriptor missing: %s", text)
	}
	if strings.Contains(text, "connector/config.json") || !strings.Contains(text, `"path":"mcp.json"`) {
		t.Fatalf("config not converted: %s", text)
	}
	if !strings.Contains(text, `\"TOKEN\":\"${TOKEN}\"`) || !strings.Contains(text, `\"Authorization\":\"${AUTHORIZATION}\"`) {
		t.Fatalf("placeholders missing: %s", text)
	}
	if !strings.Contains(text, `"path":"connector/tools.json"`) {
		t.Fatalf("tools dropped: %s", text)
	}
	if _, again, err := transformPackage(out, "connector", manifest); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
	}
}

func TestTransformPackageLeavesSkillUntouched(t *testing.T) {
	manifest := []byte(`{"plugin_name":"S","name":"s","description":"d"}`)
	pkg := []byte(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# doc") + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `]}`)
	out, changed, err := transformPackage(pkg, "skill", manifest)
	if err != nil || changed || string(out) != string(pkg) {
		t.Fatalf("changed=%v err=%v out=%s", changed, err, out)
	}
}

func repackageRunner(t *testing.T) (*Runner, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	return New(db), mock, func() { db.Close() }
}

func oldExpertPkg(t *testing.T) (string, string) {
	t.Helper()
	manifest := `{"plugin_name":"专家","name":"expert-a","description":"d"}`
	pkg := `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("expert/instruction.md", "text/markdown", "do work") + `,` +
		attachmentJSON("expert/mcp.json", "application/json", `{"mcpServers":{}}`) + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `]}`
	return manifest, pkg
}

// TestRepackagePluginsGuardsUpdateWithOldHash pins the plan builder's UPDATE
// shape: guarded by the pre-migration hash and carrying the recomputed one.
func TestRepackagePluginsGuardsUpdateWithOldHash(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	manifest, pkg := oldExpertPkg(t)
	mock.ExpectQuery(`SELECT plugin_id, plugin_type, manifest_json, plugin_json, plugin_hash FROM plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "plugin_type", "manifest_json", "plugin_json", "plugin_hash"}).
			AddRow("p1", "expert", manifest, pkg, "sha256:old"))
	var p repackagePlan
	if err := r.repackagePlugins(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 1 || len(p.issues) != 0 {
		t.Fatalf("actions=%d issues=%#v", len(p.actions), p.issues)
	}
	action := p.actions[0]
	if action.args[2] != "p1" || action.args[3] != "sha256:old" {
		t.Fatalf("guard args = %#v", action.args)
	}
	newPkg := action.args[0].(string)
	if strings.Contains(newPkg, "expert/instruction.md") || !strings.Contains(newPkg, `"path":"AGENTS.md"`) {
		t.Fatalf("package not transformed: %s", newPkg)
	}
	canonicalManifest, _ := canonicalJSONBytes([]byte(manifest))
	if action.args[1] != documentHash(canonicalManifest, []byte(newPkg)) {
		t.Fatalf("hash mismatch: %v", action.args[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRepackageAuditsRepairsHashChain feeds one plugin's full audit history
// (create -> update -> delete with a NULL after-hash and snapshot-of-before)
// in old layout and asserts the rebuilt chain: every after_hash is the hash of
// the transformed snapshot pair and each before_hash equals the previous
// row's after_hash; the delete row keeps its NULL after_hash.
func TestRepackageAuditsRepairsHashChain(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	manifest, pkg := oldExpertPkg(t)
	rows := sqlmock.NewRows([]string{"audit_log_id", "plugin_id", "manifest_snapshot_json", "plugin_snapshot_json", "before_hash", "after_hash"}).
		AddRow("a1", "p1", manifest, pkg, nil, "sha256:after1-old").
		AddRow("a2", "p1", manifest, pkg, "sha256:after1-old", "sha256:after2-old").
		AddRow("a3", "p1", manifest, pkg, "sha256:after2-old", nil)
	mock.ExpectQuery(`SELECT audit_log_id, plugin_id, manifest_snapshot_json, plugin_snapshot_json, before_hash, after_hash FROM plugin_audit_logs`).
		WillReturnRows(rows)
	var p repackagePlan
	if err := r.repackageAudits(context.Background(), map[string]string{"p1": "expert"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 3 || len(p.issues) != 0 {
		t.Fatalf("actions=%d issues=%#v", len(p.actions), p.issues)
	}
	// All three snapshots transform identically, so the repaired chain hash is
	// the same transformed-pair hash everywhere.
	newPkg := p.actions[0].args[0].(string)
	canonicalManifest, _ := canonicalJSONBytes([]byte(manifest))
	canonicalPkg, _ := canonicalJSONBytes([]byte(newPkg))
	want := documentHash(canonicalManifest, canonicalPkg)
	// a1: create — before stays NULL, after repaired.
	if p.actions[0].args[1] != nil || p.actions[0].args[2] != any(want) {
		t.Fatalf("a1 before=%v after=%v want=%s", p.actions[0].args[1], p.actions[0].args[2], want)
	}
	// a2: update — before chains to a1's repaired after, after repaired.
	if p.actions[1].args[1] != any(want) || p.actions[1].args[2] != any(want) {
		t.Fatalf("a2 before=%v after=%v want=%s", p.actions[1].args[1], p.actions[1].args[2], want)
	}
	// a3: delete — before chains to a2's repaired after, after stays NULL.
	if p.actions[2].args[1] != any(want) || p.actions[2].args[2] != nil {
		t.Fatalf("a3 before=%v after=%v", p.actions[2].args[1], p.actions[2].args[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRepackageAuditsLeavesConsistentNewLayoutRowsAlone: already-migrated
// snapshots whose chain is intact must produce zero actions (idempotency).
func TestRepackageAuditsLeavesConsistentNewLayoutRowsAlone(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	manifest, oldPkg := oldExpertPkg(t)
	newPkg, _, err := transformPackage([]byte(oldPkg), "expert", []byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	canonicalManifest, _ := canonicalJSONBytes([]byte(manifest))
	consistent := documentHash(canonicalManifest, newPkg)
	rows := sqlmock.NewRows([]string{"audit_log_id", "plugin_id", "manifest_snapshot_json", "plugin_snapshot_json", "before_hash", "after_hash"}).
		AddRow("a1", "p1", manifest, string(newPkg), nil, consistent).
		AddRow("a2", "p1", manifest, string(newPkg), consistent, consistent)
	mock.ExpectQuery(`SELECT audit_log_id, plugin_id, .* FROM plugin_audit_logs`).WillReturnRows(rows)
	var p repackagePlan
	if err := r.repackageAudits(context.Background(), map[string]string{"p1": "expert"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 0 {
		t.Fatalf("expected no actions, got %d: %#v", len(p.actions), p.actions[0].args)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRepackagePluginsRejectsSecretBearingTransform: a stored config whose
// conversion would persist a live secret is skipped, never written.
func TestRepackagePluginsRejectsSecretBearingTransform(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	manifest := `{"plugin_name":"Jira","name":"jira","description":"d"}`
	config := `{"config":{"url":"https://x","headers":{"Authorization":"Bearer real-token"}},"transport":"sse"}`
	pkg := `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("connector/config.json", "application/json", config) + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `]}`
	mock.ExpectQuery(`SELECT plugin_id, plugin_type, manifest_json, plugin_json, plugin_hash FROM plugins`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "plugin_type", "manifest_json", "plugin_json", "plugin_hash"}).
			AddRow("c1", "connector", manifest, pkg, "sha256:old"))
	var p repackagePlan
	if err := r.repackagePlugins(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 0 || len(p.issues) != 1 || p.issues[0].Code != "repackage_secret_rejected" {
		t.Fatalf("actions=%d issues=%#v", len(p.actions), p.issues)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
