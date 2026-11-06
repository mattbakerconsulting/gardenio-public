package steward

import (
	"context"
	"strings"
	"testing"
	"time"
)

var rulingNow = time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)

// ownerRules closes the plan's decision with an answer the daemon stamped as
// an operator-lane human's (resolver_admin), five minutes before rulingNow.
func ownerRules(t *testing.T, f *planFarm, decision, chosen string) {
	t.Helper()
	f.answer(t, decision, map[string]string{
		"chosen": chosen, "responded_by": "op@example.com",
		"resolver_subject": "op@example.com", "resolver_admin": "true",
	})
	f.bead(decision).ClosedAt = rulingNow.Add(-5 * time.Minute).Format(time.RFC3339)
}

func TestFindRuling_OwnersApprove(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	if r, _, err := FindRuling(context.Background(), f, f.bead(w.Plan), rulingNow); err != nil || r != nil {
		t.Fatalf("an open decision: %+v, %v; want no ruling", r, err)
	}
	ownerRules(t, f, w.Decision, PlanApprove)
	r, refused, err := FindRuling(context.Background(), f, f.bead(w.Plan), rulingNow)
	if err != nil || r == nil || len(refused) != 0 {
		t.Fatalf("FindRuling: %+v, %v, %v", r, refused, err)
	}
	want := Ruling{Decision: w.Decision, Plan: w.Plan, Land: land, Answer: PlanApprove, By: "oidc:op@example.com"}
	if *r != want {
		t.Fatalf("ruling %+v, want %+v", *r, want)
	}
	if !ServerApplies(context.Background(), f, w.Decision, rulingNow) {
		t.Error("ServerApplies: false for the ruling FindRuling picks")
	}
	a, err := AnswerPlan(context.Background(), f, r.Land, r.Decision, "plan-answer-reactor")
	if err != nil || a.State != "building" || !a.Applied {
		t.Fatalf("applying the ruling: %+v, %v", a, err)
	}
	// Re-running is a no-op.
	if a, err := AnswerPlan(context.Background(), f, r.Land, r.Decision, "plan-answer-reactor"); err != nil || a.Applied {
		t.Fatalf("second apply: %+v, %v; want nothing applied", a, err)
	}
}

// An agent cannot have the server apply an answer it shaped: an answer by a
// non-operator, one about another plan, one on a decision still open, and one
// past the window are each refused.
func TestFindRuling_Refuses(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *planFarm, w *WrittenPlan)
		reason string // "" = skipped silently (nothing to apply)
	}{
		"answered by a non-operator (bridge, machine or agent)": {
			mutate: func(f *planFarm, w *WrittenPlan) { f.bead(w.Decision).Fields["resolver_admin"] = "false" },
			reason: "not an operator-lane human",
		},
		"no resolver stamp, only a relayed responded_by": {
			mutate: func(f *planFarm, w *WrittenPlan) {
				delete(f.bead(w.Decision).Fields, "resolver_admin")
				delete(f.bead(w.Decision).Fields, "resolver_subject")
			},
			reason: "no recorded resolver",
		},
		"the decision is for another plan": {
			mutate: func(f *planFarm, w *WrittenPlan) { f.bead(w.Decision).Fields["requesting_plan"] = "tp-other" },
			reason: "requesting_plan is tp-other",
		},
		"the decision is in another land": {
			mutate: func(f *planFarm, w *WrittenPlan) { f.bead(w.Decision).Labels = []string{"project:bean-field"} },
			reason: "not a decision of " + land,
		},
		"answered more than a day ago": {
			mutate: func(f *planFarm, w *WrittenPlan) {
				f.bead(w.Decision).ClosedAt = rulingNow.Add(-RulingWindow - time.Minute).Format(time.RFC3339)
			},
			reason: "more than 24h0m0s",
		},
		// An open decision is not even listed: only closed ones are read.
		"the decision is still open":  {mutate: func(f *planFarm, w *WrittenPlan) { f.bead(w.Decision).Status = "open" }},
		"the card was dismissed":      {mutate: func(f *planFarm, w *WrittenPlan) { f.bead(w.Decision).Fields["chosen"] = "dismissed" }},
		"words with no option chosen": {mutate: func(f *planFarm, w *WrittenPlan) { f.bead(w.Decision).Fields["chosen"] = "" }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPlanFarm()
			w := writeTestPlan(t, f)
			ownerRules(t, f, w.Decision, PlanApprove)
			c.mutate(f, w)
			r, refused, err := FindRuling(context.Background(), f, f.bead(w.Plan), rulingNow)
			if err != nil || r != nil {
				t.Fatalf("FindRuling: %+v, %v; want no ruling", r, err)
			}
			if c.reason == "" && len(refused) != 0 {
				t.Errorf("refused %v, want it skipped with no reason", refused)
			}
			if c.reason != "" && (len(refused) != 1 || !strings.Contains(refused[0], c.reason)) {
				t.Errorf("refused %v, want one naming %q", refused, c.reason)
			}
			if ServerApplies(context.Background(), f, w.Decision, rulingNow) {
				t.Error("ServerApplies: true for a refused answer")
			}
			if st := f.bead(w.Plan).Fields["plan_state"]; st != "ready" {
				t.Errorf("plan_state %q, want ready", st)
			}
		})
	}
}

// A decision whose requesting_task names another plan never comes up for this
// plan, even if it lists the plan in requesting_plan.
func TestFindRuling_DecisionForAnotherPlanIsNotThisPlans(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	ownerRules(t, f, w.Decision, PlanApprove)
	f.bead(w.Decision).Fields["requesting_task"] = "tp-other"
	if r, _, err := FindRuling(context.Background(), f, f.bead(w.Plan), rulingNow); err != nil || r != nil {
		t.Fatalf("FindRuling: %+v, %v; want none", r, err)
	}
}

// Drop is applied too: the plan and its open steps close.
func TestFindRuling_Drop(t *testing.T) {
	f := newPlanFarm()
	w := writeTestPlan(t, f)
	ownerRules(t, f, w.Decision, PlanDrop)
	r, _, err := FindRuling(context.Background(), f, f.bead(w.Plan), rulingNow)
	if err != nil || r == nil || r.Answer != PlanDrop {
		t.Fatalf("FindRuling: %+v, %v", r, err)
	}
	if a, err := AnswerPlan(context.Background(), f, r.Land, r.Decision, "plan-answer-reactor"); err != nil || a.State != "abandoned" {
		t.Fatalf("drop: %+v, %v", a, err)
	}
	if f.bead(w.Plan).Status != "closed" || f.bead(w.Steps[0]).Status != "closed" {
		t.Error("drop must close the plan and its open steps")
	}
}
