package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// vmagentCapture spins up a fake vmagent import endpoint and returns the
// server plus a getter for the concatenated POSTed bodies.
func vmagentCapture(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var body strings.Builder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/import/prometheus" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body.Write(b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return body.String()
	}
}

func TestPushSalvageMetrics_Success(t *testing.T) {
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	pushSalvageMetrics(context.Background(), salvageRunStats{
		outcome:  "success",
		targets:  2,
		uploaded: 2,
		bytes:    12345,
		duration: 1500 * time.Millisecond,
	})

	body := got()
	for _, want := range []string{
		`gardenio_salvage_runs_total{outcome="success",agent="gd-agent1"} 1`,
		`gardenio_salvage_targets_total{result="uploaded",agent="gd-agent1"} 2`,
		`gardenio_salvage_bytes_total{agent="gd-agent1"} 12345`,
		`gardenio_salvage_duration_seconds{agent="gd-agent1"} 1.500`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("push body missing %q\ngot:\n%s", want, body)
		}
	}
	// A fully-successful run has no skipped targets — the series must be absent
	// rather than emitted at zero.
	if strings.Contains(body, `result="skipped"`) {
		t.Errorf("no skipped series expected on full success, got:\n%s", body)
	}
}

func TestPushSalvageMetrics_PartialAndFailed(t *testing.T) {
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	pushSalvageMetrics(context.Background(), salvageRunStats{
		outcome: "partial", targets: 3, uploaded: 1, bytes: 10, duration: time.Second,
	})
	pushSalvageMetrics(context.Background(), salvageRunStats{
		outcome: "failed", targets: 2, uploaded: 0, duration: time.Second,
	})

	body := got()
	for _, want := range []string{
		`gardenio_salvage_runs_total{outcome="partial",agent="gd-agent1"} 1`,
		`gardenio_salvage_targets_total{result="uploaded",agent="gd-agent1"} 1`,
		`gardenio_salvage_targets_total{result="skipped",agent="gd-agent1"} 2`,
		`gardenio_salvage_runs_total{outcome="failed",agent="gd-agent1"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("push body missing %q\ngot:\n%s", want, body)
		}
	}
	// The failed run uploaded nothing: no uploaded/bytes series for it beyond
	// the partial run's, and its skipped count covers both targets.
	if !strings.Contains(body, `result="skipped",agent="gd-agent1"} 2`) {
		t.Errorf("failed run should report 2 skipped targets, got:\n%s", body)
	}
}

func TestPushSalvageMetrics_NoVMAgentIsNoOp(t *testing.T) {
	t.Setenv("VMAGENT_URL", "")
	// Must not panic or hang without a vmagent configured.
	pushSalvageMetrics(context.Background(), salvageRunStats{outcome: "success", targets: 1, uploaded: 1})
}

// TestPushSalvageMetrics_ExpiredContextStillPushes: salvage often reaches the
// metrics push right at its 25s deadline; the push must detach from the dead
// context (pushVMAgentLines uses WithoutCancel + a fresh timeout).
func TestPushSalvageMetrics_ExpiredContextStillPushes(t *testing.T) {
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	t.Setenv("KD_AGENT_ID", "gd-agent1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already expired, like a salvage that hit its ceiling

	pushSalvageMetrics(ctx, salvageRunStats{outcome: "success", targets: 1, uploaded: 1, bytes: 5})
	if !strings.Contains(got(), `gardenio_salvage_runs_total{outcome="success",agent="gd-agent1"} 1`) {
		t.Errorf("push with expired context did not land, got:\n%s", got())
	}
}

// TestRunSalvage_PushesMetrics: end-to-end through runSalvage — a dirty
// worktree bundled to the in-memory S3 also lands a success run + uploaded
// target + bytes on the (fake) vmagent.
func TestRunSalvage_PushesMetrics(t *testing.T) {
	root := t.TempDir()
	work := newOriginWithClone(t, root)
	dirtyUp(t, work)

	salvageTestEnv(t)
	srv, got := vmagentCapture(t)
	t.Setenv("VMAGENT_URL", srv.URL)
	newMemS3(t)

	if err := runSalvage(context.Background(), work, "unit_test", salvageOpts{}); err != nil {
		t.Fatalf("runSalvage: %v", err)
	}

	body := got()
	for _, want := range []string{
		`gardenio_salvage_runs_total{outcome="success",agent="gd-agent1"} 1`,
		`gardenio_salvage_targets_total{result="uploaded",agent="gd-agent1"} 1`,
		`gardenio_salvage_bytes_total{agent="gd-agent1"}`,
		`gardenio_salvage_duration_seconds{agent="gd-agent1"}`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("salvage metrics missing %q\ngot:\n%s", want, body)
		}
	}
}
