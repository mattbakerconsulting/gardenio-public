package main

// Env-injection guards for GARDENIO_DEFAULT_HOST (ka-0000002401, R3 §4.4/§8.4).
// The gb CLI inside an agent pod otherwise derives its zero-config daemon/viewer
// hosts from the COMPILED default baked into the binary (cmd/gb/main.go
// resolveGardenioHost), which on new prod handed humans legacy-host links. The
// controller now forwards config.GardenioDefaultHost — a bare host derived by the
// chart from the single `unifiedHost` knob — onto every agent pod as
// GARDENIO_DEFAULT_HOST. Injection is GATED: empty ⇒ not set, so gb keeps its
// compiled fallback (today's behaviour) until the coupled default-URL cutover
// (ka-0000004747). These guards pin both the set and unset arms on the common
// config path (applyCommonConfig) and the two spec builders.

import (
	"testing"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

func TestApplyCommonConfig_GardenioDefaultHost(t *testing.T) {
	cfg := &config.Config{
		GardenioDefaultHost: "prod.gardenio.acme.example",
		ProjectCache:        map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if spec.Env["GARDENIO_DEFAULT_HOST"] != "prod.gardenio.acme.example" {
		t.Errorf("expected GARDENIO_DEFAULT_HOST=prod.gardenio.acme.example, got %q", spec.Env["GARDENIO_DEFAULT_HOST"])
	}
}

func TestApplyCommonConfig_NoGardenioDefaultHost(t *testing.T) {
	// Gated: unconfigured (empty) must NOT set the env var, so the in-pod gb
	// falls back to its compiled default exactly as before this wiring.
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{},
	}
	spec := &podmanager.AgentPodSpec{
		Project: "myproject",
		Env:     map[string]string{},
	}
	applyCommonConfig(cfg, spec)

	if _, present := spec.Env["GARDENIO_DEFAULT_HOST"]; present {
		t.Error("GARDENIO_DEFAULT_HOST must not be set when unconfigured (gated)")
	}
}

func TestBuildSpecFromBeadInfo_GardenioDefaultHost(t *testing.T) {
	// Reconciler path (pod restart) must carry the same binding as the events
	// path, so a restart never drops GARDENIO_DEFAULT_HOST.
	cfg := &config.Config{
		Namespace:           "test",
		CoopImage:           "agent:latest",
		GardenioDefaultHost: "prod.gardenio.acme.example",
		ProjectCache:        map[string]config.ProjectCacheEntry{},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{})

	if spec.Env["GARDENIO_DEFAULT_HOST"] != "prod.gardenio.acme.example" {
		t.Errorf("expected GARDENIO_DEFAULT_HOST on the reconciler path, got %q", spec.Env["GARDENIO_DEFAULT_HOST"])
	}
}
