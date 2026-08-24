package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

type fakeStore struct {
	plugins        map[string]*model.Plugin
	relations      map[string][]model.PluginRelation
	getIDs         []string
	listScope      pluginrepo.Scope
	listFilter     pluginrepo.ListFilter
	create         *model.Plugin
	createScope    pluginrepo.Scope
	createRels     []model.PluginRelation
	createAudit    model.PluginAuditLog
	update         *model.Plugin
	deleteScope    pluginrepo.Scope
	deleteID       string
	versionID      string
	publishParams  pluginrepo.PublishParams
	versions       []model.PluginVersion
	versionTotal   int64
	list           []model.Plugin
	memberCounts   map[string]int
	memberCountIDs []string
	tags           []model.TagFilter
	tagFilter      pluginrepo.TagListFilter
	publishErr     error
	err            error
}

func (f *fakeStore) List(_ context.Context, s pluginrepo.Scope, filter pluginrepo.ListFilter) ([]model.Plugin, int64, error) {
	f.listScope = s
	f.listFilter = filter
	return f.list, int64(len(f.list)), f.err
}
func (f *fakeStore) ListTags(_ context.Context, _ pluginrepo.Scope, filter pluginrepo.TagListFilter) ([]model.TagFilter, error) {
	f.tagFilter = filter
	return f.tags, f.err
}
func (f *fakeStore) GetWithRelations(_ context.Context, _ pluginrepo.Scope, id string) (*model.Plugin, []model.PluginRelation, error) {
	f.getIDs = append(f.getIDs, id)
	if f.err != nil {
		return nil, nil, f.err
	}
	p := f.plugins[id]
	if p == nil {
		return nil, nil, pluginrepo.ErrNotFound
	}
	copy := *p
	rels := append([]model.PluginRelation(nil), f.relations[id]...)
	return &copy, rels, nil
}
func (f *fakeStore) Create(_ context.Context, s pluginrepo.Scope, m pluginrepo.Mutation) (*pluginrepo.RelationSync, error) {
	p := m.Plugin
	f.create, f.createRels, f.createScope = &p, m.Relations, s
	f.createAudit = model.PluginAuditLog{OperatorID: m.OperatorID, OperatorName: m.OperatorName, RequestID: m.RequestID, Remark: m.Remark}
	if f.err != nil {
		return nil, f.err
	}
	created := make([]string, 0, len(m.Relations))
	for i := range m.Relations {
		if m.Relations[i].ID == "" {
			m.Relations[i].ID = "relation-created"
		}
		created = append(created, m.Relations[i].ID)
	}
	f.createRels = m.Relations
	return &pluginrepo.RelationSync{Created: created, Updated: []string{}, Deleted: []string{}, Relations: m.Relations}, nil
}
func (f *fakeStore) Update(_ context.Context, _ pluginrepo.Scope, m pluginrepo.Mutation) (*pluginrepo.RelationSync, error) {
	p := m.Plugin
	f.update = &p
	if f.err != nil {
		return nil, f.err
	}
	return &pluginrepo.RelationSync{Created: []string{}, Updated: []string{}, Deleted: []string{}, Relations: m.Relations}, nil
}
func (f *fakeStore) Delete(_ context.Context, s pluginrepo.Scope, id, _, _, _ string, _ *string) error {
	f.deleteScope, f.deleteID = s, id
	return f.err
}
func (f *fakeStore) ListVersions(_ context.Context, _ pluginrepo.Scope, id string, _, _ int) ([]model.PluginVersion, int64, error) {
	f.versionID = id
	return f.versions, f.versionTotal, f.err
}
func (f *fakeStore) Publish(_ context.Context, _ pluginrepo.Scope, p pluginrepo.PublishParams) (*model.PluginVersion, error) {
	f.publishParams = p
	if f.publishErr != nil {
		return nil, f.publishErr
	}
	return &model.PluginVersion{ID: "version-new", PluginID: p.PluginID, Version: p.Version, Manifest: json.RawMessage(`{"stored":true}`), Relations: json.RawMessage(`[]`), CreatedBy: p.CreatedBy}, f.err
}
func (f *fakeStore) CountMemberRelations(_ context.Context, teamIDs []string) (map[string]int, error) {
	f.memberCountIDs = teamIDs
	if f.memberCounts == nil {
		return map[string]int{}, f.err
	}
	return f.memberCounts, f.err
}

func fixedService(f *fakeStore) *Service {
	ids := []string{"plugin-new", "audit-new", "relation-new", "version-new", "placement-new"}
	n := 0
	return New(f).WithRuntime(func() string { x := ids[n%len(ids)]; n++; return x }, func() time.Time { return time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC) })
}

var testCaller = Caller{UID: "user-1", Name: "Alice", SpaceID: "space-a", RequestID: "request-1"}

func validRequest() WriteRequest {
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Example Plugin","plugin_type":"expert","name":"example-plugin","description":"An example plugin.","labels":["one","two"],"examples":[]}`)
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# Example Plugin"}]}`)
	return WriteRequest{Name: "  Example Plugin  ", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`["one","one","two"]`), Manifest: manifest, Package: pkg}
}

func connectorRequest(config json.RawMessage) WriteRequest {
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Example Plugin","plugin_type":"connector","name":"example-plugin","description":"An example plugin.","labels":["one","two"],"examples":[],"config":` + string(config) + `}`)
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","connector":{"type":"mcp","source":"connector.example-plugin"},"attachments":[{"path":"mcp.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"mcpServers\":{}}"}]}`)
	return WriteRequest{Name: "Example Plugin", Type: model.PluginTypeConnector, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`["one","two"]`), Manifest: manifest, Package: pkg}
}

func TestCreateValidatesAndCanonicalizes(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{}}
	_, err := fixedService(f).Create(context.Background(), testCaller, validRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wantManifest := `{"$schema":"cowork-plugin-manifest-1.0.json","description":"An example plugin.","examples":[],"labels":["one","two"],"name":"example-plugin","plugin_name":"Example Plugin","plugin_type":"expert"}`
	if f.create.Name != "Example Plugin" || string(f.create.Manifest) != wantManifest {
		t.Fatalf("created = %#v", f.create)
	}
	wantManifestHash, _ := hashForTest(json.RawMessage(wantManifest))
	if f.create.ManifestHash != wantManifestHash {
		t.Fatalf("manifest hash = %q, want %q", f.create.ManifestHash, wantManifestHash)
	}
	if f.create.PluginHash == "" || f.create.PluginHash == f.create.ManifestHash {
		t.Fatalf("combined plugin hash = %q", f.create.PluginHash)
	}
	if string(f.create.Tags) != `["one","two"]` {
		t.Fatalf("tags = %s", f.create.Tags)
	}
}

func TestListRejectsUnsupportedSort(t *testing.T) {
	svc := fixedService(&fakeStore{})
	for _, params := range []ListParams{{Sort: "surprise"}, {Sort: "placement"}} {
		if _, _, err := svc.List(context.Background(), testCaller, params); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("List(%#v) error = %v, want ErrInvalidRequest", params, err)
		}
	}
}

func TestListFillsTeamMemberCountsAndKeepsIcons(t *testing.T) {
	f := &fakeStore{
		list: []model.Plugin{
			{ID: "team-1", Type: model.PluginTypeExpertTeam},
			{ID: "skill-1", Type: model.PluginTypeSkill, Icon: "https://cdn.example.com/icon.png"},
		},
		memberCounts: map[string]int{"team-1": 4},
	}
	items, _, err := fixedService(f).List(context.Background(), testCaller, ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.memberCountIDs) != 1 || f.memberCountIDs[0] != "team-1" {
		t.Fatalf("member count lookup = %#v", f.memberCountIDs)
	}
	if items[0].MemberCount != 4 || items[1].MemberCount != 0 {
		t.Fatalf("member counts = %d,%d", items[0].MemberCount, items[1].MemberCount)
	}
	// Without storage configured, http(s) icons pass through unchanged.
	if items[1].IconURL != "https://cdn.example.com/icon.png" {
		t.Fatalf("icon_url = %q", items[1].IconURL)
	}
}

func TestCreateStoresIconAndMaterializesConnectorToolCount(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{}}
	req := connectorRequest(json.RawMessage(`{}`))
	req.Icon = " https://cdn.example.com/icon.png "
	var pkg map[string]any
	if err := json.Unmarshal(req.Package, &pkg); err != nil {
		t.Fatal(err)
	}
	tools := map[string]any{"path": "connector/tools.json", "content_type": "raw", "mime_type": "application/json", "raw_content": `[{"name":"a"},{"name":"b"},{"name":"c"}]`}
	pkg["attachments"] = append(pkg["attachments"].([]any), tools)
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	req.Package = raw
	if _, err := fixedService(f).Create(context.Background(), testCaller, req); err != nil {
		t.Fatal(err)
	}
	if f.create.Icon != "https://cdn.example.com/icon.png" {
		t.Fatalf("icon = %q", f.create.Icon)
	}
	if f.create.ToolCount != 3 {
		t.Fatalf("tool_count = %d", f.create.ToolCount)
	}
}

func TestListNormalizesAndBoundsTagFilters(t *testing.T) {
	f := &fakeStore{}
	svc := fixedService(f)
	if _, _, err := svc.List(context.Background(), testCaller, ListParams{Tags: []string{" a ", "", "b", "a"}}); err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(f.listFilter.Tags) != 2 || f.listFilter.Tags[0] != "a" || f.listFilter.Tags[1] != "b" {
		t.Fatalf("tags = %#v", f.listFilter.Tags)
	}
	over := make([]string, maxListTags+1)
	for i := range over {
		over[i] = "t" + strconv.Itoa(i)
	}
	for _, tags := range [][]string{over, {strings.Repeat("x", maxTagBytes+1)}, {"bad\xff"}} {
		if _, _, err := svc.List(context.Background(), testCaller, ListParams{Tags: tags}); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("List(tags=%d) error = %v, want ErrInvalidRequest", len(tags), err)
		}
	}
}

func TestListTagsValidatesAndForwardsScopedFilter(t *testing.T) {
	f := &fakeStore{tags: []model.TagFilter{{Name: "dev", Count: 3}}}
	svc := fixedService(f)
	tags, err := svc.ListTags(context.Background(), testCaller, TagListParams{PlacementCode: " default ", Type: model.PluginTypeConnector, Keyword: " de ", Mine: true, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Name != "dev" {
		t.Fatalf("tags = %#v", tags)
	}
	want := pluginrepo.TagListFilter{PlacementCode: "default", Type: model.PluginTypeConnector, Keyword: "de", Mine: true, Limit: 5}
	if f.tagFilter != want {
		t.Fatalf("filter = %#v", f.tagFilter)
	}
	if _, err := svc.ListTags(context.Background(), testCaller, TagListParams{Type: "bogus"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("type err = %v", err)
	}
	if _, err := svc.ListTags(context.Background(), testCaller, TagListParams{Limit: maxListLimit + 1}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("limit err = %v", err)
	}
	if _, err := svc.ListTags(context.Background(), Caller{}, TagListParams{}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("caller err = %v", err)
	}
}

func TestCreateRejectsInvalidFieldsAndJSONShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*WriteRequest)
	}{
		{"type", func(r *WriteRequest) { r.Type = "unknown" }},
		{"visibility", func(r *WriteRequest) { r.Visibility = "world" }},
		{"system visibility", func(r *WriteRequest) { r.Visibility = model.PluginVisibilitySystem }},
		{"empty name", func(r *WriteRequest) { r.Name = " " }},
		{"javascript icon", func(r *WriteRequest) { r.Icon = "javascript:alert(1)" }},
		{"data icon", func(r *WriteRequest) { r.Icon = "data:text/html;base64,x" }},
		{"traversal icon", func(r *WriteRequest) { r.Icon = "icons/../../etc/passwd" }},
		{"oversized icon", func(r *WriteRequest) { r.Icon = "https://cdn.example.com/" + strings.Repeat("x", maxIconBytes) }},
		{"manifest array", func(r *WriteRequest) { r.Manifest = json.RawMessage(`[]`) }},
		{"outer manifest name mismatch", func(r *WriteRequest) { r.Name = "Other" }},
		{"outer manifest type mismatch", func(r *WriteRequest) { r.Type = model.PluginTypeSkill }},
		{"outer manifest tags mismatch", func(r *WriteRequest) { r.Tags = json.RawMessage(`["other"]`) }},
		{"package scalar", func(r *WriteRequest) { r.Package = json.RawMessage(`true`) }},
		{"package missing manifest", func(r *WriteRequest) {
			r.Package = json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[]}`)
		}},
		{"tags object", func(r *WriteRequest) { r.Tags = json.RawMessage(`{}`) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validRequest()
			tt.mutate(&r)
			_, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestConnectorRejectsCamelCaseSecretFields(t *testing.T) {
	for _, key := range []string{"clientSecret", "accessToken", "privateKeyValue"} {
		r := connectorRequest(json.RawMessage(`{"` + key + `":"plain-value"}`))
		if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); !errors.Is(err, ErrSecretValue) {
			t.Fatalf("field %s accepted: %v", key, err)
		}
	}
}

func TestConnectorRejectsSecretValuesButAllowsNamesAndReferences(t *testing.T) {
	bad := []json.RawMessage{
		json.RawMessage(`{"config":{"env":{"API_TOKEN":"plain-token"}}}`),
		json.RawMessage(`{"client_secret":"plain-token"}`),
		json.RawMessage(`{"nested":{"client_secret_value":"plain-token"}}`),
		json.RawMessage(`{"headers":{"Authorization":"Bearer abc"}}`),
		json.RawMessage(`{"transport":{"bearer":"plain-token"}}`),
		json.RawMessage(`{"transport":{"auth":"plain-token"}}`),
		json.RawMessage(`{"config":{"credentials":{"username":"octo","value":"plain-token"}}}`),
		json.RawMessage(`{"config":{"secrets":{"CUSTOM_NAME":"plain-token"}}}`),
		json.RawMessage(`{"items":[{"deep":{"auth":{"value":"plain-token"}}}]}`),
	}
	for _, pkg := range bad {
		r := connectorRequest(pkg)
		if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); !errors.Is(err, ErrSecretValue) {
			t.Fatalf("package %s: err = %v", pkg, err)
		}
	}
	for _, pkg := range []json.RawMessage{
		json.RawMessage(`{"required_secret_names":["API_TOKEN"],"config":{"env":{"API_TOKEN":"","REGION":"us-east-1"}}}`),
		json.RawMessage(`{"config":{"env":{"API_TOKEN":"${API_TOKEN}"},"headers":{"Authorization":"secret://auth-header","Accept":"application/json"}}}`),
		json.RawMessage(`{"client_secret_value":"vault://connectors/client-secret","bearer":"env://BEARER_TOKEN","auth":"${AUTH_VALUE}"}`),
		json.RawMessage(`{"secrets":[{"name":"API_TOKEN","description":"connector token","required":true},{"ref":"secret://API_TOKEN"}]}`),
		json.RawMessage(`{"credentials":{"name":"OAUTH_CREDENTIALS","description":"configured externally"}}`),
	} {
		r := connectorRequest(pkg)
		if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); err != nil {
			t.Fatalf("package %s: %v", pkg, err)
		}
	}
}

func TestCreateStampsTrustedIdentity(t *testing.T) {
	f := &fakeStore{}
	caller := testCaller
	caller.BotUID, caller.BotName = "bot-1", "Builder"
	if _, err := fixedService(f).Create(context.Background(), caller, validRequest()); err != nil {
		t.Fatal(err)
	}
	if f.create.OwnerUID != caller.UID || f.create.SpaceID == nil || *f.create.SpaceID != caller.SpaceID || f.create.CreatorName != caller.Name {
		t.Fatalf("identity not stamped: %#v", f.create)
	}
	if f.create.CreatedByType != "bot" || f.create.CreatedByBotUID == nil || *f.create.CreatedByBotUID != "bot-1" {
		t.Fatalf("bot provenance = %#v", f.create)
	}
	if f.createAudit.OperatorID != caller.UID || f.createAudit.OperatorName != caller.Name || f.createAudit.RequestID != caller.RequestID {
		t.Fatalf("audit identity = %#v", f.createAudit)
	}
}

func TestCrossSpaceNotFoundPropagatesWithoutLeak(t *testing.T) {
	f := &fakeStore{err: pluginrepo.ErrNotFound}
	if _, err := fixedService(f).Detail(context.Background(), testCaller, "plugin-other", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detail err = %v", err)
	}
	if err := fixedService(f).Delete(context.Background(), testCaller, "plugin-other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete err = %v", err)
	}
}

func TestRelationSourceTargetValidation(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{"skill-1": {ID: "skill-1", Type: model.PluginTypeSkill}}}
	r := validRequest()
	r.Relations = []RelationRequest{{TargetPluginID: "skill-1", Type: "expert_skill"}}
	if _, err := fixedService(f).Create(context.Background(), testCaller, r); err != nil {
		t.Fatalf("valid relation: %v", err)
	}
	r.Relations[0].Data = json.RawMessage(`{"accessToken":"plain-value"}`)
	if _, err := fixedService(f).Create(context.Background(), testCaller, r); !errors.Is(err, ErrSecretValue) {
		t.Fatalf("secret relation data accepted: %v", err)
	}
	r.Relations[0].Data = nil
	r.Type = model.PluginTypeConnector
	if _, err := fixedService(f).Create(context.Background(), testCaller, r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid source err = %v", err)
	}
}

func TestVersionListPropagatesExactTotalsAndNotFound(t *testing.T) {
	f := &fakeStore{
		plugins:      map[string]*model.Plugin{"plugin-1": {ID: "plugin-1", Type: model.PluginTypeExpert}},
		versions:     []model.PluginVersion{{ID: "version-1"}},
		versionTotal: 42,
	}
	svc := fixedService(f)
	versions, versionTotal, err := svc.ListVersions(context.Background(), testCaller, "plugin-1", 20, 40)
	if err != nil || len(versions) != 1 || versionTotal != 42 {
		t.Fatalf("versions=%#v total=%d err=%v", versions, versionTotal, err)
	}
	f.err = pluginrepo.ErrNotFound
	if _, _, err := svc.ListVersions(context.Background(), testCaller, "foreign", 20, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version not found err=%v", err)
	}
}

func TestPublishUsesRepositoryContractAndReturnedVersionID(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{
		"plugin-1": {ID: "plugin-1", Type: model.PluginTypeExpert, OwnerUID: testCaller.UID, SpaceID: stringPtr(testCaller.SpaceID), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`), ManifestHash: "sha256:m", PluginHash: "sha256:p"},
	}}
	version, err := fixedService(f).Publish(context.Background(), testCaller, "plugin-1", PublishRequest{Version: "1.0.0", Placements: []PlacementRequest{{PlacementCode: "home", Visible: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if version.ID != "version-new" || string(version.Manifest) != `{"stored":true}` || f.publishParams.PluginID != "plugin-1" || f.publishParams.CreatedBy != testCaller.UID || len(f.publishParams.Placements) != 1 {
		t.Fatalf("version=%#v params=%#v", version, f.publishParams)
	}
}

func TestRepositoryConflictsAndInvalidPlacementsMapToServiceErrors(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{
		"plugin-1": {ID: "plugin-1", Type: model.PluginTypeExpert, OwnerUID: testCaller.UID, SpaceID: stringPtr(testCaller.SpaceID)},
	}}
	f.err = pluginrepo.ErrConflict
	if err := fixedService(f).Delete(context.Background(), testCaller, "plugin-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Delete conflict = %v, want ErrConflict", err)
	}
	f.err = pluginrepo.ErrInvalidPlacement
	if _, err := fixedService(f).Publish(context.Background(), testCaller, "plugin-1", PublishRequest{Version: "1.0.0"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Publish invalid placement = %v, want ErrInvalidRequest", err)
	}
}

func expertRequestWithConfigAttachment(configJSON string) WriteRequest {
	r := validRequest()
	r.Package = json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[` +
		`{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# Example Plugin"},` +
		`{"path":"mcp.json","content_type":"raw","mime_type":"application/json","raw_content":` + quoted(configJSON) + `}]}`)
	return r
}

func TestSecretScanCoversEmbeddedAttachmentJSONForAllTypes(t *testing.T) {
	bad := []string{
		`{"env":{"API_TOKEN":"plain-token"}}`,
		`{"headers":{"Authorization":"Bearer abc"}}`,
		`{"credentials":{"CUSTOM_NAME":"plain-token"}}`,
		`{"wrapper":` + quoted(`{"secrets":{"CUSTOM":"plain-token"}}`) + `}`,
	}
	for _, config := range bad {
		r := expertRequestWithConfigAttachment(config)
		if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); !errors.Is(err, ErrSecretValue) {
			t.Fatalf("config %s: err = %v, want ErrSecretValue", config, err)
		}
	}
	good := []string{
		`{"env":{"API_TOKEN":"${API_TOKEN}","REGION":"us-east-1"},"headers":{"Accept":"application/json"}}`,
		`{"required_secret_names":["API_TOKEN"],"transport":"stdio"}`,
	}
	for _, config := range good {
		r := expertRequestWithConfigAttachment(config)
		if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); err != nil {
			t.Fatalf("config %s: %v", config, err)
		}
	}
}

func TestSecretScanFailsClosedOnPathologicalNesting(t *testing.T) {
	payload := `{"note":"harmless"}`
	for i := 0; i < maxEmbeddedSecretScanDepth+1; i++ {
		payload = `{"nested":` + quoted(payload) + `}`
	}
	r := expertRequestWithConfigAttachment(payload)
	if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); !errors.Is(err, ErrSecretValue) {
		t.Fatalf("err = %v, want ErrSecretValue", err)
	}
}

func TestCreateRejectsSubmittedRelationIDs(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{"target-1": {ID: "target-1", Type: model.PluginTypeSkill}}}
	req := validRequest()
	req.Relations = []RelationRequest{{ID: "rel-1", TargetPluginID: "target-1", Type: "expert_skill"}}
	if _, err := fixedService(f).Create(context.Background(), testCaller, req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestUpdateAcceptsRelationIDAndMatchingSourceWireID(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{
		"plugin-1": {ID: "plugin-1", Name: "Example Plugin", Type: model.PluginTypeExpert, OwnerUID: testCaller.UID, SpaceID: stringPtr(testCaller.SpaceID)},
		"target-1": {ID: "target-1", Type: model.PluginTypeSkill},
	}}
	req := validRequest()
	req.Relations = []RelationRequest{{ID: "rel-1", SourcePluginID: "plugin-1", TargetPluginID: "target-1", Type: "expert_skill"}}
	v, err := fixedService(f).Update(context.Background(), testCaller, "plugin-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Relations) != 1 || v.Relations[0].ID != "rel-1" {
		t.Fatalf("relations = %#v", v.Relations)
	}
	if v.RelationResult == nil {
		t.Fatal("missing relation result")
	}
}

func TestUpdateRejectsForeignRelationSourceWireID(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{
		"plugin-1": {ID: "plugin-1", Name: "Example Plugin", Type: model.PluginTypeExpert, OwnerUID: testCaller.UID, SpaceID: stringPtr(testCaller.SpaceID)},
		"target-1": {ID: "target-1", Type: model.PluginTypeSkill},
	}}
	req := validRequest()
	req.Relations = []RelationRequest{{SourcePluginID: "another-plugin", TargetPluginID: "target-1", Type: "expert_skill"}}
	if _, err := fixedService(f).Update(context.Background(), testCaller, "plugin-1", req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestCreateReturnsRelationResultWithGeneratedIDs(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{"target-1": {ID: "target-1", Type: model.PluginTypeSkill}}}
	req := validRequest()
	req.Relations = []RelationRequest{{TargetPluginID: "target-1", Type: "expert_skill"}}
	v, err := fixedService(f).Create(context.Background(), testCaller, req)
	if err != nil {
		t.Fatal(err)
	}
	if v.RelationResult == nil || len(v.RelationResult.Created) != 1 || v.RelationResult.Created[0] != "relation-created" {
		t.Fatalf("relation result = %#v", v.RelationResult)
	}
	if len(v.Relations) != 1 || v.Relations[0].ID != "relation-created" {
		t.Fatalf("relations = %#v", v.Relations)
	}
}

func TestSecretScanAllowsPlaceholderReferencesInMCPConfig(t *testing.T) {
	// ${KEY} marks a user-supplied value injected at install time; both the
	// service and repository scanners must treat it as a reference, never a
	// stored secret. Real literals in the same containers stay rejected.
	ok := json.RawMessage(`{"mcpServers":{"jira":{"headers":{"Authorization":"${TOKEN}"},"env":{"API_KEY":"${API_KEY}"}}}}`)
	if err := rejectSecretValues(ok); err != nil {
		t.Fatalf("placeholder rejected: %v", err)
	}
	bad := json.RawMessage(`{"mcpServers":{"jira":{"headers":{"Authorization":"Bearer real-token"}}}}`)
	if err := rejectSecretValues(bad); !errors.Is(err, ErrSecretValue) {
		t.Fatalf("literal accepted: %v", err)
	}
}

func TestValidRelationTypeMirrorsLibEndpointMatrix(t *testing.T) {
	valid := []struct {
		relation string
		source   model.PluginType
		target   model.PluginType
	}{
		{"expert_team_expert", model.PluginTypeExpertTeam, model.PluginTypeExpert},
		{"expert_skill", model.PluginTypeExpert, model.PluginTypeSkill},
		{"expert_connector", model.PluginTypeExpert, model.PluginTypeConnector},
	}
	for _, tt := range valid {
		if !validRelationType(tt.relation, tt.source, tt.target) {
			t.Fatalf("%s %s->%s rejected", tt.relation, tt.source, tt.target)
		}
	}
	invalid := []struct {
		relation string
		source   model.PluginType
		target   model.PluginType
	}{
		{"expert_team_member", model.PluginTypeExpertTeam, model.PluginTypeExpert}, // retired name
		{"plugin_dependency", model.PluginTypeExpert, model.PluginTypeSkill},       // dropped enum
		{"expert_skill", model.PluginTypeExpertTeam, model.PluginTypeSkill},        // teams reach skills via members
		{"expert_skill", model.PluginTypeExpert, model.PluginTypeConnector},
		{"expert_team_expert", model.PluginTypeExpert, model.PluginTypeExpert},
		{"garbage", model.PluginTypeExpert, model.PluginTypeSkill},
	}
	for _, tt := range invalid {
		if validRelationType(tt.relation, tt.source, tt.target) {
			t.Fatalf("%s %s->%s accepted", tt.relation, tt.source, tt.target)
		}
	}
}
