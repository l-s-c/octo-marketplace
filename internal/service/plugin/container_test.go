package plugin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/plugindoc"
)

// textSkillZip builds a minimal text-only skill package (SKILL.md + one text
// supporting file). Text-only keeps every attachment inline so buildSkillAttachmentTree
// never needs a storage segment — the admin convention places imported skills in
// the empty global Space, which cannot host storage attachments.
func textSkillZip(t *testing.T, title string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	md, _ := zw.Create("SKILL.md")
	if _, err := md.Write([]byte("# " + title + "\nBody for " + title + ".")); err != nil {
		t.Fatal(err)
	}
	extra, _ := zw.Create("scripts/run.sh")
	if _, err := extra.Write([]byte("echo " + title)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// containerZip packs a root manifest (name -> expert.json/squad.json) plus the
// named bundled skill packages into a container archive.
func containerZip(t *testing.T, manifestName string, manifest any, bundles map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	mf, _ := zw.Create(manifestName)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mf.Write(raw); err != nil {
		t.Fatal(err)
	}
	for path, data := range bundles {
		f, _ := zw.Create(path)
		if _, err := f.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func counterIDs() func() string {
	n := 0
	return func() string { n++; return "id-" + strconv.Itoa(n) }
}

func containerService(store *fakeStore) *Service {
	return New(store, &importStorage{objects: map[string][]byte{}}).
		WithRuntime(counterIDs(), func() time.Time { return time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC) })
}

var containerCaller = Caller{UID: "admin-1", Name: "Root", RequestID: "req-import"}

func decodeAttachmentRaw(t *testing.T, pkg json.RawMessage, path string) (string, bool) {
	t.Helper()
	var doc struct {
		Attachments []struct {
			Path        string `json:"path"`
			ContentType string `json:"content_type"`
			RawContent  string `json:"raw_content"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(pkg, &doc); err != nil {
		t.Fatalf("decode package: %v", err)
	}
	for _, a := range doc.Attachments {
		if a.Path == path && a.ContentType == "raw" {
			return a.RawContent, true
		}
	}
	return "", false
}

func TestImportExpertContainerBuildsSkillsExpertAndRelations(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	manifest := map[string]any{
		"name":        "Release Captain",
		"summary":     "Ships releases safely.",
		"tags":        []string{"ops", "release"},
		"instruction": "You coordinate the release train.",
		"mcp_config":  `{"mcpServers":{}}`,
		"skills": []map[string]any{
			{"name": "Deployer", "file": "skills/deploy.zip"},
			{"name": "Verifier", "file": "skills/verify.zip"},
		},
	}
	archive := containerZip(t, "expert.json", manifest, map[string][]byte{
		"skills/deploy.zip": textSkillZip(t, "Deployer"),
		"skills/verify.zip": textSkillZip(t, "Verifier"),
	})

	detail, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive})
	if err != nil {
		t.Fatalf("ImportContainer: %v", err)
	}

	// Three plugins created in one graph: two skills, then the expert.
	if len(store.graphNodes) != 3 {
		t.Fatalf("graph nodes = %d, want 3", len(store.graphNodes))
	}
	if !store.graphScope.Admin {
		t.Fatalf("graph not written under admin scope: %#v", store.graphScope)
	}
	skillTypes := 0
	for _, n := range store.graphNodes[:2] {
		if n.Plugin.Type != model.PluginTypeSkill {
			t.Fatalf("node type = %q want skill", n.Plugin.Type)
		}
		skillTypes++
	}
	if skillTypes != 2 {
		t.Fatalf("skill nodes = %d", skillTypes)
	}

	// Only the top expert (last node) is a standalone market entry: it carries a
	// default visible placement; the bundled skill nodes carry none.
	for _, n := range store.graphNodes[:2] {
		if len(n.Placements) != 0 {
			t.Fatalf("skill node must not carry a placement: %#v", n.Placements)
		}
		if !n.Plugin.IsEmbedded {
			t.Fatalf("bundled skill must be embedded so it stays out of the skill list")
		}
	}
	if store.graphNodes[2].Plugin.IsEmbedded {
		t.Fatalf("top expert must not be embedded")
	}
	topPlacements := store.graphNodes[2].Placements
	if len(topPlacements) != 1 || topPlacements[0].PlacementCode != "default" || !topPlacements[0].Visible {
		t.Fatalf("expert node placements = %#v, want one visible default", topPlacements)
	}

	expert := detail.Plugin
	if expert.Type != model.PluginTypeExpert || expert.Visibility != model.PluginVisibilitySystem {
		t.Fatalf("expert = %q/%q", expert.Type, expert.Visibility)
	}
	if expert.SpaceID == nil || *expert.SpaceID != adminGlobalSpace {
		t.Fatalf("expert space = %v want empty global", expert.SpaceID)
	}
	if len(detail.Relations) != 2 {
		t.Fatalf("expert relations = %d, want 2", len(detail.Relations))
	}
	for _, r := range detail.Relations {
		if r.Type != "expert_skill" || r.TargetPluginType != model.PluginTypeSkill {
			t.Fatalf("relation = %#v", r)
		}
	}

	// The AGENTS.md is byte-identical to the shared renderer used by the backfill.
	agents, ok := decodeAttachmentRaw(t, expert.Package, "AGENTS.md")
	if !ok {
		t.Fatal("expert package missing AGENTS.md")
	}
	if agents != plugindoc.ExpertAgentsMarkdown("Release Captain", "Ships releases safely.", "You coordinate the release train.") {
		t.Fatalf("AGENTS.md not rendered via shared renderer: %q", agents)
	}
	if _, ok := decodeAttachmentRaw(t, expert.Package, "mcp.json"); !ok {
		t.Fatal("expert package missing mcp.json")
	}
	if string(expert.Tags) != `["ops","release"]` {
		t.Fatalf("expert tags = %s", expert.Tags)
	}
}

func TestImportSquadContainerBuildsMembersSkillsAndLeaderRelations(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	manifest := map[string]any{
		"name":       "Product Squad",
		"summary":    "Cross-functional delivery.",
		"tags":       []string{"product"},
		"leader":     "Lead",
		"strategies": []string{"clarify goals", "assess risk"},
		"dependencies": map[string]any{
			"blocking":    []string{"legal-review"},
			"recommended": []string{},
		},
		"permission": "open",
		"members": []map[string]any{
			{
				"member_key":  "lead",
				"name":        "Lead",
				"role":        "leader",
				"is_leader":   true,
				"instruction": "Coordinate the squad.",
				"skills":      []map[string]any{{"name": "Planner", "file": "skills/plan.zip"}},
			},
			{
				"member_key":  "eng",
				"name":        "Engineer",
				"role":        "builder",
				"is_leader":   false,
				"instruction": "Build the features.",
			},
		},
	}
	archive := containerZip(t, "squad.json", manifest, map[string][]byte{
		"skills/plan.zip": textSkillZip(t, "Planner"),
	})

	detail, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive})
	if err != nil {
		t.Fatalf("ImportContainer: %v", err)
	}

	// Graph: lead's skill, lead member, engineer member, team = 4 nodes.
	if len(store.graphNodes) != 4 {
		t.Fatalf("graph nodes = %d, want 4", len(store.graphNodes))
	}
	// Only the top team (last node) is a standalone market entry: it carries a
	// default visible placement; the member/skill nodes carry none.
	for _, n := range store.graphNodes[:3] {
		if len(n.Placements) != 0 {
			t.Fatalf("member/skill node must not carry a placement: %#v", n.Placements)
		}
		if !n.Plugin.IsEmbedded {
			t.Fatalf("team member/skill %q must be embedded so it stays out of the expert/skill list", n.Plugin.Type)
		}
	}
	if store.graphNodes[3].Plugin.IsEmbedded {
		t.Fatalf("top team must not be embedded")
	}
	teamPlacements := store.graphNodes[3].Placements
	if len(teamPlacements) != 1 || teamPlacements[0].PlacementCode != "default" || !teamPlacements[0].Visible {
		t.Fatalf("team node placements = %#v, want one visible default", teamPlacements)
	}
	team := detail.Plugin
	if team.Type != model.PluginTypeExpertTeam {
		t.Fatalf("top-level type = %q want expert_team", team.Type)
	}
	// team package must be exactly AGENTS.md (contract) and rendered via the shared renderer.
	agents, ok := decodeAttachmentRaw(t, team.Package, "AGENTS.md")
	if !ok {
		t.Fatal("team package missing AGENTS.md")
	}
	wantAgents := plugindoc.TeamAgentsMarkdown("Product Squad", "Cross-functional delivery.", "Lead",
		[]any{"clarify goals", "assess risk"},
		map[string]any{"blocking": []any{"legal-review"}, "recommended": []any{}}, "open")
	if agents != wantAgents {
		t.Fatalf("team AGENTS.md mismatch:\n got %q\nwant %q", agents, wantAgents)
	}

	if len(detail.Relations) != 2 {
		t.Fatalf("team relations = %d want 2", len(detail.Relations))
	}
	leaders := 0
	for _, r := range detail.Relations {
		if r.Type != "expert_team_expert" || r.TargetPluginType != model.PluginTypeExpert {
			t.Fatalf("relation = %#v", r)
		}
		var data struct {
			MemberKey string `json:"member_key"`
			Role      string `json:"role"`
			IsLeader  bool   `json:"is_leader"`
		}
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatalf("relation data: %v", err)
		}
		if data.MemberKey == "" || data.Role == "" {
			t.Fatalf("relation missing member metadata: %s", r.Data)
		}
		if data.IsLeader {
			leaders++
		}
	}
	if leaders != 1 {
		t.Fatalf("expert_team_expert leaders = %d, want exactly 1", leaders)
	}

	// One member (the lead) carries an expert_skill edge to its Planner skill.
	memberSkillEdges := 0
	for _, n := range store.graphNodes {
		for _, r := range n.Relations {
			if r.Type == "expert_skill" {
				memberSkillEdges++
			}
		}
	}
	if memberSkillEdges != 1 {
		t.Fatalf("member expert_skill edges = %d want 1", memberSkillEdges)
	}
}

func TestImportSquadContainerRejectsMultipleLeaders(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	manifest := map[string]any{
		"name":    "Bad Squad",
		"summary": "Two captains.",
		"members": []map[string]any{
			{"name": "A", "role": "r", "is_leader": true, "instruction": "x"},
			{"name": "B", "role": "r", "is_leader": true, "instruction": "y"},
		},
	}
	archive := containerZip(t, "squad.json", manifest, nil)
	_, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if store.graphNodes != nil {
		t.Fatalf("a malformed squad must not reach CreateGraph: %d nodes", len(store.graphNodes))
	}
}

func TestImportContainerMalformedDoesNotCommit(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)

	// An expert referencing a bundled skill package that is absent from the
	// archive: the parse fails before any plugin is built, so nothing commits.
	manifest := map[string]any{
		"name":        "Broken",
		"summary":     "Missing skill package.",
		"instruction": "do things",
		"skills":      []map[string]any{{"name": "Ghost", "file": "skills/missing.zip"}},
	}
	archive := containerZip(t, "expert.json", manifest, nil)
	_, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if store.graphNodes != nil {
		t.Fatalf("partial expert must not reach CreateGraph: %d nodes", len(store.graphNodes))
	}

	// A container carrying both manifests is ambiguous and rejected.
	both := containerZip(t, "expert.json", manifest, nil)
	both = appendZipEntry(t, both, "squad.json", []byte(`{"name":"x"}`))
	if _, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: both}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("ambiguous manifest err = %v", err)
	}
}

func TestImportContainerAtomicWhenGraphWriteFails(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}, graphErr: errors.New("tx aborted")}
	svc := containerService(store)
	manifest := map[string]any{
		"name":        "Release Captain",
		"summary":     "Ships releases.",
		"instruction": "coordinate",
		"skills":      []map[string]any{{"name": "Deployer", "file": "skills/deploy.zip"}},
	}
	archive := containerZip(t, "expert.json", manifest, map[string][]byte{
		"skills/deploy.zip": textSkillZip(t, "Deployer"),
	})
	if _, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive}); err == nil {
		t.Fatal("expected a graph write failure to surface")
	}
}

// appendZipEntry rewrites a zip adding one more entry (used to synthesize an
// ambiguous both-manifests container).
func appendZipEntry(t *testing.T, existing []byte, name string, content []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(existing), int64(len(existing)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		w, _ := zw.Create(f.Name)
		rc, _ := f.Open()
		data := make([]byte, f.UncompressedSize64)
		_, _ = rc.Read(data)
		rc.Close()
		_, _ = w.Write(data)
	}
	w, _ := zw.Create(name)
	_, _ = w.Write(content)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// expertReuploadArchive builds an expert container carrying the two bundled
// skills used by the reupload tests.
func expertReuploadArchive(t *testing.T) []byte {
	t.Helper()
	manifest := map[string]any{
		"name":        "Release Captain v2",
		"summary":     "Ships releases even more safely.",
		"tags":        []string{"ops"},
		"instruction": "You now coordinate the release train with canaries.",
		"mcp_config":  `{"mcpServers":{}}`,
		"skills": []map[string]any{
			{"name": "Canary", "file": "skills/canary.zip"},
			{"name": "Rollback", "file": "skills/rollback.zip"},
		},
	}
	return containerZip(t, "expert.json", manifest, map[string][]byte{
		"skills/canary.zip":   textSkillZip(t, "Canary"),
		"skills/rollback.zip": textSkillZip(t, "Rollback"),
	})
}

// existingExpertStore seeds an admin-owned expert with one old bundled skill and
// the expert_skill relation wiring it, the shape a prior import leaves behind.
func existingExpertStore(t *testing.T) (*fakeStore, string) {
	t.Helper()
	space := "space-x"
	category := "cat-orig"
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expert := &model.Plugin{ID: "expert-9", Name: "Release Captain", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilityPublic, SpaceID: &space, OwnerUID: "owner-1", CategoryID: &category, CreatorName: "Orig", CreatedByType: "human", Icon: "icons/e.png", Tags: []byte(`["ops"]`), Manifest: []byte(`{}`), Package: []byte(`{}`), CreatedAt: created, UpdatedAt: created, Status: 1}
	oldSkill := &model.Plugin{ID: "skill-old-1", Name: "Deployer", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilitySystem, SpaceID: &space, OwnerUID: "owner-1", IsEmbedded: true, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	store := &fakeStore{
		plugins: map[string]*model.Plugin{"expert-9": expert, "skill-old-1": oldSkill},
		relations: map[string][]model.PluginRelation{"expert-9": {
			{ID: "rel-old-1", SourcePluginID: "expert-9", TargetPluginID: "skill-old-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill", Status: 1},
		}},
	}
	return store, space
}

func TestReuploadExpertContainerRebuildsPackageAndSwapsSkillsPreservingIdentity(t *testing.T) {
	store, space := existingExpertStore(t)
	svc := containerService(store)

	detail, err := svc.ReuploadContainer(context.Background(), containerCaller, "expert-9", ContainerImportParams{Archive: expertReuploadArchive(t)})
	if err != nil {
		t.Fatalf("ReuploadContainer: %v", err)
	}
	if store.rebuildTop == nil {
		t.Fatal("rebuild did not reach RebuildGraph")
	}
	if !store.rebuildScope.Admin {
		t.Fatalf("rebuild not written under admin scope: %#v", store.rebuildScope)
	}
	top := store.rebuildTop.Plugin
	// Identity is preserved: id, visibility, Space, owner, creator, icon all stay.
	if top.ID != "expert-9" {
		t.Fatalf("top id = %q, want preserved expert-9", top.ID)
	}
	if top.Visibility != model.PluginVisibilityPublic {
		t.Fatalf("visibility = %q, want preserved public", top.Visibility)
	}
	if top.SpaceID == nil || *top.SpaceID != space {
		t.Fatalf("space = %v, want preserved %q", top.SpaceID, space)
	}
	if top.OwnerUID != "owner-1" || top.CreatorName != "Orig" || top.Icon != "icons/e.png" {
		t.Fatalf("owner/creator/icon not preserved: %#v", top)
	}
	// Content is replaced: the AGENTS.md carries the new instruction/summary.
	agents, ok := decodeAttachmentRaw(t, top.Package, "AGENTS.md")
	if !ok || agents != plugindoc.ExpertAgentsMarkdown("Release Captain v2", "Ships releases even more safely.", "You now coordinate the release train with canaries.") {
		t.Fatalf("package not rebuilt from the new upload: %q", agents)
	}
	// New children are fresh, embedded skill rows; none reuse the old id.
	if len(store.rebuildChild) != 2 {
		t.Fatalf("rebuilt children = %d, want 2", len(store.rebuildChild))
	}
	for _, n := range store.rebuildChild {
		if n.Plugin.Type != model.PluginTypeSkill || !n.Plugin.IsEmbedded {
			t.Fatalf("rebuilt child must be an embedded skill: %#v", n.Plugin)
		}
		if n.Plugin.ID == "skill-old-1" {
			t.Fatal("rebuilt child must not reuse the old skill id")
		}
		if len(n.Placements) != 0 {
			t.Fatalf("embedded child must carry no placement: %#v", n.Placements)
		}
	}
	// The old bundled skill is scheduled for soft-delete.
	if len(store.rebuildOldIDs) != 1 || store.rebuildOldIDs[0] != "skill-old-1" {
		t.Fatalf("old child ids = %#v, want [skill-old-1]", store.rebuildOldIDs)
	}
	// The top's new relations target the new skills, sourced from the preserved id.
	if len(store.rebuildTop.Relations) != 2 {
		t.Fatalf("top relations = %d, want 2", len(store.rebuildTop.Relations))
	}
	newChildIDs := map[string]bool{}
	for _, n := range store.rebuildChild {
		newChildIDs[n.Plugin.ID] = true
	}
	for _, r := range store.rebuildTop.Relations {
		if r.Type != "expert_skill" || r.SourcePluginID != "expert-9" || !newChildIDs[r.TargetPluginID] {
			t.Fatalf("unexpected rebuilt relation: %#v", r)
		}
	}
	if detail.Plugin.ID != "expert-9" {
		t.Fatalf("detail plugin id = %q", detail.Plugin.ID)
	}
}

func TestReuploadExpertContainerThreadsCategoryOverride(t *testing.T) {
	store, _ := existingExpertStore(t)
	svc := containerService(store)
	category := "cat-7"
	if _, err := svc.ReuploadContainer(context.Background(), containerCaller, "expert-9", ContainerImportParams{Archive: expertReuploadArchive(t), CategoryID: &category}); err != nil {
		t.Fatalf("ReuploadContainer: %v", err)
	}
	if store.rebuildTop.Plugin.CategoryID == nil || *store.rebuildTop.Plugin.CategoryID != "cat-7" {
		t.Fatalf("category override not threaded: %#v", store.rebuildTop.Plugin.CategoryID)
	}
}

// A package-only reupload (no category_id supplied) must PRESERVE the row's
// existing category rather than wiping it to NULL and desyncing the placement.
func TestReuploadExpertContainerPreservesCategoryWhenOmitted(t *testing.T) {
	store, _ := existingExpertStore(t)
	svc := containerService(store)
	if _, err := svc.ReuploadContainer(context.Background(), containerCaller, "expert-9", ContainerImportParams{Archive: expertReuploadArchive(t)}); err != nil {
		t.Fatalf("ReuploadContainer: %v", err)
	}
	if store.rebuildTop.Plugin.CategoryID == nil || *store.rebuildTop.Plugin.CategoryID != "cat-orig" {
		t.Fatalf("category not preserved on package-only reupload: %#v", store.rebuildTop.Plugin.CategoryID)
	}
}

// An embedded row (e.g. a squad member, itself an expert-typed row) must not be
// rebuildable through the top-level reupload endpoint — it is reported 404.
func TestReuploadContainerRejectsEmbeddedTarget(t *testing.T) {
	space := "space-x"
	member := &model.Plugin{ID: "member-9", Name: "Lead", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilitySystem, SpaceID: &space, IsEmbedded: true, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	store := &fakeStore{plugins: map[string]*model.Plugin{"member-9": member}}
	svc := containerService(store)
	_, err := svc.ReuploadContainer(context.Background(), containerCaller, "member-9", ContainerImportParams{Archive: expertReuploadArchive(t)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an embedded target", err)
	}
	if store.rebuildTop != nil {
		t.Fatal("an embedded target must not reach RebuildGraph")
	}
}

func TestReuploadSquadContainerReplacesMembersAndTheirSkills(t *testing.T) {
	space := "space-y"
	team := &model.Plugin{ID: "team-9", Name: "Product Squad", Type: model.PluginTypeExpertTeam, Visibility: model.PluginVisibilityPublic, SpaceID: &space, OwnerUID: "owner-2", CreatorName: "Orig", CreatedByType: "human", Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	member := &model.Plugin{ID: "member-old-1", Name: "Lead", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilitySystem, SpaceID: &space, IsEmbedded: true, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	mSkill := &model.Plugin{ID: "mskill-old-1", Name: "Planner", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilitySystem, SpaceID: &space, IsEmbedded: true, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	store := &fakeStore{
		plugins: map[string]*model.Plugin{"team-9": team, "member-old-1": member, "mskill-old-1": mSkill},
		relations: map[string][]model.PluginRelation{
			"team-9":       {{ID: "rel-tm", SourcePluginID: "team-9", TargetPluginID: "member-old-1", TargetPluginType: model.PluginTypeExpert, Type: "expert_team_expert", Status: 1}},
			"member-old-1": {{ID: "rel-ms", SourcePluginID: "member-old-1", TargetPluginID: "mskill-old-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill", Status: 1}},
		},
	}
	svc := containerService(store)
	manifest := map[string]any{
		"name":    "Product Squad v2",
		"summary": "Now with QA.",
		"members": []map[string]any{
			{"member_key": "lead", "name": "Lead", "role": "leader", "is_leader": true, "instruction": "Coordinate.", "skills": []map[string]any{{"name": "Roadmap", "file": "skills/roadmap.zip"}}},
			{"member_key": "qa", "name": "QA", "role": "tester", "is_leader": false, "instruction": "Test."},
		},
	}
	archive := containerZip(t, "squad.json", manifest, map[string][]byte{"skills/roadmap.zip": textSkillZip(t, "Roadmap")})

	if _, err := svc.ReuploadContainer(context.Background(), containerCaller, "team-9", ContainerImportParams{Archive: archive}); err != nil {
		t.Fatalf("ReuploadContainer: %v", err)
	}
	if store.rebuildTop.Plugin.ID != "team-9" || store.rebuildTop.Plugin.Type != model.PluginTypeExpertTeam {
		t.Fatalf("top not the preserved team: %#v", store.rebuildTop.Plugin)
	}
	// Old member AND its bundled skill are both scheduled for soft-delete.
	gotOld := map[string]bool{}
	for _, id := range store.rebuildOldIDs {
		gotOld[id] = true
	}
	if !gotOld["member-old-1"] || !gotOld["mskill-old-1"] {
		t.Fatalf("old descendants not fully collected: %#v", store.rebuildOldIDs)
	}
	// Every rebuilt child (new members + their skills) is embedded.
	members, skills := 0, 0
	for _, n := range store.rebuildChild {
		if !n.Plugin.IsEmbedded {
			t.Fatalf("rebuilt squad child must be embedded: %#v", n.Plugin)
		}
		switch n.Plugin.Type {
		case model.PluginTypeExpert:
			members++
		case model.PluginTypeSkill:
			skills++
		}
	}
	if members != 2 || skills != 1 {
		t.Fatalf("rebuilt children = %d members / %d skills, want 2/1", members, skills)
	}
}

func TestReuploadContainerRejectsTypeMismatch(t *testing.T) {
	store, _ := existingExpertStore(t) // an expert row
	svc := containerService(store)
	squad := containerZip(t, "squad.json", map[string]any{
		"name": "Wrong", "summary": "squad zip for an expert row",
		"members": []map[string]any{{"name": "A", "role": "r", "is_leader": true, "instruction": "x"}},
	}, nil)
	_, err := svc.ReuploadContainer(context.Background(), containerCaller, "expert-9", ContainerImportParams{Archive: squad})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if store.rebuildTop != nil {
		t.Fatal("a type-mismatched upload must not reach RebuildGraph")
	}
}

func TestReuploadContainerMissingPluginIs404(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	_, err := svc.ReuploadContainer(context.Background(), containerCaller, "ghost", ContainerImportParams{Archive: expertReuploadArchive(t)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestReuploadContainerRejectsNonContainerType(t *testing.T) {
	space := "space-z"
	skill := &model.Plugin{ID: "skill-9", Name: "S", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilitySystem, SpaceID: &space, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	store := &fakeStore{plugins: map[string]*model.Plugin{"skill-9": skill}}
	svc := containerService(store)
	_, err := svc.ReuploadContainer(context.Background(), containerCaller, "skill-9", ContainerImportParams{Archive: expertReuploadArchive(t)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for a non-container row", err)
	}
	if store.rebuildTop != nil {
		t.Fatal("a skill row must not reach RebuildGraph")
	}
}

// A reupload may tear down only the container's OWN embedded children. When an
// expert's expert_skill relation points at a STANDALONE catalog skill
// (is_embedded=0, e.g. one carrying its own market placement), that skill is
// merely referenced by this expert — buildRelations validates type, not
// ownership — and must never be soft-deleted out from under every other graph
// that shares it. Only the genuine embedded child reaches rebuildOldIDs.
func TestReuploadExpertContainerLeavesStandaloneRelationTargets(t *testing.T) {
	space := "space-x"
	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expert := &model.Plugin{ID: "expert-9", Name: "Release Captain", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilityPublic, SpaceID: &space, OwnerUID: "owner-1", Tags: []byte(`["ops"]`), Manifest: []byte(`{}`), Package: []byte(`{}`), CreatedAt: created, UpdatedAt: created, Status: 1}
	embedded := &model.Plugin{ID: "skill-embedded-1", Name: "Deployer", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilitySystem, SpaceID: &space, IsEmbedded: true, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	// A standalone catalog skill referenced by the same expert. IsEmbedded=false
	// (it lives in the skill list on its own with a market placement).
	standalone := &model.Plugin{ID: "skill-standalone-1", Name: "Shared", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilitySystem, SpaceID: &space, IsEmbedded: false, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
	store := &fakeStore{
		plugins: map[string]*model.Plugin{"expert-9": expert, "skill-embedded-1": embedded, "skill-standalone-1": standalone},
		relations: map[string][]model.PluginRelation{"expert-9": {
			{ID: "rel-1", SourcePluginID: "expert-9", TargetPluginID: "skill-embedded-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill", Status: 1},
			{ID: "rel-2", SourcePluginID: "expert-9", TargetPluginID: "skill-standalone-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill", Status: 1},
		}},
	}
	svc := containerService(store)

	if _, err := svc.ReuploadContainer(context.Background(), containerCaller, "expert-9", ContainerImportParams{Archive: expertReuploadArchive(t)}); err != nil {
		t.Fatalf("ReuploadContainer: %v", err)
	}
	if len(store.rebuildOldIDs) != 1 || store.rebuildOldIDs[0] != "skill-embedded-1" {
		t.Fatalf("old child ids = %#v, want only [skill-embedded-1]", store.rebuildOldIDs)
	}
	for _, id := range store.rebuildOldIDs {
		if id == "skill-standalone-1" {
			t.Fatal("standalone relation target must NOT be soft-deleted by a reupload")
		}
	}
}

// A container reupload must re-stamp every rebuilt embedded child with the
// container top's visibility/Space/owner, not the system/global-Space defaults
// buildGraphPlugin mints — otherwise reuploading a space/private expert silently
// promotes its bundled skills to platform-global.
func TestReuploadExpertContainerStampsChildrenWithContainerScope(t *testing.T) {
	for _, vis := range []model.PluginVisibility{model.PluginVisibilitySpace, model.PluginVisibilityPrivate} {
		t.Run(string(vis), func(t *testing.T) {
			space := "space-x"
			created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
			expert := &model.Plugin{ID: "expert-9", Name: "Release Captain", Type: model.PluginTypeExpert, Visibility: vis, SpaceID: &space, OwnerUID: "owner-1", Tags: []byte(`["ops"]`), Manifest: []byte(`{}`), Package: []byte(`{}`), CreatedAt: created, UpdatedAt: created, Status: 1}
			oldSkill := &model.Plugin{ID: "skill-old-1", Name: "Deployer", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilitySystem, SpaceID: &space, OwnerUID: "owner-1", IsEmbedded: true, Tags: []byte(`[]`), Manifest: []byte(`{}`), Package: []byte(`{}`), Status: 1}
			store := &fakeStore{
				plugins: map[string]*model.Plugin{"expert-9": expert, "skill-old-1": oldSkill},
				relations: map[string][]model.PluginRelation{"expert-9": {
					{ID: "rel-old-1", SourcePluginID: "expert-9", TargetPluginID: "skill-old-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill", Status: 1},
				}},
			}
			svc := containerService(store)
			if _, err := svc.ReuploadContainer(context.Background(), containerCaller, "expert-9", ContainerImportParams{Archive: expertReuploadArchive(t)}); err != nil {
				t.Fatalf("ReuploadContainer: %v", err)
			}
			if len(store.rebuildChild) == 0 {
				t.Fatal("no children rebuilt")
			}
			for _, n := range store.rebuildChild {
				if n.Plugin.Visibility != vis {
					t.Fatalf("child visibility = %q, want inherited %q (not system)", n.Plugin.Visibility, vis)
				}
				if n.Plugin.SpaceID == nil || *n.Plugin.SpaceID != space {
					t.Fatalf("child space = %v, want inherited %q", n.Plugin.SpaceID, space)
				}
				if n.Plugin.OwnerUID != "owner-1" {
					t.Fatalf("child owner = %q, want inherited owner-1", n.Plugin.OwnerUID)
				}
			}
		})
	}
}

// sizedSkillZip packs a skill package whose SKILL.md is the given bytes (used to
// exercise the container-wide extraction budget with highly compressible content).
func sizedSkillZip(t *testing.T, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	md, _ := zw.Create("SKILL.md")
	if _, err := md.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A container that points many skill refs at ONE nested archive must parse and
// expand it exactly once and mint exactly one skill row — not hundreds — so a
// decompression bomb cannot be amplified by ref count.
func TestImportExpertContainerDedupesRepeatedSkillFile(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	refs := make([]map[string]any, 0, 10)
	for i := 0; i < 10; i++ {
		refs = append(refs, map[string]any{"name": "Common", "file": "skills/common.zip"})
	}
	manifest := map[string]any{"name": "Bulk", "summary": "many refs, one zip", "instruction": "do", "skills": refs}
	archive := containerZip(t, "expert.json", manifest, map[string][]byte{"skills/common.zip": textSkillZip(t, "Common")})

	detail, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive})
	if err != nil {
		t.Fatalf("ImportContainer: %v", err)
	}
	// One deduped skill node + the expert = 2 nodes, and a single expert_skill edge.
	if len(store.graphNodes) != 2 {
		t.Fatalf("graph nodes = %d, want 2 (one deduped skill + expert)", len(store.graphNodes))
	}
	if store.graphNodes[0].Plugin.Type != model.PluginTypeSkill {
		t.Fatalf("first node type = %q, want skill", store.graphNodes[0].Plugin.Type)
	}
	if len(detail.Relations) != 1 {
		t.Fatalf("expert relations = %d, want 1 (deduped by file)", len(detail.Relations))
	}
}

// The same nested file referenced under two different names is ambiguous (two
// skills would collapse onto one node) and is rejected.
func TestImportExpertContainerRejectsSameFileDifferentNames(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	manifest := map[string]any{"name": "Bulk", "summary": "same file two names", "instruction": "do",
		"skills": []map[string]any{
			{"name": "A", "file": "skills/x.zip"},
			{"name": "B", "file": "skills/x.zip"},
		}}
	archive := containerZip(t, "expert.json", manifest, map[string][]byte{"skills/x.zip": textSkillZip(t, "X")})
	if _, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if store.graphNodes != nil {
		t.Fatalf("an ambiguous container must not reach CreateGraph: %d nodes", len(store.graphNodes))
	}
}

// The shared container extraction budget bounds the AGGREGATE decompressed bytes
// across every bundled skill: distinct files that individually pass the per-skill
// cap but together exceed the container budget are rejected with ErrTooLarge.
func TestImportExpertContainerRejectsOverBudgetExpansion(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	svc.SetArtifactLimits(4096) // per-file cap 4096, container budget = 5*4096 = 20480
	// Six distinct skills, each a ~4000-byte SKILL.md (< per-file cap, but 6*4000
	// = 24000 > the 20480 container budget). The bytes are highly compressible so
	// the container archive itself stays tiny.
	body := bytes.Repeat([]byte("a"), 4000)
	bundles := map[string][]byte{}
	refs := make([]map[string]any, 0, 6)
	for i := 0; i < 6; i++ {
		file := "skills/s" + strconv.Itoa(i) + ".zip"
		bundles[file] = sizedSkillZip(t, body)
		refs = append(refs, map[string]any{"name": "S" + strconv.Itoa(i), "file": file})
	}
	manifest := map[string]any{"name": "Bomb", "summary": "aggregate over budget", "instruction": "do", "skills": refs}
	archive := containerZip(t, "expert.json", manifest, bundles)
	if _, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge (container budget exhausted)", err)
	}
	if store.graphNodes != nil {
		t.Fatalf("an over-budget container must not reach CreateGraph: %d nodes", len(store.graphNodes))
	}
}

// the live import stores mcp_config verbatim (no backend secret scan/blank), so
// the value survives into the rendered mcp.json attachment. This pins the
// deliberate divergence from the offline backfill's SanitizeConnectorJSON — the
// import must NOT reject or blank the config it is handed (secret handling is a
// client-side ${PLACEHOLDER} control).
func TestImportExpertContainerPreservesMCPConfigVerbatim(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{}}
	svc := containerService(store)
	manifest := map[string]any{
		"name":        "Secret Keeper",
		"summary":     "Holds a credential-shaped config.",
		"instruction": "Guard the secrets.",
		"mcp_config":  `{"mcpServers":{"x":{"env":{"API_KEY":"abc"}}}}`,
	}
	archive := containerZip(t, "expert.json", manifest, nil)

	detail, err := svc.ImportContainer(context.Background(), containerCaller, ContainerImportParams{Archive: archive})
	if err != nil {
		t.Fatalf("ImportContainer: %v", err)
	}
	mcp, ok := decodeAttachmentRaw(t, detail.Plugin.Package, "mcp.json")
	if !ok {
		t.Fatal("expert package missing mcp.json")
	}
	// The literal credential value is preserved, not rejected and not blanked.
	if !strings.Contains(mcp, `"API_KEY":"abc"`) {
		t.Fatalf("mcp_config not preserved verbatim (secret scanned/blanked?): %q", mcp)
	}
}
