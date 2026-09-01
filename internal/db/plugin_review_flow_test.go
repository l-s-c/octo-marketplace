package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	migrate "github.com/rubenv/sql-migrate"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
	migrationsql "github.com/Mininglamp-OSS/octo-marketplace/migrations/sql"
)

// These tests drive the REAL repository against a real MySQL. The review
// workflow's load-bearing behaviour is transactional (the single-pending lock,
// the status CAS, the frozen-snapshot application) and a fake store cannot
// express any of it.

func reviewDB(t *testing.T) *sql.DB {
	t.Helper()
	database := isolatedTestDB(t)
	source := &migrate.EmbedFileSystemMigrationSource{FileSystem: migrationsql.FS, Root: "."}
	if _, err := migrate.Exec(database, "mysql", source, migrate.Up); err != nil {
		t.Fatalf("migrate Up: %v", err)
	}
	return database
}

type seedPlugin struct {
	id             string
	typ            string
	visibility     string
	currentVersion string
	// currentVersionID mimics the state a real import leaves behind: a private
	// draft that ALREADY carries a version snapshot.
	currentVersionID string
	embedded         bool
	owner            string
	space            string
	manifest         string
	pkg              string
}

func seed(t *testing.T, database *sql.DB, p seedPlugin) {
	t.Helper()
	if p.owner == "" {
		p.owner = "user-1"
	}
	if p.space == "" {
		p.space = "space-a"
	}
	if p.typ == "" {
		p.typ = "skill"
	}
	if p.manifest == "" {
		p.manifest = `{"plugin_name":"Fixture"}`
	}
	if p.pkg == "" {
		p.pkg = `{"attachments":[]}`
	}
	var versionID any
	if p.currentVersionID != "" {
		versionID = p.currentVersionID
	}
	var current any
	if p.currentVersion != "" {
		current = p.currentVersion
	}
	if _, err := database.Exec(`INSERT INTO plugins
		(plugin_id, plugin_name, plugin_type, is_embedded, tags_json, owner_uid, space_id, visibility,
		 manifest_json, plugin_json, manifest_hash, plugin_hash, current_version_id, current_version,
		 created_at, updated_at)
		VALUES (?, 'Fixture', ?, ?, JSON_ARRAY(), ?, ?, ?, CAST(? AS JSON), CAST(? AS JSON),
		        REPEAT('a',71), REPEAT('b',71), ?, ?, NOW(3), NOW(3))`,
		p.id, p.typ, p.embedded, p.owner, p.space, p.visibility, p.manifest, p.pkg, versionID, current); err != nil {
		t.Fatalf("seed %s: %v", p.id, err)
	}
	// Every create attaches a visible default placement; the review gate is
	// visibility only, so the fixture must carry one too.
	if _, err := database.Exec(`INSERT INTO plugin_placements
		(placement_id, placement_code, plugin_id, visible, sort_order, created_at, updated_at)
		VALUES (CONCAT('pl-', ?), 'default', ?, 1, 0, NOW(3), NOW(3))`, p.id, p.id); err != nil {
		t.Fatalf("seed placement %s: %v", p.id, err)
	}
}

func seedRelation(t *testing.T, database *sql.DB, id, source, target, typ string) {
	t.Helper()
	if _, err := database.Exec(`INSERT INTO plugin_relations
		(relation_id, source_plugin_id, target_plugin_id, relation_type, sort_order, status, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, 0, 1, 'user-1', NOW(3), NOW(3))`, id, source, target, typ); err != nil {
		t.Fatalf("seed relation %s: %v", id, err)
	}
}

func tenantScope() pluginrepo.Scope {
	return pluginrepo.Scope{CallerUID: "user-1", SpaceID: "space-a"}
}

// reviewerScope is a DIFFERENT uid in the same Space. Running the decision
// transactions as the applicant hides every owner-vs-reviewer scoping bug: the
// approve path must lock the plugin by Space, not by owner.
func reviewerScope() pluginrepo.Scope {
	return pluginrepo.Scope{CallerUID: "admin-1", SpaceID: "space-a"}
}

func snapshotOf(manifest, pkg string, relations []model.PluginRelation) pluginrepo.FrozenSnapshot {
	return pluginrepo.FrozenSnapshot{
		Manifest:     json.RawMessage(manifest),
		Package:      json.RawMessage(pkg),
		Relations:    relations,
		ManifestHash: "sha256:frozen-manifest",
		PluginHash:   "sha256:frozen-package",
	}
}

func newRequest(pluginID, version string) *model.PluginReviewRequest {
	return &model.PluginReviewRequest{
		PluginID:      pluginID,
		SpaceID:       "space-a",
		TargetScope:   "space",
		Status:        model.ReviewStatusPending,
		Version:       version,
		ApplicantUID:  "user-1",
		ApplicantName: "Alice",
	}
}

// TestInsertReviewRequestDerivesKindFromVisibility pins the rule that decides
// whether approval lists a plugin at all.
//
// The import path snapshots a version as part of the upload, so a private draft
// normally HAS a current_version_id. Gating "first listing" on the ABSENCE of one
// classifies every real upload as an upgrade, and the isFirst branch of
// ApproveReview — the only code that flips visibility to space — then never runs:
// the request reaches `approved`, a version is minted, and the plugin stays
// invisible forever with no error anywhere. A fixture built through the
// plugin-write API hides the bug, because that path also leaves
// current_version_id NULL and lands on the correct branch by accident.
func TestInsertReviewRequestDerivesKindFromVisibility(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)

	// The state a real upload leaves behind: private, but already versioned.
	seed(t, database, seedPlugin{id: "plugin-imported", visibility: "private", currentVersionID: "ver-1", currentVersion: "1.0.0"})
	// A private draft with no version at all (created through the write API).
	seed(t, database, seedPlugin{id: "plugin-draft", visibility: "private"})
	// A plugin already listed to the org.
	seed(t, database, seedPlugin{id: "plugin-listed", visibility: "space", currentVersionID: "ver-2", currentVersion: "1.0.0"})

	tests := []struct {
		name     string
		pluginID string
		version  string
		want     model.ReviewKind
	}{
		{
			name:     "private draft that import already versioned is still a first listing",
			pluginID: "plugin-imported", version: "9.9.9", want: model.ReviewKindFirst,
		},
		{
			name:     "private draft with no version is a first listing",
			pluginID: "plugin-draft", version: "9.9.9", want: model.ReviewKindFirst,
		},
		{
			name:     "already-listed plugin is an upgrade",
			pluginID: "plugin-listed", version: "9.9.9", want: model.ReviewKindUpgrade,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := newRequest(tt.pluginID, tt.version)
			if err := repo.InsertReviewRequest(context.Background(), tenantScope(), req, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
				t.Fatalf("InsertReviewRequest: %v", err)
			}
			if req.Kind != tt.want {
				t.Errorf("kind = %q, want %q", req.Kind, tt.want)
			}
			// The persisted row must agree with what the caller was told; approval
			// reads kind back out of the database, not off this struct.
			var stored string
			if err := database.QueryRow(`SELECT kind FROM plugin_review_requests WHERE review_id=?`, req.ID).Scan(&stored); err != nil {
				t.Fatalf("read back kind: %v", err)
			}
			if model.ReviewKind(stored) != tt.want {
				t.Errorf("persisted kind = %q, want %q", stored, tt.want)
			}
		})
	}
}

// A first listing at the import default label must be accepted: the draft's
// current_version is a DRAFT label, not a published one.
func TestSubmitAcceptsTheDraftLabelOnAFirstListing(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private", currentVersionID: "ver-1", currentVersion: "1.0.0"})
	if err := repo.InsertReviewRequest(context.Background(), tenantScope(), newRequest("plugin-1", "1.0.0"), snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatalf("first listing at the draft label was refused: %v", err)
	}
}

func TestSubmitEnforcesSinglePendingPerPlugin(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private"})
	if err := repo.InsertReviewRequest(context.Background(), tenantScope(), newRequest("plugin-1", "1.0.0"), snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	err := repo.InsertReviewRequest(context.Background(), tenantScope(), newRequest("plugin-1", "1.0.1"), snapshotOf(`{"a":1}`, `{"b":2}`, nil))
	if !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("second pending submit = %v, want ErrConflict", err)
	}
}

// Version-label rules: an already-published label is refused, a label from a
// rejected/canceled request is free to reuse. Since plugin_versions.version is a
// counter and never records the applicant's label, the review table itself is the
// published-label record.
func TestSubmitVersionLabelReuseRules(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private", currentVersionID: "ver-1", currentVersion: "0.9.0"})

	// Submit 1.0.0 and reject it.
	first := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), first, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RejectReview(ctx, reviewerScope(), pluginrepo.RejectReviewParams{
		ReviewID: first.ID, ReviewerUID: "admin-1", ReviewerName: "Adam", Reason: "needs docs",
	}); err != nil {
		t.Fatal(err)
	}
	// The SAME label must be reusable after a rejection — the applicant fixed the
	// content, the version they announced did not change.
	second := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), second, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatalf("a rejected label was not reusable: %v", err)
	}
	// Cancel it and reuse again.
	if err := repo.CancelReview(ctx, tenantScope(), second.ID, "user-1"); err != nil {
		t.Fatal(err)
	}
	third := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), third, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatalf("a canceled label was not reusable: %v", err)
	}
	// Approve it. Now the label is published.
	if _, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{
		ReviewID: third.ID, ReviewerUID: "admin-1", ReviewerName: "Adam",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.InsertReviewRequest(ctx, tenantScope(), newRequest("plugin-1", "1.0.0"), snapshotOf(`{"a":1}`, `{"b":2}`, nil)); !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("republishing an approved label = %v, want ErrConflict", err)
	}
	// A different label is fine.
	if err := repo.InsertReviewRequest(ctx, tenantScope(), newRequest("plugin-1", "1.1.0"), snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatalf("a fresh label was refused: %v", err)
	}
}

// A plugin that was already Space-visible before this feature existed has no
// approved request to compare against, so its current label counts as published
// (grandfathering, with no backfill).
func TestSubmitRefusesTheLiveLabelOfAnAlreadyListedPlugin(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "space", currentVersionID: "ver-1", currentVersion: "2.4.0"})
	if err := repo.InsertReviewRequest(context.Background(), tenantScope(), newRequest("plugin-1", "2.4.0"), snapshotOf(`{"a":1}`, `{"b":2}`, nil)); !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("resubmitting the live label = %v, want ErrConflict", err)
	}
}

// The whole point of the gate: approve is what makes a plugin org-visible, and it
// applies the FROZEN documents rather than whatever the draft says now.
func TestApproveFlipsVisibilityAndAppliesTheFrozenSnapshot(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private", currentVersionID: "ver-1", currentVersion: "0.9.0"})

	req := newRequest("plugin-1", "1.0.0")
	changelog := "first release"
	req.Changelog = &changelog
	frozenManifest := `{"plugin_name":"Frozen"}`
	frozenPackage := `{"attachments":[{"path":"SKILL.md","content_type":"raw","mime_type":"text/markdown","raw_content":"frozen"}]}`
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(frozenManifest, frozenPackage, nil)); err != nil {
		t.Fatal(err)
	}
	// The author keeps editing the draft after submitting. None of it may leak
	// into what the reviewer approves.
	if _, err := database.Exec(`UPDATE plugins SET manifest_json=CAST('{"plugin_name":"Edited after submit"}' AS JSON), plugin_hash=REPEAT('z',71) WHERE plugin_id='plugin-1'`); err != nil {
		t.Fatal(err)
	}

	out, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{
		ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam", RequestID: "req-1",
	})
	if err != nil {
		t.Fatalf("ApproveReview: %v", err)
	}
	if out.Visibility != model.PluginVisibilitySpace {
		t.Fatalf("visibility = %q, want space", out.Visibility)
	}
	if out.CurrentVersion == nil || *out.CurrentVersion != "1.0.0" {
		t.Fatalf("current_version = %v, want the applicant's label", out.CurrentVersion)
	}
	if out.PluginHash != "sha256:frozen-package" || out.ManifestHash != "sha256:frozen-manifest" {
		t.Fatalf("hashes = %q/%q; the live draft leaked into the approval", out.ManifestHash, out.PluginHash)
	}
	if !containsJSON(string(out.Manifest), "Frozen") {
		t.Fatalf("manifest = %s, want the frozen document", out.Manifest)
	}

	// The placement is untouched: it was visible before and stays visible. Hiding
	// it for private drafts would also hide them from their author's own list,
	// which INNER JOINs the placement on visible=1.
	var visible bool
	if err := database.QueryRow(`SELECT visible FROM plugin_placements WHERE plugin_id='plugin-1' AND placement_code='default'`).Scan(&visible); err != nil {
		t.Fatal(err)
	}
	if !visible {
		t.Error("the default placement is hidden after approval")
	}

	// The release history row: plugin_versions.version stays the auto-increment
	// counter. A semver label written there would corrupt the sequence.
	var versionSeq, changelogOut string
	if err := database.QueryRow(`SELECT version, changelog FROM plugin_versions WHERE plugin_id='plugin-1'`).Scan(&versionSeq, &changelogOut); err != nil {
		t.Fatalf("read version row: %v", err)
	}
	if versionSeq != "1" {
		t.Errorf("plugin_versions.version = %q, want the counter %q", versionSeq, "1")
	}
	if changelogOut != changelog {
		t.Errorf("version changelog = %q, want %q", changelogOut, changelog)
	}
	// And the audit trail records who decided it and from where.
	var action, remark string
	if err := database.QueryRow(`SELECT action, remark FROM plugin_audit_logs WHERE plugin_id='plugin-1' ORDER BY created_at DESC LIMIT 1`).Scan(&action, &remark); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if action != "review_approve" || remark != "decision_source=web" {
		t.Fatalf("audit = %q/%q", action, remark)
	}
}

// The container case: an expert_team whose membership changed between submit and
// approve must ship the FROZEN members. Freezing only the documents ships the
// reviewed manifest next to the live membership and records zero relations on the
// minted version.
func TestApproveAppliesTheFrozenRelationGraph(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "team-1", typ: "expert_team", visibility: "private", currentVersionID: "ver-1", currentVersion: "0.9.0"})
	seed(t, database, seedPlugin{id: "member-a", typ: "expert", visibility: "private", embedded: true})
	seed(t, database, seedPlugin{id: "member-b", typ: "expert", visibility: "private", embedded: true})
	seedRelation(t, database, "rel-a", "team-1", "member-a", "expert_team_expert")

	frozen := []model.PluginRelation{{
		ID: "rel-a", SourcePluginID: "team-1", TargetPluginID: "member-a",
		Type: "expert_team_expert", SortOrder: 0, Status: 1,
	}}
	req := newRequest("team-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"plugin_name":"Team"}`, `{"attachments":[]}`, frozen)); err != nil {
		t.Fatal(err)
	}
	// After submitting, the author adds a second member. The reviewer never saw it.
	seedRelation(t, database, "rel-b", "team-1", "member-b", "expert_team_expert")

	if _, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{
		ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam",
	}); err != nil {
		t.Fatalf("ApproveReview: %v", err)
	}

	live := map[string]bool{}
	rows, err := database.Query(`SELECT target_plugin_id FROM plugin_relations WHERE source_plugin_id='team-1' AND deleted_at IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatal(err)
		}
		live[target] = true
	}
	if !live["member-a"] {
		t.Error("the frozen member was not applied")
	}
	if live["member-b"] {
		t.Error("an unreviewed member was shipped by the approval")
	}

	// The minted version must record the frozen graph, not an empty array.
	var relationsJSON string
	if err := database.QueryRow(`SELECT relations_json FROM plugin_versions WHERE plugin_id='team-1'`).Scan(&relationsJSON); err != nil {
		t.Fatal(err)
	}
	if !containsJSON(relationsJSON, "member-a") {
		t.Fatalf("version relations_json = %s, want the frozen member", relationsJSON)
	}
	if containsJSON(relationsJSON, "member-b") {
		t.Fatalf("version relations_json shipped the live member: %s", relationsJSON)
	}

	// A listed container whose embedded parts stayed private is uninstallable by
	// anyone but its author (resolveInstallDetail refuses a partially visible
	// topology), so the frozen children follow the top's visibility. The member
	// dropped from the frozen graph must NOT be promoted.
	var visA, visB string
	if err := database.QueryRow(`SELECT visibility FROM plugins WHERE plugin_id='member-a'`).Scan(&visA); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(`SELECT visibility FROM plugins WHERE plugin_id='member-b'`).Scan(&visB); err != nil {
		t.Fatal(err)
	}
	if visA != "space" {
		t.Errorf("member-a visibility = %q, want space", visA)
	}
	if visB != "private" {
		t.Errorf("member-b visibility = %q, want private (it was not in the reviewed graph)", visB)
	}
}

// An upgrade swaps the listed content and does not re-flip visibility.
func TestApproveUpgradeKeepsTheListingAndSwapsContent(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "space", currentVersionID: "ver-1", currentVersion: "1.0.0"})

	req := newRequest("plugin-1", "2.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"plugin_name":"V2"}`, `{"attachments":[]}`, nil)); err != nil {
		t.Fatal(err)
	}
	if req.Kind != model.ReviewKindUpgrade {
		t.Fatalf("kind = %q, want upgrade", req.Kind)
	}
	// While the request is pending, the listed version is still the old one.
	var liveVersion string
	if err := database.QueryRow(`SELECT current_version FROM plugins WHERE plugin_id='plugin-1'`).Scan(&liveVersion); err != nil {
		t.Fatal(err)
	}
	if liveVersion != "1.0.0" {
		t.Fatalf("pending review already changed the listed version to %q", liveVersion)
	}
	out, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{
		ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam",
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Visibility != model.PluginVisibilitySpace {
		t.Fatalf("visibility = %q", out.Visibility)
	}
	if out.CurrentVersion == nil || *out.CurrentVersion != "2.0.0" {
		t.Fatalf("current_version = %v", out.CurrentVersion)
	}
}

// A reject changes nothing about the plugin: a private draft stays private.
func TestRejectLeavesThePluginUntouched(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private", currentVersion: "0.9.0"})
	req := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"plugin_name":"Frozen"}`, `{"attachments":[]}`, nil)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RejectReview(ctx, reviewerScope(), pluginrepo.RejectReviewParams{
		ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam", Reason: "needs docs",
	}); err != nil {
		t.Fatal(err)
	}
	var visibility, currentVersion string
	if err := database.QueryRow(`SELECT visibility, current_version FROM plugins WHERE plugin_id='plugin-1'`).Scan(&visibility, &currentVersion); err != nil {
		t.Fatal(err)
	}
	if visibility != "private" || currentVersion != "0.9.0" {
		t.Fatalf("reject changed the plugin: %q/%q", visibility, currentVersion)
	}
	var versions int
	if err := database.QueryRow(`SELECT COUNT(*) FROM plugin_versions WHERE plugin_id='plugin-1'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 0 {
		t.Errorf("a rejected submission minted %d version rows", versions)
	}
	var status, reason, source string
	if err := database.QueryRow(`SELECT status, reason, decision_source FROM plugin_review_requests WHERE review_id=?`, req.ID).Scan(&status, &reason, &source); err != nil {
		t.Fatal(err)
	}
	if status != "rejected" || reason != "needs docs" || source != "web" {
		t.Fatalf("request = %q/%q/%q", status, reason, source)
	}
}

// Cancel is applicant-only, and an already-decided request is a CONFLICT — not a
// 404 telling the applicant their just-approved request never existed.
func TestCancelIsApplicantOnlyAndConflictsWhenDecided(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private"})
	req := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	// Someone else cannot cancel it, and gets no hint that it exists.
	if err := repo.CancelReview(ctx, tenantScope(), req.ID, "user-9"); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("cancel by a non-applicant = %v, want ErrNotFound", err)
	}
	// Neither can a caller in another Space.
	if err := repo.CancelReview(ctx, pluginrepo.Scope{CallerUID: "user-1", SpaceID: "space-b"}, req.ID, "user-1"); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("cross-Space cancel = %v, want ErrNotFound", err)
	}
	if err := repo.CancelReview(ctx, tenantScope(), req.ID, "user-1"); err != nil {
		t.Fatalf("applicant cancel: %v", err)
	}
	// Cancelling twice, or cancelling after a decision, is a conflict.
	if err := repo.CancelReview(ctx, tenantScope(), req.ID, "user-1"); !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("second cancel = %v, want ErrConflict", err)
	}
}

// A cross-Space decision must be indistinguishable from a missing one, and must
// not mutate anything.
func TestDecisionsAreSpaceScoped(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private"})
	req := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	foreign := pluginrepo.Scope{CallerUID: "admin-2", SpaceID: "space-b"}
	if _, err := repo.ApproveReview(ctx, foreign, pluginrepo.ApproveReviewParams{ReviewID: req.ID, ReviewerUID: "admin-2"}); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("cross-Space approve = %v, want ErrNotFound", err)
	}
	if err := repo.RejectReview(ctx, foreign, pluginrepo.RejectReviewParams{ReviewID: req.ID, ReviewerUID: "admin-2", Reason: "no"}); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("cross-Space reject = %v, want ErrNotFound", err)
	}
	var status, visibility string
	if err := database.QueryRow(`SELECT rr.status, p.visibility FROM plugin_review_requests rr JOIN plugins p ON p.plugin_id=rr.plugin_id WHERE rr.review_id=?`, req.ID).Scan(&status, &visibility); err != nil {
		t.Fatal(err)
	}
	if status != "pending" || visibility != "private" {
		t.Fatalf("a cross-Space decision mutated state: %q/%q", status, visibility)
	}
	// A reviewer in the right Space still succeeds, so the refusal above was the
	// Space predicate and not a broken fixture.
	if _, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam"}); err != nil {
		t.Fatalf("in-Space approve: %v", err)
	}
}

// Two admins deciding at once: exactly one wins, and the loser gets a typed
// conflict carrying the committed outcome rather than a 404 or a 500.
func TestConcurrentDecisionsProduceOneWinner(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private"})
	req := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, errs[0] = repo.ApproveReview(ctx, pluginrepo.Scope{CallerUID: "admin-1", SpaceID: "space-a"},
			pluginrepo.ApproveReviewParams{ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam"})
	}()
	go func() {
		defer wg.Done()
		errs[1] = repo.RejectReview(ctx, pluginrepo.Scope{CallerUID: "admin-2", SpaceID: "space-a"},
			pluginrepo.RejectReviewParams{ReviewID: req.ID, ReviewerUID: "admin-2", ReviewerName: "Ada", Reason: "no"})
	}()
	wg.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, pluginrepo.ErrConflict):
			// The expected loser outcome.
		default:
			t.Fatalf("decider %d got a non-conflict error: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (errs=%v)", winners, errs)
	}
	var status string
	if err := database.QueryRow(`SELECT status FROM plugin_review_requests WHERE review_id=?`, req.ID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "approved" && status != "rejected" {
		t.Fatalf("terminal status = %q", status)
	}
	var versions int
	if err := database.QueryRow(`SELECT COUNT(*) FROM plugin_versions WHERE plugin_id='plugin-1'`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if status == "approved" && versions != 1 {
		t.Errorf("approved once but minted %d versions", versions)
	}
	if status == "rejected" && versions != 0 {
		t.Errorf("rejected but minted %d versions", versions)
	}
}

// Deciding an already-settled request is a conflict; deciding a nonexistent one
// is not found. The IM callback needs the distinction to answer `conflict` with a
// real state instead of retrying the event into the DLQ.
func TestDecidingASettledRequestConflictsAndAMissingOneIsNotFound(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private"})
	req := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam"}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{ReviewID: req.ID, ReviewerUID: "admin-1", ReviewerName: "Adam"}); !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("re-approve = %v, want ErrConflict", err)
	}
	if err := repo.RejectReview(ctx, reviewerScope(), pluginrepo.RejectReviewParams{ReviewID: req.ID, ReviewerUID: "admin-1", Reason: "no"}); !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("reject after approve = %v, want ErrConflict", err)
	}
	if _, err := repo.ApproveReview(ctx, reviewerScope(), pluginrepo.ApproveReviewParams{ReviewID: "review-nope", ReviewerUID: "admin-1"}); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("approve of a missing request = %v, want ErrNotFound", err)
	}
}

// Reads are scoped: an applicant sees only their own requests, a reviewer sees
// the whole Space, and neither sees across Spaces.
func TestReviewReadsAreScoped(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private", owner: "user-1"})
	seed(t, database, seedPlugin{id: "plugin-2", visibility: "private", owner: "user-2"})

	mine := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), mine, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	theirs := newRequest("plugin-2", "1.0.0")
	theirs.ApplicantUID = "user-2"
	theirs.ApplicantName = "Bob"
	if err := repo.InsertReviewRequest(ctx, pluginrepo.Scope{CallerUID: "user-2", SpaceID: "space-a"}, theirs, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}

	// An applicant reading someone else's request is a miss, not a 403 that
	// confirms it exists.
	if _, err := repo.GetReviewRequest(ctx, tenantScope(), theirs.ID, false); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("applicant read of another request = %v, want ErrNotFound", err)
	}
	if _, err := repo.GetReviewRequest(ctx, reviewerScope(), theirs.ID, true); err != nil {
		t.Fatalf("reviewer read = %v", err)
	}
	// Cross-Space, even as a reviewer.
	if _, err := repo.GetReviewRequest(ctx, pluginrepo.Scope{CallerUID: "admin-9", SpaceID: "space-b"}, theirs.ID, true); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("cross-Space reviewer read = %v, want ErrNotFound", err)
	}

	items, total, err := repo.ListReviewRequests(ctx, tenantScope(), pluginrepo.ReviewListFilter{SpaceID: "space-a", ApplicantUID: "user-1"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(items) != 1 || items[0].ID != mine.ID {
		t.Fatalf("mine list = %d/%d", total, len(items))
	}
	items, total, err = repo.ListReviewRequests(ctx, reviewerScope(), pluginrepo.ReviewListFilter{SpaceID: "space-a"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("space list = %d/%d", total, len(items))
	}
	_, total, err = repo.ListReviewRequests(ctx, pluginrepo.Scope{CallerUID: "admin-9", SpaceID: "space-b"}, pluginrepo.ReviewListFilter{SpaceID: "space-b"})
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 {
		t.Fatalf("another Space's queue total = %d, want 0", total)
	}

	// The list must not carry the frozen snapshot columns; the detail read must.
	list, _, err := repo.ListReviewRequests(ctx, reviewerScope(), pluginrepo.ReviewListFilter{SpaceID: "space-a"})
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list {
		if len(item.PluginJSON) != 0 || len(item.ManifestJSON) != 0 || len(item.RelationsJSON) != 0 {
			t.Errorf("list row %s carries snapshot bytes", item.ID)
		}
	}
	detail, err := repo.LoadReviewSnapshot(ctx, reviewerScope(), mine.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.PluginJSON) == 0 || len(detail.ManifestJSON) == 0 || len(detail.RelationsJSON) == 0 {
		t.Fatalf("detail is missing snapshot bytes: %+v", detail)
	}
}

// The IM callback's cross-scope read is the one query with no tenant predicate.
// It must still refuse a nonexistent id, and the receipt table must make a
// replayed event a no-op.
func TestCardActionReceiptRoundTrip(t *testing.T) {
	database := reviewDB(t)
	repo := pluginrepo.New(database)
	ctx := context.Background()
	seed(t, database, seedPlugin{id: "plugin-1", visibility: "private"})
	req := newRequest("plugin-1", "1.0.0")
	if err := repo.InsertReviewRequest(ctx, tenantScope(), req, snapshotOf(`{"a":1}`, `{"b":2}`, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetReviewRequestAnySpace(ctx, req.ID); err != nil {
		t.Fatalf("AnySpace read: %v", err)
	}
	if _, err := repo.GetReviewRequestAnySpace(ctx, "review-nope"); !errors.Is(err, pluginrepo.ErrNotFound) {
		t.Fatalf("AnySpace read of a missing id = %v, want ErrNotFound", err)
	}
	if got, err := repo.GetCardActionReceipt(ctx, "9007199254740993"); err != nil || got != nil {
		t.Fatalf("unseen receipt = %v/%v, want nil/nil", got, err)
	}
	rec := &model.CardActionReceipt{EventID: "9007199254740993", ReviewID: req.ID, Decision: "approve", StoredResponse: `{"disposition":"applied","state":"approved"}`}
	if err := repo.InsertCardActionReceipt(ctx, rec); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetCardActionReceipt(ctx, "9007199254740993")
	if err != nil || got == nil {
		t.Fatalf("stored receipt = %v/%v", got, err)
	}
	if got.StoredResponse != rec.StoredResponse {
		t.Fatalf("stored response = %q, want it byte-identical for replay", got.StoredResponse)
	}
	if err := repo.InsertCardActionReceipt(ctx, rec); !errors.Is(err, pluginrepo.ErrConflict) {
		t.Fatalf("duplicate receipt = %v, want ErrConflict", err)
	}
}

func containsJSON(haystack, needle string) bool {
	return len(haystack) > 0 && len(needle) > 0 && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
