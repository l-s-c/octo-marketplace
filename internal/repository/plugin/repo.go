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
	// ErrGraphTooLarge indicates a plugin's transitive relation closure exceeds the
	// per-request node cap. The read path fails closed (rather than truncating) so
	// callers never render a partially-missing squad/agent.
	ErrGraphTooLarge = errors.New("plugin graph exceeds node cap")
)

// maxGraphNodes caps the total number of related (non-root) plugins returned by
// a single detail_graph response. It matches maxInstallRelationTargets so the
// read surface cannot describe a graph the install surface would refuse.
const maxGraphNodes = 500

// MaxGraphNodes returns the per-response child-node cap for the graph endpoint.
func MaxGraphNodes() int { return maxGraphNodes }

// Scope is authoritative caller context; it must never come from request data.
type Scope struct {
	CallerUID string
	SpaceID   string
	// Admin drops the per-Space/owner predicates on read and write so the
	// marketplace-admin surface can operate on any plugin (system connectors,
	// global skills) regardless of owner or Space. It is set ONLY by the admin
	// service; a caller can never influence it.
	//
	// The marketplace-admin surface (AdminList/Detail/Create/Update/Delete, the
	// container import/reupload, and admin skill import) wires production callers
	// that set Admin=true through adminScope; the tenant surface always leaves it
	// false. The redaction read path (visibleTargetIDs) also branches on it.
	Admin bool
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
 p.owner_uid,p.space_id,p.visibility,p.creator_name,p.created_by_type,p.created_by_bot_uid,p.created_by_bot_name,p.icon,p.tool_count,
 p.manifest_json,p.plugin_json,p.attachment_keys_json,p.manifest_hash,p.plugin_hash,p.current_version_id,p.current_version,p.status,p.created_at,p.updated_at,p.deleted_at`

// pluginSummaryColumns omits plugin_json: list pages carry the manifest for
// display but never the full package, which can be large.
const pluginSummaryColumns = `p.plugin_id,p.plugin_name,p.plugin_type,p.is_embedded,p.category_id,p.tags_json,p.publisher,
 p.owner_uid,p.space_id,p.visibility,p.creator_name,p.created_by_type,p.created_by_bot_uid,p.created_by_bot_name,p.icon,p.tool_count,
 p.manifest_json,p.manifest_hash,p.plugin_hash,p.current_version_id,p.current_version,p.status,p.created_at,p.updated_at,p.deleted_at`
