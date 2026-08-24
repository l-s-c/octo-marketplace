// Admin plugin operations for the marketplace-admin surface (/api/v1/admin/plugins).
// These run behind the admin role gate and operate cross-Space with no ownership
// check (repo Scope.Admin). System connectors follow the MCP convention
// (visibility=system, NULL Space); admin skills follow the legacy skill-admin
// convention (visibility=public, empty global Space). Callers reach these only
// through the admin-gated routes, so the route gate is the authorization; the
// service does not re-derive a Space from the caller.

package plugin

import (
	"context"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

// adminGlobalSpace is the Space admin skills live in — empty, matching the
// legacy skill-admin GlobalTagSpaceID convention (public, space-less).
const adminGlobalSpace = ""

func adminScope(caller Caller) pluginrepo.Scope {
	return pluginrepo.Scope{CallerUID: caller.UID, Admin: true}
}

// AdminList lists plugins of one type across all Spaces, optionally narrowed to
// a visibility class (e.g. system connectors, public skills).
func (s *Service) AdminList(ctx context.Context, caller Caller, typ model.PluginType, visibility model.PluginVisibility, p ListParams) ([]model.Plugin, int64, error) {
	if !validPluginType(typ) {
		return nil, 0, ErrInvalidRequest
	}
	if p.Limit < 0 || p.Limit > maxListLimit || p.Offset < 0 {
		return nil, 0, ErrInvalidRequest
	}
	tags, err := normalizeListTags(p.Tags)
	if err != nil {
		return nil, 0, err
	}
	items, total, err := s.repo.List(ctx, adminScope(caller), pluginrepo.ListFilter{
		Type: typ, Visibility: visibility, AllSpaces: true,
		CategoryID: p.CategoryID, Tags: tags, Keyword: p.Keyword, Sort: p.Sort, Limit: p.Limit, Offset: p.Offset,
	})
	if err != nil {
		return nil, 0, mapStoreError(err)
	}
	if err := s.fillMemberCounts(ctx, items); err != nil {
		return nil, 0, mapStoreError(err)
	}
	for i := range items {
		items[i].IconURL = s.resolveIcon(ctx, items[i].Icon)
	}
	return items, total, nil
}

// AdminDetail returns any plugin's full detail by id, ignoring Space scope.
func (s *Service) AdminDetail(ctx context.Context, caller Caller, pluginID string, includeRelations bool) (*Detail, error) {
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	p, rels, err := s.repo.GetWithRelations(ctx, adminScope(caller), storageID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	p.IconURL = s.resolveIcon(ctx, p.Icon)
	if !includeRelations {
		rels = []model.PluginRelation{}
	} else {
		for i := range rels {
			rels[i].SourcePluginType = p.Type
		}
	}
	return &Detail{Plugin: p, Relations: rels}, nil
}

// adminEffectiveWrite fixes the caller identity and per-type visibility/Space so
// buildWrite mints the admin conventions: system connectors (NULL Space) and
// public skills (empty global Space). Returns the built plugin + relations.
func (s *Service) adminEffectiveWrite(ctx context.Context, caller Caller, pluginID string, req WriteRequest) (*model.Plugin, []model.PluginRelation, error) {
	eff := caller
	eff.IsSystemAdmin = true // admins may mint system visibility
	eff.SpaceID = adminGlobalSpace
	switch req.Type {
	case model.PluginTypeConnector:
		req.Visibility = model.PluginVisibilitySystem
	case model.PluginTypeSkill, model.PluginTypeExpert, model.PluginTypeExpertTeam:
		req.Visibility = model.PluginVisibilityPublic
	default:
		return nil, nil, ErrInvalidRequest
	}
	p, rels, err := s.buildWrite(ctx, eff, pluginID, req, s.now())
	if err != nil {
		return nil, nil, err
	}
	// System rows live outside the Space model (space_id=NULL); public admin
	// rows stay in the empty global Space.
	if req.Type == model.PluginTypeConnector {
		p.SpaceID = nil
	}
	return p, rels, nil
}

// AdminCreate mints a new admin plugin (system connector or public skill).
func (s *Service) AdminCreate(ctx context.Context, caller Caller, req WriteRequest) (*Detail, error) {
	p, rels, err := s.adminEffectiveWrite(ctx, caller, "", req)
	if err != nil {
		return nil, err
	}
	p.ID = s.id()
	for i := range rels {
		rels[i].SourcePluginID = p.ID
	}
	audit := s.audit(caller, p.ID, "create", nil, p, s.now())
	sync, err := s.repo.Create(ctx, adminScope(caller), mutation(*p, rels, audit))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels, RelationResult: relationResult(sync)}, nil
}

// AdminUpdate updates any admin plugin by id, regardless of owner/Space.
func (s *Service) AdminUpdate(ctx context.Context, caller Caller, pluginID string, req WriteRequest) (*Detail, error) {
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	old, _, err := s.repo.GetWithRelations(ctx, adminScope(caller), storageID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if req.Type != old.Type {
		return nil, ErrInvalidRequest
	}
	p, rels, err := s.adminEffectiveWrite(ctx, caller, storageID, req)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.CurrentVersionID = old.CreatedAt, old.CurrentVersionID
	p.CreatorName, p.CreatedByType = old.CreatorName, old.CreatedByType
	p.CreatedByBotUID, p.CreatedByBotName = old.CreatedByBotUID, old.CreatedByBotName
	p.SpaceID = old.SpaceID // preserve the row's existing Space on update
	for i := range rels {
		rels[i].SourcePluginID = storageID
	}
	audit := s.audit(caller, storageID, "update", old, p, s.now())
	sync, err := s.repo.Update(ctx, adminScope(caller), mutation(*p, rels, audit))
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels, RelationResult: relationResult(sync)}, nil
}

// AdminDelete soft-deletes any plugin by id, regardless of owner/Space.
func (s *Service) AdminDelete(ctx context.Context, caller Caller, pluginID string) error {
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return err
	}
	old, _, err := s.repo.GetWithRelations(ctx, adminScope(caller), storageID)
	if err != nil {
		return mapStoreError(err)
	}
	audit := s.audit(caller, storageID, "delete", old, nil, s.now())
	return mapStoreError(s.repo.Delete(ctx, adminScope(caller), storageID, audit.OperatorID, audit.OperatorName, audit.RequestID, audit.Remark))
}
