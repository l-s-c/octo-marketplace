// Admin plugin operations for the marketplace-admin surface (/api/v1/admin/plugins).
// These run behind the admin role gate and operate cross-Space with no ownership
// check (repo Scope.Admin). System connectors follow the MCP convention
// (visibility=system, NULL Space); admin skills/experts follow the skill-admin
// convention (visibility=system, empty global Space). Callers reach these only
// through the admin-gated routes, so the route gate is the authorization; the
// service does not re-derive a Space from the caller.

package plugin

import (
	"context"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

// adminGlobalSpace is the Space admin skills live in — empty, matching the
// legacy skill-admin GlobalTagSpaceID convention (global, space-less).
const adminGlobalSpace = ""

func adminScope(caller Caller) pluginrepo.Scope {
	return pluginrepo.Scope{CallerUID: caller.UID, Admin: true}
}

// validAdminListSort accepts the empty default plus the fixed sort whitelist the
// repository's listOrder recognizes, excluding "placement" (which needs a
// placement code the admin list never supplies).
func validAdminListSort(sort string) bool {
	switch sort {
	case "", "newest", "oldest", "updated", "name", "views", "installs", "downloads", "comprehensive":
		return true
	default:
		return false
	}
}

// AdminList lists plugins of one type across all Spaces, optionally narrowed to
// a visibility class (e.g. system connectors, system skills).
func (s *Service) AdminList(ctx context.Context, caller Caller, typ model.PluginType, visibility model.PluginVisibility, p ListParams) ([]model.Plugin, int64, error) {
	if !validPluginType(typ) {
		return nil, 0, ErrInvalidRequest
	}
	// Optional filters, but when supplied they must be known values — an unknown
	// visibility must not silently return an empty page, nor an unknown sort
	// silently fall back to the default (Q12').
	if visibility != "" && !validVisibility(visibility, true) {
		return nil, 0, ErrInvalidRequest
	}
	if !validAdminListSort(p.Sort) {
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

// conventionVisibility is the visibility an admin CREATE stamps: every plugin
// type an admin publishes is platform-global, so all of them get `system`
// (the unified "全平台可见" value). Update never forces a visibility — it
// preserves the row's existing one (see AdminUpdate) so a metadata edit cannot
// publish a tenant-private plugin.
func conventionVisibility(typ model.PluginType) (model.PluginVisibility, bool) {
	switch typ {
	case model.PluginTypeConnector, model.PluginTypeSkill, model.PluginTypeExpert, model.PluginTypeExpertTeam:
		return model.PluginVisibilitySystem, true
	default:
		return "", false
	}
}

// adminEffectiveWrite fixes the caller identity and Space and stamps the given
// visibility so buildWrite mints the admin conventions: system connectors (NULL
// Space) and system skills (empty global Space) on create; the row's preserved
// visibility on update. effectiveSpace is the Space storage-attachment keys are
// namespaced under and must be the ROW's real Space — for an AdminUpdate of a
// tenant-owned row that is old.SpaceID, not the empty global Space, otherwise
// CanonicalizeDocuments rejects every storage attachment (safeObjectSegment
// rejects the empty Space) and admins cannot edit any skill whose package spilled
// a file to object storage (P1-1). Returns the built plugin + relations.
func (s *Service) adminEffectiveWrite(ctx context.Context, caller Caller, pluginID string, req WriteRequest, visibility model.PluginVisibility, effectiveSpace string) (*model.Plugin, []model.PluginRelation, error) {
	if !conventionType(req.Type) {
		return nil, nil, ErrInvalidRequest
	}
	eff := caller
	eff.IsSystemAdmin = true // admins may mint/preserve system visibility
	eff.SpaceID = effectiveSpace
	// A preserved legacy `public` visibility is normalized to the unified `system`
	// global value so the write revalidates and the row stops carrying the retired
	// value (validVisibility rejects `public`).
	req.Visibility = model.NormalizeLegacyVisibility(visibility)
	p, rels, err := s.buildWrite(ctx, eff, pluginID, req, s.now(), true)
	if err != nil {
		return nil, nil, err
	}
	// System rows live outside the Space model (space_id=NULL); system admin
	// rows stay in the empty global Space.
	if req.Type == model.PluginTypeConnector {
		p.SpaceID = nil
	}
	return p, rels, nil
}

func conventionType(typ model.PluginType) bool {
	switch typ {
	case model.PluginTypeConnector, model.PluginTypeSkill, model.PluginTypeExpert, model.PluginTypeExpertTeam:
		return true
	default:
		return false
	}
}

// defaultMarketPlacement is the visible "default"-placement row an admin create
// auto-attaches so the plugin appears in the tenant market without a publish. It
// carries no registered-category requirement — a plain visible placement (with
// the plugin's own category, which may be nil) is enough for the market list.
func defaultMarketPlacement(categoryID *string) model.PluginPlacement {
	return model.PluginPlacement{PlacementCode: "default", CategoryID: categoryID, Visible: true, SortOrder: 0}
}

// AdminCreate mints a new admin plugin (system connector or system skill).
func (s *Service) AdminCreate(ctx context.Context, caller Caller, req WriteRequest) (*Detail, error) {
	visibility, ok := conventionVisibility(req.Type)
	if !ok {
		return nil, ErrInvalidRequest
	}
	p, rels, err := s.adminEffectiveWrite(ctx, caller, "", req, visibility, adminGlobalSpace)
	if err != nil {
		return nil, err
	}
	p.ID = s.id()
	for i := range rels {
		rels[i].SourcePluginID = p.ID
	}
	audit := s.audit(caller, p.ID, "create", nil, p, s.now())
	m := mutation(*p, rels, audit)
	m.SnapshotVersion = true
	// Admin creates auto-attach a default visible placement so the plugin surfaces
	// in the tenant market immediately (the market always lists the "default"
	// placement); publish is not required. The placement copies the plugin's
	// category (may be nil — a null-category visible placement still lists).
	m.Placements = []model.PluginPlacement{defaultMarketPlacement(p.CategoryID)}
	sync, err := s.repo.Create(ctx, adminScope(caller), m)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels, RelationResult: relationResult(sync)}, nil
}

// AdminUpdate updates any admin plugin by id, regardless of owner/Space. It
// PRESERVES the row's existing visibility, Space, and owner: an admin metadata
// edit must never force-publish a plugin (A1) — a tenant-private row stays
// private — and the owner/creator provenance is immutable.
//
// TODO: AdminUpdate does not (re)attach a default placement — a plugin that was
// never auto-placed (e.g. created before this change) stays out of the market
// until publish. Out of scope here; revisit if metadata-only edits must surface.
func (s *Service) AdminUpdate(ctx context.Context, caller Caller, pluginID string, req WriteRequest) (*Detail, error) {
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	old, _, err := s.repo.GetWithRelations(ctx, adminScope(caller), storageID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	// An embedded child (a bundled skill / squad member) is owned by its container
	// graph and must be swapped only through a container reupload — a standalone
	// PATCH must not content-edit it out of band, so it is reported as not found.
	if old.IsEmbedded {
		return nil, ErrNotFound
	}
	if req.Type != old.Type {
		return nil, ErrInvalidRequest
	}
	// Storage-attachment keys are namespaced under the row's real Space, so
	// canonicalization must validate against it — not the empty global Space
	// (P1-1). System connectors are space-less (old.SpaceID nil → empty).
	effSpace := adminGlobalSpace
	if old.SpaceID != nil {
		effSpace = *old.SpaceID
	}
	p, rels, err := s.adminEffectiveWrite(ctx, caller, storageID, req, old.Visibility, effSpace)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.CurrentVersionID = old.CreatedAt, old.CurrentVersionID
	p.CurrentVersion = old.CurrentVersion // keep the published version label, not just its id
	p.CreatorName, p.CreatedByType = old.CreatorName, old.CreatedByType
	p.CreatedByBotUID, p.CreatedByBotName = old.CreatedByBotUID, old.CreatedByBotName
	p.SpaceID = old.SpaceID   // preserve the row's existing Space on update
	p.OwnerUID = old.OwnerUID // owner provenance is immutable (Q7')
	// Publisher is display provenance; a metadata edit that omits it must not blank
	// a backfilled row's publisher, so fall back to the existing value.
	if strings.TrimSpace(req.Publisher) == "" {
		p.Publisher = old.Publisher
	}
	for i := range rels {
		rels[i].SourcePluginID = storageID
	}
	audit := s.audit(caller, storageID, "update", old, p, s.now())
	m := mutation(*p, rels, audit)
	m.SnapshotVersion = true
	sync, err := s.repo.Update(ctx, adminScope(caller), m)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels, RelationResult: relationResult(sync)}, nil
}

// AdminImportContainer ingests an uploaded expert/expert_team container archive
// and stores it as the unified plugin graph (the expert/team plus its skills,
// members, and relations) under the admin conventions. It is the admin-surface
// name for ImportContainer, which already stamps the admin identity itself.
func (s *Service) AdminImportContainer(ctx context.Context, caller Caller, p ContainerImportParams) (*Detail, error) {
	return s.ImportContainer(ctx, caller, p)
}

// AdminReuploadContainer re-uploads an expert/expert_team container archive to
// rebuild an EXISTING plugin in place (preserving its plugin_id, visibility,
// Space, owner, and market placement) under the admin conventions. It is the
// admin-surface name for ReuploadContainer, which already stamps the admin
// identity itself.
func (s *Service) AdminReuploadContainer(ctx context.Context, caller Caller, pluginID string, p ContainerImportParams) (*Detail, error) {
	return s.ReuploadContainer(ctx, caller, pluginID, p)
}

// AdminDelete soft-deletes any plugin by id, regardless of owner/Space. An
// expert/expert_team top owns embedded children (an expert's bundled skills; a
// squad's member experts and their skills), so it is torn down through DeleteGraph
// — top plus every embedded descendant in one transaction — otherwise the children
// would be orphaned (live, is_embedded=1, unreachable). DeleteGraph derives the
// embedded child set under the top's lock, so a delete racing a concurrent reupload
// cannot orphan a child; a standalone catalog skill merely referenced by the top
// (is_embedded=0) is not collected and survives. Connectors and skills carry no
// embedded children and take the single-row Delete.
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
	if old.Type == model.PluginTypeExpert || old.Type == model.PluginTypeExpertTeam {
		return mapStoreError(s.repo.DeleteGraph(ctx, adminScope(caller), storageID, audit.OperatorID, audit.OperatorName, audit.RequestID, audit.Remark))
	}
	return mapStoreError(s.repo.Delete(ctx, adminScope(caller), storageID, audit.OperatorID, audit.OperatorName, audit.RequestID, audit.Remark))
}
