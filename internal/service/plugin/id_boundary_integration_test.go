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
