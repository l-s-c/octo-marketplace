// Skill artifact reads: SKILL.md text and the packaged zip of a visible skill
// Plugin. Import-created Plugins carry both natively (raw SKILL.md attachment
// + managed-prefix package.zip); backfilled Plugins only carry legacy object
// pointers inside skill/ref.json, which the managed-prefix attachment readers
// cannot serve — these endpoints bridge both shapes behind authentication
// instead of handing out presigned URLs.

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"go.uber.org/zap"
)

// skillRefDocument is the persisted skill/ref.json shape shared by backfill
// and import. Top-level backfilled skills carry the legacy zip pointer in
// file_url; embedded snapshot skills carry object_key/zip_object_key.
type skillRefDocument struct {
	FileName     string `json:"file_name"`
	FileSize     int64  `json:"file_size"`
	FileURL      string `json:"file_url"`
	ObjectKey    string `json:"object_key"`
	ZipObjectKey string `json:"zip_object_key"`
}

func (r skillRefDocument) zipKey() string {
	if r.ZipObjectKey != "" {
		return r.ZipObjectKey
	}
	return r.FileURL
}

func (s *Service) skillRef(p *model.Plugin) skillRefDocument {
	var ref skillRefDocument
	if raw, ok := rawAttachmentContent(p.Package, "skill/ref.json"); ok {
		_ = json.Unmarshal([]byte(raw), &ref)
	}
	return ref
}

// SkillMarkdown returns the SKILL.md text of a visible skill Plugin. The
// authoritative document is the object referenced by skill/ref.json when it
// exists (snapshot skills inline only a stub entry file); the inlined raw
// attachment is the fallback for import-created and inline-only packages.
func (s *Service) SkillMarkdown(ctx context.Context, caller Caller, pluginID string) (string, error) {
	if validateCaller(caller) != nil {
		return "", ErrInvalidRequest
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return "", err
	}
	p, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
	if err != nil {
		return "", mapStoreError(err)
	}
	if p.Type != model.PluginTypeSkill {
		return "", ErrInvalidRequest
	}
	ref := s.skillRef(p)
	if s.storage != nil && trustedArtifactKey(ref.ObjectKey, p.SpaceID) {
		body, err := s.storage.GetObject(ctx, ref.ObjectKey)
		if err == nil {
			defer body.Close()
			data, err := io.ReadAll(io.LimitReader(body, s.maxAttachmentBytes))
			if err != nil {
				return "", fmt.Errorf("read skill markdown: %w", err)
			}
			return string(data), nil
		}
	}
	if raw, ok := rawAttachmentContent(p.Package, "SKILL.md"); ok {
		return raw, nil
	}
	return "", ErrNotFound
}

// OpenSkillPackage streams the packaged zip of a visible skill Plugin: the
// managed skill/package.zip storage attachment when present, else the legacy
// zip object referenced by skill/ref.json. A successful open bumps the plugin
// download counter (best-effort).
func (s *Service) OpenSkillPackage(ctx context.Context, caller Caller, pluginID string) (*AttachmentDownload, error) {
	if validateCaller(caller) != nil || s.storage == nil {
		return nil, ErrInvalidRequest
	}
	storageID, err := parseStorageID(pluginID)
	if err != nil {
		return nil, err
	}
	p, _, err := s.repo.GetWithRelations(ctx, scope(caller), storageID)
	if err != nil {
		return nil, mapStoreError(err)
	}
	if p.Type != model.PluginTypeSkill {
		return nil, ErrInvalidRequest
	}
	ref := s.skillRef(p)
	key := ""
	if managed, ok := storageAttachmentKey(p.Package, "skill/package.zip"); ok && p.SpaceID != nil && validReferencedObjectKey(managed, *p.SpaceID) {
		key = managed
	} else if trustedArtifactKey(ref.zipKey(), p.SpaceID) {
		// Legacy pointer written by the deterministic backfill; the trusted-key
		// rule keeps caller-forged pointers into other Spaces out.
		key = ref.zipKey()
	}
	if key == "" {
		return nil, ErrNotFound
	}
	info, err := s.storage.StatObject(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("stat skill package: %w", err)
	}
	if info.Size <= 0 || info.Size > s.maxArchiveBytes {
		return nil, ErrTooLarge
	}
	body, err := s.storage.GetObject(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("open skill package: %w", err)
	}
	fileName := strings.TrimSpace(ref.FileName)
	if fileName == "" {
		fileName = p.Name + ".zip"
	}
	s.trackDownload(ctx, p.ID)
	return &AttachmentDownload{Body: body, Path: fileName, ContentType: "application/zip", Size: info.Size}, nil
}

// trackDownload bumps the plugin download counter; best-effort and detached
// from the request context, mirroring trackInstall.
func (s *Service) trackDownload(ctx context.Context, pluginID string) {
	if s.metrics == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trackTimeout)
	defer cancel()
	if err := s.metrics.TrackDownload(cctx, "plugin", pluginID); err != nil {
		logging.Warn("download_metric_track_failed",
			zap.String("operation", "plugin.download.track"),
			zap.String("resource_id", pluginID),
			logging.ErrorField(err),
		)
	}
}

// trustedArtifactKey validates an object key read from persisted plugin_json
// before it is fetched. skill/ref.json is caller-writable through upsert, so
// pointers are only honored inside two namespaces: the plugin's OWN managed
// Space prefix (import-created artifacts), or the legacy read-only roots the
// deterministic backfill migrated — skills/ (top-level skills), experts/ and
// squads/ (embedded member skills) — which the unified write path can never
// produce objects under. Everything else — notably another Space's managed
// prefix — fails closed.
func trustedArtifactKey(key string, spaceID *string) bool {
	if key == "" || len(key) > 512 || strings.ContainsAny(key, "\\\x00?#") || strings.Contains(key, "://") || strings.HasPrefix(key, "/") {
		return false
	}
	clean := path.Clean(key)
	if clean != key || clean == "." || strings.HasPrefix(clean, "../") {
		return false
	}
	if spaceID != nil && safeObjectSegment.MatchString(*spaceID) && strings.HasPrefix(key, approvedAttachmentPrefix(*spaceID)) {
		return true
	}
	// The managed root outside the plugin's own Space is never trusted.
	if strings.HasPrefix(key, "plugins/") {
		return false
	}
	return strings.HasPrefix(key, "skills/") || strings.HasPrefix(key, "experts/") || strings.HasPrefix(key, "squads/")
}
