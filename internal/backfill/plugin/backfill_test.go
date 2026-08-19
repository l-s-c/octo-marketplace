package plugin

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func emptySourceExpectations(mock sqlmock.Sqlmock) {
	mock.ExpectQuery("SELECT id,COUNT\\(\\*\\) FROM").WillReturnRows(sqlmock.NewRows([]string{"id", "count"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id,name,icon_key,sort_order,created_at,updated_at,deleted_at FROM expert_categories")).WillReturnRows(sqlmock.NewRows([]string{"id", "name", "icon_key", "sort_order", "created_at", "updated_at", "deleted_at"}))
	mock.ExpectQuery("SELECT id,name FROM skill_tags").WillReturnRows(sqlmock.NewRows([]string{"id", "name"}))
	mock.ExpectQuery("SELECT id,name,display_name").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "display_name", "icon_url", "source_skill_id", "current_version_id", "description", "category_id", "tags", "owner_id", "owner_name", "creator_id", "creator_name", "space_id", "visibility", "version", "readme_content", "file_name", "file_url", "file_size", "file_sha256", "created_at", "updated_at", "is_deleted"}))
	mock.ExpectQuery("SELECT id,name,slug,slogan").WillReturnRows(sqlmock.NewRows([]string{"id", "name", "slug", "slogan", "category", "icon", "icon_version", "tags_json", "tools_json", "usage_examples_json", "faqs_json", "notes_json", "visibility", "owner_uid", "space_id", "creator_name", "created_by_type", "created_by_bot_uid", "created_by_bot_name", "transport", "config_json", "created_at", "updated_at", "deleted_at"}))
}
func TestDryRunDoesNotWrite(t *testing.T) {
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	emptySourceExpectations(mock)
	r := New(db)
	r.now = func() time.Time { return time.Unix(1, 0) }
	got, e := r.Run(context.Background(), Options{Mode: ModeDryRun})
	if e != nil {
		t.Fatal(e)
	}
	if got.Expected.Plugins != 0 || len(got.Issues) != 3 {
		t.Fatalf("unexpected report: %#v", got)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestVerifyEmptyPlanDoesNotWrite(t *testing.T) {
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	emptySourceExpectations(mock)
	got, e := New(db).Run(context.Background(), Options{Mode: ModeVerify})
	if e != nil {
		t.Fatal(e)
	}
	if got.Observed.Missing != 0 || got.ObservedHash == "" {
		t.Fatalf("unexpected report: %#v", got)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
func TestApplyEmptyPlanUsesTransaction(t *testing.T) {
	db, mock, e := sqlmock.New()
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	emptySourceExpectations(mock)
	mock.ExpectBegin()
	mock.ExpectCommit()
	if _, e = New(db).Run(context.Background(), Options{Mode: ModeApply}); e != nil {
		t.Fatal(e)
	}
	if e = mock.ExpectationsWereMet(); e != nil {
		t.Fatal(e)
	}
}
