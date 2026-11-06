package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// poolFake routes ListBeadsFiltered by query SHAPE so a buildPoolView test can
// seed the live (open+in_progress) and recent (closed) member lists
// independently. Embeds *fakeDaemon for ListProjectBeads (projectsResp) and the
// rest of the surface.
type poolFake struct {
	*fakeDaemon

	liveResp   *beadsapi.ListBeadsResult // Types=[agent], open+in_progress
	closedResp *beadsapi.ListBeadsResult // Types=[agent], closed

	liveErr   error
	closedErr error
}

func (f *poolFake) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if slices.Contains(q.Types, "agent") {
		if slices.Contains(q.Statuses, "closed") {
			return orEmpty(f.closedResp), f.closedErr
		}
		return orEmpty(f.liveResp), f.liveErr
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func poolTestNow() time.Time { return time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC) }

func poolServer(f *poolFake) *Server {
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.SetNow(poolTestNow)
	return srv
}

// poolMember builds a prewarmed-pool member agent bead. engaged=true attaches a
// task (a member handed real work); createdAgo/updatedAgo are offsets back from
// poolTestNow. state is the agent_state.
func poolMember(id, project, role, state string, engaged bool, createdAgo, updatedAgo time.Duration) *beadsapi.BeadDetail {
	fields := map[string]string{
		"agent":       "prewarmed-1780000000-" + id,
		"project":     project,
		"role":        role,
		"agent_state": state,
	}
	if engaged {
		fields["task_id"] = "gd-task-" + id
		fields["spawn_source"] = "slack-thread-pool"
	}
	now := poolTestNow()
	return &beadsapi.BeadDetail{
		ID:        "gd-" + id,
		Type:      "agent",
		Status:    "in_progress",
		Title:     fields["agent"],
		Fields:    fields,
		CreatedAt: now.Add(-createdAgo),
		UpdatedAt: now.Add(-updatedAgo),
	}
}

func poolProject(name string, roles []beadsapi.PrewarmedRolePool) beadsapi.ProjectInfo {
	return beadsapi.ProjectInfo{
		Name: name,
		PrewarmedPool: &beadsapi.PrewarmedPoolConfig{
			Enabled: true,
			Roles:   roles,
		},
	}
}

// TestBuildPoolView_FullAssembly is the happy path: a configured pool joins its
// live members (warm + engaged), a starved configured pool with no warm members
// still gets a row, a live pool with no config reads as drift, and the recent
// closed window classifies hits vs expiries.
func TestBuildPoolView_FullAssembly(t *testing.T) {
	f := &poolFake{fakeDaemon: newFake()}
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": poolProject("gardenio", []beadsapi.PrewarmedRolePool{
			{Role: "crew", Mode: "crew", MinSize: 2, MaxSize: 5},
			{Role: "thread", Mode: "crew", MinSize: 3, MaxSize: 6}, // starved: no warm members live
		}),
	}
	// Live members:
	//  - gardenio/crew: 2 warm + 1 engaged (current 3, warm 2 == min → healthy)
	//  - gardenio/thread: 1 warm only (warm 1 < min 3 → below min / starved)
	//  - kbeads/crew: 1 warm, NO config → drift
	f.liveResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		poolMember("w1", "gardenio", "crew", "prewarmed", false, 3*time.Hour, 3*time.Hour),
		poolMember("w2", "gardenio", "crew", "prewarmed", false, 30*time.Minute, 30*time.Minute),
		poolMember("e1", "gardenio", "crew", "working", true, 2*time.Hour, 10*time.Minute),
		poolMember("t1", "gardenio", "thread", "prewarmed", false, 1*time.Hour, 1*time.Hour),
		poolMember("d1", "kbeads", "crew", "prewarmed", false, 45*time.Minute, 45*time.Minute),
		// A non-pool agent (no prewarmed- prefix) must be ignored entirely.
		agentBead("real1", "crew-real", "crew", "gardenio", "working"),
	}}
	// Recent closed within window: 1 hit (engaged) + 1 expiry (unengaged) for
	// gardenio/crew; 1 ancient closed member outside the window is dropped.
	f.closedResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		poolMember("h1", "gardenio", "crew", "closed", true, 5*time.Hour, 2*time.Hour),
		poolMember("x1", "gardenio", "crew", "closed", false, 5*time.Hour, 3*time.Hour),
		poolMember("old", "gardenio", "crew", "closed", false, 40*time.Hour, 40*time.Hour),
	}}

	view := poolServer(f).buildPoolView(context.Background(), poolTestNow())

	if view.Error != "" {
		t.Fatalf("unexpected page error: %q", view.Error)
	}
	if view.ConfiguredPools != 2 {
		t.Errorf("ConfiguredPools = %d, want 2", view.ConfiguredPools)
	}
	if view.DriftPools != 1 {
		t.Errorf("DriftPools = %d, want 1", view.DriftPools)
	}
	if view.TotalWarm != 4 { // w1,w2 (crew) + t1 (thread) + d1 (kbeads drift)
		t.Errorf("TotalWarm = %d, want 4", view.TotalWarm)
	}
	if view.TotalEngaged != 1 {
		t.Errorf("TotalEngaged = %d, want 1", view.TotalEngaged)
	}
	if view.StarvedPools != 1 { // gardenio/thread
		t.Errorf("StarvedPools = %d, want 1", view.StarvedPools)
	}

	rows := indexPoolRows(view.Rows)

	crew, ok := rows[poolKey{"gardenio", "crew"}]
	if !ok {
		t.Fatalf("missing gardenio/crew row")
	}
	if crew.Current != 3 || crew.Warm != 2 || crew.Engaged != 1 {
		t.Errorf("gardenio/crew occupancy = current %d warm %d engaged %d, want 3/2/1", crew.Current, crew.Warm, crew.Engaged)
	}
	if crew.BelowMin || crew.OverMax || crew.Drift {
		t.Errorf("gardenio/crew should be healthy, got belowMin=%v overMax=%v drift=%v", crew.BelowMin, crew.OverMax, crew.Drift)
	}
	if crew.RecentHits != 1 || crew.RecentExpired != 1 {
		t.Errorf("gardenio/crew recent = hits %d exp %d, want 1/1 (ancient dropped)", crew.RecentHits, crew.RecentExpired)
	}
	// Oldest warm member is w1 (3h), newest is w2 (30m).
	if crew.OldestWarmAge != "3h" || crew.NewestWarmAge != "30m" {
		t.Errorf("gardenio/crew warm age = %q / %q, want 3h / 30m", crew.OldestWarmAge, crew.NewestWarmAge)
	}

	thread, ok := rows[poolKey{"gardenio", "thread"}]
	if !ok {
		t.Fatalf("missing gardenio/thread row")
	}
	if !thread.Configured || !thread.BelowMin {
		t.Errorf("gardenio/thread want configured+belowMin, got configured=%v belowMin=%v", thread.Configured, thread.BelowMin)
	}
	if thread.Warm != 1 || thread.MinSize != 3 {
		t.Errorf("gardenio/thread warm=%d min=%d, want 1/3", thread.Warm, thread.MinSize)
	}

	drift, ok := rows[poolKey{"kbeads", "crew"}]
	if !ok {
		t.Fatalf("missing kbeads/crew drift row")
	}
	if !drift.Drift || drift.Configured {
		t.Errorf("kbeads/crew want drift+unconfigured, got drift=%v configured=%v", drift.Drift, drift.Configured)
	}
}

// TestBuildPoolView_ConfiguredEmptyPool: a configured pool with ZERO live
// members still renders a starved row (the starvation signal the epic is about).
func TestBuildPoolView_ConfiguredEmptyPool(t *testing.T) {
	f := &poolFake{fakeDaemon: newFake()}
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": poolProject("gardenio", []beadsapi.PrewarmedRolePool{
			{Role: "crew", Mode: "crew", MinSize: 2, MaxSize: 4},
		}),
	}
	f.liveResp = &beadsapi.ListBeadsResult{}

	view := poolServer(f).buildPoolView(context.Background(), poolTestNow())
	if len(view.Rows) != 1 {
		t.Fatalf("want 1 row for the configured-but-empty pool, got %d", len(view.Rows))
	}
	row := view.Rows[0]
	if !row.Configured || !row.BelowMin || row.Current != 0 {
		t.Errorf("empty configured pool: configured=%v belowMin=%v current=%d, want true/true/0", row.Configured, row.BelowMin, row.Current)
	}
	if view.StarvedPools != 1 {
		t.Errorf("StarvedPools = %d, want 1", view.StarvedPools)
	}
	if row.OldestWarmAge != "" {
		t.Errorf("empty pool should have no warm age, got %q", row.OldestWarmAge)
	}
}

// TestBuildPoolView_OverMax: current occupancy above the configured max flags
// OverMax.
func TestBuildPoolView_OverMax(t *testing.T) {
	f := &poolFake{fakeDaemon: newFake()}
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": poolProject("gardenio", []beadsapi.PrewarmedRolePool{
			{Role: "crew", Mode: "crew", MinSize: 1, MaxSize: 2},
		}),
	}
	f.liveResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		poolMember("a", "gardenio", "crew", "prewarmed", false, time.Hour, time.Hour),
		poolMember("b", "gardenio", "crew", "prewarmed", false, time.Hour, time.Hour),
		poolMember("c", "gardenio", "crew", "prewarmed", false, time.Hour, time.Hour),
	}}
	view := poolServer(f).buildPoolView(context.Background(), poolTestNow())
	if len(view.Rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(view.Rows))
	}
	if !view.Rows[0].OverMax {
		t.Errorf("want OverMax=true for current 3 > max 2, got false")
	}
}

// TestBuildPoolView_LiveFetchError: the live-member fetch is the one hard
// dependency — its failure surfaces a page-level error.
func TestBuildPoolView_LiveFetchError(t *testing.T) {
	f := &poolFake{fakeDaemon: newFake()}
	f.liveErr = errors.New("boom")
	view := poolServer(f).buildPoolView(context.Background(), poolTestNow())
	if view.Error == "" {
		t.Fatalf("want page-level error on live-fetch failure")
	}
}

// TestBuildPoolView_RecentFetchDegrades: a closed-window fetch error degrades
// only the hit/expiry counters, never the page.
func TestBuildPoolView_RecentFetchDegrades(t *testing.T) {
	f := &poolFake{fakeDaemon: newFake()}
	f.liveResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		poolMember("w1", "gardenio", "crew", "prewarmed", false, time.Hour, time.Hour),
	}}
	f.closedErr = errors.New("closed boom")
	view := poolServer(f).buildPoolView(context.Background(), poolTestNow())
	if view.Error != "" {
		t.Fatalf("recent-fetch failure must not fail the page, got %q", view.Error)
	}
	if len(view.Rows) != 1 || view.Rows[0].Warm != 1 {
		t.Fatalf("live members must still render despite recent-fetch error")
	}
	if view.Rows[0].RecentHits != 0 || view.Rows[0].RecentExpired != 0 {
		t.Errorf("recent counters should be zero when the window fetch failed")
	}
}

// TestHandlePool_Page renders the full page and asserts the key affordances are
// present.
func TestHandlePool_Page(t *testing.T) {
	f := &poolFake{fakeDaemon: newFake()}
	f.projectsResp = map[string]beadsapi.ProjectInfo{
		"gardenio": poolProject("gardenio", []beadsapi.PrewarmedRolePool{
			{Role: "crew", Mode: "crew", MinSize: 2, MaxSize: 5},
		}),
	}
	f.liveResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		poolMember("w1", "gardenio", "crew", "prewarmed", false, time.Hour, time.Hour),
		poolMember("d1", "kbeads", "wanderer", "prewarmed", false, time.Hour, time.Hour),
	}}

	srv := poolServer(f)
	r := httptest.NewRequest(http.MethodGet, "/pool", nil)
	w := httptest.NewRecorder()
	srv.handlePool(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"Prewarmed pool",
		`href="/agents?project=gardenio"`,
		`href="/role/crew"`,
		"drift",          // kbeads/wanderer has no config
		"below&nbsp;min", // gardenio/crew warm 1 < min 2
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q\nbody=%s", want, body)
		}
	}
}

// TestNavLink_PoolPresent verifies the layout nav carries a /pool link (vibe
// mode, mirroring the Roles link gating).
func TestNavLink_PoolPresent(t *testing.T) {
	f := newFake()
	f.userResp = vibeUserResp()
	mux := mkServer(t, f)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `href="/pool"`) {
		t.Errorf("layout missing /pool nav link\nbody=%s", w.Body.String())
	}
}

func TestPoolAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{90 * time.Second, "1m"},
		{45 * time.Minute, "45m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
		{-time.Second, "0s"},
	}
	for _, c := range cases {
		if got := poolAge(c.d); got != c.want {
			t.Errorf("poolAge(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func indexPoolRows(rows []poolStatsRow) map[poolKey]poolStatsRow {
	out := map[poolKey]poolStatsRow{}
	for _, r := range rows {
		out[poolKey{r.Project, r.Role}] = r
	}
	return out
}
