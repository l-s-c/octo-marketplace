package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestDetailPassesStorageIDToRepository(t *testing.T) {
	store := &fakeStore{
		plugins: map[string]*model.Plugin{
			"opaque-1": {ID: "opaque-1", Type: model.PluginTypeSkill},
		},
		relations: map[string][]model.PluginRelation{
			"opaque-1": {{SourcePluginID: "opaque-1", TargetPluginID: "connector-1", TargetPluginType: model.PluginTypeConnector}},
		},
	}
	detail, err := fixedService(store).Detail(context.Background(), testCaller, "opaque-1", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.getIDs) != 1 || store.getIDs[0] != "opaque-1" {
		t.Fatalf("repository IDs = %#v, want opaque storage ID", store.getIDs)
	}
	if len(detail.Relations) != 1 || detail.Relations[0].SourcePluginType != model.PluginTypeSkill {
		t.Fatalf("detail relations = %#v", detail.Relations)
	}
}

func TestAllRowIDMethodsRejectMalformedIDsBeforeRepository(t *testing.T) {
	store := &fakeStore{}
	svc := fixedService(store)
	ctx := context.Background()
	// Colon-prefixed wire IDs are a retired format and must not reach storage.
	const badID = "expert:opaque-1"
	checks := []struct {
		name string
		run  func() error
	}{
		{"detail", func() error { _, err := svc.Detail(ctx, testCaller, badID, true); return err }},
		{"update", func() error { _, err := svc.Update(ctx, testCaller, badID, validRequest()); return err }},
		{"delete", func() error { return svc.Delete(ctx, testCaller, badID) }},
		{"versions", func() error { _, _, err := svc.ListVersions(ctx, testCaller, badID, 20, 0); return err }},
		{"publish", func() error {
			_, err := svc.Publish(ctx, testCaller, badID, PublishRequest{Version: "1.0.0"})
			return err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error = %v, want ErrInvalidRequest", err)
			}
		})
	}
	if len(store.getIDs) != 0 || store.deleteID != "" || store.versionID != "" || store.publishParams.PluginID != "" {
		t.Fatalf("malformed ID reached repository: %#v", store)
	}
}

func TestDetailRejectsMalformedIDsBeforeRepository(t *testing.T) {
	for _, wireID := range []string{"", " opaque-1", "skill:opaque-1", "expert_member:team-1:member-1", ".leading-dot"} {
		store := &fakeStore{}
		if _, err := fixedService(store).Detail(context.Background(), testCaller, wireID, true); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("Detail(%q) error = %v, want ErrInvalidRequest", wireID, err)
		}
		if len(store.getIDs) != 0 {
			t.Fatalf("Detail(%q) queried repository with %#v", wireID, store.getIDs)
		}
	}
}

func TestActionEndpointsPassOpaqueStorageIDs(t *testing.T) {
	store := &fakeStore{
		plugins: map[string]*model.Plugin{
			"opaque-1": {ID: "opaque-1", Name: "Original", Type: model.PluginTypeExpert, OwnerUID: testCaller.UID, SpaceID: stringPtr(testCaller.SpaceID)},
		},
		versions: []model.PluginVersion{{PluginID: "opaque-1"}},
	}
	svc := fixedService(store)
	ctx := context.Background()

	if err := svc.Delete(ctx, testCaller, "opaque-1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ListVersions(ctx, testCaller, "opaque-1", 20, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, testCaller, "opaque-1", PublishRequest{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	if store.deleteID != "opaque-1" || store.versionID != "opaque-1" || store.publishParams.PluginID != "opaque-1" {
		t.Fatalf("storage IDs: delete=%q version=%q publish=%q", store.deleteID, store.versionID, store.publishParams.PluginID)
	}
}

func TestRelationTargetTypeValidatedAgainstRow(t *testing.T) {
	store := &fakeStore{plugins: map[string]*model.Plugin{
		"target-1": {ID: "target-1", Type: model.PluginTypeSkill},
	}}
	req := validRequest()
	req.Relations = []RelationRequest{{TargetPluginID: "target-1", Type: "expert_skill"}}
	if _, err := fixedService(store).Create(context.Background(), testCaller, req); err != nil {
		t.Fatal(err)
	}
	if len(store.createRels) != 1 {
		t.Fatalf("relations = %#v", store.createRels)
	}
	relation := store.createRels[0]
	if relation.TargetPluginID != "target-1" || relation.TargetPluginType != model.PluginTypeSkill {
		t.Fatalf("persisted relation = %#v", relation)
	}

	// expert_skill must point at a skill row; a connector target is rejected
	// from the target row's actual type, no wire prefix involved.
	connectorStore := &fakeStore{plugins: map[string]*model.Plugin{
		"target-2": {ID: "target-2", Type: model.PluginTypeConnector},
	}}
	bad := validRequest()
	bad.Relations = []RelationRequest{{TargetPluginID: "target-2", Type: "expert_skill"}}
	if _, err := fixedService(connectorStore).Create(context.Background(), testCaller, bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("mismatched target type error = %v", err)
	}
}
