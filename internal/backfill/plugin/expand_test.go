package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	libplugin "github.com/Mininglamp-OSS/octo-plugin-lib/plugin"
)

type stubExpander struct {
	out, keys string
	truncate  bool
}

func (s stubExpander) ExpandSkillPackage(_ context.Context, _, _ string, pkg, _ json.RawMessage) (json.RawMessage, json.RawMessage, bool, error) {
	// A tree package (no legacy pointer) passes straight through unchanged; a
	// legacy one expands to the stub tree.
	if !hasLegacyPointer(pkg) {
		return pkg, nil, false, nil
	}
	return json.RawMessage(s.out), json.RawMessage(s.keys), true, nil
}

// WouldTruncateSkillPackage reports the configured verdict for a legacy pointer;
// most tests expand normally (truncate == false).
func (s stubExpander) WouldTruncateSkillPackage(_, _ string, pkg, _ json.RawMessage) bool {
	return s.truncate && hasLegacyPointer(pkg)
}

func legacySkillPkg() string {
	return `{"$schema":"cowork-plugin-package-2.0.json","attachments":[` +
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

// TestExpandAuditsSkipsUnexpandableSnapshotFailClosed pins the audit skip: a
// legacy snapshot whose archive/object key cannot be resolved is left unexpanded
// (never rebuilt as a stub), recorded at the non-gating "info" level so the
// rollout gate can still go green, and emits no rewrite action for that row.
func TestExpandAuditsSkipsUnexpandableSnapshotFailClosed(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db)

	manifest := `{"plugin_name":"技能","name":"skill-a","description":"d"}`
	before, after := "sha256:b", "sha256:a"
	mock.ExpectQuery(`SELECT audit_log_id, plugin_id, manifest_snapshot_json, plugin_snapshot_json, before_hash, after_hash, created_at FROM plugin_audit_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"audit_log_id", "plugin_id", "manifest_snapshot_json", "plugin_snapshot_json", "before_hash", "after_hash", "created_at"}).
			AddRow("a1", "p1", manifest, legacySkillPkg(), before, after, "2026-01-01T00:00:00Z"))

	var p expandPlan
	if err := r.expandAudits(context.Background(), stubExpander{truncate: true}, true,
		map[string]string{"p1": "skill"}, map[string]string{"p1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 0 {
		t.Fatalf("unexpandable audit must emit no rewrite action, got %d", len(p.actions))
	}
	if len(p.issues) != 1 || p.issues[0].Level != "info" || p.issues[0].Code != "audit_unexpandable" {
		t.Fatalf("expected one non-gating info skip, got %#v", p.issues)
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
	tree := `{"$schema":"cowork-plugin-package-2.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# real doc") + `]}`

	mock.ExpectQuery(`SELECT plugin_id, manifest_json, plugin_json, attachment_keys_json, plugin_hash FROM plugins WHERE plugin_type='skill'`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "manifest_json", "plugin_json", "attachment_keys_json", "plugin_hash"}).
			AddRow("s1", manifest, legacySkillPkg(), nil, "sha256:old"))

	var p expandPlan
	if err := r.expandPlugins(context.Background(), stubExpander{out: tree}, true, map[string]string{"s1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 1 || len(p.issues) != 0 {
		t.Fatalf("actions=%d issues=%#v", len(p.actions), p.issues)
	}
	a := p.actions[0]
	// args: plugin_json, attachment_keys_json, plugin_hash, plugin_id, guard hash.
	if a.args[3] != "s1" || a.args[4] != "sha256:old" {
		t.Fatalf("guard args = %#v", a.args)
	}
	if a.args[1] != nil {
		t.Fatalf("all-inline expand should carry a NULL sidecar, got %#v", a.args[1])
	}
	want, err := libplugin.ComputePluginHash([]byte(manifest), []byte(tree))
	if err != nil || a.args[2] != want {
		t.Fatalf("hash = %v want %v (err %v)", a.args[2], want, err)
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
	tree := `{"$schema":"cowork-plugin-package-2.0.json","attachments":[` +
		attachmentJSON("SKILL.md", "text/markdown", "# d") + `]}`
	mock.ExpectQuery(`SELECT plugin_id, manifest_json, plugin_json, attachment_keys_json, plugin_hash FROM plugins WHERE plugin_type='skill'`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "manifest_json", "plugin_json", "attachment_keys_json", "plugin_hash"}).
			AddRow("s1", `{"plugin_name":"x","name":"x","description":"d"}`, tree, nil, "sha256:cur"))
	var p expandPlan
	if err := r.expandPlugins(context.Background(), stubExpander{}, true, map[string]string{"s1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 0 {
		t.Fatalf("tree row should produce no action: %#v", p.actions)
	}
}

// TestApplyExpandActionsAbortsOnGuardMiss is the P1-1 regression: when a guarded
// plugins/plugin_versions CAS changes zero rows (a concurrent live write moved
// the row off the planned hash), the apply must abort BEFORE the unguarded audit
// rewrite runs, so no partial plan commits and the audit chain is never rewritten
// against a state that never existed. The audit UPDATE is deliberately NOT
// expected on the mock: if it ran, the test fails.
func TestApplyExpandActionsAbortsOnGuardMiss(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE plugins SET`).WillReturnResult(sqlmock.NewResult(0, 0)) // guard miss
	mock.ExpectRollback()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	actions := []expandAction{
		{count: func(c *ExpandCounts) *int { return &c.Plugins }, query: `UPDATE plugins SET plugin_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`, args: []any{"pkg", "sha256:new", "s1", "sha256:old"}, guard: true},
		{count: func(c *ExpandCounts) *int { return &c.Audits }, query: `UPDATE plugin_audit_logs SET plugin_snapshot_json=?, before_hash=?, after_hash=? WHERE audit_log_id=?`, args: []any{"pkg", "sha256:b", "sha256:a", "aud1"}},
	}
	var applied ExpandCounts
	if err := applyExpandActions(context.Background(), tx, actions, &applied); err == nil {
		t.Fatal("guarded zero-row apply must abort, got nil error")
	}
	_ = tx.Rollback()
	if applied.Audits != 0 {
		t.Fatalf("audit rewrite must not run after a guard miss: %#v", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestApplyExpandActionsCommitsWhenGuardHolds confirms the normal path: a guarded
// CAS that changes one row lets the audit rewrite proceed and both are counted.
func TestApplyExpandActionsCommitsWhenGuardHolds(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE plugins SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`UPDATE plugin_audit_logs SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	actions := []expandAction{
		{count: func(c *ExpandCounts) *int { return &c.Plugins }, query: `UPDATE plugins SET plugin_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`, args: []any{"pkg", "sha256:new", "s1", "sha256:old"}, guard: true},
		{count: func(c *ExpandCounts) *int { return &c.Audits }, query: `UPDATE plugin_audit_logs SET plugin_snapshot_json=?, before_hash=?, after_hash=? WHERE audit_log_id=?`, args: []any{"pkg", "sha256:b", "sha256:a", "aud1"}},
	}
	var applied ExpandCounts
	if err := applyExpandActions(context.Background(), tx, actions, &applied); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if applied.Plugins != 1 || applied.Audits != 1 {
		t.Fatalf("both rewrites should count: %#v", applied)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
