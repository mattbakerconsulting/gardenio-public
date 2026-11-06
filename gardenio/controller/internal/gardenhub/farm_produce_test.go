package gardenhub

import (
	"context"
	"fmt"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// produceWorld is a plan whose steps made things: a merged MR on a closed step,
// an open MR on a step in review, a closed-unmerged MR (compost), a report
// under a closed step under a closed sub-epic (two closed hops), the plan's
// narrative bundle directly under it, a second step sharing the merged MR's
// URL, and a loose step with an MR that reaches no plan.
func produceWorld() *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()
	f.add(planBead("gd-plan", "building", "in_progress", "gd-ph1", "gd-ph2"))
	f.add(fb("gd-ph1", "epic", "in_progress", "gd-plan", 10))
	f.add(fb("gd-ph2", "epic", "closed", "gd-plan", 10)) // a closed phase: not in the rows

	done := fb("gd-done", "task", "closed", "gd-ph1", 40)
	done.Fields["mr_url"], done.Fields["mr_iid"], done.Fields["mr_state"], done.Fields["mr_title"] = "https://example.com/pr/1", "1", "merged", "feat: the thing"
	f.add(done)
	review := fb("gd-review", "task", "in_progress", "gd-ph1", 50)
	review.Fields["mr_url"], review.Fields["mr_iid"], review.Fields["mr_state"] = "https://example.com/pr/2", "2", "opened"
	f.add(review)
	compost := fb("gd-compost", "task", "closed", "gd-ph1", 30)
	compost.Fields["mr_url"], compost.Fields["mr_state"] = "https://example.com/pr/3", "closed"
	f.add(compost)
	twin := fb("gd-twin", "task", "closed", "gd-ph1", 20)
	twin.Fields["mr_url"], twin.Fields["mr_state"] = "https://example.com/pr/1", "merged"
	f.add(twin)

	// Phase 2 is closed; under it a closed sub-epic, a closed step, its report.
	f.add(fb("gd-sub", "epic", "closed", "gd-ph2", 12))
	f.add(fb("gd-step", "task", "closed", "gd-sub", 14))
	f.add(fb("gd-report", "report", "closed", "gd-step", 16))

	f.add(fb("gd-bundle", "bundle", "open", "gd-plan", 60))

	loose := fb("gd-loose", "task", "closed", "", 70)
	loose.Fields["mr_url"], loose.Fields["mr_state"] = "https://example.com/pr/9", "merged"
	f.add(loose)
	return f
}

func findProduce(ps []FarmProduce, id string) *FarmProduce {
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i]
		}
	}
	return nil
}

func TestFarmProduceBin(t *testing.T) {
	src := produceWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-plan")
	if p == nil {
		t.Fatal("plan missing")
	}
	// The closed report hangs two closed hops below its phase: one lookup
	// round per hop, and the first tick lists it.
	if got := len(p.Produce); got != 4 {
		t.Fatalf("produce = %d items, want 4 (merged MR, open MR, report, bundle): %+v", got, p.Produce)
	}
	if p.ProduceMore != 0 {
		t.Fatalf("produce_more = %d, want 0", p.ProduceMore)
	}
	// By kind (gd-wgeINbOQMC): the report (16) leads whatever its age, then
	// the bundle (60), then the MRs.
	for i, want := range []string{"gd-report", "gd-bundle"} {
		if p.Produce[i].ID != want || !p.Produce[i].Ready {
			t.Fatalf("document order wrong at %d: %+v", i, p.Produce)
		}
	}
	// Among the MRs the expected one leads whatever its age, then the merged
	// ones newest first.
	if x := p.Produce[2]; x.ID != "gd-review" || x.Ready || x.Kind != ProduceMR || x.State != "opened" || x.Task != "gd-review" || x.Phase != "gd-ph1" {
		t.Fatalf("the expected MR should lead the MRs: %+v", x)
	}
	if x := p.Produce[3]; x.ID != "gd-done" || !x.Ready {
		t.Fatalf("the merged MR should follow: %+v", x)
	}
	mr := findProduce(p.Produce, "gd-done")
	if mr.Title != "feat: the thing" || mr.URL != "https://example.com/pr/1" || mr.IID != "1" || mr.State != "merged" || mr.Task != "gd-done" || mr.Phase != "gd-ph1" {
		t.Fatalf("merged MR wrong: %+v", mr)
	}
	if findProduce(p.Produce, "gd-twin") != nil {
		t.Fatalf("a second step on the same MR URL must not list it twice: %+v", p.Produce)
	}
	if findProduce(p.Produce, "gd-compost") != nil {
		t.Fatalf("an MR closed without merging is not produce: %+v", p.Produce)
	}
	rep := findProduce(p.Produce, "gd-report")
	if rep.Kind != ProduceReport || rep.Task != "gd-step" || rep.Phase != "gd-ph2" || rep.State != "closed" {
		t.Fatalf("report should hang off its closed step in the closed phase: %+v", rep)
	}
	bun := findProduce(p.Produce, "gd-bundle")
	if bun.Kind != ProduceBundle || bun.Task != "" || bun.Phase != "" || !bun.Ready {
		t.Fatalf("the plan's bundle hangs off the plan itself: %+v", bun)
	}
	// The loose step's MR reaches no plan.
	for _, pl := range decodeSnap(t, f).Plans {
		if findProduce(pl.Produce, "gd-loose") != nil {
			t.Fatalf("meadow produce listed under a plan: %+v", pl.Produce)
		}
	}
	// The lineage lookups are cached: a second slow tick reads no edges for them.
	before := src.depCalls
	f.slow.at = f.now().Add(-2 * farmSlowEvery)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Two deps reads per slow tick: the blocks-edge read, and the produces edge
	// of the one MR not yet merged (gd-review, re-read until it is terminal).
	// The produce lineage adds none.
	if src.depCalls-before > 2 {
		t.Fatalf("lineage not cached: %d dependency reads on the second tick", src.depCalls-before)
	}
	if got := len(findPlan(decodeSnap(t, f), "gd-plan").Produce); got != 4 {
		t.Fatalf("second tick lost produce: %d", got)
	}
}

func TestFarmProduceMerges(t *testing.T) {
	src := produceWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The open MR merges and its step closes: it moves from expected to ready
	// and stays in the bin although the step left the rows.
	src.mutate("gd-review", func(b *beadsapi.BeadDetail) { b.Status = "closed"; b.Fields["mr_state"] = "merged" })
	f.slow.at = f.now().Add(-2 * farmSlowEvery)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-plan")
	got := findProduce(p.Produce, "gd-review")
	if got == nil || !got.Ready || got.State != "merged" {
		t.Fatalf("merged MR on a now-closed step should be ready produce: %+v", p.Produce)
	}
	for _, x := range p.Produce {
		if !x.Ready {
			t.Fatalf("nothing is expected any more: %+v", p.Produce)
		}
	}
}

func TestFarmProduceCap(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-plan", "building", "in_progress", "gd-ph1"))
	src.add(fb("gd-ph1", "epic", "in_progress", "gd-plan", 10))
	for i := 0; i < maxProduce+3; i++ {
		b := fb(fmt.Sprintf("gd-s%02d", i), "task", "closed", "gd-ph1", int64(100+i))
		b.Fields["mr_url"], b.Fields["mr_state"] = fmt.Sprintf("https://example.com/pr/%d", i), "merged"
		src.add(b)
	}
	// One expected MR, older than all the merged ones: it still leads.
	exp := fb("gd-exp", "task", "in_progress", "gd-ph1", 1)
	exp.Fields["mr_url"], exp.Fields["mr_state"] = "https://example.com/pr/exp", "draft"
	src.add(exp)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-plan")
	if len(p.Produce) != maxProduce || p.ProduceMore != 4 {
		t.Fatalf("cap: listed %d more %d, want %d and 4", len(p.Produce), p.ProduceMore, maxProduce)
	}
	if p.Produce[0].ID != "gd-exp" || p.Produce[0].Ready || p.Produce[0].State != "draft" {
		t.Fatalf("the expected item must survive the cap at the top of the MRs: %+v", p.Produce[0])
	}
	if p.Produce[1].ID != fmt.Sprintf("gd-s%02d", maxProduce+2) {
		t.Fatalf("ready items are newest first: %+v", p.Produce[1])
	}
}

func TestFarmProduceReadFailureKeepsLastBins(t *testing.T) {
	src := produceWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.failProduce = true
	f.slow.at = f.now().Add(-2 * farmSlowEvery)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatalf("a produce read failure must not fail the refresh: %v", err)
	}
	if got := len(findPlan(decodeSnap(t, f), "gd-plan").Produce); got != 4 {
		t.Fatalf("last bins not kept: %d", got)
	}
}

// githubWorld is a plan whose steps link GitHub PRs the way github-bridge does
// (bug gd-9K2MsfmsNr): the step carries only mr_url and forge, and the state
// lives on the type=merge-request bead its produces edge points at.
func githubWorld() *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()
	f.add(planBead("gd-plan", "building", "in_progress", "gd-ph1"))
	f.add(fb("gd-ph1", "epic", "in_progress", "gd-plan", 10))
	step := func(id, status, url string, upd int64, mrBeads ...string) {
		b := fb(id, "task", status, "gd-ph1", upd)
		b.Fields["mr_url"], b.Fields["forge"], b.Fields["thread_id"] = url, "github", "gd-wish"
		f.add(b)
		for _, m := range mrBeads {
			f.deps[id] = append(f.deps[id], beadsapi.Dependency{BeadID: id, DependsOnID: m, Type: "produces"})
		}
	}
	mrBead := func(id, url, state, iid string, upd int64) {
		m := fb(id, "merge-request", "closed", "", upd)
		m.Fields["mr_url"], m.Fields["mr_state"], m.Fields["mr_iid"], m.Fields["mr_title"] = url, state, iid, "PR title "+iid
		if state == "opened" {
			m.Status = "open"
		} else {
			m.Fields["mr_outcome"] = state
		}
		f.add(m)
	}
	// Merged, and a stale duplicate MR bead that still says opened and was
	// touched after the one recording the merge: merged wins.
	step("gd-merged", "closed", "https://github.com/o/r/pull/1245", 40, "gd-mr1", "gd-mr1dup")
	mrBead("gd-mr1", "https://github.com/o/r/pull/1245", "merged", "1245", 41)
	mrBead("gd-mr1dup", "https://github.com/o/r/pull/1245", "opened", "1245", 45)
	// Still open.
	step("gd-open", "in_progress", "https://github.com/o/r/pull/1249", 50, "gd-mr2")
	mrBead("gd-mr2", "https://github.com/o/r/pull/1249", "opened", "1249", 51)
	// Closed without merging: compost.
	step("gd-dropped", "closed", "https://github.com/o/r/pull/1250", 30, "gd-mr3")
	mrBead("gd-mr3", "https://github.com/o/r/pull/1250", "closed", "1250", 31)
	// The step produced an MR bead for another URL than the one it carries:
	// not this step's state.
	step("gd-relinked", "closed", "https://github.com/o/r/pull/1251", 20, "gd-mr4")
	mrBead("gd-mr4", "https://github.com/o/r/pull/999", "merged", "999", 21)
	return f
}

func TestFarmProduceReadsGitHubStateFromMRBead(t *testing.T) {
	src := githubWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-plan")
	if p == nil {
		t.Fatal("plan missing")
	}
	got := findProduce(p.Produce, "gd-merged")
	if got == nil || !got.Ready || got.State != "merged" || got.IID != "1245" || got.Title != "PR title 1245" {
		t.Fatalf("a merged GitHub PR must be ready produce, state off its MR bead: %+v", got)
	}
	if got := findProduce(p.Produce, "gd-open"); got == nil || got.Ready || got.State != "opened" {
		t.Fatalf("an open GitHub PR is expected produce: %+v", got)
	}
	if got := findProduce(p.Produce, "gd-dropped"); got != nil {
		t.Fatalf("a PR closed without merging is not produce: %+v", got)
	}
	if got := findProduce(p.Produce, "gd-relinked"); got == nil || got.Ready || got.IID != "" {
		t.Fatalf("an MR bead for another URL must not speak for the step: %+v", got)
	}
	// The wish's headstone counts the merged PR too.
	if w := f.slow.wishProduce.byWish["gd-wish"]; len(w) != 1 || w[0].ID != "gd-merged" {
		t.Fatalf("wish produce = %+v, want the merged PR", w)
	}
	// The closed step's harvest names the merged PR.
	if r := findStep(closedRows(p), "gd-merged"); r == nil || r.Harvest == nil || r.Harvest.MR != "#1245" {
		t.Fatalf("merged step's harvest should carry #1245: %+v", r)
	}

	// Terminal answers are cached: the next tick reads the produces edges and
	// MR beads of the open PR and the unresolved step only.
	gets := src.getCalls
	f.slow.at = f.now().Add(-2 * farmSlowEvery)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.slow.mrDone["https://github.com/o/r/pull/1245"]; !ok {
		t.Fatal("the merged PR's state is not cached")
	}
	if src.getCalls-gets != 1 {
		t.Fatalf("second tick: %d BatchGetBeads calls, want 1 (the open PR's MR bead)", src.getCalls-gets)
	}

	// The open PR merges: its MR bead says so and the step moves to ready.
	src.mutate("gd-mr2", func(b *beadsapi.BeadDetail) {
		b.Status, b.Fields["mr_state"], b.Fields["mr_outcome"] = "closed", "merged", "merged"
	})
	f.slow.at = f.now().Add(-2 * farmSlowEvery)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p = findPlan(decodeSnap(t, f), "gd-plan")
	if got := findProduce(p.Produce, "gd-open"); got == nil || !got.Ready || got.State != "merged" {
		t.Fatalf("the PR merged; its step should be ready produce: %+v", got)
	}
	// The step rows themselves are never written.
	for _, b := range src.beads {
		if b.ID == "gd-merged" && b.Fields["mr_state"] != "" {
			t.Fatalf("the source row was mutated: %+v", b.Fields)
		}
	}
}

func TestCanonicalMRBead(t *testing.T) {
	mk := func(id, state string, upd int64) *beadsapi.BeadDetail {
		b := fb(id, "merge-request", "closed", "", upd)
		b.Fields["mr_state"] = state
		return b
	}
	for _, c := range []struct {
		a, b *beadsapi.BeadDetail
		want string
	}{
		{mk("gd-a", "merged", 1), mk("gd-b", "opened", 9), "gd-a"},
		{mk("gd-a", "opened", 9), mk("gd-b", "closed", 1), "gd-b"},
		{mk("gd-a", "closed", 9), mk("gd-b", "merged", 1), "gd-b"},
		{mk("gd-a", "opened", 1), mk("gd-b", "opened", 9), "gd-b"},
		{mk("gd-a", "opened", 5), mk("gd-b", "opened", 5), "gd-b"},
		{nil, mk("gd-b", "opened", 5), "gd-b"},
	} {
		if got := canonicalMRBead(c.a, c.b); got.ID != c.want {
			t.Errorf("canonicalMRBead(%v, %v) = %s, want %s", c.a, c.b, got.ID, c.want)
		}
		if c.a != nil {
			if got := canonicalMRBead(c.b, c.a); got.ID != c.want && c.a.UpdatedAt != c.b.UpdatedAt {
				t.Errorf("canonicalMRBead is order-dependent for %s/%s", c.a.ID, c.b.ID)
			}
		}
	}
}

func TestProduceRelevance(t *testing.T) {
	for _, typ := range []string{"bundle", "report"} {
		e := ev("bead.updated", `{"bead":{"type":"`+typ+`","status":"open"},"changes":{}}`)
		if r := classify(e); !r.Farm {
			t.Fatalf("a %s change must refresh the farm (the bins): %+v", typ, r)
		}
	}
}
