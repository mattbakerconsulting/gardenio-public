// Engine-side sweep for event-mode plan gardeners that stay quiet while their
// plan has ready work (gd-bo2RUmtpHb, the watch on gd-fULNT8wZNF).
//
// THE GAP. On 11-01 the plan gardeners moved from a 30-minute cron to event
// mode: each wakes on a bead created, updated or closed in its plan's subtree
// (gardenermode.setEvent, entry "plan-activity"), with at most a daily
// watchdog. A step can become ready with no event in that subtree (its
// blocker lives elsewhere, a decision was answered, the SSE stream dropped),
// and then nothing wakes the gardener: the plan sits with a ready step and a
// bound, enabled, silent gardener. The owner's condition for event mode was
// "we will have to watch them to make sure stuff continues to happen".
//
// THE NET. Every quietGardenerSweepInterval this sweep lists the open, enabled,
// event-mode plan gardeners whose last run is quietGardenerAge or more ago and
// that have no live cycle. For each whose plan has an open, unblocked,
// unassigned step it:
//   - wakes the gardener now, by injecting a beads.bead.updated for the plan
//     epic into the multi source, so the gardener's own plan-activity entry
//     fires it through the normal path (lineage, singleton, budgets, debounce
//     kept; the same wake an edit of the epic would cause);
//   - notes it on the plan epic.
// A gardener is woken at most once per quietGardenerAge. When the same
// gardener needs a second wake within quietGardenerRepeatWindow, event mode is
// not working for it, and the sweep files ONE operator escalation naming every
// such gardener, deduped while open as the dormant-gardener sweep does. The
// wake history is in memory: an engine restart forgets it, which costs at most
// one extra wake before the escalation.
//
// The home steward cannot do this itself: its lands-tenant tier refuses
// trigger writes and privileged spawns, and the ownership gate refuses its
// writes on plans it did not file.

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Quiet-gardener sweep tunables.
const (
	// quietGardenerAge is how long a gardener must have gone without a run
	// while its plan has a ready step (the owner's signal, gd-fULNT8wZNF).
	quietGardenerAge = 2 * time.Hour

	// quietGardenerRepeatWindow is "twice in a day": a second wake of the
	// same gardener inside it is escalated.
	quietGardenerRepeatWindow = 24 * time.Hour

	// quietGardenerSweepInterval throttles the sweep. Detection lags the
	// signal by at most this much.
	quietGardenerSweepInterval = 15 * time.Minute

	// quietGardenerStepsShown caps the ready steps named in a plan note.
	quietGardenerStepsShown = 5

	// quietGardenerStepScan caps the open steps read per plan.
	quietGardenerStepScan = 200

	// quietGardenerSweepMarkerLabel tags the escalation the sweep files so
	// the next run finds it and does not file another while it is open.
	quietGardenerSweepMarkerLabel = "quiet-gardener-sweep"

	// quietGardenerSweepActor is the CreatedBy / comment author, and the
	// actor of the injected wake event.
	quietGardenerSweepActor = "quiet-gardener-sweep"

	// quietGardenerSweepSystemOperator is the X-System-Operator source label
	// for the sweep's writes (ka-0000007257), as the sibling sweeps do.
	quietGardenerSweepSystemOperator = "quiet-gardener-sweep"

	// planGardenerLabel tags a plan's gardener trigger (plangardener.MarkerLabel;
	// plangardener imports this package, so it is repeated here).
	planGardenerLabel = "plan-gardener"
)

// quietGardener is one gardener the sweep woke.
type quietGardener struct {
	triggerID string
	title     string
	project   string
	plan      string
	lastRun   time.Time // zero when it never ran
	ready     []string  // the plan's ready, unassigned steps
	wakes     []time.Time
}

// quietGardenerSweepState throttles the sweep and remembers each gardener's
// recent wakes. Dedup of the escalation lives in the daemon.
type quietGardenerSweepState struct {
	mu        sync.Mutex
	lastRunAt time.Time
	wakes     map[string][]time.Time
}

func newQuietGardenerSweepState() *quietGardenerSweepState {
	return &quietGardenerSweepState{wakes: map[string][]time.Time{}}
}

// dueForRun reports whether the interval has elapsed since the last run and,
// if so, records now as the last run. The first call is always due.
func (s *quietGardenerSweepState) dueForRun(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.lastRunAt.IsZero() && now.Sub(s.lastRunAt) < quietGardenerSweepInterval {
		return false
	}
	s.lastRunAt = now
	return true
}

// recentWakes is the gardener's wakes inside the repeat window, oldest first;
// older ones are forgotten.
func (s *quietGardenerSweepState) recentWakes(id string, now time.Time) []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := slices.DeleteFunc(s.wakes[id], func(t time.Time) bool { return now.Sub(t) >= quietGardenerRepeatWindow })
	if len(kept) == 0 {
		delete(s.wakes, id)
		return nil
	}
	s.wakes[id] = kept
	return slices.Clone(kept)
}

func (s *quietGardenerSweepState) recordWake(id string, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wakes[id] = append(s.wakes[id], now)
}

// EnableQuietGardenerSweep turns on the quiet-gardener sweep; until it is
// called SweepQuietGardeners is a no-op. notify routes a one-line Slack notice
// via the notice publisher when the sweep files or updates its escalation.
// Call it before the engine runs, as RegisterSource.
func (e *TriggerEngine) EnableQuietGardenerSweep(notify bool) {
	e.quietGardenerSweepNotify = notify
	e.quietGardenerSweepState = newQuietGardenerSweepState()
}

// SweepQuietGardeners runs the quiet-gardener check (gd-bo2RUmtpHb). See the
// file header. Evaluate calls it before the source passes, so the multi pass
// of the same cycle takes the injected wake before the runner drains it. No-op unless EnableQuietGardenerSweep was called; skipped under a global
// triggers-pause or gardeners-pause, when no gardener runs by design. Errors
// are logged and downgraded so a daemon hiccup cannot wedge the engine.
func (e *TriggerEngine) SweepQuietGardeners(ctx context.Context) {
	if e.quietGardenerSweepState == nil {
		return
	}
	now := e.now()
	if !e.quietGardenerSweepState.dueForRun(now) {
		return
	}
	multi, ok := e.sources["multi"].(*MultiTriggerSource)
	if !ok || multi == nil {
		return
	}
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("quiet-gardener sweep: gardeners-pause engaged, skipping")
		return
	}
	gate := e.loadGateState(ctx)
	if gate.globallyPaused {
		e.logger.Info("quiet-gardener sweep: triggers-pause engaged, skipping")
		return
	}

	ctx = beadsapi.WithSystemOperator(ctx, quietGardenerSweepSystemOperator)

	res, err := e.openTriggerRoster(ctx)
	if err != nil {
		e.logger.Warn("quiet-gardener sweep: failed to load trigger roster", "error", err)
		return
	}

	var woken, repeats []quietGardener
	for _, bead := range res.Beads {
		g, plan, ok := e.quietGardenerCandidate(ctx, bead, gate, now)
		if !ok {
			continue
		}
		g.wakes = append(g.wakes, now)
		e.quietGardenerSweepState.recordWake(g.triggerID, now)
		multi.InjectBeadEvent("beads.bead.updated", quietWakeEvent(plan))
		e.noteQuietGardenerWake(ctx, g, now)
		woken = append(woken, g)
		if len(g.wakes) > 1 {
			repeats = append(repeats, g)
		}
	}

	escID, action := "", "none"
	if len(repeats) > 0 {
		escID, action = e.fileQuietGardenerEscalation(ctx, repeats, now)
	}
	e.logger.Info("quiet-gardener sweep: complete",
		"woken", len(woken), "repeats", len(repeats), "escalation", escID, "action", action)

	if e.quietGardenerSweepNotify && e.notifier != nil && (action == "filed" || action == "commented") {
		e.notifier(ctx, "quiet-gardener-sweep",
			fmt.Sprintf("quiet-gardener sweep: %d plan gardener(s) needed a second wake within a day while their plan had ready work, so event mode is not waking them (gd-bo2RUmtpHb). See %s.", len(repeats), escID),
			"")
	}
}

// quietGardenerCandidate reports whether bead is a gardener to wake now, with
// what the sweep knows of it and its plan epic.
func (e *TriggerEngine) quietGardenerCandidate(ctx context.Context, bead *beadsapi.BeadDetail, gate triggerGateState, now time.Time) (quietGardener, *beadsapi.BeadDetail, bool) {
	if bead == nil || bead.Status == "closed" || bead.Fields["enabled"] == "false" || isE2EFixture(bead) ||
		!slices.Contains(bead.Labels, planGardenerLabel) || !slices.Contains(bead.Labels, "trigger:multi") {
		return quietGardener{}, nil, false
	}
	planID := gardenerEpicLabel(bead)
	if planID == "" {
		return quietGardener{}, nil, false
	}
	lastRun := parseLivenessTimestamp(bead.Fields["last_run"])
	if !lastRun.IsZero() && now.Sub(lastRun) < quietGardenerAge {
		return quietGardener{}, nil, false
	}
	wakes := e.quietGardenerSweepState.recentWakes(bead.ID, now)
	if len(wakes) > 0 && now.Sub(wakes[len(wakes)-1]) < quietGardenerAge {
		return quietGardener{}, nil, false
	}
	trig := ParseTriggerDef(bead, strings.TrimSpace(bead.Fields["source"]))
	// A parked gardener is the dormant-gardener sweep's; waking it only runs
	// its gate again.
	if trig.Gardener != nil && trig.Gardener.DormantOnEscalation {
		return quietGardener{}, nil, false
	}
	// A gardener its project's pause gates silence (/schedule off, /trigger
	// off) would not fire on the wake; waking it only escalates a quiet the
	// operator asked for.
	if gate.skipTrigger(false, trig) {
		return quietGardener{}, nil, false
	}
	plan, err := e.daemon.GetBead(ctx, planID)
	if err != nil || plan == nil {
		if err != nil {
			e.logger.Warn("quiet-gardener sweep: failed to read plan", "trigger", bead.ID, "plan", planID, "error", err)
		}
		return quietGardener{}, nil, false
	}
	if plan.Status == "closed" {
		return quietGardener{}, nil, false
	}
	if s := strings.TrimSpace(plan.Fields["plan_state"]); s != "" && s != "building" {
		return quietGardener{}, nil, false
	}
	ready, err := e.readyUnassignedSteps(ctx, planID)
	if err != nil {
		e.logger.Warn("quiet-gardener sweep: failed to read plan steps", "trigger", bead.ID, "plan", planID, "error", err)
		return quietGardener{}, nil, false
	}
	if len(ready) == 0 {
		return quietGardener{}, nil, false
	}
	if len(e.liveSpawnHolders(ctx, trig)) > 0 {
		return quietGardener{}, nil, false
	}
	return quietGardener{
		triggerID: bead.ID,
		title:     bead.Title,
		project:   trig.Project,
		plan:      planID,
		lastRun:   lastRun,
		ready:     ready,
		wakes:     wakes,
	}, plan, true
}

// readyUnassignedSteps is the ids of the plan's open steps that wait on no open
// bead or decision (blocked on one, or parked behind one by mention as gb ready
// prints PARKED, ka-0000003797), that nobody holds, that are no person's
// (BeadDetail.HumanOnly) and whose PR is not merged and parked behind its
// release (beadsapi.AwaitsRelease, gd-fHyi9y1k5B), sorted.
func (e *TriggerEngine) readyUnassignedSteps(ctx context.Context, planID string) ([]string, error) {
	res, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:      []string{"task", "bug", "feature", "chore"},
		Parent:     planID,
		Statuses:   []string{"open"},
		NoOpenDeps: true,
		// Both decision projections are opt-in; without them a step gated on
		// or parked behind an open decision reads as ready (gd-yNp2s5Go85,
		// gd-V4QAFgnPA8).
		WithBlockedOnDecision: true,
		WithParkedOnDecision:  true,
		Limit:                 quietGardenerStepScan,
		Omit:                  []string{"description", "notes"},
	})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, b := range res.Beads {
		if b != nil && b.Status == "open" && strings.TrimSpace(b.Assignee) == "" && !b.BlockedOnDecision &&
			!b.ParkedOnDecision && !b.HumanOnly() && !beadsapi.AwaitsRelease(b.Fields) {
			ids = append(ids, b.ID)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// quietWakeEvent is the bead-updated event for plan that wakes its gardener.
func quietWakeEvent(plan *beadsapi.BeadDetail) *BeadEvent {
	return &BeadEvent{
		ID:         plan.ID,
		Type:       plan.Type,
		Title:      plan.Title,
		Status:     plan.Status,
		Assignee:   plan.Assignee,
		CreatedBy:  plan.CreatedBy,
		Labels:     plan.Labels,
		Fields:     plan.Fields,
		Priority:   plan.Priority,
		OperatedBy: plan.OperatedBy,
		Actor:      quietGardenerSweepActor,
	}
}

// noteQuietGardenerWake comments the wake on the plan epic.
func (e *TriggerEngine) noteQuietGardenerWake(ctx context.Context, g quietGardener, now time.Time) {
	since := "It has never run"
	if !g.lastRun.IsZero() {
		since = fmt.Sprintf("Its last run was %s (%s ago)", g.lastRun.UTC().Format(time.RFC3339), now.Sub(g.lastRun).Round(time.Minute))
	}
	text := fmt.Sprintf("quiet-gardener sweep: woke gardener %s now. %s, and this plan has %d ready, unassigned step(s): %s. (gd-bo2RUmtpHb)",
		g.triggerID, since, len(g.ready), quietStepList(g.ready))
	if len(g.wakes) > 1 {
		text += fmt.Sprintf(" This is its second wake within %s; the operator has been told.", quietGardenerRepeatWindow)
	}
	if err := e.daemon.AddComment(ctx, g.plan, quietGardenerSweepActor, text); err != nil {
		e.logger.Warn("quiet-gardener sweep: failed to note the wake on the plan", "plan", g.plan, "trigger", g.triggerID, "error", err)
	}
}

// fileQuietGardenerEscalation files the one operator escalation naming
// repeats, or, when an earlier one is still open, comments on it if the named
// set has changed. Returns the escalation id and what it did: "filed",
// "commented", "unchanged" or "error".
func (e *TriggerEngine) fileQuietGardenerEscalation(ctx context.Context, repeats []quietGardener, now time.Time) (string, string) {
	ids := make([]string, len(repeats))
	for i, g := range repeats {
		ids[i] = g.triggerID
	}
	sort.Strings(ids)
	body := renderQuietGardeners(repeats, now)

	open, err := e.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{quietGardenerSweepMarkerLabel},
		Limit:    10,
	})
	if err != nil {
		e.logger.Warn("quiet-gardener sweep: failed to check for an open escalation", "error", err)
		return "", "error"
	}
	if len(open.Beads) > 0 && open.Beads[0] != nil {
		existing := open.Beads[0]
		if slices.Equal(dormantGardenerEscalationIDs(existing), ids) {
			return existing.ID, "unchanged"
		}
		if err := e.daemon.AddComment(ctx, existing.ID, quietGardenerSweepActor, "More plan gardeners needed a second wake.\n\n"+body); err != nil {
			e.logger.Warn("quiet-gardener sweep: failed to comment on open escalation", "escalation", existing.ID, "error", err)
			return existing.ID, "error"
		}
		return existing.ID, "commented"
	}

	// operation_spec.gardeners is the shape dormantGardenerEscalationIDs reads.
	spec, err := json.Marshal(map[string]any{
		"action":    "investigate_quiet_event_gardener",
		"gardeners": ids,
	})
	if err != nil {
		e.logger.Warn("quiet-gardener sweep: marshal operation_spec failed", "error", err)
		return "", "error"
	}
	fields, err := json.Marshal(map[string]any{
		"operation_type": "manual_operator_action",
		"operation_spec": json.RawMessage(spec),
		"context":        body,
		"audience":       "operator",
	})
	if err != nil {
		e.logger.Warn("quiet-gardener sweep: marshal fields failed", "error", err)
		return "", "error"
	}
	labels := []string{quietGardenerSweepMarkerLabel, "trigger-engine"}
	for _, g := range repeats {
		project := g.project
		if project == "" {
			project = "gardenio"
		}
		if l := "project:" + project; !slices.Contains(labels, l) {
			labels = append(labels, l)
		}
	}
	id, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("%d event-mode plan gardener(s) needed a second wake within a day: put them back on the clock?", len(repeats)),
		ShortTitle:  "check quiet event-mode gardeners",
		Type:        "escalation",
		Description: body,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   quietGardenerSweepActor,
		Fields:      fields,
	})
	if err != nil {
		e.logger.Warn("quiet-gardener sweep: failed to file escalation", "error", err)
		return "", "error"
	}
	e.logger.Warn("quiet-gardener sweep: filed escalation", "escalation", id, "gardeners", ids)
	return id, "filed"
}

// renderQuietGardeners builds the escalation body and the change comment.
func renderQuietGardeners(repeats []quietGardener, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "As of %s, %d event-mode plan gardener(s) went %s or more without a run, twice within %s, "+
		"while their plan had a ready, unassigned step. The quiet-gardener sweep woke each one both times "+
		"(gd-bo2RUmtpHb), but nothing in its plan's subtree is waking it on its own.\n\n",
		now.UTC().Format(time.RFC3339), len(repeats), quietGardenerAge, quietGardenerRepeatWindow)
	for _, g := range repeats {
		last := "never"
		if !g.lastRun.IsZero() {
			last = g.lastRun.UTC().Format(time.RFC3339)
		}
		wakes := make([]string, len(g.wakes))
		for i, t := range g.wakes {
			wakes[i] = t.UTC().Format("15:04Z")
		}
		fmt.Fprintf(&b, "  • %s %s: plan %s, last run %s, woken at %s, ready: %s\n",
			g.triggerID, truncateForDecision(g.title, 70), g.plan, last, strings.Join(wakes, ", "), quietStepList(g.ready))
	}
	b.WriteString("\nFor each gardener, either put it back on the 30-minute clock with " +
		"`gb gardener mode <trigger> --timed`, or find why its ready steps raise no event in the plan's subtree " +
		"(a blocker outside the plan, an answered decision) and leave it on events. Close this escalation when " +
		"done; the sweep files a new one if a gardener needs a second wake again.\n")
	return b.String()
}

// quietStepList names up to quietGardenerStepsShown step ids.
func quietStepList(ids []string) string {
	if len(ids) <= quietGardenerStepsShown {
		return strings.Join(ids, ", ")
	}
	return strings.Join(ids[:quietGardenerStepsShown], ", ") + fmt.Sprintf(" and %d more", len(ids)-quietGardenerStepsShown)
}
