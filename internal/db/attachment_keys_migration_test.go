package db

import (
	"strings"
	"testing"

	migrate "github.com/rubenv/sql-migrate"

	migrationsql "github.com/Mininglamp-OSS/octo-marketplace/migrations/sql"
)

const attachmentKeysMigrationID = "20260827-00-plugin-attachment-keys.sql"

func attachmentKeysMigration(t *testing.T) *migrate.Migration {
	t.Helper()
	source := &migrate.EmbedFileSystemMigrationSource{FileSystem: migrationsql.FS, Root: "."}
	migrations, err := source.FindMigrations()
	if err != nil {
		t.Fatalf("FindMigrations() error=%v", err)
	}
	for _, migration := range migrations {
		if migration.Id == attachmentKeysMigrationID {
			return migration
		}
	}
	t.Fatalf("migration %s not found", attachmentKeysMigrationID)
	return nil
}

// TestAttachmentKeysMigrationAddsAndDropsColumn pins the sidecar-column migration
// shape: Up adds attachment_keys_json to both plugins and plugin_versions, Down
// drops it from both, so the storage-key sidecar can be relied on and rolled back.
func TestAttachmentKeysMigrationAddsAndDropsColumn(t *testing.T) {
	migration := attachmentKeysMigration(t)
	up := strings.Join(migration.Up, "\n")
	down := strings.Join(migration.Down, "\n")

	for _, table := range []string{"plugins", "plugin_versions"} {
		if !strings.Contains(up, "ALTER TABLE `"+table+"`") ||
			!strings.Contains(up, "ADD COLUMN `attachment_keys_json`") {
			t.Errorf("Up must add attachment_keys_json to %s: %s", table, up)
		}
		if !strings.Contains(down, "ALTER TABLE `"+table+"`") ||
			!strings.Contains(down, "DROP COLUMN `attachment_keys_json`") {
			t.Errorf("Down must drop attachment_keys_json from %s: %s", table, down)
		}
	}
	// Down reverses Up: drop plugin_versions before plugins (the reverse add
	// order), and touch no other table.
	if strings.Count(up, "ADD COLUMN `attachment_keys_json`") != 2 {
		t.Errorf("Up should add the column exactly twice: %s", up)
	}
	if strings.Count(down, "DROP COLUMN `attachment_keys_json`") != 2 {
		t.Errorf("Down should drop the column exactly twice: %s", down)
	}
}
