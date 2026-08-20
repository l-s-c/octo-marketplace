// Package plugin persists the unified Plugin domain.
package plugin

import (
	"database/sql"
	"errors"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/id"
)

var (
	// ErrNotFound intentionally covers absent and out-of-scope Plugins.
	ErrNotFound = errors.New("plugin not found")
	// ErrConflict indicates an immutable version or placement uniqueness conflict.
	ErrConflict = errors.New("plugin conflict")
	// ErrInvalidRelation indicates a relation whose source/target types are incompatible.
	ErrInvalidRelation = errors.New("invalid plugin relation")
	// ErrInvalidCategory indicates a missing, inactive, or type-incompatible category.
	ErrInvalidCategory = errors.New("invalid plugin category")
	// ErrInvalidPlacement indicates a category not enabled for the Plugin type and placement.
	ErrInvalidPlacement = errors.New("invalid plugin placement")
	// ErrUnsafeConnectorData prevents copying persisted connector secret values.
	ErrUnsafeConnectorData = errors.New("unsafe connector secret data")
)

// Scope is authoritative caller context; it must never come from request data.
type Scope struct {
	CallerUID string
	SpaceID   string
}

// Repo provides Plugin persistence.
type Repo struct {
	db  *sql.DB
	now func() time.Time
	id  func() string
}

func New(db *sql.DB) *Repo {
	return &Repo{db: db, now: func() time.Time { return time.Now().UTC() }, id: id.New}
}

const visibilitySQL = `(p.visibility IN ('public','system') OR (p.space_id = ? AND (p.visibility = 'space' OR p.owner_uid = ?)))`

const pluginColumns = `p.plugin_id,p.plugin_name,p.plugin_type,p.is_embedded,p.category_id,p.tags_json,p.publisher,
 p.owner_uid,p.space_id,p.visibility,p.creator_name,p.created_by_type,p.created_by_bot_uid,p.created_by_bot_name,
 p.manifest_json,p.plugin_json,p.manifest_hash,p.plugin_hash,p.current_version_id,p.status,p.created_at,p.updated_at,p.deleted_at`

// pluginSummaryColumns omits plugin_json: list pages carry the manifest for
// display but never the full package, which can be large.
const pluginSummaryColumns = `p.plugin_id,p.plugin_name,p.plugin_type,p.is_embedded,p.category_id,p.tags_json,p.publisher,
 p.owner_uid,p.space_id,p.visibility,p.creator_name,p.created_by_type,p.created_by_bot_uid,p.created_by_bot_name,
 p.manifest_json,p.manifest_hash,p.plugin_hash,p.current_version_id,p.status,p.created_at,p.updated_at,p.deleted_at`
