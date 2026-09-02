package plugin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	"github.com/go-sql-driver/mysql"
)

// ReviewListFilter scopes review-queue queries.
type ReviewListFilter struct {
	SpaceID      string
	ApplicantUID string // set for mode=mine
	Status       model.ReviewStatus
	Limit        int
	Offset       int
}

// ApproveReviewParams carries reviewer metadata into the approve transaction.
type ApproveReviewParams struct {
	ReviewID       string
	ReviewerUID    string
	ReviewerName   string
	DecisionSource model.ReviewDecisionSource
	RequestID      string
	Now            time.Time
}

// RejectReviewParams carries reviewer metadata into the reject transaction.
type RejectReviewParams struct {
	ReviewID       string
	ReviewerUID    string
	ReviewerName   string
	Reason         string
	DecisionSource model.ReviewDecisionSource
	RequestID      string
	Now            time.Time
}

// FrozenSnapshot is the content captured at submit time and stored on the
// request row so a reviewer approves exactly what will ship.
//
// Relations is part of the snapshot, not an afterthought: for expert /
// expert_team the membership graph IS the reviewable content, and freezing only
// the documents would ship the reviewed manifest alongside whatever the live
// membership happened to be when the reviewer clicked approve.
//
// AttachmentKeys is the frozen storage sidecar. Zip-submitted skill upgrades
// spill binary/oversize files at submit time to content-addressed keys that
// the live row does not reference; freezing the sidecar alongside the package
// means approve can apply the snapshot atomically without depending on the
// live sidecar still being correct.
type FrozenSnapshot struct {
	Manifest       json.RawMessage
	Package        json.RawMessage
	AttachmentKeys json.RawMessage
	Relations      []model.PluginRelation
	ManifestHash   string
	PluginHash     string
}

// InsertReviewRequest persists a new pending request with its frozen snapshot.
// Kind and the label-collision check are derived server-side inside the same
// transaction that takes the single-pending lock.
func (r *Repo) InsertReviewRequest(ctx context.Context, scope Scope, req *model.PluginReviewRequest, snap FrozenSnapshot) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Submit IS owner-only, so the owner-scoped lock is correct here (unlike the
	// reviewer transactions below, where the actor is by definition not the owner).
	current, err := getOwnedForUpdate(ctx, tx, scope, req.PluginID)
	if err != nil {
		return err
	}
	// "First listing" means the plugin is not yet listed — which is exactly
	// listing_state != published, and nothing else.
	//
	// This used to test `visibility == private`, which was the same thing back when
	// private doubled as the draft state. It no longer is: an author now declares
	// `space` visibility on the draft itself, so a first listing arrives here
	// already carrying visibility=space. Keeping the old test would classify every
	// first listing as an upgrade, the isFirst branch of ApproveReview would never
	// run, and the plugin would reach `approved` with a minted version and stay
	// invisible forever with no error anywhere.
	//
	// It still deliberately does NOT look at current_version_id. A draft normally
	// HAS one: the import path snapshots a version as part of the upload. Gating on
	// "has no version" reintroduces the same silent failure, and a fixture built
	// through the plugin-write API hides it because that path also leaves
	// current_version_id NULL.
	if current.ListingState != model.PluginListingStatePublished {
		req.Kind = model.ReviewKindFirst
	} else {
		req.Kind = model.ReviewKindUpgrade
	}
	published, err := publishedVersionLabels(ctx, tx, req.PluginID, current)
	if err != nil {
		return err
	}
	if _, taken := published[req.Version]; taken {
		return ErrConflict
	}
	// Take the single-pending lock in the same transaction as the label check so
	// two concurrent submits cannot both pass. The UNIQUE index on the generated
	// pending_plugin_id column is the hard guarantee; this read gives the loser a
	// typed ErrConflict instead of a raw 1062.
	var pendingID string
	if err := tx.QueryRowContext(ctx,
		`SELECT review_id FROM plugin_review_requests
		  WHERE plugin_id=? AND status='pending' AND deleted_at IS NULL LIMIT 1 FOR UPDATE`,
		req.PluginID).Scan(&pendingID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if pendingID != "" {
		return ErrConflict
	}

	relations := snap.Relations
	if relations == nil {
		relations = []model.PluginRelation{}
	}
	relationsJSON, err := json.Marshal(relations)
	if err != nil {
		return err
	}

	now := r.now()
	req.ID = r.id()
	req.SubmittedAt = now
	req.CreatedAt = now
	req.UpdatedAt = now
	targetScope := req.TargetScope
	if targetScope == "" {
		targetScope = "space"
	}
	var changelog any
	if req.Changelog != nil && *req.Changelog != "" {
		changelog = *req.Changelog
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO plugin_review_requests
		  (review_id,plugin_id,space_id,target_scope,status,kind,version,changelog,
		   manifest_json,plugin_json,attachment_keys_json,relations_json,manifest_hash,plugin_hash,
		   applicant_uid,applicant_name,submitted_at,created_at,updated_at)
		  VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		req.ID, req.PluginID, req.SpaceID, targetScope, string(model.ReviewStatusPending), string(req.Kind), req.Version, changelog,
		string(snap.Manifest), string(snap.Package), jsonColumn(snap.AttachmentKeys), string(relationsJSON), snap.ManifestHash, snap.PluginHash,
		req.ApplicantUID, req.ApplicantName, req.SubmittedAt, req.CreatedAt, req.UpdatedAt,
	)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return ErrConflict
		}
		return err
	}
	return tx.Commit()
}

// publishedVersionLabels reconstructs the set of version labels this plugin has
// already published to the org.
//
// It cannot come from plugin_versions: that table's `version` column is a
// per-plugin auto-increment counter ("1", "2", ...) written by snapshotVersion,
// not the applicant-typed semver label. The authoritative record of published
// labels is therefore the review table itself — every APPROVED request — plus
// the plugin's own current_version when the plugin is already listed, which
// covers rows that were Space-visible before this feature existed (no
// grandfathering backfill).
//
// A DRAFT's current_version is a draft label, not a published one, so it is
// deliberately excluded: a first listing at the import default "1.0.0" is the
// normal case and must not be refused. A DELISTED plugin's current_version is
// included — the org already saw that label, so a republish must not reuse it.
// Labels from rejected/canceled requests are likewise free to reuse — that is the
// whole point of keeping the snapshot off plugin_versions.
func publishedVersionLabels(ctx context.Context, tx *sql.Tx, pluginID string, current *model.Plugin) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx,
		`SELECT version FROM plugin_review_requests
		  WHERE plugin_id=? AND status='approved' AND deleted_at IS NULL`, pluginID)
	if err != nil {
		return nil, wrapped("load published labels", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if current != nil && current.ListingState != model.PluginListingStateDraft &&
		current.CurrentVersion != nil && *current.CurrentVersion != "" {
		out[*current.CurrentVersion] = struct{}{}
	}
	return out, nil
}

const reviewSelectBase = `SELECT rr.review_id,rr.plugin_id,rr.space_id,rr.target_scope,rr.status,rr.kind,rr.version,rr.changelog,
        rr.manifest_hash,rr.plugin_hash,
        rr.applicant_uid,rr.applicant_name,rr.reviewer_uid,rr.reviewer_name,rr.reason,rr.decision_source,
        rr.submitted_at,rr.reviewed_at,
        p.plugin_name,p.plugin_type,p.icon,p.current_version`

// reviewSelectSnapshot adds the large frozen-snapshot columns. Only the
// detail read uses it; the list query must never carry a manifest, package and
// relation graph per row.
const reviewSelectSnapshot = `SELECT rr.review_id,rr.plugin_id,rr.space_id,rr.target_scope,rr.status,rr.kind,rr.version,rr.changelog,
        rr.manifest_json,rr.plugin_json,rr.attachment_keys_json,rr.relations_json,rr.manifest_hash,rr.plugin_hash,
        rr.applicant_uid,rr.applicant_name,rr.reviewer_uid,rr.reviewer_name,rr.reason,rr.decision_source,
        rr.submitted_at,rr.reviewed_at,
        p.plugin_name,p.plugin_type,p.icon,p.current_version`

// reviewWhereScope builds the tenant scope predicate. space_id is ALWAYS
// constrained — an applicant who belongs to several Spaces must not see their
// own request from Space B while acting in Space A (CLAUDE.md: tenant-owned
// queries carry a scope predicate even after middleware). isReviewer widens the
// row set from "my requests" to "every request in this Space", never across it.
func reviewWhereScope(spaceID, callerUID string, isReviewer bool) (string, []any) {
	if isReviewer {
		return `rr.space_id=?`, []any{spaceID}
	}
	return `rr.space_id=? AND rr.applicant_uid=?`, []any{spaceID, callerUID}
}

func (r *Repo) GetReviewRequest(ctx context.Context, scope Scope, reviewID string, isReviewer bool) (*model.PluginReviewRequest, error) {
	where, args := reviewWhereScope(scope.SpaceID, scope.CallerUID, isReviewer)
	q := reviewSelectBase + ` FROM plugin_review_requests rr JOIN plugins p ON p.plugin_id=rr.plugin_id WHERE rr.review_id=? AND rr.deleted_at IS NULL AND ` + where
	args = append([]any{reviewID}, args...)
	row := r.db.QueryRowContext(ctx, q, args...)
	return scanReviewRequest(row.Scan, false)
}

// LoadReviewSnapshot is the detail read: same scoping as GetReviewRequest plus
// the frozen manifest/package/relations the caller renders a preview from.
func (r *Repo) LoadReviewSnapshot(ctx context.Context, scope Scope, reviewID string, isReviewer bool) (*model.PluginReviewRequest, error) {
	where, args := reviewWhereScope(scope.SpaceID, scope.CallerUID, isReviewer)
	q := reviewSelectSnapshot + ` FROM plugin_review_requests rr JOIN plugins p ON p.plugin_id=rr.plugin_id WHERE rr.review_id=? AND rr.deleted_at IS NULL AND ` + where
	args = append([]any{reviewID}, args...)
	row := r.db.QueryRowContext(ctx, q, args...)
	return scanReviewRequest(row.Scan, true)
}

// GetReviewRequestAnySpace loads a request without a tenant scope predicate.
// It exists solely for the IM card-action callback, which arrives with no
// authenticated tenant context — only a signed review_id and an asserted
// operator_uid. The caller MUST re-authorize the operator against the returned
// SpaceID before acting on the request; this method performs no authorization of
// its own and must never back a client-facing read.
func (r *Repo) GetReviewRequestAnySpace(ctx context.Context, reviewID string) (*model.PluginReviewRequest, error) {
	q := reviewSelectBase + ` FROM plugin_review_requests rr JOIN plugins p ON p.plugin_id=rr.plugin_id WHERE rr.review_id=? AND rr.deleted_at IS NULL`
	row := r.db.QueryRowContext(ctx, q, reviewID)
	return scanReviewRequest(row.Scan, false)
}

// HasPendingReview reports whether an open review request exists for a plugin the
// caller owns. It is a cheap pre-check for write paths that must not contradict a
// pending decision; the authoritative single-pending guarantee remains the UNIQUE
// index on the generated pending_plugin_id column, taken inside the submit
// transaction.
//
// Scoped through the plugin, not just the request: the plugin_id alone would let
// any caller probe whether an arbitrary plugin has a review open.
func (r *Repo) HasPendingReview(ctx context.Context, scope Scope, pluginID string) (bool, error) {
	var found string
	err := r.db.QueryRowContext(ctx,
		`SELECT rr.review_id FROM plugin_review_requests rr
		   JOIN plugins p ON p.plugin_id=rr.plugin_id
		  WHERE rr.plugin_id=? AND rr.status='pending' AND rr.deleted_at IS NULL
		    AND p.owner_uid=? AND p.space_id=? AND p.deleted_at IS NULL
		  LIMIT 1`,
		pluginID, scope.CallerUID, scope.SpaceID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, wrapped("check pending review", err)
	}
	return true, nil
}

func (r *Repo) ListReviewRequests(ctx context.Context, scope Scope, f ReviewListFilter) ([]*model.PluginReviewRequest, int64, error) {
	var where strings.Builder
	var args []any
	where.WriteString(`rr.deleted_at IS NULL `)
	// Space scope is unconditional; ApplicantUID (mode=mine) narrows within it
	// rather than replacing it.
	if f.SpaceID != "" {
		where.WriteString(`AND rr.space_id=? `)
		args = append(args, f.SpaceID)
	}
	if f.ApplicantUID != "" {
		where.WriteString(`AND rr.applicant_uid=? `)
		args = append(args, f.ApplicantUID)
	}
	if f.Status != "" {
		where.WriteString(`AND rr.status=? `)
		args = append(args, string(f.Status))
	}
	var total int64
	if err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM plugin_review_requests rr WHERE `+where.String(), args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	offset := f.Offset
	if offset < 0 {
		offset = 0
	}
	q := reviewSelectBase + ` FROM plugin_review_requests rr JOIN plugins p ON p.plugin_id=rr.plugin_id WHERE ` + where.String() +
		` ORDER BY rr.submitted_at DESC, rr.review_id DESC LIMIT ? OFFSET ?`
	args = append(args, limit, offset)
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*model.PluginReviewRequest{}
	for rows.Next() {
		rr, err := scanReviewRequest(rows.Scan, false)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, rr)
	}
	return out, total, rows.Err()
}

// ApproveReview applies the frozen snapshot to the live plugin and settles the
// request, in one transaction.
//
// It reuses main's write-path helpers (lockRelationTargets, syncRelations,
// snapshotVersion, insertAudit) rather than a parallel implementation, so the
// version/audit bookkeeping stays in one place. What it deliberately does NOT do
// is write the applicant's semver label into plugin_versions.version: that column
// is a per-plugin counter maintained by snapshotVersion. The label lands on
// plugins.current_version, which is what snapshotVersion already writes.
//
// The placement is never HIDDEN — the gate that keeps a draft out of the market
// is visibility alone, because the author's own "我的插件" list INNER JOINs the
// placement on visible=1 and a hidden placement would hide the draft from the
// person who created it. But approval does self-heal the placement FORWARD: it
// makes the default placement exist and be visible, so a legacy row that lacks
// one (or carries a publish-era visible=0) is actually listed by the approval
// that promised to list it. See ensureVisibleDefaultPlacement.
func (r *Repo) ApproveReview(ctx context.Context, scope Scope, p ApproveReviewParams) (*model.Plugin, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		pluginID, version, kind               string
		changelog                             sql.NullString
		manifest, pkg, attKeys, relationBytes []byte
		manifestHash, pluginHash              string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT rr.plugin_id,rr.version,rr.kind,rr.changelog,rr.manifest_json,rr.plugin_json,rr.attachment_keys_json,rr.relations_json,
		        rr.manifest_hash,rr.plugin_hash
		   FROM plugin_review_requests rr
		  WHERE rr.review_id=? AND rr.status='pending' AND rr.deleted_at IS NULL
		    AND rr.space_id=?
		  FOR UPDATE`, p.ReviewID, scope.SpaceID).
		Scan(&pluginID, &version, &kind, &changelog, &manifest, &pkg, &attKeys, &relationBytes, &manifestHash, &pluginHash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, classifyMissingPending(ctx, tx, p.ReviewID, scope.SpaceID)
		}
		return nil, err
	}
	current, err := getReviewedPluginForUpdate(ctx, tx, scope.SpaceID, pluginID)
	if err != nil {
		return nil, err
	}
	now := p.Now
	if now.IsZero() {
		now = r.now()
	}

	var frozen []model.PluginRelation
	if len(relationBytes) > 0 {
		if err := json.Unmarshal(relationBytes, &frozen); err != nil {
			return nil, wrapped("decode frozen relations", err)
		}
	}
	live, err := liveRelationTargetSet(ctx, tx, pluginID)
	if err != nil {
		return nil, err
	}
	if frozen, err = reconcileFrozenRelations(ctx, tx, pluginID, frozen, now); err != nil {
		return nil, err
	}
	// Re-lock the frozen edges' targets under the PLUGIN OWNER's scope, not the
	// reviewer's: an expert's bundled skills are private rows owned by the
	// applicant, and resolving them as the reviewer would 404 every container
	// approval. The reviewer's authority over this Space is established before we
	// get here; `live` exempts targets this source already owns from the
	// embedded-adoption guard, exactly as Update does.
	ownerScope := Scope{CallerUID: current.OwnerUID, SpaceID: scope.SpaceID}
	if err := lockRelationTargets(ctx, tx, ownerScope, current.Type, frozen, live); err != nil {
		return nil, err
	}

	// A first listing is what turns a draft into a listed plugin, so it stamps
	// listing_state alongside the frozen content. visibility is stamped too, but
	// only as a defensive normalization: under the current model the author already
	// declared `space` on the draft, so this is a no-op for every request submitted
	// after listing_state shipped. It stays because a request that was already
	// pending across the upgrade may still carry the old private-draft shape.
	isFirst := model.ReviewKind(kind) == model.ReviewKindFirst && current.ListingState != model.PluginListingStatePublished
	if isFirst {
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugins SET manifest_json=?,plugin_json=?,attachment_keys_json=?,manifest_hash=?,plugin_hash=?,visibility=?,listing_state=?,updated_at=?
			  WHERE plugin_id=? AND space_id=? AND deleted_at IS NULL`,
			string(manifest), string(pkg), jsonColumn(attKeys), manifestHash, pluginHash,
			string(model.PluginVisibilitySpace), string(model.PluginListingStatePublished), now, pluginID, scope.SpaceID); err != nil {
			return nil, wrapped("apply approved snapshot", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugins SET manifest_json=?,plugin_json=?,attachment_keys_json=?,manifest_hash=?,plugin_hash=?,updated_at=?
			  WHERE plugin_id=? AND space_id=? AND deleted_at IS NULL`,
			string(manifest), string(pkg), jsonColumn(attKeys), manifestHash, pluginHash, now, pluginID, scope.SpaceID); err != nil {
			return nil, wrapped("apply approved snapshot", err)
		}
	}
	// Approval is the only moment that promises "this plugin is in the market now",
	// and the market list needs BOTH the visibility predicate and a `visible=1`
	// default placement. Flipping visibility alone lists nothing for a row whose
	// placement is missing (pre-auto-placement legacy) or hidden (publish-era
	// visible=0), and the author cannot repair it afterwards because a listed
	// plugin's ordinary write path is 409 listed_requires_review. So self-heal the
	// placement here, in the same transaction as the status/visibility swap. It is
	// a no-op for the common already-visible case, and it never hides anything.
	if err := ensureVisibleDefaultPlacement(ctx, tx, r.id, now, pluginID, current.CategoryID); err != nil {
		return nil, err
	}
	if _, err := syncRelations(ctx, tx, r.id, now, pluginID, current.OwnerUID, frozen); err != nil {
		return nil, err
	}
	if isFirst {
		// An expert's bundled skills (and a squad's member experts and their
		// skills) are embedded rows that inherited the container's `private`
		// visibility. Listing the container without them leaves it uninstallable by
		// anyone but its author: resolveInstallDetail refuses an install whose
		// declared relation count exceeds the targets the caller can see. The child
		// set is derived AFTER the relations are synced so it reflects the frozen
		// topology, and under the same transaction's locks.
		if err := promoteEmbeddedChildren(ctx, tx, pluginID, current.Type, scope.SpaceID, now); err != nil {
			return nil, err
		}
	}

	// Mint the release snapshot through main's own helper: plugin_versions.version
	// stays the auto-increment counter, and current_version takes the applicant's
	// label. The snapshot carries the FROZEN attachment sidecar (zip-submitted
	// skill upgrades spill files at submit time whose keys only exist on the
	// request), so version history resolves correctly for every approved snapshot
	// rather than inheriting the live row's stale keys.
	snapshotPlugin := *current
	snapshotPlugin.Manifest = manifest
	snapshotPlugin.Package = pkg
	snapshotPlugin.AttachmentKeys = attKeys
	snapshotPlugin.ManifestHash = manifestHash
	snapshotPlugin.PluginHash = pluginHash
	snapshotPlugin.CurrentVersion = &version
	var changelogPtr *string
	if changelog.Valid && changelog.String != "" {
		s := changelog.String
		changelogPtr = &s
	}
	if _, err := snapshotVersion(ctx, tx, r.id, now, snapshotPlugin, frozen, changelogPtr, p.ReviewerUID); err != nil {
		return nil, err
	}

	ds := string(p.DecisionSource)
	if ds == "" {
		ds = string(model.ReviewDecisionSourceWeb)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE plugin_review_requests SET status='approved',reviewer_uid=?,reviewer_name=?,decision_source=?,reviewed_at=?,updated_at=?
		  WHERE review_id=?`,
		p.ReviewerUID, p.ReviewerName, ds, now, now, p.ReviewID)
	if err != nil {
		return nil, err
	}
	if err := mustAffect(res); err != nil {
		return nil, err
	}
	m := Mutation{OperatorID: p.ReviewerUID, OperatorName: p.ReviewerName, RequestID: p.RequestID, Remark: strPtr("decision_source=" + ds)}
	if err := insertAudit(ctx, tx, r.id(), now, snapshotPlugin, "review_approve", m, current.PluginHash, pluginHash); err != nil {
		return nil, err
	}
	refreshed, err := getReviewedPluginForUpdate(ctx, tx, scope.SpaceID, pluginID)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return refreshed, nil
}

// reconcileFrozenRelations makes the frozen edge set applicable by syncRelations,
// which rejects a non-empty relation_id that no longer matches a live edge.
// Between submit and approve the author may have deleted an edge the snapshot
// still names; that must re-create the frozen edge, not fail the approval. Any
// id that is no longer live is cleared so it inserts fresh; ids that survived
// keep their identity so a client's relation_id stays stable across approval.
func reconcileFrozenRelations(ctx context.Context, tx *sql.Tx, pluginID string, frozen []model.PluginRelation, now interface{}) ([]model.PluginRelation, error) {
	if len(frozen) == 0 {
		return frozen, nil
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT relation_id FROM plugin_relations WHERE source_plugin_id=? AND deleted_at IS NULL`, pluginID)
	if err != nil {
		return nil, wrapped("load live relation ids", err)
	}
	defer rows.Close()
	liveIDs := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		liveIDs[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]model.PluginRelation, len(frozen))
	copy(out, frozen)
	for i := range out {
		if _, ok := liveIDs[out[i].ID]; !ok {
			out[i].ID = ""
		}
		out[i].SourcePluginID = pluginID
		if out[i].Status == 0 {
			out[i].Status = 1
		}
		// A nil Data round-trips through JSON as the four bytes `null`, because
		// json.RawMessage implements Unmarshaler and the decoder hands "null" to it
		// verbatim. Left alone it reaches relation_json as the literal `null`, which
		// chk_plugin_relations_json_object rejects — so every container approval fails
		// on a relation that carries no data at all.
		if isJSONNullOrEmpty(out[i].Data) {
			out[i].Data = nil
		}
	}
	return out, nil
}

func isJSONNullOrEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// promoteEmbeddedChildren lists an approved container's embedded rows alongside
// the top. It is deliberately narrow: only is_embedded rows in the same Space
// that are not already listed, so a standalone catalog skill merely referenced by
// the container is never touched.
//
// listing_state must travel with visibility here. A child left unpublished under
// a published top is invisible to everyone but the owner, so resolveInstallDetail
// resolves fewer visible relation targets than CountDeclaredRelations and every
// non-owner install of the container fails with ErrDependencyHidden — silent until
// somebody other than the author tries to install it.
func promoteEmbeddedChildren(ctx context.Context, tx *sql.Tx, topID string, topType model.PluginType, spaceID string, now interface{}) error {
	childIDs, err := collectEmbeddedChildren(ctx, tx, topID, topType)
	if err != nil {
		return err
	}
	for _, id := range childIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE plugins SET visibility=?,listing_state=?,updated_at=?
			  WHERE plugin_id=? AND space_id=? AND is_embedded=1 AND listing_state<>? AND deleted_at IS NULL`,
			string(model.PluginVisibilitySpace), string(model.PluginListingStatePublished), now,
			id, spaceID, string(model.PluginListingStatePublished)); err != nil {
			return wrapped("promote embedded child", err)
		}
	}
	return nil
}

// getReviewedPluginForUpdate locks the plugin a review request targets.
//
// Deliberately NOT getOwnedForUpdate: that requires owner_uid = scope.CallerUID,
// but a reviewer is by definition not the applicant. Using the owner-scoped
// lookup here makes every approval by a real Space admin fail with ErrNotFound —
// it only appears to work when the same account happens to be both owner and
// admin. A reviewer's authority is Space-scoped, so the predicate is space_id,
// and the caller has already established the reviewer role for that Space.
func getReviewedPluginForUpdate(ctx context.Context, tx *sql.Tx, spaceID, pluginID string) (*model.Plugin, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+pluginColumns+` FROM plugins p
		  WHERE p.plugin_id=? AND p.space_id=? AND p.status=1 AND p.deleted_at IS NULL FOR UPDATE`,
		pluginID, spaceID)
	p, err := scanPlugin(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return p, err
}

// classifyMissingPending explains why a `status='pending' FOR UPDATE` lock read
// matched nothing. Callers need to tell two very different situations apart:
//
//   - the request exists but someone already decided it — the caller lost a race
//     and must surface the winner's outcome (ErrConflict). This is the normal
//     result of two admins clicking at once, or of an IM card click arriving
//     after a web decision.
//   - the request genuinely is not visible to this Space — ErrNotFound, which the
//     API turns into a 404 that does not confirm the id exists.
//
// The lock read reads the latest committed row (locking reads bypass the
// transaction snapshot), so by the time it misses, the winner has committed and
// this follow-up read inside the same transaction sees the terminal status.
func classifyMissingPending(ctx context.Context, tx *sql.Tx, reviewID, spaceID string) error {
	var status string
	err := tx.QueryRowContext(ctx,
		`SELECT status FROM plugin_review_requests
		  WHERE review_id=? AND space_id=? AND deleted_at IS NULL`, reviewID, spaceID).Scan(&status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if model.ReviewStatus(status) == model.ReviewStatusPending {
		// Still pending but the scoped lock read missed it: the only way this
		// happens is a Space mismatch already excluded above, so treat it as absent
		// rather than inventing a conflict.
		return ErrNotFound
	}
	return ErrConflict
}

// RejectReview settles a pending request without touching the plugin: a private
// draft stays private, an already-listed version stays live. Returns the frozen
// attachment sidecar and the live plugin's sidecar so the caller can clean up
// any objects the submission uploaded that the live row does not reference.
func (r *Repo) RejectReview(ctx context.Context, scope Scope, p RejectReviewParams) (json.RawMessage, json.RawMessage, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var pluginID string
	var frozenKeys []byte
	err = tx.QueryRowContext(ctx,
		`SELECT rr.plugin_id, rr.attachment_keys_json FROM plugin_review_requests rr
		  WHERE rr.review_id=? AND rr.status='pending' AND rr.deleted_at IS NULL
		    AND rr.space_id=?
		  FOR UPDATE`, p.ReviewID, scope.SpaceID).Scan(&pluginID, &frozenKeys)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, classifyMissingPending(ctx, tx, p.ReviewID, scope.SpaceID)
		}
		return nil, nil, err
	}
	current, err := getReviewedPluginForUpdate(ctx, tx, scope.SpaceID, pluginID)
	if err != nil {
		return nil, nil, err
	}
	now := p.Now
	if now.IsZero() {
		now = r.now()
	}
	ds := string(p.DecisionSource)
	if ds == "" {
		ds = string(model.ReviewDecisionSourceWeb)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE plugin_review_requests SET status='rejected',reviewer_uid=?,reviewer_name=?,reason=?,decision_source=?,reviewed_at=?,updated_at=?
		  WHERE review_id=?`,
		p.ReviewerUID, p.ReviewerName, p.Reason, ds, now, now, p.ReviewID)
	if err != nil {
		return nil, nil, err
	}
	if err := mustAffect(res); err != nil {
		return nil, nil, err
	}
	m := Mutation{OperatorID: p.ReviewerUID, OperatorName: p.ReviewerName, RequestID: p.RequestID, Remark: strPtr("decision_source=" + ds)}
	// before == after: a reject changes nothing about the plugin content, and the
	// audit row exists to record who refused it and from where.
	if err := insertAudit(ctx, tx, r.id(), now, *current, "review_reject", m, current.PluginHash, current.PluginHash); err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return frozenKeys, current.AttachmentKeys, nil
}

// CancelReview withdraws the applicant's own pending request.
//
// It distinguishes "already decided" (ErrConflict) from "not yours / no such
// request" (ErrNotFound). Collapsing the two into a 404 tells an applicant whose
// request was just approved that it vanished. Returns the frozen attachment
// sidecar and the live plugin's sidecar so the caller can clean up objects the
// submission uploaded that the live row does not reference.
func (r *Repo) CancelReview(ctx context.Context, scope Scope, reviewID, callerUID string) (json.RawMessage, json.RawMessage, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback()
	var status string
	var frozenKeys []byte
	var pluginID string
	err = tx.QueryRowContext(ctx,
		`SELECT status, attachment_keys_json, plugin_id FROM plugin_review_requests
		  WHERE review_id=? AND space_id=? AND applicant_uid=? AND deleted_at IS NULL FOR UPDATE`,
		reviewID, scope.SpaceID, callerUID).Scan(&status, &frozenKeys, &pluginID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, err
	}
	if model.ReviewStatus(status) != model.ReviewStatusPending {
		return nil, nil, ErrConflict
	}
	// Load the live plugin so we can return its sidecar for key-diff cleanup.
	// The plugin is NOT locked for update here (unlike reject/approve) because
	// cancel does not modify the plugin row — we only need its current
	// attachment_keys_json to know which frozen keys are still live.
	current, err := getReviewedPluginForUpdate(ctx, tx, scope.SpaceID, pluginID)
	if err != nil {
		return nil, nil, err
	}
	now := r.now()
	res, err := tx.ExecContext(ctx,
		`UPDATE plugin_review_requests SET status='canceled',reviewed_at=?,updated_at=?
		  WHERE review_id=? AND applicant_uid=? AND status='pending' AND deleted_at IS NULL AND space_id=?`,
		now, now, reviewID, callerUID, scope.SpaceID)
	if err != nil {
		return nil, nil, err
	}
	if err := mustAffect(res); err != nil {
		return nil, nil, ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, err
	}
	return frozenKeys, current.AttachmentKeys, nil
}

// --- Card-action receipts ---------------------------------------------------

func (r *Repo) GetCardActionReceipt(ctx context.Context, eventID string) (*model.CardActionReceipt, error) {
	var out model.CardActionReceipt
	err := r.db.QueryRowContext(ctx,
		`SELECT event_id,stored_response,review_id,decision,created_at FROM plugin_card_action_receipts WHERE event_id=?`,
		eventID).Scan(&out.EventID, &out.StoredResponse, &out.ReviewID, &out.Decision, &out.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

func (r *Repo) InsertCardActionReceipt(ctx context.Context, rec *model.CardActionReceipt) error {
	now := rec.CreatedAt
	if now.IsZero() {
		now = r.now()
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO plugin_card_action_receipts (event_id,review_id,decision,stored_response,created_at) VALUES (?,?,?,?,?)`,
		rec.EventID, rec.ReviewID, rec.Decision, rec.StoredResponse, now)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return ErrConflict
		}
		return err
	}
	return nil
}

// --- scanning --------------------------------------------------------------

func scanReviewRequest(scan func(dest ...any) error, withSnapshot bool) (*model.PluginReviewRequest, error) {
	rr := &model.PluginReviewRequest{}
	var (
		targetScope, st, knd              string
		cl, rvu, rvn, rsn, ds             sql.NullString
		rat                               sql.NullTime
		pn, pt, pi, cv                    sql.NullString
		mh, ph                            string
		au, an                            string
		sa                                time.Time
		manifest, pkg, attKeys, relations []byte
	)
	base := []any{&rr.ID, &rr.PluginID, &rr.SpaceID, &targetScope, &st, &knd, &rr.Version, &cl}
	if withSnapshot {
		base = append(base, &manifest, &pkg, &attKeys, &relations)
	}
	base = append(base, &mh, &ph, &au, &an, &rvu, &rvn, &rsn, &ds, &sa, &rat, &pn, &pt, &pi, &cv)
	if err := scan(base...); err != nil {
		// A miss must read as "not found", never as an internal error: the scoped
		// queries fold "wrong Space" and "no such id" into the same empty result so
		// a cross-Space probe cannot tell the two apart (CLAUDE.md: cross-Space
		// failures must not leak resource existence).
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rr.TargetScope = targetScope
	rr.Status = model.ReviewStatus(st)
	rr.Kind = model.ReviewKind(knd)
	rr.ManifestHash = mh
	rr.PluginHash = ph
	rr.ApplicantUID = au
	rr.ApplicantName = an
	rr.SubmittedAt = sa
	if withSnapshot {
		rr.ManifestJSON = manifest
		rr.PluginJSON = pkg
		rr.AttachmentKeys = attKeys
		rr.RelationsJSON = relations
	}
	if cl.Valid {
		s := cl.String
		rr.Changelog = &s
	}
	if rvu.Valid {
		rr.ReviewerUID = &rvu.String
	}
	if rvn.Valid {
		rr.ReviewerName = &rvn.String
	}
	if rsn.Valid {
		rr.Reason = &rsn.String
	}
	if ds.Valid {
		s := model.ReviewDecisionSource(ds.String)
		rr.DecisionSource = &s
	}
	if rat.Valid {
		t := rat.Time
		rr.ReviewedAt = &t
	}
	if pn.Valid {
		rr.PluginName = pn.String
	}
	if pt.Valid {
		rr.PluginType = model.PluginType(pt.String)
	}
	if pi.Valid {
		rr.PluginIcon = pi.String
	}
	if cv.Valid {
		rr.CurrentVersion = &cv.String
	}
	return rr, nil
}

func strPtr(s string) *string { return &s }
