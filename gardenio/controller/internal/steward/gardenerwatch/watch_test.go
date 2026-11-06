package gardenerwatch

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// The home steward watches its plans' gardeners (gd-bo2RUmtpHb): a building
// plan whose event-mode gardener has not run for two hours while a step is
// ready, or whose gardener is still timed, is listed every wake with the ask
// to the owner, and wakes the steward through the quiet-gardeners watch.
// The steward's side (LoadLand, the prompt, its wakes) is tested in package
// steward.

var update = flag.Bool("update", false, "rewrite the golden files")

const home = "gardenio"

var quietNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// farm is a small daemon: its beads, each bead's parent-child parent (for a
// Parent query), the beads with an open blocking dep (left out of a
// NoOpenDeps one) and each bead's comments.
type farm struct {
	beads    []*beadsapi.BeadDetail
	parent   map[string]string
	waiting  map[string]bool
	comments map[string][]beadsapi.Comment
}

func (d *farm) GetComments(_ context.Context, id string) ([]beadsapi.Comment, error) {
	return d.comments[id], nil
}

// under reports whether id is in root's subtree, root excluded.
func (d *farm) under(id, root string) bool {
	for p := d.parent[id]; p != ""; p = d.parent[p] {
		if p == root {
			return true
		}
	}
	return false
}

func (d *farm) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	var out []*beadsapi.BeadDetail
	if q.Offset > 0 {
		return &beadsapi.ListBeadsResult{}, nil
	}
	for _, b := range d.beads {
		if (len(q.Types) == 0 || slices.Contains(q.Types, b.Type)) &&
			(len(q.Statuses) == 0 || slices.Contains(q.Statuses, b.Status)) &&
			!slices.ContainsFunc(q.Labels, func(l string) bool { return !slices.Contains(b.Labels, l) }) &&
			(q.Parent == "" || d.under(b.ID, q.Parent)) && (!q.NoOpenDeps || !d.waiting[b.ID]) &&
			(!q.Unassigned || b.Assignee == "") && fieldsMatch(b, q.Fields) {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

// fieldsMatch is the daemon's field_filter: each key equal to its value.
func fieldsMatch(b *beadsapi.BeadDetail, want map[string]string) bool {
	for k, v := range want {
		if b.Fields[k] != v {
			return false
		}
	}
	return true
}

// refs are the farm's building plans, as the steward hands them to Find
// and Stalled.
func (d *farm) refs() []conditions.QuietPlanRef {
	var out []conditions.QuietPlanRef
	for _, b := range d.beads {
		if b.Type == "epic" && slices.Contains(b.Labels, plangardener.PlanLabel) && b.Fields["plan_state"] == "building" {
			out = append(out, conditions.QuietPlanRef{ID: b.ID, Title: b.Title})
		}
	}
	return out
}

func homePlan(id, title string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "epic", Title: title, Status: "open",
		Labels: []string{"project:" + home, "plan"}, Fields: map[string]string{"plan_state": "building"},
	}
}

func homeStep(d *farm, id, plan, status string, waiting bool) {
	d.beads = append(d.beads, &beadsapi.BeadDetail{
		ID: id, Type: "task", Title: "step " + id, Status: status,
		Labels: []string{"project:" + home},
	})
	d.parent[id] = plan
	if waiting {
		d.waiting[id] = true
	}
}

// eventGardener is a bound event-mode plan gardener that last ran lastRun
// before at (0: never), bound three days before at.
func eventGardener(id, plan string, at time.Time, lastRun time.Duration) *beadsapi.BeadDetail {
	f := map[string]string{"enabled": "true", "last_outcome": "no source matches (out of window / preflight not met / already fired)"}
	if lastRun > 0 {
		f["last_run"] = at.Add(-lastRun).Format(time.RFC3339)
	}
	return &beadsapi.BeadDetail{
		ID: id, Type: "trigger", Title: "gardener " + plan, Status: "open", CreatedAt: at.Add(-72 * time.Hour),
		Labels: []string{"trigger:multi", "role:gardener", plangardener.GardenerEpicLabel(plan), plangardener.MarkerLabel}, Fields: f,
	}
}

// nudgeableGardener is eventGardener carrying the steward-nudge entry, as
// gb gardener mode --event writes it.
func nudgeableGardener(id, plan string, at time.Time, lastRun time.Duration) *beadsapi.BeadDetail {
	g := eventGardener(id, plan, at, lastRun)
	raw, _ := json.Marshal(gardenermode.EventTriggers(plan, home, "5 7 * * *", "UTC", nil))
	g.Fields["triggers"] = string(raw)
	return g
}

// nudge records a steward nudge comment on plan ago before at.
func nudge(d *farm, plan, trigger string, at time.Time, ago time.Duration) {
	d.comments[plan] = append(d.comments[plan], beadsapi.Comment{
		ID: int64(len(d.comments) + 1), Author: home + "-steward-cd34", CreatedAt: at.Add(-ago),
		Text: conditions.QuietNudgeText(trigger, 3*time.Hour, 1),
	})
}

func stewardDecision(id, trigger string, at time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Type: "decision", Status: "open", CreatedBy: home + "-steward-ab12", CreatedAt: at,
		Fields: map[string]string{"subject_trigger_id": trigger},
	}
}

// quietFarm is the home garden at at with a plan of each kind: quiet with no
// steward-nudge wake, the same a second time today, already asked, still
// timed, waiting, quiet and to be nudged, nudged an hour ago, still quiet
// two and a half hours after a nudge, and quiet again after answering two
// nudges today with the same step ready (the nudge cap). gd-plan has no
// gardener at all.
func quietFarm(at time.Time) *farm {
	d := &farm{parent: map[string]string{}, waiting: map[string]bool{}, comments: map[string][]beadsapi.Comment{}}
	timed := eventGardener("gd-qg4", "gd-q4", at, 10*time.Minute)
	timed.Labels[0] = "trigger:schedule"
	timed.Fields["cron"] = "5,35 * * * *"
	d.beads = append(d.beads, homePlan("gd-plan", "Ship the steward"),
		homePlan("gd-q1", "Quiet one"), eventGardener("gd-qg1", "gd-q1", at, 3*time.Hour),
		homePlan("gd-q2", "Quiet again"), eventGardener("gd-qg2", "gd-q2", at, 3*time.Hour),
		stewardDecision("gd-qd2", "gd-qg2", at.Add(-5*time.Hour)),
		homePlan("gd-q3", "Already asked"), eventGardener("gd-qg3", "gd-q3", at, 4*time.Hour),
		stewardDecision("gd-qd3", "gd-qg3", at.Add(-time.Hour)),
		homePlan("gd-q4", "Still timed"), timed,
		homePlan("gd-q5", "Waiting"), eventGardener("gd-qg5", "gd-q5", at, 4*time.Hour),
		homePlan("gd-q6", "Nudge it"), nudgeableGardener("gd-qg6", "gd-q6", at, 3*time.Hour),
		homePlan("gd-q7", "Nudged"), nudgeableGardener("gd-qg7", "gd-q7", at, 3*time.Hour),
		homePlan("gd-q8", "Nudged, still quiet"), nudgeableGardener("gd-qg8", "gd-q8", at, 5*time.Hour),
	)
	nudge(d, "gd-q7", "gd-qg7", at, time.Hour)
	nudge(d, "gd-q8", "gd-qg8", at, 150*time.Minute)
	d.beads = append(d.beads, homePlan("gd-q9", "Passed over"), nudgeableGardener("gd-qg9", "gd-q9", at, 3*time.Hour))
	nudge(d, "gd-q9", "gd-qg9", at, 10*time.Hour)
	nudge(d, "gd-q9", "gd-qg9", at, 6*time.Hour)
	for _, s := range [][2]string{
		{"gd-qs9", "gd-q9"},
		{"gd-qs6", "gd-q6"},
		{"gd-qs7", "gd-q7"},
		{"gd-qs8", "gd-q8"},
		{"gd-qs1a", "gd-q1"},
		{"gd-qs1b", "gd-q1"},
		{"gd-qs2", "gd-q2"},
		{"gd-qs3", "gd-q3"},
	} {
		homeStep(d, s[0], s[1], "open", false)
	}
	homeStep(d, "gd-qs5a", "gd-q5", "open", true)
	homeStep(d, "gd-qs5b", "gd-q5", "in_progress", false)
	return d
}

func TestFind_QuietPlans(t *testing.T) {
	d := quietFarm(quietNow)
	found, more, err := Find(context.Background(), d, home, d.refs(), quietNow)
	if err != nil || more != 0 {
		t.Fatalf("Find: %v (more %d)", err, more)
	}
	got := map[string]Plan{}
	for _, q := range found {
		got[q.Plan] = q
	}
	for _, w := range []struct {
		plan, trigger string
		kind          conditions.QuietGardenerKind
		ask           Ask
		fix           string
		ready         int
	}{
		{"gd-q1", "gd-qg1", conditions.QuietKindQuiet, AskFire, "gb gardener fire-now gd-qg1", 2},
		{"gd-q2", "gd-qg2", conditions.QuietKindQuiet, AskFireAgain, "gb gardener fire-now gd-qg2", 1},
		{"gd-q3", "gd-qg3", conditions.QuietKindQuiet, AskAsked, "gb gardener fire-now gd-qg3", 1},
		{"gd-q4", "gd-qg4", conditions.QuietKindTimed, AskEventMode, `gb gardener mode gd-qg4 --event --fallback-cron "5 `, 0},
		{"gd-q6", "gd-qg6", conditions.QuietKindQuiet, AskNudge, "gb gardener fire-now gd-qg6", 1},
		{"gd-q7", "gd-qg7", conditions.QuietKindQuiet, AskNudged, "gb gardener fire-now gd-qg7", 1},
		{"gd-q8", "gd-qg8", conditions.QuietKindQuiet, AskFire, "gb gardener fire-now gd-qg8", 1},
		{"gd-q9", "gd-qg9", conditions.QuietKindQuiet, AskPassed, "gb trigger disable gd-qg9", 1},
	} {
		q, ok := got[w.plan]
		if !ok {
			t.Errorf("%s is not listed", w.plan)
			continue
		}
		if q.Trigger != w.trigger || q.Kind != w.kind || q.Ask != w.ask || !strings.HasPrefix(q.Fix, w.fix) || q.ReadySteps != w.ready {
			t.Errorf("%s = %+v (ask %s, fix %q), want %+v", w.plan, q.QuietGardener, q.Ask, q.Fix, w)
		}
		wantNudge := ""
		if w.ask == AskNudge {
			wantNudge = `kd comment add gd-q6 "steward: gardener gd-qg6 quiet 3h with 1 ready step; nudging"`
		}
		if q.Nudge != wantNudge {
			t.Errorf("%s nudge = %q, want %q", w.plan, q.Nudge, wantNudge)
		}
		delete(got, w.plan)
	}
	for id := range got {
		t.Errorf("%s is listed, but its gardener is fine, it waits or it has none", id)
	}
}

// Each plan's line in the steward's prompt, under "Plan gardeners to see to".
func TestDescribe_QuietPlansGolden(t *testing.T) {
	d := quietFarm(quietNow)
	found, _, err := Find(context.Background(), d, home, d.refs(), quietNow)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	var b strings.Builder
	for _, q := range found {
		b.WriteString("- " + Describe(q) + "\n")
	}
	checkGolden(t, "quiet_plans.golden", b.String())
}

func TestFallbackCron_KeepsTheMinuteOnceADay(t *testing.T) {
	for _, c := range []struct{ cron, minute string }{{"5,35 * * * *", "5 "}, {"17 * * * *", "17 "}} {
		got := FallbackCron("gd-qg4", c.cron)
		if !strings.HasPrefix(got, c.minute) || !strings.HasSuffix(got, " * * *") || got != FallbackCron("gd-qg4", c.cron) {
			t.Errorf("FallbackCron(%q) = %q", c.cron, got)
		}
		expr, err := scheduler.ParseCron(got)
		if err != nil {
			t.Fatalf("ParseCron(%q): %v", got, err)
		}
		hours := 0
		for h := range 24 {
			if expr.InWindow(time.Date(2026, 11, 2, h, 0, 0, 0, time.UTC)) {
				hours++
			}
		}
		if hours != 1 {
			t.Errorf("%q fires in %d hours a day, want 1", got, hours)
		}
	}
	if got := FallbackCron("gd-qg4", ""); len(strings.Fields(got)) != 5 {
		t.Errorf("no cron: %q", got)
	}
}

// failReady fails the watch's ready-step read only.
type failReady struct{ *farm }

func (f failReady) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.Unassigned {
		return nil, errors.New("daemon unavailable")
	}
	return f.farm.ListBeadsFiltered(ctx, q)
}

func TestEntry_EveryTwoHours(t *testing.T) {
	e := Entry(home, "UTC")
	if e.Kind != scheduler.TriggerKindCron || e.Label != WakeLabel || e.Cron != WatchCron(home) || e.Timezone != "UTC" || e.Preflight == nil {
		t.Fatalf("entry = %+v", e)
	}
	if l := e.Preflight.Leaf; l.Type != conditions.QuietGardenersLeaf || l.Op != ">" || l.Value != "0" || l.Project != home || e.Preflight.IsComposition() {
		t.Fatalf("preflight = %+v, want quiet_gardeners > 0 on %s", *e.Preflight, home)
	}
	if !strings.HasSuffix(e.Cron, " */2 * * *") {
		t.Fatalf("cron %q is not every two hours", e.Cron)
	}
	// With the preflight holding, it fires once in each even hour.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	day := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	last, fires := day.Add(-time.Hour), 0
	for m := time.Duration(0); m < 24*time.Hour; m += time.Minute {
		tctx := scheduler.TriggerEvalContext{TriggerID: "steward", Timezone: "UTC", Triggers: []scheduler.TriggerEntry{e}, LastRun: last}
		if dec := scheduler.EvaluateCronEntry(tctx, e, 0, day.Add(m), logger); dec.Fire {
			fires++
			last = day.Add(m)
		}
	}
	if fires != 12 {
		t.Fatalf("fired %d times in a day, want 12", fires)
	}
}

// ancestorFarm is the farm as trigger-engine's daemon: it also answers
// in_subtree_of, which the watch never asks.
type ancestorFarm struct{ conditions.BeadClient }

func (ancestorFarm) IsAncestor(context.Context, string, string) (bool, error) { return false, nil }

// GetComments passes the farm's comments through, as the daemon client does.
func (a ancestorFarm) GetComments(ctx context.Context, id string) ([]beadsapi.Comment, error) {
	return a.BeadClient.(conditions.CommentClient).GetComments(ctx, id)
}

// The preflight is what keeps the watch free: trigger-engine evaluates it,
// and the entry fires only while a plan's gardener is quiet or timed and not
// already asked about.
func TestEntry_PreflightGatesTheWake(t *testing.T) {
	e := Entry(home, "UTC")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	// An even hour, past the cron's minute, the steward last run before it.
	at := time.Date(2026, 11, 2, 12, 59, 0, 0, time.UTC)
	fires := func(d conditions.BeadClient) bool {
		tctx := scheduler.TriggerEvalContext{
			TriggerID: "steward", Project: home, Timezone: "UTC",
			Triggers: []scheduler.TriggerEntry{e}, LastRun: at.Add(-3 * time.Hour),
		}
		return len(scheduler.EvaluateTriggersAll(context.Background(), bridge.NewMultiTriggerSource(logger, nil).Engine(), ancestorFarm{d}, logger, tctx, at)) > 0
	}
	// The leaf reads the wall clock, so the farms are built at it.
	now := time.Now()
	if !fires(quietFarm(now)) {
		t.Error("quiet and timed plans did not wake the steward")
	}
	healthy := &farm{parent: map[string]string{}, waiting: map[string]bool{}}
	healthy.beads = append(healthy.beads, homePlan("gd-plan", "Ship the steward"), eventGardener("gd-g1", "gd-plan", now, 30*time.Minute))
	homeStep(healthy, "gd-s1", "gd-plan", "open", false)
	if fires(healthy) {
		t.Error("a gardener that ran 30 minutes ago woke the steward")
	}
	// gd-q6 nudged ago, every other plan already asked about. (Each check
	// gets its own farm: the leaf memoizes per daemon client.)
	actedOn := func(ago time.Duration) *farm {
		d := quietFarm(now)
		for _, g := range []string{"gd-qg1", "gd-qg2", "gd-qg4", "gd-qg8", "gd-qg9"} {
			d.beads = append(d.beads, stewardDecision("gd-d-"+g, g, now.Add(-time.Minute)))
		}
		nudge(d, "gd-q6", "gd-qg6", now, ago)
		return d
	}
	if fires(actedOn(time.Minute)) {
		t.Error("plans the steward already asked about or nudged woke it again")
	}
	if !fires(actedOn(2*time.Hour + time.Minute)) {
		t.Error("a gardener still quiet two hours after its nudge did not wake the steward")
	}
	// The capped plan wakes the steward until it files its one decision.
	capped := quietFarm(now)
	for _, g := range []string{"gd-qg1", "gd-qg2", "gd-qg4", "gd-qg8"} {
		capped.beads = append(capped.beads, stewardDecision("gd-d-"+g, g, now.Add(-time.Minute)))
	}
	nudge(capped, "gd-q6", "gd-qg6", now, time.Minute)
	if !fires(capped) {
		t.Error("a gardener past the nudge cap did not wake the steward")
	}
	if fires(failReady{quietFarm(now)}) {
		t.Error("a read error woke the steward")
	}
}
