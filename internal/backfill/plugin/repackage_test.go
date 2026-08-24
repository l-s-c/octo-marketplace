package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	libplugin "codex.mlamp.cn/dmwork/octo-plugin-lib/plugin"
	"github.com/DATA-DOG/go-sqlmock"
)

func attachmentJSON(path, mime, content string) string {
	raw, _ := json.Marshal(map[string]any{
		"path": path, "content_type": "raw", "mime_type": mime,
		"raw_content": content, "content_size": len(content), "content_hash": hashJSON([]byte(content)),
	})
	return string(raw)
}

func TestTransformPackageRenamesExpertEntriesAndDropsManifest(t *testing.T) {
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
		strings.Contains(text, "expert/instruction.md") || strings.Contains(text, "manifest.json") {
		t.Fatalf("out=%s", text)
	}
	if _, again, err := transformPackage(out, "expert", manifest); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
	}
}

func TestTransformPackageCollapsesTeamToSingleAgentsFile(t *testing.T) {
	manifest := []byte(`{"plugin_name":"产品研发专家团","name":"team-a","description":"跨职能协作"}`)
	pkg := []byte(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("AGENTS.md", "text/markdown", "# 旧版无依赖节") + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `,` +
		attachmentJSON("team/config.json", "application/json", `{"leader":"Alice","strategies":["先澄清目标","再评估风险"],"dependencies":{"blocking":["需求文档"],"recommended":["设计稿"]},"permission":"open"}`) + `]}`)
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
	// Contract layout: exactly one attachment, the re-synthesized AGENTS.md
	// folding in leader/strategies/dependencies/permission.
	if len(doc.Attachments) != 1 || doc.Attachments[0].Path != "AGENTS.md" {
		t.Fatalf("attachments = %#v", doc.Attachments)
	}
	deps := map[string]any{"blocking": []any{"需求文档"}, "recommended": []any{"设计稿"}}
	want := teamAgentsMarkdown("产品研发专家团", "跨职能协作", "Alice", []any{"先澄清目标", "再评估风险"}, deps, "open")
	if doc.Attachments[0].RawContent != want {
		t.Fatalf("agents=%q want=%q", doc.Attachments[0].RawContent, want)
	}
	if !strings.Contains(want, "### 依赖") || !strings.Contains(want, "### 权限") {
		t.Fatalf("renderer missing sections: %q", want)
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
	if strings.Contains(text, "connector/config.json") || strings.Contains(text, "manifest.json") || !strings.Contains(text, `"path":"mcp.json"`) {
		t.Fatalf("config not converted: %s", text)
	}
	if !strings.Contains(text, `\"TOKEN\":\"${TOKEN}\"`) || !strings.Contains(text, `\"Authorization\":\"${AUTHORIZATION}\"`) {
		t.Fatalf("placeholders missing: %s", text)
	}
	if _, again, err := transformPackage(out, "connector", manifest); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
	}
}

func TestTransformPackageStripsManifestFromSkillOnly(t *testing.T) {
	manifest := []byte(`{"plugin_name":"S","name":"s","description":"d"}`)
	withManifest := []byte(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# doc") + `,` +
		attachmentJSON("manifest.json", "application/json", "{}") + `]}`)
	out, changed, err := transformPackage(withManifest, "skill", manifest)
	if err != nil || !changed || strings.Contains(string(out), "manifest.json") {
		t.Fatalf("changed=%v err=%v out=%s", changed, err, out)
	}
	// Already-contract skill packages round-trip untouched.
	if _, again, err := transformPackage(out, "skill", manifest); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
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
// shape: guarded by the pre-migration hash and carrying the lib-formula hash.
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
	want, err := libplugin.ComputePluginHash([]byte(manifest), []byte(newPkg))
	if err != nil || action.args[1] != want {
		t.Fatalf("hash=%v want=%s err=%v", action.args[1], want, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRepackagePluginsRewritesHashEvenWithoutContentChange: the formula switch
// means content-identical rows still need a plugin_hash update.
func TestRepackagePluginsRewritesHashEvenWithoutContentChange(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	manifest := `{"plugin_name":"S","name":"s","description":"d"}`
	pkg := `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# doc") + `]}`
	libHash, err := libplugin.ComputePluginHash([]byte(manifest), []byte(pkg))
	if err != nil {
		t.Fatal(err)
	}
	rows := sqlmock.NewRows([]string{"plugin_id", "plugin_type", "manifest_json", "plugin_json", "plugin_hash"}).
		AddRow("s1", "skill", manifest, pkg, "sha256:legacy-formula").
		AddRow("s2", "skill", manifest, pkg, libHash)
	mock.ExpectQuery(`SELECT plugin_id, plugin_type, .* FROM plugins`).WillReturnRows(rows)
	var p repackagePlan
	if err := r.repackagePlugins(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	// s1 gets a hash-only rewrite; s2 is already consistent → no action.
	if len(p.actions) != 1 || p.actions[0].args[2] != "s1" || p.actions[0].args[1] != libHash {
		t.Fatalf("actions=%#v", p.actions)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRepackageAuditsRepairsHashChain feeds one plugin's full audit history
// (create -> update -> delete) in old layout and asserts the rebuilt chain
// under the lib formula; the delete row keeps its NULL after_hash.
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
	newPkg := p.actions[0].args[0].(string)
	want, err := libplugin.ComputePluginHash([]byte(manifest), []byte(newPkg))
	if err != nil {
		t.Fatal(err)
	}
	if p.actions[0].args[1] != nil || p.actions[0].args[2] != any(want) {
		t.Fatalf("a1 before=%v after=%v want=%s", p.actions[0].args[1], p.actions[0].args[2], want)
	}
	if p.actions[1].args[1] != any(want) || p.actions[1].args[2] != any(want) {
		t.Fatalf("a2 before=%v after=%v want=%s", p.actions[1].args[1], p.actions[1].args[2], want)
	}
	if p.actions[2].args[1] != any(want) || p.actions[2].args[2] != nil {
		t.Fatalf("a3 before=%v after=%v", p.actions[2].args[1], p.actions[2].args[2])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestRepackageAuditsLeavesConsistentContractRowsAlone: already-migrated
// snapshots whose chain is intact must produce zero actions (idempotency).
func TestRepackageAuditsLeavesConsistentContractRowsAlone(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	manifest, oldPkg := oldExpertPkg(t)
	newPkg, _, err := transformPackage([]byte(oldPkg), "expert", []byte(manifest))
	if err != nil {
		t.Fatal(err)
	}
	consistent, err := libplugin.ComputePluginHash([]byte(manifest), newPkg)
	if err != nil {
		t.Fatal(err)
	}
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

// TestRepackageRelationsRenamesAndRekeysDeterministicRows: backfill-derived
// relation IDs embed the type string and must be re-derived; API-created rows
// keep their IDs.
func TestRepackageRelationsRenamesAndRekeysDeterministicRows(t *testing.T) {
	r, mock, done := repackageRunner(t)
	defer done()
	oldID := deterministicRelationID("team-1", legacyTeamRelation, 0, "member-1")
	rows := sqlmock.NewRows([]string{"relation_id", "source_plugin_id", "target_plugin_id", "sort_order"}).
		AddRow(oldID, "team-1", "member-1", 0).
		AddRow("api-created-id", "team-2", "member-2", 1)
	mock.ExpectQuery(`SELECT relation_id, source_plugin_id, target_plugin_id, sort_order FROM plugin_relations WHERE relation_type=`).
		WillReturnRows(rows)
	var p repackagePlan
	if err := r.repackageRelations(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 2 {
		t.Fatalf("actions=%d", len(p.actions))
	}
	wantNew := deterministicRelationID("team-1", contractTeamRelation, 0, "member-1")
	if p.actions[0].args[0] != wantNew || p.actions[0].args[1] != contractTeamRelation || p.actions[0].args[2] != oldID {
		t.Fatalf("deterministic row args=%#v", p.actions[0].args)
	}
	if p.actions[1].args[0] != "api-created-id" {
		t.Fatalf("api row rekeyed: %#v", p.actions[1].args)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTransformVersionRelationsRewritesSnapshotEntries(t *testing.T) {
	oldID := deterministicRelationID("team-1", legacyTeamRelation, 2, "member-9")
	raw := []byte(`[{"relation_id":"` + oldID + `","target_plugin_id":"member-9","relation_type":"expert_team_member","sort_order":2,"relation":{"is_leader":true}},` +
		`{"relation_id":"keep-1","target_plugin_id":"skill-1","relation_type":"expert_skill","sort_order":0,"relation":null}]`)
	out, changed, err := transformVersionRelations(raw, "team-1")
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	text := string(out)
	wantNew := deterministicRelationID("team-1", contractTeamRelation, 2, "member-9")
	if strings.Contains(text, legacyTeamRelation) || !strings.Contains(text, contractTeamRelation) || !strings.Contains(text, wantNew) || strings.Contains(text, oldID) {
		t.Fatalf("out=%s", text)
	}
	if !strings.Contains(text, `"relation_id":"keep-1"`) {
		t.Fatalf("unrelated entry mangled: %s", text)
	}
	if _, again, err := transformVersionRelations(out, "team-1"); err != nil || again {
		t.Fatalf("second pass changed=%v err=%v", again, err)
	}
}
