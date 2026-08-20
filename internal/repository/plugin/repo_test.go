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
	query := `SELECT .* FROM plugins p\s+WHERE p.plugin_id=\? AND p.status=1 AND p.deleted_at IS NULL AND \(p.visibility IN \('public','system'\) OR \(p.space_id = \? AND \(p.visibility = 'space' OR p.owner_uid = \?\)\)\)`
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

func TestListAuditsRequiresOwnerInCurrentSpace(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller-a", SpaceID: "space-a"}

	mock.ExpectQuery(`SELECT 1 FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.deleted_at IS NULL`).
		WithArgs("public-plugin", scope.CallerUID, scope.SpaceID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}))
	_, _, err := New(db).ListAudits(context.Background(), scope, "public-plugin", 20, 0)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("non-owner public audit error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListAuditsScopesHistoryQueryToOwnerAndSpace(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller-a", SpaceID: "space-a"}

	mock.ExpectQuery(`SELECT 1 FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.deleted_at IS NULL`).
		WithArgs("plugin-a", scope.CallerUID, scope.SpaceID).
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugin_audit_logs a JOIN plugins p ON p.plugin_id=a.plugin_id WHERE a.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.deleted_at IS NULL`).
		WithArgs("plugin-a", scope.CallerUID, scope.SpaceID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(37))
	mock.ExpectQuery(`SELECT a.audit_log_id.*FROM plugin_audit_logs a JOIN plugins p ON p.plugin_id=a.plugin_id WHERE a.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.deleted_at IS NULL`).
		WithArgs("plugin-a", scope.CallerUID, scope.SpaceID, 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"audit_log_id", "plugin_id", "action", "operator_id", "operator_name", "request_id", "before_hash", "after_hash", "manifest_snapshot_json", "plugin_snapshot_json", "remark", "created_at"}))
	items, total, err := New(db).ListAudits(context.Background(), scope, "plugin-a", 20, 0)
	if err != nil || len(items) != 0 || total != 37 {
		t.Fatalf("ListAudits = %#v, total=%d, err=%v", items, total, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLockPluginCategoryRejectsMissingOrWrongType(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	category := "category-id"
	mock.ExpectQuery(`SELECT category_id FROM plugin_categories WHERE category_id=\? AND status=1 AND deleted_at IS NULL AND JSON_CONTAINS.*FOR UPDATE`).
		WithArgs(category, model.PluginTypeSkill).
		WillReturnRows(sqlmock.NewRows([]string{"category_id"}))
	if err = lockPluginCategory(context.Background(), tx, &category, model.PluginTypeSkill); !errors.Is(err, ErrInvalidCategory) {
		t.Fatalf("lockPluginCategory error = %v, want ErrInvalidCategory", err)
	}
	_ = tx.Rollback()
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
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p .* FOR UPDATE`).WithArgs("target-id", "space", "caller").WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}).AddRow(model.PluginTypeSkill))
	mock.ExpectExec(`INSERT INTO plugins`).WithArgs("plugin-id", "Name", model.PluginTypeExpert, nil, "[]", "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "{}", "{}", "sha256:m", "sha256:p", nil, 1, now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_relations`).WithArgs("relation-id", "plugin-id", "target-id", "expert_skill", 0, "{}", 1, "caller", now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("audit-id", "plugin-id", "create", "caller", "Caller", "request-id", nil, "sha256:p", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	err := r.Create(context.Background(), Scope{CallerUID: "caller", SpaceID: "space"}, Mutation{Plugin: model.Plugin{ID: "plugin-id", Name: "Name", Type: model.PluginTypeExpert, Tags: []byte(`[]`), Publisher: "pub", Visibility: model.PluginVisibilityPrivate, CreatorName: "Creator", CreatedByType: "human", Manifest: []byte(`{}`), Package: []byte(`{}`), ManifestHash: "sha256:m", PluginHash: "sha256:p", Status: 1}, Relations: []model.PluginRelation{{TargetPluginID: "target-id", Type: "expert_skill", Data: []byte(`{}`), Status: 1}}, OperatorID: "caller", OperatorName: "Caller", RequestID: "request-id"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestLockRelationTargetsRequiresActiveTarget(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p WHERE p.plugin_id=\? AND p.status=1 AND p.deleted_at IS NULL.*FOR UPDATE`).
		WithArgs("inactive", scope.SpaceID, scope.CallerUID).WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}))
	err = lockRelationTargets(context.Background(), tx, scope, model.PluginTypeExpert, []model.PluginRelation{{TargetPluginID: "inactive", Type: "expert_skill"}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("lockRelationTargets error = %v, want ErrNotFound", err)
	}
	_ = tx.Rollback()
}

func TestLockRelationTargetsValidatesExpectedWireType(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p WHERE p.plugin_id=\? AND p.status=1 AND p.deleted_at IS NULL.*FOR UPDATE`).
		WithArgs("opaque-target", scope.SpaceID, scope.CallerUID).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}).AddRow(model.PluginTypeSkill))
	err = lockRelationTargets(context.Background(), tx, scope, model.PluginTypeExpert, []model.PluginRelation{{TargetPluginID: "opaque-target", ExpectedTargetType: model.PluginTypeConnector, Type: "expert_skill"}})
	if !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("lockRelationTargets error = %v, want ErrInvalidRelation", err)
	}
	_ = tx.Rollback()
}

func TestCreateRejectsInvisibleRelationTargetBeforeWriting(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p .*p.space_id = \?.*p.owner_uid = \?.*FOR UPDATE`).WithArgs("foreign", scope.SpaceID, scope.CallerUID).WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}))
	mock.ExpectRollback()

	err := New(db).Create(context.Background(), scope, Mutation{Plugin: model.Plugin{ID: "plugin-id", Type: model.PluginTypeExpert}, Relations: []model.PluginRelation{{TargetPluginID: "foreign", Type: "expert_skill"}}})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v, want ErrNotFound", err)
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

func TestGetOwnedForUpdateRejectsInactivePlugin(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.status=1 AND p.deleted_at IS NULL FOR UPDATE`).
		WithArgs("inactive", scope.CallerUID, scope.SpaceID).
		WillReturnRows(sqlmock.NewRows(pluginTestColumns()))
	if _, err = getOwnedForUpdate(context.Background(), tx, scope, "inactive"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("getOwnedForUpdate error = %v, want ErrNotFound", err)
	}
	_ = tx.Rollback()
}

func TestDeleteRejectsLiveIncomingRelationWithoutMutatingGraph(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\?.*FOR UPDATE`).
		WithArgs("target", scope.CallerUID, scope.SpaceID).
		WillReturnRows(ownedPluginRow("target", scope, now))
	mock.ExpectQuery(`SELECT r.relation_id FROM plugin_relations r.*JOIN plugins source ON source.plugin_id=r.source_plugin_id.*r.target_plugin_id=\?.*r.deleted_at IS NULL.*source.deleted_at IS NULL AND source.status=1.*FOR UPDATE`).
		WithArgs("target").
		WillReturnRows(sqlmock.NewRows([]string{"relation_id"}).AddRow("incoming"))
	mock.ExpectRollback()

	err := r.Delete(context.Background(), scope, "target", "caller", "Caller", "request", nil)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("Delete error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteInvalidatesOnlyOutgoingRelationsAfterReferenceCheck(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.id = func() string { return "audit-id" }

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\?.*FOR UPDATE`).
		WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnRows(ownedPluginRow("plugin-id", scope, now))
	mock.ExpectQuery(`SELECT r.relation_id FROM plugin_relations r.*r.target_plugin_id=\?.*FOR UPDATE`).
		WithArgs("plugin-id").
		WillReturnRows(sqlmock.NewRows([]string{"relation_id"}))
	mock.ExpectExec(`UPDATE plugins SET deleted_at=\?,updated_at=\?.*`).
		WithArgs(now, now, "plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE plugin_relations SET deleted_at=\?,updated_at=\?.*WHERE source_plugin_id=\? AND deleted_at IS NULL`).
		WithArgs(now, now, "plugin-id").
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	if err := r.Delete(context.Background(), scope, "plugin-id", "caller", "Caller", "request", nil); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListPlacementCategoriesCarriesVisibilityScope(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	q := regexp.QuoteMeta("WHERE cp.placement_code=? AND cp.plugin_type=? AND p.plugin_type=? AND cp.visible=1 AND c.status=1 AND c.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND") + `.*p.space_id = \? .*p.owner_uid = \?`
	mock.ExpectQuery(q).WithArgs("home", model.PluginTypeExpert, model.PluginTypeExpert, "space-a", "caller-a").WillReturnRows(sqlmock.NewRows([]string{"category_id", "name", "icon_key", "plugin_types_json", "sort_order", "status", "created_at", "updated_at"}))
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
