// Plugin import turns a completed legacy upload-parse task into a skill
// Plugin: the rewritten package zip and SKILL.md land in the managed
// plugins/<space>/attachments/ prefix (so archive and attachment reads work
// natively), SKILL.md is inlined as a raw attachment for detail pages, and a
// skill/ref.json attachment carries the object pointers the Loop install path
// consumes. Everything funnels through Create/Update, so validation, secret
// scanning, hashing, and audit are identical to a direct upsert.

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
	prefix := approvedAttachmentPrefix(caller.SpaceID)
	zipKey := prefix + s.id() + ".zip"
	mdKey := prefix + s.id() + ".md"
	if err := s.storage.PutObject(ctx, zipKey, bytes.NewReader(rewritten.ZipBytes), rewritten.ZipSize, "application/zip"); err != nil {
		return nil, fmt.Errorf("upload skill package: %w", err)
	}
	if err := s.storage.PutObject(ctx, mdKey, bytes.NewReader(rewritten.SkillMD), int64(len(rewritten.SkillMD)), "text/markdown; charset=utf-8"); err != nil {
		s.deleteObjects(ctx, zipKey)
		return nil, fmt.Errorf("upload skill markdown: %w", err)
	}

	req, err := buildImportWriteRequest(f, rewritten, zipKey, mdKey, task.FileName)
	if err != nil {
		s.deleteObjects(ctx, zipKey, mdKey)
		return nil, err
	}
	var detail *Detail
	if updateID == "" {
		detail, err = s.Create(ctx, caller, *req)
	} else {
		detail, err = s.Update(ctx, caller, updateID, *req)
	}
	if err != nil {
		s.deleteObjects(ctx, zipKey, mdKey)
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
			s.deleteObjects(ctx, zipKey, mdKey)
		}
		// Update path: the document update already committed and its package
		// references zipKey/mdKey — deleting them would leave the live Plugin
		// pointing at nothing. Only the version snapshot is missing.
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

func buildImportWriteRequest(f *importFields, rewritten *skillsvc.RewriteResult, zipKey, mdKey, uploadedName string) (*WriteRequest, error) {
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
	fileName := strings.TrimSpace(uploadedName)
	if fileName == "" {
		fileName = "skill.zip"
	}
	ref := map[string]any{
		"file_name":      fileName,
		"file_sha256":    rewritten.ZipSHA256,
		"file_size":      rewritten.ZipSize,
		"files":          []string{},
		"object_key":     mdKey,
		"zip_object_key": zipKey,
	}
	refRaw, err := json.Marshal(ref)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	attachments := []map[string]any{
		{"path": "manifest.json", "content_type": "raw", "mime_type": "application/json", "raw_content": string(manifest)},
		{"path": "SKILL.md", "content_type": "raw", "mime_type": "text/markdown", "raw_content": string(rewritten.SkillMD)},
		{"path": "skill/ref.json", "content_type": "raw", "mime_type": "application/json", "raw_content": string(refRaw)},
		{"path": "skill/package.zip", "content_type": "storage", "mime_type": "application/zip", "storage_uri": zipKey, "content_size": rewritten.ZipSize, "content_hash": "sha256:" + rewritten.ZipSHA256},
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
