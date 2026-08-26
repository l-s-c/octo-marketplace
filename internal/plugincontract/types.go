package plugincontract

import (
	"encoding/json"
	"time"
)

const (
	ManifestSchemaID = "cowork-plugin-manifest-1.0.json"
	PackageSchemaID  = "cowork-plugin-package-1.0.json"
)

type Type string

const (
	TypeExpert     Type = "expert"
	TypeSkill      Type = "skill"
	TypeExpertTeam Type = "expert_team"
	TypeConnector  Type = "connector"
)

type RelationType string

const (
	RelationExpertTeamExpert RelationType = "expert_team_expert"
	RelationExpertSkill      RelationType = "expert_skill"
	RelationExpertConnector  RelationType = "expert_connector"
)

type ContentType string

const (
	ContentRaw     ContentType = "raw"
	ContentStorage ContentType = "storage"
)

type ConnectorType string

const (
	ConnectorMCP           ConnectorType = "mcp"
	ConnectorCLI           ConnectorType = "cli"
	ConnectorSkillOnly     ConnectorType = "skill-only"
	ConnectorOpenConnector ConnectorType = "openconnector"
)

// Plugin mirrors the two-table public logical contract. ManifestJSON and
// PluginJSON remain raw JSON so extensions are not lost by a Go round trip.
type Plugin struct {
	PluginID     string          `json:"plugin_id"`
	PluginName   string          `json:"plugin_name"`
	PluginType   Type            `json:"plugin_type"`
	ManifestJSON json.RawMessage `json:"manifest_json"`
	PluginJSON   json.RawMessage `json:"plugin_json"`
	PluginHash   string          `json:"plugin_hash,omitempty"`
	Status       uint8           `json:"status"`
	CreatedAt    time.Time       `json:"created_at"`
	UpdatedAt    time.Time       `json:"updated_at"`
}

type Relation struct {
	RelationID     string       `json:"relation_id"`
	SourcePluginID string       `json:"source_plugin_id"`
	TargetPluginID string       `json:"target_plugin_id"`
	RelationType   RelationType `json:"relation_type"`
	Status         uint8        `json:"status"`
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
}

type Manifest struct {
	Schema      string    `json:"$schema"`
	PluginName  string    `json:"plugin_name"`
	PluginType  Type      `json:"plugin_type"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Labels      []string  `json:"labels,omitempty"`
	Examples    []Example `json:"examples,omitempty"`
}

type Example struct {
	Title string `json:"title"`
	Input string `json:"input"`
}

type Package struct {
	Schema      string       `json:"$schema"`
	Connector   *Connector   `json:"connector,omitempty"`
	Attachments []Attachment `json:"attachments"`
}

type Connector struct {
	Type   ConnectorType `json:"type"`
	Source string        `json:"source"`
}

// Attachment either embeds UTF-8 text (raw) or points at a host-private
// immutable object (storage). StorageURI is never a cross-host transfer URL.
type Attachment struct {
	Path        string      `json:"path"`
	ContentType ContentType `json:"content_type"`
	MIMEType    string      `json:"mime_type"`
	RawContent  *string     `json:"raw_content,omitempty"`
	StorageURI  string      `json:"storage_uri,omitempty"`
	ContentSize *int64      `json:"content_size,omitempty"`
	ContentHash string      `json:"content_hash,omitempty"`
}
