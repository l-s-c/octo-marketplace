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

	libplugin "codex.mlamp.cn/dmwork/octo-plugin-lib/plugin"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

// SkillExpander turns a legacy skill package into the canonical attachment-tree
// package, reporting whether it changed. *pluginsvc.Service satisfies it.
type SkillExpander interface {
	ExpandSkillPackage(ctx context.Context, spaceID, pluginID string, pkg json.RawMessage) (json.RawMessage, bool, error)
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
		for _, action := range p.actions {
			res, err := tx.ExecContext(ctx, action.query, action.args...)
			if err != nil {
				return ExpandReport{}, fmt.Errorf("expand apply: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				*action.count(&rep.Applied)++
			}
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
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id, manifest_json, plugin_json, plugin_hash FROM plugins WHERE plugin_type='skill'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type work struct {
		id, oldHash   string
		manifest, pkg []byte
	}
	var todo []work
	for rows.Next() {
		var w work
		if err := rows.Scan(&w.id, &w.manifest, &w.pkg, &w.oldHash); err != nil {
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
		newPkg, newHash, ok := r.expandRow(ctx, exp, spaces[w.id], w.id, "plugins", w.manifest, w.pkg, p)
		if !ok {
			continue
		}
		p.actions = append(p.actions, expandAction{
			count: func(c *ExpandCounts) *int { return &c.Plugins },
			query: `UPDATE plugins SET plugin_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), newHash, w.id, w.oldHash},
		})
	}
	return nil
}

func (r *Runner) expandVersions(ctx context.Context, exp SkillExpander, apply bool, types, spaces map[string]string, p *expandPlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT version_id, plugin_id, manifest_json, plugin_json, plugin_hash FROM plugin_versions`)
	if err != nil {
		return err
	}
	defer rows.Close()
	type work struct {
		id, pluginID, oldHash string
		manifest, pkg         []byte
	}
	var todo []work
	for rows.Next() {
		var w work
		if err := rows.Scan(&w.id, &w.pluginID, &w.manifest, &w.pkg, &w.oldHash); err != nil {
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
		newPkg, newHash, ok := r.expandRow(ctx, exp, spaces[w.pluginID], w.pluginID, "plugin_versions", w.manifest, w.pkg, p)
		if !ok {
			continue
		}
		p.actions = append(p.actions, expandAction{
			count: func(c *ExpandCounts) *int { return &c.Versions },
			query: `UPDATE plugin_versions SET plugin_json=?, plugin_hash=? WHERE version_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), newHash, w.id, w.oldHash},
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
		if !apply {
			if expandable {
				p.actions = append(p.actions, expandAction{count: func(c *ExpandCounts) *int { return &c.Audits }})
			}
			continue
		}
		newPkg, changed := w.pkg, false
		if expandable {
			expanded, didChange, err := exp.ExpandSkillPackage(ctx, spaces[w.pluginID], w.pluginID, w.pkg)
			if err != nil {
				p.issues = append(p.issues, Issue{"skip", "expand_failed", "plugin_audit_logs", w.id, err.Error()})
				lastHash[w.pluginID] = oldAfter
				continue
			}
			newPkg, changed = expanded, didChange
			if changed {
				if err := pluginsvc.RejectSecretValues(newPkg); err != nil {
					p.issues = append(p.issues, Issue{"skip", "expand_secret_rejected", "plugin_audit_logs", w.id, err.Error()})
					lastHash[w.pluginID] = oldAfter
					continue
				}
			}
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
		lastHash[w.pluginID] = newAfter
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

// expandRow runs the expander for one plugins/plugin_versions row, gating on the
// secret scan and recomputing the lib hash. It returns ok=false (recording an
// issue) when the package fails to expand or the scan rejects it.
func (r *Runner) expandRow(ctx context.Context, exp SkillExpander, spaceID, pluginID, source string, manifest, pkg []byte, p *expandPlan) (newPkg []byte, newHash string, ok bool) {
	expanded, changed, err := exp.ExpandSkillPackage(ctx, spaceID, pluginID, pkg)
	if err != nil {
		p.issues = append(p.issues, Issue{"skip", "expand_failed", source, pluginID, err.Error()})
		return nil, "", false
	}
	if !changed {
		return nil, "", false
	}
	if err := pluginsvc.RejectSecretValues(expanded); err != nil {
		p.issues = append(p.issues, Issue{"skip", "expand_secret_rejected", source, pluginID, err.Error()})
		return nil, "", false
	}
	hash, err := libplugin.ComputePluginHash(manifest, expanded)
	if err != nil {
		p.issues = append(p.issues, Issue{"skip", "expand_failed", source, pluginID, err.Error()})
		return nil, "", false
	}
	return expanded, hash, true
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
