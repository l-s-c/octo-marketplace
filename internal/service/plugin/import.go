// Plugin import turns a completed legacy upload-parse task into a skill
// Plugin: the rewritten package zip is expanded into a flat attachment tree
// (one attachment per file — text inlined as raw, binary/oversize files
// uploaded to the managed plugins/<space>/attachments/ prefix under
// deterministic keys and referenced as storage attachments). Everything funnels
// through Create/Update, so validation, secret scanning, hashing, and audit are
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

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	skillrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/skill"
	skillsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/skill"
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
	if strings.TrimSpace(p.PluginID) != "" {
		updateID, err = parseStorageID(p.PluginID)
		if err != nil {
			return nil, err
		}
		old, _, err := s.repo.GetWithRelations(ctx, scope(caller), updateID)
		if err != nil {
			return nil, mapStoreError(err)
		}
		if old.OwnerUID != caller.UID || old.Type != model.PluginTypeSkill {
			return nil, ErrNotFound
		}
	}

	fields, err := resolveImportFields(p, task, caller.IsSystemAdmin)
	if err != nil {
		return nil, err
	}

	// Consume first: the optimistic status flip is the duplicate-import lock.
	// Every failure below releases it (best-effort) so the upload is retryable.
	if err := s.parseTasks.MarkParseTaskConsumed(ctx, task.ID, caller.UID, caller.SpaceID, ""); err != nil {
		if errors.Is(err, skillrepo.ErrParseTaskAlreadyConsumed) {
			return nil, ErrInvalidParseTask
		}
		return nil, fmt.Errorf("consume parse task: %w", err)
	}
	detail, err := s.importConsumedTask(ctx, caller, task, fields, updateID)
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
}

func resolveImportFields(p ImportParams, task *skillrepo.ParseTaskRow, systemAdmin bool) (*importFields, error) {
	f := &importFields{
		pluginName:  strings.TrimSpace(p.PluginName),
		name:        strings.TrimSpace(p.Name),
		description: strings.TrimSpace(p.Description),
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

func (s *Service) importConsumedTask(ctx context.Context, caller Caller, task *skillrepo.ParseTaskRow, f *importFields, updateID string) (*Detail, error) {
	zipData, err := s.readVerifiedUpload(ctx, task)
	if err != nil {
		return nil, err
	}
	pluginID := updateID
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
		return nil, fmt.Errorf("rewrite skill package: %w", err)
	}
	if !safeObjectSegment.MatchString(caller.SpaceID) {
		return nil, ErrInvalidRequest
	}
	// Expand the rewritten package into a flat attachment tree: text inline as
	// raw, binary/oversize spilled to the managed prefix under deterministic
	// keys. The rewritten SKILL.md (frontmatter injected) is the entry document.
	attachments, uploaded, _, err := s.buildSkillAttachmentTree(ctx, caller.SpaceID, pluginID, rewritten.ZipBytes, rewritten.SkillMD)
	if err != nil {
		return nil, err
	}
	req, err := buildImportWriteRequest(f, attachments)
	if err != nil {
		s.deleteObjects(ctx, uploaded...)
		return nil, err
	}
	var detail *Detail
	if updateID == "" {
		detail, err = s.Create(ctx, caller, *req)
	} else {
		detail, err = s.Update(ctx, caller, updateID, *req)
	}
	if err != nil {
		s.deleteObjects(ctx, uploaded...)
		return nil, err
	}
	// Publish rebuilds the single default placement, keeping the Plugin
	// discoverable in the confirmed marketplace scene; a version-string
	// conflict (immutable versions) fails the import after release.
	placement := PlacementRequest{PlacementCode: "default", CategoryID: f.categoryID, Visible: true}
	if _, err := s.Publish(ctx, caller, detail.Plugin.ID, PublishRequest{Version: f.version, Changelog: nil, Placements: []PlacementRequest{placement}}); err != nil {
		if updateID == "" {
			// Create rollback: nothing else references the fresh objects yet.
			_ = s.Delete(ctx, caller, detail.Plugin.ID)
			s.deleteObjects(ctx, uploaded...)
		}
		// Update path: the document update already committed and its package
		// references the uploaded objects — deleting them would leave the live
		// Plugin pointing at nothing. Only the version snapshot is missing.
		return nil, err
	}
	return detail, nil
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
