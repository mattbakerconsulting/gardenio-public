package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// specializeDaemonStub serves a single GetBead with the given fields, so
// hydrateAssignmentEnv can be exercised against an assigned-and-specialized
// generic-warm pod bead (W1, ka-0000007566).
func specializeDaemonStub(t *testing.T, fields map[string]string) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/v1/beads/") {
			raw, _ := json.Marshal(fields)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "gd-agent1", "title": "generic-agent", "type": "agent",
				"status": "in_progress", "fields": json.RawMessage(raw),
			})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestHydrateAssignmentEnv_SpecializesGenericPod verifies the pod half of
// assign-time specialization: hydrateAssignmentEnv adopts the concrete
// project/role stamped by the pool into BOAT_PROJECT/BOAT_ROLE/ROLE/PROJECT and,
// on the re-credential marker, drops the sentinel naming the target project so
// the specialization applies on spawn (ka-0000007566).
func TestHydrateAssignmentEnv_SpecializesGenericPod(t *testing.T) {
	_ = os.Remove(specializedRecredentialSentinelPath)
	t.Cleanup(func() { _ = os.Remove(specializedRecredentialSentinelPath) })

	// Seed the env keys empty so t.Setenv restores them after the test even
	// though hydrateAssignmentEnv rewrites them via os.Setenv.
	t.Setenv("PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_ROLE", "")
	t.Setenv("ROLE", "")
	t.Setenv("BOAT_TASK_ID", "")

	oldDaemon := daemon
	daemon = specializeDaemonStub(t, map[string]string{
		"agent_state":              "assigning",
		"project":                  "site-deployment",
		"role":                     "reviewer",
		"task_id":                  "gd-task9",
		"specialized_from_generic": "true",
		"recredential_project":     "site-deployment",
	})
	t.Cleanup(func() { daemon = oldDaemon })

	hydrateAssignmentEnv(context.Background(), "gd-agent1")

	if got := os.Getenv("PROJECT"); got != "site-deployment" {
		t.Errorf("PROJECT = %q, want site-deployment", got)
	}
	if got := os.Getenv("BOAT_PROJECT"); got != "site-deployment" {
		t.Errorf("BOAT_PROJECT = %q, want site-deployment", got)
	}
	if got := os.Getenv("BOAT_ROLE"); got != "reviewer" {
		t.Errorf("BOAT_ROLE = %q, want reviewer", got)
	}
	if got := os.Getenv("ROLE"); got != "reviewer" {
		t.Errorf("ROLE = %q, want reviewer", got)
	}
	if got := os.Getenv("BOAT_TASK_ID"); got != "gd-task9" {
		t.Errorf("BOAT_TASK_ID = %q, want gd-task9", got)
	}
	data, err := os.ReadFile(specializedRecredentialSentinelPath)
	if err != nil {
		t.Fatalf("expected re-credential sentinel written: %v", err)
	}
	if string(data) != "site-deployment" {
		t.Errorf("sentinel content = %q, want site-deployment", string(data))
	}
}

// TestHydrateAssignmentEnv_NoMarkerNoSentinel verifies a DEDICATED pod (no
// specialization marker) hydrates its project/role but drops NO re-credential
// sentinel — dedicated pools pay no swap.
func TestHydrateAssignmentEnv_NoMarkerNoSentinel(t *testing.T) {
	_ = os.Remove(specializedRecredentialSentinelPath)
	t.Cleanup(func() { _ = os.Remove(specializedRecredentialSentinelPath) })

	t.Setenv("PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BOAT_ROLE", "")

	oldDaemon := daemon
	daemon = specializeDaemonStub(t, map[string]string{
		"agent_state": "assigning",
		"project":     "gardenio",
		"role":        "thread",
	})
	t.Cleanup(func() { daemon = oldDaemon })

	hydrateAssignmentEnv(context.Background(), "gd-agent1")

	if got := os.Getenv("BOAT_PROJECT"); got != "gardenio" {
		t.Errorf("BOAT_PROJECT = %q, want gardenio", got)
	}
	if _, err := os.Stat(specializedRecredentialSentinelPath); !os.IsNotExist(err) {
		t.Errorf("dedicated pod must not drop the re-credential sentinel (err=%v)", err)
	}
}

// TestHydrateAssignmentEnv_SkipsGenericSentinelValues verifies hydration never
// adopts the raw "*" sentinel as a concrete project/role — a pod assigned with
// still-generic fields (defensive: the pool always specializes, but the reader
// must not leak "*").
func TestHydrateAssignmentEnv_SkipsGenericSentinelValues(t *testing.T) {
	_ = os.Remove(specializedRecredentialSentinelPath)
	t.Cleanup(func() { _ = os.Remove(specializedRecredentialSentinelPath) })

	t.Setenv("PROJECT", "")
	t.Setenv("BOAT_PROJECT", "seed")
	t.Setenv("BOAT_ROLE", "seed")

	oldDaemon := daemon
	daemon = specializeDaemonStub(t, map[string]string{
		"agent_state": "assigning",
		"project":     genericPoolSentinel,
		"role":        genericPoolSentinel,
	})
	t.Cleanup(func() { daemon = oldDaemon })

	hydrateAssignmentEnv(context.Background(), "gd-agent1")

	if got := os.Getenv("BOAT_PROJECT"); got == genericPoolSentinel {
		t.Errorf("BOAT_PROJECT must not be the raw sentinel, got %q", got)
	}
	if got := os.Getenv("BOAT_ROLE"); got == genericPoolSentinel {
		t.Errorf("BOAT_ROLE must not be the raw sentinel, got %q", got)
	}
}
