package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestPublishSnapshotsLockedStateAndRelationsAndReturnsStoredVersion(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	ids := []string{"version-id", "audit-id"}
	r.id = func() string { id := ids[0]; ids = ids[1:]; return id }
	scope := Scope{CallerUID: "caller", SpaceID: "space"}

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\?.*FOR UPDATE`).
		WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnRows(ownedPluginRow("plugin-id", scope, now))
	mock.ExpectQuery(`SELECT r.relation_id.*JOIN plugins p ON p.plugin_id=r.target_plugin_id.*p.space_id = \?.*p.owner_uid = \?.*FOR UPDATE`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID).
		WillReturnRows(relationRows("plugin-id", "target-id"))
	mock.ExpectExec(`INSERT INTO plugin_versions`).
		WithArgs("version-id", "plugin-id", "1.2.3", `{"manifest":true}`, `{"package":true}`, "sha256:m", "sha256:p", sqlmock.AnyArg(), nil, "caller", now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE pp FROM plugin_placements`).WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE plugins SET current_version_id`).WithArgs("version-id", now, "plugin-id", scope.CallerUID, scope.SpaceID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO plugin_audit_logs`).WithArgs("audit-id", "plugin-id", "publish", "caller", "Caller", "request", "sha256:p", "sha256:p", `{"manifest":true}`, `{"package":true}`, nil, now).WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	version, err := r.Publish(context.Background(), scope, PublishParams{PluginID: "plugin-id", Version: "1.2.3", CreatedBy: "caller", OperatorName: "Caller", RequestID: "request"})
	if err != nil {
		t.Fatal(err)
	}
	if version.ID != "version-id" || version.PluginID != "plugin-id" || version.Version != "1.2.3" || version.CreatedAt != now || string(version.Manifest) != `{"manifest":true}` {
		t.Fatalf("returned version = %#v", version)
	}
	var relations []model.PluginRelation
	if err := json.Unmarshal(version.Relations, &relations); err != nil || len(relations) != 1 || relations[0].TargetPluginID != "target-id" {
		t.Fatalf("relations=%s err=%v", version.Relations, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func ownedPluginRow(id string, scope Scope, now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows(pluginTestColumns()).AddRow(id, "Plugin", model.PluginTypeExpert, nil, []byte(`[]`), "pub", scope.CallerUID, scope.SpaceID, model.PluginVisibilityPrivate, "Creator", "human", nil, nil, []byte(`{"manifest":true}`), []byte(`{"package":true}`), "sha256:m", "sha256:p", nil, 1, now, now, nil)
}
