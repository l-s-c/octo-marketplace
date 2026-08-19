package plugin

import (
	"context"
	"database/sql"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// Mutation contains current state, replacement relations, and append-only audit metadata.
type Mutation struct {
	Plugin       model.Plugin
	Relations    []model.PluginRelation
	OperatorID   string
	OperatorName string
	RequestID    string
	Remark       *string
}

// Create atomically inserts current state, one-level relations, and an audit event.
func (r *Repo) Create(ctx context.Context, scope Scope, m Mutation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := r.now()
	p := m.Plugin
	p.OwnerUID = scope.CallerUID
	p.SpaceID = &scope.SpaceID
	if p.ID == "" {
		p.ID = r.id()
	}
	if err = lockRelationTargets(ctx, tx, scope, p.Type, m.Relations); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugins (plugin_id,plugin_name,plugin_type,category_id,tags_json,publisher,owner_uid,space_id,visibility,
creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,manifest_json,plugin_json,manifest_hash,plugin_hash,current_version_id,status,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.Name, p.Type, p.CategoryID, string(p.Tags), p.Publisher, p.OwnerUID, p.SpaceID, p.Visibility, p.CreatorName, p.CreatedByType, p.CreatedByBotUID, p.CreatedByBotName, string(p.Manifest), string(p.Package), p.ManifestHash, p.PluginHash, p.CurrentVersionID, p.Status, now, now)
	if err != nil {
		return wrapped("create", err)
	}
	if err = insertRelations(ctx, tx, r.id, now, p.ID, m.OperatorID, m.Relations); err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, r.id(), now, p, "create", m, "", p.PluginHash); err != nil {
		return err
	}
	return tx.Commit()
}

// Update atomically updates an owned Plugin, replaces its one-level relations, and appends audit.
func (r *Repo) Update(ctx context.Context, scope Scope, m Mutation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := getOwnedForUpdate(ctx, tx, scope, m.Plugin.ID)
	if err != nil {
		return err
	}
	now := r.now()
	p := m.Plugin
	if p.Type != before.Type {
		return ErrInvalidRelation
	}
	if err = lockRelationTargets(ctx, tx, scope, p.Type, m.Relations); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE plugins SET plugin_name=?,plugin_type=?,category_id=?,tags_json=?,publisher=?,visibility=?,creator_name=?,created_by_type=?,created_by_bot_uid=?,created_by_bot_name=?,manifest_json=?,plugin_json=?,manifest_hash=?,plugin_hash=?,status=?,updated_at=?
WHERE plugin_id=? AND owner_uid=? AND space_id=? AND deleted_at IS NULL`, p.Name, p.Type, p.CategoryID, string(p.Tags), p.Publisher, p.Visibility, p.CreatorName, p.CreatedByType, p.CreatedByBotUID, p.CreatedByBotName, string(p.Manifest), string(p.Package), p.ManifestHash, p.PluginHash, p.Status, now, p.ID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return wrapped("update", err)
	}
	if err = mustAffect(res); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE plugin_relations r JOIN plugins p ON p.plugin_id=r.source_plugin_id
SET r.deleted_at=?,r.updated_at=? WHERE r.source_plugin_id=? AND p.owner_uid=? AND p.space_id=? AND p.deleted_at IS NULL AND r.deleted_at IS NULL`, now, now, p.ID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return err
	}
	if err = insertRelations(ctx, tx, r.id, now, p.ID, m.OperatorID, m.Relations); err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, r.id(), now, p, "update", m, before.PluginHash, p.PluginHash); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete soft-deletes an owned Plugin, invalidates its relation edges, and appends audit.
func (r *Repo) Delete(ctx context.Context, scope Scope, pluginID, operatorID, operatorName, requestID string, remark *string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := getOwnedForUpdate(ctx, tx, scope, pluginID)
	if err != nil {
		return err
	}
	now := r.now()
	res, err := tx.ExecContext(ctx, `UPDATE plugins SET deleted_at=?,updated_at=? WHERE plugin_id=? AND owner_uid=? AND space_id=? AND deleted_at IS NULL`, now, now, pluginID, scope.CallerUID, scope.SpaceID)
	if err != nil {
		return err
	}
	if err = mustAffect(res); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE plugin_relations r JOIN plugins p ON p.plugin_id=? SET r.deleted_at=?,r.updated_at=?
WHERE (r.source_plugin_id=? OR r.target_plugin_id=?) AND p.owner_uid=? AND p.space_id=? AND p.deleted_at=? AND r.deleted_at IS NULL`, pluginID, now, now, pluginID, pluginID, scope.CallerUID, scope.SpaceID, now)
	if err != nil {
		return err
	}
	m := Mutation{OperatorID: operatorID, OperatorName: operatorName, RequestID: requestID, Remark: remark}
	if err = insertAudit(ctx, tx, r.id(), now, *before, "delete", m, before.PluginHash, ""); err != nil {
		return err
	}
	return tx.Commit()
}

func getOwnedForUpdate(ctx context.Context, tx *sql.Tx, scope Scope, id string) (*model.Plugin, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins p WHERE p.plugin_id=? AND p.owner_uid=? AND p.space_id=? AND p.deleted_at IS NULL FOR UPDATE`, id, scope.CallerUID, scope.SpaceID)
	p, err := scanPlugin(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return p, err
}
func mustAffect(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// lockRelationTargets revalidates every edge at the persistence boundary in the
// mutation transaction. The visibility predicate prevents cross-scope edges,
// and FOR UPDATE prevents a target from being deleted between validation and
// insertion.
func lockRelationTargets(ctx context.Context, tx *sql.Tx, scope Scope, sourceType model.PluginType, relations []model.PluginRelation) error {
	seen := make(map[string]struct{}, len(relations))
	for _, relation := range relations {
		if relation.TargetPluginID == "" {
			return ErrInvalidRelation
		}
		key := relation.Type + "\x00" + relation.TargetPluginID
		if _, exists := seen[key]; exists {
			return ErrInvalidRelation
		}
		seen[key] = struct{}{}
		row := tx.QueryRowContext(ctx, `SELECT p.plugin_type FROM plugins p WHERE p.plugin_id=? AND p.deleted_at IS NULL AND `+visibilitySQL+` FOR UPDATE`, relation.TargetPluginID, scope.SpaceID, scope.CallerUID)
		var targetType model.PluginType
		if err := row.Scan(&targetType); err != nil {
			if err == sql.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		if !validPersistedRelation(relation.Type, sourceType, targetType) {
			return ErrInvalidRelation
		}
	}
	return nil
}

func validPersistedRelation(relationType string, sourceType, targetType model.PluginType) bool {
	switch relationType {
	case "expert_team_member":
		return sourceType == model.PluginTypeExpertTeam && targetType == model.PluginTypeExpert
	case "expert_skill":
		return (sourceType == model.PluginTypeExpert || sourceType == model.PluginTypeExpertTeam) && targetType == model.PluginTypeSkill
	case "plugin_dependency":
		validSource := sourceType == model.PluginTypeExpert || sourceType == model.PluginTypeExpertTeam || sourceType == model.PluginTypeConnector
		return validSource && (targetType == model.PluginTypeSkill || targetType == model.PluginTypeConnector)
	default:
		return false
	}
}

func insertRelations(ctx context.Context, tx *sql.Tx, newID func() string, now interface{}, source, creator string, rels []model.PluginRelation) error {
	for _, x := range rels {
		id := x.ID
		if id == "" {
			id = newID()
		}
		var data any
		if len(x.Data) > 0 {
			data = string(x.Data)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO plugin_relations (relation_id,source_plugin_id,target_plugin_id,relation_type,sort_order,relation_json,status,created_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, id, source, x.TargetPluginID, x.Type, x.SortOrder, data, x.Status, creator, now, now)
		if err != nil {
			return wrapped("replace relations", err)
		}
	}
	return nil
}
func insertAudit(ctx context.Context, tx *sql.Tx, auditID string, now interface{}, p model.Plugin, action string, m Mutation, before, after string) error {
	var bh, ah any
	if before != "" {
		bh = before
	}
	if after != "" {
		ah = after
	}
	var manifest, pkg any
	if len(p.Manifest) > 0 {
		manifest = string(p.Manifest)
	}
	if len(p.Package) > 0 {
		pkg = string(p.Package)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO plugin_audit_logs (audit_log_id,plugin_id,action,operator_id,operator_name,request_id,before_hash,after_hash,manifest_snapshot_json,plugin_snapshot_json,remark,created_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, auditID, p.ID, action, m.OperatorID, m.OperatorName, m.RequestID, bh, ah, manifest, pkg, m.Remark, now)
	return wrapped("append audit", err)
}
