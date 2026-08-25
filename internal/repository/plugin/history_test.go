package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestListVersionsReturnsExactScopedTotalForEmptyPage(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT .* FROM plugins p.*p.plugin_id=\?.*p.status=1.*p.space_id = \?.*p.owner_uid = \?`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID).
		WillReturnRows(visiblePluginRow("plugin-id", scope, now))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugin_versions v JOIN plugins p ON p.plugin_id=v.plugin_id WHERE v.plugin_id=\? AND p.status=1.*p.space_id = \?.*p.owner_uid = \?`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(42))
	mock.ExpectQuery(`SELECT v.version_id.*WHERE v.plugin_id=\? AND p.status=1.*p.space_id = \?.*p.owner_uid = \?.*LIMIT \? OFFSET \?`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID, 20, 40).
		WillReturnRows(sqlmock.NewRows([]string{"version_id", "plugin_id", "version", "manifest_json", "plugin_json", "manifest_hash", "plugin_hash", "relations_json", "changelog", "created_by", "created_at"}))

	items, total, err := r.ListVersions(context.Background(), scope, "plugin-id", 20, 40)
	if err != nil || len(items) != 0 || total != 42 {
		t.Fatalf("items=%#v total=%d err=%v", items, total, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListVersionsRedactsCrossSpaceRelationTargets(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT .* FROM plugins p.*p.plugin_id=\?.*p.status=1.*p.space_id = \?.*p.owner_uid = \?`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID).
		WillReturnRows(visiblePluginRow("plugin-id", scope, now))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugin_versions v JOIN plugins p ON p.plugin_id=v.plugin_id WHERE v.plugin_id=\? AND p.status=1.*p.space_id = \?.*p.owner_uid = \?`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(1))
	// The immutable snapshot embeds two relation targets; one is a private target
	// the reading caller cannot see.
	relJSON := `[{"target_plugin_id":"vis-target","relation_type":"expert_skill"},{"target_plugin_id":"priv-target","relation_type":"expert_skill","data":{"secret":"x"}}]`
	mock.ExpectQuery(`SELECT v.version_id.*WHERE v.plugin_id=\? AND p.status=1.*p.space_id = \?.*p.owner_uid = \?.*LIMIT \? OFFSET \?`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID, 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{"version_id", "plugin_id", "version", "manifest_json", "plugin_json", "manifest_hash", "plugin_hash", "relations_json", "changelog", "created_by", "created_at"}).
			AddRow("v1", "plugin-id", "1.0.0", "{}", "{}", "mh", "ph", relJSON, nil, "creator", now))
	// The visibility re-check returns only the visible target (args order is
	// map-iteration dependent, so match the query shape, not the args). The
	// aliased `plugins p` table is load-bearing: visibilitySQL references p.*,
	// so an unaliased FROM would be a runtime ERROR 1054 against a real DB.
	mock.ExpectQuery(`SELECT p.plugin_id FROM plugins p WHERE p.plugin_id IN \(.*\?.*\) AND p.status=1 AND p.deleted_at IS NULL AND `).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id"}).AddRow("vis-target"))

	items, _, err := r.ListVersions(context.Background(), scope, "plugin-id", 20, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	var kept []map[string]any
	if err := json.Unmarshal(items[0].Relations, &kept); err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0]["target_plugin_id"] != "vis-target" {
		t.Fatalf("relations not redacted to visible target: %s", items[0].Relations)
	}
	if bytesContains(items[0].Relations, "priv-target") {
		t.Fatalf("private target leaked in version snapshot: %s", items[0].Relations)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func bytesContains(raw json.RawMessage, sub string) bool {
	return strings.Contains(string(raw), sub)
}

func TestListVersionsCrossSpaceReturnsNotFoundBeforeCount(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectQuery(`SELECT .* FROM plugins p.*p.plugin_id=\?.*p.status=1.*p.space_id = \?.*p.owner_uid = \?`).
		WithArgs("foreign", scope.SpaceID, scope.CallerUID).
		WillReturnRows(sqlmock.NewRows(pluginTestColumns()))

	_, _, err := New(db).ListVersions(context.Background(), scope, "foreign", 20, 0)
	if err != ErrNotFound {
		t.Fatalf("err=%v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

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
		WillReturnRows(publishRelationRows("plugin-id", "target-id", model.PluginTypeSkill))
	mock.ExpectExec(`INSERT INTO plugin_versions`).
		WithArgs("version-id", "plugin-id", "1.2.3", `{"manifest":true}`, `{"package":true}`, "sha256:m", "sha256:p", sqlmock.AnyArg(), nil, "caller", now).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`DELETE pp FROM plugin_placements`).WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`UPDATE plugins SET current_version_id`).WithArgs("version-id", "1.2.3", now, "plugin-id", scope.CallerUID, scope.SpaceID).WillReturnResult(sqlmock.NewResult(0, 1))
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
	if err := json.Unmarshal(version.Relations, &relations); err != nil || len(relations) != 1 || relations[0].TargetPluginID != "target-id" || relations[0].SourcePluginType != model.PluginTypeExpert || relations[0].TargetPluginType != model.PluginTypeSkill {
		t.Fatalf("relations=%s err=%v", version.Relations, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func publishRelationRows(source, target string, targetType model.PluginType) *sqlmock.Rows {
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	return sqlmock.NewRows([]string{"relation_id", "source_plugin_id", "target_plugin_id", "plugin_type", "relation_type", "sort_order", "relation_json", "status", "created_by", "created_at", "updated_at", "deleted_at"}).
		AddRow(source+"-relation", source, target, targetType, "plugin_dependency", 0, []byte(`{"role":"x"}`), 1, "source-owner", now, now, nil)
}

func TestPublishLocksAndValidatesPlacementCategory(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	now := time.Date(2026, 8, 19, 10, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	r.id = func() string { return "version-id" }
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	category := "cat-1"

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.owner_uid=\? AND p.space_id=\?.*FOR UPDATE`).
		WithArgs("plugin-id", scope.CallerUID, scope.SpaceID).
		WillReturnRows(ownedPluginRow("plugin-id", scope, now))
	mock.ExpectQuery(`SELECT r.relation_id.*FOR UPDATE`).
		WithArgs("plugin-id", scope.SpaceID, scope.CallerUID).
		WillReturnRows(publishRelationRows("plugin-id", "target-id", model.PluginTypeSkill))
	mock.ExpectQuery(`SELECT c.category_id FROM plugin_categories c.*JOIN plugin_category_placements cp ON cp.category_id=c.category_id.*JSON_CONTAINS.*cp.placement_code=\?.*cp.plugin_type=\?.*cp.visible=1.*FOR UPDATE`).
		WithArgs(category, model.PluginTypeExpert, "home", model.PluginTypeExpert).
		WillReturnRows(sqlmock.NewRows([]string{"category_id"}))
	mock.ExpectRollback()

	_, err := r.Publish(context.Background(), scope, PublishParams{PluginID: "plugin-id", Version: "1.0.0", CreatedBy: "caller", Placements: []model.PluginPlacement{{PlacementCode: "home", CategoryID: &category}}})
	if err != ErrInvalidPlacement {
		t.Fatalf("Publish error = %v, want ErrInvalidPlacement", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func ownedPluginRow(id string, scope Scope, now time.Time) *sqlmock.Rows {
	return sqlmock.NewRows(pluginTestColumns()).AddRow(id, "Plugin", model.PluginTypeExpert, 0, nil, []byte(`[]`), "pub", scope.CallerUID, scope.SpaceID, model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "", 0, []byte(`{"manifest":true}`), []byte(`{"package":true}`), "sha256:m", "sha256:p", nil, nil, 1, now, now, nil)
}

// visiblePluginRow mirrors ownedPluginRow for the Get visibility path, which
// additionally selects the correlated metric counters.
func visiblePluginRow(id string, scope Scope, now time.Time) *sqlmock.Rows {
	columns := append(pluginTestColumns(), "view_count", "install_count", "download_count")
	return sqlmock.NewRows(columns).AddRow(id, "Plugin", model.PluginTypeExpert, 0, nil, []byte(`[]`), "pub", scope.CallerUID, scope.SpaceID, model.PluginVisibilityPrivate, "Creator", "human", nil, nil, "", 0, []byte(`{"manifest":true}`), []byte(`{"package":true}`), "sha256:m", "sha256:p", nil, nil, 1, now, now, nil, 0, 0, 0)
}
