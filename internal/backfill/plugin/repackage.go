// Repackage migrates persisted plugin documents to the octo-plugin-lib
// contracts/v1 layout: every package drops the redundant embedded
// manifest.json, expert_team packages collapse to a single AGENTS.md carrying
// the collaboration/dispatch prose (team/config.json removed), legacy
// first-generation layouts (expert/instruction.md, connector/config.json) are
// still converted, expert_team_member relations rename to expert_team_expert
// (with deterministic relation IDs re-derived), and every plugin_hash is
// recomputed with the lib formula sha256(canonicalManifest||canonicalPackage).
// It rewrites plugins, plugin_versions (documents + relation snapshots), and
// plugin_audit_logs (snapshots + hash chain). Already-migrated rows produce no
// actions, so re-runs are no-ops.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	libplugin "github.com/Mininglamp-OSS/octo-plugin-lib/plugin"
)

const (
	legacyTeamRelation   = "expert_team_member"
	contractTeamRelation = "expert_team_expert"
)

type RepackageCounts struct {
	Plugins   int `json:"plugins"`
	Versions  int `json:"versions"`
	Audits    int `json:"audits"`
	Relations int `json:"relations"`
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
	// guard marks an optimistic-concurrency statement (plugins/plugin_versions,
	// WHERE ... AND plugin_hash=?). A guarded statement changing zero rows means a
	// concurrent live write moved the row off the planned hash, so the plan is
	// stale and the whole transaction must abort rather than commit the unguarded
	// audit-chain rewrite against a state that never existed. Mirrors expand.go.
	guard bool
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
			n, err := res.RowsAffected()
			if err == nil && n > 0 {
				*action.count(&rep.Applied)++
			}
			// A guarded CAS (plugins/plugin_versions) changing zero rows means a
			// concurrent live write moved the row off the planned hash: abort the
			// transaction rather than commit the unguarded audit rewrite against a
			// vanished state (silent chain break). Re-run against fresh state.
			if action.guard && err == nil && n == 0 {
				return RepackageReport{}, fmt.Errorf("repackage apply: optimistic guard failed (row changed since plan; re-run repackage against current state)")
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
	if err := r.repackageRelations(ctx, &p); err != nil {
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
		newPkg, newKeys, changed, err := transformPackage(pkg, typ, manifest)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugins", id, err.Error()})
			continue
		}
		if changed {
			if err := decodablePackage(newPkg, typ); err != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_invalid_package", "plugins", id, err.Error()})
				continue
			}
		}
		newHash, err := libplugin.ComputePluginHash(manifest, newPkg)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugins", id, err.Error()})
			continue
		}
		if !changed && newHash == oldHash {
			continue
		}
		// Only touch attachment_keys_json when this pass actually split storage
		// keys out of the package (an un-migrated row): rows already migrated by
		// the expand phase carry no inline storage_uri, so newKeys is nil and their
		// existing sidecar must be preserved rather than overwritten with NULL.
		action := repackageAction{
			count: func(c *RepackageCounts) *int { return &c.Plugins },
			query: `UPDATE plugins SET plugin_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), newHash, id, oldHash},
			guard: true,
		}
		if newKeys != nil {
			action.query = `UPDATE plugins SET plugin_json=?, attachment_keys_json=?, plugin_hash=? WHERE plugin_id=? AND plugin_hash=?`
			action.args = []any{string(newPkg), string(newKeys), newHash, id, oldHash}
		}
		p.actions = append(p.actions, action)
	}
	return rows.Err()
}

func (r *Runner) repackageVersions(ctx context.Context, types map[string]string, p *repackagePlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT version_id, plugin_id, manifest_json, plugin_json, relations_json, plugin_hash FROM plugin_versions ORDER BY version_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, pluginID, oldHash string
		var manifest, pkg, relations []byte
		if err := rows.Scan(&id, &pluginID, &manifest, &pkg, &relations, &oldHash); err != nil {
			return err
		}
		typ, known := types[pluginID]
		if !known {
			p.issues = append(p.issues, Issue{"info", "orphan_version", "plugin_versions", id, "no plugin row; snapshot left untouched"})
			continue
		}
		newPkg, newKeys, changed, err := transformPackage(pkg, typ, manifest)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_versions", id, err.Error()})
			continue
		}
		if changed {
			if err := decodablePackage(newPkg, typ); err != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_invalid_package", "plugin_versions", id, err.Error()})
				continue
			}
		}
		newRelations, relationsChanged, err := transformVersionRelations(relations, pluginID)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_versions", id, err.Error()})
			continue
		}
		newHash, err := libplugin.ComputePluginHash(manifest, newPkg)
		if err != nil {
			p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_versions", id, err.Error()})
			continue
		}
		if !changed && !relationsChanged && newHash == oldHash {
			continue
		}
		// See repackagePlugins: only set attachment_keys_json when this pass split
		// storage keys out (un-migrated snapshot); otherwise leave it untouched.
		action := repackageAction{
			count: func(c *RepackageCounts) *int { return &c.Versions },
			query: `UPDATE plugin_versions SET plugin_json=?, relations_json=?, plugin_hash=? WHERE version_id=? AND plugin_hash=?`,
			args:  []any{string(newPkg), string(newRelations), newHash, id, oldHash},
			guard: true,
		}
		if newKeys != nil {
			action.query = `UPDATE plugin_versions SET plugin_json=?, attachment_keys_json=?, relations_json=?, plugin_hash=? WHERE version_id=? AND plugin_hash=?`
			action.args = []any{string(newPkg), string(newKeys), string(newRelations), newHash, id, oldHash}
		}
		p.actions = append(p.actions, action)
	}
	return rows.Err()
}

// repackageAudits rewrites audit snapshots and repairs the per-plugin hash
// chain: after_hash becomes the lib hash of the transformed snapshot pair, and
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
			transformed, _, didChange, err := transformPackage(pkg, typ, manifest)
			if err != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_audit_logs", id, err.Error()})
				lastHash[pluginID] = oldAfter
				continue
			}
			newPkg, changed = transformed, didChange
			if changed {
				if err := decodablePackage(newPkg, typ); err != nil {
					p.issues = append(p.issues, Issue{"skip", "repackage_invalid_package", "plugin_audit_logs", id, err.Error()})
					lastHash[pluginID] = oldAfter
					continue
				}
			}
		}
		snapshotHash := ""
		if len(newPkg) > 0 && len(manifest) > 0 {
			computed, err := libplugin.ComputePluginHash(manifest, newPkg)
			if err != nil {
				p.issues = append(p.issues, Issue{"skip", "repackage_failed", "plugin_audit_logs", id, err.Error()})
				lastHash[pluginID] = oldAfter
				continue
			}
			snapshotHash = computed
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
		// A delete audit carries a NULL after_hash (newAfter==""); it must NOT
		// become the chain link for the following row, or that row's before_hash
		// repair is skipped (previous=="") and the chain breaks. Keep the last
		// non-null after so a subsequent audit still links to a real predecessor.
		if newAfter != "" {
			lastHash[pluginID] = newAfter
		}
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

// repackageRelations renames live expert_team_member relations to the contract
// enum. Backfill-derived deterministic relation IDs embed the type string, so
// rows still carrying the old deterministic ID are re-keyed to the new
// derivation; API-created rows keep their IDs and only the type changes.
func (r *Runner) repackageRelations(ctx context.Context, p *repackagePlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT relation_id, source_plugin_id, target_plugin_id, sort_order FROM plugin_relations WHERE relation_type=? ORDER BY relation_id`, legacyTeamRelation)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, source, target string
		var order int
		if err := rows.Scan(&id, &source, &target, &order); err != nil {
			return err
		}
		newID := id
		if id == deterministicRelationID(source, legacyTeamRelation, order, target) {
			newID = deterministicRelationID(source, contractTeamRelation, order, target)
		}
		if newID != id {
			// Re-keying to a deterministic contract ID that already exists would
			// raise a duplicate-key error inside the apply transaction and roll back
			// every unrelated rewrite with no way to identify the offender. Detect
			// the collision at plan time and record a skip instead (P2).
			exists, err := r.rowExists(ctx, `SELECT COUNT(*) FROM plugin_relations WHERE relation_id=?`, newID)
			if err != nil {
				return err
			}
			if exists {
				p.issues = append(p.issues, Issue{"skip", "relation_rekey_conflict", "plugin_relations", id, "target relation_id " + newID + " already exists"})
				continue
			}
		}
		p.actions = append(p.actions, repackageAction{
			count: func(c *RepackageCounts) *int { return &c.Relations },
			query: `UPDATE plugin_relations SET relation_id=?, relation_type=? WHERE relation_id=? AND relation_type=?`,
			args:  []any{newID, contractTeamRelation, id, legacyTeamRelation},
		})
	}
	return rows.Err()
}

// deterministicRelationID mirrors the backfill relation() ID derivation.
func deterministicRelationID(source, typ string, order int, target string) string {
	return DeterministicID("relation", fmt.Sprintf("%s:%s:%06d:%s", source, typ, order, target))
}

// transformVersionRelations rewrites a version snapshot's relations_json:
// expert_team_member entries rename to expert_team_expert, and deterministic
// relation IDs (derived from the type string by backfill) are re-keyed.
func transformVersionRelations(raw []byte, sourcePluginID string) ([]byte, bool, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return raw, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var entries []map[string]any
	if err := dec.Decode(&entries); err != nil {
		return nil, false, fmt.Errorf("invalid relations JSON: %w", err)
	}
	changed := false
	for _, entry := range entries {
		typ, _ := entry["relation_type"].(string)
		if typ != legacyTeamRelation {
			continue
		}
		entry["relation_type"] = contractTeamRelation
		changed = true
		id, _ := entry["relation_id"].(string)
		target, _ := entry["target_plugin_id"].(string)
		order := 0
		if number, ok := entry["sort_order"].(json.Number); ok {
			if parsed, err := number.Int64(); err == nil {
				order = int(parsed)
			}
		}
		if id != "" && id == deterministicRelationID(sourcePluginID, legacyTeamRelation, order, target) {
			entry["relation_id"] = deterministicRelationID(sourcePluginID, contractTeamRelation, order, target)
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func nullableJSON(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// hashColumn preserves the original NULL-vs-empty representation for hash
// columns that this pass did not assign a value to.
func hashColumn(value string, wasNull bool) any {
	if value == "" && wasNull {
		return nil
	}
	return value
}

// transformPackage converts one persisted package document to the contracts/v1
// layout. It is purely syntactic and idempotent:
//   - first-generation layouts still convert (expert/instruction.md ->
//     AGENTS.md, expert/mcp.json -> mcp.json, connector/config.json ->
//     descriptor + standard mcp.json);
//   - the embedded manifest.json attachment is removed for every type;
//   - expert_team packages collapse to a single AGENTS.md rendered by the
//     shared teamAgentsMarkdown (team/config.json is folded in and removed).
//
// Changed output is re-canonicalized with the lib encoder so persisted bytes
// match what a fresh backfill would emit.
// decodablePackage guards against committing a transformed package that only
// ComputePluginHash-validates but DecodePackage-rejects (duplicate attachment
// paths, a second AGENTS.md, a missing entry file). Such a row would be
// permanently uneditable through the API, which runs DecodePackage on every
// upsert — and migration is one-way, so we record a skip and leave the row
// rather than write an unrepairable package (P1-4).
func decodablePackage(pkg []byte, pluginType string) error {
	if len(strings.TrimSpace(string(pkg))) == 0 {
		return nil
	}
	_, err := libplugin.DecodePackage(libplugin.Type(pluginType), pkg)
	return err
}

func transformPackage(raw []byte, pluginType string, manifest []byte) ([]byte, []byte, bool, error) {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return raw, nil, false, nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, nil, false, fmt.Errorf("invalid package JSON: %w", err)
	}
	attachments, _ := doc["attachments"].([]any)
	changed := false
	if idx := attachmentIndex(attachments, "manifest.json"); idx >= 0 {
		attachments = append(attachments[:idx], attachments[idx+1:]...)
		doc["attachments"] = attachments
		changed = true
	}
	switch pluginType {
	case "skill":
		// Snapshot skills carry their document behind the ref.json pointer;
		// the contract entry file is a deterministic stub matching what a
		// fresh backfill emits.
		if attachmentIndex(attachments, "SKILL.md") < 0 {
			meta := manifestFields(manifest)
			content := entryMarkdown(meta.pluginName, "")
			attachments = append(attachments, map[string]any{
				"path":         "SKILL.md",
				"content_type": "raw",
				"mime_type":    "text/markdown",
				"raw_content":  content,
			})
			doc["attachments"] = attachments
			changed = true
		}
	case "expert":
		changed = renameAttachment(attachments, "expert/instruction.md", "AGENTS.md") || changed
		changed = renameAttachment(attachments, "expert/mcp.json", "mcp.json") || changed
		if attachmentIndex(attachments, "AGENTS.md") < 0 {
			meta := manifestFields(manifest)
			content := expertAgentsMarkdown(meta.pluginName, meta.description, "")
			attachments = append(attachments, map[string]any{
				"path":         "AGENTS.md",
				"content_type": "raw",
				"mime_type":    "text/markdown",
				"raw_content":  content,
			})
			doc["attachments"] = attachments
			changed = true
		}
	case "expert_team":
		changed = renameAttachment(attachments, "expert/instruction.md", "AGENTS.md") || changed
		if idx := attachmentIndex(attachments, "team/config.json"); idx >= 0 {
			meta := manifestFields(manifest)
			leader, strategies, dependencies, permission := teamConfigFields(attachments)
			content := teamAgentsMarkdown(meta.pluginName, meta.description, leader, strategies, dependencies, permission)
			attachments = append(attachments[:idx], attachments[idx+1:]...)
			entry := map[string]any{
				"path":         "AGENTS.md",
				"content_type": "raw",
				"mime_type":    "text/markdown",
				"raw_content":  content,
			}
			if existing := attachmentIndex(attachments, "AGENTS.md"); existing >= 0 {
				attachments[existing] = entry
			} else {
				attachments = append(attachments, entry)
			}
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
				return nil, nil, false, fmt.Errorf("invalid connector/config.json: %w", err)
			}
			mcpDocument, err := connectorMCPDocument(wrapper.Config, wrapper.Transport, meta.name)
			if err != nil {
				return nil, nil, false, err
			}
			mcpRaw, err := json.Marshal(mcpDocument)
			if err != nil {
				return nil, nil, false, err
			}
			attachments[idx] = map[string]any{
				"path":         "mcp.json",
				"content_type": "raw",
				"mime_type":    "application/json",
				"raw_content":  string(mcpRaw),
			}
			changed = true
		}
	}
	// Normalize legacy 1.0 shapes to the 2.0 contract the linked lib enforces:
	// raw attachments must NOT carry a derived content_size/content_hash, and a
	// storage attachment must NOT carry a host storage_uri (DecodePackage 2.0
	// rejects unknown fields) — its object key is split into the returned sidecar
	// so the row's attachment_keys_json can be populated. The $schema id advances
	// to the 2.0 generation. This runs BEFORE the no-op short-circuit so a row that
	// needs only normalization (raw size/hash or a storage_uri split, no rename)
	// is still migrated.
	keys := map[string]string{}
	for _, a := range attachments {
		m, ok := a.(map[string]any)
		if !ok {
			continue
		}
		switch ct, _ := m["content_type"].(string); ct {
		case "raw":
			if _, had := m["content_size"]; had {
				delete(m, "content_size")
				changed = true
			}
			if _, had := m["content_hash"]; had {
				delete(m, "content_hash")
				changed = true
			}
		case "storage":
			if uri, _ := m["storage_uri"].(string); uri != "" {
				if path, _ := m["path"].(string); path != "" {
					keys[path] = uri
				}
				delete(m, "storage_uri")
				changed = true
			}
		}
	}
	if s, _ := doc["$schema"].(string); s != "" && s != "cowork-plugin-package-2.0.json" {
		doc["$schema"] = "cowork-plugin-package-2.0.json"
		changed = true
	}
	if !changed {
		return raw, nil, false, nil
	}
	sort.SliceStable(attachments, func(i, j int) bool {
		left, _ := attachments[i].(map[string]any)
		right, _ := attachments[j].(map[string]any)
		lp, _ := left["path"].(string)
		rp, _ := right["path"].(string)
		return lp < rp
	})
	doc["attachments"] = attachments
	var keysJSON []byte
	if len(keys) > 0 {
		marshaledKeys, err := json.Marshal(keys)
		if err != nil {
			return nil, nil, false, err
		}
		keysJSON = marshaledKeys
	}
	marshaled, err := json.Marshal(doc)
	if err != nil {
		return nil, nil, false, err
	}
	out, err := libplugin.CanonicalJSON(marshaled)
	if err != nil {
		return nil, nil, false, err
	}
	return out, keysJSON, true, nil
}

// renameAttachment updates the path (content bytes and hash metadata stay). It
// refuses to rename onto an already-present target path, which would create two
// attachments with the same path (a duplicate the lib's DecodePackage rejects);
// callers treat a false return as "target already exists, leave it".
func renameAttachment(attachments []any, from, to string) bool {
	idx := attachmentIndex(attachments, from)
	if idx < 0 {
		return false
	}
	if attachmentIndex(attachments, to) >= 0 {
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

func teamConfigFields(attachments []any) (string, any, any, string) {
	idx := attachmentIndex(attachments, "team/config.json")
	if idx < 0 {
		return "", nil, nil, ""
	}
	entry, _ := attachments[idx].(map[string]any)
	content, _ := entry["raw_content"].(string)
	var cfg struct {
		Leader       string `json:"leader"`
		Strategies   any    `json:"strategies"`
		Dependencies any    `json:"dependencies"`
		Permission   string `json:"permission"`
	}
	_ = json.Unmarshal([]byte(content), &cfg)
	return cfg.Leader, cfg.Strategies, cfg.Dependencies, cfg.Permission
}
