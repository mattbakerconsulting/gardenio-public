// Engine-side "pouring-but-never-worked" stall sweep (ka-0000000957).
//
// This is the sibling of SweepGardenerLiveness (gardener_liveness.go) for the
// ORTHOGONAL failure mode the FLAT gardener hit. The liveness sweep catches a
// DARK gardener — one whose `last_checked` froze because the engine stopped
// evaluating it. But the FLAT gardener was the opposite: enabled=true,
// last_checked FRESH (the engine kept evaluating and pouring molecules on
// every cron window), yet it delivered nothing for days. Its apply_formula
// action poured a molecule + step beads each cycle, but the driver spawn kept
// failing (or the poured molecules were otherwise never worked), so molecules
// accumulated open with unworked step beads. To a `last_checked` watcher it
// looked perfectly alive. actionApplyFormula's own doc comment
// (triggers.go) names THIS sweep as the backstop for spawn-failed pours.
//
// The signal is therefore NOT about `last_checked` freshness (that is the
// liveness sweep's job). It is about the WORK PRODUCT: for each apply_formula
// trigger, look at the molecules it has poured and ask whether they are being
// worked. A poured molecule whose entire subtree (the molecule bead plus its
// step children) has shown no activity for longer than the trigger's cadence
// allows is INERT — nobody ever claimed or advanced it. When enough inert
// molecules accumulate (or a single one goes stale for long enough), the sweep
// alerts (notify and/or create_bead, NEVER spawn) and stamps a verdict onto the
// trigger bead so the CLI (`gb trigger show`, `gb gardener status`) and
// beads-viewer can surface the stall from a cheap field read — no read surface
// repeats the graph walk.
//
// Cost control (mirrors trigger_starve.go's recheck-TTL): the per-molecule
// graph walk is heavier than the liveness sweep's single ListBeadsFiltered, so
// each trigger's walk is throttled to at most once per pourStallRecheckTTL. In
// between, the cached verdict is re-stamped without touching the daemon.

package bridge

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Pouring-stall sweep tunables.
const (
	// pourStallInertFactor is N in "a molecule is inert when its subtree has
	// shown no activity for N× the trigger's cadence". Deliberately generous so
	// a molecule being actively worked (a driver claiming/closing steps on a
	// human cadence) is never flagged; only a molecule whose whole subtree has
	// gone quiet trips it.
	pourStallInertFactor = 3

	// pourStallInertMinAge floors the computed inert threshold so a molecule
	// poured by a very tight-cadence trigger is not flagged before a driver has
	// a realistic chance to spawn and claim its first step.
	pourStallInertMinAge = time.Hour

	// pourStallHardStaleFactor is N in "a SINGLE molecule stale for N× cadence
	// is enough to alert on its own". This collapses the one-big-molecule case
	// (a trigger that pours one large molecule per fire never accumulates
	// pourStallMinMolecules inert molecules) into the same signal as the
	// many-small-molecules case. Larger than pourStallInertFactor so a single
	// molecule must be much staler than the accumulation threshold before it
	// alerts alone.
	pourStallHardStaleFactor = 6

	// pourStallHardStaleMinAge floors the single-molecule hard-stale threshold.
	pourStallHardStaleMinAge = 6 * time.Hour

	// pourStallMinMolecules is the count of inert molecules that trips the
	// accumulation arm of the alert. The FLAT incident had 13; 3 catches a
	// developing pile-up in hours, not days, while giving a transiently slow
	// crew a couple of unworked pours of slack.
	pourStallMinMolecules = 3

	// pourStallMaxMolecules caps the number of OPEN poured molecules a single
	// sweep walks per trigger (their step subtrees are the heavier per-molecule
	// round-trips). All triggered-by candidates are hydrated cheaply first and
	// filtered to open; only the open set is capped. A trigger with more than
	// this many OPEN poured molecules is unambiguously piled up, so the cap is
	// also an alert-unconditionally floor. (Historical CLOSED molecules never
	// count — a long-lived daily trigger legitimately has hundreds.)
	pourStallMaxMolecules = 50

	// pourStallRecheckTTL bounds how often a given trigger's (heavier)
	// molecule-walk runs. Between walks the cached verdict is re-used, so the
	// steady-state cost of the sweep is a single trigger-roster ListBeadsFiltered
	// plus in-memory arithmetic — the graph walk is amortized. Mirrors
	// trigger_starve.go's starveRecheckTTL (5m) but larger because the pour walk
	// is O(molecules) round-trips, not one count call.
	pourStallRecheckTTL = 15 * time.Minute
)

// pourStallDependencyClient is the OPTIONAL daemon capability the pouring-stall
// sweep needs on top of the base TriggerBeadClient surface: the read-only
// dependency-graph traversal used to find a trigger's poured molecules and
// classify their step subtrees. The production *beadsapi.Client satisfies it;
// mocks that want to exercise the sweep implement it too. A daemon that does
// NOT implement it degrades the whole sweep to a no-op (logged once), mirroring
// the chainSpawnDaemon / GetBeadStatus optional-capability pattern in this
// package — but note the base TriggerBeadClient already lists these three
// methods, so in practice every real wiring has them.
type pourStallDependencyClient interface {
	GetReverseDependencies(ctx context.Context, beadID string) ([]beadsapi.Dependency, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
}

// Trigger-bead fields the sweep stamps so read surfaces can render the verdict
// without repeating the walk. Cleared on recovery.
const (
	pourStallField           = "pour_stall"             // "true" while stalled, "" when clear
	pourStallMoleculesField  = "pour_stall_molecules"   // count of inert molecules
	pourStallOldestAgeField  = "pour_stall_oldest_age"  // human duration of the oldest inert molecule
	pourStallDetectedAtField = "pour_stall_detected_at" // RFC3339 of first detection this episode
)

// pouringStallSweepState tracks, per trigger bead ID, the last molecule-walk
// time (for recheck-TTL throttling), the cached verdict (re-stamped between
// walks), and whether an alert has already fired for the current stall episode
// (dedup). It is the pour-stall analogue of livenessSweepState, extended with
// the walk-throttle cache the heavier graph walk needs.
type pouringStallSweepState struct {
	mu        sync.Mutex
	byTrigger map[string]*pourStallTriggerState
}

type pourStallTriggerState struct {
	lastWalkAt   time.Time       // when the molecule walk last ran (recheck-TTL)
	verdict      pourStallResult // cached verdict from the last walk
	alerted      bool            // already alerted this stall episode (dedup)
	stampedCount int             // inert count last written to the trigger bead
}

func newPouringStallSweepState() *pouringStallSweepState {
	return &pouringStallSweepState{byTrigger: make(map[string]*pourStallTriggerState)}
}

// prune drops state for triggers no longer in the roster so the map cannot grow
// unbounded as triggers are closed/deleted over time.
func (s *pouringStallSweepState) prune(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.byTrigger {
		if !live[id] {
			delete(s.byTrigger, id)
		}
	}
}

// pourStallResult is the verdict of a single trigger's molecule walk.
type pourStallResult struct {
	stalled     bool
	inertCount  int
	oldestInert time.Duration // age of the oldest inert molecule's subtree
	oldestMolID string        // ID of that molecule, for the alert body
	capped      bool          // the candidate list hit pourStallMaxMolecules
}

// SweepPouringButNeverWorked performs the pouring-but-never-worked stall pass
// (ka-0000000957). For every enabled, open apply_formula trigger that the
// engine is actively evaluating (same no-fire skip set as the liveness sweep),
// it walks the trigger's poured molecules and flags the trigger when enough of
// them are inert. A flagged trigger gets a notify and/or create_bead alert
// (never a spawn) and a verdict stamped onto its bead.
//
// No-op unless pouringStallEnabled. Safe to call every Evaluate cycle: the
// trigger-roster query is a single ListBeadsFiltered, and each trigger's
// heavier molecule walk is throttled to once per pourStallRecheckTTL. Errors
// are logged and downgraded so a daemon hiccup cannot wedge the sweep.
func (e *TriggerEngine) SweepPouringButNeverWorked(ctx context.Context) {
	if !e.pouringStallEnabled {
		return
	}

	// Attribute the notice-bead / bug-bead writes this sweep publishes to the
	// machine operator, or the daemon's fail-closed audit resolver 400s them
	// "operator could not be resolved" (ka-0000007257) and the alert never
	// reaches Slack. Mirrors closed-enabled-drain / gardener-epic-reaper. Set
	// once so both the notifier ctx and any createPourStallBead inherit it.
	ctx = beadsapi.WithSystemOperator(ctx, "pouring-stall")

	// The walk needs the dependency-graph read capability. It is part of the
	// base TriggerBeadClient surface, so this assertion succeeds for every real
	// wiring; a bare mock that omits it degrades the sweep to a no-op.
	dep, ok := e.daemon.(pourStallDependencyClient)
	if !ok {
		e.logger.Warn("pouring-stall sweep: daemon lacks dependency-graph capability, skipping")
		return
	}

	// A deliberate global pause (gardeners-pause kill-switch or triggers-pause
	// gate) means molecules legitimately sit unworked while the operator holds
	// the fleet. Skip the WHOLE sweep so a pause never manufactures a stall.
	if gardenersPauseEngaged(ctx, e.daemon, e.logger) {
		e.logger.Info("pouring-stall sweep: gardeners-pause engaged, skipping")
		return
	}
	if e.loadGateState(ctx).globallyPaused {
		e.logger.Info("pouring-stall sweep: triggers-pause engaged, skipping")
		return
	}

	res, err := e.openTriggerRoster(ctx)
	if err != nil {
		e.logger.Warn("pouring-stall sweep: failed to load trigger roster", "error", err)
		return
	}

	now := e.now()
	live := make(map[string]bool, len(res.Beads))
	swept, stalled := 0, 0

	for _, bead := range res.Beads {
		if bead == nil {
			continue
		}
		trig := ParseTriggerDef(bead, "pouring-stall")
		if !isPourStallCandidate(bead, trig) {
			continue
		}
		// Keep this candidate's dedup/recheck state alive across the prune below,
		// even when we skip walking it this cycle. Otherwise a trigger that is
		// transiently skipped (dormant / blocked / at its concurrency lease) would
		// have its state pruned and re-alert the moment it becomes walkable again.
		live[bead.ID] = true

		// Same no-fire gates the liveness sweep and evalOne honor: a trigger the
		// engine is choosing not to fire (blocked on a human, or at its live
		// concurrency lease) is expected to have unworked molecules and must not
		// be flagged.
		if bead.Fields["dormant_on_escalation"] == "true" {
			continue
		}
		if len(e.triggerBlockedBy(ctx, trig)) > 0 {
			continue
		}
		// If the engine would not fire right now because the trigger is at its
		// live concurrency lease, its molecules are waiting on a live driver by
		// design — not stalled. (Supersedes the liveness sweep's one_spawn-only
		// check with the general max_concurrent lease.)
		if trig.MaxConcurrent > 0 && e.liveSpawnCount(ctx, trig) >= trig.MaxConcurrent {
			continue
		}

		swept++

		verdict := e.pourStallVerdict(ctx, dep, bead, trig, now)
		if verdict.stalled {
			stalled++
		}
		e.applyPourStallVerdict(ctx, bead, verdict, now)
	}

	e.pouringStallState.prune(live)
	e.logger.Info("pouring-stall sweep: complete", "triggers_swept", swept, "stalled", stalled)
}

// isPourStallCandidate reports whether a trigger bead is in the target
// population: an enabled, non-fixture apply_formula trigger (plain OR gardener —
// the FLAT gardener was a plain apply_formula schedule trigger). Only
// apply_formula triggers pour molecules, so nothing else can exhibit this
// failure mode.
func isPourStallCandidate(bead *beadsapi.BeadDetail, trig TriggerDef) bool {
	if trig.Action != TriggerActionApplyFormula {
		return false
	}
	if bead.Fields["enabled"] == "false" {
		return false
	}
	if isE2EFixture(bead) {
		return false
	}
	return true
}

// pourStallVerdict returns the stall verdict for one trigger, throttling the
// heavier molecule walk to once per pourStallRecheckTTL. Between walks the
// cached verdict is returned so the caller can re-stamp it cheaply.
func (e *TriggerEngine) pourStallVerdict(ctx context.Context, dep pourStallDependencyClient, bead *beadsapi.BeadDetail, trig TriggerDef, now time.Time) pourStallResult {
	e.pouringStallState.mu.Lock()
	st := e.pouringStallState.byTrigger[bead.ID]
	if st == nil {
		st = &pourStallTriggerState{}
		e.pouringStallState.byTrigger[bead.ID] = st
	}
	fresh := !st.lastWalkAt.IsZero() && now.Sub(st.lastWalkAt) < pourStallRecheckTTL
	cached := st.verdict
	e.pouringStallState.mu.Unlock()

	if fresh {
		return cached
	}

	verdict := e.walkPourStall(ctx, dep, bead, trig, now)

	e.pouringStallState.mu.Lock()
	st.lastWalkAt = now
	st.verdict = verdict
	e.pouringStallState.mu.Unlock()
	return verdict
}

// walkPourStall performs the actual molecule graph walk for one trigger:
// discover the molecules it poured, keep the open ones, classify each open
// molecule's subtree as inert (no activity for longer than the cadence allows),
// and fold the inert set into a verdict. Read-only; all failures degrade to a
// non-stalled verdict so a daemon hiccup never manufactures an alert.
func (e *TriggerEngine) walkPourStall(ctx context.Context, dep pourStallDependencyClient, bead *beadsapi.BeadDetail, trig TriggerDef, now time.Time) pourStallResult {
	// A molecule poured by this trigger carries a `triggered-by` dep edge
	// molecule -> trigger (writeTriggerLineage), so the trigger's reverse deps
	// (its dependents) include every molecule it ever poured.
	revs, err := dep.GetReverseDependencies(ctx, bead.ID)
	if err != nil {
		e.logger.Warn("pouring-stall sweep: reverse-dep lookup failed",
			"trigger", bead.ID, "error", err)
		return pourStallResult{}
	}
	var candidateIDs []string
	for _, d := range revs {
		if d.Type == "triggered-by" && d.BeadID != "" {
			candidateIDs = append(candidateIDs, d.BeadID)
		}
	}
	if len(candidateIDs) == 0 {
		return pourStallResult{}
	}

	// Hydrate ALL candidates (one sharded BatchGetBeads, cheap) and keep only the
	// OPEN molecules — closed molecules were worked (or manually resolved) and
	// must not be walked. We hydrate before capping because GetReverseDependencies
	// returns rows in no defined order (the `deps` reverse query has no ORDER BY),
	// so slicing the raw candidate list first could drop the very open molecules
	// we care about behind a run of long-closed ones. A long-lived daily trigger
	// legitimately accumulates hundreds of CLOSED molecules; those must never be
	// mistaken for a stall.
	candidates, err := dep.BatchGetBeads(ctx, candidateIDs)
	if err != nil {
		e.logger.Warn("pouring-stall sweep: candidate hydrate failed",
			"trigger", bead.ID, "error", err)
		return pourStallResult{}
	}
	var openMolIDs []string
	openMol := make(map[string]*beadsapi.BeadDetail)
	for _, id := range candidateIDs {
		m := candidates[id]
		if m == nil || m.Type != "molecule" || m.Status != "open" {
			continue
		}
		openMolIDs = append(openMolIDs, id)
		openMol[id] = m
	}
	if len(openMolIDs) == 0 {
		return pourStallResult{}
	}
	// Cap the number of OPEN molecules whose step subtrees we walk, bounding the
	// heavier per-molecule dependency round-trips. A trigger with more than the
	// cap of OPEN poured molecules is unambiguously piled up, so the cap doubles
	// as an alert-unconditionally floor (unlike the total historical count, the
	// OPEN count is a valid stall signal).
	capped := false
	if len(openMolIDs) > pourStallMaxMolecules {
		openMolIDs = openMolIDs[:pourStallMaxMolecules]
		capped = true
	}

	// Fetch every open molecule's step children (parent-child reverse deps) in
	// one batched call, then hydrate all step beads together — the same batched
	// walk shape RollupMoleculeUsage uses.
	stepDeps, err := dep.BatchGetDependencies(ctx, openMolIDs, "reverse")
	if err != nil {
		e.logger.Warn("pouring-stall sweep: step-dep lookup failed",
			"trigger", bead.ID, "error", err)
		return pourStallResult{}
	}
	stepsByMol := make(map[string][]string, len(openMolIDs))
	var allStepIDs []string
	for _, molID := range openMolIDs {
		for _, d := range stepDeps[molID] {
			if d.Type == "parent-child" && d.BeadID != "" {
				stepsByMol[molID] = append(stepsByMol[molID], d.BeadID)
				allStepIDs = append(allStepIDs, d.BeadID)
			}
		}
	}
	stepBeads := map[string]*beadsapi.BeadDetail{}
	if len(allStepIDs) > 0 {
		stepBeads, err = dep.BatchGetBeads(ctx, allStepIDs)
		if err != nil {
			e.logger.Warn("pouring-stall sweep: step hydrate failed",
				"trigger", bead.ID, "error", err)
			return pourStallResult{}
		}
	}

	cadence := gardenerCadence(bead, now)
	inertThreshold := maxDuration(time.Duration(pourStallInertFactor)*cadence, pourStallInertMinAge)
	hardStaleThreshold := maxDuration(time.Duration(pourStallHardStaleFactor)*cadence, pourStallHardStaleMinAge)

	var result pourStallResult
	result.capped = capped
	for _, molID := range openMolIDs {
		mol := openMol[molID]
		age := moleculeInertAge(mol, stepsByMol[molID], stepBeads, now)
		if age < inertThreshold {
			continue // subtree shows recent activity — being worked
		}
		result.inertCount++
		if age > result.oldestInert {
			result.oldestInert = age
			result.oldestMolID = molID
		}
	}

	// Fire on EITHER arm: enough inert molecules accumulated, OR a single
	// molecule stale for long enough (the one-big-molecule case), OR the
	// candidate list was capped (unambiguously piled up).
	result.stalled = result.capped ||
		result.inertCount >= pourStallMinMolecules ||
		(result.inertCount >= 1 && result.oldestInert >= hardStaleThreshold)
	return result
}

// moleculeInertAge returns how long the molecule's whole subtree (the molecule
// bead plus its step children) has been quiet: now minus the FRESHEST activity
// timestamp across the subtree. "Activity" is a bead's UpdatedAt (a claim, a
// status change, a close all bump it); the molecule's own CreatedAt is the
// floor (a just-poured, never-touched molecule is exactly as old as its pour).
//
// This freshness-based inertness subsumes every stall shape robustly:
//   - never worked           -> freshest = molecule CreatedAt (old)      -> inert
//   - one step closed, driver died -> freshest = that close (now old)    -> inert
//   - a step claimed then the agent died -> freshest = that stale claim  -> inert
//   - actively being worked   -> freshest = a recent claim/close          -> NOT inert
//
// and it does not misfire on a molecule whose driver is genuinely making
// progress on a human cadence within the (generous) inert threshold.
func moleculeInertAge(mol *beadsapi.BeadDetail, stepIDs []string, stepBeads map[string]*beadsapi.BeadDetail, now time.Time) time.Duration {
	freshest := mol.CreatedAt
	if mol.UpdatedAt.After(freshest) {
		freshest = mol.UpdatedAt
	}
	for _, id := range stepIDs {
		s := stepBeads[id]
		if s == nil {
			continue
		}
		if s.UpdatedAt.After(freshest) {
			freshest = s.UpdatedAt
		}
		if s.CreatedAt.After(freshest) {
			freshest = s.CreatedAt
		}
	}
	if freshest.IsZero() {
		return 0 // no usable timestamp — treat as fresh (fail-safe, never flag)
	}
	age := now.Sub(freshest)
	if age < 0 {
		return 0
	}
	return age
}

// applyPourStallVerdict stamps the verdict onto the trigger bead and fires the
// alert on the fresh->stalled transition. Dedup: exactly one alert per stall
// episode (re-armed when the trigger recovers). The stamped fields are refreshed
// while the stall persists whenever the inert count changes — so a stall that
// grows from 3 to 13 molecules updates `gb trigger show` / beads-viewer without
// re-alerting the operator.
func (e *TriggerEngine) applyPourStallVerdict(ctx context.Context, bead *beadsapi.BeadDetail, verdict pourStallResult, now time.Time) {
	e.pouringStallState.mu.Lock()
	st := e.pouringStallState.byTrigger[bead.ID]
	if st == nil {
		st = &pourStallTriggerState{}
		e.pouringStallState.byTrigger[bead.ID] = st
	}
	wasAlerted := st.alerted
	prevStampedCount := st.stampedCount
	st.alerted = verdict.stalled
	if verdict.stalled {
		st.stampedCount = verdict.inertCount
	} else {
		st.stampedCount = 0
	}
	e.pouringStallState.mu.Unlock()

	switch {
	case verdict.stalled && !wasAlerted:
		// Newly stalled: stamp fields, log, and alert once.
		if err := e.daemon.UpdateBeadFields(ctx, bead.ID, pourStallStampFields(verdict, now)); err != nil {
			e.logger.Warn("pouring-stall sweep: failed to stamp verdict", "trigger", bead.ID, "error", err)
		}
		e.logger.Warn("pouring-stall sweep: trigger is POURING-BUT-NEVER-WORKED",
			"trigger", bead.ID, "title", bead.Title,
			"inert_molecules", verdict.inertCount,
			"oldest_inert", verdict.oldestInert.Round(time.Minute).String(),
			"capped", verdict.capped)
		e.firePourStallAlert(ctx, bead, verdict)

	case verdict.stalled && wasAlerted && verdict.inertCount != prevStampedCount:
		// Still stalled but the count changed (e.g. grew 3→13): refresh the
		// stamped fields so read surfaces show the current severity. NO re-alert
		// — the operator was already notified for this episode. Note this only
		// re-runs after the recheck-TTL produces a fresh walk (a cached verdict
		// has the same count), so it is not a per-cycle write.
		if err := e.daemon.UpdateBeadFields(ctx, bead.ID, pourStallStampFields(verdict, now)); err != nil {
			e.logger.Warn("pouring-stall sweep: failed to refresh verdict", "trigger", bead.ID, "error", err)
		}

	case !verdict.stalled && wasAlerted:
		// Recovered: clear the stamped verdict so read surfaces stop flagging it.
		if err := e.daemon.UpdateBeadFields(ctx, bead.ID, map[string]string{
			pourStallField:           "",
			pourStallMoleculesField:  "",
			pourStallOldestAgeField:  "",
			pourStallDetectedAtField: "",
		}); err != nil {
			e.logger.Warn("pouring-stall sweep: failed to clear verdict", "trigger", bead.ID, "error", err)
		}
		e.logger.Info("pouring-stall sweep: trigger recovered", "trigger", bead.ID, "title", bead.Title)
	}
}

// pourStallStampFields builds the field map the sweep writes onto a stalled
// trigger bead. pour_stall_detected_at is stamped at the current sweep time —
// on a count-refresh it advances, which is acceptable (it marks "last observed
// stalled at"); the alert dedup, not this field, guarantees one alert/episode.
func pourStallStampFields(verdict pourStallResult, now time.Time) map[string]string {
	return map[string]string{
		pourStallField:           "true",
		pourStallMoleculesField:  strconv.Itoa(verdict.inertCount),
		pourStallOldestAgeField:  verdict.oldestInert.Round(time.Minute).String(),
		pourStallDetectedAtField: now.UTC().Format(time.RFC3339),
	}
}

// firePourStallAlert emits the stall alert for a flagged trigger. It mirrors
// fireLivenessAlert's notify/create_bead fan-out but is driven off the pour
// verdict and NEVER spawns an agent.
func (e *TriggerEngine) firePourStallAlert(ctx context.Context, bead *beadsapi.BeadDetail, verdict pourStallResult) {
	countPhrase := fmt.Sprintf("%d inert poured molecule(s)", verdict.inertCount)
	if verdict.capped {
		countPhrase = fmt.Sprintf("%d+ inert poured molecule(s) (walk capped)", pourStallMaxMolecules)
	}
	msg := fmt.Sprintf(
		"🚨 pouring-stall: %s (%s) is POURING-BUT-NEVER-WORKED — %s, oldest quiet for %s. "+
			"The trigger keeps pouring molecules on cadence (it looks alive: last_checked is fresh) "+
			"but the poured molecules are not being worked — the driver spawn is likely failing or "+
			"the drivers are making no progress. Newest inert molecule: %s.",
		bead.Title, bead.ID, countPhrase, verdict.oldestInert.Round(time.Minute), defaultIfEmptyStr(verdict.oldestMolID, "—"),
	)

	if e.pouringStallNotify && e.notifier != nil {
		e.notifier(WithNoticeProject(ctx, noticeBeadProject(bead)), "pouring-stall", msg, "")
	}
	if e.pouringStallCreateBead {
		e.createPourStallBead(ctx, bead, msg)
	}
}

// createPourStallBead files a bug bead recording the stalled trigger, parented
// to the trigger so the alert is navigable from it in beads-viewer. Failure is
// logged but not fatal — the notify leg may already have alerted.
func (e *TriggerEngine) createPourStallBead(ctx context.Context, trig *beadsapi.BeadDetail, desc string) {
	labels := []string{
		"gardener-pouring-stalled",
		"source:pouring-stall-sweep",
	}
	if proj := trig.Fields["project"]; proj != "" {
		labels = append(labels, "project:"+proj)
	}
	beadID, err := e.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("Trigger pouring-but-never-worked: %s", trig.Title),
		Type:        "bug",
		Description: desc,
		Labels:      labels,
		Priority:    1,
		CreatedBy:   "pouring-stall-sweep",
	})
	if err != nil {
		e.logger.Warn("pouring-stall sweep: failed to create stall bead", "trigger", trig.ID, "error", err)
		return
	}
	if err := e.daemon.AddDependency(ctx, beadID, trig.ID, "parent-child", "pouring-stall-sweep"); err != nil {
		e.logger.Warn("pouring-stall sweep: failed to parent stall bead to trigger",
			"trigger", trig.ID, "bead", beadID, "error", err)
	}
	e.logger.Info("pouring-stall sweep: filed stall bead", "trigger", trig.ID, "bead", beadID)
}

// maxDuration returns the larger of two durations.
func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}

// defaultIfEmptyStr returns fallback when s is empty. (cmd/gb has its own
// defaultIfEmpty; the bridge package needs its own tiny helper.)
func defaultIfEmptyStr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
