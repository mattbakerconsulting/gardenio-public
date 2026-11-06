package reconciler

import (
	"context"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// TestReconcile_LocalAgent_NeitherCreatedNorClosed: a laptop agent
// (execution_target=laptop, gd-SzPJDNTBXS) has no pod by design, so the
// reconciler must neither cold-start one for it nor close it as a podless leak.
func TestReconcile_LocalAgent_NeitherCreatedNorClosed(t *testing.T) {
	local := func(id, state string) beadsapi.AgentBead {
		return beadsapi.AgentBead{
			ID: id, Project: "proj", Mode: "crew", Role: "local", AgentName: id,
			AgentState: state,
			Metadata:   map[string]string{"execution_target": podmanager.ExecutionTargetLaptop, "runtime_id": "gd-r1"},
		}
	}
	lister := &mockLister{
		beads: []beadsapi.AgentBead{
			local("bd-working", "working"),
			local("bd-done", "done"),
			{ID: "bd-pod", Project: "proj", Mode: "crew", Role: "dev", AgentName: "pod"},
			// The escalation bridge's local-fallback executor: a fleet pod
			// stamped execution_target=local, which still needs its pod.
			{
				ID: "bd-exec", Project: "proj", Mode: "crew", Role: "crew", AgentName: "exec",
				Metadata: map[string]string{"execution_target": "local", "escalation_id": "gd-esc"},
			},
		},
	}
	mgr := &mockManager{pods: nil}
	closer := &mockBeadCloser{}

	// simpleSpecBuilder, like production's resolveExecutionTarget, does not
	// carry "local" into the spec: an unrecognized target resolves to the
	// kubernetes pod path, which is how the escalation executor gets its pod.
	r := New(lister, mgr, testConfig("ns"), testLogger(), simpleSpecBuilder("img:v1"))
	r.SetBeadCloser(closer)
	if err := r.Reconcile(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var created []string
	for _, c := range mgr.created {
		created = append(created, c.AgentName)
	}
	slices.Sort(created)
	if !slices.Equal(created, []string{"exec", "pod"}) {
		t.Fatalf("expected pods for the K8s agent and the escalation executor only, got %v", created)
	}
	for _, c := range closer.closed {
		if c.beadID == "bd-working" || c.beadID == "bd-done" {
			t.Errorf("local agent %s must not be closed by the reconciler", c.beadID)
		}
	}
}
