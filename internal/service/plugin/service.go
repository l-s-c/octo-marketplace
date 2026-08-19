// Package plugin implements business rules for the unified Plugin domain.
// It has no HTTP or storage concerns; caller identity and Space are supplied by
// the trusted authentication boundary and are always passed to the repository.
package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/id"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/storage"
)

var (
	ErrNotFound       = errors.New("plugin not found")
	ErrConflict       = errors.New("plugin conflict")
	ErrInvalidRequest = errors.New("invalid plugin request")
	ErrTooLarge       = errors.New("plugin artifact exceeds size limit")
	ErrSecretValue    = errors.New("connector secret value is not allowed")
)

// Caller is populated from verified authentication context, never request JSON.
type Caller struct {
	UID           string
	Name          string
	SpaceID       string
	BotUID        string
	BotName       string
	RequestID     string
	IsSystemAdmin bool
}

// Store is the transactional persistence boundary required by Service.
// Implementations must apply Scope to every tenant-owned lookup and mutation.
type Store interface {
	List(context.Context, pluginrepo.Scope, pluginrepo.ListFilter) ([]model.Plugin, int64, error)
	GetWithRelations(context.Context, pluginrepo.Scope, string) (*model.Plugin, []model.PluginRelation, error)
	Create(context.Context, pluginrepo.Scope, pluginrepo.Mutation) error
	Update(context.Context, pluginrepo.Scope, pluginrepo.Mutation) error
	Delete(context.Context, pluginrepo.Scope, string, string, string, string, *string) error
	ListAudits(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginAuditLog, int64, error)
	ListVersions(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginVersion, int64, error)
	GetVersion(context.Context, pluginrepo.Scope, string, string) (*model.PluginVersion, error)
	Publish(context.Context, pluginrepo.Scope, pluginrepo.PublishParams) (*model.PluginVersion, error)
	DuplicateGraph(context.Context, pluginrepo.Scope, string, model.Plugin, pluginrepo.Mutation) error
}

var _ Store = (*pluginrepo.Repo)(nil)

type Service struct {
	repo               Store
	storage            storage.Storage
	id                 func() string
	now                func() time.Time
	maxAttachmentBytes int64
	maxArchiveBytes    int64
	maxArchiveFiles    int
}

func New(repo Store, stores ...storage.Storage) *Service {
	s := &Service{
		repo:               repo,
		id:                 id.New,
		now:                func() time.Time { return time.Now().UTC() },
		maxAttachmentBytes: defaultMaxAttachmentBytes,
		maxArchiveBytes:    defaultMaxArchiveBytes,
		maxArchiveFiles:    defaultMaxArchiveFiles,
	}
	if len(stores) > 0 {
		s.storage = stores[0]
	}
	return s
}

// SetArtifactLimits applies deployment upload size to individual attachments and
// keeps a larger but bounded aggregate archive limit.
func (s *Service) SetArtifactLimits(maxAttachmentBytes int64) {
	if maxAttachmentBytes > 0 {
		s.maxAttachmentBytes = maxAttachmentBytes
		s.maxArchiveBytes = maxAttachmentBytes * 5
	}
}

// WithRuntime is intended for deterministic tests and process wiring that uses
// a shared ID generator or clock.
func (s *Service) WithRuntime(idGen func() string, now func() time.Time) *Service {
	if idGen != nil {
		s.id = idGen
	}
	if now != nil {
		s.now = now
	}
	return s
}

type ListParams struct {
	PlacementCode string
	Type          model.PluginType
	CategoryID    string
	Tag           string
	Keyword       string
	Mine          bool
	Sort          string
	Limit         int
	Offset        int
}

type Detail struct {
	Plugin    *model.Plugin
	Relations []model.PluginRelation
}

type WriteRequest struct {
	Name       string
	Type       model.PluginType
	CategoryID *string
	Tags       json.RawMessage
	Publisher  string
	Visibility model.PluginVisibility
	Manifest   json.RawMessage
	Package    json.RawMessage
	Relations  []RelationRequest
}

type RelationRequest struct {
	TargetPluginID string
	Type           string
	SortOrder      int
	Data           json.RawMessage
}

type PublishRequest struct {
	Version    string
	Changelog  *string
	Placements []PlacementRequest
}

type PlacementRequest struct {
	PlacementCode string
	CategoryID    *string
	Visible       bool
	SortOrder     int
}

func scope(c Caller) pluginrepo.Scope { return pluginrepo.Scope{CallerUID: c.UID, SpaceID: c.SpaceID} }

func (s *Service) List(ctx context.Context, caller Caller, p ListParams) ([]model.Plugin, int64, error) {
	if err := validateCaller(caller); err != nil {
		return nil, 0, err
	}
	if p.Type != "" && !validPluginType(p.Type) {
		return nil, 0, ErrInvalidRequest
	}
	if p.Limit < 0 || p.Limit > maxListLimit || p.Offset < 0 {
		return nil, 0, ErrInvalidRequest
	}
	items, total, err := s.repo.List(ctx, scope(caller), pluginrepo.ListFilter{PlacementCode: strings.TrimSpace(p.PlacementCode), Type: p.Type, CategoryID: strings.TrimSpace(p.CategoryID), Tag: strings.TrimSpace(p.Tag), Keyword: strings.TrimSpace(p.Keyword), Mine: p.Mine, Sort: strings.TrimSpace(p.Sort), Limit: p.Limit, Offset: p.Offset})
	return items, total, mapStoreError(err)
}

func (s *Service) Detail(ctx context.Context, caller Caller, pluginID string) (*Detail, error) {
	if validateCaller(caller) != nil || strings.TrimSpace(pluginID) == "" {
		return nil, ErrInvalidRequest
	}
	p, rels, err := s.repo.GetWithRelations(ctx, scope(caller), pluginID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels}, nil
}

func (s *Service) Create(ctx context.Context, caller Caller, req WriteRequest) (*Detail, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	now := s.now()
	p, rels, err := s.buildWrite(ctx, caller, "", req, now)
	if err != nil {
		return nil, err
	}
	p.ID = s.id()
	for i := range rels {
		rels[i].SourcePluginID = p.ID
	}
	audit := s.audit(caller, p.ID, "create", nil, p, now)
	if err := s.repo.Create(ctx, scope(caller), mutation(*p, rels, audit)); err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels}, nil
}

func (s *Service) Update(ctx context.Context, caller Caller, pluginID string, req WriteRequest) (*Detail, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	if strings.TrimSpace(pluginID) == "" {
		return nil, ErrInvalidRequest
	}
	old, _, err := s.repo.GetWithRelations(ctx, scope(caller), pluginID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if old.OwnerUID != caller.UID || (old.SpaceID != nil && *old.SpaceID != caller.SpaceID) {
		return nil, ErrNotFound
	}
	if req.Type != old.Type {
		return nil, ErrInvalidRequest
	}
	now := s.now()
	p, rels, err := s.buildWrite(ctx, caller, pluginID, req, now)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.CurrentVersionID = old.CreatedAt, old.CurrentVersionID
	for i := range rels {
		rels[i].SourcePluginID = pluginID
	}
	audit := s.audit(caller, pluginID, "update", old, p, now)
	if err := s.repo.Update(ctx, scope(caller), mutation(*p, rels, audit)); err != nil {
		return nil, mapStoreError(err)
	}
	return &Detail{Plugin: p, Relations: rels}, nil
}

func (s *Service) Delete(ctx context.Context, caller Caller, pluginID string) error {
	if validateCaller(caller) != nil || strings.TrimSpace(pluginID) == "" {
		return ErrInvalidRequest
	}
	old, _, err := s.repo.GetWithRelations(ctx, scope(caller), pluginID)
	if err != nil {
		return mapStoreError(err)
	}
	if old.OwnerUID != caller.UID || (old.SpaceID != nil && *old.SpaceID != caller.SpaceID) {
		return ErrNotFound
	}
	audit := s.audit(caller, pluginID, "delete", old, nil, s.now())
	return mapStoreError(s.repo.Delete(ctx, scope(caller), pluginID, audit.OperatorID, audit.OperatorName, audit.RequestID, audit.Remark))
}

func (s *Service) ListAuditLogs(ctx context.Context, caller Caller, pluginID string, limit, offset int) ([]model.PluginAuditLog, int64, error) {
	if validateReadPage(caller, pluginID, limit, offset) != nil {
		return nil, 0, ErrInvalidRequest
	}
	items, total, err := s.repo.ListAudits(ctx, scope(caller), pluginID, limit, offset)
	return items, total, mapStoreError(err)
}

func (s *Service) ListVersions(ctx context.Context, caller Caller, pluginID string, limit, offset int) ([]model.PluginVersion, int64, error) {
	if validateReadPage(caller, pluginID, limit, offset) != nil {
		return nil, 0, ErrInvalidRequest
	}
	items, total, err := s.repo.ListVersions(ctx, scope(caller), pluginID, limit, offset)
	return items, total, mapStoreError(err)
}

func (s *Service) Publish(ctx context.Context, caller Caller, pluginID string, req PublishRequest) (*model.PluginVersion, error) {
	if validateCaller(caller) != nil || strings.TrimSpace(pluginID) == "" || !validVersion(req.Version) {
		return nil, ErrInvalidRequest
	}
	now := s.now()
	placements, err := s.buildPlacements(pluginID, req.Placements, now)
	if err != nil {
		return nil, err
	}
	params := pluginrepo.PublishParams{
		PluginID:     pluginID,
		Version:      strings.TrimSpace(req.Version),
		CreatedBy:    caller.UID,
		OperatorName: caller.Name,
		RequestID:    caller.RequestID,
		Changelog:    trimOptional(req.Changelog),
		Placements:   placements,
	}
	version, err := s.repo.Publish(ctx, scope(caller), params)
	if err != nil {
		return nil, mapStoreError(err)
	}
	return version, nil
}

func (s *Service) Duplicate(ctx context.Context, caller Caller, sourcePluginID, name string) (*model.Plugin, error) {
	if validateCaller(caller) != nil || strings.TrimSpace(sourcePluginID) == "" {
		return nil, ErrInvalidRequest
	}
	source, _, err := s.repo.GetWithRelations(ctx, scope(caller), sourcePluginID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if source.Type == model.PluginTypeConnector {
		if err := rejectConnectorSecrets(source.Manifest, source.Package); err != nil {
			return nil, err
		}
	}
	newName := strings.TrimSpace(name)
	if newName == "" {
		newName = source.Name + " copy"
	}
	if !validName(newName) {
		return nil, ErrInvalidRequest
	}
	now := s.now()
	spaceID := caller.SpaceID
	copy := *source
	copy.ID, copy.Name = s.id(), newName
	copy.OwnerUID, copy.SpaceID, copy.CreatorName = caller.UID, &spaceID, caller.Name
	copy.CreatedByType, copy.CreatedByBotUID, copy.CreatedByBotName = provenance(caller)
	copy.Visibility, copy.CurrentVersionID = model.PluginVisibilityPrivate, nil
	copy.CreatedAt, copy.UpdatedAt, copy.DeletedAt = now, now, nil
	copy.Manifest, copy.Package, copy.Tags = cloneJSON(source.Manifest), cloneJSON(source.Package), cloneJSON(source.Tags)
	audit := s.audit(caller, copy.ID, "duplicate", nil, &copy, now)
	if err := s.repo.DuplicateGraph(ctx, scope(caller), sourcePluginID, copy, mutation(copy, nil, audit)); err != nil {
		return nil, mapStoreError(err)
	}
	return &copy, nil
}

func (s *Service) buildWrite(ctx context.Context, c Caller, pluginID string, req WriteRequest, now time.Time) (*model.Plugin, []model.PluginRelation, error) {
	name := strings.TrimSpace(req.Name)
	if !validName(name) || !validPluginType(req.Type) || !validVisibility(req.Visibility, c.IsSystemAdmin) {
		return nil, nil, ErrInvalidRequest
	}
	manifest, manifestHash, err := normalizeObject(req.Manifest)
	if err != nil {
		return nil, nil, err
	}
	pkg, _, err := normalizeObject(req.Package)
	if err != nil {
		return nil, nil, err
	}
	pluginHash := hashJSON(append(append(cloneJSON(manifest), '\n'), pkg...))
	tags, err := normalizeTags(req.Tags)
	if err != nil {
		return nil, nil, err
	}
	if req.Type == model.PluginTypeConnector {
		if err := rejectConnectorSecrets(manifest, pkg); err != nil {
			return nil, nil, err
		}
	}
	spaceID := c.SpaceID
	createdBy, botUID, botName := provenance(c)
	p := &model.Plugin{ID: pluginID, Name: name, Type: req.Type, CategoryID: trimOptional(req.CategoryID), Tags: tags, Publisher: strings.TrimSpace(req.Publisher), OwnerUID: c.UID, SpaceID: &spaceID, Visibility: req.Visibility, CreatorName: c.Name, CreatedByType: createdBy, CreatedByBotUID: botUID, CreatedByBotName: botName, Manifest: manifest, Package: pkg, ManifestHash: manifestHash, PluginHash: pluginHash, Status: 1, CreatedAt: now, UpdatedAt: now}
	rels, err := s.buildRelations(ctx, c, p, req.Relations, now)
	if err != nil {
		return nil, nil, err
	}
	return p, rels, nil
}

func (s *Service) buildRelations(ctx context.Context, c Caller, source *model.Plugin, in []RelationRequest, now time.Time) ([]model.PluginRelation, error) {
	if len(in) > maxRelations {
		return nil, ErrInvalidRequest
	}
	out := make([]model.PluginRelation, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, r := range in {
		targetID, typ := strings.TrimSpace(r.TargetPluginID), strings.TrimSpace(r.Type)
		if targetID == "" || targetID == source.ID || !validRelationSource(typ, source.Type) {
			return nil, ErrInvalidRequest
		}
		key := typ + "\x00" + targetID
		if _, ok := seen[key]; ok {
			return nil, ErrInvalidRequest
		}
		seen[key] = struct{}{}
		target, _, err := s.repo.GetWithRelations(ctx, scope(c), targetID)
		if err != nil {
			return nil, mapStoreError(err)
		}
		if !validRelationTarget(typ, target.Type) {
			return nil, ErrInvalidRequest
		}
		data, err := normalizeOptionalObject(r.Data)
		if err != nil {
			return nil, err
		}
		out = append(out, model.PluginRelation{ID: s.id(), SourcePluginID: source.ID, TargetPluginID: targetID, Type: typ, SortOrder: r.SortOrder, Data: data, Status: 1, CreatedBy: c.UID, CreatedAt: now, UpdatedAt: now})
	}
	return out, nil
}

func (s *Service) buildPlacements(pluginID string, in []PlacementRequest, now time.Time) ([]model.PluginPlacement, error) {
	if len(in) > maxPlacements {
		return nil, ErrInvalidRequest
	}
	out := make([]model.PluginPlacement, 0, len(in))
	seen := map[string]struct{}{}
	for _, x := range in {
		code := strings.TrimSpace(x.PlacementCode)
		if !validPlacementCode(code) {
			return nil, ErrInvalidRequest
		}
		category := trimOptional(x.CategoryID)
		key := code + "\x00"
		if category != nil {
			key += *category
		}
		if _, ok := seen[key]; ok {
			return nil, ErrInvalidRequest
		}
		seen[key] = struct{}{}
		out = append(out, model.PluginPlacement{ID: s.id(), PlacementCode: code, PluginID: pluginID, CategoryID: category, Visible: x.Visible, SortOrder: x.SortOrder, CreatedAt: now, UpdatedAt: now})
	}
	return out, nil
}

func (s *Service) audit(c Caller, pluginID, action string, before, after *model.Plugin, now time.Time) model.PluginAuditLog {
	a := model.PluginAuditLog{ID: s.id(), PluginID: pluginID, Action: action, OperatorID: c.UID, OperatorName: c.Name, RequestID: c.RequestID, CreatedAt: now}
	if before != nil {
		a.BeforeHash = stringPtr(before.PluginHash)
		a.ManifestSnapshot = cloneJSON(before.Manifest)
		a.PluginSnapshot = cloneJSON(before.Package)
	}
	if after != nil {
		a.AfterHash = stringPtr(after.PluginHash)
		a.ManifestSnapshot = cloneJSON(after.Manifest)
		a.PluginSnapshot = cloneJSON(after.Package)
	}
	return a
}

func provenance(c Caller) (string, *string, *string) {
	if c.BotUID != "" {
		return "bot", stringPtr(c.BotUID), stringPtr(c.BotName)
	}
	return "human", nil, nil
}

func mutation(p model.Plugin, relations []model.PluginRelation, audit model.PluginAuditLog) pluginrepo.Mutation {
	return pluginrepo.Mutation{
		Plugin:       p,
		Relations:    relations,
		OperatorID:   audit.OperatorID,
		OperatorName: audit.OperatorName,
		RequestID:    audit.RequestID,
		Remark:       audit.Remark,
	}
}

func mapStoreError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pluginrepo.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, pluginrepo.ErrConflict):
		return ErrConflict
	case errors.Is(err, pluginrepo.ErrInvalidRelation), errors.Is(err, pluginrepo.ErrInvalidPlacement):
		return ErrInvalidRequest
	case errors.Is(err, pluginrepo.ErrUnsafeConnectorData):
		return ErrSecretValue
	default:
		return err
	}
}

func validateCaller(c Caller) error {
	if strings.TrimSpace(c.UID) == "" || strings.TrimSpace(c.SpaceID) == "" {
		return ErrInvalidRequest
	}
	return nil
}
func validateReadPage(c Caller, id string, limit, offset int) error {
	if validateCaller(c) != nil || strings.TrimSpace(id) == "" || limit < 0 || limit > maxListLimit || offset < 0 {
		return ErrInvalidRequest
	}
	return nil
}
func cloneJSON(v json.RawMessage) json.RawMessage { return append(json.RawMessage(nil), v...) }
func stringPtr(v string) *string                  { return &v }
func trimOptional(v *string) *string {
	if v == nil {
		return nil
	}
	x := strings.TrimSpace(*v)
	if x == "" {
		return nil
	}
	return &x
}
