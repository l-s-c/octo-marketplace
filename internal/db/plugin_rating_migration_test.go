package db

import (
	"strings"
	"testing"

	migrate "github.com/rubenv/sql-migrate"

	migrationsql "github.com/Mininglamp-OSS/octo-marketplace/migrations/sql"
)

func TestPluginRatingMigrationContract(t *testing.T) {
	raw, err := migrationsql.FS.ReadFile("20260904-00-plugin-rating.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, want := range []string{
		"-- +migrate Up", "information_schema.COLUMNS", "PREPARE add_plugin_rating", "'DO 0'",
		"ADD COLUMN `rating` TINYINT UNSIGNED NULL", "CHECK (`rating` IS NULL OR `rating` BETWEEN 1 AND 5)",
		"-- +migrate Down", "DROP CHECK `chk_plugins_rating_range`", "DROP COLUMN `rating`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration missing %q", want)
		}
	}
	upper := strings.ToUpper(sql)
	for _, forbidden := range []string{" AFTER `", " FIRST"} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("migration must not position the rating column with %q", strings.TrimSpace(forbidden))
		}
	}
}

func TestPluginRatingMigrationMySQL(t *testing.T) {
	database := isolatedTestDB(t)
	source := &migrate.EmbedFileSystemMigrationSource{FileSystem: migrationsql.FS, Root: "."}
	if _, err := migrate.Exec(database, "mysql", source, migrate.Up); err != nil {
		t.Fatalf("migrate Up: %v", err)
	}

	for _, tc := range []struct {
		name   string
		rating any
		ok     bool
	}{
		{name: "null", rating: nil, ok: true},
		{name: "minimum", rating: 1, ok: true},
		{name: "maximum", rating: 5, ok: true},
		{name: "zero", rating: 0},
		{name: "six", rating: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := "rating-" + tc.name
			_, err := database.Exec(`INSERT INTO plugins
				(plugin_id, plugin_name, plugin_type, tags_json, owner_uid, visibility,
				 manifest_json, plugin_json, manifest_hash, plugin_hash, rating, created_at, updated_at)
				VALUES (?, ?, 'skill', JSON_ARRAY(), 'user-1', 'private',
				        JSON_OBJECT(), JSON_OBJECT(), REPEAT('a', 71), REPEAT('b', 71), ?, NOW(3), NOW(3))`,
				id, id, tc.rating)
			if tc.ok && err != nil {
				t.Fatalf("insert rating %v: %v", tc.rating, err)
			}
			if !tc.ok && err == nil {
				t.Fatalf("rating %v unexpectedly satisfied CHECK", tc.rating)
			}
		})
	}

	// Simulate DDL committed but sql-migrate bookkeeping missing: deleting only
	// this row makes the guarded Up replay while the column already exists.
	if _, err := database.Exec("DELETE FROM gorp_migrations WHERE id = ?", "20260904-00-plugin-rating.sql"); err != nil {
		t.Fatalf("delete migration bookkeeping: %v", err)
	}
	if _, err := migrate.Exec(database, "mysql", source, migrate.Up); err != nil {
		t.Fatalf("replay guarded migration: %v", err)
	}
}
