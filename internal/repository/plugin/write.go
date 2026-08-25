package plugin

import (
	"context"
	"database/sql"
	"sort"

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

// RelationSync reports the outcome of synchronizing a Plugin's one-level
// relations to the submitted target state: an empty relation ID creates, a
// known ID with changed fields updates, and live IDs absent from the submitted
// list are soft-deleted. Relations carries the final rows with assigned IDs.
type RelationSync struct {
	Created   []string
	Updated   []string
	Deleted   []string
	Relations []model.PluginRelation
}

// Create atomically inserts current state, one-level relations, and an audit event.
func (r *Repo) Create(ctx context.Context, scope Scope, m Mutation) (*RelationSync, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	now := r.now()
	p := m.Plugin
	// Non-admin writes are pinned to the caller's own identity/Space; the admin
	// surface supplies owner/Space (and NULL Space for system rows) itself.
	if !scope.Admin {
		p.OwnerUID = scope.CallerUID
		p.SpaceID = &scope.SpaceID
	}
	if p.ID == "" {
		p.ID = r.id()
	}
	if err = lockPluginCategory(ctx, tx, p.CategoryID, p.Type); err != nil {
		return nil, err
	}
	if err = lockRelationTargets(ctx, tx, scope, p.Type, m.Relations); err != nil {
		return nil, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO plugins (plugin_id,plugin_name,plugin_type,category_id,tags_json,publisher,owner_uid,space_id,visibility,
creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,icon,tool_count,manifest_json,plugin_json,manifest_hash,plugin_hash,current_version_id,current_version,status,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, p.ID, p.Name, p.Type, p.CategoryID, string(p.Tags), p.Publisher, p.OwnerUID, p.SpaceID, p.Visibility, p.CreatorName, p.CreatedByType, p.CreatedByBotUID, p.CreatedByBotName, p.Icon, p.ToolCount, string(p.Manifest), string(p.Package), p.ManifestHash, p.PluginHash, p.CurrentVersionID, p.CurrentVersion, p.Status, now, now)
	if err != nil {
		return nil, wrapped("create", err)
	}
	created, err := insertRelations(ctx, tx, r.id, now, p.ID, m.OperatorID, m.Relations)
	if err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, r.id(), now, p, "create", m, "", p.PluginHash); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return &RelationSync{Created: created, Updated: []string{}, Deleted: []string{}, Relations: m.Relations}, nil
}

// Update atomically updates an owned Plugin, synchronizes its one-level
// relations to the submitted target state, and appends audit.
func (r *Repo) Update(ctx context.Context, scope Scope, m Mutation) (*RelationSync, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	before, err := getOwnedForUpdate(ctx, tx, scope, m.Plugin.ID)
	if err != nil {
		return nil, err
	}
	now := r.now()
	p := m.Plugin
	if p.Type != before.Type {
		return nil, ErrInvalidRelation
	}
	if err = lockPluginCategory(ctx, tx, p.CategoryID, p.Type); err != nil {
		return nil, err
	}
	if err = lockRelationTargets(ctx, tx, scope, p.Type, m.Relations); err != nil {
		return nil, err
	}
	// getOwnedForUpdate already proved existence under lock; RowsAffected reports
	// changed rows, so a byte-identical resubmit must not surface as not found.
	updWhere, updTail := `WHERE plugin_id=? AND owner_uid=? AND space_id=? AND deleted_at IS NULL`, []any{p.ID, scope.CallerUID, scope.SpaceID}
	if scope.Admin {
		updWhere, updTail = `WHERE plugin_id=? AND deleted_at IS NULL`, []any{p.ID}
	}
	updArgs := append([]any{p.Name, p.Type, p.CategoryID, string(p.Tags), p.Publisher, p.Visibility, p.Icon, p.ToolCount, string(p.Manifest), string(p.Package), p.ManifestHash, p.PluginHash, p.Status, now}, updTail...)
	_, err = tx.ExecContext(ctx, `UPDATE plugins SET plugin_name=?,plugin_type=?,category_id=?,tags_json=?,publisher=?,visibility=?,icon=?,tool_count=?,manifest_json=?,plugin_json=?,manifest_hash=?,plugin_hash=?,status=?,updated_at=?
`+updWhere, updArgs...)
	if err != nil {
		return nil, wrapped("update", err)
	}
	// Placements filter list pages by their own category copy; keep it in sync
	// with the current-state category so an updated Plugin doesn't keep
	// filtering under its old category until the next publish. A nil category
	// on the update leaves placement categories alone — publish owns clearing
	// per-placement configuration.
	if p.CategoryID != nil {
		_, err = tx.ExecContext(ctx, `UPDATE plugin_placements SET category_id=?,updated_at=? WHERE plugin_id=?`, p.CategoryID, now, p.ID)
		if err != nil {
			return nil, wrapped("update placements", err)
		}
	}
	sync, err := syncRelations(ctx, tx, r.id, now, p.ID, m.OperatorID, m.Relations)
	if err != nil {
		return nil, err
	}
	if err = insertAudit(ctx, tx, r.id(), now, p, "update", m, before.PluginHash, p.PluginHash); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return sync, nil
}

// Delete soft-deletes an owned Plugin, invalidates its outgoing relation edges,
// and appends an audit event. A live Plugin cannot be deleted while another
// live, active Plugin has a live, active incoming relation to it.
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
	if err = rejectLiveIncomingRelations(ctx, tx, pluginID); err != nil {
		return err
	}
	now := r.now()
	delWhere, delTail := `WHERE plugin_id=? AND owner_uid=? AND space_id=? AND deleted_at IS NULL`, []any{now, now, pluginID, scope.CallerUID, scope.SpaceID}
	if scope.Admin {
		delWhere, delTail = `WHERE plugin_id=? AND deleted_at IS NULL`, []any{now, now, pluginID}
	}
	res, err := tx.ExecContext(ctx, `UPDATE plugins SET deleted_at=?,updated_at=? `+delWhere, delTail...)
	if err != nil {
		return err
	}
	if err = mustAffect(res); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE plugin_relations SET deleted_at=?,updated_at=?
WHERE source_plugin_id=? AND deleted_at IS NULL`, now, now, pluginID)
	if err != nil {
		return err
	}
	m := Mutation{OperatorID: operatorID, OperatorName: operatorName, RequestID: requestID, Remark: remark}
	if err = insertAudit(ctx, tx, r.id(), now, *before, "delete", m, before.PluginHash, ""); err != nil {
		return err
	}
	return tx.Commit()
}

func rejectLiveIncomingRelations(ctx context.Context, tx *sql.Tx, pluginID string) error {
	rows, err := tx.QueryContext(ctx, `SELECT r.relation_id FROM plugin_relations r
JOIN plugins source ON source.plugin_id=r.source_plugin_id
WHERE r.target_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL
AND source.deleted_at IS NULL AND source.status=1
ORDER BY r.relation_id FOR UPDATE`, pluginID)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return ErrConflict
	}
	return rows.Err()
}

func lockPluginCategory(ctx context.Context, tx *sql.Tx, categoryID *string, pluginType model.PluginType) error {
	if categoryID == nil {
		return nil
	}
	var id string
	err := tx.QueryRowContext(ctx, `SELECT category_id FROM plugin_categories WHERE category_id=? AND status=1 AND deleted_at IS NULL AND JSON_CONTAINS(plugin_types_json,JSON_QUOTE(?),'$') FOR UPDATE`, *categoryID, pluginType).Scan(&id)
	if err == sql.ErrNoRows {
		return ErrInvalidCategory
	}
	return err
}

func getOwnedForUpdate(ctx context.Context, tx *sql.Tx, scope Scope, id string) (*model.Plugin, error) {
	var row *sql.Row
	if scope.Admin {
		row = tx.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins p WHERE p.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL FOR UPDATE`, id)
	} else {
		row = tx.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins p WHERE p.plugin_id=? AND p.owner_uid=? AND p.space_id=? AND p.status=1 AND p.deleted_at IS NULL FOR UPDATE`, id, scope.CallerUID, scope.SpaceID)
	}
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
		relWhere, relArgs := visibilitySQL, []any{relation.TargetPluginID, scope.SpaceID, scope.CallerUID}
		if scope.Admin {
			relWhere, relArgs = "1=1", []any{relation.TargetPluginID}
		}
		row := tx.QueryRowContext(ctx, `SELECT p.plugin_type FROM plugins p WHERE p.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL AND `+relWhere+` FOR UPDATE`, relArgs...)
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

// validPersistedRelation mirrors the octo-plugin-lib endpoint matrix exactly
// (expert_team_expert: expert_team -> expert, expert_skill: expert -> skill,
// expert_connector: expert -> connector) as the last write-path gate.
func validPersistedRelation(relationType string, sourceType, targetType model.PluginType) bool {
	switch relationType {
	case "expert_team_expert":
		return sourceType == model.PluginTypeExpertTeam && targetType == model.PluginTypeExpert
	case "expert_skill":
		return sourceType == model.PluginTypeExpert && targetType == model.PluginTypeSkill
	case "expert_connector":
		return sourceType == model.PluginTypeExpert && targetType == model.PluginTypeConnector
	default:
		return false
	}
}

func insertRelations(ctx context.Context, tx *sql.Tx, newID func() string, now interface{}, source, creator string, rels []model.PluginRelation) ([]string, error) {
	created := make([]string, 0, len(rels))
	for i := range rels {
		if rels[i].ID == "" {
			rels[i].ID = newID()
		}
		if err := insertRelationRow(ctx, tx, now, source, creator, rels[i]); err != nil {
			return nil, err
		}
		created = append(created, rels[i].ID)
	}
	return created, nil
}

func insertRelationRow(ctx context.Context, tx *sql.Tx, now interface{}, source, creator string, x model.PluginRelation) error {
	var data any
	if len(x.Data) > 0 {
		data = string(x.Data)
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO plugin_relations (relation_id,source_plugin_id,target_plugin_id,relation_type,sort_order,relation_json,status,created_by,created_at,updated_at) VALUES (?,?,?,?,?,?,?,?,?,?)`, x.ID, source, x.TargetPluginID, x.Type, x.SortOrder, data, x.Status, creator, now, now)
	if err != nil {
		return wrapped("replace relations", err)
	}
	return nil
}

// syncRelations reconciles the submitted target state against the live edge set
// under row locks: empty IDs insert, known IDs update when any field differs,
// and live IDs missing from the submission are soft-deleted. A non-empty ID
// that does not match a live edge of this Plugin is rejected.
func syncRelations(ctx context.Context, tx *sql.Tx, newID func() string, now interface{}, source, creator string, desired []model.PluginRelation) (*RelationSync, error) {
	type liveEdge struct {
		target    string
		typ       string
		sortOrder int
		data      sql.NullString
		status    int
	}
	rows, err := tx.QueryContext(ctx, `SELECT relation_id,target_plugin_id,relation_type,sort_order,relation_json,status FROM plugin_relations
WHERE source_plugin_id=? AND deleted_at IS NULL ORDER BY relation_id FOR UPDATE`, source)
	if err != nil {
		return nil, wrapped("load relations", err)
	}
	live := map[string]liveEdge{}
	for rows.Next() {
		var id string
		var edge liveEdge
		if err := rows.Scan(&id, &edge.target, &edge.typ, &edge.sortOrder, &edge.data, &edge.status); err != nil {
			rows.Close()
			return nil, err
		}
		live[id] = edge
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	sync := &RelationSync{Created: []string{}, Updated: []string{}, Deleted: []string{}}
	for i := range desired {
		rel := &desired[i]
		if rel.ID == "" {
			rel.ID = newID()
			if err := insertRelationRow(ctx, tx, now, source, creator, *rel); err != nil {
				return nil, err
			}
			sync.Created = append(sync.Created, rel.ID)
			continue
		}
		edge, ok := live[rel.ID]
		if !ok {
			return nil, ErrInvalidRelation
		}
		delete(live, rel.ID)
		sameData := (len(rel.Data) == 0 && !edge.data.Valid) || (len(rel.Data) > 0 && edge.data.Valid && edge.data.String == string(rel.Data))
		if edge.target == rel.TargetPluginID && edge.typ == rel.Type && edge.sortOrder == rel.SortOrder && edge.status == rel.Status && sameData {
			continue
		}
		var data any
		if len(rel.Data) > 0 {
			data = string(rel.Data)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_relations SET target_plugin_id=?,relation_type=?,sort_order=?,relation_json=?,status=?,updated_at=?
WHERE relation_id=? AND source_plugin_id=? AND deleted_at IS NULL`, rel.TargetPluginID, rel.Type, rel.SortOrder, data, rel.Status, now, rel.ID, source); err != nil {
			return nil, wrapped("update relation", err)
		}
		sync.Updated = append(sync.Updated, rel.ID)
	}
	leftover := make([]string, 0, len(live))
	for id := range live {
		leftover = append(leftover, id)
	}
	sort.Strings(leftover)
	for _, id := range leftover {
		if _, err := tx.ExecContext(ctx, `UPDATE plugin_relations SET deleted_at=?,updated_at=? WHERE relation_id=? AND source_plugin_id=? AND deleted_at IS NULL`, now, now, id, source); err != nil {
			return nil, wrapped("delete relation", err)
		}
		sync.Deleted = append(sync.Deleted, id)
	}
	sync.Relations = desired
	return sync, nil
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
