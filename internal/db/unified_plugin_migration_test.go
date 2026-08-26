package db

import (
	"strings"
	"testing"

	mysql "github.com/go-sql-driver/mysql"
	migrate "github.com/rubenv/sql-migrate"

	migrationsql "github.com/Mininglamp-OSS/octo-marketplace/migrations/sql"
)

const unifiedPluginMigrationID = "20260819-00-unified-plugin.sql"

var unifiedPluginTables = []string{
	"plugin_categories",
	"plugins",
	"plugin_relations",
	"plugin_versions",
	"plugin_audit_logs",
	"plugin_category_placements",
	"plugin_placements",
}

func unifiedPluginMigration(t *testing.T) *migrate.Migration {
	t.Helper()
	source := &migrate.EmbedFileSystemMigrationSource{FileSystem: migrationsql.FS, Root: "."}
	migrations, err := source.FindMigrations()
	if err != nil {
		t.Fatalf("FindMigrations() error=%v", err)
	}
	for _, migration := range migrations {
		if migration.Id == unifiedPluginMigrationID {
			return migration
		}
	}
	t.Fatalf("migration %s not found", unifiedPluginMigrationID)
	return nil
}

func TestUnifiedPluginMigrationContainsOnlyConfirmedTables(t *testing.T) {
	migration := unifiedPluginMigration(t)
	up := strings.Join(migration.Up, "\n")
	down := strings.Join(migration.Down, "\n")

	if got := strings.Count(up, "CREATE TABLE `"); got != len(unifiedPluginTables) {
		t.Fatalf("CREATE TABLE count=%d want=%d", got, len(unifiedPluginTables))
	}
	for _, table := range unifiedPluginTables {
		if got := strings.Count(up, "CREATE TABLE `"+table+"`"); got != 1 {
			t.Errorf("CREATE TABLE %s count=%d want=1", table, got)
		}
		if got := strings.Count(down, "DROP TABLE IF EXISTS `"+table+"`"); got != 1 {
			t.Errorf("DROP TABLE %s count=%d want=1", table, got)
		}
	}

	createOrder := []string{
		"plugin_categories",
		"plugins",
		"plugin_relations",
		"plugin_versions",
		"plugin_audit_logs",
		"plugin_category_placements",
		"plugin_placements",
	}
	assertTableOrder(t, up, "CREATE TABLE", createOrder)

	dropOrder := []string{
		"plugin_placements",
		"plugin_category_placements",
		"plugin_audit_logs",
		"plugin_versions",
		"plugin_relations",
		"plugins",
		"plugin_categories",
	}
	assertTableOrder(t, down, "DROP TABLE IF EXISTS", dropOrder)
}

func TestUnifiedPluginMigrationHasRequiredGuards(t *testing.T) {
	migration := unifiedPluginMigration(t)
	up := strings.Join(migration.Up, "\n")

	required := []string{
		"`deleted_at`       DATETIME(3)",
		"`deleted_at`          DATETIME(3)",
		"`deleted_at`     DATETIME(3)",
		"CHECK (JSON_TYPE(`tags_json`) = 'ARRAY')",
		"CHECK (JSON_TYPE(`manifest_json`) = 'OBJECT')",
		"CHECK (JSON_TYPE(`relations_json`) = 'ARRAY')",
		"UNIQUE KEY `uq_plugin_versions_plugin_version` (`plugin_id`, `version`)",
		"UNIQUE KEY `uq_plugin_category_placement` (`placement_code`, `plugin_type`, `category_id`)",
		"GENERATED ALWAYS AS (IFNULL(`category_id`, '')) STORED",
		"UNIQUE KEY `uq_plugin_placement` (`placement_code`, `plugin_id`, `category_key`)",
	}
	for _, fragment := range required {
		if !strings.Contains(up, fragment) {
			t.Errorf("migration missing required schema fragment %q", fragment)
		}
	}
}

func TestUnifiedPluginMigrationUpDownMySQL(t *testing.T) {
	database := isolatedTestDB(t)
	migration := unifiedPluginMigration(t)
	source := &migrate.MemoryMigrationSource{Migrations: []*migrate.Migration{migration}}

	if n, err := migrate.Exec(database, "mysql", source, migrate.Up); err != nil {
		t.Fatalf("migrate Up: %v", err)
	} else if n != 1 {
		t.Fatalf("migrate Up applied %d migrations, want 1", n)
	}

	for _, table := range unifiedPluginTables {
		var count int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			table,
		).Scan(&count); err != nil {
			t.Fatalf("query table %s: %v", table, err)
		}
		if count != 1 {
			t.Errorf("table %s not found after Up", table)
		}
	}

	if _, err := database.Exec(`INSERT INTO plugin_categories
		(category_id, name, plugin_types_json, created_at, updated_at)
		VALUES ('bad-json-shape', 'bad', JSON_OBJECT('type', 'skill'), NOW(3), NOW(3))`); err == nil {
		t.Error("plugin_categories accepted non-array plugin_types_json")
	}

	const placementInsert = `INSERT INTO plugin_placements
		(placement_id, placement_code, plugin_id, category_id, created_at, updated_at)
		VALUES (?, 'loop.marketplace.home', 'plugin-1', NULL, NOW(3), NOW(3))`
	if _, err := database.Exec(placementInsert, "placement-1"); err != nil {
		t.Fatalf("insert first placement: %v", err)
	}
	if _, err := database.Exec(placementInsert, "placement-2"); !isMySQLDuplicate(err) {
		t.Fatalf("duplicate NULL-category placement error=%v, want MySQL 1062", err)
	}

	if n, err := migrate.Exec(database, "mysql", source, migrate.Down); err != nil {
		t.Fatalf("migrate Down: %v", err)
	} else if n != 1 {
		t.Fatalf("migrate Down applied %d migrations, want 1", n)
	}
	for _, table := range unifiedPluginTables {
		var count int
		if err := database.QueryRow(
			"SELECT COUNT(*) FROM INFORMATION_SCHEMA.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ?",
			table,
		).Scan(&count); err != nil {
			t.Fatalf("query table %s after Down: %v", table, err)
		}
		if count != 0 {
			t.Errorf("table %s still exists after Down", table)
		}
	}
}

func assertTableOrder(t *testing.T, sqlText, operation string, tables []string) {
	t.Helper()
	previous := -1
	for _, table := range tables {
		position := strings.Index(sqlText, operation+" `"+table+"`")
		if position < 0 {
			t.Errorf("%s for %s not found", operation, table)
			continue
		}
		if position <= previous {
			t.Errorf("%s for %s is out of order", operation, table)
		}
		previous = position
	}
}

func isMySQLDuplicate(err error) bool {
	mysqlError, ok := err.(*mysql.MySQLError)
	return ok && mysqlError.Number == 1062
}
