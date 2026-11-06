package scheduler

import (
	"fmt"
	"sort"
	"time"
)

// Fire-reconciliation is the deterministic half of the wake-intent audit
// (Slack thread ka-0000007909). It answers ONE mechanical question with zero
// model judgment: given a trigger's cron (the MECHANISM) and its observed fire
// history, which cron-scheduled fires actually happened, which were missed, and
// which observed fires have no cron counterpart (over-firing)?
//
// WHY THIS IS SEPARATE FROM THE HAIKU RECONCILER. The gap the thread names is
// that intent and mechanism are the same object (the cron IS the intent), so
// nothing detects when the mechanism fails to achieve the intent. Splitting
// them needs two pieces: (1) a natural-language `wake_intent` field stating
// when the gardener SHOULD wake, and (2) a periodic reconciler that compares
// intent to reality. The reconciler must NOT guess timestamps — timestamp
// arithmetic is deterministic and a model doing it by hand is both wasteful and
// unreliable. So the model's job is the NL-intent -> config judgment, and THIS
// helper is the ground-truth timestamp diff it reads.
//
// SCOPE BOUNDARY — this computes the UNCONDITIONAL cron schedule. It ignores
// preflight `conditions`, `cooldown_minutes`, `blocked_by`, and `enabled`,
// because those are exactly the levers the haiku reconciler reasons about. A
// "missed" fire reported here is therefore a CANDIDATE miss, not a proven one:
// it may have been legitimately suppressed by a condition gate that was false
// at that cron tick, or by the gardener being blocked on an open decision. The
// reconciler cross-references the candidate misses against wake_intent and the
// gate config to classify them (drift / impossible / habitual / gated-ok). Do
// not "fix" this to subtract conditions — that would collapse the intent and
// mechanism back into one object, which is the bug.

// ExpectedFire is one cron-scheduled fire time inside the reconcile window,
// annotated with whether an observed fire matched it.
type ExpectedFire struct {
	// At is the cron-scheduled fire time (UTC), truncated to the minute.
	At time.Time `json:"at"`
	// Matched reports whether an observed fire landed within tolerance of At.
	Matched bool `json:"matched"`
	// Observed is the observed fire time that matched At (zero if Matched is
	// false). Carried so the caller can surface the lateness of a matched fire.
	Observed time.Time `json:"observed,omitzero"`
	// LatenessS is Observed-At in whole seconds when Matched (0 otherwise). A
	// consistently large positive lateness is itself a drift signal (the fire
	// happens, but far from when intended).
	LatenessS int `json:"lateness_s,omitempty"`
	// Pending reports that this expected fire is NOT YET OVERDUE: it is unmatched,
	// but its scheduled time sits within `tolerance` of the window end, so a
	// matching observed fire could still legitimately arrive AFTER the window end
	// (any fire up to At+tolerance is a match, and At+tolerance > end). It has no
	// matched/missed verdict yet — the grace period the tolerance grants a live
	// fire has not elapsed — so it is counted in PendingCount, never MissedCount
	// (ka-0000001791). A Pending fire is always unmatched (Matched=false).
	Pending bool `json:"pending,omitempty"`
}

// FireReconciliation is the deterministic expected-vs-observed diff for one
// trigger over a window. It is JSON-serialized straight to the haiku
// reconciler, so field names are stable and self-describing.
type FireReconciliation struct {
	Cron        string    `json:"cron"`
	Timezone    string    `json:"timezone,omitempty"`
	WindowStart time.Time `json:"window_start"`
	WindowEnd   time.Time `json:"window_end"`
	ToleranceS  int       `json:"tolerance_s"`

	// Expected is every unconditional cron fire in the window, oldest-first,
	// each flagged matched or missed.
	Expected []ExpectedFire `json:"expected"`
	// Extra is observed fires with NO expected cron counterpart within
	// tolerance — the over-firing signal (Matt's second point: "lots of
	// unneeded gardener wakeups is bad too"). Oldest-first, UTC.
	Extra []time.Time `json:"extra,omitempty"`

	ExpectedCount int `json:"expected_count"`
	MatchedCount  int `json:"matched_count"`
	MissedCount   int `json:"missed_count"`
	ExtraCount    int `json:"extra_count"`
	// PendingCount is the number of expected fires NOT YET OVERDUE at the window
	// end — unmatched slots whose scheduled time is younger than ToleranceS
	// relative to WindowEnd, so a matching observed fire could still arrive within
	// tolerance AFTER the window closes. These have no verdict yet and are
	// deliberately EXCLUDED from MissedCount, so a fresh boundary fire inside its
	// grace period never reads as a dark-lane miss (ka-0000001791). Invariant:
	// ExpectedCount == MatchedCount + MissedCount + PendingCount.
	PendingCount int `json:"pending_count,omitempty"`

	// Truncated is set when the cron enumeration hit the safety cap before
	// reaching WindowEnd (a pathological every-minute cron over a huge window).
	// The reconciler should treat a truncated result as "window too wide" and
	// narrow it rather than trusting the partial counts.
	Truncated bool `json:"truncated,omitempty"`
}

// maxExpectedFires caps cron enumeration so a degenerate every-minute cron over
// a multi-month window cannot allocate unbounded slices. 20000 covers an
// every-minute cron over ~13 days, or an hourly cron over ~2 years — far past
// any sane reconcile window.
const maxExpectedFires = 20000

// DefaultReconcileTolerance is the default slack between an expected cron fire
// and a matching observed fire. The engine evaluates on a ~30s poll and gates
// each fire behind cooldown + preflight conditions, so a fire can legitimately
// land several minutes after its cron minute. 10m absorbs that without letting
// a genuinely-different fire masquerade as a match on tight crons.
const DefaultReconcileTolerance = 10 * time.Minute

// ReconcileFires computes the deterministic expected-vs-observed fire diff for a
// trigger over [start, end]. cronExpr is the trigger's cron (mechanism); tz is
// its IANA timezone (empty = UTC); observed are the fire times reconstructed by
// the caller from last_run / run_log / children's schedule_fire_time. tolerance
// <= 0 uses DefaultReconcileTolerance.
//
// The match is greedy-nearest and one-to-one: each observed fire matches at most
// one expected fire and vice versa, so an over-firing burst shows up as Extra
// rather than silently satisfying several expected slots. A malformed cron is a
// hard error (the reconciler should classify that as an "impossible" config bug,
// not silently treat the gardener as never-scheduled).
func ReconcileFires(cronExpr, tz string, observed []time.Time, start, end time.Time, tolerance time.Duration) (*FireReconciliation, error) {
	if !end.After(start) {
		return nil, fmt.Errorf("reconcile window end (%s) must be after start (%s)", end, start)
	}
	expr, err := ParseCron(cronExpr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", cronExpr, err)
	}
	if tolerance <= 0 {
		tolerance = DefaultReconcileTolerance
	}

	loc := time.UTC
	if tz != "" {
		if parsed, lerr := time.LoadLocation(tz); lerr == nil {
			loc = parsed
		}
	}

	res := &FireReconciliation{
		Cron:        cronExpr,
		Timezone:    tz,
		WindowStart: start.UTC(),
		WindowEnd:   end.UTC(),
		ToleranceS:  int(tolerance / time.Second),
	}

	// Enumerate expected cron fires in (start, end]. Next() is evaluated in the
	// trigger's local zone (cron fields are local-time), then normalized to UTC
	// for storage and comparison. cursor walks forward from just before start.
	expected := make([]time.Time, 0, 64)
	cursor := start.In(loc).Add(-time.Minute)
	for {
		next := expr.Next(cursor)
		if next.IsZero() {
			break // no further cron match within Next()'s 366-day horizon
		}
		cursor = next
		if !next.After(end) {
			// Window is (start, end]: exclusive of a fire landing exactly at
			// start (that fire belongs to the previous window), inclusive of end.
			if next.After(start) {
				expected = append(expected, next.UTC())
			}
			if len(expected) >= maxExpectedFires {
				res.Truncated = true
				break
			}
			continue
		}
		break // walked past the window end
	}

	// Normalize + sort observed (UTC, minute truncation matches cron grain).
	obs := make([]time.Time, 0, len(observed))
	for _, o := range observed {
		if o.IsZero() {
			continue
		}
		obs = append(obs, o.UTC().Truncate(time.Minute))
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Before(obs[j]) })

	// Greedy one-to-one nearest match. For each expected fire (oldest-first),
	// take the closest still-unconsumed observed within tolerance.
	consumed := make([]bool, len(obs))
	res.Expected = make([]ExpectedFire, 0, len(expected))
	for _, exp := range expected {
		best, bestDelta := -1, time.Duration(1<<62)
		for i, o := range obs {
			if consumed[i] {
				continue
			}
			delta := o.Sub(exp)
			if delta < 0 {
				delta = -delta
			}
			if delta <= tolerance && delta < bestDelta {
				best, bestDelta = i, delta
			}
		}
		ef := ExpectedFire{At: exp}
		switch {
		case best >= 0:
			consumed[best] = true
			ef.Matched = true
			ef.Observed = obs[best]
			ef.LatenessS = int(obs[best].Sub(exp) / time.Second)
			res.MatchedCount++
		case end.Sub(exp) < tolerance:
			// Not yet overdue. This slot's full grace period runs to exp+tolerance,
			// which is past the window end, so a matching fire could still arrive
			// after `end`. No verdict yet — count it PENDING, never MISSED, so a
			// boundary fire that is merely fresh (seconds old against a 10m
			// tolerance) does not masquerade as a dark-lane miss (ka-0000001791).
			ef.Pending = true
			res.PendingCount++
		default:
			res.MissedCount++
		}
		res.Expected = append(res.Expected, ef)
	}

	// Any unconsumed observed fire has no expected cron counterpart -> extra.
	for i, o := range obs {
		if !consumed[i] {
			res.Extra = append(res.Extra, o)
		}
	}

	res.ExpectedCount = len(expected)
	res.ExtraCount = len(res.Extra)
	return res, nil
}
