package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	marketmiddleware "github.com/Mininglamp-OSS/octo-marketplace/internal/middleware"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
	"github.com/gin-gonic/gin"
)

type fakeService struct {
	caller           pluginsvc.Caller
	write            pluginsvc.WriteRequest
	list             []model.Plugin
	detail           *pluginsvc.Detail
	includeRelations bool
	err              error
	upload           *pluginsvc.AttachmentUpload
	download         *pluginsvc.AttachmentDownload
	archive          *pluginsvc.Archive
	archiveZip       []byte
	audits           []model.PluginAuditLog
	auditTotal       int64
	versions         []model.PluginVersion
	versionTotal     int64
}

func (f *fakeService) List(_ context.Context, c pluginsvc.Caller, _ pluginsvc.ListParams) ([]model.Plugin, int64, error) {
	f.caller = c
	return f.list, int64(len(f.list)), f.err
}
func (f *fakeService) Detail(_ context.Context, c pluginsvc.Caller, _ string, includeRelations bool) (*pluginsvc.Detail, error) {
	f.caller, f.includeRelations = c, includeRelations
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
func (f *fakeService) ListAuditLogs(context.Context, pluginsvc.Caller, string, int, int) ([]model.PluginAuditLog, int64, error) {
	return f.audits, f.auditTotal, f.err
}
func (f *fakeService) ListVersions(context.Context, pluginsvc.Caller, string, int, int) ([]model.PluginVersion, int64, error) {
	return f.versions, f.versionTotal, f.err
}
func (f *fakeService) Publish(context.Context, pluginsvc.Caller, string, pluginsvc.PublishRequest) (*model.PluginVersion, error) {
	return &model.PluginVersion{}, f.err
}
func (f *fakeService) Duplicate(context.Context, pluginsvc.Caller, string, string) (*model.Plugin, error) {
	return &model.Plugin{}, f.err
}
func (f *fakeService) InitAttachmentUpload(_ context.Context, c pluginsvc.Caller, _, _ string, _ int64) (*pluginsvc.AttachmentUpload, error) {
	f.caller = c
	return f.upload, f.err
}
func (f *fakeService) OpenAttachment(_ context.Context, c pluginsvc.Caller, _, _ string) (*pluginsvc.AttachmentDownload, error) {
	f.caller = c
	return f.download, f.err
}
func (f *fakeService) PrepareArchive(_ context.Context, c pluginsvc.Caller, _, _ string) (*pluginsvc.Archive, error) {
	f.caller = c
	return f.archive, f.err
}
func (f *fakeService) WriteArchive(_ context.Context, _ *pluginsvc.Archive, w io.Writer) error {
	if f.err == nil {
		_, _ = w.Write(f.archiveZip)
	}
	return f.err
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

func TestUpsertUsesServerDerivedIdentityAndStandardEnvelope(t *testing.T) {
	space := "space-a"
	f := &fakeService{detail: &pluginsvc.Detail{Plugin: &model.Plugin{ID: "plugin-1", Name: "Demo", Type: model.PluginTypeSkill, OwnerUID: "user-1", SpaceID: &space, Visibility: model.PluginVisibilityPrivate, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}}}
	body := []byte(`{"plugin":{"plugin_name":"Demo","plugin_type":"skill","visibility":"private","tags":[],"manifest_json":{},"plugin_json":{}},"relations":[]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/plugins/upsert", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	testEngine(f).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.caller.UID != "user-1" || f.caller.SpaceID != "space-a" || f.caller.Name != "Alice" {
		t.Fatalf("caller=%#v", f.caller)
	}
	if bytes.Contains(rec.Body.Bytes(), []byte(`"code":`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"data"`)) {
		t.Fatalf("nonstandard envelope: %s", rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"plugin_id":"skill:plugin-1"`)) {
		t.Fatalf("unprefixed create response: %s", rec.Body.String())
	}
}

func TestUpsertRejectsClientIdentityFields(t *testing.T) {
	f := &fakeService{}
	rec := httptest.NewRecorder()
	body := []byte(`{"plugin":{"plugin_name":"Demo","plugin_type":"skill","visibility":"private","tags":[],"manifest_json":{},"plugin_json":{},"owner_id":"attacker"},"relations":[]}`)
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/internal/plugins/upsert", bytes.NewReader(body)))
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
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/plugins/detail?plugin_id=other-space", nil))
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
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/plugins/detail?plugin_id=p1", nil))
	if rec.Code != http.StatusInternalServerError || bytes.Contains(rec.Body.Bytes(), []byte("secret DSN")) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAttachmentUploadReturnsStableKeyAndPresignedTarget(t *testing.T) {
	f := &fakeService{upload: &pluginsvc.AttachmentUpload{ObjectKey: "plugins/space-a/attachments/id.bin", UploadURL: "https://upload.invalid/signed", Headers: http.Header{"Content-Type": []string{"application/octet-stream"}}, ExpiresIn: 3600}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/internal/plugins/attachment/upload", strings.NewReader(`{"file_name":"x.bin","file_size":4,"content_type":"application/octet-stream"}`))
	req.Header.Set("Content-Type", "application/json")
	testEngine(f).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte(`"object_key":"plugins/space-a/attachments/id.bin"`)) || !bytes.Contains(rec.Body.Bytes(), []byte(`"method":"PUT"`)) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if f.caller.UID != "user-1" || f.caller.SpaceID != "space-a" {
		t.Fatalf("caller=%#v", f.caller)
	}
}

func TestAttachmentDownloadStreamsSafeHeaders(t *testing.T) {
	f := &fakeService{download: &pluginsvc.AttachmentDownload{Body: io.NopCloser(strings.NewReader("data")), Path: "dir/evil\r\nX-Test.txt", ContentType: "text/plain", Size: 4}}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/plugins/attachment/download?plugin_id=p1&object_key=plugins%2Fspace-a%2Fattachments%2Fid", nil))
	if rec.Code != http.StatusOK || rec.Body.String() != "data" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("status=%d headers=%#v body=%q", rec.Code, rec.Header(), rec.Body.String())
	}
	if strings.ContainsAny(rec.Header().Get("Content-Disposition"), "\r\n") {
		t.Fatalf("unsafe disposition=%q", rec.Header().Get("Content-Disposition"))
	}
}

func TestArchivePreflightErrorsRemainSanitizedJSON(t *testing.T) {
	f := &fakeService{err: errors.New("storage path /secret/key")}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/plugins/archive?plugin_id=p1", nil))
	if rec.Code != http.StatusInternalServerError || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || strings.Contains(rec.Body.String(), "/secret/key") {
		t.Fatalf("status=%d headers=%#v body=%s", rec.Code, rec.Header(), rec.Body.String())
	}
}

func TestHistoryListsUseExactRepositoryTotals(t *testing.T) {
	tests := []struct {
		name string
		url  string
		fake *fakeService
	}{
		{name: "audits", url: "/api/v1/internal/plugins/audit_logs?plugin_id=p1&page=2&page_size=10", fake: &fakeService{audits: []model.PluginAuditLog{{ID: "a1"}}, auditTotal: 37}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			testEngine(tt.fake).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tt.url, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var body struct {
				Pagination struct {
					Total int `json:"total"`
				} `json:"pagination"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body.Pagination.Total != 37 {
				t.Fatalf("pagination.total=%d want=37 body=%s", body.Pagination.Total, rec.Body.String())
			}
		})
	}
}

func TestListRequiresConfirmedQueryNames(t *testing.T) {
	f := &fakeService{}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plugins?plugin_type=skill", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"scene_code"`) {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestDetailEncodesPluginAndRelationWireIDs(t *testing.T) {
	space := "space-a"
	f := &fakeService{detail: &pluginsvc.Detail{
		Plugin:    &model.Plugin{ID: "expert-1", Type: model.PluginTypeExpert, SpaceID: &space, Tags: json.RawMessage(`[]`), Manifest: json.RawMessage(`{}`), Package: json.RawMessage(`{}`)},
		Relations: []model.PluginRelation{{ID: "rel-1", SourcePluginID: "expert-1", SourcePluginType: model.PluginTypeExpert, TargetPluginID: "skill-1", TargetPluginType: model.PluginTypeSkill, Type: "expert_skill"}},
	}}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/plugins/detail?plugin_id=expert:expert-1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{`"plugin_id":"expert:expert-1"`, `"source_plugin_id":"expert:expert-1"`, `"target_plugin_id":"skill:skill-1"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("missing %s in %s", want, rec.Body.String())
		}
	}
}

func TestDetailDefaultsRelationsAndSupportsFalse(t *testing.T) {
	for _, tc := range []struct {
		query string
		want  bool
	}{{query: "", want: true}, {query: "&include_relations=false", want: false}} {
		f := &fakeService{detail: &pluginsvc.Detail{Plugin: &model.Plugin{}}}
		rec := httptest.NewRecorder()
		testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/internal/plugins/detail?plugin_id=p1"+tc.query, nil))
		if rec.Code != http.StatusOK || f.includeRelations != tc.want {
			t.Fatalf("query=%q status=%d include_relations=%v body=%s", tc.query, rec.Code, f.includeRelations, rec.Body.String())
		}
	}
}

func TestOldRESTRoutesAreNotRegistered(t *testing.T) {
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodPost, "/api/v1/plugins", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodGet, "/api/v1/plugins/p1", nil),
		httptest.NewRequest(http.MethodPatch, "/api/v1/plugins/p1", strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/p1", nil),
	} {
		rec := httptest.NewRecorder()
		testEngine(&fakeService{}).ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s %s status=%d", req.Method, req.URL.Path, rec.Code)
		}
	}
}

func TestListUsesOffsetEnvelope(t *testing.T) {
	f := &fakeService{list: []model.Plugin{{ID: "p1", Name: "One"}}}
	rec := httptest.NewRecorder()
	testEngine(f).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/plugins?scene_code=loop&plugin_type=skill&page=2&page_size=10", nil))
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
