// Package pluginid owns the cowork-v3 wire encoding for unified Plugin IDs.
//
// The unified database intentionally continues to store opaque, unprefixed IDs.
// At an API/service boundary, load a top-level row by the ResourceID returned by
// Parse and encode a row for the wire with NewTopLevel. Composite embedded IDs
// are addresses into a parent's persisted JSON and must never be written into a
// plugins.plugin_id column. This keeps the current VARCHAR(64) storage contract
// separate from the longer, typed wire contract.
package pluginid

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// MaxSegmentLength matches the widest existing resource ID and stable-key
	// bounds. It is measured in bytes because accepted segments are ASCII only.
	MaxSegmentLength = 64

	// MaxEncodedLength is the longest valid expert_member_skill wire ID.
	MaxEncodedLength = len("expert_member_skill:") + 3*MaxSegmentLength + 2
)

// Kind is one exact cowork-v3 Plugin ID prefix. No compatibility aliases exist.
type Kind string

const (
	Expert            Kind = "expert"
	ExpertTeam        Kind = "expert_team"
	Skill             Kind = "skill"
	Connector         Kind = "connector"
	ExpertMember      Kind = "expert_member"
	ExpertSkill       Kind = "expert_skill"
	ExpertMemberSkill Kind = "expert_member_skill"
)

var ErrInvalid = errors.New("invalid plugin ID")

// ID is a parsed Plugin ID. ResourceID is the opaque top-level database ID (or
// the parent Expert/Team database ID for an embedded address). MemberKey and
// SkillKey are populated only when required by Kind.
type ID struct {
	Kind       Kind
	ResourceID string
	MemberKey  string
	SkillKey   string
}

// Parse strictly parses a prefixed cowork-v3 Plugin ID.
func Parse(value string) (ID, error) {
	if value == "" || len(value) > MaxEncodedLength || strings.TrimSpace(value) != value {
		return ID{}, invalid("invalid total length or surrounding whitespace")
	}
	parts := strings.Split(value, ":")
	if len(parts) < 2 {
		return ID{}, invalid("missing segments")
	}

	id := ID{Kind: Kind(parts[0])}
	switch id.Kind {
	case Expert, ExpertTeam, Skill, Connector:
		if len(parts) != 2 {
			return ID{}, invalid("wrong segment count")
		}
		id.ResourceID = parts[1]
	case ExpertMember:
		if len(parts) != 3 {
			return ID{}, invalid("wrong segment count")
		}
		id.ResourceID, id.MemberKey = parts[1], parts[2]
	case ExpertSkill:
		if len(parts) != 3 {
			return ID{}, invalid("wrong segment count")
		}
		id.ResourceID, id.SkillKey = parts[1], parts[2]
	case ExpertMemberSkill:
		if len(parts) != 4 {
			return ID{}, invalid("wrong segment count")
		}
		id.ResourceID, id.MemberKey, id.SkillKey = parts[1], parts[2], parts[3]
	default:
		return ID{}, invalid("unsupported prefix")
	}
	if err := id.validate(); err != nil {
		return ID{}, err
	}
	return id, nil
}

// NewTopLevel encodes an opaque database ID as a top-level wire ID.
func NewTopLevel(kind Kind, storageID string) (ID, error) {
	id := ID{Kind: kind, ResourceID: storageID}
	if !id.IsTopLevel() {
		return ID{}, invalid("kind is not top-level")
	}
	if err := id.validate(); err != nil {
		return ID{}, err
	}
	return id, nil
}

func NewExpertMember(teamID, memberKey string) (ID, error) {
	return newEmbedded(ID{Kind: ExpertMember, ResourceID: teamID, MemberKey: memberKey})
}

func NewExpertSkill(expertID, skillKey string) (ID, error) {
	return newEmbedded(ID{Kind: ExpertSkill, ResourceID: expertID, SkillKey: skillKey})
}

func NewExpertMemberSkill(teamID, memberKey, skillKey string) (ID, error) {
	return newEmbedded(ID{Kind: ExpertMemberSkill, ResourceID: teamID, MemberKey: memberKey, SkillKey: skillKey})
}

func newEmbedded(id ID) (ID, error) {
	if err := id.validate(); err != nil {
		return ID{}, err
	}
	return id, nil
}

// IsTopLevel reports whether the ID maps directly to one plugins row.
func (id ID) IsTopLevel() bool {
	switch id.Kind {
	case Expert, ExpertTeam, Skill, Connector:
		return true
	default:
		return false
	}
}

// StorageID returns the unprefixed opaque database ID only for top-level IDs.
// Embedded addresses must instead resolve ResourceID plus their stable key(s)
// against the parent resource JSON.
func (id ID) StorageID() (string, bool) {
	if !id.IsTopLevel() || id.validate() != nil {
		return "", false
	}
	return id.ResourceID, true
}

// String returns the canonical prefixed form, or an empty string for an invalid
// programmatically constructed value.
func (id ID) String() string {
	if id.validate() != nil {
		return ""
	}
	switch id.Kind {
	case Expert, ExpertTeam, Skill, Connector:
		return string(id.Kind) + ":" + id.ResourceID
	case ExpertMember:
		return string(id.Kind) + ":" + id.ResourceID + ":" + id.MemberKey
	case ExpertSkill:
		return string(id.Kind) + ":" + id.ResourceID + ":" + id.SkillKey
	case ExpertMemberSkill:
		return string(id.Kind) + ":" + id.ResourceID + ":" + id.MemberKey + ":" + id.SkillKey
	default:
		return ""
	}
}

func (id ID) validate() error {
	if !validSegment(id.ResourceID) {
		return invalid("invalid resource ID segment")
	}
	switch id.Kind {
	case Expert, ExpertTeam, Skill, Connector:
		if id.MemberKey != "" || id.SkillKey != "" {
			return invalid("unexpected embedded key")
		}
	case ExpertMember:
		if !validStableKey(id.MemberKey) || id.SkillKey != "" {
			return invalid("invalid member key")
		}
	case ExpertSkill:
		if id.MemberKey != "" || !validStableKey(id.SkillKey) {
			return invalid("invalid skill key")
		}
	case ExpertMemberSkill:
		if !validStableKey(id.MemberKey) || !validStableKey(id.SkillKey) {
			return invalid("invalid embedded key")
		}
	default:
		return invalid("unsupported prefix")
	}
	return nil
}

func validStableKey(value string) bool {
	return validSegment(value) && !strings.Contains(value, "..")
}

func validSegment(value string) bool {
	if value == "" || len(value) > MaxSegmentLength {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalid, reason)
}
