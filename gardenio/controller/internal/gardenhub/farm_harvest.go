package gardenhub

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// The harvest read model (harvest spec gardenio/docs/superpowers/specs/2026-10-22-lands-harvest-icons-design.md §3,
// step 4b.6.2): the newest closed steps of each plan carry a compact harvest object, the facts the cell's base sprite
// and badges are drawn from. The step's own fields (mr_*, ci_state, tests_touched, goldens_touched, review_rounds,
// close_reason) are on the closed row the slow tier already reads; the UX grade is a type=evidence bead joined by its
// step field; the yield for the sheaf is the produce bin's rows (farm_produce.go) joined by task. The base rule is the
// bead view's (bead_harvest.go), so the map and the panel agree.
const (
	// maxHarvestPerPlan bounds the closed rows per plan that carry a harvest object: the newest by closed_at across
	// the plan's phases. Older listed closed rows stay plain stubble.
	maxHarvestPerPlan = 12
	// maxEvidenceScan bounds the evidence list per slow tick (newest first, like the produce lists).
	maxEvidenceScan = 600
)

// stepEvidence is what the evidence list says about one step: whether it has any evidence bead (a yield), and its
// newest UX grade.
type stepEvidence struct {
	ux        bool
	score, of int
	urls      int
}

// harvestIdx is the slow tier's harvest state. evidence and yields are kept across a failed read; rows is rebuilt
// from the closed rows every slow tick.
type harvestIdx struct {
	evidence map[string]stepEvidence         // step id -> its evidence
	yields   map[string]bool                 // step id -> a ready produce row hangs off it
	rows     map[string]*beadsapi.BeadDetail // closed step id -> its row
}

// readYields reads what the plans yielded, in the slow tier: the produce bins and the evidence index. Neither read
// ever fails the refresh; a bad read keeps the last bins or the last index.
func (f *Farm) readYields(ctx context.Context, planSet map[string]*beadsapi.BeadDetail, phaseIdx map[string]string, byID map[string]*beadsapi.BeadDetail) {
	if produce, more, yields, byWish, err := f.readProduce(ctx, planSet, phaseIdx, byID); err != nil {
		f.logger.Warn("garden farm: reading the plans' produce failed", "error", err)
	} else {
		f.slow.produce, f.slow.produceMore, f.slow.harvest.yields, f.slow.wishProduce = produce, more, yields, byWish
	}
	if len(planSet) == 0 {
		f.slow.harvest.evidence = nil // no plan, no harvest row to join to
	} else if ev, err := f.readEvidence(ctx); err != nil {
		f.logger.Warn("garden farm: reading the evidence beads failed", "error", err)
	} else {
		f.slow.harvest.evidence = ev
	}
	rows := make(map[string]*beadsapi.BeadDetail, len(f.slow.closedRows))
	for _, b := range f.slow.closedRows {
		rows[b.ID] = f.withMRDone(b)
	}
	f.slow.harvest.rows = rows
}

// readEvidence lists the evidence beads, every status, newest first, and indexes them by the step they were filed
// on: the step field, or the parent when a writer left it out.
func (f *Farm) readEvidence(ctx context.Context) (map[string]stepEvidence, error) {
	res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"evidence"}, Statuses: produceStatuses, WithParent: true, Sort: "-created_at", Limit: maxEvidenceScan,
		Omit: farmRowOmit,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]stepEvidence{}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		step := strings.TrimSpace(b.Fields["step"])
		if step == "" {
			step = b.ParentID
		}
		if step == "" {
			continue
		}
		e := out[step]
		if !e.ux && strings.TrimSpace(b.Fields["kind"]) == "ux" { // the newest grade wins: the list is newest first
			e.ux, e.score, e.of, e.urls = true, fieldInt(b.Fields, "score"), fieldInt(b.Fields, "of"), len(gradeURLs(b.Fields["urls"]))
		}
		out[step] = e
	}
	return out, nil
}

// harvestPlan puts a harvest object on the plan's newest closed rows (maxHarvestPerPlan), across all its phases.
func (f *Farm) harvestPlan(plan *FarmPlan) {
	type cand struct {
		t      *FarmClosedStep
		b      *beadsapi.BeadDetail
		closed int64
	}
	var cs []cand
	add := func(ph *FarmPhase) {
		for i := range ph.Closed {
			t := &ph.Closed[i]
			if b := f.slow.harvest.rows[t.ID]; b != nil {
				c := closedAt(b.ClosedAt)
				if c == 0 {
					c = t.Updated
				}
				cs = append(cs, cand{t, b, c})
			}
		}
	}
	for i := range plan.Phases {
		add(&plan.Phases[i])
	}
	add(&plan.Unphased)
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].closed != cs[j].closed {
			return cs[i].closed > cs[j].closed
		}
		return cs[i].t.ID < cs[j].t.ID
	})
	if len(cs) > maxHarvestPerPlan {
		cs = cs[:maxHarvestPerPlan]
	}
	for _, c := range cs {
		c.t.Harvest = f.harvestOf(c.b, c.closed)
	}
}

// harvestOf is one closed step's harvest object (harvest spec §3.1). Zero and empty facts are left out.
func (f *Farm) harvestOf(b *beadsapi.BeadDetail, closed int64) *FarmHarvest {
	h := &FarmHarvest{Closed: closed, CI: strings.TrimSpace(b.Fields["ci_state"])}
	mrState := ""
	if mr := stepMR(b); mr != nil {
		mrState = mr.State
		if mr.State == "merged" {
			h.MR = harvestMRNumber(b)
		}
	}
	ev, hasEv := f.slow.harvest.evidence[b.ID]
	if ev.ux {
		h.UX = []int{ev.score, ev.of}
		h.Pics = ev.urls
	}
	if g := fieldCount(b.Fields, "goldens_touched"); g != nil {
		h.Pics += *g
	}
	if t := fieldCount(b.Fields, "tests_touched"); t != nil {
		h.Tests = *t
	}
	if r := fieldCount(b.Fields, "review_rounds"); r != nil {
		h.Rounds = *r
	}
	if base := harvestBase(b.Fields["close_reason"], mrState, hasEv || f.slow.harvest.yields[b.ID]); base != HarvestBare {
		h.Base = base
	}
	return h
}

// harvestMRNumber is the merged MR's number as the forge writes it (#825, !825). An mr_iid that already carries its
// sigil is kept as written.
func harvestMRNumber(b *beadsapi.BeadDetail) string {
	iid := strings.TrimSpace(b.Fields["mr_iid"])
	if len(iid) > 1 && (iid[0] == '!' || iid[0] == '#') {
		if _, err := strconv.Atoi(iid[1:]); err == nil {
			return iid
		}
	}
	return mrNumber(strings.TrimSpace(b.Fields["mr_url"]), iid)
}
