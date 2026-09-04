package db

import (
	"strings"
	"testing"

	migrationsql "github.com/Mininglamp-OSS/octo-marketplace/migrations/sql"
)

func TestPluginRatingMigrationContract(t *testing.T) {
	raw, err := migrationsql.FS.ReadFile("20260904-00-plugin-rating.sql")
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, want := range []string{
		"-- +migrate Up", "ADD COLUMN `rating` TINYINT UNSIGNED NULL", "CHECK (`rating` IS NULL OR `rating` BETWEEN 1 AND 5)",
		"-- +migrate Down", "DROP CHECK `chk_plugins_rating_range`", "DROP COLUMN `rating`",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("migration missing %q", want)
		}
	}
}
