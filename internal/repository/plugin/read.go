package plugin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// ListFilter restricts a scoped Plugin listing. Sort accepts newest (default),
// oldest, updated, name, placement, views, installs, downloads, or
// comprehensive; callers validate unsupported values.
type ListFilter struct {
	PlacementCode string
	Type          model.PluginType
	CategoryID    string
	Tags          []string
	Keyword       string
	Mine          bool
	Sort          string
	Limit         int
	Offset        int
	// AllSpaces drops the per-Space visibility predicate — set ONLY by the admin
	// service, never from a caller-supplied filter. Visibility optionally narrows
	// the admin listing to one visibility class (e.g. system connectors).
	AllSpaces  bool
	Visibility model.PluginVisibility
}

// pluginMetric embeds a correlated counter lookup, mirroring the expert list
// pattern: join-free, so the placement GROUP BY stays trivially valid and
// missing metric rows read as zero.
func pluginMetric(column string) string {
	return `COALESCE((SELECT rm.` + column + ` FROM resource_metrics rm WHERE rm.resource_type='plugin' AND rm.resource_id=p.plugin_id),0)`
}

var pluginMetricColumns = `,` + pluginMetric("view_count") + `,` + pluginMetric("install_count") + `,` + pluginMetric("download_count")

func (r *Repo) Get(ctx context.Context, scope Scope, pluginID string) (*model.Plugin, error) {
	var row *sql.Row
	if scope.Admin {
		row = r.db.QueryRowContext(ctx, `SELECT `+pluginColumns+pluginMetricColumns+` FROM plugins p
WHERE p.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL`, pluginID)
	} else {
		row = r.db.QueryRowContext(ctx, `SELECT `+pluginColumns+pluginMetricColumns+` FROM plugins p
WHERE p.plugin_id=? AND p.status=1 AND p.deleted_at IS NULL AND `+visibilitySQL, pluginID, scope.SpaceID, scope.CallerUID)
	}
	p, err := scanPluginWithMetrics(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// List returns one page and the total number of rows matching the same scoped filters.
func (r *Repo) List(ctx context.Context, scope Scope, f ListFilter) ([]model.Plugin, int64, error) {
	from, where, args := buildListQuery(scope, f)
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT p.plugin_id) `+from+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	queryArgs := append(append([]any(nil), args...), limit, max(f.Offset, 0))
	// The placement join can yield one row per (placement_code, category) pair
	// for the same plugin; collapse to one row so the page matches the total.
	group := ``
	if f.PlacementCode != "" {
		group = ` GROUP BY p.plugin_id`
	}
	q := `SELECT ` + pluginSummaryColumns + pluginMetricColumns + from + where + group + listOrder(f) + ` LIMIT ? OFFSET ?`
	rows, err := r.db.QueryContext(ctx, q, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []model.Plugin
	for rows.Next() {
		p, err := scanPluginSummary(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *p)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return out, total, nil
}

func buildListQuery(scope Scope, f ListFilter) (string, string, []any) {
	from := ` FROM plugins p`
	args := []any{}
	if f.PlacementCode != "" {
		from += ` JOIN plugin_placements pp ON pp.plugin_id=p.plugin_id AND pp.placement_code=? AND pp.visible=1`
		args = append(args, f.PlacementCode)
	}
	// Embedded plugins (promoted from a parent's JSON) are parts of a parent
	// asset: reachable via detail and relations, never listed as catalog entries.
	where := ` WHERE p.status=1 AND p.deleted_at IS NULL AND p.is_embedded=0`
	if f.AllSpaces {
		// Admin listing: no per-Space scoping. Never reachable from a caller
		// filter — only the admin service sets AllSpaces.
	} else {
		where += ` AND ` + visibilitySQL
		args = append(args, scope.SpaceID, scope.CallerUID)
	}
	if f.Visibility != "" {
		where += ` AND p.visibility=?`
		args = append(args, string(f.Visibility))
	}
	if f.Type != "" {
		where += ` AND p.plugin_type=?`
		args = append(args, f.Type)
	}
	if f.CategoryID != "" {
		if f.PlacementCode != "" {
			where += ` AND pp.category_id=?`
		} else {
			where += ` AND p.category_id=?`
		}
		args = append(args, f.CategoryID)
	}
	// Tag filter is AND, matching the legacy MCP list semantics: a row must
	// carry every selected tag, not the union that OR would surface.
	for _, tag := range f.Tags {
		where += ` AND JSON_CONTAINS(p.tags_json, JSON_QUOTE(?), '$')`
		args = append(args, tag)
	}
	if f.Keyword != "" {
		where += ` AND p.plugin_name LIKE ? ESCAPE '!'`
		args = append(args, "%"+escapeLike(f.Keyword)+"%")
	}
	if f.Mine {
		where += ` AND p.owner_uid=? AND p.space_id=?`
		args = append(args, scope.CallerUID, scope.SpaceID)
	}
	return from, where, args
}

func listOrder(f ListFilter) string {
	recent := `p.created_at DESC,p.plugin_id DESC`
	switch f.Sort {
	case "oldest":
		return ` ORDER BY p.created_at ASC,p.plugin_id ASC`
	case "updated":
		return ` ORDER BY p.updated_at DESC,p.plugin_id DESC`
	case "name":
		return ` ORDER BY p.plugin_name ASC,p.plugin_id ASC`
	case "views":
		return ` ORDER BY ` + pluginMetric("view_count") + ` DESC,` + recent
	case "installs":
		return ` ORDER BY ` + pluginMetric("install_count") + ` DESC,` + recent
	case "downloads":
		return ` ORDER BY ` + pluginMetric("download_count") + ` DESC,` + recent
	case "comprehensive":
		// Mirrors the expert/skill catalog ranking (repository/expert/list.go —
		// keep the weights in sync): installs 5×, views 1×, plus a recency boost
		// decaying over days so fresh listings still surface.
		return ` ORDER BY (` + pluginMetric("install_count") + ` * 5 + ` + pluginMetric("view_count") +
			` + 20 / POW(TIMESTAMPDIFF(HOUR, p.created_at, NOW()) / 24 + 2, 1.2)) DESC,` + recent
	case "placement":
		if f.PlacementCode != "" {
			return ` ORDER BY MIN(pp.sort_order) ASC,p.plugin_id ASC`
		}
	}
	return ` ORDER BY ` + recent
}

func escapeLike(s string) string {
	r := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_")
	return r.Replace(s)
}

// GetWithRelations returns only live one-level relations whose target is also visible.
func (r *Repo) GetWithRelations(ctx context.Context, scope Scope, pluginID string) (*model.Plugin, []model.PluginRelation, error) {
	p, err := r.Get(ctx, scope, pluginID)
	if err != nil {
		return nil, nil, err
	}
	relWhere, relArgs := visibilitySQL, []any{pluginID, scope.SpaceID, scope.CallerUID}
	if scope.Admin {
		relWhere, relArgs = "1=1", []any{pluginID}
	}
	rows, err := r.db.QueryContext(ctx, `SELECT r.relation_id,r.source_plugin_id,r.target_plugin_id,p.plugin_type,r.relation_type,r.sort_order,
 r.relation_json,r.status,r.created_by,r.created_at,r.updated_at,r.deleted_at
FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id
WHERE r.source_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND `+relWhere+`
ORDER BY r.sort_order,r.relation_id`, relArgs...)
	if err != nil {
		return nil, nil, err
	}
	rels, _, err := scanRelationRows(rows, false)
	if err != nil {
		return nil, nil, err
	}
	return p, rels, nil
}

// graphEdgeRow is the column set used by graph edge queries: the standard
// relation columns plus the target's is_embedded flag so the traversal can
// decide whether a child inherits its parent's visibility.
const graphEdgeColumns = `r.relation_id,r.source_plugin_id,r.target_plugin_id,p.plugin_type,r.relation_type,r.sort_order,
 r.relation_json,r.status,r.created_by,r.created_at,r.updated_at,r.deleted_at,p.is_embedded`

// graphEdgeWhere returns the target-visibility predicate and bind args for edge
// queries (used after the source-selection clause). Admin sees every live edge;
// non-admin exposes standalone targets under the caller's visibilitySQL and
// exposes all embedded targets reachable from an already-authorized source.
//
// Embedded children are per-parent copies owned by a single container graph:
// the write-path gate lockRelationTargets rejects cross-container embedded
// edges, so an embedded target returned here is always a part of the container
// we just authorized. An extra space_id predicate would incorrectly hide
// admin-created (system-visibility, NULL-space) embedded members from a
// caller who can legitimately see their system-visible parent, so we rely on
// the write-path invariant rather than re-checking Space here.
func graphEdgeWhere(scope Scope) (string, []any) {
	if scope.Admin {
		return "1=1", nil
	}
	relWhere := `((p.is_embedded=0 AND ` + visibilitySQL + `) OR p.is_embedded=1)`
	return relWhere, []any{scope.SpaceID, scope.CallerUID}
}

// scanRelationRows drains a relation-edge result set. When withEmbedded is
// true the SELECT is expected to end with p.is_embedded; otherwise the
// is_embedded column is absent (as in GetWithRelations) and all targets are
// reported as standalone (isEmbedded=false). It returns the relations in row
// order plus the IDs of embedded targets in the order they were seen.
func scanRelationRows(rows *sql.Rows, withEmbedded bool) ([]model.PluginRelation, []string, error) {
	defer rows.Close()
	var (
		rels        []model.PluginRelation
		embeddedIDs []string
	)
	for rows.Next() {
		var (
			x       model.PluginRelation
			data    []byte
			deleted sql.NullTime
		)
		dest := []any{&x.ID, &x.SourcePluginID, &x.TargetPluginID, &x.TargetPluginType, &x.Type, &x.SortOrder, &data, &x.Status, &x.CreatedBy, &x.CreatedAt, &x.UpdatedAt, &deleted}
		var isEmbedded bool
		if withEmbedded {
			dest = append(dest, &isEmbedded)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, nil, err
		}
		x.Data = cloneJSON(data)
		if deleted.Valid {
			x.DeletedAt = &deleted.Time
		}
		rels = append(rels, x)
		if withEmbedded && isEmbedded {
			embeddedIDs = append(embeddedIDs, x.TargetPluginID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return rels, embeddedIDs, nil
}

// graphNodeWhere returns the visibility predicate (excluding the mandatory
// p.plugin_id IN (…) source-list bound by the caller) and bind args for the
// batch node-payload query. Non-admin re-checks visibility per node as defense
// in depth: standalone targets must satisfy visibilitySQL; embedded targets
// must appear in embeddedAuth (the set of embedded IDs reached through an
// already-authorized edge). A space_id match is NOT required on embedded
// targets — admin/system containers own their embedded children in the global
// Space, and lockRelationTargets already guarantees those children were
// created in the same container graph.
func graphNodeWhere(scope Scope, embeddedAuth []string) (string, []any) {
	if scope.Admin {
		return "1=1", nil
	}
	embeddedClause := "FALSE"
	args := []any{scope.SpaceID, scope.CallerUID}
	if len(embeddedAuth) > 0 {
		embeddedClause = `p.plugin_id IN (` + placeholders(len(embeddedAuth)) + `)`
		args = append(args, stringSliceAsAny(embeddedAuth)...)
	}
	where := `( (p.is_embedded=0 AND ` + visibilitySQL + `) OR (p.is_embedded=1 AND ` + embeddedClause + `) )`
	return where, args
}

func stringSliceAsAny(in []string) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// GetGraphClosure returns the transitive closure of the relation graph rooted
// at rootID, with a fixed maximum depth of two hops (expert_team -> expert ->
// skill/connector) enforced by the relation-type matrix.
//
// Visibility:
//   - The root must be visible under scope (this is the authorization anchor).
//   - Standalone (is_embedded=0) descendants must satisfy the caller-scoped
//     visibility predicate.
//   - Embedded (is_embedded=1) descendants are visible when reachable through an
//     already-authorized ancestor chain AND when they live in the caller's
//     Space — they are part of the container, not independent catalog entries.
//
// The method always performs at most four SQL round-trips regardless of fan-out,
// and fails closed with ErrGraphTooLarge when the deduplicated child count
// exceeds maxGraphNodes. On success relations carries every visible edge in the
// closure (both levels, sorted by source then sort_order), and nodes carries
// the deduplicated set of visible child plugins (light projection: no package
// blob), in first-seen order.
func (r *Repo) GetGraphClosure(ctx context.Context, scope Scope, rootID string) (*model.Plugin, []model.PluginRelation, []*model.Plugin, error) {
	root, err := r.Get(ctx, scope, rootID)
	if err != nil {
		return nil, nil, nil, err
	}

	// Leaves (skill/connector) have no valid outgoing edges per the relation
	// matrix; short-circuit without issuing an edge query.
	if root.Type == model.PluginTypeSkill || root.Type == model.PluginTypeConnector {
		return root, nil, nil, nil
	}

	// ---- Level 1: edges directly from root ---------------------------------
	edgeWhere, edgeWhereArgs := graphEdgeWhere(scope)
	l1Q := `SELECT ` + graphEdgeColumns + ` FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id
WHERE r.source_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND ` + edgeWhere + `
ORDER BY r.sort_order,r.relation_id`
	l1Args := append([]any{rootID}, edgeWhereArgs...)
	l1Rows, err := r.db.QueryContext(ctx, l1Q, l1Args...)
	if err != nil {
		return nil, nil, nil, wrapped("graph l1 edges", err)
	}
	l1Rels, l1Embedded, err := scanRelationRows(l1Rows, true)
	if err != nil {
		return nil, nil, nil, wrapped("graph l1 scan", err)
	}

	// Partition level-1 targets by embedded-ness for the next hop. Use an
	// insertion-ordered set so dedup preserves the edge-discovery order.
	var (
		level1IDs     []string
		level1Seen    = map[string]bool{}
		embeddedSet   = map[string]bool{}
		embeddedOrder []string
		// We need the target plugin types to stamp SourcePluginType on level-2
		// edges and to decide whether further hops are possible.
		targetType = map[string]model.PluginType{}
	)
	addEmbedded := func(id string) {
		if embeddedSet[id] {
			return
		}
		embeddedSet[id] = true
		embeddedOrder = append(embeddedOrder, id)
	}
	addTarget := func(id string, t model.PluginType, isEmbedded bool) {
		targetType[id] = t
		if isEmbedded {
			addEmbedded(id)
		}
		if !level1Seen[id] {
			level1Seen[id] = true
			level1IDs = append(level1IDs, id)
		}
	}
	for i := range l1Rels {
		x := &l1Rels[i]
		x.SourcePluginType = root.Type
		addTarget(x.TargetPluginID, x.TargetPluginType, containsStr(l1Embedded, x.TargetPluginID))
	}

	allRels := make([]model.PluginRelation, 0, len(l1Rels))
	allRels = append(allRels, l1Rels...)

	// ---- Level 2: edges from level-1 sources (expert_team roots only) ------
	// The relation matrix only allows expert_team -> expert -> skill/connector to
	// nest two hops; expert roots already reached leaves in level 1.
	if root.Type == model.PluginTypeExpertTeam && len(level1IDs) > 0 {
		sourcePh := placeholders(len(level1IDs))
		l2Q := `SELECT ` + graphEdgeColumns + ` FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id
WHERE r.source_plugin_id IN (` + sourcePh + `) AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND ` + edgeWhere + `
ORDER BY r.source_plugin_id,r.sort_order,r.relation_id`
		l2Args := append(append([]any(nil), stringSliceAsAny(level1IDs)...), edgeWhereArgs...)
		l2Rows, err := r.db.QueryContext(ctx, l2Q, l2Args...)
		if err != nil {
			return nil, nil, nil, wrapped("graph l2 edges", err)
		}
		l2Rels, l2Embedded, err := scanRelationRows(l2Rows, true)
		if err != nil {
			return nil, nil, nil, wrapped("graph l2 scan", err)
		}
		for i := range l2Rels {
			x := &l2Rels[i]
			if st, ok := targetType[x.SourcePluginID]; ok {
				x.SourcePluginType = st
			}
			allRels = append(allRels, *x)
			isEmb := containsStr(l2Embedded, x.TargetPluginID)
			addTarget(x.TargetPluginID, x.TargetPluginType, isEmb)
		}
	}

	// ---- Node cap ----------------------------------------------------------
	// level1IDs by this point contains the deduped union of L1 and L2 targets in
	// first-seen order. Cap BEFORE fetching payloads.
	if len(level1IDs) > maxGraphNodes {
		return nil, nil, nil, ErrGraphTooLarge
	}
	if len(level1IDs) == 0 {
		return root, allRels, nil, nil
	}

	// ---- Batch node payloads (light projection) ---------------------------
	nodeWhere, nodeArgs := graphNodeWhere(scope, embeddedOrder)
	nodeQ := `SELECT ` + pluginSummaryColumns + pluginMetricColumns + ` FROM plugins p
WHERE p.status=1 AND p.deleted_at IS NULL AND p.plugin_id IN (` + placeholders(len(level1IDs)) + `) AND ` + nodeWhere
	fullArgs := append(append([]any(nil), stringSliceAsAny(level1IDs)...), nodeArgs...)
	nRows, err := r.db.QueryContext(ctx, nodeQ, fullArgs...)
	if err != nil {
		return nil, nil, nil, wrapped("graph nodes", err)
	}
	defer nRows.Close()
	present := map[string]*model.Plugin{}
	var returnOrder []string
	for nRows.Next() {
		p, err := scanPluginSummary(nRows)
		if err != nil {
			return nil, nil, nil, wrapped("graph node scan", err)
		}
		present[p.ID] = p
		returnOrder = append(returnOrder, p.ID)
	}
	if err := nRows.Err(); err != nil {
		return nil, nil, nil, wrapped("graph node iter", err)
	}

	// Defense-in-depth: if a target was filtered out by the node WHERE
	// (concurrent delete or corrupted edge), drop edges that referenced it and
	// do not include it in the node slice.
	if len(present) != len(level1IDs) {
		filtered := allRels[:0]
		for _, rel := range allRels {
			if rel.SourcePluginID != root.ID && present[rel.SourcePluginID] == nil {
				continue // edge from a vanished ancestor; drop
			}
			if present[rel.TargetPluginID] == nil {
				continue
			}
			filtered = append(filtered, rel)
		}
		allRels = filtered
	}
	nodes := make([]*model.Plugin, 0, len(returnOrder))
	for _, id := range level1IDs {
		if p, ok := present[id]; ok {
			nodes = append(nodes, p)
		}
	}
	return root, allRels, nodes, nil
}

func containsStr(hay []string, needle string) bool {
	for _, v := range hay {
		if v == needle {
			return true
		}
	}
	return false
}

// CountMemberRelations returns per-team counts of live expert_team_expert
// edges whose target Plugin is itself live. Members are embedded Plugins
// promoted with their team, so target liveness (not caller visibility) is the
// correct predicate: a team visible to the caller never leaks counts of
// resources outside its own graph.
func (r *Repo) CountMemberRelations(ctx context.Context, teamIDs []string) (map[string]int, error) {
	out := make(map[string]int, len(teamIDs))
	if len(teamIDs) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(teamIDs))
	for _, id := range teamIDs {
		args = append(args, id)
	}
	rows, err := r.db.QueryContext(ctx, `SELECT r.source_plugin_id, COUNT(*) FROM plugin_relations r
JOIN plugins t ON t.plugin_id=r.target_plugin_id AND t.status=1 AND t.deleted_at IS NULL
WHERE r.source_plugin_id IN (`+placeholders(len(teamIDs))+`) AND r.relation_type='expert_team_expert' AND r.status=1 AND r.deleted_at IS NULL
GROUP BY r.source_plugin_id`, args...)
	if err != nil {
		return nil, wrapped("count member relations", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			return nil, err
		}
		out[id] = count
	}
	return out, rows.Err()
}

// CountDeclaredRelations counts a plugin's live one-level relations to live
// targets WITHOUT the caller-visibility predicate — the full declared topology
// its publisher committed. The install path compares this against the
// visibility-filtered set (GetWithRelations) to detect that a dependency was
// hidden from the caller and refuse rather than silently provision a partial
// expert/squad (P1-1). It returns only a count, so it reveals nothing beyond the
// number of edges the plugin already declares; the target-liveness predicate
// matches GetWithRelations so the comparison isolates visibility drops from
// liveness drops.
func (r *Repo) CountDeclaredRelations(ctx context.Context, pluginID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_relations r
JOIN plugins p ON p.plugin_id=r.target_plugin_id AND p.status=1 AND p.deleted_at IS NULL
WHERE r.source_plugin_id=? AND r.status=1 AND r.deleted_at IS NULL`, pluginID).Scan(&n)
	if err != nil {
		return 0, wrapped("count declared relations", err)
	}
	return n, nil
}

func scanPlugin(s interface{ Scan(...any) error }) (*model.Plugin, error) {
	return scanPluginRow(s, true, false)
}

// scanPluginWithMetrics scans a row selected with pluginColumns plus the
// correlated metric counters appended by pluginMetricColumns.
func scanPluginWithMetrics(s interface{ Scan(...any) error }) (*model.Plugin, error) {
	return scanPluginRow(s, true, true)
}

// scanPluginSummary scans a row selected with pluginSummaryColumns plus metric
// counters; the package stays nil so list pages never materialize it.
func scanPluginSummary(s interface{ Scan(...any) error }) (*model.Plugin, error) {
	return scanPluginRow(s, false, true)
}

func scanPluginRow(s interface{ Scan(...any) error }, includePackage, includeMetrics bool) (*model.Plugin, error) {
	var p model.Plugin
	var category, space, botUID, botName, version, versionName sql.NullString
	var tags, manifest, pkg, attachKeys []byte
	var deleted sql.NullTime
	dest := []any{&p.ID, &p.Name, &p.Type, &p.IsEmbedded, &category, &tags, &p.Publisher, &p.OwnerUID, &space, &p.Visibility, &p.CreatorName, &p.CreatedByType, &botUID, &botName, &p.Icon, &p.ToolCount, &manifest}
	if includePackage {
		dest = append(dest, &pkg, &attachKeys)
	}
	dest = append(dest, &p.ManifestHash, &p.PluginHash, &version, &versionName, &p.Status, &p.CreatedAt, &p.UpdatedAt, &deleted)
	if includeMetrics {
		dest = append(dest, &p.ViewCount, &p.InstallCount, &p.DownloadCount)
	}
	if err := s.Scan(dest...); err != nil {
		return nil, err
	}
	p.CategoryID = nullString(category)
	p.SpaceID = nullString(space)
	p.CreatedByBotUID = nullString(botUID)
	p.CreatedByBotName = nullString(botName)
	p.CurrentVersionID = nullString(version)
	p.CurrentVersion = nullString(versionName)
	if deleted.Valid {
		p.DeletedAt = &deleted.Time
	}
	p.Tags = cloneJSON(tags)
	p.Manifest = cloneJSON(manifest)
	if includePackage {
		p.Package = cloneJSON(pkg)
		p.AttachmentKeys = cloneJSON(attachKeys)
	}
	return &p, nil
}
func cloneJSON(v []byte) []byte {
	if v == nil {
		return nil
	}
	return append([]byte(nil), v...)
}
func nullString(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	x := v.String
	return &x
}
func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
func wrapped(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("plugin repository %s: %w", op, err)
}
