package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func findStep(ts []FarmClosedStep, id string) *FarmClosedStep {
	for i := range ts {
		if ts[i].ID == id {
			return &ts[i]
		}
	}
	return nil
}

// listedClosed counts the closed steps listed across the document.
func listedClosed(s snap) int {
	n := 0
	for i := range s.Plans {
		n += len(closedRows(&s.Plans[i]))
	}
	return n
}

// A plan lists at most maxClosedPerPlan closed steps, newest change first
// across its phases, and the document at most maxClosedListed, newest first
// across its plans; a seed's closed steps take none of that room
// (gd-8fjCFsoAKI).
func TestFarmClosedStepCaps(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-P", "building", "in_progress", "gd-ph1", "gd-ph2"), fb("gd-ph1", "epic", "open", "gd-P", 1), fb("gd-ph2", "epic", "open", "gd-P", 1))
	for i := range 40 {
		src.add(fb(fmt.Sprintf("gd-c%02d", i), "task", "closed", []string{"gd-ph1", "gd-ph2"}[i%2], int64(100+i)))
	}
	src.add(planBead("gd-S", "draft", "open"))
	for i := range 30 {
		src.add(fb(fmt.Sprintf("gd-s%02d", i), "task", "closed", "gd-S", int64(500+i))) // newer than every step of gd-P
	}
	f := newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	p := findPlan(s, "gd-P")
	rows := closedRows(p)
	if len(rows) != maxClosedPerPlan || len(p.Phases[0].Closed) != maxClosedPerPlan/2 || p.Phases[0].Counts.Closed != 20 {
		t.Fatalf("listed %d closed steps (%d in phase 1, counts %+v), want the newest %d", len(rows), len(p.Phases[0].Closed), p.Phases[0].Counts, maxClosedPerPlan)
	}
	if rows[0].ID != "gd-c38" || findStep(rows, fmt.Sprintf("gd-c%02d", 40-maxClosedPerPlan)) == nil || findStep(rows, fmt.Sprintf("gd-c%02d", 39-maxClosedPerPlan)) != nil {
		t.Fatalf("not the newest %d: %+v", maxClosedPerPlan, rows)
	}
	// The slim row: only what the cell shows is on the wire.
	_, raw, _ := f.Snapshot()
	var doc struct {
		Plans []struct {
			Phases []struct {
				Closed []map[string]json.RawMessage `json:"closed"`
			} `json:"phases"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range doc.Plans[0].Phases[1].Closed[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if got := strings.Join(keys, ","); got != "created,harvest,id,state,title,type,updated" {
		t.Fatalf("closed row keys = %s", got)
	}

	// The document cap: plans newest first, so the oldest plan's steps fall off.
	seq = 0
	src = newFakeFarm()
	plans := maxClosedListed/maxClosedPerPlan + 2
	for p := range plans {
		pid := fmt.Sprintf("gd-P%02d", p)
		src.add(planBead(pid, "building", "in_progress"))
		for i := range maxClosedPerPlan {
			src.add(fb(fmt.Sprintf("%s-c%02d", pid, i), "task", "closed", pid, int64(1000+p*100+i)))
		}
	}
	f = newTestFarm(src)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = decodeSnap(t, f)
	if n := listedClosed(s); n != maxClosedListed {
		t.Fatalf("listed %d closed steps across the document, want %d", n, maxClosedListed)
	}
	if n := len(findPlan(s, "gd-P00").Unphased.Closed); n != 0 {
		t.Fatalf("the oldest plan lists %d closed steps past the document cap", n)
	}
	if n := len(findPlan(s, fmt.Sprintf("gd-P%02d", plans-1)).Unphased.Closed); n != maxClosedPerPlan {
		t.Fatalf("the newest plan lists %d closed steps, want %d", n, maxClosedPerPlan)
	}
}

// remove deletes a bead from the fake daemon, as a hard delete would.
func (f *fakeFarmSrc) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beads = slices.DeleteFunc(f.beads, func(b *beadsapi.BeadDetail) bool { return b.ID == id })
}

// The slow tier keeps the closed rows and reads only the delta after the first
// list: a close is upserted, a reopen leaves the set the same tick, and a full
// re-list every farmClosedFullEvery repairs a deletion no delta can see
// (gd-2COXnx326M). Every work-row read sends the omit projection.
func TestFarmClosedRowsDelta(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-P", "building", "in_progress", "gd-ph1"), fb("gd-ph1", "epic", "open", "gd-P", 1),
		fb("gd-c1", "task", "closed", "gd-ph1", 100), fb("gd-c2", "task", "closed", "gd-ph1", 110),
		fb("gd-o1", "task", "open", "gd-ph1", 200))
	f := newTestFarm(src)
	start := f.now()
	ctx := context.Background()
	steps := func() []FarmClosedStep {
		t.Helper()
		if err := f.refresh(ctx); err != nil {
			t.Fatal(err)
		}
		return closedRows(findPlan(decodeSnap(t, f), "gd-P"))
	}
	if rows := steps(); findStep(rows, "gd-c1") == nil || findStep(rows, "gd-c2") == nil || src.closedLists != 1 || src.deltas != 0 {
		t.Fatalf("first slow tick: steps %+v, %d full lists, %d deltas; want both closed steps from one full list", rows, src.closedLists, src.deltas)
	}

	// A close (the removal forces the slow tier) comes in by the delta.
	src.mutate("gd-o1", func(b *beadsapi.BeadDetail) { b.Status, b.UpdatedAt = "closed", start })
	if rows := steps(); findStep(rows, "gd-o1") == nil || src.closedLists != 1 || src.deltas != 1 {
		t.Fatalf("after a close: steps %+v, %d full lists, %d deltas; want gd-o1 listed from one delta", rows, src.closedLists, src.deltas)
	}

	// A reopen leaves the set in the next slow tick.
	f.now = func() time.Time { return start.Add(farmSlowEvery + time.Second) }
	src.mutate("gd-c1", func(b *beadsapi.BeadDetail) { b.Status, b.UpdatedAt = "open", start.Add(time.Second) })
	if rows := steps(); findStep(rows, "gd-c1") != nil || findStep(rows, "gd-c2") == nil || src.closedLists != 1 || src.deltas != 2 {
		t.Fatalf("after a reopen: steps %+v, %d full lists, %d deltas; want gd-c1 gone, gd-c2 kept", rows, src.closedLists, src.deltas)
	}

	// A deleted closed bead touches no updated_at: it stays until the full re-list.
	src.remove("gd-c2")
	f.now = func() time.Time { return start.Add(2*farmSlowEvery + time.Second) }
	if rows := steps(); findStep(rows, "gd-c2") == nil {
		t.Fatalf("a delta dropped a deleted bead it cannot see: %+v", rows)
	}
	f.now = func() time.Time { return start.Add(farmClosedFullEvery + time.Second) }
	if rows := steps(); findStep(rows, "gd-c2") != nil || findStep(rows, "gd-o1") == nil || src.closedLists != 2 {
		t.Fatalf("full re-list: steps %+v, %d full lists; want gd-c2 gone and a second full list", rows, src.closedLists)
	}
	if src.unprojected != 0 {
		t.Fatalf("%d work-row reads went without omit=description,notes", src.unprojected)
	}
	if src.counted != 0 {
		t.Fatalf("%d work-row or agent reads asked for a total they never read (gd-XOi5cqZP3I)", src.counted)
	}
}

// A delta that fills its page may be cut short, so the slow tier re-lists.
func TestFarmClosedRowsFullDeltaRelists(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-P", "building", "in_progress", "gd-ph1"), fb("gd-ph1", "epic", "open", "gd-P", 1), fb("gd-c0", "task", "closed", "gd-ph1", 100))
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	start := f.now()
	for i := range farmPageSize {
		b := fb(fmt.Sprintf("gd-n%03d", i), "task", "closed", "gd-ph1", 0)
		b.UpdatedAt = start
		src.add(b)
	}
	f.now = func() time.Time { return start.Add(farmSlowEvery + time.Second) }
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if src.deltas != 1 || src.closedLists != 3 { // the first list's page, then a two-page re-list
		t.Fatalf("%d deltas, %d full list pages; want the full delta to force a re-list", src.deltas, src.closedLists)
	}
	if len(f.closedSt.rows) != farmPageSize+1 {
		t.Fatalf("kept %d closed rows, want %d", len(f.closedSt.rows), farmPageSize+1)
	}
}
