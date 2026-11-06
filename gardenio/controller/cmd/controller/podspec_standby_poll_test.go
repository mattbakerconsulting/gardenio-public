package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/config"
)

// A prewarmed agent pod polls its own bead for the prewarmed→assigning flip.
// That poll interval (BOAT_STANDBY_POLL) is the dominant term in user-perceived
// warm-pool spawn latency, so the controller must inject it on the pod spec
// from cfg.StandbyPoll (ka-0000003812).
func TestBuildSpecFromBeadInfo_StandbyPollInjected(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "default-image",
		StandbyPoll:  500 * time.Millisecond,
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache:    map[string]config.RoleCacheEntry{},
		AccountCache: map[string]config.AccountCacheEntry{},
	}

	spec := BuildSpecFromBeadInfo(cfg, "myproject", "crew", "crew", "test-agent",
		map[string]string{"agent_state": "prewarmed"})

	if !spec.Prewarmed {
		t.Fatalf("expected Prewarmed=true for agent_state=prewarmed")
	}
	if got := spec.Env["BOAT_STANDBY"]; got != "true" {
		t.Errorf("BOAT_STANDBY: got %q, want %q", got, "true")
	}
	if got := spec.Env["BOAT_STANDBY_POLL"]; got != "500ms" {
		t.Errorf("BOAT_STANDBY_POLL: got %q, want %q", got, "500ms")
	}
}

// A non-prewarmed agent must NOT carry standby env — the poll override only
// applies to pods parked in the pool.
func TestBuildSpecFromBeadInfo_StandbyPollOnlyWhenPrewarmed(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "default-image",
		StandbyPoll:  500 * time.Millisecond,
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache:    map[string]config.RoleCacheEntry{},
		AccountCache: map[string]config.AccountCacheEntry{},
	}

	spec := BuildSpecFromBeadInfo(cfg, "myproject", "crew", "crew", "test-agent",
		map[string]string{})

	if spec.Prewarmed {
		t.Fatalf("expected Prewarmed=false without agent_state=prewarmed")
	}
	if got, ok := spec.Env["BOAT_STANDBY_POLL"]; ok {
		t.Errorf("BOAT_STANDBY_POLL should be unset for non-prewarmed pod, got %q", got)
	}
}

// A zero StandbyPoll (unconfigured) must leave BOAT_STANDBY_POLL unset so the
// pod-side default governs, rather than injecting an invalid "0s".
func TestBuildSpecFromBeadInfo_StandbyPollZeroOmitted(t *testing.T) {
	cfg := &config.Config{
		Namespace:    "test",
		CoopImage:    "default-image",
		StandbyPoll:  0,
		ProjectCache: map[string]config.ProjectCacheEntry{},
		RoleCache:    map[string]config.RoleCacheEntry{},
		AccountCache: map[string]config.AccountCacheEntry{},
	}

	spec := BuildSpecFromBeadInfo(cfg, "myproject", "crew", "crew", "test-agent",
		map[string]string{"agent_state": "prewarmed"})

	if got, ok := spec.Env["BOAT_STANDBY_POLL"]; ok {
		t.Errorf("BOAT_STANDBY_POLL should be omitted when StandbyPoll=0, got %q", got)
	}
}
