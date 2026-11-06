package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// yieldDaemonStub is a minimal beads daemon used to exercise the yield wait
// loop. It serves GET /v1/beads/{id}, GET /v1/beads (filtered list), and
// PATCH /v1/beads/{id} (field updates), recording the last fields payload so
// tests can assert how yield_count was serialized.
type yieldDaemonStub struct {
	mu sync.Mutex
	// beads keyed by id → JSON-able map with at least id/type/kind/status/fields.
	beads map[string]map[string]any
	// assignedList is returned for GET /v1/beads (newly-assigned-work poll).
	assignedList []map[string]any
	// lastPatchFields holds the raw "fields" JSON from the most recent PATCH.
	lastPatchFields json.RawMessage
	// sseEvents are streamed, in order, to any /v1/events/stream subscriber.
	// Each is written as one SSE frame then the stream stays open (so yieldSSE
	// keeps waiting) until the test's context is cancelled.
	sseEvents []sseFrame
}

// sseFrame is one server-sent event the stub emits on /v1/events/stream.
type sseFrame struct {
	event string
	data  map[string]any
}

func (s *yieldDaemonStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			s.mu.Lock()
			b, ok := s.beads[id]
			s.mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(b)

		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			body, _ := io.ReadAll(r.Body)
			var parsed map[string]json.RawMessage
			_ = json.Unmarshal(body, &parsed)
			s.mu.Lock()
			s.lastPatchFields = parsed["fields"]
			// Merge into stored bead so subsequent GETs reflect the write.
			if b, ok := s.beads[id]; ok {
				var f map[string]any
				_ = json.Unmarshal(parsed["fields"], &f)
				b["fields"] = f
			}
			s.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})

		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			s.mu.Lock()
			resp := map[string]any{"beads": s.assignedList, "total": len(s.assignedList)}
			s.mu.Unlock()
			_ = json.NewEncoder(w).Encode(resp)

		case r.Method == http.MethodGet && r.URL.Path == "/v1/events/stream":
			// Emit each queued frame as an SSE event, then hold the stream open
			// until the request context is cancelled — this mirrors the real
			// daemon, which keeps a live stream and lets yieldSSE decide when a
			// frame satisfies the wait. Holding it open is what forces yield to
			// KEEP waiting when a frame (e.g. a peer's mail) must be ignored.
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			s.mu.Lock()
			frames := s.sseEvents
			s.mu.Unlock()
			for _, f := range frames {
				b, _ := json.Marshal(f.data)
				_, _ = w.Write([]byte("event: " + f.event + "\ndata: " + string(b) + "\n\n"))
				if flusher != nil {
					flusher.Flush()
				}
			}
			<-r.Context().Done()

		default:
			http.NotFound(w, r)
		}
	})
}

func setupYieldDaemon(t *testing.T, stub *yieldDaemonStub) {
	t.Helper()
	srv := httptest.NewServer(stub.handler())
	t.Cleanup(srv.Close)

	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	oldDaemon := daemon
	daemon = c
	t.Cleanup(func() { daemon = oldDaemon })
}

func beadMap(id, typ, kind, status string, fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}
	return map[string]any{
		"id":     id,
		"type":   typ,
		"kind":   kind,
		"status": status,
		"fields": fields,
	}
}

// Bug 2: yieldPollOnce must NOT report resolution while the target decision
// is still open, even though unrelated message/mail beads may be flying past.
func TestYieldPollOnce_OpenDecisionDoesNotResolve(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		},
	}
	setupYieldDaemon(t, stub)

	out, done := yieldPollOnce(context.Background(), map[string]bool{"gd-dec": true}, "")
	if done {
		t.Fatalf("yieldPollOnce returned done=true for an open decision; out=%+v", out)
	}
	if out.resolved {
		t.Fatal("open decision must not be reported resolved")
	}
}

// Bug 2: once the target decision actually closes, yieldPollOnce reports a
// genuine resolution.
func TestYieldPollOnce_ClosedDecisionResolves(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "closed",
				map[string]any{"chosen": "yes"}),
		},
	}
	setupYieldDaemon(t, stub)

	out, done := yieldPollOnce(context.Background(), map[string]bool{"gd-dec": true}, "")
	if !done {
		t.Fatal("yieldPollOnce should report done=true for a closed decision")
	}
	if !out.resolved {
		t.Fatal("closed decision must be reported resolved")
	}
}

// Bug 2: a decision that records a choice (chosen) without closing still
// counts as resolved.
func TestDecisionResolved_ChosenWithoutClose(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open",
				map[string]any{"chosen": "option-a"}),
		},
	}
	setupYieldDaemon(t, stub)

	if !decisionResolved(context.Background(), "gd-dec") {
		t.Fatal("decision with a recorded choice must be resolved")
	}
}

// Bug 2: newly-assigned issue-kind work interrupts yield but is NOT a
// resolution — out.resolved must be false so the caller does not print
// "Decision resolved".
func TestYieldPollOnce_AssignedWorkInterruptsButNotResolved(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		},
		assignedList: []map[string]any{
			beadMap("gd-task", "task", "issue", "open", nil),
		},
	}
	setupYieldDaemon(t, stub)

	out, done := yieldPollOnce(context.Background(), map[string]bool{"gd-dec": true}, "agent-x")
	if !done {
		t.Fatal("assigned issue-kind work should interrupt yield (done=true)")
	}
	if out.resolved {
		t.Fatal("assigned work is an interruption, not a decision resolution")
	}
}

// Bug 1: the yield_count increment must serialize as a JSON integer so the
// integer-typed schema field accepts it. The previous string-map path stored
// "1" (a JSON string) on first yield, which the daemon rejected with HTTP 400
// "yield_count: must be an integer". Here we drive UpdateBeadFieldsTyped (the
// path yield now uses) and assert the wire value is a bare number.
func TestYieldCount_WritesIntegerNotString(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			// Agent bead with NO prior yield_count — the first-yield case.
			"gd-agent": beadMap("gd-agent", "agent", "config", "open", nil),
		},
	}
	setupYieldDaemon(t, stub)

	if err := daemon.UpdateBeadFieldsTyped(context.Background(), "gd-agent",
		map[string]any{"yield_count": 1}); err != nil {
		t.Fatalf("UpdateBeadFieldsTyped: %v", err)
	}

	stub.mu.Lock()
	raw := string(stub.lastPatchFields)
	stub.mu.Unlock()

	var f map[string]any
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("parse patched fields %q: %v", raw, err)
	}
	v, ok := f["yield_count"]
	if !ok {
		t.Fatalf("yield_count missing from patched fields: %q", raw)
	}
	// encoding/json decodes JSON numbers into float64; a string would decode
	// into string. Assert we sent a number, which is what the integer schema
	// requires.
	if _, isNum := v.(float64); !isNum {
		t.Fatalf("yield_count serialized as %T (%v), want JSON number; raw=%q", v, v, raw)
	}
	if !strings.Contains(raw, `"yield_count":1`) {
		t.Errorf("expected bare integer yield_count in payload, got %q", raw)
	}
}

// Regression for ka-0000006882: printYieldResult must announce "resolved" even
// when the decision is closed but its chosen/response_text fields have not yet
// propagated to the read replica (the fast SSE-close path). Previously this
// branch printed "Decision <id> closed" with no "resolved" token, which flaked
// the decisions-yield E2E assertion 'yield output mentions resolved' whenever
// the close raced ahead of field propagation.
func TestPrintYieldResult_ClosedWithoutFieldsSaysResolved(t *testing.T) {
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			// Closed, but no chosen/response_text yet (replica lag).
			"gd-dec": beadMap("gd-dec", "decision", "data", "closed", nil),
		},
	}
	setupYieldDaemon(t, stub)

	out := captureStdout(t, func() error {
		return printYieldResult("gd-dec")
	})

	if !strings.Contains(string(out), "resolved") {
		t.Fatalf("expected output to contain 'resolved' for a closed decision; got: %q", string(out))
	}
}

// ka-0000008032: the SSE mail-wake in yieldSSE must only fire for mail
// addressed to THIS agent. The kd server is shared fleet-wide, so a naive
// "any type=mail wakes" fired the instant ANY agent anywhere received mail —
// that spuriously ended an unrelated agent's park and, in the shared-actor
// E2E, let a concurrent shard's mail wake decisions-yield's scenario-5 yield
// (it printed "Mail received" / "Interrupted by new work" instead of the
// expected "Decision resolved"). mailAddressedToMe is the recipient gate.
func TestMailAddressedToMe(t *testing.T) {
	cases := []struct {
		name          string
		beadAssignee  string
		mailRecipient string
		want          bool
	}{
		{"addressed to me wakes", "agent-a", "agent-a", true},
		{"addressed to a peer is ignored", "agent-b", "agent-a", false},
		{"empty recipient (unknown identity) falls back to waking", "agent-b", "", true},
		{"unassigned mail with known identity is ignored", "", "agent-a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mailAddressedToMe(tc.beadAssignee, tc.mailRecipient); got != tc.want {
				t.Fatalf("mailAddressedToMe(%q, %q) = %v, want %v",
					tc.beadAssignee, tc.mailRecipient, got, tc.want)
			}
		})
	}
}

// ka-0000008032 (integration): yieldSSE drives the REAL SSE path. A peer's
// mail (assigned to some other agent) streamed while the target decision is
// still open must NOT wake yield; only mail addressed to THIS agent's mail
// identity does. This is the end-to-end proof of the decisions-yield flake fix:
// before the recipient gate, this test would wake on the peer's mail (the exact
// scenario-5 failure where a concurrent shard's mail resolved an unrelated
// yield).
func TestYieldSSE_PeerMailDoesNotWake(t *testing.T) {
	const myIdentity = "agent-me"
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			// The target decision resolves (closes) AFTER the peer mail frame,
			// so the only correct wake is the decision — never the peer's mail.
			"gd-dec": beadMap("gd-dec", "decision", "data", "closed",
				map[string]any{"chosen": "a"}),
		},
		sseEvents: []sseFrame{
			// A peer's mail, addressed to a DIFFERENT agent. Must be ignored.
			{event: "beads.bead.created", data: map[string]any{
				"bead": map[string]any{
					"id": "gd-peer-mail", "type": "mail", "kind": "data",
					"assignee": "some-other-agent",
				},
			}},
			// Then the target decision closes — the legitimate wake.
			{event: "beads.bead.closed", data: map[string]any{
				"bead": map[string]any{
					"id": "gd-dec", "type": "decision", "kind": "data",
				},
			}},
		},
	}
	setupYieldDaemon(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pending := []*beadsapi.BeadDetail{{ID: "gd-dec", Type: "decision"}}
	out := captureStdout(t, func() error {
		outcome := yieldSSE(ctx, pending, myIdentity, myIdentity, time.Time{})
		if !outcome.resolved {
			t.Errorf("yield must wake on the DECISION close, not the peer mail; outcome=%+v", outcome)
		}
		return nil
	})
	if strings.Contains(string(out), "Mail received") {
		t.Fatalf("yield woke on a peer's mail — recipient gate failed; output=%q", string(out))
	}
	if !strings.Contains(string(out), "resolved") {
		t.Fatalf("expected decision-resolved output; got %q", string(out))
	}
}

// ka-0000008032 (integration): mail addressed to THIS agent still wakes yield —
// the recipient gate must not over-block. Guards against a fix that ignores ALL
// mail (which would break the legitimate scenario-6 mail interrupt).
func TestYieldSSE_OwnMailWakes(t *testing.T) {
	const myIdentity = "agent-me"
	stub := &yieldDaemonStub{
		beads: map[string]map[string]any{
			"gd-dec": beadMap("gd-dec", "decision", "data", "open", nil),
		},
		sseEvents: []sseFrame{
			{event: "beads.bead.created", data: map[string]any{
				"bead": map[string]any{
					"id": "gd-my-mail", "type": "mail", "kind": "data",
					"assignee": myIdentity,
				},
			}},
		},
	}
	setupYieldDaemon(t, stub)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	pending := []*beadsapi.BeadDetail{{ID: "gd-dec", Type: "decision"}}
	out := captureStdout(t, func() error {
		outcome := yieldSSE(ctx, pending, myIdentity, myIdentity, time.Time{})
		if outcome.resolved {
			t.Errorf("mail is an interruption, not a decision resolution; outcome=%+v", outcome)
		}
		return nil
	})
	if !strings.Contains(string(out), "Mail received") {
		t.Fatalf("yield must wake on mail addressed to this agent; output=%q", string(out))
	}
}

// ka-0000006309 (gb-stop gate umbrella ka-0000004255, abatement #3): when an
// agent calls `gb yield` with no open self-owned decision — the normal gardener
// "crews in-flight, nothing to decide" state — the error must point at
// `gb stop`, NOT instruct the agent to manufacture a decision via
// `gb decision create`. Following the old hint created an option-less decision
// that the decision-options validator rejected, an unreachable yield-while-
// crews-work path. The fix returns a non-zero error (yield must not silently
// succeed and clear the stop gate) whose primary remedy is `gb stop`.
func TestNoPendingDecisionErr_RecommendsStopNotDecisionCreate(t *testing.T) {
	err := noPendingDecisionErr("gd-agent-123")
	if err == nil {
		t.Fatal("noPendingDecisionErr must return a non-zero error so yield does not silently clear the stop gate")
	}
	msg := err.Error()

	// Primary remedy for the done/waiting-on-crews case is gb stop.
	if !strings.Contains(msg, "gb stop") {
		t.Errorf("error should recommend 'gb stop' as the primary remedy; got: %q", msg)
	}

	// The agent id must be surfaced for debuggability.
	if !strings.Contains(msg, "gd-agent-123") {
		t.Errorf("error should name the agent id; got: %q", msg)
	}

	// Regression guard: the old message made `gb decision create` the primary
	// fix for the no-decision case. `gb decision create` may still be mentioned
	// as the narrow exception (a real fork), but `gb stop` must come first so
	// it reads as the recommended action.
	stopIdx := strings.Index(msg, "gb stop")
	decIdx := strings.Index(msg, "gb decision create")
	if decIdx != -1 && decIdx < stopIdx {
		t.Errorf("'gb decision create' must not precede 'gb stop' — stop is the recommended remedy; got: %q", msg)
	}
}
