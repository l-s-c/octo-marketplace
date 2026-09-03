package plugin

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestUpdateReviewPolicyRequiresSpaceOwner(t *testing.T) {
	store, svc := listingFixture(t, model.PluginVisibilityPrivate, model.PluginListingStateDraft)
	caller := testCaller
	caller.SpaceRole = SpaceRoleAdmin
	if _, err := svc.UpdateReviewPolicy(context.Background(), caller, false); !errors.Is(err, ErrReviewPolicyForbidden) {
		t.Fatalf("err=%v, want ErrReviewPolicyForbidden", err)
	}
	if store.reviewPolicySet != nil {
		t.Fatal("admin changed owner-only policy")
	}

	caller.SpaceRole = SpaceRoleOwner
	policy, err := svc.UpdateReviewPolicy(context.Background(), caller, false)
	if err != nil {
		t.Fatal(err)
	}
	if policy.IsAutoApproveEnabled || store.reviewPolicySet == nil || *store.reviewPolicySet {
		t.Fatal("owner update was not persisted")
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
