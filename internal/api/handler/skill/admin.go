package skill

import (
	"errors"
	"net/http"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/api/errcode"
	apiresponse "github.com/Mininglamp-OSS/octo-marketplace/internal/api/response"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/middleware"
	skillsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/skill"
	"github.com/gin-gonic/gin"
)

// RegisterAdmin registers admin skill routes on the given engine.
func (h *Handler) RegisterAdmin(r *gin.Engine, adminAuth *middleware.AdminAuthenticator) {
	admin := r.Group("/api/v1/admin/skills", adminAuth.Handler(middleware.RoleMarketAdmin))
	admin.GET("/:skill_id/skill_md", h.AdminGetSkillMD)
}

// AdminGetSkillMD godoc
// @Summary Get SKILL.md (admin)
// @Description Return SKILL.md for a public skill without Space restriction.
// @Tags admin_skill
// @ID admin_skill.skillmd.get
// @Accept json
// @Produce json
// @Security AdminToken
// @Param skill_id path string true "Skill ID"
// @Success 200 {object} apiresponse.Data[SkillMDResponse]
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /admin/skills/{skill_id}/skill_md [get]
func (h *Handler) AdminGetSkillMD(c *gin.Context) {
	if _, ok := middleware.Identity(c); !ok {
		apiresponse.Fail(c, http.StatusUnauthorized, errcode.Unauthorized, "authentication is required", nil, "")
		return
	}
	id := c.Param("skill_id")
	data, err := h.svc.AdminGetSkillMD(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, skillsvc.ErrNotFound) {
			apiresponse.Fail(c, http.StatusNotFound, errcode.NotFound, "not found", nil, "")
			return
		}
		if errors.Is(err, skillsvc.ErrNoFile) {
			apiresponse.Fail(c, http.StatusNotFound, errcode.NotFound, "skill-md not available", nil, "")
			return
		}
		apiresponse.Internal(c, err, "admin.skill.skillmd.get")
		return
	}
	apiresponse.OK(c, SkillMDResponse{Content: string(data)})
}
