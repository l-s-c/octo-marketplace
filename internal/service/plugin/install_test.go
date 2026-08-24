package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	expertsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/expert"
)

type fakeProvisioner struct {
	in        expertsvc.InstallInput
	agentSpec *expertsvc.ProvisionAgentSpec
	squad     *model.Squad
	err       error
}

func (f *fakeProvisioner) ProvisionAgentFromSpec(_ context.Context, in expertsvc.InstallInput, spec expertsvc.ProvisionAgentSpec) (string, error) {
	f.in, f.agentSpec = in, &spec
	return "agent-9", f.err
}
func (f *fakeProvisioner) ProvisionSquadFromSpec(_ context.Context, in expertsvc.InstallInput, m *model.Squad) (expertsvc.InstallSquadResult, error) {
	f.in, f.squad = in, m
	return expertsvc.InstallSquadResult{SquadID: "squad-9", LeaderAgentID: "agent-1"}, f.err
}

type fakeTracker struct {
	typ, id string
}

func (f *fakeTracker) TrackInstall(_ context.Context, resourceType, resourceID string) error {
	f.typ, f.id = resourceType, resourceID
	return nil
}
func (f *fakeTracker) TrackDownload(_ context.Context, resourceType, resourceID string) error {
	f.typ, f.id = resourceType, resourceID
	return nil
}

func packageWith(attachments ...string) json.RawMessage {
	return json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` + joinComma(attachments) + `]}`)
}
func joinComma(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ","
		}
		out += item
	}
	return out
}
func rawAtt(path, content string) string {
	quotedContent, _ := json.Marshal(content)
	return `{"path":"` + path + `","content_type":"raw","mime_type":"text/plain","raw_content":` + string(quotedContent) + `}`
}

func installFixture() *fakeStore {
	space := "space-a"
	skillPkg := packageWith(rawAtt("skill/ref.json", `{"file_name":"pack.zip","file_size":9,"files":["a.md"],"object_key":"legacy/skill.md","zip_object_key":"legacy/pack.zip"}`))
	expertPkg := packageWith(rawAtt("AGENTS.md", "do the work"), rawAtt("mcp.json", `{"mcpServers":{}}`))
	teamPkg := packageWith(rawAtt("AGENTS.md", "# Team\n\n## 协作方式\n1. first\n2. second"))
	return &fakeStore{
		plugins: map[string]*model.Plugin{
			"expert-1": {ID: "expert-1", Name: "Alice", Type: model.PluginTypeExpert, OwnerUID: "user-1", SpaceID: &space, Manifest: json.RawMessage(`{"description":"expert summary"}`), Package: expertPkg},
			"team-1":   {ID: "team-1", Name: "Team", Type: model.PluginTypeExpertTeam, OwnerUID: "user-1", SpaceID: &space, Manifest: json.RawMessage(`{"description":"team summary"}`), Package: teamPkg},
			"skill-1":  {ID: "skill-1", Name: "Deploy", Type: model.PluginTypeSkill, OwnerUID: "user-1", SpaceID: &space, Manifest: json.RawMessage(`{}`), Package: skillPkg},
		},
		relations: map[string][]model.PluginRelation{
			"expert-1": {{ID: "r1", SourcePluginID: "expert-1", TargetPluginID: "skill-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill", SortOrder: 0, Status: 1}},
			"team-1":   {{ID: "r2", SourcePluginID: "team-1", TargetPluginID: "expert-1", TargetPluginType: model.PluginTypeExpert, Type: "expert_team_expert", SortOrder: 0, Status: 1, Data: json.RawMessage(`{"member_key":"m1","role":"leader","is_leader":true}`)}},
		},
	}
}

func TestInstallExpertBuildsSpecFromAttachmentsAndRelations(t *testing.T) {
	f := installFixture()
	prov := &fakeProvisioner{}
	tracker := &fakeTracker{}
	svc := fixedService(f).WithProvisioner(prov).WithMetrics(tracker)
	outcome, err := svc.Install(context.Background(), testCaller, "expert-1", InstallParams{WorkspaceID: " ws-1 ", RuntimeID: "rt-1", Token: "octo-token"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.AgentID != "agent-9" || outcome.SquadID != "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	if prov.in.WorkspaceID != "ws-1" || prov.in.RuntimeID != "rt-1" || prov.in.SpaceID != "space-a" || prov.in.Token != "octo-token" {
		t.Fatalf("install input = %#v", prov.in)
	}
	spec := prov.agentSpec
	if spec.Name != "Alice" || spec.Summary != "expert summary" || spec.Instruction != "do the work" || spec.MCPConfig != `{"mcpServers":{}}` {
		t.Fatalf("spec = %#v", spec)
	}
	if len(spec.Skills) != 1 || spec.Skills[0].Name != "Deploy" || spec.Skills[0].ObjectKey != "legacy/skill.md" || spec.Skills[0].ZipObjectKey != "legacy/pack.zip" {
		t.Fatalf("skills = %#v", spec.Skills)
	}
	if tracker.typ != "plugin" || tracker.id != "expert-1" {
		t.Fatalf("tracked = %s %s", tracker.typ, tracker.id)
	}
}

func TestInstallTeamBuildsSquadModelFromAgentsDocAndMembers(t *testing.T) {
	f := installFixture()
	prov := &fakeProvisioner{}
	svc := fixedService(f).WithProvisioner(prov)
	outcome, err := svc.Install(context.Background(), testCaller, "team-1", InstallParams{WorkspaceID: "ws-1", RuntimeID: "rt-1", Token: "octo-token"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.SquadID != "squad-9" || outcome.AgentID != "" {
		t.Fatalf("outcome = %#v", outcome)
	}
	squad := prov.squad
	// Contract layout: the AGENTS.md prose is the squad's dispatch document;
	// leadership comes from member relation data, not a config attachment.
	if squad.Name != "Team" || squad.Summary != "team summary" || squad.Leader != "" {
		t.Fatalf("squad = %#v", squad)
	}
	if squad.Instructions != "# Team\n\n## 协作方式\n1. first\n2. second" {
		t.Fatalf("instructions = %q", squad.Instructions)
	}
	if len(squad.Strategies) != 0 {
		t.Fatalf("strategies = %#v", squad.Strategies)
	}
	if len(squad.Members) != 1 {
		t.Fatalf("members = %#v", squad.Members)
	}
	member := squad.Members[0]
	if member.Name != "Alice" || member.Role != "leader" || !member.IsLeader || member.MemberKey != "m1" || member.Instruction != "do the work" {
		t.Fatalf("member = %#v", member)
	}
	if len(member.Skills) != 1 || member.Skills[0].ZipObjectKey != "legacy/pack.zip" {
		t.Fatalf("member skills = %#v", member.Skills)
	}
}

func TestInstallRejectsNonInstallableTypesAndMissingProvisioner(t *testing.T) {
	f := installFixture()
	svc := fixedService(f).WithProvisioner(&fakeProvisioner{})
	if _, err := svc.Install(context.Background(), testCaller, "skill-1", InstallParams{WorkspaceID: "ws", RuntimeID: "rt"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("skill install err = %v", err)
	}
	if _, err := svc.Install(context.Background(), testCaller, "expert-1", InstallParams{WorkspaceID: "", RuntimeID: "rt"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("missing workspace err = %v", err)
	}
	bare := fixedService(installFixture())
	if _, err := bare.Install(context.Background(), testCaller, "expert-1", InstallParams{WorkspaceID: "ws", RuntimeID: "rt"}); !errors.Is(err, expertsvc.ErrFleetNotConfigured) {
		t.Fatalf("no provisioner err = %v", err)
	}
}

func TestInstallSanitizesUnknownPlugin(t *testing.T) {
	svc := fixedService(installFixture()).WithProvisioner(&fakeProvisioner{})
	if _, err := svc.Install(context.Background(), testCaller, "other-space", InstallParams{WorkspaceID: "ws", RuntimeID: "rt"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestSkillRefPrefersStoragePackageWhenLegacyPointerAbsent(t *testing.T) {
	pkg := json.RawMessage(`{"attachments":[{"path":"skill/package.zip","content_type":"storage","storage_uri":"plugins/space-a/attachments/x.zip"}]}`)
	svc := New(&fakeStore{}, &importStorage{objects: map[string][]byte{}})
	ref := svc.skillRefFromPlugin(context.Background(), &model.Plugin{Name: "New Skill", Package: pkg})
	if ref.Name != "New Skill" || ref.ZipObjectKey != "plugins/space-a/attachments/x.zip" {
		t.Fatalf("ref = %#v", ref)
	}
}

func TestSkillRefResolvesTreeContentInline(t *testing.T) {
	space := "space-a"
	binKey := "plugins/space-a/attachments/asset.bin"
	txtKey := "plugins/space-a/attachments/big.txt"
	pkg := json.RawMessage(`{"attachments":[` +
		`{"path":"SKILL.md","content_type":"raw","raw_content":"# doc"},` +
		`{"path":"scripts/run.sh","content_type":"raw","raw_content":"echo hi"},` +
		`{"path":"references/big.txt","content_type":"storage","storage_uri":"` + txtKey + `"},` +
		`{"path":"assets/asset.bin","content_type":"storage","storage_uri":"` + binKey + `"}` +
		`]}`)
	blobs := &importStorage{objects: map[string][]byte{
		txtKey: []byte("spilled text"),
		binKey: {0x00, 0xff, 0xfe},
	}}
	svc := New(&fakeStore{}, blobs)
	ref := svc.skillRefFromPlugin(context.Background(), &model.Plugin{Name: "Tree Skill", SpaceID: &space, Package: pkg})

	if ref.Markdown != "# doc" || ref.ObjectKey != "" || ref.ZipObjectKey != "" {
		t.Fatalf("tree ref should resolve inline: %#v", ref)
	}
	got := map[string]string{}
	for _, f := range ref.SupportingFiles {
		got[f.Path] = f.Content
	}
	// Inline text + storage text are included; the binary is skipped.
	if got["scripts/run.sh"] != "echo hi" || got["references/big.txt"] != "spilled text" {
		t.Fatalf("supporting files = %#v", ref.SupportingFiles)
	}
	if _, ok := got["assets/asset.bin"]; ok {
		t.Fatalf("binary should be skipped: %#v", ref.SupportingFiles)
	}
}
