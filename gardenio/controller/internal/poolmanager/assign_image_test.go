package poolmanager

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// imageGateConfig returns a Config whose project cache overrides the agent image
// for the "custom-img" project and leaves "gardenio" on the base image, so the
// image gate in AssignPrewarmed can be exercised (W4 image unification,
// ka-0000001019). CoopImage is set so effectiveImage/base resolution is realistic;
// the gate itself compares only the override layer.
func imageGateConfig() *config.Config {
	cfg := testConfig()
	cfg.CoopImage = "registry.example/agent:base"
	cfg.ProjectCache["custom-img"] = config.ProjectCacheEntry{
		Image: "registry.example/agent:custom",
	}
	// A base-image project (no Image override).
	cfg.ProjectCache["plainproj"] = config.ProjectCacheEntry{}
	return cfg
}

// TestAssignPrewarmed_GenericServesBaseImageRequest verifies the image gate is a
// no-op for the base-image case that is the state today: a generic-warm pod
// (which runs the single base image) serves a request for a project that has no
// image override.
func TestAssignPrewarmed_GenericServesBaseImageRequest(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic", map[string]string{
			"project": GenericPoolValue,
			"role":    GenericPoolValue,
		}),
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, imageGateConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "plainproj",
		Role:    "crew",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-generic" {
		t.Errorf("expected generic base-image pod gd-generic to serve a base-image request, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_GenericRejectedForImageOverride is the core W4 guard: a
// base-image generic-warm pod must NOT be handed to a project that overrides the
// image, because the container image cannot be swapped on a running pod. With no
// image-matching pod in the pool the request falls through to a cold mint
// (ErrPoolEmpty), never a wrong-image warm assign.
func TestAssignPrewarmed_GenericRejectedForImageOverride(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic", map[string]string{
			"project": GenericPoolValue,
			"role":    GenericPoolValue,
		}),
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, imageGateConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "custom-img",
		Role:    "crew",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty when only a base-image generic pod exists for an image-override project, got: %v", err)
	}
}

// TestAssignPrewarmed_ImageOverridePodServesItsProject verifies the matching
// direction: a dedicated pod whose (project,role) resolves to the SAME image
// override serves that project's request.
func TestAssignPrewarmed_ImageOverridePodServesItsProject(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-custom",
			"title": "prewarmed-custom",
			"fields": map[string]string{
				"agent": "prewarmed-custom", "agent_state": "prewarmed",
				"mode": "crew", "role": "crew", "project": "custom-img",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, imageGateConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "custom-img",
		Role:    "crew",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-custom" {
		t.Errorf("expected image-override pod gd-custom to serve its own project, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_ImageOverridePodRejectedForBaseRequest verifies the other
// exclusion direction: an override-image pod must NOT serve a base-image request,
// so a base-image request never grabs a pod running the wrong image.
func TestAssignPrewarmed_ImageOverridePodRejectedForBaseRequest(t *testing.T) {
	agents := []map[string]any{
		{
			"id":    "gd-custom",
			"title": "prewarmed-custom",
			"fields": map[string]string{
				"agent": "prewarmed-custom", "agent_state": "prewarmed",
				"mode": "crew", "role": "crew", "project": "custom-img",
			},
		},
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, imageGateConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "plainproj",
		Role:    "crew",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty when only an override-image pod exists for a base-image request, got: %v", err)
	}
}

// TestEffectiveImageOverride_LayersAndBaseExclusion unit-tests the resolver: base
// image is excluded (empty when no override), project override applies, and a
// role override supersedes the project layer.
func TestEffectiveImageOverride_LayersAndBaseExclusion(t *testing.T) {
	cfg := testConfig()
	cfg.CoopImage = "registry.example/agent:base"
	cfg.ProjectCache["p"] = config.ProjectCacheEntry{Image: "img:proj"}
	cfg.ProjectCache["plain"] = config.ProjectCacheEntry{}
	cfg.RoleCache = map[string]config.RoleCacheEntry{
		"r": {Image: "img:role"},
	}
	pm := New(nil, cfg, slog.Default())

	if got := pm.effectiveImageOverride("plain", ""); got != "" {
		t.Errorf("base-image project must resolve to empty override, got %q", got)
	}
	if got := pm.effectiveImageOverride("unknown", ""); got != "" {
		t.Errorf("unknown project must resolve to empty override, got %q", got)
	}
	if got := pm.effectiveImageOverride("p", ""); got != "img:proj" {
		t.Errorf("project override = img:proj, got %q", got)
	}
	if got := pm.effectiveImageOverride("p", "r"); got != "img:role" {
		t.Errorf("role override must supersede project, got %q", got)
	}
}
