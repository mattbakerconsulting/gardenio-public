package bridge

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// darkBudgetDaemon is a self-contained gardenerDaemon stub for the per-agent/day
// token-budget guard (R6-FU6c). It serves a fixed set of agent beads on the
// spawned_by query and records the agent_state writes + comments the guard
// performs, so a test can assert (a) crossing the ceiling flips
// agent_state=budget_exceeded and suppresses the spawn, and (b) under-budget
// agents are left untouched.
type darkBudgetDaemon struct {
	agents []*beadsapi.BeadDetail

	// recorded writes
	stateWrites map[string]string // agent ID → agent_state value written
	comments    map[string][]string
}

func newDarkBudgetDaemon(agents ...*beadsapi.BeadDetail) *darkBudgetDaemon {
	return &darkBudgetDaemon{
		agents:      agents,
		stateWrites: map[string]string{},
		comments:    map[string][]string{},
	}
}

func (d *darkBudgetDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	for _, a := range d.agents {
		if a.ID == id {
			return a, nil
		}
	}
	return nil, nil
}

func (d *darkBudgetDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	// The global gardeners-pause kill-switch lookup (gate search) must report
	// "disengaged" so the budget gate is the one exercised, not the kill-switch.
	if q.Search == gardenersPauseTitle {
		return &beadsapi.ListBeadsResult{}, nil
	}
	// Otherwise this is the spawned-agents query.
	if len(q.Types) == 1 && q.Types[0] == "agent" {
		return &beadsapi.ListBeadsResult{Beads: d.agents}, nil
	}
	return &beadsapi.ListBeadsResult{}, nil
}

func (d *darkBudgetDaemon) UpdateBeadFields(_ context.Context, id string, fields map[string]string) error {
	if v, ok := fields["agent_state"]; ok {
		d.stateWrites[id] = v
		// Reflect the write back onto the in-memory bead so idempotency holds.
		for _, a := range d.agents {
			if a.ID == id {
				if a.Fields == nil {
					a.Fields = map[string]string{}
				}
				a.Fields["agent_state"] = v
			}
		}
	}
	return nil
}

func (d *darkBudgetDaemon) UpdateBeadFieldsTyped(_ context.Context, _ string, _ map[string]any) error {
	return nil
}

func (d *darkBudgetDaemon) AddComment(_ context.Context, id, _, text string) error {
	d.comments[id] = append(d.comments[id], text)
	return nil
}

func (d *darkBudgetDaemon) AddLabel(_ context.Context, _, _ string) error {
	return nil
}

func (d *darkBudgetDaemon) CloseBead(_ context.Context, _ string, _ map[string]string) error {
	return nil
}

func (d *darkBudgetDaemon) UpdateBead(_ context.Context, _ string, _ beadsapi.UpdateBeadRequest) error {
	return nil
}

func (d *darkBudgetDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// darkAgentBead builds an agent bead spawned by the given gardener trigger,
// created `now`, carrying the given total/live spend and agent_state.
func darkAgentBead(id, triggerID, state string, totalUSD, liveUSD float64, createdAt time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:   id,
		Type: "agent",
		Fields: map[string]string{
			"spawned_by":     "trigger:" + triggerID,
			"agent_state":    state,
			"total_cost_usd": floatStr(totalUSD),
			"live_cost_usd":  floatStr(liveUSD),
		},
		CreatedAt: createdAt,
	}
}

func floatStr(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

func darkBudgetGardenerTrigger(t *testing.T, budgetUSD float64) TriggerDef {
	t.Helper()
	spec := GardenerSpec{
		AssignBead:          "gd-epic",
		RunBudget:           20,
		Stop:                json.RawMessage(boundedStopBlob(t, "168h")),
		AgentDailyBudgetUSD: budgetUSD,
	}
	return TriggerDef{
		ID:       "gd-darkcap",
		Project:  "gardenio",
		Labels:   []string{"role:gardener", "monorepo-dark"},
		Gardener: &spec,
	}
}

// (a) Crossing the daily ceiling flips agent_state=budget_exceeded AND
// suppresses the gardener's spawn.
func TestEnforceDarkAgentDailyBudget_OverBudgetFlagsAndSuppresses(t *testing.T) {
	now := time.Now().UTC()
	over := darkAgentBead("gd-agent-over", "gd-darkcap", "working", 30.0, 0, now)
	d := newDarkBudgetDaemon(over)
	trig := darkBudgetGardenerTrigger(t, 25.0)

	exceeded, agents := enforceDarkAgentDailyBudget(context.Background(), d, nil, trig, now)
	if !exceeded {
		t.Fatal("expected exceeded=true for an over-budget agent")
	}
	if len(agents) != 1 || agents[0] != "gd-agent-over" {
		t.Fatalf("expected [gd-agent-over], got %v", agents)
	}
	if d.stateWrites["gd-agent-over"] != "budget_exceeded" {
		t.Errorf("expected agent_state=budget_exceeded, got %q", d.stateWrites["gd-agent-over"])
	}
	if len(d.comments["gd-agent-over"]) == 0 {
		t.Error("expected an overrun comment on the over-budget agent")
	}
}

// (a) Through the full pre-fire path: an over-budget dark agent suppresses the
// gardener fire with the dark_agent_budget_exceeded reason.
func TestGardenerPreFireCheck_DarkAgentOverBudgetSuppressed(t *testing.T) {
	now := time.Now().UTC()
	over := darkAgentBead("gd-agent-over", "gd-darkcap", "working", 30.0, 5.0, now)
	d := newDarkBudgetDaemon(over)
	trig := darkBudgetGardenerTrigger(t, 25.0)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if !skip || reason != gardenerSkipDarkAgentBudget {
		t.Fatalf("expected dark_agent_budget_exceeded skip, got skip=%v reason=%q", skip, reason)
	}
	if d.stateWrites["gd-agent-over"] != "budget_exceeded" {
		t.Errorf("expected agent flipped to budget_exceeded, got %q", d.stateWrites["gd-agent-over"])
	}
}

// (b) Under-budget agents are unaffected: no state flip, no suppression.
func TestEnforceDarkAgentDailyBudget_UnderBudgetUnaffected(t *testing.T) {
	now := time.Now().UTC()
	under := darkAgentBead("gd-agent-under", "gd-darkcap", "working", 10.0, 2.0, now)
	d := newDarkBudgetDaemon(under)
	trig := darkBudgetGardenerTrigger(t, 25.0)

	exceeded, agents := enforceDarkAgentDailyBudget(context.Background(), d, nil, trig, now)
	if exceeded {
		t.Fatal("expected exceeded=false for an under-budget agent")
	}
	if len(agents) != 0 {
		t.Fatalf("expected no exceeded agents, got %v", agents)
	}
	if _, ok := d.stateWrites["gd-agent-under"]; ok {
		t.Errorf("under-budget agent must not be re-stated, got %q", d.stateWrites["gd-agent-under"])
	}
	if len(d.comments["gd-agent-under"]) != 0 {
		t.Error("under-budget agent must not be commented on")
	}
}

// (b) Through the full pre-fire path: an under-budget dark agent does NOT cause
// a dark-budget skip (it may fall through to a later gate, but never with the
// budget reason).
func TestGardenerPreFireCheck_DarkAgentUnderBudgetNotSuppressed(t *testing.T) {
	now := time.Now().UTC()
	under := darkAgentBead("gd-agent-under", "gd-darkcap", "working", 5.0, 1.0, now)
	d := newDarkBudgetDaemon(under)
	trig := darkBudgetGardenerTrigger(t, 25.0)

	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && reason == gardenerSkipDarkAgentBudget {
		t.Fatalf("under-budget agent wrongly suppressed with budget reason")
	}
	if _, ok := d.stateWrites["gd-agent-under"]; ok {
		t.Error("under-budget agent must not be flipped to budget_exceeded")
	}
}

// Agents created on a PRIOR day must not re-trip today's per-agent/day gate.
func TestEnforceDarkAgentDailyBudget_PriorDayIgnored(t *testing.T) {
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1)
	old := darkAgentBead("gd-agent-old", "gd-darkcap", "working", 99.0, 0, yesterday)
	d := newDarkBudgetDaemon(old)
	trig := darkBudgetGardenerTrigger(t, 25.0)

	exceeded, agents := enforceDarkAgentDailyBudget(context.Background(), d, nil, trig, now)
	if exceeded || len(agents) != 0 {
		t.Fatalf("prior-day agent must not trip today's gate: exceeded=%v agents=%v", exceeded, agents)
	}
	if _, ok := d.stateWrites["gd-agent-old"]; ok {
		t.Error("prior-day agent must not be flipped")
	}
}

// Already-flagged agents are not re-written or re-commented (idempotent).
func TestMarkAgentBudgetExceeded_Idempotent(t *testing.T) {
	now := time.Now().UTC()
	already := darkAgentBead("gd-agent-flagged", "gd-darkcap", "budget_exceeded", 50.0, 0, now)
	d := newDarkBudgetDaemon(already)
	trig := darkBudgetGardenerTrigger(t, 25.0)

	// Still over budget, but already flagged: it should still count as exceeded
	// (so the spawn stays suppressed) but must NOT be re-commented.
	exceeded, agents := enforceDarkAgentDailyBudget(context.Background(), d, nil, trig, now)
	if !exceeded || len(agents) != 1 {
		t.Fatalf("already-flagged over-budget agent should still suppress: exceeded=%v agents=%v", exceeded, agents)
	}
	if len(d.comments["gd-agent-flagged"]) != 0 {
		t.Error("already-flagged agent must not be re-commented")
	}
}

// A negative AgentDailyBudgetUSD disables the per-agent guard entirely.
func TestEnforceDarkAgentDailyBudget_DisabledByNegativeBudget(t *testing.T) {
	now := time.Now().UTC()
	over := darkAgentBead("gd-agent-over", "gd-darkcap", "working", 999.0, 0, now)
	d := newDarkBudgetDaemon(over)
	trig := darkBudgetGardenerTrigger(t, -1) // explicit opt-out

	exceeded, agents := enforceDarkAgentDailyBudget(context.Background(), d, nil, trig, now)
	if exceeded || len(agents) != 0 {
		t.Fatalf("negative budget must disable the guard: exceeded=%v agents=%v", exceeded, agents)
	}
	if _, ok := d.stateWrites["gd-agent-over"]; ok {
		t.Error("disabled guard must not flip any agent")
	}
}

// Zero budget falls back to the default ceiling.
func TestEffectiveAgentDailyBudgetUSD_Defaulting(t *testing.T) {
	if b, ok := effectiveAgentDailyBudgetUSD(&GardenerSpec{}); !ok || b != darkDefaultAgentDailyBudgetUSD {
		t.Errorf("zero budget should default: got (%v, %v), want (%v, true)", b, ok, darkDefaultAgentDailyBudgetUSD)
	}
	if b, ok := effectiveAgentDailyBudgetUSD(&GardenerSpec{AgentDailyBudgetUSD: 5}); !ok || b != 5 {
		t.Errorf("positive budget should pass through: got (%v, %v)", b, ok)
	}
	if _, ok := effectiveAgentDailyBudgetUSD(&GardenerSpec{AgentDailyBudgetUSD: -1}); ok {
		t.Error("negative budget should disable the guard")
	}
}

// agentSpendSoFarUSD adds live estimate only for live agents; a terminal agent
// counts total_cost_usd alone (no double-count).
func TestAgentSpendSoFarUSD_LiveVsTerminal(t *testing.T) {
	now := time.Now().UTC()
	live := darkAgentBead("gd-live", "gd-darkcap", "working", 10.0, 3.0, now)
	if got := agentSpendSoFarUSD(live); got != 13.0 {
		t.Errorf("live agent spend = %v, want 13.0 (total+live)", got)
	}
	term := darkAgentBead("gd-term", "gd-darkcap", "done", 10.0, 3.0, now)
	if got := agentSpendSoFarUSD(term); got != 10.0 {
		t.Errorf("terminal agent spend = %v, want 10.0 (total only)", got)
	}
}

// Non-dark gardeners must never be subject to the per-agent budget guard, even
// with an over-budget spawned agent — the gate lives inside the dark branch.
func TestGardenerPreFireCheck_NonDarkAgentBudgetUnaffected(t *testing.T) {
	now := time.Now().UTC()
	over := darkAgentBead("gd-agent-over", "gd-normalcap", "working", 999.0, 0, now)
	d := newDarkBudgetDaemon(over)
	spec := GardenerSpec{
		AssignBead:          "gd-epic",
		RunBudget:           20,
		Stop:                json.RawMessage(boundedStopBlob(t, "168h")),
		AgentDailyBudgetUSD: 25.0,
	}
	trig := TriggerDef{
		ID:       "gd-normalcap",
		Project:  "gardenio",
		Labels:   []string{"role:gardener", "project:gardenio"},
		Gardener: &spec,
	}
	skip, reason := gardenerPreFireCheck(context.Background(), d, nil, trig)
	if skip && reason == gardenerSkipDarkAgentBudget {
		t.Fatal("non-dark gardener wrongly subject to dark per-agent budget guard")
	}
	if _, ok := d.stateWrites["gd-agent-over"]; ok {
		t.Error("non-dark gardener's agent must not be flipped by the dark guard")
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (d *darkBudgetDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *darkBudgetDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *darkBudgetDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
