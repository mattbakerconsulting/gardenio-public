package gardenhub

import (
	"cmp"
	"context"
	"slices"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// maxClosedPerPlan and maxClosedListed bound the listed closed steps (each
	// about 210 B, 330 with its harvest) per plan and across the document,
	// newest change first, so a plan patch and the snapshot stay inside their
	// budgets however much history the beds have room for (gd-8fjCFsoAKI).
	// maxClosedPerPlan is twice the harvested steps (maxHarvestPerPlan);
	// maxClosedListed is enough for 40 plans' harvested steps.
	maxClosedPerPlan = 24
	maxClosedListed  = 480

	// farmClosedFullEvery bounds how stale the kept closed rows get if a delta
	// misses an update (clock skew past farmClosedCursorSlack, a long commit) or
	// a bead is deleted: past it the slow tier re-lists them all.
	farmClosedFullEvery = 10 * time.Minute
	// farmClosedCursorSlack steps each delta's updated_after back from the time
	// the previous read started: viewer-to-Postgres clock skew, updated_at
	// stamped before commit, and the parameter's whole seconds (strict >). A
	// re-read row is harmless; the delta upserts by id.
	farmClosedCursorSlack = 10 * time.Second
)

// closedState is the closed-row set the slow tier keeps between reads
// (gd-2COXnx326M). Closed rows hardly change, but listing the newest
// farmClosedRowCap of them on every slow tick was about 5.6 MB a tick, and a
// close forces a tick. So the farm keeps the set and reads only the delta: the
// work beads updated since the last read, in any status. A closed one is
// upserted and anything else is dropped, so a reopened bead leaves the set the
// same tick. A full re-list every farmClosedFullEvery, or when a delta fills
// its page, repairs whatever a delta missed. Only refresh touches it.
type closedState struct {
	rows     map[string]*beadsapi.BeadDetail
	cursor   time.Time // updated_after for the next delta
	listedAt time.Time // last full list
}

// readClosedRows is the newest closed work beads, newest change first, at most
// farmClosedRowCap: the slow tier's source for a phase's listed closed steps.
// truncated says the cap was reached (older closed steps are count-only).
func (f *Farm) readClosedRows(ctx context.Context) (rows []*beadsapi.BeadDetail, truncated bool, err error) {
	start := f.now()
	st := &f.closedSt
	if st.rows == nil || start.Sub(st.listedAt) >= farmClosedFullEvery {
		return f.listClosedRows(ctx, start)
	}
	res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:            farmWorkTypes,
		ExcludeScheduled: true,
		WithParent:       true,
		UpdatedAfter:     st.cursor,
		Sort:             "-updated_at",
		Limit:            farmPageSize,
		Omit:             farmRowOmit,
		OmitFields:       farmClosedOmitFields,
		NoTotal:          true, // a full page re-lists; the count is never read (gd-XOi5cqZP3I)
	})
	if err != nil {
		return nil, false, err
	}
	if len(res.Beads) >= farmPageSize {
		// The delta filled its page, so it may have been cut short: re-list.
		return f.listClosedRows(ctx, start)
	}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		if b.Status == "closed" {
			st.rows[b.ID] = b
		} else {
			delete(st.rows, b.ID)
		}
	}
	st.cursor = start.Add(-farmClosedCursorSlack)
	rows, truncated = st.view()
	return rows, truncated, nil
}

// listClosedRows re-lists the closed work beads, newest change first, up to
// farmClosedRowCap, and keeps them as the set the next deltas update. A failed
// read leaves the kept set as it was.
func (f *Farm) listClosedRows(ctx context.Context, start time.Time) ([]*beadsapi.BeadDetail, bool, error) {
	set := map[string]*beadsapi.BeadDetail{}
	for off := 0; off < farmClosedRowCap; off += farmPageSize {
		res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:            farmWorkTypes,
			Statuses:         []string{"closed"},
			ExcludeScheduled: true,
			WithParent:       true,
			Sort:             "-updated_at",
			Limit:            farmPageSize,
			Offset:           off,
			Omit:             farmRowOmit,
			OmitFields:       farmClosedOmitFields,
			NoTotal:          true, // a short page ends the walk (gd-XOi5cqZP3I)
		})
		if err != nil {
			return nil, false, err
		}
		for _, b := range res.Beads {
			if b != nil && b.ID != "" {
				set[b.ID] = b
			}
		}
		if len(res.Beads) < farmPageSize {
			break
		}
	}
	f.closedSt = closedState{rows: set, cursor: start.Add(-farmClosedCursorSlack), listedAt: start}
	rows, truncated := f.closedSt.view()
	return rows, truncated, nil
}

// view is the kept set as the full list returns it: newest change first (id
// breaks ties), at most farmClosedRowCap. Rows past the cap are dropped from
// the set too: they are older than every kept row, so only an update (which a
// delta brings back) could list them again. truncated says the cap was
// reached.
func (s *closedState) view() ([]*beadsapi.BeadDetail, bool) {
	out := make([]*beadsapi.BeadDetail, 0, len(s.rows))
	for _, b := range s.rows {
		out = append(out, b)
	}
	slices.SortFunc(out, func(a, b *beadsapi.BeadDetail) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(out) < farmClosedRowCap {
		return out, false
	}
	for _, b := range out[farmClosedRowCap:] {
		delete(s.rows, b.ID)
	}
	return out[:farmClosedRowCap], true
}

// placeClosed places the slow tier's closed rows into their plans' phases for
// refreshOnce, keyed plan + "\x00" + phase, from the same walk (planSet,
// phaseIdx, resolve) and the phases' open-task buckets.
func (f *Farm) placeClosed(planSet map[string]*beadsapi.BeadDetail, phaseIdx map[string]string, buckets map[string]map[string]*bucket, resolve func(*beadsapi.BeadDetail) (string, string)) map[string][]FarmClosedStep {
	// A phase's closed steps: the slow tier's newest closed rows placed by the
	// same walk, newest change first, at most the cells the bed has left after
	// its listed tasks and the "+N" crate (roomOf), at most maxClosedPerPlan per
	// plan and maxClosedListed in the document (gd-8fjCFsoAKI). A seed's are
	// never drawn, so they take no room.
	roomOf := func(plan, phase string) int {
		n := 0
		if b := buckets[plan][phase]; b != nil {
			n = len(b.tasks)
		}
		if n > maxPhaseTasks {
			return bedCells - maxPhaseTasks - 1
		}
		return bedCells - n
	}
	closedBy := map[string][]FarmClosedStep{}
	perPlan := map[string]int{}
	listed := 0
	for _, b := range f.slow.closedRows {
		if listed >= maxClosedListed {
			break
		}
		if _, isPlan := planSet[b.ID]; isPlan {
			continue
		}
		if _, isPhase := phaseIdx[b.ID]; isPhase {
			continue
		}
		pl, ph := resolve(b)
		if pl == "" || perPlan[pl] >= maxClosedPerPlan {
			continue
		}
		if st := strings.TrimSpace(planSet[pl].Fields["plan_state"]); st == "draft" || st == "ready" {
			continue
		}
		key := pl + "\x00" + ph
		if len(closedBy[key]) >= roomOf(pl, ph) {
			continue
		}
		closedBy[key] = append(closedBy[key], FarmClosedStep{
			ID: b.ID, Title: b.Title, ShortTitle: strings.TrimSpace(b.ShortTitle), Type: b.Type, State: TaskClosed, Assignee: b.Assignee, Updated: b.UpdatedAt.Unix(), Created: b.CreatedAt.Unix(),
		})
		perPlan[pl]++
		listed++
	}
	for _, ts := range closedBy {
		sort.SliceStable(ts, func(i, j int) bool {
			if ts[i].Updated != ts[j].Updated {
				return ts[i].Updated > ts[j].Updated
			}
			return ts[i].ID < ts[j].ID
		})
	}
	return closedBy
}

// readClosedPlans lists the newest closed plans (at most maxClosedPlansShown),
// with the daemon's total. Extra labels narrow the read (a project's, for the
// per-viewer cut's archives).
func (f *Farm) readClosedPlans(ctx context.Context, labels ...string) (FarmClosedPlans, error) {
	cp, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"epic"}, Labels: append([]string{"plan"}, labels...), Statuses: []string{"closed"},
		Sort: "-updated_at", Limit: maxClosedPlansShown, Omit: farmRowOmit,
	})
	if err != nil {
		return FarmClosedPlans{}, err
	}
	closed := FarmClosedPlans{Total: cp.Total, Shown: []FarmClosedPlan{}}
	for _, b := range cp.Beads {
		if b == nil {
			continue
		}
		closed.Shown = append(closed.Shown, FarmClosedPlan{
			ID: b.ID, Title: b.Title, State: b.Fields["plan_state"], Project: projectOf(b), Updated: b.UpdatedAt.Unix(),
			Cost: slimCostOf(b, f.now()),
		})
	}
	return closed, nil
}
