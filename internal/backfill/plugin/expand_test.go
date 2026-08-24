package plugin

import (
	"context"
	"encoding/json"
	"testing"

	libplugin "codex.mlamp.cn/dmwork/octo-plugin-lib/plugin"
	"github.com/DATA-DOG/go-sqlmock"
)

type stubExpander struct{ out string }

func (s stubExpander) ExpandSkillPackage(_ context.Context, _, _ string, pkg json.RawMessage) (json.RawMessage, bool, error) {
	// A tree package (no legacy pointer) passes straight through unchanged; a
	// legacy one expands to the stub tree.
	if !hasLegacyPointer(pkg) {
		return pkg, false, nil
	}
	return json.RawMessage(s.out), true, nil
}

func legacySkillPkg() string {
	return `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# stub") + `,` +
		attachmentJSON("skill/ref.json", "application/json", `{"zip_object_key":"experts/x/skill.zip"}`) + `]}`
}

func TestHasLegacyPointer(t *testing.T) {
	if !hasLegacyPointer([]byte(legacySkillPkg())) {
		t.Fatal("ref.json package should be legacy")
	}
	tree := `{"attachments":[` + attachmentJSON("SKILL.md", "text/markdown", "# d") + `]}`
	if hasLegacyPointer([]byte(tree)) {
		t.Fatal("tree package should not be legacy")
	}
}

// TestExpandPluginsGuardsUpdateWithOldHash pins the plugins-channel UPDATE:
// guarded by the pre-expansion hash and carrying the recomputed lib hash of the
// expanded tree package.
func TestExpandPluginsGuardsUpdateWithOldHash(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db)

	manifest := `{"plugin_name":"技能","name":"skill-a","description":"d"}`
	tree := `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# real doc") + `]}`

	mock.ExpectQuery(`SELECT plugin_id, manifest_json, plugin_json, plugin_hash FROM plugins WHERE plugin_type='skill'`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "manifest_json", "plugin_json", "plugin_hash"}).
			AddRow("s1", manifest, legacySkillPkg(), "sha256:old"))

	var p expandPlan
	if err := r.expandPlugins(context.Background(), stubExpander{out: tree}, true, map[string]string{"s1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 1 || len(p.issues) != 0 {
		t.Fatalf("actions=%d issues=%#v", len(p.actions), p.issues)
	}
	a := p.actions[0]
	if a.args[2] != "s1" || a.args[3] != "sha256:old" {
		t.Fatalf("guard args = %#v", a.args)
	}
	want, err := libplugin.ComputePluginHash([]byte(manifest), []byte(tree))
	if err != nil || a.args[1] != want {
		t.Fatalf("hash = %v want %v (err %v)", a.args[1], want, err)
	}
}

// TestExpandPluginsSkipsTreeRows confirms already-expanded skills produce no
// action (idempotent re-run) because they carry no legacy pointer.
func TestExpandPluginsSkipsTreeRows(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db)
	tree := `{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# d") + `]}`
	mock.ExpectQuery(`SELECT plugin_id, manifest_json, plugin_json, plugin_hash FROM plugins WHERE plugin_type='skill'`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "manifest_json", "plugin_json", "plugin_hash"}).
			AddRow("s1", `{"plugin_name":"x","name":"x","description":"d"}`, tree, "sha256:cur"))
	var p expandPlan
	if err := r.expandPlugins(context.Background(), stubExpander{}, true, map[string]string{"s1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 0 {
		t.Fatalf("tree row should produce no action: %#v", p.actions)
	}
}
