// Plugin install maps a unified expert / expert_team Plugin onto the Loop
// provisioning flow owned by the expert service: attachments become the agent
// spec, expert_skill relations become packaged skill references, and
// expert_team_expert relations become squad members. Fleet authorizes every
// call as the end user (forwarded token), so this layer does not re-check
// workspace membership or runtime access.

package plugin

import (
	"context"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	expertsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/expert"
	"go.uber.org/zap"
)

// Provisioner is the Loop provisioning surface, implemented by
// *expertsvc.Service; it owns timeouts, file budgets, and rollback.
type Provisioner interface {
	ProvisionAgentFromSpec(context.Context, expertsvc.InstallInput, expertsvc.ProvisionAgentSpec) (string, error)
	ProvisionSquadFromSpec(context.Context, expertsvc.InstallInput, *model.Squad) (expertsvc.InstallSquadResult, error)
}

// InstallTracker mirrors the expert service's metrics seam so install and
// download counts accrue to resource_type "plugin" without importing the
// metrics package.
type InstallTracker interface {
	TrackInstall(ctx context.Context, resourceType, resourceID string) error
	TrackDownload(ctx context.Context, resourceType, resourceID string) error
}

// WithProvisioner wires the Loop provisioner (chainable at construction).
func (s *Service) WithProvisioner(p Provisioner) *Service {
	s.provisioner = p
	return s
}

// WithMetrics wires the install counter; nil keeps tracking a no-op.
func (s *Service) WithMetrics(m InstallTracker) *Service {
	s.metrics = m
	return s
}

type InstallParams struct {
	WorkspaceID string
	RuntimeID   string
	// Token is the caller's forwarded credential, re-read from the request
	// header by the handler because middleware discards it.
	Token string
}

// InstallOutcome carries exactly one of the created Loop resource IDs,
// matching the Plugin's type.
type InstallOutcome struct {
	AgentID string
	SquadID string
}

func (s *Service) Install(ctx context.Context, caller Caller, pluginID string, p InstallParams) (*InstallOutcome, error) {
	if validateCaller(caller) != nil {
		return nil, ErrInvalidRequest
	}
	if s.provisioner == nil {
		return nil, expertsvc.ErrFleetNotConfigured
	}
	if strings.TrimSpace(p.WorkspaceID) == "" || strings.TrimSpace(p.RuntimeID) == "" {
		return nil, ErrInvalidRequest
	}
	detail, err := s.Detail(ctx, caller, pluginID, true)
	if err != nil {
		return nil, err
	}
	in := expertsvc.InstallInput{WorkspaceID: strings.TrimSpace(p.WorkspaceID), RuntimeID: strings.TrimSpace(p.RuntimeID), SpaceID: caller.SpaceID, Token: p.Token}
	switch detail.Plugin.Type {
	case model.PluginTypeExpert:
		spec, err := s.agentSpecFromPlugin(ctx, caller, detail)
		if err != nil {
			return nil, err
		}
		agentID, err := s.provisioner.ProvisionAgentFromSpec(ctx, in, *spec)
		if err != nil {
			return nil, err
		}
		s.trackInstall(ctx, detail.Plugin.ID)
		return &InstallOutcome{AgentID: agentID}, nil
	case model.PluginTypeExpertTeam:
		squad, err := s.squadFromPlugin(ctx, caller, detail)
		if err != nil {
			return nil, err
		}
		result, err := s.provisioner.ProvisionSquadFromSpec(ctx, in, squad)
		if err != nil {
			return nil, err
		}
		s.trackInstall(ctx, detail.Plugin.ID)
		return &InstallOutcome{SquadID: result.SquadID}, nil
	default:
		return nil, ErrInvalidRequest
	}
}

// agentSpecFromPlugin materializes one expert Plugin into a provisioning spec:
// the AGENTS.md instruction entry and root mcp.json from its package
// attachments, packaged skills from its expert_skill relation targets.
func (s *Service) agentSpecFromPlugin(ctx context.Context, caller Caller, detail *Detail) (*expertsvc.ProvisionAgentSpec, error) {
	p := detail.Plugin
	instruction, _ := rawAttachmentContent(p.Package, "AGENTS.md")
	mcpConfig, _ := rawAttachmentContent(p.Package, "mcp.json")
	skills, err := s.skillRefsFromRelations(ctx, caller, detail.Relations)
	if err != nil {
		return nil, err
	}
	return &expertsvc.ProvisionAgentSpec{
		Name:        p.Name,
		Summary:     manifestDescription(p.Manifest),
		Instruction: instruction,
		MCPConfig:   mcpConfig,
		Skills:      skills,
	}, nil
}

// squadFromPlugin materializes one expert_team Plugin into the squad model the
// expert provisioner consumes: leader and strategies from team/config.json,
// members from expert_team_expert relations (relation_json carries role,
// is_leader, and member_key), each member's spec from its own Plugin.
func (s *Service) squadFromPlugin(ctx context.Context, caller Caller, detail *Detail) (*model.Squad, error) {
	p := detail.Plugin
	// Contract layout: the team package is a single AGENTS.md document that
	// carries the collaboration/dispatch prose; it becomes the Loop squad's
	// instructions verbatim. Leadership comes from member relations
	// (relation_json is_leader), not from a config attachment.
	instructions, _ := rawAttachmentContent(p.Package, "AGENTS.md")
	memberRelations := relationsOfType(detail.Relations, "expert_team_expert")
	members := make([]model.SquadMember, 0, len(memberRelations))
	for _, rel := range memberRelations {
		memberDetail, err := s.Detail(ctx, caller, rel.TargetPluginID, true)
		if err != nil {
			return nil, err
		}
		member, err := s.squadMemberFromPlugin(ctx, caller, rel, memberDetail)
		if err != nil {
			return nil, err
		}
		members = append(members, *member)
	}
	return &model.Squad{
		ID:           p.ID,
		Name:         p.Name,
		Summary:      manifestDescription(p.Manifest),
		Instructions: instructions,
		Members:      members,
	}, nil
}

func (s *Service) squadMemberFromPlugin(ctx context.Context, caller Caller, rel model.PluginRelation, memberDetail *Detail) (*model.SquadMember, error) {
	mp := memberDetail.Plugin
	// relation_json is authoritative for squad wiring; expert/context.json is
	// the snapshot fallback carried inside the member package.
	var wiring struct {
		MemberKey string `json:"member_key"`
		Role      string `json:"role"`
		IsLeader  bool   `json:"is_leader"`
	}
	if len(rel.Data) > 0 {
		_ = json.Unmarshal(rel.Data, &wiring)
	}
	if wiring.MemberKey == "" && wiring.Role == "" {
		if raw, ok := rawAttachmentContent(mp.Package, "expert/context.json"); ok {
			_ = json.Unmarshal([]byte(raw), &wiring)
		}
	}
	instruction, _ := rawAttachmentContent(mp.Package, "AGENTS.md")
	mcpConfig, _ := rawAttachmentContent(mp.Package, "mcp.json")
	skills, err := s.skillRefsFromRelations(ctx, caller, memberDetail.Relations)
	if err != nil {
		return nil, err
	}
	return &model.SquadMember{
		MemberKey:   wiring.MemberKey,
		Name:        mp.Name,
		Role:        wiring.Role,
		IsLeader:    wiring.IsLeader,
		Instruction: instruction,
		MCPConfig:   mcpConfig,
		Skills:      skills,
	}, nil
}

// skillRefsFromRelations loads every expert_skill relation target and resolves
// it into a SkillRef the provisioner installs. Tree-shaped skills resolve their
// SKILL.md and supporting text files inline (fetching any storage-backed text
// through this service's object store); legacy pointer skills keep their
// object/zip keys for the provisioner to fetch.
func (s *Service) skillRefsFromRelations(ctx context.Context, caller Caller, relations []model.PluginRelation) ([]model.SkillRef, error) {
	skillRelations := relationsOfType(relations, "expert_skill")
	refs := make([]model.SkillRef, 0, len(skillRelations))
	// One aggregate byte budget shared across every skill in the install, so an
	// expert_team fanning out to many skills cannot multiply the resident-memory
	// bound (A4). Mirrors the download path's maxArchiveBytes ceiling.
	remaining := s.maxArchiveBytes
	for _, rel := range skillRelations {
		target, err := s.Detail(ctx, caller, rel.TargetPluginID, false)
		if err != nil {
			return nil, err
		}
		refs = append(refs, s.skillRefFromPlugin(ctx, target.Plugin, &remaining))
	}
	return refs, nil
}

// skillRefFromPlugin resolves a skill Plugin into a SkillRef. A tree-shaped
// package (no skill/ref.json pointer) resolves its SKILL.md and supporting text
// files inline; a legacy package keeps its object/zip pointers for the
// provisioner. Storage-backed supporting files are fetched here and included
// only when they are UTF-8 text (binaries are skipped, mirroring the fleet
// text-only skill-file store).
func (s *Service) skillRefFromPlugin(ctx context.Context, p *model.Plugin, remaining *int64) model.SkillRef {
	ref := model.SkillRef{Name: p.Name}
	var legacy struct {
		FileName     string   `json:"file_name"`
		FileSize     int64    `json:"file_size"`
		Files        []string `json:"files"`
		ObjectKey    string   `json:"object_key"`
		ZipObjectKey string   `json:"zip_object_key"`
	}
	hasRef := false
	if raw, ok := rawAttachmentContent(p.Package, "skill/ref.json"); ok && json.Unmarshal([]byte(raw), &legacy) == nil {
		hasRef = true
		// skill/ref.json is caller-writable, so its object keys are honored only
		// when they sit in the plugin's own managed Space prefix; a forged
		// cross-Space or arbitrary bucket key is dropped rather than fetched by
		// the provisioner. Legacy backfilled skills resolve once expand-skills has
		// rewritten them into own-Space trees.
		if trustedArtifactKey(legacy.ObjectKey, p.SpaceID) {
			ref.ObjectKey = legacy.ObjectKey
		}
		if trustedArtifactKey(legacy.ZipObjectKey, p.SpaceID) {
			ref.ZipObjectKey = legacy.ZipObjectKey
		}
		ref.FileName = legacy.FileName
		ref.FileSize = legacy.FileSize
		ref.Files = legacy.Files
	}
	if key, ok := storageAttachmentKey(p.Package, "skill/package.zip"); ok {
		hasRef = true
		// Same own-Space scoping as skill/ref.json above (Q5): the managed zip key
		// is honored only inside this plugin's own prefix, matching legacyZipKey /
		// migrationZipKey, so a forged or cross-Space key is never handed to the
		// provisioner to fetch.
		if ref.ZipObjectKey == "" && p.SpaceID != nil && validReferencedObjectKey(key, *p.SpaceID) {
			ref.ZipObjectKey = key
		}
	}
	// Legacy pointer shape: leave object/zip keys for the provisioner to fetch.
	if hasRef {
		return ref
	}
	// Tree shape: resolve SKILL.md and supporting text files from the attachments.
	if md, ok := rawAttachmentContent(p.Package, "SKILL.md"); ok {
		ref.Markdown = md
	}
	// Cap the supporting files materialized here BEFORE fetching, mirroring the
	// downstream per-skill fan-out budget (expert.maxSkillFilesPerSkill). Counting
	// every processed attachment — not just accepted text — bounds both the
	// GetObject fan-out and the in-memory SupportingFiles slice for a plugin whose
	// plugin_json packs thousands of storage attachments. The shared *remaining
	// byte budget additionally caps aggregate resident bytes across the install.
	processed := 0
	for _, a := range decodePackageAttachments(p.Package) {
		if a.Path == "SKILL.md" {
			continue
		}
		if processed >= maxInstallSupportingFiles || *remaining <= 0 {
			break
		}
		processed++
		if a.ContentType == "raw" {
			if utf8.ValidString(a.RawContent) && int64(len(a.RawContent)) <= *remaining {
				ref.SupportingFiles = append(ref.SupportingFiles, model.SkillFile{Path: a.Path, Content: a.RawContent})
				*remaining -= int64(len(a.RawContent))
			}
			continue
		}
		if content, ok := s.readStorageText(ctx, p.SpaceID, a.StorageURI); ok && int64(len(content)) <= *remaining {
			ref.SupportingFiles = append(ref.SupportingFiles, model.SkillFile{Path: a.Path, Content: content})
			*remaining -= int64(len(content))
		}
	}
	return ref
}

// maxInstallSupportingFiles bounds how many non-SKILL.md attachments one skill
// contributes to an install, applied before any storage fetch. It mirrors the
// provisioner's per-skill file budget so this pre-fetch cap can never admit more
// than the downstream stage would accept anyway.
const maxInstallSupportingFiles = 50

// readStorageText fetches a storage attachment from this plugin's managed prefix
// and returns it only when it is valid UTF-8 text (binary attachments are
// skipped, matching the text-only downstream skill-file store).
func (s *Service) readStorageText(ctx context.Context, spaceID *string, key string) (string, bool) {
	if s.storage == nil || spaceID == nil || !validReferencedObjectKey(key, *spaceID) {
		return "", false
	}
	body, err := s.storage.GetObject(ctx, key)
	if err != nil {
		return "", false
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, s.maxAttachmentBytes))
	if err != nil || !utf8.Valid(data) {
		return "", false
	}
	return string(data), true
}

func relationsOfType(relations []model.PluginRelation, relationType string) []model.PluginRelation {
	out := make([]model.PluginRelation, 0, len(relations))
	for _, rel := range relations {
		if rel.Type == relationType && rel.Status == 1 && rel.DeletedAt == nil {
			out = append(out, rel)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].SortOrder < out[j].SortOrder })
	return out
}

func manifestDescription(manifest json.RawMessage) string {
	var doc struct {
		Description string `json:"description"`
	}
	_ = json.Unmarshal(manifest, &doc)
	return doc.Description
}

// trackTimeout bounds the best-effort install counter bump; see the expert
// service for the rationale (a stalled Redis must not delay the response).
const trackTimeout = 2 * time.Second

// trackInstall bumps the plugin install counter after a successful provision.
// Best-effort and detached from the request context; failures are logged.
func (s *Service) trackInstall(ctx context.Context, pluginID string) {
	if s.metrics == nil {
		return
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), trackTimeout)
	defer cancel()
	if err := s.metrics.TrackInstall(cctx, "plugin", pluginID); err != nil {
		logging.Warn("install_metric_track_failed",
			zap.String("operation", "plugin.install.track"),
			zap.String("resource_id", pluginID),
			logging.ErrorField(err),
		)
	}
}
