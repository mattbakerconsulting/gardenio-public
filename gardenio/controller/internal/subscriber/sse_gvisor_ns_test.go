package subscriber

import (
	"testing"
)

// Freeze-path namespace-resolution regression (bug ka-0000002448).
//
// buildEvent used to unconditionally set meta["namespace"]=cfg.Namespace on
// every event. That auto-injected pin defeated routeGVisorNamespace on the
// events-path AgentStop/freeze trigger: routeGVisorNamespace only routes a
// runsc agent into cfg.GVisorNamespace when the namespace pin is empty (it
// treats any non-empty value as an operator pin). The injected default made
// the controller resolve the teardown namespace to "gardenio" instead of
// "gardenio-gvisor", so shouldFreeze looked for the pod in the wrong namespace,
// found nothing, and silently skipped the freeze — no snapshot, no thaw.
//
// The fix: buildEvent must NOT inject a namespace. A bead with no explicit
// `namespace` field must produce an event whose metadata carries no namespace
// pin, so namespaceFromEvent supplies the cfg.Namespace fallback for ordinary
// agents AND routeGVisorNamespace can re-route runsc agents on freeze.

func newTestWatcher() *SSEWatcher {
	return NewSSEWatcher(SSEConfig{
		Namespace:     "gardenio",
		CoopImage:     "coop:latest",
		BeadsGRPCAddr: "localhost:9090",
	}, testLogger())
}

// A runsc canary bead with no explicit namespace field must yield an event with
// NO namespace pin, so the downstream gVisor freeze routing can fire.
func TestBuildEvent_NoNamespaceInjection_FreezePath(t *testing.T) {
	w := newTestWatcher()
	bead := beadData{
		ID:    "gd-canary",
		Title: "gvisor canary",
		Type:  "agent",
		Fields: map[string]string{
			"project": "gardenio",
			"role":    "gvisor-poc",
			"agent":   "gardenio-canary",
			"mode":    "crew",
		},
	}

	event, ok := w.buildEvent(AgentStop, bead)
	if !ok {
		t.Fatal("buildEvent should have produced an event")
	}
	if ns, present := event.Metadata["namespace"]; present {
		t.Fatalf("buildEvent must not inject a namespace pin (defeats gVisor freeze routing); got %q", ns)
	}
}

// An explicit `namespace` field on the bead must still pass through untouched —
// an operator pin remains authoritative.
func TestBuildEvent_ExplicitNamespacePreserved(t *testing.T) {
	w := newTestWatcher()
	bead := beadData{
		ID:    "gd-pinned",
		Title: "pinned agent",
		Type:  "agent",
		Fields: map[string]string{
			"project":   "gardenio",
			"role":      "crew",
			"agent":     "pinned",
			"mode":      "crew",
			"namespace": "operator-ns",
		},
	}

	event, ok := w.buildEvent(AgentSpawn, bead)
	if !ok {
		t.Fatal("buildEvent should have produced an event")
	}
	if event.Metadata["namespace"] != "operator-ns" {
		t.Fatalf("explicit namespace pin must be preserved, got %q", event.Metadata["namespace"])
	}
}
