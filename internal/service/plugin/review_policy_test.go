package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestUpdateReviewPolicyRequiresSpaceReviewerAndIsSharedBySpace(t *testing.T) {
	store, svc := listingFixture(t, model.PluginVisibilityPrivate, model.PluginListingStateDraft)
	caller := testCaller
	caller.SpaceRole = SpaceRoleMember
	if _, err := svc.UpdateReviewPolicy(context.Background(), caller, false); !errors.Is(err, ErrReviewPolicyForbidden) {
		t.Fatalf("err=%v, want ErrReviewPolicyForbidden", err)
	}
	if store.reviewPolicySet != nil {
		t.Fatal("member changed reviewer-only policy")
	}

	caller.SpaceRole = SpaceRoleAdmin
	policy, err := svc.UpdateReviewPolicy(context.Background(), caller, false)
	if err != nil {
		t.Fatal(err)
	}
	if policy.IsAutoApproveEnabled || store.reviewPolicySet == nil || *store.reviewPolicySet {
		t.Fatal("admin update was not persisted")
	}

	owner := caller
	owner.UID = "owner-1"
	owner.SpaceRole = SpaceRoleOwner
	shared, err := svc.GetReviewPolicy(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if shared.IsAutoApproveEnabled {
		t.Fatal("owner did not observe the Space policy updated by admin")
	}
}

func TestSubmitReviewAutoApprovesWithoutReviewerRole(t *testing.T) {
	store, svc := listingFixture(t, model.PluginVisibilitySpace, model.PluginListingStateDraft)
	store.reviewPolicy = model.PluginReviewPolicy{IsAutoApproveEnabled: true}
	caller := testCaller
	caller.SpaceRole = SpaceRoleMember

	review, err := svc.SubmitReview(context.Background(), caller, ReviewSubmitParams{
		PluginID: "plugin-1", Version: "1.0.0",
	})
	if err != nil {
		t.Fatal(err)
	}
	if review.Status != model.ReviewStatusApproved {
		t.Fatalf("status=%q, want approved", review.Status)
	}
	if got := store.review.approveParams.DecisionSource; got != model.ReviewDecisionSourcePolicy {
		t.Fatalf("decision_source=%q, want policy", got)
	}
}
