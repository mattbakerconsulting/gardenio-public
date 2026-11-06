package gardenhub

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The viewport cut (gd-fBGGNdgrLc): a page says what it can see on POST /garden/farm/events/view, and its activity
// stream then carries only the calls of the gardeners in that view, or below the Plan level only which projects are
// busy.

// msgs is every message queued on ch, by event name.
func msgs(ch <-chan ActivityMsg) map[string][]ActivityMsg {
	out := map[string][]ActivityMsg{}
	for {
		select {
		case m := <-ch:
			out[m.Event] = append(out[m.Event], m)
		default:
			return out
		}
	}
}

func TestActivityViewCut(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{
		"gd-a": {ID: "gd-a", Project: "alpha"}, "gd-b": {ID: "gd-b", Project: "alpha"}, "gd-c": {ID: "gd-c", Project: "beta"},
	})
	sid, ch, cancel := a.subscribe(nil)
	defer cancel()
	call := func(ids ...string) {
		c.add(activityMinGap) // past the gap, so each goes at once
		for _, id := range ids {
			a.observe(pre(id, "Read", "r", c.t), c.t)
		}
	}
	ids := func(es []FarmActivity) string {
		var out []string
		for _, e := range es {
			out = append(out, e.ID)
		}
		return strings.Join(out, ",")
	}

	// No view said yet: every gardener the cut keeps, as before.
	call("gd-a", "gd-b", "gd-c")
	if got := ids(drain(ch)); got != "gd-a,gd-b,gd-c" {
		t.Fatalf("before a view: %s", got)
	}

	// A Plan-level view of gd-a. An id that is no gardener, an empty one and an oversized one are ignored, never an
	// error. The page was sent every call until now, so nothing is replayed.
	if !a.setView(sid, ActivityView{Level: 2, IDs: []string{"gd-a", "gd-nobody", "", strings.Repeat("x", maxViewID+1)}}, c.t) {
		t.Fatal("setView refused its own stream")
	}
	if m := msgs(ch); len(m) != 0 {
		t.Fatalf("a first view replays nothing: %v", m)
	}
	call("gd-a", "gd-b", "gd-c", "gd-pool")
	if got := ids(drain(ch)); got != "gd-a" {
		t.Errorf("an out-of-view gardener emits nothing: got %s", got)
	}

	// Panning to gd-a and gd-b: gd-b came into view and is sent its doing; gd-a was already there.
	a.setView(sid, ActivityView{Level: 3, IDs: []string{"gd-b", "gd-a"}}, c.t)
	m := msgs(ch)
	if len(m[DoingEvent]) != 1 || len(m) != 1 {
		t.Fatalf("one doing event for the gardener that came into view: %v", m)
	}
	var d FarmDoingOf
	if err := json.Unmarshal(m[DoingEvent][0].Data, &d); err != nil || d.ID != "gd-b" || d.Class != DoingRead || d.Tool != "Read" || d.Calls != 2 {
		t.Errorf("doing event: %s", m[DoingEvent][0].Data)
	}

	// Zoomed out to the Project level: no more calls; at once, the projects busy in the last 30 s.
	a.setView(sid, ActivityView{Level: 1, IDs: []string{"gd-a"}}, c.add(10*time.Second))
	m = msgs(ch)
	if len(m[BusyEvent]) != 1 || len(m) != 1 || string(m[BusyEvent][0].Data) != `{"busy":{"alpha":10,"beta":10}}` {
		t.Fatalf("dropping below Plan sends the busy projects at once: %v", m)
	}
	call("gd-a", "gd-c")
	a.flush(c.add(time.Second))
	if m := msgs(ch); len(m) != 0 {
		t.Fatalf("below Plan: no calls, and no busy event within %v: %v", activityBusyEvery, m)
	}
	a.flush(c.add(activityBusyEvery))
	m = msgs(ch)
	if len(m[BusyEvent]) != 1 || len(m) != 1 || string(m[BusyEvent][0].Data) != `{"busy":{"alpha":6,"beta":6}}` {
		t.Fatalf("the busy projects every %v: %v", activityBusyEvery, m)
	}
	a.flush(c.add(activityBusyEvery))
	if m := msgs(ch); len(m) != 0 {
		t.Errorf("nothing heard since the last busy event: nothing sent: %v", m)
	}

	// Back to Plan level from busy: every named gardener is new to the view.
	a.setView(sid, ActivityView{Level: 2, IDs: []string{"gd-a", "gd-c"}}, c.t)
	if m := msgs(ch); len(m[DoingEvent]) != 2 {
		t.Errorf("back at Plan: a doing for each gardener in view: %v", m)
	}

	if a.setView("no-such-stream", ActivityView{Level: 2}, c.t) || a.setView("", ActivityView{Level: 2}, c.t) {
		t.Error("setView took an unknown stream")
	}
	cancel()
	if a.setView(sid, ActivityView{Level: 2}, c.t) {
		t.Error("setView took a closed stream")
	}
}

func TestActivityViewKeepsTheCut(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.setGardeners(map[string]FarmGardener{"gd-a": {ID: "gd-a", Project: "alpha"}, "gd-c": {ID: "gd-c", Project: "beta"}})
	sid, ch, cancel := a.subscribe(func(g FarmGardener) bool { return g.Project == "alpha" })
	defer cancel()
	a.observe(pre("gd-a", "Read", "r", c.t), c.t)
	a.observe(pre("gd-c", "Read", "r", c.t), c.t)
	msgs(ch)

	// A view can only narrow: naming a gardener outside the cut gets neither its calls, its doing nor its project.
	a.setView(sid, ActivityView{Level: 1}, c.t)
	if m := msgs(ch); len(m[BusyEvent]) != 1 || string(m[BusyEvent][0].Data) != `{"busy":{"alpha":0}}` {
		t.Errorf("busy outside the cut: %v", m)
	}
	a.setView(sid, ActivityView{Level: 2, IDs: []string{"gd-a", "gd-c"}}, c.t)
	if m := msgs(ch); len(m[DoingEvent]) != 1 || !strings.Contains(string(m[DoingEvent][0].Data), `"id":"gd-a"`) {
		t.Errorf("doing outside the cut: %v", m)
	}
	c.add(activityMinGap)
	a.observe(pre("gd-c", "Read", "r2", c.t), c.t)
	if got := drain(ch); len(got) != 0 {
		t.Errorf("a call outside the cut: %+v", got)
	}

	// The view is bounded: past maxViewIDs the ids are ignored.
	many := make([]string, 0, maxViewIDs+1)
	for i := 0; i < maxViewIDs; i++ {
		many = append(many, fmt.Sprintf("gd-x%d", i))
	}
	a.setView(sid, ActivityView{Level: 2, IDs: append(many, "gd-a")}, c.t)
	a.mu.Lock()
	n := len(a.clients[a.sids[sid]].view.ids)
	a.mu.Unlock()
	if n != maxViewIDs {
		t.Errorf("view holds %d ids, want the cap %d", n, maxViewIDs)
	}
	c.add(activityMinGap)
	a.observe(pre("gd-a", "Read", "r3", c.t), c.t)
	if got := drain(ch); len(got) != 0 {
		t.Errorf("an id past the cap was kept: %+v", got)
	}
}

// TestActivityViewportBytes is the check-in's SSE measurement (gd-1Jx4DjLbT6: 60 gardeners at 4 calls a second, about
// 20-24 KB/s to an Everyone page) redone and kept: the same load to four pages at once, one with no view (Everyone, as
// before the cut), one at the Plan level framing the whole farm (a desktop Everyone page: all 60 named), one at the
// Plan level framing 10 plots, and one at the Project level. Every plot in the 10-plot view has a busy gardener (the
// worst case: the 10x world has 60 gardeners on 360 plans), and the page also names the 6 gardeners in the margin round
// it, so 16 of the 60. Bytes are counted as farmEvents writes them. The load runs without the stream budget (the
// figures before gd-iAq4va9XDt) and with it, each with every start captioned (bird captions, gd-gCHKhIROvB: the
// captions are the spec's section 2.3 samples, median 36 runes as measured on the live hook stream) and without.
func TestActivityViewportBytes(t *testing.T) {
	const gardeners, callsPerSec, secs = 60, 4, 30
	pages := []string{"everyone", "farm", "plan", "project"}
	captions := []string{
		"Show task, claim it, and read team news", "Poll CI status for three refreshed PRs",
		"Trial-merge main into #1282 in a detached worktree", "Find /w/ handling in beads-viewer on main",
		"Show plan ready frontier", "Read web/birds.js", "Run the gardenhub package tests",
		"Check open MRs on the caption branch", "List closed beads for prior art", "Explore gardenhub transcript endpoints",
	}
	type page struct {
		bytes, started int
	}
	measure := func(rate int, captioned bool) map[string]*page {
		c := &clock{t: time.Unix(1792870000, 0)}
		a := newActivity()
		a.rate = rate
		gs := map[string]FarmGardener{}
		var all, plan []string
		for i := 0; i < gardeners; i++ {
			id := fmt.Sprintf("gd-gardnr%04d", i)
			gs[id] = FarmGardener{ID: id, Project: fmt.Sprintf("proj%d", i%12)}
			all = append(all, id)
			if i < 16 {
				plan = append(plan, id)
			}
		}
		a.setGardeners(gs)
		chs := map[string]<-chan ActivityMsg{}
		for _, name := range pages {
			sid, ch, cancel := a.subscribe(nil)
			defer cancel()
			chs[name] = ch
			switch name {
			case "farm":
				a.setView(sid, ActivityView{Level: 2, IDs: all}, c.t)
			case "plan":
				a.setView(sid, ActivityView{Level: 2, IDs: plan}, c.t)
			case "project":
				a.setView(sid, ActivityView{Level: 1}, c.t)
			}
			msgs(ch)
		}
		got := map[string]*page{}
		for _, name := range pages {
			got[name] = &page{}
		}
		count := func() {
			for name, ch := range chs {
				for ev, ms := range msgs(ch) {
					for _, m := range ms {
						got[name].bytes += m.size()
						if ev != ActivityEvent {
							continue
						}
						var e FarmActivity
						if err := json.Unmarshal(m.Data, &e); err != nil {
							t.Fatal(err)
						}
						if e.Tool == "" {
							t.Fatalf("an activity event with no tool word: %s", m.Data)
						}
						if e.N != 0 {
							got[name].started += e.N
						} else {
							got[name].started += e.started()
						}
					}
				}
			}
		}
		// Each call is a PreToolUse and its PostToolUse 50 ms on; the gardeners are staggered across the second. The
		// loop flushes every activityTick, as the farm's does, and the pages drain as fast as farmEvents writes.
		step := 10 * time.Millisecond
		period := time.Second / callsPerSec
		start := c.t
		for tick := 0; c.t.Sub(start) < secs*time.Second; tick++ {
			el := c.t.Sub(start)
			for i := 0; i < gardeners; i++ {
				id := all[i]
				off := (el + time.Duration(i)*period/gardeners) % period
				callID := fmt.Sprintf("%s-%d", id, (el+time.Duration(i)*period/gardeners)/period)
				switch {
				case off < step:
					ev := pre(id, "Read", callID, c.t)
					if captioned {
						ev.Cap = captions[(i+int(el/period))%len(captions)]
					}
					a.observe(ev, c.t)
				case off >= 50*time.Millisecond && off < 50*time.Millisecond+step:
					a.observe(post(id, "Read", callID, c.t), c.t)
				}
			}
			if tick%int(activityTick/step) == 0 {
				a.flush(c.t)
			}
			count()
			c.add(step)
		}
		a.mu.Lock()
		dropped := a.dropped
		a.mu.Unlock()
		if dropped != 0 {
			t.Fatalf("%d events dropped: the measurement is short", dropped)
		}
		return got
	}
	bare, uncaptioned := measure(1<<30, false), measure(activityStreamRate, false)
	before, after := measure(1<<30, true), measure(activityStreamRate, true)
	rate := func(p *page) float64 { return float64(p.bytes) / secs }
	for _, name := range pages {
		t.Logf("activity SSE at %d gardeners x %d calls/s, %s page: %.0f B/s before the stream budget, %.0f B/s with it (%.0f%%), %.0f B/s with it and no captions; calls started %d before, %d with it",
			gardeners, callsPerSec, name, rate(before[name]), rate(after[name]), 100*rate(after[name])/max(rate(before[name]), 1), rate(uncaptioned[name]), before[name].started, after[name].started)
	}
	if r := rate(bare["everyone"]); r < 15000 || r > 30000 {
		t.Errorf("Everyone page %.0f B/s before the budget and the captions, the check-in measured 20-24 KB/s: the load is not the check-in's", r)
	}
	// The captions' ceilings (spec section 7.3): over them, wireCaptionMax drops to 48 runes.
	for _, name := range []string{"everyone", "farm"} {
		if r := rate(after[name]); r > 12000 {
			t.Errorf("a %s page framing the whole farm gets %.0f B/s, want at most 12000", name, r)
		}
		// Still busy: the folds say how many calls started, so every bird is still there to fly (birds.js flies a
		// fold as a flock of up to FLOCK), and every gardener in view is heard about every second.
		if b, a := before[name].started, after[name].started; a < b*95/100 {
			t.Errorf("a %s page hears of %d calls starting with the budget, %d without: the folds lose calls", name, a, b)
		}
	}
	if r := rate(after["plan"]); r > 10000 {
		t.Errorf("a 10-plot view gets %.0f B/s, want at most 10000", r)
	}
	if r := rate(after["plan"]) / rate(before["everyone"]); r > 0.3 {
		t.Errorf("a 10-plot view gets %.0f%% of the unbudgeted Everyone bytes, want at most 30%% (16 of 60 gardeners)", 100*r)
	}
	if r := rate(after["plan"]) / rate(before["plan"]); r < 0.9 {
		t.Errorf("a 10-plot view (16 busy) gets %.0f%% of its bytes with the budget: it should keep the 250 ms fold", 100*r)
	}
	if r := rate(after["project"]); r > 200 {
		t.Errorf("a Project-level page gets %.0f B/s, want the busy flags only (at most 200 B/s)", r)
	}
}

// The stream budget's fold (gd-iAq4va9XDt): a stream with more busy gardeners in view than activityStreamRate allows
// at 250 ms hears each one at most once a gap, with n the calls started in it, while a narrow view next to it keeps the
// 250 ms fold; and a view that goes quiet, or narrows, gets the short fold back.
func TestActivityStreamBudget(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	a := newActivity()
	a.rate = 8 // 2 busy gardeners at 250 ms; 4 at 500 ms; 8 at 1 s
	gs := map[string]FarmGardener{}
	var all []string
	for i := 0; i < 8; i++ {
		id := fmt.Sprintf("gd-g%d", i)
		gs[id] = FarmGardener{ID: id, Project: "alpha"}
		all = append(all, id)
	}
	a.setGardeners(gs)
	wideSID, wide, c1 := a.subscribe(nil)
	defer c1()
	narrowSID, narrow, c2 := a.subscribe(nil)
	defer c2()
	a.setView(wideSID, ActivityView{Level: 2, IDs: all}, c.t)
	a.setView(narrowSID, ActivityView{Level: 2, IDs: all[:2]}, c.t)
	tier := func(sid string) int {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.clients[a.sids[sid]].tier
	}
	if tier(wideSID) != 0 || tier(narrowSID) != 0 {
		t.Fatalf("nobody busy yet: the 250 ms fold for both, got %d and %d", tier(wideSID), tier(narrowSID))
	}

	// Every gardener calls once every 100 ms for 3 s.
	call := 0
	run := func(d time.Duration) {
		for end := c.t.Add(d); c.t.Before(end); c.add(10 * time.Millisecond) {
			if c.t.UnixMilli()%100 == 0 {
				for _, id := range all {
					call++
					a.observe(pre(id, "Bash", fmt.Sprint(call), c.t), c.t)
				}
			}
			if c.t.UnixMilli()%activityTick.Milliseconds() == 0 {
				a.flush(c.t)
			}
		}
	}
	run(activityFoldEvery + 200*time.Millisecond)
	if tier(wideSID) != 2 || tier(narrowSID) != 0 {
		t.Fatalf("8 busy in view at a budget of 8/s: the 1 s fold; 2 in view: 250 ms. Got %d and %d", tier(wideSID), tier(narrowSID))
	}
	drain(wide)
	drain(narrow)
	run(2 * time.Second)
	w, n := drain(wide), drain(narrow)
	per := map[string]int{}
	startedW := 0
	for _, e := range w {
		per[e.ID]++
		if e.Tool == "" || e.Doing == "" {
			t.Errorf("a folded event lost its tool word: %+v", e)
		}
		if e.N > 0 {
			startedW += e.N
		} else {
			startedW += e.started()
		}
	}
	for _, id := range all {
		if per[id] < 1 || per[id] > 3 {
			t.Errorf("wide stream: %d events for %s in 2 s on the 1 s fold", per[id], id)
		}
	}
	if startedW < 8*20-8*10 {
		t.Errorf("wide stream: the folds say %d calls started of %d", startedW, 8*20)
	}
	if len(n) < 2*7 || len(n) > 2*9 {
		t.Errorf("narrow stream: %d events in 2 s for 2 gardeners on the 250 ms fold", len(n))
	}

	// The farm goes quiet: past activityFoldWindow the wide view is back on the 250 ms fold, and a call goes at once.
	a.flush(c.add(activityFoldWindow))
	drain(wide) // the folds that were due
	if tier(wideSID) != 0 {
		t.Fatalf("a quiet view: tier %d, want 0", tier(wideSID))
	}
	c.add(activityMinGap) // that flush sent gd-g3's last 250 ms fold
	a.observe(pre("gd-g3", "Read", "q", c.t), c.t)
	if got := drain(wide); len(got) != 1 || got[0].ID != "gd-g3" {
		t.Errorf("a call to a quiet wide view: %+v", got)
	}

	// Narrowing a busy view puts it back on the short fold at once; dropping below Plan puts it on none.
	run(activityFoldEvery + 200*time.Millisecond)
	if tier(wideSID) != 2 {
		t.Fatalf("busy again: tier %d", tier(wideSID))
	}
	a.setView(wideSID, ActivityView{Level: 2, IDs: all[:1]}, c.t)
	if tier(wideSID) != 0 {
		t.Errorf("a view narrowed to one gardener: tier %d", tier(wideSID))
	}
	a.setView(wideSID, ActivityView{Level: 1}, c.t)
	a.mu.Lock()
	on := a.tiers
	a.mu.Unlock()
	if tier(wideSID) != -1 || on != [activityFoldTiers]int{1} {
		t.Errorf("below Plan: tier %d, streams per fold %v", tier(wideSID), on)
	}
	c1()
	c2()
	a.mu.Lock()
	on = a.tiers
	a.mu.Unlock()
	if on != [activityFoldTiers]int{} {
		t.Errorf("closed streams still counted on a fold: %v", on)
	}
}

// The whole path over HTTP: the stream's hello names it, the page POSTs its view, and an out-of-view gardener's call
// no longer reaches it.
func TestActivityViewOverHTTP(t *testing.T) {
	c := &clock{t: time.Unix(1792870000, 0)}
	f := NewFarm(nil, nil)
	f.now = c.now
	gs := map[string]FarmGardener{"gd-a": {ID: "gd-a", Project: "alpha"}, "gd-b": {ID: "gd-b", Project: "alpha"}}
	f.mu.Lock()
	f.gardeners = gardenerEntries(gs)
	f.mu.Unlock()
	f.act.setGardeners(gs)
	srv := httptest.NewServer(farmMux(&Handler{Farm: f}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/garden/farm/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	next := func(prefix string) string {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case l, ok := <-lines:
				if !ok {
					t.Fatalf("stream ended waiting for %q", prefix)
				}
				if strings.HasPrefix(l, prefix) {
					return l
				}
			case <-deadline:
				t.Fatalf("no %q line", prefix)
			}
		}
	}
	next("event: hello")
	var hello struct{ SID string }
	if err := json.Unmarshal([]byte(strings.TrimPrefix(next("data: "), "data: ")), &hello); err != nil || hello.SID == "" {
		t.Fatalf("hello: %v %+v", err, hello)
	}

	view := func(body string) int {
		r, err := http.Post(srv.URL+"/garden/farm/events/view", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Body.Close()
		return r.StatusCode
	}
	if s := view(`{"sid":"` + hello.SID + `","level":2,"ids":["gd-a","gd-unknown"]}`); s != http.StatusNoContent {
		t.Fatalf("view: %d", s)
	}
	if s := view(`{"sid":"nope","level":2,"ids":["gd-a"]}`); s != http.StatusNotFound {
		t.Errorf("unknown stream: %d, want 404", s)
	}
	if s := view(`not json`); s != http.StatusBadRequest {
		t.Errorf("bad body: %d, want 400", s)
	}

	c.add(activityMinGap)
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-b","tool_name":"Read","tool_use_id":"b1"}`))
	f.ObserveHook([]byte(`{"event":"PreToolUse","agent_bead_id":"gd-a","tool_name":"Edit","tool_use_id":"a1"}`))
	next("event: activity")
	if d := next("data: "); !strings.Contains(d, `"id":"gd-a"`) {
		t.Errorf("the first activity event is not the in-view gardener's: %s", d)
	}
	if st := f.Stats(); st.ActivityViews != 1 || st.ActivityBytes == 0 || st.ActivityFoldClients != [activityFoldTiers]int{1} || st.ActivityFoldBytes[0] == 0 {
		t.Errorf("stats: %+v", st)
	}
}
