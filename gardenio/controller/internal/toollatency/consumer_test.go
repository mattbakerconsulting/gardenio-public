package toollatency

// consumer_test.go exercises the durable JetStream consumer end-to-end against
// an embedded NATS+JetStream server, mirroring the
// internal/assistantturnminter embedded-fixture convention
// (assistantturnminter_replay_test.go's startEmbeddedJetStreamNATS).

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	dto "github.com/prometheus/client_model/go"

	"github.com/prometheus/client_golang/prometheus"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/metrics"
)

// startEmbeddedHookEventsNATS spins up an in-process NATS+JetStream server and
// creates the HOOK_EVENTS stream the consumer subscribes to.
func startEmbeddedHookEventsNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1,
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded JetStream NATS not ready within 5s")
	}
	t.Cleanup(func() { ns.Shutdown() })

	url := ns.ClientURL()
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     hookEventsStreamName,
		Subjects: []string{"hooks.>"},
		Storage:  nats.FileStorage,
		MaxMsgs:  10000,
	}); err != nil {
		t.Fatalf("add stream: %v", err)
	}
	return url
}

// mockBeadAPI records every RecordSlowToolCall call for assertion. Safe for
// concurrent use since the JetStream delivery callback runs on its own
// goroutine.
type mockBeadAPI struct {
	mu   sync.Mutex
	rows []*beadsapi.SlowToolCallRow
}

func (m *mockBeadAPI) RecordSlowToolCall(_ context.Context, row *beadsapi.SlowToolCallRow) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, row)
	return nil
}

func (m *mockBeadAPI) rowCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.rows)
}

func (m *mockBeadAPI) lastRow() *beadsapi.SlowToolCallRow {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.rows) == 0 {
		return nil
	}
	return m.rows[len(m.rows)-1]
}

func publishHookEvent(t *testing.T, conn *nats.Conn, agent, event string, evt hookEventWire) {
	t.Helper()
	evt.Event = event
	data, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	subject := "hooks." + agent + "." + event
	if err := conn.Publish(subject, data); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

// waitFor polls cond up to a deadline rather than sleeping a fixed duration,
// since JetStream delivery to the consumer callback is asynchronous.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

// metricSampleCount sums the histogram sample count across every series of
// metricName in reg whose labels match wantLabels (a subset match — only the
// named labels are checked).
func metricSampleCount(t *testing.T, reg *prometheus.Registry, metricName string, wantLabels map[string]string) uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total uint64
	for _, mf := range mfs {
		if mf.GetName() != metricName {
			continue
		}
		for _, m := range mf.GetMetric() {
			if labelsMatch(m, wantLabels) {
				total += m.GetHistogram().GetSampleCount()
			}
		}
	}
	return total
}

func labelsMatch(m *dto.Metric, want map[string]string) bool {
	got := make(map[string]string, len(m.GetLabel()))
	for _, lp := range m.GetLabel() {
		got[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

func newTestConsumer(t *testing.T, url string, daemon BeadAPI, threshold time.Duration) (*Consumer, *prometheus.Registry, *metrics.ToolLatencyMetrics) {
	t.Helper()
	tm := metrics.NewToolLatencyMetrics("test")
	reg := prometheus.NewRegistry()
	if err := tm.Register(reg); err != nil {
		t.Fatalf("register metrics: %v", err)
	}
	c := New(url, "", "", daemon, tm, threshold, nil)
	if c == nil {
		t.Fatal("New returned nil consumer")
	}
	if err := c.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(c.Close)
	return c, reg, tm
}

func TestConsumer_NativeDurationClaudeSource(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon := &mockBeadAPI{}
	_, reg, _ := newTestConsumer(t, url, daemon, time.Second)

	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		AgentBeadID:    "gd-abc123",
		SessionID:      "sess-1",
		Project:        "gardenio",
		ToolName:       "Read",
		ToolUseID:      "tu-1",
		DurationMs:     500,
		DurationSource: "claude",
	})

	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg, "gardenio_tool_latency_seconds", map[string]string{
			"tool": "Read", "project": "gardenio", "source": "claude",
		}) == 1
	})
	if got := daemon.rowCount(); got != 0 {
		t.Errorf("rowCount = %d, want 0 (below threshold)", got)
	}
}

func TestConsumer_HarnessLabelFromEvent(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon := &mockBeadAPI{}
	_, reg, _ := newTestConsumer(t, url, daemon, 10*time.Second)

	publishHookEvent(t, conn, "agent-codex", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		Project:        "gardenio",
		Harness:        "codex",
		ToolName:       "Bash",
		ToolUseID:      "tu-codex",
		DurationMs:     200,
		DurationSource: "claude",
		ToolInput:      map[string]any{"command": "git push origin main"},
	})

	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg, "gardenio_tool_latency_seconds", map[string]string{
			"tool": "Bash", "project": "gardenio", "harness": "codex",
		}) == 1 && metricSampleCount(t, reg, "gardenio_bash_cmd_latency_seconds", map[string]string{
			"cmd_class": "git_push", "harness": "codex",
		}) == 1
	})
}

func TestConsumer_DerivedDurationFromPairing(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon := &mockBeadAPI{}
	_, reg, _ := newTestConsumer(t, url, daemon, time.Second)

	preTS := time.Now().UTC()
	postTS := preTS.Add(300 * time.Millisecond)

	publishHookEvent(t, conn, "agent-1", "PreToolUse", hookEventWire{
		TS:        preTS.Format(time.RFC3339Nano),
		ToolName:  "Edit",
		ToolUseID: "tu-2",
	})
	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:        postTS.Format(time.RFC3339Nano),
		ToolName:  "Edit",
		ToolUseID: "tu-2",
		Project:   "gardenio",
	})

	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg, "gardenio_tool_latency_seconds", map[string]string{
			"tool": "Edit", "project": "gardenio", "source": "derived",
		}) == 1
	})
}

func TestConsumer_BashEmitsCmdClassSample(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon := &mockBeadAPI{}
	_, reg, _ := newTestConsumer(t, url, daemon, 10*time.Second)

	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		Project:        "gardenio",
		ToolName:       "Bash",
		ToolUseID:      "tu-3",
		DurationMs:     200,
		DurationSource: "claude",
		ToolInput:      map[string]any{"command": "git push origin main"},
	})

	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg, "gardenio_bash_cmd_latency_seconds", map[string]string{
			"cmd_class": "git_push", "project": "gardenio", "source": "claude",
		}) == 1
	})
}

func TestConsumer_SlowCallWritesRedactedLedgerRow(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon := &mockBeadAPI{}
	newTestConsumer(t, url, daemon, 1*time.Second)

	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		AgentBeadID:    "gd-slow1",
		SessionID:      "sess-9",
		Project:        "gardenio",
		ToolName:       "Bash",
		ToolUseID:      "tu-4",
		DurationMs:     3000,
		DurationSource: "claude",
		ToolInput: map[string]any{
			"command": "curl -H 'Authorization: Bearer sk-ABCDEFGHIJKLMNOP' https://example.com", // gitleaks:allow: fake token for the redaction test
		},
	})

	waitFor(t, 2*time.Second, func() bool { return daemon.rowCount() == 1 })

	row := daemon.lastRow()
	if row == nil {
		t.Fatal("expected one ledger row")
	}
	if row.AgentBeadID != "gd-slow1" || row.SessionID != "sess-9" || row.ToolName != "Bash" {
		t.Errorf("unexpected row identity fields: %+v", row)
	}
	if row.DurationMs != 3000 {
		t.Errorf("DurationMs = %d, want 3000", row.DurationMs)
	}
	if strings.Contains(row.CommandRedacted, "sk-ABCDEFGHIJKLMNOP") {
		t.Errorf("CommandRedacted leaked the secret: %q", row.CommandRedacted)
	}
	if strings.Contains(string(row.ToolInputRedacted), "sk-ABCDEFGHIJKLMNOP") {
		t.Errorf("ToolInputRedacted leaked the secret: %s", row.ToolInputRedacted)
	}
}

func TestConsumer_BelowThresholdDoesNotWriteLedgerRow(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon := &mockBeadAPI{}
	_, reg, _ := newTestConsumer(t, url, daemon, 2*time.Second)

	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		Project:        "gardenio",
		ToolName:       "Read",
		ToolUseID:      "tu-5",
		DurationMs:     100,
		DurationSource: "claude",
	})

	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg, "gardenio_tool_latency_seconds", map[string]string{
			"tool": "Read",
		}) == 1
	})
	if got := daemon.rowCount(); got != 0 {
		t.Errorf("rowCount = %d, want 0 (below threshold)", got)
	}
}

// TestConsumer_DurableRebindResumesWithoutLoss simulates a controller restart:
// Start, publish one event, Close, then Start a FRESH Consumer bound to the
// same durable name. Because the durable consumer already acked the first
// message, the second consumer's live subscription must not redeliver it; a
// second event published only after the rebind must still be delivered,
// proving the rebind resumed delivery rather than losing the stream.
func TestConsumer_DurableRebindResumesWithoutLoss(t *testing.T) {
	url := startEmbeddedHookEventsNATS(t)
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close()

	daemon1 := &mockBeadAPI{}
	c1, reg1, _ := newTestConsumer(t, url, daemon1, time.Second)

	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		Project:        "gardenio",
		ToolName:       "Read",
		ToolUseID:      "tu-6",
		DurationMs:     100,
		DurationSource: "claude",
	})
	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg1, "gardenio_tool_latency_seconds", map[string]string{"tool": "Read"}) == 1
	})
	c1.Close()

	// Close must leave the durable in place: a consumer nats.go created itself
	// is deleted on drain, and the rebind would then replay from DeliverAll.
	js, err := conn.JetStream()
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if _, err := js.ConsumerInfo(hookEventsStreamName, consumerDurableName); err != nil {
		t.Fatalf("durable consumer gone after Close: %v", err)
	}
	// The server drops c1's push binding asynchronously; rebinding before
	// that fails with "consumer is already bound to a subscription".
	waitFor(t, 2*time.Second, func() bool {
		info, err := js.ConsumerInfo(hookEventsStreamName, consumerDurableName)
		return err == nil && !info.PushBound
	})

	daemon2 := &mockBeadAPI{}
	tm2 := metrics.NewToolLatencyMetrics("test")
	reg2 := prometheus.NewRegistry()
	if err := tm2.Register(reg2); err != nil {
		t.Fatalf("register: %v", err)
	}
	c2 := New(url, "", "", daemon2, tm2, time.Second, nil)
	if err := c2.Start(); err != nil {
		t.Fatalf("Start (rebind): %v", err)
	}
	defer c2.Close()

	publishHookEvent(t, conn, "agent-1", "PostToolUse", hookEventWire{
		TS:             time.Now().UTC().Format(time.RFC3339Nano),
		Project:        "gardenio",
		ToolName:       "Write",
		ToolUseID:      "tu-7",
		DurationMs:     150,
		DurationSource: "claude",
	})

	waitFor(t, 2*time.Second, func() bool {
		return metricSampleCount(t, reg2, "gardenio_tool_latency_seconds", map[string]string{"tool": "Write"}) == 1
	})
	// The first (already-acked) call must NOT have been redelivered to the
	// rebound consumer.
	if got := metricSampleCount(t, reg2, "gardenio_tool_latency_seconds", map[string]string{"tool": "Read"}); got != 0 {
		t.Errorf("redelivered an already-acked message after rebind: Read sample count = %d", got)
	}
}

func TestNew_EmptyURLReturnsNil(t *testing.T) {
	if c := New("", "", "", &mockBeadAPI{}, metrics.NewToolLatencyMetrics("test"), time.Second, nil); c != nil {
		t.Error("New with empty natsURL should return nil")
	}
}

func TestConsumer_NilSafe(t *testing.T) {
	var c *Consumer
	if err := c.Start(); err != nil {
		t.Errorf("nil Start should be a no-op, got %v", err)
	}
	c.Close() // must not panic
}
