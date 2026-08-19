package plugin

import (
	"context"
	"database/sql/driver"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestListReturnsTotalAndAppliesConfirmedFilters(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	args := []driver.Value{"home", "space", "caller", model.PluginTypeSkill, "cat", "100%_done!", "%100!%!_done!!%"}
	mock.ExpectQuery(`SELECT COUNT\(DISTINCT p.plugin_id\).*JOIN plugin_placements.*pp.placement_code=\?.*p.status=1.*JSON_CONTAINS.*p.plugin_name LIKE \? ESCAPE '!'`).WithArgs(args...).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(7))
	mock.ExpectQuery(`SELECT .*JOIN plugin_placements.*p.status=1.*ORDER BY pp.sort_order ASC,p.plugin_id ASC LIMIT \? OFFSET \?`).WithArgs(append(args, 20, 0)...).WillReturnRows(sqlmock.NewRows(pluginTestColumns()))
	items, total, err := New(db).List(context.Background(), scope, ListFilter{PlacementCode: "home", Type: model.PluginTypeSkill, CategoryID: "cat", Tag: "100%_done!", Keyword: "100%_done!", Sort: "placement"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 || total != 7 {
		t.Fatalf("items=%d total=%d", len(items), total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestEscapeLike(t *testing.T) {
	if got := escapeLike(`a!b%c_d`); got != `a!!b!%c!_d` {
		t.Fatalf("escapeLike=%q", got)
	}
}
