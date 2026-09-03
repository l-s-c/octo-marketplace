package plugin

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestReviewPolicyEndpointsUseAuthenticatedSpaceAndStandardEnvelope(t *testing.T) {
	f := &fakeService{reviewPolicy: model.PluginReviewPolicy{IsAutoApproveEnabled: true}}
	identity := model.Identity{UID: "owner-1", Name: "Owner", SpaceRoles: map[string]int{"space-a": model.SpaceRoleOwner}}
	engine := engineWithIdentity(identity, f)

	get := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugin_review_policies", nil)
	req.Header.Set("X-Space-Id", "space-a")
	engine.ServeHTTP(get, req)
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"is_auto_approve_enabled":true`) {
		t.Fatalf("GET status=%d body=%s", get.Code, get.Body.String())
	}

	patch := httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPatch, "/api/v1/plugin_review_policies", bytes.NewBufferString(`{"is_auto_approve_enabled":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Space-Id", "space-a")
	engine.ServeHTTP(patch, req)
	if patch.Code != http.StatusOK || f.reviewPolicySet == nil || *f.reviewPolicySet {
		t.Fatalf("PATCH status=%d body=%s value=%v", patch.Code, patch.Body.String(), f.reviewPolicySet)
	}
	if f.caller.SpaceID != "space-a" || f.caller.SpaceRole != 2 {
		t.Fatalf("caller=%#v", f.caller)
	}
}

func TestUpdateReviewPolicyRequiresBoolean(t *testing.T) {
	f := &fakeService{}
	identity := model.Identity{UID: "owner-1", SpaceRoles: map[string]int{"space-a": model.SpaceRoleOwner}}
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/plugin_review_policies", bytes.NewBufferString(`{}`))
	req.Header.Set("Content-Type", "application/json")
	engineWithIdentity(identity, f).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), `"code":"VALIDATION_ERROR"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
