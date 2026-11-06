package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

var errFakeList = errors.New("fake list err")

// mkHealthServer wires a beads-viewer Server pointed at a fake snapshot
// daemon URL, so tests can exercise the full snapshot fetch + decode path.
func mkHealthServer(t *testing.T, daemon beadAPI, beadsURL string) *http.ServeMux {
	t.Helper()
	srv := NewServer(daemon, slog.Default(), "", beadsURL, "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux
}

// fakeSnapshotServer returns a httptest.Server that serves a canned
// /v1/health/snapshot payload. Tests pass a customise func to mutate the
// payload before encoding.
func fakeSnapshotServer(t *testing.T, customise func(*kbeadsHealthSnapshot)) *httptest.Server {
	t.Helper()
	now := time.Now().UTC()
	payload := &kbeadsHealthSnapshot{
		SchemaVersion: 1,
		Now:           now,
		Fleet: healthTileSnapshot{
			Data: jsonRaw(t, fleetSnapshotData{AgentsAlive: 3, AgentsStuck: 1, AgentsTotal: 5}),
			Spec: map[string]any{},
			Meta: map[string]any{},
		},
		Decisions: healthTileSnapshot{
			Data: jsonRaw(t, decisionsSnapshotData{
				Pending: 2,
				Next3: []decisionPreviewSnapshot{
					{ID: "gd-dec1", Title: "first", AgeSeconds: 90},
					{ID: "gd-dec2", Title: "second", AgeSeconds: 600},
				},
			}),
			Spec: map[string]any{},
			Meta: map[string]any{},
		},
		Events: healthTileSnapshot{
			Data: jsonRaw(t, eventsSnapshotData{
				WindowSeconds: 300,
				Histogram: []eventHistogramBucket{
					{Time: now.Add(-2 * time.Minute), Count: 4},
					{Time: now.Add(-1 * time.Minute), Count: 7},
				},
			}),
			Spec: map[string]any{},
			Meta: map[string]any{"total_24h_count": float64(1234)},
		},
		Incidents: healthTileSnapshot{
			Data: jsonRaw(t, incidentsSnapshotData{
				WindowSeconds: 24 * 3600,
				RecentReleaseBugs: []incidentPreviewSnapshot{
					{ID: "gd-bug1", Title: "release broken", CreatedAt: now.Add(-2 * time.Hour)},
				},
				RecentAgentCrashes: []incidentPreviewSnapshot{
					{ID: "gd-bug2", Title: "agent OOM", CreatedAt: now.Add(-30 * time.Minute)},
				},
			}),
			Spec: map[string]any{},
			Meta: map[string]any{},
		},
		Kbeads: healthTileSnapshot{
			Data: jsonRaw(t, kbeadsSnapshotData{OK: true, LastHealthAt: now}),
			Spec: map[string]any{},
			Meta: map[string]any{},
		},
	}
	if customise != nil {
		customise(payload)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health/snapshot" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonRaw(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return b
}

func TestHandleHealth_RendersAllTiles(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 42}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Open beads",
		"Agent fleet",
		"Bead-event throughput",
		"Recent incidents",
		"Bridges",
		"Decisions waiting on operator",
		"Gardener cycles",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing tile heading %q", want)
		}
	}
	// Each tile div should poll its own fragment endpoint every 30s.
	for _, want := range []string{
		"/health/tile/open-beads",
		"/health/tile/fleet",
		"/health/tile/events",
		"/health/tile/incidents",
		"/health/tile/bridges",
		"/health/tile/decisions",
		"/health/tile/gardeners",
		"every 30s",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing hx-get target %q", want)
		}
	}
	// Every poller must pause while the tab is hidden: a backgrounded health
	// page otherwise keeps fanning out list queries to kbeads (gd-g25jihMHFs).
	if polls, gated := strings.Count(body, `hx-trigger="every 30s`), strings.Count(body, `hx-trigger="every 30s [document.visibilityState==='visible']"`); polls == 0 || polls != gated {
		t.Errorf("health tile pollers: %d total, %d visibility-gated; want all gated", polls, gated)
	}
}

func TestHandleHealth_PageInlinesSnapshotData(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 17}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// Fleet alive=3, stuck=1, total=5 from fixture.
	if !strings.Contains(body, ">3<") || !strings.Contains(body, ">5<") {
		t.Errorf("expected fleet alive/total in body, got %s", body)
	}
	// Decisions pending = 2.
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected decisions pending=2 in body, got %s", body)
	}
	// Events 24h total = 1234.
	if !strings.Contains(body, "1234") {
		t.Errorf("expected events 24h total=1234 in body, got %s", body)
	}
}

func TestHealthTile_OpenBeads(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 99}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/open-beads", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Open beads") {
		t.Errorf("missing tile heading, got %s", body)
	}
	if !strings.Contains(body, "99") {
		t.Errorf("expected open total 99 in body, got %s", body)
	}
	// Verify the daemon was queried for both open + ready (NoOpenDeps),
	// and that all open-beads-tile queries are scoped to kind=issue so the
	// counter excludes audit-event ledger beads and session config beads
	// (ka-0000006881).
	var sawReady bool
	for _, q := range f.listAllQ {
		if q.NoOpenDeps && q.ExcludeScheduled {
			sawReady = true
		}
		if len(q.Statuses) > 0 && (q.Statuses[0] == "open" || q.Statuses[0] == "in_progress") {
			if len(q.Kinds) != 1 || q.Kinds[0] != "issue" {
				t.Errorf("expected Kinds=[issue] on open-beads tile query, got %+v", q)
			}
		}
	}
	if !sawReady {
		t.Errorf("expected ready-queue query with NoOpenDeps + ExcludeScheduled, got %+v", f.listAllQ)
	}
}

func TestHealthTile_Fleet(t *testing.T) {
	f := newFake()
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/fleet", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"Agent fleet", ">3<", ">1<", ">5<"} {
		if !strings.Contains(body, want) {
			t.Errorf("fleet tile missing %q, body=%s", want, body)
		}
	}
}

func TestHealthTile_Events_Snapshot24hTotal(t *testing.T) {
	f := newFake()
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/events", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "1234") {
		t.Errorf("expected 24h total=1234, body=%s", body)
	}
	// 5m total = 4 + 7 = 11.
	if !strings.Contains(body, ">11<") {
		t.Errorf("expected 5m total=11, body=%s", body)
	}
}

func TestHealthTile_Incidents_RendersBoth(t *testing.T) {
	f := newFake()
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/incidents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{"gd-bug1", "gd-bug2", "release broken", "agent OOM"} {
		if !strings.Contains(body, want) {
			t.Errorf("incidents tile missing %q, body=%s", want, body)
		}
	}
	// The new always-on counter pills surface 24h and open totals — verify the
	// 4 pill labels render even when daemon list returns nothing (counters
	// still come out as zero, not missing).
	for _, want := range []string{
		"Release bugs (24h)",
		"Agent crashes (24h)",
		"Open release bugs",
		"Open agent crashes",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incidents tile missing counter %q, body=%s", want, body)
		}
	}
}

func TestHealthTile_Incidents_EmptyShowsZeroCounters(t *testing.T) {
	f := newFake()
	// Empty snapshot lists + nil daemon resp → all four counters render as
	// "0" stat pills rather than the previous "None in last 24h" paragraphs.
	snap := fakeSnapshotServer(t, func(p *kbeadsHealthSnapshot) {
		p.Incidents.Data = jsonRaw(t, incidentsSnapshotData{
			WindowSeconds:      24 * 3600,
			RecentReleaseBugs:  nil,
			RecentAgentCrashes: nil,
		})
	})
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/incidents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">0<") {
		t.Errorf("expected at least one zero counter pill in empty state, body=%s", body)
	}
	// Audit recommendation: tile must still carry a stable footer summary.
	if !strings.Contains(body, "No incidents in the last 24h") {
		t.Errorf("expected stable empty-state footer, body=%s", body)
	}
}

func TestHealthTile_Incidents_OpenCountersFromDaemon(t *testing.T) {
	f := newFake()
	// Drive the fake's catch-all listResp to a non-zero Total so both
	// daemon-backed counters surface the same number — sufficient to verify
	// the queries fired and the view picked up Total.
	f.listResp = &beadsapi.ListBeadsResult{Total: 7}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/incidents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">7<") {
		t.Errorf("expected open release-bugs/agent-crashes count 7, body=%s", body)
	}
	// Verify both label filters were exercised.
	var sawReleaseBug, sawAgentCrash bool
	for _, q := range f.listAllQ {
		for _, l := range q.Labels {
			if l == "release-bug" {
				sawReleaseBug = true
			}
			if l == "agent-crash" {
				sawAgentCrash = true
			}
		}
	}
	if !sawReleaseBug {
		t.Errorf("expected ListBeadsFiltered with label=release-bug, got %+v", f.listAllQ)
	}
	if !sawAgentCrash {
		t.Errorf("expected ListBeadsFiltered with label=agent-crash, got %+v", f.listAllQ)
	}
}

func TestHealthTile_Decisions(t *testing.T) {
	f := newFake()
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{"gd-dec1", "gd-dec2", "first", "second"} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions tile missing %q, body=%s", want, body)
		}
	}
}

func TestHealthTile_Bridges_PlaceholderState(t *testing.T) {
	f := newFake()
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/bridges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{"slack-bridge", "jira-bridge", "gitlab-bridge", "controller", "unknown"} {
		if !strings.Contains(body, want) {
			t.Errorf("bridges tile missing %q, body=%s", want, body)
		}
	}
}

func TestHealthTile_Gardeners_EmptyState(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Gardener cycles") {
		t.Errorf("gardeners tile missing heading, body=%s", body)
	}
	if !strings.Contains(body, "no triggers registered") {
		t.Errorf("expected empty-state note, body=%s", body)
	}
}

func TestHealthTile_Gardeners_AggregatesByRole(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-s1", Type: "schedule", Status: "open", Fields: map[string]string{"role": "gardener", "enabled": "true"}},
			{ID: "gd-s2", Type: "schedule", Status: "open", Fields: map[string]string{"role": "gardener", "enabled": "false"}},
			{ID: "gd-s3", Type: "schedule", Status: "open", Fields: map[string]string{"role": "rescope", "enabled": "true"}},
			{ID: "gd-s4", Type: "schedule", Status: "closed", Fields: map[string]string{"role": "ignored", "enabled": "true"}},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{"Gardener cycles", "gardener", "rescope", "Enabled", "Disabled"} {
		if !strings.Contains(body, want) {
			t.Errorf("gardeners tile missing %q, body=%s", want, body)
		}
	}
	// Closed schedule must not appear.
	if strings.Contains(body, "ignored") {
		t.Errorf("closed schedule role 'ignored' should not be rendered, body=%s", body)
	}
	// /triggers link present.
	if !strings.Contains(body, "/triggers") {
		t.Errorf("missing /triggers link, body=%s", body)
	}
}

func TestHealthTile_Gardeners_DaemonErrorRendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = errFakeList
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded tile, not 5xx)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "schedule list unavailable") {
		t.Errorf("expected error banner, body=%s", body)
	}
}

func TestHealthTile_Fleet_SnapshotErrorRendersBanner(t *testing.T) {
	f := newFake()
	// Empty URL forces fetchKbeadsHealthSnapshot to error.
	mux := mkHealthServer(t, f, "")

	req := httptest.NewRequest("GET", "/health/tile/fleet", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded tile, not 5xx)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "fleet snapshot unavailable") {
		t.Errorf("expected error banner, body=%s", body)
	}
}

func TestHealthTile_Decisions_SnapshotErrorRendersBanner(t *testing.T) {
	f := newFake()
	mux := mkHealthServer(t, f, "")

	req := httptest.NewRequest("GET", "/health/tile/decisions", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "decisions snapshot unavailable") {
		t.Errorf("expected error banner, body=%s", body)
	}
}

func TestSnapshotStaleAge(t *testing.T) {
	now := time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		snap      *kbeadsHealthSnapshot
		wantStale bool
	}{
		{"nil snapshot", nil, false},
		{"zero now", &kbeadsHealthSnapshot{}, false},
		{"fresh", &kbeadsHealthSnapshot{Now: now.Add(-10 * time.Second)}, false},
		{"at threshold", &kbeadsHealthSnapshot{Now: now.Add(-60 * time.Second)}, false},
		{"just over", &kbeadsHealthSnapshot{Now: now.Add(-61 * time.Second)}, true},
		{"way stale", &kbeadsHealthSnapshot{Now: now.Add(-5 * time.Minute)}, true},
		{"clock skew (future)", &kbeadsHealthSnapshot{Now: now.Add(10 * time.Second)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, stale := snapshotStaleAge(c.snap, now)
			if stale != c.wantStale {
				t.Errorf("stale = %v, want %v", stale, c.wantStale)
			}
		})
	}
}

func TestHealthTile_Fleet_StaleSnapshotRendersPill(t *testing.T) {
	f := newFake()
	// Snapshot.Now set far enough in the past that snapshotStaleAge
	// flags it as stale. The test only asserts pill rendering — it
	// does NOT assert the exact age string (formatting is covered by
	// TestFmtAgeSeconds).
	snap := fakeSnapshotServer(t, func(p *kbeadsHealthSnapshot) {
		p.Now = time.Now().Add(-3 * time.Minute)
	})
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/fleet", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "health-pill-stale") {
		t.Errorf("expected stale pill class, body=%s", body)
	}
	if !strings.Contains(body, ">stale<") {
		t.Errorf("expected stale pill text, body=%s", body)
	}
}

func TestHealthTile_Fleet_FreshSnapshotNoPill(t *testing.T) {
	f := newFake()
	snap := fakeSnapshotServer(t, nil) // Now = time.Now().UTC()
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/fleet", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "health-pill-stale") {
		t.Errorf("fresh snapshot should not render stale pill, body=%s", body)
	}
}

func TestHealthTile_OpenBeads_StaleInProgress(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 5}
	now := time.Now()
	f.inProgressResp = &beadsapi.ListBeadsResult{
		Total: 4,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-fresh1", UpdatedAt: now.Add(-30 * time.Minute)},
			{ID: "gd-fresh2", UpdatedAt: now.Add(-90 * time.Minute)},
			{ID: "gd-stale1", UpdatedAt: now.Add(-3 * time.Hour)},
			{ID: "gd-stale2", UpdatedAt: now.Add(-25 * time.Hour)},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/open-beads", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "In-progress") {
		t.Errorf("tile missing in-progress label, body=%s", body)
	}
	// In-progress total = 4 from inProgressResp.Total.
	if !strings.Contains(body, ">4<") {
		t.Errorf("expected in-progress=4 in body, got %s", body)
	}
	// Stale (>2h) = gd-stale1 + gd-stale2 = 2.
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected stale=2 in body, got %s", body)
	}
	if !strings.Contains(body, "Stale") {
		t.Errorf("tile missing Stale label, body=%s", body)
	}
}

func TestHealthTile_Costs(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 0}
	f.accountResp = &beadsapi.ListBeadsResult{
		Total: 3,
		Beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-acc1", Title: "alpha", Status: "active",
				Fields: map[string]string{
					"status":          "active",
					"daily_cost_usd":  "12.50",
					"weekly_cost_usd": "60.00",
				},
			},
			{
				ID: "gd-acc2", Title: "beta", Status: "active",
				Fields: map[string]string{
					"status":          "active",
					"daily_cost_usd":  "7.25",
					"weekly_cost_usd": "30.00",
					"throttle_status": "throttled",
				},
			},
			{
				ID: "gd-acc3", Title: "gamma",
				Fields: map[string]string{},
			},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Cost burn") {
		t.Errorf("missing tile heading, body=%s", body)
	}
	// Daily total = 12.50 + 7.25 = 19.75.
	if !strings.Contains(body, "$19.75") {
		t.Errorf("expected daily total $19.75, body=%s", body)
	}
	// Active count = 3: alpha + beta have status=active, and gamma's blank
	// status field is also usable per the controller's own check
	// (account_spread.go selectAccount: only a non-empty, non-"active" value
	// excludes an account) — "" is active-by-default, not absent.
	if !strings.Contains(body, ">3<") {
		t.Errorf("expected active count 3, body=%s", body)
	}
	// Throttled count = 1 (beta).
	if !strings.Contains(body, "throttled") {
		t.Errorf("expected throttled label, body=%s", body)
	}
	// Link to /costs.
	if !strings.Contains(body, "/costs") {
		t.Errorf("expected /costs link, body=%s", body)
	}
}

func TestHealthTile_Costs_DaemonErrorRendersBanner(t *testing.T) {
	f := newFake()
	f.listErr = errFakeBoom
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/costs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (degraded tile)", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "costs snapshot unavailable") {
		t.Errorf("expected error banner, body=%s", body)
	}
}

func TestHealthTile_Build_RendersVersionAndReleaseBugs(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{}
	// Two release bugs in the canned incidents fixture path — the default
	// fixture has 1 release bug.
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/build", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "Build") {
		t.Errorf("missing tile heading, body=%s", body)
	}
	// Default version constant is "dev" (set in main.go init); the tile falls
	// back to literal "dev" when blank.
	if !strings.Contains(body, "dev") {
		t.Errorf("expected version=dev in body, got %s", body)
	}
	// Default fixture has 1 release bug → ">1<".
	if !strings.Contains(body, ">1<") {
		t.Errorf("expected release-bug count 1, body=%s", body)
	}
	if !strings.Contains(body, "/triggers") {
		t.Errorf("expected /triggers link, body=%s", body)
	}
}

func TestHealthPage_IncludesNewTiles(t *testing.T) {
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 1}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		"/health/tile/costs",
		"/health/tile/build",
		"Cost burn",
		"Build",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("health page missing %q, body excerpt=%.500s", want, body)
		}
	}
}

var errFakeBoom = errFakeBoomT{}

type errFakeBoomT struct{}

func (errFakeBoomT) Error() string { return "boom" }

func TestParseCalverDate(t *testing.T) {
	cases := []struct {
		in       string
		wantOK   bool
		wantDate string
	}{
		{"2026.135.1", true, "2026-05-15"},
		{"2026.1.0", true, "2026-01-01"},
		{"2024.366.5", true, "2024-12-31"}, // leap-year day 366 → Dec 31
		{"2026.365.5", true, "2026-12-31"}, // non-leap day 365 → Dec 31
		{"dev", false, ""},
		{"", false, ""},
		{"2026", false, ""},
		{"abc.def.1", false, ""},
		{"2026.0.1", false, ""},   // doy must be >= 1
		{"2026.400.1", false, ""}, // doy must be <= 366
		{"1999.10.1", false, ""},  // year guard
	}
	for _, c := range cases {
		got, ok := parseCalverDate(c.in)
		if ok != c.wantOK {
			t.Errorf("parseCalverDate(%q) ok = %v, want %v", c.in, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if got.Format("2006-01-02") != c.wantDate {
			t.Errorf("parseCalverDate(%q) = %s, want %s", c.in, got.Format("2006-01-02"), c.wantDate)
		}
	}
}

func TestHealthTile_Build_RendersReleaseDateWhenCalver(t *testing.T) {
	// Temporarily flip the package-level version to a calver value so the
	// build tile renders the Released row. Other tests use "dev" which
	// suppresses the row — explicit guard preserves that behavior here.
	prev := version
	version = "2026.135.1"
	t.Cleanup(func() { version = prev })

	f := newFake()
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/build", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "Released") {
		t.Errorf("expected Released label when version is calver, body=%s", body)
	}
	if !strings.Contains(body, "2026-05-15") {
		t.Errorf("expected calver-derived date 2026-05-15, body=%s", body)
	}
}

func TestFmtAgeSeconds(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0s"},
		{45, "45s"},
		{60, "1m"},
		{125, "2m"},
		{3600, "1h"},
		{7200, "2h"},
		{86400, "1d"},
		{172800, "2d"},
		{-5, "0s"},
	}
	for _, c := range cases {
		if got := fmtAgeSeconds(c.in); got != c.want {
			t.Errorf("fmtAgeSeconds(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHealthTile_OpenBeads_PriorityAndOldestAge(t *testing.T) {
	now := time.Now()
	// fake daemon returns a single canned listResp for any catch-all query
	// (sufficient for both open-count and priority-scan paths since the
	// fake doesn't otherwise discriminate by Sort). The fixture exercises
	// the P0/P1 counters and the oldest-open lookup.
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Total: 42,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-old", Priority: 0, CreatedAt: now.Add(-72 * time.Hour)},
			{ID: "gd-p0b", Priority: 0, CreatedAt: now.Add(-1 * time.Hour)},
			{ID: "gd-p1a", Priority: 1, CreatedAt: now.Add(-2 * time.Hour)},
			{ID: "gd-p1b", Priority: 1, CreatedAt: now.Add(-30 * time.Minute)},
			{ID: "gd-p3", Priority: 3, CreatedAt: now.Add(-15 * time.Minute)},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/open-beads", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{"P0 open", "P1 open", "Oldest open", "gd-old"} {
		if !strings.Contains(body, want) {
			t.Errorf("open-beads tile missing %q, body=%s", want, body)
		}
	}
	// P0 = 2 (gd-old, gd-p0b); P1 = 2 (gd-p1a, gd-p1b).
	if !strings.Contains(body, ">2<") {
		t.Errorf("expected P0/P1 count 2 in body, got %s", body)
	}
	// Oldest open is gd-old at -72h → renders as 3d.
	if !strings.Contains(body, "3d") {
		t.Errorf("expected oldest age 3d in body, got %s", body)
	}
	// The priority-scan query must use Sort=priority so the daemon returns
	// lowest-priority numerically first — guards against future refactors
	// that drop the ordering hint.
	var sawPriorityScan bool
	for _, q := range f.listAllQ {
		if q.Sort == "priority" && len(q.Statuses) == 1 && q.Statuses[0] == "open" {
			sawPriorityScan = true
			break
		}
	}
	if !sawPriorityScan {
		t.Errorf("expected ListBeadsFiltered with Sort=priority + Statuses=[open], got %+v", f.listAllQ)
	}
}

func TestHealthTile_OpenBeads_NoPriorityRowsHidesOldest(t *testing.T) {
	// When no P0/P1 rows are returned and CreatedAt is zero, the tile
	// should still render zero counters but suppress the Oldest open row
	// rather than rendering an empty "(<link>)" line.
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{Total: 1, Beads: []*beadsapi.BeadDetail{}}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/open-beads", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "P0 open") || !strings.Contains(body, "P1 open") {
		t.Errorf("expected P0/P1 labels even with empty list, body=%s", body)
	}
	if strings.Contains(body, "Oldest open") {
		t.Errorf("Oldest open should be suppressed when no rows have CreatedAt, body=%s", body)
	}
}

func TestHealthTile_Bridges_PerSourceLastEvent(t *testing.T) {
	// Every source-labelled query routes through the fake's catch-all
	// listResp branch. We return a fresh bead so each of the three bridge
	// rows surfaces an "ok" pill alongside the canned bead id.
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Total: 1,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-src-recent", CreatedAt: now.Add(-2 * time.Minute)},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/bridges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	for _, want := range []string{
		"slack-bridge",
		"jira-bridge",
		"gitlab-bridge",
		"controller",
		"gd-src-recent",
		">ok<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("bridges tile missing %q, body=%s", want, body)
		}
	}
	// Verify each of the three bridge source labels was queried with a
	// -created_at sort + limit=1 lookback. Captures regressions that drop
	// a source row or fall back to the empty placeholder.
	seen := map[string]bool{}
	for _, q := range f.listAllQ {
		if q.Sort != "-created_at" || q.Limit != 1 {
			continue
		}
		for _, l := range q.Labels {
			if strings.HasPrefix(l, "source:") {
				seen[l] = true
			}
		}
	}
	for _, want := range []string{"source:slack", "source:jira", "source:gitlab"} {
		if !seen[want] {
			t.Errorf("expected per-bridge lookback query for label %q, got %+v", want, f.listAllQ)
		}
	}
}

func TestHealthTile_Bridges_StaleSourceShowsErrPill(t *testing.T) {
	// CreatedAt older than healthBridgeStaleAfter → classifyBridgeAge
	// returns "stale". Renders the err chip rather than slow/ok/unknown.
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Total: 1,
		Beads: []*beadsapi.BeadDetail{
			{ID: "gd-src-stale", CreatedAt: now.Add(-12 * time.Hour)},
		},
	}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	req := httptest.NewRequest("GET", "/health/tile/bridges", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, ">stale<") {
		t.Errorf("expected stale pill for old bridge events, body=%s", body)
	}
}

func TestClassifyBridgeAge(t *testing.T) {
	cases := []struct {
		age  time.Duration
		want string
	}{
		{0, healthBridgeStateOK},
		{10 * time.Minute, healthBridgeStateOK},
		{31 * time.Minute, healthBridgeStateWarn},
		{2 * time.Hour, healthBridgeStateWarn},
		{7 * time.Hour, healthBridgeStateStale},
		{48 * time.Hour, healthBridgeStateStale},
	}
	for _, c := range cases {
		if got := classifyBridgeAge(c.age); got != c.want {
			t.Errorf("classifyBridgeAge(%v) = %q, want %q", c.age, got, c.want)
		}
	}
}

func TestHealthTile_Gardeners_ListsEventModeGardeners(t *testing.T) {
	// gd-fL4PM5z8kc: event-mode (trigger:multi) gardeners count in the tile and
	// are listed with their mode, watchdog cadence, last run and liveness.
	now := time.Now().UTC()
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	f := newFake()
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		eventGardenerBead("gd-ev-ok", "gd-p-ok", map[string]string{"last_run": ago(2 * time.Hour)}),
		eventGardenerBead("gd-ev-stale", "gd-p-stale", map[string]string{"last_run": ago(72 * time.Hour)}),
		eventGardenerBead("gd-ev-idle", "gd-p-idle", map[string]string{"last_run": ago(72 * time.Hour)}),
		eventGardenerBead("gd-ev-paused", "gd-p-paused", map[string]string{"enabled": "false"}),
		// Not a gardener: must not be counted or listed.
		{ID: "gd-multi-plain", Type: "trigger", Status: "open", Labels: []string{"trigger:multi"}, Fields: map[string]string{"role": "plainmulti"}},
		// A timed gardener: counted in the role table, not in the event list.
		{ID: "gd-timed", Type: "trigger", Status: "open", Labels: []string{"trigger:schedule", "role:gardener"}, Fields: map[string]string{"role": "gardener", "cron": "*/30 * * * *"}},
	}}
	f.readyWorkCount = map[string]int{"gd-p-stale": 3}
	snap := fakeSnapshotServer(t, nil)
	mux := mkHealthServer(t, f, snap.URL)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/health/tile/gardeners", nil))
	body := w.Body.String()

	for _, want := range []string{
		"Event mode", "1 stale",
		"plan-gardener", ">event<", ">timed<",
		"watchdog 17 4 * * *",
		"2h ago",
		`<tr data-gardener-state="stale">`,
		`<tr data-gardener-state="idle">`,
		`<tr data-gardener-state="ok">`,
		`<tr data-gardener-state="paused">`,
		"/gardener/gd-ev-stale",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("gardeners tile missing %q\nbody=%s", want, body)
		}
	}
	for _, unwanted := range []string{"gd-multi-plain", "plainmulti", "/gardener/gd-timed"} {
		if strings.Contains(body, unwanted) {
			t.Errorf("gardeners tile must not render %q\nbody=%s", unwanted, body)
		}
	}
	// Stale sorts first in the event list.
	if i, j := strings.Index(body, "gd-ev-stale"), strings.Index(body, "gd-ev-ok"); i < 0 || j < 0 || i > j {
		t.Errorf("stale gardener should be listed before the healthy one (stale@%d ok@%d)", i, j)
	}
}

func TestEventGardenerWindow_UsesWatchdogInterval(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	if got, want := eventGardenerWindow("17 4 * * *", "UTC", 0, now), 24*time.Hour+gardenerOverdueTolerance; got != want {
		t.Errorf("daily watchdog: window %s, want %s", got, want)
	}
	if got, want := eventGardenerWindow("17 4 * * *", "UTC", 5, now), 24*time.Hour+5*time.Minute+gardenerOverdueTolerance; got != want {
		t.Errorf("daily watchdog + cooldown: window %s, want %s", got, want)
	}
	if got, want := eventGardenerWindow("", "", 0, now), eventGardenerNoWatchdogWindow+gardenerOverdueTolerance; got != want {
		t.Errorf("no watchdog: window %s, want %s", got, want)
	}
}

func TestEventGardenerRows_Liveness(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	noWatchdog := map[string]string{"triggers": `[{"kind":"bead_event","label":"plan-activity"}]`}
	withFields := func(base, extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	young := eventGardenerBead("gd-young", "gd-p-young", nil)
	young.CreatedAt = now.Add(-time.Hour)
	noScope := eventGardenerBead("gd-noscope", "x", map[string]string{"last_run": ago(72 * time.Hour)})
	noScope.Labels = []string{"trigger:multi", "role:gardener"}

	cases := []struct {
		name      string
		bead      *beadsapi.BeadDetail
		ready     int
		readyErr  error
		wantState string
		wantProbe bool
	}{
		// Over 30 minutes without a run is normal for event mode: no probe, ok.
		{"ran 3h ago, daily watchdog", eventGardenerBead("gd-a", "gd-p-a", map[string]string{"last_run": ago(3 * time.Hour)}), 9, nil, eventGardenerStateOK, false},
		{"ran 23h ago, daily watchdog", eventGardenerBead("gd-b", "gd-p-b", map[string]string{"last_run": ago(23 * time.Hour)}), 9, nil, eventGardenerStateOK, false},
		{"quiet 2d, ready work", eventGardenerBead("gd-c", "gd-p-c", map[string]string{"last_run": ago(48 * time.Hour)}), 2, nil, eventGardenerStateStale, true},
		{"quiet 2d, no ready work", eventGardenerBead("gd-d", "gd-p-d", map[string]string{"last_run": ago(48 * time.Hour)}), 0, nil, eventGardenerStateIdle, true},
		{"quiet 2d, probe fails", eventGardenerBead("gd-e", "gd-p-e", map[string]string{"last_run": ago(48 * time.Hour)}), 0, errFakeList, eventGardenerStateUnchecked, true},
		{"no watchdog, ran 20h ago", eventGardenerBead("gd-f", "gd-p-f", withFields(noWatchdog, map[string]string{"last_run": ago(20 * time.Hour)})), 9, nil, eventGardenerStateOK, false},
		{"paused", eventGardenerBead("gd-g", "gd-p-g", map[string]string{"enabled": "false", "last_run": ago(240 * time.Hour)}), 9, nil, eventGardenerStatePaused, false},
		{"never run, created 1h ago", young, 9, nil, eventGardenerStateOK, false},
		{"quiet, no plan subtree", noScope, 9, nil, eventGardenerStateUnchecked, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.readyWorkCount = map[string]int{eventGardenerReadyScope(tc.bead): tc.ready}
			f.readyWorkErr = tc.readyErr
			srv := NewServer(f, slog.Default(), "", "", "", nil)
			rows, stale := srv.eventGardenerRows(context.Background(), []*beadsapi.BeadDetail{tc.bead}, now)
			if len(rows) != 1 {
				t.Fatalf("rows: want 1, got %d", len(rows))
			}
			if rows[0].State != tc.wantState {
				t.Errorf("state: want %q, got %q (detail %q)", tc.wantState, rows[0].State, rows[0].Detail)
			}
			if wantStale := tc.wantState == eventGardenerStateStale; (stale == 1) != wantStale {
				t.Errorf("stale count: got %d, want stale=%v", stale, wantStale)
			}
			probed := false
			for _, q := range f.listAllQ {
				if q.NoOpenDeps && q.CountOnly && q.Parent != "" {
					probed = true
				}
			}
			if probed != tc.wantProbe {
				t.Errorf("ready-work probe sent=%v, want %v", probed, tc.wantProbe)
			}
		})
	}
}

// readyProbes returns the ready-work count queries the fake saw.
func readyProbes(f *fakeDaemon) []beadsapi.ListBeadsQuery {
	var out []beadsapi.ListBeadsQuery
	for _, q := range f.listAllQ {
		if q.NoOpenDeps && q.CountOnly && q.Parent != "" {
			out = append(out, q)
		}
	}
	return out
}

func TestEventGardenerRows_ReadyWorkCountsStepsNotContainers(t *testing.T) {
	// An open phase epic (or molecule) under a plan is a container, not work
	// waiting on the gardener: the probe must ask for step types only.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newFake()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	b := eventGardenerBead("gd-q", "gd-p-q", map[string]string{"last_run": now.Add(-72 * time.Hour).Format(time.RFC3339)})
	srv.eventGardenerRows(context.Background(), []*beadsapi.BeadDetail{b}, now)

	qs := readyProbes(f)
	if len(qs) != 1 {
		t.Fatalf("ready probes: want 1, got %d", len(qs))
	}
	q := qs[0]
	if len(q.Kinds) != 0 {
		t.Errorf("probe must not filter by kind (kind=issue includes epics), got Kinds=%v", q.Kinds)
	}
	for _, bad := range []string{"epic", "molecule"} {
		if slices.Contains(q.Types, bad) {
			t.Errorf("probe Types %v must not include %q", q.Types, bad)
		}
	}
	for _, want := range []string{"task", "bug"} {
		if !slices.Contains(q.Types, want) {
			t.Errorf("probe Types %v must include %q", q.Types, want)
		}
	}
	if q.Parent != "gd-p-q" || !slices.Equal(q.Statuses, []string{"open"}) {
		t.Errorf("probe scope: want Parent=gd-p-q Statuses=[open], got Parent=%q Statuses=%v", q.Parent, q.Statuses)
	}
}

func TestEventGardenerRows_InFlightRunIsOK(t *testing.T) {
	// last_run is stamped at spawn, so a long run still going looks quiet. When
	// the gardener's last agent bead is open/in_progress it reads ok and no
	// ready-work probe is sent; once that agent closes, the quiet rule applies.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	quiet := now.Add(-72 * time.Hour).Format(time.RFC3339)
	cases := []struct {
		name        string
		fields      map[string]string
		agentStatus string
		wantState   string
		wantReady   int // ready probes expected
	}{
		{"blob last_agent_id in_progress", map[string]string{"gardener": `{"terminal_goal":"x","last_agent_id":"gd-agent-1"}`}, "in_progress", eventGardenerStateOK, 0},
		{"top-level last_agent_id open", map[string]string{"last_agent_id": "gd-agent-1"}, "open", eventGardenerStateOK, 0},
		{"last agent closed", map[string]string{"last_agent_id": "gd-agent-1"}, "closed", eventGardenerStateStale, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields := map[string]string{"last_run": quiet}
			for k, v := range tc.fields {
				fields[k] = v
			}
			f := newFake()
			f.beads["gd-agent-1"] = &beadsapi.BeadDetail{ID: "gd-agent-1", Type: "agent", Status: tc.agentStatus}
			f.readyWorkCount = map[string]int{"gd-p-live": 4}
			srv := NewServer(f, slog.Default(), "", "", "", nil)
			rows, _ := srv.eventGardenerRows(context.Background(),
				[]*beadsapi.BeadDetail{eventGardenerBead("gd-live", "gd-p-live", fields)}, now)
			if rows[0].State != tc.wantState {
				t.Errorf("state: want %q, got %q (detail %q)", tc.wantState, rows[0].State, rows[0].Detail)
			}
			if got := len(readyProbes(f)); got != tc.wantReady {
				t.Errorf("ready probes: want %d, got %d", tc.wantReady, got)
			}
		})
	}
}

func TestEventGardenerRows_ProbesCachedForTTL(t *testing.T) {
	// The tile refreshes every 30s per tab; probe answers are reused for 60s.
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.readyWorkCount = map[string]int{"gd-p-c": 2}
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	beads := []*beadsapi.BeadDetail{eventGardenerBead("gd-c", "gd-p-c", map[string]string{"last_run": now.Add(-72 * time.Hour).Format(time.RFC3339)})}

	for i, at := range []time.Time{now, now.Add(30 * time.Second)} {
		rows, stale := srv.eventGardenerRows(context.Background(), beads, at)
		if rows[0].State != eventGardenerStateStale || stale != 1 {
			t.Fatalf("render %d: want stale, got %q (stale=%d)", i, rows[0].State, stale)
		}
	}
	if got := len(readyProbes(f)); got != 1 {
		t.Errorf("within the TTL: want 1 ready probe, got %d", got)
	}
	srv.eventGardenerRows(context.Background(), beads, now.Add(healthEventProbeTTL+time.Second))
	if got := len(readyProbes(f)); got != 2 {
		t.Errorf("after the TTL: want 2 ready probes, got %d", got)
	}
}
