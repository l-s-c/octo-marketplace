package plugin

import (
	"database/sql"
	"encoding/json"
	"os"
	"testing"
	"time"

	libplugin "github.com/Mininglamp-OSS/octo-plugin-lib/plugin"
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
	rows, err := db.Query(`SELECT plugin_id, plugin_name, plugin_type, manifest_json, plugin_json, plugin_hash, status, created_at, updated_at FROM plugins WHERE deleted_at IS NULL AND status=1`)
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
		var status uint8
		if err := rows.Scan(&item.PluginID, &item.PluginName, &typ, &manifest, &pkg, &item.PluginHash, &status, &created, &updated); err != nil {
			t.Fatal(err)
		}
		item.PluginType = libplugin.Type(typ)
		// plugins.status is the host int enum (1=active, 2=archived); the 2.0
		// contract Status is a string enum that ValidatePlugin now enforces, so the
		// int must be mapped explicitly — an unset Status fails every row.
		item.Status = sweepStatus(status)
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

// sweepStatus maps the host plugins.status int enum to the 2.0 contract string
// enum. Anything other than archived is treated as active (the sweep query
// already restricts to status=1).
func sweepStatus(status uint8) libplugin.Status {
	if status == 2 {
		return libplugin.StatusArchived
	}
	return libplugin.StatusActive
}

// TestSweepStatusMappingIsRequired is the non-gated probe for the sweep blocker:
// the 2.0 ValidatePlugin enforces Status (a string enum) which the inlined 1.0
// validator did not, so a Plugin built without mapping the host int fails on
// every row. It reproduces the reviewer's finding and proves the mapping fixes
// it, without needing MYSQL_DSN.
func TestSweepStatusMappingIsRequired(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	ts := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	manifest := []byte(`{"$schema":"cowork-plugin-manifest-2.0.json","plugin_name":"S","plugin_type":"skill","name":"s","description":"d","labels":[],"examples":[]}`)
	pkg := []byte(`{"$schema":"cowork-plugin-package-2.0.json","attachments":[{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"}]}`)
	cm, _ := libplugin.CanonicalJSON(manifest)
	cp, _ := libplugin.CanonicalJSON(pkg)
	ph, _ := libplugin.ComputePluginHash(manifest, pkg)
	base := libplugin.Plugin{
		PluginID: id, PluginName: "S", PluginType: libplugin.TypeSkill,
		ManifestJSON: json.RawMessage(cm), PluginJSON: json.RawMessage(cp),
		PluginHash: ph, CreatedAt: ts, UpdatedAt: ts,
	}
	// Reproduce: an unset Status (the old sweep) fails an otherwise-valid row.
	if err := libplugin.ValidatePlugin(base); err == nil {
		t.Fatal("expected ValidatePlugin to reject a row with unset Status")
	}
	// The fix: mapping the host int (1 -> ACTIVE) makes it validate.
	base.Status = sweepStatus(1)
	if err := libplugin.ValidatePlugin(base); err != nil {
		t.Fatalf("status-mapped row still rejected: %v", err)
	}
}
