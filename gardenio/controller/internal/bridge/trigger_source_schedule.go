package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// ScheduleTriggerSource implements TriggerSource for enhanced schedule triggers.
// Unlike the standalone scheduler package (which manages schedule beads directly),
// this source integrates cron-based triggers into the shared TriggerEngine with
// support for pre-flight conditions: the trigger fires at cron time only if all
// conditions are met (e.g., ready queue has items, no maintenance gate open,
// no agent already working on a role).
type ScheduleTriggerSource struct {
	logger *slog.Logger
	// nowFunc allows tests to control the clock. Defaults to time.Now.
	nowFunc func() time.Time
	// healBackoff is the bounded retry applied to the self-heal sweeps' own
	// list queries (gd-niQPoeR7ja). Defaults to healListBackoff(); tests
	// shrink it so a deliberately-failing daemon does not cost real seconds.
	healBackoff BackoffConfig

	mu     sync.Mutex
	daemon TriggerBeadClient // cached from the most recent LoadTriggers call
}

// scheduleTriggerData holds schedule-specific fields parsed from a trigger bead.
type scheduleTriggerData struct {
	// Cron is the 5-field cron expression (e.g., "*/30 9-17 * * 1-5").
	Cron string
	// Timezone is the IANA timezone for cron evaluation (e.g., "America/Los_Angeles").
	// Empty means UTC.
	Timezone string
	// LastRun is the timestamp of the last successful fire.
	LastRun time.Time
	// LastMatch is the fireKey (minute-granularity match string) of the last
	// successfully fired cron match, persisted alongside last_run. Evaluate
	// skips any candidate fireKey <= LastMatch so a restart can never re-fire
	// a match that already spawned, independent of the LastRun timestamp
	// comparison (gd-pLf5LSHDEh).
	LastMatch string
	// Conditions is a list of pre-flight checks that must all pass before firing.
	Conditions []scheduler.Condition
}

// NewScheduleTriggerSource creates a new schedule trigger source.
func NewScheduleTriggerSource(logger *slog.Logger) *ScheduleTriggerSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &ScheduleTriggerSource{
		logger:      logger,
		nowFunc:     time.Now,
		healBackoff: healListBackoff(),
	}
}

func (s *ScheduleTriggerSource) Name() string { return "schedule" }

// LoadTriggers fetches all enabled trigger beads with label "trigger:schedule".
func (s *ScheduleTriggerSource) LoadTriggers(ctx context.Context, daemon TriggerBeadClient) ([]TriggerDef, error) {
	// Cache daemon reference for condition checks during Evaluate.
	s.mu.Lock()
	s.daemon = daemon
	s.mu.Unlock()

	// Self-heal wedged gardener triggers before the normal open-only load
	// (ka-0000006681). A schedule trigger bead is only ever fired by the
	// engine, which never moves it out of status=open — so an in_progress
	// schedule trigger is always an anomaly: a spawned agent (or some other
	// actor) erroneously claimed the trigger bead itself and then died
	// without releasing it. With the load query filtered to status=open the
	// trigger silently drops out of the scheduler forever — last_checked
	// freezes and the gardener goes dark with no automatic recovery (this is
	// exactly what stranded the investigative-janitor for ~3 days, 6/1–6/4).
	// healWedgedTriggers releases such beads back to open so they re-enter
	// the rotation on this same pass.
	s.healWedgedTriggers(ctx, daemon)

	// Self-heal STALLED open gardener triggers (ka-0000003265). The wedged heal
	// above only covers status=in_progress anomalies. A separate failure mode
	// keeps a gardener trigger at status=open + enabled=true yet frozen: a
	// no_progress_for premature stop deadlocks in gardenerResumeOrEscalate's
	// bare-reconcile no-op (the re-arm resets ProgressState, but
	// gardenerFinalizeRunsFromIterations rebuilds the same zero-score tail from
	// the immutable closed-iteration history next cycle, so no_progress_for
	// re-matches forever). evalOne returns from the pre-fire stop gate before
	// stamping last_checked, so the trigger goes dark while still open + enabled —
	// disable/enable does NOT recover it because the trap is derived from
	// immutable history, not status. This heal detects that exact fingerprint and
	// applies a durable reset so the gardener fires a genuinely fresh window.
	//
	// The heal lists the same open trigger:schedule set this load needs; when it
	// wrote nothing that list is current and is reused, which saves one ~550 KB
	// read per cycle (gd-6PXLuR3juD).
	result := s.healStalledOpenTriggers(ctx, daemon)
	if result == nil {
		var err error
		result, err = daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:    []string{"trigger"},
			Statuses: []string{"open"},
			Labels:   []string{"trigger:schedule"},
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

		// Skip beads-viewer-e2e schedule fixtures (ka-0000005435). They are
		// seeded enabled=true into the shared prod daemon so the viewer specs
		// have live rows, but firing them spawns throwaway crew agents on the
		// fixture's cron. They still render enabled in beads-viewer — only the
		// scheduler firing is suppressed.
		if isE2EFixture(bead) {
			s.logger.Info("schedule trigger: skipping E2E fixture (never fires in prod)",
				"trigger", bead.ID, "title", bead.Title)
			continue
		}

		t := ParseTriggerDef(bead, "schedule")

		// Mis-config guardrail (ka-0000002562): a trigger:schedule bead only
		// fires its cron expression — the triggers[] array (bead_event,
		// decision_resolved, cron-with-preflight entries) is the exclusive
		// domain of trigger:multi / schedule-mirror sources and is silently
		// dropped here. Warn loudly so the mis-label is discoverable rather
		// than producing a gardener that never fires its event wakes. Emitted
		// before the cron skip so a bead with triggers[] but no cron (which
		// otherwise fires from nothing) is still surfaced.
		if len(t.Triggers) > 0 {
			s.logger.Warn("schedule trigger: triggers[] array ignored — relabel trigger:schedule -> trigger:multi to fire event wakes",
				"trigger", bead.ID, "entries", len(t.Triggers))
		}

		cronExpr := bead.Fields["cron"]
		if cronExpr == "" {
			s.logger.Warn("schedule trigger: missing cron expression, skipping",
				"trigger", bead.ID)
			continue
		}

		data := scheduleTriggerData{
			Cron:     cronExpr,
			Timezone: bead.Fields["timezone"],
		}

		// Parse last_run timestamp.
		if raw := bead.Fields["last_run"]; raw != "" {
			for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
				if parsed, err := time.Parse(layout, raw); err == nil {
					data.LastRun = parsed
					break
				}
			}
		}

		// Parse last_match, the fireKey of the last fired match.
		data.LastMatch = bead.Fields["last_match"]

		// Parse conditions from JSON array field.
		if cond := bead.Fields["conditions"]; cond != "" {
			conditions, err := scheduler.ParseConditions(cond)
			if err != nil {
				s.logger.Warn("schedule trigger: invalid conditions JSON",
					"trigger", bead.ID, "error", err)
				continue
			}
			data.Conditions = conditions
		}

		t.SourceData = data
		triggers = append(triggers, t)
	}
	return triggers, nil
}

// healListBackoff is the retry applied to the self-heal sweeps' list queries
// (gd-niQPoeR7ja). Deliberately BOUNDED, unlike the startup catch-up's
// retry-forever: these sweeps run inside LoadTriggers on every poll cycle, so a
// still-unreachable daemon gets a free retry in poll_interval seconds anyway,
// and blocking the evaluation cycle on an open-ended retry would starve every
// other source. Short attempts ride out the sub-second blip (a daemon rollout,
// a DNS hiccup) that used to make the whole sweep silently skip a cycle.
func healListBackoff() BackoffConfig {
	return BackoffConfig{
		Initial:     250 * time.Millisecond,
		Max:         2 * time.Second,
		Factor:      2,
		MaxAttempts: 3,
	}
}

// wedgedTriggerStaleAfter is how long a schedule trigger bead may sit in
// status=in_progress before the self-heal treats it as wedged and releases it
// back to open. The engine never parks a schedule trigger in_progress, so the
// only thing that puts one there is an erroneous external claim (a spawned
// agent claiming the trigger bead instead of its own task bead, then dying).
// The grace window is generous so the heal never races a legitimate, transient
// claim that some future code path might introduce — a genuinely wedged trigger
// has typically been stuck for hours by the time anyone notices.
const wedgedTriggerStaleAfter = 30 * time.Minute

// healWedgedTriggers finds schedule trigger beads stuck in status=in_progress
// and releases them back to open so the scheduler resumes evaluating them.
//
// Rationale (ka-0000006681): a `trigger:schedule` gardener is fired exclusively
// by the TriggerEngine, which leaves the trigger bead in status=open across
// every cycle — the per-run *task* bead is what carries the in_progress run,
// not the trigger itself. So an in_progress trigger bead is always an anomaly:
// an actor outside the engine (almost always a spawned agent that claimed the
// gardener bead by mistake, or a `gb claim` against the wrong id) moved it to
// in_progress and never released it. Because LoadTriggers filters to
// status=open, the wedged trigger falls out of the scheduler entirely, its
// last_checked freezes, and the gardener goes permanently dark with no recovery
// path short of a human running `kd update --status open`. This sweep is that
// recovery path, made automatic.
//
// Guardrails:
//   - enabled=="false" triggers are left alone (operator deliberately paused).
//   - A trigger is only released once last_checked is older than
//     wedgedTriggerStaleAfter, so a freshly-claimed bead (if some future path
//     ever legitimately claims one) gets a grace window before being reset.
//     A missing/unparseable last_checked is treated as stale (the field freezes
//     precisely when the wedge starts, so absence means "long abandoned").
//
// All failures are logged and swallowed: the heal is best-effort and must never
// block the normal trigger load.
func (s *ScheduleTriggerSource) healWedgedTriggers(ctx context.Context, daemon TriggerBeadClient) {
	healWedgedTriggers(ctx, daemon, s.logger, s.nowFunc, "trigger:schedule", s.healBackoff)
}

// healWedgedTriggers is the label-parameterized core of the wedged-trigger heal
// (ka-0000006681). It is shared by ScheduleTriggerSource (label=trigger:schedule)
// and MultiTriggerSource (label=trigger:multi), as healStalledOpenGardeners is:
// an event-mode plan gardener (PR #1907) is trigger:multi, its loader lists
// status=open only, and without this heal an erroneous claim that left it
// in_progress dropped it out of the engine silently (gd-7XjRN5uCUW). The engine
// never parks a trigger:multi bead in_progress either, so the same reasoning,
// guardrails and release apply whichever label loaded the trigger; see the doc
// comment on ScheduleTriggerSource.healWedgedTriggers.
func healWedgedTriggers(ctx context.Context, daemon TriggerBeadClient, logger *slog.Logger, nowFunc func() time.Time, label string, backoff BackoffConfig) {
	// Retry the list on failure (gd-niQPoeR7ja): a single transient error used
	// to skip the whole heal for the cycle, so a wedged gardener stayed dark an
	// extra poll interval for what was often a sub-second daemon blip. The
	// retry is always bounded — a zero-value config (a source built without
	// the constructor) would otherwise mean "retry forever", which inside an
	// evaluation cycle is a wedge, not a fix.
	if backoff.MaxAttempts <= 0 {
		backoff = healListBackoff()
	}
	var result *beadsapi.ListBeadsResult
	err := RetryWithBackoff(ctx, logger, "schedule trigger: wedged-trigger heal list", backoff,
		func(ctx context.Context) error {
			var err error
			result, err = daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
				Types:    []string{"trigger"},
				Statuses: []string{"in_progress"},
				Labels:   []string{label},
				Limit:    100,
			})
			return err
		})
	if err != nil {
		logger.Warn("schedule trigger: wedged-trigger heal list failed", "label", label, "error", err)
		return
	}

	now := nowFunc()
	for _, bead := range result.Beads {
		if bead == nil {
			continue
		}
		if bead.Fields["enabled"] == "false" {
			// Operator-paused; leave it parked exactly as they left it.
			continue
		}

		lastChecked := bead.Fields["last_checked"]
		if !wedgedTriggerIsStale(lastChecked, now) {
			logger.Info("schedule trigger: in_progress trigger not yet stale, deferring heal",
				"trigger", bead.ID, "label", label, "last_checked", lastChecked)
			continue
		}

		open := "open"
		clearAssignee := ""
		if err := daemon.UpdateBead(ctx, bead.ID, beadsapi.UpdateBeadRequest{
			Status:   &open,
			Assignee: &clearAssignee,
		}); err != nil {
			logger.Warn("schedule trigger: wedged-trigger heal release failed",
				"trigger", bead.ID, "label", label, "error", err)
			continue
		}

		logger.Warn("schedule trigger: released wedged in_progress trigger back to open",
			"trigger", bead.ID, "title", bead.Title, "label", label,
			"prev_assignee", bead.Assignee, "last_checked", lastChecked)

		// Leave an audit trail on the bead so the recovery is discoverable
		// (and the fleet-liveness guard's alert thread has a closing note).
		// Best-effort: a failed comment must not undo the release.
		note := fmt.Sprintf(
			"auto-heal: released wedged %s trigger from status=in_progress (assignee=%q, last_checked=%s, stale>%s) back to open so the scheduler resumes evaluating it. An in_progress %s trigger is always an erroneous claim — the engine never parks one there. (ka-0000006681)",
			label, bead.Assignee, lastChecked, wedgedTriggerStaleAfter, label)
		if err := daemon.AddComment(ctx, bead.ID, "trigger-engine", note); err != nil {
			logger.Warn("schedule trigger: wedged-trigger heal comment failed",
				"trigger", bead.ID, "label", label, "error", err)
		}
	}
}

// wedgedTriggerIsStale reports whether a trigger's last_checked timestamp is old
// enough (or absent/unparseable) to treat the in_progress trigger as wedged. An
// empty or unparseable value is stale: last_checked stops advancing the moment a
// trigger drops out of the scheduler, so its absence means "abandoned long ago".
func wedgedTriggerIsStale(lastChecked string, now time.Time) bool {
	if lastChecked == "" {
		return true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, lastChecked); err == nil {
			return now.Sub(parsed) >= wedgedTriggerStaleAfter
		}
	}
	return true
}

// stalledOpenTriggerStaleFactor is N in "last_checked must be older than N× the
// gardener's cron cadence" before healStalledOpenTriggers treats an open+enabled
// gardener trigger as stalled. Deliberately more generous than the liveness
// sweep's alert factor (3) so the heal — which mutates the gardener — only acts on
// a gardener that has missed many cron ticks, never one merely between ticks or
// briefly slow.
const stalledOpenTriggerStaleFactor = 6

// stalledOpenTriggerMinStale floors the computed stale threshold so a gardener
// with a very tight cron is not healed on a handful of slow polls.
const stalledOpenTriggerMinStale = time.Hour

// stalledOpenTriggerMaxStale caps the computed stale threshold so a slow cron
// cannot defer the heal indefinitely (ka-0000005387). Without the cap, 6× a
// WEEKLY cron's cadence is 42 days — ka-0000005781 sat in the deadlock 19 days
// and missed two Monday fires while the heal (correctly, per its own math)
// declined to act. The cap is safe regardless of cadence: a healthy enabled
// gardener has last_checked re-stamped every evaluate cycle (minutes, not
// cadence), and every legitimate freeze — operator-paused, dormant, blocked_by
// an open decision, one_spawn with a live run — is excluded by the fingerprint
// checks, so two weeks frozen on a fingerprint match is unambiguous deadlock.
const stalledOpenTriggerMaxStale = 14 * 24 * time.Hour

// healStalledOpenTriggers recovers schedule gardener triggers stuck at
// status=open + enabled=true with a frozen last_checked (ka-0000003265).
//
// This is the sibling of healWedgedTriggers for a DIFFERENT failure mode.
// healWedgedTriggers releases an erroneously-claimed in_progress trigger back to
// open; here the trigger is ALREADY open and never dropped out of the load query.
// What froze it is a no_progress_for pre-fire DEADLOCK:
//
//   - a no_progress_for premature stop matched and re-armed the gardener, which
//     reset ProgressState to give it a clean window;
//   - but gardenerFinalizeRunsFromIterations rebuilds ProgressState.Runs from the
//     gardener's IMMUTABLE closed-iteration history on the next cycle, restoring
//     the same zero-score tail, so no_progress_for re-matches immediately;
//   - gardenerResumeOrEscalate then hits its bare-reconcile no-op (RunsSpawned
//     hasn't advanced past LastResumeRunsSpawned) and returns without counting a
//     resume or escalating;
//   - gardenerPreFireCheck returns skip, so evalOne returns BEFORE stamping
//     last_checked. RunsSpawned never advances (the gardener never fires past the
//     stop gate), so the bare-reconcile guard never releases. The gardener is dark
//     forever, still open + enabled.
//
// `gb trigger disable` then `enable` does NOT recover it: the trap is derived
// from immutable iteration history, not from status or the enabled flag.
//
// This heal detects that exact fingerprint and applies the SAME durable reset the
// fixed no_progress_for re-arm now applies (gardener_evaluator.go): clear
// ProgressState and stamp RunsFinalizedAfter=now so finalize skips the pre-reset
// zero iterations. It also clears the resume bookkeeping and re-stamps
// last_checked so the recovery is atomic and immediately visible. On the next
// cycle no_progress_for is not met (empty window), the gardener fires a genuinely
// fresh run, and if it keeps scoring zero its escalation is bounded by
// ResumesWithoutProgress as designed.
//
// Fingerprint (all must hold) — deliberately narrow so the heal targets ONLY the
// diagnosed deadlock and never churns a healthy gardener:
//   - carries a gardener block (only gardeners run the no_progress_for loop);
//   - enabled != "false" (operator-paused gardeners are left exactly as parked);
//   - not an E2E fixture (never fires by design; last_checked intentionally frozen);
//   - not dormant_on_escalation (intentionally frozen while awaiting the operator);
//   - NOT blocked_by an OPEN decision/escalation, and NOT a one_spawn_at_a_time
//     gardener with a still-running agent. These are the OTHER two evalOne gates
//     that pause a gardener WITHOUT stamping last_checked (triggers.go blocked_by
//     gate; one_spawn gate) — so a gardener sitting behind either freezes
//     last_checked while perfectly alive. Critically, neither gate clears the
//     resume marker: a gardener that re-armed on no_progress_for and THEN became
//     blocked_by an open decision keeps LastResumeReason=="no_progress_for" with
//     RunsSpawned pinned at LastResumeRunsSpawned indefinitely (the blocked_by gate
//     returns before the pre-fire check, so nothing advances RunsSpawned or clears
//     the marker). It is therefore NOT naturally excluded by the fingerprint below
//     and MUST be skipped explicitly — otherwise it gets ProgressState wiped while
//     healthily waiting on a human. This skip set is the authoritative no-stamp set
//     the read-only liveness sweep already enforces (gardener_liveness.go:236-256);
//     the mutating heal mirrors it exactly via triggerBlockedBy /
//     gardenerAgentStillRunning so the two sweeps agree on which gardeners are dark.
//   - LastResumeReason == "no_progress_for" AND RunsSpawned <= LastResumeRunsSpawned
//     — the bare-reconcile signature: re-armed on no-progress but never fired again.
//   - last_checked older than N× the cron cadence (floored).
//
// The reset clears LastResumeReason, so a healed gardener no longer matches the
// fingerprint — the heal is idempotent (one write per deadlock episode, no churn).
// All failures are logged and swallowed: best-effort, never blocks the load.
// Returns the open-trigger list for reuse, as healStalledOpenGardeners does.
func (s *ScheduleTriggerSource) healStalledOpenTriggers(ctx context.Context, daemon TriggerBeadClient) *beadsapi.ListBeadsResult {
	return healStalledOpenGardeners(ctx, daemon, s.logger, s.nowFunc, "trigger:schedule", s.healBackoff)
}

// healStalledOpenGardeners is the label-parameterized core of the no_progress_for
// deadlock heal (ka-0000003265). It is shared by ScheduleTriggerSource
// (label=trigger:schedule) and MultiTriggerSource (label=trigger:multi) so BOTH
// gardener shapes recover from the deadlock — the release/delivery gardeners are
// trigger:multi and were previously invisible to the schedule-only heal, which
// left them dark for ~24h (bug ka-0000003498). See the doc comment on
// healStalledOpenTriggers for the full failure-mode analysis; the fingerprint
// and durable-reset logic below are identical regardless of which label loaded
// the gardener.
//
// It returns the open `label` trigger list it read when it healed nothing, so
// the caller's load can reuse it instead of listing the same rows again
// (gd-6PXLuR3juD). It returns nil when the list failed or a trigger was
// written, and the caller then lists afresh.
func healStalledOpenGardeners(ctx context.Context, daemon TriggerBeadClient, logger *slog.Logger, nowFunc func() time.Time, label string, backoff BackoffConfig) *beadsapi.ListBeadsResult {
	// Same bounded retry as the wedged heal (gd-niQPoeR7ja): ride out a blip
	// rather than skipping the cycle's heal entirely. Always bounded, for the
	// reason healWedgedTriggers spells out.
	if backoff.MaxAttempts <= 0 {
		backoff = healListBackoff()
	}
	var result *beadsapi.ListBeadsResult
	err := RetryWithBackoff(ctx, logger, "schedule trigger: stalled-open heal list", backoff,
		func(ctx context.Context) error {
			var err error
			result, err = daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
				Types:    []string{"trigger"},
				Statuses: []string{"open"},
				Labels:   []string{label},
				Limit:    100,
			})
			return err
		})
	if err != nil {
		logger.Warn("schedule trigger: stalled-open heal list failed", "label", label, "error", err)
		return nil
	}

	now := nowFunc()
	wrote := false
	for _, bead := range result.Beads {
		if bead == nil {
			continue
		}
		if bead.Fields["enabled"] == "false" {
			continue // operator-paused; leave parked.
		}
		if isE2EFixture(bead) {
			continue // never fires by design; last_checked intentionally frozen.
		}
		if bead.Fields["dormant_on_escalation"] == "true" {
			continue // parked awaiting the operator; frozen by design.
		}

		raw := bead.Fields["gardener"]
		if raw == "" {
			continue // only gardeners run the no_progress_for loop.
		}
		var cap GardenerSpec
		if err := json.Unmarshal([]byte(raw), &cap); err != nil {
			logger.Warn("schedule trigger: stalled-open heal gardener parse failed",
				"trigger", bead.ID, "error", err)
			continue
		}

		// Bare-reconcile deadlock fingerprint: re-armed on no_progress_for but
		// never fired again since the re-arm.
		if cap.LastResumeReason != scheduler.StopKindNoProgressFor {
			continue
		}
		if cap.RunsSpawned > cap.LastResumeRunsSpawned {
			continue // the gardener has fired since the re-arm — not deadlocked.
		}

		// Only act once the freeze is unambiguous: last_checked older than N× the
		// cron cadence. A missing/unparseable last_checked on a gardener matching
		// the deadlock fingerprint is treated as stale (it froze the moment the
		// deadlock formed).
		lastChecked := bead.Fields["last_checked"]
		if !stalledOpenTriggerIsStale(lastChecked, bead, now) {
			continue
		}

		// Skip the two OTHER evalOne gates that freeze last_checked WITHOUT the
		// gardener being dark — the authoritative no-stamp set the read-only
		// liveness sweep already enforces (gardener_liveness.go:236-256). Neither
		// gate advances RunsSpawned or clears the no_progress_for resume marker, so
		// a gardener parked behind either still matches the deadlock fingerprint
		// above. Without these skips the heal would wipe ProgressState on a gardener
		// that is healthily waiting on a human (blocked_by) or on a live prior run
		// (one_spawn) — the exact false-positive the mutating heal must avoid.
		//
		//   1. blocked_by an OPEN decision/escalation. triggerBlockedBy is a
		//      *TriggerEngine method the heal (a *ScheduleTriggerSource method)
		//      cannot call, so mirror its semantics — GetBead + isOpenStatus, errors
		//      downgraded to not-blocking — against the daemon client here.
		//   2. one_spawn_at_a_time with a still-running LastAgentID.
		//   3. max_concurrent_runs: the single-live-instance default (a gardener
		//      deferring while a prior run is still spawning/working — bug
		//      ka-0000003097), the general form of #2 that also freezes last_checked
		//      without the gardener being dark.
		if blocked := parseStringList(bead.Fields["blocked_by"]); len(blocked) > 0 &&
			scheduleTriggerBlockedByOpen(ctx, daemon, logger, bead.ID, blocked) {
			continue
		}
		if cap.OneSpawnAtATime && cap.LastAgentID != "" &&
			gardenerAgentStillRunning(ctx, daemon, logger, cap.LastAgentID, leaseStaleHoursField(bead.Fields), now) {
			continue
		}
		if gardenerAtConcurrentRunsCap(ctx, daemon, logger, ParseTriggerDef(bead, label), now) {
			continue
		}

		// Durable un-stick: mirror the fixed no_progress_for re-arm's reset so the
		// gardener fires a genuinely fresh window instead of re-matching the stale
		// zero tail. RunsFinalizedAfter is the load-bearing field — clearing
		// ProgressState alone would be undone by gardenerFinalizeRunsFromIterations
		// on the next cycle.
		updated := cap
		updated.ProgressState = nil
		updated.RunsFinalizedAfter = now
		updated.ResumesWithoutProgress = 0
		updated.LastResumeReason = ""
		updated.LastResumeRunsSpawned = 0

		blob, mErr := json.Marshal(updated)
		if mErr != nil {
			logger.Warn("schedule trigger: stalled-open heal marshal failed",
				"trigger", bead.ID, "error", mErr)
			continue
		}
		wrote = true
		if err := daemon.UpdateBeadFields(ctx, bead.ID, map[string]string{
			"gardener":     string(blob),
			"last_checked": now.UTC().Format(time.RFC3339),
		}); err != nil {
			logger.Warn("schedule trigger: stalled-open heal update failed",
				"trigger", bead.ID, "error", err)
			continue
		}

		logger.Warn("schedule trigger: healed stalled open gardener (no_progress_for deadlock)",
			"trigger", bead.ID, "title", bead.Title, "last_checked", lastChecked,
			"runs_spawned", cap.RunsSpawned, "last_resume_runs_spawned", cap.LastResumeRunsSpawned)

		note := fmt.Sprintf(
			"auto-heal: recovered stalled open schedule gardener from a no_progress_for deadlock (last_checked=%s frozen; runs_spawned=%d ≤ last_resume_runs_spawned=%d). "+
				"The re-arm reset ProgressState but gardenerFinalizeRunsFromIterations rebuilt the same zero-score tail from immutable iteration history every cycle, so no_progress_for re-matched forever and evalOne returned before stamping last_checked. "+
				"Applied a durable reset (ProgressState cleared, runs_finalized_after stamped) so the gardener fires a fresh no-progress window; further no-progress now escalates in a bounded number of resumes. (ka-0000003265)",
			lastChecked, cap.RunsSpawned, cap.LastResumeRunsSpawned)
		if err := daemon.AddComment(ctx, bead.ID, "trigger-engine", note); err != nil {
			logger.Warn("schedule trigger: stalled-open heal comment failed",
				"trigger", bead.ID, "error", err)
		}
	}
	if wrote {
		return nil
	}
	return result
}

// scheduleTriggerBlockedByOpen reports whether ANY of the referenced bead IDs is
// still open (i.e. still blocking the gardener). It is the *ScheduleTriggerSource
// analogue of TriggerEngine.triggerBlockedBy (triggers.go): the heal is a
// ScheduleTriggerSource method and cannot call the engine method, so it mirrors
// the same semantics against the daemon client it already holds —
//
//   - each referenced bead is fetched via GetBead; a bead with status open /
//     in_progress (isOpenStatus) counts as blocking;
//   - a GetBead error or a missing/nil bead is downgraded to "not blocking" so a
//     daemon hiccup or a deleted decision never wedges recovery. This is the same
//     fail-open posture triggerBlockedBy takes for the same reason.
//
// Returns true on the FIRST open reference (short-circuit) — the heal only needs
// to know "blocked or not", not the full pending set.
func scheduleTriggerBlockedByOpen(ctx context.Context, daemon TriggerBeadClient, logger *slog.Logger, triggerID string, blockedBy []string) bool {
	for _, id := range blockedBy {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		bead, err := daemon.GetBead(ctx, id)
		if err != nil {
			logger.Warn("schedule trigger: stalled-open heal blocked_by lookup failed",
				"trigger", triggerID, "blocked_by", id, "error", err)
			continue
		}
		if bead == nil {
			continue
		}
		if isOpenStatus(bead.Status) {
			return true
		}
	}
	return false
}

// stalledOpenTriggerIsStale reports whether an open gardener trigger's
// last_checked is old enough — older than N× its cron cadence, floored and
// capped — to treat as stalled. An empty or unparseable value is stale
// (last_checked freezes the moment the deadlock forms). A gardener with
// no/invalid cron uses the flat floor.
func stalledOpenTriggerIsStale(lastChecked string, bead *beadsapi.BeadDetail, now time.Time) bool {
	threshold := max(time.Duration(stalledOpenTriggerStaleFactor)*gardenerCadence(bead, now), stalledOpenTriggerMinStale)
	threshold = min(threshold, stalledOpenTriggerMaxStale)
	if lastChecked == "" {
		return true
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if parsed, err := time.Parse(layout, lastChecked); err == nil {
			return now.Sub(parsed) >= threshold
		}
	}
	return true
}

// Evaluate checks each schedule trigger's cron expression against the current time.
// If a cron fire time has passed since the last run and all pre-flight conditions
// are met, the trigger produces a match.
func (s *ScheduleTriggerSource) Evaluate(ctx context.Context, trig TriggerDef) ([]TriggerMatch, error) {
	data, ok := trig.SourceData.(scheduleTriggerData)
	if !ok {
		return nil, nil
	}

	now := s.nowFunc()

	// Parse cron expression.
	expr, err := scheduler.ParseCron(data.Cron)
	if err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", data.Cron, err)
	}

	// Resolve timezone.
	loc := time.UTC
	if data.Timezone != "" {
		parsed, err := time.LoadLocation(data.Timezone)
		if err != nil {
			s.logger.Warn("schedule trigger: invalid timezone, using UTC",
				"trigger", trig.ID, "timezone", data.Timezone, "error", err)
		} else {
			loc = parsed
		}
	}

	nowLocal := now.In(loc)

	// Check if we're within the schedule's active window (hour/day/month match).
	if !expr.InWindow(nowLocal) {
		return nil, nil
	}

	// Find the most recent fire time before now.
	lastFire := expr.Prev(nowLocal).UTC()
	if lastFire.IsZero() {
		return nil, nil
	}

	// If last_run is after or equal to the most recent fire time, skip.
	if !data.LastRun.IsZero() && !data.LastRun.Before(lastFire) {
		return nil, nil
	}

	// Build a unique key for this fire: fire time (minute-granularity).
	fireKey := lastFire.Format("2006-01-02T15:04")

	// If this exact match already fired, skip — independent of the LastRun
	// timestamp check above. last_match is a string-exact record of the fired
	// fireKey (lexicographically ordered by construction), so this catches
	// restarts where the in-memory seen-set was lost and LastRun alone proved
	// insufficient (gd-pLf5LSHDEh).
	if data.LastMatch != "" && fireKey <= data.LastMatch {
		return nil, nil
	}

	// Cron time has passed — check pre-flight conditions. Fail-closed: any
	// evaluator error blocks the fire (so a flaky daemon can't auto-spawn).
	daemon := s.getDaemon()
	if daemon == nil {
		s.logger.Warn("schedule trigger: no daemon cached, skipping",
			"trigger", trig.ID)
		return nil, nil
	}
	met, err := scheduler.EvaluateConditions(ctx, daemon, data.Conditions, trig.Project)
	if err != nil {
		s.logger.Warn("schedule trigger: condition evaluation failed, skipping",
			"trigger", trig.ID, "error", err)
		return nil, nil
	}
	if !met {
		s.logger.Debug("schedule trigger: conditions not met, skipping",
			"trigger", trig.ID, "title", trig.Title)
		return nil, nil
	}

	vars := map[string]string{
		"{{trigger_id}}":      trig.ID,
		"{{trigger_title}}":   trig.Title,
		"{{cron}}":            data.Cron,
		"{{timezone}}":        data.Timezone,
		"{{fire_time}}":       lastFire.Format(time.RFC3339),
		"{{fire_time_local}}": lastFire.In(loc).Format("2006-01-02 15:04"),
	}

	fields := map[string]string{
		"schedule_fire_key":  fireKey,
		"schedule_fire_time": lastFire.Format(time.RFC3339),
		"schedule_cron":      data.Cron,
	}

	return []TriggerMatch{{
		Key:          fireKey,
		Title:        fmt.Sprintf("Schedule: %s", trig.Title),
		Fields:       fields,
		Labels:       []string{"source:schedule"},
		TemplateVars: vars,
	}}, nil
}

// CronDue reports whether this trigger's cron is due right now (in-window and
// past last_run; read-only). The bridge engine's cooldown gate calls this so
// a cron-due schedule trigger can bypass the per-trigger cooldown — without
// it, a cooldown that divides 24h evenly phase-locks the evaluation slots
// outside the cron's active window and the trigger never fires again while
// last_checked stays fresh (bug ka-0000004307). Pre-flight conditions are
// deliberately NOT evaluated here (they need daemon queries; Evaluate applies
// them fail-closed) — a due-but-condition-blocked trigger costs one extra
// evaluation per engine cycle for the duration of the window.
func (s *ScheduleTriggerSource) CronDue(_ context.Context, trig TriggerDef) bool {
	data, ok := trig.SourceData.(scheduleTriggerData)
	if !ok || data.Cron == "" {
		return false
	}
	expr, err := scheduler.ParseCron(data.Cron)
	if err != nil {
		return false
	}
	loc := time.UTC
	if data.Timezone != "" {
		if parsed, err := time.LoadLocation(data.Timezone); err == nil {
			loc = parsed
		}
	}
	nowLocal := s.nowFunc().In(loc)
	if !expr.InWindow(nowLocal) {
		return false
	}
	lastFire := expr.Prev(nowLocal).UTC()
	if lastFire.IsZero() {
		return false
	}
	return data.LastRun.IsZero() || data.LastRun.Before(lastFire)
}

// CatchUpKeys returns dedup keys for existing beads created by this trigger.
func (s *ScheduleTriggerSource) CatchUpKeys(ctx context.Context, daemon TriggerBeadClient, triggerID string) ([]string, error) {
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
		if key := bead.Fields["schedule_fire_key"]; key != "" {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// FireTaskKeyField names the task-bead field CatchUpKeys reads, opting the
// schedule source into orphaned-fire recovery (fireTaskKeyedSource,
// gd-CZnK7jeKSG). Evaluate stops producing a window once last_run / last_match
// record its fire, so the recovery lookups run only for a window deduped
// without a recorded fire.
func (s *ScheduleTriggerSource) FireTaskKeyField() string { return "schedule_fire_key" }

// getDaemon returns the cached daemon reference (set during LoadTriggers).
func (s *ScheduleTriggerSource) getDaemon() TriggerBeadClient {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.daemon
}
