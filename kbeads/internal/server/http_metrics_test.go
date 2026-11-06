package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

func TestMetricsEndpoint(t *testing.T) {
	ms := newMockStore()

	// Add agent beads with cost/session fields.
	ms.beads["agent-1"] = &model.Bead{
		ID:     "agent-1",
		Type:   "agent",
		Kind:   model.KindConfig,
		Status: model.StatusOpen,
		Fields: json.RawMessage(`{"total_cost_usd":"12.50","session_count":"3"}`),
	}
	ms.labels["agent-1"] = []string{"project:gardenio", "role:crew"}

	ms.beads["agent-2"] = &model.Bead{
		ID:     "agent-2",
		Type:   "agent",
		Kind:   model.KindConfig,
		Status: model.StatusInProgress,
		Fields: json.RawMessage(`{"total_cost_usd":"7.25","session_count":"1"}`),
	}
	ms.labels["agent-2"] = []string{"project:gardenio"}

	ms.beads["agent-3"] = &model.Bead{
		ID:     "agent-3",
		Type:   "agent",
		Kind:   model.KindConfig,
		Status: model.StatusClosed,
		Fields: json.RawMessage(`{"total_cost_usd":"5.00","session_count":"2"}`),
	}
	ms.labels["agent-3"] = []string{"project:other"}

	// Add non-agent beads for type/status breakdown.
	ms.beads["task-1"] = &model.Bead{
		ID:     "task-1",
		Type:   model.TypeTask,
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
	}
	ms.beads["task-2"] = &model.Bead{
		ID:     "task-2",
		Type:   model.TypeTask,
		Kind:   model.KindIssue,
		Status: model.StatusInProgress,
	}
	ms.beads["bug-1"] = &model.Bead{
		ID:     "bug-1",
		Type:   model.TypeBug,
		Kind:   model.KindIssue,
		Status: model.StatusOpen,
	}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	handler := srv.MetricsHandler(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	// Verify fleet cost metric is present.
	if !strings.Contains(body, "gardenio_beads_total_cost_usd") {
		t.Error("missing gardenio_beads_total_cost_usd metric")
	}

	// Verify active agents metric is present.
	if !strings.Contains(body, "gardenio_beads_active_agents") {
		t.Error("missing gardenio_beads_active_agents metric")
	}

	// Verify sessions metric is present.
	if !strings.Contains(body, "gardenio_beads_sessions_total") {
		t.Error("missing gardenio_beads_sessions_total metric")
	}

	// Verify beads total metric is present.
	if !strings.Contains(body, "gardenio_beads_total") {
		t.Error("missing gardenio_beads_total metric")
	}

	// Verify specific label combinations exist.
	if !strings.Contains(body, `project="gardenio"`) {
		t.Error("missing project=gardenio label")
	}
	if !strings.Contains(body, `type="task"`) {
		t.Error("missing type=task label")
	}

	// The audit-retention sweep's counters are on the registry (gd-j4wScilGlk).
	for _, name := range []string{
		"gardenio_audit_retention_closed_total",
		"gardenio_audit_retention_deleted_total",
	} {
		if !strings.Contains(body, "# TYPE "+name+" counter") {
			t.Errorf("missing %s metric", name)
		}
	}
}

// TestMetricsEmittedBeforeFirstUsage verifies that cost and sessions metrics
// appear in /metrics output even when agent beads exist but have not yet
// recorded any usage (no total_cost_usd / session_count fields). Without
// baseline zero entries, Prometheus collectors omit HELP/TYPE lines entirely
// when no samples are emitted — which breaks scrapers that key off metric
// name presence (see gd-RmeGMXqDMB).
func TestMetricsEmittedBeforeFirstUsage(t *testing.T) {
	ms := newMockStore()

	// Agent bead with no usage fields — simulates a fresh agent that hasn't
	// closed any session yet.
	ms.beads["agent-new"] = &model.Bead{
		ID:     "agent-new",
		Type:   "agent",
		Kind:   model.KindConfig,
		Status: model.StatusOpen,
	}
	ms.labels["agent-new"] = []string{"project:gardenio"}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	handler := srv.MetricsHandler(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE gardenio_beads_total_cost_usd gauge",
		"# TYPE gardenio_beads_sessions_total counter",
		"# TYPE gardenio_beads_active_agents gauge",
		`gardenio_beads_total_cost_usd{project="gardenio"} 0`,
		`gardenio_beads_sessions_total{project="gardenio"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing expected line %q in body:\n%s", want, body)
		}
	}
}

// TestQueueDepthGauge verifies the gardenio_beads_open_queue_depth gauge is
// emitted per (project, type) for OPEN operator-facing queue beads only —
// closed queue beads and non-queue types are excluded (ka-0000002235).
func TestQueueDepthGauge(t *testing.T) {
	ms := newMockStore()

	// Two open decisions and one open escalation in gardenio.
	ms.beads["dec-1"] = &model.Bead{ID: "dec-1", Type: "decision", Kind: model.KindIssue, Status: model.StatusOpen}
	ms.labels["dec-1"] = []string{"project:gardenio"}
	ms.beads["dec-2"] = &model.Bead{ID: "dec-2", Type: "decision", Kind: model.KindIssue, Status: model.StatusOpen}
	ms.labels["dec-2"] = []string{"project:gardenio"}
	ms.beads["esc-1"] = &model.Bead{ID: "esc-1", Type: "escalation", Kind: model.KindIssue, Status: model.StatusOpen}
	ms.labels["esc-1"] = []string{"project:gardenio"}

	// A CLOSED decision must NOT count toward depth.
	ms.beads["dec-closed"] = &model.Bead{ID: "dec-closed", Type: "decision", Kind: model.KindIssue, Status: model.StatusClosed}
	ms.labels["dec-closed"] = []string{"project:gardenio"}

	// An open mail bead with no project label → "unknown".
	ms.beads["mail-1"] = &model.Bead{ID: "mail-1", Type: "mail", Kind: model.KindIssue, Status: model.StatusOpen}

	// A non-queue open bead must NOT appear in the queue-depth gauge.
	ms.beads["task-1"] = &model.Bead{ID: "task-1", Type: model.TypeTask, Kind: model.KindIssue, Status: model.StatusOpen}
	ms.labels["task-1"] = []string{"project:gardenio"}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	handler := srv.MetricsHandler(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	for _, want := range []string{
		"# TYPE gardenio_beads_open_queue_depth gauge",
		`gardenio_beads_open_queue_depth{project="gardenio",type="decision"} 2`,
		`gardenio_beads_open_queue_depth{project="gardenio",type="escalation"} 1`,
		`gardenio_beads_open_queue_depth{project="unknown",type="mail"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing expected line %q in body:\n%s", want, body)
		}
	}

	// The closed decision must not inflate the gardenio/decision depth to 3.
	if strings.Contains(body, `gardenio_beads_open_queue_depth{project="gardenio",type="decision"} 3`) {
		t.Error("closed decision incorrectly counted toward queue depth")
	}
	// A non-queue type (task) must never carry the queue-depth gauge.
	if strings.Contains(body, `gardenio_beads_open_queue_depth{project="gardenio",type="task"}`) {
		t.Error("non-queue type task appeared in queue-depth gauge")
	}
}

// TestQueueOldestAgeGauge verifies the gardenio_beads_open_queue_oldest_age_seconds
// gauge reports the age of the OLDEST open queue bead per (project, type), that
// closed/non-queue beads are excluded, and that an empty queue emits no series
// (rescope ka-0000003130).
func TestQueueOldestAgeGauge(t *testing.T) {
	ms := newMockStore()

	now := time.Now()
	// Two open decisions in gardenio: the oldest is 2h old, so the bucket's
	// oldest-age must reflect the 2h item, not the 10m one.
	ms.beads["dec-old"] = &model.Bead{ID: "dec-old", Type: "decision", Kind: model.KindIssue, Status: model.StatusOpen, CreatedAt: now.Add(-2 * time.Hour)}
	ms.labels["dec-old"] = []string{"project:gardenio"}
	ms.beads["dec-new"] = &model.Bead{ID: "dec-new", Type: "decision", Kind: model.KindIssue, Status: model.StatusOpen, CreatedAt: now.Add(-10 * time.Minute)}
	ms.labels["dec-new"] = []string{"project:gardenio"}

	// A CLOSED decision, even if far older, must NOT influence the oldest-age.
	ms.beads["dec-closed"] = &model.Bead{ID: "dec-closed", Type: "decision", Kind: model.KindIssue, Status: model.StatusClosed, CreatedAt: now.Add(-100 * time.Hour)}
	ms.labels["dec-closed"] = []string{"project:gardenio"}

	// A non-queue open bead must never carry this gauge.
	ms.beads["task-1"] = &model.Bead{ID: "task-1", Type: model.TypeTask, Kind: model.KindIssue, Status: model.StatusOpen, CreatedAt: now.Add(-5 * time.Hour)}
	ms.labels["task-1"] = []string{"project:gardenio"}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	handler := srv.MetricsHandler(t.Context())

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "# TYPE gardenio_beads_open_queue_oldest_age_seconds gauge") {
		t.Errorf("missing TYPE line for oldest-age gauge in body:\n%s", body)
	}

	// A non-queue type (task) must never carry this gauge.
	if strings.Contains(body, `gardenio_beads_open_queue_oldest_age_seconds{project="gardenio",type="task"}`) {
		t.Error("non-queue type task appeared in oldest-age gauge")
	}

	// The gardenio/decision bucket's oldest age must reflect the 2h item
	// (~7200s), never the 10m item and never the 100h closed one. Parse the
	// emitted value and bound-check it to avoid asserting an exact float.
	got, ok := parseGaugeValue(body, `gardenio_beads_open_queue_oldest_age_seconds{project="gardenio",type="decision"}`)
	if !ok {
		t.Fatalf("missing gardenio/decision oldest-age series in body:\n%s", body)
	}
	if got < 7200 || got > 7200+300 {
		t.Errorf("gardenio/decision oldest-age = %v, want ~7200s (2h, from the oldest OPEN decision)", got)
	}
}

// parseGaugeValue returns the float value of the first sample line whose text
// begins with prefix (a full metric name + labels), and whether it was found.
func parseGaugeValue(body, prefix string) (float64, bool) {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, false
		}
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

func TestMetricsNotOnMainHandler(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	handler := srv.NewHTTPHandler(nil, withTestToken(nil, "secret-token"))

	// /metrics must NOT be served on the main HTTP handler (it lives on a
	// separate internal-only metrics port).
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("expected /metrics to be absent from main handler, got 200")
	}
}

func TestMetricsCache(t *testing.T) {
	ms := newMockStore()
	ms.beads["agent-1"] = &model.Bead{
		ID:     "agent-1",
		Type:   "agent",
		Kind:   model.KindConfig,
		Status: model.StatusOpen,
		Fields: json.RawMessage(`{"total_cost_usd":"1.00","session_count":"1"}`),
	}
	ms.labels["agent-1"] = []string{"project:test"}

	collector := newFleetCollector(ms, 5*time.Second, time.Minute)

	// Synchronous initial refresh populates the cache (off the scrape path).
	collector.refreshLifetime(context.Background())
	life1, _ := collector.currentLifetime()
	if life1 == nil {
		t.Fatal("expected non-nil lifetime snapshot")
	}
	if life1.costByProject["test"] != 1.0 {
		t.Errorf("expected cost 1.0, got %f", life1.costByProject["test"])
	}

	// Mutate underlying data.
	ms.beads["agent-1"].Fields = json.RawMessage(`{"total_cost_usd":"99.00","session_count":"1"}`)

	// A scrape (Collect) must NOT re-query the store — it serves the cached
	// snapshot only. So the mutated value must not appear until a refresh runs.
	life2, _ := collector.currentLifetime()
	if life2.costByProject["test"] != 1.0 {
		t.Errorf("expected cached cost 1.0, got %f (Collect must not query the store)", life2.costByProject["test"])
	}

	// The fast refresh leaves the lifetime rollup alone (it has its own
	// cadence, gd-ipZ10t3oTo).
	collector.refresh(context.Background())
	life3, _ := collector.currentLifetime()
	if life3.costByProject["test"] != 1.0 {
		t.Errorf("fast refresh changed lifetime cost to %f", life3.costByProject["test"])
	}
	if snap, _ := collector.currentSnapshot(); snap == nil || snap.activeAgentsByProject["test"] != 1 {
		t.Errorf("expected 1 active agent in project test, got %+v", snap)
	}

	// After a lifetime refresh the new value is picked up.
	collector.refreshLifetime(context.Background())
	life4, _ := collector.currentLifetime()
	if life4.costByProject["test"] != 99.0 {
		t.Errorf("expected refreshed cost 99.0, got %f", life4.costByProject["test"])
	}
}

// TestActiveAgentsZeroForClosedOnlyProject: a project whose agents are all
// closed still reads gardenio_beads_active_agents 0, as it did when active
// counts came from the lifetime rollup (gd-ipZ10t3oTo).
func TestActiveAgentsZeroForClosedOnlyProject(t *testing.T) {
	ms := newMockStore()
	ms.beads["agent-closed"] = &model.Bead{
		ID:     "agent-closed",
		Type:   "agent",
		Kind:   model.KindConfig,
		Status: model.StatusClosed,
	}
	ms.labels["agent-closed"] = []string{"project:other"}

	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	handler := srv.MetricsHandler(t.Context())
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rec.Body.String()
	for _, want := range []string{
		`gardenio_beads_active_agents{project="other"} 0`,
		"# TYPE gardenio_beads_fleet_lifetime_metrics_staleness_seconds gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing expected line %q in body:\n%s", want, body)
		}
	}
}

func TestParseFloat(t *testing.T) {
	tests := []struct {
		name  string
		input map[string]any
		key   string
		want  float64
		ok    bool
	}{
		{"string value", map[string]any{"cost": "12.50"}, "cost", 12.50, true},
		{"float value", map[string]any{"cost": 7.25}, "cost", 7.25, true},
		{"missing key", map[string]any{}, "cost", 0, false},
		{"invalid string", map[string]any{"cost": "abc"}, "cost", 0, false},
		{"nil map", nil, "cost", 0, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseFloat(tt.input, tt.key)
			if ok != tt.ok {
				t.Errorf("ok = %v, want %v", ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("got %f, want %f", got, tt.want)
			}
		})
	}
}

func TestProjectFromLabels(t *testing.T) {
	tests := []struct {
		labels []string
		want   string
	}{
		{[]string{"project:gardenio", "role:crew"}, "gardenio"},
		{[]string{"role:crew"}, "unknown"},
		{nil, "unknown"},
		{[]string{"project:"}, "unknown"},
	}

	for _, tt := range tests {
		got := projectFromLabels(tt.labels)
		if got != tt.want {
			t.Errorf("projectFromLabels(%v) = %q, want %q", tt.labels, got, tt.want)
		}
	}
}

// parseFloat extracts a numeric value from a map field, handling both
// string and numeric JSON types.
func parseFloat(fields map[string]any, key string) (float64, bool) {
	v, ok := fields[key]
	if !ok {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return val, true
	case string:
		f, err := strconv.ParseFloat(val, 64)
		return f, err == nil
	case json.Number:
		f, err := val.Float64()
		return f, err == nil
	}
	return 0, false
}
