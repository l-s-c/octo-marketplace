package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/id"
	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

type categoryStore interface {
	ListPlacementCategories(context.Context, pluginrepo.Scope, string, model.PluginType) ([]model.PluginCategory, error)
	ListAdminCategories(context.Context, model.PluginType) ([]model.PluginCategory, error)
	CreateCategory(ctx context.Context, id, name, iconKey string, pluginTypesJSON []byte, sortOrder int) error
	UpdateCategory(ctx context.Context, id, name, iconKey string, pluginTypesJSON []byte, sortOrder int) error
	DeleteCategory(ctx context.Context, id string) (int, error)
}

// Categories serves the placement-aware category read, which stays separate
// from Service because it needs only the read-side repository.
type Categories struct {
	repo categoryStore
	id   func() string
}

func NewCategories(repo categoryStore) *Categories {
	return &Categories{repo: repo, id: id.New}
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

// ErrCategoryInUse is returned when a category delete is blocked by live plugins
// still referencing it; the count of blocking plugins is carried for the caller.
var ErrCategoryInUse = errors.New("plugin category in use")

// validCategoryTypes reports whether every type is a known plugin type and the
// list is non-empty.
func validCategoryTypes(types []model.PluginType) bool {
	if len(types) == 0 {
		return false
	}
	for _, t := range types {
		if !validPluginType(t) {
			return false
		}
	}
	return true
}

// AdminListCategories lists every category applicable to a plugin type with its
// live-plugin count (admin taxonomy management).
func (s *Categories) AdminListCategories(ctx context.Context, typ model.PluginType) ([]model.PluginCategory, error) {
	if !validPluginType(typ) {
		return nil, ErrInvalidRequest
	}
	return s.repo.ListAdminCategories(ctx, typ)
}

// AdminCreateCategory mints a taxonomy category and returns its generated id.
func (s *Categories) AdminCreateCategory(ctx context.Context, name, iconKey string, types []model.PluginType, sortOrder int) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len([]rune(name)) > 128 || !validCategoryTypes(types) || sortOrder < 0 {
		return "", ErrInvalidRequest
	}
	typesJSON, err := json.Marshal(types)
	if err != nil {
		return "", ErrInvalidRequest
	}
	catID := s.id()
	if err := s.repo.CreateCategory(ctx, catID, name, strings.TrimSpace(iconKey), typesJSON, sortOrder); err != nil {
		return "", mapStoreError(err)
	}
	return catID, nil
}

// AdminUpdateCategory updates a taxonomy category by id.
func (s *Categories) AdminUpdateCategory(ctx context.Context, id, name, iconKey string, types []model.PluginType, sortOrder int) error {
	name = strings.TrimSpace(name)
	if strings.TrimSpace(id) == "" || name == "" || len([]rune(name)) > 128 || !validCategoryTypes(types) || sortOrder < 0 {
		return ErrInvalidRequest
	}
	typesJSON, err := json.Marshal(types)
	if err != nil {
		return ErrInvalidRequest
	}
	return mapStoreError(s.repo.UpdateCategory(ctx, id, name, strings.TrimSpace(iconKey), typesJSON, sortOrder))
}

// AdminDeleteCategory soft-deletes a category, returning the blocking-plugin
// count wrapped in ErrCategoryInUse when it is still referenced.
func (s *Categories) AdminDeleteCategory(ctx context.Context, id string) (int, error) {
	if strings.TrimSpace(id) == "" {
		return 0, ErrInvalidRequest
	}
	count, err := s.repo.DeleteCategory(ctx, id)
	if errors.Is(err, pluginrepo.ErrCategoryInUse) {
		return count, ErrCategoryInUse
	}
	return 0, mapStoreError(err)
}
