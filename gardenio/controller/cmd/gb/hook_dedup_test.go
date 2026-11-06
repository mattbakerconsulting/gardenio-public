package main

// hook_dedup_test.go pins the ka-0000007688 context-injection de-dups:
//
//   (2) PreCompact double-emit: `gb hook prime --enqueue` must write the prime
//       body to the inject queue ONLY, not also to stdout — drain-queue.sh
//       re-emits it after compaction, so printing here too injected the same
//       2–15 KB twice around every compaction.
//
//   (3) check-mail re-injection: `gb hook check-mail` runs on every
//       UserPromptSubmit; it must inject only mail that is NEW since the last
//       check, not the full open inbox every operator turn.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── (2) PreCompact double-emit ─────────────────────────────────────────────

// TestHookPrime_EnqueueDoesNotPrintPrimeBody verifies that with --enqueue the
// prime body is routed to the queue only, so stdout does NOT carry the same
// content the PostToolUse drain will re-emit after compaction.
func TestHookPrime_EnqueueDoesNotPrintPrimeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")
	oldQ := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = oldQ })

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")
	// No assignment bead / claim — keep the assignment + claim-reminder blocks
	// off stdout so this test isolates the prime-body routing decision.
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	oldEnqueue := primeEnqueue
	primeEnqueue = true
	t.Cleanup(func() { primeEnqueue = oldEnqueue })

	cmd := hookPrimeCmd
	cmd.SetContext(context.Background())

	out := string(captureStdout(t, func() error {
		return cmd.RunE(cmd, nil)
	}))

	// The prime body (jacks + the pointer line) must NOT appear on stdout when
	// enqueuing — that is the whole double-emit this fixes.
	if strings.Contains(out, "loaded in the system prompt") {
		t.Errorf("--enqueue must not also print the prime body to stdout; got:\n%s", out)
	}

	// ...but it MUST be in the queue for drain-queue.sh to re-emit post-compaction.
	data, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("reading queue file: %v", err)
	}
	var entry map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("unmarshalling queue entry: %v", err)
	}
	if !strings.Contains(entry["content"], "loaded in the system prompt") {
		t.Errorf("prime body must be enqueued for post-compaction recovery; queue content:\n%s", entry["content"])
	}
}

// TestHookPrime_NoEnqueuePrintsToStdout verifies the SessionStart path
// (no --enqueue) still prints the prime body to stdout — there is no queue
// drain in that path, so stdout is the only channel.
func TestHookPrime_NoEnqueuePrintsToStdout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": []any{}})
	}))
	defer srv.Close()
	swapDaemon(t, srv.URL)

	tmpDir := t.TempDir()
	queuePath := filepath.Join(tmpDir, "inject-queue.jsonl")
	oldQ := injectQueueFile
	injectQueueFile = queuePath
	t.Cleanup(func() { injectQueueFile = oldQ })

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")
	t.Setenv("KD_AGENT_ID", "")

	oldEnqueue := primeEnqueue
	primeEnqueue = false
	t.Cleanup(func() { primeEnqueue = oldEnqueue })

	cmd := hookPrimeCmd
	cmd.SetContext(context.Background())

	out := string(captureStdout(t, func() error {
		return cmd.RunE(cmd, nil)
	}))

	if !strings.Contains(out, "loaded in the system prompt") {
		t.Errorf("SessionStart path (no --enqueue) must print the prime body to stdout; got:\n%s", out)
	}
	// Nothing should be queued when not enqueuing.
	if _, err := os.Stat(queuePath); !os.IsNotExist(err) {
		t.Errorf("no --enqueue: queue file must not be created")
	}
}

// ── (3) check-mail re-injection ────────────────────────────────────────────

// mailServer returns an httptest server that answers the check-mail query with
// the given mail beads.
func mailServer(t *testing.T, beads []map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"beads": beads, "total": len(beads)})
	}))
}

func withSeenMailFile(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "check-mail-seen")
	old := checkMailSeenFile
	checkMailSeenFile = path
	t.Cleanup(func() { checkMailSeenFile = old })
}

func runCheckMail(t *testing.T) string {
	t.Helper()
	cmd := hookCheckMailCmd
	cmd.SetContext(context.Background())
	return string(captureStdout(t, func() error {
		return cmd.RunE(cmd, nil)
	}))
}

// TestCheckMail_FirstTurnAnnouncesAll: a fresh pod (no seen file) announces the
// whole open inbox once.
func TestCheckMail_FirstTurnAnnouncesAll(t *testing.T) {
	srv := mailServer(t, []map[string]any{
		{"id": "gd-M1", "title": "hello", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:alice"}},
		{"id": "gd-M2", "title": "ping", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:bob"}},
	})
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withSeenMailFile(t)
	t.Setenv("KD_ACTOR", "tester")

	out := runCheckMail(t)
	if !strings.Contains(out, "gd-M1") || !strings.Contains(out, "gd-M2") {
		t.Errorf("first turn must announce all open mail; got:\n%s", out)
	}
}

// TestCheckMail_SecondTurnSuppressesSeen: the same open inbox on a later turn is
// NOT re-injected — this is the per-turn re-injection the fix removes.
func TestCheckMail_SecondTurnSuppressesSeen(t *testing.T) {
	srv := mailServer(t, []map[string]any{
		{"id": "gd-M1", "title": "hello", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:alice"}},
		{"id": "gd-M2", "title": "ping", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:bob"}},
	})
	defer srv.Close()
	swapDaemon(t, srv.URL)
	withSeenMailFile(t)
	t.Setenv("KD_ACTOR", "tester")

	_ = runCheckMail(t) // first turn: announce + record seen
	out := runCheckMail(t)
	if strings.TrimSpace(out) != "" {
		t.Errorf("second turn with no new mail must emit nothing; got:\n%s", out)
	}
}

// TestCheckMail_OnlyNewMailAnnounced: after a turn, only mail that arrived since
// is announced — not the already-seen backlog.
func TestCheckMail_OnlyNewMailAnnounced(t *testing.T) {
	withSeenMailFile(t)
	t.Setenv("KD_ACTOR", "tester")

	srv1 := mailServer(t, []map[string]any{
		{"id": "gd-M1", "title": "hello", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:alice"}},
	})
	swapDaemon(t, srv1.URL)
	if out := runCheckMail(t); !strings.Contains(out, "gd-M1") {
		t.Fatalf("first turn must announce gd-M1; got:\n%s", out)
	}
	srv1.Close()

	// A new message arrives; the backlog (gd-M1) is still open.
	srv2 := mailServer(t, []map[string]any{
		{"id": "gd-M2", "title": "new", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:bob"}},
		{"id": "gd-M1", "title": "hello", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:alice"}},
	})
	defer srv2.Close()
	swapDaemon(t, srv2.URL)

	out := runCheckMail(t)
	if !strings.Contains(out, "gd-M2") {
		t.Errorf("new mail gd-M2 must be announced; got:\n%s", out)
	}
	if strings.Contains(out, "gd-M1") {
		t.Errorf("already-seen mail gd-M1 must NOT be re-announced; got:\n%s", out)
	}
}

// TestCheckMail_ClosedMailForgotten: once mail leaves the open set, its ID is
// dropped from the seen file (we persist the current open set, not a cumulative
// union), so it never wedges the bounded state — and if it were ever reopened it
// would be announced again.
func TestCheckMail_ClosedMailForgotten(t *testing.T) {
	withSeenMailFile(t)
	t.Setenv("KD_ACTOR", "tester")

	srv1 := mailServer(t, []map[string]any{
		{"id": "gd-M1", "title": "hello", "type": "mail", "status": "open", "assignee": "tester", "labels": []string{"from:alice"}},
	})
	swapDaemon(t, srv1.URL)
	_ = runCheckMail(t) // sees gd-M1
	srv1.Close()

	// gd-M1 is closed → no longer in the open set.
	srv2 := mailServer(t, []map[string]any{})
	swapDaemon(t, srv2.URL)
	_ = runCheckMail(t) // empty inbox: current set recorded as empty
	srv2.Close()

	// The seen file must no longer carry gd-M1 (bounded to the current window).
	if seen := loadSeenMailIDs(); seen["gd-M1"] {
		t.Errorf("closed mail gd-M1 must be forgotten from the seen set; still present")
	}
}
