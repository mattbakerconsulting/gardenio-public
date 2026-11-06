package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Real-binary test for the Gemini stop gate (ADR 0005 §6/§7 phase 2): the pinned
// Gemini CLI runs a turn with the settings.json `gb setup gemini` writes, the
// AfterAgent hook is the translated `gb hook stop-gate --format gemini` run as a
// real subprocess against a fake daemon, and the test asserts the CLI made a
// SECOND model call carrying the gate's reason, i.e. the turn was actually
// continued. Asserting only the hook's output string would pass for a format the
// CLI ignores (`{"continue":true}` does exactly that), and a wrong-format block
// fails open.
//
// Skips unless a gemini binary (the agent image installs 0.64.0 via
// images/agent/install/gemini.sh; or point GEMINI_BIN at gemini.js) and node are
// available. It needs no API key: the CLI talks to the scripted stand-in
// docs/research/gemini-harness/tools/mock-api, so only the CLI's own behaviour is
// under test.

// runAsGbIfRequested lets this test binary stand in for `gb` when a Gemini hook
// subprocess execs it (see the wrapper script below). The stop-gate scratch
// files default to /tmp, which a real agent shares with its own gate, so they
// are redirected before main runs.
func runAsGbIfRequested() {
	if os.Getenv("GB_TEST_AS_GB") != "1" {
		return
	}
	if dir := os.Getenv("GB_TEST_SCRATCH"); dir != "" {
		stopGateCooldownFile = filepath.Join(dir, "stop-gate-last-block")
		stopGateBlockCountFile = filepath.Join(dir, "stop-gate-block-count")
		stopGateYieldMarkerFile = filepath.Join(dir, "stop-gate-yielding")
		checkMailSeenFile = filepath.Join(dir, "check-mail-seen")
		injectQueueFile = filepath.Join(dir, "inject-queue.jsonl")
	}
	main()
	os.Exit(0)
}

func geminiBinary(t *testing.T) []string {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH")
	}
	if p := os.Getenv("GEMINI_BIN"); p != "" {
		if strings.HasSuffix(p, ".js") {
			return []string{"node", p}
		}
		return []string{p}
	}
	p, err := exec.LookPath("gemini")
	if err != nil {
		t.Skip("gemini CLI not on PATH (installed in the agent image by images/agent/install/gemini.sh)")
	}
	return []string{p}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startMockGeminiAPI runs the scripted Gemini API and returns its base URL and
// request log path.
func startMockGeminiAPI(t *testing.T) (string, string) {
	t.Helper()
	script, err := filepath.Abs("../../../../docs/research/gemini-harness/tools/mock-api/mock_gemini_api.mjs")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Skipf("mock Gemini API not found: %v", err)
	}
	port := freeTCPPort(t)
	logPath := filepath.Join(t.TempDir(), "requests.jsonl")
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, "node", script, fmt.Sprint(port), logPath)
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	for i := 0; i < 50; i++ {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond); err == nil {
			c.Close()
			return fmt.Sprintf("http://127.0.0.1:%d", port), logPath
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("mock Gemini API did not start")
	return "", ""
}

type mockRequest struct {
	Path string `json:"path"`
	Body struct {
		Contents []struct {
			Role  string `json:"role"`
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		} `json:"contents"`
		GenerationConfig struct {
			ResponseMimeType string `json:"responseMimeType"`
		} `json:"generationConfig"`
	} `json:"body"`
}

// mainModelCalls returns the requests that are the agent's own model calls (not
// the CLI's internal structured classifier/router calls).
func mainModelCalls(t *testing.T, logPath string) []mockRequest {
	t.Helper()
	f, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []mockRequest
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<22), 1<<22)
	for sc.Scan() {
		var r mockRequest
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			continue
		}
		if strings.HasSuffix(r.Path, ":streamGenerateContent") && r.Body.GenerationConfig.ResponseMimeType != "application/json" {
			out = append(out, r)
		}
	}
	return out
}

func lastUserText(r mockRequest) string {
	for i := len(r.Body.Contents) - 1; i >= 0; i-- {
		if r.Body.Contents[i].Role == "user" {
			var sb strings.Builder
			for _, p := range r.Body.Contents[i].Parts {
				sb.WriteString(p.Text)
			}
			return sb.String()
		}
	}
	return ""
}

type fakeGateDaemon struct {
	mu        sync.Mutex
	block     bool // the first stop-gate emit blocks, later ones allow
	emits     int
	published []string // hook-relay subjects
	harnesses []string // Harness field of each Stop emit
}

func (d *fakeGateDaemon) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/hooks/emit", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Harness string `json:"harness"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.emits++
		first := d.emits == 1
		d.harnesses = append(d.harnesses, req.Harness)
		d.mu.Unlock()
		resp := map[string]any{"block": d.block && first}
		if d.block && first {
			resp["reason"] = "GATE-REASON-7f3a: create a decision checkpoint before stopping"
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/v1/hooks/publish", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Subject string          `json:"subject"`
			Payload json.RawMessage `json:"payload"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		d.mu.Lock()
		d.published = append(d.published, req.Subject+" "+string(req.Payload))
		d.mu.Unlock()
		_, _ = w.Write([]byte("{}"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) })
	return mux
}

// runGeminiTurn runs one headless turn with the translated gardenio hooks and
// returns the stream-json output.
func runGeminiTurn(t *testing.T, prompt string, gateBlocks bool) (stream string, calls []mockRequest, d *fakeGateDaemon) {
	t.Helper()
	gemini := geminiBinary(t)
	baseURL, logPath := startMockGeminiAPI(t)

	d = &fakeGateDaemon{block: gateBlocks}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)

	home := t.TempDir()
	ws := t.TempDir()
	scratch := t.TempDir()
	bin := t.TempDir()

	// `gb` on PATH is this test binary in disguise.
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper := fmt.Sprintf("#!/bin/sh\nGB_TEST_AS_GB=1 exec %q \"$@\"\n", self)
	if err := os.WriteFile(filepath.Join(bin, "gb"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}

	// The settings the product writes, unmodified: translated default hooks,
	// auth/statistics/update pins and the model pin.
	for _, k := range []string{"JIRA_BASE_URL", "JIRA_EMAIL", "JIRA_API_TOKEN"} {
		t.Setenv(k, "")
	}
	in := geminiSettingsInput{Model: "gemini-2.5-pro"}
	if err := writeGeminiSettings(filepath.Join(home, ".gemini"), in, defaultHookSettings()); err != nil {
		t.Fatal(err)
	}

	args := append(append([]string{}, gemini[1:]...), "-p", prompt, "-o", "stream-json", "--approval-mode", "yolo")
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, gemini[0], args...)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(),
		"HOME="+home,
		"GOOGLE_GEMINI_BASE_URL="+baseURL,
		"GEMINI_API_KEY=mock-key",
		"GEMINI_CLI_TRUST_WORKSPACE=true",
		"GEMINI_CLI_HOME=",
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"GB_TEST_SCRATCH="+scratch,
		"BEADS_HTTP_URL="+srv.URL,
		"GARDENIO_HARNESS=gemini",
		"BOAT_AGENT=test-agent",
		"KD_AGENT_ID=",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gemini failed: %v\n%s", err, out)
	}
	return string(out), mainModelCalls(t, logPath), d
}

func TestGeminiStopGate_RealBinaryContinuesTurn(t *testing.T) {
	stream, calls, d := runGeminiTurn(t, "say pong", true)

	if len(calls) != 2 {
		t.Fatalf("model calls = %d, want 2: the gate's block must continue the turn\n%s", len(calls), stream)
	}
	if got := lastUserText(calls[1]); !strings.Contains(got, "GATE-REASON-7f3a") {
		t.Errorf("second model call's last user turn = %q, want it to carry the gate's reason", got)
	}
	if !strings.Contains(stream, "Agent execution blocked: ") || !strings.Contains(stream, "GATE-REASON-7f3a") {
		t.Errorf("stream-json lacks the blocked-stop warning:\n%s", stream)
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// The re-fired AfterAgent (stop_hook_active=true) hits the gate's cooldown and
	// allows the stop before emitting again, so the daemon sees one Stop.
	if d.emits != 1 {
		t.Errorf("daemon saw %d Stop emits, want 1", d.emits)
	}
	for _, h := range d.harnesses {
		if h != "gemini" {
			t.Errorf("stop emit harness = %q, want gemini", h)
		}
	}
}

// Control: when the gate allows, the same turn makes exactly one model call, so
// the second call above is the gate's doing and not the CLI's own retry.
func TestGeminiStopGate_RealBinaryAllowEndsTurn(t *testing.T) {
	_, calls, d := runGeminiTurn(t, "say pong", false)
	if len(calls) != 1 {
		t.Fatalf("model calls = %d, want 1 when the gate allows the stop", len(calls))
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.emits != 1 {
		t.Errorf("stop-gate ran %d times, want 1", d.emits)
	}
}

// The translated relay hooks publish Gemini's tool events under the canonical
// subject names with harness=gemini, from the real hook payloads.
func TestGeminiRelay_RealBinaryPublishesCanonicalToolEvents(t *testing.T) {
	_, _, d := runGeminiTurn(t, "MOCK:TOOL echo hi", false)
	d.mu.Lock()
	defer d.mu.Unlock()
	var pre, post bool
	for _, p := range d.published {
		if strings.HasPrefix(p, "hooks.test-agent.PreToolUse ") {
			pre = true
			if !strings.Contains(p, `"harness":"gemini"`) || !strings.Contains(p, `"tool_name":"run_shell_command"`) {
				t.Errorf("PreToolUse payload = %s", p)
			}
		}
		if strings.HasPrefix(p, "hooks.test-agent.PostToolUse ") {
			post = true
		}
		if strings.Contains(p, ".BeforeTool ") || strings.Contains(p, ".AfterTool ") {
			t.Errorf("Gemini event name leaked onto the bus: %s", p)
		}
	}
	if !pre || !post {
		t.Errorf("published = %v, want PreToolUse and PostToolUse", d.published)
	}
}
