package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

var adminCaller = Caller{UID: "admin-1", Name: "Root", RequestID: "req-admin"}

func adminSkillRequest() WriteRequest {
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"Ops Skill","plugin_type":"skill","name":"ops-skill","description":"An ops skill.","labels":[],"examples":[]}`)
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# Ops Skill"}]}`)
	return WriteRequest{Name: "Ops Skill", Type: model.PluginTypeSkill, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`[]`), Manifest: manifest, Package: pkg}
}

// TestAdminListForwardsAdminScopeAllSpacesAndVisibility locks the cross-Space
// read: the admin scope drops per-Space predicates (Admin=true), AllSpaces is
// set, and the visibility narrow is forwarded verbatim.
func TestAdminListForwardsAdminScopeAllSpacesAndVisibility(t *testing.T) {
	f := &fakeStore{}
	_, _, err := fixedService(f).AdminList(context.Background(), adminCaller, model.PluginTypeConnector, model.PluginVisibilitySystem, ListParams{})
	if err != nil {
		t.Fatalf("AdminList: %v", err)
	}
	if !f.listScope.Admin || f.listScope.CallerUID != "admin-1" {
		t.Fatalf("listScope=%#v want admin caller", f.listScope)
	}
	if !f.listFilter.AllSpaces {
		t.Fatalf("AdminList must set AllSpaces: %#v", f.listFilter)
	}
	if f.listFilter.Visibility != model.PluginVisibilitySystem || f.listFilter.Type != model.PluginTypeConnector {
		t.Fatalf("listFilter=%#v", f.listFilter)
	}
}

func TestAdminListRejectsBadType(t *testing.T) {
	if _, _, err := fixedService(&fakeStore{}).AdminList(context.Background(), adminCaller, model.PluginType("bogus"), "", ListParams{}); err != ErrInvalidRequest {
		t.Fatalf("err=%v want ErrInvalidRequest", err)
	}
}

// TestAdminCreateConnectorMintsSystemNullSpace verifies the connector
// convention: visibility=system and a NULL Space (system rows live outside the
// Space model), written under the admin scope.
func TestAdminCreateConnectorMintsSystemNullSpace(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{}}
	config := json.RawMessage(`{"env_user_supplied":[],"headers_user_supplied":[]}`)
	if _, err := fixedService(f).AdminCreate(context.Background(), adminCaller, connectorRequest(config)); err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	if f.create == nil {
		t.Fatal("no plugin created")
	}
	if f.create.Visibility != model.PluginVisibilitySystem {
		t.Fatalf("visibility=%q want system", f.create.Visibility)
	}
	if f.create.SpaceID != nil {
		t.Fatalf("connector Space=%v want NULL", *f.create.SpaceID)
	}
	if !f.createScope.Admin {
		t.Fatalf("create not under admin scope: %#v", f.createScope)
	}
}

// TestAdminCreateSkillMintsPublicGlobalSpace verifies the skill/expert
// convention: visibility=public in the empty global Space.
func TestAdminCreateSkillMintsPublicGlobalSpace(t *testing.T) {
	f := &fakeStore{plugins: map[string]*model.Plugin{}}
	if _, err := fixedService(f).AdminCreate(context.Background(), adminCaller, adminSkillRequest()); err != nil {
		t.Fatalf("AdminCreate: %v", err)
	}
	if f.create.Visibility != model.PluginVisibilityPublic {
		t.Fatalf("visibility=%q want public", f.create.Visibility)
	}
	if f.create.SpaceID == nil || *f.create.SpaceID != adminGlobalSpace {
		t.Fatalf("skill Space=%v want empty global", f.create.SpaceID)
	}
}

// TestAdminUpdateRejectsTypeChange guards against reclassifying a plugin's type
// through the admin update path.
func TestAdminUpdateRejectsTypeChange(t *testing.T) {
	space := adminGlobalSpace
	existing := &model.Plugin{ID: "plugin-1", Name: "Ops Skill", Type: model.PluginTypeSkill, SpaceID: &space, Visibility: model.PluginVisibilityPublic, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`)}
	f := &fakeStore{plugins: map[string]*model.Plugin{"plugin-1": existing}}
	req := connectorRequest(json.RawMessage(`{"env_user_supplied":[],"headers_user_supplied":[]}`))
	_, err := fixedService(f).AdminUpdate(context.Background(), adminCaller, "plugin-1", req)
	if err != ErrInvalidRequest {
		t.Fatalf("err=%v want ErrInvalidRequest", err)
	}
}

// TestAdminUpdatePreservesTenantVisibilityAndOwner is the A1 regression: an
// admin metadata edit of a tenant-private, cross-Space row must NOT force-flip
// it public, and must leave the owner provenance intact.
func TestAdminUpdatePreservesTenantVisibilityAndOwner(t *testing.T) {
	tenantSpace := "tenant-space"
	existing := &model.Plugin{ID: "skill-1", Name: "Tenant Skill", Type: model.PluginTypeSkill, OwnerUID: "tenant-user", SpaceID: &tenantSpace, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`)}
	f := &fakeStore{plugins: map[string]*model.Plugin{"skill-1": existing}}
	// The request even tries to set a public visibility; it must be ignored.
	req := adminSkillRequest()
	req.Visibility = model.PluginVisibilityPublic
	if _, err := fixedService(f).AdminUpdate(context.Background(), adminCaller, "skill-1", req); err != nil {
		t.Fatalf("AdminUpdate: %v", err)
	}
	if f.update == nil {
		t.Fatal("no update issued")
	}
	if f.update.Visibility != model.PluginVisibilityPrivate {
		t.Fatalf("visibility force-flipped to %q; tenant-private row would be published", f.update.Visibility)
	}
	if f.update.SpaceID == nil || *f.update.SpaceID != tenantSpace {
		t.Fatalf("space not preserved: %v", f.update.SpaceID)
	}
	if f.update.OwnerUID != "tenant-user" {
		t.Fatalf("owner rewritten to %q, want tenant-user", f.update.OwnerUID)
	}
}

// TestAdminUpdateResolvesRelationTargetsUnderAdminScope is the regression for
// the admin relation-target scope loss: an admin edit of a space-scoped expert
// that relates to a space-scoped skill must validate the relation target
// cross-Space (admin scope). Under the tenant scope the target is invisible, so
// the edit would 404 (echo) or silently drop every edge (omit).
func TestAdminUpdateResolvesRelationTargetsUnderAdminScope(t *testing.T) {
	tenant := "tenant-space"
	expert := &model.Plugin{ID: "expert-1", Name: "E", Type: model.PluginTypeExpert, OwnerUID: "tenant-user", SpaceID: &tenant, Visibility: model.PluginVisibilitySpace, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`)}
	skill := &model.Plugin{ID: "skill-1", Name: "S", Type: model.PluginTypeSkill, OwnerUID: "tenant-user", SpaceID: &tenant, Visibility: model.PluginVisibilitySpace, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`)}
	f := &fakeStore{
		scopeAware: true,
		plugins:    map[string]*model.Plugin{"expert-1": expert, "skill-1": skill},
		relations:  map[string][]model.PluginRelation{"expert-1": {{ID: "r1", SourcePluginID: "expert-1", TargetPluginID: "skill-1", Type: "expert_skill", Status: 1}}},
	}
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-1.0.json","plugin_name":"E","plugin_type":"expert","name":"e","description":"d","labels":[],"examples":[]}`)
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-1.0.json","attachments":[{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# doc"}]}`)
	req := WriteRequest{
		Name:       "E",
		Type:       model.PluginTypeExpert,
		Visibility: model.PluginVisibilitySpace,
		Tags:       json.RawMessage(`[]`),
		Manifest:   manifest,
		Package:    pkg,
		Relations:  []RelationRequest{{TargetPluginID: "skill-1", Type: "expert_skill", SortOrder: 0}},
	}
	if _, err := fixedService(f).AdminUpdate(context.Background(), adminCaller, "expert-1", req); err != nil {
		t.Fatalf("admin update echoing a space-scoped relation failed: %v", err)
	}
	// The relation survived to the repo — not dropped by a lost-scope 404.
	if f.update == nil || len(f.updateRels) != 1 || f.updateRels[0].TargetPluginID != "skill-1" {
		t.Fatalf("relation not carried to Update: %#v", f.updateRels)
	}
}

// TestAdminDeleteUsesAdminScope confirms delete runs cross-Space with no owner
// predicate (Scope.Admin), unlike the owner-gated Delete.
func TestAdminDeleteUsesAdminScope(t *testing.T) {
	space := adminGlobalSpace
	existing := &model.Plugin{ID: "plugin-1", Name: "Ops Skill", Type: model.PluginTypeSkill, SpaceID: &space, Visibility: model.PluginVisibilityPublic, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`)}
	f := &fakeStore{plugins: map[string]*model.Plugin{"plugin-1": existing}}
	if err := fixedService(f).AdminDelete(context.Background(), adminCaller, "plugin-1"); err != nil {
		t.Fatalf("AdminDelete: %v", err)
	}
	if !f.deleteScope.Admin || f.deleteID != "plugin-1" {
		t.Fatalf("deleteScope=%#v id=%q", f.deleteScope, f.deleteID)
	}
}
