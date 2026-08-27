package skill

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestListTagsScopesToSpaceAndFuzzyQuery(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery("SELECT ranked\\.id, ranked\\.space_id, ranked\\.name").
		WithArgs("space-1", "space-1", GlobalTagSpaceID, "%auto%", 25).
		WillReturnRows(sqlmock.NewRows([]string{"id", "space_id", "name", "created_by", "created_at", "updated_at"}).
			AddRow(int64(1), "space-1", "automation", "user-1", now, now))

	rows, err := New(db).ListTags(context.Background(), "space-1", "auto", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "automation" {
		t.Fatalf("rows = %#v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListTagsIncludesGlobalTags(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery("ROW_NUMBER\\(\\) OVER").
		WithArgs("space-1", "space-1", GlobalTagSpaceID, 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "space_id", "name", "created_by", "created_at", "updated_at"}).
			AddRow(int64(1), GlobalTagSpaceID, "official", "admin", now, now).
			AddRow(int64(2), "space-1", "team", "user-1", now, now))

	rows, err := New(db).ListTags(context.Background(), "space-1", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].SpaceID != GlobalTagSpaceID || rows[0].Name != "official" {
		t.Fatalf("global tag missing: %#v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListTagsDeduplicatesByNameWithSpaceLocalFirst(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	now := time.Now().UTC()
	mock.ExpectQuery("PARTITION BY name.*CASE WHEN space_id = \\? THEN 0 ELSE 1 END").
		WithArgs("space-1", "space-1", GlobalTagSpaceID, 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "space_id", "name", "created_by", "created_at", "updated_at"}).
			AddRow(int64(11), "space-1", "AI", "user-1", now, now))

	rows, err := New(db).ListTags(context.Background(), "space-1", "", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %#v", rows)
	}
	if rows[0].ID != 11 || rows[0].SpaceID != "space-1" || rows[0].Name != "AI" {
		t.Fatalf("space-local tag should win for duplicate names, got %#v", rows[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOrCreateTagIDsPrefersSpaceLocalCollision(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT id").
		WithArgs("AI", GlobalTagSpaceID, "space-1", "space-1").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(22)))

	ids, err := resolveOrCreateTagIDs(context.Background(), db, "space-1", "user-1", []string{"AI"})
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != 22 {
		t.Fatalf("ids = %#v, want local tag id 22", ids)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListFiltersByAllTags(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// With comprehensive sort (default), expect a count query first, then the data query.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("space-1", "user-1", "space-1", "1", "2").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery("JSON_CONTAINS\\(s\\.tags, \\?\\).*OR.*JSON_CONTAINS\\(s\\.tags, \\?\\)").
		WithArgs("space-1", "user-1", "space-1", "1", "2", 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "display_name", "icon_url", "description", "category_id", "tags",
			"owner_id", "owner_name", "space_id", "visibility", "version",
			"readme_content", "file_name", "file_url", "file_size", "file_sha256",
			"created_at", "updated_at", "view_count", "download_count",
		}))

	_, err = New(db).List(context.Background(), ListFilter{
		SpaceID:     "space-1",
		UserID:      "user-1",
		TagIDGroups: [][]int64{{1, 2}},
		Limit:       20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestListSearchMatchesNameAndDisplayNameFuzzy(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// With comprehensive sort (default), expect a count query first, then the data query.
	mock.ExpectQuery("SELECT COUNT").
		WithArgs("space-1", "user-1", "space-1", "%auto%", "%auto%").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	mock.ExpectQuery("s\\.name LIKE \\?.*s\\.display_name LIKE \\?").
		WithArgs("space-1", "user-1", "space-1", "%auto%", "%auto%", 20, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id",
			"description", "category_id", "tags",
			"owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version",
			"readme_content", "file_name", "file_url", "file_size", "file_sha256",
			"created_at", "updated_at", "resolved_version", "version_storage", "view_count", "download_count",
		}))

	_, err = New(db).List(context.Background(), ListFilter{
		SpaceID: "space-1",
		UserID:  "user-1",
		Query:   "auto",
		Limit:   20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
