package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

type categoryStore interface {
	ListPlacementCategories(context.Context, pluginrepo.Scope, string, model.PluginType) ([]model.PluginCategory, error)
	ListAdminCategories(context.Context, model.PluginType) ([]model.PluginCategory, error)
	CreateCategory(context.Context, model.PluginCategory) error
	UpdateCategory(context.Context, model.PluginCategory) error
	DeleteCategory(context.Context, string) error
}

// Categories serves the placement-aware category read plus the admin taxonomy
// CRUD, which stays separate from Service because it needs only the category
// repository. id mints category IDs for admin creates.
type Categories struct {
	repo categoryStore
	id   func() string
}

func NewCategories(repo categoryStore, idGen func() string) *Categories {
	return &Categories{repo: repo, id: idGen}
}

func (s *Categories) ListCategories(ctx context.Context, caller Caller, placement string, typ model.PluginType) ([]model.PluginCategory, error) {
	if strings.TrimSpace(caller.UID) == "" || strings.TrimSpace(caller.SpaceID) == "" || strings.TrimSpace(placement) == "" {
		return nil, ErrInvalidRequest
	}
	if !validPluginType(typ) {
		return nil, ErrInvalidRequest
	}
	items, err := s.repo.ListPlacementCategories(ctx, pluginrepo.Scope{CallerUID: caller.UID, SpaceID: caller.SpaceID}, placement, typ)
	if errors.Is(err, pluginrepo.ErrNotFound) {
		return nil, ErrNotFound
	}
	return items, err
}

// AdminListCategories lists every category applicable to a plugin type with its
// live-plugin count (admin taxonomy management), across all Spaces.
func (s *Categories) AdminListCategories(ctx context.Context, typ model.PluginType) ([]model.PluginCategory, error) {
	if !validPluginType(typ) {
		return nil, ErrInvalidRequest
	}
	return s.repo.ListAdminCategories(ctx, typ)
}

// AdminCreateCategory mints a new taxonomy row applicable to pluginTypes. It
// assigns the category id; the repository stamps status/timestamps.
func (s *Categories) AdminCreateCategory(ctx context.Context, name, iconKey string, pluginTypes []model.PluginType, sortOrder int) (*model.PluginCategory, error) {
	name = strings.TrimSpace(name)
	raw, err := marshalPluginTypes(name, pluginTypes)
	if err != nil {
		return nil, err
	}
	cat := model.PluginCategory{ID: s.id(), Name: name, IconKey: iconKey, PluginTypes: raw, SortOrder: sortOrder, Status: 1}
	if err := s.repo.CreateCategory(ctx, cat); err != nil {
		return nil, mapCategoryErr(err)
	}
	return &cat, nil
}

// AdminUpdateCategory mutates an existing category's editable fields.
func (s *Categories) AdminUpdateCategory(ctx context.Context, id, name, iconKey string, pluginTypes []model.PluginType, sortOrder int) (*model.PluginCategory, error) {
	if strings.TrimSpace(id) == "" {
		return nil, ErrInvalidRequest
	}
	name = strings.TrimSpace(name)
	raw, err := marshalPluginTypes(name, pluginTypes)
	if err != nil {
		return nil, err
	}
	cat := model.PluginCategory{ID: id, Name: name, IconKey: iconKey, PluginTypes: raw, SortOrder: sortOrder}
	if err := s.repo.UpdateCategory(ctx, cat); err != nil {
		return nil, mapCategoryErr(err)
	}
	return &cat, nil
}

// AdminDeleteCategory soft-deletes a category, surfacing ErrConflict while a
// live plugin still references it.
func (s *Categories) AdminDeleteCategory(ctx context.Context, id string) error {
	if strings.TrimSpace(id) == "" {
		return ErrInvalidRequest
	}
	return mapCategoryErr(s.repo.DeleteCategory(ctx, id))
}

// marshalPluginTypes validates a category write payload: a non-empty name and a
// non-empty list of valid plugin types, marshaled to the stored JSON array.
func marshalPluginTypes(name string, pluginTypes []model.PluginType) (json.RawMessage, error) {
	if name == "" || len(pluginTypes) == 0 {
		return nil, ErrInvalidRequest
	}
	for _, t := range pluginTypes {
		if !validPluginType(t) {
			return nil, ErrInvalidRequest
		}
	}
	raw, err := json.Marshal(pluginTypes)
	if err != nil {
		return nil, ErrInvalidRequest
	}
	return raw, nil
}

// mapCategoryErr translates repository sentinels into the service's error set.
func mapCategoryErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pluginrepo.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, pluginrepo.ErrConflict):
		return ErrConflict
	default:
		return err
	}
}
