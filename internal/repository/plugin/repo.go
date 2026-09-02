// Package plugin persists the unified Plugin domain.
package plugin

import (
	"database/sql"
	"errors"
	"strconv"
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
	// per-request node or edge cap. The read path fails closed (rather than
	// truncating) so callers never render a partially-missing squad/agent.
	ErrGraphTooLarge = errors.New("plugin graph exceeds node or edge cap")
)

// containerImportMaxMembers and containerImportMaxSkillsPerMember mirror
// containerMaxMembers / containerMaxSkills in internal/service/plugin/container.go.
// Container import is the writer that mints squad graphs, so its ceiling — not
// the install budget — is what the read caps below must clear.
// TestGraphCapsClearContainerImportCeiling (internal/service/plugin) fails if
// the two ever drift apart.
const (
	containerImportMaxMembers         = 30
	containerImportMaxSkillsPerMember = 20
)

// maxGraphNodes caps the total number of related (non-root) plugins returned by
// a single detail_graph response. A maximum-size legal container import mints
// containerImportMaxMembers members plus containerImportMaxSkillsPerMember
// embedded skills each (skills dedupe only by (file,name), so distinct names
// mint distinct nodes), and that squad's detail page must render rather than
// 413 forever. Note this sits above maxInstallRelationTargets (500): a
// maximum-size container is currently importable and readable but not
// installable — a pre-existing mismatch on the install side, not a read cap to
// reconcile downward.
const maxGraphNodes = containerImportMaxMembers * (1 + containerImportMaxSkillsPerMember) // 630

// maxGraphEdges caps the total number of edges (across both levels) returned by
// a single detail_graph response, as a defense against graphs that stay well
// under the node cap by sharing many nodes while still fanning out a huge edge
// set against pre-existing standalone catalog plugins.
//
// The container ceiling above produces exactly one edge per child (630), since
// every embedded child has a single parent. Sharing targets is what decouples
// the two counts, and only the upsert API can build that shape — where
// maxRelations (200 per plugin) would otherwise admit ~40k edges in a two-hop
// closure, far past what one detail page should render. This allows ~3x the
// container ceiling for member-shared-target squads and fails closed above it.
const maxGraphEdges = 2000

// graphEdgeLimit bounds each edge query server-side at one row past the cap.
// The mid-scan check in graphEdges.drain still decides the outcome; the LIMIT
// keeps MySQL from sorting, and the driver from draining off the wire, a result
// set that the client is going to abandon anyway. One extra row is enough for
// drain to observe the overflow, because a query returning exactly the limit
// can only exceed the cap on the row after it.
var graphEdgeLimit = strconv.Itoa(maxGraphEdges + 1)

// MaxGraphNodes returns the per-response child-node cap for the graph endpoint.
func MaxGraphNodes() int { return maxGraphNodes }

// MaxGraphEdges returns the per-response edge cap for the graph endpoint.
func MaxGraphEdges() int { return maxGraphEdges }

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
