package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func pluginTestColumnsWithMetrics() []string {
	return append(pluginTestColumns(), "view_count", "install_count", "download_count")
}

// pluginSummaryTestColumns mirrors pluginSummaryColumns + metrics, i.e. no
// plugin_json / attachment_keys_json.
func pluginSummaryTestColumns() []string {
	return []string{"plugin_id", "plugin_name", "plugin_type", "is_embedded", "category_id", "tags_json", "publisher", "owner_uid", "space_id", "visibility", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "icon", "tool_count", "manifest_json", "manifest_hash", "plugin_hash", "current_version_id", "current_version", "status", "created_at", "updated_at", "deleted_at", "view_count", "install_count", "download_count"}
}

func graphEdgeTestColumns() []string {
	return []string{"relation_id", "source_plugin_id", "target_plugin_id", "plugin_type", "relation_type", "sort_order", "relation_json", "status", "created_by", "created_at", "updated_at", "deleted_at", "is_embedded"}
}

func TestGetGraphClosure_ExpertRoot_OneHop(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// Query 1: root
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.status=1 AND p.deleted_at IS NULL AND \(p.visibility IN`).
		WithArgs("expert-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()).
			AddRow("expert-1", "Expert 1", model.PluginTypeExpert, 0, nil, []byte(`[]`), "pub", "owner", "space", model.PluginVisibilitySpace, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), nil, "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))
	// Query 2: level-1 edges.
	mock.ExpectQuery(`SELECT .* FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id WHERE r.source_plugin_id=\? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND .*OR p.is_embedded=1.*ORDER BY r.sort_order,r.relation_id`).
		WithArgs("expert-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns()).
			AddRow("rel-1", "expert-1", "skill-1", model.PluginTypeSkill, "expert_skill", 0, []byte(`{"source_index":0}`), 1, "owner", now, now, nil, 0))
	// Query 3: node payloads (summary + metrics).
	mock.ExpectQuery(`SELECT `).WithArgs("skill-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(pluginSummaryTestColumns()).
			AddRow("skill-1", "Skill 1", model.PluginTypeSkill, 0, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySpace, "Creator", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))

	root, rels, nodes, err := r.GetGraphClosure(context.Background(), scope, "expert-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root == nil || root.ID != "expert-1" {
		t.Fatalf("root mismatch: %+v", root)
	}
	if len(rels) != 1 || rels[0].TargetPluginID != "skill-1" {
		t.Fatalf("rels = %+v", rels)
	}
	if len(nodes) != 1 || nodes[0].ID != "skill-1" {
		t.Fatalf("nodes = %+v", nodes)
	}
	// Related plugins must NOT have Package populated (summary projection).
	if nodes[0].Package != nil {
		t.Fatalf("expected related node to omit plugin_json, got %s", nodes[0].Package)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sqlmock expectations: %v", err)
	}
}

func TestGetGraphClosure_LeafRoot_NoEdgeQueriesBeyondRoot(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\?`).
		WithArgs("skill-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()).
			AddRow("skill-1", "Skill 1", model.PluginTypeSkill, 0, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySpace, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), nil, "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))

	root, rels, nodes, err := r.GetGraphClosure(context.Background(), scope, "skill-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if root.ID != "skill-1" {
		t.Fatalf("root = %+v", root)
	}
	if len(rels) != 0 || len(nodes) != 0 {
		t.Fatalf("expected no relations/nodes for leaf root, got rels=%d nodes=%d", len(rels), len(nodes))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestGetGraphClosure_ExpertTeam_TwoHops_DedupesShared(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\?`).
		WithArgs("team-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()).
			AddRow("team-1", "Team", model.PluginTypeExpertTeam, 0, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySystem, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), nil, "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))
	// L1: two embedded members
	mock.ExpectQuery(`FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id WHERE r.source_plugin_id=\? AND .*OR p.is_embedded=1\).*ORDER BY r.sort_order,r.relation_id`).
		WithArgs("team-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns()).
			AddRow("rel-m1", "team-1", "m1", model.PluginTypeExpert, "expert_team_expert", 0, []byte(`{"member_key":"a","role":"member","is_leader":false,"source_index":0}`), 1, "owner", now, now, nil, 1).
			AddRow("rel-m2", "team-1", "m2", model.PluginTypeExpert, "expert_team_expert", 1, []byte(`{"member_key":"b","role":"member","is_leader":true,"source_index":1}`), 1, "owner", now, now, nil, 1))
	// L2 edges from m1 and m2 — m2 shares the same skill as m1
	mock.ExpectQuery(`r.source_plugin_id IN \(\?,\?\).*OR p.is_embedded=1\)`).
		WithArgs("m1", "m2", "space", "caller").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns()).
			AddRow("rel-s1", "m1", "s1", model.PluginTypeSkill, "expert_skill", 0, []byte(`{"source_index":0}`), 1, "owner", now, now, nil, 1).
			AddRow("rel-s2a", "m1", "s2", model.PluginTypeSkill, "expert_skill", 1, []byte(`{"source_index":1}`), 1, "owner", now, now, nil, 1).
			AddRow("rel-s2b", "m2", "s2", model.PluginTypeSkill, "expert_skill", 0, []byte(`{"source_index":0}`), 1, "owner", now, now, nil, 1))
	// Node payloads: m1, m2, s1, s2 (deduped); embedded branch has no extra space bind
	mock.ExpectQuery(`SELECT .*p.plugin_id IN \(\?,\?,\?,\?\).*`).
		WithArgs("m1", "m2", "s1", "s2", "space", "caller", "m1", "m2", "s1", "s2").
		WillReturnRows(sqlmock.NewRows(pluginSummaryTestColumns()).
			AddRow("m1", "Member 1", model.PluginTypeExpert, 1, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySystem, "Creator", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0).
			AddRow("m2", "Member 2", model.PluginTypeExpert, 1, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySystem, "Creator", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0).
			AddRow("s1", "Skill 1", model.PluginTypeSkill, 1, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySystem, "Creator", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0).
			AddRow("s2", "Skill 2", model.PluginTypeSkill, 1, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySystem, "Creator", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))

	root, rels, nodes, err := r.GetGraphClosure(context.Background(), scope, "team-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if root.ID != "team-1" || root.Type != model.PluginTypeExpertTeam {
		t.Fatalf("root: %+v", root)
	}
	// 2 member edges + 3 skill edges = 5
	if len(rels) != 5 {
		t.Fatalf("want 5 rels, got %d: %+v", len(rels), rels)
	}
	// Deduped nodes: m1, m2, s1, s2 = 4
	if len(nodes) != 4 {
		t.Fatalf("want 4 nodes, got %d: %+v", len(nodes), nodes)
	}
	// Round-trip edge data for the is_leader=true edge.
	var foundLeader bool
	for _, rel := range rels {
		if rel.TargetPluginID == "m2" && rel.Type == "expert_team_expert" {
			var data map[string]any
			if err := json.Unmarshal(rel.Data, &data); err != nil {
				t.Fatalf("leader edge data unmarshal: %v", err)
			}
			if v, _ := data["is_leader"].(bool); v {
				foundLeader = true
			}
		}
	}
	if !foundLeader {
		t.Fatal("is_leader=true edge data lost")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestGetGraphClosure_HiddenStandaloneChild_Omitted(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space-a"}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\?`).
		WithArgs("team-1", "space-a", "caller").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()).
			AddRow("team-1", "Team", model.PluginTypeExpertTeam, 0, nil, []byte(`[]`), "", "owner", "space-a", model.PluginVisibilitySpace, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), nil, "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))
	// Return only one visible embedded member — the cross-space standalone member is filtered out by the JOIN predicate.
	mock.ExpectQuery(`FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id WHERE r.source_plugin_id=\? AND .*ORDER BY r.sort_order,r.relation_id`).
		WithArgs("team-1", "space-a", "caller").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns()).
			AddRow("rel-m1", "team-1", "m1", model.PluginTypeExpert, "expert_team_expert", 0, nil, 1, "owner", now, now, nil, 1))
	mock.ExpectQuery(`r.source_plugin_id IN \(\?\).*`).
		WithArgs("m1", "space-a", "caller").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns())) // no skills returned
	// Only m1 in payload query
	mock.ExpectQuery(`p.plugin_id IN \(\?\)`).
		WithArgs("m1", "space-a", "caller", "m1").
		WillReturnRows(sqlmock.NewRows(pluginSummaryTestColumns()).
			AddRow("m1", "Member", model.PluginTypeExpert, 1, nil, []byte(`[]`), "", "owner", "space-a", model.PluginVisibilitySpace, "Creator", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))

	_, rels, nodes, err := r.GetGraphClosure(context.Background(), scope, "team-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(rels) != 1 || rels[0].TargetPluginID != "m1" {
		t.Fatalf("expected only visible m1 edge, got %+v", rels)
	}
	if len(nodes) != 1 || nodes[0].ID != "m1" {
		t.Fatalf("nodes = %+v", nodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestGetGraphClosure_Admin_SeesCrossSpaceStandalone(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "admin", SpaceID: "", Admin: true}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	// Admin root query has NO visibility predicate.
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\? AND p.status=1 AND p.deleted_at IS NULL$`).
		WithArgs("team-1").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()).
			AddRow("team-1", "Team", model.PluginTypeExpertTeam, 0, nil, []byte(`[]`), "", "owner", "space-b", model.PluginVisibilitySpace, "Creator", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), nil, "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))
	// Admin edge predicate is 1=1 (no visibility/space bind).
	mock.ExpectQuery(`WHERE r.source_plugin_id=\? AND r.status=1 AND r.deleted_at IS NULL AND p.status=1 AND p.deleted_at IS NULL AND 1=1`).
		WithArgs("team-1").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns()).
			AddRow("rel-m1", "team-1", "m1", model.PluginTypeExpert, "expert_team_expert", 0, nil, 1, "owner", now, now, nil, 0))
	mock.ExpectQuery(`r.source_plugin_id IN \(\?\).*AND 1=1`).
		WithArgs("m1").
		WillReturnRows(sqlmock.NewRows(graphEdgeTestColumns()))
	// Admin node WHERE is 1=1 (no visibility).
	mock.ExpectQuery(`p.plugin_id IN \(\?\).*AND 1=1`).
		WithArgs("m1").
		WillReturnRows(sqlmock.NewRows(pluginSummaryTestColumns()).
			AddRow("m1", "M", model.PluginTypeExpert, 0, nil, []byte(`[]`), "", "owner", "space-b", model.PluginVisibilitySpace, "C", "human", nil, nil, "", 0, []byte(`{}`), "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))

	root, _, nodes, err := r.GetGraphClosure(context.Background(), scope, "team-1")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if root.ID != "team-1" || len(nodes) != 1 {
		t.Fatalf("root=%v nodes=%+v", root.ID, nodes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestGetGraphClosure_CapEnforcedBeforeNodeQuery(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\?`).
		WithArgs("expert-1", "space", "caller").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()).
			AddRow("expert-1", "Expert", model.PluginTypeExpert, 0, nil, []byte(`[]`), "", "owner", "space", model.PluginVisibilitySpace, "C", "human", nil, nil, "", 0, []byte(`{}`), []byte(`{}`), nil, "mh", "ph", nil, nil, 1, now, now, nil, 0, 0, 0))
	// Generate maxGraphNodes+1 distinct target ids; easiest to assert the error fires without constructing that many rows — use maxGraphNodes+1 via direct call but here just push > cap.
	rows := sqlmock.NewRows(graphEdgeTestColumns())
	const over = maxGraphNodes + 1
	for i := 0; i < over; i++ {
		tid := "s" + itoa(i)
		rows.AddRow("r-"+tid, "expert-1", tid, model.PluginTypeSkill, "expert_skill", i, nil, 1, "owner", now, now, nil, 0)
	}
	mock.ExpectQuery(`FROM plugin_relations r JOIN plugins p ON p.plugin_id=r.target_plugin_id WHERE r.source_plugin_id=\? AND .*ORDER BY r.sort_order,r.relation_id`).
		WithArgs("expert-1", "space", "caller").
		WillReturnRows(rows)

	_, _, _, err := r.GetGraphClosure(context.Background(), scope, "expert-1")
	if err != ErrGraphTooLarge {
		t.Fatalf("want ErrGraphTooLarge, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet: %v", err)
	}
}

func TestGetGraphClosure_RootNotFound(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	r := New(db)
	scope := Scope{CallerUID: "caller", SpaceID: "space"}
	mock.ExpectQuery(`SELECT .* FROM plugins p WHERE p.plugin_id=\?`).
		WithArgs("missing", "space", "caller").
		WillReturnRows(sqlmock.NewRows(pluginTestColumnsWithMetrics()))
	_, _, _, err := r.GetGraphClosure(context.Background(), scope, "missing")
	if err != ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := make([]byte, 0, 4)
	for i > 0 {
		buf = append([]byte{byte('0' + i%10)}, buf...)
		i /= 10
	}
	return string(buf)
}
