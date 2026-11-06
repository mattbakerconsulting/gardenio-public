package main

// wrapup_progress.go — enforcement of `progress_score` and
// `progress_summary` wrap-up fields when the agent belongs to a schedule
// whose progress spec opts in via `progress.surface.in_wrapup: true`.
//
// Per plan §T5: a gardener whose schedule declares progress instrumentation
// owes the loop a one-line self-assessment at every despawn so subsequent
// runs see whether the prior run made material progress. Without this,
// `no_progress_for` stop kinds would be deaf to the gardener's intent.

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/scheduler/progress"
)

// progressWrapupRequired reports whether the agent's schedule bead opts
// into wrap-up enforcement of progress_score/progress_summary. Returns
// false on any lookup or parse failure (fail-open: a busted schedule must
// not wedge gb stop).
func progressWrapupRequired(ctx context.Context, agentID string) bool {
	if daemon == nil || agentID == "" {
		return false
	}
	agentBead, err := daemon.GetBead(ctx, agentID)
	if err != nil || agentBead == nil {
		return false
	}
	scheduleID := agentBead.Fields["schedule_id"]
	if scheduleID == "" {
		return false
	}
	sched, err := daemon.GetBead(ctx, scheduleID)
	if err != nil || sched == nil {
		return false
	}
	spec, err := progress.ParseSpec(sched.Fields["progress"])
	if err != nil {
		return false
	}
	return spec.Surface.InWrapup
}

// validateProgressWrapup returns the list of issues with the wrap-up's
// progress fields when enforcement is active. Empty slice means valid.
func validateProgressWrapup(w *WrapUp) []string {
	var issues []string
	if w == nil {
		return []string{"wrap-up missing — schedule requires progress_score and progress_summary"}
	}
	if w.ProgressScore == nil {
		issues = append(issues, "required field \"progress_score\" is missing (int: 0=nothing material, 1=incremental, ≥2=substantive)")
	}
	if w.ProgressSummary == "" {
		issues = append(issues, "required field \"progress_summary\" is empty (one-line justification of progress_score)")
	}
	return issues
}

// formatProgressIssues renders enforcement issues into a multi-line error
// suitable for printing to stderr before returning a stop-blocking error.
func formatProgressIssues(issues []string) string {
	var b strings.Builder
	b.WriteString("Progress wrap-up validation failed (schedule has progress.surface.in_wrapup):\n")
	for _, i := range issues {
		b.WriteString("  - ")
		b.WriteString(i)
		b.WriteString("\n")
	}
	b.WriteString("\nExample:\n  gb stop --wrapup '{\"tldr\":\"<one-line outcome>\",\"accomplishments\":\"...\",\"progress_score\":1,\"progress_summary\":\"closed ka-X, dispatched crew\"}'\n")
	return b.String()
}

// enforceProgressWrapup runs progress-spec enforcement on the wrap-up.
// Returns a non-nil error suitable for returning from runStop when the
// schedule requires progress fields and they are missing. Soft mode
// (currently always hard when enabled) is reserved for future use.
func enforceProgressWrapup(ctx context.Context, agentID string, w *WrapUp, force bool) error {
	if !progressWrapupRequired(ctx, agentID) {
		return nil
	}
	issues := validateProgressWrapup(w)
	if len(issues) == 0 {
		return nil
	}
	if force {
		return nil
	}
	return fmt.Errorf("%s", formatProgressIssues(issues))
}
