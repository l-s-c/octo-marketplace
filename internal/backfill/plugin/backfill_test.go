package plugin

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func emptySourceExpectations(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT id,COUNT\\(\\*\\) FROM").WillReturnRows(sqlmock.NewRows([]string{"id", "count"}))
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
	if got.Expected.Plugins != 0 || len(got.Issues) != 1 {
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
	mock.ExpectQuery("SELECT id,COUNT\\(\\*\\) FROM").WillReturnRows(sqlmock.NewRows([]string{"id", "count"}).AddRow("expert-1", 1).AddRow("squad-1", 1))
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
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
