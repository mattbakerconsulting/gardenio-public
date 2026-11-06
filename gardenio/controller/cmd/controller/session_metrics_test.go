package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/subscriber"
)

// beadServer returns an httptest server that serves a single agent bead at
// GET /v1/beads/<id> with the supplied created_at/closed_at timestamps.
func beadServer(t *testing.T, id, createdAt, closedAt string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/") {
			body := map[string]any{
				"id":         id,
				"type":       "agent",
				"status":     "closed",
				"created_at": createdAt,
				"closed_at":  closedAt,
				"fields":     map[string]string{"project": "acme", "role": "crew"},
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newBeadClient(t *testing.T, srv *httptest.Server) *beadsapi.Client {
	t.Helper()
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestEmitSessionCloseMetrics_DoneRecordsDurationAndLifecycle(t *testing.T) {
	srv := beadServer(t, "crew-acme-crew-alpha",
		"2026-06-05T00:00:00Z", "2026-06-05T00:05:00Z") // 300s session
	daemon := newBeadClient(t, srv)

	em := gardeniometrics.NewEventMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatal(err)
	}

	ev := subscriber.Event{
		Type:      subscriber.AgentDone,
		Project:   "acme",
		Role:      "crew",
		AgentName: "alpha",
		BeadID:    "crew-acme-crew-alpha",
	}
	emitSessionCloseMetrics(context.Background(), discardLogger(), daemon, em, ev, ev.BeadID, nil)

	// Lifecycle: AgentDone → "despawned".
	want := `
# HELP gardenio_agent_lifecycle_events_total Agent lifecycle transitions by kind (enum: spawned/started/stopped/oom_killed/stuck/drained/despawned). Labels {event,env,project,role,session_id}; session_id is populated only for oom_killed (FSM composite-alert drill-down, FSM_CORRELATION_CONTRACT §5a). env is the cross-estate join key.
# TYPE gardenio_agent_lifecycle_events_total counter
gardenio_agent_lifecycle_events_total{env="prod",event="despawned",project="acme",role="crew",session_id=""} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want),
		"gardenio_agent_lifecycle_events_total"); err != nil {
		t.Fatalf("lifecycle mismatch: %v", err)
	}

	// Histogram: one observation in the session-duration metric, sum == 300.
	if got := testutil.CollectAndCount(reg, "gardenio_agent_session_duration_seconds"); got == 0 {
		t.Fatal("expected session-duration series to be present")
	}
	sum := dumpSum(t, reg, "gardenio_agent_session_duration_seconds")
	if sum != 300 {
		t.Fatalf("expected session duration sum 300s, got %v", sum)
	}
}

func TestEmitSessionCloseMetrics_KillIsFailedOutcome(t *testing.T) {
	srv := beadServer(t, "crew-acme-crew-beta",
		"2026-06-05T00:00:00Z", "2026-06-05T00:00:30Z")
	daemon := newBeadClient(t, srv)
	em := gardeniometrics.NewEventMetrics("")
	reg := prometheus.NewRegistry()
	if err := em.Register(reg); err != nil {
		t.Fatal(err)
	}

	ev := subscriber.Event{
		Type:    subscriber.AgentKill,
		Project: "acme",
		Role:    "crew",
		BeadID:  "crew-acme-crew-beta",
	}
	emitSessionCloseMetrics(context.Background(), discardLogger(), daemon, em, ev, ev.BeadID, nil)

	// outcome="failed" must appear on the histogram label set.
	out := gatherText(t, reg)
	if !strings.Contains(out, `outcome="failed"`) {
		t.Fatalf("expected failed outcome in:\n%s", out)
	}
	if !strings.Contains(out, `event="despawned"`) {
		t.Fatalf("expected despawned lifecycle in:\n%s", out)
	}
}

func TestEmitSessionCloseMetrics_NilMetricsNoPanic(t *testing.T) {
	// Must not panic / dereference when metrics are absent.
	emitSessionCloseMetrics(context.Background(), discardLogger(), nil, nil,
		subscriber.Event{Type: subscriber.AgentDone}, "x", nil)
}

func dumpSum(t *testing.T, g prometheus.Gatherer, name string) float64 {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if h := m.GetHistogram(); h != nil {
				return h.GetSampleSum()
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

func gatherText(t *testing.T, g prometheus.Gatherer) string {
	t.Helper()
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			sb.WriteString(mf.GetName())
			for _, l := range m.GetLabel() {
				sb.WriteString(" ")
				sb.WriteString(l.GetName())
				sb.WriteString(`="`)
				sb.WriteString(l.GetValue())
				sb.WriteString(`"`)
			}
			sb.WriteString("\n")
		}
	}
	return sb.String()
}
