// Plugin import turns a completed legacy upload-parse task into a skill
// Plugin: the rewritten package zip is expanded into a flat attachment tree
// (one attachment per file — text inlined as raw, binary/oversize files
// uploaded to the managed plugins/<space>/attachments/ prefix under
// deterministic keys and referenced as storage attachments). Everything funnels
// through Create/Update, so validation, hashing, and audit are
// identical to a direct upsert.

package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	skillrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/skill"
	skillsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/skill"
	"go.uber.org/zap"
)

// ErrInvalidParseTask covers absent, foreign, unfinished, and already-consumed
// parse tasks with one answer so import cannot probe other users' uploads.
var ErrInvalidParseTask = errors.New("invalid or consumed parse task")

// ParseTaskStore is the legacy upload-parse pipeline surface import consumes;
// *skillrepo.Repo satisfies it.
type ParseTaskStore interface {
	GetParseTask(ctx context.Context, id string) (*skillrepo.ParseTaskRow, error)
	MarkParseTaskConsumed(ctx context.Context, id, ownerID, spaceID, skillID string) error
	ReleaseConsumedParseTask(ctx context.Context, id string) error
}

// WithParseTasks wires the parse-task source (chainable at construction).
func (s *Service) WithParseTasks(p ParseTaskStore) *Service {
	s.parseTasks = p
	return s
}

type ImportParams struct {
	ParseTaskID string
	// PluginID selects an existing caller-owned skill Plugin to update; empty
	// creates a new one.
	PluginID    string
	PluginName  string
	Name        string
	Description string
	CategoryID  *string
	Tags        []string
	Visibility  model.PluginVisibility
	Icon        string
	Version     string
	Changelog   *string
}

func (s *Service) Import(ctx context.Context, caller Caller, p ImportParams) (*Detail, error) {
	if validateCaller(caller) != nil {
		return nil, ErrInvalidRequest
	}
	if s.parseTasks == nil || s.storage == nil {
		return nil, errors.New("plugin import is not configured")
	}
	task, err := s.parseTasks.GetParseTask(ctx, strings.TrimSpace(p.ParseTaskID))
	if err != nil {
		return nil, fmt.Errorf("load parse task: %w", err)
	}
	// Ownership, Space, completion, and reupload-binding checks collapse to one
	// error so a caller cannot distinguish foreign tasks from missing ones.
	if task == nil || task.OwnerID != caller.UID || task.SpaceID != caller.SpaceID || task.Status != "success" || task.SkillID != "" {
		return nil, ErrInvalidParseTask
	}

	updateID := ""
	var oldPlugin *model.Plugin
	var oldRels []model.PluginRelation
	if strings.TrimSpace(p.PluginID) != "" {
		updateID, err = parseStorageID(p.PluginID)
		if err != nil {
			return nil, err
		}
		old, rels, err := s.repo.GetWithRelations(ctx, scope(caller), updateID)
		if err != nil {
			return nil, mapStoreError(err)
		}
		if old.OwnerUID != caller.UID || old.Type != model.PluginTypeSkill {
			return nil, ErrNotFound
		}
		oldPlugin, oldRels = old, rels
	}

	fields, err := resolveImportFields(p, task, caller.IsSystemAdmin)
	if err != nil {
		return nil, err
	}

	// Pre-flight the immutable-version conflict on the re-import path BEFORE any
	// document mutation or object upload. Re-importing an existing version string
	// is the ordinary user mistake; catching it here means the publish failure can
	// no longer leave the live document half-updated under the old version pointer.
	if updateID != "" {
		exists, err := s.repo.VersionExists(ctx, scope(caller), updateID, fields.version)
		if err != nil {
			return nil, mapStoreError(err)
		}
		if exists {
			return nil, ErrConflict
		}
	}

	// Consume first: the optimistic status flip is the duplicate-import lock.
	// Every failure below releases it (best-effort) so the upload is retryable.
	if err := s.parseTasks.MarkParseTaskConsumed(ctx, task.ID, caller.UID, caller.SpaceID, ""); err != nil {
		if errors.Is(err, skillrepo.ErrParseTaskAlreadyConsumed) {
			return nil, ErrInvalidParseTask
		}
		return nil, fmt.Errorf("consume parse task: %w", err)
	}
	detail, err := s.importConsumedTask(ctx, caller, task, fields, updateID, oldPlugin, oldRels)
	if err != nil {
		_ = s.parseTasks.ReleaseConsumedParseTask(context.WithoutCancel(ctx), task.ID)
		return nil, err
	}
	return detail, nil
}

// importFields are the resolved (request value falling back to parse result)
// document fields of one import.
type importFields struct {
	pluginName  string
	name        string
	description string
	version     string
	tags        []string
	visibility  model.PluginVisibility
	categoryID  *string
	icon        string
	changelog   *string
}

func resolveImportFields(p ImportParams, task *skillrepo.ParseTaskRow, systemAdmin bool) (*importFields, error) {
	f := &importFields{
		pluginName:  strings.TrimSpace(p.PluginName),
		name:        strings.TrimSpace(p.Name),
		description: strings.TrimSpace(p.Description),
		changelog:   trimOptional(p.Changelog),
		version:     strings.TrimSpace(p.Version),
		visibility:  p.Visibility,
		categoryID:  trimOptional(p.CategoryID),
		icon:        strings.TrimSpace(p.Icon),
	}
	if f.name == "" {
		f.name = strings.TrimSpace(task.ResultName)
	}
	if f.pluginName == "" {
		f.pluginName = f.name
	}
	if f.description == "" && task.ResultDescription != nil {
		f.description = strings.TrimSpace(*task.ResultDescription)
	}
	if f.version == "" {
		f.version = strings.TrimSpace(task.ResultVersion)
	}
	if f.version == "" {
		f.version = "1.0.0"
	}
	f.tags = p.Tags
	if f.tags == nil && len(task.ResultTags) > 0 {
		_ = json.Unmarshal(task.ResultTags, &f.tags)
	}
	if f.visibility == "" {
		f.visibility = model.PluginVisibilitySpace
	}
	// Match the legacy skill upload rule: uploads never publish publicly.
	if f.visibility == model.PluginVisibilityPublic {
		return nil, ErrInvalidRequest
	}
	if !validName(f.pluginName) || f.name == "" || !validVersion(f.version) || !validVisibility(f.visibility, systemAdmin) {
		return nil, ErrInvalidRequest
	}
	return f, nil
}

func (s *Service) importConsumedTask(ctx context.Context, caller Caller, task *skillrepo.ParseTaskRow, f *importFields, updateID string, oldPlugin *model.Plugin, oldRels []model.PluginRelation) (*Detail, error) {
	// A package-only re-upload omits the icon (nothing to change); the import is
	// a full replace, so fall back to the existing row's icon rather than
	// clearing it. A fresh create has no prior icon, so this only affects updates.
	if updateID != "" && f.icon == "" && oldPlugin != nil {
		f.icon = oldPlugin.Icon
	}
	req, pluginID, uploaded, err := s.buildImportedSkillWrite(ctx, caller.SpaceID, updateID, task, f, true)
	if err != nil {
		return nil, err
	}
	var detail *Detail
	if updateID == "" {
		// Persist under the reserved ID so the shipped SKILL.md frontmatter, the
		// spilled object namespace, and the row all agree on one plugin_id.
		detail, err = s.createWithID(ctx, caller, *req, pluginID)
	} else {
		detail, err = s.Update(ctx, caller, updateID, *req)
	}
	if err != nil {
		s.deleteObjects(ctx, uploaded...)
		return nil, err
	}
	// Publish rebuilds the single default placement, keeping the Plugin
	// discoverable in the confirmed marketplace scene. The ordinary version-string
	// conflict was pre-flighted before mutation; a conflict here can only come
	// from a concurrent publish racing between that check and this write.
	placement := PlacementRequest{PlacementCode: "default", CategoryID: f.categoryID, Visible: true}
	if _, err := s.Publish(ctx, caller, detail.Plugin.ID, PublishRequest{Version: f.version, Changelog: f.changelog, Placements: []PlacementRequest{placement}}); err != nil {
		if updateID == "" {
			// Create rollback: nothing else references the fresh objects yet.
			_ = s.Delete(ctx, caller, detail.Plugin.ID)
			s.deleteObjects(ctx, uploaded...)
			return nil, err
		}
		// Update rollback: restore the prior document so the failed re-publish
		// leaves no half-applied content change under the old version pointer.
		// Only on a successful restore are the newly-uploaded objects safe to
		// drop — buildSkillAttachmentTree recorded solely genuinely-new keys
		// (Q3), so none are shared with the restored state. If the restore itself
		// fails the live row still references those objects, so they are kept.
		// NOTE (B5): the restore replays through Service.Update, so a not-yet-
		// expanded backfilled skill whose stored package still carries a legacy-root
		// skill/ref.json fails the caller-path canonicalization gate and the restore
		// is skipped (logged). This only affects the narrow concurrent-publish race
		// on an un-expanded legacy row; the runbook's expand-skills phase removes
		// those legacy pointers before the row is served.
		if oldPlugin != nil {
			if _, restoreErr := s.Update(ctx, caller, updateID, restoreWriteRequest(oldPlugin, oldRels)); restoreErr == nil {
				s.deleteObjects(ctx, uploaded...)
			} else {
				logging.Error("import_restore_failed",
					zap.String("operation", "plugin.import.restore"),
					zap.String("plugin_id", updateID),
					logging.ErrorField(restoreErr),
				)
			}
		}
		return nil, err
	}
	return detail, nil
}

// buildImportedSkillWrite verifies the uploaded skill zip, reserves the plugin
// ID (reservedID for a reupload, freshly minted otherwise), rewrites the package
// under that ID, expands it into the flat attachment tree namespaced under
// objectSpace, and assembles the skill WriteRequest. It returns the reserved
// pluginID — baked into the shipped SKILL.md frontmatter and the spilled object
// keys, so the persisted row must carry the same one — plus the newly-uploaded
// object keys for rollback. Shared by the tenant Import and the admin skill
// import: requireSafeSpace enforces a valid managed-prefix Space up front (tenant
// callers always carry one), while the admin global-Space import passes false and
// relies on buildSkillAttachmentTree to reject only a binary that must spill.
func (s *Service) buildImportedSkillWrite(ctx context.Context, objectSpace, reservedID string, task *skillrepo.ParseTaskRow, f *importFields, requireSafeSpace bool) (*WriteRequest, string, []string, error) {
	zipData, err := s.readVerifiedUpload(ctx, task)
	if err != nil {
		return nil, "", nil, err
	}
	pluginID := reservedID
	if pluginID == "" {
		pluginID = s.id()
	}
	rewritten, err := skillsvc.RewriteZipPackage(bytes.NewReader(zipData), int64(len(zipData)), skillsvc.RewriteParams{
		Name:        f.name,
		Desc:        f.description,
		Version:     f.version,
		Tags:        f.tags,
		ID:          pluginID,
		RawMetadata: decodeMetadata(task.ResultMetadata),
	})
	if err != nil {
		return nil, "", nil, fmt.Errorf("rewrite skill package: %w", err)
	}
	if requireSafeSpace && !safeObjectSegment.MatchString(objectSpace) {
		return nil, "", nil, ErrInvalidRequest
	}
	// Expand the rewritten package into a flat attachment tree: text inline as
	// raw, binary/oversize spilled to the managed prefix under deterministic
	// keys. The rewritten SKILL.md (frontmatter injected) is the entry document.
	attachments, uploaded, _, err := s.buildSkillAttachmentTree(ctx, objectSpace, pluginID, rewritten.ZipBytes, rewritten.SkillMD, nil)
	if err != nil {
		return nil, "", nil, err
	}
	req, err := buildImportWriteRequest(f, attachments)
	if err != nil {
		s.deleteObjects(ctx, uploaded...)
		return nil, "", nil, err
	}
	return req, pluginID, uploaded, nil
}

// restoreWriteRequest rebuilds the write request that reproduces a plugin's
// current persisted state, used to undo a committed import update when the
// follow-up publish fails. The stored manifest/package/tags are already
// canonical, so feeding them back through Update is idempotent; relations are
// resubmitted by ID so the sync keeps them rather than deleting them.
func restoreWriteRequest(old *model.Plugin, rels []model.PluginRelation) WriteRequest {
	visibility := old.Visibility
	relations := make([]RelationRequest, 0, len(rels))
	for _, r := range rels {
		relations = append(relations, RelationRequest{
			ID:             r.ID,
			SourcePluginID: r.SourcePluginID,
			TargetPluginID: r.TargetPluginID,
			Type:           r.Type,
			SortOrder:      r.SortOrder,
			Data:           r.Data,
		})
	}
	return WriteRequest{
		Name:       old.Name,
		Type:       old.Type,
		CategoryID: old.CategoryID,
		Tags:       old.Tags,
		Publisher:  old.Publisher,
		Icon:       old.Icon,
		Visibility: visibility,
		Manifest:   old.Manifest,
		Package:    old.Package,
		Relations:  relations,
	}
}

func decodeMetadata(raw json.RawMessage) map[string]any {
	if len(raw) == 0 {
		return nil
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func (s *Service) deleteObjects(ctx context.Context, keys ...string) {
	cctx := context.WithoutCancel(ctx)
	for _, key := range keys {
		_ = s.storage.DeleteObject(cctx, key)
	}
}

// readVerifiedUpload downloads the parse task's temporary archive and verifies
// its recorded size and SHA-256 before any byte of it is trusted.
func (s *Service) readVerifiedUpload(ctx context.Context, task *skillrepo.ParseTaskRow) ([]byte, error) {
	if task.FileURL == "" || task.FileSize <= 0 || task.FileSHA256 == "" {
		return nil, ErrInvalidParseTask
	}
	if task.FileSize > s.maxArchiveBytes {
		return nil, ErrTooLarge
	}
	body, err := s.storage.GetObject(ctx, task.FileURL)
	if err != nil {
		return nil, fmt.Errorf("download uploaded archive: %w", err)
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, task.FileSize+1))
	if err != nil {
		return nil, fmt.Errorf("read uploaded archive: %w", err)
	}
	if int64(len(data)) != task.FileSize {
		return nil, ErrInvalidParseTask
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), task.FileSHA256) {
		return nil, ErrInvalidParseTask
	}
	return data, nil
}

func buildImportWriteRequest(f *importFields, attachments []map[string]any) (*WriteRequest, error) {
	tagsJSON, err := canonicalJSONValue(nonNilTags(f.tags))
	if err != nil {
		return nil, ErrInvalidRequest
	}
	draft := map[string]any{
		"$schema":     manifestSchema,
		"plugin_name": f.pluginName,
		"plugin_type": string(model.PluginTypeSkill),
		"name":        f.name,
		"description": f.description,
		"labels":      nonNilTags(f.tags),
		"examples":    []any{},
	}
	draftRaw, err := canonicalJSONValue(draft)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	manifest, canonicalTags, err := CanonicalizeManifest(f.pluginName, model.PluginTypeSkill, tagsJSON, draftRaw)
	if err != nil {
		return nil, err
	}
	pkg, err := json.Marshal(map[string]any{"$schema": packageSchema, "attachments": attachments})
	if err != nil {
		return nil, ErrInvalidRequest
	}
	return &WriteRequest{
		Name:       f.pluginName,
		Type:       model.PluginTypeSkill,
		CategoryID: f.categoryID,
		Tags:       canonicalTags,
		Icon:       f.icon,
		Visibility: f.visibility,
		Manifest:   manifest,
		Package:    pkg,
	}, nil
}

func nonNilTags(tags []string) []string {
	if tags == nil {
		return []string{}
	}
	return tags
}
