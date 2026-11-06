package main

// Tests for the Jira-style /plans board + the card-peek drawer (Matt's ask,
// thread ka-0000005903; plans_board.go). The board is the ?view= default; the
// peek is the GET /plans/{id}/peek fragment plans_board.js mounts into the
// slide-out drawer.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- lane sectioning (pure) -------------------------------------------------

func boardRow(id, status string) planRowView {
	return planRowView{ID: id, Title: "Plan " + id, Status: status}
}

// TestPlanBoardLaneKey exercises the derived-lane precedence (Matt's
// follow-ups: "in progress should be all approved plans with complete steps",
// "also stalled/blocked should be a column").
func TestPlanBoardLaneKey(t *testing.T) {
	stalledRow := boardRow("gd-x", "open")
	stalledRow.Stall = planStallInfo{Stalled: true, Reason: "open work, but the plan's gardener is paused"}
	escRow := boardRow("gd-y", "in_progress")
	escRow.Escalations = planEscalationInfo{Count: 1}
	buildingStalled := boardRow("gd-z", "open")
	buildingStalled.PlanState = string(planStateBuilding)
	buildingStalled.Stall = planStallInfo{Stalled: true}

	withState := func(id, status, state string, closedSteps int) planRowView {
		r := boardRow(id, status)
		r.PlanState = state
		r.StepsClosed = closedSteps
		return r
	}

	cases := []struct {
		name string
		row  planRowView
		want string
	}{
		{"closed status wins", boardRow("gd-a", "closed"), "closed"},
		{"deferred status wins", boardRow("gd-b", "deferred"), "deferred"},
		{"blocked status → stalled lane", boardRow("gd-c", "blocked"), planBoardLaneStalled},
		{"stall signal → stalled lane", stalledRow, planBoardLaneStalled},
		{"blocking escalation → stalled lane", escRow, planBoardLaneStalled},
		{"attention trumps approval", buildingStalled, planBoardLaneStalled},
		{"literally claimed epic stays in progress", boardRow("gd-d", "in_progress"), "in_progress"},
		{"approved (building) plan is in progress", withState("gd-e", "open", string(planStateBuilding), 0), "in_progress"},
		{"draft plan is open even with done steps", withState("gd-f", "open", string(planStateDraft), 3), "open"},
		{"ready plan is open even with done steps", withState("gd-g", "open", string(planStateReady), 3), "open"},
		{"ungated plan with a completed step is in progress", withState("gd-h", "open", "", 1), "in_progress"},
		{"unstarted ungated plan is open", boardRow("gd-i", "open"), "open"},
	}
	for _, c := range cases {
		if got := planBoardLaneKey(c.row); got != c.want {
			t.Errorf("%s: lane = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestPlanBoardLanes_DefaultScope proves the active scope renders the core
// columns (Open / In progress / Stalled / Closed) even when empty, folds
// closed rows into a Done lane, and omits an empty Deferred lane.
func TestPlanBoardLanes_DefaultScope(t *testing.T) {
	rows := []planRowView{
		boardRow("gd-a", "in_progress"),
		boardRow("gd-b", "open"),
		boardRow("gd-c", "closed"),
	}
	lanes := planBoardLanes(rows, nil, planStatusActive, false)

	keys := make([]string, 0, len(lanes))
	for _, l := range lanes {
		keys = append(keys, l.Key)
	}
	want := []string{"open", "in_progress", planBoardLaneStalled, "closed"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("lane order = %v, want %v", keys, want)
	}
	if n := len(lanes[0].Rows); n != 1 || lanes[0].Rows[0].ID != "gd-b" {
		t.Errorf("open lane rows = %d, want the gd-b card", n)
	}
	if n := len(lanes[1].Rows); n != 1 || lanes[1].Rows[0].ID != "gd-a" {
		t.Errorf("in_progress lane rows = %d, want the gd-a card", n)
	}
	if n := len(lanes[2].Rows); n != 0 {
		t.Errorf("stalled lane should render empty, got %d rows", n)
	}
	if lanes[2].Label != "Stalled / Blocked" {
		t.Errorf("stalled lane label = %q, want %q", lanes[2].Label, "Stalled / Blocked")
	}
	if n := len(lanes[3].Rows); n != 1 || lanes[3].Rows[0].ID != "gd-c" {
		t.Errorf("closed lane rows = %d, want the gd-c card", n)
	}
}

// TestPlanBoardLanes_StalledUnionsSignals proves one Stalled/Blocked column
// gathers all three shapes: blocked status, the ⚠ stall signal, and ⛔
// blocked-by-escalation.
func TestPlanBoardLanes_StalledUnionsSignals(t *testing.T) {
	stalled := boardRow("gd-s", "open")
	stalled.Stall = planStallInfo{Stalled: true}
	esc := boardRow("gd-e", "in_progress")
	esc.Escalations = planEscalationInfo{Count: 2}
	rows := []planRowView{boardRow("gd-b", "blocked"), stalled, esc}

	lanes := planBoardLanes(rows, nil, planStatusActive, false)
	for _, l := range lanes {
		if l.Key == planBoardLaneStalled {
			if len(l.Rows) != 3 {
				t.Fatalf("stalled lane rows = %d, want all 3 (blocked + stall + escalation)", len(l.Rows))
			}
			return
		}
	}
	t.Fatalf("no stalled lane rendered")
}

// TestPlanBoardLanes_DeferredOnlyWhenPopulated proves the Deferred column
// appears exactly when a deferred plan exists in the active scope.
func TestPlanBoardLanes_DeferredOnlyWhenPopulated(t *testing.T) {
	lanes := planBoardLanes([]planRowView{boardRow("gd-d", "deferred")}, nil, planStatusActive, false)
	found := false
	for _, l := range lanes {
		if l.Key == "deferred" {
			found = true
			if len(l.Rows) != 1 {
				t.Errorf("deferred lane rows = %d, want 1", len(l.Rows))
			}
		}
	}
	if !found {
		t.Fatalf("a deferred plan should surface the Deferred lane; lanes = %v", lanes)
	}
}

// TestPlanBoardLanes_SingleStatusFilter proves a narrowed ?status= renders
// exactly that one raw-status column — even when it is empty — bypassing the
// derived laning (rows must never vanish out of a narrowed scope).
func TestPlanBoardLanes_SingleStatusFilter(t *testing.T) {
	lanes := planBoardLanes(nil, nil, planStatusBlocked, false)
	if len(lanes) != 1 || lanes[0].Key != "blocked" {
		t.Fatalf("status=blocked should render exactly the blocked lane, got %v", lanes)
	}
	if len(lanes[0].Rows) != 0 {
		t.Errorf("empty blocked lane should still render with 0 rows")
	}

	// A stalled-but-open plan under ?status=open stays in the Open column —
	// the derived stalled lane is out of scope there.
	stalled := boardRow("gd-s", "open")
	stalled.Stall = planStallInfo{Stalled: true}
	lanes = planBoardLanes([]planRowView{stalled}, nil, planStatusOpen, false)
	if len(lanes) != 1 || lanes[0].Key != "open" || len(lanes[0].Rows) != 1 {
		t.Fatalf("status=open must keep the stalled row in its raw-status lane, got %v", lanes)
	}
}

// TestPlanBoardLanes_StaleSweepSingleLane proves the ?status=stale sweep renders
// as ONE "Stale" lane rather than scattering the already-subtracted swept rows
// back across four raw-status columns (open/in_progress/blocked/deferred). The
// sweep is a derived cross-status cut; fracturing it into per-status lanes would
// bury a small sweep in mostly-empty columns and defeat the select-all → retire
// gesture the whole view exists for.
func TestPlanBoardLanes_StaleSweepSingleLane(t *testing.T) {
	// Rows of differing raw status — handlePlans has already subtracted them to the
	// stale set; the board must keep them together, not re-section by status.
	rows := []planRowView{
		boardRow("gd-a", "open"),
		boardRow("gd-b", "in_progress"),
		boardRow("gd-c", "blocked"),
		boardRow("gd-d", "deferred"),
	}
	lanes := planBoardLanes(rows, nil, planStatusStale, false)
	if len(lanes) != 1 {
		t.Fatalf("stale sweep should render exactly one lane, got %d: %v", len(lanes), lanes)
	}
	if lanes[0].Key != planStatusStale {
		t.Errorf("stale lane key = %q, want %q", lanes[0].Key, planStatusStale)
	}
	if lanes[0].Label != "Stale" {
		t.Errorf("stale lane label = %q, want %q", lanes[0].Label, "Stale")
	}
	if len(lanes[0].Rows) != 4 {
		t.Errorf("stale lane should hold all 4 swept rows regardless of raw status, got %d", len(lanes[0].Rows))
	}
}

// TestPlanBoardLanes_ClosedTruncatedNote proves the Done lane carries the
// truncation marker when the bounded recent-closed fetch was clipped.
func TestPlanBoardLanes_ClosedTruncatedNote(t *testing.T) {
	lanes := planBoardLanes([]planRowView{boardRow("gd-c", "closed")}, nil, planStatusActive, true)
	for _, l := range lanes {
		if l.Key == "closed" {
			if !l.Truncated {
				t.Errorf("closed lane should be marked truncated")
			}
			return
		}
	}
	t.Fatalf("no closed lane rendered")
}

// --- view resolution --------------------------------------------------------

func TestResolvePlanView(t *testing.T) {
	cases := []struct {
		raw     string
		hasKey  bool
		group   string
		want    string
		comment string
	}{
		{"", false, planGroupNone, planViewBoard, "cold load defaults to the board"},
		{"", false, planGroupProject, planViewGrid, "a ?group= URL implies the grid"},
		{"grid", true, planGroupNone, planViewGrid, "explicit grid"},
		{"board", true, planGroupProject, planViewBoard, "explicit board wins over group"},
		{"garbage", true, planGroupNone, planViewBoard, "unrecognized falls back to board"},
	}
	for _, c := range cases {
		if got := resolvePlanView(c.raw, c.hasKey, c.group); got != c.want {
			t.Errorf("resolvePlanView(%q,%v,%q) = %q, want %q (%s)", c.raw, c.hasKey, c.group, got, c.want, c.comment)
		}
	}
}

// --- board page render ------------------------------------------------------

// TestPlansIndex_BoardIsDefault proves the cold-load /plans renders the
// Jira-style lane board — columns with headings, the plan card in its status
// lane — and no flat grid.
func TestPlansIndex_BoardIsDefault(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="plans-board"`,
		`class="plans-lane plans-lane-open"`,
		`class="plans-lane plans-lane-in_progress"`,
		`class="plans-lane plans-lane-stalled"`,
		"Stalled / Blocked",
		`class="plans-lane plans-lane-closed"`,
		"Plan: ship the /plans page", // the seeded card still renders
		`aria-label="View"`,          // the board/grid toggle row
		">Board<",
		">Grid<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("default board missing %q", want)
		}
	}
	// The seeded plan is claimed in_progress BUT has open work with no gardener
	// and no live assistant — the ⚠ stall signal fires, and the derived board
	// puts attention ahead of progress: the card must sit inside the
	// Stalled/Blocked lane, exactly agreeing with its own stall badge.
	stalledLane := body[strings.Index(body, `class="plans-lane plans-lane-stalled"`):]
	if end := strings.Index(stalledLane, `class="plans-lane plans-lane-closed"`); end >= 0 {
		stalledLane = stalledLane[:end]
	}
	if !strings.Contains(stalledLane, "gd-plan1") {
		t.Errorf("the stalled in_progress plan should render inside the Stalled/Blocked lane")
	}
}

// TestPlansIndex_BoardShipsPeekDrawer proves the board page carries the peek
// drawer shell + driver script the card click depends on.
func TestPlansIndex_BoardShipsPeekDrawer(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-peek-drawer"`,
		`id="plan-peek-backdrop"`,
		`id="plan-peek-body"`,
		"plans_board.js",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("board page missing peek-drawer piece %q", want)
		}
	}
}

// TestPlansIndex_BoardFetchesRecentClosed proves the default board issues the
// bounded recent-closed Done-lane query alongside the active-scope main query.
func TestPlansIndex_BoardFetchesRecentClosed(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	sawClosed := false
	for _, q := range f.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "epic" &&
			len(q.Statuses) == 1 && q.Statuses[0] == "closed" {
			sawClosed = true
			if q.Limit != plansBoardClosedLimit {
				t.Errorf("closed-lane query limit = %d, want %d", q.Limit, plansBoardClosedLimit)
			}
		}
	}
	if !sawClosed {
		t.Errorf("default board should issue the recent-closed Done-lane query")
	}
}

// TestPlansIndex_GridSkipsClosedFetch proves ?view=grid keeps the historical
// single-scope query — no Done-lane fetch.
func TestPlansIndex_GridSkipsClosedFetch(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&view=grid", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	for _, q := range f.listAllQ {
		if len(q.Types) == 1 && q.Types[0] == "epic" &&
			len(q.Statuses) == 1 && q.Statuses[0] == "closed" {
			t.Errorf("grid view must not issue the Done-lane closed query")
		}
	}
}

// TestPlansIndex_GroupURLStillRendersSections proves a legacy ?group= URL
// (no ?view=) keeps its sectioned grid — the board never hijacks it.
func TestPlansIndex_GroupURLStillRendersSections(t *testing.T) {
	f := seedPlansFake()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=&group=status", ""))
	body := w.Body.String()
	if !strings.Contains(body, `class="plan-group"`) {
		t.Errorf("?group= without ?view= should keep the sectioned grid")
	}
	if strings.Contains(body, `class="plans-board"`) {
		t.Errorf("?group= without ?view= must not render the board")
	}
}

// TestPlanFilterState_ViewPreserved proves a status chip URL keeps an explicit
// grid view, and the board default stays key-free.
func TestPlanFilterState_ViewPreserved(t *testing.T) {
	grid := planFilterState{status: planStatusActive, view: planViewGrid, base: "/plans"}
	got := mustQuery(t, grid.chipURL(map[string]string{"status": planStatusClosed}))
	if got.Get("view") != planViewGrid {
		t.Errorf("status chip dropped the grid view: %v", got)
	}
	board := planFilterState{status: planStatusActive, view: planViewBoard, base: "/plans"}
	got = mustQuery(t, board.chipURL(map[string]string{"status": planStatusClosed}))
	if got.Has("view") {
		t.Errorf("board is the default — chip URLs must not carry view=board: %v", got)
	}
}

// --- the peek fragment ------------------------------------------------------

// TestPlanPeek_RendersHeaderAndSteps proves GET /plans/{id}/peek returns the
// drawer fragment: title linked to the full cockpit, status chip, and the SAME
// live #plan-steps section the cockpit renders (so step verbs work in place).
func TestPlanPeek_RendersHeaderAndSteps(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="plan-peek"`,
		"Plan: ship the /plans page",
		`href="/plans/gd-plan1"`, // full-cockpit escape hatch
		"Open full cockpit",
		`id="plan-steps"`, // the live step timeline mount
		"Wire the route",  // the seeded step renders
		"chip-status-in_progress",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("peek fragment missing %q", want)
		}
	}
	// A fragment, not a page: no layout chrome.
	if strings.Contains(body, "<nav") {
		t.Errorf("peek must render as a bare fragment without the site nav")
	}
}

// TestPlanPeek_NotFound proves a missing or non-epic id 404s.
func TestPlanPeek_NotFound(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-task9"] = &beadsapi.BeadDetail{ID: "gd-task9", Type: "task", Status: "open"}
	mux := mkServer(t, f)

	for _, id := range []string{"gd-nope", "gd-task9"} {
		req := httptest.NewRequest("GET", "/plans/"+id+"/peek", nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Errorf("peek %s: got %d, want 404", id, w.Code)
		}
	}
}

// TestPlanPeek_RendersPriorityPill proves the peek header carries the priority
// pill the index card face carries (ka-0000005352 iter-72) — an off-baseline
// priority pops the SAME "P{N}" pill the index shows, so an operator triaging
// from the drawer sees urgency without closing it and scanning the index.
func TestPlanPeek_RendersPriorityPill(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Priority = 0 // P0 — urgent, off the P2 baseline
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"plan-prio-chip",   // the pill renders
		"plan-prio-chip-0", // colored for P0
		">P0<",             // the face text
	} {
		if !strings.Contains(body, want) {
			t.Errorf("peek header missing priority pill %q", want)
		}
	}
}

// TestPlanPeek_SuppressesBaselinePriority proves the pill self-suppresses on a
// routine P2 plan (the `ne .Priority 2` gate, matching the index card face) so a
// baseline-priority plan adds no ink — the "render only where the value
// discriminates" density discipline the index pill uses.
func TestPlanPeek_SuppressesBaselinePriority(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Priority = 2 // baseline — no pill
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "plan-prio-chip") {
		t.Errorf("a baseline P2 plan must render no priority pill in the peek header")
	}
}

// TestPlanPeek_RendersWorkingCrew proves the drawer surfaces the crew roster
// Matt asked for (thread ka-0000002125: "show working crew") — the SAME
// plan_crew partial + tally the cockpit renders, so a working crew reads live
// in the preview without opening the full page.
func TestPlanPeek_RendersWorkingCrew(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	seedCrew(f, "gd-crewA", "gardenio-wire-route-abc", "gd-step1", "working", "in_progress")
	// Fixture clock so the seeded working crew reads fresh (live) under the iter-42
	// freshness gate — the "1 working" tally only renders for a live-classified crew.
	mux := mkServerWithNow(t, f, crewFixtureNowFn)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`id="plan-crew"`,          // the roster section
		"gardenio-wire-route-abc", // the crew name
		"plan-crew-member",        // a roster row
		"1 working",               // the header tally — "whether crew is working"
	} {
		if !strings.Contains(body, want) {
			t.Errorf("peek crew roster missing %q", want)
		}
	}
}

// TestPlanPeek_CrewEmptyState proves a crew-less plan still renders the roster
// section (empty state), so the drawer always answers "who's on it".
func TestPlanPeek_CrewEmptyState(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	body := w.Body.String()
	if !strings.Contains(body, `id="plan-crew"`) {
		t.Errorf("crew-less peek should still render the #plan-crew section")
	}
	if !strings.Contains(body, "No crew yet") {
		t.Errorf("crew-less peek should render the empty state")
	}
}

// TestPlanPeek_RendersGardenerLiveChip proves the drawer header carries the
// unified plan-agent chip Matt asked for (thread ka-0000002125: "whether the
// gardener is live"): a bound-and-enabled gardener with a live iteration reads
// "running · live" with the pulsing liveness dot — the SAME chip markup the
// cockpit header + index card render.
func TestPlanPeek_RendersGardenerLiveChip(t *testing.T) {
	f := seedPlansFake()
	// A bound, enabled plan gardener...
	seedPlanGardenerBead(f, "gd-cap1", "gd-plan1", "true", "open", "gd-asstlive")
	// ...with a live bound iteration (the assistant the chip's liveness dot reads).
	seedAssistantBead(f, "gd-asstlive", "gardenio-plan-asst-1", "gd-plan1", "working", planGardenerTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planGardenerTestNow })

	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"plan-agent-chip",     // the shared chip markup
		"running · live",      // loop running + a live iteration
		"plan-agent-chip-dot", // the pulsing liveness dot
	} {
		if !strings.Contains(body, want) {
			t.Errorf("peek header missing gardener-live chip piece %q; body=%s", want, body)
		}
	}
}

// TestPlanPeek_RendersChatTranscript proves the drawer shows the plan-agent
// chat at the bottom (Matt's ask: "showing chat at the bottom wouldn't hurt
// either") — the SAME read-only bead_chat direction-bubble log the cockpit
// renders, reconstructed from the durable fields.plan_id-bound message beads.
func TestPlanPeek_RendersChatTranscript(t *testing.T) {
	f := seedPlansFake()
	base := planAssistantTestNow.Add(-10 * time.Minute)
	seedTranscriptTurn(f, "gd-turn0", "gd-plan1", "matt", planTurnRoleOperator,
		"Draft me a plan to add a New-plan button.", base)
	seedTranscriptTurn(f, "gd-turn1", "gd-plan1", "gardenio-plan-asst-1", planTurnRoleAssistant,
		"On it — researching the existing /plans routes now.", base.Add(time.Minute))
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`class="bead-chat"`,                                   // the chat log
		"Draft me a plan to add a New-plan button.",           // the operator turn
		"On it — researching the existing /plans routes now.", // the agent turn
	} {
		if !strings.Contains(body, want) {
			t.Errorf("peek chat transcript missing %q", want)
		}
	}
	// The compose box (send) stays on the full cockpit — the drawer is read-only.
	if strings.Contains(body, "plan-assistant-form") {
		t.Errorf("peek chat must be read-only — no compose form in the drawer")
	}
}

// TestPlanPeek_NoChatWhenNoTurns proves the chat log self-gates: a plan with no
// persisted turns renders no bead_chat section (no empty console).
func TestPlanPeek_NoChatWhenNoTurns(t *testing.T) {
	mux := mkServer(t, seedPlansFake())
	req := httptest.NewRequest("GET", "/plans/gd-plan1/peek", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if strings.Contains(w.Body.String(), `class="bead-chat"`) {
		t.Errorf("a plan with no turns must not render the chat log")
	}
}

// --- the drawer driver asset ------------------------------------------------

// The drawer open/close + click interception is pure client-side JS in
// plans_board.js. There is no JS test runner in CI, so — like
// plan_composer_asset_test.go — this Go test guards (1) the asset is embedded
// + served and (2) the shipped file still carries the behavioural contract
// markers the drawer depends on.
func TestPlansBoardAsset_ServedStatic(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/plans_board.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("plans_board.js: got %d want 200", w.Code)
	}
	body := w.Body.String()
	if len(body) < 1000 {
		t.Fatalf("plans_board.js suspiciously small: %d bytes", len(body))
	}
	// Each marker maps to a contract point:
	//   plan-peek-drawer / plan-peek-body — the shell + mount ids.
	//   /peek                             — the fragment route.
	//   closest && evt.target.closest     — card click delegation.
	//   data-plan-id (dataset.planId)     — the card's plan binding.
	//   searchParams.set('peek'           — ?peek= history mirroring.
	//   popstate                          — back/forward restore.
	//   Escape                            — keyboard close.
	//   metaKey                           — modified clicks pass through to hrefs.
	//   htmx.ajax                         — mounted fragments get hx-* processed.
	for _, want := range []string{
		"plan-peek-drawer",
		"plan-peek-body",
		"/peek",
		"dataset.planId",
		"searchParams.set('peek'",
		"popstate",
		"'Escape'",
		"metaKey",
		"htmx.ajax",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("plans_board.js missing contract marker %q", want)
		}
	}
}
