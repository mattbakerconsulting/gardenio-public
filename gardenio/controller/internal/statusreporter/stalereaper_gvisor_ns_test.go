package statusreporter

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// A stale runsc (gVisor) agent's pod lives in the PSA-privileged gVisor
// namespace (ka-0000008076). The reaper must delete it there — not in the
// primary namespace — mirroring routeGVisorNamespace. A non-runsc agent in the
// same pass is still deleted from the primary namespace.

func TestStaleReaper_RoutesRunscDeleteToGVisorNamespace(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID:     "bd-gvisor-stale",
			Title:  "gvisor-stale",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":       "gardenio",
				"mode":          "crew",
				"role":          "crew",
				"agent":         "gvisor-stale",
				"agent_state":   "working",
				"runtime_class": "runsc",
			}),
			CreatedAt: stale,
			UpdatedAt: stale,
		},
		{
			ID:     "bd-normal-stale",
			Title:  "normal-stale",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":     "gardenio",
				"mode":        "crew",
				"role":        "crew",
				"agent":       "normal-stale",
				"agent_state": "working",
			}),
			CreatedAt: stale,
			UpdatedAt: stale,
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
	reaper.SetGVisorNamespace("gardenio-gvisor")

	stats := reaper.Reap(context.Background())

	if stats.Reaped != 2 {
		t.Fatalf("reaped: got %d, want 2", stats.Reaped)
	}
	if got := pods.deletedNS["crew-gardenio-crew-gvisor-stale"]; got != "gardenio-gvisor" {
		t.Errorf("runsc agent deleted from namespace %q, want gardenio-gvisor", got)
	}
	if got := pods.deletedNS["crew-gardenio-crew-normal-stale"]; got != "gardenio" {
		t.Errorf("non-runsc agent deleted from namespace %q, want gardenio", got)
	}
}

// Without SetGVisorNamespace a runsc agent's pod is still deleted from the
// primary namespace — guards that routing is opt-in.
func TestStaleReaper_GVisorNamespaceOptIn(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID:     "bd-gvisor-stale",
			Title:  "gvisor-stale",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":       "gardenio",
				"mode":          "crew",
				"role":          "crew",
				"agent":         "gvisor-stale",
				"agent_state":   "working",
				"runtime_class": "runsc",
			}),
			CreatedAt: stale,
			UpdatedAt: stale,
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
	// No SetGVisorNamespace.

	reaper.Reap(context.Background())

	if got := pods.deletedNS["crew-gardenio-crew-gvisor-stale"]; got != "gardenio" {
		t.Errorf("without routing, deleted from namespace %q, want primary gardenio", got)
	}
}

// An explicitly pinned namespace on the bead always wins over runsc routing.
func TestStaleReaper_PinnedNamespaceWins(t *testing.T) {
	stale := time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)

	handler := beadFixture([]beadEntry{
		{
			ID:     "bd-pinned",
			Title:  "pinned-agent",
			Type:   "agent",
			Kind:   "config",
			Status: "open",
			Fields: mustJSON(map[string]string{
				"project":       "gardenio",
				"mode":          "crew",
				"role":          "crew",
				"agent":         "pinned-agent",
				"agent_state":   "working",
				"runtime_class": "runsc",
				"namespace":     "custom-ns",
			}),
			CreatedAt: stale,
			UpdatedAt: stale,
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
	reaper.SetGVisorNamespace("gardenio-gvisor")

	reaper.Reap(context.Background())

	if got := pods.deletedNS["crew-gardenio-crew-pinned-agent"]; got != "custom-ns" {
		t.Errorf("pinned namespace ignored: deleted from %q, want custom-ns", got)
	}
}

// SetGVisorNamespace ignores empty and primary-namespace values (no-op).
func TestStaleReaper_SetGVisorNamespaceIgnoresEmptyAndPrimary(t *testing.T) {
	pods := &mockPodManager{}
	reaper := NewStaleReaper(nil, pods, "gardenio", 2*time.Hour, staleReaperLogger())
	reaper.SetGVisorNamespace("")
	if reaper.gvisorNamespace != "" {
		t.Errorf("empty value should be ignored, got %q", reaper.gvisorNamespace)
	}
	reaper.SetGVisorNamespace("gardenio")
	if reaper.gvisorNamespace != "" {
		t.Errorf("primary-namespace value should be ignored, got %q", reaper.gvisorNamespace)
	}
}
