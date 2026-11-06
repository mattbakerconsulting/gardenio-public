// ScheduleMirrorTriggerSource fires `type=trigger` mirror beads (labelled
// `schedule-mirror`) through the same scheduler.EvaluateTriggers pipeline
// MultiTriggerSource uses. Mirrors carry the full multi-source `triggers[]`
// array plus `schedule_id` / `last_run` / `enabled` fields.
//
// Phase 3b/D10 removed type=schedule firing entirely (controller scheduler
// RunLoop + dual-write are gone), so the previous "skip mirrors whose source
// schedule is still open" race-guard is no longer needed — there is no other
// path firing these beads. Mirrors fire whenever enabled=true with a
// non-empty triggers[] array.

package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"
	_ "time/tzdata"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// scheduleMirrorMarkerLabel is the shared label every mirror trigger carries.
const scheduleMirrorMarkerLabel = "schedule-mirror"

// ScheduleMirrorTriggerSource fires `schedule-mirror`-labelled type=trigger
// beads. Sibling of MultiTriggerSource — loads by label instead of by
// `trigger:multi`.
type ScheduleMirrorTriggerSource struct {
	logger  *slog.Logger
	engine  *scheduler.EntryEngine
	nowFunc func() time.Time

	mu     sync.Mutex
	daemon scheduler.TriggerDaemon
}

// scheduleMirrorData carries per-mirror transient state parsed off the bead.
// Mirrors the multiTriggerData shape so EvaluateTriggers can consume it.
type scheduleMirrorData struct {
	LastRun  time.Time
	Timezone string
	// SourceScheduleID is the dual-write back-pointer (mirror.fields.schedule_id).
	// Captured for log breadcrumbs; firing semantics use only LastRun/Triggers.
	SourceScheduleID string
}

// NewScheduleMirrorTriggerSource constructs the source. If engine is nil a
// fresh *scheduler.EntryEngine is allocated; production callers MUST share
// the engine with MultiTriggerSource so the same event buffer feeds both
// (otherwise event-driven mirror entries would never see SSE/NATS events).
func NewScheduleMirrorTriggerSource(logger *slog.Logger, engine *scheduler.EntryEngine) *ScheduleMirrorTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	if engine == nil {
		engine = scheduler.NewEntryEngine()
	}
	return &ScheduleMirrorTriggerSource{
		logger:  logger,
		engine:  engine,
		nowFunc: time.Now,
	}
}

func (s *ScheduleMirrorTriggerSource) Name() string { return "schedule-mirror" }

// SkipRuntimeBootstrap opts out of the engine's runtime-bootstrap guard for
// the same reason MultiTriggerSource does: cron entries are forward-only
// (suppressed by the bridge engine's seen set seeded from CatchUpKeys) and
// event entries only fire for events buffered since the last drain.
func (s *ScheduleMirrorTriggerSource) SkipRuntimeBootstrap() bool { return true }

// Engine returns the underlying *scheduler.EntryEngine.
func (s *ScheduleMirrorTriggerSource) Engine() *scheduler.EntryEngine { return s.engine }

// LoadTriggers fetches `schedule-mirror`-labelled trigger beads. Mirrors
// with empty triggers[] or enabled=false are skipped.
func (s *ScheduleMirrorTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	if bd, ok := daemon.(scheduler.TriggerDaemon); ok {
		s.mu.Lock()
		s.daemon = bd
		s.mu.Unlock()
	}

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"trigger"},
		Statuses: []string{"open"},
		Labels:   []string{scheduleMirrorMarkerLabel},
		Limit:    200,
	})
	if err != nil {
		return nil, err
	}
	if len(result.Beads) == 0 {
		return nil, nil
	}

	var triggers []TriggerDef
	for _, bead := range result.Beads {
		if bead.Fields["enabled"] == "false" {
			continue
		}
		// Skip beads-viewer-e2e fixtures so the shared prod scheduler never
		// fires a test fixture (ka-0000005435). enabled is left untouched so
		// the bead still renders in beads-viewer.
		if isE2EFixture(bead) {
			s.logger.Info("schedule-mirror: skipping E2E fixture (never fires in prod)",
				"trigger", bead.ID, "title", bead.Title)
			continue
		}
		t := ParseTriggerDef(bead, "schedule-mirror")
		if len(t.Triggers) == 0 {
			s.logger.Debug("schedule-mirror: skipping mirror with empty triggers[]",
				"trigger", bead.ID)
			continue
		}

		data := scheduleMirrorData{
			Timezone:         bead.Fields["timezone"],
			SourceScheduleID: bead.Fields["schedule_id"],
		}
		if raw := bead.Fields["last_run"]; raw != "" {
			for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
				if parsed, err := time.Parse(layout, raw); err == nil {
					data.LastRun = parsed
					break
				}
			}
		}
		t.SourceData = data
		triggers = append(triggers, t)
	}
	return triggers, nil
}

// Evaluate fans-in trig.Triggers via scheduler.EvaluateTriggers and converts
// the firing entry's FireDecision into a TriggerMatch. Returns nil when no
// entry fires this tick.
func (s *ScheduleMirrorTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	if len(trig.Triggers) == 0 {
		return nil, nil
	}
	data, _ := trig.SourceData.(scheduleMirrorData)

	s.mu.Lock()
	daemon := s.daemon
	s.mu.Unlock()

	tctx := scheduler.TriggerEvalContext{
		TriggerID: trig.ID,
		Project:   trig.Project,
		Timezone:  data.Timezone,
		Triggers:  trig.Triggers,
		LastRun:   data.LastRun,
	}

	decisions := scheduler.EvaluateTriggersAll(ctx, s.engine, daemon, s.logger, tctx, s.nowFunc())
	if len(decisions) == 0 {
		return nil, nil
	}

	// Stamp the latest fire time across the batch so the throttle window
	// reflects the most-recent event.
	var latest time.Time
	matches := make([]TriggerMatch, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Time.After(latest) {
			latest = decision.Time
		}
		matches = append(matches, scheduleMirrorDecisionToMatch(trig, decision, data))
	}
	s.engine.RecordFire(trig.ID, decisions[0].EntryIndex, latest)

	return matches, nil
}

// EventReady reports whether this mirror has at least one event-kind entry
// with a matching buffered event waiting (read-only — it does not consume the
// event or record a fire). The bridge engine's cooldown gate calls this so an
// event-eligible mirror can bypass the per-trigger cooldown when a real
// matching event is pending, while cron/idle re-eval stays cooldown-gated.
//
// Mirrors fire event-kind entries (bead_event/decision_resolved/mail) off the
// SHARED EntryEngine event buffer (this source shares MultiTriggerSource's
// engine), so without this method a matching event arriving mid-cooldown is
// buffered, never consumed (cooldown skips the mirror before Evaluate runs),
// then drained at cycle end and silently lost. This is the schedule-mirror
// instance of the cooldown/event race (bug ka-0000005988, fixed for the multi
// and bead sources; ka-0000004937 for this one) — the event-kind sibling of
// CronDue, mirroring MultiTriggerSource.EventReady exactly.
func (s *ScheduleMirrorTriggerSource) EventReady(ctx context.Context, trig TriggerDef) bool {
	if len(trig.Triggers) == 0 {
		return false
	}
	data, _ := trig.SourceData.(scheduleMirrorData)

	s.mu.Lock()
	daemon := s.daemon
	s.mu.Unlock()

	tctx := scheduler.TriggerEvalContext{
		TriggerID: trig.ID,
		Project:   trig.Project,
		Timezone:  data.Timezone,
		Triggers:  trig.Triggers,
		LastRun:   data.LastRun,
	}
	return scheduler.HasPendingEventMatch(ctx, s.engine, daemon, s.logger, tctx, s.nowFunc())
}

// CronDue reports whether this mirror has a cron entry due right now
// (in-window and past last_run; read-only). The bridge engine's cooldown gate
// calls this so a cron-due mirror can bypass the per-trigger cooldown —
// without it, a cooldown that divides 24h evenly phase-locks the evaluation
// slots outside the cron's active window and the mirror never fires again
// while last_checked stays fresh (bug ka-0000004307).
func (s *ScheduleMirrorTriggerSource) CronDue(_ context.Context, trig TriggerDef) bool {
	if len(trig.Triggers) == 0 {
		return false
	}
	data, _ := trig.SourceData.(scheduleMirrorData)
	tctx := scheduler.TriggerEvalContext{
		TriggerID: trig.ID,
		Project:   trig.Project,
		Timezone:  data.Timezone,
		Triggers:  trig.Triggers,
		LastRun:   data.LastRun,
	}
	return scheduler.HasDueCronEntry(s.engine, s.logger, tctx, s.nowFunc())
}

// CatchUpKeys seeds the bridge engine's seen map on startup so cron entries
// that already fired before restart don't re-fire. Keys live on prior issue
// beads under `mirror_fire_key`.
func (s *ScheduleMirrorTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"open", "in_progress", "closed"},
		Labels:   []string{"trigger:" + triggerID},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, bead := range result.Beads {
		if key := bead.Fields["mirror_fire_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// scheduleMirrorDecisionToMatch converts a FireDecision into a TriggerMatch.
// Key shape: "<entry_idx>:<entry_label_or_idx>:<source_match_key>" — same as
// MultiTriggerSource, scoped to mirror-issued fires via the `mirror_fire_key`
// field name on the created issue bead.
func scheduleMirrorDecisionToMatch(trig TriggerDef, decision scheduler.FireDecision, data scheduleMirrorData) TriggerMatch {
	entry := trig.Triggers[decision.EntryIndex]
	label := entry.Label
	if label == "" {
		label = fmt.Sprintf("%d", decision.EntryIndex)
	}

	srcKey := mirrorSourceMatchKey(decision)
	key := fmt.Sprintf("%d:%s:%s", decision.EntryIndex, label, srcKey)

	fields := map[string]string{
		"mirror_fire_key":   key,
		"mirror_entry_idx":  fmt.Sprintf("%d", decision.EntryIndex),
		"mirror_entry_kind": decision.Kind,
		"mirror_cause":      decision.Cause,
		"mirror_fire_time":  decision.Time.UTC().Format(time.RFC3339),
	}
	if entry.Label != "" {
		fields["mirror_entry_label"] = entry.Label
	}
	if data.SourceScheduleID != "" {
		fields["source_schedule_id"] = data.SourceScheduleID
	}

	vars := map[string]string{
		"{{trigger_id}}":    trig.ID,
		"{{trigger_title}}": trig.Title,
		"{{entry_index}}":   fmt.Sprintf("%d", decision.EntryIndex),
		"{{entry_label}}":   entry.Label,
		"{{entry_kind}}":    decision.Kind,
		"{{cause}}":         decision.Cause,
		"{{fire_time}}":     decision.Time.UTC().Format(time.RFC3339),
	}
	if decision.Event != nil {
		vars["{{event_bead_id}}"] = decision.Event.BeadID
		vars["{{event_type}}"] = decision.Event.EventType
	}

	return TriggerMatch{
		Key:          key,
		Title:        fmt.Sprintf("ScheduleMirror[%s]: %s", decision.Kind, trig.Title),
		Fields:       fields,
		Labels:       []string{"source:schedule-mirror", "mirror-entry:" + label},
		TemplateVars: vars,
	}
}

// mirrorSourceMatchKey computes the source-specific match key portion of the
// dedup key for a FireDecision. Mirrors multiSourceMatchKey — duplicated here
// so future divergence (e.g. mirror-specific keying) is local.
func mirrorSourceMatchKey(decision scheduler.FireDecision) string {
	switch decision.Kind {
	case scheduler.TriggerKindCron:
		return decision.Time.UTC().Format("2006-01-02T15:04")
	case scheduler.TriggerKindBeadEvent:
		if decision.Event != nil {
			return fmt.Sprintf("%s:%s", decision.Event.EventType, decision.Event.BeadID)
		}
	case scheduler.TriggerKindDecisionResolved:
		if decision.Event != nil {
			return decision.Event.BeadID
		}
	}
	return decision.Time.UTC().Format(time.RFC3339)
}
