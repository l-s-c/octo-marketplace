package plugin

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	libplugin "github.com/Mininglamp-OSS/octo-marketplace/internal/plugincontract"
	_ "github.com/go-sql-driver/mysql"
)

func TestSweepLiveRowsAgainstLibContract(t *testing.T) {
	dsn := os.Getenv("MYSQL_DSN")
	if dsn == "" {
		t.Skip("MYSQL_DSN not set")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT plugin_id, plugin_name, plugin_type, manifest_json, plugin_json, plugin_hash, created_at, updated_at FROM plugins WHERE deleted_at IS NULL AND status=1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	total, failed := 0, 0
	for rows.Next() {
		var item libplugin.Plugin
		var manifest, pkg []byte
		var created, updated time.Time
		var typ string
		if err := rows.Scan(&item.PluginID, &item.PluginName, &typ, &manifest, &pkg, &item.PluginHash, &created, &updated); err != nil {
			t.Fatal(err)
		}
		item.PluginType = libplugin.Type(typ)
		item.CreatedAt, item.UpdatedAt = created, updated
		if updated.Before(created) {
			item.UpdatedAt = created
		}
		cm, _ := libplugin.CanonicalJSON(manifest)
		cp, _ := libplugin.CanonicalJSON(pkg)
		item.ManifestJSON, item.PluginJSON = json.RawMessage(cm), json.RawMessage(cp)
		total++
		if err := libplugin.ValidatePlugin(item); err != nil {
			failed++
			t.Errorf("plugin %s (%s): %v", item.PluginID, typ, err)
			if failed > 8 {
				t.Fatal("too many failures")
			}
		}
	}
	t.Logf("validated %d live rows against octo-plugin-lib", total)
}
