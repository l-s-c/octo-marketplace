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
	mock.ExpectExec(`INSERT INTO plugins`).WithArgs("plugin-id", "Name", model.PluginTypeExpert, nil, "[]", "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "", 0, "{}", "{}", "sha256:m", "sha256:p", nil, nil, 1, now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_relations`).WithArgs("relation-id", "plugin-id", "target-id", "expert_skill", 0, "{}", 1, "caller", now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("audit-id", "plugin-id", "create", "caller", "Caller", "request-id", nil, "sha256:p", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()
	_, err := r.Create(context.Background(), Scope{CallerUID: "caller", SpaceID: "space"}, Mutation{Plugin: model.Plugin{ID: "plugin-id", Name: "Name", Type: model.PluginTypeExpert, Tags: []byte(`[]`), Publisher: "pub", Visibility: model.PluginVisibilityPrivate, CreatorName: "Creator", CreatedByType: "human", Manifest: []byte(`{}`), Package: []byte(`{}`), ManifestHash: "sha256:m", PluginHash: "sha256:p", Status: 1}, Relations: []model.PluginRelation{{TargetPluginID: "target-id", Type: "expert_skill", Data: []byte(`{}`), Status: 1}}, OperatorID: "caller", OperatorName: "Caller", RequestID: "request-id"})
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

func TestLockRelationTargetsValidatesRowType(t *testing.T) {
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
		WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}).AddRow(model.PluginTypeConnector))
	err = lockRelationTargets(context.Background(), tx, scope, model.PluginTypeExpert, []model.PluginRelation{{TargetPluginID: "opaque-target", Type: "expert_skill"}})
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

	_, err := New(db).Create(context.Background(), scope, Mutation{Plugin: model.Plugin{ID: "plugin-id", Type: model.PluginTypeExpert}, Relations: []model.PluginRelation{{TargetPluginID: "foreign", Type: "expert_skill"}}})
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
	_, err := r.Create(context.Background(), Scope{CallerUID: "caller", SpaceID: "space"}, Mutation{Plugin: model.Plugin{ID: "plugin-id", Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`)}, OperatorID: "caller"})
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
	q := regexp.QuoteMeta("AND p.plugin_type=? AND p.status=1 AND p.deleted_at IS NULL AND") + `.*p.space_id = \? .*p.owner_uid = \?.*` +
		regexp.QuoteMeta("WHERE cp.placement_code=? AND cp.plugin_type=? AND cp.visible=1 AND c.status=1 AND c.deleted_at IS NULL")
	mock.ExpectQuery(q).WithArgs(model.PluginTypeExpert, "space-a", "caller-a", "home", model.PluginTypeExpert).WillReturnRows(sqlmock.NewRows([]string{"category_id", "name", "icon_key", "plugin_types_json", "sort_order", "status", "created_at", "updated_at", "plugin_count"}))
	_, err := New(db).ListPlacementCategories(context.Background(), Scope{CallerUID: "caller-a", SpaceID: "space-a"}, "home", model.PluginTypeExpert)
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func pluginTestColumns() []string {
	return []string{"plugin_id", "plugin_name", "plugin_type", "is_embedded", "category_id", "tags_json", "publisher", "owner_uid", "space_id", "visibility", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "icon", "tool_count", "manifest_json", "plugin_json", "manifest_hash", "plugin_hash", "current_version_id", "current_version", "status", "created_at", "updated_at", "deleted_at"}
}

func TestUpdateSynchronizesRelationsToTargetState(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ids := []string{"relation-new", "audit-id"}
	r.id = func() string { x := ids[0]; ids = ids[1:]; return x }
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.status=1 AND p.deleted_at IS NULL FOR UPDATE`).
		WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnRows(sqlmock.NewRows(pluginTestColumns()).AddRow("plugin-id", "Plugin", model.PluginTypeExpert, 0, nil, []byte(`[]`), "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), "sha256:m", "sha256:before", nil, nil, 1, now, now, nil))
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p .* FOR UPDATE`).WithArgs("target-1", "space", "caller").WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}).AddRow(model.PluginTypeSkill))
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p .* FOR UPDATE`).WithArgs("target-2", "space", "caller").WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}).AddRow(model.PluginTypeSkill))
	mock.ExpectExec(`UPDATE plugins SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT relation_id,target_plugin_id,relation_type,sort_order,relation_json,status FROM plugin_relations\s+WHERE source_plugin_id=\? AND deleted_at IS NULL ORDER BY relation_id FOR UPDATE`).
		WithArgs("plugin-id").
		WillReturnRows(sqlmock.NewRows([]string{"relation_id", "target_plugin_id", "relation_type", "sort_order", "relation_json", "status"}).
			AddRow("rel-0", "target-0", "expert_skill", 0, nil, 1).
			AddRow("rel-1", "target-1", "expert_skill", 0, nil, 1))
	mock.ExpectExec(`UPDATE plugin_relations SET target_plugin_id=`).WithArgs("target-1", "expert_skill", 2, nil, 1, now, "rel-1", "plugin-id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO plugin_relations`).WithArgs("relation-new", "plugin-id", "target-2", "expert_skill", 0, nil, 1, "caller", now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`UPDATE plugin_relations SET deleted_at=`).WithArgs(now, now, "rel-0", "plugin-id").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	sync, err := r.Update(context.Background(), scope, Mutation{
		Plugin: model.Plugin{ID: "plugin-id", Name: "Plugin", Type: model.PluginTypeExpert, Tags: []byte(`[]`), Publisher: "pub", Visibility: model.PluginVisibilityPrivate, Manifest: []byte(`{}`), Package: []byte(`{}`), ManifestHash: "sha256:m", PluginHash: "sha256:after", Status: 1},
		Relations: []model.PluginRelation{
			{ID: "rel-1", TargetPluginID: "target-1", Type: "expert_skill", SortOrder: 2, Status: 1},
			{TargetPluginID: "target-2", Type: "expert_skill", Status: 1},
		},
		OperatorID: "caller",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sync.Created) != 1 || sync.Created[0] != "relation-new" ||
		len(sync.Updated) != 1 || sync.Updated[0] != "rel-1" ||
		len(sync.Deleted) != 1 || sync.Deleted[0] != "rel-0" {
		t.Fatalf("sync = %#v", sync)
	}
	if len(sync.Relations) != 2 || sync.Relations[1].ID != "relation-new" {
		t.Fatalf("relations = %#v", sync.Relations)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRejectsUnknownSubmittedRelationID(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\? AND p.status=1 AND p.deleted_at IS NULL FOR UPDATE`).
		WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnRows(sqlmock.NewRows(pluginTestColumns()).AddRow("plugin-id", "Plugin", model.PluginTypeExpert, 0, nil, []byte(`[]`), "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), "sha256:m", "sha256:before", nil, nil, 1, now, now, nil))
	mock.ExpectQuery(`SELECT p.plugin_type FROM plugins p .* FOR UPDATE`).WithArgs("target-1", "space", "caller").WillReturnRows(sqlmock.NewRows([]string{"plugin_type"}).AddRow(model.PluginTypeSkill))
	mock.ExpectExec(`UPDATE plugins SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT relation_id,target_plugin_id,relation_type,sort_order,relation_json,status FROM plugin_relations\s+WHERE source_plugin_id=\? AND deleted_at IS NULL ORDER BY relation_id FOR UPDATE`).
		WithArgs("plugin-id").
		WillReturnRows(sqlmock.NewRows([]string{"relation_id", "target_plugin_id", "relation_type", "sort_order", "relation_json", "status"}))
	mock.ExpectRollback()

	_, err := r.Update(context.Background(), scope, Mutation{
		Plugin:    model.Plugin{ID: "plugin-id", Name: "Plugin", Type: model.PluginTypeExpert, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1},
		Relations: []model.PluginRelation{{ID: "rel-forged", TargetPluginID: "target-1", Type: "expert_skill", Status: 1}},
	})
	if !errors.Is(err, ErrInvalidRelation) {
		t.Fatalf("err = %v, want ErrInvalidRelation", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateSyncsPlacementCategoryOnlyWhenSet(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 22, 8, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.id = func() string { return "audit-id" }
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	category := "cat-1"

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\?.*FOR UPDATE`).
		WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnRows(ownedPluginRow("plugin-id", scope, now))
	mock.ExpectQuery(`SELECT category_id FROM plugin_categories WHERE category_id=\?.*FOR UPDATE`).
		WithArgs(category, model.PluginTypeExpert).
		WillReturnRows(sqlmock.NewRows([]string{"category_id"}).AddRow(category))
	mock.ExpectExec(`UPDATE plugins SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE plugin_placements SET category_id=\?,updated_at=\? WHERE plugin_id=\?`).
		WithArgs(category, now, "plugin-id").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT relation_id,target_plugin_id,relation_type,sort_order,relation_json,status FROM plugin_relations`).
		WithArgs("plugin-id").
		WillReturnRows(sqlmock.NewRows([]string{"relation_id", "target_plugin_id", "relation_type", "sort_order", "relation_json", "status"}))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	_, err := r.Update(context.Background(), scope, Mutation{Plugin: model.Plugin{ID: "plugin-id", Name: "Plugin", Type: model.PluginTypeExpert, CategoryID: &category, Tags: []byte(`[]`), Visibility: model.PluginVisibilityPrivate, Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
