package main

// Gardener overdue / silent-gardener detection (bug ka-0000006095 / A1).
//
// The gardener page computes a LIVE next-fire from the cron and renders it
// regardless of whether the gardener is actually firing. So an enabled gardener
// whose cron has silently stopped firing — a stuck cooldown, a
// one_spawn_at_a_time deadlock, an engine bug — still shows a healthy-looking
// "fires in 12m", with no signal that it "should have fired N ago and didn't".
// This is the accuracy analogue of the decision-gate gap: the page must be able
// to tell a firing gardener from a silently-dead one.
//
// Two distinct failure shapes are surfaced (see gardenerLiveness):
//
//   - OVERDUE — an enabled gardener that has effectively STOPPED making
//     headway. Three independent signals, any of which flags it:
//       * budget exhausted: schedule_runs_spawned >= schedule_max_runs (with a
//         real, positive max_runs) — the gardener has spent its run budget and is
//         not resuming;
//       * cron lateness: a parseable cron whose last fire is older than the
//         expected cadence + cooldown + tolerance — it should have fired again
//         and did not;
//       * progress frozen while NOT spawning: last_progress_at is stale beyond
//         the threshold and no fresh run is starting — the loop has quietly
//         gone dark.
//
//   - SILENT — a gardener that IS still spawning (a recent last_run) but whose
//     *progress* has gone stale: last_progress_at is old even though runs keep
//     starting. The loop is alive but making no headway — the runs spin and
//     quietly stop. This is distinct from "stalled" (the trajectory verdict,
//     which reads scored outcomes): silence is a wall-clock signal on the
//     progress heartbeat, catching a gardener whose runs each look individually
//     fine but collectively advance nothing.
//
// Both are folded into needs-attention so an on-call operator sees a wedged
// loop at a glance instead of a reassuring next-fire.

import (
	"strconv"
	"strings"
	"time"
)

// gardenerOverdueTolerance is the slack added on top of (cadence + cooldown)
// before an enabled cron gardener is called overdue. A gardener fires on cron
// windows, and the engine's evaluation floor + cooldown mean the true next fire
// can legitimately land a little past the naive cron instant; this tolerance
// keeps a gardener that is merely between windows from flashing overdue. It also
// covers clock skew and a single missed evaluation tick. Set generously — the
// signal is for a gardener that has gone DARK, not one that is a minute late.
const gardenerOverdueTolerance = 30 * time.Minute

// gardenerSilentProgressThreshold is how stale last_progress_at may be before we
// treat the gardener's progress as frozen — silent when it is still spawning,
// overdue when it is not. Matches the ground-truth signal in the bug
// (last_progress_at / last_outcome_at > this).
const gardenerSilentProgressThreshold = 8 * time.Hour

// gardenerLiveness is the derived firing-health verdict for an enabled gardener,
// computed live from the trigger bead's fire/progress signals against its
// cadence + budget. Zero value (neither flag set) means "no liveness concern":
// the gardener is firing on schedule, or we lack the signals to judge (disabled,
// closed, no cron/budget, no recorded fire yet), in which case we stay silent
// rather than guess.
type gardenerLiveness struct {
	// Overdue is true when the gardener has effectively stopped making headway:
	// budget exhausted, cron lateness, or a frozen progress heartbeat while not
	// spawning. OverdueDetail is the human evidence.
	Overdue       bool
	OverdueDetail string

	// Silent is true when the gardener is still spawning runs but its
	// last_progress_at heartbeat is stale beyond gardenerSilentProgressThreshold
	// — the loop is alive but making no progress. SilentDetail is the evidence.
	Silent       bool
	SilentDetail string
}

// NeedsAttention reports whether either liveness concern warrants an operator's
// eye. It lets callers fold the liveness signal into the existing
// needs-attention machinery with one predicate.
func (l gardenerLiveness) NeedsAttention() bool { return l.Overdue || l.Silent }

// gardenerLivenessInput is the minimal, fetch-free surface deriveGardenerLiveness
// needs. It is derived entirely from fields already on the trigger bead (plus a
// caller-supplied last_progress_at, which lives on the iteration beads the
// caller has already fetched), so computing liveness adds no daemon round-trips.
type gardenerLivenessInput struct {
	Enabled  bool
	IsClosed bool

	// Cron / cadence, already resolved by the caller (the legacy cron field or
	// the triggers[] fallback). Empty for a bead-trigger gardener with no clock
	// cadence — cron-lateness detection is skipped (no expected tempo to be late
	// against), though budget/progress signals still apply.
	Cron     string
	Timezone string

	// CooldownMinutes is the gardener's cooldown_minutes (0 when unset). Added to
	// the overdue budget so a long-cooldown gardener is not flagged early.
	CooldownMinutes int

	// RunsSpawned / MaxRuns are the engine's run-budget counters
	// (schedule_runs_spawned / schedule_max_runs). Budget exhaustion
	// (RunsSpawned >= MaxRuns) is asserted ONLY when MaxRuns > 0 — an unbounded
	// / standing gardener has MaxRuns 0 and can never be "over budget".
	RunsSpawned int
	MaxRuns     int

	// LastFire is the most recent time the gardener actually fired, taken as the
	// freshest of last_outcome_at / last_run. Zero when the gardener has no
	// recorded fire yet (brand-new — never flag cron-late).
	LastFire time.Time

	// LastProgress is the freshest genuine-progress heartbeat (last_progress_at)
	// across the gardener's runs, supplied by the caller from the iteration beads
	// it already holds. Zero when absent — a progress signal is only asserted
	// from a real, stale heartbeat, never manufactured from a missing field.
	LastProgress time.Time
}

// stillSpawning reports whether the gardener is still starting runs — i.e. its
// last fire is recent (within the silent-progress window). It splits a frozen
// progress heartbeat into SILENT (still spawning) vs OVERDUE (gone dark), and is
// derived from LastFire so it needs no extra input.
func (in gardenerLivenessInput) stillSpawning(now time.Time) bool {
	return !in.LastFire.IsZero() && now.Sub(in.LastFire) <= gardenerSilentProgressThreshold
}

// deriveGardenerLiveness computes the overdue / silent verdict from the gardener's
// bead-surface signals and the current time. It is pure (no fetch, no clock of
// its own) so it is fully unit-testable and the caller controls `now`.
//
// Guard rails:
//   - A disabled or closed gardener will not fire; there is nothing to be late
//     against, so never flag it.
//   - Budget exhaustion requires a real positive MaxRuns (standing gardeners have
//     MaxRuns 0 and are never over budget).
//   - Cron lateness requires a parseable cron AND a recorded last fire.
//   - A progress signal requires a real, stale last_progress_at; it is split
//     into silent (still spawning) vs overdue (not spawning). Absent heartbeat
//     yields no progress signal (we do not invent one from absence).
func deriveGardenerLiveness(in gardenerLivenessInput, now time.Time) gardenerLiveness {
	var l gardenerLiveness
	if !in.Enabled || in.IsClosed {
		return l
	}

	// --- Budget exhausted: spent its run budget and not resuming. ---
	if in.MaxRuns > 0 && in.RunsSpawned >= in.MaxRuns {
		l.Overdue = true
		l.OverdueDetail = "run budget exhausted (" + strconv.Itoa(in.RunsSpawned) +
			"/" + strconv.Itoa(in.MaxRuns) + " runs spawned)"
	}

	// --- Cron lateness: should have fired again and did not. ---
	if !l.Overdue {
		if cron := strings.TrimSpace(in.Cron); cron != "" && !in.LastFire.IsZero() {
			if cadence, ok := cronInterval(cron, in.Timezone, now); ok {
				budget := cadence + time.Duration(in.CooldownMinutes)*time.Minute + gardenerOverdueTolerance
				if since := now.Sub(in.LastFire); since > budget {
					l.Overdue = true
					l.OverdueDetail = "last fired " + shortDuration(since.Round(time.Minute)) +
						" ago, expected every " + shortDuration(cadence.Round(time.Minute))
				}
			}
		}
	}

	// --- Progress heartbeat frozen: silent (spawning) vs overdue (dark). ---
	if !in.LastProgress.IsZero() {
		if since := now.Sub(in.LastProgress); since > gardenerSilentProgressThreshold {
			stale := "no progress in " + shortDuration(since.Round(time.Minute))
			if in.stillSpawning(now) {
				l.Silent = true
				l.SilentDetail = "spawning, but " + stale
			} else if !l.Overdue {
				l.Overdue = true
				l.OverdueDetail = stale + " and not spawning"
			}
		}
	}

	return l
}

// cronInterval estimates the gardener's firing CADENCE — the gap between two
// consecutive fires — by asking the same cron parser the next-fire projection
// uses for the next two fire instants after `now` and taking their difference.
// This handles the common uniform-cadence schedules (`*/30 * * * *`,
// `27,57 * * * *`, `@hourly`) exactly, which is what the overdue budget needs.
//
// Returns ok=false when the cron does not parse or the two fires collapse to a
// non-positive gap — the caller then skips cron-lateness detection rather than
// computing an expectation from a cadence it could not read (a wrong-values-key
// silent no-op is worse than no signal). Note: for a highly irregular schedule
// the single-interval estimate can be off, but it is only ever ADDED to the
// generous tolerance, so the failure mode is "flag a little late", never a
// false overdue.
func cronInterval(cron, tz string, now time.Time) (time.Duration, bool) {
	next, after, err := computeNextFire2(cron, tz, now)
	if err != nil || next.IsZero() || after.IsZero() {
		return 0, false
	}
	d := after.Sub(next)
	if d <= 0 {
		return 0, false
	}
	return d, true
}

// gardenerCooldownMinutes parses the cooldown_minutes field off a gardener trigger
// bead's fields, returning 0 for an absent, unparseable, or negative value.
func gardenerCooldownMinutes(fields map[string]string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(fields["cooldown_minutes"]))
	if n < 0 {
		return 0
	}
	return n
}

// gardenerLastFire returns the gardener's most recent actual fire time, taken as
// the freshest of last_outcome_at and last_run (both RFC3339). Zero time when
// neither is present/parseable. last_outcome_at is stamped every evaluation
// (including declines), so it is the freshest liveness signal; last_run is the
// fire-only mirror. We take the max so a gardener that recently fired is never
// mis-read as overdue because one of the two fields lagged.
func gardenerLastFire(fields map[string]string) time.Time {
	return maxTime(
		parseRFC3339OrZero(strings.TrimSpace(fields["last_outcome_at"])),
		parseRFC3339OrZero(strings.TrimSpace(fields["last_run"])),
	)
}

// maxTime returns the later of two times (either may be zero).
func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// freshestProgress returns the freshest last_progress_at across a gardener's
// life-thread — its most recent genuine-progress heartbeat. last_progress_at is
// stamped on an agent bead only when a real, usage-bearing turn lands (it
// freezes the instant work stops), so the newest value across all the gardener's
// runs is the truest "when did this gardener last make headway" signal. Zero
// time when no iteration carries a parseable heartbeat.
func freshestProgress(iters []gardenerIterationView) time.Time {
	var newest time.Time
	for _, it := range iters {
		if it.LastProgressAt.After(newest) {
			newest = it.LastProgressAt
		}
	}
	return newest
}
