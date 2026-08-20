package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// ListFilter restricts a scoped Plugin listing. Sort accepts newest (default),
// oldest, name, or placement; callers validate unsupported values.
type ListFilter struct {
	PlacementCode string
	Type          model.PluginType
	CategoryID    string
	Tag           string
	Keyword       string
	Mine          bool
	Sort          string
	Limit         int
	Offset        int
}

func (r *Repo) Get(ctx context.Context, scope Scope, pluginID string) (*model.Plugin, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins p
WHERE p.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL, pluginID, scope.SpaceID, scope.CallerUID)
	p, err := scanPlugin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// List returns one page and the total number of rows matching the same scoped filters.
func (r *Repo) List(ctx context.Context, scope Scope, f ListFilter) ([]model.Plugin, int64, error) {
	from, where, args := buildListQuery(scope, f)
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT p.plugin_id) `+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	queryArgs := append(append([]any(nil), args...), limit, max(f.Offset, 0))
	// The placement join can yield one row per (placement_code, category) pair
	// for the same plugin; collapse to one row so the page matches the total.
	group := ``
	if f.PlacementCode != "" {
		group = ` GROUP BY p.plugin_id`
	}
	q := `SELECT ` + pluginColumns + from + where + group + listOrder(f) + ` LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Plugin
	for rows.Next() {
		p, err := scanPlugin(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func buildListQuery(scope Scope, f ListFilter) (string, string, []any) {
	from := ` FROM plugins p`
	args := []any{}
	if f.PlacementCode != "" {
		from += ` JOIN plugin_placements pp ON pp.plugin_id=p.plugin_id AND pp.placement_code=? AND pp.visible=1`
		args = append(args, f.PlacementCode)
	}
	where := ` WHERE p.status=1 AND p.deleted_at IS NULL AND ` + visibilitySQL
	args = append(args, scope.SpaceID, scope.CallerUID)
	if f.Type != "" {
		where += ` AND p.plugin_type=?`
		args = append(args, f.Type)
	}
	if f.CategoryID != "" {
		if f.PlacementCode != "" {
			where += ` AND pp.category_id=?`
		} else {
			where += ` AND p.category_id=?`
		}
		args = append(args, f.CategoryID)
	}
	if f.Tag != "" {
		where += ` AND JSON_CONTAINS(p.tags_json, JSON_QUOTE(?), '$')`
		args = append(args, f.Tag)
	}
	if f.Keyword != "" {
		where += ` AND p.plugin_name LIKE ? ESCAPE '!'`
		args = append(args, "%"+escapeLike(f.Keyword)+"%")
	}
	if f.Mine {
		where += ` AND p.owner_uid=? AND p.space_id=?`
		args = append(args, scope.CallerUID, scope.SpaceID)
	}
	return from, where, args
}

func listOrder(f ListFilter) string {
	switch f.Sort {
	case "oldest":
		return ` ORDER BY p.created_at ASC,p.plugin_id ASC`
	case "name":
		return ` ORDER BY p.plugin_name ASC,p.plugin_id ASC`
	case "placement":
		if f.PlacementCode != "" {
			return ` ORDER BY MIN(pp.sort_order) ASC,p.plugin_id ASC`
		}
	}
	return ` ORDER BY p.created_at DESC,p.plugin_id DESC`
}

func escapeLike(s string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return r.Replace(s)
}

// GetWithRelations returns only live one-level relations whose target is also visible.
func (r *Repo) GetWithRelations(ctx context.Context, scope Scope, pluginID string) (*model.Plugin, []model.PluginRelation, error) {
	p, err := r.Get(ctx, scope, pluginID)
	if err != nil {
		return nil, nil, err
	}
	rows, err := r.db.QueryContext(ctx, `SELECT r.relation_id,r.source_plugin_id,r.target_plugin_id,p.plugin_type,r.relation_type,r.sort_order,
 r.relation_json,r.status,r.created_by,r.created_at,r.updated_at,r.deleted_at
FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id
WHERE r.source_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL+`
ORDER BY r.sort_order,r.relation_id`, pluginID, scope.SpaceID, scope.CallerUID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var rels []model.PluginRelation
	for rows.Next() {
		var x model.PluginRelation
		var data []byte
		var deleted sql.NullTime
		if err := rows.Scan(&x.ID, &x.SourcePluginID, &x.TargetPluginID, &x.TargetPluginType, &x.Type, &x.SortOrder, &data, &x.Status, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &deleted); err != nil {
			return nil, nil, err
		}
		x.Data = cloneJSON(data)
		if deleted.Valid {
			x.DeletedAt = &deleted.Time
		}
		rels = append(rels, x)
	}
	return p, rels, rows.Err()
}

func scanPlugin(s interface{ Scan(...any) error }) (*model.Plugin, error) {
	var p model.Plugin
	var category, space, botUID, botName, version sql.NullString
	var tags, manifest, pkg []byte
	var deleted sql.NullTime
	err := s.Scan(&p.ID, &p.Name, &p.Type, &category, &tags, &p.Publisher, &p.OwnerUID, &space, &p.Visibility, &p.CreatorName, &p.CreatedByType, &botUID, &botName, &manifest, &pkg, &p.ManifestHash, &p.PluginHash, &version, &p.Status, &p.CreatedAt, &p.UpdatedAt, &deleted)
	if err != nil {
		return nil, err
	}
	p.CategoryID = nullString(category)
	p.SpaceID = nullString(space)
	p.CreatedByBotUID = nullString(botUID)
	p.CreatedByBotName = nullString(botName)
	p.CurrentVersionID = nullString(version)
	if deleted.Valid {
		p.DeletedAt = &deleted.Time
	}
	p.Tags = cloneJSON(tags)
	p.Manifest = cloneJSON(manifest)
	p.Package = cloneJSON(pkg)
	return &p, nil
}
func cloneJSON(v []byte) []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v...)
}
func nullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	x := v.String
	return &x
}
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
func wrapped(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("plugin repository %s: %w", op, err)
}
