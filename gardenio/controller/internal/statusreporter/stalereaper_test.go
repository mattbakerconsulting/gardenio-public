package statusreporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// --- mock pod manager for stale reaper tests ---

type mockPodManager struct {
	mu        sync.Mutex
	deleted   []string
	deletedNS map[string]string // pod name → namespace it was deleted from
	deleteErr error
	// livePods, when non-nil, is the set of agent pod names ListAgentPods
	// reports as present (all in the primary namespace). listErr, when set,
	// makes ListAgentPods fail so tests can exercise the fail-safe path.
	livePods map[string]bool
	listErr  error
}

func (m *mockPodManager) CreateAgentPod(_ context.Context, _ podmanager.AgentPodSpec) error {
	return nil
}

func (m *mockPodManager) DeleteAgentPod(_ context.Context, name, namespace string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleted = append(m.deleted, name)
	if m.deletedNS == nil {
		m.deletedNS = make(map[string]string)
	}
	m.deletedNS[name] = namespace
	return m.deleteErr
}

func (m *mockPodManager) DeleteAllAgentPods(_ context.Context, _ string, _ bool) (int, error) {
	return 0, nil
}

func (m *mockPodManager) ListAgentPods(_ context.Context, namespace string, _ map[string]string) ([]corev1.Pod, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	if m.livePods == nil {
		return nil, nil
	}
	var out []corev1.Pod
	for name := range m.livePods {
		out = append(out, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
				Labels:    map[string]string{podmanager.LabelAgent: name},
			},
		})
	}
	return out, nil
}

func (m *mockPodManager) GetAgentPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	return nil, nil
}

func (m *mockPodManager) GetNode(_ context.Context, _ string) (*corev1.Node, error) {
	return nil, nil
}

// beadFixture builds a daemon HTTP response for ListAgentBeads.
func beadFixture(beads []beadEntry) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodGet {
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: beads}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Close bead endpoint.
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}
}

type beadEntry struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	Type      string          `json:"type"`
	Kind      string          `json:"kind"`
	Status    string          `json:"status"`
	Assignee  string          `json:"assignee,omitempty"`
	Labels    []string        `json:"labels,omitempty"`
	Fields    json.RawMessage `json:"fields"`
	CreatedAt string          `json:"created_at,omitempty"`
	UpdatedAt string          `json:"updated_at,omitempty"`
}

func staleReaperLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestNewStaleReaper_DisabledWhenZeroTimeout(t *testing.T) {
	r := NewStaleReaper(nil, nil, "ns", 0, staleReaperLogger())
	if r != nil {
		t.Fatal("expected nil reaper when timeout is 0")
	}
}

func TestStaleReaper_ReapsWorkingAgentsPastTimeout(t *testing.T) {
	twoHoursAgo := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	recent := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID:     "bd-stale1",
			Title:  "stale-agent",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":     "gardenio",
				"mode":        "crew",
				"role":        "crew",
				"agent":       "stale-agent",
				"agent_state": "working",
			}),
			CreatedAt: twoHoursAgo,
			UpdatedAt: twoHoursAgo,
		},
		{
			ID:     "bd-fresh1",
			Title:  "fresh-agent",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":     "gardenio",
				"mode":        "crew",
				"role":        "crew",
				"agent":       "fresh-agent",
				"agent_state": "working",
			}),
			CreatedAt: recent,
			UpdatedAt: recent,
		},
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())

	stats := reaper.Reap(context.Background())

	if stats.Checked != 2 {
		t.Errorf("checked: got %d, want 2", stats.Checked)
	}
	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1", stats.Reaped)
	}
	if len(pods.deleted) != 1 {
		t.Fatalf("deleted pods: got %d, want 1", len(pods.deleted))
	}
	if pods.deleted[0] != "crew-gardenio-crew-stale-agent" {
		t.Errorf("deleted pod name: got %q, want %q", pods.deleted[0], "crew-gardenio-crew-stale-agent")
	}
}

func TestStaleReaper_SkipsProtectedStates(t *testing.T) {
	// "prewarmed" is unconditionally protected.
	// "spawning" and "yielding" are protected within 2x timeout.
	// "queued" within 2x timeout is also protected.
	// Use 3h old (within 2x 2h timeout = 4h) for spawning/yielding.
	withinDoubleTimeout := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	entries := []beadEntry{
		{
			ID: "bd-skip-spawning", Title: "agent-spawning", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "agent-spawning", "agent_state": "spawning",
			}),
			CreatedAt: withinDoubleTimeout, UpdatedAt: withinDoubleTimeout,
		},
		{
			ID: "bd-skip-prewarmed", Title: "agent-prewarmed", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "agent-prewarmed", "agent_state": "prewarmed",
			}),
			// prewarmed is unconditionally protected — use very old timestamp.
			CreatedAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
			UpdatedAt: time.Now().Add(-24 * time.Hour).UTC().Format(time.RFC3339),
		},
		{
			ID: "bd-skip-yielding", Title: "agent-yielding", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "agent-yielding", "agent_state": "yielding",
			}),
			CreatedAt: withinDoubleTimeout, UpdatedAt: withinDoubleTimeout,
		},
		{
			ID: "bd-skip-queued", Title: "agent-queued", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "agent-queued", "agent_state": "queued",
			}),
			CreatedAt: withinDoubleTimeout, UpdatedAt: withinDoubleTimeout,
		},
	}

	srv := httptest.NewServer(beadFixture(entries))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (protected states within grace period should be skipped)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsStuckSpawning(t *testing.T) {
	// A "spawning" agent stuck for > 2x timeout should be reaped.
	veryOld := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-spawn-stuck", Title: "spawn-stuck", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "spawn-stuck", "agent_state": "spawning",
			}),
			CreatedAt: veryOld, UpdatedAt: veryOld,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (spawning agent past 2x timeout should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsStuckYielding(t *testing.T) {
	// A "yielding" agent stuck for > 2x timeout should be reaped.
	veryOld := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-yield-stuck", Title: "yield-stuck", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "yield-stuck", "agent_state": "yielding",
			}),
			CreatedAt: veryOld, UpdatedAt: veryOld,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (yielding agent past 2x timeout should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsQueuedAfterDoubleTimeout(t *testing.T) {
	// A queued agent that's been waiting for 2x the timeout with no source
	// task closed should still be reaped to prevent indefinite accumulation.
	veryOld := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-queued-old", Title: "queued-old", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "queued-old", "agent_state": "queued",
			}),
			CreatedAt: veryOld,
			UpdatedAt: veryOld,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (queued agent past 2x timeout should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsEmptyAgentState(t *testing.T) {
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	// "recent" must be within noPodReapAfter (15m): a podless empty-state bead
	// older than that is now fast-failed as a wedged spawn (ka-0000004685). The
	// mock pod manager reports no live pods, so both beads are podless; the
	// young one is spared purely by the grace window.
	recent := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID: "bd-orphan1", Title: "orphan-old", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "orphan-old",
			}),
			CreatedAt: old, UpdatedAt: old,
		},
		{
			ID: "bd-orphan2", Title: "orphan-fresh", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "orphan-fresh",
			}),
			CreatedAt: recent, UpdatedAt: recent,
		},
	})

	srv := httptest.NewServer(handler)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (old empty-state bead should be reaped)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-crew-orphan-old" {
		t.Errorf("deleted pods: got %v, want [crew-gardenio-crew-orphan-old]", pods.deleted)
	}
}

// TestStaleReaper_FastFailsPodlessEmptyState covers the ka-0000004685 durable
// fix: an empty-state bead with NO live pod is reaped once it ages past
// noPodReapAfter (15m) — far short of the 2h base timeout — so a spawn that
// wedged podless (the "gardener with no session" card Matt flagged) clears in
// minutes instead of lingering visible for up to 2h.
func TestStaleReaper_FastFailsPodlessEmptyState(t *testing.T) {
	// 20m old: past noPodReapAfter (15m) but far under the 2h base timeout.
	wedged := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID: "bd-wedged", Title: "wedged-gardener", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "plan-gardener",
				"agent": "wedged-gardener",
			}),
			CreatedAt: wedged, UpdatedAt: wedged,
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	// livePods is an empty (non-nil) set: ListAgentPods succeeds and reports NO
	// pods, so the bead is genuinely podless.
	pods := &mockPodManager{livePods: map[string]bool{}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1 (podless empty-state bead past 15m should fast-fail)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-plan-gardener-wedged-gardener" {
		t.Errorf("deleted pods: got %v, want [crew-gardenio-plan-gardener-wedged-gardener]", pods.deleted)
	}
}

// TestStaleReaper_SparesEmptyStateWithLivePod is the race guard for the
// ka-0000004685 fast-fail: an empty-state bead that DOES have a live pod is
// merely slow to report status (a reconciler-created Pending pod stays
// empty-state until the next SyncAll stamps "spawning"). It must NOT be
// early-reaped — only the full 2h base timeout applies — so a live-but-quiet
// pod is never killed under the pod.
func TestStaleReaper_SparesEmptyStateWithLivePod(t *testing.T) {
	// 20m old: past noPodReapAfter, but a live pod exists so the fast-fail must
	// not fire and 2h has not elapsed.
	slow := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	podName := "crew-gardenio-plan-gardener-slow-reporter"

	handler := beadFixture([]beadEntry{
		{
			ID: "bd-slow", Title: "slow-reporter", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "plan-gardener",
				"agent": "slow-reporter",
			}),
			CreatedAt: slow, UpdatedAt: slow,
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{livePods: map[string]bool{podName: true}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (empty-state bead WITH a live pod must not be early-reaped)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Errorf("deleted pods: got %v, want none", pods.deleted)
	}
}

// TestStaleReaper_PodListErrorSkipsFastFail is the fail-safe guard: when
// ListAgentPods errors, livePodNames returns nil and the reaper treats every
// bead as possibly-podded, so a transient K8s API blip never triggers a
// spurious early reap. The wedged bead falls through to the unchanged 2h
// base-timeout path (not elapsed here), so it is left alone.
func TestStaleReaper_PodListErrorSkipsFastFail(t *testing.T) {
	wedged := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID: "bd-wedged2", Title: "wedged2", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "plan-gardener",
				"agent": "wedged2",
			}),
			CreatedAt: wedged, UpdatedAt: wedged,
		},
	})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{listErr: fmt.Errorf("k8s API unavailable")}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (pod-list error must skip the fast-fail, not reap)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsTerminalStates(t *testing.T) {
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	// Terminal states with open beads should be reaped.
	states := []string{"done", "failed", "rate_limited"}
	var entries []beadEntry
	for i, state := range states {
		entries = append(entries, beadEntry{
			ID:     fmt.Sprintf("bd-term%d", i),
			Title:  fmt.Sprintf("term-agent-%d", i),
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":     "gardenio",
				"mode":        "crew",
				"role":        "crew",
				"agent":       fmt.Sprintf("term-agent-%d", i),
				"agent_state": state,
			}),
			CreatedAt: old,
			UpdatedAt: old,
		})
	}

	srv := httptest.NewServer(beadFixture(entries))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 3 {
		t.Errorf("reaped: got %d, want 3 (terminal states with open beads should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_TerminalStatesReapedFaster(t *testing.T) {
	// Terminal states (done, failed) use a shorter timeout (1/4 of normal,
	// min 15 min). An agent 45 min old with "done" state should be reaped
	// when the normal timeout is 2h (terminal timeout = 30 min).
	fortyFiveMinAgo := time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-term-fast", Title: "done-fast", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "done-fast", "agent_state": "done",
			}),
			CreatedAt: fortyFiveMinAgo,
			UpdatedAt: fortyFiveMinAgo,
		},
		{
			ID: "bd-working-ok", Title: "working-ok", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "working-ok", "agent_state": "working",
			}),
			CreatedAt: fortyFiveMinAgo,
			UpdatedAt: fortyFiveMinAgo,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	// "done" should be reaped (45m > 30m terminal timeout), but
	// "working" should NOT (45m < 2h normal timeout).
	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (terminal state reaped faster, working spared)", stats.Reaped)
	}
}

func TestStaleReaper_SkipsZeroUpdatedAt(t *testing.T) {
	entries := []beadEntry{
		{
			ID:     "bd-nots",
			Title:  "no-timestamp",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":     "gardenio",
				"mode":        "crew",
				"role":        "crew",
				"agent":       "no-timestamp",
				"agent_state": "working",
			}),
		},
	}

	srv := httptest.NewServer(beadFixture(entries))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (zero updated_at should be skipped)", stats.Reaped)
	}
}

func TestStaleReaper_ContinuesOnCloseError(t *testing.T) {
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)
	callCount := 0

	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/beads" && r.Method == http.MethodGet {
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-err1", Title: "err-agent", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "err-agent", "agent_state": "working",
					}),
					CreatedAt: old, UpdatedAt: old,
				},
				{
					ID: "bd-ok1", Title: "ok-agent", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "ok-agent", "agent_state": "working",
					}),
					CreatedAt: old, UpdatedAt: old,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		if r.Method == http.MethodPost {
			callCount++
			if callCount == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	// First bead close fails, second succeeds.
	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (should continue after close error)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsQueuedOrphan_TaskIDField(t *testing.T) {
	// Agent bead queued with task_id pointing to a closed task.
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-queued1", Title: "queued-agent", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "queued-agent", "agent_state": "queued",
						"task_id": "gd-TASK1",
					}),
					CreatedAt: old, UpdatedAt: old,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/beads/gd-TASK1" && r.Method == http.MethodGet:
			// Source task is closed.
			task := map[string]interface{}{
				"id": "gd-TASK1", "title": "Enable TLS", "type": "task",
				"kind": "issue", "status": "closed",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (queued agent with closed task should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_SkipsQueuedOrphan_TaskStillOpen(t *testing.T) {
	// Agent bead queued with task_id pointing to an open task — should NOT be reaped.
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-queued2", Title: "queued-agent", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "queued-agent", "agent_state": "queued",
						"task_id": "gd-TASK2",
					}),
					CreatedAt: old, UpdatedAt: old,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/beads/gd-TASK2" && r.Method == http.MethodGet:
			task := map[string]interface{}{
				"id": "gd-TASK2", "title": "Still Open Task", "type": "task",
				"kind": "issue", "status": "open",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (queued agent with open task should NOT be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsQueuedOrphan_SpawnedFromDescription(t *testing.T) {
	// Agent bead queued with "Spawned from gd-XXX" in description (iOS app pattern).
	old := time.Now().Add(-5 * time.Hour).UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-queued3", Title: "app-spawned-agent", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "app-spawned-agent", "agent_state": "queued",
					}),
					CreatedAt: old, UpdatedAt: old,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/beads/bd-queued3" && r.Method == http.MethodGet:
			// Full bead detail includes description.
			bead := map[string]interface{}{
				"id": "bd-queued3", "title": "app-spawned-agent", "type": "agent",
				"kind": "config", "status": "open",
				"description": "Spawned from gd-TASK3: Enable TLS everywhere",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(bead)

		case r.URL.Path == "/v1/beads/gd-TASK3" && r.Method == http.MethodGet:
			task := map[string]interface{}{
				"id": "gd-TASK3", "title": "Enable TLS", "type": "task",
				"kind": "issue", "status": "closed",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (queued agent with closed source task from description should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_SkipsRecentlyQueuedOrphan(t *testing.T) {
	// Agent bead queued recently (within timeout) — should NOT check task status.
	recent := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-queued4", Title: "fresh-queued", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "fresh-queued", "agent_state": "queued",
						"task_id": "gd-TASK4",
					}),
					CreatedAt: recent, UpdatedAt: recent,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		default:
			// Task lookup should NOT be called for recent beads.
			if r.URL.Path == "/v1/beads/gd-TASK4" {
				t.Error("unexpected task lookup for recently queued agent")
			}
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (recently queued agent should not be checked)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsWorkingOrphan_ClosedSourceTask(t *testing.T) {
	// A "working" agent whose source task is already closed should be reaped
	// once it has been idle past the orphan grace window. updated_at is the
	// idle proxy: SyncAll skips bumping it for idle coop agents, so an aged
	// updated_at means the pod is parked and merely forgot `gb stop`.
	recent := time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-work1", Title: "working-orphan", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "working-orphan", "agent_state": "working",
						"task_id": "gd-TASK-WORK",
					}),
					CreatedAt: recent, UpdatedAt: recent,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/beads/gd-TASK-WORK" && r.Method == http.MethodGet:
			task := map[string]interface{}{
				"id": "gd-TASK-WORK", "title": "Finished work", "type": "task",
				"kind": "issue", "status": "closed",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (working agent with closed source task should be reaped)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-crew-working-orphan" {
		t.Errorf("deleted pods: got %v, want [crew-gardenio-crew-working-orphan]", pods.deleted)
	}
}

// stalledOrphanHandler serves one "working" crew agent whose updated_at is
// FRESH (SyncAll keeps bumping it while coop reports "working") but whose
// last_progress_at is older than ProgressStaleThreshold, plus its source task
// in the given status/assignee. Models gd-udek4CJVRY: a --resume that never
// submitted its prompt pins coop at "working" for hours.
func stalledOrphanHandler(agent, lastProgress, taskStatus, taskAssignee string) http.HandlerFunc {
	fresh := time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339)
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			fields := map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": agent, "agent_state": "working",
				"task_id": "gd-TASK-STALL",
			}
			if lastProgress != "" {
				fields["last_progress_at"] = lastProgress
			}
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{{
				ID: "bd-stall1", Title: agent, Type: "agent", Kind: "config", Status: "open",
				Fields:    mustJSON(fields),
				CreatedAt: time.Now().Add(-11 * time.Hour).UTC().Format(time.RFC3339), UpdatedAt: fresh,
			}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/v1/beads/gd-TASK-STALL" && r.Method == http.MethodGet:
			task := map[string]interface{}{
				"id": "gd-TASK-STALL", "title": "Step", "type": "task", "kind": "issue",
				"status": taskStatus, "assignee": taskAssignee,
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func runStalledOrphanReap(t *testing.T, h http.HandlerFunc) (StaleReaperStats, *mockPodManager) {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	return reaper.Reap(context.Background()), pods
}

// TestStaleReaper_ReapsStalledWorkingAgent_TaskReassigned: a crew whose
// heartbeat stalled 9h ago while another agent now holds its task is reaped
// even though updated_at is fresh (gd-udek4CJVRY).
func TestStaleReaper_ReapsStalledWorkingAgent_TaskReassigned(t *testing.T) {
	old := time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339)
	stats, pods := runStalledOrphanReap(t, stalledOrphanHandler("crew-5vi", old, "in_progress", "crew-xgt"))
	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1 (stalled incumbent whose task another agent holds)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-crew-crew-5vi" {
		t.Errorf("deleted pods: got %v, want [crew-gardenio-crew-crew-5vi]", pods.deleted)
	}
}

// TestStaleReaper_ReapsStalledWorkingAgent_TaskClosed: same stalled crew, task
// closed by the replacement — reaped despite the fresh updated_at that the
// updated_at-gated orphan rule would wait on forever.
func TestStaleReaper_ReapsStalledWorkingAgent_TaskClosed(t *testing.T) {
	old := time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339)
	stats, _ := runStalledOrphanReap(t, stalledOrphanHandler("crew-5vi", old, "closed", "crew-xgt"))
	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1", stats.Reaped)
	}
}

// TestStaleReaper_SparesStalledWorkingAgent_StillOwnsTask: a stalled heartbeat
// alone (long quiet tool call, task still its own and open) is never a reap.
func TestStaleReaper_SparesStalledWorkingAgent_StillOwnsTask(t *testing.T) {
	old := time.Now().Add(-9 * time.Hour).UTC().Format(time.RFC3339)
	stats, _ := runStalledOrphanReap(t, stalledOrphanHandler("crew-5vi", old, "in_progress", "crew-5vi"))
	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (agent still owns its open task)", stats.Reaped)
	}
}

// TestStaleReaper_SparesReassignedTask_FreshHeartbeat: a task held by another
// agent is not enough on its own — a live heartbeat means the incumbent is
// still turning and the hand-off is someone else's mistake to sort out.
func TestStaleReaper_SparesReassignedTask_FreshHeartbeat(t *testing.T) {
	fresh := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)
	stats, _ := runStalledOrphanReap(t, stalledOrphanHandler("crew-5vi", fresh, "in_progress", "crew-xgt"))
	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (heartbeat is fresh)", stats.Reaped)
	}
}

// TestStaleReaper_SparesReassignedTask_NoHeartbeat: no last_progress_at at all
// (older controller, or no first turn yet) fails open.
func TestStaleReaper_SparesReassignedTask_NoHeartbeat(t *testing.T) {
	stats, _ := runStalledOrphanReap(t, stalledOrphanHandler("crew-5vi", "", "in_progress", "crew-xgt"))
	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (no heartbeat fails open)", stats.Reaped)
	}
}

func TestStaleReaper_SkipsWorkingAgent_TaskStillOpen(t *testing.T) {
	// A "working" agent with a still-open source task and fresh updated_at
	// should NOT be reaped.
	recent := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-work2", Title: "working-live", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "crew",
						"agent": "working-live", "agent_state": "working",
						"task_id": "gd-TASK-OPEN",
					}),
					CreatedAt: recent, UpdatedAt: recent,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/beads/gd-TASK-OPEN" && r.Method == http.MethodGet:
			task := map[string]interface{}{
				"id": "gd-TASK-OPEN", "title": "Still working", "type": "task",
				"kind": "issue", "status": "open",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (working agent with open task and fresh updates should not be reaped)", stats.Reaped)
	}
}

// orphanGraceHandler builds a daemon handler with a single working/empty-state
// agent bead whose source task (gd-TASK-X) is closed, used to exercise the
// orphan grace window. role and state let callers pick crew vs gardener and
// working vs empty agent_state.
func orphanGraceHandler(role, state, updatedAt string) http.HandlerFunc {
	fields := map[string]string{
		"project": "gardenio", "mode": "crew", "role": role,
		"agent": "orphan-grace", "task_id": "gd-TASK-X",
	}
	if state != "" {
		fields["agent_state"] = state
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-grace", Title: "orphan-grace", Type: "agent", Kind: "config", Status: "open",
					Fields:    mustJSON(fields),
					CreatedAt: updatedAt, UpdatedAt: updatedAt,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.URL.Path == "/v1/beads/gd-TASK-X" && r.Method == http.MethodGet:
			task := map[string]any{
				"id": "gd-TASK-X", "title": "Closed", "type": "task",
				"kind": "issue", "status": "closed",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func reapOrphanGrace(t *testing.T, role, state string, age time.Duration) StaleReaperStats {
	t.Helper()
	updatedAt := time.Now().Add(-age).UTC().Format(time.RFC3339)
	srv := httptest.NewServer(orphanGraceHandler(role, state, updatedAt))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	return reaper.Reap(context.Background())
}

// TestStaleReaper_SkipsWorkingOrphan_WithinGrace is the core regression for
// ka-0000000018: a working agent whose source task just closed but whose
// updated_at is fresh (busy / mid-wrap-up) must NOT be reaped instantly.
func TestStaleReaper_SkipsWorkingOrphan_WithinGrace(t *testing.T) {
	stats := reapOrphanGrace(t, "crew", "working", 30*time.Second)
	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (busy/fresh working orphan within grace must not be reaped)", stats.Reaped)
	}
}

// TestStaleReaper_ReapsWorkingOrphan_AfterGrace verifies a working agent whose
// source task is closed AND whose updated_at has aged past orphanReapAfter
// (parked, forgot `gb stop`) is reaped.
func TestStaleReaper_ReapsWorkingOrphan_AfterGrace(t *testing.T) {
	stats := reapOrphanGrace(t, "crew", "working", orphanReapAfter+time.Minute)
	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (idle working orphan past grace should be reaped)", stats.Reaped)
	}
}

// TestStaleReaper_SkipsEmptyStateOrphan_WithinGrace covers the empty
// agent_state branch sharing the same grace gate.
func TestStaleReaper_SkipsEmptyStateOrphan_WithinGrace(t *testing.T) {
	stats := reapOrphanGrace(t, "crew", "", 30*time.Second)
	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (empty-state orphan within grace must not be reaped)", stats.Reaped)
	}
}

// TestStaleReaper_SkipsGardenerOrphan_WithinExtendedGrace verifies a gardener
// gets the longer gardenerOrphanReapAfter window: aged past the crew grace but
// still within the gardener grace, it must NOT be reaped (loop runway).
func TestStaleReaper_SkipsGardenerOrphan_WithinExtendedGrace(t *testing.T) {
	// Between orphanReapAfter (5m) and gardenerOrphanReapAfter (15m).
	stats := reapOrphanGrace(t, "gardener", "working", orphanReapAfter+2*time.Minute)
	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (gardener within extended grace must not be reaped)", stats.Reaped)
	}
}

// TestStaleReaper_ReapsGardenerOrphan_AfterExtendedGrace verifies a gardener
// idle past gardenerOrphanReapAfter (genuinely dormant) is reaped.
func TestStaleReaper_ReapsGardenerOrphan_AfterExtendedGrace(t *testing.T) {
	stats := reapOrphanGrace(t, "gardener", "working", gardenerOrphanReapAfter+time.Minute)
	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (dormant gardener past extended grace should be reaped)", stats.Reaped)
	}
}

func TestStaleReaper_ReapsUnschedulableSpawning(t *testing.T) {
	// A "spawning" agent whose pod is Pending+Unschedulable (cluster out of
	// capacity) is reaped after 10 minutes — well before the normal 2x
	// spawning timeout — so operators aren't staring at zombie cards.
	fifteenMinAgo := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-unsched", Title: "unsched", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "unsched", "agent_state": "spawning",
				"pod_scheduling_blocked": "true",
			}),
			CreatedAt: fifteenMinAgo, UpdatedAt: fifteenMinAgo,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	// Use a 2h timeout — far longer than the 15m staleness — to confirm the
	// unschedulable branch fires on its own short clock, not the 2x fallback.
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1 (Pending+Unschedulable >10m should be reaped)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-crew-unsched" {
		t.Errorf("deleted pods: got %v, want [crew-gardenio-crew-unsched]", pods.deleted)
	}
}

func TestStaleReaper_SkipsRecentlyUnschedulable(t *testing.T) {
	// pod_scheduling_blocked=true but only 5 minutes old — under the 10m
	// grace window. Cluster autoscaler may still react; do not reap yet.
	fiveMinAgo := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-unsched-fresh", Title: "unsched-fresh", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "unsched-fresh", "agent_state": "spawning",
				"pod_scheduling_blocked": "true",
			}),
			CreatedAt: fiveMinAgo, UpdatedAt: fiveMinAgo,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (Unschedulable <10m should ride out the cluster scale-up window)", stats.Reaped)
	}
}

func TestStaleReaper_SkipsSpawningWithoutSchedulingBlock(t *testing.T) {
	// A spawning agent that's been pending for 15m but whose pod is NOT
	// flagged unschedulable (image pulling, init containers) should ride
	// the normal 2x-timeout grace, not the short unschedulable clock.
	fifteenMinAgo := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-pulling", Title: "pulling", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "pulling", "agent_state": "spawning",
				"pod_scheduling_blocked": "false",
			}),
			CreatedAt: fifteenMinAgo, UpdatedAt: fifteenMinAgo,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (spawning without scheduling block should use 2x timeout)", stats.Reaped)
	}
}

// TestStaleReaper_ReapsStuckAssigning is the G6 reclaim sweep (ka-0000006802):
// a bead flipped prewarmed→assigning whose pod died post-assign is stranded —
// the pool reconciler only counts "prewarmed" agents, so it is invisible to
// pool accounting. Past the 10m grace with NO live pod it must be fast-failed
// (closed with agent_state=failed) so the leaked slot clears.
func TestStaleReaper_ReapsStuckAssigning(t *testing.T) {
	// 15m old: past assigningReapAfter (10m) but far under the 2h base timeout.
	wedged := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-assign-stuck", Title: "assign-stuck", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "assign-stuck", "agent_state": "assigning",
			}),
			CreatedAt: wedged, UpdatedAt: wedged,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	// Empty (non-nil) live-pod set: ListAgentPods succeeds and reports NO pods,
	// so the assigning bead is genuinely podless.
	pods := &mockPodManager{livePods: map[string]bool{}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1 (podless assigning bead past 10m should fast-fail)", stats.Reaped)
	}
	if len(pods.deleted) != 1 || pods.deleted[0] != "crew-gardenio-crew-assign-stuck" {
		t.Errorf("deleted pods: got %v, want [crew-gardenio-crew-assign-stuck]", pods.deleted)
	}
}

// TestStaleReaper_SparesAssigningWithLivePod is the race guard for the G6 reap:
// a bead flipped to "assigning" whose pod is alive is merely slow to report
// "working" (the entrypoint is still polling its bead / hydrating the assignment
// env). It must NOT be early-reaped.
func TestStaleReaper_SparesAssigningWithLivePod(t *testing.T) {
	slow := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)
	podName := "crew-gardenio-crew-slow-assign"

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-slow-assign", Title: "slow-assign", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "slow-assign", "agent_state": "assigning",
			}),
			CreatedAt: slow, UpdatedAt: slow,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{livePods: map[string]bool{podName: true}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (assigning bead WITH a live pod must not be early-reaped)", stats.Reaped)
	}
	if len(pods.deleted) != 0 {
		t.Errorf("deleted pods: got %v, want none", pods.deleted)
	}
}

// TestStaleReaper_SkipsRecentAssigning guards the grace window: a bead flipped
// to "assigning" only a few minutes ago is a normal in-flight hand-off and must
// be left alone even when podless (the pod is still booting).
func TestStaleReaper_SkipsRecentAssigning(t *testing.T) {
	// 5m old: within the 10m assigningReapAfter grace.
	recent := time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-fresh-assign", Title: "fresh-assign", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "fresh-assign", "agent_state": "assigning",
			}),
			CreatedAt: recent, UpdatedAt: recent,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{livePods: map[string]bool{}}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0 (assigning bead within 10m grace must not be reaped)", stats.Reaped)
	}
}

// TestStaleReaper_AssigningPodListErrorSkipsReap is the fail-safe guard: when
// ListAgentPods errors, livePodNames returns nil and every bead is treated as
// possibly-podded, so a transient K8s API blip never triggers a spurious reap
// of an assigning bead — it falls through to the unchanged 2h base-timeout path.
func TestStaleReaper_AssigningPodListErrorSkipsReap(t *testing.T) {
	wedged := time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339)

	srv := httptest.NewServer(beadFixture([]beadEntry{
		{
			ID: "bd-assign-blip", Title: "assign-blip", Type: "agent", Kind: "config", Status: "open",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "assign-blip", "agent_state": "assigning",
			}),
			CreatedAt: wedged, UpdatedAt: wedged,
		},
	}))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{listErr: fmt.Errorf("k8s API unavailable")}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Fatalf("reaped: got %d, want 0 (pod-list error must skip the assigning reap, not reap)", stats.Reaped)
	}
}

func TestReapCloseState(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		prior  string
		want   string
	}{
		{"orphan source task is benign done", "orphan: source task gd-X already closed", "working", "done"},
		{"orphan source task ignores prior state", "orphan: source task gd-X already closed", "spawning", "done"},
		{"orphan terminal preserves done", `orphan: terminal state "done" but bead still open after 20m`, "done", "done"},
		{"orphan terminal preserves failed", `orphan: terminal state "failed" but bead still open after 20m`, "failed", "failed"},
		{"orphan terminal preserves rate_limited", `orphan: terminal state "rate_limited" but bead still open after 20m`, "rate_limited", "rate_limited"},
		{"orphan terminal unknown prior defaults done", `orphan: terminal state "weird" but bead still open after 20m`, "weird", "done"},
		{"stale working is a failure", "stale: no activity for 2h0m0s", "working", "failed"},
		{"stale spawning is a failure", "stale: stuck in spawning state for 4h0m0s", "spawning", "failed"},
		{"unschedulable is a failure", "stale: unschedulable, cluster capacity (1h0m0s)", "spawning", "failed"},
		{"unknown state is a failure", `stale: unknown state "x", no activity for 2h0m0s`, "x", "failed"},
		{"snapshotless frozen thaw target is benign done", "orphan: frozen thaw target without snapshot, still open after 20m0s", "stopping", "done"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reapCloseState(tt.reason, tt.prior); got != tt.want {
				t.Errorf("reapCloseState(%q, %q) = %q, want %q", tt.reason, tt.prior, got, tt.want)
			}
		})
	}
}

// TestStaleReaper_OrphanSourceTaskClosesAsDone verifies the benign "source
// task already closed" orphan path closes the agent bead with agent_state=done
// (not failed), so a gardener that finished its loop and went dormant does not
// render a ❌ "failed" card. Regression: ka-0000001336.
func TestStaleReaper_OrphanSourceTaskClosesAsDone(t *testing.T) {
	// Gardener role uses the longer gardenerOrphanReapAfter grace, so age the
	// bead well past it to exercise the reap + benign-close path.
	recent := time.Now().Add(-30 * time.Minute).UTC().Format(time.RFC3339)

	var closedState string
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: []beadEntry{
				{
					ID: "bd-cap1", Title: "gardener-orphan", Type: "agent", Kind: "config", Status: "open",
					Fields: mustJSON(map[string]string{
						"project": "gardenio", "mode": "crew", "role": "gardener",
						"agent": "gardener-orphan", "agent_state": "working",
						"task_id": "gd-TASK-CAP",
					}),
					CreatedAt: recent, UpdatedAt: recent,
				},
			}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.URL.Path == "/v1/beads/gd-TASK-CAP" && r.Method == http.MethodGet:
			task := map[string]interface{}{
				"id": "gd-TASK-CAP", "title": "Finished", "type": "task",
				"kind": "issue", "status": "closed",
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(task)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			// CloseBeadAs flattens fields to the top level of the body.
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if s, ok := body["agent_state"].(string); ok {
				closedState = s
			}
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(handler))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Fatalf("reaped: got %d, want 1", stats.Reaped)
	}
	if closedState != "done" {
		t.Errorf("closed agent_state: got %q, want \"done\" (benign orphan must not be marked failed)", closedState)
	}
}

// --- run-instance task reconcile tests (ka-0000000889) ---

// runInstanceHandler builds a daemon HTTP handler that serves agent beads for
// ListAgentBeads (type=agent), in_progress task beads for the reconcile query
// (type=task), and records every bead close (POST /v1/beads/<id>/close) into
// closed, keyed by bead ID → close_reason. Both agent-bead closes (from the
// reap loop) and task-bead closes (from the reconciler) land here; tests assert
// on the specific task IDs.
func runInstanceHandler(t *testing.T, agentBeads, taskBeads []beadEntry, closed map[string]string, mu *sync.Mutex) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/close"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/close")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			reason, _ := body["close_reason"].(string)
			mu.Lock()
			closed[id] = reason
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			// The reconciler queries type=task&status=in_progress; the reaper's
			// ListAgentBeads queries type=agent. Serve whichever set matches.
			beads := agentBeads
			if strings.Contains(r.URL.RawQuery, "type=task") {
				beads = taskBeads
			}
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: beads}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func runInstanceTask(id, assignee, label string, updatedAt time.Time) beadEntry {
	return beadEntry{
		ID:       id,
		Title:    "Schedule: Triage-intake gardener",
		Type:     "task",
		Kind:     "issue",
		Status:   "in_progress",
		Assignee: assignee,
		Labels:   []string{label, "project:gardenio"},
		Fields: mustJSON(map[string]string{
			"project":           "gardenio",
			"schedule_fire_key": "2026-06-06T21:17",
		}),
		CreatedAt: updatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
	}
}

func workingAgentBead(id, name string, updatedAt time.Time) beadEntry {
	return beadEntry{
		ID: id, Title: name, Type: "agent", Kind: "config", Status: "open",
		Fields: mustJSON(map[string]string{
			"project": "gardenio", "mode": "crew", "role": "gardener",
			"agent": name, "agent_state": "working",
		}),
		CreatedAt: updatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
	}
}

func TestStaleReaper_ReconcilesRunInstance_AssigneeReapedThisPass(t *testing.T) {
	// An agent reaped this pass strands its in_progress run-instance task. The
	// reconciler closes that task immediately, regardless of the task's age.
	old := time.Now().Add(-5 * time.Hour)      // agent stale → reaped
	recent := time.Now().Add(-1 * time.Minute) // task within grace window

	agentBeads := []beadEntry{workingAgentBead("bd-agent-reaped", "triage-agent", old)}
	taskBeads := []beadEntry{runInstanceTask("gd-RUN1", "triage-agent", "source:schedule", recent)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, agentBeads, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1", stats.Reaped)
	}
	if stats.Reconciled != 1 {
		t.Errorf("reconciled: got %d, want 1", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-RUN1"]; !ok {
		t.Errorf("gd-RUN1 was not closed; closed=%v", closed)
	}
}

func TestStaleReaper_ReconcilesRunInstance_AssigneeGonePriorPass(t *testing.T) {
	// The assignee is absent from the agent-bead list (crashed/OOMed and purged
	// in a prior pass). A run-instance task older than the grace window is
	// reconciled even though nothing was reaped this pass.
	stale := time.Now().Add(-30 * time.Minute) // older than the 15m grace

	taskBeads := []beadEntry{runInstanceTask("gd-RUN2", "ghost-agent", "source:multi", stale)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0", stats.Reaped)
	}
	if stats.Reconciled != 1 {
		t.Errorf("reconciled: got %d, want 1", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-RUN2"]; !ok {
		t.Errorf("gd-RUN2 was not closed; closed=%v", closed)
	}
}

func TestStaleReaper_SkipsRunInstance_AssigneeLive(t *testing.T) {
	// A run-instance task whose assignee is still a live (not-reaped) agent must
	// NOT be reconciled — the agent is actively working it.
	recent := time.Now().Add(-1 * time.Minute) // agent fresh → not reaped

	agentBeads := []beadEntry{workingAgentBead("bd-agent-live", "live-agent", recent)}
	// Task is old, but its assignee is alive.
	taskBeads := []beadEntry{runInstanceTask("gd-RUN3", "live-agent", "source:schedule", time.Now().Add(-2*time.Hour))}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, agentBeads, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reconciled != 0 {
		t.Errorf("reconciled: got %d, want 0 (live assignee must be left alone)", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-RUN3"]; ok {
		t.Errorf("gd-RUN3 was closed (%q) but should have been left alone", closed["gd-RUN3"])
	}
}

func TestStaleReaper_SkipsRunInstance_GoneButWithinGrace(t *testing.T) {
	// Assignee absent from the agent list (gone in a prior pass) but the task was
	// claimed very recently — within the grace window. Skip it to avoid racing a
	// freshly-claimed task whose agent bead hasn't surfaced in ListAgentBeads.
	fresh := time.Now().Add(-2 * time.Minute) // inside the 15m grace

	taskBeads := []beadEntry{runInstanceTask("gd-RUN4", "maybe-new-agent", "source:multi", fresh)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reconciled != 0 {
		t.Errorf("reconciled: got %d, want 0 (recently-claimed task within grace should be skipped)", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-RUN4"]; ok {
		t.Errorf("gd-RUN4 was closed but should have been left within grace")
	}
}

func TestStaleReaper_SkipsNonRunInstanceTask(t *testing.T) {
	// An ordinary (non run-instance) in_progress task whose assignee is gone is
	// not schedule/cron residue and must be left alone — its lifecycle is not
	// owned by an ephemeral run-instance agent.
	stale := time.Now().Add(-30 * time.Minute)

	ordinary := beadEntry{
		ID: "gd-ORD1", Title: "Implement feature X", Type: "task", Kind: "issue", Status: "in_progress",
		Assignee:  "ghost-agent",
		Labels:    []string{"project:gardenio"}, // no source:schedule/multi label, no fire-key
		Fields:    mustJSON(map[string]string{"project": "gardenio"}),
		CreatedAt: stale.UTC().Format(time.RFC3339),
		UpdatedAt: stale.UTC().Format(time.RFC3339),
	}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, []beadEntry{ordinary}, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reconciled != 0 {
		t.Errorf("reconciled: got %d, want 0 (ordinary task must not be touched)", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-ORD1"]; ok {
		t.Errorf("gd-ORD1 ordinary task was closed but should have been left alone")
	}
}

func TestStaleReaper_ReconcilesRunInstance_FireKeyFieldFallback(t *testing.T) {
	// A run-instance task whose source labels were stripped but still carries a
	// multi_fire_key field is recognized via the field fallback.
	stale := time.Now().Add(-30 * time.Minute)

	task := beadEntry{
		ID: "gd-RUN5", Title: "Multi[cron]: gardener-release-owner", Type: "task", Kind: "issue", Status: "in_progress",
		Assignee: "ghost-agent",
		Labels:   []string{"project:gardenio"}, // labels stripped
		Fields: mustJSON(map[string]string{
			"project":        "gardenio",
			"multi_fire_key": "0:0:2026-06-06T20:43",
		}),
		CreatedAt: stale.UTC().Format(time.RFC3339),
		UpdatedAt: stale.UTC().Format(time.RFC3339),
	}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, []beadEntry{task}, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reconciled != 1 {
		t.Errorf("reconciled: got %d, want 1 (multi_fire_key fallback should match)", stats.Reconciled)
	}
}

// --- open run-instance reconcile (ka-0000000075) ---

// stubReaperTunables satisfies statusreporter.Tunables with a fixed GetBool
// result, so tests can exercise the open-run-instance close gate in both dry-run
// (compiled default, false) and enabled (true) modes.
type stubReaperTunables struct{ closeOpen bool }

func (s stubReaperTunables) GetBool(key, envVar string, compiled bool) bool {
	return s.closeOpen
}

// openRunInstanceTask builds an OPEN, never-claimed schedule run-instance
// fire-record bead — the shape reconcileOpenRunInstanceTasks reaps. Empty
// assignee (never claimed) so the in_progress reconciler leaves it alone.
func openRunInstanceTask(id string, updatedAt time.Time) beadEntry {
	return beadEntry{
		ID:       id,
		Title:    "Schedule: Triage-intake gardener",
		Type:     "task",
		Kind:     "issue",
		Status:   "open",
		Assignee: "", // never claimed
		Labels:   []string{"source:schedule", "project:gardenio"},
		Fields: mustJSON(map[string]string{
			"project":           "gardenio",
			"schedule_fire_key": "2026-06-06T21:17",
		}),
		CreatedAt: updatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
	}
}

func TestStaleReaper_ClosesOpenRunInstance_WhenEnabled(t *testing.T) {
	// An OPEN, never-claimed run-instance aged past the grace is CLOSED once the
	// gate is enabled. This is the ~930-fire-record backlog (ka-0000000075).
	stale := time.Now().Add(-30 * time.Minute) // older than the 15m grace

	taskBeads := []beadEntry{openRunInstanceTask("gd-OPEN1", stale)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetTunables(stubReaperTunables{closeOpen: true})
	stats := reaper.Reap(context.Background())

	if stats.OpenRunInstancesClosed != 1 {
		t.Errorf("OpenRunInstancesClosed: got %d, want 1", stats.OpenRunInstancesClosed)
	}
	if stats.Reconciled != 0 {
		t.Errorf("Reconciled: got %d, want 0 (empty-assignee open bead is not the in_progress path)", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	reason, ok := closed["gd-OPEN1"]
	if !ok {
		t.Fatalf("gd-OPEN1 was not closed; closed=%v", closed)
	}
	if !strings.Contains(reason, "stranded open run-instance (never claimed)") {
		t.Errorf("close reason = %q, want it to mention 'stranded open run-instance (never claimed)'", reason)
	}
}

func TestStaleReaper_ClosesOpenRunInstance_ByDefaultWhenNoTunables(t *testing.T) {
	// With no tunables source wired (tunables nil), the gate honors the compiled
	// default, which is now ON after the 37-day dry-run confirmed the count and
	// precision (ka-0000006054). An aged open run-instance is CLOSED, not left in
	// dry-run.
	stale := time.Now().Add(-30 * time.Minute)

	taskBeads := []beadEntry{openRunInstanceTask("gd-OPEN2", stale)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	// No SetTunables → tunables nil → gate honors the compiled default (now ON).
	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.OpenRunInstancesClosed != 1 {
		t.Errorf("OpenRunInstancesClosed: got %d, want 1", stats.OpenRunInstancesClosed)
	}
	mu.Lock()
	defer mu.Unlock()
	reason, ok := closed["gd-OPEN2"]
	if !ok {
		t.Fatalf("gd-OPEN2 was not closed; default is now ON so it should be reaped; closed=%v", closed)
	}
	if !strings.Contains(reason, "stranded open run-instance (never claimed)") {
		t.Errorf("close reason = %q, want it to mention 'stranded open run-instance (never claimed)'", reason)
	}
}

func TestStaleReaper_DryRunExplicitFalse_DoesNotCloseOpenRunInstance(t *testing.T) {
	// An explicit tunable of false is the operator kill-switch back to dry-run.
	stale := time.Now().Add(-30 * time.Minute)

	taskBeads := []beadEntry{openRunInstanceTask("gd-OPEN3", stale)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetTunables(stubReaperTunables{closeOpen: false})
	stats := reaper.Reap(context.Background())

	if stats.OpenRunInstancesClosed != 1 {
		t.Errorf("OpenRunInstancesClosed (dry-run count): got %d, want 1", stats.OpenRunInstancesClosed)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-OPEN3"]; ok {
		t.Errorf("gd-OPEN3 was closed with the gate off but should have been left alone")
	}
}

func TestStaleReaper_SkipsOpenRunInstance_WithinGrace(t *testing.T) {
	// A freshly-fired open run-instance (within the 15m grace) is left alone even
	// when the close gate is enabled — it may be seconds from being claimed.
	fresh := time.Now().Add(-2 * time.Minute) // inside the grace

	taskBeads := []beadEntry{openRunInstanceTask("gd-OPEN4", fresh)}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, taskBeads, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetTunables(stubReaperTunables{closeOpen: true})
	stats := reaper.Reap(context.Background())

	if stats.OpenRunInstancesClosed != 0 {
		t.Errorf("OpenRunInstancesClosed: got %d, want 0 (within grace)", stats.OpenRunInstancesClosed)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-OPEN4"]; ok {
		t.Errorf("gd-OPEN4 was closed but is within the grace window")
	}
}

func TestStaleReaper_SkipsOpenRunInstance_WithAssignee(t *testing.T) {
	// A CLAIMED open run-instance (assignee set) is a live/racing claim, not an
	// abandoned fire record — the open path must leave it to the in_progress path.
	stale := time.Now().Add(-30 * time.Minute)

	claimed := openRunInstanceTask("gd-OPEN5", stale)
	claimed.Assignee = "some-agent" // claimed → not ours

	closed := map[string]string{}
	var mu sync.Mutex
	// Provide a live agent so the in_progress reconciler also leaves it alone.
	agentBeads := []beadEntry{workingAgentBead("bd-live", "some-agent", time.Now().Add(-1*time.Minute))}
	srv := httptest.NewServer(runInstanceHandler(t, agentBeads, []beadEntry{claimed}, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetTunables(stubReaperTunables{closeOpen: true})
	stats := reaper.Reap(context.Background())

	if stats.OpenRunInstancesClosed != 0 {
		t.Errorf("OpenRunInstancesClosed: got %d, want 0 (assignee set → open path must skip)", stats.OpenRunInstancesClosed)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-OPEN5"]; ok {
		t.Errorf("gd-OPEN5 (claimed) was closed by the open path but should have been skipped")
	}
}

func TestStaleReaper_SkipsOpenNonRunInstanceTask(t *testing.T) {
	// Defense in depth behind the server-side label filter: an ordinary OPEN task
	// with no source label / fire-key must NOT be closed even if it reaches the
	// open reconciler (isRunInstanceTask re-check).
	stale := time.Now().Add(-30 * time.Minute)

	ordinary := beadEntry{
		ID: "gd-OPEN6", Title: "Implement feature X", Type: "task", Kind: "issue", Status: "open",
		Assignee:  "",
		Labels:    []string{"project:gardenio"}, // no source:schedule/multi, no fire-key
		Fields:    mustJSON(map[string]string{"project": "gardenio"}),
		CreatedAt: stale.UTC().Format(time.RFC3339),
		UpdatedAt: stale.UTC().Format(time.RFC3339),
	}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandler(t, nil, []beadEntry{ordinary}, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetTunables(stubReaperTunables{closeOpen: true})
	stats := reaper.Reap(context.Background())

	if stats.OpenRunInstancesClosed != 0 {
		t.Errorf("OpenRunInstancesClosed: got %d, want 0 (non-run-instance must be skipped)", stats.OpenRunInstancesClosed)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := closed["gd-OPEN6"]; ok {
		t.Errorf("gd-OPEN6 (ordinary open task) was closed but must be left alone")
	}
}

// runInstanceHandlerWithAgentLookup extends runInstanceHandler to also serve
// the GET /v1/agents-by-name/{name} endpoint that FindMostRecentAgentBead hits,
// returning agentByName[name] (a single bead JSON) or 404 when absent. Used to
// exercise the reaper's clean-stop provenance check (ka-0000006105).
func runInstanceHandlerWithAgentLookup(t *testing.T, agentBeads, taskBeads []beadEntry, agentByName map[string]beadEntry, closed map[string]string, mu *sync.Mutex) http.HandlerFunc {
	t.Helper()
	base := runInstanceHandler(t, agentBeads, taskBeads, closed, mu)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/") {
			name := strings.TrimPrefix(r.URL.Path, "/v1/agents-by-name/")
			if b, ok := agentByName[name]; ok {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(b)
				return
			}
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		base(w, r)
	}
}

// closedAgentBeadWithWrapUp builds a CLOSED agent bead carrying a genuine
// agent-recorded wrap-up (accomplishments set, not synthesized) — the fingerprint
// of an agent that stopped cleanly via gb stop rather than crashing.
func closedAgentBeadWithWrapUp(name string) beadEntry {
	wrapup := `{"tldr":"finished grooming","accomplishments":"triaged 5 beads"}`
	return beadEntry{
		ID: "bd-" + name, Title: name, Type: "agent", Kind: "config", Status: "closed",
		Fields: mustJSON(map[string]string{
			"project": "gardenio", "mode": "crew", "role": "gardener",
			"agent": name, "agent_state": "done",
			beadsapi.WrapUpFieldName: wrapup,
		}),
	}
}

// TestStaleReaper_RunInstance_CleanStopProvenance verifies that when the stranded
// run-instance task's assignee has a genuine wrap-up on its (closed) agent bead,
// the reconcile close_reason reflects a clean gb stop, NOT a false crash/OOM
// (ka-0000006105).
func TestStaleReaper_RunInstance_CleanStopProvenance(t *testing.T) {
	stale := time.Now().Add(-30 * time.Minute)
	taskBeads := []beadEntry{runInstanceTask("gd-RUNCS", "clean-agent", "source:schedule", stale)}
	// Assignee absent from active agent list (its bead is closed), but resolvable
	// by-name with a real wrap-up.
	agentByName := map[string]beadEntry{"clean-agent": closedAgentBeadWithWrapUp("clean-agent")}

	closed := map[string]string{}
	var mu sync.Mutex
	srv := httptest.NewServer(runInstanceHandlerWithAgentLookup(t, nil, taskBeads, agentByName, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reconciled != 1 {
		t.Fatalf("reconciled: got %d, want 1", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	reason := closed["gd-RUNCS"]
	if reason == "" {
		t.Fatalf("gd-RUNCS was not closed; closed=%v", closed)
	}
	if strings.Contains(reason, "crash/OOM") {
		t.Errorf("close_reason falsely asserts crash/OOM for a clean stop: %q", reason)
	}
	if !strings.Contains(reason, "gb stop") {
		t.Errorf("close_reason should credit a clean gb stop, got: %q", reason)
	}
}

// TestStaleReaper_RunInstance_CrashProvenanceWhenNoWrapUp confirms the default is
// preserved: an assignee with no resolvable wrap-up (true crash/OOM) still gets
// the crash/OOM close_reason.
func TestStaleReaper_RunInstance_CrashProvenanceWhenNoWrapUp(t *testing.T) {
	stale := time.Now().Add(-30 * time.Minute)
	taskBeads := []beadEntry{runInstanceTask("gd-RUNCRASH", "ghost-agent", "source:multi", stale)}

	closed := map[string]string{}
	var mu sync.Mutex
	// No agentByName entry → by-name lookup 404s → crash/OOM default holds.
	srv := httptest.NewServer(runInstanceHandlerWithAgentLookup(t, nil, taskBeads, nil, closed, &mu))
	defer srv.Close()

	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()

	pods := &mockPodManager{}
	reaper := NewStaleReaper(daemon, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	stats := reaper.Reap(context.Background())

	if stats.Reconciled != 1 {
		t.Fatalf("reconciled: got %d, want 1", stats.Reconciled)
	}
	mu.Lock()
	defer mu.Unlock()
	reason := closed["gd-RUNCRASH"]
	if !strings.Contains(reason, "crash/OOM") {
		t.Errorf("close_reason should keep the crash/OOM default when no wrap-up exists, got: %q", reason)
	}
}

func mustJSON(m map[string]string) json.RawMessage {
	b, err := json.Marshal(m)
	if err != nil {
		panic(err)
	}
	return b
}

// --- EOL salvage Phase 2b: reopen stranded claimed ISSUE tasks (ka-0000002469) ---

// reopenResult captures what the reopen path did to a single task bead: the
// PATCH (status/assignee) and the audit comment.
type reopenResult struct {
	patched        bool
	status         string
	assignee       string
	hasAssigneeKey bool
	comment        string
	labels         []string
	hasLabelsKey   bool
	reapedAssignee string
}

// reopenExtras carries the optional gd-t0gm8q62Ac Part 2 fixtures
// (open-handle probe results and per-task comment trails) that older
// reopenIssueHandler callers don't need and so omit.
type reopenExtras struct {
	// openHandleTasks maps taskID -> true when a live escalation/decision
	// bead (requesting_task=taskID) should be reported as still parking it.
	openHandleTasks map[string]bool
	// commentsByTask maps taskID -> comment texts returned by GET
	// /v1/beads/{id}/comments, for the linked-PR comment-scan fallback.
	commentsByTask map[string][]string
}

// reopenIssueHandler serves the daemon endpoints reopenStrandedIssueTasks hits:
//   - GET /v1/beads?type=agent...          → agentBeads (ListAgentBeads)
//   - GET /v1/beads?kind=issue...          → issueBeads (the reopen list)
//   - GET /v1/beads?type=task...           → empty (run-instance reconcile; unused here)
//   - GET /v1/agents-by-name/{name}        → agentByName[name] or 404 (assigneeIsAgent)
//   - PATCH /v1/beads/{id}                 → recorded in results (the reopen)
//   - POST /v1/beads/{id}/comments         → recorded in results (audit comment)
//
// Ordering of the GET /v1/beads branch matters: check type=agent before
// kind=issue so the agent listing is never mistaken for the issue listing.
func reopenIssueHandler(
	t *testing.T,
	agentBeads, issueBeads []beadEntry,
	agentByName map[string]beadEntry,
	results map[string]*reopenResult,
	mu *sync.Mutex,
	extras ...reopenExtras,
) http.HandlerFunc {
	t.Helper()
	var ex reopenExtras
	if len(extras) > 0 {
		ex = extras[0]
	}
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.RawQuery, "type=escalation"):
			// taskHasOpenHandle's probe (gd-t0gm8q62Ac Part 2): the field=
			// param carries "requesting_task=<taskID>".
			taskID := ""
			for _, f := range r.URL.Query()["field"] {
				if after, ok := strings.CutPrefix(f, "requesting_task="); ok {
					taskID = after
				}
			}
			var beads []beadEntry
			if ex.openHandleTasks[taskID] {
				beads = []beadEntry{{ID: "esc-" + taskID, Type: "escalation", Status: "open"}}
			}
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: beads}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			type comment struct {
				ID     int64  `json:"id"`
				Author string `json:"author"`
				Text   string `json:"text"`
			}
			var comments []comment
			for i, text := range ex.commentsByTask[id] {
				comments = append(comments, comment{ID: int64(i + 1), Author: "gardenio", Text: text})
			}
			resp := struct {
				Comments []comment `json:"comments"`
			}{Comments: comments}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			// Emulate the daemon's status-scoped lookup so
			// FindMostRecentAgentBead's two-call behavior (active first, then
			// closed) is exercised faithfully: only return the bead when its
			// status matches the requested ?status= filter (empty = any).
			name := strings.TrimPrefix(r.URL.Path, "/v1/agents-by-name/")
			b, ok := agentByName[name]
			if ok {
				if want := r.URL.Query().Get("status"); want != "" && !strings.Contains(want, b.Status) {
					http.Error(w, "agent not found", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(b)
				return
			}
			http.Error(w, "agent not found", http.StatusNotFound)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			res := results[id]
			if res == nil {
				res = &reopenResult{}
				results[id] = res
			}
			res.patched = true
			if s, ok := body["status"].(string); ok {
				res.status = s
			}
			if a, ok := body["assignee"]; ok {
				res.hasAssigneeKey = true
				res.assignee, _ = a.(string)
			}
			if f, ok := body["fields"].(map[string]any); ok {
				res.reapedAssignee, _ = f[beadsapi.ReapedAssigneeField].(string)
			}
			if l, ok := body["labels"].([]any); ok {
				res.hasLabelsKey = true
				res.labels = nil
				for _, v := range l {
					if s, ok := v.(string); ok {
						res.labels = append(res.labels, s)
					}
				}
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			res := results[id]
			if res == nil {
				res = &reopenResult{}
				results[id] = res
			}
			res.comment = body["text"]
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			var beads []beadEntry
			switch {
			case strings.Contains(r.URL.RawQuery, "type=agent"):
				beads = agentBeads
			case strings.Contains(r.URL.RawQuery, "kind=issue"):
				// Status-filter like the real daemon so a status=open query
				// (clearStrandedOpenAssignees) never sees a fixture still at
				// status=in_progress, and vice versa — otherwise a single
				// static issueBeads fixture would cross-contaminate the two
				// sibling sweeps within one Reap() call.
				want := r.URL.Query().Get("status")
				for _, b := range issueBeads {
					if want == "" || strings.Contains(want, b.Status) {
						beads = append(beads, b)
					}
				}
			}
			// A type=task (run-instance reconcile) query falls through to empty.
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: beads}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// claimedIssueTask builds an ordinary (non run-instance) claimed issue task —
// the shape a crew agent claims via `kd claim` and strands in_progress when it
// is hard-killed. No source:schedule/multi label, no fire-key field.
func claimedIssueTask(id, assignee string, updatedAt time.Time) beadEntry {
	return beadEntry{
		ID:        id,
		Title:     "Fix the widget",
		Type:      "task",
		Kind:      "issue",
		Status:    "in_progress",
		Assignee:  assignee,
		Labels:    []string{"project:gardenio"},
		Fields:    mustJSON(map[string]string{"project": "gardenio"}),
		CreatedAt: updatedAt.UTC().Format(time.RFC3339),
		UpdatedAt: updatedAt.UTC().Format(time.RFC3339),
	}
}

// closedAgentBead builds a CLOSED type=agent bead — the fingerprint of an agent
// that has terminated. assigneeIsGoneAgent requires this (a *closed* status) as
// positive proof the agent is gone before reopening its task.
func closedAgentBead(id, name string, updatedAt time.Time) beadEntry {
	b := workingAgentBead(id, name, updatedAt)
	b.Status = "closed"
	return b
}

func newReopenReaper(t *testing.T, handler http.HandlerFunc) (*StaleReaper, func()) {
	t.Helper()
	srv := httptest.NewServer(handler)
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	reaper := NewStaleReaper(daemon, &mockPodManager{}, "gardenio", 2*time.Hour, staleReaperLogger())
	return reaper, func() {
		daemon.Close()
		srv.Close()
	}
}

func TestStaleReaper_ReopensStrandedIssue_AssigneeReapedThisPass(t *testing.T) {
	// A crew agent stale past the timeout is reaped this pass, stranding the
	// issue task it had claimed. The reopen path returns that task to the ready
	// queue immediately (no grace, no agent-lookup — reaped == provably gone).
	old := time.Now().Add(-5 * time.Hour)      // agent stale → reaped this pass
	recent := time.Now().Add(-1 * time.Minute) // task claimed recently

	agentBeads := []beadEntry{workingAgentBead("bd-worker", "worker-agent", old)}
	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE1", "worker-agent", recent)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, agentBeads, issueBeads, nil, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1", stats.Reaped)
	}
	if stats.Reopened != 1 {
		t.Errorf("reopened: got %d, want 1", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	res := results["gd-ISSUE1"]
	if res == nil || !res.patched {
		t.Fatalf("gd-ISSUE1 was not reopened; results=%v", results)
	}
	if res.status != "open" {
		t.Errorf("reopen status: got %q, want %q", res.status, "open")
	}
	if !res.hasAssigneeKey || res.assignee != "" {
		t.Errorf("reopen must clear the assignee; hasKey=%v assignee=%q", res.hasAssigneeKey, res.assignee)
	}
	if res.reapedAssignee != "worker-agent" {
		t.Errorf("reopen must record the cleared assignee as %s, got %q", beadsapi.ReapedAssigneeField, res.reapedAssignee)
	}
	if !strings.Contains(res.comment, "worker-agent") || !strings.Contains(res.comment, "ready queue") {
		t.Errorf("audit comment missing expected content: %q", res.comment)
	}
}

func TestStaleReaper_ReopensStrandedIssue_AgentGonePriorPass(t *testing.T) {
	// The assignee agent is absent from the active agent list (hard-killed /
	// OOMed in a prior pass, never reaped). A task older than the grace window
	// whose assignee resolves by-name as an AGENT is reopened even though
	// nothing was reaped this pass.
	stale := time.Now().Add(-30 * time.Minute) // older than the 15m grace

	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE2", "ghost-agent", stale)}
	// The agent's bead is CLOSED (it terminated) — positive proof it is gone.
	agentByName := map[string]beadEntry{"ghost-agent": closedAgentBead("bd-ghost", "ghost-agent", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, agentByName, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reaped != 0 {
		t.Errorf("reaped: got %d, want 0", stats.Reaped)
	}
	if stats.Reopened != 1 {
		t.Errorf("reopened: got %d, want 1", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-ISSUE2"]; res == nil || res.status != "open" || res.assignee != "" {
		t.Errorf("gd-ISSUE2 not reopened correctly; res=%+v", res)
	}
}

func TestStaleReaper_SkipsIssue_AssigneeLive(t *testing.T) {
	// An issue task whose assignee is still a live (not-reaped) agent must NOT
	// be reopened — the agent is actively working it.
	recent := time.Now().Add(-1 * time.Minute) // agent fresh → not reaped

	agentBeads := []beadEntry{workingAgentBead("bd-live", "live-agent", recent)}
	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE3", "live-agent", time.Now().Add(-2*time.Hour))}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, agentBeads, issueBeads, nil, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (live assignee must be left alone)", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-ISSUE3"]; res != nil && res.patched {
		t.Errorf("gd-ISSUE3 was reopened but should have been left alone; res=%+v", res)
	}
}

func TestStaleReaper_SkipsIssue_GoneButWithinGrace(t *testing.T) {
	// Assignee absent from the agent list (gone in a prior pass) but the task
	// was claimed very recently — within the grace window. Skip it to avoid
	// racing a freshly-claimed task whose agent bead hasn't surfaced yet.
	fresh := time.Now().Add(-2 * time.Minute) // inside the 15m grace

	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE4", "maybe-new-agent", fresh)}
	// Even with a closed agent bead available, the grace guard fires first.
	agentByName := map[string]beadEntry{"maybe-new-agent": closedAgentBead("bd-maybe", "maybe-new-agent", fresh)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, agentByName, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (recently-claimed task within grace must be skipped)", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-ISSUE4"]; res != nil && res.patched {
		t.Errorf("gd-ISSUE4 was reopened but should have been left within grace")
	}
}

func TestStaleReaper_SkipsIssue_HumanAssignee(t *testing.T) {
	// The critical safety guard: an issue task claimed by a HUMAN (whose name
	// does not resolve to an agent bead), stranded past the grace, must NEVER be
	// reopened out from under its owner. The by-name agent lookup 404s → the
	// assignee is not provably an agent → leave it alone.
	stale := time.Now().Add(-30 * time.Minute) // past grace

	// No agent named "matthew.baker" in the active list and no by-name entry.
	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE5", "matthew.baker", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, nil, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (a human-claimed task must never be reopened)", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-ISSUE5"]; res != nil && res.patched {
		t.Errorf("gd-ISSUE5 (human-claimed) was reopened but must be left alone; res=%+v", res)
	}
}

func TestStaleReaper_SkipsIssue_TruncatedButLiveAgent(t *testing.T) {
	// Safety guard for the page-cap hazard (adversarial review Finding 3): on a
	// very large fleet ListAgentBeads is page-capped, so a genuinely LIVE agent
	// can be truncated out of the liveAgents map. Absence from liveAgents must
	// NOT be treated as "gone". Here the assignee is absent from the (empty)
	// agent list but resolves by-name to an ACTIVE (open, working) agent bead —
	// proof it is still alive. Its task must be left alone even past the grace.
	stale := time.Now().Add(-30 * time.Minute) // past grace

	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE7", "busy-but-truncated", stale)}
	// Active agent bead (open) → still alive → must NOT be reopened.
	agentByName := map[string]beadEntry{
		"busy-but-truncated": workingAgentBead("bd-busy", "busy-but-truncated", stale),
	}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, agentByName, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (a live-but-truncated agent's task must never be reopened)", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-ISSUE7"]; res != nil && res.patched {
		t.Errorf("gd-ISSUE7 (live-but-truncated agent) was reopened but must be left alone; res=%+v", res)
	}
}

func TestStaleReaper_SkipsRunInstanceInIssueReopen(t *testing.T) {
	// A run-instance task (schedule/cron residue) surfaces in the kind=issue
	// listing too, but the reopen path must SKIP it — run-instances are
	// single-shot and owned by reconcileRunInstanceTasks (which CLOSES them),
	// not retriable work to be reopened.
	stale := time.Now().Add(-30 * time.Minute)

	// Assignee resolves as an agent, so only the isRunInstanceTask guard keeps
	// this from being reopened.
	issueBeads := []beadEntry{runInstanceTask("gd-RUNRI", "ghost-agent", "source:schedule", stale)}
	agentByName := map[string]beadEntry{"ghost-agent": workingAgentBead("bd-ghost2", "ghost-agent", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, issueBeads, agentByName, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (run-instance must not be reopened)", stats.Reopened)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-RUNRI"]; res != nil && res.patched {
		t.Errorf("gd-RUNRI (run-instance) was reopened but must be left to the close path")
	}
}

func TestStaleReaper_SkipsIssue_Unassigned(t *testing.T) {
	// An unassigned in_progress issue task (no owner to release) is a no-op for
	// the reopen path.
	stale := time.Now().Add(-30 * time.Minute)
	unassigned := claimedIssueTask("gd-ISSUE6", "", stale)

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, nil, []beadEntry{unassigned}, nil, results, &mu))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (unassigned task is a no-op)", stats.Reopened)
	}
}

// --- gd-t0gm8q62Ac Part 2: liveness reconciler guards on reopenStrandedIssueTasks ---

func TestStaleReaper_SkipsIssue_OpenEscalationHandle(t *testing.T) {
	// The assignee agent is definitively gone (reaped this pass), but a live
	// escalation bead still parks the task (requesting_task=gd-ISSUE7). The
	// backstop must NOT reopen — Part 1's respawn-on-resolve, or a human
	// resolving the escalation, is what should bring the task back.
	old := time.Now().Add(-5 * time.Hour)
	recent := time.Now().Add(-1 * time.Minute)

	agentBeads := []beadEntry{workingAgentBead("bd-worker", "worker-agent", old)}
	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE7", "worker-agent", recent)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	extras := reopenExtras{openHandleTasks: map[string]bool{"gd-ISSUE7": true}}
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, agentBeads, issueBeads, nil, results, &mu, extras))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (open escalation handle must block reopen)", stats.Reopened)
	}
	if stats.FlaggedOrphanedWithPR != 0 {
		t.Errorf("flagged: got %d, want 0 (open-handle skip is not a PR flag)", stats.FlaggedOrphanedWithPR)
	}
	if res := results["gd-ISSUE7"]; res != nil && res.patched {
		t.Errorf("gd-ISSUE7 should not have been touched; results=%+v", res)
	}
}

func TestStaleReaper_FlagsIssue_LinkedPRInsteadOfReopening(t *testing.T) {
	// The assignee agent is gone (reaped this pass) and no escalation/decision
	// parks the task, but a comment on the task links a GitHub PR. The backstop
	// must NOT reopen — reopening risks a second crew duplicating work already
	// up for review — and instead labels + comments for a follow-through crew.
	old := time.Now().Add(-5 * time.Hour)
	recent := time.Now().Add(-1 * time.Minute)

	agentBeads := []beadEntry{workingAgentBead("bd-worker", "worker-agent", old)}
	issueBeads := []beadEntry{claimedIssueTask("gd-ISSUE8", "worker-agent", recent)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	prURL := "https://github.com/joyousaversion/gardenio/pull/42"
	extras := reopenExtras{commentsByTask: map[string][]string{
		"gd-ISSUE8": {"Opened " + prURL + " for review."},
	}}
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, agentBeads, issueBeads, nil, results, &mu, extras))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reopened != 0 {
		t.Errorf("reopened: got %d, want 0 (linked PR must block reopen)", stats.Reopened)
	}
	if stats.FlaggedOrphanedWithPR != 1 {
		t.Errorf("flagged: got %d, want 1", stats.FlaggedOrphanedWithPR)
	}
	mu.Lock()
	defer mu.Unlock()
	res := results["gd-ISSUE8"]
	if res == nil || !res.patched {
		t.Fatalf("gd-ISSUE8 was not labeled; results=%v", results)
	}
	if res.status != "" {
		t.Errorf("flag must not change status: got %q, want unset", res.status)
	}
	if res.hasAssigneeKey {
		t.Errorf("flag must not touch assignee")
	}
	if !res.hasLabelsKey || !slices.Contains(res.labels, orphanedWithPRLabel) {
		t.Errorf("expected label %q on flagged task; got labels=%v", orphanedWithPRLabel, res.labels)
	}
	if !slices.Contains(res.labels, "project:gardenio") {
		t.Errorf("flag must preserve existing labels; got labels=%v", res.labels)
	}
	if !strings.Contains(res.comment, prURL) {
		t.Errorf("audit comment missing linked PR URL: %q", res.comment)
	}
}

// --- gd-ovzZSuuHny: a closed or merged linked PR does not keep a task flagged ---

// runLinkedPRReap reaps one stranded task (assignee reaped this pass) whose
// comments link prURLs, with lookup as the PR state reader (nil = none). It
// returns the stats and what the reaper did to the task.
func runLinkedPRReap(t *testing.T, labels []string, comments []string, lookup PRStateLookup) (StaleReaperStats, *reopenResult) {
	t.Helper()
	task := claimedIssueTask("gd-ISSUEPR", "worker-agent", time.Now().Add(-1*time.Minute))
	if labels != nil {
		task.Labels = labels
	}
	stats, results := reapLinkedPRTasks(t, []beadEntry{task}, map[string][]string{"gd-ISSUEPR": comments}, func(r *StaleReaper) {
		if lookup != nil {
			r.SetPRStateLookup(lookup)
		}
	})
	res := results["gd-ISSUEPR"]
	if res == nil {
		res = &reopenResult{}
	}
	return stats, res
}

// reapLinkedPRTasks reaps tasks, all claimed by worker-agent (reaped this
// pass), with commentsByTask as their comment trails. configure sets the
// reaper up before the pass. It returns the stats and what the reaper did to
// each task.
func reapLinkedPRTasks(t *testing.T, tasks []beadEntry, commentsByTask map[string][]string, configure func(*StaleReaper)) (StaleReaperStats, map[string]*reopenResult) {
	t.Helper()
	old := time.Now().Add(-5 * time.Hour)
	agentBeads := []beadEntry{workingAgentBead("bd-worker", "worker-agent", old)}
	results := map[string]*reopenResult{}
	var mu sync.Mutex
	extras := reopenExtras{commentsByTask: commentsByTask}
	reaper, cleanup := newReopenReaper(t, reopenIssueHandler(t, agentBeads, tasks, nil, results, &mu, extras))
	defer cleanup()
	configure(reaper)
	stats := reaper.Reap(context.Background())
	mu.Lock()
	defer mu.Unlock()
	return stats, results
}

// prStates is a PRStateLookup answering from a fixed map; a URL not in it is
// a lookup error. Every URL asked is recorded in asked.
func prStates(states map[string]string, asked *[]string) PRStateLookup {
	return func(_ context.Context, prURL string) (string, error) {
		if asked != nil {
			*asked = append(*asked, prURL)
		}
		if s, ok := states[prURL]; ok {
			return s, nil
		}
		return "", errors.New("HTTP 502")
	}
}

const (
	testPR1 = "https://github.com/joyousaversion/gardenio/pull/1"
	testPR2 = "https://github.com/joyousaversion/gardenio/pull/2"
)

func TestStaleReaper_LinkedPROpen_Flags(t *testing.T) {
	stats, res := runLinkedPRReap(t, nil, []string{"Opened " + testPR1}, prStates(map[string]string{testPR1: PRStateOpen}, nil))
	if stats.Reopened != 0 || stats.FlaggedOrphanedWithPR != 1 {
		t.Fatalf("reopened=%d flagged=%d, want 0/1 (an open PR keeps the flag)", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
	if res.status != "" || !slices.Contains(res.labels, orphanedWithPRLabel) {
		t.Errorf("want flag only; got status=%q labels=%v", res.status, res.labels)
	}
}

func TestStaleReaper_LinkedPRClosedOrMerged_Reopens(t *testing.T) {
	for _, state := range []string{PRStateClosed, PRStateMerged} {
		t.Run(state, func(t *testing.T) {
			stats, res := runLinkedPRReap(t, nil, []string{"Opened " + testPR1}, prStates(map[string]string{testPR1: state}, nil))
			if stats.Reopened != 1 || stats.FlaggedOrphanedWithPR != 0 {
				t.Fatalf("reopened=%d flagged=%d, want 1/0", stats.Reopened, stats.FlaggedOrphanedWithPR)
			}
			if res.status != "open" || !res.hasAssigneeKey || res.assignee != "" {
				t.Errorf("want status=open assignee cleared; got %+v", res)
			}
			if !strings.Contains(res.comment, testPR1+" ("+state+")") {
				t.Errorf("reopen comment should name the %s PR: %q", state, res.comment)
			}
		})
	}
}

// TestStaleReaper_AwaitsRelease_NotReopened: a task with awaits_release=true,
// a reaped assignee and a merged PR is done, and its release closes it from
// in_progress; reopening it put shipped work back on `gb ready`
// (gd-aaGqLb2cUk). TestStaleReaper_LinkedPRClosedOrMerged_Reopens is the same
// task without the field.
func TestStaleReaper_AwaitsRelease_NotReopened(t *testing.T) {
	task := claimedIssueTask("gd-ISSUEPR", "worker-agent", time.Now().Add(-1*time.Minute))
	task.Fields = mustJSON(map[string]string{"project": "gardenio", "awaits_release": "true"})
	lookup := prStates(map[string]string{testPR1: PRStateMerged}, nil)
	stats, results := reapLinkedPRTasks(t, []beadEntry{task}, map[string][]string{"gd-ISSUEPR": {"Opened " + testPR1}}, func(r *StaleReaper) {
		r.SetPRStateLookup(lookup)
	})
	if stats.Reopened != 0 || stats.FlaggedOrphanedWithPR != 0 {
		t.Fatalf("reopened=%d flagged=%d, want 0/0", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
	if res := results["gd-ISSUEPR"]; res != nil && (res.status != "" || res.hasAssigneeKey || res.comment != "") {
		t.Errorf("awaits_release task must be left untouched; got %+v", res)
	}
}

func TestStaleReaper_LinkedPRSettled_DropsExistingFlag(t *testing.T) {
	// gd-ke6PC7bRto: flagged on an earlier pass, PR closed since. The reopen
	// must shed task:orphaned-with-pr and keep every other label.
	labels := []string{"project:gardenio", orphanedWithPRLabel}
	stats, res := runLinkedPRReap(t, labels, []string{"Opened " + testPR1}, prStates(map[string]string{testPR1: PRStateClosed}, nil))
	if stats.Reopened != 1 {
		t.Fatalf("reopened=%d, want 1", stats.Reopened)
	}
	if !res.hasLabelsKey || slices.Contains(res.labels, orphanedWithPRLabel) || !slices.Contains(res.labels, "project:gardenio") {
		t.Errorf("want %q dropped and project label kept; got labels=%v (sent=%v)", orphanedWithPRLabel, res.labels, res.hasLabelsKey)
	}
}

func TestStaleReaper_LinkedPRLookupError_Flags(t *testing.T) {
	stats, res := runLinkedPRReap(t, nil, []string{"Opened " + testPR1}, prStates(nil, nil))
	if stats.Reopened != 0 || stats.FlaggedOrphanedWithPR != 1 {
		t.Fatalf("reopened=%d flagged=%d, want 0/1 (an unreadable PR keeps today's flag)", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
	if res.status != "" || !strings.Contains(res.comment, testPR1) {
		t.Errorf("want flag naming the PR; got status=%q comment=%q", res.status, res.comment)
	}
}

func TestStaleReaper_LinkedPROneClosedOneOpen_FlagsTheOpenOne(t *testing.T) {
	// The newer PR replaced a closed one: the task is still up for review.
	var asked []string
	lookup := prStates(map[string]string{testPR1: PRStateClosed, testPR2: PRStateOpen}, &asked)
	stats, res := runLinkedPRReap(t, nil, []string{"Opened " + testPR1, "Closed that, reopened as " + testPR2}, lookup)
	if stats.Reopened != 0 || stats.FlaggedOrphanedWithPR != 1 {
		t.Fatalf("reopened=%d flagged=%d, want 0/1", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
	if !strings.Contains(res.comment, testPR2) {
		t.Errorf("flag should name the open PR %s: %q", testPR2, res.comment)
	}
	if len(asked) == 0 || asked[0] != testPR2 {
		t.Errorf("the most recent PR should be asked first; asked=%v", asked)
	}
}

func TestStaleReaper_LinkedPRAllSettled_ReopensEvenWithTwo(t *testing.T) {
	lookup := prStates(map[string]string{testPR1: PRStateClosed, testPR2: PRStateMerged}, nil)
	stats, _ := runLinkedPRReap(t, nil, []string{"Opened " + testPR1, "Superseded by " + testPR2}, lookup)
	if stats.Reopened != 1 || stats.FlaggedOrphanedWithPR != 0 {
		t.Fatalf("reopened=%d flagged=%d, want 1/0", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
}

func TestStaleReaper_LinkedPRURLs_MostRecentFirst(t *testing.T) {
	r := &StaleReaper{daemon: nil, logger: staleReaperLogger()}
	base := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Served out of order: the newer comment first.
		_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
			{"id": 2, "text": "now " + testPR2, "created_at": base.Format(time.RFC3339)},
			{"id": 1, "text": "first " + testPR1, "created_at": base.Add(-time.Hour).Format(time.RFC3339)},
		}})
	}))
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	r.daemon = daemon
	pr3 := "https://github.com/joyousaversion/gardenio/pull/3"
	got := r.linkedPRURLs(context.Background(), &beadsapi.BeadDetail{ID: "gd-X", Description: "see " + pr3 + " and " + testPR1})
	want := []string{testPR2, testPR1, pr3}
	if !slices.Equal(got, want) {
		t.Errorf("linkedPRURLs = %v, want %v", got, want)
	}
}

func TestStaleReaper_PRStateCached(t *testing.T) {
	var asked []string
	r := &StaleReaper{now: time.Now, prState: prStates(map[string]string{testPR1: PRStateOpen}, &asked)}
	for range 3 {
		if s, err := r.lookupPRState(context.Background(), testPR1, nil); err != nil || s != PRStateOpen {
			t.Fatalf("lookup = %q, %v", s, err)
		}
	}
	if len(asked) != 1 {
		t.Errorf("asked GitHub %d times, want 1 (cached)", len(asked))
	}
	// Errors are not cached.
	for range 2 {
		if _, err := r.lookupPRState(context.Background(), testPR2, nil); err == nil {
			t.Fatal("want error")
		}
	}
	if len(asked) != 3 {
		t.Errorf("asked %d times, want a failed lookup retried", len(asked))
	}
	// Past the TTL the answer is read again.
	r.now = func() time.Time { return time.Now().Add(prStateCacheTTL + time.Minute) }
	_, _ = r.lookupPRState(context.Background(), testPR1, nil)
	if len(asked) != 4 {
		t.Errorf("asked %d times, want a re-read after the TTL", len(asked))
	}
}

// --- gd-wahYHJndnb: per-pass lookup budget, own-PR priority, closed-state TTL ---

func TestStaleReaper_PRLookupPassBudget_HungGitHubFlagsWithoutStalling(t *testing.T) {
	// GitHub hangs. The first lookup is cut off at the pass budget, the rest
	// are not even tried, and every task is flagged, never reopened.
	recent := time.Now().Add(-1 * time.Minute)
	var tasks []beadEntry
	comments := map[string][]string{}
	for i := range 4 {
		id := fmt.Sprintf("gd-HUNG%d", i)
		tasks = append(tasks, claimedIssueTask(id, "worker-agent", recent))
		comments[id] = []string{fmt.Sprintf("Opened https://github.com/joyousaversion/gardenio/pull/%d", 100+i)}
	}
	var calls atomic.Int32
	hung := func(ctx context.Context, _ string) (string, error) {
		calls.Add(1)
		<-ctx.Done()
		return "", ctx.Err()
	}
	start := time.Now()
	stats, _ := reapLinkedPRTasks(t, tasks, comments, func(r *StaleReaper) {
		r.SetPRStateLookup(hung)
		r.prLookupBudget = 50 * time.Millisecond
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("reap took %v; a hung GitHub must not stall the pass past its budget", elapsed)
	}
	if stats.Reopened != 0 || stats.FlaggedOrphanedWithPR != 4 {
		t.Errorf("reopened=%d flagged=%d, want 0/4 (an unknown state flags)", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("GitHub asked %d times, want 1 (the budget is spent by the first hung call)", n)
	}
}

func TestStaleReaper_PRLookupPass_CachedAnswerServedAfterBudgetSpent(t *testing.T) {
	r := &StaleReaper{now: time.Now, prState: prStates(map[string]string{testPR1: PRStateOpen}, nil)}
	if _, err := r.lookupPRState(context.Background(), testPR1, nil); err != nil {
		t.Fatal(err)
	}
	spent := &prLookupPass{}
	if s, err := r.lookupPRState(context.Background(), testPR1, spent); err != nil || s != PRStateOpen {
		t.Errorf("cached lookup with spent budget = %q, %v; want open", s, err)
	}
	if _, err := r.lookupPRState(context.Background(), testPR2, spent); !errors.Is(err, errPRLookupBudgetSpent) {
		t.Errorf("uncached lookup with spent budget: err = %v, want errPRLookupBudgetSpent", err)
	}
	if spent.starved != 1 {
		t.Errorf("starved = %d, want 1", spent.starved)
	}
}

func TestStaleReaper_LinkedPRURLs_OwnPRFirstAndOtherReposDropped(t *testing.T) {
	// The task's own mr_url leads, and PRs in other repositories mentioned on
	// the task are references, not its work.
	other := func(n int) string { return fmt.Sprintf("https://github.com/someone/elsewhere/pull/%d", n) }
	var mentions []string
	for n := range 6 {
		mentions = append(mentions, other(n+1))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"comments": []map[string]any{
			{"id": 1, "text": "prior art: " + strings.Join(mentions, " "), "created_at": time.Now().Format(time.RFC3339)},
			{"id": 2, "text": "split out " + testPR2, "created_at": time.Now().Format(time.RFC3339)},
		}})
	}))
	defer srv.Close()
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer daemon.Close()
	r := &StaleReaper{daemon: daemon, logger: staleReaperLogger()}
	task := &beadsapi.BeadDetail{ID: "gd-X", Fields: map[string]string{"mr_url": testPR1}, Description: "see " + other(9)}
	got := r.linkedPRURLs(context.Background(), task)
	if want := []string{testPR1, testPR2}; !slices.Equal(got, want) {
		t.Errorf("linkedPRURLs = %v, want %v", got, want)
	}
	// Without an mr_url every linked PR still counts.
	task.Fields = nil
	if got := r.linkedPRURLs(context.Background(), task); len(got) != 8 {
		t.Errorf("without mr_url linkedPRURLs = %v, want all 8 PRs", got)
	}
}

func TestStaleReaper_LinkedPRMentionsInOtherRepos_DoNotHitCap(t *testing.T) {
	// More than maxPRLookupsPerTask unrelated PRs are mentioned, but the
	// task's own PR is merged: it reopens instead of being flagged by the cap.
	task := claimedIssueTask("gd-ISSUEPR", "worker-agent", time.Now().Add(-1*time.Minute))
	task.Fields = mustJSON(map[string]string{"project": "gardenio", "mr_url": testPR1})
	var mentions []string
	for n := range maxPRLookupsPerTask + 1 {
		mentions = append(mentions, fmt.Sprintf("https://github.com/someone/elsewhere/pull/%d", n+1))
	}
	var asked []string
	stats, _ := reapLinkedPRTasks(t, []beadEntry{task}, map[string][]string{"gd-ISSUEPR": {"related: " + strings.Join(mentions, ", ")}}, func(r *StaleReaper) {
		r.SetPRStateLookup(prStates(map[string]string{testPR1: PRStateMerged}, &asked))
	})
	if stats.Reopened != 1 || stats.FlaggedOrphanedWithPR != 0 {
		t.Errorf("reopened=%d flagged=%d, want 1/0", stats.Reopened, stats.FlaggedOrphanedWithPR)
	}
	if !slices.Equal(asked, []string{testPR1}) {
		t.Errorf("asked %v, want only the task's own PR", asked)
	}
}

func TestStaleReaper_PRStateClosedCachedBriefly(t *testing.T) {
	// A closed PR can be reopened, so its answer expires well before an open
	// or merged one does.
	var asked []string
	r := &StaleReaper{now: time.Now, prState: prStates(map[string]string{testPR1: PRStateClosed, testPR2: PRStateMerged}, &asked)}
	_, _ = r.lookupPRState(context.Background(), testPR1, nil)
	_, _ = r.lookupPRState(context.Background(), testPR2, nil)
	_, _ = r.lookupPRState(context.Background(), testPR1, nil)
	if len(asked) != 2 {
		t.Fatalf("asked %d times, want 2 (closed reused within its TTL)", len(asked))
	}
	r.now = func() time.Time { return time.Now().Add(prStateClosedCacheTTL + time.Second) }
	_, _ = r.lookupPRState(context.Background(), testPR1, nil)
	_, _ = r.lookupPRState(context.Background(), testPR2, nil)
	if want := []string{testPR1, testPR2, testPR1}; !slices.Equal(asked, want) {
		t.Errorf("asked %v, want %v (closed re-read, merged still cached)", asked, want)
	}
}

// --- half-reap backstop tests (ka-0000000908): clearStrandedOpenAssignees ---

// openIssueBead builds an ordinary (non run-instance) issue task that is ALREADY
// status=open but still carries an assignee — the half-reap residue a gardener
// leaves when it reaps with `kd update --status open` but forgets `--assignee ""`.
func openIssueBead(id, assignee string, updatedAt time.Time) beadEntry {
	b := claimedIssueTask(id, assignee, updatedAt)
	b.Status = "open"
	return b
}

// halfReapHandler serves the daemon endpoints clearStrandedOpenAssignees hits.
// It is the status-aware sibling of reopenIssueHandler: a kind=issue GET is
// routed by its status= filter so the in_progress reopen list and the open
// half-reap list are served independently (both paths run in one Reap()).
//
//   - GET /v1/beads?type=agent...              → agentBeads (ListAgentBeads)
//   - GET /v1/beads?kind=issue...&status=open  → openIssues (the half-reap list)
//   - GET /v1/beads?kind=issue...              → inProgressIssues (reopen list)
//   - GET /v1/beads?type=task...               → empty (run-instance reconcile)
//   - GET /v1/agents-by-name/{name}            → agentByName[name] or 404
//   - PATCH /v1/beads/{id}                      → recorded in results
//   - POST /v1/beads/{id}/comments              → recorded in results
//
// openListRawQuery, when non-nil, captures the RawQuery of the open half-reap
// list request so a test can assert the recency window bound is sent (paging
// advice ka-0000003555: the window is the thing under test, not just the result).
func halfReapHandler(
	t *testing.T,
	agentBeads, openIssues, inProgressIssues []beadEntry,
	agentByName map[string]beadEntry,
	results map[string]*reopenResult,
	mu *sync.Mutex,
	openListRawQuery *string,
) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/agents-by-name/"):
			name := strings.TrimPrefix(r.URL.Path, "/v1/agents-by-name/")
			b, ok := agentByName[name]
			if ok {
				if want := r.URL.Query().Get("status"); want != "" && !strings.Contains(want, b.Status) {
					http.Error(w, "agent not found", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(b)
				return
			}
			http.Error(w, "agent not found", http.StatusNotFound)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			res := results[id]
			if res == nil {
				res = &reopenResult{}
				results[id] = res
			}
			res.patched = true
			if s, ok := body["status"].(string); ok {
				res.status = s
			}
			if a, ok := body["assignee"]; ok {
				res.hasAssigneeKey = true
				res.assignee, _ = a.(string)
			}
			if f, ok := body["fields"].(map[string]any); ok {
				res.reapedAssignee, _ = f[beadsapi.ReapedAssigneeField].(string)
			}
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			res := results[id]
			if res == nil {
				res = &reopenResult{}
				results[id] = res
			}
			res.comment = body["text"]
			mu.Unlock()
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/v1/beads" && r.Method == http.MethodGet:
			var beads []beadEntry
			switch {
			case strings.Contains(r.URL.RawQuery, "type=agent"):
				beads = agentBeads
			case strings.Contains(r.URL.RawQuery, "kind=issue") && strings.Contains(r.URL.RawQuery, "status=open"):
				if openListRawQuery != nil {
					mu.Lock()
					*openListRawQuery = r.URL.RawQuery
					mu.Unlock()
				}
				beads = openIssues
			case strings.Contains(r.URL.RawQuery, "kind=issue"):
				beads = inProgressIssues
			}
			resp := struct {
				Beads []beadEntry `json:"beads"`
			}{Beads: beads}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func TestStaleReaper_ClearsHalfReapAssignee_AgentGonePriorPass(t *testing.T) {
	// The canonical half-reap: a bead is status=open but still assigned to an
	// agent that has since terminated (its agent bead is CLOSED). The backstop
	// clears the dead assignee — leaving status untouched (already open) — so the
	// work re-enters the ready queue.
	stale := time.Now().Add(-30 * time.Minute) // past the 15m grace, inside the 6h window

	openIssues := []beadEntry{openIssueBead("gd-HALF1", "ghost-crew", stale)}
	agentByName := map[string]beadEntry{"ghost-crew": closedAgentBead("bd-ghost", "ghost-crew", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, openIssues, nil, agentByName, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 1 {
		t.Errorf("open_assignees_cleared: got %d, want 1", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	res := results["gd-HALF1"]
	if res == nil || !res.patched {
		t.Fatalf("gd-HALF1 assignee was not cleared; results=%v", results)
	}
	if !res.hasAssigneeKey || res.assignee != "" {
		t.Errorf("clear must set assignee=\"\"; hasKey=%v assignee=%q", res.hasAssigneeKey, res.assignee)
	}
	if res.reapedAssignee != "ghost-crew" {
		t.Errorf("clear must record the cleared assignee as %s, got %q", beadsapi.ReapedAssigneeField, res.reapedAssignee)
	}
	// The bead is ALREADY open — the clear must NOT send a status change.
	if res.status != "" {
		t.Errorf("clear must not touch status; got status=%q (want unset)", res.status)
	}
	if !strings.Contains(res.comment, "half-reap") || !strings.Contains(res.comment, "ready queue") {
		t.Errorf("audit comment missing expected content: %q", res.comment)
	}
}

func TestStaleReaper_ClearsHalfReapAssignee_ReapedThisPass(t *testing.T) {
	// A crew reaped this pass leaves an open bead it had been assigned. The clear
	// path fires immediately (no grace, no by-name lookup — reaped == gone).
	old := time.Now().Add(-5 * time.Hour)      // agent stale → reaped this pass
	recent := time.Now().Add(-1 * time.Minute) // bead touched recently, inside window

	agentBeads := []beadEntry{workingAgentBead("bd-worker", "worker-crew", old)}
	openIssues := []beadEntry{openIssueBead("gd-HALF2", "worker-crew", recent)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, agentBeads, openIssues, nil, nil, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.Reaped != 1 {
		t.Errorf("reaped: got %d, want 1", stats.Reaped)
	}
	if stats.OpenAssigneesCleared != 1 {
		t.Errorf("open_assignees_cleared: got %d, want 1", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-HALF2"]; res == nil || res.assignee != "" || res.status != "" {
		t.Errorf("gd-HALF2 not cleared correctly (assignee must clear, status untouched); res=%+v", res)
	}
}

func TestStaleReaper_SkipsHalfReap_LiveAssignee(t *testing.T) {
	// A bead legitimately open+assigned to a LIVE agent (mid-work) must be left
	// alone — clearing it would yank the claim out from under an active crew.
	recent := time.Now().Add(-1 * time.Minute)

	agentBeads := []beadEntry{workingAgentBead("bd-live", "live-crew", recent)}
	openIssues := []beadEntry{openIssueBead("gd-HALF3", "live-crew", time.Now().Add(-2*time.Hour))}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, agentBeads, openIssues, nil, nil, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (live assignee must be left alone)", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-HALF3"]; res != nil && res.patched {
		t.Errorf("gd-HALF3 (live assignee) was cleared but must be left alone; res=%+v", res)
	}
}

func TestStaleReaper_SkipsHalfReap_HumanAssignee(t *testing.T) {
	// The critical safety guard shared with the reopen path: an open bead assigned
	// to a HUMAN (name does not resolve to an agent bead) must NEVER be cleared.
	// This is what spares the thousands of legitimately open+assigned beads whose
	// assignee is a human or a long-dead thread that no longer resolves to an
	// agent bead.
	stale := time.Now().Add(-30 * time.Minute)

	openIssues := []beadEntry{openIssueBead("gd-HALF4", "Matthew.Baker", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, openIssues, nil, nil, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (human assignee must never be cleared)", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-HALF4"]; res != nil && res.patched {
		t.Errorf("gd-HALF4 (human-assigned) was cleared but must be left alone; res=%+v", res)
	}
}

func TestStaleReaper_SkipsHalfReap_TruncatedButLiveAgent(t *testing.T) {
	// Page-cap hazard: a genuinely LIVE agent truncated out of liveAgents must not
	// be treated as gone. The assignee resolves by-name to an ACTIVE agent bead →
	// still alive → leave the bead alone even past the grace.
	stale := time.Now().Add(-30 * time.Minute)

	openIssues := []beadEntry{openIssueBead("gd-HALF5", "busy-truncated", stale)}
	agentByName := map[string]beadEntry{"busy-truncated": workingAgentBead("bd-busy", "busy-truncated", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, openIssues, nil, agentByName, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (live-but-truncated agent must be left alone)", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-HALF5"]; res != nil && res.patched {
		t.Errorf("gd-HALF5 (live-but-truncated) was cleared but must be left alone; res=%+v", res)
	}
}

func TestStaleReaper_SkipsHalfReap_WithinGrace(t *testing.T) {
	// Assignee gone in a prior pass but the bead was touched very recently — inside
	// the 15m grace. Skip to avoid racing a freshly-claimed bead whose agent bead
	// has not surfaced yet.
	fresh := time.Now().Add(-2 * time.Minute)

	openIssues := []beadEntry{openIssueBead("gd-HALF6", "maybe-new", fresh)}
	agentByName := map[string]beadEntry{"maybe-new": closedAgentBead("bd-maybe", "maybe-new", fresh)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, openIssues, nil, agentByName, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (within grace must be skipped)", stats.OpenAssigneesCleared)
	}
}

func TestStaleReaper_SkipsHalfReap_Unassigned(t *testing.T) {
	// An already-unassigned open bead (correctly reaped or never claimed) is a
	// no-op — it is already in the ready queue.
	stale := time.Now().Add(-30 * time.Minute)
	openIssues := []beadEntry{openIssueBead("gd-HALF7", "", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, openIssues, nil, nil, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (unassigned open bead is a no-op)", stats.OpenAssigneesCleared)
	}
}

func TestStaleReaper_SkipsHalfReap_RunInstance(t *testing.T) {
	// A run-instance surfaces in the open kind=issue listing too, but the clear
	// path must SKIP it — run-instances are single-shot residue owned by the
	// run-instance reconcile paths, not retriable claimed work.
	stale := time.Now().Add(-30 * time.Minute)

	ri := runInstanceTask("gd-HALFRI", "ghost-crew", "source:schedule", stale)
	ri.Status = "open"
	agentByName := map[string]beadEntry{"ghost-crew": closedAgentBead("bd-ghost", "ghost-crew", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, []beadEntry{ri}, nil, agentByName, results, &mu, nil))
	defer cleanup()

	stats := reaper.Reap(context.Background())

	if stats.OpenAssigneesCleared != 0 {
		t.Errorf("open_assignees_cleared: got %d, want 0 (run-instance must be skipped)", stats.OpenAssigneesCleared)
	}
	mu.Lock()
	defer mu.Unlock()
	if res := results["gd-HALFRI"]; res != nil && res.patched {
		t.Errorf("gd-HALFRI (run-instance) was cleared but must be left to the run-instance paths")
	}
}

// TestStaleReaper_HalfReap_ScanIsRecencyBounded is the paging-advice guard
// (ka-0000003555): the open-status scan MUST send an updated_after bound so it
// never walks the full open backlog. Assert on the REQUEST, not just the result —
// a correct-looking result can still come from an unbounded fetch. It also
// asserts the limit stays a single page (never above the daemon's clamp).
func TestStaleReaper_HalfReap_ScanIsRecencyBounded(t *testing.T) {
	stale := time.Now().Add(-30 * time.Minute)
	openIssues := []beadEntry{openIssueBead("gd-HALF8", "ghost-crew", stale)}
	agentByName := map[string]beadEntry{"ghost-crew": closedAgentBead("bd-ghost", "ghost-crew", stale)}

	results := map[string]*reopenResult{}
	var mu sync.Mutex
	var openQuery string
	reaper, cleanup := newReopenReaper(t,
		halfReapHandler(t, nil, openIssues, nil, agentByName, results, &mu, &openQuery))
	defer cleanup()

	_ = reaper.Reap(context.Background())

	mu.Lock()
	defer mu.Unlock()
	if openQuery == "" {
		t.Fatal("open half-reap list was never requested")
	}
	if !strings.Contains(openQuery, "updated_after=") {
		t.Errorf("half-reap scan must send an updated_after recency bound (advice ka-0000003555); query=%q", openQuery)
	}
	if strings.Contains(openQuery, "limit=") {
		// A limit is fine, but it must be a single bounded page well under the
		// daemon's 10000 clamp — assert it is the 1000 the code sends.
		if !strings.Contains(openQuery, "limit=1000") {
			t.Errorf("half-reap scan limit must be a single bounded page (1000); query=%q", openQuery)
		}
	}
}

// TestReapReason_ExemptsMicrovmAgent verifies the C5 exemption (ka-0000001837):
// a lambda_microvm agent has no pod, so the pod-based stale reaper must never
// reap it — even in a "working" state with a long-stale updated_at that would
// reap an ordinary agent. Its lifecycle is owned by the reconciler's runtime
// reconcile + leak GC instead.
func TestReapReason_ExemptsMicrovmAgent(t *testing.T) {
	reaper := NewStaleReaper(nil, nil, "gardenio", 2*time.Hour, staleReaperLogger())
	now := time.Now()
	veryStale := now.Add(-24 * time.Hour) // far past every reap threshold

	microvm := beadsapi.AgentBead{
		ID:         "gd-mvm",
		AgentName:  "gd-mvm",
		Project:    "gardenio",
		AgentState: "working",
		UpdatedAt:  veryStale,
		Metadata: map[string]string{
			"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
			"runtime_id":       "mvm-1",
		},
	}
	if reason := reaper.reapReason(context.Background(), microvm, now, true); reason != "" {
		t.Errorf("MicroVM agent must be exempt from stale reaping, got reason %q", reason)
	}

	// Control: an identical NON-MicroVM agent in a terminal state this stale IS
	// reaped (the "done" branch fires purely on updated_at age, no daemon call),
	// so the exemption — not some other guard — is what spares the MicroVM agent.
	k8s := microvm
	k8s.ID = "gd-pod"
	k8s.AgentState = "done"
	k8s.Metadata = map[string]string{} // no execution_target
	if reason := reaper.reapReason(context.Background(), k8s, now, true); reason == "" {
		t.Error("a non-MicroVM terminal agent this stale should be reaped (control)")
	}
}

// TestReapReason_ExemptsLocalAgent: a laptop agent (execution_target=laptop,
// gd-SzPJDNTBXS) has no pod and is held, not reaped, while its laptop is
// away (local-agents D5), however stale its bead.
func TestReapReason_ExemptsLocalAgent(t *testing.T) {
	reaper := NewStaleReaper(nil, nil, "gardenio", 2*time.Hour, staleReaperLogger())
	now := time.Now()
	for _, state := range []string{"working", "done"} {
		local := beadsapi.AgentBead{
			ID:         "gd-local",
			AgentName:  "local-laptop",
			Project:    "gardenio",
			AgentState: state,
			UpdatedAt:  now.Add(-24 * time.Hour),
			Metadata: map[string]string{
				"execution_target": podmanager.ExecutionTargetLaptop,
				"runtime_id":       "gd-r1",
			},
		}
		if reason := reaper.reapReason(context.Background(), local, now, true); reason != "" {
			t.Errorf("local agent (%s) must be exempt from stale reaping, got reason %q", state, reason)
		}
	}
}

// TestReapReason_StoppingFrozenAwaitingThaw pins the reaper's handling of the
// frozen-awaiting-thaw resting state (ka-0000002104): a suspended agent's bead
// rests OPEN at agent_state=stopping with a persisted snapshot, and the reaper
// must never close it — before this fix "stopping" fell into the default
// unknown-state branch and was reaped as failed after the stale timeout,
// destroying the thaw-target signal the wake paths (and the resume-latency
// histogram's sole call site) key on.
func TestReapReason_StoppingFrozenAwaitingThaw(t *testing.T) {
	reaper := NewStaleReaper(nil, nil, "gardenio", 2*time.Hour, staleReaperLogger())
	now := time.Now()
	veryStale := now.Add(-24 * time.Hour) // far past every reap threshold

	frozen := beadsapi.AgentBead{
		ID:         "gd-frozen",
		AgentName:  "gardenio-plan-gardener-x",
		Project:    "gardenio",
		AgentState: "stopping",
		UpdatedAt:  veryStale,
	}

	// Snapshot present: parked indefinitely — never reaped, at any age.
	reaper.SetSnapshotChecker(func(_ context.Context, agentName string) bool {
		if agentName != frozen.AgentName {
			t.Errorf("snapshot checker keyed on %q, want %q", agentName, frozen.AgentName)
		}
		return true
	})
	if reason := reaper.reapReason(context.Background(), frozen, now, true); reason != "" {
		t.Errorf("frozen-awaiting-thaw agent (snapshot present) must not be reaped, got %q", reason)
	}

	// Snapshot gone: the bead is a genuine leak — reaped on the short terminal
	// clock with the dedicated benign reason (closes as done, not failed).
	reaper.SetSnapshotChecker(func(context.Context, string) bool { return false })
	reason := reaper.reapReason(context.Background(), frozen, now, true)
	if !strings.HasPrefix(reason, "orphan: frozen thaw target without snapshot") {
		t.Errorf("snapshotless stopping bead should reap as frozen-thaw-target orphan, got %q", reason)
	}
	if got := reapCloseState(reason, "stopping"); got != "done" {
		t.Errorf("snapshotless stopping reap must close as done, got %q", got)
	}

	// Snapshot gone but within the terminal grace: left alone (no premature reap
	// racing a thaw that just consumed the snapshot).
	fresh := frozen
	fresh.UpdatedAt = now.Add(-5 * time.Minute)
	if reason := reaper.reapReason(context.Background(), fresh, now, true); reason != "" {
		t.Errorf("recently-updated snapshotless stopping bead must ride out the grace, got %q", reason)
	}

	// No checker wired (freeze/thaw disabled): "stopping" is a zombie state with
	// no snapshot machinery — reaped on the same terminal clock.
	reaper.SetSnapshotChecker(nil)
	if reason := reaper.reapReason(context.Background(), frozen, now, true); !strings.HasPrefix(reason, "orphan: frozen thaw target without snapshot") {
		t.Errorf("stopping bead with freeze/thaw disabled should reap as frozen-thaw-target orphan, got %q", reason)
	}
}

// TestReapReason_ParkedPersistentIdentity pins the reaper's exemption for a
// parked persistent identity awaiting its wake (persistent-agents Phase 2,
// task ka-0000004902): a bead carrying successor=same + a FUTURE next_respawn_at
// must never be reaped, even podless and far past every stale threshold, so the
// wake intent survives until the wake-scanner reopens it. Once the wake time is
// past (scanner owns it) or the wake field is cleared (already woken), the bead
// falls through to the normal rules.
func TestReapReason_ParkedPersistentIdentity(t *testing.T) {
	reaper := NewStaleReaper(nil, nil, "gardenio", 2*time.Hour, staleReaperLogger())
	now := time.Now()
	veryStale := now.Add(-24 * time.Hour) // far past every reap threshold

	base := beadsapi.AgentBead{
		ID:         "gd-parked",
		AgentName:  "gardenio-crew-persist",
		Project:    "gardenio",
		AgentState: "", // reopened-but-podless resting shape
		UpdatedAt:  veryStale,
	}

	// Future wake stamped: parked, exempt at any age, even with no pod.
	parked := base
	parked.Metadata = map[string]string{
		beadsapi.FieldSuccessor:     beadsapi.SuccessorSame,
		beadsapi.FieldNextRespawnAt: now.Add(30 * time.Minute).UTC().Format(time.RFC3339),
	}
	if reason := reaper.reapReason(context.Background(), parked, now, false); reason != "" {
		t.Errorf("parked identity with a future wake must not be reaped, got %q", reason)
	}

	// Wake time already past: the scanner owns it now, not the reaper — the
	// exemption no longer applies and the normal empty-state/no-pod rule governs.
	due := base
	due.Metadata = map[string]string{
		beadsapi.FieldSuccessor:     beadsapi.SuccessorSame,
		beadsapi.FieldNextRespawnAt: now.Add(-30 * time.Minute).UTC().Format(time.RFC3339),
	}
	if reason := reaper.reapReason(context.Background(), due, now, false); reason == "" {
		t.Error("a due (past-wake) empty-state podless bead should fall through to the normal rules (control)")
	}

	// No successor marker: an ordinary closed/empty bead is never protected.
	plain := base
	plain.Metadata = map[string]string{
		beadsapi.FieldNextRespawnAt: now.Add(30 * time.Minute).UTC().Format(time.RFC3339),
	}
	if reason := reaper.reapReason(context.Background(), plain, now, false); reason == "" {
		t.Error("a bead without successor=same must not be exempted (control)")
	}
}
