package main

import (
	"context"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestListLegacyAuthConfigs_PrefersMostRecentlyUpdated verifies that when
// multiple open config:auth beads share the same project label (common in
// E2E fixtures that reuse "project:e2e-test" across runs), the
// most-recently-updated bead wins. First-wins semantics let stale beads
// from prior runs mask the current run's config and silenced the
// controller-driven failover path (gd-L2ONYcGQO8).
func TestListLegacyAuthConfigs_PrefersMostRecentlyUpdated(t *testing.T) {
	now := time.Now().UTC()
	stale := authConfig("gd-stale", "e2e-test", "acct-stale-a")
	stale.UpdatedAt = now.Add(-2 * time.Hour)
	stale.Description = `{"account":"acct-stale-a"}`
	current := authConfig("gd-current", "e2e-test", "acct-current-a")
	current.UpdatedAt = now
	current.Description = `{"account":"acct-current-a"}`

	store := newMockStore(stale, current)
	srv := mockDaemonServer(t, store)
	defer srv.Close()
	client := newClient(t, srv)

	got, err := listLegacyAuthConfigs(context.Background(), client)
	if err != nil {
		t.Fatalf("listLegacyAuthConfigs: %v", err)
	}
	cfg, ok := got["e2e-test"]
	if !ok {
		t.Fatalf("expected map entry for project e2e-test, got %v", got)
	}
	if cfg.ID != "gd-current" {
		t.Errorf("listLegacyAuthConfigs picked %s, want gd-current (most recently updated)", cfg.ID)
	}
}

// TestFindAuthConfigForProject_PrefersMostRecentlyUpdated covers the same
// stale-fixture scenario for the per-project lookup helper.
func TestFindAuthConfigForProject_PrefersMostRecentlyUpdated(t *testing.T) {
	now := time.Now().UTC()
	stale := authConfig("gd-stale", "e2e-test", "acct-stale-a")
	stale.UpdatedAt = now.Add(-3 * time.Hour)
	current := authConfig("gd-current", "e2e-test", "acct-current-a")
	current.UpdatedAt = now

	store := newMockStore(stale, current)
	srv := mockDaemonServer(t, store)
	defer srv.Close()
	client := newClient(t, srv)

	cfg, err := findAuthConfigForProject(context.Background(), client, "e2e-test")
	if err != nil {
		t.Fatalf("findAuthConfigForProject: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected a config bead, got nil")
	}
	if cfg.ID != "gd-current" {
		t.Errorf("findAuthConfigForProject picked %s, want gd-current (most recently updated)", cfg.ID)
	}
}

// TestPrimaryStuck_RecognizesRejectedThrottleStatus verifies that an
// account whose throttle_status is "rejected" but whose status field has
// not been promoted to "exhausted" (e.g. because authFailover.Handle
// bailed early without a fallback to switch to) is still classified as
// stuck. The retry path must see this state to fire when the chain is
// later repaired.
func TestPrimaryStuck_RecognizesRejectedThrottleStatus(t *testing.T) {
	now := time.Now()
	rejected := &beadsapi.BeadDetail{
		Title: "primary",
		Fields: map[string]string{
			"status":          "active",
			"throttle_status": "rejected",
		},
	}
	if !primaryStuck(rejected, now) {
		t.Error("primaryStuck should be true when throttle_status=rejected even if status=active")
	}

	healthy := &beadsapi.BeadDetail{
		Title: "primary",
		Fields: map[string]string{
			"status":          "active",
			"throttle_status": "allowed",
		},
	}
	if primaryStuck(healthy, now) {
		t.Error("primaryStuck should be false for an active+allowed account")
	}

	cooled := &beadsapi.BeadDetail{
		Title: "primary",
		Fields: map[string]string{
			"status":         "exhausted",
			"cooldown_until": now.Add(time.Hour).Format(time.RFC3339),
		},
	}
	if !primaryStuck(cooled, now) {
		t.Error("primaryStuck should be true for an account in active cooldown")
	}

	expired := &beadsapi.BeadDetail{
		Title: "primary",
		Fields: map[string]string{
			"status":          "exhausted",
			"cooldown_until":  now.Add(-time.Hour).Format(time.RFC3339),
			"throttle_status": "allowed",
		},
	}
	if primaryStuck(expired, now) {
		t.Error("primaryStuck should be false once cooldown has expired and throttle is allowed")
	}
}

// TestRetryStuckFailovers_FiresOnRejectedPrimaryWithoutCooldown is the
// regression test for scenario 3 in test-credential-rotation.sh: project
// stuck on a primary whose chain has no healthy fallback, where the
// failover handler bailed without persisting a cooldown. After the chain
// is fixed and the recovered account is back online, retryStuckFailovers
// must still fire because the primary's throttle_status remains
// "rejected" even though status stayed "active".
func TestRetryStuckFailovers_FiresOnRejectedPrimaryWithoutCooldown(t *testing.T) {
	store := newMockStore(
		authConfig("gd-cfg", "stuck-proj", "acct-c"),
		// C is throttled (rejected) but status was never promoted to
		// exhausted because Handle found no fallback. Chain has been
		// repaired post-hoc to point at B.
		accountNoProject("gd-c", "acct-c", map[string]any{
			"provider":        "anthropic",
			"status":          "active",
			"throttle_status": "rejected",
			"fallback":        "acct-b",
		}),
		// B has just recovered.
		accountNoProject("gd-b", "acct-b", map[string]any{
			"provider":        "anthropic",
			"status":          "active",
			"throttle_status": "allowed",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()
	client := newClient(t, srv)

	f := newAuthFailover(client, nil, testFailoverLogger(), 0, 0)
	l := newProbeEventListener(client, f, testFailoverLogger())

	l.retryStuckFailovers(context.Background(), "acct-b")

	// The legacy bead is read-only for the controller; the retry's effect is
	// visible in the runtime state.
	if got := parseAuthConfigAccount(store.description("gd-cfg")); got != "acct-c" {
		t.Errorf("config:auth account = %q, want acct-c (controller must not rewrite it)", got)
	}
	state, ok := f.runtime.Get("stuck-proj")
	if !ok {
		t.Fatal("expected runtime state for stuck-proj after retry")
	}
	if state.Current != "acct-b" {
		t.Errorf("runtime Current = %q, want acct-b", state.Current)
	}
}
