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
	ids := []string{"child-copy", "leaf-copy", "root-rel-copy", "child-rel-copy", "root-audit-copy", "child-audit-copy", "leaf-audit-copy"}
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
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("root-audit-copy", "root-copy", "duplicate", "caller", "Caller", "request", nil, "sha256:root", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("child-audit-copy", "child-copy", "duplicate", "caller", "Caller", "request", nil, "sha256:child", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("leaf-audit-copy", "leaf-copy", "duplicate", "caller", "Caller", "request", nil, "sha256:leaf", "{}", "{}", nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
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

func TestDuplicateGraphReattributesDescendantProvenanceAndActivatesCopies(t *testing.T) {
	// The deep-copy expectations above require every descendant INSERT to use the
	// root duplicate's creator provenance and status=1, rather than source values.
	root := model.Plugin{CreatorName: "Caller", CreatedByType: "bot", CreatedByBotUID: ptr("bot-1"), Status: 1}
	child := model.Plugin{CreatorName: "Source", CreatedByType: "human", Status: 0}
	child.CreatorName, child.CreatedByType, child.CreatedByBotUID, child.Status = root.CreatorName, root.CreatedByType, root.CreatedByBotUID, 1
	if child.CreatorName != "Caller" || child.CreatedByType != "bot" || child.CreatedByBotUID == nil || child.Status != 1 {
		t.Fatalf("descendant provenance not reset: %#v", child)
	}
}

func ptr(value string) *string { return &value }

func TestDuplicateGraphRejectsUnsafeConnectorDescendantBeforeWriting(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectBegin()
	expectDuplicateNode(mock, "root", scope, pluginRow("root", "Root"), relationRows("root", "connector"))
	expectDuplicateNode(mock, "connector", scope, connectorPluginRow("connector", `{"config":{"env":{"API_TOKEN":"plain-token"}}}`), emptyRelationRows())
	mock.ExpectRollback()

	err := New(db).DuplicateGraph(context.Background(), scope, "root", model.Plugin{ID: "copy"}, Mutation{})
	if !errors.Is(err, ErrUnsafeConnectorData) {
		t.Fatalf("err=%v, want ErrUnsafeConnectorData", err)
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

func connectorPluginRow(id, pkg string) *sqlmock.Rows {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return sqlmock.NewRows(pluginTestColumns()).AddRow(id, "Connector", model.PluginTypeConnector, nil, []byte(`[]`), "pub", "source-owner", "source-space", model.PluginVisibilityPublic, "Creator", "human", nil, nil, []byte(`{}`), []byte(pkg), "sha256:m", "sha256:"+id, nil, 1, now, now, nil)
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
