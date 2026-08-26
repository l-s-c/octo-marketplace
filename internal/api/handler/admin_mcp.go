package handler

import (
	"context"

	apiresponse "github.com/Mininglamp-OSS/octo-marketplace/internal/api/response"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/apierr"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/service"
	"github.com/gin-gonic/gin"
)

type AdminMCPService interface {
	Probe(context.Context, service.ProbeRequest) (service.ProbeResponse, *apierr.Error)
}

type AdminMCP struct{ svc AdminMCPService }

func NewAdminMCP(svc AdminMCPService) *AdminMCP { return &AdminMCP{svc: svc} }

// Probe godoc
// @Summary Probe system MCP server
// @Description Probe a remote MCP connection through the administrator surface without persisting it.
// @Tags admin_mcp
// @ID admin_mcp.probe
// @Accept json
// @Produce json
// @Security AdminToken
// @Param body body service.ProbeRequest true "Connection to probe"
// @Success 200 {object} apiresponse.Data[service.ProbeResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /admin/mcps/_probe [post]
func (h *AdminMCP) Probe(c *gin.Context) {
	var req service.ProbeRequest
	if err := decodeJSON(c, &req); err != nil {
		writeError(c, err)
		return
	}
	resp, apiErr := h.svc.Probe(c.Request.Context(), req)
	if apiErr != nil {
		writeError(c, apiErr)
		return
	}
	if resp.Tools == nil {
		resp.Tools = []model.Tool{}
	}
	apiresponse.OK(c, resp)
}
