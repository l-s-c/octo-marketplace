package skill

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	categoryrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/category"
	skillrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/skill"
)

// TestAdminGet_RejectsNonPublic verifies that AdminGet returns ErrNotFound for
// skills that are not visibility='public'.
func TestAdminGet_RejectsNonPublic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo := skillrepo.New(db)
	catRepo := categoryrepo.New(db)
	store := &fakeStorage{}
	svc := New(repo, catRepo, store, func() string { return "id" })

	// Return a private skill
	rows := sqlmock.NewRows([]string{
		"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id",
		"description", "category_id", "tags",
		"owner_id", "owner_name", "space_id", "visibility", "version",
		"readme_content", "file_name", "file_url", "file_size", "file_sha256",
		"created_at", "updated_at",
		"resolved_version", "version_storage",
		"view_count", "download_count",
	}).AddRow(
		"sk-1", "private-skill", "", "", "", "",
		"desc", "", []byte(`[]`),
		"u1", "admin", "sp1", "private", "1.0.0",
		"", "", "", int64(0), "",
		time.Now(), time.Now(),
		"1.0.0", "",
		int64(0), int64(0),
	)
	mock.ExpectQuery("SELECT .+ FROM skills").WithArgs("sk-1").WillReturnRows(rows)

	_, err = svc.AdminGet(context.Background(), "sk-1")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestAdminGet_AcceptsPublic verifies that AdminGet succeeds for public skills.
func TestAdminGet_AcceptsPublic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo := skillrepo.New(db)
	catRepo := categoryrepo.New(db)
	store := &fakeStorage{}
	svc := New(repo, catRepo, store, func() string { return "id" })

	rows := sqlmock.NewRows([]string{
		"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id",
		"description", "category_id", "tags",
		"owner_id", "owner_name", "space_id", "visibility", "version",
		"readme_content", "file_name", "file_url", "file_size", "file_sha256",
		"created_at", "updated_at",
		"resolved_version", "version_storage",
		"view_count", "download_count",
	}).AddRow(
		"sk-2", "public-skill", "Public Skill", "", "", "v1",
		"a public skill", "cat1", []byte(`["demo"]`),
		"admin-uid", "Admin", "", "public", "1.0.0",
		"", "skill.zip", "skills/sk-2/v1.0.0/skill.zip", int64(1024), "sha",
		time.Now(), time.Now(),
		"1.0.0", "",
		int64(5), int64(10),
	)
	mock.ExpectQuery("SELECT .+ FROM skills").WithArgs("sk-2").WillReturnRows(rows)

	item, err := svc.AdminGet(context.Background(), "sk-2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if item.ID != "sk-2" {
		t.Fatalf("expected skill_id=sk-2, got %s", item.ID)
	}
	if item.Visibility != "public" {
		t.Fatalf("expected visibility=public, got %s", item.Visibility)
	}
}

// TestAdminGetSkillMD_RejectsNonPublic verifies that AdminGetSkillMD returns
// ErrNotFound for non-public skills.
func TestAdminGetSkillMD_RejectsNonPublic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	repo := skillrepo.New(db)
	catRepo := categoryrepo.New(db)
	store := &fakeStorage{}
	svc := New(repo, catRepo, store, func() string { return "id" })

	rows := sqlmock.NewRows([]string{
		"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id",
		"description", "category_id", "tags",
		"owner_id", "owner_name", "space_id", "visibility", "version",
		"readme_content", "file_name", "file_url", "file_size", "file_sha256",
		"created_at", "updated_at",
		"resolved_version", "version_storage",
		"view_count", "download_count",
	}).AddRow(
		"sk-priv", "space-skill", "", "", "", "",
		"desc", "", []byte(`[]`),
		"u1", "owner", "sp1", "space", "1.0.0",
		"", "", "", int64(0), "",
		time.Now(), time.Now(),
		"1.0.0", `{"type":"s3","zip_object_key":"x","skill_md_object_key":"y"}`,
		int64(0), int64(0),
	)
	mock.ExpectQuery("SELECT .+ FROM skills").WithArgs("sk-priv").WillReturnRows(rows)

	_, err = svc.AdminGetSkillMD(context.Background(), "sk-priv")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}
