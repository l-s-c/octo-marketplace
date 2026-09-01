package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/logging"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/notify"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
	"go.uber.org/zap"
)

// CardActionTypeReviewDecision is the action_type shared with octo-server. It
// must match the OCTO_CARD_ACTION_ROUTES entry that points at this service's
// /v1/card-actions/decide endpoint, or cards dispatch to nowhere.
const CardActionTypeReviewDecision = "marketplace.plugin_review.decision"

// SpaceRole values match octo-server's space_member.role encoding exactly.
// octo-web displays the inverse (1=owner, 2=admin); that mapping is the
// frontend's problem and never crosses this boundary.
const (
	SpaceRoleMember = 0
	SpaceRoleAdmin  = 1
	SpaceRoleOwner  = 2
)

// maxRejectReasonRunes bounds the operator-supplied reject reason. The column is
// TEXT; the bound exists so an applicant-visible field cannot be used as bulk
// storage.
const maxRejectReasonRunes = 1000

var (
	ErrReviewInvalid = errors.New("review request invalid")
	// ErrReviewForbidden is an authorization refusal, distinct from ErrNotFound:
	// the caller may know the resource exists (it is in their Space) but lacks the
	// reviewer role. writeServiceError maps it to 403.
	ErrReviewForbidden = errors.New("review operation not permitted")
	ErrReasonRequired  = errors.New("reject reason is required")
	// ErrReviewContentRequired is returned when an upgrade submission carries no
	// content. Freezing the live row for an already-listed plugin would make the
	// review theatre: the content is already visible org-wide, and approval would
	// only mint a version label for something that shipped days ago.
	ErrReviewContentRequired = errors.New("review submission must carry the reviewed content")
	// ErrListedRequiresReview is returned when a tenant tries to modify an
	// already-listed plugin through the ordinary write path. It is a STATE
	// conflict, not a permission problem — the owner may change this plugin, just
	// through a review request. Delisting it first (space -> private) also works.
	ErrListedRequiresReview = errors.New("a listed plugin may only be changed through review")
)

// ReviewSubmitParams is the user-supplied input for a submit.
type ReviewSubmitParams struct {
	PluginID  string
	Version   string
	Changelog string
	// Manifest and Package are the reviewed CONTENT, supplied together or not at
	// all. REQUIRED when the plugin is already listed (kind=upgrade) and optional
	// while it is a private draft (kind=first), where the plugin row is nobody
	// else's business and snapshotting it is honest.
	Manifest json.RawMessage
	Package  json.RawMessage
	// Relations, when non-nil, is the authoritative target state of the reviewed
	// relation graph, with the same semantics as /plugins/upsert: an empty list
	// means "no relations". A nil pointer means "not submitted" and inherits the
	// plugin's live graph, so a client that only edits documents cannot silently
	// empty an expert team by omitting the field.
	Relations *[]RelationRequest
}

// SubmitReview freezes the plugin's current draft content under the applicant's
// version label and queues it for Space review.
func (s *Service) SubmitReview(ctx context.Context, caller Caller, params ReviewSubmitParams) (*model.PluginReviewRequest, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	params.PluginID = strings.TrimSpace(params.PluginID)
	params.Version = strings.TrimSpace(params.Version)
	params.Changelog = strings.TrimSpace(params.Changelog)
	if params.PluginID == "" || !validVersion(params.Version) {
		return nil, ErrReviewInvalid
	}
	if utf8.RuneCountInString(params.Changelog) > maxRejectReasonRunes {
		return nil, ErrReviewInvalid
	}
	sc := scope(caller)
	// includeRelations: the membership graph of an expert/expert_team is part of
	// what is being reviewed and must be frozen with the documents.
	detail, err := s.Detail(ctx, caller, params.PluginID, true)
	if err != nil {
		return nil, err
	}
	if detail.Plugin.OwnerUID != caller.UID {
		return nil, ErrNotFound
	}
	// An embedded child versions with its container and is never independently
	// reviewable, matching Update/Delete.
	if detail.Plugin.IsEmbedded {
		return nil, ErrNotFound
	}
	snap, err := s.freezeSubmission(ctx, caller, detail, params)
	if err != nil {
		return nil, err
	}
	now := s.now()
	req := &model.PluginReviewRequest{
		PluginID:      params.PluginID,
		SpaceID:       sc.SpaceID,
		TargetScope:   "space",
		Status:        model.ReviewStatusPending,
		Version:       params.Version,
		ApplicantUID:  caller.UID,
		ApplicantName: caller.Name,
		SubmittedAt:   now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	if params.Changelog != "" {
		req.Changelog = &params.Changelog
	}
	if err := s.repo.InsertReviewRequest(ctx, sc, req, snap); err != nil {
		return nil, mapStoreError(err)
	}
	stored, err := s.repo.GetReviewRequest(ctx, sc, req.ID, false)
	if err != nil {
		return nil, mapStoreError(err)
	}
	s.decorateReview(ctx, stored)
	s.dispatchReviewCard(caller, stored, detail.Plugin)
	return stored, nil
}

// freezeSubmission builds the frozen snapshot a reviewer will decide on.
//
// For an already-listed plugin the submission MUST carry its own content.
// Snapshotting the plugin row instead would make the review theatre: that row is
// what the org is already reading, so approval would only mint a version label
// for content that shipped the moment the author saved it. (The complementary
// half of this rule lives in Service.update, which refuses to let a tenant touch
// a listed row at all.) While the plugin is a private draft the row is nobody
// else's business, so snapshotting it is honest and content stays optional.
//
// Submitted content goes through the SAME canonicalization an ordinary write
// uses, so a malformed manifest is a 400 at submit rather than a surprise at
// approve time. It is validated against the plugin's existing name/type/tags:
// this endpoint reviews CONTENT, not market metadata.
func (s *Service) freezeSubmission(ctx context.Context, caller Caller, detail *Detail, params ReviewSubmitParams) (pluginrepo.FrozenSnapshot, error) {
	plugin := detail.Plugin
	hasManifest := len(bytes.TrimSpace(params.Manifest)) > 0
	hasPackage := len(bytes.TrimSpace(params.Package)) > 0
	if hasManifest != hasPackage {
		// Half a document set cannot be reviewed, and silently filling the other half
		// from the live row would reintroduce exactly the no-op above.
		return pluginrepo.FrozenSnapshot{}, ErrReviewInvalid
	}
	if !hasManifest && plugin.Visibility != model.PluginVisibilityPrivate {
		return pluginrepo.FrozenSnapshot{}, ErrReviewContentRequired
	}

	snap := pluginrepo.FrozenSnapshot{
		Manifest:     cloneJSON(plugin.Manifest),
		Package:      cloneJSON(plugin.Package),
		Relations:    detail.Relations,
		ManifestHash: plugin.ManifestHash,
		PluginHash:   plugin.PluginHash,
	}
	if hasManifest {
		// A fetch-edit-save client echoes back the GET package, whose storage
		// attachments no longer carry an inline key (it lives in the host sidecar).
		// Re-inject the stored key for unchanged storage content so the round trip is
		// not rejected, exactly as Service.update does.
		pkg := reinjectUpdateStorageKeys(params.Package, plugin.Package, plugin.AttachmentKeys)
		docs, err := CanonicalizeDocuments(plugin.Name, plugin.Type, plugin.Tags, params.Manifest, pkg, caller.SpaceID)
		if err != nil {
			return pluginrepo.FrozenSnapshot{}, err
		}
		// The snapshot deliberately does not freeze the storage sidecar (no schema
		// change), so the frozen package must reference exactly the object keys the
		// live row already holds — otherwise approve would pair frozen attachment
		// paths with a sidecar that has no entry for them. Introducing or changing a
		// storage attachment therefore has to go through the import/reupload path,
		// which owns object lifecycle.
		if !sameAttachmentSidecar(docs.AttachmentKeys, plugin.AttachmentKeys) {
			return pluginrepo.FrozenSnapshot{}, ErrInvalidRequest
		}
		snap.Manifest, snap.Package = docs.Manifest, docs.Package
		snap.ManifestHash, snap.PluginHash = docs.ManifestHash, docs.PluginHash
	}
	if params.Relations != nil {
		rels, err := s.buildRelations(ctx, caller, false, plugin, *params.Relations, s.now())
		if err != nil {
			return pluginrepo.FrozenSnapshot{}, err
		}
		snap.Relations = rels
	}
	return snap, nil
}

// sameAttachmentSidecar reports whether two storage sidecars hold the same
// path -> object-key map, tolerant of nil/empty/key ordering.
func sameAttachmentSidecar(a, b json.RawMessage) bool {
	ma, mb := attachmentKeyMap(a), attachmentKeyMap(b)
	if len(ma) != len(mb) {
		return false
	}
	for path, key := range ma {
		if other, ok := mb[path]; !ok || other != key {
			return false
		}
	}
	return true
}

// dispatchReviewCard queues the IM approval card. Everything it does — including
// building the payload — happens inside the post-commit best-effort hook, on a
// detached context: the request is already committed, so nothing here may fail
// the submit, and nothing here may add a round trip to the response path. An
// earlier design fetched the Space-admin roster synchronously before responding,
// which put an octo-server call (3s timeout) in front of every submit and lost
// the card entirely when the client disconnected right after commit.
func (s *Service) dispatchReviewCard(caller Caller, stored *model.PluginReviewRequest, plugin *model.Plugin) {
	if s.notify == nil || !s.notify.Enabled() || s.bestEffort == nil || stored == nil {
		return
	}
	pluginName := stored.PluginName
	if pluginName == "" && plugin != nil {
		pluginName = plugin.Name
	}
	var currentVersion string
	if stored.CurrentVersion != nil {
		currentVersion = *stored.CurrentVersion
	}
	var changelog string
	if stored.Changelog != nil {
		changelog = *stored.Changelog
	}
	req := notify.NotifyRequest{
		SpaceID:  stored.SpaceID,
		Service:  "marketplace",
		ActorUID: caller.UID,
		ApprovalCard: &notify.ApprovalCard{
			ActionType:  CardActionTypeReviewDecision,
			Title:       notify.CardTitle(pluginName),
			Description: notify.CardDescription(stored.PluginType, caller.Name, stored.Version, currentVersion, stored.Kind, changelog),
			Data:        map[string]string{"review_id": stored.ID, "plugin_id": stored.PluginID},
		},
	}
	reviewID := stored.ID
	n := s.notify
	s.bestEffort("review.submit", func(ctx context.Context) error {
		resp, err := n.NotifySpaceAdmins(ctx, req)
		if err != nil {
			return err
		}
		// HTTP 200 does not mean every admin received the card. With target_role
		// the caller never learns the roster, so `delivered` is the only delivery
		// record there is, and an empty one means the Space has no active human
		// admin — a legitimate state, but one an operator needs to see.
		if len(resp.Delivered) == 0 {
			logging.Warn("review: approval card reached no admin",
				zap.String("review_id", reviewID),
				zap.String("space_id", req.SpaceID),
				zap.Int("filtered", len(resp.Filtered)))
		} else {
			logging.Info("review: approval card dispatched",
				zap.String("review_id", reviewID),
				zap.Int("delivered", len(resp.Delivered)),
				zap.Int("filtered", len(resp.Filtered)))
		}
		for uid, reason := range resp.Filtered {
			logging.Warn("review: approval card filtered for target",
				zap.String("review_id", reviewID),
				zap.String("target_uid", uid),
				zap.String("reason", reason))
		}
		return nil
	})
}

func (s *Service) ListReviews(ctx context.Context, caller Caller, mode string, status model.ReviewStatus, page, pageSize int) ([]*model.PluginReviewRequest, int64, error) {
	if err := validateCaller(caller); err != nil {
		return nil, 0, err
	}
	if pageSize <= 0 || pageSize > maxListLimit {
		pageSize = 20
	}
	if page < 1 {
		page = 1
	}
	sc := scope(caller)
	f := pluginrepo.ReviewListFilter{
		SpaceID: sc.SpaceID,
		Status:  status,
		Limit:   pageSize,
		Offset:  (page - 1) * pageSize,
	}
	switch mode {
	case "mine":
		f.ApplicantUID = caller.UID
	case "space":
		if !s.isReviewer(caller) {
			return nil, 0, ErrReviewForbidden
		}
	default:
		return nil, 0, ErrReviewInvalid
	}
	items, total, err := s.repo.ListReviewRequests(ctx, sc, f)
	if err != nil {
		return nil, 0, mapStoreError(err)
	}
	for _, item := range items {
		s.decorateReview(ctx, item)
	}
	return items, total, nil
}

func (s *Service) GetReview(ctx context.Context, caller Caller, reviewID string) (*model.PluginReviewRequest, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	reviewID = strings.TrimSpace(reviewID)
	if reviewID == "" {
		return nil, ErrReviewInvalid
	}
	sc := scope(caller)
	rr, err := s.repo.LoadReviewSnapshot(ctx, sc, reviewID, s.isReviewer(caller))
	if err != nil {
		return nil, mapStoreError(err)
	}
	s.decorateReview(ctx, rr)
	// The reviewer is deciding on the frozen text, not on whatever the live plugin
	// says now, so the preview is extracted from the snapshot.
	rr.ReadmeContent = frozenReadme(rr.PluginJSON, rr.ManifestJSON)
	return rr, nil
}

func (s *Service) ApproveReview(ctx context.Context, caller Caller, reviewID string) (*model.Plugin, error) {
	if err := validateCaller(caller); err != nil {
		return nil, err
	}
	reviewID = strings.TrimSpace(reviewID)
	if reviewID == "" {
		return nil, ErrReviewInvalid
	}
	if !s.isReviewer(caller) {
		return nil, ErrReviewForbidden
	}
	plug, err := s.repo.ApproveReview(ctx, scope(caller), pluginrepo.ApproveReviewParams{
		ReviewID:       reviewID,
		ReviewerUID:    caller.UID,
		ReviewerName:   caller.Name,
		DecisionSource: model.ReviewDecisionSourceWeb,
		RequestID:      caller.RequestID,
	})
	if err != nil {
		return nil, mapStoreError(err)
	}
	plug.IconURL = s.resolveIcon(ctx, plug.Icon)
	return plug, nil
}

func (s *Service) RejectReview(ctx context.Context, caller Caller, reviewID, reason string) error {
	if err := validateCaller(caller); err != nil {
		return err
	}
	reviewID = strings.TrimSpace(reviewID)
	if reviewID == "" {
		return ErrReviewInvalid
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return ErrReasonRequired
	}
	if utf8.RuneCountInString(reason) > maxRejectReasonRunes {
		return ErrReviewInvalid
	}
	if !s.isReviewer(caller) {
		return ErrReviewForbidden
	}
	return mapStoreError(s.repo.RejectReview(ctx, scope(caller), pluginrepo.RejectReviewParams{
		ReviewID:       reviewID,
		ReviewerUID:    caller.UID,
		ReviewerName:   caller.Name,
		Reason:         reason,
		DecisionSource: model.ReviewDecisionSourceWeb,
		RequestID:      caller.RequestID,
	}))
}

// CancelReview withdraws the applicant's own pending request. It carries no role
// check: the repository predicate is applicant_uid, and a reviewer who wants the
// request gone rejects it (with a reason) rather than silently withdrawing it.
func (s *Service) CancelReview(ctx context.Context, caller Caller, reviewID string) error {
	if err := validateCaller(caller); err != nil {
		return err
	}
	reviewID = strings.TrimSpace(reviewID)
	if reviewID == "" {
		return ErrReviewInvalid
	}
	return mapStoreError(s.repo.CancelReview(ctx, scope(caller), reviewID, caller.UID))
}

// --- card protocol ------------------------------------------------------------

// CardActionResult is the typed response returned to octo-server for an IM
// decision. The field set and both enums are fixed by octo-server's
// DecodeDecisionResult (internal/cardactiondispatch/contract.go): it decodes with
// DisallowUnknownFields and rejects any value outside the enums, so this struct
// must stay exactly four fields.
type CardActionResult struct {
	Disposition string            `json:"disposition"`
	State       string            `json:"state"`
	Requester   string            `json:"requester_uid,omitempty"`
	Display     map[string]string `json:"display,omitempty"`
}

// Card-action protocol enums. These are octo-server's vocabulary, not ours.
const (
	cardDispositionApplied   = "applied"
	cardDispositionForbidden = "forbidden"
	cardDispositionConflict  = "conflict"
	cardDispositionNotFound  = "not_found"

	cardStatePending   = "pending"
	cardStateApproved  = "approved"
	cardStateDenied    = "denied"
	cardStateCancelled = "cancelled"
)

// cardState translates our review status into the card protocol's state enum.
// The vocabularies deliberately differ — we say rejected/canceled, the protocol
// says denied/cancelled — and emitting our spelling makes octo-server reject the
// whole response as an invalid enum, so never hand it string(status) directly.
func cardState(status model.ReviewStatus) string {
	switch status {
	case model.ReviewStatusApproved:
		return cardStateApproved
	case model.ReviewStatusRejected:
		return cardStateDenied
	case model.ReviewStatusCanceled:
		return cardStateCancelled
	default:
		return cardStatePending
	}
}

// DecideReviewFromCard is the IM-callback entry: idempotency, operator
// re-verification, and the decision itself.
//
// Every value it RETURNS is a handled outcome the transport answers with 200. A
// returned ERROR means "transient, ask octo-server to retry" — the caller turns
// it into a 5xx. Keeping that distinction is the whole reliability story: a
// forbidden/conflict/not_found body is acked and never redelivered, so nothing
// that might succeed later may be reported that way.
func (s *Service) DecideReviewFromCard(ctx context.Context, eventID, operatorUID, decision, reviewID string) (*CardActionResult, error) {
	eventID = strings.TrimSpace(eventID)
	operatorUID = strings.TrimSpace(operatorUID)
	reviewID = strings.TrimSpace(reviewID)
	decision = strings.TrimSpace(decision)
	if eventID == "" || operatorUID == "" || reviewID == "" || (decision != "approve" && decision != "deny") {
		return nil, ErrReviewInvalid
	}
	// Idempotency first: a redelivered event must replay the stored answer
	// verbatim rather than re-running the decision.
	if existing, err := s.repo.GetCardActionReceipt(ctx, eventID); err != nil {
		return nil, err
	} else if existing != nil {
		var out CardActionResult
		if err := json.Unmarshal([]byte(existing.StoredResponse), &out); err != nil {
			return nil, err
		}
		return &out, nil
	}
	// Load the request cross-scope to learn its Space. There is no tenant context
	// on this path — the review_id arrived inside a signed card payload — so the
	// operator is authorized against the Space this lookup returns, below.
	req, err := s.repo.GetReviewRequestAnySpace(ctx, reviewID)
	if err != nil {
		// A vanished request is terminal, not a server fault: report it in-band so
		// octo-server renders the card and stops redelivering instead of pushing the
		// event to the DLQ.
		if errors.Is(mapStoreError(err), ErrNotFound) {
			return &CardActionResult{Disposition: cardDispositionNotFound, State: cardStatePending}, nil
		}
		return nil, mapStoreError(err)
	}
	// octo-server's operator_uid is an identity assertion, not an authorization
	// grant: an admin who lost the role between card delivery and the click must
	// not be able to decide. A LOOKUP FAILURE IS NOT A REFUSAL — collapsing "the
	// role service is down" into forbidden returns a 200 that octo-server acks and
	// never redelivers, silently discarding a real admin's click.
	role, err := s.operatorRole(ctx, req.SpaceID, operatorUID)
	if err != nil {
		return nil, err
	}
	if role == nil || *role < SpaceRoleAdmin {
		return &CardActionResult{Disposition: cardDispositionForbidden, State: cardStatePending}, nil
	}
	if req.Status != model.ReviewStatusPending {
		return s.cardConflict(ctx, req), nil
	}

	adminScope := pluginrepo.Scope{CallerUID: operatorUID, SpaceID: req.SpaceID}
	var state string
	var applyErr error
	if decision == "approve" {
		_, applyErr = s.repo.ApproveReview(ctx, adminScope, pluginrepo.ApproveReviewParams{
			ReviewID:       reviewID,
			ReviewerUID:    operatorUID,
			ReviewerName:   operatorUID,
			DecisionSource: model.ReviewDecisionSourceIM,
		})
		state = cardStateApproved
	} else {
		applyErr = s.repo.RejectReview(ctx, adminScope, pluginrepo.RejectReviewParams{
			ReviewID:       reviewID,
			ReviewerUID:    operatorUID,
			ReviewerName:   operatorUID,
			Reason:         model.DefaultIMDenyReason,
			DecisionSource: model.ReviewDecisionSourceIM,
		})
		state = cardStateDenied
	}
	if applyErr != nil {
		// Losing the CAS race is an expected outcome, not an error: another admin
		// (or a web decision) already settled this request. Report the authoritative
		// terminal state so the card renders correctly, rather than returning 5xx
		// and having the event retried into the DLQ.
		mapped := mapStoreError(applyErr)
		if errors.Is(mapped, ErrConflict) || errors.Is(mapped, ErrNotFound) {
			return s.cardConflict(ctx, req), nil
		}
		return nil, applyErr
	}
	result := &CardActionResult{
		Disposition: cardDispositionApplied,
		State:       state,
		// Terminal states must carry requester_uid or octo-server's finalizer
		// cannot DM the applicant, and DecodeDecisionResult rejects the response.
		Requester: req.ApplicantUID,
		Display:   map[string]string{"title": req.PluginName},
	}
	body, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	if err := s.repo.InsertCardActionReceipt(ctx, &model.CardActionReceipt{
		EventID:        eventID,
		ReviewID:       reviewID,
		Decision:       decision,
		StoredResponse: string(body),
		CreatedAt:      s.now(),
	}); err != nil {
		// Two deliveries of the SAME event raced past the read above; the winner's
		// stored response is authoritative.
		if errors.Is(err, pluginrepo.ErrConflict) {
			if existing, _ := s.repo.GetCardActionReceipt(ctx, eventID); existing != nil {
				var out CardActionResult
				if json.Unmarshal([]byte(existing.StoredResponse), &out) == nil {
					return &out, nil
				}
			}
		}
		return nil, err
	}
	return result, nil
}

// cardConflict reports the authoritative terminal state of an already-decided
// request, re-reading it so the answer reflects the winner's outcome rather than
// the stale row this callback started from.
func (s *Service) cardConflict(ctx context.Context, req *model.PluginReviewRequest) *CardActionResult {
	settled := req
	if cur, err := s.repo.GetReviewRequestAnySpace(ctx, req.ID); err == nil && cur != nil {
		settled = cur
	}
	return &CardActionResult{
		Disposition: cardDispositionConflict,
		State:       cardState(settled.Status),
		Requester:   settled.ApplicantUID,
		Display:     map[string]string{"title": settled.PluginName},
	}
}

// operatorRole re-checks a role for the IM card-action path, which carries an
// asserted operator_uid instead of a resolved Caller. It returns (nil, nil) for
// "not a member" and an error for "could not find out" — the caller must keep
// those apart.
func (s *Service) operatorRole(ctx context.Context, spaceID, uid string) (*int, error) {
	if s.notify == nil || !s.notify.Enabled() {
		// Not a refusal: a deployment that mounted the callback without the internal
		// token cannot authorize anyone, and answering "forbidden" would burn the
		// event. Surface it as a fault so the operator sees it and a redelivery
		// after the fix still lands.
		return nil, errors.New("review: space role lookup is not configured")
	}
	return s.notify.MemberRole(ctx, spaceID, uid)
}

// --- helpers ------------------------------------------------------------------

// isReviewer reports whether the caller may act on the Space review queue.
//
// It reads Caller.SpaceRole, which the HTTP layer populates from the verified
// octo-server identity — never from request JSON, and deliberately never through
// the IM notifier: an earlier design resolved the web role through the
// notification wiring, so nobody could approve anything unless IM notification
// happened to be configured.
//
// adminCaller (internal/service/plugin/admin.go) builds a Caller with SpaceID=""
// and IsSystemAdmin=true for the cross-Space admin surface. That surface does not
// route here, but the system-admin short-circuit comes first so such a Caller can
// never be demoted to member by the empty-Space comparison.
func (s *Service) isReviewer(caller Caller) bool {
	return caller.IsSystemAdmin || caller.SpaceRole >= SpaceRoleAdmin
}

// decorateReview turns storage-side values into display values: the icon column
// holds an object key for uploaded icons, which is a 404 if handed to a browser.
// Resolved through the same path the plugin list uses.
func (s *Service) decorateReview(ctx context.Context, rr *model.PluginReviewRequest) {
	if rr == nil {
		return
	}
	rr.PluginIcon = s.resolveIcon(ctx, rr.PluginIcon)
}

// frozenReadmePaths are the primary documents, most specific first. A skill
// package carries SKILL.md; the other types have no mandated entry document, so
// the common conventions are tried before falling back to the manifest.
var frozenReadmePaths = []string{"SKILL.md", "README.md", "AGENTS.md"}

// frozenReadme extracts the reviewable body from the FROZEN package snapshot.
// Storage-backed attachments are not fetched: this runs on a read request, the
// object may be large, and the inline entry document is what a reviewer needs.
// Falling back to the manifest description keeps the field populated for
// connectors, which carry no markdown at all.
func frozenReadme(pkg, manifest json.RawMessage) string {
	for _, path := range frozenReadmePaths {
		if raw, ok := rawAttachmentContent(pkg, path); ok && strings.TrimSpace(raw) != "" {
			return raw
		}
	}
	return manifestDescription(manifest)
}
