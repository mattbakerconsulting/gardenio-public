// Package bridge — dark MR-generation rate-limit / concurrency cap (R6-FU6c,
// ka-0000002822).
//
// The global bounded-stop guard (gardener_bounds_guard.go, ka-0000006744) bounds
// how many TIMES a gardener can spawn (run_budget + hard wall-clock). But each
// spawn can in turn open many MRs against monorepo-dark / gitlab-dark — so a
// gardener that is "bounded" by spawn count can still flood review and burn CI
// minutes if every spawn opens a fistful of MRs, or if many bounded gardeners
// pile on at once. This file bounds the MR BLAST RADIUS, the dimension the
// run_budget never touches:
//
//   - FLEET concurrency cap (darkMaxConcurrentOpenDarkMRs) — a ceiling on the
//     total number of OPEN dark MR beads across the whole fleet. When the open
//     dark-MR backlog is already at the cap, NO dark gardener fires until the
//     backlog drains (review/merge/close brings it back under). This is the
//     analogue of the kill-switch, but automatic and self-releasing.
//
//   - PER-GARDENER rate window (darkMaxMRsPerGardenerWindow over
//     darkMRPerGardenerWindow) — a ceiling on how many dark MRs a SINGLE gardener
//     may have produced inside a rolling time window. A runaway gardener that
//     opens MR after MR trips its own window and is paused (without disabling —
//     it resumes once older MRs age out of the window), while well-behaved
//     gardeners keep firing.
//
// Both caps are evaluated in gardenerPreFireCheck for IsDarkGardener triggers
// only — non-dark gardeners are completely untouched (they never reach this
// code). The caps fire FAIL-CLOSED on a confirmed over-limit count, and
// FAIL-OPEN on a daemon error (a flaky read must not freeze the dark universe;
// the global bounded-stop guard + the global gardeners-pause kill-switch are
// independent backstops).
//
// "Dark MR" identification is by the MR bead's mr_url host (the gitlab-dark
// host substring) OR a dark label, with upstream-mirror beads
// (external-mirror:true) explicitly excluded — those are read-only projections
// of human-owned upstream MRs (E-PROV FW-5) and are never produced by a dark
// gardener. This keeps the cap from counting non-dark or upstream MRs, so a
// non-dark MR is provably unaffected.
package bridge

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// darkMaxConcurrentOpenDarkMRs is the fleet-wide ceiling on simultaneously-open
// dark MR beads. While at or above this many open dark MRs exist, every dark
// gardener's fire is suppressed until the backlog drains. Sized generously above
// the steady-state open-MR count of a healthy dark program while still being a
// real wall against a flood (a runaway gardener can open dozens of MRs in
// minutes). Tunable here; not operator-configurable per-gardener by design — it
// is a fleet safety rail, not a per-gardener knob.
const darkMaxConcurrentOpenDarkMRs = 40

// darkMRPerGardenerWindow is the rolling window over which a single gardener's
// produced dark MRs are counted for the per-gardener rate cap.
const darkMRPerGardenerWindow = 1 * time.Hour

// darkMaxMRsPerGardenerWindow is the maximum number of dark MRs a single gardener
// may have produced within darkMRPerGardenerWindow before its fire is paused.
// Counted against OPEN dark MR beads created in the window and attributed to
// this gardener (via the producing task bead's trigger_id/schedule_id or the
// gardener's AssignBead). A gardener at the cap resumes automatically once its
// older MRs age out of the window or are merged/closed — no disable, unlike the
// unbounded-guard.
const darkMaxMRsPerGardenerWindow = 8

// darkMRListLimit bounds the open-MR bead scan. The open dark-MR set is small
// in steady state; this is a backstop against an unbounded fetch if the backlog
// somehow explodes. If the real count exceeds this, the fleet cap has already
// tripped many times over, so a truncated scan is still correctly over-limit.
const darkMRListLimit = 500

// producingTaskLister is the optional capability the per-gardener MR-rate cap
// needs: walk an MR bead back to the task beads that produced it (the reverse
// `produces` edge). The production *beadsapi.Client implements it
// (ProducingTaskBeadsForMR); the gardener daemon interface deliberately does NOT
// declare it, so this is discovered by type-assertion at call time. A daemon
// that lacks it (most test mocks) simply skips the per-gardener cap — the fleet
// cap still applies, and the bounded-stop guard + kill-switch remain in force.
type producingTaskLister interface {
	ProducingTaskBeadsForMR(ctx context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error)
}

// darkMRCapDecision is the outcome of the dark MR caps. Skip=true means the
// gardener fire must be suppressed this cycle; Reason carries the structured
// cause for logs/tests. Skip=false means neither cap tripped (or the check
// fail-opened on a daemon error).
type darkMRCapDecision struct {
	Skip   bool
	Reason gardenerSkipReason
}

// isDarkMRBead reports whether an MR bead belongs to the monorepo-dark universe
// and therefore counts toward the dark MR caps. An upstream-mirror bead
// (external_mirror / external-mirror:true) is a read-only projection of a
// human-owned upstream MR and never a dark-gardener product, so it is excluded
// up front. Otherwise a bead is dark when its mr_url host contains the
// gitlab-dark marker OR it carries a dark label. This is the load-bearing
// discriminator that keeps NON-dark MRs (and upstream mirrors) from being
// counted, so they are provably unaffected by the cap.
func isDarkMRBead(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	// Exclude read-only upstream mirrors (E-PROV FW-5, ka-0000003146).
	if b.Fields["external_mirror"] == "true" || b.Fields["upstream"] == "true" {
		return false
	}
	for _, l := range b.Labels {
		if l == "external-mirror:true" || l == "upstream:true" {
			return false
		}
	}
	// Dark by mr_url host: the gitlab-dark deployment host carries the
	// "gitlab-dark" marker (e.g. gitlab-dark.gardenio-e2e.e2e.example.com).
	if ref := ParseMRURL(b.Fields["mr_url"]); ref != nil {
		if strings.Contains(ref.Host, "gitlab-dark") {
			return true
		}
	}
	// Dark by label — same dark markers the gardener guard keys on.
	for _, l := range b.Labels {
		if slices.Contains(darkGardenerLabels, l) {
			return true
		}
	}
	return false
}

// listOpenDarkMRBeads returns the open dark MR beads, filtered through
// isDarkMRBead. It queries open type=merge-request beads once and filters
// client-side — there is no server-side host filter, and the open MR set is
// small. Returns (beads, true) on success; (nil, false) when the daemon read
// failed so the caller can fail-open.
func listOpenDarkMRBeads(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger) ([]*beadsapi.BeadDetail, bool) {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"merge-request"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    darkMRListLimit,
	})
	if err != nil {
		logger.Warn("dark MR cap: ListBeadsFiltered failed, treating as under-limit (fail-open)",
			"error", err)
		return nil, false
	}
	if res == nil {
		return nil, true
	}
	out := make([]*beadsapi.BeadDetail, 0, len(res.Beads))
	for _, b := range res.Beads {
		if isDarkMRBead(b) {
			out = append(out, b)
		}
	}
	return out, true
}

// gardenerOwnsMR reports whether the given dark MR bead is attributable to the
// gardener identified by gardenerID / assignBead. Attribution walks the MR's
// producing task beads (reverse `produces` edge): a task whose trigger_id or
// schedule_id equals the gardener ID, or whose ID is the gardener's AssignBead
// (the long-lived home task an AssignBead-pinned gardener reuses across runs),
// means this gardener produced the MR. Fail-quiet: a per-MR lookup error counts
// as "not owned" so one bad edge cannot over-count a gardener into a false trip
// — the conservative direction for a fail-closed cap is to under-count, never
// to invent ownership.
func gardenerOwnsMR(ctx context.Context, lister producingTaskLister, logger *slog.Logger, mrBeadID, gardenerID, assignBead string, createdInWindow bool) bool {
	if !createdInWindow {
		return false
	}
	tasks, err := lister.ProducingTaskBeadsForMR(ctx, mrBeadID)
	if err != nil {
		logger.Warn("dark MR cap: ProducingTaskBeadsForMR failed, treating MR as not-owned",
			"mr_bead", mrBeadID, "gardener", gardenerID, "error", err)
		return false
	}
	for _, t := range tasks {
		if t == nil {
			continue
		}
		if assignBead != "" && t.ID == assignBead {
			return true
		}
		if t.Fields["trigger_id"] == gardenerID || t.Fields["schedule_id"] == gardenerID {
			return true
		}
	}
	return false
}

// evaluateDarkMRCaps enforces the fleet concurrency cap and the per-gardener MR
// rate window for a dark gardener. It is called from gardenerPreFireCheck after
// the kill-switch and bounded-stop guards, and only for IsDarkGardener triggers.
//
// Order: the fleet cap is checked first (a cheap single list + count) — when the
// whole dark universe is at its open-MR ceiling, every gardener is paused
// regardless of its own recent rate, so there is no point computing per-gardener
// attribution. The per-gardener window is checked second, reusing the same fetched
// bead set.
//
// FAIL-CLOSED on a confirmed over-limit count (Skip=true). FAIL-OPEN on a daemon
// read error (Skip=false) — a flaky read must never freeze the dark universe;
// the bounded-stop guard and kill-switch are independent backstops. The
// per-gardener cap additionally degrades to a no-op when the daemon does not
// implement producingTaskLister (the fleet cap still applies).
func evaluateDarkMRCaps(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger, trig TriggerDef) darkMRCapDecision {
	if logger == nil {
		logger = slog.Default()
	}

	openDarkMRs, ok := listOpenDarkMRBeads(ctx, daemon, logger)
	if !ok {
		// Fail-open: a daemon read error must not wedge the dark universe.
		return darkMRCapDecision{Skip: false, Reason: gardenerSkipNone}
	}

	// (1) FLEET concurrency cap — total open dark MRs across the fleet.
	if len(openDarkMRs) >= darkMaxConcurrentOpenDarkMRs {
		logger.Warn("dark MR cap: fleet concurrent-open-dark-MR ceiling reached, suppressing fire",
			"trigger", trig.ID, "open_dark_mrs", len(openDarkMRs), "cap", darkMaxConcurrentOpenDarkMRs)
		return darkMRCapDecision{Skip: true, Reason: gardenerSkipDarkMRFleetCap}
	}

	// (2) PER-GARDENER rate window. Needs the reverse-`produces` walk; skip the
	// cap entirely if the daemon doesn't expose it (the fleet cap still held
	// above). The window cutoff is computed from now; an MR bead created at or
	// after the cutoff and still open counts toward this gardener's window.
	lister, hasLister := daemon.(producingTaskLister)
	if !hasLister {
		return darkMRCapDecision{Skip: false, Reason: gardenerSkipNone}
	}

	assignBead := ""
	if trig.Gardener != nil {
		assignBead = strings.TrimSpace(trig.Gardener.AssignBead)
	}
	cutoff := time.Now().Add(-darkMRPerGardenerWindow)

	owned := 0
	for _, mr := range openDarkMRs {
		createdInWindow := !mr.CreatedAt.IsZero() && !mr.CreatedAt.Before(cutoff)
		if gardenerOwnsMR(ctx, lister, logger, mr.ID, trig.ID, assignBead, createdInWindow) {
			owned++
			if owned >= darkMaxMRsPerGardenerWindow {
				logger.Warn("dark MR cap: per-gardener MR-rate window reached, pausing this gardener",
					"trigger", trig.ID, "owned_open_mrs_in_window", owned,
					"cap", darkMaxMRsPerGardenerWindow, "window", darkMRPerGardenerWindow)
				return darkMRCapDecision{Skip: true, Reason: gardenerSkipDarkMRPerGardenerCap}
			}
		}
	}

	return darkMRCapDecision{Skip: false, Reason: gardenerSkipNone}
}
