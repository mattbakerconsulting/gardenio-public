// TEST-ONLY probe seam for the gardener terminal_goal master gate.
//
// The resume-or-escalate gate (gardenerResumeOrEscalate, bead ka-0000005353)
// runs inside evalOne's gardenerPreFireCheck, which the engine invokes for
// EVERY gardener trigger it loads on a source cycle. That makes the gate
// impossible to exercise end-to-end against a LIVE daemon without either (a)
// waiting for a real cron tick and real agent runs to accumulate, or (b)
// running EvaluateSource("schedule"), which would fire the gate against every
// real gardener in the fleet — an unacceptable blast radius for an e2e.
//
// ProbeGardenerResumeOrEscalate is the surgical alternative: it drives the gate
// against exactly ONE named gardener trigger bead and nothing else. It loads the
// bead through the SAME ParseTriggerDef the engine uses, so the GardenerSpec /
// operator / labels are parsed identically to production, then calls the real
// gardenerResumeOrEscalate with the given premature stop kind. The daemon writes
// it makes (re-arm comment + blob update, or escalate label + close), the
// gardener_resume_total counter increments, and the gardener_resumes_without_
// progress gauge set are all the production code paths — only the "which
// gardener, driven when" decision is lifted out of the fleet-wide loop.
//
// This mirrors the SeedSeen("for testing") seam already on TriggerEngine and is
// consumed only by cmd/trigger-probe (test-only, ships in no product image) and
// the gardener-resume-escalate claudeless e2e scenario. It is NOT wired into any
// engine cycle, so it cannot affect a running fleet.

package bridge

import (
	"context"
	"fmt"
	"log/slog"
)

// ProbeGardenerResumeOrEscalate loads the gardener trigger bead identified by
// gardenerID and runs the terminal_goal master gate's resume-or-escalate
// decision against it exactly once, as if a premature loop_end stop of the
// given kind had just fired. It is a TEST-ONLY seam (see file header): the real
// engine reaches gardenerResumeOrEscalate through gardenerPreFireCheck.
//
// kind is a scheduler.StopKind* string (e.g. "max_runs", "no_progress_for",
// "wall_clock", "consecutive_failures") — the premature stop that triggered the
// decision. It flows straight into the reason label of gardener_resume_total.
//
// Returns an error only when the bead cannot be loaded or is not a gardener
// (nil Gardener block); the gate's own outcome (re-arm vs escalate vs dormant)
// is observed via the bead's post-state and the /metrics scrape, exactly as an
// operator would see it.
func (e *TriggerEngine) ProbeGardenerResumeOrEscalate(ctx context.Context, gardenerID, kind string) error {
	logger := e.logger
	if logger == nil {
		logger = slog.Default()
	}
	bead, err := e.daemon.GetBead(ctx, gardenerID)
	if err != nil {
		return fmt.Errorf("probe gardener: GetBead(%s): %w", gardenerID, err)
	}
	if bead == nil {
		return fmt.Errorf("probe gardener: bead %s not found", gardenerID)
	}
	trig := ParseTriggerDef(bead, "schedule")
	if trig.Gardener == nil {
		return fmt.Errorf("probe gardener: bead %s has no gardener block (not a gardener trigger)", gardenerID)
	}
	// e.daemon (TriggerBeadClient) is a superset of gardenerDaemon — the engine
	// passes the very same value into gardenerPreFireCheck in evalOne, so this
	// call also serves as the compile-time proof that the two interfaces still
	// line up (if they drift, this file stops compiling).
	gardenerResumeOrEscalate(ctx, e.daemon, logger, trig, kind)
	return nil
}
