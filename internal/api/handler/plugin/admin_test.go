package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	marketmiddleware "github.com/Mininglamp-OSS/octo-marketplace/internal/middleware"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
	"github.com/gin-gonic/gin"
)

type fakeAdminService struct {
	caller     pluginsvc.Caller
	listType   model.PluginType
	listVis    model.PluginVisibility
	listParams pluginsvc.ListParams
	list       []model.Plugin
	detail     *pluginsvc.Detail
	write      pluginsvc.WriteRequest
	deletedID  string
	err        error
}

func (f *fakeAdminService) AdminList(_ context.Context, c pluginsvc.Caller, typ model.PluginType, vis model.PluginVisibility, p pluginsvc.ListParams) ([]model.Plugin, int64, error) {
	f.caller, f.listType, f.listVis, f.listParams = c, typ, vis, p
	return f.list, int64(len(f.list)), f.err
}
func (f *fakeAdminService) AdminDetail(_ context.Context, c pluginsvc.Caller, _ string, _ bool) (*pluginsvc.Detail, error) {
	f.caller = c
	return f.detail, f.err
}
func (f *fakeAdminService) AdminCreate(_ context.Context, c pluginsvc.Caller, r pluginsvc.WriteRequest) (*pluginsvc.Detail, error) {
	f.caller, f.write = c, r
	return f.detail, f.err
}
func (f *fakeAdminService) AdminUpdate(_ context.Context, c pluginsvc.Caller, _ string, r pluginsvc.WriteRequest) (*pluginsvc.Detail, error) {
	f.caller, f.write = c, r
	return f.detail, f.err
}
func (f *fakeAdminService) AdminDelete(_ context.Context, c pluginsvc.Caller, id string) error {
	f.caller, f.deletedID = c, id
	return f.err
}

type fakeAdminCategories struct {
	listType model.PluginType
	list     []model.PluginCategory
	err      error
}

func (f *fakeAdminCategories) AdminListCategories(_ context.Context, typ model.PluginType) ([]model.PluginCategory, error) {
	f.listType = typ
	return f.list, f.err
}

// adminTestEngine mounts the admin surface behind the dev admin authenticator,
// which stamps a fixed admin identity — the same trusted path production uses to
// admit marketAdmin/superAdmin, so no caller-supplied identity is trusted here.
func adminTestEngine(svc AdminService, cats AdminCategoryService) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(logging.RequestID())
	adminAuth := marketmiddleware.NewAdminAuthenticator(false, nil, model.Identity{UID: "admin-1", Name: "Root"})
	NewAdmin(svc, cats).RegisterAdmin(r, adminAuth)
	return r
}

func TestAdminListPassesVisibilityAndSystemAdminCaller(t *testing.T) {
	space := "space-x"
	f := &fakeAdminService{list: []model.Plugin{{ID: "plugin-1", Name: "Sys", Type: model.PluginTypeConnector, SpaceID: &space, Visibility: model.PluginVisibilitySystem, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}}}
	rec := httptest.NewRecorder()
	adminTestEngine(f, &fakeAdminCategories{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/plugins?plugin_type=connector&visibility=system&q=foo", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !f.caller.IsSystemAdmin || f.caller.UID != "admin-1" {
		t.Fatalf("caller=%#v want system admin admin-1", f.caller)
	}
	if f.caller.SpaceID != "" {
		t.Fatalf("admin caller must carry no Space, got %q", f.caller.SpaceID)
	}
	if f.listType != model.PluginTypeConnector || f.listVis != model.PluginVisibilitySystem || f.listParams.Keyword != "foo" {
		t.Fatalf("type=%q vis=%q kw=%q", f.listType, f.listVis, f.listParams.Keyword)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"plugin_id":"plugin-1"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"data"`)) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAdminListRequiresPluginType(t *testing.T) {
	rec := httptest.NewRecorder()
	adminTestEngine(&fakeAdminService{}, &fakeAdminCategories{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/plugins", nil))
	if rec.Code != http.StatusBadRequest || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"VALIDATION_ERROR"`)) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAdminGetNotFoundIsSanitized(t *testing.T) {
	f := &fakeAdminService{err: pluginsvc.ErrNotFound}
	rec := httptest.NewRecorder()
	adminTestEngine(f, &fakeAdminCategories{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/plugins/other", nil))
	if rec.Code != http.StatusNotFound || !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"NOT_FOUND"`)) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte("space")) {
		t.Fatalf("existence leaked: %s", rec.Body.String())
	}
}

func TestAdminCreateStampsAdminCallerAndSanitizesInternalError(t *testing.T) {
	f := &fakeAdminService{err: errors.New("sql: secret DSN")}
	body := []byte(`{"plugin":{"plugin_name":"Sys","plugin_type":"connector","visibility":"system","tags":[],"manifest_json":{},"plugin_json":{}},"relations":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/plugins", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	adminTestEngine(f, &fakeAdminCategories{}).ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError || bytes.Contains(rec.Body.Bytes(), []byte("secret DSN")) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !f.caller.IsSystemAdmin {
		t.Fatalf("create did not stamp a system-admin caller: %#v", f.caller)
	}
}

func TestAdminDeleteReturnsPluginID(t *testing.T) {
	f := &fakeAdminService{}
	rec := httptest.NewRecorder()
	adminTestEngine(f, &fakeAdminCategories{}).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/plugins/plugin-9", nil))
	if rec.Code != http.StatusOK || f.deletedID != "plugin-9" {
		t.Fatalf("status=%d deletedID=%q body=%s", rec.Code, f.deletedID, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"plugin_id":"plugin-9"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"deleted":true`)) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestAdminDeleteCategoryInUseIsConflictWithCount(t *testing.T) {
	// The unified admin category-write endpoints were withdrawn until the
	// placement model supports runtime creation; only the read remains.
	rec := httptest.NewRecorder()
	adminTestEngine(&fakeAdminService{}, &fakeAdminCategories{}).ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/admin/plugin_categories/cat-1", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("category delete should be unmounted, status=%d", rec.Code)
	}
}

// TestAdminRoutesAreRoleGated proves the admin surface is behind the admin
// authenticator: with auth enabled and no token, every route is refused (Q10').
func TestAdminRoutesAreRoleGated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(logging.RequestID())
	// Enabled authenticator with a resolver that admits nothing → no token = 401.
	adminAuth := marketmiddleware.NewAdminAuthenticator(true, denyResolver{}, model.Identity{})
	NewAdmin(&fakeAdminService{}, &fakeAdminCategories{}).RegisterAdmin(r, adminAuth)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/admin/plugins?plugin_type=skill"},
		{http.MethodPost, "/api/v1/admin/plugins"},
		{http.MethodGet, "/api/v1/admin/plugins/p1"},
		{http.MethodPatch, "/api/v1/admin/plugins/p1"},
		{http.MethodDelete, "/api/v1/admin/plugins/p1"},
		{http.MethodGet, "/api/v1/admin/plugin_categories?plugin_type=skill"},
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s status=%d, want 401 (route not gated)", tc.method, tc.path, rec.Code)
		}
	}
}

type denyResolver struct{}

func (denyResolver) Resolve(context.Context, string) (model.Identity, error) {
	return model.Identity{}, nil
}

func TestAdminListCategoriesRendersSnakeCaseDTO(t *testing.T) {
	cats := &fakeAdminCategories{list: []model.PluginCategory{{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: json.RawMessage(`["connector"]`), SortOrder: 2, PluginCount: 3}}}
	rec := httptest.NewRecorder()
	adminTestEngine(&fakeAdminService{}, cats).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/plugin_categories?plugin_type=connector", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if cats.listType != model.PluginTypeConnector {
		t.Fatalf("plugin_type not forwarded: %q", cats.listType)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"category_id":"cat-1"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"plugin_count":3`)) {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"createdAt"`)) || bytes.Contains(rec.Body.Bytes(), []byte(`"iconKey"`)) {
		t.Fatalf("raw model camelCase leaked: %s", rec.Body.String())
	}
}
