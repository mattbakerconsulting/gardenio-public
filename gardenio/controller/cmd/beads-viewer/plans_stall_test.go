package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// --- deriveStall: the predicate truth table ---------------------------------

func TestDeriveStall_Table(t *testing.T) {
	cases := []struct {
		name          string
		gardenerState string
		deadSteps     int
		openWork      bool
		assistantLive bool
		crewLive      bool
		planTerminal  bool
		wantStalled   bool
		wantReasonSub string
	}{
		{"dead agent step always stalls", "running", 1, true, true, false, false, true, "dead or absent agent"},
		{"multiple dead steps counted in reason", "running", 3, true, true, false, false, true, "3 steps"},
		{"open work + paused gardener + no live agent", "paused", 0, true, false, false, false, true, "gardener is paused"},
		{"open work + no gardener + no live agent", "none", 0, true, false, false, false, true, "no gardener is bound"},
		{"live assistant exempts paused gardener", "paused", 0, true, true, false, false, false, ""},
		{"live assistant exempts missing gardener", "none", 0, true, true, false, false, false, ""},
		// The ka-0000000734 false-stall: gardener paused/none but a crew agent is
		// live on a step. The crew IS working the plan, so it must not flag —
		// even though the bound assistant is not live.
		{"live crew exempts paused gardener", "paused", 0, true, false, true, false, false, ""},
		{"live crew exempts missing gardener", "none", 0, true, false, true, false, false, ""},
		// A live crew does NOT rescue a genuinely orphaned step: a dead-agent
		// step on the plan still stalls even while another step has a live crew.
		{"live crew does not exempt a dead-agent step", "running", 1, true, false, true, false, true, "dead or absent agent"},
		{"running gardener is healthy", "running", 0, true, false, false, false, false, ""},
		{"no open work never stalls on gardener state", "none", 0, false, false, false, false, false, ""},
		// The false-stall the audit surfaced (ka-0000005352 iteration): a TERMINAL
		// plan (closed/deferred) is never stalled, whatever its gardener state or
		// leftover child steps say. A closed plan's work is over; a deferred
		// plan's is intentionally parked — there is nothing to watch.
		{"terminal plan never stalls on no-gardener", "none", 0, true, false, false, true, false, ""},
		{"terminal plan never stalls on paused gardener", "paused", 0, true, false, false, true, false, ""},
		{"terminal plan never stalls even on a dead-agent step", "running", 2, true, false, false, true, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveStall(tc.gardenerState, "gd-cap", tc.deadSteps, tc.openWork, tc.assistantLive, tc.crewLive, tc.planTerminal)
			if got.Stalled != tc.wantStalled {
				t.Errorf("Stalled = %v, want %v", got.Stalled, tc.wantStalled)
			}
			if tc.wantReasonSub != "" && !strings.Contains(got.Reason, tc.wantReasonSub) {
				t.Errorf("Reason = %q, want it to mention %q", got.Reason, tc.wantReasonSub)
			}
			if !tc.wantStalled && got.Reason != "" {
				t.Errorf("healthy plan must carry no reason, got %q", got.Reason)
			}
		})
	}
}

// --- BadgeLabel: the face discriminator projection --------------------------

// TestPlanStallBadgeLabel pins the compact face discriminator (ka-0000001697):
// it must name WHICH condition fired, in deriveStall's OWN precedence (dead
// steps outrank gardener state), and stay empty for a non-stalled info so the
// badge never renders a misleading "· …" when it isn't even shown.
func TestPlanStallBadgeLabel(t *testing.T) {
	cases := []struct {
		name string
		info planStallInfo
		want string
	}{
		{"not stalled yields no label", planStallInfo{Stalled: false, GardenerState: "paused"}, ""},
		{"one dead step", planStallInfo{Stalled: true, DeadSteps: 1}, "1 dead step"},
		{"multiple dead steps pluralize + count", planStallInfo{Stalled: true, DeadSteps: 3}, "3 dead steps"},
		{"paused gardener", planStallInfo{Stalled: true, GardenerState: "paused"}, "gardener paused"},
		{"no gardener", planStallInfo{Stalled: true, GardenerState: "none"}, "no gardener"},
		// Dead steps outrank gardener state — deriveStall fires the dead-agent
		// branch first, so the face label must match its Reason (which also names
		// the dead-agent condition), never the gardener condition.
		{"dead steps outrank gardener state", planStallInfo{Stalled: true, DeadSteps: 2, GardenerState: "none"}, "2 dead steps"},
		// A stalled info that classifies to neither (running gardener, no dead
		// steps — an unreachable deriveStall state, but the projection must not
		// invent a label) falls back to empty so the badge stays bare "⚠ stalled".
		{"unclassifiable stall yields no label", planStallInfo{Stalled: true, GardenerState: "running"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.info.BadgeLabel(); got != tc.want {
				t.Errorf("BadgeLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPlanStallBadgeLabel_AgreesWithDeriveStall proves the face label can never
// disagree with the Reason tooltip: for every stalled deriveStall output, the
// condition BadgeLabel names is the SAME condition Reason names (both key off
// DeadSteps-then-GardenerState in the same order). This is the anti-drift guard —
// the whole point of deriving the label from the same fields deriveStall set.
func TestPlanStallBadgeLabel_AgreesWithDeriveStall(t *testing.T) {
	cases := []struct {
		gardenerState string
		deadSteps     int
	}{
		{"running", 1}, {"running", 4}, {"paused", 0}, {"none", 0},
	}
	for _, tc := range cases {
		info := deriveStall(tc.gardenerState, "gd-cap", tc.deadSteps, true, false, false, false)
		if !info.Stalled {
			t.Fatalf("expected a stalled info for %+v", tc)
		}
		label := info.BadgeLabel()
		if label == "" {
			t.Fatalf("a classified stall (%+v) must yield a face label, got empty", tc)
		}
		// The label names the same condition family as the reason.
		switch {
		case tc.deadSteps >= 1:
			if !strings.Contains(label, "dead step") || !strings.Contains(info.Reason, "dead or absent agent") {
				t.Errorf("dead-step stall: label=%q reason=%q must both name the dead-agent condition", label, info.Reason)
			}
		case tc.gardenerState == "paused":
			if !strings.Contains(label, "gardener paused") || !strings.Contains(info.Reason, "gardener is paused") {
				t.Errorf("paused stall: label=%q reason=%q must both name the paused-gardener condition", label, info.Reason)
			}
		case tc.gardenerState == "none":
			if !strings.Contains(label, "no gardener") || !strings.Contains(info.Reason, "no gardener is bound") {
				t.Errorf("no-gardener stall: label=%q reason=%q must both name the no-gardener condition", label, info.Reason)
			}
		}
	}
}

// --- deadStepCount: the grace window + liveness -----------------------------

func TestDeadStepCount(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	alive := func(name string) bool { return name == "gardenio-live-agent-1" }

	refs := []stallStepRef{
		// Dead agent, past grace → counts.
		{Assignee: "gardenio-dead-agent-1", UpdatedAt: now.Add(-2 * time.Hour)},
		// Live agent, past grace → does not count.
		{Assignee: "gardenio-live-agent-1", UpdatedAt: now.Add(-2 * time.Hour)},
		// Dead agent but within grace (just claimed / mid-respawn) → no flap.
		{Assignee: "gardenio-dead-agent-2", UpdatedAt: now.Add(-10 * time.Minute)},
		// No assignee at all, past grace → orphaned by definition.
		{Assignee: "", UpdatedAt: now.Add(-2 * time.Hour)},
		// Zero UpdatedAt (no stamp) is treated as past grace — degrade toward
		// flagging, matching the "silence is the failure mode" posture.
		{Assignee: "gardenio-dead-agent-3", UpdatedAt: time.Time{}},
	}
	if got := deadStepCount(refs, now, alive); got != 3 {
		t.Errorf("deadStepCount = %d, want 3 (dead-past-grace, unassigned, unstamped)", got)
	}
}

// --- anyStepAgentLive: crew liveness feeds the stall exemption ---------------

func TestAnyStepAgentLive(t *testing.T) {
	alive := func(name string) bool { return name == "gardenio-live-crew-1" }

	// No live agent among the steps → not live (grace window is irrelevant to
	// this right-now check).
	dead := []stallStepRef{
		{Assignee: "gardenio-dead-crew-1", UpdatedAt: time.Time{}},
		{Assignee: "", UpdatedAt: time.Time{}},
	}
	if anyStepAgentLive(dead, alive) {
		t.Error("no live crew among steps must report not-live")
	}

	// One live crew among otherwise-dead steps → live, regardless of when it
	// last moved (a just-claimed live crew counts).
	mixed := []stallStepRef{
		{Assignee: "gardenio-dead-crew-1"},
		{Assignee: "gardenio-live-crew-1"},
	}
	if !anyStepAgentLive(mixed, alive) {
		t.Error("a live crew among the steps must report live")
	}

	// Empty / unassigned steps never count as a live agent.
	if anyStepAgentLive(nil, alive) {
		t.Error("no steps must report not-live")
	}
	if anyStepAgentLive([]stallStepRef{{Assignee: "  "}}, alive) {
		t.Error("a blank assignee must not report live")
	}
}

// --- gardenerStateOf + pickBetterGardener --------------------------------------

func TestGardenerStateOf(t *testing.T) {
	s := testServerWithDaemon(newFake())
	s.SetNow(func() time.Time { return planAssistantTestNow })
	if state, id := s.gardenerStateOf(nil); state != "none" || id != "" {
		t.Errorf("nil gardener = (%q,%q), want (none,)", state, id)
	}
	closed := &beadsapi.BeadDetail{ID: "gd-c", Type: "trigger", Status: "closed",
		Labels: []string{"trigger:schedule"}, Fields: map[string]string{"enabled": "true"}}
	if state, _ := s.gardenerStateOf(closed); state != "none" {
		t.Errorf("closed gardener = %q, want none (a retired watchdog watches nothing)", state)
	}
	// trigger:schedule beads are enabled-by-default unless "false".
	paused := &beadsapi.BeadDetail{ID: "gd-p", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule"}, Fields: map[string]string{"enabled": "false"}}
	if state, id := s.gardenerStateOf(paused); state != "paused" || id != "gd-p" {
		t.Errorf("paused gardener = (%q,%q), want (paused,gd-p)", state, id)
	}
	running := &beadsapi.BeadDetail{ID: "gd-r", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule"}, Fields: map[string]string{"enabled": "true"}}
	if state, _ := s.gardenerStateOf(running); state != "running" {
		t.Errorf("running gardener = %q, want running", state)
	}
}

func TestPickBetterGardener_OpenEnabledWins(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	closed := &beadsapi.BeadDetail{ID: "gd-old", Type: "trigger", Status: "closed",
		Labels: []string{"trigger:schedule"}, Fields: map[string]string{}}
	paused := &beadsapi.BeadDetail{ID: "gd-paused", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule"}, Fields: map[string]string{"enabled": "false"}}
	running := &beadsapi.BeadDetail{ID: "gd-run", Type: "trigger", Status: "open",
		Labels: []string{"trigger:schedule"}, Fields: map[string]string{}}

	if got := pickBetterGardener(closed, paused, now); got.ID != "gd-paused" {
		t.Errorf("open beats closed: got %s", got.ID)
	}
	if got := pickBetterGardener(paused, running, now); got.ID != "gd-run" {
		t.Errorf("enabled beats paused: got %s", got.ID)
	}
	if got := pickBetterGardener(running, paused, now); got.ID != "gd-run" {
		t.Errorf("order-independent: got %s", got.ID)
	}
}

// --- index handler: the badge end-to-end -------------------------------------

// seedPlanGardener seeds a gardener trigger pinned to the epic via the
// gardener-epic label, with the given enabled state.
func seedPlanGardener(f *fakeDaemon, id, epicID, enabled string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Plan gardener for " + epicID,
		Type:   "trigger",
		Kind:   "data",
		Status: "open",
		Labels: []string{"trigger:schedule", "role:gardener", "gardener-epic:" + epicID},
		Fields: map[string]string{"enabled": enabled, "role": "plan-assistant"},
	}
	f.beads[id] = b
	// planGardenersByEpic lists via Labels=[role:gardener] → route through
	// triggerScheduleResp (the fake's Types=[trigger] branch).
	if f.triggerScheduleResp == nil {
		f.triggerScheduleResp = &beadsapi.ListBeadsResult{}
	}
	f.triggerScheduleResp.Beads = append(f.triggerScheduleResp.Beads, b)
	f.triggerScheduleResp.Total = len(f.triggerScheduleResp.Beads)
	return b
}

// seedPlanGardenerEvent is the event-mode (mode-flipped) sibling of
// seedPlanGardener for the /plans INDEX join: trigger:multi (not
// trigger:schedule), cleared cron, and a triggers[] with a bead_event entry, so
// the index chip's firing-mode glyph reads ⚡ (Task 8, epic ka-0000006020).
func seedPlanGardenerEvent(f *fakeDaemon, id, epicID, enabled string) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Plan gardener for " + epicID,
		Type:   "trigger",
		Kind:   "data",
		Status: "open",
		Labels: []string{"trigger:multi", "role:gardener", "gardener-epic:" + epicID},
		Fields: map[string]string{
			"enabled":  enabled,
			"role":     "plan-assistant",
			"cron":     "",
			"triggers": `[{"kind":"bead_event","events":["created","updated","closed"]},{"kind":"cron","cron":"11 2,8,14,20 * * *"}]`,
		},
	}
	f.beads[id] = b
	if f.triggerScheduleResp == nil {
		f.triggerScheduleResp = &beadsapi.ListBeadsResult{}
	}
	f.triggerScheduleResp.Beads = append(f.triggerScheduleResp.Beads, b)
	f.triggerScheduleResp.Total = len(f.triggerScheduleResp.Beads)
	return b
}

// TestPlansIndex_StallBadgeWhenUnwatched proves the exact ka-0000004457 shape:
// open work + a PAUSED gardener + no live assistant renders the ⚠ badge.
func TestPlansIndex_StallBadgeWhenUnwatched(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("expected the stall badge on a plan with open work and a paused gardener")
	}
	if !strings.Contains(body, "gardener is paused") {
		t.Error("badge tooltip should name the paused-gardener condition")
	}
	// The face discriminator (ka-0000001697) renders the condition inline so an
	// operator need not hover the tooltip.
	if !strings.Contains(body, "plan-stall-badge-reason") || !strings.Contains(body, "gardener paused") {
		t.Error("stall badge face should carry the compact 'gardener paused' reason")
	}
}

// TestPlansIndex_StallBadgeWhenNoGardener proves open work with NO gardener at
// all (and no live assistant) also flags.
func TestPlansIndex_StallBadgeWhenNoGardener(t *testing.T) {
	f := seedPlansFake()
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("expected the stall badge on a plan with open work and no gardener")
	}
	if !strings.Contains(body, "no gardener is bound") {
		t.Error("badge tooltip should name the no-gardener condition")
	}
	if !strings.Contains(body, "plan-stall-badge-reason") || !strings.Contains(body, "no gardener") {
		t.Error("stall badge face should carry the compact 'no gardener' reason")
	}
}

// TestPlansIndex_NoStallBadgeWhenGardenerRunning proves a running gardener
// suppresses the badge.
func TestPlansIndex_NoStallBadgeWhenGardenerRunning(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("a plan watched by a running gardener must not flag stalled")
	}
}

// TestPlansIndex_NoStallBadgeWhenAssistantLive proves a live plan agent exempts
// the unwatched condition — the plan-gardener lineage means a live iteration IS
// the bound agent working the plan.
func TestPlansIndex_NoStallBadgeWhenAssistantLive(t *testing.T) {
	f := seedPlansFake()
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("a plan with a live assistant must not flag stalled on gardener state")
	}
}

// TestPlansIndex_NoStallBadgeWhenCrewLive proves the ka-0000000734 fix on the
// index: a plan whose gardener is PAUSED and whose bound assistant is NOT live
// still must not flag stalled when a crew agent is live on an in_progress step.
// The dispatched crew IS the plan making progress; flagging it cries wolf.
func TestPlansIndex_NoStallBadgeWhenCrewLive(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // gardener paused
	// The step is in_progress under a live crew agent (no assistant seeded, so
	// the bound-assistant lane is not live — only the crew keeps it healthy).
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Assignee = "gardenio-live-crew-1"
	f.beads["gd-step1"].UpdatedAt = planAssistantTestNow.Add(-2 * time.Hour)
	f.beads["gd-crew1"] = &beadsapi.BeadDetail{
		ID:        "gd-crew1",
		Title:     "gardenio-live-crew-1",
		Type:      "agent",
		Kind:      "config",
		Status:    "in_progress",
		Labels:    []string{"project:gardenio", "role:crew"},
		Fields:    map[string]string{"agent": "gardenio-live-crew-1", "agent_state": "working"},
		UpdatedAt: planAssistantTestNow,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("a plan with a live crew on an in_progress step must not flag stalled, even with a paused gardener")
	}
}

// TestPlansIndex_StallBadgeOnDeadAgentStep proves the dead-agent condition
// flags EVEN with a running gardener and a live assistant — an in_progress step
// under a dead agent is orphaned work regardless of who is watching.
func TestPlansIndex_StallBadgeOnDeadAgentStep(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	// Flip the seeded step to in_progress under an agent that does not exist,
	// last moved 2h ago (past the 1h grace).
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Assignee = "gardenio-vanished-agent-1"
	f.beads["gd-step1"].UpdatedAt = planAssistantTestNow.Add(-2 * time.Hour)
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("expected the stall badge on a plan with a dead-agent step")
	}
	if !strings.Contains(body, "dead or absent agent") {
		t.Error("badge tooltip should name the dead-agent condition")
	}
	// The face discriminator names the dead-step count (a single seeded orphan).
	if !strings.Contains(body, "plan-stall-badge-reason") || !strings.Contains(body, "1 dead step") {
		t.Error("stall badge face should carry the compact '1 dead step' reason")
	}
}

// --- detail handler: same predicate, same badge -------------------------------

func TestPlanDetail_StallBadgeWhenUnwatched(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("expected the stall badge on the plan detail header")
	}
	if !strings.Contains(body, "gardener is paused") {
		t.Error("detail badge tooltip should name the paused-gardener condition")
	}
}

func TestPlanDetail_NoStallBadgeWhenHealthy(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("a healthy plan must not render the stall badge on the detail page")
	}
}

// TestPlanDetail_NoStallBadgeWhenPlanClosed proves the false-stall the audit
// surfaced (ka-0000005352 iteration): a CLOSED plan carrying a leftover
// un-closed child step and NO bound gardener used to read "open work, but no
// gardener is bound to watch this plan" and render a loud ⚠ badge — crying wolf
// on a finished plan. A closed plan's work is over; there is nothing to watch,
// so the badge must not fire whatever its leftover steps or gardener state say.
func TestPlanDetail_NoStallBadgeWhenPlanClosed(t *testing.T) {
	f := seedPlansFake()
	// Close the plan epic but leave the open step (the exact prod shape: 4
	// closed plans each with one un-closed child step, no gardener bound).
	f.beads["gd-plan1"].Status = "closed"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("a closed plan must not render the stall badge, even with an open child step and no gardener")
	}
}

// TestPlanDetail_NoStallBadgeWhenPlanDeferred proves the same exemption for a
// DEFERRED plan — intentionally parked work is not stalled work. deferred is the
// other terminal lane planBoardLaneKey short-circuits on.
func TestPlanDetail_NoStallBadgeWhenPlanDeferred(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-plan1"].Status = "deferred"
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("a deferred plan must not render the stall badge, even with an open child step and no gardener")
	}
}

// --- reconcileStallWithCrew: the badge-vs-crew-chip truth reconcile (iter-19) -

// TestReconcileStallWithCrew pins the monotonic reconcile that makes the ⚠
// stalled badge agree with the crew chip on the SAME card (ka-0000005352
// iteration 19 + 23): a gardener-paused/no-gardener stall (DeadSteps == 0) is
// CLEARED when the authoritative crew rollup says a crew is working right now.
// A dead-agent stall (DeadSteps ≥ 1) is NEVER cleared — an orphaned in_progress
// step under a dead agent is a real stall even while OTHER steps have live crew
// — but with live crew it is RE-SCOPED (Orphaned = true, iteration 23): it stays
// Stalled (board lane + attention ranker still treat it as attention-worthy) yet
// the badge FACE softens from the loud plan-level "stalled" to the per-step
// "orphaned" so it no longer contradicts the crew chip's "N working". The
// reconcile can only clear or soften — never create — a stall.
func TestReconcileStallWithCrew(t *testing.T) {
	cases := []struct {
		name         string
		info         planStallInfo
		crewLive     bool
		crewQueued   bool
		wantStalled  bool
		wantOrphaned bool
	}{
		{
			name:        "paused-gardener stall cleared by live crew",
			info:        planStallInfo{Stalled: true, Reason: "open work, but the plan's gardener is paused", GardenerState: "paused", GardenerID: "gd-cap"},
			crewLive:    true,
			wantStalled: false,
		},
		{
			name:        "no-gardener stall cleared by live crew",
			info:        planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
			crewLive:    true,
			wantStalled: false,
		},
		{
			// iteration 51: a gardener-state stall is ALSO a false alarm when the
			// gardener dispatched crew now QUEUED in the pool (not working yet) —
			// "· no gardener" contradicts the card's own "● N crew · K queued".
			name:        "no-gardener stall cleared by queued crew (iter-51)",
			info:        planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
			crewQueued:  true,
			wantStalled: false,
		},
		{
			name:        "paused-gardener stall cleared by queued crew (iter-51)",
			info:        planStallInfo{Stalled: true, Reason: "open work, but the plan's gardener is paused", GardenerState: "paused", GardenerID: "gd-cap"},
			crewQueued:  true,
			wantStalled: false,
		},
		{
			name:        "paused-gardener stall stands when no crew is live or queued",
			info:        planStallInfo{Stalled: true, Reason: "open work, but the plan's gardener is paused", GardenerState: "paused", GardenerID: "gd-cap"},
			crewLive:    false,
			crewQueued:  false,
			wantStalled: true,
		},
		{
			// iteration 23: a dead-agent stall with live crew elsewhere stays
			// Stalled but is RE-SCOPED to "orphaned" so the badge no longer reads
			// as a contradiction beside the crew chip's "N working".
			name:         "dead-agent stall re-scoped to orphaned by live crew elsewhere",
			info:         planStallInfo{Stalled: true, Reason: "1 step in progress with a dead or absent agent", DeadSteps: 1, GardenerState: "running"},
			crewLive:     true,
			wantStalled:  true,
			wantOrphaned: true,
		},
		{
			name:         "multi dead-agent stall re-scoped to orphaned by live crew",
			info:         planStallInfo{Stalled: true, Reason: "3 steps in progress with a dead or absent agent", DeadSteps: 3},
			crewLive:     true,
			wantStalled:  true,
			wantOrphaned: true,
		},
		{
			// iteration 23: with NO live crew the orphan is genuinely unwatched, so
			// the loud plan-level "stalled" verdict stands — NOT re-scoped.
			name:         "dead-agent stall stays loud stalled when no crew is live",
			info:         planStallInfo{Stalled: true, Reason: "1 step in progress with a dead or absent agent", DeadSteps: 1, GardenerState: "none"},
			crewLive:     false,
			wantStalled:  true,
			wantOrphaned: false,
		},
		{
			// iteration 51: a merely-QUEUED crew does NOT watch an orphaned
			// in_progress step (it has not started), so a real dead-agent orphan
			// stays LOUD and is NOT re-scoped to "orphaned" — the queued arm clears
			// only the gardener-state (DeadSteps==0) alarm, never softens a genuine
			// orphan the way live crew do (iter-23).
			name:         "dead-agent stall stays loud stalled when crew only queued (iter-51)",
			info:         planStallInfo{Stalled: true, Reason: "1 step in progress with a dead or absent agent", DeadSteps: 1, GardenerState: "none"},
			crewLive:     false,
			crewQueued:   true,
			wantStalled:  true,
			wantOrphaned: false,
		},
		{
			name:        "healthy info stays healthy (never invents a stall)",
			info:        planStallInfo{Stalled: false, GardenerState: "running"},
			crewLive:    true,
			wantStalled: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileStallWithCrew(tc.info, tc.crewLive, tc.crewQueued)
			if got.Stalled != tc.wantStalled {
				t.Errorf("Stalled = %v, want %v", got.Stalled, tc.wantStalled)
			}
			if got.Orphaned != tc.wantOrphaned {
				t.Errorf("Orphaned = %v, want %v", got.Orphaned, tc.wantOrphaned)
			}
			if !got.Stalled && got.Reason != "" {
				t.Errorf("a cleared stall must carry no reason, got %q", got.Reason)
			}
			// A re-scoped (orphaned) stall must PRESERVE the dead-step count +
			// reason so the tooltip and lane still describe a real orphan — only the
			// face word softens, never the underlying signal.
			if tc.wantOrphaned {
				if got.DeadSteps != tc.info.DeadSteps {
					t.Errorf("re-scoped stall must preserve DeadSteps: got %d, want %d", got.DeadSteps, tc.info.DeadSteps)
				}
				if got.Reason != tc.info.Reason {
					t.Errorf("re-scoped stall must preserve Reason: got %q, want %q", got.Reason, tc.info.Reason)
				}
				// The re-scoped face suppresses the reason ("count rides the
				// headline") and the headline carries the count + "orphaned".
				if got.FaceLabel() != "" {
					t.Errorf("an orphaned stall must suppress the FaceLabel (count moves to the headline), got %q", got.FaceLabel())
				}
				if got.HeadlineLabel() != "orphaned" {
					t.Errorf("HeadlineLabel = %q, want %q", got.HeadlineLabel(), "orphaned")
				}
				if got.HeadlineCount() != tc.info.DeadSteps {
					t.Errorf("HeadlineCount = %d, want %d", got.HeadlineCount(), tc.info.DeadSteps)
				}
			}
			// The gardener identity is preserved across a clear (downstream chips
			// still read GardenerState/GardenerID off the info).
			if got.GardenerState != tc.info.GardenerState || got.GardenerID != tc.info.GardenerID {
				t.Errorf("gardener identity must survive the reconcile: got state=%q id=%q, want state=%q id=%q",
					got.GardenerState, got.GardenerID, tc.info.GardenerState, tc.info.GardenerID)
			}
		})
	}
}

// TestReconcileStallWithCrew_LaneAndAttentionUnchanged proves the iteration-23
// re-scope softens only the badge FACE — a re-scoped "orphaned" plan still lanes
// into Stalled/Blocked and still ranks in the stalled attention tier, so an
// orphan under a dead agent never silently drops off the operator's radar just
// because other crew are live. This is the "re-scope the verdict, do NOT
// suppress the signal" guarantee (no iteration-10/11 regression).
func TestReconcileStallWithCrew_LaneAndAttentionUnchanged(t *testing.T) {
	orphaned := reconcileStallWithCrew(
		planStallInfo{Stalled: true, Reason: "1 step in progress with a dead or absent agent", DeadSteps: 1},
		true,  // live crew elsewhere
		false, // no queued crew (re-scope is a live-crew-only softening)
	)
	if !orphaned.Orphaned {
		t.Fatal("setup: expected the dead-agent stall to re-scope to orphaned")
	}
	row := planRowView{Stall: orphaned}
	if got := planBoardLaneKey(row); got != planBoardLaneStalled {
		t.Errorf("a re-scoped orphaned plan must still lane into %q, got %q", planBoardLaneStalled, got)
	}
	if got := planAttention(row); got != planAttentionStalled {
		t.Errorf("a re-scoped orphaned plan must still rank in the stalled attention tier, got %v", got)
	}
}

// TestPlansIndex_NoStallBadgeWhenCrewChipLive is the iteration-19 anti-drift
// regression: it reproduces the exact self-contradiction the mission names — a
// card that renders "● N crew · M working" AND "⚠ stalled · no gardener" at the
// same time. The trap is that the stall badge and the crew chip were reading
// liveness from DIFFERENT sources: the stall proxy (anyStepAgentLive) inspects
// only an in_progress step's Assignee FIELD, while the crew chip (planCrewBulk)
// counts every crew linked by an `assigned` EDGE across all rendered steps. Here
// the crew is dispatched (assigned edge) onto an OPEN step with NO Assignee
// field — so the chip shows "1 working" but the narrow proxy misses it, and the
// no-gardener branch used to fire. After the reconcile the card must show the
// crew chip and NO badge: if it says a crew is working, it cannot also say
// stalled.
func TestPlansIndex_NoStallBadgeWhenCrewChipLive(t *testing.T) {
	f := seedPlansFake()
	// No gardener bound, no assistant → the no-gardener stall branch is armed.
	// The step stays OPEN (not in_progress) and carries NO Assignee, so the
	// stall's anyStepAgentLive proxy sees nothing — only the assigned-edge crew
	// keeps the plan healthy.
	seedCrew(f, "gd-crewLive", "gardenio-live-crew-x", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The crew chip must show the working crew...
	if !strings.Contains(body, `<a class="plan-card-crew plan-card-crew-live"`) {
		t.Fatal("expected the live-crew chip — the test setup did not produce a working crew")
	}
	if !strings.Contains(body, "1 working") {
		t.Error("expected the crew chip to read '1 working'")
	}
	// ...and therefore the card must NOT simultaneously claim the plan is stalled.
	if strings.Contains(body, "⚠ stalled") {
		t.Error("a card that shows '1 working' crew must not also render '⚠ stalled' — the badge must agree with the crew chip (iteration 19 false-stall)")
	}
}

// TestPlanDetail_NoStallBadgeWhenCrewRosterLive is the cockpit twin of the
// iteration-19 regression: the /plans/{id} header ⚠ badge must agree with the
// crew roster on the same page. Same trap, same fix — the stall reconciles
// against the authoritative planCrew roster's HasLive().
func TestPlanDetail_NoStallBadgeWhenCrewRosterLive(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // gardener paused
	// A working crew dispatched (assigned edge) onto the OPEN step — invisible to
	// the stall proxy, counted by the crew roster.
	seedCrew(f, "gd-crewLive", "gardenio-live-crew-x", "gd-step1", "working", "in_progress")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("the cockpit header must not flag stalled when the crew roster shows a live crew, even with a paused gardener (iteration 19)")
	}
}

// TestPlansIndex_NoStallBadgeWhenCrewQueued is the iteration-51 render regression
// (ka-0000005352): the QUEUED-crew half of the mission-named false-stall that
// iteration-19 left. iteration-19 cleared the gardener-state stall only when crew
// were WORKING (Working>0); a plan whose gardener dispatched crew now sitting
// QUEUED in the pool (an open idle/queued agent_state, Idle>0/Working==0) still
// rendered "⚠ stalled · no gardener" beside the card's own "● N crew · 1 queued"
// facet — the SAME self-contradiction (the gardener plainly acted, work is pending
// in the pool). Here a queued crew is dispatched onto the OPEN step with no
// gardener bound, so the no-gardener branch is armed but must be CLEARED by the
// queued crew. The card must show the crew chip's queued facet and NO stall badge.
func TestPlansIndex_NoStallBadgeWhenCrewQueued(t *testing.T) {
	f := seedPlansFake()
	// No gardener, no assistant → the no-gardener stall branch is armed. The crew is
	// dispatched (assigned edge) onto the OPEN step but is QUEUED (pool-deferred),
	// not working — Working==0 so iteration-19's HasLive() clear does NOT fire; only
	// the iteration-51 queued arm keeps the plan healthy.
	seedCrew(f, "gd-crewQ", "gardenio-queued-crew-x", "gd-step1", "queued", "open")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The crew chip must show the queued crew...
	if !strings.Contains(body, "plan-card-crew-idle") {
		t.Fatal("expected the queued-crew facet — the test setup did not produce a queued crew")
	}
	if !strings.Contains(body, "queued") {
		t.Error("expected the crew chip to read a 'queued' facet")
	}
	// ...and therefore the card must NOT simultaneously claim the plan is stalled:
	// the gardener dispatched crew, so "· no gardener" is a false alarm (iteration 51).
	if strings.Contains(body, "⚠ stalled") {
		t.Error("a card that shows queued crew must not also render '⚠ stalled' — the gardener dispatched work, so the gardener-state badge is a false alarm (iteration 51)")
	}
}

// TestPlanDetail_NoStallBadgeWhenCrewQueued is the cockpit twin of the
// iteration-51 regression: the /plans/{id} header ⚠ badge must clear the false
// gardener-state stall when the roster carries queued crew, exactly as the index
// card does (crew.HasQueued()).
func TestPlanDetail_NoStallBadgeWhenCrewQueued(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // gardener paused
	seedCrew(f, "gd-crewQ", "gardenio-queued-crew-x", "gd-step1", "queued", "open")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if strings.Contains(w.Body.String(), "⚠ stalled") {
		t.Error("the cockpit header must not flag stalled when the crew roster shows queued crew, even with a paused gardener (iteration 51)")
	}
}

// TestPlansIndex_DeadAgentStallRescopedWhenCrewLive is the iteration-23 render
// regression (ka-0000005352): it reproduces the OTHER half of the mission-named
// false-stall that iteration-19 left — a card that renders "● N crew · M
// working" AND "⚠ stalled · 1 dead step" at the same time. iteration-19's
// reconcile only CLEARED the DeadSteps==0 (paused/no-gardener) case; a dead-agent
// orphan coexisting with live crew stayed loud "stalled", still contradicting
// the crew chip. This asserts the re-scope: with a dead-agent orphan on one step
// AND live crew on ANOTHER, the badge softens to "⚠ N orphaned" (NOT "stalled")
// while the crew chip still shows the working crew — coherent siblings, not a
// contradiction. The dead-step count rides the headline and the plan stays
// flagged (still rendered as a red badge).
func TestPlansIndex_DeadAgentStallRescopedWhenCrewLive(t *testing.T) {
	f := seedPlansFake()
	// The seed step becomes a dead-agent orphan: in_progress, assignee an agent
	// that FindAgentBeadsByNames won't resolve as live, frozen well past grace.
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Assignee = "gardenio-dead-orphan-zzz"
	f.beads["gd-step1"].UpdatedAt = planAssistantTestNow.Add(-48 * time.Hour)
	// A SECOND step with a live working crew (assigned edge) — the coexistence.
	step2 := &beadsapi.BeadDetail{ID: "gd-step2", Title: "Second step", Kind: "issue", Type: "task", Status: "open", Labels: []string{"project:gardenio"}}
	f.beads["gd-step2"] = step2
	f.revDeps["gd-plan1"] = append(f.revDeps["gd-plan1"], beadsapi.Dependency{BeadID: "gd-step2", DependsOnID: "gd-plan1", Type: "parent-child"})
	f.epicSubtreeCount = map[string]int{"gd-plan1": 2, "gd-plan1|closed": 0}
	seedCrew(f, "gd-crewLive", "gardenio-live-crew-x", "gd-step2", "working", "in_progress")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The crew chip must still show the working crew...
	if !strings.Contains(body, `class="plan-card-crew plan-card-crew-live"`) || !strings.Contains(body, "1 working") {
		t.Fatal("setup: expected the live-crew chip to read '1 working'")
	}
	// ...the badge must be RE-SCOPED to "orphaned", NOT the loud "stalled"...
	if !strings.Contains(body, "⚠ 1 orphaned") {
		t.Errorf("a dead-agent orphan coexisting with live crew must re-scope the badge to '⚠ 1 orphaned'; body missing it")
	}
	if strings.Contains(body, "⚠ stalled") {
		t.Error("the loud '⚠ stalled' verdict must NOT render when live crew coexist with the orphan — that is the iteration-23 contradiction ('N working + stalled')")
	}
	// ...and the "N dead step" face reason must be gone (the count rides the
	// headline now), so the softer label is not undercut by the loud phrasing.
	if strings.Contains(body, "dead step") {
		t.Error("the re-scoped 'orphaned' badge must drop the '· N dead step' face reason (the count moved to the headline)")
	}
	// The plan still lands in the STALLED/BLOCKED lane (a re-scope softens the
	// face, it does not drop the plan off the operator's radar).
	if !strings.Contains(body, "STALLED") && !strings.Contains(body, "Stalled") {
		t.Error("a re-scoped orphaned plan must still render in the stalled/blocked board lane")
	}
}

// TestPlansIndex_DeadAgentStallStaysLoudWhenNoCrewLive is the CONTROL for the
// iteration-23 re-scope: a dead-agent orphan with NO live crew is genuinely
// unwatched, so the badge must keep the loud plan-level "⚠ stalled · 1 dead
// step" exactly as before — the re-scope must never soften a genuinely-stalled
// plan (no iteration-1/6/10/11 regression).
func TestPlansIndex_DeadAgentStallStaysLoudWhenNoCrewLive(t *testing.T) {
	f := seedPlansFake()
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Assignee = "gardenio-dead-orphan-zzz"
	f.beads["gd-step1"].UpdatedAt = planAssistantTestNow.Add(-48 * time.Hour)
	// No crew at all → the orphan is genuinely unwatched.
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("a dead-agent orphan with no live crew must keep the loud '⚠ stalled' verdict")
	}
	if !strings.Contains(body, `<span class="plan-stall-badge-reason">· 1 dead step`) {
		t.Error("the loud stall must still carry its '· 1 dead step' face reason when no crew is live")
	}
	if strings.Contains(body, "orphaned") {
		t.Error("a genuinely-unwatched orphan must NOT be re-scoped to 'orphaned' — that reads as if someone is working it")
	}
}

// --- reconcileStallWithEscalation: the badge-vs-escalation de-dup (iter-26) ---

// TestReconcileStallWithEscalation pins the monotonic reconcile that suppresses
// the ⚠ stalled BADGE on a card ALREADY ⛔ blocked by an open escalation
// (ka-0000005352 iteration 26). The two badges give CONTRADICTORY remediation —
// the ⛔ escalation badge (the harder stop that OUTRANKS stall in planAttention)
// says "a privileged operator must act", while "⚠ stalled · no gardener" says
// "bind a gardener", which cannot clear the escalation. So a gardener-state stall
// (DeadSteps == 0) on an escalation-blocked plan has its badge FACE suppressed
// (EscalationBlocked → ShowBadge false) while Stalled stays true — the board
// lane and attention ranker are untouched. A dead-agent orphan (DeadSteps ≥ 1)
// is a real system failure independent of the escalation, so it keeps its loud
// badge. A stall with no blocking escalation is unaffected. Like
// reconcileStallWithCrew, the reconcile can only soften — never create — a stall.
func TestReconcileStallWithEscalation(t *testing.T) {
	cases := []struct {
		name        string
		info        planStallInfo
		blocked     bool
		wantStalled bool
		wantShow    bool
	}{
		{
			name:        "no-gardener stall badge suppressed by a blocking escalation",
			info:        planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
			blocked:     true,
			wantStalled: true, // stays flagged for lane + ranker
			wantShow:    false,
		},
		{
			name:        "paused-gardener stall badge suppressed by a blocking escalation",
			info:        planStallInfo{Stalled: true, Reason: "open work, but the plan's gardener is paused", GardenerState: "paused", GardenerID: "gd-cap"},
			blocked:     true,
			wantStalled: true,
			wantShow:    false,
		},
		{
			name:        "no-gardener stall badge STANDS when nothing is escalation-blocked",
			info:        planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
			blocked:     false,
			wantStalled: true,
			wantShow:    true,
		},
		{
			// A dead-agent orphan is a real system failure regardless of the
			// human-approval wait, so its loud badge survives the escalation.
			name:        "dead-agent stall badge STAYS loud even under a blocking escalation",
			info:        planStallInfo{Stalled: true, Reason: "1 step in progress with a dead or absent agent", DeadSteps: 1, GardenerState: "none"},
			blocked:     true,
			wantStalled: true,
			wantShow:    true,
		},
		{
			name:        "multi dead-agent stall badge STAYS loud under a blocking escalation",
			info:        planStallInfo{Stalled: true, Reason: "3 steps in progress with a dead or absent agent", DeadSteps: 3},
			blocked:     true,
			wantStalled: true,
			wantShow:    true,
		},
		{
			name:        "healthy info stays healthy (never invents a stall)",
			info:        planStallInfo{Stalled: false, GardenerState: "running"},
			blocked:     true,
			wantStalled: false,
			wantShow:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileStallWithEscalation(tc.info, tc.blocked)
			if got.Stalled != tc.wantStalled {
				t.Errorf("Stalled = %v, want %v (the reconcile must never change the lane/ranker signal)", got.Stalled, tc.wantStalled)
			}
			if got.ShowBadge() != tc.wantShow {
				t.Errorf("ShowBadge() = %v, want %v", got.ShowBadge(), tc.wantShow)
			}
			// A suppressed stall PRESERVES every underlying field — only the badge
			// render gates on ShowBadge, so the tooltip/reason/count still describe
			// the real condition and downstream chips read GardenerState/GardenerID.
			if got.GardenerState != tc.info.GardenerState || got.GardenerID != tc.info.GardenerID {
				t.Errorf("gardener identity must survive the reconcile: got state=%q id=%q, want state=%q id=%q",
					got.GardenerState, got.GardenerID, tc.info.GardenerState, tc.info.GardenerID)
			}
			if got.DeadSteps != tc.info.DeadSteps {
				t.Errorf("DeadSteps must survive the reconcile: got %d, want %d", got.DeadSteps, tc.info.DeadSteps)
			}
			if got.Reason != tc.info.Reason {
				t.Errorf("Reason must survive the reconcile: got %q, want %q", got.Reason, tc.info.Reason)
			}
		})
	}
}

// TestReconcileStallWithEscalation_LaneAndAttentionUnchanged proves the iter-26
// suppression drops only the badge FACE — an escalation-blocked plan whose stall
// badge was suppressed still lanes into Stalled/Blocked and still ranks in the
// escalation attention tier (which already OUTRANKS stalled), so it never drops
// off the operator's radar. This is the "suppress the redundant face, keep the
// signal" guarantee.
func TestReconcileStallWithEscalation_LaneAndAttentionUnchanged(t *testing.T) {
	suppressed := reconcileStallWithEscalation(
		planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
		true, // escalation-blocked
	)
	if suppressed.ShowBadge() {
		t.Fatal("setup: expected the gardener-state stall badge to be suppressed under an escalation")
	}
	if !suppressed.Stalled {
		t.Fatal("a suppressed stall must keep Stalled true so lane + ranker are unaffected")
	}
	// The card still lanes into Stalled/Blocked (planBoardLaneKey unions
	// escalation + stall into one lane regardless), and the escalation tier —
	// which the card qualifies for — outranks the stalled tier.
	row := planRowView{Stall: suppressed, Escalations: planEscalationInfo{Count: 1}}
	if got := planBoardLaneKey(row); got != planBoardLaneStalled {
		t.Errorf("a suppressed-stall escalation-blocked plan must still lane into %q, got %q", planBoardLaneStalled, got)
	}
	if got := planAttention(row); got != planAttentionEscalation {
		t.Errorf("an escalation-blocked plan must rank in the escalation tier, got %v", got)
	}
}

// TestPlansIndex_NoStallBadgeWhenEscalationBlocked is the iter-26 render
// regression (ka-0000005352): it reproduces the exact double-signal the pour
// found on the live board — a card that renders "⛔ blocked by escalation" AND
// "⚠ stalled · no gardener" at once, two badges giving contradictory
// remediation. With a no-gardener stall (seedPlansFake's default) AND a blocking
// escalation, the card must show the ⛔ escalation badge and NO ⚠ stall badge:
// the escalation already owns the card's attention. Proven to FAIL before the
// reconcile (both badges rendered) and PASS after.
func TestPlansIndex_NoStallBadgeWhenEscalationBlocked(t *testing.T) {
	f := seedPlansFake()
	// seedPlansFake has open work + no gardener → the no-gardener stall is armed.
	// Add a blocking escalation on the step.
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "internal")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The escalation badge MUST render (it owns the card's attention)...
	if !strings.Contains(body, "blocked by escalation") {
		t.Fatal("expected the ⛔ escalation badge — the test setup did not produce a blocking escalation")
	}
	// ...and therefore the redundant, contradictory ⚠ stalled badge must NOT.
	if strings.Contains(body, "⚠ stalled") {
		t.Error("a card already ⛔ blocked by an escalation must not also render '⚠ stalled · no gardener' — the two badges give contradictory remediation (iteration 26)")
	}
}

// TestPlansIndex_DeadAgentStallStaysLoudUnderEscalation is the iter-26 boundary:
// suppression is scoped to the gardener-state stall. A dead-agent orphan is a
// real system failure that persists regardless of a human-approval wait, so its
// loud "⚠ stalled · N dead step" badge must SURVIVE a coexisting escalation —
// suppressing it would hide a genuinely orphaned step behind an unrelated
// escalation. Both badges render here, and that is correct: they describe two
// different real problems (a privileged action is owed AND a step is orphaned).
func TestPlansIndex_DeadAgentStallStaysLoudUnderEscalation(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "true")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", planAssistantTestNow)
	// A dead-agent orphan: in_progress under a vanished agent, frozen past grace.
	f.beads["gd-step1"].Status = "in_progress"
	f.beads["gd-step1"].Assignee = "gardenio-vanished-agent-1"
	f.beads["gd-step1"].UpdatedAt = planAssistantTestNow.Add(-2 * time.Hour)
	// A blocking escalation on the same step — the coexistence.
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "internal")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Fatal("expected the ⛔ escalation badge in the boundary setup")
	}
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("a dead-agent orphan must keep its loud '⚠ stalled' badge even under a coexisting escalation — it is a real system failure independent of the human-approval wait (iteration 26 boundary)")
	}
	if !strings.Contains(body, "1 dead step") {
		t.Error("the surviving dead-agent stall must still carry its '1 dead step' face reason")
	}
}

// TestPlanDetail_NoStallBadgeWhenEscalationBlocked is the cockpit twin of the
// iter-26 index regression: the /plans/{id} header ⚠ badge must be suppressed
// when the ⛔ escalation banner already blocks the plan, so the header agrees
// with the index card. Same trap, same fix — the stall reconciles against the
// escalation count derived on the same page.
func TestPlanDetail_NoStallBadgeWhenEscalationBlocked(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // gardener paused → stall armed
	seedPlanEscalation(f, "gd-esc1", "gd-step1", "Grant IAM for deploy", "aws_iam", "internal")
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "blocked by escalation") {
		t.Fatal("expected the ⛔ escalation banner on the cockpit in the iter-26 setup")
	}
	if strings.Contains(body, "⚠ stalled") {
		t.Error("the cockpit header must not flag '⚠ stalled' when an escalation already blocks the plan (iteration 26)")
	}
}

// --- reconcileStallWithLifecycle: the badge-vs-lifecycle de-dup (iter-49) ---

// TestReconcileStallWithLifecycle pins the monotonic reconcile that suppresses
// the ⚠ stalled BADGE on a plan still in its PRE-BUILD authoring lifecycle
// (draft/ready), for the gardener-state stall only (ka-0000005352 iteration 49).
// A draft/ready plan has NO enabled gardener BY DESIGN — gardenerEnableGatedByState
// (the R6 build gate) forbids one until "Approve & build" — so "⚠ stalled · no
// gardener" / "· gardener paused" implies a remediation the lifecycle deliberately
// holds back, contradicting the READY/DRAFT lifecycle chip already on the card.
// So a gardener-state stall (DeadSteps == 0) on a build-gated plan has its badge
// FACE suppressed (PreBuildLifecycle → ShowBadge false) while Stalled stays true
// — board lane and attention ranker untouched. A dead-agent orphan (DeadSteps ≥
// 1) is a real system failure independent of the lifecycle, so it keeps its loud
// badge. A stall on a NOT-build-gated plan (building/complete/unset) is
// unaffected. Like the iter-26 twin, the reconcile can only soften — never create
// — a stall.
func TestReconcileStallWithLifecycle(t *testing.T) {
	cases := []struct {
		name        string
		info        planStallInfo
		buildGated  bool
		wantStalled bool
		wantShow    bool
	}{
		{
			name:        "no-gardener stall badge suppressed on a build-gated (draft/ready) plan",
			info:        planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
			buildGated:  true,
			wantStalled: true, // stays flagged for lane + ranker
			wantShow:    false,
		},
		{
			name:        "paused-gardener stall badge suppressed on a build-gated plan",
			info:        planStallInfo{Stalled: true, Reason: "open work, but the plan's gardener is paused", GardenerState: "paused", GardenerID: "gd-cap"},
			buildGated:  true,
			wantStalled: true,
			wantShow:    false,
		},
		{
			// building/complete/legacy-unset are NOT build-gated: such a plan's
			// gardener is not lifecycle-held, so a missing/paused gardener IS a real
			// "nothing is watching" stall the operator should see.
			name:        "no-gardener stall badge STANDS on a NOT-build-gated (building/unset) plan",
			info:        planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
			buildGated:  false,
			wantStalled: true,
			wantShow:    true,
		},
		{
			// A dead-agent orphan is a real system failure even on a draft plan
			// (crew can be dispatched onto a step that then dies), so its loud badge
			// survives the build-gated lifecycle.
			name:        "dead-agent stall badge STAYS loud even on a build-gated plan",
			info:        planStallInfo{Stalled: true, Reason: "1 step in progress with a dead or absent agent", DeadSteps: 1, GardenerState: "none"},
			buildGated:  true,
			wantStalled: true,
			wantShow:    true,
		},
		{
			name:        "multi dead-agent stall badge STAYS loud on a build-gated plan",
			info:        planStallInfo{Stalled: true, Reason: "3 steps in progress with a dead or absent agent", DeadSteps: 3},
			buildGated:  true,
			wantStalled: true,
			wantShow:    true,
		},
		{
			name:        "healthy info stays healthy (never invents a stall)",
			info:        planStallInfo{Stalled: false, GardenerState: "running"},
			buildGated:  true,
			wantStalled: false,
			wantShow:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reconcileStallWithLifecycle(tc.info, tc.buildGated)
			if got.Stalled != tc.wantStalled {
				t.Errorf("Stalled = %v, want %v (the reconcile must never change the lane/ranker signal)", got.Stalled, tc.wantStalled)
			}
			if got.ShowBadge() != tc.wantShow {
				t.Errorf("ShowBadge() = %v, want %v", got.ShowBadge(), tc.wantShow)
			}
			// A suppressed stall PRESERVES every underlying field — only the badge
			// render gates on ShowBadge.
			if got.GardenerState != tc.info.GardenerState || got.GardenerID != tc.info.GardenerID {
				t.Errorf("gardener identity must survive the reconcile: got state=%q id=%q, want state=%q id=%q",
					got.GardenerState, got.GardenerID, tc.info.GardenerState, tc.info.GardenerID)
			}
			if got.DeadSteps != tc.info.DeadSteps {
				t.Errorf("DeadSteps must survive the reconcile: got %d, want %d", got.DeadSteps, tc.info.DeadSteps)
			}
			if got.Reason != tc.info.Reason {
				t.Errorf("Reason must survive the reconcile: got %q, want %q", got.Reason, tc.info.Reason)
			}
		})
	}
}

// TestReconcileStallWithLifecycle_LaneAndAttentionUnchanged proves the iter-49
// suppression drops only the badge FACE — a build-gated plan whose gardener-state
// stall badge was suppressed still lanes into Stalled/Blocked and, when ready,
// still ranks in the ready attention tier (which already OUTRANKS stalled). It
// never drops off the operator's radar. This is the "suppress the redundant face,
// keep the signal" guarantee.
func TestReconcileStallWithLifecycle_LaneAndAttentionUnchanged(t *testing.T) {
	suppressed := reconcileStallWithLifecycle(
		planStallInfo{Stalled: true, Reason: "open work, but no gardener is bound to watch this plan", GardenerState: "none"},
		true, // build-gated (draft/ready)
	)
	if suppressed.ShowBadge() {
		t.Fatal("setup: expected the gardener-state stall badge to be suppressed on a build-gated plan")
	}
	if !suppressed.Stalled {
		t.Fatal("a suppressed stall must keep Stalled true so lane + ranker are unaffected")
	}
	// A ready plan still lanes into Stalled/Blocked (planBoardLaneKey unions the
	// stall in regardless) and ranks in the ready tier, which outranks stalled.
	ready := planRowView{Stall: suppressed, PlanState: string(planStateReady)}
	if got := planBoardLaneKey(ready); got != planBoardLaneStalled {
		t.Errorf("a suppressed-stall build-gated plan must still lane into %q, got %q", planBoardLaneStalled, got)
	}
	if got := planAttention(ready); got != planAttentionReady {
		t.Errorf("a ready plan must rank in the ready tier, got %v", got)
	}
	// A draft plan has no higher tier, so it keeps the stalled tier + lane slot.
	draft := planRowView{Stall: suppressed, PlanState: string(planStateDraft)}
	if got := planBoardLaneKey(draft); got != planBoardLaneStalled {
		t.Errorf("a suppressed-stall draft plan must still lane into %q, got %q", planBoardLaneStalled, got)
	}
	if got := planAttention(draft); got != planAttentionStalled {
		t.Errorf("a draft plan whose only signal is the (suppressed-face) stall must keep the stalled tier, got %v", got)
	}
}

// TestPlansIndex_NoStallBadgeWhenPreBuildLifecycle is the iter-49 render
// regression (ka-0000005352): it reproduces the exact double-signal the pour
// found on the live board — a card that renders the READY lifecycle chip AND
// "⚠ stalled · no gardener" at once, contradicting itself (a ready plan has no
// gardener BY DESIGN). With a no-gardener stall (seedPlansFake's default) AND
// plan_state=ready, the card must show the ready lifecycle chip and NO ⚠ stall
// badge. Proven to FAIL before the reconcile (both rendered) and PASS after.
func TestPlansIndex_NoStallBadgeWhenPreBuildLifecycle(t *testing.T) {
	f := seedPlansFake()
	// seedPlansFake has open work + no gardener → the no-gardener stall is armed.
	// Put the plan in the ready authoring lifecycle (build-gated).
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateReady)}
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The ready lifecycle chip MUST render (it explains why no gardener is bound)...
	if !strings.Contains(body, "plan-lifecycle-chip-ready") {
		t.Fatal("expected the READY lifecycle chip — the test setup did not put the plan in the ready state")
	}
	// ...and therefore the redundant, contradictory ⚠ stalled badge must NOT.
	if strings.Contains(body, "⚠ stalled") {
		t.Error("a draft/ready plan (no gardener BY DESIGN) must not also render '⚠ stalled · no gardener' — the badge contradicts the READY/DRAFT lifecycle chip beside it (iteration 49)")
	}
}

// TestPlansIndex_StallBadgeStandsOnBuildingPlan is the iter-49 boundary:
// suppression is scoped to the build-gated (draft/ready) lifecycle. A BUILDING
// plan is approved — its gardener SHOULD be running — so a no-gardener/paused
// stall on it is a REAL "nothing is watching" alarm the operator must see. The
// loud "⚠ stalled · no gardener" badge must SURVIVE plan_state=building.
func TestPlansIndex_StallBadgeStandsOnBuildingPlan(t *testing.T) {
	f := seedPlansFake()
	// Approved/building lifecycle, but still no gardener bound → a real stall.
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateBuilding)}
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans?operated_by=", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "⚠ stalled") {
		t.Error("a BUILDING plan (gardener expected to run) with no gardener bound must keep its loud '⚠ stalled' badge — the build-gated suppression is scoped to draft/ready (iteration 49 boundary)")
	}
	if !strings.Contains(body, "no gardener") {
		t.Error("the surviving stall on a building plan must still carry its '· no gardener' face reason")
	}
}

// TestPlanDetail_NoStallBadgeWhenPreBuildLifecycle is the cockpit twin of the
// iter-49 index regression: the /plans/{id} header ⚠ badge must be suppressed
// when the plan is still draft/ready (no gardener BY DESIGN), so the header agrees
// with the index card. Same trap, same fix — the stall reconciles against the
// plan_state on the same epic bead.
func TestPlanDetail_NoStallBadgeWhenPreBuildLifecycle(t *testing.T) {
	f := seedPlansFake()
	seedPlanGardener(f, "gd-cap1", "gd-plan1", "false") // gardener paused → stall armed
	f.beads["gd-plan1"].Fields = map[string]string{planStateFieldKey: string(planStateReady)}
	mux := mkServerWithNow(t, f, func() time.Time { return planAssistantTestNow })

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, plansIndexReq("/plans/gd-plan1", ""))
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "plan-lifecycle-chip-ready") {
		t.Fatal("expected the READY lifecycle chip on the cockpit in the iter-49 setup")
	}
	if strings.Contains(body, "⚠ stalled") {
		t.Error("the cockpit header must not flag '⚠ stalled' when the plan is still draft/ready — no gardener is bound BY DESIGN (iteration 49)")
	}
}
