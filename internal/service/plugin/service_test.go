package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

type fakeStore struct {
	plugins       map[string]*model.Plugin
	create        *model.Plugin
	createRels    []model.PluginRelation
	createAudit   model.PluginAuditLog
	update        *model.Plugin
	deleteScope   pluginrepo.Scope
	publishParams pluginrepo.PublishParams
	duplicate     model.Plugin
	duplicateMeta pluginrepo.Mutation
	audits        []model.PluginAuditLog
	auditTotal    int64
	versions      []model.PluginVersion
	versionTotal  int64
	err           error
}

func (f *fakeStore) List(context.Context, pluginrepo.Scope, pluginrepo.ListFilter) ([]model.Plugin, int64, error) {
	return nil, 0, f.err
}
func (f *fakeStore) GetWithRelations(_ context.Context, _ pluginrepo.Scope, id string) (*model.Plugin, []model.PluginRelation, error) {
	if f.err != nil {
		return nil, nil, f.err
	}
	p := f.plugins[id]
	if p == nil {
		return nil, nil, pluginrepo.ErrNotFound
	}
	copy := *p
	return &copy, nil, nil
}
func (f *fakeStore) Create(_ context.Context, _ pluginrepo.Scope, m pluginrepo.Mutation) error {
	p := m.Plugin
	f.create, f.createRels = &p, m.Relations
	f.createAudit = model.PluginAuditLog{OperatorID: m.OperatorID, OperatorName: m.OperatorName, RequestID: m.RequestID, Remark: m.Remark}
	return f.err
}
func (f *fakeStore) Update(_ context.Context, _ pluginrepo.Scope, m pluginrepo.Mutation) error {
	p := m.Plugin
	f.update = &p
	return f.err
}
func (f *fakeStore) Delete(_ context.Context, s pluginrepo.Scope, _, _, _, _ string, _ *string) error {
	f.deleteScope = s
	return f.err
}
func (f *fakeStore) ListAudits(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginAuditLog, int64, error) {
	return f.audits, f.auditTotal, f.err
}
func (f *fakeStore) ListVersions(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginVersion, int64, error) {
	return f.versions, f.versionTotal, f.err
}
func (f *fakeStore) GetVersion(_ context.Context, _ pluginrepo.Scope, pluginID, version string) (*model.PluginVersion, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &model.PluginVersion{PluginID: pluginID, Version: version, Package: json.RawMessage(`{"attachments":[]}`)}, nil
}
func (f *fakeStore) Publish(_ context.Context, _ pluginrepo.Scope, p pluginrepo.PublishParams) (*model.PluginVersion, error) {
	f.publishParams = p
	return &model.PluginVersion{ID: "version-new", PluginID: p.PluginID, Version: p.Version, Manifest: json.RawMessage(`{"stored":true}`), Relations: json.RawMessage(`[]`), CreatedBy: p.CreatedBy}, f.err
}
func (f *fakeStore) DuplicateGraph(_ context.Context, _ pluginrepo.Scope, _ string, p model.Plugin, m pluginrepo.Mutation) error {
	f.duplicate, f.duplicateMeta = p, m
	return f.err
}

func fixedService(f *fakeStore) *Service {
	ids := []string{"plugin-new", "audit-new", "relation-new", "version-new", "placement-new"}
	n := 0
	return New(f).WithRuntime(func() string { x := ids[n%len(ids)]; n++; return x }, func() time.Time { return time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC) })
}

var testCaller = Caller{UID: "user-1", Name: "Alice", SpaceID: "space-a", RequestID: "request-1"}

func validRequest() WriteRequest {
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Example Plugin","plugin_type":"expert","name":"example-plugin","description":"An example plugin.","labels":["one","two"],"examples":[]}`)
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"manifest.json","content_type":"raw","mime_type":"application/json","raw_content":"{\"$schema\":\"cowork-plugin-manifest-1.0.json\",\"description\":\"An example plugin.\",\"examples\":[],\"labels\":[\"one\",\"two\"],\"name\":\"example-plugin\",\"plugin_name\":\"Example Plugin\",\"plugin_type\":\"expert\"}"}]}`)
	return WriteRequest{Name: "  Example Plugin  ", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`["one","one","two"]`), Manifest: manifest, Package: pkg}
}

func connectorRequest(config json.RawMessage) WriteRequest {
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Example Plugin","plugin_type":"connector","name":"example-plugin","description":"An example plugin.","labels":["one","two"],"examples":[],"config":` + string(config) + `}`)
	canonical, _, err := normalizeManifest(manifest, "Example Plugin", model.PluginTypeConnector, json.RawMessage(`["one","two"]`))
	if err != nil {
		panic(err)
	}
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"manifest.json","content_type":"raw","mime_type":"application/json","raw_content":` + quoted(string(canonical)) + `}]}`)
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

func TestCreateRejectsInvalidFieldsAndJSONShapes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*WriteRequest)
	}{
		{"type", func(r *WriteRequest) { r.Type = "unknown" }},
		{"visibility", func(r *WriteRequest) { r.Visibility = "world" }},
		{"system visibility", func(r *WriteRequest) { r.Visibility = model.PluginVisibilitySystem }},
		{"empty name", func(r *WriteRequest) { r.Name = " " }},
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
	if _, err := fixedService(f).Detail(context.Background(), testCaller, "expert:plugin-other", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("detail err = %v", err)
	}
	if err := fixedService(f).Delete(context.Background(), testCaller, "plugin-other"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete err = %v", err)
	}
}

func TestRelationSourceTargetValidation(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{"skill-1": {ID: "skill-1", Type: model.PluginTypeSkill}}}
	r := validRequest()
	r.Relations = []RelationRequest{{TargetPluginID: "skill:skill-1", Type: "expert_skill"}}
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

func TestHistoryListsPropagateExactTotalsAndNotFound(t *testing.T) {
	f := &fakeStore{
		audits:       []model.PluginAuditLog{{ID: "audit-1"}},
		auditTotal:   37,
		versions:     []model.PluginVersion{{ID: "version-1"}},
		versionTotal: 42,
	}
	svc := fixedService(f)
	audits, auditTotal, err := svc.ListAuditLogs(context.Background(), testCaller, "plugin-1", 20, 20)
	if err != nil || len(audits) != 1 || auditTotal != 37 {
		t.Fatalf("audits=%#v total=%d err=%v", audits, auditTotal, err)
	}
	versions, versionTotal, err := svc.ListVersions(context.Background(), testCaller, "plugin-1", 20, 40)
	if err != nil || len(versions) != 1 || versionTotal != 42 {
		t.Fatalf("versions=%#v total=%d err=%v", versions, versionTotal, err)
	}
	f.err = pluginrepo.ErrNotFound
	if _, _, err := svc.ListAuditLogs(context.Background(), testCaller, "foreign", 20, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("audit not found err=%v", err)
	}
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

func TestDuplicatePassesIndependentRootAndAuditMetadata(t *testing.T) {
	source := &model.Plugin{ID: "source", Name: "Source", Type: model.PluginTypeExpert, OwnerUID: "another", Visibility: model.PluginVisibilityPublic, Manifest: json.RawMessage(`{"a":1}`), Package: json.RawMessage(`{"b":2}`), Tags: json.RawMessage(`["x"]`), PluginHash: "sha256:p"}
	f := &fakeStore{plugins: map[string]*model.Plugin{"source": source}}
	copy, err := fixedService(f).Duplicate(context.Background(), testCaller, "source", "Copy")
	if err != nil {
		t.Fatal(err)
	}
	if copy.ID != "plugin-new" || f.duplicate.ID != copy.ID || f.duplicateMeta.OperatorID != testCaller.UID {
		t.Fatalf("copy=%#v duplicate=%#v metadata=%#v", copy, f.duplicate, f.duplicateMeta)
	}
	f.duplicate.Manifest[0] = '['
	if source.Manifest[0] == '[' {
		t.Fatal("duplicate manifest aliases source manifest")
	}
}
