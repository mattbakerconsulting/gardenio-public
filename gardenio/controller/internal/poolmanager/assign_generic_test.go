package poolmanager

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
)

// genericAgent builds a prewarmed agent bead with the given project/role/runtime
// fields, omitting empty ones so a truly generic (no project/role) member can be
// modeled. Used by the W1 generic-warm assign tests (ka-0000007566).
func genericAgent(id string, fields map[string]string) map[string]any {
	f := map[string]string{
		"agent":       id,
		"agent_state": "prewarmed",
		"mode":        "crew",
	}
	for k, v := range fields {
		if v != "" {
			f[k] = v
		}
	}
	return map[string]any{"id": id, "title": id, "fields": f}
}

// TestAssignPrewarmed_GenericSentinelServesAnyProjectRole verifies the lifted
// filter (ka-0000007566): a member carrying the "*" sentinel in project AND role
// is assignable to a concrete (project,role) request that no dedicated pod
// matches — the core "one pool serves many" behavior.
func TestAssignPrewarmed_GenericSentinelServesAnyProjectRole(t *testing.T) {
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
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "site-deployment",
		Role:    "reviewer",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-generic" {
		t.Errorf("expected generic pod gd-generic to serve the request, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_EmptyFieldsAreGeneric verifies a member with UNSET
// project/role (empty, not the explicit sentinel) is treated as generic, so a
// pool warmed without project/role set needs no migration to serve many.
func TestAssignPrewarmed_EmptyFieldsAreGeneric(t *testing.T) {
	agents := []map[string]any{
		// No project or role fields at all.
		genericAgent("gd-bare", map[string]string{}),
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "kbeads",
		Role:    "crew",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-bare" {
		t.Errorf("expected bare (empty-field) pod gd-bare to serve the request, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_DedicatedPodStillExactMatch is the regression guard: the
// lift must NOT make a project-A dedicated pod serve a project-B request. A
// dedicated pod (concrete project, not sentinel) still only serves its own pool.
func TestAssignPrewarmed_DedicatedPodStillExactMatch(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-dedicated-A", map[string]string{
			"project": "gardenio",
			"role":    "thread",
		}),
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "site-deployment", // different project
		Role:    "thread",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty — a dedicated project pod must not serve a foreign project, got: %v", err)
	}
}

// genericRuntimeConfig pins the "gvisor-poc" role to runsc so effectiveRuntime
// Class resolution can be exercised as a fallback, while the generic-pod tests
// rely on the stamped runtime_class metadata instead.
func genericRuntimeConfig() *config.Config {
	cfg := testConfig()
	cfg.RoleCache = map[string]config.RoleCacheEntry{
		"gvisor-poc": {RuntimeClassName: "runsc"},
	}
	return cfg
}

// TestAssignPrewarmed_GenericRuntimeGateMatchesStamped verifies the runtime gate
// is NOT lifted for generic pods and reads the pod's ACTUAL baked runtime: a
// generic pod stamped runtime_class=runsc serves a runsc request, while a
// generic runc pod (no stamp) does not — so a runsc channel never grabs a runc
// generic pod (ka-0000007566, preserving ka-0000004563).
func TestAssignPrewarmed_GenericRuntimeGateMatchesStamped(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic-runc", map[string]string{
			"project": GenericPoolValue,
			"role":    GenericPoolValue,
			// no runtime_class → cluster default (runc)
		}),
		genericAgent("gd-generic-runsc", map[string]string{
			"project":       GenericPoolValue,
			"role":          GenericPoolValue,
			"runtime_class": "runsc",
		}),
	}
	srv := mockDaemon(t, agents)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, genericRuntimeConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project:             "gardenio",
		Role:                "gvisor-poc",
		RequireRuntimeClass: "runsc",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-generic-runsc" {
		t.Errorf("runsc request must claim the runsc-stamped generic pod, got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_GenericRuntimeGateNoMatch verifies a runsc request finds
// no candidate when the pool holds only a generic RUNC pod — it falls through to
// a cold mint rather than grabbing a runtime-incompatible generic pod.
func TestAssignPrewarmed_GenericRuntimeGateNoMatch(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic-runc", map[string]string{
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
	pm := New(daemon, genericRuntimeConfig(), slog.Default())

	_, err = pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project:             "gardenio",
		Role:                "gvisor-poc",
		RequireRuntimeClass: "runsc",
	})
	if err != ErrPoolEmpty {
		t.Fatalf("expected ErrPoolEmpty — a generic runc pod must not serve a runsc request, got: %v", err)
	}
}

// TestAssignPrewarmed_StampsSpecializationOnGenericPod verifies the assign-time
// specialization write (ka-0000007566): picking a generic pod for a concrete
// (project,role) stamps project, role, specialized_from_generic=true, and
// recredential_project in the SAME atomic update that flips agent_state.
func TestAssignPrewarmed_StampsSpecializationOnGenericPod(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic", map[string]string{
			"project": GenericPoolValue,
			"role":    GenericPoolValue,
		}),
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "site-deployment",
		Role:    "reviewer",
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if got, _ := captured["project"].(string); got != "site-deployment" {
		t.Errorf("project = %q, want site-deployment", got)
	}
	if got, _ := captured["role"].(string); got != "reviewer" {
		t.Errorf("role = %q, want reviewer", got)
	}
	if got, _ := captured["specialized_from_generic"].(string); got != "true" {
		t.Errorf("specialized_from_generic = %q, want true", got)
	}
	if got, _ := captured["recredential_project"].(string); got != "site-deployment" {
		t.Errorf("recredential_project = %q, want site-deployment", got)
	}
	if got, _ := captured["agent_state"].(string); got != "assigning" {
		t.Errorf("agent_state = %q, want assigning", got)
	}
}

// TestAssignPrewarmed_DedicatedPodNoSpecializationMarker verifies a DEDICATED
// pod (already project-scoped, holds the right creds) is NOT stamped with the
// re-credential marker — only a genuinely generic pod needs re-credentialing, so
// existing dedicated pools pay no swap.
func TestAssignPrewarmed_DedicatedPodNoSpecializationMarker(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-dedicated", map[string]string{
			"project": "gardenio",
			"role":    "thread",
		}),
	}
	var captured map[string]any
	var mu sync.Mutex
	srv := capturingDaemon(t, agents, &captured, &mu)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	pm := New(daemon, testConfig(), slog.Default())

	if _, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "gardenio",
		Role:    "thread",
	}); err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured == nil {
		t.Fatal("no PATCH captured — AssignPrewarmed did not update the bead")
	}
	if _, ok := captured["specialized_from_generic"]; ok {
		t.Errorf("dedicated pod must not carry specialized_from_generic, got %v", captured["specialized_from_generic"])
	}
	if _, ok := captured["recredential_project"]; ok {
		t.Errorf("dedicated pod must not carry recredential_project, got %v", captured["recredential_project"])
	}
	// Role is still stamped idempotently (same value it already had).
	if got, _ := captured["role"].(string); got != "thread" {
		t.Errorf("role = %q, want thread", got)
	}
}

// TestPoolDimensionMatches unit-tests the lifted matcher directly across the
// generic/empty/exact/mismatch cases so the filter's contract is pinned
// independent of the assign wiring.
func TestPoolDimensionMatches(t *testing.T) {
	cases := []struct {
		have, want string
		match      bool
	}{
		{"gardenio", "gardenio", true},     // exact
		{"gardenio", "kbeads", false},      // dedicated mismatch
		{"gardenio", "", true},             // unconstrained request
		{GenericPoolValue, "kbeads", true}, // generic sentinel serves any
		{"", "kbeads", true},               // empty (generic) serves any
		{GenericPoolValue, "", true},       // generic + unconstrained
	}
	for _, c := range cases {
		if got := poolDimensionMatches(c.have, c.want); got != c.match {
			t.Errorf("poolDimensionMatches(%q,%q) = %v, want %v", c.have, c.want, got, c.match)
		}
	}
}
