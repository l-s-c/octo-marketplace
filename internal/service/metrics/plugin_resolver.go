package metrics

import (
	"context"
	"errors"

	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

// PluginService is the subset of the unified plugin service needed for
// visibility checks.
type PluginService interface {
	Detail(ctx context.Context, caller pluginsvc.Caller, pluginID string, includeRelations bool) (*pluginsvc.Detail, error)
}

// PluginResolver checks whether a unified Plugin exists and is visible to the
// caller, so metric tracking cannot probe cross-Space existence.
type PluginResolver struct {
	pluginSvc PluginService
}

// NewPluginResolver creates a PluginResolver.
func NewPluginResolver(pluginSvc PluginService) *PluginResolver {
	return &PluginResolver{pluginSvc: pluginSvc}
}

// CanView returns true if the Plugin exists and is visible to the caller.
// Not-found, out-of-scope, and malformed IDs all map to (false, nil) so the
// tracking surface answers them identically; internal errors propagate.
func (r *PluginResolver) CanView(ctx context.Context, resourceID string, caller Caller) (bool, error) {
	detail, err := r.pluginSvc.Detail(ctx, pluginsvc.Caller{UID: caller.UID, SpaceID: caller.SpaceID}, resourceID, false)
	if err != nil {
		if errors.Is(err, pluginsvc.ErrNotFound) || errors.Is(err, pluginsvc.ErrInvalidRequest) {
			return false, nil
		}
		return false, err
	}
	return detail != nil && detail.Plugin != nil, nil
}
