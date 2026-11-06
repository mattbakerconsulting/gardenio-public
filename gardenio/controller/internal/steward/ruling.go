package steward

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Plan rulings applied by the server (gd-Dl88Htipuq, owner decision
// gd-fBSlZfEvmo item 2). When the owner answers a plan's approve / change /
// drop card, the beads-viewer's plan answer reactor applies the answer itself,
// the same way gb lands plan answer does (AnswerPlan, and on a home-garden
// approve the gardener start). The agent that asked no longer has to, so it
// files no escalation for the owner to click a second time.
//
// Whose answer counts. Only one the daemon stamped as given by an operator-lane
// human: resolver_admin=true, which kbeads writes on every answering write
// from an OIDC or edge-auth session holding gardenio-admin, and which no
// client can write (decisionResolverStamp). An answer relayed by a bridge, a
// machine or an agent token is left to gb lands plan answer as before; so is
// a tenant's own answer on a tenant land, which the land's steward applies.
// And only an answer the decision gives about this plan: its requesting_task
// (and requesting_plan, when set) names the plan, both fixed at create, and it
// is a decision of the plan's land.
//
// What counts as an answer: an option chosen (approve, change or drop). A
// dismissed card or words with no option apply nothing, as in AnswerPlan.
//
// The answer must be recent (RulingWindow), so a plan approved long ago and
// still unbuilt is not started by surprise, and so the gardener the approve
// starts is sealed from that approval (kbeads planApprovalSealWindow).

// RulingWindow is how long after the owner answers the server still applies
// the answer. It matches kbeads' planApprovalSealWindow.
const RulingWindow = 24 * time.Hour

// Ruling is an owner's answer on a plan decision the server may apply.
type Ruling struct {
	Decision string
	Plan     string
	Land     string
	// Answer is approve, change or drop.
	Answer string
	// By is the daemon-stamped resolver, as oidc:<email>.
	By string
}

// FindRuling returns the newest answer on plan's decisions that the server
// may apply, or nil when there is none. refused names each answer it would
// not apply, with why, for the caller's log.
func FindRuling(ctx context.Context, st PlanStore, plan *beadsapi.BeadDetail, now time.Time) (r *Ruling, refused []string, err error) {
	land := planProject(plan)
	if land == "" {
		return nil, nil, nil
	}
	res, err := st.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"closed"},
		Fields:   map[string]string{"requesting_task": plan.ID},
		Limit:    50,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing the decisions of plan %s: %w", plan.ID, err)
	}
	var newest time.Time
	for _, dec := range res.Beads {
		answer := chosenOption(dec.Fields)
		if answer != PlanApprove && answer != PlanChange && answer != PlanDrop {
			continue
		}
		at, why := rulingRefused(dec, plan.ID, land, now)
		if why != "" {
			refused = append(refused, dec.ID+": "+why)
			continue
		}
		if r == nil || at.After(newest) {
			newest = at
			r = &Ruling{Decision: dec.ID, Plan: plan.ID, Land: land, Answer: answer,
				By: "oidc:" + strings.TrimPrefix(strings.TrimSpace(dec.Fields["resolver_subject"]), "oidc:")}
		}
	}
	return r, refused, nil
}

// rulingRefused says why the server must not apply dec's answer to plan, or
// "" with the time it was answered when it may.
func rulingRefused(dec *beadsapi.BeadDetail, plan, land string, now time.Time) (time.Time, string) {
	f := dec.Fields
	switch {
	case dec.Type != "decision" || dec.Status != "closed":
		return time.Time{}, "not a closed decision"
	case strings.TrimSpace(f["requesting_task"]) != plan:
		return time.Time{}, "it asks about " + strings.TrimSpace(f["requesting_task"]) + ", not " + plan
	case strings.TrimSpace(f["requesting_plan"]) != "" && strings.TrimSpace(f["requesting_plan"]) != plan:
		return time.Time{}, "its requesting_plan is " + strings.TrimSpace(f["requesting_plan"]) + ", not " + plan
	case !slices.Contains(dec.Labels, "project:"+land):
		return time.Time{}, "it is not a decision of " + land
	case f["resolver_admin"] != "true":
		by := strings.TrimSpace(f["resolver_subject"])
		if by == "" {
			by = "no recorded resolver"
		}
		return time.Time{}, "answered by " + by + ", not an operator-lane human (resolver_admin)"
	case !strings.Contains(f["resolver_subject"], "@"):
		return time.Time{}, "the resolver stamp names no person"
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(dec.ClosedAt))
	if err != nil {
		return time.Time{}, "no closed_at"
	}
	if now.Sub(at) > RulingWindow {
		return time.Time{}, fmt.Sprintf("answered %s ago, more than %s", now.Sub(at).Round(time.Minute), RulingWindow)
	}
	return at, ""
}

// ServerApplies reports whether the server applies the answer on decisionID
// itself: it is the ruling FindRuling picks for its plan. gb lands plan
// answer says so when its own apply fails, so the agent does not escalate.
func ServerApplies(ctx context.Context, st PlanStore, decisionID string, now time.Time) bool {
	dec, err := st.GetBead(ctx, decisionID)
	if err != nil || dec == nil {
		return false
	}
	plan, err := st.GetBead(ctx, strings.TrimSpace(dec.Fields["requesting_task"]))
	if err != nil || plan == nil || plan.Type != "epic" || !slices.Contains(plan.Labels, "plan") {
		return false
	}
	r, _, err := FindRuling(ctx, st, plan, now)
	return err == nil && r != nil && r.Decision == decisionID
}

// planProject is the plan's one project: label, or "" when it has none or
// several.
func planProject(b *beadsapi.BeadDetail) string {
	var land string
	for _, l := range b.Labels {
		if p, ok := strings.CutPrefix(l, "project:"); ok {
			if land != "" {
				return ""
			}
			land = p
		}
	}
	return land
}
