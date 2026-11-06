package gardenhub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// A page whose version the ring no longer reaches is sent one patch from it, not the whole document again: the rows
// that changed at their current value, the removals of rows it held (never one it was not sent), the blobs that moved.
func TestFarmDeltaPastTheRing(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	refresh := func() {
		t.Helper()
		if err := f.refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	refresh()
	_, body, _ := f.Snapshot()
	page := holdDoc(t, body)

	// Past the ring: plan A changes on every refresh.
	for i := range ringCapacity + 20 {
		src.mutate("gd-A2b", func(b *beadsapi.BeadDetail) {
			b.Title = fmt.Sprintf("rename %d", i)
			b.UpdatedAt = time.Unix(baseUnix+3000+int64(i), 0)
		})
		refresh()
	}
	// A seed the page held leaves; one it never saw comes and goes.
	src.remove("gd-seed2")
	refresh()
	src.add(planBead("gd-seed9", "draft", "open"))
	refresh()
	src.remove("gd-seed9")
	refresh()
	if _, ok := f.st.replay(page.v); ok {
		t.Fatal("setup: the ring still reaches the page's version")
	}

	catch, _, cancel := f.Subscribe(page.v)
	cancel()
	if len(catch) != 1 || catch[0].Event != "patch" {
		t.Fatalf("catch-up from v%d: %d messages %+v, want one patch", page.v, len(catch), catch)
	}
	var p farmPatch
	if err := json.Unmarshal(catch[0].Data, &p); err != nil {
		t.Fatal(err)
	}
	if p.Prev != page.v || p.V != f.st.version {
		t.Fatalf("patch is v%d -> v%d, want v%d -> v%d", p.Prev, p.V, page.v, f.st.version)
	}
	if p.Plans == nil || len(p.Plans.Up) != 1 || p.Seeds == nil || !slices.Equal(p.Seeds.Rm, []string{"gd-seed2"}) || len(p.Seeds.Up) != 0 {
		t.Fatalf("delta = %s, want plan A once and the seed the page held removed", catch[0].Data)
	}
	if !page.apply(t, catch[0].Data) {
		t.Fatal("the delta does not follow the page's version")
	}
	_, now, _ := f.Snapshot()
	page.same(t, now)

	reset := f.Reset()
	t.Logf("catch-up after %d refreshes: delta %d bytes, reset %d bytes", ringCapacity+23, len(catch[0].Data), len(reset.Data))
	if len(catch[0].Data)*2 > len(reset.Data) {
		t.Errorf("delta is %d bytes, the reset %d: want well under half", len(catch[0].Data), len(reset.Data))
	}
	if st := f.Stats(); st.Deltas != 1 || st.Resets != 1 { // the one reset is f.Reset above
		t.Errorf("stats: %d deltas, %d resets; want 1, 1", st.Deltas, st.Resets)
	}

	// Resume, a laggard's catch-up, is the same patch; at the current version it is nothing.
	if r := f.Resume(page.v); len(r) != 0 { // page.v is the current version once the delta is applied
		t.Errorf("resume at the current version sent %d messages", len(r))
	}
	if r := f.Resume(p.Prev); len(r) != 1 || string(r[0].Data) != string(catch[0].Data) {
		t.Errorf("resume from v%d = %+v, want the same delta", p.Prev, r)
	}
	// A version this process never issued, or none, is still a reset.
	for _, since := range []uint64{0, 999, f.st.version + 1} {
		if r := f.Resume(since); len(r) != 1 || r[0].Event != "reset" {
			t.Errorf("resume from v%d = %+v, want one reset", since, r)
		}
	}
}

// A gardener that leaves after the page's version is removed by the delta with its exit, as the live patch carried it.
func TestFarmDeltaCarriesExits(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	_ = f.refresh(ctx)
	_ = f.refresh(ctx) // the previous gardener read is complete: an id dropped next is an exit
	_, body, _ := f.Snapshot()
	page := holdDoc(t, body)
	var gone string
	for id := range page.sets["gardeners"] {
		if gone == "" || id < gone {
			gone = id
		}
	}
	src.remove(gone)
	_ = f.refresh(ctx)
	var live farmPatch
	_ = json.Unmarshal(f.st.ring[len(f.st.ring)-1].msg.Data, &live)
	if len(live.Exits) == 0 {
		t.Skipf("the fixture's gardener %s left without an exit", gone)
	}
	src.mutate("gd-A2b", func(b *beadsapi.BeadDetail) { b.Title = "after the exit" })
	_ = f.refresh(ctx)

	p, ok := f.dl.patch(page.v, f.st.version, f.viewLocked())
	if !ok || p.Gardeners == nil || !slices.Contains(p.Gardeners.Rm, gone) {
		t.Fatalf("delta %+v does not remove %s", p, gone)
	}
	if len(p.Exits) != len(live.Exits) || p.Exits[0].ID != live.Exits[0].ID {
		t.Errorf("delta exits %+v, the live patch's %+v", p.Exits, live.Exits)
	}
}

// The log is bounded: past deltaKeep the floor rises, and a version below it, or one the stream never issued (a cut
// takes its versions from a counter every cut shares), is not covered.
func TestDeltaLogFloor(t *testing.T) {
	l := newDeltaLog(100, cutView{plans: map[string]entry{"gd-a": {raw: []byte(`{"id":"gd-a"}`)}}})
	l.note(105, farmPatch{Plans: &setPatch{}}, [3][]string{{"gd-a"}})
	l.note(110, farmPatch{Plans: &setPatch{Rm: []string{"gd-a"}}}, [3][]string{})
	for since, want := range map[uint64]bool{100: true, 105: true, 110: true, 103: false, 99: false} {
		if l.covers(since) != want {
			t.Errorf("covers(%d) = %v, want %v", since, !want, want)
		}
	}
	if p, ok := l.patch(105, 110, cutView{}); !ok || p.Plans == nil || !slices.Equal(p.Plans.Rm, []string{"gd-a"}) {
		t.Errorf("patch from 105 = %+v, want gd-a removed", p)
	}

	v := uint64(110)
	for i := range deltaKeep + 1 {
		v++
		id := fmt.Sprintf("gd-t%d", i)
		l.note(v, farmPatch{Plans: &setPatch{}}, [3][]string{{id}})
		v++
		l.note(v, farmPatch{Plans: &setPatch{Rm: []string{id}}}, [3][]string{})
	}
	if l.covers(100) || l.floor <= 110 {
		t.Errorf("floor %d after %d versions: the oldest are still covered", l.floor, 2*deltaKeep)
	}
	if len(l.issued) > deltaKeep || l.nGone > deltaKeep {
		t.Errorf("log holds %d versions and %d tombstones, want at most %d each", len(l.issued), l.nGone, deltaKeep)
	}
	if !l.covers(v-1) || !l.covers(l.floor) {
		t.Errorf("the newest versions and the floor %d are not covered", l.floor)
	}
}

// fillPlanRaw is web/plans.js fillPlan over raw entries: plan entry p with each stub replaced by the bytes of what the
// page holds (held), so a filled plan is byte for byte the document's.
func fillPlanRaw(t *testing.T, held, p json.RawMessage) json.RawMessage {
	t.Helper()
	var m, hm map[string]json.RawMessage
	if err := json.Unmarshal(p, &m); err != nil {
		t.Fatalf("plan entry: %v\n%s", err, p)
	}
	_ = json.Unmarshal(held, &hm)
	var phs, hphs []json.RawMessage
	_ = json.Unmarshal(m["phases"], &phs)
	_ = json.Unmarshal(hm["phases"], &hphs)
	byID := map[string]json.RawMessage{}
	for _, ph := range hphs {
		byID[phaseID(ph)] = ph
	}
	out, stubbed := []byte(p), false
	parts := make([][]byte, len(phs))
	for i, ph := range phs {
		parts[i] = ph
		var s struct {
			ID   string
			Same int
		}
		if _ = json.Unmarshal(ph, &s); s.Same == 1 {
			h, ok := byID[s.ID]
			if !ok {
				t.Errorf("a stub for phase %q of a plan the page holds without it", s.ID)
			}
			parts[i], stubbed = h, true
		}
	}
	if stubbed {
		out = spliceValue(out, phasesKey, m["phases"], append(append([]byte{'['}, bytes.Join(parts, []byte{','})...), ']'))
	}
	if bytes.Equal(m["unphased"], sameStub) {
		if hm["unphased"] == nil {
			t.Error("an unphased stub for a plan the page holds without one")
		}
		out = spliceValue(out, unphasedKey, sameStub, hm["unphased"])
	}
	return out
}

// A step that moves re-sends its phase: the plan's other phases and its unchanged Other row go as stubs, and the page's
// copy filled from them is the document.
func TestFarmPlanPatchStubsPhases(t *testing.T) {
	src := fixtureWorld()
	f := newTestFarm(src)
	ctx := context.Background()
	_ = f.refresh(ctx)
	_, body, _ := f.Snapshot()
	page := holdDoc(t, body)
	full := len(page.sets["plans"]["gd-planA"])
	_, live, cancel := f.Subscribe(page.v)
	defer cancel()

	src.mutate("gd-A2b", func(b *beadsapi.BeadDetail) { b.Title = "a new name for one step" })
	_ = f.refresh(ctx)
	m := <-live
	var p struct {
		Plans struct {
			Up []struct {
				Phases   []map[string]any `json:"phases"`
				Unphased map[string]any   `json:"unphased"`
			} `json:"up"`
		} `json:"plans"`
	}
	if err := json.Unmarshal(m.Data, &p); err != nil || len(p.Plans.Up) != 1 {
		t.Fatalf("patch %s: %v", m.Data, err)
	}
	var stubs, sent int
	for _, ph := range p.Plans.Up[0].Phases {
		if ph["same"] == float64(1) {
			stubs++
		} else {
			sent++
		}
	}
	if sent != 1 || stubs == 0 {
		t.Errorf("plan A went with %d phases and %d stubs, want the moved phase and the rest as stubs: %s", sent, stubs, m.Data)
	}
	if !page.apply(t, m.Data) {
		t.Fatal("the patch does not follow the page's version")
	}
	_, now, _ := f.Snapshot()
	page.same(t, now)
	t.Logf("plan A: %d bytes whole, %d in the patch", full, len(m.Data))
	if len(m.Data) >= full {
		t.Errorf("the patch is %d bytes, the plan %d", len(m.Data), full)
	}

	// A plan change outside its phases stubs every phase.
	src.mutate("gd-planA", func(b *beadsapi.BeadDetail) { b.Title = "Plan A, renamed" })
	_ = f.refresh(ctx)
	m = <-live
	if !page.apply(t, m.Data) {
		t.Fatal("the rename does not follow")
	}
	_, now, _ = f.Snapshot()
	page.same(t, now)
	if strings.Contains(string(m.Data), `"tasks"`) {
		t.Errorf("a plan rename re-sent a phase: %s", m.Data)
	}

	// A catch-up from before both (the two stubbed patches, or one delta with plan A whole) takes the first paint there.
	catch, _, c := f.Subscribe(page.v - 2)
	c()
	first := holdDoc(t, body)
	for _, m := range catch {
		if !first.apply(t, m.Data) {
			t.Fatalf("catch-up %s v%d does not follow v%d", m.Event, m.V, first.v)
		}
	}
	first.same(t, now)
	if p, ok := f.dl.patch(page.v-2, page.v, f.viewLocked()); !ok || strings.Contains(string(mustJSON(t, p)), `"same"`) {
		t.Errorf("the delta from v%d carries a stub or none: %s", page.v-2, mustJSON(t, p))
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// At 10x (bigWorld: the row cap keeps about 83 plans of 5 phases), one step that moves: the live patch, against the
// plan it used to re-send whole; and a page back after every plan moved, against the reset it used to be sent.
func TestFarmTenXPatchBytes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the 10x farm")
	}
	src := bigWorld(360, 5, 4, 25, 300, 60)
	addHarvests(src, 360, 5, 14)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	_, body, _ := f.Snapshot()
	page := holdDoc(t, body)
	var plan string
	for id := range page.sets["plans"] {
		if plan == "" || id < plan {
			plan = id
		}
	}
	_, live, cancel := f.Subscribe(page.v)
	defer cancel()
	src.mutate(plan+"-ph2-t1", func(b *beadsapi.BeadDetail) { b.Status = "in_progress"; b.UpdatedAt = time.Unix(baseUnix+9000, 0) })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	m := <-live
	whole := len(page.sets["plans"][plan])
	if !page.apply(t, m.Data) {
		t.Fatal("the patch does not follow")
	}
	t.Logf("10x, one step moves: patch %d bytes; plan %s whole %d bytes; document %d bytes", len(m.Data), plan, whole, len(body))
	if len(m.Data)*2 > whole {
		t.Errorf("the patch is %d bytes, the plan %d: want under half", len(m.Data), whole)
	}
	_, now, _ := f.Snapshot()
	page.same(t, now)

	// A page that was away while ten plans moved: one delta, against the reset it used to be sent past the ring.
	away := page.v
	var ids []string
	for id := range page.sets["plans"] {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for i, id := range ids[:10] {
		src.mutate(id+"-ph1-t2", func(b *beadsapi.BeadDetail) {
			b.Status = "in_progress"
			b.UpdatedAt = time.Unix(baseUnix+9100+int64(i), 0)
		})
		if err := f.refresh(ctx); err != nil {
			t.Fatal(err)
		}
	}
	m, ok := f.dl.msg(away, f.st.version, f.viewLocked())
	if !ok || !page.apply(t, m.Data) {
		t.Fatal("no delta from the page's version, or it does not follow")
	}
	_, now, _ = f.Snapshot()
	page.same(t, now)
	t.Logf("10x, back after 10 plans moved: delta %d bytes; reset %d bytes", len(m.Data), len(now))
	if len(m.Data)*5 > len(now) { // ten whole plans of the ~83 in the document
		t.Errorf("the delta is %d bytes, the document %d: want under a fifth", len(m.Data), len(now))
	}
}
