package plugin

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	apiresponse "github.com/Mininglamp-OSS/octo-marketplace/internal/api/response"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

// maxReviewBodyBytes bounds a review request body. These payloads are a handful
// of short strings; the plugin content itself is never resubmitted here.
const maxReviewBodyBytes = 64 << 10

// decodeReviewBody is a size-bounded, unknown-field-TOLERANT decoder. The strict
// `decode` used by /plugins/upsert would reject a client that sends a harmless
// extra field, and the octo-web review client is already built and shipped
// against this contract — rejecting its payload outright is a worse failure than
// ignoring a field we do not read.
func decodeReviewBody(c *gin.Context, dst any) bool {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxReviewBodyBytes)
	if err := json.NewDecoder(c.Request.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			apiresponse.Fail(c, http.StatusRequestEntityTooLarge, "PAYLOAD_TOO_LARGE", "request body is too large", map[string]any{"max_bytes": maxReviewBodyBytes}, "Reduce the request size.")
			return false
		}
		if errors.Is(err, io.EOF) {
			validation(c, "body")
			return false
		}
		validation(c, "body")
		return false
	}
	return true
}

// reviewRequestResponse is the wire form of a review request. It deliberately
// omits the frozen manifest/package/relation bytes: a list must not carry them
// at all, and the detail view exposes the reviewable content as readme_content
// rather than shipping the whole snapshot to the browser.
type reviewRequestResponse struct {
	ReviewID       string                      `json:"review_id"`
	PluginID       string                      `json:"plugin_id"`
	SpaceID        string                      `json:"space_id"`
	TargetScope    string                      `json:"target_scope"`
	Status         model.ReviewStatus          `json:"status"`
	Kind           model.ReviewKind            `json:"kind"`
	Version        string                      `json:"version"`
	Changelog      *string                     `json:"changelog,omitempty"`
	ManifestHash   string                      `json:"manifest_hash"`
	PluginHash     string                      `json:"plugin_hash"`
	ApplicantID    string                      `json:"applicant_id"`
	ApplicantName  string                      `json:"applicant_name"`
	ReviewerID     *string                     `json:"reviewer_id,omitempty"`
	ReviewerName   *string                     `json:"reviewer_name,omitempty"`
	Reason         *string                     `json:"reason,omitempty"`
	DecisionSource *model.ReviewDecisionSource `json:"decision_source,omitempty"`
	SubmittedAt    time.Time                   `json:"submitted_at" swaggertype:"string,date-time"`
	ReviewedAt     *time.Time                  `json:"reviewed_at,omitempty" swaggertype:"string,date-time"`
	PluginName     string                      `json:"plugin_name,omitempty"`
	PluginType     model.PluginType            `json:"plugin_type,omitempty"`
	// PluginIcon is a display URL (an uploaded icon is stored as an object key and
	// must be presigned), resolved by the service through the same path the plugin
	// list uses.
	PluginIcon     string  `json:"plugin_icon,omitempty"`
	CurrentVersion *string `json:"current_version,omitempty"`
	// ReadmeContent is the primary document of the FROZEN submission; populated
	// on the detail read only.
	ReadmeContent string `json:"readme_content,omitempty"`
}

// reviewSubmitRequest is the submit body.
//
// manifest_json / plugin_json are the reviewed CONTENT and are supplied together
// or not at all. They are REQUIRED when the plugin is already listed to the org
// (kind=upgrade): a listed plugin's live row is what the Space is already
// reading, so freezing it would make the review a formality over content that
// already shipped. While the plugin is a private draft they may be omitted and
// the draft row is snapshotted.
//
// relations uses the same target-state shape and semantics as /plugins/upsert:
// present (even empty) replaces the reviewed graph, ABSENT inherits the plugin's
// live graph — so a client editing only documents cannot empty an expert team by
// forgetting the field.
type reviewSubmitRequest struct {
	PluginID     string             `json:"plugin_id"`
	Version      string             `json:"version"`
	Changelog    string             `json:"changelog,omitempty"`
	ManifestJSON json.RawMessage    `json:"manifest_json,omitempty" swaggertype:"object"`
	PluginJSON   json.RawMessage    `json:"plugin_json,omitempty" swaggertype:"object"`
	Relations    *[]relationRequest `json:"relations,omitempty"`
}

type reviewRejectRequest struct {
	Reason string `json:"reason"`
}

func reviewDTO(r *model.PluginReviewRequest) reviewRequestResponse {
	if r == nil {
		return reviewRequestResponse{}
	}
	return reviewRequestResponse{
		ReviewID:       r.ID,
		PluginID:       r.PluginID,
		SpaceID:        r.SpaceID,
		TargetScope:    r.TargetScope,
		Status:         r.Status,
		Kind:           r.Kind,
		Version:        r.Version,
		Changelog:      r.Changelog,
		ManifestHash:   r.ManifestHash,
		PluginHash:     r.PluginHash,
		ApplicantID:    r.ApplicantUID,
		ApplicantName:  r.ApplicantName,
		ReviewerID:     r.ReviewerUID,
		ReviewerName:   r.ReviewerName,
		Reason:         r.Reason,
		DecisionSource: r.DecisionSource,
		SubmittedAt:    r.SubmittedAt,
		ReviewedAt:     r.ReviewedAt,
		PluginName:     r.PluginName,
		PluginType:     r.PluginType,
		PluginIcon:     r.PluginIcon,
		CurrentVersion: r.CurrentVersion,
		ReadmeContent:  r.ReadmeContent,
	}
}

// SubmitReview submits a plugin for Space visibility review.
//
// @Summary Submit a plugin for Space review
// @Description Freezes the reviewed content under a caller-supplied version label and queues it for Space owner/admin approval. manifest_json and plugin_json are supplied together and are REQUIRED once the plugin is listed to the org; while it is a private draft they may be omitted and the draft row is snapshotted. Submitting NEVER changes the plugin row — the listed content only changes when a reviewer approves. Only the plugin owner may submit, only one request per plugin may be pending, and a version label already published for that plugin is refused.
// @Tags plugin
// @ID plugin.review_request.create
// @Accept json
// @Produce json
// @Security Bearer
// @Param payload body reviewSubmitRequest true "Submission"
// @Success 200 {object} apiresponse.Data[reviewRequestResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR: manifest_json/plugin_json are required for an already-listed plugin"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT: a request is already pending, or the version label is already published"
// @Failure 429 {object} apiresponse.Error "RATE_LIMITED"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins/review_requests [post]
func (h *Handler) SubmitReview(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	var body reviewSubmitRequest
	if !decodeReviewBody(c, &body) {
		return
	}
	params := pluginsvc.ReviewSubmitParams{
		PluginID:  body.PluginID,
		Version:   body.Version,
		Changelog: body.Changelog,
		Manifest:  body.ManifestJSON,
		Package:   body.PluginJSON,
	}
	if body.Relations != nil {
		rels := make([]pluginsvc.RelationRequest, 0, len(*body.Relations))
		for _, r := range *body.Relations {
			rels = append(rels, pluginsvc.RelationRequest{
				ID:             r.RelationID,
				SourcePluginID: r.SourcePluginID,
				TargetPluginID: r.TargetPluginID,
				Type:           r.RelationType,
				SortOrder:      r.SortOrder,
				Data:           r.Data,
			})
		}
		params.Relations = &rels
	}
	out, err := h.svc.SubmitReview(c.Request.Context(), caller, params)
	if err != nil {
		writeServiceError(c, err, "plugin_review_request.create")
		return
	}
	apiresponse.OK(c, reviewDTO(out))
}

// ListReviews lists review requests for the caller or for the Space queue.
//
// @Summary List plugin review requests
// @Description mode=mine lists the caller's own submissions; mode=space lists every request in the Space and requires the Space owner/admin role. Both are always scoped to the Space of the request.
// @Tags plugin
// @ID plugin.review_request.list
// @Accept json
// @Produce json
// @Security Bearer
// @Param mode query string true "List mode" Enums(mine,space)
// @Param status query string false "Filter by status" Enums(pending,approved,rejected,canceled)
// @Param page query int false "Page number, default 1"
// @Param page_size query int false "Page size, default 20, max 100"
// @Success 200 {object} apiresponse.OffsetList[reviewRequestResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN: mode=space requires the Space owner/admin role"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 429 {object} apiresponse.Error "RATE_LIMITED"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins/review_requests [get]
func (h *Handler) ListReviews(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	page, pageSize, ok := pagination(c)
	if !ok {
		validation(c, "page")
		return
	}
	status := model.ReviewStatus(strings.TrimSpace(c.Query("status")))
	switch status {
	case "", model.ReviewStatusPending, model.ReviewStatusApproved, model.ReviewStatusRejected, model.ReviewStatusCanceled:
	default:
		validation(c, "status")
		return
	}
	// Validated here rather than left to the service so the 400 names the
	// offending query parameter instead of a generic "body".
	mode := strings.TrimSpace(c.Query("mode"))
	if mode != "mine" && mode != "space" {
		validation(c, "mode")
		return
	}
	items, total, err := h.svc.ListReviews(c.Request.Context(), caller, mode, status, page, pageSize)
	if err != nil {
		writeServiceError(c, err, "plugin_review_request.list")
		return
	}
	out := make([]reviewRequestResponse, 0, len(items))
	for _, item := range items {
		out = append(out, reviewDTO(item))
	}
	apiresponse.Offset(c, out, int(total), page, pageSize)
}

// GetReview returns one review request including the frozen submission preview.
//
// @Summary Get a plugin review request
// @Description Returns the request plus readme_content extracted from the frozen submission. Applicants see their own requests; Space owners/admins see every request in the Space. A request in another Space is reported as not found.
// @Tags plugin
// @ID plugin.review_request.get
// @Accept json
// @Produce json
// @Security Bearer
// @Param review_id path string true "Review request ID"
// @Success 200 {object} apiresponse.Data[reviewRequestResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 429 {object} apiresponse.Error "RATE_LIMITED"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins/review_requests/{review_id} [get]
func (h *Handler) GetReview(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	reviewID := strings.TrimSpace(c.Param("review_id"))
	if reviewID == "" {
		validation(c, "review_id")
		return
	}
	out, err := h.svc.GetReview(c.Request.Context(), caller, reviewID)
	if err != nil {
		writeServiceError(c, err, "plugin_review_request.get")
		return
	}
	apiresponse.OK(c, reviewDTO(out))
}

// ApproveReview approves a pending request and applies the frozen submission.
//
// @Summary Approve a plugin review request
// @Description Requires the Space owner/admin role. Applies the frozen documents and relation graph to the plugin, records a release version carrying the applicant's label, and for a first listing flips visibility from private to space. Losing a concurrent decision race returns CONFLICT.
// @Tags plugin
// @ID plugin.review_request.approve
// @Accept json
// @Produce json
// @Security Bearer
// @Param review_id path string true "Review request ID"
// @Success 200 {object} apiresponse.Data[detailResponse]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN: Space owner/admin role required"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT: already decided"
// @Failure 429 {object} apiresponse.Error "RATE_LIMITED"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins/review_requests/{review_id}/approve [post]
func (h *Handler) ApproveReview(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	reviewID := strings.TrimSpace(c.Param("review_id"))
	if reviewID == "" {
		validation(c, "review_id")
		return
	}
	plug, err := h.svc.ApproveReview(c.Request.Context(), caller, reviewID)
	if err != nil {
		writeServiceError(c, err, "plugin_review_request.approve")
		return
	}
	apiresponse.OK(c, detailResponse{Plugin: pluginDTO(plug), Relations: []relationResponse{}})
}

// RejectReview rejects a pending request with a required reason.
//
// @Summary Reject a plugin review request
// @Description Requires the Space owner/admin role and a non-empty reason of at most 1000 characters. The plugin is left untouched: a private draft stays private, and an already-listed version stays live. The applicant may edit and resubmit, reusing the same version label.
// @Tags plugin
// @ID plugin.review_request.reject
// @Accept json
// @Produce json
// @Security Bearer
// @Param review_id path string true "Review request ID"
// @Param payload body reviewRejectRequest true "Rejection reason"
// @Success 200 {object} apiresponse.Data[apiresponse.EmptyResp]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR: reason is required"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 403 {object} apiresponse.Error "FORBIDDEN: Space owner/admin role required"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND"
// @Failure 409 {object} apiresponse.Error "CONFLICT: already decided"
// @Failure 429 {object} apiresponse.Error "RATE_LIMITED"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins/review_requests/{review_id}/reject [post]
func (h *Handler) RejectReview(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	reviewID := strings.TrimSpace(c.Param("review_id"))
	if reviewID == "" {
		validation(c, "review_id")
		return
	}
	var body reviewRejectRequest
	if !decodeReviewBody(c, &body) {
		return
	}
	if err := h.svc.RejectReview(c.Request.Context(), caller, reviewID, body.Reason); err != nil {
		writeServiceError(c, err, "plugin_review_request.reject")
		return
	}
	apiresponse.OK(c, apiresponse.EmptyResp{})
}

// CancelReview withdraws the caller's own pending request.
//
// @Summary Cancel a plugin review request
// @Description Withdraws a pending request. Only the applicant may cancel; reviewers reject instead. A request that has already been decided returns CONFLICT, not NOT_FOUND.
// @Tags plugin
// @ID plugin.review_request.cancel
// @Accept json
// @Produce json
// @Security Bearer
// @Param review_id path string true "Review request ID"
// @Success 200 {object} apiresponse.Data[apiresponse.EmptyResp]
// @Failure 400 {object} apiresponse.Error "VALIDATION_ERROR"
// @Failure 401 {object} apiresponse.Error "AUTH_REQUIRED"
// @Failure 404 {object} apiresponse.Error "NOT_FOUND: no such pending request for this applicant"
// @Failure 409 {object} apiresponse.Error "CONFLICT: already decided"
// @Failure 429 {object} apiresponse.Error "RATE_LIMITED"
// @Failure 500 {object} apiresponse.Error "INTERNAL_ERROR"
// @Router /plugins/review_requests/{review_id}/cancel [post]
func (h *Handler) CancelReview(c *gin.Context) {
	caller, ok := caller(c)
	if !ok {
		unauthorized(c)
		return
	}
	reviewID := strings.TrimSpace(c.Param("review_id"))
	if reviewID == "" {
		validation(c, "review_id")
		return
	}
	if err := h.svc.CancelReview(c.Request.Context(), caller, reviewID); err != nil {
		writeServiceError(c, err, "plugin_review_request.cancel")
		return
	}
	apiresponse.OK(c, apiresponse.EmptyResp{})
}
