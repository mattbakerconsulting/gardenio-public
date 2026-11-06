//go:build e2e

// End-to-end proof that a SubagentStop hook forwards the subagent's FINAL
// assistant message all the way to the agent stream (bead ka-0000004963, parent
// ka-0000000958). This is the e2e backfill for the fix in gd-TdQnFDzJdo
// (MR !6760): before that fix HandleSubagentStop flipped the delegation chunk to
// "complete" with an EMPTY Output — the subagent's result was silently dropped.
// The fix threads evt.LastAssistantMessage from hookstream.go through
// HandleSubagentStop into the chunk Output; this probe drives the WHOLE chain so
// it FAILS against the pre-fix behaviour (empty Output), not just a unit shim.
//
// It is `//go:build e2e`-tagged (invisible to `go build ./...`, `go test ./...`,
// and golangci-lint, none of which pass `-tags e2e`) so the heavy embedded
// nats-server never inflates the default build/lint surface — the same tagging
// discipline as internal/claudemore/hooks_natsrelay_e2e_test.go. It runs ONLY
// when the SubagentStop e2e module invokes it via
// `go test -tags e2e -run TestSubagentStopForwardsFinalMessageE2E`.
//
// THE PATH UNDER TEST (production wiring, no shims on the hot path):
//
//	gb hook relay (SubagentStop + last_assistant_message)   ← the REAL binary
//	  → NATS  hooks.<agent>.SubagentStop                    ← embedded server
//	    → HookStream.handleMessage (the REAL subscriber)     ← production code
//	      → AgentStreamManager.HandleSubagentStop            ← the fixed handler
//	        → stream chunk {type:task_update,status:complete,output:<msg>}
//
// Only the terminal Slack sink is a mock (mockStreamClient) — it is the seam
// where the chunk becomes observable, exactly as the unit tests use it. The
// assertions target the chunk's SHAPE and OUTPUT, never model text, so the probe
// is deterministic.
//
// It emits PASS:/FAIL:/PROBE_SKIP: log lines the shell harness
// (test-subagent-stop.sh) parses, mirroring the nats-relay probe's contract.
package bridge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

const (
	subStopAgentName = "e2e-subagentstop-probe"
	subStopChannel   = "C-SUBSTOP-E2E"
	subStopThreadTS  = "thread-substop-e2e"
	subStopUserID    = "U-SUBSTOP-E2E"
	subStopType      = "Explore"
	subStopSubID     = "sub-e2e-1"
	// The unique final message the subagent "returns". A unique marker rules out
	// the assertion passing on stray text from any other chunk on the stream.
	subStopFinalMsg = "SUBAGENT_FINAL_MARKER: found the root cause in auth.go:42"
	subStopWait     = 8 * time.Second
)

// TestSubagentStopForwardsFinalMessageE2E drives the real gb hook relay binary
// through the real HookStream subscriber and asserts the subagent's final
// message reaches the agent-stream chunk Output with status "complete".
func TestSubagentStopForwardsFinalMessageE2E(t *testing.T) {
	gbBin := os.Getenv("GB_BIN")
	if gbBin == "" {
		gbBin = "gb"
	}
	if _, err := exec.LookPath(gbBin); err != nil {
		// A missing gb binary is a precondition gap, not a failure — the shell
		// wrapper decides whether that is a clean skip.
		t.Skipf("PROBE_SKIP: gb binary %q not found on PATH (%v)", gbBin, err)
	}

	// ── Embed a NATS server ────────────────────────────────────────────
	ns, err := natsserver.NewServer(&natsserver.Options{Host: "127.0.0.1", Port: -1})
	if err != nil {
		t.Fatalf("FAIL: start embedded NATS: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("FAIL: embedded NATS not ready within 5s")
	}
	defer ns.Shutdown()
	natsURL := ns.ClientURL()

	// ── Wire the REAL bridge consumer: HookStream → AgentStreamManager ──
	// mockStreamClient is the only seam — it captures the chunks that would go
	// to Slack, exactly as the unit tests observe them. Everything upstream of
	// it (subscriber dispatch, handler, chunk construction) is production code.
	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	// A live session is a precondition for any chunk to be emitted
	// (sendTaskUpdate drops events for agents without a session).
	mgr.StartSession(context.Background(), subStopAgentName, subStopChannel, subStopThreadTS, subStopUserID)

	hs := NewHookStream(HookStreamConfig{
		NatsURL:       natsURL,
		StreamManager: mgr,
		Logger:        testLogger(),
	})
	if hs == nil {
		t.Fatal("FAIL: NewHookStream returned nil (NATS URL or stream manager unset)")
	}
	if err := hs.Start(); err != nil {
		t.Fatalf("FAIL: HookStream.Start: %v", err)
	}
	defer hs.Close()

	// ── Drive the REAL `gb hook relay` for the SubagentStop hook ───────
	// This is byte-for-byte the input Claude Code delivers to the relay on a
	// SubagentStop. buildRelayEvent reads agent_id/agent_type/last_assistant_message
	// and publishes hooks.<agent>.SubagentStop — the exact production hop.
	subStopInput := map[string]any{
		"hook_event_name":        "SubagentStop",
		"session_id":             "e2e-substop-sess-1",
		"cwd":                    "/work",
		"agent_id":               subStopSubID,
		"agent_type":             subStopType,
		"last_assistant_message": subStopFinalMsg,
	}
	if err := relaySubagentStop(gbBin, natsURL, subStopInput); err != nil {
		t.Fatalf("FAIL: gb hook relay (SubagentStop): %v", err)
	}

	// ── Wait for the completion chunk to land ──────────────────────────
	// Poll the mock for an appendChunks call carrying the subagent chunk. The
	// relay → NATS → subscriber → handler hop is async, so wait up to subStopWait.
	var chunkJSON string
	deadline := time.Now().Add(subStopWait)
	for time.Now().Before(deadline) {
		chunkJSON = lastSubagentChunk(mock.getCalls())
		if chunkJSON != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if chunkJSON == "" {
		t.Fatalf("FAIL: no subagent task_update chunk reached the stream within %s "+
			"(relay→NATS→subscriber→handler chain dropped the event)", subStopWait)
	}
	t.Logf("PASS: SubagentStop relayed end-to-end to a stream chunk")

	// Decode the chunk so assertions target fields, not substrings — the field
	// shape is the contract HandleSubagentStop must honour.
	var chunks []StreamChunk
	if err := json.Unmarshal([]byte(chunkJSON), &chunks); err != nil {
		t.Fatalf("FAIL: chunk JSON did not decode: %v (raw: %s)", err, chunkJSON)
	}
	if len(chunks) != 1 {
		t.Fatalf("FAIL: expected exactly 1 chunk, got %d (raw: %s)", len(chunks), chunkJSON)
	}
	chunk := chunks[0]

	if chunk.Type != "task_update" {
		t.Errorf("FAIL: chunk type: got %q want task_update", chunk.Type)
	} else {
		t.Log("PASS: chunk is a task_update")
	}
	if chunk.Status != "complete" {
		t.Errorf("FAIL: chunk status: got %q want complete", chunk.Status)
	} else {
		t.Log("PASS: delegation marked complete")
	}
	wantChunkID := "subagent:" + subStopSubID
	if chunk.ID != wantChunkID {
		t.Errorf("FAIL: chunk id: got %q want %q", chunk.ID, wantChunkID)
	} else {
		t.Log("PASS: chunk id targets the subagent delegation")
	}

	// ── THE HEADLINE ASSERTION — the exact drop the fix repaired ───────
	// Pre-fix, Output was hard-coded "" and this fails; post-fix the subagent's
	// final message is forwarded verbatim (within the 300-char truncation).
	if chunk.Output == "" {
		t.Errorf("FAIL: subagent final message was DROPPED — chunk Output is empty "+
			"(this is the pre-fix regression from gd-TdQnFDzJdo; want %q)", subStopFinalMsg)
	} else if chunk.Output != subStopFinalMsg {
		t.Errorf("FAIL: chunk Output: got %q want %q", chunk.Output, subStopFinalMsg)
	} else {
		t.Logf("PASS: subagent FINAL message forwarded to the parent stream (Output=%q)", chunk.Output)
	}
}

// lastSubagentChunk returns the chunksJSON of the most recent appendChunks call
// whose payload carries the subagent delegation chunk, or "" if none. It scans
// newest-first so a later completion chunk wins over an earlier in_progress one.
func lastSubagentChunk(calls []streamCall) string {
	for i := len(calls) - 1; i >= 0; i-- {
		c := calls[i]
		if c.method != "appendChunks" {
			continue
		}
		if strings.Contains(c.chunksJSON, `"subagent:`) && strings.Contains(c.chunksJSON, `"complete"`) {
			return c.chunksJSON
		}
	}
	return ""
}

// relaySubagentStop runs `gb hook relay`, feeding the hook input JSON on stdin
// with BEADS_NATS_URL pointed at the embedded server. It forces the relay's
// PREFERRED HTTP publisher to a dead endpoint (--http-url http://127.0.0.1:1 →
// immediate connection-refused) so the relay deterministically falls through to
// the NATS publish path this probe's subscriber listens on. Without this the
// relay could publish to a reachable CLUSTER daemon and the event would never
// reach the embedded server — a false negative. A closed loopback port fails
// fast, keeping the probe quick. Mirrors invokeHookRelay in
// internal/claudemore/hooks_natsrelay_e2e_test.go.
func relaySubagentStop(gbBin, natsURL string, input map[string]any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gbBin, "hook", "relay", "--http-url", "http://127.0.0.1:1")
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Env = append(os.Environ(),
		"BOAT_AGENT="+subStopAgentName,
		"BEADS_NATS_URL="+natsURL,
		"BEADS_HTTP_ADDR=",
		"BEADS_HTTP_URL=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if len(out) == 0 {
			return err
		}
		return &subStopRelayError{err: err, out: strings.TrimSpace(string(out))}
	}
	return nil
}

type subStopRelayError struct {
	err error
	out string
}

func (e *subStopRelayError) Error() string { return e.err.Error() + ": " + e.out }
func (e *subStopRelayError) Unwrap() error { return e.err }
