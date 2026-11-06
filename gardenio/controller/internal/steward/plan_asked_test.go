package steward

import (
	"context"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// A plan the home garden's owner asks for in their own thread starts building
// with no card (P8.7, owner ruling 3 on gd-7qUpu15BBK): bound to the thread,
// so the server seals the gardener the thread binds. Anything else still asks.

const (
	askHead  = "kd-head1"
	askOwner = "oidc:op@example.com"
)

// newAskFarm is the home garden, operated by askOwner, with a thread head the
// owner opened and a reply in it.
func newAskFarm() *planFarm {
	f := newPlanFarm()
	home := f.bead("kd-proj")
	home.Fields["land_default_channel"] = "kd-general"
	home.OperatedBy = askOwner
	f.beads = append(f.beads,
		&beadsapi.BeadDetail{ID: askHead, Type: "task", Status: "open", CreatedBy: "Op@Example.com",
			Labels: []string{"project:gardenio", "source:slack", "thread"}, Fields: map[string]string{}},
		&beadsapi.BeadDetail{ID: "kd-reply1", Type: "message", Status: "closed", CreatedBy: "Op@Example.com",
			Labels: []string{"project:gardenio"}, Fields: map[string]string{"thread_id": askHead, "author": askOwner}},
	)
	return f
}

func askOpts(thread string) WritePlanOptions {
	o := writeOpts()
	o.Thread, o.AgentThread, o.GiftedBy = thread, askHead, askOwner
	return o
}

func TestWritePlan_OwnerAskedInThreadStartsBuilding(t *testing.T) {
	for _, thread := range []string{askHead, "kd-reply1"} {
		t.Run(thread, func(t *testing.T) {
			f := newAskFarm()
			d, _ := ParsePlan(planMD)
			w, err := WritePlan(context.Background(), f, "gardenio", d, askOpts(thread))
			if err != nil {
				t.Fatalf("WritePlan: %v", err)
			}
			if !w.Building || w.Decision != "" {
				t.Fatalf("building %v decision %q, want building and no card", w.Building, w.Decision)
			}
			plan := f.bead(w.Plan)
			// type:epic declares no thread_id, so the create must not send it;
			// the daemon stamps the thread agent's own thread_id.
			if plan.Fields["plan_state"] != "building" || plan.Fields["thread_id"] != "" {
				t.Errorf("plan fields %v, want building with no thread_id on the create", plan.Fields)
			}
			for _, c := range f.created {
				if c.Type == "decision" {
					t.Fatalf("a card was filed: %q", c.Title)
				}
			}
			if !slices.ContainsFunc(f.comments, func(c string) bool {
				return strings.HasPrefix(c, w.Plan+": plan_state → building") && strings.Contains(c, askHead)
			}) {
				t.Errorf("no building comment naming the thread on the plan: %v", f.comments)
			}
		})
	}
}

func TestWritePlan_AsksTheOwnerWhen(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*planFarm)
		opts   func(*WritePlanOptions)
	}{
		"the agent proposes it":         {opts: func(o *WritePlanOptions) { o.Propose = true }},
		"the agent has no thread":       {opts: func(o *WritePlanOptions) { o.AgentThread = "" }},
		"it is another agent's thread":  {opts: func(o *WritePlanOptions) { o.AgentThread = "kd-head2" }},
		"the card is for someone else":  {opts: func(o *WritePlanOptions) { o.Owner = "oidc:bob@example.com" }},
		"the thread is unknown":         {opts: func(o *WritePlanOptions) { o.Thread = "kd-gone" }},
		"an agent opened the thread":    {mutate: func(f *planFarm) { f.bead(askHead).CreatedBy = "gardenio-crew-abc" }},
		"another person opened it":      {mutate: func(f *planFarm) { f.bead(askHead).CreatedBy = "bob@example.com" }},
		"the head is not a thread head": {mutate: func(f *planFarm) { f.bead(askHead).Labels = []string{"project:gardenio"} }},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAskFarm()
			if tc.mutate != nil {
				tc.mutate(f)
			}
			o := askOpts(askHead)
			if tc.opts != nil {
				tc.opts(&o)
			}
			d, _ := ParsePlan(planMD)
			w, err := WritePlan(context.Background(), f, "gardenio", d, o)
			if err != nil {
				t.Fatalf("WritePlan: %v", err)
			}
			if w.Building || w.Decision == "" || f.bead(w.Plan).Fields["plan_state"] != "ready" {
				t.Fatalf("building %v decision %q state %q, want ready and a card", w.Building, w.Decision, f.bead(w.Plan).Fields["plan_state"])
			}
		})
	}
}

// A tenant land keeps the steward's card even in its owner's thread.
func TestWritePlan_TenantLandStillAsks(t *testing.T) {
	f := newPlanFarm()
	o := writeOpts()
	o.AgentThread = o.Thread
	d, _ := ParsePlan(planMD)
	w, err := WritePlan(context.Background(), f, land, d, o)
	if err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	if w.Building || w.Decision == "" {
		t.Fatalf("building %v decision %q, want a card", w.Building, w.Decision)
	}
}
