package plugin

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestGetExplicitlyScopesCallerAndSpace(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := New(db)
	query := `SELECT .* FROM plugins p\s+WHERE p.plugin_id=\? AND p.deleted_at IS NULL AND \(p.visibility IN \('public','system'\) OR \(p.space_id = \? AND \(p.visibility = 'space' OR p.owner_uid = \?\)\)\)`
	mock.ExpectQuery(query).WithArgs("plugin-a", "space-a", "caller-a").WillReturnRows(sqlmock.NewRows(pluginTestColumns()))
	_, err = repo.Get(context.Background(), Scope{CallerUID: "caller-a", SpaceID: "space-a"}, "plugin-a")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetDoesNotRetryWithoutScope(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(`SELECT .* WHERE p.plugin_id=\? .*p.space_id = \? .*p.owner_uid = \?`).WithArgs("foreign", "space-a", "caller-a").WillReturnRows(sqlmock.NewRows(pluginTestColumns()))
	_, err := New(db).Get(context.Background(), Scope{CallerUID: "caller-a", SpaceID: "space-a"}, "foreign")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCommitsCurrentRelationsAndAuditTogether(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ids := []string{"relation-id", "audit-id"}
	r.id = func() string { x := ids[0]; ids = ids[1:]; return x }
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO plugins`).WithArgs("plugin-id", "Name", model.PluginTypeSkill, nil, "[]", "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "{}", "{}", "sha256:m", "sha256:p", nil, 1, now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_relations`).WithArgs("relation-id", "plugin-id", "target-id", "expert_skill", 0, "{}", 1, "caller", now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("audit-id", "plugin-id", "create", "caller", "Caller", "request-id", nil, "sha256:p", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	err := r.Create(context.Background(), Scope{CallerUID: "caller", SpaceID: "space"}, Mutation{Plugin: model.Plugin{ID: "plugin-id", Name: "Name", Type: model.PluginTypeSkill, Tags: []byte(`[]`), Publisher: "pub", Visibility: model.PluginVisibilityPrivate, CreatorName: "Creator", CreatedByType: "human", Manifest: []byte(`{}`), Package: []byte(`{}`), ManifestHash: "sha256:m", PluginHash: "sha256:p", Status: 1}, Relations: []model.PluginRelation{{TargetPluginID: "target-id", Type: "expert_skill", Data: []byte(`{}`), Status: 1}}, OperatorID: "caller", OperatorName: "Caller", RequestID: "request-id"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateRollsBackWhenAuditAppendFails(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	r.id = func() string { return "audit-id" }
	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO plugins`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WillReturnError(errors.New("audit unavailable"))
	mock.ExpectRollback()
	err := r.Create(context.Background(), Scope{CallerUID: "caller", SpaceID: "space"}, Mutation{Plugin: model.Plugin{ID: "plugin-id", Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`)}, OperatorID: "caller"})
	if err == nil {
		t.Fatal("expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListPlacementCategoriesCarriesVisibilityScope(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := regexp.QuoteMeta("WHERE cp.placement_code=? AND cp.plugin_type=? AND cp.visible=1 AND c.status=1 AND c.deleted_at IS NULL AND p.deleted_at IS NULL AND") + `.*p.space_id = \? .*p.owner_uid = \?`
	mock.ExpectQuery(q).WithArgs("home", model.PluginTypeExpert, "space-a", "caller-a").WillReturnRows(sqlmock.NewRows([]string{"category_id", "name", "icon_key", "plugin_types_json", "sort_order", "status", "created_at", "updated_at"}))
	_, err := New(db).ListPlacementCategories(context.Background(), Scope{CallerUID: "caller-a", SpaceID: "space-a"}, "home", model.PluginTypeExpert)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func pluginTestColumns() []string {
	return []string{"plugin_id", "plugin_name", "plugin_type", "category_id", "tags_json", "publisher", "owner_uid", "space_id", "visibility", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "manifest_json", "plugin_json", "manifest_hash", "plugin_hash", "current_version_id", "status", "created_at", "updated_at", "deleted_at"}
}
