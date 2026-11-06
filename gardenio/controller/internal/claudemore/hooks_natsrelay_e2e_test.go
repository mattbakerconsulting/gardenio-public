//go:build e2e

// End-to-end proof of the PreToolUse/PostToolUse → NATS relay path for the
// claudemore hooks e2e (bead ka-0000004452). It is `//go:build e2e`-tagged so it
// is invisible to `go build ./...`, `go test ./...`, and golangci-lint (none of
// which pass `-tags e2e`) — it runs ONLY when the hooks e2e module invokes it via
// `go test -tags e2e -run TestHooksNATSRelayE2E`. Tagging keeps the heavy
// nats-server embed off the default lint/build surface (which otherwise pushed
// the lint task over its 5-minute timeout).
//
// It proves the relay path WITHOUT a cluster: embed a NATS server, subscribe to
// `hooks.>`, invoke the REAL `gb hook relay` command (the exact code a
// PreToolUse/PostToolUse hook runs in production) with BEADS_NATS_URL pointed at
// the embedded server and the preferred HTTP publisher forced to a dead endpoint
// so the relay deterministically falls through to the NATS path, and assert a
// correctly-shaped event lands for each hook.
//
// It emits PASS:/FAIL:/PROBE_SKIP: lines the shell harness
// (test-claudemore-hooks.sh) parses, and fails the test on any missing event.
package claudemore

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

const (
	relayAgentName = "e2e-claudemore-hooks-probe"
	relaySubjectWc = "hooks.>"
	relayWait      = 8 * time.Second
)

func TestHooksNATSRelayE2E(t *testing.T) {
	gbBin := os.Getenv("GB_BIN")
	if gbBin == "" {
		gbBin = "gb"
	}
	if _, err := exec.LookPath(gbBin); err != nil {
		// A missing gb binary is a precondition gap, not a test failure — the
		// shell wrapper decides whether that is a clean skip.
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

	// ── Subscribe to hooks.> and collect events ────────────────────────
	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("FAIL: connect to embedded NATS: %v", err)
	}
	defer nc.Close()

	var mu sync.Mutex
	events := map[string]map[string]any{}
	subjects := map[string]string{}
	sub, err := nc.Subscribe(relaySubjectWc, func(m *nats.Msg) {
		var payload map[string]any
		if json.Unmarshal(m.Data, &payload) != nil {
			return
		}
		name, _ := payload["event"].(string)
		if name == "" {
			return
		}
		mu.Lock()
		events[name] = payload
		subjects[name] = m.Subject
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("FAIL: subscribe to %s: %v", relaySubjectWc, err)
	}
	// Flush so the subscription's interest propagates BEFORE we publish — a
	// publish that races ahead of interest is silently dropped (core NATS).
	_ = nc.Flush()
	defer func() { _ = sub.Unsubscribe() }()

	// ── Drive the REAL `gb hook relay` for each hook ───────────────────
	pre := map[string]any{
		"hook_event_name": "PreToolUse",
		"session_id":      "e2e-sess-1",
		"cwd":             "/work",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "echo hi"},
	}
	post := map[string]any{
		"hook_event_name": "PostToolUse",
		"session_id":      "e2e-sess-1",
		"cwd":             "/work",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": "echo hi"},
		"tool_response":   map[string]any{"stdout": "hi"},
	}
	for _, in := range []map[string]any{pre, post} {
		if err := invokeHookRelay(gbBin, natsURL, in); err != nil {
			t.Fatalf("FAIL: gb hook relay (%v): %v", in["hook_event_name"], err)
		}
	}

	// ── Wait for both events to land ───────────────────────────────────
	deadline := time.Now().Add(relayWait)
	for time.Now().Before(deadline) {
		mu.Lock()
		_, gotPre := events["PreToolUse"]
		_, gotPost := events["PostToolUse"]
		mu.Unlock()
		if gotPre && gotPost {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// ── Assert ─────────────────────────────────────────────────────────
	mu.Lock()
	defer mu.Unlock()

	checkEvent := func(name string) {
		evt, ok := events[name]
		if !ok {
			t.Errorf("FAIL: %s event never arrived on NATS %s", name, relaySubjectWc)
			return
		}
		wantSubject := "hooks." + relayAgentName + "." + name
		if subjects[name] != wantSubject {
			t.Errorf("FAIL: %s subject: got %q want %q", name, subjects[name], wantSubject)
			return
		}
		if tool, _ := evt["tool_name"].(string); tool != "Bash" {
			t.Errorf("FAIL: %s tool_name: got %q want Bash", name, tool)
			return
		}
		t.Logf("PASS: %s relayed to %s with tool_name=Bash", name, wantSubject)
	}
	checkEvent("PreToolUse")
	checkEvent("PostToolUse")

	// PostToolUse must additionally carry the tool_response the relay extracts.
	if evt, ok := events["PostToolUse"]; ok {
		if _, hasResp := evt["tool_response"]; hasResp {
			t.Log("PASS: PostToolUse carries tool_response")
		} else {
			t.Error("FAIL: PostToolUse missing tool_response field")
		}
	}
}

// invokeHookRelay runs `gb hook relay`, feeding the hook input JSON on stdin with
// BEADS_NATS_URL pointed at the embedded server. It forces the relay's PREFERRED
// HTTP publisher to a dead endpoint (--http-url http://127.0.0.1:1 → immediate
// connection-refused) so the relay deterministically falls through to the NATS
// publish path this test subscribes to. Without this the relay could publish to a
// reachable CLUSTER daemon and the event would never reach the embedded server —
// a false negative. A closed loopback port fails fast, so the probe stays quick.
func invokeHookRelay(gbBin, natsURL string, input map[string]any) error {
	body, err := json.Marshal(input)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gbBin, "hook", "relay", "--http-url", "http://127.0.0.1:1")
	cmd.Stdin = strings.NewReader(string(body))
	cmd.Env = append(os.Environ(),
		"BOAT_AGENT="+relayAgentName,
		"BEADS_NATS_URL="+natsURL,
		"BEADS_HTTP_ADDR=",
		"BEADS_HTTP_URL=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return errWithOutput(err, out)
	}
	return nil
}

func errWithOutput(err error, out []byte) error {
	if len(out) == 0 {
		return err
	}
	return &relayError{err: err, out: strings.TrimSpace(string(out))}
}

type relayError struct {
	err error
	out string
}

func (e *relayError) Error() string { return e.err.Error() + ": " + e.out }
func (e *relayError) Unwrap() error  { return e.err }
