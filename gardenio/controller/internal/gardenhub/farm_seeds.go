package gardenhub

import (
	"context"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// seedAsk is what an open seed decision's card needs that the work rows omit
// (farmRowOmitFields drops prompt and options): read once per decision, since
// a decision's question and options never change.
type seedAsk struct {
	prompt  string
	options []QueueOption
}

const (
	maxSeedSteps   = 24  // steps listed on a ready or asked seed
	seedSummaryMax = 600 // runes of a ready or asked seed's description
)

// seedNarr is a ready or asked seed's summary as read at the plan's updated_at:
// the work rows omit descriptions (farmRowOmit), so it is read apart and kept
// until the plan changes.
type seedNarr struct {
	updated time.Time
	summary string
}

// seedPlans picks the seeds out of the plans: those still in the Nursery
// (plan_state draft or ready).
func seedPlans(planIDs []string, planSet map[string]*beadsapi.BeadDetail) map[string]*beadsapi.BeadDetail {
	seeds := map[string]*beadsapi.BeadDetail{}
	for _, pid := range planIDs {
		if st := strings.TrimSpace(planSet[pid].Fields["plan_state"]); st == "draft" || st == "ready" {
			seeds[pid] = planSet[pid]
		}
	}
	return seeds
}

// seedState is a seed's Seed* state from its plan_state, overlaid by its open
// decision: a seed with one is SeedAsked and carries it.
func seedState(planState string, d *FarmSeedAsk) (string, *FarmSeedAsk) {
	switch {
	case d != nil:
		return SeedAsked, d
	case planState == "ready":
		return SeedReady, nil
	}
	return SeedFrosted, nil
}

// seedDecisions finds each seed's open decision in the work rows: a decision
// whose requesting_plan (else requesting_task) names the seed's plan, the
// newest when there are more. The prompt and options of a decision not seen
// before are read in one batch; a failed read leaves that card with the
// decision's title and a written answer, and is tried again next refresh.
func (f *Farm) seedDecisions(ctx context.Context, rows []*beadsapi.BeadDetail, seeds map[string]*beadsapi.BeadDetail) map[string]*FarmSeedAsk {
	byPlan := map[string]*beadsapi.BeadDetail{}
	for _, b := range rows {
		if b.Type != "decision" || b.Status == "closed" {
			continue
		}
		plan := strings.TrimSpace(b.Fields["requesting_plan"])
		if plan == "" {
			plan = strings.TrimSpace(b.Fields["requesting_task"])
		}
		if _, ok := seeds[plan]; !ok {
			continue
		}
		if o := byPlan[plan]; o == nil || b.CreatedAt.After(o.CreatedAt) || (b.CreatedAt.Equal(o.CreatedAt) && b.ID > o.ID) {
			byPlan[plan] = b
		}
	}

	live := make(map[string]struct{}, len(byPlan))
	var unread []string
	for _, d := range byPlan {
		live[d.ID] = struct{}{}
		if _, ok := f.seedAsks[d.ID]; !ok {
			unread = append(unread, d.ID)
		}
	}
	for id := range f.seedAsks {
		if _, ok := live[id]; !ok {
			delete(f.seedAsks, id)
		}
	}
	if len(unread) > 0 {
		got, err := f.src.BatchGetBeads(ctx, unread)
		if err != nil {
			f.logger.Warn("garden farm: reading the seeds' decisions failed", "error", err, "decisions", len(unread))
		}
		for _, id := range unread {
			if b := got[id]; b != nil {
				f.seedAsks[id] = seedAsk{prompt: strings.TrimSpace(b.Fields["prompt"]), options: queueOptions(b.Fields["options"])}
			}
		}
	}

	out := make(map[string]*FarmSeedAsk, len(byPlan))
	for plan, d := range byPlan {
		a := f.seedAsks[d.ID]
		prompt := a.prompt
		if prompt == "" {
			prompt = d.Title
		}
		op := d.OperatedBy
		if op == "" {
			op = d.Fields["operated_by"]
		}
		out[plan] = &FarmSeedAsk{
			ID: d.ID, Prompt: seedPrompt(prompt, plan, seeds[plan].Title), Options: a.options,
			For: lower(op), Since: d.CreatedAt.Unix(),
		}
	}
	return out
}

// seedSummaries is each ready or asked seed's summary: its description cut at
// seedSummaryMax runes. Descriptions not seen at the plan's current updated_at
// are read in one batch; a failed read leaves those seeds without one, and is
// tried again next refresh.
func (f *Farm) seedSummaries(ctx context.Context, seeds map[string]*beadsapi.BeadDetail, asks map[string]*FarmSeedAsk) map[string]string {
	var unread []string
	live := map[string]struct{}{}
	for pid, b := range seeds {
		if st, _ := seedState(strings.TrimSpace(b.Fields["plan_state"]), asks[pid]); st == SeedFrosted {
			continue
		}
		live[pid] = struct{}{}
		if n, ok := f.seedNarrs[pid]; !ok || !n.updated.Equal(b.UpdatedAt) {
			unread = append(unread, pid)
		}
	}
	for pid := range f.seedNarrs {
		if _, ok := live[pid]; !ok {
			delete(f.seedNarrs, pid)
		}
	}
	if len(unread) > 0 {
		sort.Strings(unread)
		got, err := f.src.BatchGetBeads(ctx, unread)
		if err != nil {
			f.logger.Warn("garden farm: reading the seeds' descriptions failed", "error", err, "plans", len(unread))
		}
		for _, pid := range unread {
			if b := got[pid]; b != nil {
				f.seedNarrs[pid] = seedNarr{updated: seeds[pid].UpdatedAt, summary: clipKeep(b.Description, seedSummaryMax)}
			} else {
				delete(f.seedNarrs, pid) // an older summary is not this plan's any more
			}
		}
	}
	out := make(map[string]string, len(live))
	for pid := range live {
		if n, ok := f.seedNarrs[pid]; ok && n.summary != "" {
			out[pid] = n.summary
		}
	}
	return out
}

// seedSteps is a seed's open steps (its work beads, under the plan or its
// phases) in the plan's step_order, those it does not list after them by
// created, at most maxSeedSteps. N numbers them in that order from 1.
func seedSteps(plan *beadsapi.BeadDetail, beads []*beadsapi.BeadDetail) []FarmSeedStep {
	if len(beads) == 0 {
		return nil
	}
	at := map[string]int{}
	for i, id := range idList(plan.Fields["step_order"]) {
		if _, dup := at[id]; !dup {
			at[id] = i
		}
	}
	bs := append([]*beadsapi.BeadDetail(nil), beads...)
	sort.Slice(bs, func(i, j int) bool {
		ai, iok := at[bs[i].ID]
		aj, jok := at[bs[j].ID]
		switch {
		case iok && jok:
			return ai < aj
		case iok != jok:
			return iok
		case !bs[i].CreatedAt.Equal(bs[j].CreatedAt):
			return bs[i].CreatedAt.Before(bs[j].CreatedAt)
		}
		return bs[i].ID < bs[j].ID
	})
	if len(bs) > maxSeedSteps {
		bs = bs[:maxSeedSteps]
	}
	out := make([]FarmSeedStep, len(bs))
	for i, b := range bs {
		out[i] = FarmSeedStep{ID: b.ID, Title: b.Title, N: i + 1, Status: b.Status, Heraldry: b.Heraldry}
	}
	return out
}

// seedPrompt is a seed decision's question without the "Plan <id>: <title> - "
// steward.WritePlan opens it with (the pot names its plan already), its first
// letter raised: "Run it?". A prompt of another shape is kept whole.
func seedPrompt(prompt, plan, title string) string {
	prompt = strings.TrimSpace(prompt)
	head := "Plan " + plan + ": " + strings.TrimSpace(title) + " - "
	q, ok := strings.CutPrefix(prompt, head)
	if !ok || q == "" {
		return prompt
	}
	return strings.ToUpper(q[:1]) + q[1:]
}

// queueOptions is a decision's raw options JSON as answer buttons, as the
// Mailbox row reads it (Item.OptionList): an option with no id is left out.
func queueOptions(raw string) []QueueOption {
	var out []QueueOption
	for _, o := range (Item{Options: raw}).OptionList() {
		out = append(out, QueueOption(o))
	}
	return out
}
