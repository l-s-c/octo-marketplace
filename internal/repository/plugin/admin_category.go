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

// CreateCategory inserts a taxonomy category. pluginTypesJSON is a JSON array of
// plugin type strings the category applies to.
func (r *Repo) CreateCategory(ctx context.Context, id, name, iconKey string, pluginTypesJSON []byte, sortOrder int) error {
	now := r.now()
	_, err := r.db.ExecContext(ctx, `INSERT INTO plugin_categories (category_id,name,icon_key,plugin_types_json,sort_order,status,created_at,updated_at)
VALUES (?,?,?,?,?,1,?,?)`, id, name, iconKey, string(pluginTypesJSON), sortOrder, now, now)
	if err != nil {
		return wrapped("create category", err)
	}
	return nil
}

// UpdateCategory updates a live category's mutable fields by id.
func (r *Repo) UpdateCategory(ctx context.Context, id, name, iconKey string, pluginTypesJSON []byte, sortOrder int) error {
	res, err := r.db.ExecContext(ctx, `UPDATE plugin_categories SET name=?,icon_key=?,plugin_types_json=?,sort_order=?,updated_at=?
WHERE category_id=? AND status=1 AND deleted_at IS NULL`, name, iconKey, string(pluginTypesJSON), sortOrder, r.now(), id)
	if err != nil {
		return wrapped("update category", err)
	}
	return mustAffect(res)
}

// DeleteCategory soft-deletes a category, refusing while live plugins still
// reference it (ErrCategoryInUse) so a delete can never orphan a listing.
func (r *Repo) DeleteCategory(ctx context.Context, id string) (int, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugins WHERE category_id=? AND status=1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&count); err != nil {
		return 0, err
	}
	if count > 0 {
		return count, ErrCategoryInUse
	}
	res, err := tx.ExecContext(ctx, `UPDATE plugin_categories SET status=0,deleted_at=?,updated_at=? WHERE category_id=? AND status=1 AND deleted_at IS NULL`, r.now(), r.now(), id)
	if err != nil {
		return 0, wrapped("delete category", err)
	}
	if err := mustAffect(res); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return 0, nil
}
