// Package plugin exposes the unified Plugin HTTP API.
package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/api/errcode"
	apiresponse "github.com/Mininglamp-OSS/octo-marketplace/internal/api/response"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	marketmiddleware "github.com/Mininglamp-OSS/octo-marketplace/internal/middleware"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
	"github.com/gin-gonic/gin"
)

const maxBodyBytes = 3 << 20

// Service is the handler-facing boundary over the unified Plugin service.
type Service interface {
	List(context.Context, pluginsvc.Caller, pluginsvc.ListParams) ([]model.Plugin, int64, error)
	Detail(context.Context, pluginsvc.Caller, string, bool) (*pluginsvc.Detail, error)
	Create(context.Context, pluginsvc.Caller, pluginsvc.WriteRequest) (*pluginsvc.Detail, error)
	Update(context.Context, pluginsvc.Caller, string, pluginsvc.WriteRequest) (*pluginsvc.Detail, error)
	Delete(context.Context, pluginsvc.Caller, string) error
	ListAuditLogs(context.Context, pluginsvc.Caller, string, int, int) ([]model.PluginAuditLog, int64, error)
	ListVersions(context.Context, pluginsvc.Caller, string, int, int) ([]model.PluginVersion, int64, error)
	Publish(context.Context, pluginsvc.Caller, string, pluginsvc.PublishRequest) (*model.PluginVersion, error)
	Duplicate(context.Context, pluginsvc.Caller, string, string) (*model.Plugin, error)
	InitAttachmentUpload(context.Context, pluginsvc.Caller, string, string, int64) (*pluginsvc.AttachmentUpload, error)
	OpenAttachment(context.Context, pluginsvc.Caller, string, string) (*pluginsvc.AttachmentDownload, error)
	PrepareArchive(context.Context, pluginsvc.Caller, string, string) (*pluginsvc.Archive, error)
	WriteArchive(context.Context, *pluginsvc.Archive, io.Writer) error
}

// CategoryService is separate because category listing is a read-only operation
// served by the read-side category service rather than the full Plugin service.
type CategoryService interface {
	ListCategories(context.Context, pluginsvc.Caller, string, model.PluginType) ([]model.PluginCategory, error)
}

type Handler struct {
	svc        Service
	categories CategoryService
}

func New(svc Service, categories ...CategoryService) *Handler {
	h := &Handler{svc: svc}
	if len(categories) > 0 {
		h.categories = categories[0]
	}
	return h
}

func (h *Handler) Register(rg *gin.RouterGroup) {
	rg.GET("/plugins", h.List)
	rg.GET("/plugin_categories", h.ListCategories)
	internal := rg.Group("/internal/plugins")
	internal.GET("/detail", h.Get)
	internal.POST("/upsert", h.Upsert)
	internal.POST("/delete", h.Delete)
	internal.POST("/duplicate", h.Duplicate)
	internal.GET("/audit_logs", h.ListAuditLogs)
	internal.GET("/versions", h.ListVersions)
	internal.GET("/archive", h.DownloadArchive)
	internal.POST("/attachment/upload", h.InitAttachmentUpload)
	internal.GET("/attachment/download", h.DownloadAttachment)
	internal.POST("/publish", h.Publish)
}

type relationRequest struct {
	RelationID     string          `json:"relation_id,omitempty"`
	SourcePluginID string          `json:"source_plugin_id,omitempty"`
	TargetPluginID string          `json:"target_plugin_id"`
	RelationType   string          `json:"relation_type"`
	SortOrder      int             `json:"sort_order"`
	Data           json.RawMessage `json:"data,omitempty" swaggertype:"object"`
}

type pluginWriteRequest struct {
	PluginID     string                 `json:"plugin_id,omitempty"`
	PluginName   string                 `json:"plugin_name"`
	PluginType   model.PluginType       `json:"plugin_type"`
	CategoryID   *string                `json:"category_id,omitempty"`
	Tags         []string               `json:"tags"`
	Publisher    string                 `json:"publisher,omitempty"`
	Visibility   model.PluginVisibility `json:"visibility"`
	ManifestJSON json.RawMessage        `json:"manifest_json" swaggertype:"object"`
	PluginJSON   json.RawMessage        `json:"plugin_json" swaggertype:"object"`
}

type upsertRequest struct {
	Plugin    pluginWriteRequest `json:"plugin"`
	Relations []relationRequest  `json:"relations"`
}

func (r upsertRequest) serviceRequest() pluginsvc.WriteRequest {
	relations := make([]pluginsvc.RelationRequest, len(r.Relations))
	for i, x := range r.Relations {
		relations[i] = pluginsvc.RelationRequest{ID: x.RelationID, SourcePluginID: x.SourcePluginID, TargetPluginID: x.TargetPluginID, Type: x.RelationType, SortOrder: x.SortOrder, Data: x.Data}
	}
	return pluginsvc.WriteRequest{Name: r.Plugin.PluginName, Type: r.Plugin.PluginType, CategoryID: r.Plugin.CategoryID, Tags: rawJSON(r.Plugin.Tags), Publisher: r.Plugin.Publisher, Visibility: r.Plugin.Visibility, Manifest: r.Plugin.ManifestJSON, Package: r.Plugin.PluginJSON, Relations: relations}
}

type placementRequest struct {
	PlacementCode string  `json:"placement_code"`
	CategoryID    *string `json:"category_id,omitempty"`
	IsVisible     bool    `json:"is_visible"`
	SortOrder     int     `json:"sort_order"`
}

type publishRequest struct {
	PluginID   string             `json:"plugin_id"`
	Version    string             `json:"version"`
	Changelog  *string            `json:"changelog,omitempty"`
	Placements []placementRequest `json:"placements,omitempty"`
}

type duplicateRequest struct {
	SourcePluginID string `json:"source_plugin_id"`
	PluginName     string `json:"plugin_name,omitempty"`
}

type deleteRequest struct {
	PluginID string `json:"plugin_id"`
}

type deleteResponse struct {
	PluginID string `json:"plugin_id"`
	Deleted  bool   `json:"deleted"`
}

type attachmentUploadRequest struct {
	FileName    string `json:"file_name"`
	FileSize    int64  `json:"file_size"`
	ContentType string `json:"content_type,omitempty"`
}

type attachmentUploadResponse struct {
	ObjectKey string            `json:"object_key"`
	UploadURL string            `json:"upload_url" swaggertype:"string,uri"`
	Method    string            `json:"method"`
	Headers   map[string]string `json:"headers"`
	ExpiresIn int               `json:"expires_in"`
}

type pluginResponse struct {
	PluginID         string                 `json:"plugin_id"`
	PluginName       string                 `json:"plugin_name"`
	PluginType       model.PluginType       `json:"plugin_type"`
	IsEmbedded       bool                   `json:"is_embedded"`
	CategoryID       *string                `json:"category_id,omitempty"`
	Tags             []string               `json:"tags"`
	Publisher        string                 `json:"publisher,omitempty"`
	OwnerID          string                 `json:"owner_id"`
	SpaceID          *string                `json:"space_id,omitempty"`
	Visibility       model.PluginVisibility `json:"visibility"`
	CreatorName      string                 `json:"creator_name"`
	CreatedByType    string                 `json:"created_by_type"`
	CreatedByBotID   *string                `json:"created_by_bot_id,omitempty"`
	CreatedByBotName *string                `json:"created_by_bot_name,omitempty"`
	ManifestJSON     json.RawMessage        `json:"manifest_json" swaggertype:"object"`
	PluginJSON       json.RawMessage        `json:"plugin_json" swaggertype:"object"`
	ManifestHash     string                 `json:"manifest_hash"`
	PluginHash       string                 `json:"plugin_hash"`
	CurrentVersionID *string                `json:"current_version_id,omitempty"`
	Status           int                    `json:"status"`
	CreatedAt        time.Time              `json:"created_at" swaggertype:"string,date-time"`
	UpdatedAt        time.Time              `json:"updated_at" swaggertype:"string,date-time"`
}

// listItemResponse is the list-page projection: it carries the manifest for
// display but never the full plugin_json package.
type listItemResponse struct {
	PluginID         string                 `json:"plugin_id"`
	PluginName       string                 `json:"plugin_name"`
	PluginType       model.PluginType       `json:"plugin_type"`
	IsEmbedded       bool                   `json:"is_embedded"`
	CategoryID       *string                `json:"category_id,omitempty"`
	Tags             []string               `json:"tags"`
	Publisher        string                 `json:"publisher,omitempty"`
	OwnerID          string                 `json:"owner_id"`
	SpaceID          *string                `json:"space_id,omitempty"`
	Visibility       model.PluginVisibility `json:"visibility"`
	CreatorName      string                 `json:"creator_name"`
	CreatedByType    string                 `json:"created_by_type"`
	CreatedByBotID   *string                `json:"created_by_bot_id,omitempty"`
	CreatedByBotName *string                `json:"created_by_bot_name,omitempty"`
	ManifestJSON     json.RawMessage        `json:"manifest_json" swaggertype:"object"`
	ManifestHash     string                 `json:"manifest_hash"`
	PluginHash       string                 `json:"plugin_hash"`
	CurrentVersionID *string                `json:"current_version_id,omitempty"`
	Status           int                    `json:"status"`
	CreatedAt        time.Time              `json:"created_at" swaggertype:"string,date-time"`
	UpdatedAt        time.Time              `json:"updated_at" swaggertype:"string,date-time"`
}

type relationResponse struct {
	RelationID     string          `json:"relation_id"`
	SourcePluginID string          `json:"source_plugin_id"`
	TargetPluginID string          `json:"target_plugin_id"`
	RelationType   string          `json:"relation_type"`
	SortOrder      int             `json:"sort_order"`
	Data           json.RawMessage `json:"data,omitempty" swaggertype:"object"`
}

type detailResponse struct {
	Plugin    pluginResponse     `json:"plugin"`
	Relations []relationResponse `json:"relations"`
	// RelationResult reports upsert relation target-state synchronization:
	// empty relation_id created, known relation_id with changes updated, live
	// relations omitted from the submission deleted. Absent on reads.
	RelationResult *relationResultResponse `json:"relation_result,omitempty"`
}

type relationResultResponse struct {
	Created []string `json:"created"`
	Updated []string `json:"updated"`
	Deleted []string `json:"deleted"`
}

type auditLogResponse struct {
	AuditLogID       string          `json:"audit_log_id"`
	PluginID         string          `json:"plugin_id"`
	Action           string          `json:"action"`
	OperatorID       string          `json:"operator_id"`
	OperatorName     string          `json:"operator_name"`
	RequestID        string          `json:"request_id"`
	BeforeHash       *string         `json:"before_hash,omitempty"`
	AfterHash        *string         `json:"after_hash,omitempty"`
	ManifestSnapshot json.RawMessage `json:"manifest_snapshot,omitempty" swaggertype:"object"`
	PluginSnapshot   json.RawMessage `json:"plugin_snapshot,omitempty" swaggertype:"object"`
	Remark           *string         `json:"remark,omitempty"`
	CreatedAt        time.Time       `json:"created_at" swaggertype:"string,date-time"`
}

type versionResponse struct {
	VersionID    string           `json:"version_id"`
	PluginID     string           `json:"plugin_id"`
	Version      string           `json:"version"`
	Manifest     json.RawMessage  `json:"manifest" swaggertype:"object"`
	Package      json.RawMessage  `json:"package" swaggertype:"object"`
	ManifestHash string           `json:"manifest_hash"`
	PluginHash   string           `json:"plugin_hash"`
	Relations    []map[string]any `json:"relations"`
	Changelog    *string          `json:"changelog,omitempty"`
	CreatedBy    string           `json:"created_by"`
	CreatedAt    time.Time        `json:"created_at" swaggertype:"string,date-time"`
}

type categoryResponse struct {
	CategoryID  string   `json:"category_id"`
	Name        string   `json:"name"`
	IconKey     string   `json:"icon_key,omitempty"`
	PluginTypes []string `json:"plugin_types"`
	SortOrder   int      `json:"sort_order"`
	PluginCount int      `json:"plugin_count"`
}

// List godoc
// @Summary List plugins
// @Description List Plugins visible in the authoritative caller Space using offset pagination.
// @Tags plugin
// @ID plugin.list
// @Accept json
// @Produce json
// @Security Bearer
// @Param scene_code query string true "Marketplace scene code"
// @Param plugin_type query string true "Plugin type" Enums(expert,expert_team,skill,connector)
// @Param category_id query string false "Category ID (matches the plugin placement category in this scene)"
// @Param q query string false "Plugin name search query"
// @Param sort query string false "Sort order" Enums(newest,oldest,updated,name,placement)
// @Param page query int false "Page number, default 1"
// @Param page_size query int false "Page size, default 20, max 100"
// @Success 200 {object} apiresponse.OffsetList[listItemResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins [get]
func (h *Handler) List(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	page, pageSize, ok := pagination(c)
	if !ok {
		validation(c, "pagination")
		return
	}
	sceneCode := strings.TrimSpace(c.Query("scene_code"))
	if sceneCode == "" {
		validation(c, "scene_code")
		return
	}
	pluginType := model.PluginType(c.Query("plugin_type"))
	if pluginType == "" {
		validation(c, "plugin_type")
		return
	}
	items, total, err := h.svc.List(c.Request.Context(), caller, pluginsvc.ListParams{PlacementCode: sceneCode, Type: pluginType, CategoryID: c.Query("category_id"), Keyword: c.Query("q"), Sort: c.Query("sort"), Limit: pageSize, Offset: (page - 1) * pageSize})
	if err != nil {
		writeServiceError(c, err, "plugin.list")
		return
	}
	out := make([]listItemResponse, len(items))
	for i := range items {
		out[i] = listItemDTO(&items[i])
	}
	apiresponse.Offset(c, out, int(total), page, pageSize)
}

// Get godoc
// @Summary Get plugin
// @Description Return one Plugin and its visible one-level relations without leaking cross-Space existence.
// @Tags plugin
// @ID plugin.get
// @Accept json
// @Produce json
// @Security Bearer
// @Param plugin_id query string true "Plugin ID"
// @Param include_relations query bool false "Include one-level relations (default true)"
// @Success 200 {object} apiresponse.Data[detailResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/detail [get]
func (h *Handler) Get(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	includeRelations := true
	if raw, present := c.GetQuery("include_relations"); present {
		var err error
		includeRelations, err = strconv.ParseBool(raw)
		if err != nil {
			validation(c, "include_relations")
			return
		}
	}
	v, err := h.svc.Detail(c.Request.Context(), caller, c.Query("plugin_id"), includeRelations)
	if err != nil {
		writeServiceError(c, err, "plugin.get")
		return
	}
	apiresponse.OK(c, detailDTO(v))
}

// Upsert godoc
// @Summary Upsert plugin
// @Description Create or replace a Plugin from {plugin,relations}; owner and Space remain server-derived.
// @Tags plugin
// @ID plugin.upsert
// @Accept json
// @Produce json
// @Security Bearer
// @Param body body upsertRequest true "Plugin current state and relations"
// @Success 200 {object} apiresponse.Data[detailResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT"
// @Failure 413 {object} apiresponse.Error "PAYLOAD_TOO_LARGE"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/upsert [post]
func (h *Handler) Upsert(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req upsertRequest
	if !decode(c, &req) {
		return
	}
	var (
		v   *pluginsvc.Detail
		err error
	)
	if strings.TrimSpace(req.Plugin.PluginID) == "" {
		v, err = h.svc.Create(c.Request.Context(), caller, req.serviceRequest())
	} else {
		v, err = h.svc.Update(c.Request.Context(), caller, req.Plugin.PluginID, req.serviceRequest())
	}
	if err != nil {
		writeServiceError(c, err, "plugin.upsert")
		return
	}
	apiresponse.OK(c, detailDTO(v))
}

// ListAuditLogs godoc
// @Summary List plugin audit logs
// @Description List append-only audit records for a caller-owned Plugin in the current Space using offset pagination.
// @Tags plugin
// @ID plugin.audit_log.list
// @Accept json
// @Produce json
// @Security Bearer
// @Param plugin_id query string true "Plugin ID"
// @Param page query int false "Page number, default 1"
// @Param page_size query int false "Page size, default 20, max 100"
// @Success 200 {object} apiresponse.OffsetList[auditLogResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/audit_logs [get]
func (h *Handler) ListAuditLogs(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	page, size, ok := pagination(c)
	if !ok {
		validation(c, "pagination")
		return
	}
	items, total, err := h.svc.ListAuditLogs(c.Request.Context(), caller, c.Query("plugin_id"), size, (page-1)*size)
	if err != nil {
		writeServiceError(c, err, "plugin.audit_log.list")
		return
	}
	out := make([]auditLogResponse, len(items))
	for i, x := range items {
		out[i] = auditDTO(x)
	}
	apiresponse.Offset(c, out, int(total), page, size)
}

// Delete godoc
// @Summary Delete plugin
// @Description Soft-delete a caller-owned Plugin; live incoming relations from other Plugins block deletion.
// @Tags plugin
// @ID plugin.delete
// @Accept json
// @Produce json
// @Security Bearer
// @Param body body deleteRequest true "Plugin ID"
// @Success 200 {object} apiresponse.Data[deleteResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/delete [post]
func (h *Handler) Delete(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req deleteRequest
	if !decode(c, &req) {
		return
	}
	if err := h.svc.Delete(c.Request.Context(), caller, req.PluginID); err != nil {
		writeServiceError(c, err, "plugin.delete")
		return
	}
	apiresponse.OK(c, deleteResponse{PluginID: req.PluginID, Deleted: true})
}

// ListVersions godoc
// @Summary List plugin versions
// @Description List immutable published versions of a visible Plugin using offset pagination.
// @Tags plugin
// @ID plugin.version.list
// @Accept json
// @Produce json
// @Security Bearer
// @Param plugin_id query string true "Plugin ID"
// @Param page query int false "Page number, default 1"
// @Param page_size query int false "Page size, default 20, max 100"
// @Success 200 {object} apiresponse.OffsetList[versionResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/versions [get]
func (h *Handler) ListVersions(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	page, size, ok := pagination(c)
	if !ok {
		validation(c, "pagination")
		return
	}
	items, total, err := h.svc.ListVersions(c.Request.Context(), caller, c.Query("plugin_id"), size, (page-1)*size)
	if err != nil {
		writeServiceError(c, err, "plugin.version.list")
		return
	}
	out := make([]versionResponse, len(items))
	for i, x := range items {
		out[i] = versionDTO(x)
	}
	apiresponse.Offset(c, out, int(total), page, size)
}

// Publish godoc
// @Summary Publish plugin version
// @Description Create an immutable Plugin snapshot and atomically replace its marketplace placements.
// @Tags plugin
// @ID plugin.publish
// @Accept json
// @Produce json
// @Security Bearer
// @Param body body publishRequest true "Plugin ID, version, and placements"
// @Success 201 {object} apiresponse.Data[versionResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT"
// @Failure 413 {object} apiresponse.Error "PAYLOAD_TOO_LARGE"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/publish [post]
func (h *Handler) Publish(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req publishRequest
	if !decode(c, &req) {
		return
	}
	placements := make([]pluginsvc.PlacementRequest, len(req.Placements))
	for i, x := range req.Placements {
		placements[i] = pluginsvc.PlacementRequest{PlacementCode: x.PlacementCode, CategoryID: x.CategoryID, Visible: x.IsVisible, SortOrder: x.SortOrder}
	}
	v, err := h.svc.Publish(c.Request.Context(), caller, req.PluginID, pluginsvc.PublishRequest{Version: req.Version, Changelog: req.Changelog, Placements: placements})
	if err != nil {
		writeServiceError(c, err, "plugin.publish")
		return
	}
	c.JSON(http.StatusCreated, apiresponse.Data[versionResponse]{Data: versionDTO(*v)})
}

// Duplicate godoc
// @Summary Duplicate plugin
// @Description Clone a visible Plugin and its relation graph into a new private Plugin owned by the caller.
// @Tags plugin
// @ID plugin.duplicate
// @Accept json
// @Produce json
// @Security Bearer
// @Param body body duplicateRequest true "Source Plugin ID and optional Plugin name"
// @Success 201 {object} apiresponse.Data[pluginResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT"
// @Failure 413 {object} apiresponse.Error "PAYLOAD_TOO_LARGE"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/duplicate [post]
func (h *Handler) Duplicate(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req duplicateRequest
	if !decode(c, &req) {
		return
	}
	v, err := h.svc.Duplicate(c.Request.Context(), caller, req.SourcePluginID, req.PluginName)
	if err != nil {
		writeServiceError(c, err, "plugin.duplicate")
		return
	}
	apiresponse.Created(c, pluginDTO(v))
}

// InitAttachmentUpload godoc
// @Summary Initialize plugin attachment upload
// @Description Create a server-scoped object key and short-lived PUT target without modifying a Plugin.
// @Tags plugin
// @ID plugin.attachment.create
// @Accept json
// @Produce json
// @Security Bearer
// @Param body body attachmentUploadRequest true "Attachment metadata"
// @Success 200 {object} apiresponse.Data[attachmentUploadResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 413 {object} apiresponse.Error "PAYLOAD_TOO_LARGE"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/attachment/upload [post]
func (h *Handler) InitAttachmentUpload(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req attachmentUploadRequest
	if !decode(c, &req) {
		return
	}
	result, err := h.svc.InitAttachmentUpload(c.Request.Context(), caller, req.FileName, req.ContentType, req.FileSize)
	if err != nil {
		writeServiceError(c, err, "plugin.attachment.create")
		return
	}
	headers := make(map[string]string, len(result.Headers))
	for key, values := range result.Headers {
		if len(values) > 0 {
			headers[key] = values[0]
		}
	}
	apiresponse.OK(c, attachmentUploadResponse{ObjectKey: result.ObjectKey, UploadURL: result.UploadURL, Method: http.MethodPut, Headers: headers, ExpiresIn: result.ExpiresIn})
}

// DownloadAttachment godoc
// @Summary Download plugin attachment
// @Description Stream an object referenced by the visible Plugin Package after enforcing its managed Space prefix.
// @Tags plugin
// @ID plugin.attachment.download
// @Accept json
// @Produce application/octet-stream
// @Security Bearer
// @Param plugin_id query string true "Plugin ID"
// @Param object_key query string true "Package-referenced object key"
// @Success 200 {file} binary "Attachment bytes"
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/attachment/download [get]
func (h *Handler) DownloadAttachment(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	result, err := h.svc.OpenAttachment(c.Request.Context(), caller, c.Query("plugin_id"), c.Query("object_key"))
	if err != nil {
		writeServiceError(c, err, "plugin.attachment.download")
		return
	}
	defer result.Body.Close()
	c.Header("Content-Type", result.ContentType)
	c.Header("Content-Disposition", contentDisposition(result.Path))
	c.Header("Content-Length", strconv.FormatInt(result.Size, 10))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	if _, err := io.CopyN(c.Writer, result.Body, result.Size); err != nil {
		logging.Error("plugin_attachment_stream_failed", logging.ErrorField(err))
	}
}

// DownloadArchive godoc
// @Summary Download plugin archive
// @Description Build and stream a bounded ZIP from the current or requested immutable Plugin Package.
// @Tags plugin
// @ID plugin.archive.download
// @Accept json
// @Produce application/zip
// @Security Bearer
// @Param plugin_id query string true "Plugin ID"
// @Param version query string false "Immutable Plugin version"
// @Success 200 {file} binary "Plugin ZIP archive"
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 413 {object} apiresponse.Error "PAYLOAD_TOO_LARGE"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /internal/plugins/archive [get]
func (h *Handler) DownloadArchive(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	pluginID := c.Query("plugin_id")
	archive, err := h.svc.PrepareArchive(c.Request.Context(), caller, pluginID, c.Query("version"))
	if err != nil {
		writeServiceError(c, err, "plugin.archive.download")
		return
	}
	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", contentDisposition(pluginID+".zip"))
	c.Header("X-Content-Type-Options", "nosniff")
	c.Status(http.StatusOK)
	if err := h.svc.WriteArchive(c.Request.Context(), archive, c.Writer); err != nil {
		logging.Error("plugin_archive_stream_failed", logging.ErrorField(err))
	}
}

// ListCategories godoc
// @Summary List plugin categories
// @Description List placement-aware categories backed by Plugins visible in the current Space.
// @Tags plugin
// @ID plugin_category.list
// @Accept json
// @Produce json
// @Security Bearer
// @Param scene_code query string true "Marketplace scene code"
// @Param plugin_type query string true "Plugin type" Enums(expert,expert_team,skill,connector)
// @Success 200 {object} apiresponse.Data[[]categoryResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugin_categories [get]
func (h *Handler) ListCategories(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	placement, typ := strings.TrimSpace(c.Query("scene_code")), model.PluginType(c.Query("plugin_type"))
	if placement == "" {
		validation(c, "scene_code")
		return
	}
	if typ == "" {
		validation(c, "plugin_type")
		return
	}
	if h.categories == nil {
		apiresponse.Internal(c, errors.New("plugin category service is unavailable"), "plugin_category.list")
		return
	}
	items, err := h.categories.ListCategories(c.Request.Context(), caller, placement, typ)
	if err != nil {
		writeServiceError(c, err, "plugin_category.list")
		return
	}
	out := make([]categoryResponse, len(items))
	for i, x := range items {
		out[i] = categoryResponse{CategoryID: x.ID, Name: x.Name, IconKey: x.IconKey, PluginTypes: stringSlice(x.PluginTypes), SortOrder: x.SortOrder, PluginCount: x.PluginCount}
	}
	apiresponse.OK(c, out)
}

func caller(c *gin.Context) (pluginsvc.Caller, bool) {
	identity, ok := marketmiddleware.Identity(c)
	spaceID := marketmiddleware.SpaceID(c)
	if !ok || identity.UID == "" || spaceID == "" {
		return pluginsvc.Caller{}, false
	}
	out := pluginsvc.Caller{UID: identity.UID, Name: identity.Name, SpaceID: spaceID, RequestID: logging.RequestIDFromGin(c), IsSystemAdmin: identity.Role == marketmiddleware.RoleSuperAdmin}
	if bot, ok := marketmiddleware.BotIdentity(c); ok {
		out.BotUID, out.BotName = bot.BotUID, bot.BotName
	}
	return out, true
}
func pagination(c *gin.Context) (int, int, bool) {
	page, size := 1, 20
	var err error
	if raw := c.Query("page"); raw != "" {
		page, err = strconv.Atoi(raw)
		if err != nil || page < 1 {
			return 0, 0, false
		}
	}
	if raw := c.Query("page_size"); raw != "" {
		size, err = strconv.Atoi(raw)
		if err != nil || size < 1 || size > 100 {
			return 0, 0, false
		}
	}
	return page, size, true
}
func decode(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
	dec := json.NewDecoder(c.Request.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apiresponse.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body is too large", map[string]any{"max_bytes": maxBodyBytes}, "Reduce the request size.")
			return false
		}
		validation(c, "body")
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		validation(c, "body")
		return false
	}
	return true
}
func unauthorized(c *gin.Context) {
	apiresponse.Fail(c, http.StatusUnauthorized, errcode.Unauthorized, "authentication required", map[string]any{"reason": "invalid"}, "Authenticate and try again.")
}
func validation(c *gin.Context, field string) {
	apiresponse.Fail(c, http.StatusBadRequest, errcode.BadRequest, "request validation failed", map[string]any{"field": field, "reason": "invalid"}, "Correct the request and try again.")
}
func writeServiceError(c *gin.Context, err error, operation string) {
	switch {
	case errors.Is(err, pluginsvc.ErrInvalidRequest), errors.Is(err, pluginsvc.ErrSecretValue):
		validation(c, "body")
	case errors.Is(err, pluginsvc.ErrNotFound):
		apiresponse.Fail(c, http.StatusNotFound, errcode.NotFound, "plugin not found", map[string]any{"resource": "plugin"}, "Verify the plugin_id and try again.")
	case errors.Is(err, pluginsvc.ErrTooLarge):
		apiresponse.Fail(c, http.StatusRequestEntityTooLarge, errcode.FileTooLarge, "plugin artifact exceeds the size limit", nil, "Reduce the attachment size and try again.")
	case errors.Is(err, pluginsvc.ErrConflict):
		apiresponse.Fail(c, http.StatusConflict, errcode.Conflict, "plugin state conflicts with an existing resource", map[string]any{"conflict_reason": "state"}, "Refresh the resource and try again.")
	default:
		apiresponse.Internal(c, err, operation)
	}
}
func pluginDTO(p *model.Plugin) pluginResponse {
	if p == nil {
		return pluginResponse{}
	}
	return pluginResponse{PluginID: p.ID, PluginName: p.Name, PluginType: p.Type, IsEmbedded: p.IsEmbedded, CategoryID: p.CategoryID, Tags: stringSlice(p.Tags), Publisher: p.Publisher, OwnerID: p.OwnerUID, SpaceID: p.SpaceID, Visibility: p.Visibility, CreatorName: p.CreatorName, CreatedByType: p.CreatedByType, CreatedByBotID: p.CreatedByBotUID, CreatedByBotName: p.CreatedByBotName, ManifestJSON: normalizedObjectRaw(p.Manifest), PluginJSON: normalizedObjectRaw(p.Package), ManifestHash: p.ManifestHash, PluginHash: p.PluginHash, CurrentVersionID: p.CurrentVersionID, Status: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}
func listItemDTO(p *model.Plugin) listItemResponse {
	if p == nil {
		return listItemResponse{}
	}
	return listItemResponse{PluginID: p.ID, PluginName: p.Name, PluginType: p.Type, IsEmbedded: p.IsEmbedded, CategoryID: p.CategoryID, Tags: stringSlice(p.Tags), Publisher: p.Publisher, OwnerID: p.OwnerUID, SpaceID: p.SpaceID, Visibility: p.Visibility, CreatorName: p.CreatorName, CreatedByType: p.CreatedByType, CreatedByBotID: p.CreatedByBotUID, CreatedByBotName: p.CreatedByBotName, ManifestJSON: normalizedObjectRaw(p.Manifest), ManifestHash: p.ManifestHash, PluginHash: p.PluginHash, CurrentVersionID: p.CurrentVersionID, Status: p.Status, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt}
}
func detailDTO(d *pluginsvc.Detail) detailResponse {
	if d == nil {
		return detailResponse{Relations: []relationResponse{}}
	}
	rels := make([]relationResponse, len(d.Relations))
	for i, x := range d.Relations {
		sourceType := x.SourcePluginType
		if sourceType == "" && d.Plugin != nil && x.SourcePluginID == d.Plugin.ID {
			sourceType = d.Plugin.Type
		}
		rels[i] = relationResponse{RelationID: x.ID, SourcePluginID: x.SourcePluginID, TargetPluginID: x.TargetPluginID, RelationType: x.Type, SortOrder: x.SortOrder, Data: normalizedObjectRaw(x.Data)}
	}
	out := detailResponse{Plugin: pluginDTO(d.Plugin), Relations: rels}
	if d.RelationResult != nil {
		out.RelationResult = &relationResultResponse{Created: d.RelationResult.Created, Updated: d.RelationResult.Updated, Deleted: d.RelationResult.Deleted}
	}
	return out
}
func auditDTO(x model.PluginAuditLog) auditLogResponse {
	return auditLogResponse{AuditLogID: x.ID, PluginID: x.PluginID, Action: x.Action, OperatorID: x.OperatorID, OperatorName: x.OperatorName, RequestID: x.RequestID, BeforeHash: x.BeforeHash, AfterHash: x.AfterHash, ManifestSnapshot: x.ManifestSnapshot, PluginSnapshot: x.PluginSnapshot, Remark: x.Remark, CreatedAt: x.CreatedAt}
}
func versionDTO(x model.PluginVersion) versionResponse {
	return versionResponse{VersionID: x.ID, PluginID: x.PluginID, Version: x.Version, Manifest: x.Manifest, Package: x.Package, ManifestHash: x.ManifestHash, PluginHash: x.PluginHash, Relations: versionRelationSlice(x.Relations), Changelog: x.Changelog, CreatedBy: x.CreatedBy, CreatedAt: x.CreatedAt}
}

func rawJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func normalizedObjectRaw(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out map[string]any
	if dec.Decode(&out) != nil || out == nil {
		return json.RawMessage(`{}`)
	}
	return rawJSON(out)
}

func stringSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil || out == nil {
		return []string{}
	}
	return out
}

func contentDisposition(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return '_'
		}
		return r
	}, name)
	if name == "" || name == "." {
		name = "download"
	}
	return `attachment; filename="` + name + `"`
}

func versionRelationSlice(raw json.RawMessage) []map[string]any {
	var relations []model.PluginRelation
	if len(raw) == 0 || json.Unmarshal(raw, &relations) != nil || relations == nil {
		return []map[string]any{}
	}
	out := make([]map[string]any, 0, len(relations))
	for _, relation := range relations {
		out = append(out, map[string]any{
			"relation_id":      relation.ID,
			"source_plugin_id": relation.SourcePluginID,
			"target_plugin_id": relation.TargetPluginID,
			"relation_type":    relation.Type,
			"sort_order":       relation.SortOrder,
			"data":             normalizedObjectRaw(relation.Data),
		})
	}
	return out
}
