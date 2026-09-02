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
	// ErrReviewPending indicates a state change refused because an open review
	// request exists on the plugin. Reported from inside a transaction that holds
	// the plugin row lock, so it closes the window the service's own unlocked
	// pre-check leaves open. The service maps it back to its own ErrReviewPending.
	ErrReviewPending = errors.New("a review request is pending on this plugin")
	// ErrListedRequiresReview indicates an ordinary edit refused because the
	// plugin row is listed to the organization (published AND space). Reported
	// from inside Repo.Update's transaction, which holds the plugin row lock, so
	// it re-derives the gate from the LOCKED row rather than the service's earlier
	// unlocked read — closing the window in which an approval or publish commits
	// between the two and turns a stale-legal edit into unreviewed content on a
	// live org row. The service maps it back to its own ErrListedRequiresReview.
	ErrListedRequiresReview = errors.New("a listed plugin must go through review")
	// ErrInvalidRelation indicates a relation whose source/target types are incompatible.
	ErrInvalidRelation = errors.New("invalid plugin relation")
	// ErrInvalidCategory indicates a missing, inactive, or type-incompatible category.
	ErrInvalidCategory = errors.New("invalid plugin category")
	// ErrInvalidPlacement indicates a category not enabled for the Plugin type and placement.
	ErrInvalidPlacement = errors.New("invalid plugin placement")
)

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

// visibilitySQL is the single catalog-read predicate. Eight read sites and one
// write site (lockRelationTargets) embed it, so it is the one place a scope leak
// can be fixed — or introduced.
//
// listing_state is checked ONLY inside the `space` disjunct. Three properties are
// load-bearing:
//
//  1. The owner disjunct is deliberately NOT gated on listing_state. The author
//     must be able to read their own draft in order to edit and publish it, and
//     "我的插件" runs this same query. Excluding a draft from the market GRID is a
//     separate, list-only concern (buildListQuery), not a scope rule.
//  2. 'published' is a LITERAL, not a placeholder, so every call site keeps its
//     existing argument list unchanged.
//  3. The public/system disjunct is untouched. A `system` row is admin-owned,
//     reaches every Space and has no per-Space listing lifecycle; gating it would
//     make every admin-created connector and global skill vanish the moment a
//     write path forgot to stamp 'published'. A future 全平台可见 tenant flow would
//     need this extended — it is not extended today, on purpose.
const visibilitySQL = `(p.visibility IN ('public','system') OR (p.space_id = ? AND ((p.visibility = 'space' AND p.listing_state = 'published') OR p.owner_uid = ?)))`

const pluginColumns = `p.plugin_id,p.plugin_name,p.plugin_type,p.is_embedded,p.category_id,p.tags_json,p.publisher,
 p.owner_uid,p.space_id,p.visibility,p.listing_state,p.creator_name,p.created_by_type,p.created_by_bot_uid,p.created_by_bot_name,p.icon,p.tool_count,
 p.manifest_json,p.plugin_json,p.attachment_keys_json,p.manifest_hash,p.plugin_hash,p.current_version_id,p.current_version,p.status,p.created_at,p.updated_at,p.deleted_at`

// pluginSummaryColumns omits plugin_json: list pages carry the manifest for
// display but never the full package, which can be large.
const pluginSummaryColumns = `p.plugin_id,p.plugin_name,p.plugin_type,p.is_embedded,p.category_id,p.tags_json,p.publisher,
 p.owner_uid,p.space_id,p.visibility,p.listing_state,p.creator_name,p.created_by_type,p.created_by_bot_uid,p.created_by_bot_name,p.icon,p.tool_count,
 p.manifest_json,p.manifest_hash,p.plugin_hash,p.current_version_id,p.current_version,p.status,p.created_at,p.updated_at,p.deleted_at`
