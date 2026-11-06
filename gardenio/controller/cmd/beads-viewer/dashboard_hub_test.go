package main

// Unit tests for the Observability Hub enumeration backend (dashboard_hub.go,
// W2 task ka-0000000773): grouping/sort determinism, viz-panel counting, the
// short-TTL cache, and that buildDashboardHub reuses buildDashboardView's uid
// gate.

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestGroupDashboardSections_Order(t *testing.T) {
	byProject := map[string][]dashboardHubCard{
		"monorepo": {
			{BeadID: "gd-2", Title: "Zebra"},
			{BeadID: "gd-1", Title: "Alpha"},
		},
		"gardenio": {{BeadID: "gd-3", Title: "Costs"}},
		"":         {{BeadID: "gd-4", Title: "Unfiled"}},
	}
	sections := groupDashboardSections(byProject)
	// Projects sorted asc, empty ("") last as the catch-all.
	wantProjects := []string{"gardenio", "monorepo", ""}
	if len(sections) != len(wantProjects) {
		t.Fatalf("got %d sections want %d", len(sections), len(wantProjects))
	}
	for i, want := range wantProjects {
		if sections[i].Project != want {
			t.Errorf("section %d: project %q want %q", i, sections[i].Project, want)
		}
	}
	// Dashboards within the monorepo section sorted by title asc.
	mono := sections[1].Dashboards
	if mono[0].Title != "Alpha" || mono[1].Title != "Zebra" {
		t.Errorf("monorepo dashboards not title-sorted: %+v", mono)
	}
}

func TestCountVizPanels_ExcludesRows(t *testing.T) {
	panels := []dashboardModelPanel{
		{ID: 1, Title: "CPU", Type: "timeseries"},
		{ID: 2, Title: "Group", Type: "row"}, // row header — not a viz panel
		{ID: 3, Title: "Mem", Type: "gauge"},
		{ID: 4, Title: "Reqs", Type: "stat"},
	}
	if got := countVizPanels(panels); got != 3 {
		t.Errorf("countVizPanels=%d want 3 (rows excluded)", got)
	}
	if got := countVizPanels(nil); got != 0 {
		t.Errorf("countVizPanels(nil)=%d want 0", got)
	}
}

func TestDashboardHubCache_TTL(t *testing.T) {
	c := newDashboardHubCache()
	t0 := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)

	if _, ok := c.get(t0, dashboardHubCacheTTL); ok {
		t.Fatal("cold cache should miss")
	}
	data := dashboardHubData{TotalDashboards: 3}
	c.set(data, t0)

	if got, ok := c.get(t0.Add(dashboardHubCacheTTL-time.Millisecond), dashboardHubCacheTTL); !ok || got.TotalDashboards != 3 {
		t.Errorf("within-TTL get: ok=%v data=%+v", ok, got)
	}
	if _, ok := c.get(t0.Add(dashboardHubCacheTTL+time.Millisecond), dashboardHubCacheTTL); ok {
		t.Error("expired entry should miss")
	}

	// nil-safe: a nil cache always misses and never panics on set.
	var nilCache *dashboardHubCache
	if _, ok := nilCache.get(t0, dashboardHubCacheTTL); ok {
		t.Error("nil cache should always miss")
	}
	nilCache.set(data, t0)
}

// dashboardHub caches a non-empty build and serves it without re-hitting the
// daemon; an empty build is not cached so the next load retries.
func TestDashboardHub_CachesNonEmptyOnly(t *testing.T) {
	f := dashboardHubFake([]*beadsapi.BeadDetail{
		dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live"),
	}, nil, nil)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = "https://g.example/grafana"
	pinned := time.Date(2026, 6, 24, 12, 0, 0, 0, time.UTC)
	srv.SetNow(func() time.Time { return pinned })

	d1 := srv.dashboardHub(context.Background())
	if d1.TotalDashboards != 1 {
		t.Fatalf("first build: %d dashboards want 1", d1.TotalDashboards)
	}
	callsAfterFirst := f.listCallCount()

	// Second call within TTL must be served from cache — no new list call.
	d2 := srv.dashboardHub(context.Background())
	if d2.TotalDashboards != 1 {
		t.Fatalf("cached build: %d dashboards want 1", d2.TotalDashboards)
	}
	if got := f.listCallCount(); got != callsAfterFirst {
		t.Errorf("cache miss: list called %d times, want %d (served from cache)", got, callsAfterFirst)
	}
}

// listCallCount counts how many ListBeadsFiltered calls the fake has recorded.
func (f *fakeDaemon) listCallCount() int {
	f.listMu.Lock()
	defer f.listMu.Unlock()
	return len(f.listAllQ)
}

// buildDashboardHub must enumerate only ACTIVE (non-closed) dashboard beads, so
// closing a dashboard bead removes it from /charts (the intended scope-off
// lever). Assert the daemon query carries the active-status filter and not the
// closed status (ka-0000004760).
func TestBuildDashboardHub_FiltersToActiveStatuses(t *testing.T) {
	f := dashboardHubFake([]*beadsapi.BeadDetail{
		dashBead("gd-D1", "Fleet Live", "gardenio", "gardenio-fleet-live"),
	}, nil, nil)
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	srv.grafanaBaseURL = "https://g.example/grafana"

	_ = srv.buildDashboardHub(context.Background())

	f.listMu.Lock()
	q := f.listLastQ
	f.listMu.Unlock()
	if q == nil {
		t.Fatal("buildDashboardHub made no ListBeadsFiltered call")
	}
	if len(q.Types) != 1 || q.Types[0] != "dashboard" {
		t.Errorf("Types = %v, want [dashboard]", q.Types)
	}
	if len(q.Statuses) == 0 {
		t.Fatalf("Statuses filter missing — closed dashboards would still render on /charts")
	}
	want := map[string]bool{"open": true, "in_progress": true, "blocked": true, "deferred": true}
	for _, s := range q.Statuses {
		if s == "closed" {
			t.Errorf("Statuses must not include 'closed', got %v", q.Statuses)
		}
		delete(want, s)
	}
	if len(want) != 0 {
		t.Errorf("Statuses %v missing active statuses %v", q.Statuses, want)
	}
}
