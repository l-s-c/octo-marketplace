package plugin

import (
	"context"
	"database/sql"
	"errors"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/go-sql-driver/mysql"
)

func (r *Repo) ListAudits(ctx context.Context, scope Scope, pluginID string, limit, offset int) ([]model.PluginAuditLog, error) {
	if _, err := r.Get(ctx, scope, pluginID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `SELECT a.audit_log_id,a.plugin_id,a.action,a.operator_id,a.operator_name,a.request_id,a.before_hash,a.after_hash,a.manifest_snapshot_json,a.plugin_snapshot_json,a.remark,a.created_at
FROM plugin_audit_logs a JOIN plugins p ON p.plugin_id=a.plugin_id WHERE a.plugin_id=? AND p.deleted_at IS NULL AND `+visibilitySQL+` ORDER BY a.created_at DESC,a.audit_log_id DESC LIMIT ? OFFSET ?`, pluginID, scope.SpaceID, scope.CallerUID, limit, max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PluginAuditLog
	for rows.Next() {
		var a model.PluginAuditLog
		var before, after, remark sql.NullString
		var manifest, pkg []byte
		if err := rows.Scan(&a.ID, &a.PluginID, &a.Action, &a.OperatorID, &a.OperatorName, &a.RequestID, &before, &after, &manifest, &pkg, &remark, &a.CreatedAt); err != nil {
			return nil, err
		}
		a.BeforeHash = nullString(before)
		a.AfterHash = nullString(after)
		a.Remark = nullString(remark)
		a.ManifestSnapshot = cloneJSON(manifest)
		a.PluginSnapshot = cloneJSON(pkg)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *Repo) ListVersions(ctx context.Context, scope Scope, pluginID string, limit, offset int) ([]model.PluginVersion, error) {
	if _, err := r.Get(ctx, scope, pluginID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `SELECT v.version_id,v.plugin_id,v.version,v.manifest_json,v.plugin_json,v.manifest_hash,v.plugin_hash,v.relations_json,v.changelog,v.created_by,v.created_at
FROM plugin_versions v JOIN plugins p ON p.plugin_id=v.plugin_id WHERE v.plugin_id=? AND p.deleted_at IS NULL AND `+visibilitySQL+` ORDER BY v.created_at DESC,v.version_id DESC LIMIT ? OFFSET ?`, pluginID, scope.SpaceID, scope.CallerUID, limit, max(offset, 0))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PluginVersion
	for rows.Next() {
		var v model.PluginVersion
		var manifest, pkg, rels []byte
		var changelog sql.NullString
		if err := rows.Scan(&v.ID, &v.PluginID, &v.Version, &manifest, &pkg, &v.ManifestHash, &v.PluginHash, &rels, &changelog, &v.CreatedBy, &v.CreatedAt); err != nil {
			return nil, err
		}
		v.Manifest = cloneJSON(manifest)
		v.Package = cloneJSON(pkg)
		v.Relations = cloneJSON(rels)
		v.Changelog = nullString(changelog)
		out = append(out, v)
	}
	return out, rows.Err()
}

// PublishParams describes an immutable version and its replacement placements.
type PublishParams struct {
	PluginID, Version, CreatedBy, OperatorName, RequestID string
	Changelog                                             *string
	Relations                                             []byte
	Placements                                            []model.PluginPlacement
}

func (r *Repo) Publish(ctx context.Context, scope Scope, p PublishParams) (string, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	current, err := getOwnedForUpdate(ctx, tx, scope, p.PluginID)
	if err != nil {
		return "", err
	}
	now := r.now()
	versionID := r.id()
	_, err = tx.ExecContext(ctx, `INSERT INTO plugin_versions (version_id,plugin_id,version,manifest_json,plugin_json,manifest_hash,plugin_hash,relations_json,changelog,created_by,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, versionID, p.PluginID, p.Version, string(current.Manifest), string(current.Package), current.ManifestHash, current.PluginHash, string(p.Relations), p.Changelog, p.CreatedBy, now)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return "", ErrConflict
		}
		return "", err
	}
	_, err = tx.ExecContext(ctx, `DELETE pp FROM plugin_placements pp JOIN plugins p ON p.plugin_id=pp.plugin_id WHERE pp.plugin_id=? AND p.owner_uid=? AND p.space_id=? AND p.deleted_at IS NULL`, p.PluginID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return "", err
	}
	for _, x := range p.Placements {
		id := x.ID
		if id == "" {
			id = r.id()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO plugin_placements (placement_id,placement_code,plugin_id,category_id,visible,sort_order,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, id, x.PlacementCode, p.PluginID, x.CategoryID, x.Visible, x.SortOrder, now, now)
		if err != nil {
			return "", err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE plugins SET current_version_id=?,updated_at=? WHERE plugin_id=? AND owner_uid=? AND space_id=? AND deleted_at IS NULL`, versionID, now, p.PluginID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return "", err
	}
	if err = mustAffect(res); err != nil {
		return "", err
	}
	m := Mutation{OperatorID: p.CreatedBy, OperatorName: p.OperatorName, RequestID: p.RequestID}
	if err = insertAudit(ctx, tx, r.id(), now, *current, "publish", m, current.PluginHash, current.PluginHash); err != nil {
		return "", err
	}
	if err = tx.Commit(); err != nil {
		return "", err
	}
	return versionID, nil
}

func (r *Repo) ListPlacementCategories(ctx context.Context, scope Scope, placementCode string, typ model.PluginType) ([]model.PluginCategory, error) {
	// Scope arguments are deliberately part of this query even though categories are global: only categories backed by a Plugin visible to this caller/Space are returned.
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT c.category_id,c.name,c.icon_key,c.plugin_types_json,cp.sort_order,c.status,c.created_at,c.updated_at
FROM plugin_category_placements cp JOIN plugin_categories c ON c.category_id=cp.category_id JOIN plugin_placements pp ON pp.placement_code=cp.placement_code AND pp.category_id=c.category_id AND pp.visible=1 JOIN plugins p ON p.plugin_id=pp.plugin_id
WHERE cp.placement_code=? AND cp.plugin_type=? AND cp.visible=1 AND c.status=1 AND c.deleted_at IS NULL AND p.deleted_at IS NULL AND `+visibilitySQL+` ORDER BY cp.sort_order,c.category_id`, placementCode, typ, scope.SpaceID, scope.CallerUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PluginCategory
	for rows.Next() {
		var c model.PluginCategory
		var types []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.IconKey, &types, &c.SortOrder, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		c.PluginTypes = cloneJSON(types)
		out = append(out, c)
	}
	return out, rows.Err()
}
