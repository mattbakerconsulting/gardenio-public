package progress

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// RenderPrimeBlock builds the "## Loop Progress" markdown injected by
// `gb prime` when the schedule's progress.surface.in_prime is true.
// Layout follows plan-of-record §3.3:
//
//	## Loop Progress
//
//	Window `this_run` (since 2026-04-28 13:42):
//	  • crew_dispatched   ×1   (ka-aaa @ 13:43)
//	  • child_closed      ×0
//	  ...
//	  Net: spawned 1 crew. No closes/merges yet.
//
// The block is omitted entirely when the spec has no windows OR no
// events — empty progress instrumentation has nothing to render.
// When an event has zero matches in every window the line is still
// emitted (×0) so the gardener sees what is being measured.
func RenderPrimeBlock(spec Spec, state State, now time.Time) string {
	if len(spec.Windows) == 0 || len(spec.Events) == 0 {
		return ""
	}
	counts := CountByWindow(spec, state, now)
	kinds := EventKinds(spec)

	var b strings.Builder
	b.WriteString("## Loop Progress\n\n")
	for _, w := range spec.Windows {
		writeWindowHeader(&b, w, state, now)
		row := counts[w.Name]
		writeKindLines(&b, kinds, row, state, w)
		writeNetSummary(&b, kinds, row)
		b.WriteString("\n")
	}
	writeProgressSelfReportContract(&b, spec)
	return b.String()
}

// writeProgressSelfReportContract appends the one-line self-report contract that
// tells the gardener how to set `progress_score` in its wrap-up. Rendered only
// when the schedule surfaces progress in the wrap-up (spec.Surface.InWrapup) —
// i.e. the gardener actually owes a progress self-assessment at despawn.
//
// Why this lives in the shared prime block (tune ka-0000007473; sibling
// ka-0000001254 / ka-0000004879): progress_score is PURELY self-reported and is
// the primary D1 (progress-yield) signal; FinalizeRun's event-aware rescue and
// ReconcileAsyncProgress are only a backstop that lifts a mis-reported score-0 on
// a LATER reconcile. A delegating gardener that closes children / merges MRs but
// leaves progress_score at 0 records a false score-0 run that mis-scores D1 and
// masks its real effectiveness (the exact First Mate / Jira-write-control
// symptom: D2 throughput high, D1 yield collapsed). Every progress-instrumented
// gardener reads this block each run, so stating the invariant here fixes the
// self-report at the source for all of them — rather than hand-editing one
// gardener's data-plane prompt, which spec-less gardeners don't have.
//
// Two contract shapes, keyed off whether the spec references any DELEGATION
// progress kind (child_closed / mr_merged / decision_resolved — the
// CreditedProgressKinds an anti-gaming guard would itself credit):
//
//   - Delegator (spec references ≥1 credited kind): score >= 1 when a child
//     closed, an MR merged, or a decision resolved — the anti-gaming-aligned
//     wording.
//   - Report-only sentinel (spec's ONLY progress event is a `wrapup_field`
//     self-report — no credited kind): a gardener that never closes a child,
//     merges an MR, or resolves a decision. The delegator wording would tell it
//     to score 0 EVERY cycle, and three zero-scored runs wedge its otherwise
//     healthy loop in the `no_progress_for` pre-fire deadlock (ka-0000003265;
//     the exact recurring wedge of the weekly gardener-audit roll-up
//     ka-0000005781, root-caused in ka-0000007551). For these, completing the
//     run's own deliverable IS progress — say so, so the score never floors.
func writeProgressSelfReportContract(b *strings.Builder, spec Spec) {
	if !spec.Surface.InWrapup {
		return
	}
	if specHasCreditedProgressKind(spec) {
		b.WriteString("Self-report contract: at `gb stop`, set `progress_score` from THIS run's " +
			"outcomes — >= 1 if a child closed, an MR merged, or a decision resolved " +
			"(including work by crew you dispatched); 0 only when nothing moved forward. " +
			"`progress_score` is the primary progress signal — do not leave it at 0 on a run that delivered.\n")
		return
	}
	b.WriteString("Self-report contract: at `gb stop`, set `progress_score` from THIS run's " +
		"outcomes — this is a report-only loop, so completing the run's deliverable " +
		"(landing the report/roll-up, or a clean \"nothing in window\" result) is itself " +
		">= 1; score 0 ONLY when the run could not complete its work at all. " +
		"`progress_score` is the primary progress signal — do not leave it at 0 on a run that delivered.\n")
}

// specHasCreditedProgressKind reports whether the spec references any
// delegation progress kind (child_closed / mr_merged / decision_resolved). A
// spec whose only progress event is a self-reported `wrapup_field` (a
// report-only sentinel gardener) returns false, selecting the report-only
// contract wording above.
func specHasCreditedProgressKind(spec Spec) bool {
	for _, m := range spec.Events {
		for _, credited := range CreditedProgressKinds {
			if m.Kind == credited {
				return true
			}
		}
	}
	return false
}

func writeWindowHeader(b *strings.Builder, w Window, state State, _ time.Time) {
	switch w.Scope {
	case WindowScopeSinceRunStart:
		if !state.CurrentRunStart.IsZero() {
			fmt.Fprintf(b, "Window `%s` (since %s):\n",
				w.Name, state.CurrentRunStart.Format("2006-01-02 15:04"))
		} else {
			fmt.Fprintf(b, "Window `%s` (since run start):\n", w.Name)
		}
	case WindowScopeLastNRuns:
		fmt.Fprintf(b, "Window `%s` (last %d runs):\n", w.Name, w.N)
	case WindowScopeSince:
		d, _ := w.ParseDuration()
		if d > 0 {
			fmt.Fprintf(b, "Window `%s` (last %s):\n", w.Name, d)
		} else {
			fmt.Fprintf(b, "Window `%s`:\n", w.Name)
		}
	default:
		fmt.Fprintf(b, "Window `%s`:\n", w.Name)
	}
}

func writeKindLines(b *strings.Builder, kinds []string, row map[string]int, state State, w Window) {
	maxLen := 0
	for _, k := range kinds {
		if len(k) > maxLen {
			maxLen = len(k)
		}
	}
	since := windowSince(w, state, time.Now())
	for _, k := range kinds {
		count := row[k]
		fmt.Fprintf(b, "  • %-*s ×%d", maxLen, k, count)
		if count > 0 {
			if rec := mostRecentEvent(state, k, since); rec != nil {
				fmt.Fprintf(b, "   (%s @ %s)", rec.TargetID, rec.OccurredAt.Format("15:04"))
			}
		}
		b.WriteString("\n")
	}
}

// mostRecentEvent returns the most-recent EventRecord with matching
// kind whose OccurredAt is >= since. Nil when none match.
func mostRecentEvent(state State, kind string, since time.Time) *EventRecord {
	var pick *EventRecord
	for i := range state.Events {
		e := &state.Events[i]
		if e.Kind != kind {
			continue
		}
		if !since.IsZero() && e.OccurredAt.Before(since) {
			continue
		}
		if pick == nil || e.OccurredAt.After(pick.OccurredAt) {
			pick = e
		}
	}
	return pick
}

// writeNetSummary writes the "Net:"/"Trend:" line below a window's
// per-kind counts. Plan §3.3 shows several variants; we render a
// terse generic summary (sum of all counts, plus "no progress" when
// every count is zero).
func writeNetSummary(b *strings.Builder, kinds []string, row map[string]int) {
	total := 0
	parts := []string{}
	for _, k := range kinds {
		c := row[k]
		total += c
		if c > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c, k))
		}
	}
	sort.Strings(parts)
	if total == 0 {
		b.WriteString("  Net: no progress events yet.\n")
		return
	}
	fmt.Fprintf(b, "  Net: %s.\n", strings.Join(parts, ", "))
}
