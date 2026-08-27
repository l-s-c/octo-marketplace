package plugin

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// Admin plugin-category persistence. Unlike the placement-scoped catalog read
// (ListPlacementCategories), these are the marketplace-admin CRUD operations
// over the plugin_categories taxonomy table, with no Space scoping.

// ListAdminCategories returns every live category whose plugin_types include
// typ, with a live-plugin reference count for that SAME type, ordered by
// sort_order. The count is type-scoped (a category shared across plugin types
// must not tally another type's rows) and excludes embedded rows (bundled
// skills / squad members), matching the catalog lists' is_embedded=0 filter.
func (r *Repo) ListAdminCategories(ctx context.Context, typ model.PluginType) ([]model.PluginCategory, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT c.category_id,c.name,c.icon_key,c.plugin_types_json,c.sort_order,c.status,c.created_at,c.updated_at,
 (SELECT COUNT(*) FROM plugins p WHERE p.category_id=c.category_id AND p.plugin_type=? AND p.is_embedded=0 AND p.status=1 AND p.deleted_at IS NULL) AS plugin_count
FROM plugin_categories c
WHERE c.status=1 AND c.deleted_at IS NULL AND JSON_CONTAINS(c.plugin_types_json, JSON_QUOTE(?), '$')
ORDER BY c.sort_order, c.category_id`, typ, typ)
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

// CreateCategory inserts a new taxonomy row. The caller pre-assigns category_id;
// status (active) and the created/updated timestamps are stamped here.
func (r *Repo) CreateCategory(ctx context.Context, c model.PluginCategory) error {
	now := r.now()
	if _, err := r.db.ExecContext(ctx, `INSERT INTO plugin_categories (category_id,name,icon_key,plugin_types_json,sort_order,status,created_at,updated_at)
VALUES (?,?,?,?,?,1,?,?)`, c.ID, c.Name, c.IconKey, string(c.PluginTypes), c.SortOrder, now, now); err != nil {
		return wrapped("create category", err)
	}
	return nil
}

// UpdateCategory mutates the editable fields of a live category, returning
// ErrNotFound when no live row carries the id. A type-NARROWING update (dropping
// a plugin type the category currently serves) is refused with ErrConflict while
// a live, non-deleted plugin still references this category under a dropped type:
// otherwise those rows would be stranded (lockPluginCategory then fails every
// future write for them, matching DeleteCategory's refusal). The row lock, the
// per-dropped-type reference count, and the update run in one transaction (the
// category row is locked FOR UPDATE first) so a plugin cannot adopt the category
// under a dropped type between the count and the update.
func (r *Repo) UpdateCategory(ctx context.Context, c model.PluginCategory) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var currentTypes []byte
	err = tx.QueryRowContext(ctx, `SELECT plugin_types_json FROM plugin_categories WHERE category_id=? AND deleted_at IS NULL FOR UPDATE`, c.ID).Scan(&currentTypes)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return wrapped("lock category", err)
	}
	dropped, err := droppedPluginTypes(currentTypes, c.PluginTypes)
	if err != nil {
		return err
	}
	for _, typ := range dropped {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugins WHERE category_id=? AND plugin_type=? AND status=1 AND deleted_at IS NULL`, c.ID, typ).Scan(&count); err != nil {
			return wrapped("count category plugins", err)
		}
		if count > 0 {
			return ErrConflict
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE plugin_categories SET name=?,icon_key=?,plugin_types_json=?,sort_order=?,updated_at=?
WHERE category_id=? AND deleted_at IS NULL`, c.Name, c.IconKey, string(c.PluginTypes), c.SortOrder, r.now(), c.ID)
	if err != nil {
		return wrapped("update category", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return wrapped("update category", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// droppedPluginTypes returns the plugin types present in the category's current
// type set but absent from the requested one — the narrowing an in-use category
// may not apply.
func droppedPluginTypes(current, next json.RawMessage) ([]model.PluginType, error) {
	var cur, nxt []model.PluginType
	if err := json.Unmarshal(current, &cur); err != nil {
		return nil, wrapped("decode category types", err)
	}
	if err := json.Unmarshal(next, &nxt); err != nil {
		return nil, wrapped("decode category types", err)
	}
	keep := make(map[model.PluginType]struct{}, len(nxt))
	for _, t := range nxt {
		keep[t] = struct{}{}
	}
	var dropped []model.PluginType
	for _, t := range cur {
		if _, ok := keep[t]; !ok {
			dropped = append(dropped, t)
		}
	}
	return dropped, nil
}

// DeleteCategory soft-deletes a live category (status=0, deleted_at stamped). It
// refuses (ErrConflict) while any live plugin still references the category, so
// the taxonomy row never disappears out from under an in-use plugin. ErrNotFound
// when no live row carries the id. The row lock, the reference count, and the
// soft-delete run in one transaction (the category row is locked FOR UPDATE
// first) so a plugin cannot adopt the category between the count and the delete.
func (r *Repo) DeleteCategory(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var locked string
	err = tx.QueryRowContext(ctx, `SELECT category_id FROM plugin_categories WHERE category_id=? AND deleted_at IS NULL FOR UPDATE`, id).Scan(&locked)
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	if err != nil {
		return wrapped("lock category", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugins WHERE category_id=? AND status=1 AND deleted_at IS NULL`, id).Scan(&count); err != nil {
		return wrapped("count category plugins", err)
	}
	if count > 0 {
		return ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE plugin_categories SET status=0, deleted_at=? WHERE category_id=? AND deleted_at IS NULL`, r.now(), id); err != nil {
		return wrapped("delete category", err)
	}
	return tx.Commit()
}
