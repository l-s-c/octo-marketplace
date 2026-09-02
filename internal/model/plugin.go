package model

import (
	"encoding/json"
	"time"
)

// PluginType identifies one of the unified marketplace resource kinds.
type PluginType string

const (
	PluginTypeExpert     PluginType = "expert"
	PluginTypeExpertTeam PluginType = "expert_team"
	PluginTypeSkill      PluginType = "skill"
	PluginTypeConnector  PluginType = "connector"
)

// PluginVisibility controls which authenticated scope may discover a Plugin.
type PluginVisibility string

const (
	PluginVisibilityPublic  PluginVisibility = "public"
	PluginVisibilitySpace   PluginVisibility = "space"
	PluginVisibilityPrivate PluginVisibility = "private"
	PluginVisibilitySystem  PluginVisibility = "system"
)

// PluginListingState is the listing lifecycle axis, independent of review state.
//
// visibility says WHO should see a Plugin once it is listed (a declared intent);
// listing_state says WHETHER it is listed. Splitting them is what makes a draft
// expressible: before this type `private` doubled as "draft", so saving and
// publishing were the same act.
//
// It is deliberately NOT a review-status field. Review state stays on
// plugin_review_requests so a listed v1 and an in-review v2 coexist
// (.octospec/tasks/plugin-space-review/brief.md item 26). Never add a review
// value here — "审核中" is derived by DisplayStatus from a pending request.
type PluginListingState string

const (
	// PluginListingStateDraft is never discoverable by anyone but the owner, and
	// is excluded even from the owner's marketplace grid — that exclusion is the
	// only thing distinguishing a private draft from a published private Plugin.
	PluginListingStateDraft PluginListingState = "draft"
	// PluginListingStatePublished is listed, subject to visibility.
	PluginListingStatePublished PluginListingState = "published"
	// PluginListingStateDelisted was published and was taken down by a Space
	// admin. It leaves the marketplace but stays editable and re-publishable by
	// its owner, and its current_version label stays spent.
	PluginListingStateDelisted PluginListingState = "delisted"
)

// NormalizeLegacyVisibility maps a row's PRESERVED legacy `public` visibility to
// the unified `system` global value, passing every current enum value through
// unchanged. Write paths that carry an existing row's visibility forward — an
// admin metadata edit (adminEffectiveWrite) and a container reupload (RebuildGraph
// re-deriving the locked row's visibility) — route it through this single helper
// so a preserved `public` revalidates as `system` and the row stops carrying the
// value retired on the write path.
func NormalizeLegacyVisibility(v PluginVisibility) PluginVisibility {
	if v == PluginVisibilityPublic {
		return PluginVisibilitySystem
	}
	return v
}

// Plugin is the authoritative mutable current state of a unified Plugin.
type Plugin struct {
	ID         string
	Name       string
	Type       PluginType
	IsEmbedded bool
	CategoryID *string
	Tags       json.RawMessage
	Publisher  string
	OwnerUID   string
	SpaceID    *string
	Visibility PluginVisibility
	// ListingState is written by exactly four paths — insertPlugin, ApproveReview,
	// PublishPlugin, and DelistPlugin. An ordinary save deliberately cannot change
	// it, so the UPDATE statements omit the column entirely.
	ListingState     PluginListingState
	CreatorName      string
	CreatedByType    string
	CreatedByBotUID  *string
	CreatedByBotName *string
	// Icon is the stored write-canonical value: a persistent public URL or a
	// managed storage object key. IconURL is derived at read time (presigned
	// when Icon is an object key) and never persisted, so clients can render
	// IconURL but must echo Icon back on updates.
	Icon    string
	IconURL string
	// ToolCount is materialized from the connector/tools.json attachment on the
	// write path because list queries never load plugin_json.
	ToolCount int
	// MemberCount is derived from live expert_team_expert relations for list
	// responses; it is never persisted.
	MemberCount int
	// View/Install/DownloadCount are read-only counters resolved from
	// resource_metrics (resource_type "plugin"); they are never written here.
	ViewCount     int
	InstallCount  int
	DownloadCount int
	Manifest      json.RawMessage
	Package       json.RawMessage
	// AttachmentKeys is the host-private sidecar for storage attachments: a JSON
	// object mapping a package attachment path to its managed object key. The
	// octo-plugin-lib 2.0 package forbids host fields inside attachments, so the
	// object key lives here instead of a `storage_uri` in Package, and is never
	// part of plugin_hash. NULL/empty for rows without spilled storage files.
	AttachmentKeys   json.RawMessage
	ManifestHash     string
	PluginHash       string
	CurrentVersionID *string
	CurrentVersion   *string
	Status           int
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// PluginRelation is one directed, one-level composition edge.
type PluginRelation struct {
	ID               string
	SourcePluginID   string
	SourcePluginType PluginType
	TargetPluginID   string
	TargetPluginType PluginType
	Type             string
	SortOrder        int
	Data             json.RawMessage
	Status           int
	CreatedBy        string
	CreatedAt        time.Time
	UpdatedAt        time.Time
	DeletedAt        *time.Time
}

// PluginAuditLog is an append-only operation record.
type PluginAuditLog struct {
	ID               string
	PluginID         string
	PluginType       PluginType
	Action           string
	OperatorID       string
	OperatorName     string
	RequestID        string
	BeforeHash       *string
	AfterHash        *string
	ManifestSnapshot json.RawMessage
	PluginSnapshot   json.RawMessage
	Remark           *string
	CreatedAt        time.Time
}

// PluginVersion is an immutable published snapshot.
type PluginVersion struct {
	ID         string
	PluginID   string
	PluginType PluginType
	Version    string
	Manifest   json.RawMessage
	Package    json.RawMessage
	// AttachmentKeys is the version snapshot's copy of the storage-attachment
	// path->object-key sidecar; see Plugin.AttachmentKeys.
	AttachmentKeys json.RawMessage
	ManifestHash   string
	PluginHash     string
	Relations      json.RawMessage
	Changelog      *string
	CreatedBy      string
	CreatedAt      time.Time
}

// PluginPlacement configures one Plugin at a marketplace placement point.
type PluginPlacement struct {
	ID            string
	PlacementCode string
	PluginID      string
	CategoryID    *string
	Visible       bool
	SortOrder     int
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// PluginCategory is a category returned for a placement and Plugin type.
type PluginCategory struct {
	ID          string
	Name        string
	IconKey     string
	PluginTypes json.RawMessage
	SortOrder   int
	Status      int
	PluginCount int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
