package plugin

import (
	"context"
	"encoding/json"
	"strings"
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

// TestExpandPluginsSkipsTruncatableLiveRowFailClosed pins the LIVE-row guard: a
// skill whose archive/object key cannot be resolved is skipped (fail-closed) with
// a gating "skip" issue rather than collapsed to a SKILL.md stub — the guard is
// wired to the live plugins/plugin_versions paths, not only the audit table.
func TestExpandPluginsSkipsTruncatableLiveRowFailClosed(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db)
	manifest := `{"$schema":"cowork-plugin-manifest-2.0.json","plugin_name":"S","name":"s","description":"d"}`
	mock.ExpectQuery(`SELECT plugin_id, manifest_json, plugin_json, attachment_keys_json, plugin_hash FROM plugins WHERE plugin_type='skill'`).
		WillReturnRows(sqlmock.NewRows([]string{"plugin_id", "manifest_json", "plugin_json", "attachment_keys_json", "plugin_hash"}).
			AddRow("s1", manifest, legacySkillPkg(), nil, "sha256:old"))
	var p expandPlan
	if err := r.expandPlugins(context.Background(), stubExpander{truncate: true}, true, map[string]string{"s1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}
	if len(p.actions) != 0 {
		t.Fatalf("truncatable live row must emit no rewrite action, got %d", len(p.actions))
	}
	if len(p.issues) != 1 || p.issues[0].Level != "skip" || p.issues[0].Code != "expand_would_truncate" {
		t.Fatalf("expected one gating expand_would_truncate skip, got %#v", p.issues)
	}
}

// zipTruncateExpander truncates only snapshots that carry skill/package.zip (an
// unresolvable managed archive) and expands ref.json snapshots — so one plugin's
// audit history can mix expanded and skipped rows.
type zipTruncateExpander struct{ out string }

func (zipTruncateExpander) WouldTruncateSkillPackage(_, _ string, pkg, _ json.RawMessage) bool {
	return strings.Contains(string(pkg), "skill/package.zip")
}

func (z zipTruncateExpander) ExpandSkillPackage(_ context.Context, _, _ string, pkg, _ json.RawMessage) (json.RawMessage, json.RawMessage, bool, error) {
	if !hasLegacyPointer(pkg) {
		return pkg, nil, false, nil
	}
	return json.RawMessage(z.out), nil, true, nil
}

// TestExpandAuditsRepairsChainAcrossSkippedBoundary pins the audit-chain fix: when
// a plugin's history mixes an expanded row (A, rehashed) and a skipped truncatable
// row (B), B's before_hash must be repaired to A's NEW after_hash so the per-plugin
// chain stays linked across the expanded→skipped boundary. B's snapshot and its own
// after_hash stay untouched, and B is still a non-gating info skip.
func TestExpandAuditsRepairsChainAcrossSkippedBoundary(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r := New(db)

	manifest := `{"plugin_name":"技能","name":"skill-a","description":"d"}`
	refPkg := legacySkillPkg() // skill/ref.json → expandable, not truncatable
	zipPkg := `{"attachments":[` + attachmentJSON("SKILL.md", "text/markdown", "# stub") +
		`,{"path":"skill/package.zip","content_type":"storage","mime_type":"application/zip","content_size":10,"content_hash":"sha256:0"}]}`
	tree := `{"$schema":"cowork-plugin-package-2.0.json","attachments":[` + attachmentJSON("SKILL.md", "text/markdown", "# real") + `]}`

	mock.ExpectQuery(`SELECT audit_log_id, plugin_id, manifest_snapshot_json, plugin_snapshot_json, before_hash, after_hash, created_at FROM plugin_audit_logs`).
		WillReturnRows(sqlmock.NewRows([]string{"audit_log_id", "plugin_id", "manifest_snapshot_json", "plugin_snapshot_json", "before_hash", "after_hash", "created_at"}).
			AddRow("a1", "p1", manifest, refPkg, "sha256:root", "sha256:a-old", "2026-01-01T00:00:00Z").
			AddRow("a2", "p1", manifest, zipPkg, "sha256:a-old", "sha256:b", "2026-01-02T00:00:00Z"))

	var p expandPlan
	if err := r.expandAudits(context.Background(), zipTruncateExpander{out: tree}, true,
		map[string]string{"p1": "skill"}, map[string]string{"p1": "space-a"}, &p); err != nil {
		t.Fatal(err)
	}

	var aNewAfter, bBeforeRepair string
	var bRepairSeen bool
	for _, act := range p.actions {
		if strings.Contains(act.query, "plugin_snapshot_json") && len(act.args) == 4 && act.args[3] == "a1" {
			aNewAfter, _ = act.args[2].(string) // after_hash
		}
		if act.query == `UPDATE plugin_audit_logs SET before_hash=? WHERE audit_log_id=?` && act.args[1] == "a2" {
			bRepairSeen = true
			bBeforeRepair, _ = act.args[0].(string)
		}
	}
	if aNewAfter == "" {
		t.Fatal("A did not get a rewritten after_hash")
	}
	if !bRepairSeen {
		t.Fatal("B's before_hash was not repaired — the chain breaks at the expanded→skipped boundary")
	}
	if bBeforeRepair != aNewAfter {
		t.Fatalf("B before_hash %q != A new after_hash %q — chain not linked", bBeforeRepair, aNewAfter)
	}
	if len(p.issues) != 1 || p.issues[0].Code != "audit_unexpandable" {
		t.Fatalf("expected one non-gating info skip for B, got %#v", p.issues)
	}
}
