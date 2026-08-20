package pluginid

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAllKinds(t *testing.T) {
	tests := []struct {
		wire string
		want ID
	}{
		{"expert:01JEXPERT123", ID{Kind: Expert, ResourceID: "01JEXPERT123"}},
		{"expert_team:01JSQUAD456", ID{Kind: ExpertTeam, ResourceID: "01JSQUAD456"}},
		{"skill:550e8400-e29b-41d4-a716-446655440000", ID{Kind: Skill, ResourceID: "550e8400-e29b-41d4-a716-446655440000"}},
		{"connector:01JMCP789", ID{Kind: Connector, ResourceID: "01JMCP789"}},
		{"expert_member:01JSQUAD456:member_01", ID{Kind: ExpertMember, ResourceID: "01JSQUAD456", MemberKey: "member_01"}},
		{"expert_skill:01JEXPERT123:01JSKILL001", ID{Kind: ExpertSkill, ResourceID: "01JEXPERT123", SkillKey: "01JSKILL001"}},
		{"expert_member_skill:01JSQUAD456:member_01:01JSKILL002", ID{Kind: ExpertMemberSkill, ResourceID: "01JSQUAD456", MemberKey: "member_01", SkillKey: "01JSKILL002"}},
	}
	for _, tt := range tests {
		t.Run(tt.wire, func(t *testing.T) {
			got, err := Parse(tt.wire)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("Parse() = %#v, want %#v", got, tt.want)
			}
			if got.String() != tt.wire {
				t.Fatalf("String() = %q, want %q", got.String(), tt.wire)
			}
		})
	}
}

func TestConstructorsRoundTrip(t *testing.T) {
	constructors := []func() (ID, error){
		func() (ID, error) { return NewTopLevel(Expert, "expert-1") },
		func() (ID, error) { return NewTopLevel(ExpertTeam, "team-1") },
		func() (ID, error) { return NewTopLevel(Skill, "skill-1") },
		func() (ID, error) { return NewTopLevel(Connector, "connector-1") },
		func() (ID, error) { return NewExpertMember("team-1", "member_01") },
		func() (ID, error) { return NewExpertSkill("expert-1", "01JSKILL001") },
		func() (ID, error) { return NewExpertMemberSkill("team-1", "member_01", "01JSKILL002") },
	}
	for _, constructor := range constructors {
		want, err := constructor()
		if err != nil {
			t.Fatal(err)
		}
		got, err := Parse(want.String())
		if err != nil {
			t.Fatalf("Parse(%q): %v", want.String(), err)
		}
		if got != want {
			t.Fatalf("round trip = %#v, want %#v", got, want)
		}
	}
}

func TestParseRejectsMalformedIDs(t *testing.T) {
	long := strings.Repeat("a", MaxSegmentLength+1)
	tests := []string{
		"",
		"expert",
		"expert:",
		":id",
		"unknown:id",
		"mcp:id",
		"team:id",
		"expert:id:extra",
		"expert_member:team",
		"expert_member:team:member:extra",
		"expert_skill:expert::skill",
		"expert_member_skill:team:member",
		"expert_member_skill:team:member:skill:extra",
		" expert:id",
		"expert:id ",
		"expert:id/child",
		"expert:id@host",
		"expert:" + long,
		"expert_member:team:",
		"expert_member:team:member key",
		"expert_member:team:..",
		"expert_member:team:a..b",
		"expert_member:team:" + long,
		"expert_skill:expert:技能",
		"expert_skill:expert:" + long,
		"expert_member_skill:team:member:../skill",
	}
	for _, value := range tests {
		t.Run(value, func(t *testing.T) {
			if got, err := Parse(value); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Parse() = %#v, %v; want ErrInvalid", got, err)
			}
		})
	}
}

func TestStorageBoundary(t *testing.T) {
	top, err := Parse("skill:550e8400-e29b-41d4-a716-446655440000")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := top.StorageID(); !ok || got != "550e8400-e29b-41d4-a716-446655440000" {
		t.Fatalf("StorageID() = %q, %v", got, ok)
	}

	embedded, err := Parse("expert_member:team-1:member_01")
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := embedded.StorageID(); ok || got != "" {
		t.Fatalf("embedded StorageID() = %q, %v; want no direct row mapping", got, ok)
	}
	if embedded.ResourceID != "team-1" || embedded.MemberKey != "member_01" {
		t.Fatalf("embedded address = %#v", embedded)
	}
}

func TestConstructorRejectsWrongKindAndInvalidState(t *testing.T) {
	if _, err := NewTopLevel(ExpertMember, "team-1"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("NewTopLevel embedded kind error = %v", err)
	}
	invalid := ID{Kind: Expert, ResourceID: "id", MemberKey: "member"}
	if invalid.String() != "" {
		t.Fatalf("invalid String() = %q", invalid.String())
	}
	if _, ok := invalid.StorageID(); ok {
		t.Fatal("invalid ID unexpectedly mapped to storage")
	}
}

func TestMaximumLengthEmbeddedID(t *testing.T) {
	segment := strings.Repeat("a", MaxSegmentLength)
	id, err := NewExpertMemberSkill(segment, segment, segment)
	if err != nil {
		t.Fatal(err)
	}
	if len(id.String()) != MaxEncodedLength {
		t.Fatalf("encoded length = %d, want %d", len(id.String()), MaxEncodedLength)
	}
	if _, err := Parse(id.String()); err != nil {
		t.Fatalf("Parse(maximum ID): %v", err)
	}
}
