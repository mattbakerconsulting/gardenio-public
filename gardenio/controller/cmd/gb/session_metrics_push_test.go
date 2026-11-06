package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestBuildPrometheusPayload(t *testing.T) {
	labels := sessionMetricsLabels{
		AgentName: "test-agent",
		Project:   "gardenio",
		Account:   "acme-corp",
		SessionID: "gd-ABC123",
	}
	usage := &beadsapi.SessionUsage{
		InputTokens:      1000,
		OutputTokens:     200,
		CacheReadTokens:  500,
		CacheWriteTokens: 50,
		TotalCostUSD:     0.0425,
		RequestCount:     5,
		ToolCallCount:    3,
		ErrorCount:       1,
		CompactionCount:   0,
		ContextTokens:     1550,
		BootContextTokens: 71661,
	}

	payload := buildPrometheusPayload(labels, usage, 300)

	// Verify all expected metrics are present.
	expected := []string{
		`gardenio_session_tokens_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123",type="input"} 1000`,
		`gardenio_session_tokens_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123",type="output"} 200`,
		`gardenio_session_tokens_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123",type="cache_read"} 500`,
		`gardenio_session_tokens_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123",type="cache_write"} 50`,
		`gardenio_session_duration_seconds{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 300`,
		`gardenio_session_request_count_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 5`,
		// Canonical turn counter (ka-0000003327): same value as request_count,
		// which is kept as an alias for one release.
		`gardenio_session_turns_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 5`,
		// Live context-window level (ka-0000002524).
		`gardenio_session_context_tokens{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 1550`,
		// Boot-context-at-start level (ka-0000003733, Part B): the turn-0 resident context.
		`gardenio_session_boot_context_tokens{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 71661`,
		`gardenio_session_tool_calls_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 3`,
		`gardenio_session_errors_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 1`,
		`gardenio_session_compactions_total{agent_name="test-agent",project="gardenio",account="acme-corp",session_id="gd-ABC123"} 0`,
	}

	for _, line := range expected {
		if !strings.Contains(payload, line) {
			t.Errorf("payload missing expected line:\n  want: %s\n  payload:\n%s", line, payload)
		}
	}

	// ka-0000000999: the gb push must NOT emit any cost metric. Cost is the
	// in-pod JSONL response_cost estimate (low-biased, never reconciled); the
	// authoritative cost counters are emitted controller-side at session close.
	for _, costMetric := range []string{"gardenio_session_cost_usd", "gardenio_operator_cost_usd_total"} {
		if strings.Contains(payload, costMetric) {
			t.Errorf("payload must not contain non-authoritative cost metric %q:\n%s", costMetric, payload)
		}
	}
}

// TestBuildPrometheusPayload_BootContextAbsentWhenZero verifies the boot-context
// metric is omitted for a resumed session with no turn 0 (BootContextTokens==0),
// so a zero sample never masquerades as a genuine tiny boot (epic ka-0000003733).
func TestBuildPrometheusPayload_BootContextAbsentWhenZero(t *testing.T) {
	labels := sessionMetricsLabels{AgentName: "a", Project: "gardenio", SessionID: "gd-Z"}
	usage := &beadsapi.SessionUsage{InputTokens: 100, RequestCount: 1} // BootContextTokens defaults to 0

	payload := buildPrometheusPayload(labels, usage, 60)

	if strings.Contains(payload, "gardenio_session_boot_context_tokens") {
		t.Errorf("boot-context metric must be absent when BootContextTokens==0:\n%s", payload)
	}
}

func TestBuildPrometheusPayload_EmptyLabels(t *testing.T) {
	labels := sessionMetricsLabels{}
	usage := &beadsapi.SessionUsage{InputTokens: 100, TotalCostUSD: 0.01}

	payload := buildPrometheusPayload(labels, usage, 60)

	if !strings.Contains(payload, `agent_name=""`) {
		t.Error("empty agent_name should produce empty quoted label")
	}
	// Tokens are still emitted; cost is not (see TestBuildPrometheusPayload).
	if !strings.Contains(payload, "gardenio_session_tokens_total") {
		t.Error("payload should contain token metrics")
	}
}

func TestFormatLabels(t *testing.T) {
	labels := sessionMetricsLabels{
		AgentName: "my-agent",
		Project:   "proj",
		Account:   "acct",
		SessionID: "sess-1",
	}

	got := formatLabels(labels)
	want := `agent_name="my-agent",project="proj",account="acct",session_id="sess-1"`
	if got != want {
		t.Errorf("formatLabels:\n  got:  %s\n  want: %s", got, want)
	}
}

func TestPushSessionMetrics_PostsToVmagent(t *testing.T) {
	var receivedBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/import/prometheus" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("unexpected method: %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "text/plain" {
			t.Errorf("unexpected content-type: %s", ct)
		}
		data, _ := io.ReadAll(r.Body)
		receivedBody = string(data)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	t.Setenv("VMAGENT_URL", srv.URL)

	labels := sessionMetricsLabels{
		AgentName: "test-agent",
		Project:   "gardenio",
		SessionID: "gd-123",
	}
	usage := &beadsapi.SessionUsage{
		InputTokens:  500,
		OutputTokens: 100,
		TotalCostUSD: 0.05,
		RequestCount: 2,
	}

	pushSessionMetrics(context.Background(), labels, usage, 120)

	if receivedBody == "" {
		t.Fatal("vmagent server received no body")
	}
	if !strings.Contains(receivedBody, "gardenio_session_tokens_total") {
		t.Error("body should contain token metrics")
	}
	if !strings.Contains(receivedBody, `agent_name="test-agent"`) {
		t.Error("body should contain agent_name label")
	}
}

func TestPushSessionMetrics_SkipsWhenNoURL(t *testing.T) {
	t.Setenv("VMAGENT_URL", "")

	// Should not panic or error — silently skips.
	pushSessionMetrics(context.Background(), sessionMetricsLabels{}, &beadsapi.SessionUsage{}, 0)
}

func TestPushSessionMetrics_SkipsNilUsage(t *testing.T) {
	t.Setenv("VMAGENT_URL", "http://localhost:9999")

	// Should not panic — silently skips nil usage.
	pushSessionMetrics(context.Background(), sessionMetricsLabels{}, nil, 0)
}

// --- operator_id metrics (ka-0000007818) ---

// TestBuildPrometheusPayload_OperatorMetrics asserts the operator-attributed
// metrics carry a non-empty operator_id + project + model, and crucially that
// session_id is NOT propagated to them (they are rolled up per operator, not
// per session).
func TestBuildPrometheusPayload_OperatorMetrics(t *testing.T) {
	labels := sessionMetricsLabels{
		AgentName:  "bridge-spawned-agent",
		Project:    "gardenio",
		Account:    "acme-corp",
		SessionID:  "gd-SESSION1",
		OperatorID: "42",
		Model:      "claude-opus-4-8",
	}
	usage := &beadsapi.SessionUsage{TotalCostUSD: 0.0425}

	payload := buildPrometheusPayload(labels, usage, 300)

	// Only the duration metric is emitted here. gardenio_operator_cost_usd_total is
	// now driven authoritatively controller-side (ka-0000000999) and must NOT
	// appear in the gb push.
	if want := `gardenio_operator_session_duration_seconds{operator_id="42",project="gardenio",model="claude-opus-4-8"} 300`; !strings.Contains(payload, want) {
		t.Errorf("payload missing expected operator line:\n  want: %s\n  payload:\n%s", want, payload)
	}
	if strings.Contains(payload, "gardenio_operator_cost_usd_total") {
		t.Errorf("gb push must not emit gardenio_operator_cost_usd_total (now controller-driven):\n%s", payload)
	}

	// operator_id must be present and non-empty on every operator metric.
	for _, l := range strings.Split(payload, "\n") {
		if strings.HasPrefix(l, "gardenio_operator_") {
			if !strings.Contains(l, `operator_id="`) || strings.Contains(l, `operator_id=""`) {
				t.Errorf("operator metric missing non-empty operator_id: %s", l)
			}
			// session_id must NOT leak onto the new counters.
			if strings.Contains(l, "session_id") {
				t.Errorf("operator metric must not carry session_id: %s", l)
			}
		}
	}
}

// TestBuildPrometheusPayload_OperatorSentinel asserts that an unresolved
// operator (empty OperatorID) is emitted as the "0" sentinel, never an empty
// label.
func TestBuildPrometheusPayload_OperatorSentinel(t *testing.T) {
	labels := sessionMetricsLabels{
		Project: "gardenio",
		Model:   "claude-opus-4-8",
		// OperatorID intentionally empty (NULL/legacy/S3-resume).
	}
	usage := &beadsapi.SessionUsage{TotalCostUSD: 0.01}

	payload := buildPrometheusPayload(labels, usage, 60)

	if !strings.Contains(payload, `gardenio_operator_session_duration_seconds{operator_id="0",project="gardenio",model="claude-opus-4-8"} 60`) {
		t.Errorf("expected operator_id=\"0\" sentinel on duration metric, got:\n%s", payload)
	}
	if strings.Contains(payload, `operator_id=""`) {
		t.Errorf("operator_id must never be an empty label, got:\n%s", payload)
	}
}

func TestFormatOperatorLabels(t *testing.T) {
	got := formatOperatorLabels(sessionMetricsLabels{OperatorID: "7", Project: "p", Model: "m", SessionID: "gd-x"})
	want := `operator_id="7",project="p",model="m"`
	if got != want {
		t.Errorf("formatOperatorLabels:\n  got:  %s\n  want: %s", got, want)
	}
	// Empty operator falls back to the sentinel.
	gotEmpty := formatOperatorLabels(sessionMetricsLabels{Project: "p", Model: "m"})
	wantEmpty := `operator_id="0",project="p",model="m"`
	if gotEmpty != wantEmpty {
		t.Errorf("formatOperatorLabels sentinel:\n  got:  %s\n  want: %s", gotEmpty, wantEmpty)
	}
}

// TestResolveOperatorID_BridgeSpawned simulates a bridge-spawned agent whose
// agent bead carries a non-NULL operated_by_id, and asserts the resolved
// operator_id is the stringified FK (non-empty, not the sentinel).
func TestResolveOperatorID_BridgeSpawned(t *testing.T) {
	var opID int64 = 42
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/beads/gd-AGENT1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             "gd-AGENT1",
			"operated_by_id": opID,
			"operated_by":    "slack:U0OPERATOR1",
		})
	}))
	defer srv.Close()

	prev := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = prev })

	t.Setenv("KD_AGENT_ID", "gd-AGENT1")

	got := resolveOperatorID(context.Background())
	if got != "42" {
		t.Errorf("resolveOperatorID = %q, want %q", got, "42")
	}
	if got == operatorIDSentinel || got == "" {
		t.Errorf("bridge-spawned operator_id must be non-empty and not the sentinel, got %q", got)
	}
}

// TestResolveOperatorID_Unattributed asserts the "0" sentinel for an agent
// bead with a NULL operated_by_id (legacy / S3-resume / anonymous).
func TestResolveOperatorID_Unattributed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// operated_by_id omitted -> NULL on the bead.
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "gd-AGENT2"})
	}))
	defer srv.Close()

	prev := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = prev })

	t.Setenv("KD_AGENT_ID", "gd-AGENT2")

	if got := resolveOperatorID(context.Background()); got != operatorIDSentinel {
		t.Errorf("resolveOperatorID (NULL operated_by_id) = %q, want sentinel %q", got, operatorIDSentinel)
	}
}

// TestResolveOperatorID_NoAgentBeadID asserts the sentinel when no agent bead
// id is present in the environment.
func TestResolveOperatorID_NoAgentBeadID(t *testing.T) {
	prev := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: "http://localhost:1"})
	t.Cleanup(func() { daemon = prev })

	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_AGENT_BEAD_ID", "")

	if got := resolveOperatorID(context.Background()); got != operatorIDSentinel {
		t.Errorf("resolveOperatorID (no agent bead id) = %q, want sentinel %q", got, operatorIDSentinel)
	}
}

// TestBuildPrometheusPayload_AccidentalEmailHashed is the W2 §6 acceptance
// (ka-0000007286): if a resolver regression ever stamped an OIDC email into
// operator_id (instead of the opaque FK), the producer-side catch-all must hash
// it before it is rendered into the push payload that reaches vmsingle. The
// payload must contain the h_ hashed marker and never the cleartext email.
func TestBuildPrometheusPayload_AccidentalEmailHashed(t *testing.T) {
	t.Setenv("OBS_REDACTION_KEY", "unit-test-key")

	labels := sessionMetricsLabels{
		Project:    "gardenio",
		OperatorID: "matthew.baker@acme.example", // accidental leak
		Model:      "claude-opus-4-8",
	}
	usage := &beadsapi.SessionUsage{TotalCostUSD: 0.01}

	payload := buildPrometheusPayload(labels, usage, 60)

	if strings.Contains(payload, "matthew.baker@acme.example") {
		t.Fatalf("operator email leaked into push payload:\n%s", payload)
	}
	for _, l := range strings.Split(payload, "\n") {
		if strings.HasPrefix(l, "gardenio_operator_") {
			if !strings.Contains(l, `operator_id="h_`) {
				t.Errorf("operator_id should be hashed (h_ prefix): %s", l)
			}
		}
	}
}

// TestSessionMetricsFlushLoop_PushesPeriodically covers ka-0000003327: while
// coop runs, the flush loop re-parses the session log and pushes metrics on
// every tick, so turn-count/context-size are real time series rather than one
// close-time sample.
func TestSessionMetricsFlushLoop_PushesPeriodically(t *testing.T) {
	pushes := make(chan string, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		select {
		case pushes <- string(data):
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	t.Setenv("VMAGENT_URL", srv.URL)

	// A one-turn session log: context level 100+800+50.
	f, err := os.CreateTemp(t.TempDir(), "session-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	line := `{"type":"assistant","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":800,"cache_creation_input_tokens":50}}}` + "\n"
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	f.Close()

	oldInterval := sessionMetricsFlushInterval
	sessionMetricsFlushInterval = 10 * time.Millisecond
	defer func() { sessionMetricsFlushInterval = oldInterval }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sessionMetricsFlushLoop(ctx, sessionMetricsLabels{
		AgentName: "flush-agent", Project: "gardenio", SessionID: "gd-FLUSH1",
	}, time.Now(), func() string { return f.Name() })

	select {
	case body := <-pushes:
		for _, want := range []string{
			`gardenio_session_turns_total{agent_name="flush-agent",project="gardenio",account="",session_id="gd-FLUSH1"} 1`,
			`gardenio_session_context_tokens{agent_name="flush-agent",project="gardenio",account="",session_id="gd-FLUSH1"} 950`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("flush body missing:\n  want: %s\n  body:\n%s", want, body)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("flush loop never pushed within 5s")
	}
}

// TestSessionMetricsFlushLoop_SkipsWithoutSessionID guards against emitting
// label-less series when session bead registration failed.
func TestSessionMetricsFlushLoop_SkipsWithoutSessionID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no push expected without a session id")
	}))
	defer srv.Close()
	t.Setenv("VMAGENT_URL", srv.URL)

	oldInterval := sessionMetricsFlushInterval
	sessionMetricsFlushInterval = 5 * time.Millisecond
	defer func() { sessionMetricsFlushInterval = oldInterval }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sessionMetricsFlushLoop(ctx, sessionMetricsLabels{AgentName: "a"}, time.Now(), func() string { return "" })
}
