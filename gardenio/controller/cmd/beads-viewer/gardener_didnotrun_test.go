package main

// Tests for the never-ran spawn-shell discriminator (bug ka-0000006805).
//
// A gardener stamps the SAME owner key (trigger_id / schedule_id) on three beads
// per fire: a scheduler eval-tick `event`, a per-fire `task`, and the real
// `agent` run. The count paths already constrain Types:["agent"], so the
// eval-tick events/tasks never inflate counts. The residual conflation is a
// real type=agent bead that NEVER RAN: the reconciler auto-closes a spawn that
// sat in the coop queue past the expiry timeout with agent_state="expired"
// (reconciler.expireDeferredBeads). That shell survives the type filter and was
// being counted as an iteration. These tests pin the fix: such shells are
// flagged DidNotRun, excluded from the run count, but still rendered.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// expiredShellBead builds a never-ran spawn shell: a closed agent bead carrying
// agent_state="expired" and none of the ran-signal fields.
func expiredShellBead(id, prior string, created time.Time) *beadsapi.BeadDetail {
	b := invIterBead(id, prior, "closed", created, "")
	b.Fields[fieldAgentState] = agentStateExpired
	return b
}

// ranBead builds a real, completed run: a closed agent bead with agent_state
// "done" and a wrap-up.
func ranBead(id, prior string, created time.Time) *beadsapi.BeadDetail {
	b := invIterBead(id, prior, "closed", created, `{"tldr":"did real work"}`)
	b.Fields[fieldAgentState] = "done"
	return b
}

func TestIterationDidNotRun(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{"expired shell", expiredShellBead("gd-x", "", t0), true},
		{"real done run", ranBead("gd-y", "", t0), false},
		{"live working run", func() *beadsapi.BeadDetail {
			b := invIterBead("gd-z", "", "in_progress", t0, "")
			b.Fields[fieldAgentState] = "working"
			return b
		}(), false},
		{"closed no agent_state", invIterBead("gd-w", "", "closed", t0, ""), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := iterationDidNotRun(tc.bead); got != tc.want {
				t.Fatalf("iterationDidNotRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOrderGardenerIterations_FlagsExpiredShells asserts the ordering layer
// stamps DidNotRun on expired shells while leaving real runs unflagged, and
// that shells are still part of the rendered thread (they are not dropped).
func TestOrderGardenerIterations_FlagsExpiredShells(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Chain: run1 (ran) <- shell2 (expired) <- run3 (ran).
	beads := []*beadsapi.BeadDetail{
		ranBead("gd-r1", "", t0),
		expiredShellBead("gd-s2", "gd-r1", t0.Add(time.Minute)),
		ranBead("gd-r3", "gd-s2", t0.Add(2*time.Minute)),
	}
	views := orderGardenerIterations(beads, "gd-r3", idBuild)
	if len(views) != 3 {
		t.Fatalf("rendered thread length = %d, want 3 (shells must still render)", len(views))
	}
	dnr := map[string]bool{}
	for _, v := range views {
		dnr[v.ID] = v.DidNotRun
	}
	if !dnr["gd-s2"] {
		t.Errorf("expired shell gd-s2 not flagged DidNotRun")
	}
	if dnr["gd-r1"] || dnr["gd-r3"] {
		t.Errorf("real runs wrongly flagged DidNotRun: r1=%v r3=%v", dnr["gd-r1"], dnr["gd-r3"])
	}
}

// TestRunIterationCount_ExcludesShells is the core bug assertion: the run count
// counts real runs only, excluding never-ran shells, while countDidNotRun
// reports the shells separately. A gardener with 2 real runs + 1 expired spawn
// must show "Iterations 2", not 3 (bug ka-0000006805).
func TestRunIterationCount_ExcludesShells(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		ranBead("gd-r1", "", t0),
		expiredShellBead("gd-s2", "gd-r1", t0.Add(time.Minute)),
		ranBead("gd-r3", "gd-s2", t0.Add(2*time.Minute)),
	}
	views := orderGardenerIterations(beads, "gd-r3", idBuild)

	if got := runIterationCount(views); got != 2 {
		t.Errorf("runIterationCount = %d, want 2 (real runs only)", got)
	}
	if got := countDidNotRun(views); got != 1 {
		t.Errorf("countDidNotRun = %d, want 1", got)
	}
	// The full rendered thread is still 3 cards: count + shells == thread length.
	if runIterationCount(views)+countDidNotRun(views) != len(views) {
		t.Errorf("run count (%d) + shells (%d) != thread length (%d)",
			runIterationCount(views), countDidNotRun(views), len(views))
	}
}

// TestRunIterationCount_AllShells covers the degenerate "never-ran gardener":
// every iteration is an expired shell, so the run count is 0 even though the
// thread renders the shells. This is the exact symptom the bug names — a
// gardener that "never ran" must show Iterations 0, not N.
func TestRunIterationCount_AllShells(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		expiredShellBead("gd-s1", "", t0),
		expiredShellBead("gd-s2", "gd-s1", t0.Add(time.Minute)),
	}
	views := orderGardenerIterations(beads, "gd-s2", idBuild)
	if got := runIterationCount(views); got != 0 {
		t.Errorf("runIterationCount = %d, want 0 (all shells never ran)", got)
	}
	if got := countDidNotRun(views); got != 2 {
		t.Errorf("countDidNotRun = %d, want 2", got)
	}
}

// TestRunIterationCount_NoShells confirms the common case is unchanged: with no
// shells, the run count equals the thread length, so the existing header
// invariant (IterationCount == rendered real cards) is preserved.
func TestRunIterationCount_NoShells(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	beads := []*beadsapi.BeadDetail{
		ranBead("gd-r1", "", t0),
		ranBead("gd-r2", "gd-r1", t0.Add(time.Minute)),
		ranBead("gd-r3", "gd-r2", t0.Add(2*time.Minute)),
	}
	views := orderGardenerIterations(beads, "gd-r3", idBuild)
	if got := runIterationCount(views); got != len(views) {
		t.Errorf("runIterationCount = %d, want %d (no shells)", got, len(views))
	}
	if got := countDidNotRun(views); got != 0 {
		t.Errorf("countDidNotRun = %d, want 0", got)
	}
}

// TestGardenerDetail_NeverRanShell_GoldenRender renders the REAL gardener detail
// page through the handler for a gardener with 2 real runs + 1 expired spawn
// shell, and asserts the end-to-end contract: the Iterations chip shows 2 (not
// 3), the header annotates "(+1 never ran)", and the shell card carries the
// "did not run" chip while still rendering. This locks the template + handler
// path, not just the pure helpers (bug ka-0000006805).
func TestGardenerDetail_NeverRanShell_GoldenRender(t *testing.T) {
	f := &fakeDaemon{}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap": goldenGardenerBead(t, "gd-cap", "Shelltest gardener", map[string]any{
			"name":          "shelltest",
			"terminal_goal": "do the thing",
		}),
	}
	t0 := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// Two real runs and one expired shell, all owned by gd-cap via schedule_id.
	r1 := mkIteration("gd-r1", "gd-cap", "", 1, "closed", t0)
	r1.Fields[fieldAgentState] = "done"
	shell := mkIteration("gd-s2", "gd-cap", "gd-r1", 2, "closed", t0.Add(time.Hour))
	shell.Fields[fieldAgentState] = agentStateExpired
	r3 := mkIteration("gd-r3", "gd-cap", "gd-s2", 3, "closed", t0.Add(2*time.Hour))
	r3.Fields[fieldAgentState] = "done"
	f.liveAgentsResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{shell, r3, r1},
		Total: 3,
	}

	mux := mkServerWithNow(t, f, gardenerPageNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// PRIMARY Iterations row counts the 2 real runs, NOT the 3 beads.
	if !strings.Contains(body, "<dt>Iterations</dt><dd>2") {
		t.Errorf("Iterations chip should show 2 real runs; body=%s", body)
	}
	// The header explains the third bead as a never-ran spawn.
	if !strings.Contains(body, "never ran") {
		t.Errorf("header missing the '(+N never ran)' annotation; body=%s", body)
	}
	// All three cards still render (the shell is shown, not dropped).
	if cards := renderedIterationCards(body); cards != 3 {
		t.Errorf("rendered %d iteration cards, want 3 (shell still shown)", cards)
	}
	// The shell card carries the explicit "did not run" chip.
	if !strings.Contains(body, "did not run") {
		t.Errorf("shell card missing 'did not run' chip; body=%s", body)
	}
}
