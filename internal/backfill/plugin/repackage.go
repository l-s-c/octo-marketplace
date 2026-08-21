// Repackage migrates persisted plugin packages from the first-generation
// layout (expert/instruction.md, expert/mcp.json, team/config.json only,
// connector/config.json) to the plugin-lib layout (root AGENTS.md + mcp.json,
// team AGENTS.md entry, top-level connector descriptor + standard mcp.json).
// It rewrites plugins.plugin_json (+plugin_hash), every plugin_versions
// snapshot, and every plugin_audit_logs snapshot, repairing the audit hash
// chain. Already-migrated rows produce no actions, so re-runs are no-ops.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

type RepackageCounts struct {
	Plugins  int `json:"plugins"`
	Versions int `json:"versions"`
	Audits   int `json:"audits"`
}

type RepackageReport struct {
	Mode       Mode            `json:"mode"`
	Planned    RepackageCounts `json:"planned"`
	Applied    RepackageCounts `json:"applied,omitempty"`
	Remaining  RepackageCounts `json:"remaining,omitempty"`
	Issues     []Issue         `json:"issues"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at"`
}

type repackageAction struct {
	count func(*RepackageCounts) *int
	query string
	args  []any
}

type repackagePlan struct {
	actions []repackageAction
	issues  []Issue
}

func (r *Runner) Repackage(ctx context.Context, o Options) (RepackageReport, error) {
	if o.Mode == "" {
		o.Mode = ModeDryRun
	}
	if o.Mode != ModeDryRun && o.Mode != ModeApply && o.Mode != ModeVerify {
		return RepackageReport{}, fmt.Errorf("invalid mode %q", o.Mode)
	}
	rep := RepackageReport{Mode: o.Mode, StartedAt: r.now(), Issues: []Issue{}}
	p, err := r.buildRepackage(ctx)
	if err != nil {
		return RepackageReport{}, err
	}
	rep.Issues = append(rep.Issues, p.issues...)
	for _, action := range p.actions {
		*action.count(&rep.Planned)++
	}
	if o.Mode == ModeApply {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return RepackageReport{}, err
		}
		defer tx.Rollback()
		for _, action := range p.actions {
			res, err := tx.ExecContext(ctx, action.query, action.args...)
			if err != nil {
				return RepackageReport{}, fmt.Errorf("repackage apply: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				*action.count(&rep.Applied)++
			}
		}
		if err := tx.Commit(); err != nil {
			return RepackageReport{}, err
		}
	}
	if o.Mode != ModeDryRun {
		remaining, err := r.buildRepackage(ctx)
		if err != nil {
			return RepackageReport{}, err
		}
		for _, action := range remaining.actions {
			*action.count(&rep.Remaining)++
		}
	}
	rep.FinishedAt = r.now()
	return rep, nil
}

func (r *Runner) buildRepackage(ctx context.Context) (repackagePlan, error) {
	var p repackagePlan
	types, err := r.pluginTypes(ctx)
	if err != nil {
		return p, err
	}
	if err := r.repackagePlugins(ctx, &p); err != nil {
		return p, err
	}
	if err := r.repackageVersions(ctx, types, &p); err != nil {
		return p, err
	}
	if err := r.repackageAudits(ctx, types, &p); err != nil {
		return p, err
	}
	return p, nil
}

// pluginTypes maps every plugin_id (live or deleted) to its type; version and
// audit snapshots do not carry the type themselves.
func (r *Runner) pluginTypes(ctx context.Context) (map[string]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id, plugin_type FROM plugins`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	types := map[string]string{}
	for rows.Next() {
		var id, typ string
		if err := rows.Scan(&id, &typ); err != nil {
			return nil, err
		}
		types[id] = typ
	}
	return types, rows.Err()
}

func (r *Runner) repackagePlugins(ctx context.Context, p *repackagePlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id, plugin_type, manifest_json, plugin_json, plugin_hash FROM plugins ORDER BY plugin_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, typ, oldHash string
		var manifest, pkg []byte
		if err := rows.Scan(&id, &typ, &manifest, &pkg, &oldHash); err != nil {
			return err
		}
		newPkg, changed, err := transformPackage(pkg, typ, manifest)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugins", id, err.Error()})
			continue
		}
		if !changed {
			continue
		}
		canonicalManifest, err := canonicalJSONBytes(manifest)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugins", id, err.Error()})
			continue
		}
		// Direct-SQL writes bypass the repository gate, so the transformed
		// package must pass the same secret scan the write path enforces.
		if err := pluginsvc.RejectSecretValues(newPkg); err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_secret_rejected", "plugins", id, err.Error()})
			continue
		}
		newHash := documentHash(canonicalManifest, newPkg)
		p.actions = append(p.actions, repackageAction{
			count: func(c *RepackageCounts) *int { return &c.Plugins },
			query: `UPDATE plugins SET plugin_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), newHash, id, oldHash},
		})
	}
	return rows.Err()
}

func (r *Runner) repackageVersions(ctx context.Context, types map[string]string, p *repackagePlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT version_id, plugin_id, manifest_json, plugin_json, plugin_hash FROM plugin_versions ORDER BY version_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, pluginID, oldHash string
		var manifest, pkg []byte
		if err := rows.Scan(&id, &pluginID, &manifest, &pkg, &oldHash); err != nil {
			return err
		}
		typ, known := types[pluginID]
		if !known {
			p.issues = append(p.issues, Issue{"info", "orphan_version", "plugin_versions", id, "no plugin row; snapshot left untouched"})
			continue
		}
		newPkg, changed, err := transformPackage(pkg, typ, manifest)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_versions", id, err.Error()})
			continue
		}
		if !changed {
			continue
		}
		canonicalManifest, err := canonicalJSONBytes(manifest)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_versions", id, err.Error()})
			continue
		}
		if err := pluginsvc.RejectSecretValues(newPkg); err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_secret_rejected", "plugin_versions", id, err.Error()})
			continue
		}
		newHash := documentHash(canonicalManifest, newPkg)
		p.actions = append(p.actions, repackageAction{
			count: func(c *RepackageCounts) *int { return &c.Versions },
			query: `UPDATE plugin_versions SET plugin_json=?, plugin_hash=? WHERE version_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), newHash, id, oldHash},
		})
	}
	return rows.Err()
}

// repackageAudits rewrites audit snapshots and repairs the per-plugin hash
// chain: after_hash becomes the hash of the transformed snapshot pair, and
// each row's before_hash becomes the previous row's after_hash.
func (r *Runner) repackageAudits(ctx context.Context, types map[string]string, p *repackagePlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT audit_log_id, plugin_id, manifest_snapshot_json, plugin_snapshot_json, before_hash, after_hash FROM plugin_audit_logs ORDER BY plugin_id, created_at, audit_log_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	lastHash := map[string]string{}
	for rows.Next() {
		var id, pluginID string
		var manifest, pkg []byte
		var beforeHash, afterHash *string
		if err := rows.Scan(&id, &pluginID, &manifest, &pkg, &beforeHash, &afterHash); err != nil {
			return err
		}
		typ, known := types[pluginID]
		if !known {
			continue
		}
		oldBefore, oldAfter := "", ""
		beforeWasNull, afterWasNull := beforeHash == nil, afterHash == nil
		if beforeHash != nil {
			oldBefore = *beforeHash
		}
		if afterHash != nil {
			oldAfter = *afterHash
		}
		newPkg, changed := pkg, false
		if len(pkg) > 0 {
			transformed, didChange, err := transformPackage(pkg, typ, manifest)
			if err != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_audit_logs", id, err.Error()})
				lastHash[pluginID] = oldAfter
				continue
			}
			newPkg, changed = transformed, didChange
		}
		if changed {
			if err := pluginsvc.RejectSecretValues(newPkg); err != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_secret_rejected", "plugin_audit_logs", id, err.Error()})
				lastHash[pluginID] = oldAfter
				continue
			}
		}
		snapshotHash := ""
		if len(newPkg) > 0 && len(manifest) > 0 {
			canonicalManifest, mErr := canonicalJSONBytes(manifest)
			canonicalPkg, pErr := canonicalJSONBytes(newPkg)
			if mErr != nil || pErr != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_audit_logs", id, "invalid snapshot JSON"})
				lastHash[pluginID] = oldAfter
				continue
			}
			snapshotHash = documentHash(canonicalManifest, canonicalPkg)
		}
		newBefore, newAfter := oldBefore, oldAfter
		if oldBefore != "" {
			if previous, seen := lastHash[pluginID]; seen && previous != "" {
				newBefore = previous
			}
		}
		if oldAfter != "" && snapshotHash != "" {
			newAfter = snapshotHash
		}
		lastHash[pluginID] = newAfter
		if !changed && newBefore == oldBefore && newAfter == oldAfter {
			continue
		}
		p.actions = append(p.actions, repackageAction{
			count: func(c *RepackageCounts) *int { return &c.Audits },
			query: `UPDATE plugin_audit_logs SET plugin_snapshot_json=?, before_hash=?, after_hash=? WHERE audit_log_id=?`,
			args:  []any{nullableJSON(newPkg), hashColumn(newBefore, beforeWasNull), hashColumn(newAfter, afterWasNull), id},
		})
	}
	return rows.Err()
}

// canonicalJSONBytes re-canonicalizes JSON read back from a MySQL JSON column
// (which reformats with spaces) into the compact json.Marshal form every hash
// in this system is computed over. Numbers survive via UseNumber.
func canonicalJSONBytes(raw []byte) ([]byte, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func documentHash(canonicalManifest, canonicalPackage []byte) string {
	return hashJSON(append(append(append([]byte{}, canonicalManifest...), '\n'), canonicalPackage...))
}

// hashColumn preserves the original NULL-vs-empty representation for hash
// columns that this pass did not assign a value to.
func hashColumn(value string, wasNull bool) any {
	if value == "" && wasNull {
		return nil
	}
	return value
}

func nullableJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// transformPackage converts one persisted package document to the plugin-lib
// layout. It is purely syntactic: decode with number preservation, rename or
// synthesize attachments, re-sort, re-marshal — json.Marshal of the decoded
// value is exactly the service canonical form, so untouched rows round-trip
// byte-identically and changed rows match what a fresh backfill would emit.
func transformPackage(raw []byte, pluginType string, manifest []byte) ([]byte, bool, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return raw, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, false, fmt.Errorf("invalid package JSON: %w", err)
	}
	attachments, _ := doc["attachments"].([]any)
	changed := false
	switch pluginType {
	case "expert":
		changed = renameAttachment(attachments, "expert/instruction.md", "AGENTS.md") || changed
		changed = renameAttachment(attachments, "expert/mcp.json", "mcp.json") || changed
	case "expert_team":
		changed = renameAttachment(attachments, "expert/instruction.md", "AGENTS.md") || changed
		if attachmentIndex(attachments, "AGENTS.md") < 0 {
			meta := manifestFields(manifest)
			leader, strategies := teamConfigFields(attachments)
			content := teamAgentsMarkdown(meta.pluginName, meta.description, leader, strategies)
			entry := map[string]any{
				"path":         "AGENTS.md",
				"content_type": "raw",
				"mime_type":    "text/markdown",
				"raw_content":  content,
				"content_size": json.Number(fmt.Sprintf("%d", len(content))),
				"content_hash": hashJSON([]byte(content)),
			}
			attachments = append(attachments, entry)
			doc["attachments"] = attachments
			changed = true
		}
	case "connector":
		if _, has := doc["connector"]; !has {
			meta := manifestFields(manifest)
			doc["connector"] = map[string]any{"type": "mcp", "source": "connector." + meta.name}
			changed = true
		}
		if idx := attachmentIndex(attachments, "connector/config.json"); idx >= 0 {
			meta := manifestFields(manifest)
			entry, _ := attachments[idx].(map[string]any)
			content, _ := entry["raw_content"].(string)
			var wrapper struct {
				Config    json.RawMessage `json:"config"`
				Transport string          `json:"transport"`
			}
			if err := json.Unmarshal([]byte(content), &wrapper); err != nil {
				return nil, false, fmt.Errorf("invalid connector/config.json: %w", err)
			}
			mcpDocument, err := connectorMCPDocument(wrapper.Config, wrapper.Transport, meta.name)
			if err != nil {
				return nil, false, err
			}
			mcpRaw, err := json.Marshal(mcpDocument)
			if err != nil {
				return nil, false, err
			}
			attachments[idx] = map[string]any{
				"path":         "mcp.json",
				"content_type": "raw",
				"mime_type":    "application/json",
				"raw_content":  string(mcpRaw),
				"content_size": json.Number(fmt.Sprintf("%d", len(mcpRaw))),
				"content_hash": hashJSON(mcpRaw),
			}
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	sort.SliceStable(attachments, func(i, j int) bool {
		left, _ := attachments[i].(map[string]any)
		right, _ := attachments[j].(map[string]any)
		lp, _ := left["path"].(string)
		rp, _ := right["path"].(string)
		return lp < rp
	})
	doc["attachments"] = attachments
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// renameAttachment updates the path (and refreshed content hash metadata stays
// as-is: only the path changes, the bytes do not).
func renameAttachment(attachments []any, from, to string) bool {
	idx := attachmentIndex(attachments, from)
	if idx < 0 {
		return false
	}
	entry, _ := attachments[idx].(map[string]any)
	entry["path"] = to
	return true
}

func attachmentIndex(attachments []any, path string) int {
	for i, item := range attachments {
		entry, _ := item.(map[string]any)
		if p, _ := entry["path"].(string); p == path {
			return i
		}
	}
	return -1
}

type manifestMeta struct {
	pluginName  string
	name        string
	description string
}

func manifestFields(manifest []byte) manifestMeta {
	var m struct {
		PluginName  string `json:"plugin_name"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(manifest, &m)
	if m.Name == "" {
		m.Name = m.PluginName
	}
	return manifestMeta{pluginName: m.PluginName, name: m.Name, description: m.Description}
}

func teamConfigFields(attachments []any) (string, any) {
	idx := attachmentIndex(attachments, "team/config.json")
	if idx < 0 {
		return "", nil
	}
	entry, _ := attachments[idx].(map[string]any)
	content, _ := entry["raw_content"].(string)
	var cfg struct {
		Leader     string `json:"leader"`
		Strategies any    `json:"strategies"`
	}
	_ = json.Unmarshal([]byte(content), &cfg)
	return cfg.Leader, cfg.Strategies
}
