package main

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/subscriber"
)

// Both spec builders — buildAgentPodSpec (events/SSE spawn path) and
// BuildSpecFromBeadInfo (reconciler path) — must resolve an agent-level
// metadata["runtime_class"] pin identically. The events path historically
// dropped this overlay entirely, so a channel_runtime_class=runsc pin stamped
// by the bridge silently no-oped: the pod stayed runc in the baseline namespace
// and routeGVisorNamespace/markFreezeEligible (both keyed off RuntimeClassName)
// never fired. Reconciler-created pods (BuildSpecFromBeadInfo) applied the same
// pin correctly, so the two builders drifted (bug ka-0000005431 — same drift
// class as ka-0000003657 namespace routing and ka-0000001990 service_account).
//
// This table asserts the two builders produce identical RuntimeClassName AND
// Namespace across the layers that can set (or should NOT set) the runtime
// class. It is the regression guard for the whole events-vs-reconciler
// runtime_class drift class: if a future refactor mirrors the overlay to only
// one builder again, one of these rows fails.
func TestBothBuilders_RuntimeClassParity(t *testing.T) {
	// base is a two-namespace config (baseline + gVisor) with no runtime_class
	// pinned at any layer, so each case adds exactly the layer it exercises.
	base := func() *config.Config {
		return &config.Config{
			Namespace:       "gardenio-system",
			GVisorNamespace: "gardenio-gvisor",
			ProjectCache:    map[string]config.ProjectCacheEntry{},
			RoleCache:       map[string]config.RoleCacheEntry{},
		}
	}

	cases := []struct {
		name          string
		cfg           *config.Config
		project       string
		mode          string
		role          string
		metadata      map[string]string
		wantRuntime   string
		wantNamespace string
	}{
		{
			// The measured bug: an agent-level runtime_class=runsc pin (as the
			// bridge stamps from channel_runtime_class) must land runsc + route
			// into the gVisor namespace on BOTH paths.
			name:          "agent-metadata runsc pin",
			cfg:           base(),
			project:       "gardenio",
			mode:          "crew",
			role:          "thread",
			metadata:      map[string]string{"runtime_class": podmanager.RuntimeClassRunsc},
			wantRuntime:   podmanager.RuntimeClassRunsc,
			wantNamespace: "gardenio-gvisor",
		},
		{
			// No pin anywhere — both builders leave RuntimeClassName empty and
			// keep the agent in the baseline namespace (runc).
			name:          "no runtime_class anywhere",
			cfg:           base(),
			project:       "gardenio",
			mode:          "crew",
			role:          "crew",
			metadata:      map[string]string{},
			wantRuntime:   "",
			wantNamespace: "gardenio-system",
		},
		{
			// Role-level pin (no agent metadata) must resolve identically.
			name: "role-level runsc pin",
			cfg: &config.Config{
				Namespace:       "gardenio-system",
				GVisorNamespace: "gardenio-gvisor",
				ProjectCache:    map[string]config.ProjectCacheEntry{},
				RoleCache: map[string]config.RoleCacheEntry{
					"gvisor-poc": {RuntimeClassName: podmanager.RuntimeClassRunsc},
				},
			},
			project:       "gardenio",
			mode:          "crew",
			role:          "gvisor-poc",
			metadata:      map[string]string{},
			wantRuntime:   podmanager.RuntimeClassRunsc,
			wantNamespace: "gardenio-gvisor",
		},
		{
			// Agent metadata is the highest layer — it overrides a role pin on
			// both paths.
			name: "agent metadata overrides role pin",
			cfg: &config.Config{
				Namespace:       "gardenio-system",
				GVisorNamespace: "gardenio-gvisor",
				ProjectCache:    map[string]config.ProjectCacheEntry{},
				RoleCache: map[string]config.RoleCacheEntry{
					"gvisor-poc": {RuntimeClassName: podmanager.RuntimeClassRunsc},
				},
			},
			project:       "gardenio",
			mode:          "crew",
			role:          "gvisor-poc",
			metadata:      map[string]string{"runtime_class": "kata"},
			wantRuntime:   "kata",
			wantNamespace: "gardenio-system", // kata is not runsc, so no gVisor routing
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Events/SSE spawn path.
			eventsSpec := buildAgentPodSpec(tc.cfg, subscriber.Event{
				Project:   tc.project,
				Mode:      tc.mode,
				Role:      tc.role,
				AgentName: "parity-agent",
				Metadata:  tc.metadata,
			})

			// Reconciler path.
			reconSpec := BuildSpecFromBeadInfo(tc.cfg, tc.project, tc.mode, tc.role, "parity-agent", tc.metadata)

			// The two builders must agree.
			if eventsSpec.RuntimeClassName != reconSpec.RuntimeClassName {
				t.Errorf("RuntimeClassName drift: events=%q reconciler=%q",
					eventsSpec.RuntimeClassName, reconSpec.RuntimeClassName)
			}
			if eventsSpec.Namespace != reconSpec.Namespace {
				t.Errorf("Namespace drift: events=%q reconciler=%q",
					eventsSpec.Namespace, reconSpec.Namespace)
			}

			// And they must match the expected resolution.
			if eventsSpec.RuntimeClassName != tc.wantRuntime {
				t.Errorf("RuntimeClassName = %q, want %q", eventsSpec.RuntimeClassName, tc.wantRuntime)
			}
			if eventsSpec.Namespace != tc.wantNamespace {
				t.Errorf("Namespace = %q, want %q", eventsSpec.Namespace, tc.wantNamespace)
			}
		})
	}
}
