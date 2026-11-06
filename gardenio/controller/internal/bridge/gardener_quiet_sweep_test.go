package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
)

// quietSweepNow is the fixed clock the quiet-gardener sweep tests start at.
var quietSweepNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// quietSweepEngine wires a TriggerEngine with the quiet-gardener sweep on, a
// registered multi source, a recording notifier and a mutable clock.
func quietSweepEngine(daemon *mockTriggerDaemon) (*TriggerEngine, *MultiTriggerSource, *recordingNotifier, *time.Time) {
	rec := &recordingNotifier{}
	now := quietSweepNow
	clk := &now
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:   daemon,
		Notifier: rec.notify,
		Logger:   slog.Default(),
		Clock:    func() time.Time { return *clk },
	})
	engine.EnableQuietGardenerSweep(true)
	multi := NewMultiTriggerSource(nil, nil)
	engine.RegisterSource(multi)
	return engine, multi, rec, clk
}

// quietPlanDaemon is a building plan gd-plan-1 with one open, unassigned step
// and its event-mode gardener gd-gard-1, last run lastRun.
func quietPlanDaemon(lastRun time.Time) *mockTriggerDaemon {
	return &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{
				ID: "gd-gard-1", Type: "trigger", Status: "open", Title: "Plan gardener: quiet plan",
				Labels: []string{"trigger:multi", "plan-gardener", "gardener-epic:gd-plan-1", "project:gardenio"},
				Fields: map[string]string{"enabled": "true", "last_run": lastRun.UTC().Format(time.RFC3339), "project": "gardenio"},
			},
			{
				ID: "gd-plan-1", Type: "epic", Status: "open", Title: "Quiet plan",
				Labels: []string{"plan", "project:gardenio"}, Fields: map[string]string{"plan_state": "building"},
			},
			{ID: "gd-step-1", Type: "task", Status: "open", Title: "ready step", Labels: []string{"project:gardenio"}},
		},
		parentOf: map[string]string{"gd-step-1": "gd-plan-1"},
	}
}

// wakeEvents is the plan-epic wakes buffered on the multi source.
func wakeEvents(multi *MultiTriggerSource) []string {
	var ids []string
	for _, ev := range multi.Engine().SnapshotEvents() {
		if ev.EventType == "updated" {
			ids = append(ids, ev.BeadID)
		}
	}
	return ids
}

func commentsOn(m *mockTriggerDaemon, id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.commentedIDs {
		if c == id {
			n++
		}
	}
	return n
}

// A gardener quiet for 3h while its plan has a ready, unassigned step is woken
// with an updated event on its plan epic, and the wake is noted on the plan.
func TestSweepQuietGardeners_WakesQuietGardener(t *testing.T) {
	daemon := quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
	engine, multi, _, _ := quietSweepEngine(daemon)

	engine.SweepQuietGardeners(context.Background())

	if got := wakeEvents(multi); !slices.Equal(got, []string{"gd-plan-1"}) {
		t.Fatalf("want one wake on gd-plan-1, got %v", got)
	}
	ev := multi.Engine().SnapshotEvents()[0]
	if ev.Payload["actor"] != quietGardenerSweepActor || ev.Payload["id"] != "gd-plan-1" {
		t.Errorf("wake payload must name the plan and the sweep as actor, got %v", ev.Payload)
	}
	if n := commentsOn(daemon, "gd-plan-1"); n != 1 {
		t.Errorf("want 1 note on the plan, got %d", n)
	}
	if escs := escalationsCreated(daemon); len(escs) != 0 {
		t.Errorf("a first wake files no escalation, got %d", len(escs))
	}
}

// ancestorTriggerDaemon adds IsAncestor to the mock, answering as
// beadsapi.Client does (a bead is in its own subtree), so the gardener's
// in_subtree_of match on its plan epic resolves.
type ancestorTriggerDaemon struct{ *mockTriggerDaemon }

func (d ancestorTriggerDaemon) IsAncestor(_ context.Context, root, child string) (bool, error) {
	if root == child {
		return true, nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isDescendantOf(child, root), nil
}

// quietGardenerThroughEvaluate wires the quiet plan's gardener with its real
// event-mode triggers[] (notify action) into an engine with the sweep on, and
// returns a cycle that runs Evaluate then the runner's DrainPending, as in
// cmd/trigger-engine, at the time *now holds.
func quietGardenerThroughEvaluate(t *testing.T) (daemon *mockTriggerDaemon, multi *MultiTriggerSource, rec *recordingNotifier, now *time.Time, cycle func()) {
	t.Helper()
	daemon = quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
	triggers, err := json.Marshal(gardenermode.EventTriggers("gd-plan-1", "gardenio", "", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	gard := daemon.beads[0]
	gard.Fields["action"] = string(TriggerActionNotify)
	gard.Fields["triggers"] = string(triggers)
	gard.Fields["last_checked"] = quietSweepNow.Add(-time.Minute).Format(time.RFC3339)

	rec = &recordingNotifier{}
	at := quietSweepNow
	now = &at
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon:   ancestorTriggerDaemon{daemon},
		Notifier: rec.notify,
		Logger:   slog.Default(),
		Clock:    func() time.Time { return *now },
	})
	engine.EnableQuietGardenerSweep(true)
	multi = NewMultiTriggerSource(slog.Default(), nil)
	multi.nowFunc = func() time.Time { return *now }
	engine.RegisterSource(multi)
	ctx := context.Background()
	cycle = func() {
		engine.Evaluate(ctx)
		multi.DrainPending()
	}
	return daemon, multi, rec, now, cycle
}

// The wake goes through Evaluate and the runner's DrainPending, as in
// cmd/trigger-engine: the gardener's plan-activity entry takes it into its
// 10m debounce in the cycle that injects it, and the gardener fires once the
// debounce settles (gd-yNp2s5Go85). The sweep used to run after the source
// passes, so the drain dropped the wake and the gardener never fired.
func TestSweepQuietGardeners_WakeFiresGardenerThroughEvaluate(t *testing.T) {
	daemon, _, rec, now, cycle := quietGardenerThroughEvaluate(t)

	cycle()
	if n := rec.count(); n != 0 {
		t.Fatalf("the wake arms the 10m debounce and fires nothing yet, got %d notifies", n)
	}
	if n := commentsOn(daemon, "gd-plan-1"); n != 1 {
		t.Fatalf("want the wake noted on the plan, got %d notes", n)
	}

	*now = quietSweepNow.Add(11 * time.Minute)
	cycle()
	if n := rec.count(); n != 1 {
		t.Fatalf("the plan gardener must fire once the debounce settles, got %d notifies", n)
	}
}

// A second sweep wake 2.5h after a fired first one fires the gardener again.
// Every wake is the same updated event on the plan epic; while bead-event
// keys had no occurrence part the seen set (24h) and the spawn idempotency
// lookup (forever) swallowed every wake after the first (gd-wRullKPlWJ).
func TestSweepQuietGardeners_SecondWakeFiresGardenerAgain(t *testing.T) {
	_, _, rec, now, cycle := quietGardenerThroughEvaluate(t)

	cycle()
	*now = quietSweepNow.Add(11 * time.Minute)
	cycle()
	if n := rec.count(); n != 1 {
		t.Fatalf("the first wake must fire the gardener, got %d notifies", n)
	}

	*now = quietSweepNow.Add(150 * time.Minute)
	cycle()
	*now = quietSweepNow.Add(161 * time.Minute)
	cycle()
	// The second wake also files the repeat escalation, which notifies once.
	if n := rec.count(); n != 3 {
		t.Fatalf("the second wake must fire the gardener again (want 3 notifies: 2 fires + the escalation notice), got %d", n)
	}
}

// The plan's own gardener does not wake itself: its update, create or close in
// the subtree misses plan-activity, while another agent's update fires it.
// With per-occurrence keys (gd-wRullKPlWJ) a self-wake would recur each time
// the debounce and throttle ran out.
func TestPlanActivity_GardenerOwnWritesDoNotWakeIt(t *testing.T) {
	daemon, multi, rec, now, cycle := quietGardenerThroughEvaluate(t)
	// Fresh gardener: the sweep has nothing to wake, so only the injected
	// events below can fire it.
	daemon.beads[0].Fields["last_run"] = quietSweepNow.Format(time.RFC3339)
	self := "gardenio-plan-gardener-for-gd-plan-1-ab12"

	step := func(actor string) *BeadEvent {
		return &BeadEvent{ID: "gd-step-1", Type: "task", Status: "open", Actor: actor}
	}
	multi.InjectBeadEvent("beads.bead.updated", step(self))
	multi.InjectBeadEvent("beads.bead.created", &BeadEvent{ID: "gd-step-1", Type: "task", Status: "open", CreatedBy: self})
	multi.InjectBeadEvent("beads.bead.closed", &BeadEvent{ID: "gd-step-1", Type: "task", Status: "closed", ClosedBy: self})
	cycle()
	*now = quietSweepNow.Add(11 * time.Minute)
	cycle()
	if n := rec.count(); n != 0 {
		t.Fatalf("the gardener's own writes must not wake it, got %d notifies", n)
	}

	multi.InjectBeadEvent("beads.bead.updated", step("gardenio-some-crew-x1y"))
	cycle()
	*now = quietSweepNow.Add(22 * time.Minute)
	cycle()
	if n := rec.count(); n != 1 {
		t.Fatalf("a crew update on a step must wake the gardener, got %d notifies", n)
	}
}

// Each case is one reason not to wake the gardener.
func TestSweepQuietGardeners_LeavesGardenerAlone(t *testing.T) {
	cases := map[string]func(m *mockTriggerDaemon){
		"ran within 2h": func(m *mockTriggerDaemon) {
			m.beads[0].Fields["last_run"] = quietSweepNow.Add(-90 * time.Minute).Format(time.RFC3339)
		},
		"timed gardener": func(m *mockTriggerDaemon) {
			m.beads[0].Labels = []string{"trigger:schedule", "plan-gardener", "gardener-epic:gd-plan-1"}
		},
		"not a plan gardener": func(m *mockTriggerDaemon) {
			m.beads[0].Labels = []string{"trigger:multi", "gardener-epic:gd-plan-1"}
		},
		"disabled": func(m *mockTriggerDaemon) { m.beads[0].Fields["enabled"] = "false" },
		"step assigned": func(m *mockTriggerDaemon) {
			m.beads[2].Assignee = "gardenio-crew-x"
		},
		"step in progress": func(m *mockTriggerDaemon) { m.beads[2].Status = "in_progress" },
		"step waits on the owner at a device": func(m *mockTriggerDaemon) {
			m.beads[2].Labels = append(m.beads[2].Labels, "owner-at-device")
		},
		"step waits on a decision": func(m *mockTriggerDaemon) {
			// The mock filters nothing on the flag, so model the daemon: it
			// projects BlockedOnDecision only when the query asks for it.
			m.beads[2].BlockedOnDecision = true
			m.listBeadsHook = func(_ int, q beadsapi.ListBeadsQuery) error {
				m.beads[2].BlockedOnDecision = q.WithBlockedOnDecision
				return nil
			}
		},
		"step parked behind a decision": func(m *mockTriggerDaemon) {
			m.beads[2].ParkedOnDecision = true
			m.listBeadsHook = func(_ int, q beadsapi.ListBeadsQuery) error {
				m.beads[2].ParkedOnDecision = q.WithParkedOnDecision
				return nil
			}
		},
		"step awaits its release": func(m *mockTriggerDaemon) {
			m.beads[2].Fields = map[string]string{beadsapi.AwaitsReleaseField: "true"}
		},
		"plan not building": func(m *mockTriggerDaemon) { m.beads[1].Fields["plan_state"] = "ready" },
		"project schedules off": func(m *mockTriggerDaemon) {
			m.beads[0].Fields["gardener"] = `{"assign_bead":"gd-plan-1"}`
			m.projects = map[string]beadsapi.ProjectInfo{"gardenio": {TriggersEnabled: true, SchedulesEnabled: false}}
		},
		"plan closed": func(m *mockTriggerDaemon) { m.beads[1].Status = "closed" },
		"live cycle": func(m *mockTriggerDaemon) {
			m.beads = append(m.beads, &beadsapi.BeadDetail{
				ID: "gd-agent-1", Type: "agent", Status: "open",
				Fields:    map[string]string{"spawned_by": "trigger:gd-gard-1"},
				CreatedAt: quietSweepNow.Add(-5 * time.Minute), UpdatedAt: quietSweepNow.Add(-time.Minute),
			})
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			daemon := quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
			mutate(daemon)
			engine, multi, _, _ := quietSweepEngine(daemon)

			engine.SweepQuietGardeners(context.Background())

			if got := wakeEvents(multi); len(got) != 0 {
				t.Errorf("want no wake, got %v", got)
			}
			if n := commentsOn(daemon, "gd-plan-1"); n != 0 {
				t.Errorf("want no note on the plan, got %d", n)
			}
		})
	}
}

// A plan with one step parked behind its release and one plain open step
// wakes its gardener and lists only the plain step (gd-fHyi9y1k5B).
func TestSweepQuietGardeners_SkipsStepAwaitingRelease(t *testing.T) {
	daemon := quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
	daemon.beads[2].Fields = map[string]string{beadsapi.AwaitsReleaseField: "true"}
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{ID: "gd-step-2", Type: "task", Status: "open", Title: "plain step"})
	daemon.parentOf["gd-step-2"] = "gd-plan-1"
	engine, multi, _, _ := quietSweepEngine(daemon)

	ready, err := engine.readyUnassignedSteps(context.Background(), "gd-plan-1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ready, []string{"gd-step-2"}) {
		t.Fatalf("want only gd-step-2 ready, got %v", ready)
	}
	engine.SweepQuietGardeners(context.Background())
	if got := wakeEvents(multi); !slices.Equal(got, []string{"gd-plan-1"}) {
		t.Fatalf("want one wake on gd-plan-1, got %v", got)
	}
}

// After a wake the gardener is left alone for 2h even if it has not run; a
// second wake within the day files one operator escalation naming it, and a
// third run with the same gardener does not file another.
func TestSweepQuietGardeners_SecondWakeInADayEscalatesOnce(t *testing.T) {
	daemon := quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
	engine, multi, rec, clk := quietSweepEngine(daemon)
	ctx := context.Background()

	engine.SweepQuietGardeners(ctx)
	multi.Engine().DrainEvents()

	*clk = quietSweepNow.Add(30 * time.Minute)
	engine.SweepQuietGardeners(ctx)
	if got := wakeEvents(multi); len(got) != 0 {
		t.Fatalf("a gardener woken 30m ago is not woken again, got %v", got)
	}

	*clk = quietSweepNow.Add(150 * time.Minute)
	engine.SweepQuietGardeners(ctx)
	if got := wakeEvents(multi); !slices.Equal(got, []string{"gd-plan-1"}) {
		t.Fatalf("want a second wake 2.5h later, got %v", got)
	}
	escs := escalationsCreated(daemon)
	if len(escs) != 1 {
		t.Fatalf("want 1 escalation after the second wake, got %d", len(escs))
	}
	esc := escs[0]
	if !slices.Contains(esc.Labels, quietGardenerSweepMarkerLabel) || !slices.Contains(esc.Labels, "project:gardenio") {
		t.Errorf("escalation labels: got %v", esc.Labels)
	}
	if esc.ShortTitle == "" || len([]rune(esc.ShortTitle)) > 40 {
		t.Errorf("short title must be set and at most 40 runes, got %q", esc.ShortTitle)
	}
	for _, want := range []string{"gd-gard-1", "gd-plan-1", "gd-step-1", "gb gardener mode <trigger> --timed"} {
		if !strings.Contains(esc.Description, want) {
			t.Errorf("escalation must mention %q, got:\n%s", want, esc.Description)
		}
	}
	if len(rec.calls) != 1 {
		t.Errorf("want one Slack notice for the filed escalation, got %d", len(rec.calls))
	}

	// The filed escalation is now open; a third wake names the same gardener.
	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, &beadsapi.BeadDetail{
		ID: "gd-esc-q", Type: "escalation", Status: "open", Labels: esc.Labels,
		Fields: map[string]string{"operation_spec": `{"gardeners":["gd-gard-1"]}`},
	})
	daemon.mu.Unlock()
	multi.Engine().DrainEvents()
	*clk = quietSweepNow.Add(5 * time.Hour)
	engine.SweepQuietGardeners(ctx)
	if got := wakeEvents(multi); !slices.Equal(got, []string{"gd-plan-1"}) {
		t.Fatalf("want a third wake, got %v", got)
	}
	if escs := escalationsCreated(daemon); len(escs) != 1 {
		t.Errorf("an open escalation naming the same gardener is not filed again, got %d", len(escs))
	}
	if n := commentsOn(daemon, "gd-esc-q"); n != 0 {
		t.Errorf("an unchanged set is not commented, got %d", n)
	}
}

// A wake older than a day is forgotten: the next wake is a first one again.
func TestSweepQuietGardeners_WakeADayAgoIsForgotten(t *testing.T) {
	daemon := quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
	engine, multi, _, clk := quietSweepEngine(daemon)
	ctx := context.Background()

	engine.SweepQuietGardeners(ctx)
	multi.Engine().DrainEvents()
	*clk = quietSweepNow.Add(25 * time.Hour)
	engine.SweepQuietGardeners(ctx)

	if got := wakeEvents(multi); !slices.Equal(got, []string{"gd-plan-1"}) {
		t.Fatalf("want a wake a day later, got %v", got)
	}
	if escs := escalationsCreated(daemon); len(escs) != 0 {
		t.Errorf("a wake a day after the last is a first wake, got %d escalations", len(escs))
	}
}

// Off by default: an engine the sweep was not enabled on never wakes anything.
func TestSweepQuietGardeners_DisabledIsNoop(t *testing.T) {
	daemon := quietPlanDaemon(quietSweepNow.Add(-3 * time.Hour))
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default(),
		Clock: func() time.Time { return quietSweepNow }})
	multi := NewMultiTriggerSource(nil, nil)
	engine.RegisterSource(multi)

	engine.SweepQuietGardeners(context.Background())

	if got := wakeEvents(multi); len(got) != 0 {
		t.Errorf("disabled sweep must not wake, got %v", got)
	}
}
