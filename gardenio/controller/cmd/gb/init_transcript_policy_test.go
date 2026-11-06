package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// -- helpers -----------------------------------------------------------------

// writeTranscript lays down projects/<hash>/<uuid>.jsonl (+ companion dir and
// session-env entry) under a fake claude state dir and returns the .jsonl path.
func writeTranscript(t *testing.T, stateDir, uuid string, age time.Duration) string {
	t.Helper()
	proj := filepath.Join(stateDir, "projects", "-work")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(proj, uuid+".jsonl")
	if err := os.WriteFile(p, bigJsonl(), 0o644); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	if err := os.Chtimes(p, mt, mt); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(proj, uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "session-env", uuid), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// listJSONLs returns the *.jsonl files coop's tier-2 discovery could latch onto.
func listJSONLs(t *testing.T, stateDir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(stateDir, "projects", "*", "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// -- retireAllSessionLogs ------------------------------------------------------

func TestRetireAllSessionLogs_KeepsBytesHidesFromDiscovery(t *testing.T) {
	dir := t.TempDir()
	live := writeTranscript(t, dir, "abc-123", 0)

	if n := retireAllSessionLogs(dir); n != 1 {
		t.Fatalf("retired %d, want 1", n)
	}
	// gd-2g0LXWF4Sz: nothing named *.jsonl may remain for coop tier-2 discovery.
	if got := listJSONLs(t, dir); len(got) != 0 {
		t.Fatalf("live .jsonl left for coop discovery to latch: %v", got)
	}
	// The record itself survives.
	if !exists(live + ".stale") {
		t.Errorf("transcript bytes were not kept at %s.stale", live)
	}
	if !exists(filepath.Join(dir, "projects", "-work", "abc-123.stale")) {
		t.Errorf("companion dir not retired alongside the transcript")
	}
	if !exists(filepath.Join(dir, "session-env", "abc-123.stale")) {
		t.Errorf("session-env not retired alongside the transcript")
	}
}

func TestRetireAllSessionLogs_ReRetireSameUUIDIsClean(t *testing.T) {
	// coop re-places the restored transcript at the SAME path every boot, so the
	// same UUID is retired on consecutive restarts. The second pass must replace
	// the first retiree (and its companion) rather than fail on the rename.
	dir := t.TempDir()
	for i := 0; i < 2; i++ {
		live := writeTranscript(t, dir, "abc-123", 0)
		if n := retireAllSessionLogs(dir); n != 1 {
			t.Fatalf("pass %d: retired %d, want 1", i, n)
		}
		if exists(live) || !exists(live+".stale") {
			t.Fatalf("pass %d: wrong post-state (live=%v stale=%v)", i, exists(live), exists(live+".stale"))
		}
	}
}

func TestRetireAllSessionLogs_BoundsRetainedCount(t *testing.T) {
	dir := t.TempDir()
	for i, u := range []string{"a", "b", "c", "d", "e"} {
		p := writeTranscript(t, dir, u, 0)
		retireAllSessionLogs(dir)
		// Age the retiree so ordering by mtime is deterministic: a oldest … e newest.
		mt := time.Now().Add(time.Duration(i-10) * time.Minute)
		_ = os.Chtimes(p+".stale", mt, mt)
	}
	retireAllSessionLogs(dir) // prune pass with nothing live
	var kept []string
	m, _ := filepath.Glob(filepath.Join(dir, "projects", "*", "*.jsonl.stale"))
	for _, f := range m {
		kept = append(kept, filepath.Base(f))
	}
	if len(kept) != maxRetainedSessionLogs {
		t.Fatalf("kept %v, want %d newest", kept, maxRetainedSessionLogs)
	}
	for _, old := range []string{"a.jsonl.stale", "b.jsonl.stale"} {
		for _, k := range kept {
			if k == old {
				t.Errorf("oldest retiree %s should have been pruned; kept=%v", old, kept)
			}
		}
	}
}

func TestRetireAllSessionLogs_NoProjectsDirIsNoOp(t *testing.T) {
	dir := t.TempDir()
	if n := retireAllSessionLogs(dir); n != 0 {
		t.Fatalf("retired %d on a fresh PVC", n)
	}
	if exists(filepath.Join(dir, "projects")) {
		t.Errorf("projects/ must not be created")
	}
}

// -- prepareLocalSessionLogs: the runInitLoop purge combinator ------------------
//
// runInitLoop calls prepareLocalSessionLogs before every coop (re)launch, so the
// combinator's truth table is exercised here exactly as the loop sees it.

func TestPrepareLocalSessionLogs_Combinator(t *testing.T) {
	cases := []struct {
		name       string
		bucket     string
		beadID     string
		forceClean bool
		wantPurged bool // true => deleted outright; false => retired (kept as .stale)
	}{
		{"no bucket (fresh) purges", "", "gd-x", false, true},
		{"bucket but no session key purges", "b", "", false, true},
		{"s3 resume intended retires, keeps bytes", "b", "gd-x", false, false},
		{"force-clean purges even with s3 intended", "b", "gd-x", true, true},
		{"no bucket and force-clean purges", "", "gd-x", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			forceCleanTestReset(t)
			t.Setenv("COOP_S3_BUCKET", tc.bucket)
			t.Setenv("KD_AGENT_ID", tc.beadID)
			t.Setenv("BOAT_AGENT_BEAD_ID", tc.beadID)
			canonicalS3Key.Store(nil)
			decision := tc.forceClean
			forceCleanLatched.Store(&decision)

			dir := t.TempDir()
			live := writeTranscript(t, dir, "prev-incarnation", 0)

			prepareLocalSessionLogs(dir)

			if got := listJSONLs(t, dir); len(got) != 0 {
				t.Errorf("live .jsonl survived into the coop launch: %v", got)
			}
			if kept := exists(live + ".stale"); kept == tc.wantPurged {
				t.Errorf(".stale kept=%v, want kept=%v", kept, !tc.wantPurged)
			}
			if tc.wantPurged && exists(filepath.Join(dir, "projects", "-work", "prev-incarnation")) {
				t.Errorf("purge must also remove the companion dir")
			}
		})
	}
}

// -- evidence-gated wake trigger -----------------------------------------------

func sessionBead(agentBead, status string, resumed bool, runtime time.Duration) *beadsapi.BeadDetail {
	start := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	f := map[string]string{
		"agent_bead_id": agentBead,
		"status":        status,
		"resumed":       "false",
		"started_at":    start.Format(time.RFC3339),
	}
	if resumed {
		f["resumed"] = "true"
	}
	if status != "active" {
		f["ended_at"] = start.Add(runtime).Format(time.RFC3339)
	}
	return &beadsapi.BeadDetail{Fields: f}
}

func TestCountFailedResumes(t *testing.T) {
	const me = "gd-me"
	fast, slow := 30*time.Second, 2*time.Hour
	cases := []struct {
		name string
		in   []*beadsapi.BeadDetail // newest first
		want int64
	}{
		{"empty", nil, 0},
		{"two fast failed resumes", []*beadsapi.BeadDetail{
			sessionBead(me, "crashed", true, fast), sessionBead(me, "crashed", true, fast)}, 2},
		{"active session is skipped", []*beadsapi.BeadDetail{
			sessionBead(me, "active", true, 0), sessionBead(me, "crashed", true, fast)}, 1},
		{"OOM after a long resumed run is churn, not a failed resume", []*beadsapi.BeadDetail{
			sessionBead(me, "crashed", true, slow)}, 0},
		{"fresh (not resumed) crash is not a failed resume", []*beadsapi.BeadDetail{
			sessionBead(me, "crashed", false, fast)}, 0},
		{"a healthy completed session ends the streak", []*beadsapi.BeadDetail{
			sessionBead(me, "crashed", true, fast),
			sessionBead(me, "completed", true, slow),
			sessionBead(me, "crashed", true, fast)}, 1},
		{"other agents' sessions are ignored", []*beadsapi.BeadDetail{
			sessionBead("gd-other", "crashed", true, fast)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countFailedResumes(tc.in, me); got != tc.want {
				t.Fatalf("countFailedResumes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestDecideForceClean_WakeTriggerNeedsEvidence(t *testing.T) {
	thr := forceCleanThresholds{wakes: 12, wakeMinFailures: 1}
	if fire, _ := decideForceClean(forceCleanSignals{wakes: 40}, thr); fire {
		t.Fatal("bare wake count must not force-clean a churning agent")
	}
	fire, reason := decideForceClean(forceCleanSignals{wakes: 12, failedResumes: 1}, thr)
	if !fire || !strings.Contains(reason, "wakes=12") || !strings.Contains(reason, "failed_resumes=1") {
		t.Fatalf("wakes + failed resume must fire with both in the reason; got %v %q", fire, reason)
	}
	// Legacy escape hatch: gate disabled => bare count fires again.
	thr.wakeMinFailures = 0
	if fire, _ := decideForceClean(forceCleanSignals{wakes: 12}, thr); !fire {
		t.Fatal("wakeMinFailures<=0 must restore the legacy bare-wake-count trigger")
	}
	// Turn/token triggers are unaffected by the gate.
	thr = forceCleanThresholds{turns: 800, wakes: 12, wakeMinFailures: 1}
	if fire, _ := decideForceClean(forceCleanSignals{turns: 900}, thr); !fire {
		t.Fatal("turn trigger must still fire without resume-failure evidence")
	}
}

// serveBeadsAndSessions answers the agent-bead GET and the session-bead list.
func serveBeadsAndSessions(t *testing.T, agentBeadID string, agentFields map[string]any, sessions []map[string]any) *beadsapi.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads/"+agentBeadID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": agentBeadID, "type": "agent", "kind": "config", "status": "open", "fields": agentFields,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": sessions, "total": len(sessions)})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func sessionWire(agentBead, status string, resumed bool, runtime time.Duration) map[string]any {
	d := sessionBead(agentBead, status, resumed, runtime)
	f := map[string]any{}
	for k, v := range d.Fields {
		f[k] = v
	}
	f["resumed"] = resumed
	return map[string]any{"id": "gd-sess", "type": "session", "kind": "config", "status": "closed", "fields": f}
}

type wakeAlertCapture struct {
	mu       sync.Mutex
	outcomes []forceCleanWakeMetricOutcome
}

func captureWakeAlerts(t *testing.T) *wakeAlertCapture {
	t.Helper()
	c := &wakeAlertCapture{}
	old := forceCleanWakeAlerter
	forceCleanWakeAlerter = func(_ context.Context, _ string, _ forceCleanSignals, _ forceCleanThresholds, o forceCleanWakeMetricOutcome) {
		c.mu.Lock()
		c.outcomes = append(c.outcomes, o)
		c.mu.Unlock()
	}
	t.Cleanup(func() { forceCleanWakeAlerter = old })
	return c
}

func setupWakePolicy(t *testing.T, wakes int, sessions []map[string]any) {
	t.Helper()
	forceCleanTestReset(t)
	t.Setenv("COOP_S3_BUCKET", "gardenio-sessions")
	t.Setenv("KD_AGENT_ID", "gd-wake1")
	t.Setenv("BOAT_AGENT", "wake-agent")
	t.Setenv(forceCleanWakeMinResumeFailuresEnv, "")
	canonicalS3Key.Store(nil)
	oldDaemon := daemon
	daemon = serveBeadsAndSessions(t, "gd-wake1", map[string]any{
		"live_turns": 10, "context_tokens": 1000, "session_count": wakes,
	}, sessions)
	t.Cleanup(func() { daemon = oldDaemon })
}

// End-to-end through resolveForceCleanPolicy: a churning agent over the wake
// threshold, with no failed resume on record, keeps its transcript — but the
// crossing is alerted.
func TestResolveForceCleanPolicy_WakesWithoutFailedResumeAlertsButKeepsTranscript(t *testing.T) {
	alerts := captureWakeAlerts(t)
	setupWakePolicy(t, 14, []map[string]any{
		sessionWire("gd-wake1", "crashed", true, 3*time.Hour), // OOM after long productive run
	})

	resolveForceCleanPolicy(context.Background())

	if forceCleanActive() {
		t.Fatal("churning agent without a failed resume must NOT be force-cleaned")
	}
	if len(alerts.outcomes) != 1 || alerts.outcomes[0] != wakeOutcomeNoEvidence {
		t.Fatalf("want one %q alert, got %v", wakeOutcomeNoEvidence, alerts.outcomes)
	}
	// And the loop therefore retires (keeps) rather than deletes.
	dir := t.TempDir()
	live := writeTranscript(t, dir, "prev", 0)
	prepareLocalSessionLogs(dir)
	if !exists(live + ".stale") {
		t.Error("transcript should survive the restart of a churning agent")
	}
}

func TestResolveForceCleanPolicy_WakesWithFailedResumeForceCleans(t *testing.T) {
	alerts := captureWakeAlerts(t)
	setupWakePolicy(t, 12, []map[string]any{
		sessionWire("gd-wake1", "crashed", true, 20*time.Second),
	})

	resolveForceCleanPolicy(context.Background())

	if !forceCleanActive() {
		t.Fatal("wake threshold + failed resume must latch force-clean")
	}
	if len(alerts.outcomes) != 1 || alerts.outcomes[0] != wakeOutcomeFired {
		t.Fatalf("want one %q alert, got %v", wakeOutcomeFired, alerts.outcomes)
	}
	if !envContains(buildCoopEnv(initConfig{role: "crew"}), forceCleanEnvVar+"="+forceCleanEnvValue) {
		t.Error("force-clean must wire COOP_S3_NO_RESTORE into the coop env")
	}
	// Force-clean => the loop deletes the transcript outright.
	dir := t.TempDir()
	live := writeTranscript(t, dir, "prev", 0)
	prepareLocalSessionLogs(dir)
	if exists(live) || exists(live+".stale") {
		t.Error("force-clean must purge the oversized transcript, not retain it")
	}
}

func TestResolveForceCleanPolicy_BelowWakeThresholdNeitherAlertsNorReadsSessions(t *testing.T) {
	alerts := captureWakeAlerts(t)
	setupWakePolicy(t, 11, nil)
	resolveForceCleanPolicy(context.Background())
	if forceCleanActive() || len(alerts.outcomes) != 0 {
		t.Fatalf("11 wakes: active=%v alerts=%v, want neither", forceCleanActive(), alerts.outcomes)
	}
}

func TestResolveForceCleanPolicy_WakeEnvOverridesAndKillSwitch(t *testing.T) {
	failed := []map[string]any{sessionWire("gd-wake1", "crashed", true, 10*time.Second)}

	t.Run("wake threshold override", func(t *testing.T) {
		captureWakeAlerts(t)
		setupWakePolicy(t, 3, failed)
		t.Setenv(forceCleanWakeThresholdEnv, "3")
		resolveForceCleanPolicy(context.Background())
		if !forceCleanActive() {
			t.Fatal("GARDENIO_FORCE_CLEAN_WAKE_THRESHOLD=3 must be honored")
		}
	})
	t.Run("min-failures 0 restores legacy bare count", func(t *testing.T) {
		captureWakeAlerts(t)
		setupWakePolicy(t, 20, nil) // no sessions at all
		t.Setenv(forceCleanWakeMinResumeFailuresEnv, "0")
		resolveForceCleanPolicy(context.Background())
		if !forceCleanActive() {
			t.Fatal("GARDENIO_FORCE_CLEAN_WAKE_MIN_RESUME_FAILURES=0 must restore bare-count trigger")
		}
	})
	t.Run("kill switch beats evidence", func(t *testing.T) {
		alerts := captureWakeAlerts(t)
		setupWakePolicy(t, 99, failed)
		t.Setenv(forceCleanDisableEnv, "1")
		resolveForceCleanPolicy(context.Background())
		if forceCleanActive() || len(alerts.outcomes) != 0 {
			t.Fatalf("kill switch: active=%v alerts=%v", forceCleanActive(), alerts.outcomes)
		}
	})
}
