package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// closedStep is a closed task under parent, closed at baseUnix+at (updated then too).
func closedStep(id, parent string, at int64) *beadsapi.BeadDetail {
	b := fb(id, "task", "closed", parent, at)
	b.ClosedAt = time.Unix(baseUnix+at, 0).UTC().Format(time.RFC3339)
	return b
}

// evidence is a UX grade filed on step, created at baseUnix+at.
func evidence(id, step string, at int64, score, of int, urls string) *beadsapi.BeadDetail {
	b := fb(id, "evidence", "closed", step, at)
	b.CreatedAt = time.Unix(baseUnix+at, 0)
	b.Fields["kind"], b.Fields["step"] = "ux", step
	b.Fields["score"], b.Fields["of"], b.Fields["urls"] = fmt.Sprint(score), fmt.Sprint(of), urls
	return b
}

// harvestWorld is a plan with 14 closed steps over two phases and the unphased row, one of every harvest shape among
// the newest, and an open task so the phases have a bed.
func harvestWorld() *fakeFarmSrc {
	seq = 0
	f := newFakeFarm()
	f.add(planBead("gd-plan", "building", "in_progress", "gd-ph1", "gd-ph2"))
	f.add(fb("gd-ph1", "epic", "in_progress", "gd-plan", 10), fb("gd-ph2", "epic", "open", "gd-plan", 10))
	f.add(fb("gd-open", "task", "open", "gd-ph1", 900))

	// The full harvest: merged GitHub PR, a re-graded UX pass, goldens, tests, green CI, two rounds.
	full := closedStep("gd-full", "gd-ph1", 500)
	full.Fields["mr_url"], full.Fields["mr_iid"], full.Fields["mr_state"] = "https://github.com/o/r/pull/825", "825", "merged"
	full.Fields["ci_state"], full.Fields["tests_touched"], full.Fields["goldens_touched"], full.Fields["review_rounds"] = "success", "3", "2", "2"
	// A step whose report is its only yield.
	report := closedStep("gd-report", "gd-ph2", 490)
	// A step with only an evidence bead.
	graded := closedStep("gd-graded", "gd-ph2", 480)
	// Abandoned by close_reason, even with a merged MR on GitLab.
	wont := closedStep("gd-wont", "gd-ph1", 470)
	wont.Fields["close_reason"] = "Superseded by gd-full"
	wont.Fields["mr_url"], wont.Fields["mr_state"] = "https://gitlab.example.com/g/p/-/merge_requests/77", "merged"
	// An MR closed unmerged and nothing else: trampled.
	compost := closedStep("gd-compost", "gd-plan", 460)
	compost.Fields["mr_url"], compost.Fields["mr_state"] = "https://github.com/o/r/pull/9", "closed"
	// Failed CI and zero tests recorded: no lantern, no vial, still a fact for the status line.
	red := closedStep("gd-red", "gd-ph1", 450)
	red.Fields["ci_state"], red.Fields["tests_touched"] = "failed", "0"
	f.add(full, report, graded, wont, compost, red)
	// Eight bare steps, older: gd-old0 and gd-old1 are past the 12 and carry no harvest.
	for i := range 8 {
		f.add(closedStep(fmt.Sprintf("gd-old%d", i), []string{"gd-ph1", "gd-ph2"}[i%2], int64(100+i*10)))
	}

	f.add(fb("gd-rep", "report", "closed", "gd-report", 495))
	f.add(evidence("gd-ev1", "gd-full", 600, 3, 5, "https://img.example/a.png"),
		evidence("gd-ev2", "gd-full", 700, 5, 5, "https://img.example/b.png\nhttps://img.example/c.png\nnot a url"),
		evidence("gd-ev3", "gd-graded", 650, 4, 6, ""),
		evidence("gd-ev4", "gd-old0", 660, 5, 5, "https://img.example/d.png")) // not among the 12: dropped
	return f
}

func closedRows(p *FarmPlan) []FarmClosedStep {
	var out []FarmClosedStep
	for _, ph := range p.Phases {
		out = append(out, ph.Closed...)
	}
	return append(out, p.Unphased.Closed...)
}

func TestFarmHarvestNewestTwelvePerPlan(t *testing.T) {
	f := newTestFarm(harvestWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := findPlan(decodeSnap(t, f), "gd-plan")
	rows := closedRows(p)
	if len(rows) != 14 {
		t.Fatalf("listed closed rows = %d, want all 14", len(rows))
	}
	n := 0
	for _, r := range rows {
		if r.Harvest == nil {
			if r.ID != "gd-old0" && r.ID != "gd-old1" {
				t.Errorf("%s is among the 12 newest but has no harvest", r.ID)
			}
			continue
		}
		n++
		if r.ID == "gd-old0" || r.ID == "gd-old1" {
			t.Errorf("%s is past the 12 newest but has a harvest: %+v", r.ID, r.Harvest)
		}
	}
	if n != maxHarvestPerPlan {
		t.Fatalf("harvest objects = %d, want %d", n, maxHarvestPerPlan)
	}
}

func TestFarmHarvestFacts(t *testing.T) {
	f := newTestFarm(harvestWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := closedRows(findPlan(decodeSnap(t, f), "gd-plan"))
	h := func(id string) string {
		r := findStep(rows, id)
		if r == nil {
			t.Fatalf("%s not listed", id)
		}
		raw, _ := json.Marshal(r.Harvest)
		return string(raw)
	}
	for id, want := range map[string]string{
		// The newest grade (5/5, two links) wins over the older 3/5; pics adds the two goldens.
		"gd-full":    fmt.Sprintf(`{"closed":%d,"base":"sheaf","mr":"#825","ux":[5,5],"pics":4,"tests":3,"ci":"success","rounds":2}`, baseUnix+500),
		"gd-report":  fmt.Sprintf(`{"closed":%d,"base":"sheaf"}`, baseUnix+490),
		"gd-graded":  fmt.Sprintf(`{"closed":%d,"base":"sheaf","ux":[4,6]}`, baseUnix+480),
		"gd-wont":    fmt.Sprintf(`{"closed":%d,"base":"trampled","mr":"!77"}`, baseUnix+470),
		"gd-compost": fmt.Sprintf(`{"closed":%d,"base":"trampled"}`, baseUnix+460),
		"gd-red":     fmt.Sprintf(`{"closed":%d,"ci":"failed"}`, baseUnix+450),
		"gd-old7":    fmt.Sprintf(`{"closed":%d}`, baseUnix+170),
	} {
		if got := h(id); got != want {
			t.Errorf("%s harvest = %s, want %s", id, got, want)
		}
	}
}

func TestFarmHarvestClosedAtOrdersNotUpdated(t *testing.T) {
	src := harvestWorld()
	// gd-old0 was touched after the rest closed (a late field write): it is still the oldest close.
	src.mutate("gd-old0", func(b *beadsapi.BeadDetail) { b.UpdatedAt = time.Unix(baseUnix+990, 0) })
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows := closedRows(findPlan(decodeSnap(t, f), "gd-plan"))
	if r := findStep(rows, "gd-old0"); r == nil || r.Harvest != nil {
		t.Fatalf("gd-old0 closed oldest and must stay plain stubble: %+v", r)
	}
}

func TestFarmHarvestFailedEvidenceReadKeepsIndex(t *testing.T) {
	src := harvestWorld()
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	src.failEvidence = true
	f.slow.at = time.Time{} // force the slow tier
	src.add(fb("gd-new", "task", "open", "gd-ph1", 950))
	if err := f.refresh(context.Background()); err != nil {
		t.Fatalf("a failed evidence read must not fail the refresh: %v", err)
	}
	if r := findStep(closedRows(findPlan(decodeSnap(t, f), "gd-plan")), "gd-graded"); r == nil || r.Harvest == nil || len(r.Harvest.UX) != 2 {
		t.Fatalf("the last evidence index should be kept: %+v", r)
	}
}

// addHarvests closes perPlan steps in every plan of a bigWorld, each with a full harvest (the worst case for bytes),
// and grades each one.
func addHarvests(src *fakeFarmSrc, plans, phases, perPlan int) {
	for p := range plans {
		for i := range perPlan {
			step := fmt.Sprintf("gd-P%03d-ph%d-c%d", p, i%phases, i)
			b := closedStep(step, fmt.Sprintf("gd-P%03d-ph%d", p, i%phases), int64(2000+i))
			b.Title = "A realistically long task title that describes the work in about eighty chars"
			b.Assignee = "gardenio-some-crew-agent-abc"
			b.Fields["mr_url"], b.Fields["mr_iid"], b.Fields["mr_state"] = "https://github.com/o/r/pull/12345", "12345", "merged"
			b.Fields["ci_state"], b.Fields["tests_touched"], b.Fields["goldens_touched"], b.Fields["review_rounds"] = "success", "12", "8", "3"
			src.add(b, evidence(step+"-ev", step, int64(3000+i), 12, 12, "https://img.example/a.png\nhttps://img.example/b.png"))
		}
	}
}

// harvestBytes is what the harvest objects add to a snapshot: each one's `,"harvest":{...}` (the key is omitted
// when nil), the count of objects and the largest object.
func harvestBytes(s snap) (added, n, largest int) {
	for i := range s.Plans {
		for _, r := range closedRows(&s.Plans[i]) {
			if r.Harvest == nil {
				continue
			}
			raw, _ := json.Marshal(r.Harvest)
			added += len(`,"harvest":`) + len(raw)
			n++
			largest = max(largest, len(raw))
		}
	}
	return added, n, largest
}

// The harvest spec's §3.3 bounds: an object is at most 120 bytes, and the 36-plan farm gains about 432 x 120 B. The
// closed rows that carry them (FarmPhase.Closed) cost more than the objects; they are bounded by maxClosedPerPlan and
// maxClosedListed, and the snapshot budgets that include them are TestFarmRealisticScaleStaysSmall and
// TestFarmWorstCaseIsBounded (gd-8fjCFsoAKI).
func TestFarmHarvestRealisticScaleStaysSmall(t *testing.T) {
	src := bigWorld(36, 5, 4, 25, 300, 60)
	addHarvests(src, 36, 5, 14)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	added, n, largest := harvestBytes(s)
	_, raw, _ := f.Snapshot()
	t.Logf("typical farm: %d harvest objects add %d bytes (largest %d); snapshot %d bytes", n, added, largest, len(raw))
	if n != 36*maxHarvestPerPlan {
		t.Fatalf("harvest objects = %d, want %d", n, 36*maxHarvestPerPlan)
	}
	if largest > 120 {
		t.Fatalf("a harvest object is %d bytes, budget 120", largest)
	}
	if added > 60_000 {
		t.Fatalf("harvest adds %d bytes to the typical farm, budget 60000", added)
	}
}

// At 10x (360 plans) the row cap keeps about 83 plans in the document, and the newest 2000 closed rows are shared
// among them, so the harvest adds at most about 83 x 12 objects, not 360 x 12.
func TestFarmHarvestTenXIsBounded(t *testing.T) {
	src := bigWorld(360, 5, 4, 25, 300, 60)
	addHarvests(src, 360, 5, 14)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	added, n, largest := harvestBytes(s)
	_, raw, _ := f.Snapshot()
	t.Logf("10x farm: %d plans in the document, %d harvest objects add %d bytes (largest %d); snapshot %d bytes", len(s.Plans), n, added, largest, len(raw))
	if n > maxHarvestPerPlan*len(s.Plans) || largest > 120 {
		t.Fatalf("%d harvest objects for %d plans, largest %d bytes", n, len(s.Plans), largest)
	}
	if added > 125_000 {
		t.Fatalf("harvest adds %d bytes at 10x, budget 125000", added)
	}
	// About 520 KB of open work plus at most maxClosedListed closed steps (gd-8fjCFsoAKI), plus about 22 bytes per
	// listed task and closed step for its created (gd-wDdun2AVwq): 722 KB when that landed.
	if len(raw) > 750_000 {
		t.Fatalf("10x snapshot is %d bytes, budget 750000", len(raw))
	}
}

func TestFarmHarvestWorstCaseIsBounded(t *testing.T) {
	// The worst-case farm (TestFarmWorstCaseIsBounded) with 14 closed steps per plan, 12 of them harvested.
	src := bigWorld(40, 12, 30, 25, 1000, 200)
	addHarvests(src, 40, 12, 14)
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	added, n, _ := harvestBytes(decodeSnap(t, f))
	_, raw, _ := f.Snapshot()
	t.Logf("worst-case farm: %d harvest objects add %d bytes; snapshot %d bytes", n, added, len(raw))
	if len(raw) > 700_000 {
		t.Fatalf("worst-case snapshot is %d bytes, budget 700000", len(raw))
	}
}
