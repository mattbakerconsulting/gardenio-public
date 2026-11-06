package poolmanager

import (
	"context"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// W2 per-runtimeClass bucket partition (ka-0000003380). runc vs runsc is an
// immutable pod-spec field, so warm pods form one generic pool PER runtime
// bucket. These tests prove the assign path honors that partition on BOTH sides:
//   - a runtime-pinned (runsc) request only ever claims a runsc pod (the gate,
//     already covered by the W1 generic tests, guarded again here for the mixed
//     bucket case), and
//   - a runtime-UNCONSTRAINED (default runc) request prefers its OWN bucket and
//     does not needlessly drain the scarce runsc bucket, while still falling back
//     to a runsc pod when no runc pod is warm (warm-hit rate never regresses).

// TestAssignPrewarmed_UnconstrainedPrefersRuncBucket: with a generic runc pod and
// a generic runsc pod both assignable, a default request (RequireRuntimeClass="")
// must claim the runc-bucket pod, leaving the scarce runsc pod for the runsc
// channels it was warmed for.
func TestAssignPrewarmed_UnconstrainedPrefersRuncBucket(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic-runsc", map[string]string{
			"project":       GenericPoolValue,
			"role":          GenericPoolValue,
			"runtime_class": "runsc",
		}),
		genericAgent("gd-generic-runc", map[string]string{
			"project": GenericPoolValue,
			"role":    GenericPoolValue,
			// no runtime_class → cluster default (runc) bucket
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
		Project: "kbeads",
		Role:    "crew",
		// RequireRuntimeClass unset → default runc channel, home bucket is runc.
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-generic-runc" {
		t.Errorf("default request must prefer the runc-bucket pod, got %s (a runc request must not drain the runsc bucket)", result.BeadID)
	}
}

// TestAssignPrewarmed_UnconstrainedFallsBackToRunsc: the bucket preference is a
// preference, not a hard gate — when the pool holds ONLY a runsc pod, a default
// request still gets it rather than falling through to a cold mint. This is the
// warm-hit-preserving fallback that keeps the partition from regressing latency.
func TestAssignPrewarmed_UnconstrainedFallsBackToRunsc(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic-runsc-only", map[string]string{
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
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project: "kbeads",
		Role:    "crew",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-generic-runsc-only" {
		t.Errorf("with only a runsc pod warm, a default request must fall back to it (warm hit), got %s", result.BeadID)
	}
}

// TestAssignPrewarmed_RunscRequestUnaffectedByBucketPreference is the regression
// guard for the pinned-runtime side: with a runc pod and a runsc pod both warm, a
// runsc request must still claim the runsc pod. The bucket preference only
// reorders the UNCONSTRAINED path, so a pinned request is unaffected — the gate
// already confined its candidates to the runsc bucket.
func TestAssignPrewarmed_RunscRequestUnaffectedByBucketPreference(t *testing.T) {
	agents := []map[string]any{
		genericAgent("gd-generic-runc", map[string]string{
			"project": GenericPoolValue,
			"role":    GenericPoolValue,
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
	pm := New(daemon, testConfig(), slog.Default())

	result, err := pm.AssignPrewarmed(context.Background(), AssignRequest{
		Project:             "gardenio",
		Role:                "gvisor-poc",
		RequireRuntimeClass: "runsc",
	})
	if err != nil {
		t.Fatalf("AssignPrewarmed returned error: %v", err)
	}
	if result.BeadID != "gd-generic-runsc" {
		t.Errorf("runsc request must claim the runsc pod, got %s", result.BeadID)
	}
}
