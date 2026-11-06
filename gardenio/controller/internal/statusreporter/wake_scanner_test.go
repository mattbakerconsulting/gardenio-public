package statusreporter

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// wakeFakeDaemon is a stateful HTTP fake for the beads daemon that serves the
// closed-agent list and records the PATCH (reopen / clear) and comment writes
// the wake-scanner issues, so tests can assert on exactly what it wrote.
type wakeFakeDaemon struct {
	mu sync.Mutex

	// beads served by GET /v1/beads (the closed-agent scan). Keyed by id.
	beads []beadEntry

	// patches records every PATCH body, keyed by bead id.
	patches map[string]map[string]any
	// comments records comment text posted, keyed by bead id.
	comments map[string]string

	// patchErrIDs makes a PATCH to any listed bead id fail (503) so tests can
	// exercise the retry-next-pass path.
	patchErrIDs map[string]bool
	// listQueries records the query of every GET /v1/beads.
	listQueries []url.Values
}

func newWakeFakeDaemon(beads []beadEntry) *wakeFakeDaemon {
	return &wakeFakeDaemon{
		beads:       beads,
		patches:     map[string]map[string]any{},
		comments:    map[string]string{},
		patchErrIDs: map[string]bool{},
	}
}

func (f *wakeFakeDaemon) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			// Honor the status filter the scanner sends (status=closed) so a test
			// that seeds a mix of statuses exercises the same server-side scope.
			f.listQueries = append(f.listQueries, r.URL.Query())
			wantStatus := r.URL.Query().Get("status")
			var out []beadEntry
			for _, b := range f.beads {
				if wantStatus != "" && !strings.Contains(wantStatus, b.Status) {
					continue
				}
				out = append(out, b)
			}
			_ = json.NewEncoder(w).Encode(struct {
				Beads []beadEntry `json:"beads"`
				Total int         `json:"total"`
			}{Beads: out, Total: len(out)})

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			if f.patchErrIDs[id] {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]any
			_ = json.Unmarshal(body, &parsed)
			f.patches[id] = parsed
			w.WriteHeader(http.StatusOK)

		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/beads/"), "/comments")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]string
			_ = json.Unmarshal(body, &parsed)
			f.comments[id] = parsed["text"]
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

// newWakeScannerForTest wires a scanner against the fake daemon with a fixed now.
func newWakeScannerForTest(t *testing.T, f *wakeFakeDaemon, now time.Time) (*WakeScanner, func()) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	daemon, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		srv.Close()
		t.Fatal(err)
	}
	s := NewWakeScanner(daemon, staleReaperLogger())
	s.now = func() time.Time { return now }
	return s, func() { daemon.Close(); srv.Close() }
}

func parkedBead(id, agent, nextRespawnAt string, extra map[string]string) beadEntry {
	fields := map[string]string{
		"project":     "gardenio",
		"mode":        "crew",
		"role":        "crew",
		"agent":       agent,
		"agent_state": "done",
		"successor":   "same",
	}
	if nextRespawnAt != "" {
		fields["next_respawn_at"] = nextRespawnAt
	}
	for k, v := range extra {
		fields[k] = v
	}
	return beadEntry{
		ID: id, Title: agent, Type: "agent", Kind: "config", Status: "closed",
		Fields: mustJSON(fields),
	}
}

func TestWakeScanner_RevivesDueParkedBead(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-5 * time.Minute).Format(time.RFC3339)

	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-park-due", "persist-agent", past, nil),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Woken != 1 {
		t.Fatalf("woken: got %d, want 1", stats.Woken)
	}
	if stats.BudgetExhausted != 0 || stats.Errors != 0 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	patch, ok := f.patches["bd-park-due"]
	if !ok {
		t.Fatal("expected a PATCH to the parked bead")
	}
	if patch["status"] != "open" {
		t.Errorf("status: got %v, want open", patch["status"])
	}
	fields, _ := patch["fields"].(map[string]any)
	if fields == nil {
		t.Fatal("expected a fields patch")
	}
	// next_respawn_at + agent_state cleared (JSON null), respawn_count bumped to 1.
	if v, present := fields["next_respawn_at"]; !present || v != nil {
		t.Errorf("next_respawn_at: got %v (present=%v), want explicit null", v, present)
	}
	if v, present := fields["agent_state"]; !present || v != nil {
		t.Errorf("agent_state: got %v (present=%v), want explicit null", v, present)
	}
	if fields["respawn_count"] != "1" {
		t.Errorf("respawn_count: got %v, want 1", fields["respawn_count"])
	}
	// wake_reason=timer is stamped so the controller surfaces BOAT_WAKE_REASON to
	// the revived pod (ka-0000000027).
	if fields[beadsapi.FieldWakeReason] != beadsapi.WakeReasonTimer {
		t.Errorf("wake_reason: got %v, want %q", fields[beadsapi.FieldWakeReason], beadsapi.WakeReasonTimer)
	}
	if f.comments["bd-park-due"] == "" {
		t.Error("expected an audit comment on the revived bead")
	}
}

// TestWakeScanner_ListAsksOnlyForStampedBeads pins the scan's server-side
// filter: closed agents with successor=same AND a stamped next_respawn_at,
// with no total. Without the presence term every closed agent that stopped
// with successor=same is paged through (and counted) on each pass; it was
// the beads database's costliest statement (gd-lCYkpAlNas, gd-4uUkp4DQFD).
func TestWakeScanner_ListAsksOnlyForStampedBeads(t *testing.T) {
	f := newWakeFakeDaemon(nil)
	s, cleanup := newWakeScannerForTest(t, f, time.Now().UTC())
	defer cleanup()

	s.Scan(context.Background())

	if len(f.listQueries) != 1 {
		t.Fatalf("list calls: got %d, want 1", len(f.listQueries))
	}
	q := f.listQueries[0]
	if q.Get("status") != "closed" || q.Get("type") != "agent" {
		t.Errorf("status/type: got %q/%q, want closed/agent", q.Get("status"), q.Get("type"))
	}
	for _, want := range []string{"successor=same", "next_respawn_at=*"} {
		if !slices.Contains(q["field"], want) {
			t.Errorf("field filters %v lack %q", q["field"], want)
		}
	}
	if q.Get("no_total") != "true" {
		t.Errorf("no_total: got %q, want true", q.Get("no_total"))
	}
}

func TestWakeScanner_SkipsFutureWake(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(20 * time.Minute).Format(time.RFC3339)

	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-park-future", "not-yet", future, nil),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Scanned != 0 || stats.Woken != 0 {
		t.Fatalf("expected no due candidates, got %+v", stats)
	}
	if len(f.patches) != 0 {
		t.Errorf("expected no PATCH for a future wake, got %v", f.patches)
	}
}

func TestWakeScanner_SkipsMissingWakeTimestamp(t *testing.T) {
	now := time.Now().UTC()
	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-park-nostamp", "no-stamp", "", nil), // successor=same but no next_respawn_at
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Scanned != 0 || stats.Woken != 0 {
		t.Fatalf("expected no due candidates for a stampless bead, got %+v", stats)
	}
	if len(f.patches) != 0 {
		t.Errorf("expected no PATCH, got %v", f.patches)
	}
}

// TestWakeScanner_IgnoresCIWaitPark asserts the timer wake-scanner leaves a CI
// park alone (plan wake-on-ci-completion.md §2.1, task gd-AwuRDezONo): `gb stop`
// with a pending `gb wait ci` closes the bead successor=same but stamps no
// next_respawn_at, so only the CI resolver wakes it.
func TestWakeScanner_IgnoresCIWaitPark(t *testing.T) {
	now := time.Now().UTC()
	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-ci-park", "ci-waiter", "", map[string]string{
			"close_reason":            "ci_wait_parked",
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:   "joyousaversion/gardenio@0123456789abcdef0123456789abcdef01234567",
			beadsapi.FieldCIWaitTask:  "gd-WORK01",
		}),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Scanned != 0 || stats.Woken != 0 {
		t.Fatalf("a CI park must not be a timer-wake candidate, got %+v", stats)
	}
	if len(f.patches) != 0 {
		t.Errorf("expected no PATCH, got %v", f.patches)
	}
}

// TestWakeScanner_RevivedCIParkClearsStopRequested is the gd-NGlLifFXbO
// regression: `gb stop` sets stop_requested=true before it parks on a pending
// `gb wait ci`, and gb init's shouldExitBeforeStart re-closes a pod whose bead
// still reads "true". The timer revive must clear it in the same PATCH that
// reopens the bead, or the woken crew closes itself at boot without running a
// turn and, its next_respawn_at already spent, is never woken again.
func TestWakeScanner_RevivedCIParkClearsStopRequested(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339)
	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-ci-park", "ci-waiter", past, map[string]string{
			"close_reason":            "ci_wait_parked",
			"stop_requested":          "true",
			beadsapi.FieldCIWaitState: beadsapi.CIWaitStatePending,
			beadsapi.FieldCIWaitKey:   "mattbakerconsulting/gardenio@0123456789abcdef0123456789abcdef01234567",
		}),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	if stats := s.Scan(context.Background()); stats.Woken != 1 {
		t.Fatalf("woken: got %+v, want 1", stats)
	}
	fields, _ := f.patches["bd-ci-park"]["fields"].(map[string]any)
	if v, present := fields[beadsapi.FieldStopRequested]; !present || v != "" {
		t.Errorf("stop_requested: got %v (present=%v), want cleared to \"\" in the revive PATCH", v, present)
	}
}

func TestWakeScanner_BudgetExhaustedClearsWithoutRevive(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339)

	f := newWakeFakeDaemon([]beadEntry{
		// respawn_count == max_respawns → budget spent.
		parkedBead("bd-park-spent", "looper", past, map[string]string{
			"respawn_count": "5", "max_respawns": "5",
		}),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.BudgetExhausted != 1 || stats.Woken != 0 {
		t.Fatalf("expected budget-exhausted clear, got %+v", stats)
	}
	patch := f.patches["bd-park-spent"]
	if patch == nil {
		t.Fatal("expected a clear PATCH")
	}
	// A budget clear must NOT reopen the bead.
	if _, reopened := patch["status"]; reopened {
		t.Errorf("budget clear must not reopen the bead, got status=%v", patch["status"])
	}
	fields, _ := patch["fields"].(map[string]any)
	if v, present := fields["next_respawn_at"]; !present || v != nil {
		t.Errorf("next_respawn_at: want explicit null, got %v (present=%v)", v, present)
	}
	if v, present := fields["successor"]; !present || v != nil {
		t.Errorf("successor: want explicit null (so it never re-selects), got %v (present=%v)", v, present)
	}
	if f.comments["bd-park-spent"] != "" {
		t.Error("a budget clear should not post a revive audit comment")
	}
}

func TestWakeScanner_DefaultBudgetAllowsRevive(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339)

	f := newWakeFakeDaemon([]beadEntry{
		// respawn_count 4, no max_respawns override → default cap 5, still allowed.
		parkedBead("bd-park-4", "almost", past, map[string]string{"respawn_count": "4"}),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Woken != 1 {
		t.Fatalf("woken: got %d, want 1 (4 < default cap 5)", stats.Woken)
	}
	if f.patches["bd-park-4"]["status"] != "open" {
		t.Error("expected reopen")
	}
	fields, _ := f.patches["bd-park-4"]["fields"].(map[string]any)
	if fields["respawn_count"] != "5" {
		t.Errorf("respawn_count: got %v, want 5", fields["respawn_count"])
	}
}

func TestWakeScanner_MalformedWakeClearsWithoutRevive(t *testing.T) {
	now := time.Now().UTC()
	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-park-bad", "garbled", "not-a-timestamp", nil),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Woken != 0 {
		t.Fatalf("a malformed stamp must not revive, got %+v", stats)
	}
	if stats.BudgetExhausted != 1 {
		t.Fatalf("expected the malformed stamp to be cleared, got %+v", stats)
	}
	patch := f.patches["bd-park-bad"]
	if _, reopened := patch["status"]; reopened {
		t.Error("malformed-stamp clear must not reopen the bead")
	}
}

func TestWakeScanner_GoalUnachievableClearsWithoutRevive(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339)

	// A bead that (defensively) still carries successor=same + a due wake, but
	// also declares its goal deterministically unachievable. The goal-unachievable
	// close should have cleared the wake already; this is the belt-and-suspenders
	// guard for a stray successor=same that slipped through (P3, ka-0000005843).
	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-park-unachievable", "doomed", past, map[string]string{
			"goal_unachievable": "true",
		}),
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Woken != 0 {
		t.Fatalf("a goal_unachievable bead must never be revived, got %+v", stats)
	}
	if stats.BudgetExhausted != 1 {
		t.Fatalf("expected the wake to be cleared without revive, got %+v", stats)
	}
	patch := f.patches["bd-park-unachievable"]
	if patch == nil {
		t.Fatal("expected a clear PATCH")
	}
	if _, reopened := patch["status"]; reopened {
		t.Errorf("goal_unachievable clear must not reopen the bead, got status=%v", patch["status"])
	}
	fields, _ := patch["fields"].(map[string]any)
	if v, present := fields["successor"]; !present || v != nil {
		t.Errorf("successor: want explicit null (so it never re-selects), got %v (present=%v)", v, present)
	}
	if f.comments["bd-park-unachievable"] != "" {
		t.Error("a goal_unachievable clear should not post a revive audit comment")
	}
}

func TestWakeScanner_ReopenFailureRetries(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339)

	f := newWakeFakeDaemon([]beadEntry{
		parkedBead("bd-park-flaky", "flaky", past, nil),
	})
	f.patchErrIDs["bd-park-flaky"] = true
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Errors != 1 || stats.Woken != 0 {
		t.Fatalf("expected a retryable error, got %+v", stats)
	}
	// The wake field was never cleared (the PATCH failed), so a later pass retries.
	if f.comments["bd-park-flaky"] != "" {
		t.Error("no audit comment should be posted when the reopen failed")
	}
}

func TestWakeScanner_IgnoresNonParkedClosedBeads(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Minute).Format(time.RFC3339)

	f := newWakeFakeDaemon([]beadEntry{
		// A terminal (non-persistent) closed agent: successor absent → never woken.
		{
			ID: "bd-terminal", Title: "term", Type: "agent", Kind: "config", Status: "closed",
			Fields: mustJSON(map[string]string{
				"project": "gardenio", "mode": "crew", "role": "crew",
				"agent": "term", "agent_state": "done", "next_respawn_at": past,
			}),
		},
	})
	s, cleanup := newWakeScannerForTest(t, f, now)
	defer cleanup()

	stats := s.Scan(context.Background())

	if stats.Scanned != 0 || stats.Woken != 0 {
		t.Fatalf("a closed bead without successor=same must be ignored, got %+v", stats)
	}
	if len(f.patches) != 0 {
		t.Errorf("expected no writes to a non-parked bead, got %v", f.patches)
	}
}

func TestWakeScanner_NilDaemonReturnsNil(t *testing.T) {
	if NewWakeScanner(nil, staleReaperLogger()) != nil {
		t.Fatal("expected nil scanner when daemon is nil")
	}
}
