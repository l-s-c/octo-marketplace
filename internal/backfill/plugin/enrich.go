package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

// enrichStamp is the deterministic timestamp for rows the enrich phase
// creates itself (connector categories have no legacy source timestamps);
// a constant keeps re-runs byte-identical.
var enrichStamp = time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC)

// EnrichCounts reports per-step enrichment work: Planned counts the gaps the
// current database still has, Applied counts rows changed by apply mode.
type EnrichCounts struct {
	ConnectorCategories int `json:"connector_categories"`
	CategoryPlacements  int `json:"category_placements"`
	PluginCategories    int `json:"plugin_categories"`
	PlacementCategories int `json:"placement_categories"`
	Icons               int `json:"icons"`
	ToolCounts          int `json:"tool_counts"`
	Metrics             int `json:"metrics"`
}

type EnrichReport struct {
	Mode       Mode         `json:"mode"`
	Planned    EnrichCounts `json:"planned"`
	Applied    EnrichCounts `json:"applied,omitempty"`
	Remaining  EnrichCounts `json:"remaining,omitempty"`
	Issues     []Issue      `json:"issues"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at"`
}

type enrichAction struct {
	// count selects the EnrichCounts field this action reports under.
	count func(*EnrichCounts) *int
	query string
	args  []any
}

type enrichPlan struct {
	actions []enrichAction
	issues  []Issue
}

// Enrich fills display data the deterministic plan phase deliberately leaves
// out so already-applied databases never conflict with the plan hash: legacy
// icons, connector categories (registered from the legacy enum and stamped
// onto plugins and placements), materialized connector tool counts, and a
// one-time copy of legacy resource_metrics onto plugin-keyed rows. Every step
// only touches rows still missing the value, so re-runs are no-ops.
func (r *Runner) Enrich(ctx context.Context, o Options) (EnrichReport, error) {
	if o.Mode == "" {
		o.Mode = ModeDryRun
	}
	if o.Mode != ModeDryRun && o.Mode != ModeApply && o.Mode != ModeVerify {
		return EnrichReport{}, fmt.Errorf("invalid mode %q", o.Mode)
	}
	rep := EnrichReport{Mode: o.Mode, StartedAt: r.now(), Issues: []Issue{}}
	p, err := r.buildEnrich(ctx)
	if err != nil {
		return EnrichReport{}, err
	}
	rep.Issues = append(rep.Issues, p.issues...)
	for _, action := range p.actions {
		*action.count(&rep.Planned)++
	}
	if o.Mode == ModeApply {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return EnrichReport{}, err
		}
		defer tx.Rollback()
		for _, action := range p.actions {
			res, err := tx.ExecContext(ctx, action.query, action.args...)
			if err != nil {
				return EnrichReport{}, fmt.Errorf("enrich apply: %w", err)
			}
			if n, err := res.RowsAffected(); err == nil && n > 0 {
				*action.count(&rep.Applied)++
			}
		}
		if err := tx.Commit(); err != nil {
			return EnrichReport{}, err
		}
	}
	if o.Mode != ModeDryRun {
		remainingPlan, err := r.buildEnrich(ctx)
		if err != nil {
			return EnrichReport{}, err
		}
		for _, action := range remainingPlan.actions {
			*action.count(&rep.Remaining)++
		}
	}
	rep.FinishedAt = r.now()
	return rep, nil
}

// buildEnrich reads the current database and produces only the statements
// whose target rows still miss the enriched value.
func (r *Runner) buildEnrich(ctx context.Context) (enrichPlan, error) {
	var p enrichPlan
	if err := r.enrichConnectorCategories(ctx, &p); err != nil {
		return p, err
	}
	if err := r.enrichIcons(ctx, &p); err != nil {
		return p, err
	}
	if err := r.enrichToolCounts(ctx, &p); err != nil {
		return p, err
	}
	if err := r.enrichMetrics(ctx, &p); err != nil {
		return p, err
	}
	return p, nil
}

// ConnectorCategoryID maps a legacy MCP category enum key onto its registered
// unified category so the API and web adapters can rely on one derivation.
func ConnectorCategoryID(key string) string { return DeterministicID("mcpcat", key) }

// connectorCategoryNames localizes the legacy MCP category slugs to the display
// names stored directly in plugin_categories — mirroring how skill categories
// carry their Chinese name from the legacy `categories` table. The web renders
// the stored name as-is (dynamic categories), so the display label lives in the
// DB rather than a client-side translation. Unknown slugs keep their raw key.
var connectorCategoryNames = map[string]string{
	"dev":          "开发工具",
	"data":         "数据服务",
	"search":       "搜索检索",
	"productivity": "效率协作",
	"ai":           "AI能力",
}

func connectorCategoryName(key string) string {
	if name, ok := connectorCategoryNames[key]; ok {
		return name
	}
	return key
}

func (r *Runner) enrichConnectorCategories(ctx context.Context, p *enrichPlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT id, category FROM mcp_servers WHERE deleted_at IS NULL ORDER BY id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byKey := map[string][]string{}
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return err
		}
		if key == "" {
			continue
		}
		byKey[key] = append(byKey[key], id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	keys := make([]string, 0, len(byKey))
	for key := range byKey {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for order, key := range keys {
		catID := ConnectorCategoryID(key)
		exists, err := r.rowExists(ctx, `SELECT COUNT(*) FROM plugin_categories WHERE category_id=?`, catID)
		if err != nil {
			return err
		}
		if !exists {
			// Store the localized display name directly (see connectorCategoryNames);
			// the web renders plugin_categories.name as-is.
			p.actions = append(p.actions, enrichAction{
				count: func(c *EnrichCounts) *int { return &c.ConnectorCategories },
				query: `INSERT INTO plugin_categories(category_id,name,icon_key,plugin_types_json,sort_order,status,created_at,updated_at) VALUES(?,?,?,?,?,1,?,?)`,
				args:  []any{catID, connectorCategoryName(key), "", `["connector"]`, order, enrichStamp, enrichStamp},
			})
		} else if name := connectorCategoryName(key); name != key {
			// The category already exists — likely enriched by an earlier run that
			// stored the raw slug as the name (before localization). Sync a stale
			// raw-slug name to the localized one; scoping the UPDATE to name=key keeps
			// it idempotent (a row already carrying the localized name is skipped) and
			// never clobbers an operator-renamed category.
			stale, err := r.rowExists(ctx, `SELECT COUNT(*) FROM plugin_categories WHERE category_id=? AND name=?`, catID, key)
			if err != nil {
				return err
			}
			if stale {
				p.actions = append(p.actions, enrichAction{
					count: func(c *EnrichCounts) *int { return &c.ConnectorCategories },
					query: `UPDATE plugin_categories SET name=?,updated_at=? WHERE category_id=? AND name=?`,
					args:  []any{name, enrichStamp, catID, key},
				})
			}
		}
		cpID := DeterministicID("category_placement", "default#connector#"+catID)
		exists, err = r.rowExists(ctx, `SELECT COUNT(*) FROM plugin_category_placements WHERE placement_id=?`, cpID)
		if err != nil {
			return err
		}
		if !exists {
			p.actions = append(p.actions, enrichAction{
				count: func(c *EnrichCounts) *int { return &c.CategoryPlacements },
				query: `INSERT INTO plugin_category_placements(placement_id,placement_code,plugin_type,category_id,visible,sort_order,created_at,updated_at) VALUES(?,?,?,?,1,?,?,?)`,
				args:  []any{cpID, "default", "connector", catID, order, enrichStamp, enrichStamp},
			})
		}
		for _, mcpID := range byKey[key] {
			pluginID := PluginID("connector", mcpID)
			missing, err := r.rowExists(ctx, `SELECT COUNT(*) FROM plugins WHERE plugin_id=? AND category_id IS NULL`, pluginID)
			if err != nil {
				return err
			}
			if missing {
				p.actions = append(p.actions, enrichAction{
					count: func(c *EnrichCounts) *int { return &c.PluginCategories },
					query: `UPDATE plugins SET category_id=? WHERE plugin_id=? AND category_id IS NULL`,
					args:  []any{catID, pluginID},
				})
			}
			missing, err = r.rowExists(ctx, `SELECT COUNT(*) FROM plugin_placements WHERE placement_code='default' AND plugin_id=? AND category_id IS NULL`, pluginID)
			if err != nil {
				return err
			}
			if missing {
				p.actions = append(p.actions, enrichAction{
					count: func(c *EnrichCounts) *int { return &c.PlacementCategories },
					query: `UPDATE plugin_placements SET category_id=? WHERE placement_code='default' AND plugin_id=? AND category_id IS NULL`,
					args:  []any{catID, pluginID},
				})
			}
		}
	}
	return nil
}

func (r *Runner) enrichIcons(ctx context.Context, p *enrichPlan) error {
	for _, source := range []struct {
		query  string
		family string
	}{
		{`SELECT id, icon FROM mcp_servers WHERE icon<>'' ORDER BY id`, "connector"},
		{`SELECT id, icon_url FROM skills WHERE icon_url<>'' ORDER BY id`, "skill"},
	} {
		rows, err := r.db.QueryContext(ctx, source.query)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id, icon string
			if err := rows.Scan(&id, &icon); err != nil {
				rows.Close()
				return err
			}
			pluginID := PluginID(source.family, id)
			missing, err := r.rowExists(ctx, `SELECT COUNT(*) FROM plugins WHERE plugin_id=? AND icon=''`, pluginID)
			if err != nil {
				rows.Close()
				return err
			}
			if missing {
				p.actions = append(p.actions, enrichAction{
					count: func(c *EnrichCounts) *int { return &c.Icons },
					query: `UPDATE plugins SET icon=? WHERE plugin_id=? AND icon=''`,
					args:  []any{icon, pluginID},
				})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	return nil
}

// enrichToolCounts recomputes tool_count from the stored plugin_json through
// the same rule the write path uses, so import- and backfill-created
// connectors converge on one definition.
func (r *Runner) enrichToolCounts(ctx context.Context, p *enrichPlan) error {
	rows, err := r.db.QueryContext(ctx, `SELECT plugin_id, plugin_json, tool_count FROM plugins WHERE plugin_type='connector' AND deleted_at IS NULL ORDER BY plugin_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var pkg []byte
		var stored int
		if err := rows.Scan(&id, &pkg, &stored); err != nil {
			return err
		}
		want := pluginsvc.ConnectorToolCount(json.RawMessage(pkg))
		if want == stored {
			continue
		}
		p.actions = append(p.actions, enrichAction{
			count: func(c *EnrichCounts) *int { return &c.ToolCounts },
			query: `UPDATE plugins SET tool_count=? WHERE plugin_id=? AND tool_count=?`,
			args:  []any{want, id, stored},
		})
	}
	return rows.Err()
}

// enrichMetrics copies legacy per-resource counters onto plugin-keyed rows
// once. The legacy rows stay untouched and frozen; after the web clients
// switch to resource_type "plugin" only the new rows accumulate.
func (r *Runner) enrichMetrics(ctx context.Context, p *enrichPlan) error {
	families := map[string]string{"skill": "skill", "expert": "expert", "squad": "expertteam"}
	rows, err := r.db.QueryContext(ctx, `SELECT resource_type, resource_id, view_count, download_count, install_count FROM resource_metrics WHERE resource_type IN ('skill','expert','squad') ORDER BY resource_type, resource_id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, id string
		var views, downloads, installs int64
		if err := rows.Scan(&typ, &id, &views, &downloads, &installs); err != nil {
			return err
		}
		pluginID := PluginID(families[typ], id)
		exists, err := r.rowExists(ctx, `SELECT COUNT(*) FROM plugins WHERE plugin_id=?`, pluginID)
		if err != nil {
			return err
		}
		if !exists {
			p.issues = append(p.issues, Issue{"info", "orphan_metrics", "resource_metrics", typ + ":" + id, "no migrated plugin row; counters left behind"})
			continue
		}
		migrated, err := r.rowExists(ctx, `SELECT COUNT(*) FROM resource_metrics WHERE resource_type='plugin' AND resource_id=?`, pluginID)
		if err != nil {
			return err
		}
		if migrated {
			// A plugin metrics row already exists — most likely a view/install in
			// the deploy window (API live before enrich) created it with zero
			// counters. Merging the legacy counts here would double-count on a
			// re-run (enrich is documented idempotent), so record a skip issue with
			// the un-copied totals instead of silently dropping them (P2). The
			// operator merges these manually if the legacy counts matter.
			if views > 0 || downloads > 0 || installs > 0 {
				p.issues = append(p.issues, Issue{"skip", "metrics_already_present", "resource_metrics",
					typ + ":" + id, fmt.Sprintf("plugin metrics row exists; legacy views=%d downloads=%d installs=%d not merged", views, downloads, installs)})
			}
			continue
		}
		p.actions = append(p.actions, enrichAction{
			count: func(c *EnrichCounts) *int { return &c.Metrics },
			query: `INSERT INTO resource_metrics(resource_type,resource_id,view_count,download_count,install_count) VALUES('plugin',?,?,?,?)`,
			args:  []any{pluginID, views, downloads, installs},
		})
	}
	return rows.Err()
}

func (r *Runner) rowExists(ctx context.Context, query string, args ...any) (bool, error) {
	var count int
	if err := r.db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
