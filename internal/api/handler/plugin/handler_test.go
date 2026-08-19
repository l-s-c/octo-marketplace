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

type fakeService struct {
	caller pluginsvc.Caller
	write  pluginsvc.WriteRequest
	list   []model.Plugin
	detail *pluginsvc.Detail
	err    error
}

func (f *fakeService) List(_ context.Context, c pluginsvc.Caller, _ pluginsvc.ListParams) ([]model.Plugin, error) {
	f.caller = c
	return f.list, f.err
}
func (f *fakeService) Detail(_ context.Context, c pluginsvc.Caller, _ string) (*pluginsvc.Detail, error) {
	f.caller = c
	return f.detail, f.err
}
func (f *fakeService) Create(_ context.Context, c pluginsvc.Caller, r pluginsvc.WriteRequest) (*pluginsvc.Detail, error) {
	f.caller, f.write = c, r
	return f.detail, f.err
}
func (f *fakeService) Update(_ context.Context, c pluginsvc.Caller, _ string, r pluginsvc.WriteRequest) (*pluginsvc.Detail, error) {
	f.caller, f.write = c, r
	return f.detail, f.err
}
func (f *fakeService) Delete(_ context.Context, c pluginsvc.Caller, _ string) error {
	f.caller = c
	return f.err
}
func (f *fakeService) ListAuditLogs(context.Context, pluginsvc.Caller, string, int, int) ([]model.PluginAuditLog, error) {
	return nil, f.err
}
func (f *fakeService) ListVersions(context.Context, pluginsvc.Caller, string, int, int) ([]model.PluginVersion, error) {
	return nil, f.err
}
func (f *fakeService) Publish(context.Context, pluginsvc.Caller, string, pluginsvc.PublishRequest) (*model.PluginVersion, error) {
	return &model.PluginVersion{}, f.err
}
func (f *fakeService) Duplicate(context.Context, pluginsvc.Caller, string, string) (*model.Plugin, error) {
	return &model.Plugin{}, f.err
}
func (f *fakeService) ListCategories(context.Context, pluginsvc.Caller, string, model.PluginType) ([]model.PluginCategory, error) {
	return nil, f.err
}

func testEngine(s Service) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(logging.RequestID())
	r.Use(func(c *gin.Context) {
		ctx := c.Request.Context()
		ctx = context.WithValue(ctx, interfaceKey("marketplace.identity"), model.Identity{})
		_ = ctx
		c.Next()
	})
	// Use the real development authenticator so identity and Space are stamped by
	// the same trusted middleware path as production.
	auth := marketmiddleware.NewAuthenticator(false, nil, model.Identity{UID: "user-1", Name: "Alice"}, "space-a")
	v1 := r.Group("/api/v1", auth.Handler())
	New(s).Register(v1)
	return r
}

type interfaceKey string

func TestCreateUsesServerDerivedIdentityAndStandardEnvelope(t *testing.T) {
	space := "space-a"
	f := &fakeService{detail: &pluginsvc.Detail{Plugin: &model.Plugin{ID: "plugin-1", Name: "Demo", Type: model.PluginTypeSkill, OwnerUID: "user-1", SpaceID: &space, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}}}
	body := []byte(`{"name":"Demo","type":"skill","visibility":"private","tags":[],"manifest":{},"package":{}}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	testEngine(f).ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.caller.UID != "user-1" || f.caller.SpaceID != "space-a" || f.caller.Name != "Alice" {
		t.Fatalf("caller=%#v", f.caller)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"code":`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"data"`)) {
		t.Fatalf("nonstandard envelope: %s", rec.Body.String())
	}
}

func TestCreateRejectsClientIdentityFields(t *testing.T) {
	f := &fakeService{}
	rec := httptest.NewRecorder()
	body := []byte(`{"name":"Demo","type":"skill","visibility":"private","tags":[],"manifest":{},"package":{},"owner_id":"attacker"}`)
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/plugins", bytes.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"VALIDATION_ERROR"`)) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestCrossSpaceNotFoundIsSanitized(t *testing.T) {
	f := &fakeService{err: pluginsvc.ErrNotFound}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plugins/other-space", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"code":"NOT_FOUND"`)) || bytes.Contains(rec.Body.Bytes(), []byte("space")) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestInternalErrorIsSanitized(t *testing.T) {
	f := &fakeService{err: errors.New("sql: secret DSN")}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plugins/p1", nil))
	if rec.Code != http.StatusInternalServerError || bytes.Contains(rec.Body.Bytes(), []byte("secret DSN")) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestListUsesOffsetEnvelope(t *testing.T) {
	f := &fakeService{list: []model.Plugin{{ID: "p1", Name: "One"}}}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plugins?page=2&page_size=10", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Pagination struct {
			Page     int `json:"page"`
			PageSize int `json:"page_size"`
		} `json:"pagination"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Pagination.Page != 2 || body.Pagination.PageSize != 10 {
		t.Fatalf("body=%s", rec.Body.String())
	}
}
