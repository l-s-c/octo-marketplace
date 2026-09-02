package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

func TestDetailGraph_ValidatesCaller(t *testing.T) {
	svc := fixedService(&fakeStore{})
	if _, err := svc.DetailGraph(context.Background(), Caller{}, "id"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty caller = %v", err)
	}
}

func TestDetailGraph_MapsNotFoundAndTooLarge(t *testing.T) {
	svc := fixedService(&fakeStore{err: pluginrepo.ErrNotFound})
	if _, err := svc.DetailGraph(context.Background(), testCaller, "plug-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("not found = %v", err)
	}
	svc = fixedService(&fakeStore{err: pluginrepo.ErrGraphTooLarge})
	if _, err := svc.DetailGraph(context.Background(), testCaller, "plug-1"); !errors.Is(err, ErrGraphTooLarge) {
		t.Fatalf("too large = %v", err)
	}
}

func TestDetailGraph_FillsMemberCountsFromEdgeSlice(t *testing.T) {
	team := &model.Plugin{ID: "team-1", Type: model.PluginTypeExpertTeam}
	m1 := &model.Plugin{ID: "m1", Type: model.PluginTypeExpert}
	m2 := &model.Plugin{ID: "m2", Type: model.PluginTypeExpert}
	s1 := &model.Plugin{ID: "s1", Type: model.PluginTypeSkill}
	rels := []model.PluginRelation{
		{ID: "r1", SourcePluginID: "team-1", TargetPluginID: "m1", Type: "expert_team_expert", SourcePluginType: model.PluginTypeExpertTeam, TargetPluginType: model.PluginTypeExpert},
		{ID: "r2", SourcePluginID: "team-1", TargetPluginID: "m2", Type: "expert_team_expert", SourcePluginType: model.PluginTypeExpertTeam, TargetPluginType: model.PluginTypeExpert},
		{ID: "r3", SourcePluginID: "m1", TargetPluginID: "s1", Type: "expert_skill", SourcePluginType: model.PluginTypeExpert, TargetPluginType: model.PluginTypeSkill},
	}
	svc := fixedService(&fakeStore{detailGraphRoot: team, detailGraphRels: rels, detailGraphRelated: []*model.Plugin{m1, m2, s1}})
	out, err := svc.DetailGraph(context.Background(), testCaller, "team-1")
	if err != nil {
		t.Fatal(err)
	}
	if out.Plugin.MemberCount != 2 {
		t.Fatalf("root.MemberCount = %d want 2", out.Plugin.MemberCount)
	}
}

func TestDetailGraph_RootNotFound(t *testing.T) {
	svc := fixedService(&fakeStore{})
	if _, err := svc.DetailGraph(context.Background(), testCaller, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
