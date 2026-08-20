package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestDetailParsesStorageIDAndTypeChecksPrefix(t *testing.T) {
	store := &fakeStore{
		plugins: map[string]*model.Plugin{
			"opaque-1": {ID: "opaque-1", Type: model.PluginTypeSkill},
		},
		relations: map[string][]model.PluginRelation{
			"opaque-1": {{SourcePluginID: "opaque-1", TargetPluginID: "connector-1", TargetPluginType: model.PluginTypeConnector}},
		},
	}
	detail, err := fixedService(store).Detail(context.Background(), testCaller, "skill:opaque-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.getIDs) != 1 || store.getIDs[0] != "opaque-1" {
		t.Fatalf("repository IDs = %#v, want opaque storage ID", store.getIDs)
	}
	if len(detail.Relations) != 1 || detail.Relations[0].SourcePluginType != model.PluginTypeSkill {
		t.Fatalf("detail relations = %#v", detail.Relations)
	}

	store.getIDs = nil
	if _, err := fixedService(store).Detail(context.Background(), testCaller, "expert:opaque-1", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("mismatched prefix error = %v, want ErrNotFound", err)
	}
}

func TestAllRowIDMethodsRejectRawIDsBeforeRepository(t *testing.T) {
	store := &fakeStore{}
	svc := fixedService(store)
	ctx := context.Background()
	checks := []struct {
		name string
		run  func() error
	}{
		{"detail", func() error { _, err := svc.Detail(ctx, testCaller, "raw-id", true); return err }},
		{"update", func() error { _, err := svc.Update(ctx, testCaller, "raw-id", validRequest()); return err }},
		{"delete", func() error { return svc.Delete(ctx, testCaller, "raw-id") }},
		{"audit", func() error { _, _, err := svc.ListAuditLogs(ctx, testCaller, "raw-id", 20, 0); return err }},
		{"versions", func() error { _, _, err := svc.ListVersions(ctx, testCaller, "raw-id", 20, 0); return err }},
		{"publish", func() error {
			_, err := svc.Publish(ctx, testCaller, "raw-id", PublishRequest{Version: "1.0.0"})
			return err
		}},
		{"duplicate", func() error { _, err := svc.Duplicate(ctx, testCaller, "raw-id", "Copy"); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if len(store.getIDs) != 0 || store.deleteID != "" || store.auditID != "" || store.versionID != "" || store.publishParams.PluginID != "" || store.duplicateID != "" {
		t.Fatalf("raw ID reached repository: %#v", store)
	}
}

func TestDetailRejectsNonRowWireIDsBeforeRepository(t *testing.T) {
	for _, wireID := range []string{"opaque-1", " skill:opaque-1", "expert_member:team-1:member-1"} {
		store := &fakeStore{}
		if _, err := fixedService(store).Detail(context.Background(), testCaller, wireID, true); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Detail(%q) error = %v, want ErrInvalidRequest", wireID, err)
		}
		if len(store.getIDs) != 0 {
			t.Fatalf("Detail(%q) queried repository with %#v", wireID, store.getIDs)
		}
	}
}

func TestActionEndpointsPassOnlyOpaqueStorageIDs(t *testing.T) {
	store := &fakeStore{
		plugins: map[string]*model.Plugin{
			"opaque-1": {ID: "opaque-1", Name: "Original", Type: model.PluginTypeExpert, OwnerUID: testCaller.UID, SpaceID: stringPtr(testCaller.SpaceID)},
		},
		audits:   []model.PluginAuditLog{{PluginID: "opaque-1"}},
		versions: []model.PluginVersion{{PluginID: "opaque-1"}},
	}
	svc := fixedService(store)
	ctx := context.Background()

	if err := svc.Delete(ctx, testCaller, "expert:opaque-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ListAuditLogs(ctx, testCaller, "expert:opaque-1", 20, 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ListVersions(ctx, testCaller, "expert:opaque-1", 20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, testCaller, "expert:opaque-1", PublishRequest{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Duplicate(ctx, testCaller, "expert:opaque-1", "Copy"); err != nil {
		t.Fatal(err)
	}
	if store.deleteID != "opaque-1" || store.auditID != "opaque-1" || store.versionID != "opaque-1" || store.publishParams.PluginID != "opaque-1" || store.duplicateID != "opaque-1" {
		t.Fatalf("storage IDs: delete=%q audit=%q version=%q publish=%q duplicate=%q", store.deleteID, store.auditID, store.versionID, store.publishParams.PluginID, store.duplicateID)
	}
	if store.publishParams.ExpectedPluginType != model.PluginTypeExpert {
		t.Fatalf("publish expected type = %q", store.publishParams.ExpectedPluginType)
	}
}

func TestRelationWireTargetBecomesOpaqueAndKeepsExpectedType(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"target-1": {ID: "target-1", Type: model.PluginTypeSkill},
	}}
	req := validRequest()
	req.Relations = []RelationRequest{{TargetPluginID: "skill:target-1", Type: "expert_skill"}}
	if _, err := fixedService(store).Create(context.Background(), testCaller, req); err != nil {
		t.Fatal(err)
	}
	if len(store.createRels) != 1 {
		t.Fatalf("relations = %#v", store.createRels)
	}
	relation := store.createRels[0]
	if relation.TargetPluginID != "target-1" || relation.ExpectedTargetType != model.PluginTypeSkill || relation.TargetPluginType != model.PluginTypeSkill {
		t.Fatalf("persisted relation = %#v", relation)
	}

	bad := validRequest()
	bad.Relations = []RelationRequest{{TargetPluginID: "connector:target-1", Type: "expert_skill"}}
	if _, err := fixedService(store).Create(context.Background(), testCaller, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("mismatched target prefix error = %v", err)
	}
}
