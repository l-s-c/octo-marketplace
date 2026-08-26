package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
	pluginsvc "github.com/Mininglamp-OSS/octo-marketplace/internal/service/plugin"
)

type fakePluginService struct {
	caller pluginsvc.Caller
	detail *pluginsvc.Detail
	err    error
}

func (f *fakePluginService) Detail(_ context.Context, caller pluginsvc.Caller, _ string, _ bool) (*pluginsvc.Detail, error) {
	f.caller = caller
	return f.detail, f.err
}

func TestPluginResolverVisibleWhenDetailSucceeds(t *testing.T) {
	f := &fakePluginService{detail: &pluginsvc.Detail{Plugin: &model.Plugin{ID: "plugin-1"}}}
	ok, err := NewPluginResolver(f).CanView(context.Background(), "plugin-1", Caller{UID: "user-1", SpaceID: "space-a"})
	if err != nil || !ok {
		t.Fatalf("CanView = %v, %v", ok, err)
	}
	if f.caller.UID != "user-1" || f.caller.SpaceID != "space-a" {
		t.Fatalf("caller forwarded = %#v", f.caller)
	}
}

func TestPluginResolverMapsNotFoundAndInvalidToInvisible(t *testing.T) {
	for _, cause := range []error{pluginsvc.ErrNotFound, pluginsvc.ErrInvalidRequest} {
		ok, err := NewPluginResolver(&fakePluginService{err: cause}).CanView(context.Background(), "missing", Caller{UID: "u", SpaceID: "s"})
		if err != nil || ok {
			t.Fatalf("CanView(%v) = %v, %v", cause, ok, err)
		}
	}
}

func TestPluginResolverPropagatesInternalErrors(t *testing.T) {
	cause := errors.New("db down")
	ok, err := NewPluginResolver(&fakePluginService{err: cause}).CanView(context.Background(), "plugin-1", Caller{UID: "u", SpaceID: "s"})
	if ok || !errors.Is(err, cause) {
		t.Fatalf("CanView = %v, %v", ok, err)
	}
}
