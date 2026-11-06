package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/steward"
)

// An approved home-garden plan starts with nobody running another command:
// the answer binds the plan's gardener enabled and fires its first cycle on
// the plan; a second run finds the cycle live and fires no other.
func TestStartHomePlan_BindsAndFires(t *testing.T) {
	stub := newGardenerStub()
	plan := makeEpic("gd-PLAN1", "Wishes as goats", "plan", "project:gardenio")
	plan["fields"] = map[string]any{"plan_state": "building"}
	stub.addBead(plan)
	setupGardenerStub(t, stub)

	s, err := startHomePlan(context.Background(), "gd-PLAN1", "oidc:op@example.com")
	if err != nil {
		t.Fatalf("startHomePlan: %v", err)
	}
	var trig, agent map[string]any
	for _, c := range stub.creates {
		switch c["type"] {
		case "trigger":
			trig = c
		case "agent":
			agent = c
		}
	}
	if trig == nil || agent == nil || len(stub.creates) != 2 {
		t.Fatalf("creates %v, want one gardener and one cycle", stub.creates)
	}
	tf, _ := trig["fields"].(map[string]any)
	if tf["enabled"] != true || tf["role"] != "plan-gardener" || tf["project"] != "gardenio" {
		t.Errorf("gardener fields %v, want enabled plan-gardener in gardenio", tf)
	}
	af, _ := agent["fields"].(map[string]any)
	if af["task_id"] != "gd-PLAN1" || af["spawned_by"] != "trigger:"+s.ID {
		t.Errorf("cycle fields %v, want the plan and the gardener's lineage", af)
	}
	if !s.Bound || s.Cycle == "" || s.FireError != "" {
		t.Errorf("start %+v", s)
	}
	if got := describeHomePlanStart(s); !strings.Contains(got, "bound and enabled") || !strings.Contains(got, "first cycle: "+s.Cycle) {
		t.Errorf("printout:\n%s", got)
	}

	again, err := startHomePlan(context.Background(), "gd-PLAN1", "oidc:op@example.com")
	if err != nil || again.Bound || again.Live == "" || len(stub.creates) != 2 {
		t.Errorf("second run %+v, %v, creates %d; want the live cycle and nothing new", again, err, len(stub.creates))
	}
}

// A gardener the daemon sealed to the owner's approval is left to its cron
// (gd-YaoqmhIx8x): firing it from the thread's own token would only file an
// escalation for the privileged plan-gardener role.
func TestStartHomePlan_SealedGardenerLeftToCron(t *testing.T) {
	stub := newGardenerStub()
	stub.sealTriggerCreates = true
	plan := makeEpic("gd-PLAN1", "Wishes as goats", "plan", "project:gardenio")
	plan["fields"] = map[string]any{"plan_state": "building"}
	stub.addBead(plan)
	setupGardenerStub(t, stub)

	s, err := startHomePlan(context.Background(), "gd-PLAN1", "oidc:op@example.com")
	if err != nil {
		t.Fatalf("startHomePlan: %v", err)
	}
	if !s.Bound || !s.Sealed || s.Cycle != "" || s.Escalation != "" {
		t.Errorf("start %+v, want a bound, sealed gardener and no fired cycle", s)
	}
	for _, c := range stub.creates {
		if c["type"] != "trigger" {
			t.Errorf("unexpected create %v: a sealed gardener's first cycle is the engine's", c)
		}
	}
	if got := describeHomePlanStart(s); !strings.Contains(got, "sealed to the owner's approval") {
		t.Errorf("printout:\n%s", got)
	}
}

// The answer's printout tells a home-garden thread agent that the gardener
// takes the first step, not that it should spawn one.
func TestDescribePlanAnswer_Home(t *testing.T) {
	a := &steward.PlanAnswer{Plan: "gd-PLAN1", Title: "Goats", Answer: steward.PlanApprove, State: "building", Applied: true,
		Next: "gd-S1", NextTitle: "Draw the goat", NextStatus: "open", Home: true}
	if got := describePlanAnswer(a); !strings.Contains(got, "the plan's gardener spawns a crew on it") || strings.Contains(got, "spawn a gardener on it") {
		t.Errorf("home printout:\n%s", got)
	}
	a.Home = false
	if got := describePlanAnswer(a); !strings.Contains(got, "spawn a gardener on it") {
		t.Errorf("land printout:\n%s", got)
	}
}

// The thread reply names the plan's arms before its title, as every line
// that names a plan does; a plan with none reads as before.
func TestPlanWrittenReply_Arms(t *testing.T) {
	d := steward.PlanDraft{Title: "Add a watering schedule"}
	res := &steward.WrittenPlan{Plan: "gd-PLAN1", Steps: []string{"gd-S1", "gd-S2"}, Owner: "oidc:ada@example.com", Arms: "🚿⏰📅"}
	want := `I wrote this up as plan gd-PLAN1, 🚿⏰📅 "Add a watering schedule", in 2 steps, and asked ada@example.com to answer the card to run it. Nothing runs until they do.`
	if got := planWrittenReply(res, d); got != want {
		t.Errorf("reply with arms:\n got %s\nwant %s", got, want)
	}
	res.Arms = ""
	if got := planWrittenReply(res, d); !strings.Contains(got, `plan gd-PLAN1, "Add a watering schedule", in 2 steps`) {
		t.Errorf("reply without arms: %s", got)
	}
}

// A plan the owner asked for in the thread is building: the reply is the
// "change or drop" notice, not a pointer to a card.
func TestPlanWrittenReply_Building(t *testing.T) {
	d := steward.PlanDraft{Title: "Add a watering schedule"}
	res := &steward.WrittenPlan{Plan: "gd-PLAN1", Steps: []string{"gd-S1"}, Owner: "oidc:ada@example.com", Arms: "🚿⏰📅", Building: true}
	got := planWrittenReply(res, d)
	for _, want := range []string{`plan gd-PLAN1, 🚿⏰📅 "Add a watering schedule", in 1 step, is building`, "To change it or drop it, say so in this thread."} {
		if !strings.Contains(got, want) {
			t.Errorf("building reply %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "card") {
		t.Errorf("building reply mentions a card: %s", got)
	}
}
