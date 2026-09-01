package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/Mininglamp-OSS/octo-marketplace/internal/model"
)

// The review gate is visibility and nothing else: a tenant plugin stays
// `private` until an approved review flips it to `space`. There are exactly two
// tenant write paths that could otherwise set the column directly, and if either
// one is open the entire workflow is bypassable in a single API call.
//
// Every assertion below is on the visibility that WOULD BE PERSISTED, not on an
// HTTP status: a handler that 200s while writing `private` is correct, and a
// handler that 400s while writing `space` would pass a status-only check.

func visibilityFixture(t *testing.T, current model.PluginVisibility) (*fakeStore, *Service) {
	t.Helper()
	space := "space-a"
	manifest := json.RawMessage(`{"$schema":"cowork-plugin-manifest-2.0.json","plugin_name":"Example Plugin","plugin_type":"expert","name":"example-plugin","description":"An example plugin.","labels":["one","two"],"examples":[]}`)
	pkg := json.RawMessage(`{"$schema":"cowork-plugin-package-2.0.json","attachments":[{"path":"AGENTS.md","content_type":"raw","mime_type":"text/markdown","raw_content":"# Example Plugin"}]}`)
	store := &fakeStore{
		relations: map[string][]model.PluginRelation{},
		plugins: map[string]*model.Plugin{
			"plugin-1": {
				ID: "plugin-1", Name: "Example Plugin", Type: model.PluginTypeExpert,
				OwnerUID: "user-1", SpaceID: &space, Visibility: current,
				Tags: json.RawMessage(`["one","two"]`), Manifest: manifest, Package: pkg,
			},
		},
	}
	return store, fixedService(store)
}

// A fresh tenant create lands private even when the client asks for `space` —
// this is the path the old upload modal used, with visibility hardcoded to
// "space".
func TestTenantCreateAlwaysLandsPrivate(t *testing.T) {
	for _, asked := range []model.PluginVisibility{
		model.PluginVisibilitySpace,
		model.PluginVisibilityPrivate,
		"",
	} {
		store := &fakeStore{}
		svc := fixedService(store)
		req := validRequest()
		req.Visibility = asked
		_, err := svc.Create(context.Background(), testCaller, req)
		if asked == "" {
			// An unset visibility was already invalid before this change and stays so;
			// what matters is that it does not become `space`.
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("empty visibility err = %v, want ErrInvalidRequest", err)
			}
			if store.create != nil {
				t.Fatalf("empty visibility persisted %q", store.create.Visibility)
			}
			continue
		}
		if err != nil {
			t.Fatalf("visibility=%q create err = %v", asked, err)
		}
		if store.create == nil {
			t.Fatalf("visibility=%q: nothing persisted", asked)
		}
		if store.create.Visibility != model.PluginVisibilityPrivate {
			t.Fatalf("asked for %q, PERSISTED %q; a tenant create must never mint an org-visible row", asked, store.create.Visibility)
		}
	}
}

// The second and last bypass: raising visibility on update. `space` -> `private`
// (self-delisting) and no-change must keep working, or authors cannot delist and
// every ordinary metadata edit of a listed plugin 400s.
func TestTenantUpdateCannotRaiseVisibility(t *testing.T) {
	tests := []struct {
		name      string
		current   model.PluginVisibility
		requested model.PluginVisibility
		wantErr   bool
		wantStore model.PluginVisibility
	}{
		{
			name:    "private draft cannot self-promote to space",
			current: model.PluginVisibilityPrivate, requested: model.PluginVisibilitySpace,
			wantErr: true,
		},
		{
			name:    "private draft cannot self-promote to system",
			current: model.PluginVisibilityPrivate, requested: model.PluginVisibilitySystem,
			wantErr: true,
		},
		{
			name:    "private stays private",
			current: model.PluginVisibilityPrivate, requested: model.PluginVisibilityPrivate,
			wantStore: model.PluginVisibilityPrivate,
		},
		// A listed plugin cannot be edited through this path at all any more; that
		// refusal has its own test (TestTenantCannotEditAListedPluginDirectly), and
		// its typed error is ErrListedRequiresReview rather than ErrInvalidRequest.
		{
			name:    "an author may delist their own listed plugin",
			current: model.PluginVisibilitySpace, requested: model.PluginVisibilityPrivate,
			wantStore: model.PluginVisibilityPrivate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, svc := visibilityFixture(t, tt.current)
			req := validRequest()
			req.Visibility = tt.requested
			_, err := svc.Update(context.Background(), testCaller, "plugin-1", req)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidRequest) {
					t.Fatalf("err = %v, want ErrInvalidRequest", err)
				}
				if store.update != nil {
					t.Fatalf("refused transition still PERSISTED visibility %q", store.update.Visibility)
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if store.update == nil {
				t.Fatal("nothing persisted")
			}
			if store.update.Visibility != tt.wantStore {
				t.Fatalf("persisted visibility = %q, want %q", store.update.Visibility, tt.wantStore)
			}
		})
	}
}

// tenantVisibilityAllowed is the predicate both call sites share; pinning it
// directly means a future edit to either call site cannot quietly widen it.
func TestTenantVisibilityAllowedMatrix(t *testing.T) {
	all := []model.PluginVisibility{
		model.PluginVisibilityPrivate,
		model.PluginVisibilitySpace,
		model.PluginVisibilitySystem,
		model.PluginVisibilityPublic,
	}
	for _, current := range all {
		for _, next := range all {
			want := next == model.PluginVisibilityPrivate || next == current
			if got := tenantVisibilityAllowed(current, next); got != want {
				t.Errorf("tenantVisibilityAllowed(%q,%q) = %v, want %v", current, next, got, want)
			}
		}
	}
	// The one transition the whole feature hangs on.
	if tenantVisibilityAllowed(model.PluginVisibilityPrivate, model.PluginVisibilitySpace) {
		t.Fatal("private -> space is allowed on the tenant path; the review gate is open")
	}
}

// The other half of the upgrade gate: once a plugin is listed, the ordinary write
// path must refuse it. Hiding the edit button removes the affordance; without
// this, the bypass is one curl away — same class as the upsert-visibility hole.
func TestTenantCannotEditAListedPluginDirectly(t *testing.T) {
	store, svc := visibilityFixture(t, model.PluginVisibilitySpace)
	req := validRequest()
	req.Visibility = model.PluginVisibilitySpace
	req.Publisher = "Someone else"
	_, err := svc.Update(context.Background(), testCaller, "plugin-1", req)
	if !errors.Is(err, ErrListedRequiresReview) {
		t.Fatalf("err = %v, want ErrListedRequiresReview", err)
	}
	if store.update != nil {
		t.Fatal("an unreviewed edit of a listed plugin was PERSISTED")
	}
}

// A private draft is still freely editable — that is the whole point of the
// private-drafts-are-free half of the design.
func TestTenantMayEditAPrivateDraft(t *testing.T) {
	store, svc := visibilityFixture(t, model.PluginVisibilityPrivate)
	req := validRequest()
	req.Visibility = model.PluginVisibilityPrivate
	if _, err := svc.Update(context.Background(), testCaller, "plugin-1", req); err != nil {
		t.Fatalf("editing a private draft was refused: %v", err)
	}
	if store.update == nil {
		t.Fatal("nothing persisted")
	}
}

// Delisting is how an author takes their plugin down to work on it, so a combined
// "delist + edit" call stays allowed: the content lands on a row nobody else sees.
func TestTenantMayDelistAndEditInOneCall(t *testing.T) {
	store, svc := visibilityFixture(t, model.PluginVisibilitySpace)
	req := validRequest()
	req.Visibility = model.PluginVisibilityPrivate
	if _, err := svc.Update(context.Background(), testCaller, "plugin-1", req); err != nil {
		t.Fatalf("delist-and-edit was refused: %v", err)
	}
	if store.update == nil {
		t.Fatal("nothing persisted")
	}
	if store.update.Visibility != model.PluginVisibilityPrivate {
		t.Fatalf("persisted visibility = %q, want private", store.update.Visibility)
	}
}
