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
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT plugin_types_json FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_types_json"}))
	mock.ExpectRollback()
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
	mock.ExpectBegin()
	// Current types already include the requested type, so nothing is narrowed and
	// no reference count runs before the update.
	mock.ExpectQuery(`SELECT plugin_types_json FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_types_json"}).AddRow([]byte(`["expert"]`)))
	mock.ExpectExec(`UPDATE plugin_categories SET name=\?`).
		WithArgs("Ops", "k", `["expert"]`, 2, sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := New(db).UpdateCategory(context.Background(), model.PluginCategory{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: []byte(`["expert"]`), SortOrder: 2}); err != nil {
		t.Fatalf("UpdateCategory error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateCategoryConflictWhenNarrowingStrandsLivePlugins is the P2-4 guard: a
// type-narrowing update (dropping "expert") is refused with ErrConflict while a
// live expert still references the category under the dropped type, so those rows
// are never stranded. The update is not issued and the transaction rolls back.
func TestUpdateCategoryConflictWhenNarrowingStrandsLivePlugins(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT plugin_types_json FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_types_json"}).AddRow([]byte(`["expert","skill"]`)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE category_id=\? AND plugin_type=\? AND status=1 AND deleted_at IS NULL`).
		WithArgs("cat-1", model.PluginTypeExpert).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(3))
	mock.ExpectRollback()
	err := New(db).UpdateCategory(context.Background(), model.PluginCategory{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: []byte(`["skill"]`), SortOrder: 2})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateCategory error = %v, want ErrConflict", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateCategoryNarrowsWhenNoLivePluginUsesDroppedType proves the narrowing
// guard is not over-eager: dropping "expert" is allowed when no live expert
// references the category under it, and the update commits.
func TestUpdateCategoryNarrowsWhenNoLivePluginUsesDroppedType(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT plugin_types_json FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"plugin_types_json"}).AddRow([]byte(`["expert","skill"]`)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE category_id=\? AND plugin_type=\? AND status=1 AND deleted_at IS NULL`).
		WithArgs("cat-1", model.PluginTypeExpert).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectExec(`UPDATE plugin_categories SET name=\?`).
		WithArgs("Ops", "k", `["skill"]`, 2, sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := New(db).UpdateCategory(context.Background(), model.PluginCategory{ID: "cat-1", Name: "Ops", IconKey: "k", PluginTypes: []byte(`["skill"]`), SortOrder: 2}); err != nil {
		t.Fatalf("UpdateCategory error = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteCategoryConflictWhenPluginsReference(t *testing.T) {
	db, mock, _ := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT category_id FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"category_id"}).AddRow("cat-1"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE category_id=\? AND status=1 AND deleted_at IS NULL`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(2))
	// No UPDATE is issued: the guard aborts before the soft delete and rolls back.
	mock.ExpectRollback()
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
	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT category_id FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"category_id"}).AddRow("cat-1"))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM plugins WHERE category_id=\? AND status=1 AND deleted_at IS NULL`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(0))
	mock.ExpectExec(`UPDATE plugin_categories SET status=0, deleted_at=\? WHERE category_id=\? AND deleted_at IS NULL`).
		WithArgs(sqlmock.AnyArg(), "cat-1").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
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
	mock.ExpectBegin()
	// The FOR UPDATE lock finds no live row → ErrNotFound before any count/delete.
	mock.ExpectQuery(`SELECT category_id FROM plugin_categories WHERE category_id=\? AND deleted_at IS NULL FOR UPDATE`).
		WithArgs("cat-1").
		WillReturnRows(sqlmock.NewRows([]string{"category_id"}))
	mock.ExpectRollback()
	if err := New(db).DeleteCategory(context.Background(), "cat-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DeleteCategory error = %v, want ErrNotFound", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
