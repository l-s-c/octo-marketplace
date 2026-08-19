package plugin

import (
	"context"
	"errors"
	"strings"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginrepo "github.com/Mininglamp-OSS/octo-marketplace/internal/repository/plugin"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

type categoryRepository interface {
	ListPlacementCategories(context.Context, pluginrepo.Scope, string, model.PluginType) ([]model.PluginCategory, error)
}

// RepositoryCategories adapts the repository's placement-aware category read to
// the handler boundary while retaining trusted service Caller semantics.
type RepositoryCategories struct{ repo categoryRepository }

func NewRepositoryCategories(repo categoryRepository) *RepositoryCategories {
	return &RepositoryCategories{repo: repo}
}

func (s *RepositoryCategories) ListCategories(ctx context.Context, caller pluginsvc.Caller, placement string, typ model.PluginType) ([]model.PluginCategory, error) {
	if strings.TrimSpace(caller.UID) == "" || strings.TrimSpace(caller.SpaceID) == "" || strings.TrimSpace(placement) == "" {
		return nil, pluginsvc.ErrInvalidRequest
	}
	switch typ {
	case model.PluginTypeExpert, model.PluginTypeExpertTeam, model.PluginTypeSkill, model.PluginTypeConnector:
	default:
		return nil, pluginsvc.ErrInvalidRequest
	}
	items, err := s.repo.ListPlacementCategories(ctx, pluginrepo.Scope{CallerUID: caller.UID, SpaceID: caller.SpaceID}, placement, typ)
	if errors.Is(err, pluginrepo.ErrNotFound) {
		return nil, pluginsvc.ErrNotFound
	}
	return items, err
}
