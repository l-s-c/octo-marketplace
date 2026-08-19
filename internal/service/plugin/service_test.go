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
	plugins     map[string]*model.Plugin
	create      *model.Plugin
	createRels  []model.PluginRelation
	createAudit model.PluginAuditLog
	update      *model.Plugin
	deleteScope pluginrepo.Scope
	err         error
}

func (f *fakeStore) List(context.Context, pluginrepo.Scope, pluginrepo.ListFilter) ([]model.Plugin, error) {
	return nil, f.err
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
func (f *fakeStore) Create(_ context.Context, _ pluginrepo.Scope, p *model.Plugin, rels []model.PluginRelation, audit model.PluginAuditLog) error {
	f.create, f.createRels, f.createAudit = p, rels, audit
	return f.err
}
func (f *fakeStore) Update(_ context.Context, _ pluginrepo.Scope, p *model.Plugin, _ []model.PluginRelation, _ model.PluginAuditLog) error {
	f.update = p
	return f.err
}
func (f *fakeStore) Delete(_ context.Context, s pluginrepo.Scope, _ string, _ model.PluginAuditLog, _ time.Time) error {
	f.deleteScope = s
	return f.err
}
func (f *fakeStore) ListAuditLogs(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginAuditLog, error) {
	return nil, f.err
}
func (f *fakeStore) ListVersions(context.Context, pluginrepo.Scope, string, int, int) ([]model.PluginVersion, error) {
	return nil, f.err
}
func (f *fakeStore) Publish(context.Context, pluginrepo.Scope, model.PluginVersion, []model.PluginPlacement, model.PluginAuditLog) error {
	return f.err
}
func (f *fakeStore) DuplicateGraph(context.Context, pluginrepo.Scope, string, *model.Plugin, model.PluginAuditLog) error {
	return f.err
}

func fixedService(f *fakeStore) *Service {
	ids := []string{"plugin-new", "audit-new", "relation-new", "version-new", "placement-new"}
	n := 0
	return New(f).WithRuntime(func() string { x := ids[n%len(ids)]; n++; return x }, func() time.Time { return time.Date(2026, 8, 19, 8, 0, 0, 0, time.UTC) })
}

var testCaller = Caller{UID: "user-1", Name: "Alice", SpaceID: "space-a", RequestID: "request-1"}

func validRequest() WriteRequest {
	return WriteRequest{Name: "  Example Plugin  ", Type: model.PluginTypeExpert, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`["one","one","two"]`), Manifest: json.RawMessage(`{"z":1,"a":{"b":2}}`), Package: json.RawMessage(`{"files":[]}`)}
}

func TestCreateValidatesAndCanonicalizes(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{}}
	_, err := fixedService(f).Create(context.Background(), testCaller, validRequest())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.create.Name != "Example Plugin" || string(f.create.Manifest) != `{"a":{"b":2},"z":1}` {
		t.Fatalf("created = %#v", f.create)
	}
	wantManifestHash, _ := hashForTest(json.RawMessage(`{"a":{"b":2},"z":1}`))
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
		{"package scalar", func(r *WriteRequest) { r.Package = json.RawMessage(`true`) }},
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

func TestConnectorRejectsSecretValuesButAllowsNamesAndReferences(t *testing.T) {
	bad := []json.RawMessage{
		json.RawMessage(`{"config":{"env":{"API_TOKEN":"plain-token"}}}`),
		json.RawMessage(`{"client_secret":"plain-token"}`),
		json.RawMessage(`{"headers":{"Authorization":"Bearer abc"}}`),
	}
	for _, pkg := range bad {
		r := validRequest()
		r.Type = model.PluginTypeConnector
		r.Package = pkg
		if _, err := fixedService(&fakeStore{}).Create(context.Background(), testCaller, r); !errors.Is(err, ErrSecretValue) {
			t.Fatalf("package %s: err = %v", pkg, err)
		}
	}
	for _, pkg := range []json.RawMessage{
		json.RawMessage(`{"required_secret_names":["API_TOKEN"],"config":{"env":{"API_TOKEN":""}}}`),
		json.RawMessage(`{"config":{"env":{"API_TOKEN":"${API_TOKEN}"},"headers":{"Authorization":"secret://auth-header"}}}`),
	} {
		r := validRequest()
		r.Type = model.PluginTypeConnector
		r.Package = pkg
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
	if _, err := fixedService(f).Detail(context.Background(), testCaller, "plugin-other"); !errors.Is(err, ErrNotFound) {
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
	r.Type = model.PluginTypeConnector
	if _, err := fixedService(f).Create(context.Background(), testCaller, r); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("invalid source err = %v", err)
	}
}
