package gardenhub

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// held is a page's copy of a farm document, kept as web/plans.js applyPatch keeps it.
type held struct {
	v     uint64
	sets  map[string]map[string]json.RawMessage // plans, seeds, gardeners: id -> entry
	blobs map[string]string
}

var heldSets, heldBlobs = []string{"plans", "seeds", "gardeners"}, []string{"closed_plans", "meadow", "wishes", "home", "escalations", "releases", "piles", "meta", "cost"}

func entryID(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var e struct{ ID string }
	if err := json.Unmarshal(raw, &e); err != nil || e.ID == "" {
		t.Fatalf("entry without an id: %s", raw)
	}
	return e.ID
}

func holdDoc(t *testing.T, body []byte) *held {
	t.Helper()
	var d map[string]json.RawMessage
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("document: %v\n%s", err, body)
	}
	h := &held{sets: map[string]map[string]json.RawMessage{}, blobs: map[string]string{}}
	_ = json.Unmarshal(d["v"], &h.v)
	for _, k := range heldSets {
		var rs []json.RawMessage
		_ = json.Unmarshal(d[k], &rs)
		h.sets[k] = map[string]json.RawMessage{}
		for _, r := range rs {
			h.sets[k][entryID(t, r)] = r
		}
	}
	for _, k := range heldBlobs {
		h.blobs[k] = string(d[k])
	}
	return h
}

// apply is applyPatch: false on a gap.
func (h *held) apply(t *testing.T, data []byte) bool {
	t.Helper()
	var p struct{ V, Prev uint64 }
	var d map[string]json.RawMessage
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("patch: %v\n%s", err, data)
	}
	_ = json.Unmarshal(d["v"], &p.V)
	_ = json.Unmarshal(d["prev"], &p.Prev)
	if p.Prev != h.v {
		return false
	}
	for _, k := range heldSets {
		var sp setPatch
		if d[k] == nil {
			continue
		}
		_ = json.Unmarshal(d[k], &sp)
		for _, r := range sp.Up {
			id := entryID(t, r)
			if k == "plans" {
				r = fillPlanRaw(t, h.sets[k][id], r)
			}
			h.sets[k][id] = r
		}
		for _, id := range sp.Rm {
			if _, ok := h.sets[k][id]; !ok {
				t.Errorf("patch removes %s %s, which the page was never sent", k, id)
			}
			delete(h.sets[k], id)
		}
	}
	for _, k := range heldBlobs {
		if d[k] != nil {
			h.blobs[k] = string(d[k])
		}
	}
	h.v = p.V
	return true
}

// same reports how h differs from the document body: the sets and blobs, the gardeners by id (doing is baked into a
// document, never patched).
func (h *held) same(t *testing.T, body []byte) {
	t.Helper()
	want := holdDoc(t, body)
	if h.v != want.v {
		t.Errorf("held v %d, the document is at %d", h.v, want.v)
	}
	for _, k := range []string{"plans", "seeds"} {
		if len(h.sets[k]) != len(want.sets[k]) {
			t.Errorf("%s: held %d, the document has %d", k, len(h.sets[k]), len(want.sets[k]))
		}
		for id, r := range want.sets[k] {
			if !bytes.Equal(h.sets[k][id], r) {
				t.Errorf("%s %s: held %s, the document has %s", k, id, h.sets[k][id], r)
			}
		}
	}
	if a, b := slices.Sorted(maps.Keys(h.sets["gardeners"])), slices.Sorted(maps.Keys(want.sets["gardeners"])); !slices.Equal(a, b) {
		t.Errorf("gardeners: held %v, the document has %v", a, b)
	}
	for _, k := range heldBlobs {
		if h.blobs[k] != want.blobs[k] {
			t.Errorf("%s: held %s, the document has %s", k, h.blobs[k], want.blobs[k])
		}
	}
}

// drainMsgs takes every message queued on ch.
func drainMsgs(ch <-chan Msg) []Msg {
	var out []Msg
	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// Ids and words only gardenio's cut may carry, and only zeta's.
var (
	gardenioOnly = []string{`"gardenio"`, "gd-planA", "gd-A1a", "gd-A9", "gd-seed1", "gd-seed2", "gd-M1", "gd-MN", "gd-gWork1", "gd-gVisit", "gd-gAway", "gd-Z1c", "gd-Z1e", "gd-A2new"}
	zetaOnly     = []string{`"zeta"`, "gd-planZ", "gd-Z1a", "gd-seedZ", "gd-MZ", "gd-gZeta", "gd-A2z", "gd-Z1new", "gd-doneZ"}
)

// foreignProjects is a task's blocked_by_foreign, the one place a cut names a foreign project (by name only,
// gd-U2pbNv3lLC), so noLeak looks past it.
var foreignProjects = regexp.MustCompile(`"blocked_by_foreign":\[[^\]]*\]`)

func noLeak(t *testing.T, who string, raw []byte, foreign []string) {
	t.Helper()
	rest := foreignProjects.ReplaceAll(raw, nil)
	for _, w := range foreign {
		if bytes.Contains(rest, []byte(w)) {
			t.Errorf("%s was sent %q: %s", who, w, raw)
		}
	}
}

// TestFarmCutStream is the per-viewer cut of /garden/farm/events (gd-UKO0ZpXeha): two scoped viewers in different
// lands and an admin watch the same farm move. Each scoped viewer is sent only its own ids, in its first paint, its
// patches, its removals and a resume, and what it holds after them is its cut document; the admin stream is byte for
// byte the stream of a twin farm nobody scoped ever looked at.
func TestFarmCutStream(t *testing.T) {
	src, twinSrc := multiLandWorld(), multiLandWorld()
	f, twin := newCutFarm(t, src), newCutFarm(t, twinSrc)
	// Read both lands' archives first, so their arrival is not part of what is checked below.
	f.mu.Lock()
	f.wantArchivesLocked([]string{"gardenio", "zeta"}, f.now(), true)
	f.mu.Unlock()
	refresh := func() {
		t.Helper()
		for _, x := range []*Farm{f, twin} {
			if err := x.refresh(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
	}
	refresh()

	zeta, gard := FarmCut{Scoped: true, Projects: []string{"zeta"}}, FarmCut{Scoped: true, Projects: []string{"gardenio"}}
	_, zBody, _, _ := f.CutSnapshot(zeta)
	_, gBody, _, _ := f.CutSnapshot(gard)
	noLeak(t, "zeta's first paint", zBody, gardenioOnly)
	noLeak(t, "gardenio's first paint", gBody, zetaOnly)
	zHeld, gHeld := holdDoc(t, zBody), holdDoc(t, gBody)
	z0 := zHeld.v

	zCatch, zLive, zKeep, zCancel := f.SubscribeCut(zeta, zHeld.v)
	defer zCancel()
	gCatch, gLive, _, gCancel := f.SubscribeCut(gard, gHeld.v)
	defer gCancel()
	if len(zCatch)+len(gCatch) != 0 {
		t.Fatalf("a viewer at its first paint's version was sent a catch-up: %d, %d", len(zCatch), len(gCatch))
	}
	aCatch, aLive, aCancel := f.Subscribe(f.st.version)
	defer aCancel()
	tCatch, tLive, tCancel := twin.Subscribe(twin.st.version)
	defer tCancel()
	if len(aCatch)+len(tCatch) != 0 {
		t.Fatal("the admin was sent a catch-up at its own version")
	}

	var zSent []Msg
	step := func(name string, zWant, gWant bool) {
		t.Helper()
		refresh()
		zs, gs := drainMsgs(zLive), drainMsgs(gLive)
		if (len(zs) > 0) != zWant || (len(gs) > 0) != gWant {
			t.Errorf("%s: zeta was sent %d patches (want any: %v), gardenio %d (want any: %v)", name, len(zs), zWant, len(gs), gWant)
		}
		for _, m := range zs {
			noLeak(t, name+": zeta's patch", m.Data, gardenioOnly)
			if m.Event != "patch" || !zHeld.apply(t, m.Data) {
				t.Fatalf("%s: zeta patch %s v%d does not follow v%d", name, m.Event, m.V, zHeld.v)
			}
			zSent = append(zSent, m)
		}
		for _, m := range gs {
			noLeak(t, name+": gardenio's patch", m.Data, zetaOnly)
			if m.Event != "patch" || !gHeld.apply(t, m.Data) {
				t.Fatalf("%s: gardenio patch %s v%d does not follow v%d", name, m.Event, m.V, gHeld.v)
			}
		}
		as, ts := drainMsgs(aLive), drainMsgs(tLive)
		if len(as) != len(ts) {
			t.Fatalf("%s: the admin was sent %d patches, the twin's admin %d", name, len(as), len(ts))
		}
		for i := range as {
			if as[i].Event != ts[i].Event || !bytes.Equal(as[i].Data, ts[i].Data) {
				t.Errorf("%s: admin patch %d differs from the twin's:\n%s\n%s", name, i, as[i].Data, ts[i].Data)
			}
		}
		if len(as) == 0 {
			t.Errorf("%s: the admin heard nothing", name)
		}
	}
	both := func(fn func(*fakeFarmSrc)) { fn(src); fn(twinSrc) }

	// A gardenio-only change moves gardenio's stream, not zeta's.
	both(func(s *fakeFarmSrc) {
		s.mutate("gd-A9", func(b *beadsapi.BeadDetail) { b.Title = "Rotate the staging signing key, again" })
	})
	step("gardenio title", false, true)

	// A new step in each land: a zeta step in plan Z, a gardenio step in plan A.
	// Built once, so both farms' copies share their created stamps (fb counts up).
	newSteps := []*beadsapi.BeadDetail{zb("gd-Z1new", "task", "open", "gd-Z1", 9900), fb("gd-A2new", "task", "open", "gd-A2", 9900)}
	both(func(s *fakeFarmSrc) {
		for _, b := range newSteps {
			c := *b
			c.Labels, c.Fields = slices.Clone(b.Labels), maps.Clone(b.Fields)
			s.add(&c)
		}
	})
	step("a step each", true, true)

	// Removals in each land: a seed and a gardener each, a meadow bead in zeta.
	both(func(s *fakeFarmSrc) {
		for _, id := range []string{"gd-seedZ", "gd-gZeta", "gd-MZ", "gd-seed1", "gd-gWork1"} {
			s.remove(id)
		}
	})
	step("removals", true, true)
	if _, ok := zHeld.sets["seeds"]["gd-seedZ"]; ok {
		t.Error("zeta still holds its removed seed")
	}
	if _, ok := zHeld.sets["gardeners"]["gd-gZeta"]; ok {
		t.Error("zeta still holds its removed gardener")
	}
	if _, ok := gHeld.sets["seeds"]["gd-seed1"]; ok {
		t.Error("gardenio still holds its removed seed")
	}

	// What each holds is its cut document now.
	_, zNow, _, _ := f.CutSnapshot(zeta)
	_, gNow, _, _ := f.CutSnapshot(gard)
	zHeld.same(t, zNow)
	gHeld.same(t, gNow)
	if zKeep(FarmGardener{ID: "gd-gVisit", Project: "zeta"}) {
		t.Error("the activity cut keeps gd-gVisit, a zeta gardener on a gardenio plot that zeta's document drops")
	}

	// A resume from the first paint takes the first paint to what the live viewer holds now: one delta patch
	// (farm_delta.go) or the chain it was sent live, whichever is fewer bytes; from a version that is not this cut's (the
	// whole farm's, the other land's) it is a reset to this cut's document.
	catch, _, _, c := f.SubscribeCut(zeta, z0)
	c()
	first := holdDoc(t, zBody)
	for _, m := range catch {
		noLeak(t, "zeta's resume", m.Data, gardenioOnly)
		if m.Event != "patch" || !first.apply(t, m.Data) {
			t.Fatalf("resume %s v%d does not follow v%d", m.Event, m.V, first.v)
		}
	}
	first.same(t, zNow)
	if msgBytes(catch) > msgBytes(zSent) || (len(catch) != 1 && len(catch) != len(zSent)) {
		t.Errorf("resume is %d messages, %d bytes; the chain sent live %d, %d bytes", len(catch), msgBytes(catch), len(zSent), msgBytes(zSent))
	}
	for name, since := range map[string]uint64{"the farm's version": f.st.version, "gardenio's version": gHeld.v, "nothing": 0} {
		catch, _, _, c := f.SubscribeCut(zeta, since)
		c()
		if len(catch) != 1 || catch[0].Event != "reset" || !bytes.Equal(catch[0].Data, zNow) {
			t.Errorf("zeta resuming from %s: %d messages, want one reset to its cut", name, len(catch))
			continue
		}
		noLeak(t, "zeta's reset", catch[0].Data, gardenioOnly)
	}
	if r := f.CutReset(zeta); r.Event != "reset" || r.V != zHeld.v || !bytes.Equal(r.Data, zNow) {
		t.Errorf("a laggard's reset is v%d, want zeta's document at v%d", r.V, zHeld.v)
	}

	// The admin documents are the same too, and a cut never shares the farm's version.
	_, a, _ := f.Snapshot()
	_, b, _ := twin.Snapshot()
	if !bytes.Equal(a, b) {
		t.Error("the admin document differs from the twin's")
	}
	if zHeld.v == f.st.version || gHeld.v == f.st.version || zHeld.v == gHeld.v {
		t.Errorf("versions: zeta %d, gardenio %d, farm %d; want each its own", zHeld.v, gHeld.v, f.st.version)
	}
	if st := f.Stats(); st.CutStreams != 2 || st.CutClients != 2 || st.Clients != 1 {
		t.Errorf("stats: %d cut streams, %d cut clients, %d clients; want 2, 2, 1", st.CutStreams, st.CutClients, st.Clients)
	}
}

// Exits ride a scoped patch only for the gardeners it removes, without what can name a bead outside the set.
func TestFarmCutExits(t *testing.T) {
	exits := []FarmExit{
		{ID: "gd-gZeta", Agent: "g-zeta", Kind: "done", TLDR: "merged gd-planA's fix", Tends: "gd-A1a", Escalation: "gd-Z1e", Plot: &FarmPlot{Plan: "gd-planZ", Phase: "gd-Z1"}},
		{ID: "gd-gGone", Agent: "g-gone", Kind: "done", Plot: &FarmPlot{Plan: "gd-planA"}},
		{ID: "gd-gWork1", Agent: "g-work-1", Kind: "done"},
	}
	got := cutExits(exits, []string{"gd-gGone", "gd-gZeta"}, map[string]entry{"gd-planZ": {}}, map[string]entry{})
	if len(got) != 2 {
		t.Fatalf("exits = %+v, want the two removed gardeners'", got)
	}
	if z := got[0]; z.TLDR != "" || z.Tends != "" || z.Escalation != "" || z.Plot == nil || z.Plot.Plan != "gd-planZ" {
		t.Errorf("zeta exit = %+v, want no tldr, tends or escalation, and its plot in view", z)
	}
	if g := got[1]; g.Plot != nil {
		t.Errorf("exit = %+v, want its plot dropped: its plan was never in view", g)
	}
}

// A cut stream with no viewer is dropped once nobody has asked for it in farmArchiveWantTTL; one with a viewer stays.
func TestFarmCutStreamLifetime(t *testing.T) {
	f := newCutFarm(t, multiLandWorld())
	now := f.now()
	f.now = func() time.Time { return now }
	f.CutSnapshot(FarmCut{Scoped: true, Projects: []string{"zeta"}})
	_, _, _, cancel := f.SubscribeCut(FarmCut{Scoped: true, Projects: []string{"gardenio"}}, 0)
	now = now.Add(farmArchiveWantTTL)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.cuts["zeta"]; ok {
		t.Error("an unwatched cut stream outlived farmArchiveWantTTL")
	}
	if _, ok := f.cuts["gardenio"]; !ok {
		t.Fatal("a watched cut stream was dropped")
	}
	cancel()
	now = now.Add(farmArchiveWantTTL)
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.cuts) != 0 {
		t.Errorf("%d cut streams left, want none", len(f.cuts))
	}
}

// Over HTTP: a scoped viewer resuming with Last-Event-ID gets its own cut's catch-up under its own ids, and activity
// only for the gardeners in its document, even one of its land that its document drops.
func TestFarmCutStreamOverHTTP(t *testing.T) {
	src := multiLandWorld()
	f := newCutFarm(t, src)
	c := &clock{t: f.now()}
	f.now = c.now
	h := &Handler{
		Farm: f,
		Cut: func(r *http.Request) FarmCut {
			return FarmCut{Scoped: true, Projects: []string{r.URL.Query().Get("p")}}
		},
		// What beads-viewer wires: a gardener of the viewer's projects.
		SeesGardener: func(r *http.Request) func(FarmGardener) bool {
			p := r.URL.Query().Get("p")
			return func(g FarmGardener) bool { return g.Project == p }
		},
	}
	srv := httptest.NewServer(farmMux(h))
	defer srv.Close()

	_, body, _, _ := f.CutSnapshot(FarmCut{Scoped: true, Projects: []string{"zeta"}})
	v0 := holdDoc(t, body).v
	src.add(zb("gd-Z1new", "task", "open", "gd-Z1", 9900), fb("gd-A2new", "task", "open", "gd-A2", 9900))
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/garden/farm/events?p=zeta", nil)
	req.Header.Set("Last-Event-ID", strconv.FormatUint(v0, 10))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("scoped stream = %d", resp.StatusCode)
	}
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(prefix string) string {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended waiting for %q", prefix)
				}
				noLeak(t, "the zeta stream", []byte(l), gardenioOnly)
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q line", prefix)
			}
		}
	}
	next("event: hello")
	id := next("id: ")
	if ev := next("event: "); ev != "event: patch" {
		t.Fatalf("catch-up is %q, want a patch", ev)
	}
	data := next("data: ")
	var p farmPatch
	if err := json.Unmarshal([]byte(strings.TrimPrefix(data, "data: ")), &p); err != nil || p.Prev != v0 || id != "id: "+strconv.FormatUint(p.V, 10) {
		t.Fatalf("catch-up %s %s: want the patch from v%d under its own id", id, data, v0)
	}
	if !strings.Contains(data, "gd-Z1new") {
		t.Errorf("catch-up %s does not carry the new zeta step", data)
	}

	// gd-gVisit is a zeta gardener on a gardenio plot: its land passes SeesGardener, its document drops it.
	c.add(activityMinGap)
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-gVisit","tool_name":"Read","tool_use_id":"v1"}`))
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-gZeta","tool_name":"Edit","tool_use_id":"z1"}`))
	next("event: activity")
	if d := next("data: "); !strings.Contains(d, `"id":"gd-gZeta"`) {
		t.Errorf("the first activity event is not zeta's own gardener's: %s", d)
	}
}

// The cut stream carries wishes patches for the viewer's cut (gd-gUeBN96RRL on gd-UKO0ZpXeha): a new zeta wish reaches
// zeta's stream recut (its gardenio kin and plot dropped), a new gardenio wish reaches only gardenio's, and each viewer
// then holds its cut document's wishes.
func TestFarmCutStreamWishes(t *testing.T) {
	src := wishWorld()
	f := newCutFarm(t, src)
	f.mu.Lock()
	f.wantArchivesLocked([]string{"gardenio", "zeta"}, f.now(), true)
	f.mu.Unlock()
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	zeta, gard := FarmCut{Scoped: true, Projects: []string{"zeta"}}, FarmCut{Scoped: true, Projects: []string{"gardenio"}}
	_, zBody, _, _ := f.CutSnapshot(zeta)
	_, gBody, _, _ := f.CutSnapshot(gard)
	zHeld, gHeld := holdDoc(t, zBody), holdDoc(t, gBody)
	if !strings.Contains(zHeld.blobs["wishes"], `"gd-zw1"`) {
		t.Fatalf("zeta's first paint has no zeta wish: %s", zHeld.blobs["wishes"])
	}
	_, zLive, _, zCancel := f.SubscribeCut(zeta, zHeld.v)
	defer zCancel()
	_, gLive, _, gCancel := f.SubscribeCut(gard, gHeld.v)
	defer gCancel()

	wish := func(b *beadsapi.BeadDetail) *beadsapi.BeadDetail {
		b.Labels = append(b.Labels, "source:slack", wishLabel)
		return b
	}
	step := func(name string, zWant, gWant string) {
		t.Helper()
		if err := f.refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		for _, c := range []struct {
			who        string
			live       <-chan Msg
			h          *held
			cut        FarmCut
			want       string
			foreign    []string
			wishesSent bool
		}{
			{"zeta", zLive, zHeld, zeta, zWant, append(slices.Clone(gardenioOnly), "gd-wishG"), zWant != ""},
			{"gardenio", gLive, gHeld, gard, gWant, append(slices.Clone(zetaOnly), "gd-zwNew", "gd-zw1"), gWant != ""},
		} {
			sent := false
			for _, m := range drainMsgs(c.live) {
				noLeak(t, name+": "+c.who+"'s patch", m.Data, c.foreign)
				if m.Event != "patch" || !c.h.apply(t, m.Data) {
					t.Fatalf("%s: %s patch %s v%d does not follow v%d", name, c.who, m.Event, m.V, c.h.v)
				}
				var d map[string]json.RawMessage
				_ = json.Unmarshal(m.Data, &d)
				sent = sent || d["wishes"] != nil
			}
			if sent != c.wishesSent {
				t.Errorf("%s: %s was sent a wishes patch: %v, want %v", name, c.who, sent, c.wishesSent)
			}
			if c.want != "" && !strings.Contains(c.h.blobs["wishes"], `"`+c.want+`"`) {
				t.Errorf("%s: %s holds wishes without %s: %s", name, c.who, c.want, c.h.blobs["wishes"])
			}
			_, body, _, _ := f.CutSnapshot(c.cut)
			c.h.same(t, body)
		}
	}

	// A zeta wish with a gardenio kin on gardenio's plan A: zeta is sent it without the kin or the plot.
	src.add(wish(zb("gd-zwNew", "task", "open", "", 9900)))
	src.add(fb("gd-A2k", "task", "open", "gd-A2", 9901))
	src.mutate("gd-A2k", func(b *beadsapi.BeadDetail) { b.Fields["thread_id"] = "gd-zwNew" })
	step("a zeta wish", "gd-zwNew", "")
	if strings.Contains(zHeld.blobs["wishes"], "gd-A2k") || strings.Contains(zHeld.blobs["wishes"], "gd-planA") {
		t.Errorf("zeta's wishes carry gardenio's kin or plot: %s", zHeld.blobs["wishes"])
	}

	// A gardenio wish moves only gardenio's wishes.
	src.add(wish(fb("gd-wishG", "task", "open", "", 9902)))
	step("a gardenio wish", "", "gd-wishG")
}
