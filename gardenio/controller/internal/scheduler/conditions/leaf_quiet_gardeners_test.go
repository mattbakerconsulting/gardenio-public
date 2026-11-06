package conditions

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The quiet-gardener check (gd-bo2RUmtpHb): a building plan whose bound,
// event-mode gardener has not run for two hours while a step is ready, or
// whose gardener is still timed, is what wakes the home steward.

var quietAt = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// quietFarm is a small bead store: ListBeadsFiltered honours the filters the
// check sends, as the daemon does.
type quietFarm struct {
	beads   []*beadsapi.BeadDetail
	parent  map[string]string
	waiting map[string]bool
	// fail, when set, fails a query it returns true for.
	fail    func(beadsapi.ListBeadsQuery) bool
	queries int
	// comments are each bead's comments; failComments fails their read.
	comments     map[string][]beadsapi.Comment
	failComments bool
}

func (f *quietFarm) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	if f.failComments {
		return nil, errors.New("daemon unavailable")
	}
	return f.comments[id], nil
}

func (f *quietFarm) under(id, root string) bool {
	for p := f.parent[id]; p != ""; p = f.parent[p] {
		if p == root {
			return true
		}
	}
	return false
}

func (f *quietFarm) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.queries++
	if f.fail != nil && f.fail(q) {
		return nil, errors.New("daemon unavailable")
	}
	var out []*beadsapi.BeadDetail
	for _, b := range f.beads {
		ok := (len(q.Types) == 0 || slices.Contains(q.Types, b.Type)) &&
			(len(q.Statuses) == 0 || slices.Contains(q.Statuses, b.Status)) &&
			!slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) &&
			(q.Parent == "" || f.under(b.ID, q.Parent)) &&
			(!q.NoOpenDeps || !f.waiting[b.ID]) &&
			(!q.Unassigned || b.Assignee == "") &&
			(q.CreatedAfter.IsZero() || !b.CreatedAt.Before(q.CreatedAfter))
		for k, v := range q.Fields {
			ok = ok && b.Fields[k] == v
		}
		if !ok {
			continue
		}
		// The daemon projects each decision flag only when asked for it.
		if (b.BlockedOnDecision && !q.WithBlockedOnDecision) || (b.ParkedOnDecision && !q.WithParkedOnDecision) {
			c := *b
			c.BlockedOnDecision = c.BlockedOnDecision && q.WithBlockedOnDecision
			c.ParkedOnDecision = c.ParkedOnDecision && q.WithParkedOnDecision
			b = &c
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (f *quietFarm) plan(id, state string) {
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: id, Type: "epic", Title: "plan " + id, Status: "open",
		Labels: []string{"project:gardenio", "plan"}, Fields: map[string]string{"plan_state": state},
	})
}

// gardener binds plan a gardener in mode ("event" or "timed") that last ran
// lastRun ago (0: never), created three days back.
func (f *quietFarm) gardener(id, plan, mode string, lastRun time.Duration, fields map[string]string) *beadsapi.BeadDetail {
	label := "trigger:multi"
	if mode == "timed" {
		label = "trigger:schedule"
	}
	all := map[string]string{"enabled": "true", "last_outcome": "no source matches (out of window / preflight not met / already fired)\nmore"}
	if lastRun > 0 {
		all["last_run"] = quietAt.Add(-lastRun).Format(time.RFC3339)
	}
	for k, v := range fields {
		all[k] = v
	}
	b := &beadsapi.BeadDetail{
		ID: id, Type: "trigger", Title: "gardener " + plan, Status: "open", CreatedAt: quietAt.Add(-72 * time.Hour),
		Labels: []string{label, "role:gardener", "gardener-epic:" + plan, "plan-gardener"}, Fields: all,
	}
	f.beads = append(f.beads, b)
	return b
}

func (f *quietFarm) step(id, parent, typ, status string, edit func(*beadsapi.BeadDetail)) {
	b := &beadsapi.BeadDetail{ID: id, Type: typ, Title: "step " + id, Status: status, Labels: []string{"project:gardenio"}}
	if edit != nil {
		edit(b)
	}
	f.beads = append(f.beads, b)
	f.parent[id] = parent
}

func (f *quietFarm) decision(id, trigger, by string, ago time.Duration) {
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: id, Type: "decision", Status: "open", CreatedBy: by,
		CreatedAt: quietAt.Add(-ago), Fields: map[string]string{"subject_trigger_id": trigger},
	})
}

// newQuietFarm is the home garden with a plan of each kind.
func newQuietFarm() *quietFarm {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	// Quiet: a ready step and no run for three hours.
	f.plan("gd-p1", "building")
	f.gardener("gd-g1", "gd-p1", "event", 3*time.Hour, nil)
	f.step("gd-s1", "gd-p1", "task", "open", nil)
	// Quiet, its ready steps under a phase and of every ready type.
	f.plan("gd-p2", "building")
	f.gardener("gd-g2", "gd-p2", "event", 5*time.Hour, nil)
	f.step("gd-p2-ph", "gd-p2", "epic", "open", nil)
	for _, s := range []struct{ id, typ string }{{"gd-s2a", "bug"}, {"gd-s2b", "feature"}, {"gd-s2c", "chore"}, {"gd-s2d", "task"}} {
		f.step(s.id, "gd-p2-ph", s.typ, "open", nil)
	}
	// Waiting: every open step waits on a bead or a decision, or is parked
	// behind one by mention.
	f.plan("gd-p3", "building")
	f.gardener("gd-g3", "gd-p3", "event", 6*time.Hour, nil)
	f.step("gd-s3a", "gd-p3", "task", "open", nil)
	f.waiting["gd-s3a"] = true
	f.step("gd-s3b", "gd-p3", "task", "open", func(b *beadsapi.BeadDetail) { b.BlockedOnDecision = true })
	f.step("gd-s3c", "gd-p3", "bug", "open", func(b *beadsapi.BeadDetail) { b.ParkedOnDecision = true })
	// In progress: one step claimed, one assigned, one blocked, one a doc,
	// one the owner's at a device.
	f.plan("gd-p4", "building")
	f.gardener("gd-g4", "gd-p4", "event", 6*time.Hour, nil)
	f.step("gd-s4a", "gd-p4", "task", "in_progress", nil)
	f.step("gd-s4b", "gd-p4", "task", "open", func(b *beadsapi.BeadDetail) { b.Assignee = "gardenio-crew-x1y" })
	f.step("gd-s4c", "gd-p4", "task", "blocked", nil)
	f.step("gd-s4d", "gd-p4", "doc", "open", nil)
	f.step("gd-s4e", "gd-p4", "task", "open", func(b *beadsapi.BeadDetail) { b.Labels = append(b.Labels, "owner-at-device") })
	// Ran inside the threshold.
	f.plan("gd-p5", "building")
	f.gardener("gd-g5", "gd-p5", "event", 119*time.Minute, nil)
	f.step("gd-s5", "gd-p5", "task", "open", nil)
	// A cycle is live.
	f.plan("gd-p6", "building")
	f.gardener("gd-g6", "gd-p6", "event", 4*time.Hour, nil)
	f.step("gd-s6", "gd-p6", "task", "open", nil)
	f.beads = append(f.beads, &beadsapi.BeadDetail{
		ID: "gd-a6", Type: "agent", Status: "in_progress",
		Fields: map[string]string{"spawned_by": "trigger:gd-g6"},
	})
	// Still timed, run ten minutes ago, nothing ready.
	f.plan("gd-p7", "building")
	f.gardener("gd-g7", "gd-p7", "timed", 10*time.Minute, map[string]string{"cron": "5,35 * * * *"})
	// Paused: the gardener-less list's.
	f.plan("gd-p8", "building")
	f.gardener("gd-g8", "gd-p8", "event", 9*time.Hour, map[string]string{"enabled": "false"})
	f.step("gd-s8", "gd-p8", "task", "open", nil)
	// Not building.
	f.plan("gd-p9", "draft")
	f.gardener("gd-g9", "gd-p9", "event", 9*time.Hour, nil)
	f.step("gd-s9", "gd-p9", "task", "open", nil)
	return f
}

func findQuiet(t *testing.T, f *quietFarm) map[string]QuietGardener {
	t.Helper()
	plans, err := ListQuietPlans(context.Background(), f, "gardenio")
	if err != nil {
		t.Fatal(err)
	}
	found, err := FindQuietGardeners(context.Background(), f, "gardenio", plans, quietAt)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]QuietGardener{}
	for _, q := range found {
		got[q.Plan] = q
	}
	return got
}

func TestFindQuietGardeners_ReadyWaitingInProgressAndThreshold(t *testing.T) {
	got := findQuiet(t, newQuietFarm())
	for _, w := range []QuietGardener{
		{Plan: "gd-p1", Kind: QuietKindQuiet, Trigger: "gd-g1", ReadySteps: 1, Ready: []string{"gd-s1"}, QuietFor: 3 * time.Hour},
		{Plan: "gd-p2", Kind: QuietKindQuiet, Trigger: "gd-g2", ReadySteps: 4, Ready: []string{"gd-s2a", "gd-s2b", "gd-s2c"}, QuietFor: 5 * time.Hour},
		{Plan: "gd-p7", Kind: QuietKindTimed, Trigger: "gd-g7", Cron: "5,35 * * * *", QuietFor: 10 * time.Minute},
	} {
		g, ok := got[w.Plan]
		if !ok {
			t.Errorf("%s is not found", w.Plan)
			continue
		}
		if g.Kind != w.Kind || g.Trigger != w.Trigger || g.ReadySteps != w.ReadySteps || !slices.Equal(g.Ready, w.Ready) ||
			g.QuietFor != w.QuietFor || g.Cron != w.Cron || !g.Wakes() {
			t.Errorf("%s = %+v, want %+v", w.Plan, g, w)
		}
		delete(got, w.Plan)
	}
	// gd-p3 waits, gd-p4 is in progress, gd-p5 ran inside two hours, gd-p6
	// has a live cycle, gd-p8 is paused and gd-p9 is a draft.
	for id, g := range got {
		t.Errorf("%s is found (%+v), but it is fine", id, g)
	}
}

func TestFindQuietGardeners_OutcomeAndLastRun(t *testing.T) {
	q := findQuiet(t, newQuietFarm())["gd-p1"]
	if q.LastOutcome != "no source matches (out of window / preflight not met / already fired)" {
		t.Errorf("LastOutcome = %q, want its first line", q.LastOutcome)
	}
	if !q.LastRun.Equal(quietAt.Add(-3*time.Hour)) || !q.Since.Equal(q.LastRun) {
		t.Errorf("LastRun %v, Since %v", q.LastRun, q.Since)
	}
}

func TestFindQuietGardeners_ThresholdIsTwoHours(t *testing.T) {
	for _, c := range []struct {
		ago  time.Duration
		want bool
	}{{QuietGardenerAfter - time.Second, false}, {QuietGardenerAfter, true}, {QuietGardenerAfter + time.Hour, true}} {
		f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
		f.plan("gd-p1", "building")
		f.gardener("gd-g1", "gd-p1", "event", c.ago, nil)
		f.step("gd-s1", "gd-p1", "task", "open", nil)
		if _, quiet := findQuiet(t, f)["gd-p1"]; quiet != c.want {
			t.Errorf("no run for %v: quiet = %v, want %v", c.ago, quiet, c.want)
		}
	}
}

// A gardener that never ran is quiet two hours after it was bound.
func TestFindQuietGardeners_NeverRanCountsFromItsBind(t *testing.T) {
	for _, c := range []struct {
		bound time.Duration
		want  bool
	}{{30 * time.Minute, false}, {3 * time.Hour, true}} {
		f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
		f.plan("gd-p1", "building")
		f.gardener("gd-g1", "gd-p1", "event", 0, nil).CreatedAt = quietAt.Add(-c.bound)
		f.step("gd-s1", "gd-p1", "task", "open", nil)
		q, quiet := findQuiet(t, f)["gd-p1"]
		if quiet != c.want {
			t.Errorf("bound %v ago, never ran: quiet = %v, want %v", c.bound, quiet, c.want)
		}
		if quiet && (!q.LastRun.IsZero() || q.QuietFor != c.bound) {
			t.Errorf("never ran: %+v", q)
		}
	}
}

// A timed gardener is a regression whatever its plan holds, and whether it
// ran a minute ago.
func TestFindQuietGardeners_TimedIsARegression(t *testing.T) {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	f.plan("gd-p1", "building")
	f.gardener("gd-g1", "gd-p1", "timed", time.Minute, map[string]string{"cron": "7,37 * * * *"})
	f.step("gd-s1", "gd-p1", "task", "open", nil)
	q, ok := findQuiet(t, f)["gd-p1"]
	if !ok || q.Kind != QuietKindTimed || q.Cron != "7,37 * * * *" || q.ReadySteps != 0 {
		t.Fatalf("timed gardener: %+v, %v", q, ok)
	}
}

// The newest open, enabled gardener of a plan is the one judged.
func TestFindQuietGardeners_NewestLiveGardener(t *testing.T) {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	f.plan("gd-p1", "building")
	f.gardener("gd-old", "gd-p1", "timed", time.Minute, nil)
	f.gardener("gd-new", "gd-p1", "event", 3*time.Hour, nil).CreatedAt = quietAt.Add(-24 * time.Hour)
	f.step("gd-s1", "gd-p1", "task", "open", nil)
	if q := findQuiet(t, f)["gd-p1"]; q.Trigger != "gd-new" || q.Kind != QuietKindQuiet {
		t.Fatalf("judged %+v, want the newer gd-new", q)
	}
}

// A steward decision about the gardener filed since its last run raises the
// spell: the plan is still listed but wakes nobody. One filed before that
// run, the same day, makes this the second time.
func TestFindQuietGardeners_StewardDecisions(t *testing.T) {
	f := newQuietFarm()
	f.decision("gd-d1", "gd-g1", "gardenio-steward-ab12", time.Hour)    // after gd-g1's run 3h ago
	f.decision("gd-d2", "gd-g2", "gardenio-steward-cd34", 6*time.Hour)  // before gd-g2's run 5h ago
	f.decision("gd-d3", "gd-g2", "gardenio-steward-ef56", 30*time.Hour) // outside the window
	f.decision("gd-d4", "gd-g2", "gardenio-fix-x1y", time.Hour)         // not a steward run
	f.decision("gd-d5", "gd-g2", "tomatoes-steward-gh78", time.Hour)    // another land's steward
	f.decision("gd-d6", "gd-g7", "gardenio-steward-ij90", 20*time.Hour) // timed: any in the window
	f.beads[len(f.beads)-1].Status = "closed"                           // answered
	got := findQuiet(t, f)
	if q := got["gd-p1"]; q.Raised != "gd-d1" || q.Wakes() || q.AskedBefore() {
		t.Errorf("gd-p1 = %+v, want raised by gd-d1", q)
	}
	if q := got["gd-p2"]; q.Raised != "" || !q.Wakes() || !q.AskedBefore() || len(q.Asked) != 1 || q.Asked[0].ID != "gd-d2" {
		t.Errorf("gd-p2 = %+v, want asked once before today, not raised", q)
	}
	if q := got["gd-p7"]; q.Raised != "gd-d6" || q.Wakes() {
		t.Errorf("gd-p7 = %+v, want raised by the answered gd-d6", q)
	}
}

func TestFindQuietGardeners_ReadErrorFailsTheCheck(t *testing.T) {
	for name, fail := range map[string]func(beadsapi.ListBeadsQuery) bool{
		"gardeners": func(q beadsapi.ListBeadsQuery) bool { return slices.Contains(q.Types, "trigger") },
		"live":      func(q beadsapi.ListBeadsQuery) bool { return slices.Contains(q.Types, "agent") },
		"ready":     func(q beadsapi.ListBeadsQuery) bool { return q.NoOpenDeps },
		"decisions": func(q beadsapi.ListBeadsQuery) bool { return slices.Contains(q.Types, "decision") },
	} {
		t.Run(name, func(t *testing.T) {
			f := newQuietFarm()
			plans, err := ListQuietPlans(context.Background(), f, "gardenio")
			if err != nil {
				t.Fatal(err)
			}
			f.fail = fail
			if got, err := FindQuietGardeners(context.Background(), f, "gardenio", plans, quietAt); err == nil {
				t.Fatalf("a failed %s read gave %+v, want an error", name, got)
			}
		})
	}
}

// A last_run that cannot be read leaves the plan out rather than calling it
// quiet.
func TestFindQuietGardeners_UnreadableLastRun(t *testing.T) {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	f.plan("gd-p1", "building")
	f.gardener("gd-g1", "gd-p1", "event", 0, map[string]string{"last_run": "yesterday"})
	f.step("gd-s1", "gd-p1", "task", "open", nil)
	if q, ok := findQuiet(t, f)["gd-p1"]; ok {
		t.Fatalf("unreadable last_run: %+v", q)
	}
}

// The labels mirrored here must stay the ones the gardener packages write.
func TestQuietLabelsMirrorTheGardenerPackages(t *testing.T) {
	if quietPlanGardenerMarker != "plan-gardener" || quietEpicLabelPrefix != "gardener-epic:" ||
		quietTimedLabel != "trigger:schedule" || quietEventLabel != "trigger:multi" || quietStewardRole != "steward" {
		t.Fatal("a mirrored label changed; update the steward's lockstep test too")
	}
}

func resetQuietMemo(t *testing.T, now *time.Time) {
	t.Helper()
	quietMemoMu.Lock()
	quietMemo = map[quietMemoKey]quietMemoEntry{}
	quietMemoMu.Unlock()
	prev := quietNow
	quietNow = func() time.Time { return *now }
	t.Cleanup(func() {
		quietNow = prev
		quietMemoMu.Lock()
		quietMemo = map[quietMemoKey]quietMemoEntry{}
		quietMemoMu.Unlock()
	})
}

func quietLeaf() Expression {
	return Expression{Leaf: Leaf{Type: "quiet_gardeners", Op: ">", Value: "0", Project: "gardenio"}}
}

func TestQuietGardenersLeaf_TrueOnlyWhenAPlanWakes(t *testing.T) {
	now := quietAt
	resetQuietMemo(t, &now)
	quiet := newQuietFarm()
	if ok, err := Evaluate(context.Background(), quietLeaf(), Env{Bead: quiet}); err != nil || !ok {
		t.Fatalf("quiet farm: %v, %v; want true", ok, err)
	}

	resetQuietMemo(t, &now)
	healthy := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
	healthy.plan("gd-p1", "building")
	healthy.gardener("gd-g1", "gd-p1", "event", 30*time.Minute, nil)
	healthy.step("gd-s1", "gd-p1", "task", "open", nil)
	healthy.plan("gd-p2", "building")
	healthy.gardener("gd-g2", "gd-p2", "event", 9*time.Hour, nil)
	if ok, err := Evaluate(context.Background(), quietLeaf(), Env{Bead: healthy}); err != nil || ok {
		t.Fatalf("healthy farm: %v, %v; want false", ok, err)
	}

	// Once the steward has asked about each, nothing wakes it.
	resetQuietMemo(t, &now)
	quiet.decision("gd-d1", "gd-g1", "gardenio-steward-a", time.Minute)
	quiet.decision("gd-d2", "gd-g2", "gardenio-steward-a", time.Minute)
	quiet.decision("gd-d7", "gd-g7", "gardenio-steward-a", time.Minute)
	quiet.comment("gd-p8", "gardenio-steward-a", "steward: told the owner it is paused"+GardenerlessSeenMarker, time.Minute)
	if ok, err := Evaluate(context.Background(), quietLeaf(), Env{Bead: quiet}); err != nil || ok {
		t.Fatalf("every plan raised: %v, %v; want false", ok, err)
	}
}

func TestQuietGardenersLeaf_ProjectFromEnv(t *testing.T) {
	now := quietAt
	resetQuietMemo(t, &now)
	l := quietLeaf()
	l.Project = ""
	if ok, err := Evaluate(context.Background(), l, Env{Bead: newQuietFarm(), DefaultProject: "gardenio"}); err != nil || !ok {
		t.Fatalf("default project: %v, %v", ok, err)
	}
	resetQuietMemo(t, &now)
	if _, err := Evaluate(context.Background(), l, Env{Bead: newQuietFarm()}); err == nil {
		t.Fatal("no project at all: want an error")
	}
}

// A read error is an error, never a wake, and it survives a not().
func TestQuietGardenersLeaf_FailsSafe(t *testing.T) {
	now := quietAt
	resetQuietMemo(t, &now)
	f := newQuietFarm()
	f.fail = func(beadsapi.ListBeadsQuery) bool { return true }
	if ok, err := Evaluate(context.Background(), quietLeaf(), Env{Bead: f}); err == nil || ok {
		t.Fatalf("read error: %v, %v; want false and an error", ok, err)
	}
	inner := quietLeaf()
	if ok, err := Evaluate(context.Background(), Expression{Not: &inner}, Env{Bead: f}); err == nil || ok {
		t.Fatalf("not(read error): %v, %v; want false and an error", ok, err)
	}
	if !strings.Contains(fmtErr(Evaluate(context.Background(), quietLeaf(), Env{Bead: f})), "quiet_gardeners") {
		t.Fatal("the error does not name the leaf")
	}
}

func fmtErr(_ bool, err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// The engine evaluates a due cron's preflight every tick; the count is read
// at most once per quietMemoTTL, and an error is never reused.
func TestQuietGardenersLeaf_Memo(t *testing.T) {
	now := quietAt
	resetQuietMemo(t, &now)
	f := newQuietFarm()
	if ok, _ := Evaluate(context.Background(), quietLeaf(), Env{Bead: f}); !ok {
		t.Fatal("want true")
	}
	n := f.queries
	now = now.Add(quietMemoTTL - time.Second)
	if ok, _ := Evaluate(context.Background(), quietLeaf(), Env{Bead: f}); !ok || f.queries != n {
		t.Fatalf("inside the memo: %v, %d new queries", ok, f.queries-n)
	}
	now = now.Add(2 * time.Second)
	f.fail = func(beadsapi.ListBeadsQuery) bool { return true }
	if ok, err := Evaluate(context.Background(), quietLeaf(), Env{Bead: f}); ok || err == nil {
		t.Fatalf("past the memo with a failing daemon: %v, %v", ok, err)
	}
	f.fail = nil
	n = f.queries
	if ok, _ := Evaluate(context.Background(), quietLeaf(), Env{Bead: f}); !ok || f.queries == n {
		t.Fatal("an error was memoized")
	}
}

// nudgeEntries is a triggers[] carrying the steward-nudge entry.
const nudgeEntries = `[{"kind":"bead_event","label":"plan-activity"},{"kind":"bead_event","label":"steward-nudge"},{"kind":"cron","label":"fallback-watchdog"}]`

func (f *quietFarm) comment(plan, author, text string, ago time.Duration) {
	if f.comments == nil {
		f.comments = map[string][]beadsapi.Comment{}
	}
	f.comments[plan] = append(f.comments[plan], beadsapi.Comment{Author: author, Text: text, CreatedAt: quietAt.Add(-ago)})
}

func TestQuietNudgeText(t *testing.T) {
	if got := QuietNudgeText("gd-g1", 3*time.Hour+40*time.Minute, 2); got != "steward: gardener gd-g1 quiet 3h with 2 ready steps; nudging" {
		t.Errorf("text = %q", got)
	}
	if got := QuietNudgeText("gd-g1", 2*time.Hour, 1); got != "steward: gardener gd-g1 quiet 2h with 1 ready step; nudging" {
		t.Errorf("text = %q", got)
	}
	if !strings.HasSuffix(QuietNudgeText("x", 0, 0), QuietNudgeMarker) || StewardRunPrefix("gardenio") != "gardenio-steward-" {
		t.Error("the nudge text must end with the marker the gardener's entry matches")
	}
}

// A quiet gardener is nudged first; the nudge holds the wake off for two
// hours, as a decision would; one still quiet after that wakes the steward
// again. Only the land's steward's nudge in this spell counts.
func TestFindQuietGardeners_Nudges(t *testing.T) {
	nudge := QuietNudgeText("gd-g", 3*time.Hour, 1)
	cases := []struct {
		name      string
		nudgeable bool
		edit      func(f *quietFarm)
		needs     bool
		pending   bool
		wakes     bool
	}{
		{"no steward-nudge entry: straight to the owner", false, nil, false, false, true},
		{"not yet nudged", true, nil, true, false, true},
		{"nudged an hour ago", true, func(f *quietFarm) { f.comment("gd-p", "gardenio-steward-ab12", nudge, time.Hour) }, false, true, false},
		{"nudged two hours ago, still quiet", true, func(f *quietFarm) { f.comment("gd-p", "gardenio-steward-ab12", nudge, 2*time.Hour+time.Minute) }, false, false, true},
		{"nudged before the gardener last ran", true, func(f *quietFarm) { f.comment("gd-p", "gardenio-steward-ab12", nudge, 4*time.Hour) }, true, false, true},
		{"a crew comment is no nudge", true, func(f *quietFarm) { f.comment("gd-p", "gardenio-fix-it-x1z", nudge, time.Hour) }, true, false, true},
		{"a steward note is no nudge", true, func(f *quietFarm) { f.comment("gd-p", "gardenio-steward-ab12", "steward: asked the owner", time.Hour) }, true, false, true},
		{"another land's steward", true, func(f *quietFarm) { f.comment("gd-p", "tenant-steward-ab12", nudge, time.Hour) }, true, false, true},
		{"a decision raises it", true, func(f *quietFarm) { f.decision("gd-d", "gd-g", "gardenio-steward-ab12", time.Hour) }, false, false, false},
	}
	for _, tc := range cases {
		f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
		f.plan("gd-p", "building")
		fields := map[string]string{}
		if tc.nudgeable {
			fields["triggers"] = nudgeEntries
		}
		f.gardener("gd-g", "gd-p", "event", 3*time.Hour, fields)
		f.step("gd-s", "gd-p", "task", "open", nil)
		if tc.edit != nil {
			tc.edit(f)
		}
		got, err := FindQuietGardeners(context.Background(), f, "gardenio", []QuietPlanRef{{ID: "gd-p"}}, quietAt)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v %+v", tc.name, err, got)
		}
		q := got[0]
		if q.Nudgeable != tc.nudgeable || q.NeedsNudge() != tc.needs || q.NudgePending() != tc.pending || q.Wakes() != tc.wakes {
			t.Errorf("%s: nudgeable=%v needs=%v pending=%v wakes=%v (nudged %v), want %v/%v/%v/%v",
				tc.name, q.Nudgeable, q.NeedsNudge(), q.NudgePending(), q.Wakes(), q.Nudged, tc.nudgeable, tc.needs, tc.pending, tc.wakes)
		}
	}
}

// listOnly is a daemon client that cannot read comments.
type listOnly struct{ f *quietFarm }

func (l listOnly) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return l.f.ListBeadsFiltered(ctx, q)
}

// A nudge that cannot be read fails the check, so it wakes nobody.
func TestFindQuietGardeners_NudgeReadFailsSafe(t *testing.T) {
	f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}, failComments: true}
	f.plan("gd-p", "building")
	f.gardener("gd-g", "gd-p", "event", 3*time.Hour, map[string]string{"triggers": nudgeEntries})
	f.step("gd-s", "gd-p", "task", "open", nil)
	if _, err := FindQuietGardeners(context.Background(), f, "gardenio", []QuietPlanRef{{ID: "gd-p"}}, quietAt); err == nil {
		t.Error("a failed comment read was not an error")
	}
	f.failComments = false
	if _, err := FindQuietGardeners(context.Background(), listOnly{f}, "gardenio", []QuietPlanRef{{ID: "gd-p"}}, quietAt); err == nil {
		t.Error("a client that cannot read comments was not an error")
	}
}

// At most QuietNudgeCap nudges a plan in a rolling day: a gardener that ran
// after each of them while steps ready before the first are still ready is
// capped, its passed-over steps named, and one decision filed after the cap
// was reached raises it.
func TestFindQuietGardeners_NudgeCap(t *testing.T) {
	nudge := QuietNudgeText("gd-g", 3*time.Hour, 1)
	type want struct {
		capped, needs, wakes bool
		passed               []string
	}
	cases := []struct {
		name string
		edit func(f *quietFarm)
		want want
	}{
		{"two nudges, both answered: capped, the old step named", func(f *quietFarm) {
			f.comment("gd-p", "gardenio-steward-ab12", nudge, 10*time.Hour)
			f.comment("gd-p", "gardenio-steward-cd34", nudge, 6*time.Hour)
		}, want{true, false, true, []string{"gd-old"}}},
		{"capped and a decision since the second nudge", func(f *quietFarm) {
			f.comment("gd-p", "gardenio-steward-ab12", nudge, 10*time.Hour)
			f.comment("gd-p", "gardenio-steward-cd34", nudge, 6*time.Hour)
			f.decision("gd-d", "gd-g", "gardenio-steward-ef56", 5*time.Hour)
		}, want{true, false, false, []string{"gd-old"}}},
		{"capped, a decision only between the nudges", func(f *quietFarm) {
			f.comment("gd-p", "gardenio-steward-ab12", nudge, 10*time.Hour)
			f.comment("gd-p", "gardenio-steward-cd34", nudge, 6*time.Hour)
			f.decision("gd-d", "gd-g", "gardenio-steward-ef56", 8*time.Hour)
		}, want{true, false, true, []string{"gd-old"}}},
		{"one nudge a day ago and one today: not capped", func(f *quietFarm) {
			f.comment("gd-p", "gardenio-steward-ab12", nudge, 25*time.Hour)
			f.comment("gd-p", "gardenio-steward-cd34", nudge, 6*time.Hour)
		}, want{false, true, true, nil}},
		{"a crew comment is not a nudge", func(f *quietFarm) {
			f.comment("gd-p", "gardenio-fix-it-x1z", nudge, 10*time.Hour)
			f.comment("gd-p", "gardenio-steward-cd34", nudge, 6*time.Hour)
		}, want{false, true, true, nil}},
		{"capped, only steps that came after the first nudge", func(f *quietFarm) {
			f.comment("gd-p", "gardenio-steward-ab12", nudge, 19*time.Hour)
			f.comment("gd-p", "gardenio-steward-cd34", nudge, 6*time.Hour)
			for _, b := range f.beads {
				if b.ID == "gd-old" {
					b.CreatedAt = quietAt.Add(-12 * time.Hour)
				}
			}
		}, want{true, false, true, nil}},
	}
	for _, tc := range cases {
		f := &quietFarm{parent: map[string]string{}, waiting: map[string]bool{}}
		f.plan("gd-p", "building")
		f.gardener("gd-g", "gd-p", "event", 3*time.Hour, map[string]string{"triggers": nudgeEntries})
		f.step("gd-old", "gd-p", "task", "open", func(b *beadsapi.BeadDetail) { b.CreatedAt = quietAt.Add(-20 * time.Hour) })
		f.step("gd-new", "gd-p", "task", "open", func(b *beadsapi.BeadDetail) { b.CreatedAt = quietAt.Add(-time.Hour) })
		tc.edit(f)
		got, err := FindQuietGardeners(context.Background(), f, "gardenio", []QuietPlanRef{{ID: "gd-p"}}, quietAt)
		if err != nil || len(got) != 1 {
			t.Fatalf("%s: %v %+v", tc.name, err, got)
		}
		q := got[0]
		if q.NudgeCapped() != tc.want.capped || q.NeedsNudge() != tc.want.needs || q.Wakes() != tc.want.wakes || !slices.Equal(q.Passed, tc.want.passed) {
			t.Errorf("%s: capped=%v needs=%v wakes=%v passed=%v (nudges %v, raised %q), want %+v",
				tc.name, q.NudgeCapped(), q.NeedsNudge(), q.Wakes(), q.Passed, q.Nudges, q.Raised, tc.want)
		}
	}
}
