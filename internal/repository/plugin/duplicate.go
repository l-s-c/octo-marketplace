package plugin

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

const (
	maxDuplicateDepth = 16
	maxDuplicateNodes = 500
)

type duplicateNode struct {
	plugin    model.Plugin
	relations []model.PluginRelation
}

// DuplicateGraph atomically deep-copies the source's live relation graph and
// appends an audit event for the new root. Every node must be visible in Scope.
// Cycles, excessive depth, and excessive node counts are rejected before any
// duplicate rows are inserted.
func (r *Repo) DuplicateGraph(ctx context.Context, scope Scope, sourcePluginID string, duplicate model.Plugin, audit Mutation) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	nodes, order, err := r.loadDuplicateGraph(ctx, tx, scope, sourcePluginID)
	if err != nil {
		return err
	}
	// Complete secret preflight before generating IDs or issuing any write.
	for _, sourceID := range order {
		node := nodes[sourceID].plugin
		if node.Type == model.PluginTypeConnector {
			if err := rejectPersistedConnectorSecrets(node.Manifest, node.Package); err != nil {
				return err
			}
		}
	}

	now := r.now()
	ids := make(map[string]string, len(order))
	for _, sourceID := range order {
		if sourceID == sourcePluginID {
			if duplicate.ID == "" {
				duplicate.ID = r.id()
			}
			ids[sourceID] = duplicate.ID
		} else {
			ids[sourceID] = r.id()
		}
	}

	copiesBySource := make(map[string]model.Plugin, len(order))
	for _, sourceID := range order {
		copy := nodes[sourceID].plugin
		if sourceID == sourcePluginID {
			copy = duplicate
		}
		copy.ID = ids[sourceID]
		copy.OwnerUID = scope.CallerUID
		copy.SpaceID = &scope.SpaceID
		copy.Visibility = model.PluginVisibilityPrivate
		// Every copied node is a new resource created by the current caller. Do
		// not preserve a source descendant's creator/bot provenance or status.
		copy.CreatorName = duplicate.CreatorName
		copy.CreatedByType = duplicate.CreatedByType
		copy.CreatedByBotUID = duplicate.CreatedByBotUID
		copy.CreatedByBotName = duplicate.CreatedByBotName
		copy.Status = 1
		copy.CurrentVersionID = nil
		copy.DeletedAt = nil
		if err := insertDuplicatePlugin(ctx, tx, copy, now); err != nil {
			return err
		}
		copiesBySource[sourceID] = copy
	}

	for _, sourceID := range order {
		relations := nodes[sourceID].relations
		copies := make([]model.PluginRelation, 0, len(relations))
		for _, relation := range relations {
			relation.ID = ""
			relation.SourcePluginID = ids[sourceID]
			relation.TargetPluginID = ids[relation.TargetPluginID]
			relation.CreatedBy = scope.CallerUID
			relation.DeletedAt = nil
			copies = append(copies, relation)
		}
		if err := insertRelations(ctx, tx, r.id, now, ids[sourceID], scope.CallerUID, copies); err != nil {
			return err
		}
	}

	if audit.OperatorID == "" {
		audit.OperatorID = scope.CallerUID
	}
	for _, sourceID := range order {
		copy := copiesBySource[sourceID]
		audit.Plugin = copy
		if err := insertAudit(ctx, tx, r.id(), now, copy, "duplicate", audit, "", copy.PluginHash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (r *Repo) loadDuplicateGraph(ctx context.Context, tx *sql.Tx, scope Scope, rootID string) (map[string]duplicateNode, []string, error) {
	nodes := make(map[string]duplicateNode)
	visiting := make(map[string]bool)
	order := make([]string, 0)

	var visit func(string, int) error
	visit = func(pluginID string, depth int) error {
		if depth > maxDuplicateDepth {
			return fmt.Errorf("duplicate graph exceeds maximum depth %d", maxDuplicateDepth)
		}
		if visiting[pluginID] {
			return fmt.Errorf("duplicate graph contains a cycle at %q", pluginID)
		}
		if _, ok := nodes[pluginID]; ok {
			return nil
		}
		if len(nodes) >= maxDuplicateNodes {
			return fmt.Errorf("duplicate graph exceeds maximum node count %d", maxDuplicateNodes)
		}

		plugin, relations, err := loadDuplicateNode(ctx, tx, scope, pluginID)
		if err != nil {
			return err
		}
		visiting[pluginID] = true
		nodes[pluginID] = duplicateNode{plugin: *plugin, relations: relations}
		order = append(order, pluginID)
		for _, relation := range relations {
			if err := visit(relation.TargetPluginID, depth+1); err != nil {
				return err
			}
		}
		visiting[pluginID] = false
		return nil
	}
	if err := visit(rootID, 0); err != nil {
		return nil, nil, err
	}
	return nodes, order, nil
}

func loadDuplicateNode(ctx context.Context, tx *sql.Tx, scope Scope, pluginID string) (*model.Plugin, []model.PluginRelation, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+pluginColumns+` FROM plugins p
WHERE p.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL+` FOR UPDATE`, pluginID, scope.SpaceID, scope.CallerUID)
	plugin, err := scanPlugin(row)
	if err == sql.ErrNoRows {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}

	// Read every live edge from the already scoped source. Targets are loaded
	// separately with the same Scope so an inaccessible dependency rejects the
	// duplicate instead of being silently omitted from the copy.
	rows, err := tx.QueryContext(ctx, `SELECT r.relation_id,r.source_plugin_id,r.target_plugin_id,r.relation_type,r.sort_order,
 r.relation_json,r.status,r.created_by,r.created_at,r.updated_at,r.deleted_at
FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.source_plugin_id
WHERE r.source_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL+`
ORDER BY r.sort_order,r.relation_id`, pluginID, scope.SpaceID, scope.CallerUID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var relations []model.PluginRelation
	for rows.Next() {
		var relation model.PluginRelation
		var data []byte
		var deleted sql.NullTime
		if err := rows.Scan(&relation.ID, &relation.SourcePluginID, &relation.TargetPluginID, &relation.Type, &relation.SortOrder, &data, &relation.Status, &relation.CreatedBy, &relation.CreatedAt, &relation.UpdatedAt, &deleted); err != nil {
			return nil, nil, err
		}
		relation.Data = cloneJSON(data)
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return plugin, relations, nil
}

func insertDuplicatePlugin(ctx context.Context, tx *sql.Tx, plugin model.Plugin, now interface{}) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO plugins (plugin_id,plugin_name,plugin_type,category_id,tags_json,publisher,owner_uid,space_id,visibility,
creator_name,created_by_type,created_by_bot_uid,created_by_bot_name,manifest_json,plugin_json,manifest_hash,plugin_hash,current_version_id,status,created_at,updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, plugin.ID, plugin.Name, plugin.Type, plugin.CategoryID, string(plugin.Tags), plugin.Publisher, plugin.OwnerUID, plugin.SpaceID, plugin.Visibility, plugin.CreatorName, plugin.CreatedByType, plugin.CreatedByBotUID, plugin.CreatedByBotName, string(plugin.Manifest), string(plugin.Package), plugin.ManifestHash, plugin.PluginHash, nil, plugin.Status, now, now)
	return wrapped("duplicate current state", err)
}
