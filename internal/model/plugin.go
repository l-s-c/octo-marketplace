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

// Plugin is the authoritative mutable current state of a unified Plugin.
type Plugin struct {
	ID               string
	Name             string
	Type             PluginType
	IsEmbedded       bool
	CategoryID       *string
	Tags             json.RawMessage
	Publisher        string
	OwnerUID         string
	SpaceID          *string
	Visibility       PluginVisibility
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
	ViewCount        int
	InstallCount     int
	DownloadCount    int
	Manifest         json.RawMessage
	Package          json.RawMessage
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
	ID           string
	PluginID     string
	PluginType   PluginType
	Version      string
	Manifest     json.RawMessage
	Package      json.RawMessage
	ManifestHash string
	PluginHash   string
	Relations    json.RawMessage
	Changelog    *string
	CreatedBy    string
	CreatedAt    time.Time
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
