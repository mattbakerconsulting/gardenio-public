package main

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/spendlogs"
)

// slTestNow is the fixed "now" every sweep test runs at; the projection window
// is then [slTestNow-lookback-flushLag, slTestNow-flushLag).
var slTestNow = time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)

// slDaemon stubs the beads daemon for the sweep: GET /v1/beads honoring
// ?type / ?created_after / ?limit / ?offset, and PATCH /v1/beads/{id}
// recording every typed field write for assertions.
type slDaemon struct {
	mu      sync.Mutex
	beads   []map[string]any
	patches map[string][]map[string]any // bead id → recorded PATCH field payloads
	// listQueries records the query params of every GET /v1/beads list request
	// so tests can assert on the WALK SHAPE (page limit, advancing offsets,
	// sort) — the memory profile is what the paging fix guards, and a
	// correct-looking result can still come from one unbounded fetch.
	listQueries []slListQuery
}

// slListQuery is the subset of GET /v1/beads params the paging regression cares
// about.
type slListQuery struct {
	typ    string
	limit  int
	offset int
	sort   string
}

// agentListQueries returns only the type=agent list requests — the tracked-bead
// walk — filtering out the account-bead enumeration ProxyURLs also issues.
func (d *slDaemon) agentListQueries() []slListQuery {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []slListQuery
	for _, q := range d.listQueries {
		if q.typ == "agent" {
			out = append(out, q)
		}
	}
	return out
}

func (d *slDaemon) server(t *testing.T) *httptest.Server {
	t.Helper()
	d.patches = map[string][]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/beads":
			d.mu.Lock()
			defer d.mu.Unlock()
			q := r.URL.Query()
			var createdAfter time.Time
			if v := q.Get("created_after"); v != "" {
				createdAfter, _ = time.Parse(time.RFC3339, v)
			}
			var matched []map[string]any
			for _, b := range d.beads {
				if wt := q.Get("type"); wt != "" && b["type"] != wt {
					continue
				}
				if !createdAfter.IsZero() {
					created, err := time.Parse(time.RFC3339, b["created_at"].(string))
					if err != nil || created.Before(createdAfter) {
						continue
					}
				}
				matched = append(matched, b)
			}
			offset, _ := strconv.Atoi(q.Get("offset"))
			limit, _ := strconv.Atoi(q.Get("limit"))
			d.listQueries = append(d.listQueries, slListQuery{typ: q.Get("type"), limit: limit, offset: offset, sort: q.Get("sort")})
			if offset > len(matched) {
				offset = len(matched)
			}
			page := matched[offset:]
			if limit > 0 && len(page) > limit {
				page = page[:limit]
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"beads": page, "total": len(matched)})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			// UpdateBeadFieldsTyped reads the bead before patching (partial
			// patch + no-op suppression, beadsapi/client.go).
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			d.mu.Lock()
			defer d.mu.Unlock()
			for _, b := range d.beads {
				if b["id"] == id {
					_ = json.NewEncoder(w).Encode(b)
					return
				}
			}
			http.NotFound(w, r)
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/v1/beads/"):
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			var body struct {
				Fields map[string]any `json:"fields"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			d.mu.Lock()
			d.patches[id] = append(d.patches[id], body.Fields)
			d.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (d *slDaemon) patchCount(id string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.patches[id])
}

// setBeadField mutates a fixture bead's field, simulating the daemon having
// persisted a prior sweep's write (the sweep re-reads beads each cycle).
func (d *slDaemon) setBeadField(id, key, value string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, b := range d.beads {
		if b["id"] == id {
			b["fields"].(map[string]any)[key] = value
		}
	}
}

func slAgentBead(id, agent, project, role string, createdAt time.Time) map[string]any {
	return map[string]any{
		"id":         id,
		"type":       "agent",
		"status":     "open",
		"created_at": createdAt.Format(time.RFC3339),
		"fields":     map[string]any{"agent": agent, "project": project, "role": role},
	}
}

// slLiteLLM stubs one LiteLLM proxy's /spend/logs/v2 with a fixed row set,
// served as a single page. failAll switches every response to a 500 so tests
// can exercise the keep-previous-series path.
type slLiteLLM struct {
	mu      sync.Mutex
	rows    []spendlogs.Row
	failAll bool
}

func (l *slLiteLLM) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.failAll {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		if r.URL.Path != "/spend/logs/v2" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": l.rows, "page": 1, "page_size": len(l.rows), "total": len(l.rows), "total_pages": 1,
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (l *slLiteLLM) setFail(fail bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.failAll = fail
}

func slRow(id, agent string, spend float64, at time.Time) spendlogs.Row {
	return spendlogs.Row{
		RequestID:   id,
		Spend:       spend,
		StartTime:   at.Format(time.RFC3339),
		RequestTags: []string{"agent:" + agent, "role:crew", "project:gardenio"},
	}
}

// newTestSpendLogsSweep wires a sweep against the two stubs with a pinned
// clock and a registered metrics sink.
func newTestSpendLogsSweep(t *testing.T, daemon *slDaemon, litellm *slLiteLLM) (*spendLogsSweep, *gardeniometrics.SpendMetrics, *prometheus.Registry) {
	t.Helper()
	dsrv := daemon.server(t)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: dsrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	lsrv := litellm.server(t)
	slClient := spendlogs.New("sk-test", slog.Default())
	slClient.Throttle = 0

	metrics := gardeniometrics.NewSpendMetrics()
	reg := prometheus.NewRegistry()
	if err := metrics.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}
	sweep := newSpendLogsSweep(client, slClient, lsrv.URL, metrics, 0, 0, slog.Default())
	sweep.now = func() time.Time { return slTestNow }
	return sweep, metrics, reg
}

func spendLogsGaugeValue(t *testing.T, m *gardeniometrics.SpendMetrics, agent, project, role, model, account string) float64 {
	t.Helper()
	return testutil.ToFloat64(m.SpendLogsCostGauge().WithLabelValues(agent, project, role, model, account))
}

func TestSpendLogsSweepPublishesGaugeAndWritesField(t *testing.T) {
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
		slAgentBead("gd-b", "agent-b", "monorepo", "gardener", slTestNow.Add(-3*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{
		slRow("r1", "agent-a", 1.25, inWindow),
		slRow("r2", "agent-a", 0.75, inWindow.Add(time.Minute)),
		slRow("r3", "agent-b", 5.00, inWindow),
	}}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)
	sweep.resolveLabels = func(project, role string) (string, string) {
		return "claude-test", "acct-" + project
	}

	sweep.syncOnce(t.Context())

	if got := spendLogsGaugeValue(t, metrics, "agent-a", "gardenio", "crew", "claude-test", "acct-gardenio"); got != 2.0 {
		t.Errorf("agent-a gauge = %v, want 2.0", got)
	}
	if got := spendLogsGaugeValue(t, metrics, "agent-b", "monorepo", "gardener", "claude-test", "acct-monorepo"); got != 5.0 {
		t.Errorf("agent-b gauge = %v, want 5.0", got)
	}
	for id, want := range map[string]float64{"gd-a": 2.0, "gd-b": 5.0} {
		daemon.mu.Lock()
		patches := daemon.patches[id]
		daemon.mu.Unlock()
		if len(patches) != 1 {
			t.Fatalf("bead %s: %d PATCHes, want 1", id, len(patches))
		}
		got, ok := patches[0][spendLogsCostField].(float64)
		if !ok || got != want {
			t.Errorf("bead %s field write = %v, want %v", id, patches[0][spendLogsCostField], want)
		}
	}
}

func TestSpendLogsSweepSkipsNoOpFieldWrite(t *testing.T) {
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{slRow("r1", "agent-a", 2.5, inWindow)}}
	sweep, _, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())
	if n := daemon.patchCount("gd-a"); n != 1 {
		t.Fatalf("first sweep: %d PATCHes, want 1", n)
	}
	// Simulate the daemon having persisted the write; an unchanged recompute
	// must not re-PATCH.
	daemon.setBeadField("gd-a", spendLogsCostField, "2.5")
	sweep.syncOnce(t.Context())
	if n := daemon.patchCount("gd-a"); n != 1 {
		t.Errorf("second sweep with unchanged value: %d PATCHes, want still 1", n)
	}
	// New spend moves the figure → one more write.
	litellm.mu.Lock()
	litellm.rows = append(litellm.rows, slRow("r2", "agent-a", 0.5, inWindow.Add(2*time.Minute)))
	litellm.mu.Unlock()
	sweep.syncOnce(t.Context())
	if n := daemon.patchCount("gd-a"); n != 2 {
		t.Errorf("third sweep with new spend: %d PATCHes, want 2", n)
	}
}

func TestSpendLogsSweepSkipsUncoveredAndRowlessAgents(t *testing.T) {
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		// Created BEFORE the window start → partial coverage → skipped even
		// though it has in-window rows (the daemon-side created_after filter
		// already excludes it; the fixture proves the stub honors it).
		slAgentBead("gd-old", "agent-old", "gardenio", "crew", slTestNow.Add(-100*time.Hour)),
		// Covered but no rows yet → no series, no field write.
		slAgentBead("gd-idle", "agent-idle", "gardenio", "crew", slTestNow.Add(-10*time.Minute)),
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{
		slRow("r1", "agent-old", 9.99, inWindow),
		slRow("r2", "agent-a", 1.0, inWindow),
	}}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())

	if n := testutil.CollectAndCount(metrics.SpendLogsCostGauge()); n != 1 {
		t.Errorf("gauge series = %d, want 1 (only the covered, spending agent)", n)
	}
	if got := spendLogsGaugeValue(t, metrics, "agent-a", "gardenio", "crew", "", ""); got != 1.0 {
		t.Errorf("agent-a gauge = %v, want 1.0", got)
	}
	for _, id := range []string{"gd-old", "gd-idle"} {
		if n := daemon.patchCount(id); n != 0 {
			t.Errorf("bead %s: %d PATCHes, want 0", id, n)
		}
	}
}

func TestSpendLogsSweepSkipsSharedAgentNames(t *testing.T) {
	// Two tracked beads under ONE agent name (observed live: a respawn
	// pattern re-minting the bead, keeping the pod name). The per-name row
	// sum cannot be split between them, so booking it to each would
	// double-count — both must be skipped, without disturbing other agents.
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-x1", "agent-x", "gardenio", "crew", slTestNow.Add(-3*time.Hour)),
		slAgentBead("gd-x2", "agent-x", "gardenio", "crew", slTestNow.Add(-1*time.Hour)),
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{
		slRow("r1", "agent-x", 4.0, inWindow),
		slRow("r2", "agent-a", 1.0, inWindow),
	}}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())

	if n := testutil.CollectAndCount(metrics.SpendLogsCostGauge()); n != 1 {
		t.Errorf("gauge series = %d, want 1 (the shared name is ambiguous and skipped)", n)
	}
	if got := spendLogsGaugeValue(t, metrics, "agent-a", "gardenio", "crew", "", ""); got != 1.0 {
		t.Errorf("agent-a gauge = %v, want 1.0", got)
	}
	for _, id := range []string{"gd-x1", "gd-x2"} {
		if n := daemon.patchCount(id); n != 0 {
			t.Errorf("bead %s: %d PATCHes, want 0 (ambiguous name)", id, n)
		}
	}
}

func TestSpendLogsSweepKeepsSeriesOnProjectionFailure(t *testing.T) {
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{slRow("r1", "agent-a", 3.0, inWindow)}}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())
	if got := spendLogsGaugeValue(t, metrics, "agent-a", "gardenio", "crew", "", ""); got != 3.0 {
		t.Fatalf("agent-a gauge = %v, want 3.0", got)
	}

	// A failing proxy must leave the previous snapshot standing (a Reset here
	// would dent increase() fleet-wide for one flaky cycle) and write nothing.
	litellm.setFail(true)
	sweep.syncOnce(t.Context())
	if got := spendLogsGaugeValue(t, metrics, "agent-a", "gardenio", "crew", "", ""); got != 3.0 {
		t.Errorf("after failed projection: agent-a gauge = %v, want previous 3.0", got)
	}
	if n := daemon.patchCount("gd-a"); n != 1 {
		t.Errorf("after failed projection: %d PATCHes, want still 1", n)
	}
}

func TestSpendLogsSweepEmptyPopulationClearsGauge(t *testing.T) {
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{slRow("r1", "agent-a", 3.0, inWindow)}}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())
	if n := testutil.CollectAndCount(metrics.SpendLogsCostGauge()); n != 1 {
		t.Fatalf("gauge series = %d, want 1", n)
	}

	// The agent ages out of the lookback (no beads created in-window any
	// more) → an honest empty snapshot retires its series.
	daemon.mu.Lock()
	daemon.beads = nil
	daemon.mu.Unlock()
	sweep.syncOnce(t.Context())
	if n := testutil.CollectAndCount(metrics.SpendLogsCostGauge()); n != 0 {
		t.Errorf("after population emptied: gauge series = %d, want 0", n)
	}
}

// TestSpendLogsSweepWindowEdges pins the projection window the sweep asks the
// client for: [now - lookback - flushLag, now - flushLag) — the flush-lag
// guard trails BOTH edges so the window length stays exactly the lookback.
func TestSpendLogsSweepWindowEdges(t *testing.T) {
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-time.Hour)),
	}}
	var gotStart, gotEnd string
	lsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStart = r.URL.Query().Get("start_date")
		gotEnd = r.URL.Query().Get("end_date")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []spendlogs.Row{}, "page": 1, "total": 0, "total_pages": 1})
	}))
	t.Cleanup(lsrv.Close)

	dsrv := daemon.server(t)
	client, err := beadsapi.New(beadsapi.Config{HTTPAddr: dsrv.URL})
	if err != nil {
		t.Fatalf("beadsapi.New: %v", err)
	}
	slClient := spendlogs.New("sk-test", slog.Default())
	slClient.Throttle = 0
	sweep := newSpendLogsSweep(client, slClient, lsrv.URL, nil, 0, 0, slog.Default())
	sweep.now = func() time.Time { return slTestNow }

	sweep.syncOnce(t.Context())

	to := slTestNow.Add(-spendLogsFlushLag)
	from := to.Add(-spendLogsSweepLookback)
	// The client widens the server window by 1s each side (undocumented
	// boundary inclusivity — spendlogs.fetchPage); the strict filter is
	// client-side.
	wantStart := from.UTC().Add(-time.Second).Format("2006-01-02 15:04:05")
	wantEnd := to.UTC().Add(time.Second).Format("2006-01-02 15:04:05")
	if gotStart != wantStart || gotEnd != wantEnd {
		t.Errorf("window = [%q, %q], want [%q, %q]", gotStart, gotEnd, wantStart, wantEnd)
	}
}

// Guard the constants the P10 pair's population-window comment depends on
// (ka-0000006903).
func TestSpendLogsSweepDefaultsMatchParityWindow(t *testing.T) {
	if xcheckSpendParityWindow != spendLogsSweepLookback {
		t.Errorf("xcheckSpendParityWindow (%v) should equal spendLogsSweepLookback (%v) so P10 compares exactly the actively-tracked population",
			xcheckSpendParityWindow, spendLogsSweepLookback)
	}
}

// Compile-time-ish guard that the sweep's field name matches what the P10
// pair scans — a drift here silently empties P10's side B.
func TestSpendLogsFieldNameStable(t *testing.T) {
	if spendLogsCostField != "spend_logs_cost_usd" {
		t.Errorf("spendLogsCostField = %q; renaming it breaks the kbeads type:agent schema and P10", spendLogsCostField)
	}
}

// TestSpendLogsSweepTrackedBeadsWalkIsPaged is the regression guard for
// ka-0000001774: trackedAgentBeads must page the type=agent window and reduce
// each page to the compact trackedAgent projection, never asking the daemon for
// more than one page (spendLogsListPageLimit) at a time. The old loop paged the
// query but appended every FULL BeadDetail into one slice, so peak memory was the
// whole 48h window (~150 MiB at the 20k cap) — the structural twin of the
// fleet-archetype OOM (ka-0000006669).
//
// It asserts on the REQUESTS, not just the result: a correct per-agent tally can
// still be derived by one unbounded fetch, so the walk shape (bounded page limit,
// advancing offsets, ≥2 pages actually exercised) is the thing under test.
func TestSpendLogsSweepTrackedBeadsWalkIsPaged(t *testing.T) {
	// 1,200 beads spanning the window = 3 pages at the 500 page size, so paging
	// is actually exercised (a single-shot fetch would be one call).
	inWindow := slTestNow.Add(-1 * time.Hour)
	const n = 1200
	beads := make([]map[string]any, 0, n)
	rows := make([]spendlogs.Row, 0, n)
	for i := range n {
		name := "agent-" + strconv.Itoa(i)
		beads = append(beads, slAgentBead("gd-"+strconv.Itoa(i), name, "gardenio", "crew", slTestNow.Add(-2*time.Hour)))
		rows = append(rows, slRow("r"+strconv.Itoa(i), name, 1.0, inWindow))
	}
	daemon := &slDaemon{beads: beads}
	litellm := &slLiteLLM{rows: rows}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())

	calls := daemon.agentListQueries()
	if len(calls) == 0 {
		t.Fatal("no type=agent list requests recorded")
	}
	for i, c := range calls {
		if c.limit > spendLogsListPageLimit {
			t.Errorf("call %d requested limit=%d, exceeds page size %d — an unbounded fetch would re-open ka-0000001774",
				i, c.limit, spendLogsListPageLimit)
		}
		if c.limit <= 0 {
			t.Errorf("call %d requested limit=%d; an unlimited request lets the daemon choose the page size", i, c.limit)
		}
		if want := i * spendLogsListPageLimit; c.offset != want {
			t.Errorf("call %d offset = %d, want %d (pages must advance or the walk repeats/skips rows)", i, c.offset, want)
		}
		// created_at is immutable, so it is the only sort that gives a stable
		// page sequence while new agent beads are minted underneath the walk.
		if c.sort != "created_at" {
			t.Errorf("call %d sort = %q, want \"created_at\" for a stable paged walk", i, c.sort)
		}
	}
	// 1200 beads / 500 per page = 3 pages (the 3rd is short, so the walk stops).
	if len(calls) != 3 {
		t.Errorf("made %d type=agent list calls, want 3 (%d beads / %d per page)", len(calls), n, spendLogsListPageLimit)
	}
	// The whole point: a bounded, page-by-page walk still attributes every agent.
	if got := testutil.CollectAndCount(metrics.SpendLogsCostGauge()); got != n {
		t.Errorf("gauge series = %d, want %d (every tracked agent attributed across all pages)", got, n)
	}
}

// TestSpendLogsSweepTrackedBeadsStopsOnShortPage guards the termination
// condition: a population smaller than one page must cost exactly one list
// request, not a walk to spendLogsSweepMaxPages.
func TestSpendLogsSweepTrackedBeadsStopsOnShortPage(t *testing.T) {
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{slRow("r1", "agent-a", 1.0, slTestNow.Add(-time.Hour))}}
	sweep, _, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())

	if got := len(daemon.agentListQueries()); got != 1 {
		t.Errorf("made %d type=agent list calls for a 1-bead population, want 1 — a short page means the walk is done", got)
	}
}

// TestTrackedAgentIsCompactProjection pins the fetch-many/use-one invariant at
// the type level: the walk's accumulator element must stay a fixed-size value —
// no map, no full/pointer BeadDetail — so no future edit can quietly reintroduce
// the ~150 MiB accumulation of full agent beads (the ka-0000001774 regression).
// A trackedAgent holds only scalars and strings, so its shell size is constant
// and it never retains the heavy Fields map (which carries the spawn prompt).
func TestTrackedAgentIsCompactProjection(t *testing.T) {
	typ := reflect.TypeFor[trackedAgent]()
	for i := range typ.NumField() {
		f := typ.Field(i)
		switch f.Type.Kind() {
		case reflect.Map, reflect.Slice:
			t.Errorf("trackedAgent.%s is a %s — a variable-size field re-opens the accumulation bug (ka-0000001774)",
				f.Name, f.Type.Kind())
		case reflect.Pointer, reflect.Struct:
			if strings.Contains(f.Type.String(), "BeadDetail") {
				t.Errorf("trackedAgent.%s retains a BeadDetail (%s) — the whole point is NOT to hold full beads",
					f.Name, f.Type)
			}
		}
	}
}

// slDaemon must be usable concurrently by the projection's proxy enumeration
// (ProxyURLs lists account beads) — ensure the stub tolerates the account
// query returning nothing without panicking.
func TestSpendLogsSweepProxyEnumerationTolerated(t *testing.T) {
	inWindow := slTestNow.Add(-1 * time.Hour)
	daemon := &slDaemon{beads: []map[string]any{
		slAgentBead("gd-a", "agent-a", "gardenio", "crew", slTestNow.Add(-2*time.Hour)),
		// An account bead with a base_url pointing at the same stub proxy —
		// ProxyURLs dedups it against the global URL, so the projection still
		// sweeps the proxy exactly once (no double-count).
		{"id": "gd-acct", "type": "account", "status": "open", "created_at": slTestNow.Add(-30 * 24 * time.Hour).Format(time.RFC3339), "fields": map[string]any{}},
	}}
	litellm := &slLiteLLM{rows: []spendlogs.Row{slRow("r1", "agent-a", 1.0, inWindow)}}
	sweep, metrics, _ := newTestSpendLogsSweep(t, daemon, litellm)

	sweep.syncOnce(t.Context())
	if got := spendLogsGaugeValue(t, metrics, "agent-a", "gardenio", "crew", "", ""); got != 1.0 {
		t.Errorf("agent-a gauge = %v, want 1.0", got)
	}
}
