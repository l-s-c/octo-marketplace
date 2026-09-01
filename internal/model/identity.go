package model

// Space member roles, using octo-server's `space_member.role` column encoding.
// Coupled by convention — not by import — to octo-server, exactly like
// middleware.RoleSuperAdmin. Grep both repos before changing a value.
//
// WARNING: octo-web's `Space.role` uses a different, INVERTED encoding
// (1=owner, 2=admin, 3=member). The two are never interchangeable: a web
// "member" (3) read with this encoding compares as >= SpaceRoleAdmin and would
// be handed review authority, and a web "owner" (1) would be demoted to admin.
// Never compare, copy, or default one encoding across to the other.
const (
	SpaceRoleMember = 0
	SpaceRoleAdmin  = 1
	SpaceRoleOwner  = 2
)

type Identity struct {
	UID              string              `json:"uid"`
	Name             string              `json:"name"`
	Role             string              `json:"role,omitempty"`
	Spaces           []string            `json:"spaces,omitempty"`
	OwnedBotsBySpace map[string][]string `json:"owned_bots_by_space,omitempty"`

	// SpaceRoles maps space_id to the caller's role in that Space, as returned
	// by octo-server's POST /v1/auth/verify?include=context alongside `spaces`
	// and `owned_bots_by_space`. Values use the SpaceRole* encoding above
	// (0=member, 1=admin, 2=owner); a plugin reviewer is >= SpaceRoleAdmin.
	//
	// A MISSING entry is indistinguishable from an explicit SpaceRoleMember:
	// both read as 0 and both mean "not a reviewer". Do not use the two-value
	// map read to infer anything about membership — `Spaces` is the membership
	// list, and an older octo-server that does not send `space_roles` at all
	// leaves every Space looking like plain membership, which fails closed.
	SpaceRoles map[string]int `json:"space_roles,omitempty"`

	ContextIncluded bool `json:"context_included,omitempty"`
}

// SpaceRole returns the caller's role in spaceID, or SpaceRoleMember when the
// Space carries no entry. See the SpaceRoles field: absent and 0 are the same
// answer on purpose.
func (i Identity) SpaceRole(spaceID string) int {
	if spaceID == "" {
		return SpaceRoleMember
	}
	return i.SpaceRoles[spaceID]
}

// CanReviewSpace reports whether this identity may act on Space-level plugin
// review decisions in spaceID — that is, whether it is an admin or owner of
// that specific Space. It is deliberately Space-scoped: holding owner in one
// Space says nothing about any other, so callers must pass the Space the
// request is actually operating on and never a cached or default one.
func (i Identity) CanReviewSpace(spaceID string) bool {
	return i.SpaceRole(spaceID) >= SpaceRoleAdmin
}

type BotIdentity struct {
	BotUID    string `json:"bot_uid"`
	BotName   string `json:"bot_name"`
	OwnerUID  string `json:"owner_uid"`
	OwnerName string `json:"owner_name"`
	SpaceID   string `json:"space_id"`
}
