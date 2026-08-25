package plugin

import (
	"context"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
)

type categoryStore interface {
	ListPlacementCategories(context.Context, pluginrepo.Scope, string, model.PluginType) ([]model.PluginCategory, error)
}

// Categories serves the placement-aware category read, which stays separate
// from Service because it needs only the read-side repository.
type Categories struct {
	repo categoryStore
}

func NewCategories(repo categoryStore) *Categories {
	return &Categories{repo: repo}
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

