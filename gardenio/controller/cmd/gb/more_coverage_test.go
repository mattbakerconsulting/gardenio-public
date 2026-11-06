package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// ── escalation.go helpers ────────────────────────────────────────────────

func TestTruncField(t *testing.T) {
	for _, tc := range []struct {
		in   string
		n    int
		want string
	}{
		{"short", 10, "short"},
		{"exactly10x", 10, "exactly10x"},
		{"way too long for the field", 10, "way too l…"},
	} {
		if got := truncField(tc.in, tc.n); got != tc.want {
			t.Errorf("truncField(%q,%d)=%q, want %q", tc.in, tc.n, got, tc.want)
		}
	}
}

func TestPrintField(t *testing.T) {
	// No assertion on stdout; just exercise both branches.
	printField("Label", "value")
	printField("Label", "")
}

func TestFormatJSONCompact(t *testing.T) {
	// Valid small JSON.
	got := formatJSONCompact(`{"a":1}`)
	if got != `{"a":1}` {
		t.Errorf("small JSON: %q", got)
	}
	// Invalid JSON returns as-is.
	if formatJSONCompact("not-json") != "not-json" {
		t.Error("invalid JSON path")
	}
	// Long JSON → multi-line pretty output.
	bigObj := map[string]string{}
	for i := range 20 {
		bigObj[fmt.Sprintf("key_number_%02d", i)] = fmt.Sprintf("value_%02d", i)
	}
	raw, _ := json.Marshal(bigObj)
	out := formatJSONCompact(string(raw))
	if !strings.Contains(out, "\n") {
		t.Error("expected multiline pretty output")
	}
}

// ── dm.go helpers ─────────────────────────────────────────────────────────

func TestTruncateDMTitle(t *testing.T) {
	short := "hello"
	if got := truncateDMTitle(short); got != short {
		t.Errorf("short: %q", got)
	}
	long := strings.Repeat("x", 200)
	got := truncateDMTitle(long)
	if len(got) != 100 {
		t.Errorf("expected len 100, got %d", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("expected ellipsis, got %q", got)
	}
}

func TestSleepChan(t *testing.T) {
	ch := sleepChan(0)
	select {
	case <-ch:
		// OK — received on time.
	case <-time.After(2 * time.Second):
		t.Error("sleepChan(0) did not fire within 2s")
	}
}

// ── identity.go filterIdentities ─────────────────────────────────────────

func TestFilterIdentities(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "1", Title: "Alice Smith"},
		{ID: "2", Title: "Bob", Fields: map[string]string{"gitlab_username": "bobby"}},
		{ID: "3", Title: "Carol", Fields: map[string]string{"email": "carol@example.com"}},
		{ID: "4", Title: "Dave", Fields: map[string]string{"slack_id": "U999XYZ"}},
		{ID: "5", Title: "Eve", Fields: map[string]string{"jira_user": "eve.j"}},
	}
	if got := filterIdentities(beads, "alice"); len(got) != 1 || got[0].ID != "1" {
		t.Errorf("title match: %+v", got)
	}
	if got := filterIdentities(beads, "bobby"); len(got) != 1 || got[0].ID != "2" {
		t.Errorf("gitlab match: %+v", got)
	}
	if got := filterIdentities(beads, "example.com"); len(got) != 1 || got[0].ID != "3" {
		t.Errorf("email match: %+v", got)
	}
	if got := filterIdentities(beads, "u999"); len(got) != 1 || got[0].ID != "4" {
		t.Errorf("slack_id match: %+v", got)
	}
	if got := filterIdentities(beads, "eve.j"); len(got) != 1 || got[0].ID != "5" {
		t.Errorf("jira match: %+v", got)
	}
	if got := filterIdentities(beads, "nobody"); len(got) != 0 {
		t.Errorf("no match: %+v", got)
	}
}

// ── hook.go stop-gate helpers (coverage gaps) ─────────────────────────────

func setupStopGateFiles(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldC, oldB := stopGateCooldownFile, stopGateBlockCountFile
	stopGateCooldownFile = filepath.Join(dir, "cooldown")
	stopGateBlockCountFile = filepath.Join(dir, "count")
	t.Cleanup(func() {
		stopGateCooldownFile = oldC
		stopGateBlockCountFile = oldB
	})
}

func TestStopGateEscapeHatch(t *testing.T) {
	setupStopGateFiles(t)
	if stopGateEscapeHatch() {
		t.Error("no count → should not escape")
	}
	_ = os.WriteFile(stopGateBlockCountFile, fmt.Appendf(nil, "%d\n", stopGateMaxBlocks), 0o644)
	if !stopGateEscapeHatch() {
		t.Error("count=max should escape")
	}
	_ = os.WriteFile(stopGateBlockCountFile, fmt.Appendf(nil, "%d\n", stopGateMaxBlocks+5), 0o644)
	if !stopGateEscapeHatch() {
		t.Error("count>max should escape")
	}
}

func TestRecordStopGateBlock(t *testing.T) {
	setupStopGateFiles(t)
	recordStopGateBlock()
	if _, err := os.Stat(stopGateCooldownFile); err != nil {
		t.Errorf("cooldown file missing: %v", err)
	}
	if got := readStopGateBlockCount(); got != 1 {
		t.Errorf("expected count=1 after first block, got %d", got)
	}
	recordStopGateBlock()
	if got := readStopGateBlockCount(); got != 2 {
		t.Errorf("expected count=2 after second block, got %d", got)
	}
}

func TestClearStopGateCooldown(t *testing.T) {
	setupStopGateFiles(t)
	_ = os.WriteFile(stopGateCooldownFile, []byte("100"), 0o644)
	_ = os.WriteFile(stopGateBlockCountFile, []byte("3"), 0o644)
	clearStopGateCooldown()
	if _, err := os.Stat(stopGateCooldownFile); !os.IsNotExist(err) {
		t.Error("cooldown file should be removed")
	}
	if _, err := os.Stat(stopGateBlockCountFile); !os.IsNotExist(err) {
		t.Error("count file should be removed")
	}
	// Idempotent: calling again should not error.
	clearStopGateCooldown()
}

func TestReadStopGateBlockCount_InvalidContent(t *testing.T) {
	setupStopGateFiles(t)
	_ = os.WriteFile(stopGateBlockCountFile, []byte("not-a-number"), 0o644)
	if got := readStopGateBlockCount(); got != 0 {
		t.Errorf("invalid content should yield 0, got %d", got)
	}
}

func TestIsAgentRateLimited_Refused(t *testing.T) {
	t.Setenv("COOP_URL", "http://127.0.0.1:1")
	if isAgentRateLimited() {
		t.Error("refused connection should return false")
	}
}

func TestIsAgentRateLimited_OKRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error_category": "rate_limited"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("COOP_URL", srv.URL)
	if !isAgentRateLimited() {
		t.Error("expected rate_limited=true")
	}
}

func TestIsAgentRateLimited_Normal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"error_category": "none"})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("COOP_URL", srv.URL)
	if isAgentRateLimited() {
		t.Error("expected rate_limited=false for non-rate-limited category")
	}
}

func TestIsAgentRateLimited_GarbageBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("not json"))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("COOP_URL", srv.URL)
	if isAgentRateLimited() {
		t.Error("bad JSON should yield false")
	}
}

// ── stop.go helpers ───────────────────────────────────────────────────────

func TestRetireAgentSessions_EmptyDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	retired, errs := retireAgentSessions()
	if retired != 0 || len(errs) != 0 {
		t.Errorf("empty: retired=%d errs=%v", retired, errs)
	}
}

func TestRetireAgentSessions_RenamesAndSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projDir := filepath.Join(home, ".claude", "projects", "proj-x")
	subDir := filepath.Join(projDir, "subagents")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// One jsonl in proj-x — should be retired.
	primary := filepath.Join(projDir, "session-1.jsonl")
	if err := os.WriteFile(primary, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One jsonl under subagents/ — should be skipped.
	subSession := filepath.Join(subDir, "session-2.jsonl")
	if err := os.WriteFile(subSession, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-jsonl file — ignored.
	if err := os.WriteFile(filepath.Join(projDir, "other.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	retired, errs := retireAgentSessions()
	if retired != 1 || len(errs) != 0 {
		t.Errorf("retired=%d errs=%v", retired, errs)
	}
	if _, err := os.Stat(primary + ".stale"); err != nil {
		t.Errorf("expected %s.stale: %v", primary, err)
	}
	if _, err := os.Stat(subSession); err != nil {
		t.Error("subagent session should still exist (not retired)")
	}
}

func TestAgentSessionDuration_NoDaemon(t *testing.T) {
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })
	if d := agentSessionDuration(context.Background(), "gd-1"); d != 0 {
		t.Errorf("no daemon → 0, got %v", d)
	}
	if d := agentSessionDuration(context.Background(), ""); d != 0 {
		t.Errorf("empty id → 0, got %v", d)
	}
}

func TestAgentSessionDuration_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	if d := agentSessionDuration(context.Background(), "gd-1"); d != 0 {
		t.Errorf("server error → 0, got %v", d)
	}
}

func TestAgentSessionDuration_OK(t *testing.T) {
	created := time.Now().Add(-5 * time.Minute)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id":         "gd-1",
			"title":      "agent",
			"type":       "agent",
			"status":     "open",
			"created_at": created.Format(time.RFC3339Nano),
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	d := agentSessionDuration(context.Background(), "gd-1")
	if d < 4*time.Minute || d > 10*time.Minute {
		t.Errorf("expected ~5min, got %v", d)
	}
}

func TestProcessWrapUp_Valid(t *testing.T) {
	raw := `{"accomplishments":"did stuff","blockers":"none","handoff_notes":"n","pull_requests":["!99"]}`
	w, comment, err := processWrapUp(raw)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if w == nil || w.Accomplishments != "did stuff" {
		t.Errorf("bad parse: %+v", w)
	}
	if comment == "" {
		t.Error("empty comment")
	}
}

func TestProcessWrapUp_Invalid(t *testing.T) {
	if _, _, err := processWrapUp("not-json"); err == nil {
		t.Error("expected JSON error")
	}
}

// ── decision.go printDecisionResult ───────────────────────────────────────

func TestPrintDecisionResult_Chosen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-D",
			"status": "closed",
			"fields": map[string]string{
				"chosen":            "opt-a",
				"required_artifact": "plan",
				"artifact_status":   "pending",
			},
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	if err := printDecisionResult("gd-D"); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPrintDecisionResult_ResponseText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-D",
			"status": "closed",
			"fields": map[string]string{"response_text": "go!"},
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	if err := printDecisionResult("gd-D"); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPrintDecisionResult_Neither(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"id":     "gd-D",
			"status": "closed",
			"fields": map[string]string{},
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	if err := printDecisionResult("gd-D"); err != nil {
		t.Errorf("err: %v", err)
	}
}

func TestPrintDecisionResult_FetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	if err := printDecisionResult("gd-D"); err == nil {
		t.Error("expected error on fetch failure")
	}
}

// ── hook.go outputClaimReminder ───────────────────────────────────────────

func TestOutputClaimReminder_NoAgentName(t *testing.T) {
	// Empty name is an early return.
	outputClaimReminder(context.Background(), "")
}

func TestOutputClaimReminder_HasAssignedTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// ListAssignedTask hits /bead/{id} or /query? Let it succeed with a bead.
		json.NewEncoder(w).Encode(map[string]any{
			"id": "gd-T", "title": "task", "type": "task", "status": "in_progress",
			"fields": map[string]string{},
		})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	// Exercise path — no panic / no error propagated.
	outputClaimReminder(context.Background(), "agent-name")
}

func TestOutputClaimReminder_NoTaskNoDecision(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Both ListAssignedTask and ListBeadsFiltered return empty.
		if strings.Contains(r.URL.Path, "assigned") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	outputClaimReminder(context.Background(), "nobody")
}

// ── state.go format for injection (currently only partial) ────────────────

func TestFormatStateForInjection_Extra(t *testing.T) {
	// Empty map.
	if got := formatStateForInjection(nil); got != "" {
		t.Errorf("nil map should be empty, got %q", got)
	}
	// Populated.
	s := map[string]string{"FOO": "bar", "AAA": "xyz"}
	got := formatStateForInjection(s)
	if !strings.Contains(got, "FOO") || !strings.Contains(got, "bar") {
		t.Errorf("missing content: %q", got)
	}
	// Sorted — AAA should come before FOO in output.
	if strings.Index(got, "AAA") > strings.Index(got, "FOO") {
		t.Error("keys not sorted")
	}
}

func TestReadState_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)
	t.Setenv("HOME", dir) // fallback
	if err := os.MkdirAll(filepath.Join(dir, ".state"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".state", "learned.json"), []byte("not-json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(); err == nil {
		t.Error("expected parse error")
	}
}

func TestWriteState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)
	t.Setenv("HOME", dir)
	in := map[string]string{"k1": "v1", "k2": "v2"}
	if err := writeState(in); err != nil {
		t.Fatalf("writeState: %v", err)
	}
	out, err := readState()
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if len(out) != 2 || out["k1"] != "v1" || out["k2"] != "v2" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestStateFile_UsesKDWorkspaceThenHome(t *testing.T) {
	t.Setenv("KD_WORKSPACE", "/tmp/ws")
	if got := stateFile(); got != "/tmp/ws/.state/learned.json" {
		t.Errorf("KD_WORKSPACE: %q", got)
	}
	t.Setenv("KD_WORKSPACE", "")
	t.Setenv("HOME", "/tmp/home")
	if got := stateFile(); got != "/tmp/home/.state/learned.json" {
		t.Errorf("HOME: %q", got)
	}
}

func TestSortedKeys(t *testing.T) {
	in := map[string]string{"z": "", "a": "", "m": ""}
	got := sortedKeys(in)
	want := []string{"a", "m", "z"}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("sortedKeys[%d]=%q, want %q", i, got[i], k)
		}
	}
}

// ── session.go fileExists & walkSessionLogs ───────────────────────────────

func TestFileExists_Extra(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileExists(f) {
		t.Error("expected true for existing file")
	}
	if fileExists(filepath.Join(dir, "missing.txt")) {
		t.Error("expected false for missing")
	}
	if fileExists(dir) {
		t.Error("directory should not be reported as file")
	}
}

func TestWalkSessionLogs(t *testing.T) {
	dir := t.TempDir()
	// project dir with a valid jsonl + a subagent jsonl that should be skipped.
	proj := filepath.Join(dir, "projA")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(proj, "session.jsonl")
	if err := os.WriteFile(valid, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	subagent := filepath.Join(proj, "subagent-thing.jsonl")
	if err := os.WriteFile(subagent, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(proj, "other.txt")
	if err := os.WriteFile(other, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-directory entry at the top-level.
	if err := os.WriteFile(filepath.Join(dir, "top.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	var seen []string
	err := walkSessionLogs(dir, func(path string, mt time.Time) {
		seen = append(seen, path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || !strings.HasSuffix(seen[0], "session.jsonl") {
		t.Errorf("expected 1 session.jsonl, got %+v", seen)
	}
}

func TestWalkSessionLogs_MissingDir(t *testing.T) {
	if err := walkSessionLogs("/nonexistent/nope", func(string, time.Time) {}); err == nil {
		t.Error("expected error for missing dir")
	}
}

func TestFindLatestSessionLog_NoHome(t *testing.T) {
	// Even with a bogus HOME, must not panic — returns "".
	t.Setenv("HOME", "/nonexistent/zz")
	if got := findLatestSessionLog(); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

// ── init_agent.go envDuration (edge case for bad-int path) ────────────────

func TestEnvDuration_BadInt(t *testing.T) {
	t.Setenv("TEST_ENV_DURATION_BAD", "abc")
	got := envDuration("TEST_ENV_DURATION_BAD", 7*time.Second)
	if got != 7*time.Second {
		t.Errorf("bad int should fall back to default, got %v", got)
	}
}

func TestShouldExitBeforeStart_NoAgentID(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	old := daemon
	daemon = nil
	t.Cleanup(func() { daemon = old })
	if shouldExitBeforeStart(context.Background()) {
		t.Error("no agent id, no daemon → false")
	}
}
