package plugin

import (
	"context"
	"strconv"

	apiresponse "github.com/Mininglamp-OSS/octo-marketplace/internal/api/response"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	marketmiddleware "github.com/Mininglamp-OSS/octo-marketplace/internal/middleware"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
	"github.com/gin-gonic/gin"
)

// AdminService is the handler-facing boundary over the unified Plugin service's
// admin (cross-Space, no ownership check) operations.
type AdminService interface {
	AdminList(context.Context, pluginsvc.Caller, model.PluginType, model.PluginVisibility, pluginsvc.ListParams) ([]model.Plugin, int64, error)
	AdminDetail(context.Context, pluginsvc.Caller, string, bool) (*pluginsvc.Detail, error)
	AdminCreate(context.Context, pluginsvc.Caller, pluginsvc.WriteRequest) (*pluginsvc.Detail, error)
	AdminUpdate(context.Context, pluginsvc.Caller, string, pluginsvc.WriteRequest) (*pluginsvc.Detail, error)
	AdminDelete(context.Context, pluginsvc.Caller, string) error
}

// AdminCategoryService is the handler-facing boundary over the admin taxonomy
// read. Category writes go through the legacy per-type category surfaces until
// the unified placement model supports runtime category creation.
type AdminCategoryService interface {
	AdminListCategories(context.Context, model.PluginType) ([]model.PluginCategory, error)
}

// AdminHandler serves the marketplace-admin plugin surface (/api/v1/admin/plugins*).
// It mirrors the legacy per-type admin surfaces (system MCPs, global skills,
// skill categories) over the unified plugin service, cross-Space and without an
// ownership check. The route group is gated by the admin authenticator, so the
// route gate is the authorization; the handlers build an admin caller with no
// Space (system rows live outside the Space model).
type AdminHandler struct {
	svc  AdminService
	cats AdminCategoryService
}

func NewAdmin(svc AdminService, cats AdminCategoryService) *AdminHandler {
	return &AdminHandler{svc: svc, cats: cats}
}

// RegisterAdmin mounts the admin plugin routes behind the admin gate. Static
// sibling paths (/admin/plugin_categories) go in a separate group so they never
// collide with the /admin/plugins/:plugin_id wildcard.
func (h *AdminHandler) RegisterAdmin(r *gin.Engine, adminAuth *marketmiddleware.AdminAuthenticator) {
	plugins := r.Group("/api/v1/admin/plugins", adminAuth.Handler(marketmiddleware.RoleMarketAdmin))
	plugins.GET("", h.List)
	plugins.POST("", h.Create)
	plugins.GET("/:plugin_id", h.Get)
	plugins.PATCH("/:plugin_id", h.Update)
	plugins.DELETE("/:plugin_id", h.Delete)

	admin := r.Group("/api/v1/admin", adminAuth.Handler(marketmiddleware.RoleMarketAdmin))
	admin.GET("/plugin_categories", h.ListCategories)
}

// adminCaller builds a caller from the admin identity. Unlike caller(c) it does
// not require a Space (admin routes carry none); the role gate already admitted
// the request, so any identity here is an admin.
func adminCaller(c *gin.Context) (pluginsvc.Caller, bool) {
	identity, ok := marketmiddleware.Identity(c)
	if !ok || identity.UID == "" {
		return pluginsvc.Caller{}, false
	}
	return pluginsvc.Caller{
		UID:           identity.UID,
		Name:          identity.Name,
		RequestID:     logging.RequestIDFromGin(c),
		IsSystemAdmin: true, // admitted through the admin gate
	}, true
}

// List godoc
// @Summary List plugins (admin)
// @Description List plugins of one type across all Spaces, optionally narrowed to a visibility class (e.g. system connectors). Admin only.
// @Tags admin_plugin
// @ID admin_plugin.list
// @Produce json
// @Security Bearer
// @Param plugin_type query string true "Plugin type" Enums(expert,expert_team,skill,connector)
// @Param visibility query string false "Filter by visibility" Enums(public,space,private,system)
// @Param q query string false "Search keyword"
// @Param category_id query string false "Category id"
// @Param sort query string false "Sort mode"
// @Param page query int false "Page number, default 1"
// @Param page_size query int false "Page size, default 20, max 100"
// @Success 200 {object} apiresponse.OffsetList[listItemResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /admin/plugins [get]
func (h *AdminHandler) List(c *gin.Context) {
	caller, ok := adminCaller(c)
	if !ok {
		unauthorized(c)
		return
	}
	page, pageSize, ok := pagination(c)
	if !ok {
		validation(c, "pagination")
		return
	}
	pluginType := model.PluginType(c.Query("plugin_type"))
	if pluginType == "" {
		validation(c, "plugin_type")
		return
	}
	items, total, err := h.svc.AdminList(c.Request.Context(), caller, pluginType, model.PluginVisibility(c.Query("visibility")), pluginsvc.ListParams{
		CategoryID: c.Query("category_id"), Tags: splitQuery(c.QueryArray("tag")), Keyword: c.Query("q"), Sort: c.Query("sort"), Limit: pageSize, Offset: (page - 1) * pageSize,
	})
	if err != nil {
		writeServiceError(c, err, "plugin.admin.list")
		return
	}
	out := make([]listItemResponse, len(items))
	for i := range items {
		out[i] = listItemDTO(&items[i])
	}
	apiresponse.Offset(c, out, int(total), page, pageSize)
}

// Get godoc
// @Summary Get plugin (admin)
// @Description Return any plugin's full detail by id, ignoring Space scope. Admin only.
// @Tags admin_plugin
// @ID admin_plugin.get
// @Produce json
// @Security Bearer
// @Param plugin_id path string true "Plugin ID"
// @Param include_relations query bool false "Include one-level relations (default true)"
// @Success 200 {object} apiresponse.Data[detailResponse]
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Router /admin/plugins/{plugin_id} [get]
func (h *AdminHandler) Get(c *gin.Context) {
	caller, ok := adminCaller(c)
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
	v, err := h.svc.AdminDetail(c.Request.Context(), caller, c.Param("plugin_id"), includeRelations)
	if err != nil {
		writeServiceError(c, err, "plugin.admin.get")
		return
	}
	apiresponse.OK(c, detailDTO(v))
}

// Create godoc
// @Summary Create plugin (admin)
// @Description Mint a system connector or public skill/expert. Visibility and Space are set by convention (connector=system/NULL Space; others=public/global). Admin only.
// @Tags admin_plugin
// @ID admin_plugin.create
// @Accept json
// @Produce json
// @Security Bearer
// @Param body body upsertRequest true "Plugin document"
// @Success 200 {object} apiresponse.Data[detailResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 409 {object} apiresponse.Error "CONFLICT"
// @Router /admin/plugins [post]
func (h *AdminHandler) Create(c *gin.Context) {
	caller, ok := adminCaller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req upsertRequest
	if !decode(c, &req) {
		return
	}
	v, err := h.svc.AdminCreate(c.Request.Context(), caller, req.serviceRequest())
	if err != nil {
		writeServiceError(c, err, "plugin.admin.create")
		return
	}
	apiresponse.OK(c, detailDTO(v))
}

// Update godoc
// @Summary Update plugin (admin)
// @Description Update any plugin by id, ignoring owner/Space. Admin only.
// @Tags admin_plugin
// @ID admin_plugin.update
// @Accept json
// @Produce json
// @Security Bearer
// @Param plugin_id path string true "Plugin ID"
// @Param body body upsertRequest true "Plugin document"
// @Success 200 {object} apiresponse.Data[detailResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Router /admin/plugins/{plugin_id} [patch]
func (h *AdminHandler) Update(c *gin.Context) {
	caller, ok := adminCaller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var req upsertRequest
	if !decode(c, &req) {
		return
	}
	v, err := h.svc.AdminUpdate(c.Request.Context(), caller, c.Param("plugin_id"), req.serviceRequest())
	if err != nil {
		writeServiceError(c, err, "plugin.admin.update")
		return
	}
	apiresponse.OK(c, detailDTO(v))
}

// Delete godoc
// @Summary Delete plugin (admin)
// @Description Soft-delete any plugin by id, ignoring owner/Space. Admin only.
// @Tags admin_plugin
// @ID admin_plugin.delete
// @Produce json
// @Security Bearer
// @Param plugin_id path string true "Plugin ID"
// @Success 200 {object} apiresponse.Data[deleteResponse]
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Router /admin/plugins/{plugin_id} [delete]
func (h *AdminHandler) Delete(c *gin.Context) {
	caller, ok := adminCaller(c)
	if !ok {
		unauthorized(c)
		return
	}
	id := c.Param("plugin_id")
	if err := h.svc.AdminDelete(c.Request.Context(), caller, id); err != nil {
		writeServiceError(c, err, "plugin.admin.delete")
		return
	}
	apiresponse.OK(c, deleteResponse{PluginID: id, Deleted: true})
}

// ListCategories godoc
// @Summary List plugin categories (admin)
// @Description List every category applicable to a plugin type with its live-plugin usage count, including empty ones. Admin only.
// @Tags admin_plugin
// @ID admin_plugin_category.list
// @Produce json
// @Security Bearer
// @Param plugin_type query string true "Plugin type"
// @Success 200 {object} apiresponse.Data[[]categoryResponse]
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /admin/plugin_categories [get]
func (h *AdminHandler) ListCategories(c *gin.Context) {
	if _, ok := adminCaller(c); !ok {
		unauthorized(c)
		return
	}
	items, err := h.cats.AdminListCategories(c.Request.Context(), model.PluginType(c.Query("plugin_type")))
	if err != nil {
		writeServiceError(c, err, "plugin.admin.category.list")
		return
	}
	out := make([]categoryResponse, len(items))
	for i, x := range items {
		out[i] = categoryResponse{CategoryID: x.ID, Name: x.Name, IconKey: x.IconKey, PluginTypes: stringSlice(x.PluginTypes), SortOrder: x.SortOrder, PluginCount: x.PluginCount}
	}
	apiresponse.OK(c, out)
}
