// Expand migrates persisted skill packages from the legacy pointer layout
// (SKILL.md stub + skill/ref.json, or a skill/package.zip storage attachment)
// to the flat attachment tree: one attachment per file, text inlined as raw and
// binary/oversize files re-uploaded to the Space's managed prefix as storage
// attachments. Unlike the pure DB-transform phases this phase is STORAGE-AWARE —
// it fetches each skill's stored zip and re-uploads expanded files — so it needs
// object-storage credentials, and the transform is delegated to a SkillExpander
// (the live plugin service). It rewrites plugins, plugin_versions, and
// plugin_audit_logs (snapshots + hash chain). Already-expanded rows carry no
// legacy pointer and are skipped, so re-runs are no-ops.

package plugin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	libplugin "github.com/Mininglamp-OSS/octo-plugin-lib/plugin"
)

// SkillExpander turns a legacy skill package into the canonical attachment-tree
// package, reporting whether it changed. *pluginsvc.Service satisfies it.
type SkillExpander interface {
	ExpandSkillPackage(ctx context.Context, spaceID, pluginID string, pkg, keys json.RawMessage) (json.RawMessage, json.RawMessage, bool, error)
	WouldTruncateSkillPackage(spaceID, pluginID string, pkg, keys json.RawMessage) bool
}

type ExpandCounts struct {
	Plugins  int `json:"plugins"`
	Versions int `json:"versions"`
	Audits   int `json:"audits"`
}

type ExpandReport struct {
	Mode       Mode         `json:"mode"`
	Planned    ExpandCounts `json:"planned"`
	Applied    ExpandCounts `json:"applied,omitempty"`
	Remaining  ExpandCounts `json:"remaining,omitempty"`
	Spilled    []string     `json:"spilled,omitempty"`
	Issues     []Issue      `json:"issues"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
}

type expandAction struct {
	count func(*ExpandCounts) *int
	query string
	args  []any
	// guard marks a statement that carries an optimistic-concurrency predicate
	// (WHERE ... AND plugin_hash=?): the plugins and plugin_versions rewrites are
	// planned against a specific pre-scan hash. A guarded statement that changes
	// zero rows means the row was mutated by a concurrent live write between plan
	// and apply, so the plan no longer matches reality and the whole transaction
	// must abort rather than commit the unguarded audit-chain rewrite against a
	// state that never existed (a silent, unrecoverable chain break).
	guard bool
}

type expandPlan struct {
	actions []expandAction
	issues  []Issue
}

// ExpandSkills runs the storage-aware skill expansion. In apply mode it fetches
// each legacy skill's zip through the expander (uploading expanded files) and
// then rewrites the three tables in a single transaction; dry-run and verify
// only scan for remaining legacy pointers and touch neither storage nor the DB.
func (r *Runner) ExpandSkills(ctx context.Context, o Options, exp SkillExpander) (ExpandReport, error) {
	if o.Mode == "" {
		o.Mode = ModeDryRun
	}
	if o.Mode != ModeDryRun && o.Mode != ModeApply && o.Mode != ModeVerify {
		return ExpandReport{}, fmt.Errorf("invalid mode %q", o.Mode)
	}
	rep := ExpandReport{Mode: o.Mode, StartedAt: r.now(), Issues: []Issue{}}

	p, err := r.buildExpand(ctx, exp, o.Mode == ModeApply)
	if err != nil {
		return ExpandReport{}, err
	}
	rep.Issues = append(rep.Issues, p.issues...)
	for _, action := range p.actions {
		*action.count(&rep.Planned)++
	}
	if o.Mode == ModeApply {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return ExpandReport{}, err
		}
		defer tx.Rollback()
		if err := applyExpandActions(ctx, tx, p.actions, &rep.Applied); err != nil {
			return ExpandReport{}, err
		}
		if err := tx.Commit(); err != nil {
			return ExpandReport{}, err
		}
	}
	if o.Mode != ModeDryRun {
		remaining, err := r.buildExpand(ctx, exp, false)
		if err != nil {
			return ExpandReport{}, err
		}
		for _, action := range remaining.actions {
			*action.count(&rep.Remaining)++
		}
	}
	rep.FinishedAt = r.now()
	return rep, nil
}

// applyExpandActions executes a planned rewrite inside the caller's transaction.
// A guarded action (the plugins/plugin_versions optimistic CAS) that changes
// zero rows means a concurrent live write moved the row off the hash the plan
// was built against; the plan is stale, so it returns an error and the caller's
// deferred Rollback aborts the whole transaction rather than committing the
// unguarded audit-chain rewrite against a state that never existed. The phase is
// idempotent, so the operator simply re-runs against fresh state.
func applyExpandActions(ctx context.Context, tx *sql.Tx, actions []expandAction, applied *ExpandCounts) error {
	for _, action := range actions {
		res, err := tx.ExecContext(ctx, action.query, action.args...)
		if err != nil {
			return fmt.Errorf("expand apply: %w", err)
		}
		n, err := res.RowsAffected()
		if err == nil && n > 0 {
			*action.count(applied)++
		}
		if action.guard && err == nil && n == 0 {
			return fmt.Errorf("expand apply: optimistic guard failed (row changed since plan; re-run expand-skills against current state)")
		}
	}
	return nil
}

// buildExpand scans the three tables for skill rows still in the legacy pointer
// layout. When apply is false it produces count-only actions (no storage, no
// bytes computed) — used for planning and remaining-verify. When apply is true it
// runs the expander (fetching zips, uploading expanded files), recomputes the
// hash, and produces exec actions guarded on the current hash; audit rows also
// get their before/after hash chain repaired.
func (r *Runner) buildExpand(ctx context.Context, exp SkillExpander, apply bool) (expandPlan, error) {
	var p expandPlan
	types, spaces, err := r.pluginTypesAndSpaces(ctx)
	if err != nil {
		return p, err
	}
	if err := r.expandPlugins(ctx, exp, apply, spaces, &p); err != nil {
		return p, err
	}
	if err := r.expandVersions(ctx, exp, apply, types, spaces, &p); err != nil {
		return p, err
	}
	if err := r.expandAudits(ctx, exp, apply, types, spaces, &p); err != nil {
		return p, err
	}
	return p, nil
}

// pluginTypesAndSpaces maps every plugin_id to its type and Space; version and
// audit snapshots carry neither and inherit them from the parent plugin.
func (r *Runner) pluginTypesAndSpaces(ctx context.Context) (types, spaces map[string]string, err error) {
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id, plugin_type, space_id FROM plugins`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	types, spaces = map[string]string{}, map[string]string{}
	for rows.Next() {
		var id, typ string
		var space sql.NullString
		if err := rows.Scan(&id, &typ, &space); err != nil {
			return nil, nil, err
		}
		types[id] = typ
		if space.Valid {
			spaces[id] = space.String
		}
	}
	return types, spaces, rows.Err()
}

func (r *Runner) expandPlugins(ctx context.Context, exp SkillExpander, apply bool, spaces map[string]string, p *expandPlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id, manifest_json, plugin_json, attachment_keys_json, plugin_hash FROM plugins WHERE plugin_type='skill'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type work struct {
		id, oldHash   string
		manifest, pkg []byte
		keys          []byte
	}
	var todo []work
	for rows.Next() {
		var w work
		if err := rows.Scan(&w.id, &w.manifest, &w.pkg, &w.keys, &w.oldHash); err != nil {
			return err
		}
		if !hasLegacyPointer(w.pkg) {
			continue
		}
		todo = append(todo, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range todo {
		if !apply {
			p.actions = append(p.actions, expandAction{count: func(c *ExpandCounts) *int { return &c.Plugins }})
			continue
		}
		newPkg, newKeys, newHash, ok := r.expandRow(ctx, exp, spaces[w.id], w.id, "plugins", w.manifest, w.pkg, w.keys, p)
		if !ok {
			continue
		}
		p.actions = append(p.actions, expandAction{
			count: func(c *ExpandCounts) *int { return &c.Plugins },
			query: `UPDATE plugins SET plugin_json=?, attachment_keys_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), nullableJSON(newKeys), newHash, w.id, w.oldHash},
			guard: true,
		})
	}
	return nil
}

func (r *Runner) expandVersions(ctx context.Context, exp SkillExpander, apply bool, types, spaces map[string]string, p *expandPlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT version_id, plugin_id, manifest_json, plugin_json, attachment_keys_json, plugin_hash FROM plugin_versions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type work struct {
		id, pluginID, oldHash string
		manifest, pkg         []byte
		keys                  []byte
	}
	var todo []work
	for rows.Next() {
		var w work
		if err := rows.Scan(&w.id, &w.pluginID, &w.manifest, &w.pkg, &w.keys, &w.oldHash); err != nil {
			return err
		}
		if types[w.pluginID] != "skill" || !hasLegacyPointer(w.pkg) {
			continue
		}
		todo = append(todo, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range todo {
		if !apply {
			p.actions = append(p.actions, expandAction{count: func(c *ExpandCounts) *int { return &c.Versions }})
			continue
		}
		newPkg, newKeys, newHash, ok := r.expandRow(ctx, exp, spaces[w.pluginID], w.pluginID, "plugin_versions", w.manifest, w.pkg, w.keys, p)
		if !ok {
			continue
		}
		p.actions = append(p.actions, expandAction{
			count: func(c *ExpandCounts) *int { return &c.Versions },
			query: `UPDATE plugin_versions SET plugin_json=?, attachment_keys_json=?, plugin_hash=? WHERE version_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), nullableJSON(newKeys), newHash, w.id, w.oldHash},
			guard: true,
		})
	}
	return nil
}

// expandAudits rewrites skill audit snapshots and repairs the per-plugin hash
// chain, mirroring repackageAudits: after_hash becomes the lib hash of the
// expanded snapshot pair, and each row's before_hash becomes the previous row's
// after_hash.
func (r *Runner) expandAudits(ctx context.Context, exp SkillExpander, apply bool, types, spaces map[string]string, p *expandPlan) error {
	// Order in Go rather than SQL: after expansion the snapshot JSON columns are
	// large, and a server-side filesort over them can exhaust the sort buffer.
	rows, err := r.db.QueryContext(ctx, `SELECT audit_log_id, plugin_id, manifest_snapshot_json, plugin_snapshot_json, before_hash, after_hash, created_at FROM plugin_audit_logs`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type work struct {
		id, pluginID          string
		manifest, pkg         []byte
		beforeHash, afterHash *string
		createdAt             string
	}
	var todo []work
	for rows.Next() {
		var w work
		if err := rows.Scan(&w.id, &w.pluginID, &w.manifest, &w.pkg, &w.beforeHash, &w.afterHash, &w.createdAt); err != nil {
			return err
		}
		todo = append(todo, w)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Per-plugin chain order: created_at, then audit_log_id as a stable tiebreak.
	sort.SliceStable(todo, func(i, j int) bool {
		if todo[i].pluginID != todo[j].pluginID {
			return todo[i].pluginID < todo[j].pluginID
		}
		if todo[i].createdAt != todo[j].createdAt {
			return todo[i].createdAt < todo[j].createdAt
		}
		return todo[i].id < todo[j].id
	})

	lastHash := map[string]string{}
	for _, w := range todo {
		if types[w.pluginID] != "skill" {
			continue
		}
		oldBefore, oldAfter := "", ""
		beforeWasNull, afterWasNull := w.beforeHash == nil, w.afterHash == nil
		if w.beforeHash != nil {
			oldBefore = *w.beforeHash
		}
		if w.afterHash != nil {
			oldAfter = *w.afterHash
		}
		expandable := len(w.pkg) > 0 && hasLegacyPointer(w.pkg)
		// Audit rows carry no sidecar (keys == nil), and repackage stripped the
		// inline storage_uri, so a snapshot whose real files live in a managed
		// archive/object can no longer be resolved — ExpandSkillPackage would
		// collapse it to a SKILL.md stub (irreversible truncation of an immutable
		// record). Probe the actual resolvability (not a path-shape) and, when it
		// would truncate, leave the snapshot unexpanded. This is a permanent,
		// data-preserving skip (audit packages are never dereferenced for bytes),
		// so it is recorded at the non-gating "info" level rather than blocking the
		// rollout gate; see docs/plugin-backfill.md.
		wouldTruncate := expandable && exp.WouldTruncateSkillPackage(spaces[w.pluginID], w.pluginID, w.pkg, nil)
		if !apply {
			if wouldTruncate {
				p.issues = append(p.issues, Issue{"info", "audit_unexpandable", "plugin_audit_logs", w.id, "snapshot's archive/object key is unresolvable without a sidecar; left unexpanded to preserve the immutable audit record"})
			} else if expandable {
				p.actions = append(p.actions, expandAction{count: func(c *ExpandCounts) *int { return &c.Audits }})
			}
			continue
		}
		if wouldTruncate {
			p.issues = append(p.issues, Issue{"info", "audit_unexpandable", "plugin_audit_logs", w.id, "snapshot's archive/object key is unresolvable without a sidecar; left unexpanded to preserve the immutable audit record"})
			// A delete audit carries a NULL after_hash — it must NOT become the
			// chain link for the following row (mirrors the guard below).
			if oldAfter != "" {
				lastHash[w.pluginID] = oldAfter
			}
			continue
		}
		newPkg, changed := w.pkg, false
		if expandable {
			// Audit snapshots have no sidecar column and are never dereferenced for
			// object bytes, so the split keys are intentionally discarded — the
			// snapshot is only stripped to stay 2.0-valid and rehashed for the chain.
			expanded, _, didChange, err := exp.ExpandSkillPackage(ctx, spaces[w.pluginID], w.pluginID, w.pkg, nil)
			if err != nil {
				p.issues = append(p.issues, Issue{"skip", "expand_failed", "plugin_audit_logs", w.id, err.Error()})
				lastHash[w.pluginID] = oldAfter
				continue
			}
			newPkg, changed = expanded, didChange
		}
		snapshotHash := ""
		if len(newPkg) > 0 && len(w.manifest) > 0 {
			computed, err := libplugin.ComputePluginHash(w.manifest, newPkg)
			if err != nil {
				p.issues = append(p.issues, Issue{"skip", "expand_failed", "plugin_audit_logs", w.id, err.Error()})
				lastHash[w.pluginID] = oldAfter
				continue
			}
			snapshotHash = computed
		}
		newBefore, newAfter := oldBefore, oldAfter
		if oldBefore != "" {
			if previous, seen := lastHash[w.pluginID]; seen && previous != "" {
				newBefore = previous
			}
		}
		if oldAfter != "" && snapshotHash != "" {
			newAfter = snapshotHash
		}
		// A delete audit carries a NULL after_hash (newAfter==""); it must NOT
		// become the chain link for the following row, or that row's before_hash
		// repair is skipped (previous=="") and the chain breaks. Keep the last
		// non-null after so a subsequent audit still links to a real predecessor.
		if newAfter != "" {
			lastHash[w.pluginID] = newAfter
		}
		if !changed && newBefore == oldBefore && newAfter == oldAfter {
			continue
		}
		p.actions = append(p.actions, expandAction{
			count: func(c *ExpandCounts) *int { return &c.Audits },
			query: `UPDATE plugin_audit_logs SET plugin_snapshot_json=?, before_hash=?, after_hash=? WHERE audit_log_id=?`,
			args:  []any{nullableJSON(newPkg), hashColumn(newBefore, beforeWasNull), hashColumn(newAfter, afterWasNull), w.id},
		})
	}
	return nil
}

// expandRow runs the expander for one plugins/plugin_versions row and recomputes
// the lib hash. It returns ok=false (recording an issue) when the package fails
// to expand.
func (r *Runner) expandRow(ctx context.Context, exp SkillExpander, spaceID, pluginID, source string, manifest, pkg, keys []byte, p *expandPlan) (newPkg []byte, newKeys []byte, newHash string, ok bool) {
	expanded, splitKeys, changed, err := exp.ExpandSkillPackage(ctx, spaceID, pluginID, pkg, keys)
	if err != nil {
		p.issues = append(p.issues, Issue{"skip", "expand_failed", source, pluginID, err.Error()})
		return nil, nil, "", false
	}
	if !changed {
		return nil, nil, "", false
	}
	hash, err := libplugin.ComputePluginHash(manifest, expanded)
	if err != nil {
		p.issues = append(p.issues, Issue{"skip", "expand_failed", source, pluginID, err.Error()})
		return nil, nil, "", false
	}
	return expanded, splitKeys, hash, true
}

// hasLegacyPointer reports whether a package still carries a skill/ref.json or
// skill/package.zip attachment — the marker of the pre-expansion layout.
func hasLegacyPointer(pkg []byte) bool {
	var doc struct {
		Attachments []struct {
			Path string `json:"path"`
		} `json:"attachments"`
	}
	if json.Unmarshal(pkg, &doc) != nil {
		return false
	}
	for _, a := range doc.Attachments {
		if a.Path == "skill/ref.json" || a.Path == "skill/package.zip" {
			return true
		}
	}
	return false
}
