package plugin

import (
	"context"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// Admin plugin-category persistence. Unlike the placement-scoped catalog read
// (ListPlacementCategories), these are the marketplace-admin CRUD operations
// over the plugin_categories taxonomy table, with no Space scoping.

// ListAdminCategories returns every live category whose plugin_types include
// typ, with a live-plugin reference count, ordered by sort_order.
func (r *Repo) ListAdminCategories(ctx context.Context, typ model.PluginType) ([]model.PluginCategory, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.category_id,c.name,c.icon_key,c.plugin_types_json,c.sort_order,c.status,c.created_at,c.updated_at,
 (SELECT COUNT(*) FROM plugins p WHERE p.category_id=c.category_id AND p.status=1 AND p.deleted_at IS NULL) AS plugin_count
FROM plugin_categories c
WHERE c.status=1 AND c.deleted_at IS NULL AND JSON_CONTAINS(c.plugin_types_json, JSON_QUOTE(?), '$')
ORDER BY c.sort_order, c.category_id`, typ)
	if err != nil {
		return nil, wrapped("list admin categories", err)
	}
	defer rows.Close()
	var out []model.PluginCategory
	for rows.Next() {
		var c model.PluginCategory
		if err := rows.Scan(&c.ID, &c.Name, &c.IconKey, &c.PluginTypes, &c.SortOrder, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.PluginCount); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
