package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

type fakeCategoryStore struct {
	created   model.PluginCategory
	updated   model.PluginCategory
	deletedID string
	createErr error
	updateErr error
	deleteErr error
}

func (f *fakeCategoryStore) ListPlacementCategories(context.Context, pluginrepo.Scope, string, model.PluginType) ([]model.PluginCategory, error) {
	return nil, nil
}
func (f *fakeCategoryStore) ListAdminCategories(context.Context, model.PluginType) ([]model.PluginCategory, error) {
	return nil, nil
}
func (f *fakeCategoryStore) CreateCategory(_ context.Context, c model.PluginCategory) error {
	f.created = c
	return f.createErr
}
func (f *fakeCategoryStore) UpdateCategory(_ context.Context, c model.PluginCategory) error {
	f.updated = c
	return f.updateErr
}
func (f *fakeCategoryStore) DeleteCategory(_ context.Context, id string) error {
	f.deletedID = id
	return f.deleteErr
}

func newTestCategories(store *fakeCategoryStore) *Categories {
	return NewCategories(store, func() string { return "cat-generated" })
}

func TestAdminCreateCategoryAssignsIDAndMarshalsTypes(t *testing.T) {
	store := &fakeCategoryStore{}
	cat, err := newTestCategories(store).AdminCreateCategory(context.Background(), "  Ops  ", "k", []model.PluginType{model.PluginTypeExpert, model.PluginTypeExpertTeam}, 5)
	if err != nil {
		t.Fatalf("AdminCreateCategory error = %v", err)
	}
	if cat.ID != "cat-generated" || cat.Name != "Ops" || cat.Status != 1 || cat.SortOrder != 5 {
		t.Fatalf("category = %#v", cat)
	}
	if string(store.created.PluginTypes) != `["expert","expert_team"]` {
		t.Fatalf("plugin_types = %s", store.created.PluginTypes)
	}
	if store.created.ID != "cat-generated" {
		t.Fatalf("repo got ID %q", store.created.ID)
	}
}

func TestAdminCreateCategoryRejectsEmptyNameAndTypes(t *testing.T) {
	store := &fakeCategoryStore{}
	svc := newTestCategories(store)
	for _, tc := range []struct {
		name        string
		catName     string
		pluginTypes []model.PluginType
	}{
		{"empty name", "   ", []model.PluginType{model.PluginTypeExpert}},
		{"empty types", "Ops", nil},
		{"invalid type", "Ops", []model.PluginType{"bogus"}},
	} {
		if _, err := svc.AdminCreateCategory(context.Background(), tc.catName, "k", tc.pluginTypes, 0); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
	}
}

func TestAdminUpdateCategoryMapsNotFound(t *testing.T) {
	store := &fakeCategoryStore{updateErr: pluginrepo.ErrNotFound}
	_, err := newTestCategories(store).AdminUpdateCategory(context.Background(), "cat-1", "Ops", "k", []model.PluginType{model.PluginTypeExpert}, 2)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if store.updated.ID != "cat-1" || store.updated.SortOrder != 2 {
		t.Fatalf("repo got %#v", store.updated)
	}
	if !json.Valid(store.updated.PluginTypes) {
		t.Fatalf("plugin_types not marshaled: %s", store.updated.PluginTypes)
	}
}

func TestAdminUpdateCategoryRejectsEmptyID(t *testing.T) {
	store := &fakeCategoryStore{}
	if _, err := newTestCategories(store).AdminUpdateCategory(context.Background(), "  ", "Ops", "k", []model.PluginType{model.PluginTypeExpert}, 0); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestAdminDeleteCategoryMapsConflict(t *testing.T) {
	store := &fakeCategoryStore{deleteErr: pluginrepo.ErrConflict}
	if err := newTestCategories(store).AdminDeleteCategory(context.Background(), "cat-1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if store.deletedID != "cat-1" {
		t.Fatalf("repo got %q", store.deletedID)
	}
}

func TestAdminDeleteCategorySucceeds(t *testing.T) {
	store := &fakeCategoryStore{}
	if err := newTestCategories(store).AdminDeleteCategory(context.Background(), "cat-1"); err != nil {
		t.Fatalf("err = %v", err)
	}
}
