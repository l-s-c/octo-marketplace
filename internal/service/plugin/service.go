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
	"unicode/utf8"

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
	Create(context.Context, pluginrepo.Scope, pluginrepo.Mutation) (*pluginrepo.RelationSync, error)
	Update(context.Context, pluginrepo.Scope, pluginrepo.Mutation) (*pluginrepo.RelationSync, error)
	Delete(context.Context, pluginrepo.Scope, string, string, string, string, *string) error
	ListVersions(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginVersion, int64, error)
	VersionExists(context.Context, pluginrepo.Scope, string, string) (bool, error)
	Publish(context.Context, pluginrepo.Scope, pluginrepo.PublishParams) (*model.PluginVersion, error)
	CountMemberRelations(context.Context, []string) (map[string]int, error)
	ListTags(context.Context, pluginrepo.Scope, pluginrepo.TagListFilter) ([]model.TagFilter, error)
}

var _ Store = (*pluginrepo.Repo)(nil)

type Service struct {
	repo               Store
	storage            storage.Storage
	provisioner        Provisioner
	metrics            InstallTracker
	parseTasks         ParseTaskStore
	id                 func() string
	now                func() time.Time
	maxAttachmentBytes int64
	maxArchiveBytes    int64
}

func New(repo Store, stores ...storage.Storage) *Service {
	s := &Service{
		repo:               repo,
		id:                 id.New,
		now:                func() time.Time { return time.Now().UTC() },
		maxAttachmentBytes: defaultMaxAttachmentBytes,
		maxArchiveBytes:    defaultMaxArchiveBytes,
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
	Tags          []string
	Keyword       string
	Mine          bool
	Sort          string
	Limit         int
	Offset        int
}

type Detail struct {
	Plugin    *model.Plugin
	Relations []model.PluginRelation
	// RelationResult reports upsert relation synchronization; nil on reads.
	RelationResult *RelationResult
}

// RelationResult mirrors the target-state relation sync outcome on the wire:
// empty relation_id created, known relation_id with changes updated, live
// relations absent from the submission deleted.
type RelationResult struct {
	Created []string
	Updated []string
	Deleted []string
}

type WriteRequest struct {
	Name       string
	Type       model.PluginType
	CategoryID *string
	Tags       json.RawMessage
	Publisher  string
	// Icon is a persistent public URL or a managed storage object key; it is
	// display metadata kept outside the content-addressed manifest.
	Icon       string
	Visibility model.PluginVisibility
	Manifest   json.RawMessage
	Package    json.RawMessage
	Relations  []RelationRequest
}

type RelationRequest struct {
	// ID is the submitted relation_id: empty creates, a live relation's ID
	// updates, and live relations omitted from the submission are deleted.
	ID string
	// SourcePluginID, when supplied, must address the Plugin being upserted.
	SourcePluginID string
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
	switch p.Sort {
	case "", "newest", "oldest", "updated", "name", "placement", "views", "installs", "downloads", "comprehensive":
	default:
		return nil, 0, ErrInvalidRequest
	}
	if p.Sort == "placement" && strings.TrimSpace(p.PlacementCode) == "" {
		return nil, 0, ErrInvalidRequest
	}
	if p.Limit < 0 || p.Limit > maxListLimit || p.Offset < 0 {
		return nil, 0, ErrInvalidRequest
	}
	tags, err := normalizeListTags(p.Tags)
	if err != nil {
		return nil, 0, err
	}
	items, total, err := s.repo.List(ctx, scope(caller), pluginrepo.ListFilter{PlacementCode: strings.TrimSpace(p.PlacementCode), Type: p.Type, CategoryID: strings.TrimSpace(p.CategoryID), Tags: tags, Keyword: strings.TrimSpace(p.Keyword), Mine: p.Mine, Sort: strings.TrimSpace(p.Sort), Limit: p.Limit, Offset: p.Offset})
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

// TagListParams drives the aggregated tag suggestions; the visible set mirrors
// List so callers only see tags on Plugins they could open.
type TagListParams struct {
	PlacementCode string
	Type          model.PluginType
	Keyword       string
	Mine          bool
	Limit         int
}

func (s *Service) ListTags(ctx context.Context, caller Caller, p TagListParams) ([]model.TagFilter, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	if p.Type != "" && !validPluginType(p.Type) {
		return nil, ErrInvalidRequest
	}
	if p.Limit < 0 || p.Limit > maxListLimit {
		return nil, ErrInvalidRequest
	}
	tags, err := s.repo.ListTags(ctx, scope(caller), pluginrepo.TagListFilter{
		PlacementCode: strings.TrimSpace(p.PlacementCode),
		Type:          p.Type,
		Keyword:       strings.TrimSpace(p.Keyword),
		Mine:          p.Mine,
		Limit:         p.Limit,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	return tags, nil
}

// fillMemberCounts derives list-only member counts for expert teams in one
// batched relation query instead of persisting a column that upsert, relation
// sync, and delete would each have to keep consistent.
func (s *Service) fillMemberCounts(ctx context.Context, items []model.Plugin) error {
	var teamIDs []string
	for i := range items {
		if items[i].Type == model.PluginTypeExpertTeam {
			teamIDs = append(teamIDs, items[i].ID)
		}
	}
	if len(teamIDs) == 0 {
		return nil
	}
	counts, err := s.repo.CountMemberRelations(ctx, teamIDs)
	if err != nil {
		return err
	}
	for i := range items {
		items[i].MemberCount = counts[items[i].ID]
	}
	return nil
}

// iconNamespaces are the only storage roots resolveIcon will presign: the
// legacy icon upload endpoints generate keys under them. Icon is a
// caller-writable field, so presigning arbitrary key shapes would hand out
// signed URLs for unrelated objects (e.g. another Space's attachments).
var iconNamespaces = []string{"icons/", "mcp-icons/"}

func presignableIconKey(icon string) bool {
	if !iconKeyPattern.MatchString(icon) || strings.Contains(icon, "..") {
		return false
	}
	for _, namespace := range iconNamespaces {
		if strings.HasPrefix(icon, namespace) {
			return true
		}
	}
	return false
}

// resolveIcon turns a stored icon object key into a time-limited display URL
// while passing persistent http(s) URLs and text glyphs (legacy emoji icons)
// through unchanged. Only known icon namespaces are presigned; resolution
// failures keep the raw value, matching the legacy skill icon behavior.
func (s *Service) resolveIcon(ctx context.Context, icon string) string {
	if icon == "" {
		return ""
	}
	if strings.HasPrefix(icon, "http://") || strings.HasPrefix(icon, "https://") || s.storage == nil || !presignableIconKey(icon) {
		return icon
	}
	if resolved, err := s.storage.PresignGet(ctx, icon, time.Hour); err == nil {
		return resolved
	}
	return icon
}

// normalizeListTags trims, deduplicates, and bounds the AND-combined tag
// filters so a request cannot inflate the query with unbounded predicates.
func normalizeListTags(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, tag := range raw {
		tag = strings.TrimSpace(tag)
		if tag == "" {
			continue
		}
		if !utf8.ValidString(tag) || len(tag) > maxTagBytes {
			return nil, ErrInvalidRequest
		}
		if _, dup := seen[tag]; dup {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	if len(out) > maxListTags {
		return nil, ErrInvalidRequest
	}
	return out, nil
}

func (s *Service) Detail(ctx context.Context, caller Caller, pluginID string, includeRelations bool) (*Detail, error) {
	if validateCaller(caller) != nil {
		return nil, ErrInvalidRequest
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	p, rels, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
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

func (s *Service) Create(ctx context.Context, caller Caller, req WriteRequest) (*Detail, error) {
	return s.createWithID(ctx, caller, req, "")
}

// createWithID is Create with an optional caller-reserved plugin ID. Import
// reserves the ID up front — it is baked into the SKILL.md frontmatter and used
// to namespace the spilled attachment object keys — so the persisted row must
// carry that same ID rather than minting a second one, or the shipped id, the
// object namespace, and the row would all disagree.
func (s *Service) createWithID(ctx context.Context, caller Caller, req WriteRequest, reservedID string) (*Detail, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	now := s.now()
	p, rels, err := s.buildWrite(ctx, caller, "", req, now)
	if err != nil {
		return nil, err
	}
	p.ID = reservedID
	if p.ID == "" {
		p.ID = s.id()
	}
	for i := range rels {
		rels[i].SourcePluginID = p.ID
	}
	audit := s.audit(caller, p.ID, "create", nil, p, now)
	sync, err := s.repo.Create(ctx, scope(caller), mutation(*p, rels, audit))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if sync != nil && sync.Relations != nil {
		rels = sync.Relations
	}
	return &Detail{Plugin: p, Relations: rels, RelationResult: relationResult(sync)}, nil
}

func (s *Service) Update(ctx context.Context, caller Caller, pluginID string, req WriteRequest) (*Detail, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	old, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
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
	p, rels, err := s.buildWrite(ctx, caller, storageID, req, now)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, p.CurrentVersionID = old.CreatedAt, old.CurrentVersionID
	// Creation provenance is immutable; keep the original creator identity.
	p.CreatorName, p.CreatedByType = old.CreatorName, old.CreatedByType
	p.CreatedByBotUID, p.CreatedByBotName = old.CreatedByBotUID, old.CreatedByBotName
	for i := range rels {
		rels[i].SourcePluginID = storageID
	}
	audit := s.audit(caller, storageID, "update", old, p, now)
	sync, err := s.repo.Update(ctx, scope(caller), mutation(*p, rels, audit))
	if err != nil {
		return nil, mapStoreError(err)
	}
	if sync != nil && sync.Relations != nil {
		rels = sync.Relations
	}
	return &Detail{Plugin: p, Relations: rels, RelationResult: relationResult(sync)}, nil
}

func relationResult(sync *pluginrepo.RelationSync) *RelationResult {
	if sync == nil {
		return &RelationResult{Created: []string{}, Updated: []string{}, Deleted: []string{}}
	}
	out := &RelationResult{Created: sync.Created, Updated: sync.Updated, Deleted: sync.Deleted}
	if out.Created == nil {
		out.Created = []string{}
	}
	if out.Updated == nil {
		out.Updated = []string{}
	}
	if out.Deleted == nil {
		out.Deleted = []string{}
	}
	return out
}

func (s *Service) Delete(ctx context.Context, caller Caller, pluginID string) error {
	if validateCaller(caller) != nil {
		return ErrInvalidRequest
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return err
	}
	old, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
	if err != nil {
		return mapStoreError(err)
	}
	if old.OwnerUID != caller.UID || (old.SpaceID != nil && *old.SpaceID != caller.SpaceID) {
		return ErrNotFound
	}
	audit := s.audit(caller, storageID, "delete", old, nil, s.now())
	return mapStoreError(s.repo.Delete(ctx, scope(caller), storageID, audit.OperatorID, audit.OperatorName, audit.RequestID, audit.Remark))
}

func (s *Service) ListVersions(ctx context.Context, caller Caller, pluginID string, limit, offset int) ([]model.PluginVersion, int64, error) {
	if validateCaller(caller) != nil || limit < 0 || limit > maxListLimit || offset < 0 {
		return nil, 0, ErrInvalidRequest
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, 0, err
	}
	p, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
	if err != nil {
		return nil, 0, mapStoreError(err)
	}
	items, total, err := s.repo.ListVersions(ctx, scope(caller), storageID, limit, offset)
	for i := range items {
		items[i].PluginType = p.Type
	}
	return items, total, mapStoreError(err)
}

func (s *Service) Publish(ctx context.Context, caller Caller, pluginID string, req PublishRequest) (*model.PluginVersion, error) {
	if validateCaller(caller) != nil || !validVersion(req.Version) {
		return nil, ErrInvalidRequest
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	now := s.now()
	placements, err := s.buildPlacements(storageID, req.Placements, now)
	if err != nil {
		return nil, err
	}
	params := pluginrepo.PublishParams{
		PluginID:     storageID,
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

func (s *Service) buildWrite(ctx context.Context, c Caller, pluginID string, req WriteRequest, now time.Time) (*model.Plugin, []model.PluginRelation, error) {
	name := strings.TrimSpace(req.Name)
	if !validName(name) || !validPluginType(req.Type) || !validVisibility(req.Visibility, c.IsSystemAdmin) {
		return nil, nil, ErrInvalidRequest
	}
	docs, err := CanonicalizeDocuments(name, req.Type, req.Tags, req.Manifest, req.Package, c.SpaceID)
	if err != nil {
		return nil, nil, err
	}
	icon := strings.TrimSpace(req.Icon)
	if !validIcon(icon) {
		return nil, nil, ErrInvalidRequest
	}
	toolCount := 0
	if req.Type == model.PluginTypeConnector {
		toolCount = ConnectorToolCount(docs.Package)
	}
	spaceID := c.SpaceID
	createdBy, botUID, botName := provenance(c)
	p := &model.Plugin{ID: pluginID, Name: name, Type: req.Type, CategoryID: trimOptional(req.CategoryID), Tags: docs.Tags, Publisher: strings.TrimSpace(req.Publisher), OwnerUID: c.UID, SpaceID: &spaceID, Visibility: req.Visibility, CreatorName: c.Name, CreatedByType: createdBy, CreatedByBotUID: botUID, CreatedByBotName: botName, Icon: icon, IconURL: s.resolveIcon(ctx, icon), ToolCount: toolCount, Manifest: docs.Manifest, Package: docs.Package, ManifestHash: docs.ManifestHash, PluginHash: docs.PluginHash, Status: 1, CreatedAt: now, UpdatedAt: now}
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
		relationID := strings.TrimSpace(r.ID)
		// A relation ID can only address an existing edge, so it is meaningless
		// while the Plugin itself is being created (source.ID is still empty).
		if relationID != "" && (source.ID == "" || !validRelationID(relationID)) {
			return nil, ErrInvalidRequest
		}
		if submittedSource := strings.TrimSpace(r.SourcePluginID); submittedSource != "" {
			sourceID, err := parseStorageID(submittedSource)
			if err != nil || source.ID == "" || sourceID != source.ID {
				return nil, ErrInvalidRequest
			}
		}
		targetID, err := parseStorageID(r.TargetPluginID)
		typ := strings.TrimSpace(r.Type)
		if err != nil || targetID == source.ID || !validRelationSource(typ, source.Type) {
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
		if !validRelationType(typ, source.Type, target.Type) {
			return nil, ErrInvalidRequest
		}
		data, err := normalizeOptionalObject(r.Data)
		if err != nil {
			return nil, err
		}
		if len(data) > 0 {
			if err := rejectSecretValues(data); err != nil {
				return nil, err
			}
		}
		out = append(out, model.PluginRelation{ID: relationID, SourcePluginID: source.ID, SourcePluginType: source.Type, TargetPluginID: targetID, TargetPluginType: target.Type, Type: typ, SortOrder: r.SortOrder, Data: data, Status: 1, CreatedBy: c.UID, CreatedAt: now, UpdatedAt: now})
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
	case errors.Is(err, pluginrepo.ErrInvalidRelation), errors.Is(err, pluginrepo.ErrInvalidCategory), errors.Is(err, pluginrepo.ErrInvalidPlacement):
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
