package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/go-sql-driver/mysql"
)

// VersionExists reports whether an immutable version row already exists for the
// plugin. It authorizes the caller through Get first (same visibility scope as
// ListVersions), so it never discloses versions of a plugin the caller cannot
// see. Used to pre-flight an import re-publish before mutating the document.
func (r *Repo) VersionExists(ctx context.Context, scope Scope, pluginID, version string) (bool, error) {
	if _, err := r.Get(ctx, scope, pluginID); err != nil {
		return false, err
	}
	var one int
	err := r.db.QueryRowContext(ctx, `SELECT 1 FROM plugin_versions WHERE plugin_id=? AND version=? LIMIT 1`, pluginID, version).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (r *Repo) ListVersions(ctx context.Context, scope Scope, pluginID string, limit, offset int) ([]model.PluginVersion, int64, error) {
	if _, err := r.Get(ctx, scope, pluginID); err != nil {
		return nil, 0, err
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_versions v JOIN plugins p ON p.plugin_id=v.plugin_id WHERE v.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL, pluginID, scope.SpaceID, scope.CallerUID).Scan(&total); err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, `SELECT v.version_id,v.plugin_id,v.version,v.manifest_json,v.plugin_json,v.attachment_keys_json,v.manifest_hash,v.plugin_hash,v.relations_json,v.changelog,v.created_by,v.created_at
FROM plugin_versions v JOIN plugins p ON p.plugin_id=v.plugin_id WHERE v.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL+` ORDER BY v.created_at DESC,v.version_id DESC LIMIT ? OFFSET ?`, pluginID, scope.SpaceID, scope.CallerUID, limit, max(offset, 0))
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.PluginVersion
	for rows.Next() {
		v, err := scanPluginVersion(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *v)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := r.redactVersionRelations(ctx, scope, out); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

// redactVersionRelations drops, from each immutable version snapshot, any
// relation whose target the reading caller cannot currently see. The snapshot is
// denormalized JSON captured at the PUBLISHER's visibility, so a public plugin
// relating to a same-Space private target would otherwise leak that target's id,
// relation type, sort order and data to a cross-Space caller — the current-state
// read (GetWithRelations) already filters this way. An unparseable snapshot is
// redacted to empty (fail closed).
func (r *Repo) redactVersionRelations(ctx context.Context, scope Scope, versions []model.PluginVersion) error {
	parsed := make([][]map[string]any, len(versions))
	targets := map[string]struct{}{}
	for i := range versions {
		if len(bytes.TrimSpace(versions[i].Relations)) == 0 {
			continue
		}
		var rels []map[string]any
		if err := json.Unmarshal(versions[i].Relations, &rels); err != nil {
			versions[i].Relations = json.RawMessage("[]")
			continue
		}
		parsed[i] = rels
		for _, rel := range rels {
			if id, ok := rel["target_plugin_id"].(string); ok && id != "" {
				targets[id] = struct{}{}
			}
		}
	}
	if len(targets) == 0 {
		return nil
	}
	visible, err := r.visibleTargetIDs(ctx, scope, targets)
	if err != nil {
		return err
	}
	for i := range versions {
		if parsed[i] == nil {
			continue
		}
		kept := make([]map[string]any, 0, len(parsed[i]))
		for _, rel := range parsed[i] {
			id, _ := rel["target_plugin_id"].(string)
			if _, ok := visible[id]; ok {
				kept = append(kept, rel)
			}
		}
		raw, err := json.Marshal(kept)
		if err != nil {
			return err
		}
		versions[i].Relations = raw
	}
	return nil
}

// visibleTargetIDs returns the subset of ids the caller may currently see, under
// the same visibility predicate as the catalog read (admin scope sees all).
func (r *Repo) visibleTargetIDs(ctx context.Context, scope Scope, ids map[string]struct{}) (map[string]struct{}, error) {
	idList := make([]any, 0, len(ids))
	for id := range ids {
		idList = append(idList, id)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(idList)), ",")
	where := visibilitySQL
	args := append([]any{}, idList...)
	if scope.Admin {
		where = "1=1"
	} else {
		args = append(args, scope.SpaceID, scope.CallerUID)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT p.plugin_id FROM plugins p WHERE p.plugin_id IN (`+placeholders+`) AND p.status=1 AND p.deleted_at IS NULL AND `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	visible := make(map[string]struct{}, len(idList))
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		visible[id] = struct{}{}
	}
	return visible, rows.Err()
}

func scanPluginVersion(s interface{ Scan(...any) error }) (*model.PluginVersion, error) {
	var v model.PluginVersion
	var manifest, pkg, attachKeys, rels []byte
	var changelog sql.NullString
	if err := s.Scan(&v.ID, &v.PluginID, &v.Version, &manifest, &pkg, &attachKeys, &v.ManifestHash, &v.PluginHash, &rels, &changelog, &v.CreatedBy, &v.CreatedAt); err != nil {
		return nil, err
	}
	v.Manifest = cloneJSON(manifest)
	v.Package = cloneJSON(pkg)
	v.AttachmentKeys = cloneJSON(attachKeys)
	v.Relations = cloneJSON(rels)
	v.Changelog = nullString(changelog)
	return &v, nil
}

// PublishParams describes an immutable version and its replacement placements.
type PublishParams struct {
	PluginID, Version, CreatedBy, OperatorName, RequestID string
	Changelog                                             *string
	Placements                                            []model.PluginPlacement
}

// Publish atomically snapshots locked current state and live visible relations,
// replaces placements, advances current_version_id, and returns that exact row.
func (r *Repo) Publish(ctx context.Context, scope Scope, p PublishParams) (*model.PluginVersion, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := getOwnedForUpdate(ctx, tx, scope, p.PluginID)
	if err != nil {
		return nil, err
	}
	relations, err := loadPublishRelations(ctx, tx, scope, p.PluginID)
	if err != nil {
		return nil, err
	}
	for i := range relations {
		relations[i].SourcePluginType = current.Type
	}
	if err = lockPublishPlacementCategories(ctx, tx, current.Type, p.Placements); err != nil {
		return nil, err
	}
	relationJSON, err := json.Marshal(relations)
	if err != nil {
		return nil, err
	}
	now := r.now()
	version := &model.PluginVersion{ID: r.id(), PluginID: p.PluginID, PluginType: current.Type, Version: p.Version, Manifest: cloneJSON(current.Manifest), Package: cloneJSON(current.Package), AttachmentKeys: cloneJSON(current.AttachmentKeys), ManifestHash: current.ManifestHash, PluginHash: current.PluginHash, Relations: cloneJSON(relationJSON), Changelog: p.Changelog, CreatedBy: p.CreatedBy, CreatedAt: now}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugin_versions (version_id,plugin_id,version,manifest_json,plugin_json,attachment_keys_json,manifest_hash,plugin_hash,relations_json,changelog,created_by,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, version.ID, version.PluginID, version.Version, string(version.Manifest), string(version.Package), jsonColumn(version.AttachmentKeys), version.ManifestHash, version.PluginHash, string(version.Relations), version.Changelog, version.CreatedBy, version.CreatedAt)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return nil, ErrConflict
		}
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `DELETE pp FROM plugin_placements pp JOIN plugins p ON p.plugin_id=pp.plugin_id WHERE pp.plugin_id=? AND p.owner_uid=? AND p.space_id=? AND p.deleted_at IS NULL`, p.PluginID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return nil, err
	}
	for _, x := range p.Placements {
		id := x.ID
		if id == "" {
			id = r.id()
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO plugin_placements (placement_id,placement_code,plugin_id,category_id,visible,sort_order,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?)`, id, x.PlacementCode, p.PluginID, x.CategoryID, x.Visible, x.SortOrder, now, now)
		if err != nil {
			var me *mysql.MySQLError
			if errors.As(err, &me) && me.Number == 1062 {
				return nil, ErrConflict
			}
			return nil, err
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE plugins SET current_version_id=?,current_version=?,updated_at=? WHERE plugin_id=? AND owner_uid=? AND space_id=? AND deleted_at IS NULL`, version.ID, version.Version, now, p.PluginID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return nil, err
	}
	if err = mustAffect(res); err != nil {
		return nil, err
	}
	m := Mutation{OperatorID: p.CreatedBy, OperatorName: p.OperatorName, RequestID: p.RequestID}
	if err = insertAudit(ctx, tx, r.id(), now, *current, "publish", m, current.PluginHash, current.PluginHash); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return version, nil
}

func loadPublishRelations(ctx context.Context, tx *sql.Tx, scope Scope, pluginID string) ([]model.PluginRelation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.relation_id,r.source_plugin_id,r.target_plugin_id,p.plugin_type,r.relation_type,r.sort_order,r.relation_json,r.status,r.created_by,r.created_at,r.updated_at,r.deleted_at
FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id
WHERE r.source_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL+`
ORDER BY r.sort_order,r.relation_id FOR UPDATE`, pluginID, scope.SpaceID, scope.CallerUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	relations := make([]model.PluginRelation, 0)
	for rows.Next() {
		var relation model.PluginRelation
		var data []byte
		var deleted sql.NullTime
		if err := rows.Scan(&relation.ID, &relation.SourcePluginID, &relation.TargetPluginID, &relation.TargetPluginType, &relation.Type, &relation.SortOrder, &data, &relation.Status, &relation.CreatedBy, &relation.CreatedAt, &relation.UpdatedAt, &deleted); err != nil {
			return nil, err
		}
		relation.Data = cloneJSON(data)
		relations = append(relations, relation)
	}
	return relations, rows.Err()
}

func lockPublishPlacementCategories(ctx context.Context, tx *sql.Tx, pluginType model.PluginType, placements []model.PluginPlacement) error {
	for _, placement := range placements {
		if placement.CategoryID == nil {
			continue
		}
		var categoryID string
		err := tx.QueryRowContext(ctx, `SELECT c.category_id FROM plugin_categories c
JOIN plugin_category_placements cp ON cp.category_id=c.category_id
WHERE c.category_id=? AND c.status=1 AND c.deleted_at IS NULL
AND JSON_CONTAINS(c.plugin_types_json,JSON_QUOTE(?))
AND cp.placement_code=? AND cp.plugin_type=? AND cp.visible=1
FOR UPDATE`, *placement.CategoryID, pluginType, placement.PlacementCode, pluginType).Scan(&categoryID)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInvalidPlacement
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *Repo) ListPlacementCategories(ctx context.Context, scope Scope, placementCode string, typ model.PluginType) ([]model.PluginCategory, error) {
	// Categories are placement configuration: every active category registered
	// for this placement and plugin type is returned, backed by plugins or not.
	// Scope only shapes plugin_count, which tallies published plugins visible to
	// this caller/Space so counts never leak cross-Space existence.
	rows, err := r.db.QueryContext(ctx, `SELECT c.category_id,c.name,c.icon_key,c.plugin_types_json,cp.sort_order,c.status,c.created_at,c.updated_at,
(SELECT COUNT(DISTINCT p.plugin_id) FROM plugin_placements pp JOIN plugins p ON p.plugin_id=pp.plugin_id
 WHERE pp.placement_code=cp.placement_code AND pp.category_id=c.category_id AND pp.visible=1
 AND p.plugin_type=? AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL+`) AS plugin_count
FROM plugin_category_placements cp JOIN plugin_categories c ON c.category_id=cp.category_id
WHERE cp.placement_code=? AND cp.plugin_type=? AND cp.visible=1 AND c.status=1 AND c.deleted_at IS NULL ORDER BY cp.sort_order,c.category_id`, typ, scope.SpaceID, scope.CallerUID, placementCode, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.PluginCategory
	for rows.Next() {
		var c model.PluginCategory
		var types []byte
		if err := rows.Scan(&c.ID, &c.Name, &c.IconKey, &types, &c.SortOrder, &c.Status, &c.CreatedAt, &c.UpdatedAt, &c.PluginCount); err != nil {
			return nil, err
		}
		c.PluginTypes = cloneJSON(types)
		out = append(out, c)
	}
	return out, rows.Err()
}
