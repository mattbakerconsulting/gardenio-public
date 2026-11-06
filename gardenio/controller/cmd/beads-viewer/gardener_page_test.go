package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gardenerPageNow pins the wall clock so uptime/ago strings in the rendered
// thread are deterministic.
func gardenerPageNow() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

// seedGardenerPageFake builds a fakeDaemon with one gardener trigger bead and a
// three-run life-thread (a <- b <- c, c in-flight) stamped with schedule_id.
func seedGardenerPageFake(t *testing.T) *fakeDaemon {
	t.Helper()
	f := newFake()

	spec := map[string]any{
		"name":           "delivery",
		"terminal_goal":  "drain the ready queue",
		"run_budget":     100,
		"max_runs":       10,
		"runs_spawned":   3,
		"runs_completed": 2,
		"last_agent_id":  "gd-r3",
		"stop":           map[string]any{"max_runs": 10},
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:        "gd-cap",
		Title:     "Delivery gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    map[string]string{"role": "gardener", "gardener": string(raw)},
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	r1 := mkIteration("gd-r1", "gd-cap", "", 1, "closed", t0)
	r2 := mkIteration("gd-r2", "gd-cap", "gd-r1", 2, "closed", t0.Add(time.Hour))
	r3 := mkIteration("gd-r3", "gd-cap", "gd-r2", 3, "in_progress", t0.Add(2*time.Hour))
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{r2, r3, r1}, // shuffled to prove chain ordering
		Total: 3,
	}
	return f
}

func TestHandleGardenerDetail_RendersHeaderAndThread(t *testing.T) {
	f := seedGardenerPageFake(t)
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Header surface: name, terminal goal, counters, gardener id.
	for _, want := range []string{
		"delivery",
		"drain the ready queue",
		"gd-cap",
		"Life-thread",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q", want)
		}
	}

	// Life-thread: all three runs render, with the in-flight head labelled.
	for _, want := range []string{"gd-r1", "gd-r2", "gd-r3", "Iteration 3 (in progress)", "Iteration 1 of 10"} {
		if !strings.Contains(body, want) {
			t.Errorf("thread missing %q", want)
		}
	}

	// Newest-first: the live head (gd-r3) appears before the root (gd-r1).
	if strings.Index(body, "gd-r3") > strings.Index(body, "gd-r1") {
		t.Errorf("thread not newest-first: gd-r3 should precede gd-r1")
	}
}

// renderedIterationCards counts the iteration cards in a rendered gardener page.
// Each card is a <li class="bead-row gardener-iter..."> emitted by the
// gardener_iteration_card partial, so counting that marker counts the runs the
// life-thread actually rendered.
func renderedIterationCards(body string) int {
	return strings.Count(body, `class="bead-row gardener-iter`)
}

// TestHandleGardenerDetail_HeaderCountMatchesRenderedCards is the accuracy
// regression guard (ka-0000002549): the header's authoritative Iterations count
// must equal the number of iteration cards rendered in the life-thread, and —
// when the engine snapshot counter agrees with the live count — no drift
// annotation is shown. The seeded fixture has runs_spawned=3 and a known
// three-run thread, so the live count (3) matches the spec snapshot (3).
func TestHandleGardenerDetail_HeaderCountMatchesRenderedCards(t *testing.T) {
	f := seedGardenerPageFake(t)
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The fixture thread has exactly three runs.
	cards := renderedIterationCards(body)
	if cards != 3 {
		t.Fatalf("rendered %d iteration cards, want 3", cards)
	}

	// The header's authoritative Iterations count must equal the rendered card
	// count. The header renders "<dt>Iterations</dt><dd>3...", and the
	// life-thread heading echoes "(3 iterations)" — labeled "iterations", not
	// "runs", so it never conflates the rendered iteration cards with the
	// engine's lifetime spawn tally (bead ka-0000005522).
	if !strings.Contains(body, "<dt>Iterations</dt><dd>3") {
		t.Errorf("header Iterations count != rendered card count (3); body=%s", body)
	}
	if !strings.Contains(body, "(3 iterations)") {
		t.Errorf("life-thread heading missing the matching iteration count; body=%s", body)
	}

	// The engine lifetime tally is present as a labeled, demoted SECONDARY line
	// ("Engine count" / "lifetime tally"), not as a co-equal authority.
	if !strings.Contains(body, "Engine count") || !strings.Contains(body, "lifetime tally") {
		t.Errorf("engine tally not rendered as a labeled secondary; body=%s", body)
	}

	// runs_spawned (3) is within the prune gap of the live count (3): the
	// secondary is shown but NOT framed as pruned.
	if strings.Contains(body, "older runs pruned") {
		t.Errorf("unexpected 'older runs pruned' framing when spawn ≈ live; body=%s", body)
	}
}

// TestHandleGardenerDetail_EnginePrunedSecondary asserts that when the engine
// lifetime tally (RunsSpawned) outruns the live iteration count — the real
// shape at scale, e.g. 439 spawned vs 13 findable — the page (a) keeps the
// authoritative live count as the PRIMARY displayed Iterations value, (b) still
// renders the true number of cards, and (c) frames the SECONDARY engine line as
// "older runs pruned" so the gap reads as an explained lifetime tally rather
// than a silently-wrong / lost-data number (design doc §1.3, ka-0000003322).
func TestHandleGardenerDetail_EnginePrunedSecondary(t *testing.T) {
	f := seedGardenerPageFake(t)
	// Overwrite the gardener spec so runs_spawned (439) far outruns the live
	// three-run thread — the prune scenario.
	spec := map[string]any{
		"name":           "delivery",
		"terminal_goal":  "drain the ready queue",
		"max_runs":       1000,
		"runs_spawned":   439,
		"runs_completed": 413,
		"last_agent_id":  "gd-r3",
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"].Fields["gardener"] = string(raw)

	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Live count is the PRIMARY authority: 3 cards, header shows Iterations 3.
	if cards := renderedIterationCards(body); cards != 3 {
		t.Fatalf("rendered %d cards, want 3", cards)
	}
	if !strings.Contains(body, "<dt>Iterations</dt><dd>3") {
		t.Errorf("displayed Iterations is not the authoritative live count (3); body=%s", body)
	}

	// The secondary engine line surfaces both lifetime numbers honestly AND the
	// explanatory "older runs pruned" framing.
	if !strings.Contains(body, "439 spawned / 413 completed (older runs pruned)") {
		t.Errorf("missing framed engine secondary '439 spawned / 413 completed (older runs pruned)'; body=%s", body)
	}

	// Heading honesty (bead ka-0000005522): the Life-thread heading must echo
	// the rendered card count labeled "iterations" — NEVER "runs". With 439
	// runs spawned but only 3 iteration cards findable, calling the heading
	// "(3 runs)" re-conflates the per-iteration index with the lifetime spawn
	// tally — the exact contradiction the page's Iterations-vs-Engine-count
	// split exists to kill. Assert the honest label and the absence of any
	// "(N runs)" heading.
	if !strings.Contains(body, "(3 iterations)") {
		t.Errorf("life-thread heading not labeled '(3 iterations)'; body=%s", body)
	}
	if strings.Contains(body, "(3 runs)") || strings.Contains(body, "(439 runs)") {
		t.Errorf("life-thread heading still conflates iterations with runs (found a '(N runs)' label); body=%s", body)
	}
}

// TestHandleGardenerDetail_LegacySpecLessDerivesEngineCount guards the detail-page
// half of bug ka-0000005661: a legacy spawn_agent / bare-trigger gardener carries
// no GardenerSpec `gardener` blob, so its RunsSpawned/RunsCompleted decode to zero.
// Before the fix the page rendered "Engine count … 0 spawned / 0 completed"
// directly beneath an authoritative "Iterations 3" — a flat contradiction. The
// fix DERIVES the secondary line from the rendered life-thread and source-labels
// it "from life-thread" rather than "lifetime tally".
func TestHandleGardenerDetail_LegacySpecLessDerivesEngineCount(t *testing.T) {
	f := seedGardenerPageFake(t)
	// Strip the engine spec entirely — the legacy spawn_agent shape. The bead
	// keeps its role:gardener label + action=spawn_agent; gardenerViewFromBead
	// decodes a nil spec, so the engine tally is zero.
	delete(f.beads["gd-cap"].Fields, "gardener")
	f.beads["gd-cap"].Fields["action"] = "spawn_agent"

	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// Iterations is still the authoritative primary count (3 cards).
	if !strings.Contains(body, "<dt>Iterations</dt><dd>3") {
		t.Errorf("displayed Iterations is not the authoritative live count (3); body=%s", body)
	}
	// The derived secondary line agrees with the thread: 3 spawned, 2 completed
	// (run #3 is in_progress), and is source-labeled "from life-thread".
	if !strings.Contains(body, "3 spawned / 2 completed") {
		t.Errorf("missing derived engine line '3 spawned / 2 completed'; body=%s", body)
	}
	if !strings.Contains(body, "from life-thread") {
		t.Errorf("derived engine line not source-labeled 'from life-thread'; body=%s", body)
	}
	// The contradictory zero line must be gone, and the line must NOT claim to be
	// the engine's lifetime tally.
	if strings.Contains(body, "0 spawned / 0 completed") {
		t.Errorf("page still renders the contradictory '0 spawned / 0 completed'; body=%s", body)
	}
}

func TestHandleGardenerDetail_NotFound(t *testing.T) {
	f := newFake()
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-missing", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// TestHandleGardenerRunsSection_NotFound pins the fix for ka-0000007587: the lazy
// /gardener/{id}/runs fragment must 404 for an absent gardener, exactly like its
// parent /gardener/{id} page. Before the fix it returned 200 with an empty
// section, so a liveness probe hitting only the fragment read green for a
// gardener that does not exist.
func TestHandleGardenerRunsSection_NotFound(t *testing.T) {
	f := newFake()
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-missing/runs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for /runs of an absent gardener, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGardenerRunsSection_FixtureReturns404 asserts the /runs fragment
// applies the same fixture-gardener exclusion as the detail page (bug
// ka-0000008054): a fixture's synthetic runs must not leak through the lazy
// tail either (ka-0000007587).
func TestHandleGardenerRunsSection_FixtureReturns404(t *testing.T) {
	f := newFake()
	fix := mkFixtureGardenerBead()
	f.beads[fix.ID] = fix

	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardener/"+fix.ID+"/runs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for fixture /runs, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestHandleGardenerDetail_RendersTrajectory verifies the cross-run progress
// narrative (task ka-0000003359): a gardener whose progress_state carries scored
// runs + merged-MR events renders the trajectory strip with a "making progress"
// verdict and per-run sparkline bars.
func TestHandleGardenerDetail_RendersTrajectory(t *testing.T) {
	f := newFake()

	spec := map[string]any{
		"name":           "delivery",
		"runs_completed": 3,
		"progress_state": map[string]any{
			"runs": []map[string]any{
				{"agent_id": "a1", "progress_score": 2, "agent_state": "done"},
				{"agent_id": "a2", "progress_score": 0, "agent_state": "done"},
				{"agent_id": "a3", "progress_score": 5, "agent_state": "done"},
			},
			"events": []map[string]any{
				{"kind": "mr_merged", "target_id": "gd-mr1", "occurred_at": "2026-06-01T10:00:00Z"},
				{"kind": "child_closed", "target_id": "gd-c1", "occurred_at": "2026-06-01T10:05:00Z"},
			},
		},
	}
	raw, _ := json.Marshal(spec)
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Delivery gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener"},
		Fields: map[string]string{"role": "gardener", "gardener": string(raw)},
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"gardener-trajectory",       // the strip container
		"gardener-verdict-progress", // verdict styling hook
		"making progress",           // verdict text
		"gardener-spark-bar",        // per-run sparkline bars
		"Outcome rate",              // aggregate stat label
		"MRs merged",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("trajectory strip missing %q", want)
		}
	}
}

func TestHandleGardenerDetail_NoRuns(t *testing.T) {
	f := newFake()
	f.beads["gd-empty"] = &beadsapi.BeadDetail{
		ID:     "gd-empty",
		Title:  "Fresh gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"role": "gardener", "gardener": `{"name":"fresh"}`},
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-empty", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "has not spawned any iterations") {
		t.Errorf("expected empty-thread copy, got %s", w.Body.String())
	}
}

func TestHandleGardenerRunsSection_RendersOlderRuns(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Busy gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"role": "gardener", "gardener": `{"name":"busy","max_runs":100}`},
	}

	// Build a chain longer than gardenerEagerRuns so there is a lazy tail.
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	n := gardenerEagerRuns + 5
	beads := make([]*beadsapi.BeadDetail, 0, n)
	prior := ""
	for i := range n {
		id := "gd-run" + itoa(i)
		beads = append(beads, mkIteration(id, "gd-cap", prior, i+1, "closed", t0.Add(time.Duration(i)*time.Minute)))
		prior = id
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: beads, Total: n}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	// The eager page truncates and surfaces the lazy shell.
	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("eager: expected 200, got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "/gardener/gd-cap/runs") {
		t.Errorf("eager page missing lazy older-runs shell")
	}

	// The lazy section renders the tail (the oldest runs, beyond the eager head).
	req2 := httptest.NewRequest("GET", "/gardener/gd-cap/runs", nil)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("runs section: expected 200, got %d", w2.Code)
	}
	body := w2.Body.String()
	// The very first run (gd-run0, run #1) is the oldest and must be in the tail.
	if !strings.Contains(body, "gd-run0") {
		t.Errorf("lazy section missing oldest run gd-run0")
	}
	// A run inside the eager head (the newest, gd-run%d) must NOT be in the tail.
	newest := "gd-run" + itoa(n-1)
	if strings.Contains(body, newest+`"`) {
		t.Errorf("lazy section unexpectedly contains eager-head run %s", newest)
	}
}

// TestHandleGardenerPage_IterationCardRendersPerRunCost guards C2 (bug
// ka-0000001009): a gardener's life-thread iteration card surfaces that run's
// OWN reconciled cost via the shared usage-chip strip, so an operator can
// attribute a spend spike to a specific run on the detail page.
func TestHandleGardenerPage_IterationCardRendersPerRunCost(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Costly gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"role": "gardener", "gardener": `{"name":"costly","max_runs":100}`},
	}
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	run := mkIteration("gd-run0", "gd-cap", "", 1, "closed", t0)
	run.Fields["total_cost_usd"] = "7.89"
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{run}, Total: 1}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "$7.89") {
		t.Errorf("iteration card must render the run's own cost $7.89")
	}
}

// TestHandleGardenerRunsSection_CursorPagesTailWithoutDupes drives the stateless
// (schedule_key, id) cursor end to end through the HTTP route (design doc §7/§8,
// task ka-0000006972): a tail longer than one page must be served in bounded
// gardenerRunsPageSize chunks, each handing back the next page's cursor, with
// every older-than-eager run appearing exactly once across the whole walk and no
// eager-head run leaking into the tail.
func TestHandleGardenerRunsSection_CursorPagesTailWithoutDupes(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Busy gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"role": "gardener", "gardener": `{"name":"busy","max_runs":500}`},
	}

	// A tail spanning multiple pages: eager head + just over two full pages.
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	n := gardenerEagerRuns + 2*gardenerRunsPageSize + 3
	beads := make([]*beadsapi.BeadDetail, 0, n)
	prior := ""
	for i := range n {
		id := "gd-run" + itoa(i)
		// Stamp a monotonic schedule_key so ordering uses the fire-slot key and
		// the cursor is keyed on a real (schedule_key, id) pair.
		b := mkIteration(id, "gd-cap", prior, i+1, "closed", t0.Add(time.Duration(i)*time.Minute))
		b.Fields[fieldScheduleKey] = fmt.Sprintf("2026-06-01T%02d:%02d", i/60, i%60)
		beads = append(beads, b)
		prior = id
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: beads, Total: n}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	// Walk the cursor chain from the first tail page to exhaustion.
	seen := map[string]int{}
	pages := 0
	url := "/gardener/gd-cap/runs"
	for hops := 0; ; hops++ {
		if hops > n {
			t.Fatalf("cursor chain did not terminate after %d hops", hops)
		}
		req := httptest.NewRequest("GET", url, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("runs page %d: expected 200, got %d", pages, w.Code)
		}
		body := w.Body.String()
		pages++
		for i := range n {
			id := "gd-run" + itoa(i)
			if strings.Contains(body, `data-bead-id="`+id+`"`) {
				seen[id]++
			}
		}
		next, ok := nextGardenerRunsURL(body)
		if !ok {
			break
		}
		url = next
	}

	// Multi-page: the tail (n-eager = 53 cards) spans 3 pages of <=25.
	if pages < 3 {
		t.Errorf("expected the tail to span multiple pages, walked only %d", pages)
	}
	// Every run older than the eager head appears exactly once.
	for i := 0; i < n-gardenerEagerRuns; i++ {
		id := "gd-run" + itoa(i)
		if seen[id] != 1 {
			t.Errorf("tail run %s seen %d times, want exactly 1", id, seen[id])
		}
	}
	// No eager-head run (the newest gardenerEagerRuns) leaks into the tail.
	for i := n - gardenerEagerRuns; i < n; i++ {
		id := "gd-run" + itoa(i)
		if seen[id] != 0 {
			t.Errorf("eager-head run %s leaked into the tail (seen %d)", id, seen[id])
		}
	}
}

// TestHandleGardenerRunsSection_NoEmptyTailDeadEnd verifies that when the lazy
// older-runs section has no findable tail, it renders nothing rather than a
// literal "No older runs." dead-end (which reads like an error). The partial is
// swapped in over the "Loading older runs…" shell via hx-swap="outerHTML", so
// empty output simply removes the shell. Pairs with invariant no_empty_tail
// (gd-5nBd9Vukd6).
func TestHandleGardenerRunsSection_NoEmptyTailDeadEnd(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Quiet gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"role": "gardener", "gardener": `{"name":"quiet","max_runs":100}`},
	}

	// A chain that fits entirely within the eager head leaves no lazy tail.
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	n := min(2, gardenerEagerRuns)
	beads := make([]*beadsapi.BeadDetail, 0, n)
	prior := ""
	for i := range n {
		id := "gd-run" + itoa(i)
		beads = append(beads, mkIteration(id, "gd-cap", prior, i+1, "closed", t0.Add(time.Duration(i)*time.Minute)))
		prior = id
	}
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: beads, Total: n}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-cap/runs", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("runs section: expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "No older runs") {
		t.Errorf("lazy section rendered a 'No older runs.' dead-end: %q", body)
	}
	if strings.Contains(body, "gardener-thread-older") {
		t.Errorf("lazy section rendered an older-runs list when there was no tail: %q", body)
	}
}

// TestHandleGardenerDetail_OutcomeFirstCards verifies that each iteration card
// leads with the run's outcome (wrap-up tldr) rather than a bare agent name,
// demotes the agent name to metadata, and surfaces the per-run outcome chips
// (beads-closed count + exit reason). Regression guard for ka-0000001187.
func TestHandleGardenerDetail_OutcomeFirstCards(t *testing.T) {
	f := newFake()
	f.beads["gd-cap"] = &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Outcome gardener",
		Type:   "trigger",
		Status: "open",
		Fields: map[string]string{"role": "gardener", "gardener": `{"name":"outcome","max_runs":10}`},
	}

	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// run #1: rich wrap-up with a tldr + beads_closed → tldr is the headline,
	// "2 closed" chip, clean exit chip.
	r1 := mkIteration("gd-agent-alpha", "gd-cap", "", 1, "closed", t0)
	r1.Fields["wrapup"] = `{"tldr":"Merged MR 7; drained 2 ready beads","accomplishments":"x","beads_closed":["gd-x","gd-y"]}`
	// run #2: no wrap-up but it closed → derived/fallback outcome, "no wrap-up"
	// exit chip, name still demoted to meta.
	r2 := mkIteration("gd-agent-beta", "gd-cap", "gd-agent-alpha", 2, "closed", t0.Add(time.Hour))

	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{r1, r2},
		Total: 2,
	}
	mux := mkServerWithNow(t, f, gardenerPageNow)

	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The tldr is the PRIMARY (linked) text of run #1's card.
	if !strings.Contains(body, "Merged MR 7; drained 2 ready beads") {
		t.Fatalf("card missing wrap-up tldr headline; body=%s", body)
	}
	// The agent name is demoted to the meta row, not the headline.
	if !strings.Contains(body, `class="meta agent-name-meta"`) || !strings.Contains(body, "gd-agent-alpha") {
		t.Errorf("agent name not demoted to meta row")
	}

	// Outcome chips: beads-closed count + exit reasons.
	for _, want := range []string{"2 closed", "clean exit", "no wrap-up"} {
		if !strings.Contains(body, want) {
			t.Errorf("card missing outcome chip %q", want)
		}
	}

	// The tldr headline is the linked title text — it should sit inside the
	// bead-row-title anchor, i.e. before the demoted agent-name meta span of the
	// SAME (newest, run #2) card. We assert outcome-first layout by checking the
	// first card's headline anchor precedes its meta-row name span.
	firstTitle := strings.Index(body, `class="bead-row-title`)
	firstNameMeta := strings.Index(body, `class="meta agent-name-meta"`)
	if firstTitle < 0 || firstNameMeta < 0 || firstTitle > firstNameMeta {
		t.Errorf("headline title (idx %d) should precede demoted agent name (idx %d)", firstTitle, firstNameMeta)
	}
}
