package plugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

func TestListAdminCategoriesCountsOnlyRequestedType(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// The count subquery must be type-scoped (p.plugin_type=?) and exclude
	// embedded rows (p.is_embedded=0): a category registered for several types
	// reports only the requested type's live, standalone plugins. Both the count
	// subquery and the JSON_CONTAINS type filter bind the SAME requested type, so
	// two identical args flow in the SQL text order (count first, then filter).
	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT c\.category_id.*\(SELECT COUNT\(\*\) FROM plugins p WHERE p\.category_id=c\.category_id AND p\.plugin_type=\? AND p\.is_embedded=0 AND p\.status=1 AND p\.deleted_at IS NULL\).*JSON_CONTAINS\(c\.plugin_types_json, JSON_QUOTE\(\?\), '\$'\)`).
		WithArgs(model.PluginTypeExpert, model.PluginTypeExpert).
		WillReturnRows(sqlmock.NewRows([]string{"category_id", "name", "icon_key", "plugin_types_json", "sort_order", "status", "created_at", "updated_at", "plugin_count"}).
			AddRow("cat-1", "Shared", "k", []byte(`["expert","skill"]`), 1, 1, now, now, 2))

	cats, err := New(db).ListAdminCategories(context.Background(), model.PluginTypeExpert)
	if err != nil {
		t.Fatalf("ListAdminCategories error = %v", err)
	}
	if len(cats) != 1 || cats[0].ID != "cat-1" || cats[0].PluginCount != 2 {
		t.Fatalf("categories = %#v", cats)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCreateCategoryInsertsWithActiveStatus(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectExec(`INSERT INTO plugin_categories .*status.* VALUES \(\?,\?,\?,\?,\?,1,\?,\?\)`).
		WithArgs("cat-1", "Ops", "k", `["expert","expert_team"]`, 5, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))
	err = New(db).CreateCategory(context.Background(), model.PluginCategory{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: []byte(`["expert","expert_team"]`), SortOrder: 5})
	if err != nil {
		t.Fatalf("CreateCategory error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCategoryNotFoundWhenNoRow(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectExec(`UPDATE plugin_categories SET name=\?,icon_key=\?,plugin_types_json=\?,sort_order=\?,updated_at=\? WHERE category_id=\? AND deleted_at IS NULL`).
		WithArgs("Ops", "k", `["expert"]`, 2, sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	err := New(db).UpdateCategory(context.Background(), model.PluginCategory{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: []byte(`["expert"]`), SortOrder: 2})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdateCategory error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateCategorySucceeds(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectExec(`UPDATE plugin_categories SET name=\?`).
		WithArgs("Ops", "k", `["expert"]`, 2, sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := New(db).UpdateCategory(context.Background(), model.PluginCategory{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: []byte(`["expert"]`), SortOrder: 2}); err != nil {
		t.Fatalf("UpdateCategory error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteCategoryConflictWhenPluginsReference(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE category_id=\? AND status=1 AND deleted_at IS NULL`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))
	// No UPDATE is issued: the guard aborts before the soft delete.
	err := New(db).DeleteCategory(context.Background(), "cat-1")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("DeleteCategory error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteCategorySoftDeletesWhenUnused(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE category_id=\? AND status=1 AND deleted_at IS NULL`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectExec(`UPDATE plugin_categories SET status=0, deleted_at=\? WHERE category_id=\? AND deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	if err := New(db).DeleteCategory(context.Background(), "cat-1"); err != nil {
		t.Fatalf("DeleteCategory error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteCategoryNotFoundWhenNoRow(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectExec(`UPDATE plugin_categories SET status=0`).
		WithArgs(sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := New(db).DeleteCategory(context.Background(), "cat-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteCategory error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
