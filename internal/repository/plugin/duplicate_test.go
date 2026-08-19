package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestDuplicateGraphDeepCopiesGraphAndCommits(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 19, 9, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ids := []string{"child-copy", "leaf-copy", "root-rel-copy", "child-rel-copy", "audit-copy"}
	r.id = func() string { x := ids[0]; ids = ids[1:]; return x }
	scope := Scope{CallerUID: "caller", SpaceID: "space"}

	mock.ExpectBegin()
	expectDuplicateNode(mock, "root", scope, pluginRow("root", "Root"), relationRows("root", "child"))
	expectDuplicateNode(mock, "child", scope, pluginRow("child", "Child"), relationRows("child", "leaf"))
	expectDuplicateNode(mock, "leaf", scope, pluginRow("leaf", "Leaf"), emptyRelationRows())
	mock.ExpectExec(`INSERT INTO plugins`).WithArgs("root-copy", "Root copy", model.PluginTypeExpert, nil, "[]", "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "{}", "{}", "sha256:m", "sha256:root", nil, 1, now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugins`).WithArgs("child-copy", "Child", model.PluginTypeExpert, nil, "[]", "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "{}", "{}", "sha256:m", "sha256:child", nil, 1, now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugins`).WithArgs("leaf-copy", "Leaf", model.PluginTypeExpert, nil, "[]", "pub", "caller", "space", model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "{}", "{}", "sha256:m", "sha256:leaf", nil, 1, now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_relations`).WithArgs("root-rel-copy", "root-copy", "child-copy", "plugin_dependency", 0, `{"role":"x"}`, 1, "caller", now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_relations`).WithArgs("child-rel-copy", "child-copy", "leaf-copy", "plugin_dependency", 0, `{"role":"x"}`, 1, "caller", now, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("audit-copy", "root-copy", "duplicate", "caller", "Caller", "request", nil, "sha256:root", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	duplicate := model.Plugin{ID: "root-copy", Name: "Root copy", Type: model.PluginTypeExpert, Tags: []byte(`[]`), Publisher: "pub", CreatorName: "Creator", CreatedByType: "human", Manifest: []byte(`{}`), Package: []byte(`{}`), ManifestHash: "sha256:m", PluginHash: "sha256:root", Status: 1}
	err := r.DuplicateGraph(context.Background(), scope, "root", duplicate, Mutation{OperatorID: "caller", OperatorName: "Caller", RequestID: "request"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Fatalf("unused generated IDs: %v", ids)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateGraphRejectsCycleBeforeWriting(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectBegin()
	expectDuplicateNode(mock, "root", scope, pluginRow("root", "Root"), relationRows("root", "child"))
	expectDuplicateNode(mock, "child", scope, pluginRow("child", "Child"), relationRows("child", "root"))
	mock.ExpectRollback()

	err := New(db).DuplicateGraph(context.Background(), scope, "root", model.Plugin{ID: "copy"}, Mutation{})
	if err == nil {
		t.Fatal("expected cycle error")
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDuplicateGraphRollsBackWhenSourceIsCrossSpace(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller-a", SpaceID: "space-a"}
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p.*p.plugin_id=\?.*p.space_id = \?.*p.owner_uid = \?.*FOR UPDATE`).WithArgs("foreign", scope.SpaceID, scope.CallerUID).WillReturnRows(sqlmock.NewRows(pluginTestColumns()))
	mock.ExpectRollback()
	err := New(db).DuplicateGraph(context.Background(), scope, "foreign", model.Plugin{}, Mutation{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err=%v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func expectDuplicateNode(mock sqlmock.Sqlmock, id string, scope Scope, pluginRows, relations *sqlmock.Rows) {
	mock.ExpectQuery(`SELECT .* FROM plugins p.*p.plugin_id=\?.*p.space_id = \?.*p.owner_uid = \?.*FOR UPDATE`).WithArgs(id, scope.SpaceID, scope.CallerUID).WillReturnRows(pluginRows)
	mock.ExpectQuery(`SELECT r.relation_id.*FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.source_plugin_id.*r.source_plugin_id=\?.*p.space_id = \?.*p.owner_uid = \?`).WithArgs(id, scope.SpaceID, scope.CallerUID).WillReturnRows(relations)
}

func pluginRow(id, name string) *sqlmock.Rows {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return sqlmock.NewRows(pluginTestColumns()).AddRow(id, name, model.PluginTypeExpert, nil, []byte(`[]`), "pub", "source-owner", "source-space", model.PluginVisibilityPublic, "Creator", "human", nil, nil, []byte(`{}`), []byte(`{}`), "sha256:m", "sha256:"+id, nil, 1, now, now, nil)
}

func relationRows(source, target string) *sqlmock.Rows {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return sqlmock.NewRows(duplicateRelationColumns()).AddRow(source+"-relation", source, target, "plugin_dependency", 0, []byte(`{"role":"x"}`), 1, "source-owner", now, now, nil)
}

func emptyRelationRows() *sqlmock.Rows {
	return sqlmock.NewRows(duplicateRelationColumns())
}

func duplicateRelationColumns() []string {
	return []string{"relation_id", "source_plugin_id", "target_plugin_id", "relation_type", "sort_order", "relation_json", "status", "created_by", "created_at", "updated_at", "deleted_at"}
}
