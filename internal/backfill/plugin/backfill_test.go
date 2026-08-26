package plugin

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func emptySourceExpectations(mock sqlmock.Sqlmock) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM expert_categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name FROM skill_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name FROM expert_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name,display_name").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id", "description", "category_id", "tags", "owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version", "readme_content", "file_name", "file_url", "file_size", "file_sha256", "created_at", "updated_at", "is_deleted"}))
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows([]string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "instruction", "mcp_config", "skills_json", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows([]string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "leader", "strategies_json", "dependencies_json", "permission", "members_json", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name,slug,slogan").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "slug", "slogan", "category", "icon", "icon_version", "tags_json", "tools_json", "usage_examples_json", "faqs_json", "notes_json", "visibility", "owner_uid", "space_id", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "transport", "config_json", "created_at", "updated_at", "deleted_at"}))
}
func TestDryRunDoesNotWrite(t *testing.T) {
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	emptySourceExpectations(mock)
	r := New(db)
	r.now = func() time.Time { return time.Unix(1, 0) }
	got, e := r.Run(context.Background(), Options{Mode: ModeDryRun})
	if e != nil {
		t.Fatal(e)
	}
	if got.Expected.Plugins != 0 || len(got.Issues) != 0 {
		t.Fatalf("unexpected report: %#v", got)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestVerifyEmptyPlanDoesNotWrite(t *testing.T) {
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	emptySourceExpectations(mock)
	got, e := New(db).Run(context.Background(), Options{Mode: ModeVerify})
	if e != nil {
		t.Fatal(e)
	}
	if got.Observed.Missing != 0 || got.ObservedHash == "" {
		t.Fatalf("unexpected report: %#v", got)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestBuildExpertAndSquadGraph(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(100, 0).UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM expert_categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}).AddRow("cat", "Category", "icon", 1, now, now, nil))
	mock.ExpectQuery("SELECT id,name FROM skill_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name FROM expert_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).AddRow(7, "Expert Tag"))
	mock.ExpectQuery("SELECT id,name,display_name").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id", "description", "category_id", "tags", "owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version", "readme_content", "file_name", "file_url", "file_size", "file_sha256", "created_at", "updated_at", "is_deleted"}))
	expertCols := []string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "instruction", "mcp_config", "skills_json", "created_at", "updated_at", "deleted_at"}
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows(expertCols).AddRow("expert-1", "E", "Expert", "summary", "cat", `[7]`, "pub", "owner", "creator", "human", nil, nil, "space", "public", "instruction", `{"env":{"TOKEN":"secret"}}`, `[{"name":"Skill","object_key":"stable/key"}]`, now, now, nil))
	squadCols := []string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "leader", "strategies_json", "dependencies_json", "permission", "members_json", "created_at", "updated_at", "deleted_at"}
	members := `[{"member_key":"lead","name":"Lead","role":"lead","is_leader":true,"instruction":"do","mcp_config":"{\"headers\":{\"Authorization\":\"secret\"}}","skills":[{"name":"Member Skill"}]}]`
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows(squadCols).AddRow("squad-1", "S", "Squad", "summary", "cat", `[7]`, "pub", "owner", "creator", "human", nil, nil, "space", "public", "Lead", `[]`, `{"blocking":[],"recommended":[]}`, "perm", members, now, now, nil))
	mock.ExpectQuery("SELECT id,name,slug,slogan").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "slug", "slogan", "category", "icon", "icon_version", "tags_json", "tools_json", "usage_examples_json", "faqs_json", "notes_json", "visibility", "owner_uid", "space_id", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "transport", "config_json", "created_at", "updated_at", "deleted_at"}))
	p, err := New(db).build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.plugins) != 5 || len(p.versions) != 5 || len(p.relations) != 3 {
		t.Fatalf("counts: plugins=%d versions=%d relations=%d", len(p.plugins), len(p.versions), len(p.relations))
	}
	for _, x := range p.plugins {
		if x.visibility != "space" {
			t.Fatalf("legacy public visibility not scoped: %#v", x)
		}
		if regexp.MustCompile(`secret`).MatchString(x.pkg) {
			t.Fatalf("secret leaked in package: %s", x.pkg)
		}
	}
	if p.plugins[0].id == "Skill" {
		t.Fatal("snapshot skill identity inferred from name")
	}
	for _, plugin := range p.plugins {
		assertCompliantPluginDocuments(t, plugin)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func assertCompliantPluginDocuments(t *testing.T, plugin plugRow) {
	t.Helper()
	var manifestValue pluginManifest
	if err := json.Unmarshal([]byte(plugin.manifest), &manifestValue); err != nil {
		t.Fatalf("plugin %q manifest: %v", plugin.id, err)
	}
	if manifestValue.Schema != "cowork-plugin-manifest-1.0.json" || manifestValue.PluginName != plugin.name || manifestValue.PluginType != plugin.typ || manifestValue.Labels == nil || manifestValue.Examples == nil {
		t.Fatalf("plugin %q manifest is not compliant: %#v", plugin.id, manifestValue)
	}
	var packageValue pluginPackage
	if err := json.Unmarshal([]byte(plugin.pkg), &packageValue); err != nil {
		t.Fatalf("plugin %q package: %v", plugin.id, err)
	}
	if packageValue.Schema != "cowork-plugin-package-1.0.json" || len(packageValue.Attachments) == 0 {
		t.Fatalf("plugin %q package is not compliant: %#v", plugin.id, packageValue)
	}
	paths := make([]string, 0, len(packageValue.Attachments))
	for _, attachment := range packageValue.Attachments {
		paths = append(paths, attachment.Path)
		if attachment.ContentType != "raw" || attachment.ContentSize != len([]byte(attachment.RawContent)) || attachment.ContentHash != hashJSON([]byte(attachment.RawContent)) {
			t.Fatalf("plugin %q attachment invalid: %#v", plugin.id, attachment)
		}
		// Contract layout: the manifest lives only in the manifest_json
		// column, never as an embedded attachment.
		if attachment.Path == "manifest.json" {
			t.Fatalf("plugin %q embeds manifest.json", plugin.id)
		}
	}
	if !sort.StringsAreSorted(paths) {
		t.Fatalf("plugin %q package paths = %#v", plugin.id, paths)
	}
	if plugin.mhash != hashJSON([]byte(plugin.manifest)) || plugin.phash != both([]byte(plugin.manifest), []byte(plugin.pkg)) {
		t.Fatalf("plugin %q hashes do not match canonical documents", plugin.id)
	}
}

func TestBuildSkillAndConnectorDocuments(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(100, 0).UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}).AddRow("cat", "Category", "icon", 1, now, now, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM expert_categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name FROM skill_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}).AddRow(1, "Skill Tag"))
	mock.ExpectQuery("SELECT id,name FROM expert_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	skillCols := []string{"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id", "description", "category_id", "tags", "owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version", "readme_content", "file_name", "file_url", "file_size", "file_sha256", "created_at", "updated_at", "is_deleted"}
	mock.ExpectQuery("SELECT id,name,display_name").WillReturnRows(sqlmock.NewRows(skillCols).AddRow("skill-1", "canonical-skill", "Display Skill", "", "", "", "description", "cat", `[1]`, "owner", "Owner", "creator-id", "Creator", "space", "space", "1.0.0", "# Skill", "skill.zip", "https://temporary.invalid", 10, "legacy-hash", now, now, false))
	mock.ExpectQuery("SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions").WithArgs("skill-1").WillReturnRows(sqlmock.NewRows([]string{"id", "version", "changelog", "storage", "changed_by", "created_at"}))
	expertCols := []string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "instruction", "mcp_config", "skills_json", "created_at", "updated_at", "deleted_at"}
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows(expertCols))
	squadCols := []string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "leader", "strategies_json", "dependencies_json", "permission", "members_json", "created_at", "updated_at", "deleted_at"}
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows(squadCols))
	connectorCols := []string{"id", "name", "slug", "slogan", "category", "icon", "icon_version", "tags_json", "tools_json", "usage_examples_json", "faqs_json", "notes_json", "visibility", "owner_uid", "space_id", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "transport", "config_json", "created_at", "updated_at", "deleted_at"}
	mock.ExpectQuery("SELECT id,name,slug,slogan").WillReturnRows(sqlmock.NewRows(connectorCols).AddRow("connector-1", "Connector", "connector-slug", "slogan", "cat", "icon", 1, `["connector-tag"]`, `[{"name":"tool"}]`, `["try connector"]`, `[]`, `[]`, "space", "owner", "space", "Creator", "human", nil, nil, "stdio", `{"command":"run","env":{"TOKEN":"actual"},"headers":{"Authorization":"actual"}}`, now, now, nil))
	p, err := New(db).build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.plugins) != 2 {
		t.Fatalf("plugins=%d issues=%#v", len(p.plugins), p.issues)
	}
	for _, plugin := range p.plugins {
		assertCompliantPluginDocuments(t, plugin)
		if regexp.MustCompile(`actual|temporary\\.invalid`).MatchString(plugin.pkg) {
			t.Fatalf("sensitive or temporary value leaked: %s", plugin.pkg)
		}
	}
	byType := make(map[string]plugRow, len(p.plugins))
	for _, plugin := range p.plugins {
		if _, duplicate := byType[plugin.typ]; duplicate {
			t.Fatalf("duplicate plugin type %q in %#v", plugin.typ, p.plugins)
		}
		byType[plugin.typ] = plugin
	}
	skillPlugin, ok := byType["skill"]
	if !ok || skillPlugin.id != PluginID("skill", "skill-1") || skillPlugin.name != "Display Skill" {
		t.Fatalf("skill plugin = %#v; all plugins = %#v", skillPlugin, p.plugins)
	}
	connectorPlugin, ok := byType["connector"]
	if !ok || connectorPlugin.id != PluginID("connector", "connector-1") || connectorPlugin.name != "Connector" {
		t.Fatalf("connector plugin = %#v; all plugins = %#v", connectorPlugin, p.plugins)
	}
	var connectorManifest pluginManifest
	if err := json.Unmarshal([]byte(connectorPlugin.manifest), &connectorManifest); err != nil {
		t.Fatal(err)
	}
	if connectorManifest.Name != "connector-slug" || len(connectorManifest.Examples) != 1 || connectorManifest.Examples[0].Input != "try connector" {
		t.Fatalf("connector manifest = %#v", connectorManifest)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSkillVersionsSelectsExactCurrentPointer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created := time.Unix(100, 0).UTC()
	manifest := []byte(`{"manifest":true}`)
	pkg := []byte(`{"package":true}`)
	mock.ExpectQuery("SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions").WithArgs("skill-1").WillReturnRows(
		sqlmock.NewRows([]string{"id", "version", "changelog", "storage", "changed_by", "created_at"}).
			AddRow("version-a", "1.0.0", nil, `{}`, "author-a", created).
			AddRow("version-b", "2.0.0", "selected", `{}`, "author-b", created.Add(time.Hour)),
	)
	versions, selected, issue, err := New(db).skillVersions(context.Background(), "skill-1", "storage-plugin", "version-a", "fallback", "fallback-author", created, manifest, pkg)
	if err != nil {
		t.Fatal(err)
	}
	if issue != nil || len(versions) != 2 || selected.id != DeterministicID("skillver", "version-a") || selected.version != "1.0.0" {
		t.Fatalf("versions=%#v selected=%#v issue=%#v", versions, selected, issue)
	}
	if selected.manifest != string(manifest) || selected.pkg != string(pkg) || selected.phash != both(manifest, pkg) {
		t.Fatalf("selected payload mismatch: %#v", selected)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSkillVersionsSelectsDeterministicLatestWhenPointerEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created := time.Unix(100, 0).UTC()
	mock.ExpectQuery("SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions").WithArgs("skill-1").WillReturnRows(
		sqlmock.NewRows([]string{"id", "version", "changelog", "storage", "changed_by", "created_at"}).
			AddRow("version-a", "1.0.0", nil, nil, "", created).
			AddRow("version-b", "1.1.0", nil, nil, "", created),
	)
	versions, selected, issue, err := New(db).skillVersions(context.Background(), "skill-1", "storage-plugin", "", "fallback", "fallback-author", created, []byte(`{}`), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if issue != nil || len(versions) != 2 || selected.id != DeterministicID("skillver", "version-b") {
		t.Fatalf("versions=%#v selected=%#v issue=%#v", versions, selected, issue)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSkillVersionsCreatesSyntheticCurrentWithoutHistory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	created := time.Unix(100, 0).UTC()
	mock.ExpectQuery("SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions").WithArgs("skill-1").WillReturnRows(
		sqlmock.NewRows([]string{"id", "version", "changelog", "storage", "changed_by", "created_at"}),
	)
	versions, selected, issue, err := New(db).skillVersions(context.Background(), "skill-1", "storage-plugin", "", "1.0.0", "author", created, []byte(`{}`), []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 1 || selected.id != versions[0].id || issue == nil || selected.version != "1.0.0" {
		t.Fatalf("versions=%#v selected=%#v issue=%#v", versions, selected, issue)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSkillVersionsRejectsDanglingCurrentPointer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions").WithArgs("skill-1").WillReturnRows(
		sqlmock.NewRows([]string{"id", "version", "changelog", "storage", "changed_by", "created_at"}).
			AddRow("version-a", "1.0.0", nil, nil, "author", time.Unix(100, 0).UTC()),
	)
	if _, _, _, err := New(db).skillVersions(context.Background(), "skill-1", "storage-plugin", "missing", "fallback", "fallback-author", time.Time{}, []byte(`{}`), []byte(`{}`)); err == nil {
		t.Fatal("dangling current pointer accepted")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyCountersSurviveVerificationProjection(t *testing.T) {
	rep := Report{Observed: Counts{Inserted: 3, Existing: 4}}
	inserted, existing := rep.Observed.Inserted, rep.Observed.Existing
	rep.Observed = Counts{Plugins: 7}
	rep.Observed.Inserted, rep.Observed.Existing = inserted, existing
	if rep.Observed.Inserted != 3 || rep.Observed.Existing != 4 || rep.Observed.Plugins != 7 {
		t.Fatalf("apply counters lost: %#v", rep.Observed)
	}
}

func TestPlanHashMatchesVerifyProjection(t *testing.T) {
	p := plan{
		cats:      []catRow{{id: "cat", name: "Category", types: `["skill"]`}},
		plugins:   []plugRow{{id: "plugin", phash: "plugin-hash"}},
		relations: []relRow{{id: "relation", source: "plugin", target: "target", typ: "expert_skill", order: 2, data: `{"b":2,"a":1}`}},
		versions:  []verRow{{id: "version", phash: "version-hash", relations: `[{"target_plugin_id":"target"}]`}},
	}
	lines := []string{
		`c:cat:Category:["skill"]`,
		`p:plugin:plugin-hash`,
		`r:relation:plugin:target:expert_skill:2:{"a":1,"b":2}`,
		`v:version:version-hash:[{"target_plugin_id":"target"}]`,
	}
	if got, want := p.hash(), digestLines(lines); got != want {
		t.Fatalf("plan hash %q want verify projection %q", got, want)
	}
}

func TestApplyRowExactExisting(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM plugins WHERE plugin_id").WithArgs("p", "expected").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := Report{}
	if err = applyRow(context.Background(), tx, &rep, "plugins", "plugin_id", "p", "SELECT COUNT(*) FROM plugins WHERE plugin_id=? AND plugin_name=?", "INSERT INTO plugins VALUES(?)", []any{"p", "expected"}, []any{"p"}); err != nil {
		t.Fatal(err)
	}
	if rep.Observed.Existing != 1 || rep.Observed.Inserted != 0 {
		t.Fatalf("report %#v", rep.Observed)
	}
	mock.ExpectRollback()
	_ = tx.Rollback()
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestApplyRowConflictDoesNotInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM plugins WHERE plugin_id").WithArgs("p", "expected").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))
	mock.ExpectQuery("SELECT COUNT\\(\\*\\) FROM plugins WHERE plugin_id=\\?").WithArgs("p").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep := Report{}
	err = applyRow(context.Background(), tx, &rep, "plugins", "plugin_id", "p", "SELECT COUNT(*) FROM plugins WHERE plugin_id=? AND plugin_name=?", "INSERT INTO plugins VALUES(?)", []any{"p", "expected"}, []any{"p"})
	if err == nil || rep.Observed.Inserted != 0 {
		t.Fatalf("expected conflict, report=%#v err=%v", rep.Observed, err)
	}
	mock.ExpectRollback()
	_ = tx.Rollback()
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePlanReferencesRejectsDanglingRows(t *testing.T) {
	now := time.Unix(1, 0).UTC()
	base := plan{
		cats:     []catRow{{id: "cat"}},
		plugins:  []plugRow{{id: "plugin", cat: "cat", versionID: "version"}},
		versions: []verRow{{id: "version", pid: "plugin", created: now}},
	}
	if err := validatePlanReferences(base); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
	cases := []plan{
		{plugins: []plugRow{{id: "plugin", cat: "missing", versionID: "version"}}, versions: []verRow{{id: "version", pid: "plugin"}}},
		{plugins: []plugRow{{id: "plugin", versionID: "version"}}, versions: []verRow{{id: "version", pid: "plugin"}}, relations: []relRow{{id: "relation", source: "plugin", target: "missing"}}},
		{plugins: []plugRow{{id: "plugin", versionID: "missing"}}},
	}
	for i, candidate := range cases {
		if err := validatePlanReferences(candidate); err == nil {
			t.Fatalf("case %d accepted dangling reference", i)
		}
	}
}

func TestApplyEmptyPlanUsesTransaction(t *testing.T) {
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	emptySourceExpectations(mock)
	mock.ExpectBegin()
	mock.ExpectCommit()
	if _, e = New(db).Run(context.Background(), Options{Mode: ModeApply}); e != nil {
		t.Fatal(e)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}

func TestTopLevelSkillKeepsArtifactPointer(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(100, 0).UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM expert_categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name FROM skill_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name FROM expert_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	skillCols := []string{"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id", "description", "category_id", "tags", "owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version", "readme_content", "file_name", "file_url", "file_size", "file_sha256", "created_at", "updated_at", "is_deleted"}
	mock.ExpectQuery("SELECT id,name,display_name").WillReturnRows(sqlmock.NewRows(skillCols).AddRow("skill-1", "prd-outline", "PRD Outline", "", "", "", "desc", "", `[]`, "owner", "Owner", "creator-1", "Creator", "space", "space", "1.0.0", "# readme", "prd.zip", "oss://bucket/prd.zip", 2048, "abc123", now, now, false))
	mock.ExpectQuery("SELECT id,version,changelog,storage,changed_by,created_at FROM skill_versions").WillReturnRows(sqlmock.NewRows([]string{"id", "version", "changelog", "storage", "changed_by", "created_at"}))
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows([]string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "instruction", "mcp_config", "skills_json", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows([]string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "leader", "strategies_json", "dependencies_json", "permission", "members_json", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name,slug,slogan").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "slug", "slogan", "category", "icon", "icon_version", "tags_json", "tools_json", "usage_examples_json", "faqs_json", "notes_json", "visibility", "owner_uid", "space_id", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "transport", "config_json", "created_at", "updated_at", "deleted_at"}))
	p, err := New(db).build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.plugins) != 1 {
		t.Fatalf("plugins=%d issues=%#v", len(p.plugins), p.issues)
	}
	pkg := p.plugins[0].pkg
	for _, want := range []string{`skill/ref.json`, `oss://bucket/prd.zip`, `abc123`, `\"file_size\":2048`} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(pkg) {
			t.Fatalf("package missing %q: %s", want, pkg)
		}
	}
}

func TestExpertWithInlineMCPRecordsUnlinkedConnectorIssue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Unix(100, 0).UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM expert_categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name FROM skill_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name FROM expert_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name,display_name").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id", "description", "category_id", "tags", "owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version", "readme_content", "file_name", "file_url", "file_size", "file_sha256", "created_at", "updated_at", "is_deleted"}))
	expertCols := []string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "instruction", "mcp_config", "skills_json", "created_at", "updated_at", "deleted_at"}
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows(expertCols).AddRow("expert-1", "E", "Expert", "summary", "", `[]`, "pub", "owner", "creator", "human", nil, nil, "space", "private", "", `{"mcpServers":{"octo":{"command":"octo-mcp"}}}`, `[]`, now, now, nil))
	mock.ExpectQuery("SELECT id,short_name,name,summary,category_id,tags,publisher,owner_uid,creator_name,created_by_type").WillReturnRows(sqlmock.NewRows([]string{"id", "short_name", "name", "summary", "category_id", "tags", "publisher", "owner_uid", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "space_id", "visibility", "leader", "strategies_json", "dependencies_json", "permission", "members_json", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name,slug,slogan").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "slug", "slogan", "category", "icon", "icon_version", "tags_json", "tools_json", "usage_examples_json", "faqs_json", "notes_json", "visibility", "owner_uid", "space_id", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "transport", "config_json", "created_at", "updated_at", "deleted_at"}))
	p, err := New(db).build(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(p.plugins) != 1 {
		t.Fatalf("plugins=%d issues=%#v", len(p.plugins), p.issues)
	}
	found := false
	for _, issue := range p.issues {
		if issue.Code == "expert_connector_unlinked" && issue.ID == "expert-1" && issue.Level == "info" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected expert_connector_unlinked issue, got %#v", p.issues)
	}
}
