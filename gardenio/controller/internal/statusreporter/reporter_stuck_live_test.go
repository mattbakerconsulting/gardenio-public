// Tests for the live-agent guards added to the stuck-agent heuristic
// (bug ka-0000002709): advancing context_tokens / cost counts as proof-of-life
// even when the terminal sequences are quiet, and long-lived orchestrator
// roles (gardener / plan-gardener) get a proportionally longer grace window
// before being flagged as wedged. Both stop the AgentStuck delete+recreate
// from hard-killing a live agent mid-turn.
package statusreporter

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// stuckCoopServerForRole is stuckCoopServer with a configurable pod role so
// the role-aware threshold can be exercised. Returns the server, a mutable
// response body pointer, and the wired pod.
func stuckCoopServerForRole(t *testing.T, role string) (*httptest.Server, *string, *corev1.Pod) {
	t.Helper()
	body := `{"state":"working","since_seq":1,"screen_seq":1}`
	bodyPtr := &body
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agent" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(*bodyPtr))
	}))
	t.Cleanup(srv.Close)

	host, _, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split server URL %q: %v", srv.URL, err)
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardener-proj-" + role + "-alpha",
			Namespace: "ns",
			Labels:    agentLabels("proj", role, "alpha"),
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "agent"},
				{Name: "coop", Ports: []corev1.ContainerPort{{ContainerPort: 8080}}},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: host,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	return srv, bodyPtr, pod
}

// newReporterWithStuckMultiplier wires a reporter with the heuristic enabled,
// a configurable long-lived multiplier, and a mockable clock.
func newReporterWithStuckMultiplier(t *testing.T, threshold time.Duration, multiplier float64, srv *httptest.Server, pod *corev1.Pod) (*HTTPReporter, *recordingPublisher, *time.Time) {
	t.Helper()
	client := fake.NewSimpleClientset(pod)
	daemon := &mockBeadUpdater{}
	r := NewHTTPReporter(daemon, client, "ns", testLogger())
	wireReporterToCoopServer(r, srv)

	pub := &recordingPublisher{}
	r.SetStuckDetection(pub, threshold, multiplier)

	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return now }
	return r, pub, &now
}

// TestSyncAll_StuckAgent_ContextTokensAdvanceIsAlive verifies that a pod whose
// terminal sequences are frozen but whose context window keeps growing (a live
// agent mid-model-request) is NOT flagged as stuck. This is the core
// live-agent guard from bug ka-0000002709.
func TestSyncAll_StuckAgent_ContextTokensAdvanceIsAlive(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0.5}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Past the base threshold, sequences unchanged but context_tokens grew —
	// the agent is mid-turn, not wedged.
	*now = now.Add(6 * time.Minute)
	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":2000,"cost_usd":0.5}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected context_tokens growth to count as alive, got %d stuck events: %+v", len(got), got)
	}
}

// TestSyncAll_StuckAgent_CostAdvanceIsAlive verifies the same guard via a
// growing running-cost estimate while every other signal holds steady.
func TestSyncAll_StuckAgent_CostAdvanceIsAlive(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0.5}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(6 * time.Minute)
	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0.9}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected cost growth to count as alive, got %d stuck events: %+v", len(got), got)
	}
}

// TestSyncAll_StuckAgent_AllSignalsFrozenStillStuck verifies the heuristic
// still fires when NO liveness signal advances — a genuine wedge is not masked
// by the new token/cost guard.
func TestSyncAll_StuckAgent_AllSignalsFrozenStillStuck(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0.5}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(6 * time.Minute)
	// Identical payload — nothing advanced.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 1 {
		t.Fatalf("expected a genuinely frozen agent to still be flagged stuck, got %d: %+v", len(got), got)
	}
}

// TestSyncAll_StuckAgent_LongLivedRoleGetsLongerGrace verifies that a
// gardener's frozen pod is NOT flagged just past the base threshold, but IS
// flagged once past the multiplied threshold — the role-aware grace window.
func TestSyncAll_StuckAgent_LongLivedRoleGetsLongerGrace(t *testing.T) {
	srv, _, pod := stuckCoopServerForRole(t, "plan-gardener")
	// Base 5m, multiplier 4 => effective 20m for plan-gardener.
	r, pub, now := newReporterWithStuckMultiplier(t, 5*time.Minute, 4, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// 6m: past base threshold but well within the 20m long-lived window.
	*now = now.Add(6 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected plan-gardener to survive past base threshold, got %d: %+v", len(got), got)
	}

	// 21m total: past the multiplied threshold — now genuinely wedged.
	*now = now.Add(15 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if got := pub.snapshot(); len(got) != 1 {
		t.Fatalf("expected plan-gardener flagged past multiplied threshold, got %d: %+v", len(got), got)
	}
}

// TestSyncAll_StuckAgent_NonLongLivedRoleUnaffectedByMultiplier verifies the
// role bias does NOT extend the window for ordinary roles: a crew pod is still
// flagged at the base threshold even with a multiplier configured.
func TestSyncAll_StuckAgent_NonLongLivedRoleUnaffectedByMultiplier(t *testing.T) {
	srv, _, pod := stuckCoopServerForRole(t, "crew")
	r, pub, now := newReporterWithStuckMultiplier(t, 5*time.Minute, 4, srv, pod)

	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(6 * time.Minute)
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := pub.snapshot(); len(got) != 1 {
		t.Fatalf("expected crew flagged at base threshold regardless of multiplier, got %d: %+v", len(got), got)
	}
}

// TestStuckThresholdForRole covers the threshold computation directly.
func TestStuckThresholdForRole(t *testing.T) {
	r := &HTTPReporter{stuckThreshold: 10 * time.Minute, stuckLongLivedMultiplier: 4}
	cases := []struct {
		role string
		want time.Duration
	}{
		{"gardener", 40 * time.Minute},
		{"plan-gardener", 40 * time.Minute},
		{"plan-gardener,thread", 40 * time.Minute}, // primary role authoritative
		{"crew", 10 * time.Minute},
		{"thread", 10 * time.Minute},
		{"", 10 * time.Minute},
	}
	for _, c := range cases {
		if got := r.stuckThresholdForRole(c.role); got != c.want {
			t.Errorf("stuckThresholdForRole(%q) = %v, want %v", c.role, got, c.want)
		}
	}

	// Multiplier <= 1 disables the bias entirely.
	r2 := &HTTPReporter{stuckThreshold: 10 * time.Minute, stuckLongLivedMultiplier: 1}
	if got := r2.stuckThresholdForRole("gardener"); got != 10*time.Minute {
		t.Errorf("multiplier<=1 should not extend: got %v want 10m", got)
	}
}

// TestSyncAll_StuckAgent_TurnsAdvanceIsAlive verifies that an advancing
// round-trip count (coop's request_count) counts as proof-of-life on its own.
//
// This is the signal that actually carries the guard on the OAuth/Bedrock
// path, where cost_usd is structurally always zero: coop only accumulates cost
// when LiteLLM stamps response_cost on the turn, so the cost signal can never
// advance there and contributes no safety at all. Before turns was counted,
// the effective signal set for a quiet-terminal agent collapsed to
// context_tokens — which only updates when a turn COMPLETES (ka-0000000084).
func TestSyncAll_StuckAgent_TurnsAdvanceIsAlive(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0,"request_count":10}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	// Past the threshold with EVERY other signal frozen — including cost at
	// its permanent zero — but the agent completed more round-trips.
	*now = now.Add(6 * time.Minute)
	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0,"request_count":11}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 0 {
		t.Fatalf("expected request_count growth to count as alive, got %d stuck events: %+v", len(got), got)
	}
}

// TestSyncAll_StuckAgent_ZeroCostDoesNotMaskFrozenAgent guards the other
// direction: cost_usd pinned at zero (the normal OAuth/Bedrock case) must not
// be mistaken for "advanced". An agent frozen on every signal is still stuck.
func TestSyncAll_StuckAgent_ZeroCostDoesNotMaskFrozenAgent(t *testing.T) {
	srv, body, pod := stuckCoopServer(t)
	r, pub, now := newReporterWithStuck(t, 5*time.Minute, srv, pod)

	*body = `{"state":"working","since_seq":1,"screen_seq":1,"context_tokens":1000,"cost_usd":0,"request_count":10}`
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	*now = now.Add(6 * time.Minute)
	// Identical payload — nothing advanced, cost still zero.
	if err := r.SyncAll(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	if got := pub.snapshot(); len(got) != 1 {
		t.Fatalf("expected a fully frozen zero-cost agent to be flagged stuck, got %d: %+v", len(got), got)
	}
}
