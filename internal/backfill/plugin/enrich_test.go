package plugin

import (
	"context"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func countRows(n int) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"count"}).AddRow(n)
}

func TestEnrichDryRunPlansEveryGapWithoutWriting(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	connectorID := PluginID("connector", "mcp-1")
	skillID := PluginID("skill", "s1")

	// Connector categories: one live mcp row with category "dev", nothing
	// registered or stamped yet.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, category FROM mcp_servers WHERE deleted_at IS NULL ORDER BY id")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "category"}).AddRow("mcp-1", "dev"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugin_categories WHERE category_id=\?`).
		WithArgs(ConnectorCategoryID("dev")).WillReturnRows(countRows(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugin_category_placements WHERE placement_id=\?`).
		WillReturnRows(countRows(0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE plugin_id=\? AND category_id IS NULL`).
		WithArgs(connectorID).WillReturnRows(countRows(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugin_placements WHERE placement_code='default' AND plugin_id=\? AND category_id IS NULL`).
		WithArgs(connectorID).WillReturnRows(countRows(1))

	// Icons: the connector has an emoji icon not yet copied; no skill icons.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, icon FROM mcp_servers WHERE icon<>'' ORDER BY id")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "icon"}).AddRow("mcp-1", "🐙"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE plugin_id=\? AND icon=''`).
		WithArgs(connectorID).WillReturnRows(countRows(1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, icon_url FROM skills WHERE icon_url<>'' ORDER BY id")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "icon_url"}))

	// Tool counts: stored 0, package carries two tools.
	pkg := `{"attachments":[{"path":"connector/tools.json","content_type":"raw","raw_content":"[{},{}]"}]}`
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plugin_id, plugin_json, tool_count FROM plugins WHERE plugin_type='connector' AND deleted_at IS NULL ORDER BY plugin_id")).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "plugin_json", "tool_count"}).AddRow(connectorID, []byte(pkg), 0))

	// Metrics: one legacy skill counter row not yet copied.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT resource_type, resource_id, view_count, download_count, install_count FROM resource_metrics WHERE resource_type IN ('skill','expert','squad') ORDER BY resource_type, resource_id")).
		WillReturnRows(sqlmock.NewRows([]string{"resource_type", "resource_id", "view_count", "download_count", "install_count"}).AddRow("skill", "s1", 7, 5, 3))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE plugin_id=\?`).
		WithArgs(skillID).WillReturnRows(countRows(1))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM resource_metrics WHERE resource_type='plugin' AND resource_id=\?`).
		WithArgs(skillID).WillReturnRows(countRows(0))

	report, err := New(db).Enrich(context.Background(), Options{Mode: ModeDryRun})
	if err != nil {
		t.Fatal(err)
	}
	want := EnrichCounts{ConnectorCategories: 1, CategoryPlacements: 1, PluginCategories: 1, PlacementCategories: 1, Icons: 1, ToolCounts: 1, Metrics: 1}
	if report.Planned != want {
		t.Fatalf("planned = %#v, want %#v", report.Planned, want)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnrichSkipsMetricsWithoutMigratedPlugin(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, category FROM mcp_servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "category"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, icon FROM mcp_servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "icon"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, icon_url FROM skills")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "icon_url"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plugin_id, plugin_json, tool_count FROM plugins")).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "plugin_json", "tool_count"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT resource_type, resource_id, view_count, download_count, install_count FROM resource_metrics")).
		WillReturnRows(sqlmock.NewRows([]string{"resource_type", "resource_id", "view_count", "download_count", "install_count"}).AddRow("expert", "gone", 1, 0, 0))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE plugin_id=\?`).
		WithArgs(PluginID("expert", "gone")).WillReturnRows(countRows(0))

	report, err := New(db).Enrich(context.Background(), Options{Mode: ModeDryRun})
	if err != nil {
		t.Fatal(err)
	}
	if report.Planned.Metrics != 0 {
		t.Fatalf("planned metrics = %d, want 0", report.Planned.Metrics)
	}
	if len(report.Issues) != 1 || report.Issues[0].Code != "orphan_metrics" {
		t.Fatalf("issues = %#v", report.Issues)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEnrichApplyExecutesPlannedStatementsInOneTransaction(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	connectorID := PluginID("connector", "mcp-1")

	// Build pass: only the icon copy is still missing.
	emptyEnrichExpectationsExceptIcon(mock, connectorID, true)
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE plugins SET icon=\? WHERE plugin_id=\? AND icon=''`).
		WithArgs("🐙", connectorID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	// Remaining pass after apply: gap closed.
	emptyEnrichExpectationsExceptIcon(mock, connectorID, false)

	report, err := New(db).Enrich(context.Background(), Options{Mode: ModeApply})
	if err != nil {
		t.Fatal(err)
	}
	if report.Planned.Icons != 1 || report.Applied.Icons != 1 {
		t.Fatalf("report = %#v", report)
	}
	if report.Remaining != (EnrichCounts{}) {
		t.Fatalf("remaining = %#v", report.Remaining)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func emptyEnrichExpectationsExceptIcon(mock sqlmock.Sqlmock, connectorID string, missing bool) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, category FROM mcp_servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "category"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, icon FROM mcp_servers")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "icon"}).AddRow("mcp-1", "🐙"))
	stillMissing := 0
	if missing {
		stillMissing = 1
	}
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE plugin_id=\? AND icon=''`).
		WithArgs(connectorID).WillReturnRows(countRows(stillMissing))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, icon_url FROM skills")).
		WillReturnRows(sqlmock.NewRows([]string{"id", "icon_url"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT plugin_id, plugin_json, tool_count FROM plugins")).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "plugin_json", "tool_count"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT resource_type, resource_id, view_count, download_count, install_count FROM resource_metrics")).
		WillReturnRows(sqlmock.NewRows([]string{"resource_type", "resource_id", "view_count", "download_count", "install_count"}))
}
