package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedMapsFake serves three gardeners via the type=trigger query: an active
// (live) "delivery" gardener, a disabled "watchdog", and a closed "retired"
// gardener. The board defaults to the live fleet, so only "delivery" renders
// without ?all=1.
func seedMapsFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()

	mk := func(id, name, goal string, status string, enabled bool) *beadsapi.BeadDetail {
		spec := map[string]any{
			"name":           name,
			"terminal_goal":  goal,
			"runs_spawned":   6,
			"runs_completed": 5,
		}
		raw, _ := json.Marshal(spec)
		fields := map[string]string{"role": "gardener", "gardener": string(raw)}
		if !enabled {
			fields["enabled"] = "false"
		}
		return &beadsapi.BeadDetail{
			ID:        id,
			Title:     name,
			Type:      "trigger",
			Status:    status,
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    fields,
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}
	}

	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			mk("gd-deliv", "delivery", "drain the ready queue", "open", true),
			mk("gd-watch", "watchdog", "watch the cycle health", "open", false),
			mk("gd-retire", "retired", "ship the migration", "closed", false),
		},
		Total: 3,
	}
	return f
}

func mapsNow() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

// TestHandleMaps_RendersLiveBoardByDefault: /maps renders the card grid scoped
// to the live fleet (active + enabled + trigger), so only the delivery gardener
// shows. Each card links to /gardener/{id}; the nav and trigger node are present.
func TestHandleMaps_RendersLiveBoardByDefault(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	for _, want := range []string{
		"maps-board",
		"delivery",
		"drain the ready queue",
		`href="/gardener/gd-deliv"`,
		"maps-trigger", // trigger node
		"/triggers",    // trigger node links to /triggers
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Live-only scope: the disabled watchdog and closed retired gardener must NOT
	// render in the default view.
	for _, absent := range []string{`href="/gardener/gd-watch"`, `href="/gardener/gd-retire"`} {
		if strings.Contains(body, absent) {
			t.Errorf("default live scope leaked non-live gardener: %q", absent)
		}
	}
}

// TestHandleMaps_ScopeToggleIsolatedActiveChip guards the ka-0000002434 fix:
// the `.maps-scope` paragraph carries THREE toggles that each use `.is-active`
// (scope, group-by-owner, runs). The maps.spec.js Playwright assertion
// `.maps-scope-links a.is-active → toHaveText('Live fleet')` is strict
// (single-element), so the scope anchors MUST live inside their own
// `.maps-scope-links` fence — otherwise the bare selector matches the
// group toggle's active "None" chip too and the e2e flakes. This test pins
// the markup contract the spec relies on: a `.maps-scope-links` span exists,
// the group toggle has its own `.maps-group-links` fence, and the active
// scope chip text is "Live fleet" by default / "All gardeners" under ?all=1.
func TestHandleMaps_ScopeToggleIsolatedActiveChip(t *testing.T) {
	for _, tc := range []struct {
		name       string
		path       string
		wantActive string
	}{
		{name: "default", path: "/maps", wantActive: "Live fleet"},
		{name: "all", path: "/maps?all=1", wantActive: "All gardeners"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)
			req := httptest.NewRequest("GET", tc.path, nil)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
			}
			body := w.Body.String()

			// The scope toggle and the group toggle each have their own fence,
			// so the e2e's `.maps-scope-links a.is-active` resolves to exactly
			// one anchor.
			for _, want := range []string{`class="maps-scope-links"`, `class="maps-group-links"`} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q", want)
				}
			}

			// Exactly one active scope chip inside the scope fence, with the
			// expected text. Extract the .maps-scope-links span and assert.
			seg := body
			if i := strings.Index(seg, `class="maps-scope-links"`); i >= 0 {
				seg = seg[i:]
				if j := strings.Index(seg, "</span>"); j >= 0 {
					seg = seg[:j]
				}
			} else {
				t.Fatal("maps-scope-links span not found")
			}
			if got := strings.Count(seg, "is-active"); got != 1 {
				t.Errorf("scope fence has %d is-active chips, want 1; seg=%s", got, seg)
			}
			if !strings.Contains(seg, tc.wantActive) {
				t.Errorf("active scope chip text: want %q in %q", tc.wantActive, seg)
			}
		})
	}
}

// TestHandleMaps_RendersSearchBox: the client-side search box (maps2 v2,
// ka-0000006434) renders above the board, and each card carries a lowercased
// data-search haystack covering its name/title/goal/owner/id so the JS filter
// has something to match.
func TestHandleMaps_RendersSearchBox(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"data-maps-search",       // the search box container
		`id="maps-search-input"`, // the input
		"data-search=",           // per-card haystack attribute
		"drain the ready queue",  // goal folded into the haystack (lowercased)
		"maps-search-input",      // the wiring script references the input
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}
}

// TestHandleMaps_AllScopeIncludesClosed: ?all=1 widens the board to every
// gardener, including the disabled and closed ones.
func TestHandleMaps_AllScopeIncludesClosed(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)

	req := httptest.NewRequest("GET", "/maps?all=1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`href="/gardener/gd-deliv"`,
		`href="/gardener/gd-watch"`,
		`href="/gardener/gd-retire"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("?all=1 board missing %q", want)
		}
	}
}

// TestHandleMaps_RendersOwnedEpicRollup: a gardener pinned to an epic via the
// gardener-epic:<id> label renders the owned-epic rollup chip on its card, with
// the closed/total count, the percentage, and a link to the epic (task
// ka-0000001266).
func TestHandleMaps_RendersOwnedEpicRollup(t *testing.T) {
	f := newFake()
	spec := map[string]any{"name": "epic gardener", "runs_spawned": 6, "runs_completed": 5}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:        "gd-epiccap",
			Title:     "epic gardener",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio", "gardener-epic:gd-ownedepic"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}},
		Total: 1,
	}
	f.epicSubtreeCount = map[string]int{
		"gd-ownedepic":        40,
		"gd-ownedepic|closed": 12,
	}

	mux := mkServerWithNow(t, f, mapsNow)
	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"maps-epic",            // chip class
		`href="/gd-ownedepic"`, // links to the epic
		"12/40 done · 30%",     // rollup label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("owned-epic chip missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleMaps_NavLinkPointsAtMaps: the "Maps" nav link in layout.html points
// at /maps (the promotion target — ka-0000002415).
func TestHandleMaps_NavLinkPointsAtMaps(t *testing.T) {
	// Maps is a vibe-only nav link after gb-ux #3 (ka-0000003340); render as a
	// vibe-mode viewer so the gated link is present.
	f := seedMapsFake(t)
	f.userResp = vibeUserResp()
	mux := mkServerWithNow(t, f, mapsNow)
	req := httptest.NewRequest("GET", "/maps", nil)
	req.Header.Set("X-Auth-Request-Email", authorEmail)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `href="/maps">Maps</a>`) {
		t.Errorf("nav Maps link does not point at /maps")
	}
}

// TestHandleMaps_DegradesOnLoadError: a daemon failure still returns 200 with an
// empty board (degrade-render), so the nav link never 500s.
func TestHandleMaps_DegradesOnLoadError(t *testing.T) {
	f := newFake()
	f.listErr = &beadsapi.APIError{StatusCode: http.StatusInternalServerError, Message: "boom"} // make the gardener queries fail
	mux := mkServerWithNow(t, f, mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected degraded 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Failed to load gardeners") {
		t.Errorf("expected load-error notice in degraded body")
	}
}

// TestLoadGardenerRows_CopiesTrajectoryRuns guards the projection add for /maps
// (ka-0000001644): gardenerRowFromBead must copy the gardener's trajectory run
// glyphs (built from the embedded ProgressState, no extra fetch) into row.Runs
// so the board can read the recent-run strip directly.
func TestLoadGardenerRows_CopiesTrajectoryRuns(t *testing.T) {
	f := newFake()
	spec := map[string]any{
		"name":          "runner",
		"terminal_goal": "do the thing",
		"progress_state": map[string]any{
			"runs": []map[string]any{
				{"progress_score": 3},
				{"progress_score": 5},
			},
		},
	}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:        "gd-runner",
			Title:     "runner",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}},
		Total: 1,
	}
	srv := gardenerTestServer(t, f)
	rows, _, _, err := srv.loadGardenerRows(t.Context())
	if err != nil {
		t.Fatalf("loadGardenerRows: %v", err)
	}
	var found bool
	for _, r := range rows {
		if r.ID == "gd-runner" {
			found = true
			if len(r.Runs) != 2 {
				t.Errorf("row.Runs = %d glyphs, want 2 (copied from trajectory)", len(r.Runs))
			}
		}
	}
	if !found {
		t.Fatalf("gd-runner not found in rows")
	}
}

// TestHandleMaps_RunStripPills: a gardener with ProgressState runs renders the
// recent-run strip pills on its card.
func TestHandleMaps_RunStripPills(t *testing.T) {
	f := newFake()
	// A gardener whose ProgressState carries scored runs → trajectory glyphs →
	// row.Runs → the board strip pills.
	spec := map[string]any{
		"name":          "runner",
		"terminal_goal": "do the thing",
		"progress_state": map[string]any{
			"runs": []map[string]any{
				{"progress_score": 3},
				{"progress_score": 0},
				{"progress_score": 5},
			},
		},
	}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:        "gd-runner",
			Title:     "runner",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}},
		Total: 1,
	}
	mux := mkServerWithNow(t, f, mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "gardener-spark-bar") {
		t.Errorf("expected recent-run strip pills (gardener-spark-bar) for a gardener with runs")
	}
}

// TestHandleMaps_TrajectorySparkline: a gardener with >= 2 ProgressState runs
// renders the verdict-over-time sparkline (SVG polyline) as the DEFAULT
// recent-run display, the discrete pills remain in the DOM as the toggle
// fallback, and the board exposes the Sparkline⇄Pills toggle (task
// ka-0000001693).
func TestHandleMaps_TrajectorySparkline(t *testing.T) {
	f := newFake()
	spec := map[string]any{
		"name":          "runner",
		"terminal_goal": "do the thing",
		"progress_state": map[string]any{
			"runs": []map[string]any{
				{"progress_score": 3, "agent_state": "done"},
				{"progress_score": 0, "agent_state": "done"},
				{"progress_score": 5, "agent_state": "done"},
			},
		},
	}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:        "gd-runner",
			Title:     "runner",
			Type:      "trigger",
			Status:    "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}},
		Total: 1,
	}
	mux := mkServerWithNow(t, f, mapsNow)

	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, "maps-runs-spark") {
		t.Errorf("expected the verdict-over-time sparkline (maps-runs-spark) for a gardener with >=2 runs")
	}
	if !strings.Contains(body, "maps-spark-line") || !strings.Contains(body, "maps-spark-dot") {
		t.Errorf("expected the sparkline polyline + outcome dots in the rendered card")
	}
	// The discrete pills survive in the DOM as the toggle/fallback.
	if !strings.Contains(body, "gardener-spark-bar") {
		t.Errorf("expected the discrete strip pills to remain as the toggle fallback")
	}
	// The board-level Sparkline⇄Pills toggle is present.
	if !strings.Contains(body, "maps-runs-toggle") || !strings.Contains(body, `data-runs-mode="pills"`) {
		t.Errorf("expected the Sparkline⇄Pills board toggle")
	}
}

// TestHandleMaps_LiveAgentOverlay: a running gardener's card names and links its
// currently-live agent (the last-agent bead), with the pulsing dot, instead of
// a bare "running now" (maps2 v2, ka-0000000595). The agent identity rides on
// the same batch-fetched last-agent bead — no extra daemon fan-out.
// TestHandleMaps_CostContextChips renders /maps end-to-end and asserts the
// cost + context chips appear on a gardener card (maps2 v2, ka-0000001668). Cost
// rides on the gardener bead's controller-maintained gardener_spend_usd; context
// rides on the live agent bead's context_tokens — neither adds daemon fan-out.
func TestHandleMaps_CostContextChips(t *testing.T) {
	f := newFake()
	now := mapsNow()

	spec := map[string]any{"name": "delivery", "terminal_goal": "drain the queue", "last_agent_id": "gd-agentCtx"}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-deliv", Title: "delivery", Type: "trigger", Status: "open",
			Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields: map[string]string{
				"role":               "gardener",
				"gardener":           string(raw),
				"gardener_spend_usd": "12.34",
			},
			UpdatedAt: now,
		}},
		Total: 1,
	}
	// The live agent bead — working + fresh, reporting context occupancy.
	f.beads["gd-agentCtx"] = &beadsapi.BeadDetail{
		ID: "gd-agentCtx", Status: "in_progress",
		Fields: map[string]string{
			"agent":          "gardenio-delivery-drive-abc1",
			"agent_state":    "working",
			"context_tokens": "128000",
		},
		UpdatedAt: now.Add(-1 * time.Minute),
	}

	mux := mkServerWithNow(t, f, mapsNow)
	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"maps-cost",    // cost chip class
		"$12.34",       // the rolled-up spend
		"maps-ctx",     // context chip class
		"maps-ctx-bar", // the occupancy mini-bar
		"128.0k",       // humanized context token count
	} {
		if !strings.Contains(body, want) {
			t.Errorf("cost/context chip missing %q\nbody=%s", want, body)
		}
	}
}

func TestHandleMaps_LiveAgentOverlay(t *testing.T) {
	f := newFake()
	now := mapsNow()

	spec := map[string]any{"name": "delivery", "terminal_goal": "drain the queue", "last_agent_id": "gd-agentLive"}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-deliv", Title: "delivery", Type: "trigger", Status: "open",
			Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
			Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
			UpdatedAt: now,
		}},
		Total: 1,
	}
	// The live agent bead — working + fresh, so it reads as alive.
	f.beads["gd-agentLive"] = &beadsapi.BeadDetail{
		ID: "gd-agentLive", Status: "in_progress",
		Fields:    map[string]string{"agent": "gardenio-delivery-drive-abc1", "agent_state": "working"},
		UpdatedAt: now.Add(-1 * time.Minute),
	}

	mux := mkServerWithNow(t, f, mapsNow)
	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"gardenio-delivery-drive-abc1", // the live agent's name
		`href="/gd-agentLive"`,         // links to the agent bead
		"maps-live-agent",              // the overlay link class
		"maps-running-dot",             // pulsing dot — agent is alive
	} {
		if !strings.Contains(body, want) {
			t.Errorf("live-agent overlay missing %q\nbody=%s", want, body)
		}
	}
}

// TestHandleMaps_SuccessorAffordanceRenders proves the card-graph AUTHORING
// affordance (task ka-0000005273 / "C3"): every live gardener card carries a
// "+ successor" handle that opens the authoring sheet targeting that gardener.
func TestHandleMaps_SuccessorAffordanceRenders(t *testing.T) {
	mux := mkServerWithNow(t, seedMapsFake(t), mapsNow)
	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"maps-card-successor",
		`hx-get="/gardener/gd-deliv/successor"`,
		`hx-target="#quick-spawn-host"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("successor affordance missing %q", want)
		}
	}
}

// TestHandleMaps_PendingSuccessorWorkflow proves a gardener that DECLARES a
// chain.to (but whose heir has not materialized) renders a PENDING workflow
// rail — a drawn successor visible before the engine spawns it (C3).
func TestHandleMaps_PendingSuccessorWorkflow(t *testing.T) {
	f := newFake()
	spec := map[string]any{"name": "research", "terminal_goal": "design done"}
	raw, _ := json.Marshal(spec)
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID:     "gd-research",
			Title:  "research",
			Type:   "trigger",
			Status: "open",
			Labels: []string{"trigger:schedule", "role:gardener", "project:gardenio", "gardener-epic:gd-epicRoot"},
			Fields: map[string]string{
				"role":     "gardener",
				"gardener": string(raw),
				"chain":    `{"to":{"phase":"impl","terminal_goal":"impl done"}}`,
			},
			UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
		}},
		Total: 1,
	}

	mux := mkServerWithNow(t, f, mapsNow)
	req := httptest.NewRequest("GET", "/maps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"maps-workflows",    // a workflow rail rendered
		"maps-rail-pending", // the pending heir node
		"pending successor", // the pending caption
		"impl",              // the declared heir's phase label
		"will chain to",     // the pending arrow label
	} {
		if !strings.Contains(body, want) {
			t.Errorf("pending successor workflow missing %q\nbody=%s", want, body)
		}
	}
}
