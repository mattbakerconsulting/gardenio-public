package bridge

// Per-trigger cooldown-starvation SYMPTOM detector (Alert B of epic
// ka-0000005107, task ka-0000000052).
//
// The 2026-07-01 jira-bridge outage: a sandbox bridge misconfigured onto the
// prod beads daemon stamped last_checked on prod trigger beads ~10s before
// every prod poll, so the prod engine cooldown-skipped 414 consecutive cycles
// (~13h) without a single fire and without a page. Alert A
// (ServiceFieldForeignFleetWrite, shipped in MR !5055) detects that ROOT
// CAUSE — a foreign fleet writing service-managed fields. This detector is
// the SYMPTOM side: it notices a trigger that never wins a fire window at
// all, regardless of cause (foreign stamper, clock skew, a future cooldown
// bypass bug), and would have caught the outage in ~15m instead of 13h.
//
// Mechanism: evalOne reports every cycle outcome for each trigger to this
// detector. A cycle that ends at the cooldown gate ("skipped (cooldown)")
// grows the trigger's consecutive-skip streak; a cycle that gets PAST the
// gate resets it — whatever happened next (fired / no-new-matches /
// dedup-suppressed / blocked_by / evaluation error), the engine HAD its fire
// window, so it was not starved. The only way to build a long streak is for
// last_checked to keep advancing without this engine ever getting through:
// the engine's own stamps release the gate after effectiveCooldown, so a
// streak lasting far beyond the trigger's own cooldown means some OTHER
// writer keeps re-arming it.
//
// A trigger is STARVED once its streak crosses BOTH thresholds:
//   - starveMinCycles consecutive cooldown-skips (default 30), and
//   - max(starveMinDuration, starveCooldownMult × effectiveCooldown) of
//     wall-clock time (default max(15m, 3×cooldown)). The multiplier keeps a
//     long-cooldown trigger (e.g. cooldown_minutes=30 polled every 60s, which
//     legitimately skips ~29 of 30 cycles on its own stamp) from ever false
//     positive: a self-stamped streak can never outlive one cooldown period.
//
// When the thresholds cross AND the source can count the trigger's upstream
// matches (the approximateCounter interface; the JIRA source answers via the
// cheap /search/approximate-count endpoint from the JQL-count exporter work,
// commit 94a188743), the verdict is additionally gated on count > 0 so a
// trigger whose query currently matches nothing does not page — there is
// nothing being missed. The count is re-checked at most every
// starveRecheckTTL to stay far inside Jira's rate budget during a long
// incident.
//
// The verdict is published as gardenio_trigger_starved{source,trigger} (0/1)
// plus the raw streak gauge, served from whichever binary hosts the engine —
// the jira-bridge's internal :9093 /metrics in the deployment this was built
// for. The paging PrometheusRule lives in
// helm/gardenio/templates/jira-bridge/trigger-starved-alert.yaml.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	// defaultStarveMinCycles is the minimum consecutive cooldown-skip cycles
	// before a trigger can be considered starved (~30 cycles ≈ 15m at the
	// 30s cooldown floor).
	defaultStarveMinCycles = 30
	// defaultStarveMinDuration is the minimum wall-clock length of the skip
	// streak before a trigger can be considered starved.
	defaultStarveMinDuration = 15 * time.Minute
	// starveCooldownMult scales the duration threshold with the trigger's own
	// cooldown: a self-stamped trigger is skipped for at most ONE cooldown
	// period before the gate releases, so a streak outliving several periods
	// cannot be the engine's own doing.
	starveCooldownMult = 3
	// defaultStarveRecheckTTL bounds how often the upstream match count is
	// re-queried for a threshold-crossed trigger.
	defaultStarveRecheckTTL = 5 * time.Minute
)

var triggerCooldownSkipStreak = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_trigger_cooldown_skip_streak",
		Help: "Consecutive trigger-engine evaluation cycles that ended at the cooldown gate for this trigger; resets to 0 whenever the engine gets past the gate.",
	},
	[]string{"source", "trigger"},
)

var triggerStarvedGauge = promauto.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "gardenio_trigger_starved",
		Help: "1 while the trigger has only cooldown-skipped for longer than its own cooldown can explain (and, when the source can count, its query still matches upstream items) — the cross-env cooldown-starvation symptom of epic ka-0000005107.",
	},
	[]string{"source", "trigger"},
)

// triggerStarveState is the per-trigger detector state.
type triggerStarveState struct {
	source    string    // source label the gauges were last published under
	skips     int       // consecutive cooldown-skip cycles
	firstSkip time.Time // when the current streak began
	starved   bool      // current published verdict

	// Upstream match-count verdict cache (starveRecheckTTL).
	lastCountAt time.Time
	lastCountOK bool
}

// approximateCounter is an optional interface a TriggerSource may implement
// to report the approximate upstream match count for a trigger's query — the
// "is there actually matter to fire on?" arm of the starvation verdict. The
// JIRA source answers via /rest/api/3/search/approximate-count (the cheap
// endpoint the JQL-count exporter uses, commit 94a188743). supported=false
// means the source cannot count this particular trigger (e.g. non-JQL
// SourceData); the streak alone then decides.
type approximateCounter interface {
	ApproximateMatchCount(ctx context.Context, trig TriggerDef) (count int64, supported bool, err error)
}

// dropStaleGaugeSeries removes this state's published series when the trigger
// has moved to a different source label (multi-source fan-in triggers are
// evaluated by more than one source under the same bead ID; without this, the
// old source's series would linger at its last value and could page forever).
// Caller must hold e.mu.
func (st *triggerStarveState) dropStaleGaugeSeries(triggerID, newSource string) {
	if st.source != "" && st.source != newSource {
		triggerCooldownSkipStreak.DeleteLabelValues(st.source, triggerID)
		triggerStarvedGauge.DeleteLabelValues(st.source, triggerID)
	}
}

// noteCooldownSkip records that this evaluation cycle ended at the cooldown
// gate and re-evaluates the starvation verdict. Called from evalOne on the
// "skipped (cooldown)" path only.
func (e *TriggerEngine) noteCooldownSkip(ctx context.Context, source TriggerSource, trig TriggerDef) {
	now := e.now()

	e.mu.Lock()
	st := e.starve[trig.ID]
	if st == nil {
		st = &triggerStarveState{}
		e.starve[trig.ID] = st
	}
	st.dropStaleGaugeSeries(trig.ID, source.Name())
	st.source = source.Name()
	if st.skips == 0 {
		st.firstSkip = now
	}
	st.skips++
	skips := st.skips
	elapsed := now.Sub(st.firstSkip)
	wasStarved := st.starved
	e.mu.Unlock()

	triggerCooldownSkipStreak.WithLabelValues(source.Name(), trig.ID).Set(float64(skips))

	minElapsed := max(e.starveMinDuration, starveCooldownMult*effectiveCooldown(trig))
	if skips < e.starveMinCycles || elapsed < minElapsed {
		return
	}

	// Thresholds crossed: this trigger has not won a single fire window for
	// far longer than its own cooldown explains. Gate the verdict on the
	// upstream match count when the source can provide one.
	starved := true
	if counter, ok := source.(approximateCounter); ok {
		starved = e.confirmUpstreamMatches(ctx, counter, source, trig, now)
	}

	e.mu.Lock()
	st.starved = starved
	e.mu.Unlock()

	if starved {
		triggerStarvedGauge.WithLabelValues(source.Name(), trig.ID).Set(1)
	} else {
		triggerStarvedGauge.WithLabelValues(source.Name(), trig.ID).Set(0)
	}

	if starved && !wasStarved {
		e.logger.Warn("triggers: STARVED — trigger has only cooldown-skipped for longer than its own cooldown can explain; another writer is likely advancing last_checked (runbook: epic ka-0000005107)",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title,
			"consecutive_skips", skips, "starved_for", elapsed.String(),
			"cooldown", effectiveCooldown(trig).String())
	}
}

// noteCooldownPassed records that this evaluation cycle got PAST the cooldown
// gate — the engine had its fire window, so the trigger is not starved
// regardless of what the evaluation then produced. Resets the streak and
// clears the published verdict. Called from evalOne immediately after the
// cooldown gate (including the pending-event bypass).
func (e *TriggerEngine) noteCooldownPassed(source TriggerSource, trig TriggerDef) {
	e.mu.Lock()
	st := e.starve[trig.ID]
	if st == nil {
		st = &triggerStarveState{}
		e.starve[trig.ID] = st
	}
	st.dropStaleGaugeSeries(trig.ID, source.Name())
	wasStarved := st.starved
	*st = triggerStarveState{source: source.Name()}
	e.mu.Unlock()

	triggerCooldownSkipStreak.WithLabelValues(source.Name(), trig.ID).Set(0)
	triggerStarvedGauge.WithLabelValues(source.Name(), trig.ID).Set(0)

	if wasStarved {
		e.logger.Info("triggers: starvation cleared — trigger won a fire window",
			"source", source.Name(), "trigger", trig.ID, "title", trig.Title)
	}
}

// confirmUpstreamMatches resolves the "is there matter to fire on?" arm of
// the starvation verdict for a threshold-crossed trigger, caching the answer
// for starveRecheckTTL so a long incident costs at most one cheap count call
// per TTL. Verdict mapping:
//   - count > 0  → starved (items are sitting upstream while the trigger
//     never evaluates — the outage signature)
//   - count == 0 → not starved (nothing is being missed; keep watching)
//   - unsupported or error → starved. Fail toward paging: a streak this long
//     is already anomalous on its own, and an error here cannot be "Jira is
//     down" starving the trigger — evaluation failures happen past the
//     cooldown gate and would have reset the streak.
func (e *TriggerEngine) confirmUpstreamMatches(ctx context.Context, counter approximateCounter, source TriggerSource, trig TriggerDef, now time.Time) bool {
	e.mu.Lock()
	if st := e.starve[trig.ID]; st != nil && !st.lastCountAt.IsZero() && now.Sub(st.lastCountAt) < e.starveRecheckTTL {
		verdict := st.lastCountOK
		e.mu.Unlock()
		return verdict
	}
	e.mu.Unlock()

	count, supported, err := counter.ApproximateMatchCount(ctx, trig)
	verdict := true
	switch {
	case !supported:
		// Source cannot count this trigger's query; the streak alone decides.
	case err != nil:
		e.logger.Warn("triggers: starvation upstream-count check failed; treating as starved",
			"source", source.Name(), "trigger", trig.ID, "error", err)
	default:
		verdict = count > 0
	}

	e.mu.Lock()
	if st := e.starve[trig.ID]; st != nil {
		st.lastCountAt = now
		st.lastCountOK = verdict
	}
	e.mu.Unlock()
	return verdict
}
