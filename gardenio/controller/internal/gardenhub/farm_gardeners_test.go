package gardenhub

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func findGardener(s snap, id string) *FarmGardener {
	for i := range s.Gardeners {
		if s.Gardeners[i].ID == id {
			return &s.Gardeners[i]
		}
	}
	return nil
}

func plotString(p *FarmPlot) string {
	if p == nil {
		return "home"
	}
	return p.Plan + "/" + p.Phase
}

// nextPatch reads one patch off a subscription.
func nextPatch(t *testing.T, live <-chan Msg) farmPatch {
	t.Helper()
	select {
	case m := <-live:
		var p farmPatch
		if err := json.Unmarshal(m.Data, &p); err != nil {
			t.Fatalf("bad patch: %v\n%s", err, m.Data)
		}
		return p
	case <-time.After(time.Second):
		t.Fatal("no patch")
	}
	return farmPatch{}
}

func patchGardeners(t *testing.T, p farmPatch) []FarmGardener {
	t.Helper()
	if p.Gardeners == nil {
		return nil
	}
	out := make([]FarmGardener, len(p.Gardeners.Up))
	for i, raw := range p.Gardeners.Up {
		if err := json.Unmarshal(raw, &out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// TestFarmGardeners is the fixture farm's gardeners: on their tasks' plots,
// several on one plot, a plan gardener at the gate, one two hops below its
// phase, and the ones at home; pool shells, a leaving agent and a closed bead
// are not gardeners.
func TestFarmGardeners(t *testing.T) {
	f := newTestFarm(fixtureWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	want := map[string]struct{ plot, bucket string }{
		"gd-gWork1":      {"gd-planA/gd-A1", GardenerAlive},
		"gd-gWork2":      {"gd-planA/gd-A1", GardenerAlive},
		"gd-gVisit":      {"gd-planA/gd-A1", GardenerAlive},
		"gd-gForeman":    {"gd-planA/", GardenerAlive},      // the plan epic itself: the gate
		"gd-gNested":     {"gd-planA/gd-A2", GardenerAlive}, // task -> sub-epic -> phase
		"gd-gPhase":      {"gd-planA/gd-A2", GardenerAlive}, // the phase container itself
		"gd-gAsk":        {"gd-planA/gd-A2", GardenerAlive}, // yielding
		"gd-gDown":       {"gd-planA/", GardenerStuck},      // an unphased step
		"gd-gSeed":       {"home", GardenerAlive},           // a seed is a pot, not a plot
		"gd-gAway":       {"home", GardenerAlive},           // a meadow bead
		"gd-gLate":       {"home", GardenerAlive},           // a closed task
		"gd-gIdle":       {"home", GardenerIdle},            // no task
		"gd-agent":       {"home", GardenerIdle},            // nothing set at all
		"gd-gThread":     {"home", GardenerAlive},           // a thread agent: no task, at the barn
		"gd-gThreadDown": {"home", GardenerStuck},           // one with no room in the cluster: down, not gone
	}
	if len(s.Gardeners) != len(want) {
		var ids []string
		for _, g := range s.Gardeners {
			ids = append(ids, g.ID)
		}
		t.Fatalf("gardeners = %v, want the %d of the fixture", ids, len(want))
	}
	if !sort.SliceIsSorted(s.Gardeners, func(i, j int) bool { return s.Gardeners[i].ID < s.Gardeners[j].ID }) {
		t.Fatal("snapshot gardeners are not sorted by id")
	}
	for id, w := range want {
		g := findGardener(s, id)
		if g == nil {
			t.Errorf("%s missing", id)
			continue
		}
		if plotString(g.Plot) != w.plot || g.Bucket != w.bucket {
			t.Errorf("%s: plot %s bucket %s, want %s %s", id, plotString(g.Plot), g.Bucket, w.plot, w.bucket)
		}
		if g.Href != "/garden?bead="+id {
			t.Errorf("%s: href %q", id, g.Href)
		}
	}

	w1 := findGardener(s, "gd-gWork1")
	if w1.Agent != "g-work-1" || w1.Role != "crew" || w1.Project != "gardenio" || w1.Operator != "oidc:matt@example.com" ||
		w1.State != "working" || w1.Task != "gd-A1a" || w1.TaskTitle != "Token exchange for agent pods" {
		t.Errorf("g-work-1 = %+v", w1)
	}
	if want := (baseUnix + 9_437) / 60 * 60; w1.Since != want {
		t.Errorf("since = %d, want last_progress_at floored to the minute (%d)", w1.Since, want)
	}
	if w1.Chat != "/garden/c/bead/gd-gWork1" || findGardener(s, "gd-gWork2").Chat != "/garden/c/bead/gd-gWork2" {
		t.Errorf("chat: thread_id and the legacy slack_thread_bead_id both name a thread")
	}
	if g := findGardener(s, "gd-gIdle"); g.Chat != "" || g.Since != (baseUnix+7800)/60*60 {
		t.Errorf("g-idle: chat %q since %d; want no chat and updated_at floored", g.Chat, g.Since)
	}
	if g := findGardener(s, "gd-gVisit"); g.Project != "zeta" {
		t.Errorf("a visitor keeps its own project: %q", g.Project)
	}
	if g := findGardener(s, "gd-gAway"); g.Project != "zeta" || g.TaskTitle == "" {
		t.Errorf("g-away: project %q (from fields.project), task_title %q (a meadow bead is an open row)", g.Project, g.TaskTitle)
	}
	if g := findGardener(s, "gd-gLate"); g.Task != "gd-A1y" || g.TaskTitle != "Title gd-A1y" {
		t.Errorf("g-late: a closed task is named from the farm's own read of it (gd-YvdhZD2UtL): %+v", g)
	}
	if g := findGardener(s, "gd-gForeman"); g.Role != "gardener,plan-gardener" || g.TaskTitle != "Edge IdP rollout" {
		t.Errorf("g-foreman = %+v", g)
	}
	// A thread agent names its thread head without the bridge's lead (5b); a crew whose thread head the farm did not read
	// has its chat and no title.
	if g := findGardener(s, "gd-gThread"); g.Chat != "/garden/c/bead/gd-gThread" || g.ThreadTitle != "a pen for the goats" || g.Plot != nil {
		t.Errorf("g-thread = %+v; want its Commons thread and the head's title, at home", g)
	}
	if g := findGardener(s, "gd-gThreadDown"); g.ThreadTitle != "can the exchange run behind the edge?" || g.Reason == nil || g.Reason.Kind != ReasonDown ||
		g.Reason.Text != "Failed: stale: unschedulable, cluster capacity" {
		t.Errorf("g-thread-down = %+v (reason %+v); want down with the reaper's close reason", g, g.Reason)
	}
	if w1.ThreadTitle != "" {
		t.Errorf("g-work-1 thread_title %q; its head gd-thread1 is not a row and it is no thread agent, so it is not read", w1.ThreadTitle)
	}
}

// TestFarmGardenerTaskArms is the fixture farm's gardeners wearing their line arms.
func TestFarmGardenerTaskArms(t *testing.T) {
	f := newTestFarm(fixtureWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	// Line arms (heraldry step 5.9, gd-QboM9sPYCg): a gardener wears the arms of its task's nearest armed ancestor, from
	// the rows the farm holds: under Foundations (🧭🔥📦) and Rollout (🚀🔑📣), nested or on the phase itself, and from the
	// farm's own read of a closed task (g-late, whose step is Foundations'); none under an unarmed line or with no task.
	for id, want := range map[string]string{
		"gd-gWork1": "🧭🔥📦", "gd-gLate": "🧭🔥📦", "gd-gNested": "🚀🔑📣", "gd-gAsk": "🚀🔑📣", "gd-gPhase": "🚀🔑📣",
		"gd-gForeman": "", "gd-gDown": "", "gd-gAway": "", "gd-gIdle": "", "gd-gThread": "",
	} {
		g := findGardener(s, id)
		got := ""
		if g.TaskArms != nil {
			got = g.TaskArms.Canonical
			if g.TaskArms.N != 3 || !strings.HasPrefix(g.TaskArms.Name, "arms: ") {
				t.Errorf("%s: task_arms = %+v; want three charges and a plain-words name", id, g.TaskArms)
			}
		}
		if got != want {
			t.Errorf("%s: task_arms %q, want %q", id, got, want)
		}
	}
}

// TestFarmGardenersWire is the fixture farm's gardeners on the wire.
func TestFarmGardenersWire(t *testing.T) {
	f := newTestFarm(fixtureWorld())
	if err := f.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A working gardener's reason and a home plot are null,
	// not missing (TestFarmGardenerReasons has the rest); exits is never in a
	// snapshot.
	_, raw, _ := f.Snapshot()
	var doc struct {
		Gardeners []map[string]json.RawMessage `json:"gardeners"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, g := range doc.Gardeners {
		if g["reason"] == nil || g["plot"] == nil {
			t.Fatalf("gardener %s: reason %s plot %s; want both always present", g["id"], g["reason"], g["plot"])
		}
		if string(g["id"]) == `"gd-gWork1"` && string(g["reason"]) != "null" {
			t.Fatalf("a working gardener's reason is null, got %s", g["reason"])
		}
	}
	if strings.Contains(string(raw), `"exits"`) {
		t.Fatal("exits is patch-only")
	}
}

// TestFarmGardenerFollowsTaskTwoHops: the gardener stands on the plot its task
// is drawn on, through two ancestor hops (task -> sub-epic -> phase), and
// follows the task when it moves. A field that ticks with each turn does not
// re-send the gardener.
// gardenerArms walks the rows the farm holds and its reference reads by badge.ResolveArms: a task's own stored arms never
// count (R6), the walk passes an unarmed phase to the plan, and it ends without arms where the line leaves what the farm
// read (a parent it never saw, a bead the reference read did not find, no bead at all).
func TestGardenerArms(t *testing.T) {
	plan := fb("gd-plan", "epic", "in_progress", "", 1)
	plan.Heraldry = "🧭🔥📦"
	phase := fb("gd-ph", "epic", "open", "gd-plan", 1)
	armed := fb("gd-ph2", "epic", "open", "gd-plan", 1)
	armed.Heraldry = "🚀🔑📣"
	task := fb("gd-t", "task", "open", "gd-ph", 1)
	task.Heraldry = "🛡️🗣️📢" // stored on a task by an older write: never shown (R6)
	lost := fb("gd-lost", "task", "open", "gd-nowhere", 1)
	closed := fb("gd-closed", "task", "closed", "gd-ph2", 1)
	byID := map[string]*beadsapi.BeadDetail{"gd-plan": plan, "gd-ph": phase, "gd-ph2": armed, "gd-t": task, "gd-lost": lost}
	refs := map[string]refBead{"gd-closed": {title: "closed", line: farmLineNode(closed)}, "gd-gone": {}}
	armsOf := gardenerArms(byID, refs)
	want := map[string]string{"gd-t": "🧭🔥📦", "gd-ph2": "🚀🔑📣", "gd-plan": "🧭🔥📦", "gd-closed": "🚀🔑📣", "gd-lost": "", "gd-gone": "", "gd-unknown": "", "": ""}
	for id, w := range want {
		got := ""
		if a := armsOf(id); a != nil {
			got = a.Canonical
		}
		if got != w {
			t.Errorf("armsOf(%q) = %q, want %q", id, got, w)
		}
	}
	if a, b := armsOf("gd-t"), armsOf("gd-t"); a != b {
		t.Error("a second ask for the same bead is not the memoised answer")
	}
}

func TestFarmGardenerFollowsTaskTwoHops(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-planH", "building", "in_progress", "gd-H1", "gd-H2"),
		fb("gd-H1", "epic", "open", "gd-planH", 1), fb("gd-H2", "epic", "open", "gd-planH", 1),
		fb("gd-Hsub1", "epic", "open", "gd-H1", 1), fb("gd-Hsub2", "epic", "open", "gd-H2", 1),
		fb("gd-Ht", "task", "in_progress", "gd-Hsub1", 2))
	a := agentBead("gd-aWalk", "a-walk", "crew", "working", "gd-Ht", 10)
	a.Fields["last_progress_at"] = time.Unix(baseUnix+600, 0).UTC().Format(time.RFC3339)
	src.add(a)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	g := findGardener(decodeSnap(t, f), "gd-aWalk")
	if g == nil || plotString(g.Plot) != "gd-planH/gd-H1" {
		t.Fatalf("gardener = %+v, want on plan H phase H1", g)
	}
	_, live, cancel := f.Subscribe(f.Stats().Version)
	defer cancel()

	// Per-turn churn: cost, tokens and updated_at move, the minute does not.
	v := f.Stats().Version
	src.mutate("gd-aWalk", func(b *beadsapi.BeadDetail) {
		b.Fields["live_turns"], b.Fields["context_tokens"] = "12", "80000"
		b.UpdatedAt = b.UpdatedAt.Add(time.Minute)
		b.Fields["last_progress_at"] = time.Unix(baseUnix+610, 0).UTC().Format(time.RFC3339)
	})
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Stats().Version != v {
		t.Fatal("a per-turn field re-sent the gardener")
	}

	src.mutate("gd-Ht", func(b *beadsapi.BeadDetail) { b.ParentID = "gd-Hsub2"; b.UpdatedAt = b.UpdatedAt.Add(time.Second) })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p := nextPatch(t, live)
	up := patchGardeners(t, p)
	if len(up) != 1 || up[0].ID != "gd-aWalk" || plotString(up[0].Plot) != "gd-planH/gd-H2" || len(p.Gardeners.Rm) != 0 || p.Exits != nil {
		t.Fatalf("patch = %+v; want gd-aWalk upserted whole onto phase H2", p.Gardeners)
	}
}

// TestFarmGardenerExits: a gardener that leaves the set is an rm, and the
// patch says why: finished, parked behind an open handle, or interrupted.
func TestFarmGardenerExits(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	src.add(planBead("gd-planX", "building", "in_progress", "gd-X1"), fb("gd-X1", "epic", "open", "gd-planX", 1),
		fb("gd-Xt1", "task", "in_progress", "gd-X1", 2), fb("gd-Xt2", "task", "in_progress", "gd-X1", 3))
	src.add(agentBead("gd-aDone", "a-done", "crew", "working", "gd-Xt1", 10),
		agentBead("gd-aPark", "a-park", "crew", "working", "gd-Xt2", 10),
		agentBead("gd-aKill", "a-kill", "crew", "working", "gd-Xt2", 10),
		agentBead("gd-aGate", "a-gate", "plan-gardener", "working", "gd-planX", 10),
		agentBead("gd-aHome", "a-home", "crew", "idle", "", 10),
		agentBead("gd-aStay", "a-stay", "crew", "working", "gd-Xt2", 10))
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	_, live, cancel := f.Subscribe(f.Stats().Version)
	defer cancel()

	long := strings.Repeat("word ", 60)
	stop := func(tldr string) func(*beadsapi.BeadDetail) {
		return func(b *beadsapi.BeadDetail) {
			b.Status, b.Fields["agent_state"], b.Fields["stop_requested"] = "closed", "done", "true"
			raw, _ := json.Marshal(map[string]string{"tldr": tldr, "accomplishments": "x"})
			b.Fields["wrapup"] = string(raw)
		}
	}
	src.mutate("gd-Xt1", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	src.mutate("gd-aDone", stop("Merged MR 3286"))
	src.mutate("gd-aPark", stop("Parked behind a merge escalation"))
	src.mutate("gd-aKill", func(b *beadsapi.BeadDetail) {
		b.Status, b.Fields["agent_state"] = "closed", "failed"
		b.Fields["wrapup"] = `{"tldr":"closed without a wrap-up"}`
	})
	// Still open, on its way out: gone from the set the moment it stops.
	src.mutate("gd-aGate", func(b *beadsapi.BeadDetail) {
		b.Fields["agent_state"], b.Fields["stop_requested"] = "stop_requested", "true"
		b.Fields["wrapup"] = `{"tldr":"` + long + `"}`
	})
	src.mutate("gd-aHome", stop("Answered the thread"))
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p := nextPatch(t, live)
	if p.Gardeners == nil || strings.Join(p.Gardeners.Rm, ",") != "gd-aDone,gd-aGate,gd-aHome,gd-aKill,gd-aPark" || len(p.Gardeners.Up) != 0 {
		t.Fatalf("gardeners patch = %+v", p.Gardeners)
	}
	want := map[string]struct{ kind, plot string }{
		"gd-aDone": {ExitFinished, "gd-planX/gd-X1"},    // its task closed
		"gd-aGate": {ExitFinished, "gd-planX/"},         // its task is the plan epic
		"gd-aHome": {ExitFinished, "home"},              // no task
		"gd-aKill": {ExitInterrupted, "gd-planX/gd-X1"}, // no gb stop
		"gd-aPark": {ExitParked, "gd-planX/gd-X1"},      // its task is still an open step
	}
	if len(p.Exits) != len(want) {
		t.Fatalf("exits = %+v", p.Exits)
	}
	for _, x := range p.Exits {
		w := want[x.ID]
		if x.Kind != w.kind || plotString(x.Plot) != w.plot || x.At != f.now().Unix() || x.Agent == "" {
			t.Errorf("%s: %+v, want %s from %s", x.ID, x, w.kind, w.plot)
		}
		switch x.ID {
		case "gd-aDone":
			if x.TLDR != "Merged MR 3286" {
				t.Errorf("tldr = %q", x.TLDR)
			}
		case "gd-aKill":
			if x.TLDR != "" {
				t.Errorf("an interrupted exit shows no tldr: %q", x.TLDR)
			}
		case "gd-aGate":
			if n := len([]rune(x.TLDR)); n != maxExitTLDR {
				t.Errorf("tldr is %d runes, want cut at %d", n, maxExitTLDR)
			}
		}
	}
	_, raw, _ := f.Snapshot()
	if strings.Contains(string(raw), `"exits"`) {
		t.Fatal("exits leaked into the snapshot")
	}
	if s := decodeSnap(t, f); len(s.Gardeners) != 1 || s.Gardeners[0].ID != "gd-aStay" {
		t.Fatalf("gardeners left = %+v", s.Gardeners)
	}
}

// agentFailSrc fails the agent reads only, while fail is set.
type agentFailSrc struct {
	*fakeFarmSrc
	fail bool
}

func (s *agentFailSrc) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.fail && in(q.Types, "agent") {
		return nil, fmt.Errorf("daemon hiccup")
	}
	return s.fakeFarmSrc.ListBeadsFiltered(ctx, q)
}

// TestFarmGardenersSurviveBadAgentRead: a failed agent read never fails the
// refresh, keeps the last good set, says how old it is once stale, and makes
// no exits out of what it missed.
func TestFarmGardenersSurviveBadAgentRead(t *testing.T) {
	seq = 0
	base := newFakeFarm()
	base.add(planBead("gd-planY", "building", "in_progress", "gd-Y1"), fb("gd-Y1", "epic", "open", "gd-planY", 1),
		fb("gd-Yt", "task", "in_progress", "gd-Y1", 2))
	base.add(agentBead("gd-aOne", "a-one", "crew", "working", "gd-Yt", 10), agentBead("gd-aTwo", "a-two", "crew", "working", "gd-Yt", 10))
	src := &agentFailSrc{fakeFarmSrc: base}
	f := newTestFarm(src)
	now := time.Unix(baseUnix+10_000, 0)
	f.now = func() time.Time { return now }
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	goodAt := now.Unix()
	v := f.Stats().Version
	_, live, cancel := f.Subscribe(v)
	defer cancel()

	src.fail = true
	base.mutate("gd-aOne", func(b *beadsapi.BeadDetail) { b.Fields["agent_state"] = "failed" })
	base.mutate("gd-aTwo", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	now = now.Add(time.Second)
	if err := f.refresh(ctx); err != nil {
		t.Fatalf("a failed agent read failed the refresh: %v", err)
	}
	if f.Stats().Version != v || len(decodeSnap(t, f).Gardeners) != 2 {
		t.Fatal("a failed agent read changed the gardeners")
	}

	// Past farmRosterStale the document says how old the set is.
	now = now.Add(farmRosterStale)
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p := nextPatch(t, live)
	if p.Gardeners != nil || p.Meta == nil {
		t.Fatalf("stale patch = %+v; want only meta", p)
	}
	if s := decodeSnap(t, f); s.Meta.GardenersAsOf != goodAt {
		t.Fatalf("gardeners_as_of = %d, want %d", s.Meta.GardenersAsOf, goodAt)
	}

	// Back: the set catches up, the key goes away, and the gardener that left
	// during the outage leaves without an exit (the previous read was not good).
	src.fail = false
	now = now.Add(time.Second)
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p = nextPatch(t, live)
	if p.Gardeners == nil || strings.Join(p.Gardeners.Rm, ",") != "gd-aTwo" || len(p.Exits) != 0 {
		t.Fatalf("recovery patch = %+v exits %+v", p.Gardeners, p.Exits)
	}
	if up := patchGardeners(t, p); len(up) != 1 || up[0].Bucket != GardenerStuck {
		t.Fatalf("recovery upserts = %+v", up)
	}
	if s := decodeSnap(t, f); s.Meta.GardenersAsOf != 0 {
		t.Fatalf("gardeners_as_of = %d after a good read, want omitted", s.Meta.GardenersAsOf)
	}
}

// heavyFailSrc fails the list reads fail picks (all of them pass while it is nil) and counts the reads it failed.
type heavyFailSrc struct {
	*fakeFarmSrc
	fail   func(beadsapi.ListBeadsQuery) bool
	failed int
}

func (s *heavyFailSrc) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.fail != nil && s.fail(q) {
		s.failed++
		return nil, fmt.Errorf("context deadline exceeded")
	}
	return s.fakeFarmSrc.ListBeadsFiltered(ctx, q)
}

// TestFarmGardenersOutliveHeavyReadFailures (gd-NiFezSFVxz): under a saturated daemon the SLOW tier's closed reads
// timed out on every refresh, every refresh failed, and the gardeners stood still for 45 min while the agents behind
// them came and went. A failed slow tier or work-row read now keeps its last good read and the snapshot is still
// built, so the gardeners follow the live agents; the refresh still reports the error, and a failed slow tier is not
// asked again before farmSlowBackoff.
func TestFarmGardenersOutliveHeavyReadFailures(t *testing.T) {
	seq = 0
	base := newFakeFarm()
	base.add(planBead("gd-planH", "building", "in_progress", "gd-H1"), fb("gd-H1", "epic", "open", "gd-planH", 1),
		fb("gd-Ht", "task", "in_progress", "gd-H1", 2))
	base.add(agentBead("gd-aOne", "a-one", "crew", "working", "gd-Ht", 10))
	src := &heavyFailSrc{fakeFarmSrc: base}
	f := newTestFarm(src)
	now := time.Unix(baseUnix+10_000, 0)
	f.now = func() time.Time { return now }
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	gardeners := func() []string {
		var ids []string
		for _, g := range decodeSnap(t, f).Gardeners {
			ids = append(ids, g.ID)
		}
		sort.Strings(ids)
		return ids
	}

	// The slow tier's closed reads fail: the new agent is still drawn.
	src.fail = func(q beadsapi.ListBeadsQuery) bool { return in(q.Statuses, "closed") }
	base.add(agentBead("gd-aTwo", "a-two", "crew", "working", "gd-Ht", 20))
	now = now.Add(farmSlowEvery)
	if err := f.refresh(ctx); err == nil {
		t.Fatal("a failed slow tier was not reported")
	}
	if got := strings.Join(gardeners(), ","); got != "gd-aOne,gd-aTwo" {
		t.Fatalf("gardeners after a failed slow tier = %s, want gd-aOne,gd-aTwo", got)
	}
	if f.Stats().LastError == "" {
		t.Fatal("LastError is empty after a failed slow tier")
	}

	// Inside the backoff the slow tier is not asked again, and the refresh is good.
	failed := src.failed
	base.add(agentBead("gd-aThree", "a-three", "crew", "working", "gd-Ht", 30))
	now = now.Add(farmSlowEvery)
	if err := f.refresh(ctx); err != nil {
		t.Fatalf("a refresh inside the slow backoff failed: %v", err)
	}
	if src.failed != failed {
		t.Fatalf("the slow tier was read %d more times inside its backoff", src.failed-failed)
	}
	if got := len(gardeners()); got != 3 {
		t.Fatalf("%d gardeners inside the slow backoff, want 3", got)
	}

	// Past it the slow tier reads again and clears the backoff.
	src.fail = nil
	slow := f.Stats().SlowRefreshes
	now = now.Add(farmSlowBackoff)
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if f.Stats().SlowRefreshes != slow+1 || !f.slowFailedAt.IsZero() {
		t.Fatalf("slow tier after its backoff: refreshes %d -> %d, failed at %v", slow, f.Stats().SlowRefreshes, f.slowFailedAt)
	}

	// The open work rows fail: the last rows stand (the plan is still drawn) and the agent that left leaves.
	src.fail = func(q beadsapi.ListBeadsQuery) bool { return in(q.Statuses, "open") && !in(q.Types, "agent") }
	base.mutate("gd-aThree", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	now = now.Add(time.Second)
	if err := f.refresh(ctx); err == nil {
		t.Fatal("a failed work-row read was not reported")
	}
	if got := strings.Join(gardeners(), ","); got != "gd-aOne,gd-aTwo" {
		t.Fatalf("gardeners after a failed work-row read = %s, want gd-aOne,gd-aTwo", got)
	}
	if len(decodeSnap(t, f).Plans) != 1 {
		t.Fatal("a failed work-row read dropped the plan")
	}
}

// farmRetryAfter doubles from retryAfterError and stops at farmRetryMax.
func TestFarmRetryAfter(t *testing.T) {
	for fails, want := range map[int]time.Duration{1: retryAfterError, 2: 2 * retryAfterError, 3: 4 * retryAfterError, 50: farmRetryMax} {
		if got := farmRetryAfter(fails); got != want {
			t.Errorf("farmRetryAfter(%d) = %v, want %v", fails, got, want)
		}
	}
}

func TestAgentBucket(t *testing.T) {
	for state, want := range map[string]string{
		"working": GardenerAlive, "spawning": GardenerAlive, "yielding": GardenerAlive,
		"failed": GardenerStuck, "rate_limited": GardenerStuck, "budget_exceeded": GardenerStuck,
		"dormant": GardenerDormant,
		"":        GardenerIdle, "idle": GardenerIdle, "assigning": GardenerIdle, "queued": GardenerIdle, "prewarmed": GardenerIdle,
		"done": GardenerIdle, "no-such-state": GardenerIdle,
	} {
		if got := AgentBucket(state); got != want {
			t.Errorf("AgentBucket(%q) = %q, want %q", state, got, want)
		}
	}
}

// TestFarmGardenerSent: a gardener the garden's "Send a gardener" started (tend.go) names the bead it was sent to, stands
// on that bead's plot when it has no task of its own, keeps its own task's plot when it has one, and its exit says
// which bead it was sent to.
func TestFarmGardenerSent(t *testing.T) {
	seq = 0
	src := fixtureWorld()
	sent := agentBead("gd-gSent", "g-sent", "thread", "working", "", 7900)
	sent.Fields["thread_id"], sent.Fields["spawn_source"] = "gd-sentHead", "commons-thread"
	sent.Fields["slack_root_text"] = "@gardenio tend gd-A1b: Figure out what's up with this bead and make progress on it, or say what unblocks it."
	loose := agentBead("gd-gLoose", "g-loose", "thread", "working", "", 7950) // sent to a meadow bead: at home, but it knows where
	loose.Fields["slack_root_text"] = "@gardenio tend gd-M1: Figure out what's up with this bead."
	busy := agentBead("gd-gBusy", "g-busy", "crew", "working", "gd-A2e", 7960) // a task of its own, and a send: the task places it
	busy.Fields["slack_root_text"] = "@gardenio tend gd-A1b: look"
	other := agentBead("gd-gOther", "g-other", "thread", "working", "", 7970) // a thread that is not a send
	other.Fields["slack_root_text"] = "@gardenio adopt gd-A1b into a plan."
	src.add(sent, loose, busy, other)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	if g := findGardener(s, "gd-gSent"); g == nil || g.Tends != "gd-A1b" || g.TendsTitle != "Staging soak test for exchange" || g.Task != "" || plotString(g.Plot) != "gd-planA/gd-A1" || g.Chat != "/garden/c/bead/gd-gSent" {
		t.Errorf("g-sent = %+v; want sent to step A1b, named, no task, on A1b's plot, with its thread", g)
	}
	if g := findGardener(s, "gd-gLoose"); g == nil || g.Tends != "gd-M1" || g.TendsTitle == "" || g.Plot != nil {
		t.Errorf("g-loose = %+v; want sent to the meadow bead, named (an open row), at home", g)
	}
	if g := findGardener(s, "gd-gBusy"); g == nil || g.Tends != "gd-A1b" || g.Task != "gd-A2e" || plotString(g.Plot) != "gd-planA/gd-A2" {
		t.Errorf("g-busy = %+v; want its own task's plot, and the send named", g)
	}
	if g := findGardener(s, "gd-gOther"); g == nil || g.Tends != "" || g.Plot != nil {
		t.Errorf("g-other = %+v; want no send read from another mention", g)
	}
	if g := findGardener(s, "gd-gWork1"); g.Tends != "" {
		t.Errorf("a gardener on a task was sent nowhere: tends %q", g.Tends)
	}
	for _, g := range decodeRawGardeners(t, f) {
		if _, ok := g["tends"]; ok != (string(g["id"]) == `"gd-gSent"` || string(g["id"]) == `"gd-gLoose"` || string(g["id"]) == `"gd-gBusy"`) {
			t.Errorf("gardener %s: tends is on the wire only for a sent gardener (%v)", g["id"], ok)
		}
	}

	_, live, cancel := f.Subscribe(f.Stats().Version)
	defer cancel()
	src.mutate("gd-gSent", func(b *beadsapi.BeadDetail) {
		b.Fields["agent_state"], b.Fields["stop_requested"] = "done", "true"
		b.Fields[beadsapi.WrapUpFieldName] = `{"tldr":"A1b waits on the soak run; restart it with the new key."}`
		b.UpdatedAt = b.UpdatedAt.Add(time.Second)
	})
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p := nextPatch(t, live)
	if len(p.Exits) != 1 || p.Exits[0].ID != "gd-gSent" || p.Exits[0].Tends != "gd-A1b" || p.Exits[0].TLDR == "" {
		t.Fatalf("exits = %+v; want g-sent's, naming the bead it was sent to, with its tldr", p.Exits)
	}
}

// TestFarmGardenerExecutor: the bridge's escalation executor (an agent bead with escalation_id and no task) stands on the
// plot of the escalation it runs, which is parented to the step that filed it, and is named as running it; one with a task
// of its own is placed by the task; an executor of an escalation off any plan (or not read) is at home but still names it;
// its exit names the escalation so the bead view can show what it said (gd-sXAaYVo2UB); its escalation is in escalations.open,
// which the page joins to stand it beside the icon too (gd-B5aDmNhmjz).
func TestFarmGardenerExecutor(t *testing.T) {
	seq = 0
	src := fixtureWorld()
	esc := fb("gd-E1", "escalation", "open", "gd-A1b", 905)
	esc.Fields["operation_type"], esc.Fields["requesting_task"], esc.Fields["executor"] = "gitlab_mr_merge", "gd-A1b", "escalation-gd-e1"
	src.add(esc)
	exec := agentBead("gd-gExec", "escalation-gd-e1", "crew", "working", "", 7900)
	exec.Fields["escalation_id"], exec.Fields["spawned_by"] = "gd-E1", "escalation-bridge"
	busy := agentBead("gd-gBusyExec", "g-busy-exec", "crew", "working", "gd-A2e", 7960) // nudged to run it with a task of its own: the task places it
	busy.Fields["escalation_id"] = "gd-E1"
	lost := agentBead("gd-gLostExec", "escalation-gd-gone", "crew", "working", "", 7970) // its escalation is closed (not read): at home
	lost.Fields["escalation_id"] = "gd-ce1"
	src.add(exec, busy, lost)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	if g := findGardener(s, "gd-gExec"); g == nil || g.Escalation != "gd-E1" || g.EscalationTitle != "Title gd-E1" || g.Task != "" || g.Tends != "" || plotString(g.Plot) != "gd-planA/gd-A1" {
		t.Errorf("executor = %+v; want running E1, named, no task, on A1b's plot", g)
	}
	if g := findGardener(s, "gd-gBusyExec"); g == nil || g.Escalation != "gd-E1" || g.Task != "gd-A2e" || plotString(g.Plot) != "gd-planA/gd-A2" {
		t.Errorf("busy executor = %+v; want its own task's plot, and the escalation named", g)
	}
	if g := findGardener(s, "gd-gLostExec"); g == nil || g.Escalation != "gd-ce1" || g.EscalationTitle != "Title gd-ce1" || g.Plot != nil {
		t.Errorf("lost executor = %+v; want the escalation named, titled from the farm's own read of it, at home", g)
	}
	if g := findGardener(s, "gd-gWork1"); g.Escalation != "" {
		t.Errorf("a gardener on a task runs no escalation: %q", g.Escalation)
	}
	checkExecutorStand(t, s)
	for _, g := range decodeRawGardeners(t, f) {
		id := string(g["id"])
		if _, ok := g["escalation"]; ok != (id == `"gd-gExec"` || id == `"gd-gBusyExec"` || id == `"gd-gLostExec"`) {
			t.Errorf("gardener %s: escalation is on the wire only for an executor (%v)", id, ok)
		}
	}

	_, live, cancel := f.Subscribe(f.Stats().Version)
	defer cancel()
	src.mutate("gd-gExec", func(b *beadsapi.BeadDetail) {
		b.Fields["agent_state"], b.Fields["stop_requested"] = "done", "true"
		b.Fields[beadsapi.WrapUpFieldName] = `{"tldr":"Merged #1114."}`
		b.UpdatedAt = b.UpdatedAt.Add(time.Second)
	})
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	p := nextPatch(t, live)
	if len(p.Exits) != 1 || p.Exits[0].ID != "gd-gExec" || p.Exits[0].Escalation != "gd-E1" || p.Exits[0].Kind != ExitFinished || p.Exits[0].TLDR != "Merged #1114." {
		t.Fatalf("exits = %+v; want the executor's, naming its escalation, finished, with its tldr", p.Exits)
	}
}

// checkExecutorStand: an executor's second stand, beside its escalation's icon (WORLD.escStands, step 7 gd-B5aDmNhmjz), is
// the page's join of the gardener's escalation to escalations.open. E1 is there with its family (the card's "Running merge
// escalation"); the closed one is not, so its executor stays at home only.
func checkExecutorStand(t *testing.T, s snap) {
	t.Helper()
	open := map[string]FarmWait{}
	for _, w := range s.Escalations.Open {
		open[w.ID] = w
	}
	if w, ok := open["gd-E1"]; !ok || w.Family != EscMerge || w.Title != "Title gd-E1" {
		t.Errorf("escalations.open[gd-E1] = %+v (%v); want it open, a merge, titled, for the executor's Escalations stand", w, ok)
	}
	if _, ok := open["gd-ce1"]; ok {
		t.Error("a closed escalation is in escalations.open: its executor would stand beside an icon that is not drawn")
	}
}

// decodeRawGardeners is the snapshot's gardeners[] as raw JSON objects, for what is and is not on the wire.
func decodeRawGardeners(t *testing.T, f *Farm) []map[string]json.RawMessage {
	t.Helper()
	_, raw, _ := f.Snapshot()
	var doc struct {
		Gardeners []map[string]json.RawMessage `json:"gardeners"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Gardeners
}

// TestFarmThreadTitles: a thread agent whose thread head is not among the rows (here a closed head) is named from one
// read of the head, which is cached and not asked again; a head nobody names any more leaves the cache, and a crew's
// thread is never read.
func TestFarmThreadTitles(t *testing.T) {
	seq = 0
	src := newFakeFarm()
	head := fb("gd-oldHead", "task", "closed", "", 10)
	head.Title, head.Labels = "Commons thread: why is the barn so quiet?", append(head.Labels, "thread", "source:commons")
	th := agentBead("gd-gT", "g-t", "thread", "working", "", 20)
	th.Fields["thread_id"] = "gd-oldHead"
	crew := agentBead("gd-gC", "g-c", "crew", "working", "", 30)
	crew.Fields["thread_id"] = "gd-otherHead"
	src.add(head, th, crew)
	f := newTestFarm(src)
	ctx := context.Background()
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	s := decodeSnap(t, f)
	if g := findGardener(s, "gd-gT"); g == nil || g.ThreadTitle != "why is the barn so quiet?" {
		t.Fatalf("g-t = %+v; want the closed head's title, its lead dropped", g)
	}
	if g := findGardener(s, "gd-gC"); g == nil || g.ThreadTitle != "" || g.Chat == "" {
		t.Fatalf("g-c = %+v; want its chat and no title (a crew's head is not read)", g)
	}
	if _, ok := f.refTitles["gd-otherHead"]; ok || len(f.refTitles) != 1 {
		t.Fatalf("refTitles = %v; want only the thread agent's head", f.refTitles)
	}
	src.mu.Lock()
	gets := src.getCalls
	src.mu.Unlock()
	src.mutate("gd-gT", func(b *beadsapi.BeadDetail) { b.Fields["agent_state"] = "yielding" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	src.mu.Lock()
	again := src.getCalls - gets
	src.mu.Unlock()
	if again != 0 {
		t.Errorf("the cached head was read again (%d batch reads)", again)
	}
	if g := findGardener(decodeSnap(t, f), "gd-gT"); g == nil || g.ThreadTitle != "why is the barn so quiet?" {
		t.Errorf("g-t lost its title on the next refresh: %+v", g)
	}
	src.mutate("gd-gT", func(b *beadsapi.BeadDetail) { b.Status = "closed" })
	if err := f.refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if len(f.refTitles) != 0 {
		t.Errorf("refTitles = %v after its agent left; want empty", f.refTitles)
	}
}
