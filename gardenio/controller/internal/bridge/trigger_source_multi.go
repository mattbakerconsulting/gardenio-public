package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/planspend"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// MultiTriggerSource implements TriggerSource for the Phase 3b/A4 multi-source
// fan-in (ka-0000000695 / ka-0000007712). A trigger bead labelled
// `trigger:multi` with a non-empty `triggers` JSON array is dispatched here.
// Each entry in TriggerDef.Triggers is evaluated in order via
// scheduler.EvaluateTriggers — the first firing entry wins.
//
// Dedup key shape: "<entry_idx>:<entry_label_or_idx>:<source_match_key>".
//   - cron entries: source_match_key = fire time at minute granularity
//   - bead_event entries: source_match_key = "<event_type>:<bead_id>"
//   - decision_resolved entries: source_match_key = decision bead id
//
// When a trigger bead is labelled `trigger:multi` but its `triggers` array
// is empty / unparseable, the bead is silently skipped — the legacy per-source
// loaders (jira/gitlab/schedule/bead/webhook) still claim it via their own
// `trigger:<source>` label, preserving the legacy single-source path.
//
// Event delivery: the underlying *scheduler.EntryEngine buffers bead
// lifecycle events fed in via RegisterSSE plus decision-resolved events fed
// in via OnDecisionResolved. The buffer is drained externally after each
// engine evaluation cycle by calling DrainPending.
type MultiTriggerSource struct {
	logger  *slog.Logger
	engine  *scheduler.EntryEngine
	nowFunc func() time.Time
	// healBackoff is the bounded retry applied to the stalled-open heal's list
	// query (gd-niQPoeR7ja); see healListBackoff.
	healBackoff BackoffConfig

	mu     sync.Mutex
	daemon scheduler.TriggerDaemon
}

// multiTriggerData carries per-trigger transient state parsed off the bead.
type multiTriggerData struct {
	LastRun  time.Time
	Timezone string
	// FirstFire is set when the bead is a sealed event-mode plan gardener
	// that has never fired (planGardenerAwaitsFirstFire). Evaluate then fires
	// it once even with no subtree event pending, gated on FirstFireGate.
	FirstFire     bool
	FirstFireGate *conditions.Expression
}

// multiFirstFireKind is the FireDecision kind of a plan gardener's first fire
// (gd-Vk2ADepJZ2). Its dedup key is fixed ("<idx>:<label>:first-fire"), so the
// engine's seen set can never let it fire twice in one process; across a
// restart the gardener's last_run / runs_spawned (gardenerPostFire) keep it
// from qualifying again.
const multiFirstFireKind = "first_fire"

// planGardenerAwaitsFirstFire reports whether bead is an event-mode plan
// gardener that should fire once now instead of waiting for the next change
// under its plan or its daily watchdog (gd-Vk2ADepJZ2). LoadTriggers only
// reaches here for an open, enabled trigger:multi bead, so "enabled" and
// "loadable by the engine" already hold. On top of that it must be:
//
//   - a plan gardener (plan-gardener marker) with a bead_event entry;
//   - sealed: its spawn_lineage seal names an oidc: author. Only the trigger
//     engine can spend a seal (kbeads spawn_lineage_authz.go), which is why the
//     first fire happens here and not in the binder. The daemon still checks
//     the seal's digest and age on the spawn; an unsealed gardener would only
//     be refused, so it is not tried;
//   - never fired: no last_run, no last_agent_id, no runs_spawned. A paused
//     gardener that ran before and is re-enabled does not qualify, nor does a
//     gardener with a live or earlier cycle;
//   - not failing since its seal: no last_fire_error, or one stamped before the
//     seal (sealPostdatesFireError). A refused first fire is not retried each
//     tick (that would walk consecutive_failures to the hard_stop); the
//     plan-activity entry and the watchdog take over as before. A reseal after
//     the refusal (`gb trigger reseal`, the usual fix for a stale seal) makes it
//     eligible again (gd-wT7Q0kFTbT); if that fire is refused too, the fresh
//     error postdates the seal and stops it again, so there is one try per seal.
func planGardenerAwaitsFirstFire(bead *beadsapi.BeadDetail, t TriggerDef) bool {
	if bead == nil || !slices.Contains(bead.Labels, "plan-gardener") {
		return false
	}
	if !slices.ContainsFunc(t.Triggers, func(e scheduler.TriggerEntry) bool { return e.Kind == scheduler.TriggerKindBeadEvent }) {
		return false
	}
	if strings.TrimSpace(bead.Fields["last_run"]) != "" || strings.TrimSpace(bead.Fields["last_agent_id"]) != "" {
		return false
	}
	if g := t.Gardener; g != nil && (g.RunsSpawned > 0 || g.LastAgentID != "") {
		return false
	}
	var seal struct {
		Author string `json:"author"`
		IAT    int64  `json:"iat"`
	}
	if json.Unmarshal([]byte(bead.Fields["spawn_lineage"]), &seal) != nil || !strings.HasPrefix(seal.Author, "oidc:") {
		return false
	}
	if strings.TrimSpace(bead.Fields["last_fire_error"]) != "" && !sealPostdatesFireError(bead.Fields["last_fire_error_at"], seal.IAT) {
		return false
	}
	return true
}

// firstFireResealMargin is how much later than last_fire_error_at a seal's iat
// must be before a reseal re-qualifies a refused first fire. The two clocks
// differ: the controller stamps last_fire_error_at (stampFireError) and the
// beads daemon stamps iat. The margin keeps a refusal that lands just after a
// reseal, on a controller clock running behind the daemon's, from reading as
// older than the seal and being retried. A reseal is a person's act, minutes
// after the refusal they are answering, so the margin costs nothing real.
const firstFireResealMargin = time.Minute

// sealPostdatesFireError reports whether a spawn_lineage seal issued at iat
// (unix seconds, the daemon's clock) is later than a fire error stamped at
// errAt (RFC3339, as stampFireError writes it) by more than
// firstFireResealMargin. A missing or unreadable timestamp on either side is
// "no": the error keeps disqualifying, which is the behaviour before reseals
// were honoured.
func sealPostdatesFireError(errAt string, iat int64) bool {
	if iat <= 0 {
		return false
	}
	at, err := time.Parse(time.RFC3339, strings.TrimSpace(errAt))
	if err != nil {
		return false
	}
	return time.Unix(iat, 0).After(at.Add(firstFireResealMargin))
}

// firstFireGate is the open-children gate a first fire must pass: the
// watchdog entry's preflight, else the bead's top-level conditions. A plan
// gardener with neither gets no first fire.
func firstFireGate(bead *beadsapi.BeadDetail, t TriggerDef) *conditions.Expression {
	for _, e := range t.Triggers {
		if e.Kind == scheduler.TriggerKindCron && e.Preflight != nil {
			return e.Preflight
		}
	}
	return gardenermode.PreflightFromConditions(bead.Fields["conditions"])
}

// NewMultiTriggerSource constructs the source. If engine is nil a fresh
// *scheduler.EntryEngine is allocated; callers wanting to share an engine
// with the scheduler should pass an existing instance.
func NewMultiTriggerSource(logger *slog.Logger, engine *scheduler.EntryEngine) *MultiTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	if engine == nil {
		engine = scheduler.NewEntryEngine()
	}
	return &MultiTriggerSource{
		logger:      logger,
		engine:      engine,
		nowFunc:     time.Now,
		healBackoff: healListBackoff(),
	}
}

func (s *MultiTriggerSource) Name() string { return "multi" }

// SkipRuntimeBootstrap opts out of the engine's runtime-bootstrap guard.
// Cron entries are forward-only (suppressed by the bridge engine's seen set
// seeded from CatchUpKeys) and event entries fire only for events buffered
// since the last DrainPending, so there is no thundering-herd risk on
// runtime trigger creation.
func (s *MultiTriggerSource) SkipRuntimeBootstrap() bool { return true }

// Engine returns the underlying *scheduler.EntryEngine so callers can wire
// SSE / NATS event sources into the same event buffer.
func (s *MultiTriggerSource) Engine() *scheduler.EntryEngine { return s.engine }

// RegisterSSE attaches bead lifecycle event handlers to the SSE stream so
// every created/updated/closed bead becomes a TriggerEvent in the engine's
// buffer. Mirrors BeadStateTriggerSource.RegisterSSE.
//
// In addition to the three full-payload bead topics, this also subscribes
// to edge-shaped events (dependency/comment/label/follower add+remove,
// bead deletion/transfer) catalogued in edgeEventTopics. Edge SSE payloads
// carry only bead ids, so each affected bead is hydrated via the daemon
// (GetBead) before firing so type/status/label/field conditions (and the
// in_subtree_of leaf, via the daemon ancestor walk) can match. Hydration
// fails open: a nil daemon or GetBead error skips the event rather than
// firing an unmatchable id-only one. See ka-0000001427.
func (s *MultiTriggerSource) RegisterSSE(stream *SSEStream) {
	beadTopics := []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"}
	for _, topic := range beadTopics {
		t := topic
		stream.On(t, func(_ context.Context, data []byte) {
			bead := ParseBeadEvent(data)
			if bead == nil {
				return
			}
			s.injectBeadEventFromTopic(t, bead)
		})
	}
	for _, topic := range edgeEventTopics {
		t := topic
		stream.On(t, func(ctx context.Context, data []byte) {
			ids := ParseEdgeEventBeadIDs(t, data)
			if len(ids) == 0 {
				return
			}
			// "beads.dependency.added" -> "dependency.added"
			eventType := t
			if idx := strings.Index(t, "."); idx >= 0 {
				eventType = t[idx+1:]
			}
			// Snapshot the daemon under the mutex; LoadTriggers populates it.
			s.mu.Lock()
			daemon := s.daemon
			s.mu.Unlock()
			for _, id := range ids {
				// Hydrate the affected bead so type/status/label/field
				// conditions can match. Fail open on any hydration miss — an
				// id-only event can never satisfy those conditions, so firing
				// one is worse than skipping (ka-0000001427).
				payload, labels, ok := hydrateEdgeEvent(ctx, daemon, s.logger, id, t, eventType)
				if !ok {
					continue
				}
				if t == "beads.comment.added" {
					// Who wrote the comment, and what it says: the hydrated
					// bead cannot tell (its created_by is the bead's author).
					// Set over any same-named bead field, since they describe
					// this event (gd-bo2RUmtpHb).
					maps.Copy(payload, CommentAddedFields(data))
				}
				s.engine.OnBeadEventAt(eventType, id, payload, labels, s.nowFunc())
			}
		})
	}
	s.logger.Info("multi trigger source: registered SSE handlers",
		"bead_topics", beadTopics, "edge_topics", edgeEventTopics)
}

// injectBeadEventFromTopic is the single seam that turns a full-payload bead
// topic ("beads.bead.created"|updated|closed) + parsed bead into a buffered
// engine event. RegisterSSE and InjectBeadEvent both route through it so a
// synthetically-injected event is byte-identical to one delivered over live
// SSE — same event-type derivation, same beadEventToPayload shape, same
// labels. Keeping the two paths on one helper is the fixture-fidelity
// guarantee for the multi-source cooldown/event-race e2e (ka-0000000405):
// the test drives the exact payload prod emits, not a hand-reconstructed one.
func (s *MultiTriggerSource) injectBeadEventFromTopic(topic string, bead *BeadEvent) {
	eventType := topic
	if idx := strings.LastIndex(topic, "."); idx >= 0 {
		eventType = topic[idx+1:]
	}
	if eventType == "updated" && bookkeepingOnlyUpdate(bead) {
		return
	}
	payload := beadEventToPayload(bead)
	if _, ok := payload["actor"]; !ok {
		// A created or closed frame carries no actor; its creator or closer
		// is who made this change, so an entry can tell its own writes from
		// another's on every event (gardenermode.EventTriggers).
		switch {
		case eventType == "created" && bead.CreatedBy != "":
			payload["actor"] = bead.CreatedBy
		case eventType == "closed" && bead.ClosedBy != "":
			payload["actor"] = bead.ClosedBy
		}
	}
	s.engine.OnBeadEventAt(eventType, bead.ID, payload, bead.Labels, s.nowFunc())
}

// triggerBookkeepingFields are the fields the trigger engine stamps on a
// trigger bead as it evaluates and fires it (stampCheckedOutcome, stampLastRun,
// stampFireError, clearFireError).
var triggerBookkeepingFields = map[string]bool{
	"last_checked":       true,
	"last_outcome":       true,
	"last_outcome_at":    true,
	"last_run":           true,
	"last_agent_id":      true,
	"last_match":         true,
	"last_fired":         true,
	"last_fire_error":    true,
	"last_fire_error_at": true,
}

// rollupFields are the fields the controller's spend rollups rewrite as a
// figure moves: the plan rollup on any bead, the gardener rollup on a trigger.
var (
	planRollupFields = map[string]bool{
		planspend.FieldUSD:      true,
		planspend.FieldSyncedAt: true,
		planspend.FieldCrew:     true,
		planspend.FieldLive:     true,
	}
	gardenerRollupFields = map[string]bool{
		planspend.GardenerFieldUSD:      true,
		planspend.GardenerFieldSyncedAt: true,
	}
)

// bookkeepingOnlyUpdate reports whether an update changed only fields the
// controller stamps as bookkeeping: the trigger engine's check and fire stamps
// on a trigger (gd-OvY1D7G5Il), or a spend rollup's figure (gd-yeakVgR34c).
// Such an update records the controller's own pass, not a change to the bead,
// so no bead_event entry hears it: a plan gardener would otherwise be woken on
// every check tick, and again each time a crew's cost lands after the close
// that already woke it. Changes carries the opaque "fields" blob beside one key
// per changed field; an update naming no per-field key cannot be told apart,
// so it is kept.
func bookkeepingOnlyUpdate(bead *BeadEvent) bool {
	trigger := bead.Type == "trigger"
	seen := false
	for k := range bead.Changes {
		if k == "fields" {
			continue
		}
		switch {
		case planRollupFields[k]:
		case trigger && (triggerBookkeepingFields[k] || gardenerRollupFields[k]):
		default:
			return false
		}
		seen = true
	}
	return seen
}

// InjectBeadEvent appends one synthetic bead lifecycle event to the underlying
// engine's buffer, exactly as the SSE bead-topic handler would for a live
// frame. topic is a full bead topic ("beads.bead.created"|"beads.bead.updated"
// |"beads.bead.closed"); the event fires on the next Evaluate/EventReady pass
// (multi opts out of the runtime-bootstrap guard, so a runtime-injected event
// is not suppressed). Mirrors BeadStateTriggerSource.InjectBeadEvent so
// cmd/trigger-probe can drive both sources through the identical recipe. A nil
// bead or empty topic is a no-op.
func (s *MultiTriggerSource) InjectBeadEvent(topic string, bead *BeadEvent) {
	if bead == nil || topic == "" {
		return
	}
	s.injectBeadEventFromTopic(topic, bead)
}

// OnDecisionResolved buffers a decision-resolved event. Wire this into the
// decisions watcher so multi-source triggers with a
// `kind: decision_resolved` entry can observe operator picks.
func (s *MultiTriggerSource) OnDecisionResolved(decisionID string, payload map[string]any, labels []string) {
	s.engine.OnDecisionResolved(decisionID, payload, labels)
}

// FreezePending fixes the buffered events the coming full Evaluate pass
// sees; an event that arrives during the pass is kept for the next one
// (scheduler.EntryEngine.FreezeEvents, gd-IQcMk16PkH).
func (s *MultiTriggerSource) FreezePending() {
	s.engine.FreezeEvents()
}

// DrainPending clears the events the pass saw: those FreezePending froze,
// or the whole buffer when the pass was not frozen. The trigger engine
// runner must call this after every full Evaluate pass so events that did
// not match any multi-source trigger don't pile up.
func (s *MultiTriggerSource) DrainPending() {
	s.engine.DrainEvents()
}

// PendingCount reports the number of buffered events (for tests / metrics).
func (s *MultiTriggerSource) PendingCount() int {
	return s.engine.PendingEventCount()
}

// LoadTriggers fetches `trigger:multi`-labelled trigger beads with a
// non-empty triggers[] array. Beads with an empty triggers[] are skipped —
// the legacy per-source loaders still claim them via their own labels.
func (s *MultiTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	if bd, ok := daemon.(scheduler.TriggerDaemon); ok {
		s.mu.Lock()
		s.daemon = bd
		s.mu.Unlock()
	}

	// Self-heal WEDGED trigger:multi beads before the open-only load
	// (gd-7XjRN5uCUW), the same heal the schedule source runs (ka-0000006681).
	// An event-mode plan gardener is trigger:multi (PR #1907), and the engine
	// never parks one in_progress, so an in_progress one is an erroneous claim
	// that would otherwise drop it out of this load for good. Released beads are
	// open again by the time the stalled-open heal below lists, so they load on
	// this same pass.
	healWedgedTriggers(ctx, daemon, s.logger, s.nowFunc, "trigger:multi", s.healBackoff)

	// Self-heal STALLED open gardener triggers (ka-0000003265 / ka-0000003498).
	// The perpetual release-owner + delivery gardeners are trigger:multi, so the
	// schedule-source heal (filtered to trigger:schedule) never saw them: a
	// no_progress_for deadlock left them dark for ~24h with last_checked frozen.
	// Run the same label-parameterized heal here so trigger:multi gardeners
	// recover too. See healStalledOpenTriggers' doc comment for the full
	// failure-mode analysis.
	// The heal's list is reused when it wrote nothing (gd-6PXLuR3juD).
	result := healStalledOpenGardeners(ctx, daemon, s.logger, s.nowFunc, "trigger:multi", s.healBackoff)
	if result == nil {
		var err error
		result, err = daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"trigger"},
			Statuses: []string{"open"},
			Labels:   []string{"trigger:multi"},
			Limit:    100,
		})
		if err != nil {
			return nil, err
		}
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
			s.logger.Info("multi trigger source: skipping E2E fixture (never fires in prod)",
				"trigger", bead.ID, "title", bead.Title)
			continue
		}
		t := ParseTriggerDef(bead, "multi")
		gardenermode.UpgradeLegacyPlanActivity(t.Triggers, gardenermode.EpicOf(bead.Labels))
		if len(t.Triggers) == 0 {
			s.logger.Debug("multi trigger source: skipping trigger:multi bead with empty triggers[]",
				"trigger", bead.ID)
			continue
		}

		data := multiTriggerData{Timezone: bead.Fields["timezone"]}
		if planGardenerAwaitsFirstFire(bead, t) {
			if gate := firstFireGate(bead, t); gate != nil {
				data.FirstFire, data.FirstFireGate = true, gate
			}
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
// the first firing entry's FireDecision into a TriggerMatch. Returns nil
// (no match) when no entry fires this tick.
func (s *MultiTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	if len(trig.Triggers) == 0 {
		return nil, nil
	}
	data, _ := trig.SourceData.(multiTriggerData)

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
	if len(decisions) == 0 && data.FirstFire {
		if dec, ok := s.firstFireDecision(ctx, daemon, trig, tctx, data.FirstFireGate); ok {
			decisions = []scheduler.FireDecision{dec}
		}
	}
	if len(decisions) == 0 {
		return nil, nil
	}

	// Record the fire on the engine so throttle windows close and repeat
	// cron fires within the same minute are suppressed without relying on
	// the bridge engine's seen map alone. Stamp the latest fire time across
	// the batch so the throttle window reflects the most-recent event.
	var latest time.Time
	matches := make([]TriggerMatch, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Time.After(latest) {
			latest = decision.Time
		}
		matches = append(matches, multiDecisionToMatch(trig, decision))
	}
	s.engine.RecordFire(trig.ID, decisions[0].EntryIndex, latest)

	return matches, nil
}

// firstFireDecision builds the one-time fire of a newly enabled, sealed plan
// gardener (planGardenerAwaitsFirstFire), attributed to its first bead_event
// entry, when its open-children gate holds now. It goes through the normal
// engine fire path, so the cooldown, the live max_concurrent lease, the
// gardener pre-fire checks (one_spawn_at_a_time, run budget) and the daemon's
// spawn-lineage gate all apply exactly as for an event fire.
func (s *MultiTriggerSource) firstFireDecision(ctx context.Context, daemon scheduler.TriggerDaemon, trig TriggerDef, tctx scheduler.TriggerEvalContext, gate *conditions.Expression) (scheduler.FireDecision, bool) {
	idx := slices.IndexFunc(trig.Triggers, func(e scheduler.TriggerEntry) bool { return e.Kind == scheduler.TriggerKindBeadEvent })
	if idx < 0 || gate == nil {
		return scheduler.FireDecision{}, false
	}
	if !scheduler.PreflightHolds(ctx, daemon, s.logger, tctx, gate, idx) {
		return scheduler.FireDecision{}, false
	}
	s.logger.Info("multi trigger source: first fire of a newly enabled plan gardener", "trigger", trig.ID)
	return scheduler.FireDecision{
		Fire:       true,
		EntryIndex: idx,
		Kind:       multiFirstFireKind,
		Cause:      "first fire: plan gardener newly bound or enabled",
		Time:       s.nowFunc(),
	}, true
}

// EventReady reports whether this trigger has at least one event-kind entry
// with a matching buffered event waiting (read-only — it does not consume the
// event or record a fire). The bridge engine's cooldown gate calls this so an
// event-eligible multi-source trigger can bypass the per-trigger cooldown when
// a real matching event is pending, while cron/idle re-eval stays cooldown-
// gated. Fixes the cooldown/event race (bug ka-0000005988): without this, a
// bead_event arriving mid-cooldown is buffered, never consumed (cooldown skips
// the trigger before Evaluate runs), then drained at cycle end.
func (s *MultiTriggerSource) EventReady(ctx context.Context, trig TriggerDef) bool {
	if len(trig.Triggers) == 0 {
		return false
	}
	data, _ := trig.SourceData.(multiTriggerData)

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

// CronDue reports whether this trigger has a cron entry due right now
// (in-window and past last_run; read-only — it consumes no state and records
// no fire). The bridge engine's cooldown gate calls this so a cron-due
// trigger can bypass the per-trigger cooldown — the cron sibling of
// EventReady. Without it, a cooldown that divides 24h evenly phase-locks the
// evaluation slots outside the cron's active window and the trigger never
// fires again while last_checked stays fresh (bug ka-0000004307).
func (s *MultiTriggerSource) CronDue(_ context.Context, trig TriggerDef) bool {
	if len(trig.Triggers) == 0 {
		return false
	}
	data, _ := trig.SourceData.(multiTriggerData)
	tctx := scheduler.TriggerEvalContext{
		TriggerID: trig.ID,
		Project:   trig.Project,
		Timezone:  data.Timezone,
		Triggers:  trig.Triggers,
		LastRun:   data.LastRun,
	}
	return scheduler.HasDueCronEntry(s.engine, s.logger, tctx, s.nowFunc())
}

// CatchUpKeys returns dedup keys persisted on prior issue beads created by
// this trigger. Seeds the bridge engine's seen map on startup so cron
// entries that already fired before restart don't re-fire.
func (s *MultiTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
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
		if key := bead.Fields["multi_fire_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// multiDecisionToMatch converts a FireDecision into a TriggerMatch. The Key
// shape is "<entry_idx>:<entry_label_or_idx>:<source_match_key>" per A4.
func multiDecisionToMatch(trig TriggerDef, decision scheduler.FireDecision) TriggerMatch {
	entry := trig.Triggers[decision.EntryIndex]
	label := entry.Label
	if label == "" {
		label = fmt.Sprintf("%d", decision.EntryIndex)
	}

	srcKey := multiSourceMatchKey(decision)
	key := fmt.Sprintf("%d:%s:%s", decision.EntryIndex, label, srcKey)

	fields := map[string]string{
		"multi_fire_key":   key,
		"multi_entry_idx":  fmt.Sprintf("%d", decision.EntryIndex),
		"multi_entry_kind": decision.Kind,
		"multi_cause":      decision.Cause,
		"multi_fire_time":  decision.Time.UTC().Format(time.RFC3339),
	}
	if entry.Label != "" {
		fields["multi_entry_label"] = entry.Label
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
		Title:        fmt.Sprintf("Multi[%s]: %s", decision.Kind, trig.Title),
		Fields:       fields,
		Labels:       []string{"source:multi", "multi-entry:" + label},
		TemplateVars: vars,
	}
}

// multiSourceMatchKey computes the source-specific match key portion of the
// dedup key for a FireDecision.
func multiSourceMatchKey(decision scheduler.FireDecision) string {
	switch decision.Kind {
	case scheduler.TriggerKindCron:
		return decision.Time.UTC().Format("2006-01-02T15:04")
	case scheduler.TriggerKindBeadEvent:
		if decision.Event != nil {
			// Every event is its own occurrence, keyed by when the source
			// took it in (a debounced burst by its last event). Without the
			// time, a bead's first update (or a plan's first steward nudge)
			// was the only one that ever fired: the seen set and the spawn
			// idempotency lookup (any agent bead with this multi_key, of any
			// age) swallowed every later one as already fired (gd-wRullKPlWJ).
			// The time is stamped once, when the SSE frame is dispatched, and
			// a reconnect resumes after the last dispatched id, so one
			// occurrence does not come back under a second key.
			return fmt.Sprintf("%s:%s@%s", decision.Event.EventType, decision.Event.BeadID, decision.Time.UTC().Format(time.RFC3339))
		}
	case scheduler.TriggerKindDecisionResolved:
		if decision.Event != nil {
			return decision.Event.BeadID
		}
	case multiFirstFireKind:
		return "first-fire"
	}
	return decision.Time.UTC().Format(time.RFC3339)
}

// beadEventToPayload converts a parsed BeadEvent into the map[string]any
// payload shape expected by conditions.Env for match expressions. Top-level
// columns are exposed alongside the bead's extensible Fields; labels are
// kept on the event itself (withLabels surfaces them at match time).
//
// approval_transition is "true" only on the event that approves an escalation
// (approvalTransition), never on a later write to an approved one, so an entry
// can wake on approval without waking on every cosmetic update (the
// escalation-coordinator trigger, gardenio/docs/escalation-coordinator-prompt.md;
// gd-TMYvBxNY6h). It is always set, so no bead field can supply it.
func beadEventToPayload(bead *BeadEvent) map[string]any {
	payload := map[string]any{
		"id":                  bead.ID,
		"type":                bead.Type,
		"title":               bead.Title,
		"status":              bead.Status,
		"assignee":            bead.Assignee,
		"created_by":          bead.CreatedBy,
		"priority":            bead.Priority,
		"approval_transition": strconv.FormatBool(bead.Type == "escalation" && approvalTransition(bead)),
	}
	if bead.ClosedBy != "" {
		payload["closed_by"] = bead.ClosedBy
	}
	// actor is who made this change (an OIDC email, or an agent name), so an
	// entry can tell a person's edit from an agent's (a steward wakes on a
	// person editing its notes, not on its own write; internal/steward).
	if bead.Actor != "" {
		payload["actor"] = bead.Actor
	}
	for k, v := range bead.Fields {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	return payload
}

// beadDetailToPayload converts a hydrated *beadsapi.BeadDetail into the
// map[string]any payload shape expected by conditions.Env for match
// expressions. It mirrors beadEventToPayload but is sourced from the daemon
// hydration response used by edge-event topics — those SSE payloads carry only
// ids, so we re-fetch the full bead to populate the columns and fields that
// condition matching needs. Labels are NOT placed in the payload here; they are
// passed separately as the OnBeadEvent labels argument, which the engine folds
// into payload["labels"] via withLabels so label: leaves match. Subtree
// matching uses the in_subtree_of daemon leaf, which walks ancestors of the
// bead id and so needs no parent column here.
func beadDetailToPayload(d *beadsapi.BeadDetail) map[string]any {
	payload := map[string]any{
		"id":         d.ID,
		"type":       d.Type,
		"title":      d.Title,
		"status":     d.Status,
		"assignee":   d.Assignee,
		"created_by": d.CreatedBy,
		"priority":   d.Priority,
	}
	if d.ClosedBy != "" {
		payload["closed_by"] = d.ClosedBy
	}
	for k, v := range d.Fields {
		if _, exists := payload[k]; !exists {
			payload[k] = v
		}
	}
	return payload
}

// beadHydrator is the minimal daemon surface needed to turn an edge-event bead
// id into a full payload: a single bead fetch. scheduler.TriggerDaemon (the
// type stored on the source for EvaluateTriggers) does not declare GetBead, but
// the concrete *beadsapi.Client does, so the edge handlers type-assert to this
// interface. Tests can stub it. Kept here so the bead-state source can reuse it.
type beadHydrator interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
}

// hydrateEdgeEvent fetches full bead detail for an edge-affected bead id and
// builds the payload + labels for OnBeadEvent. It fails open: a nil daemon, a
// daemon that cannot hydrate (no GetBead), a GetBead error, or a nil detail all
// yield ok=false so the caller skips firing an unmatchable id-only event rather
// than firing one that can never satisfy type/status/label/field conditions.
// The returned payload always carries the originating edge topic + event type
// so authors can match on those too.
func hydrateEdgeEvent(ctx context.Context, daemon scheduler.TriggerDaemon, logger *slog.Logger, id, topic, eventType string) (payload map[string]any, labels []string, ok bool) {
	hydrator, canHydrate := daemon.(beadHydrator)
	if daemon == nil || !canHydrate {
		logger.Debug("edge event: no hydrator available; skipping unmatchable id-only event",
			"bead_id", id, "topic", topic)
		return nil, nil, false
	}
	detail, err := hydrator.GetBead(ctx, id)
	if err != nil || detail == nil {
		logger.Debug("edge event: hydration failed; skipping id-only event",
			"bead_id", id, "topic", topic, "error", err)
		return nil, nil, false
	}
	payload = beadDetailToPayload(detail)
	payload["topic"] = topic
	payload["edge_event"] = eventType
	return payload, detail.Labels, true
}
