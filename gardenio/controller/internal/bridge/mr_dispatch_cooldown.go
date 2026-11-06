package bridge

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mr_dispatch_cooldown.go adds a per-MR dispatch COOLDOWN to the gardener-delivery
// review-merge suppress-dispatch gate (task ka-0000001101, epic ka-0000002615).
//
// # The leak this closes
//
// R1 (bug ka-0000001050, mr_dispatch_gate.go) made the gate skip an MR that is
// parked on an open operator decision/escalation. But an MR with NO operator gate
// — a green orphan whose crew reviewed, correctly HELD (needs a human to click
// merge, or just isn't merge-worthy yet) and despawned — is re-discovered by the
// walk every 20m and gets a FRESH Opus crew each cycle, even though its diff has
// not changed a byte since the last crew. That is pure re-work: same head SHA,
// same verdict, another paid Opus dispatch. The scorecard (ka-0000000331) measured
// ~50% of all review-merge dispatches as this churn on the same handful of MRs.
//
// # The signal
//
// A dispatch is only NEW INFORMATION when the MR's HEAD moved. So we stamp, on the
// canonical type=merge-request bead, the head SHA + timestamp of the last dispatch
// (FormatCooldownStamp), and on the next cycle SUPPRESS a re-dispatch when BOTH:
//
//   - the MR's current head SHA equals the last-stamped SHA (no code changed), AND
//   - the last dispatch was within the cooldown window (~2-3 cycles / 40-60m at the
//     20m cadence — DispatchCooldownWindow).
//
// A new head SHA OR an expired window bypasses the cooldown, so a real push always
// re-crews immediately and a stale-but-still-green MR still gets a periodic (much
// rarer) re-look. The stamp is written by the gate itself on the may-dispatch path,
// so the cheap Haiku gardener needs no new logic — it already dispatches only on the
// gate's exit-0.
//
// Everything here is a PURE function (no daemon/GitLab I/O) so it is fully
// table-testable, mirroring EvaluateDispatchGate. The caller (gb mr dispatch-gate)
// fetches the current head SHA, reads the MR bead's comments for the last stamp,
// and — on dispatch — writes the fresh stamp.

// GardenerDeliveryCooldownMarker is the HTML-comment sentinel that identifies a
// cooldown stamp among a bead's comments, so it is never confused with a human or
// bridge comment. Kept stable — LatestCooldownStamp keys on it.
const GardenerDeliveryCooldownMarker = "<!-- gardener-delivery-cooldown -->"

// DispatchCooldownWindow is the default suppress window: how long after a dispatch
// a same-head-SHA MR stays off the dispatch list. 50m ≈ 2.5 cycles at the gardener's
// 20m cadence, so the two intervening cycles (+20m, +40m) are suppressed and the
// third (+60m) re-dispatches — the "2-3 cycles" the task calls for. A new head SHA
// always bypasses this regardless of elapsed time.
const DispatchCooldownWindow = 50 * time.Minute

// cooldownStampSHAPattern / cooldownStampAtPattern extract the two stamped values
// from a cooldown comment body (FormatCooldownStamp's layout). Kept as anchored
// key=value captures so extra prose in the comment can never be mis-parsed as a SHA
// or timestamp.
var (
	cooldownStampSHAPattern = regexp.MustCompile(`head_sha=([0-9A-Za-z]+)`)
	cooldownStampAtPattern  = regexp.MustCompile(`dispatched_at=(\S+)`)
)

// DispatchCooldownInput is the input to the pure cooldown decision.
type DispatchCooldownInput struct {
	// CurrentHeadSHA is the MR's current head commit SHA (GitLabMR.SHA). Empty when
	// the caller could not resolve it (no token, GitLab error) — the gate then
	// fails OPEN (dispatch), never suppressing on an unread SHA.
	CurrentHeadSHA string
	// LastHeadSHA / LastDispatchedAt are the most-recent cooldown stamp read back
	// from the MR bead (LatestCooldownStamp). LastHeadSHA is empty (and
	// LastDispatchedAt zero) when the MR has never been stamped — the first dispatch.
	LastHeadSHA      string
	LastDispatchedAt time.Time
	// Now is the evaluation time (injected for determinism in tests).
	Now time.Time
	// Window is the suppress window; a non-positive value falls back to
	// DispatchCooldownWindow.
	Window time.Duration
}

// DispatchCooldownDecision is the outcome of the cooldown gate.
type DispatchCooldownDecision struct {
	// MayDispatch is true when the cooldown does NOT suppress a fresh crew (no prior
	// stamp, the head SHA moved, or the window expired). False only when the MR's
	// head is unchanged AND the last dispatch is still inside the window.
	MayDispatch bool
	// Reason is a one-line, human-readable explanation for the gate's output/log.
	Reason string
}

// EvaluateDispatchCooldown decides whether a per-MR dispatch cooldown suppresses a
// fresh review-merge crew, given the MR's current head SHA and its last dispatch
// stamp (task ka-0000001101).
//
// SUPPRESS (MayDispatch=false) iff the current head SHA equals the last-stamped SHA
// AND 0 <= (Now - LastDispatchedAt) < Window. Otherwise DISPATCH:
//
//   - no current SHA (unresolvable)            → dispatch (fail-open)
//   - no prior stamp (first dispatch)          → dispatch
//   - head SHA changed since the last dispatch → dispatch (real new code)
//   - window expired / clock anomaly           → dispatch (periodic re-look)
//
// Fail-OPEN on every uncertainty, matching EvaluateDispatchGate: over-dispatching
// one extra crew is the tolerable prior behavior, whereas suppressing a genuinely
// new head would strand real work — strictly worse.
func EvaluateDispatchCooldown(in DispatchCooldownInput) DispatchCooldownDecision {
	window := in.Window
	if window <= 0 {
		window = DispatchCooldownWindow
	}

	if in.CurrentHeadSHA == "" {
		return DispatchCooldownDecision{
			MayDispatch: true,
			Reason:      "MR head SHA could not be resolved — cooldown skipped (fail-open); dispatch",
		}
	}
	if in.LastHeadSHA == "" || in.LastDispatchedAt.IsZero() {
		return DispatchCooldownDecision{
			MayDispatch: true,
			Reason:      "no prior dispatch stamp for this MR — first dispatch",
		}
	}
	if in.CurrentHeadSHA != in.LastHeadSHA {
		return DispatchCooldownDecision{
			MayDispatch: true,
			Reason: fmt.Sprintf(
				"MR head moved since last dispatch (%s → %s) — new code, dispatch",
				shortSHA(in.LastHeadSHA), shortSHA(in.CurrentHeadSHA)),
		}
	}

	// Same head SHA — suppress only while inside the window. A negative elapsed
	// (stamp in the future / clock skew) falls through to dispatch, never a stuck
	// suppress.
	elapsed := in.Now.Sub(in.LastDispatchedAt)
	if elapsed >= 0 && elapsed < window {
		return DispatchCooldownDecision{
			MayDispatch: false,
			Reason: fmt.Sprintf(
				"MR head unchanged (%s) and last review-merge crew dispatched %s ago (< %s cooldown) — NO new crew (nothing changed to re-review)",
				shortSHA(in.CurrentHeadSHA), roundDur(elapsed), roundDur(window)),
		}
	}
	return DispatchCooldownDecision{
		MayDispatch: true,
		Reason: fmt.Sprintf(
			"MR head unchanged (%s) but last dispatch was %s ago (>= %s cooldown) — periodic re-look, dispatch",
			shortSHA(in.CurrentHeadSHA), roundDur(elapsed), roundDur(window)),
	}
}

// FormatCooldownStamp renders the cooldown stamp comment body written on the MR
// bead when the gate dispatches. LatestCooldownStamp/ParseCooldownStamp read it
// back. The marker is on its own first line so the human-readable second line can
// evolve without breaking the parse (which keys on the head_sha=/dispatched_at=
// tokens, not the surrounding prose).
func FormatCooldownStamp(headSHA string, at time.Time) string {
	return fmt.Sprintf(
		"%s\ngardener-delivery dispatch cooldown: head_sha=%s dispatched_at=%s (skip a same-SHA re-dispatch for %s)",
		GardenerDeliveryCooldownMarker, headSHA, at.UTC().Format(time.RFC3339), roundDur(DispatchCooldownWindow))
}

// ParseCooldownStamp extracts the (headSHA, dispatchedAt) a cooldown stamp records.
// ok is false unless the text carries the marker AND both a non-empty head_sha and
// a parseable RFC3339 dispatched_at — a partial/foreign comment yields ok=false so
// it is ignored (fail-open toward dispatch).
func ParseCooldownStamp(text string) (headSHA string, at time.Time, ok bool) {
	if !containsMarker(text) {
		return "", time.Time{}, false
	}
	shaMatch := cooldownStampSHAPattern.FindStringSubmatch(text)
	atMatch := cooldownStampAtPattern.FindStringSubmatch(text)
	if shaMatch == nil || atMatch == nil {
		return "", time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, atMatch[1])
	if err != nil {
		return "", time.Time{}, false
	}
	return shaMatch[1], parsed, true
}

// LatestCooldownStamp returns the head SHA + timestamp of the MOST-RECENT cooldown
// stamp among a bead's comments (by dispatched_at, comment ID breaking ties), or
// ok=false when the bead carries none. Order-independent: it scans every comment
// rather than trusting the daemon's return order.
func LatestCooldownStamp(comments []beadsapi.Comment) (headSHA string, at time.Time, ok bool) {
	var bestID int64
	for _, c := range comments {
		sha, ts, parsed := ParseCooldownStamp(c.Text)
		if !parsed {
			continue
		}
		if !ok || ts.After(at) || (ts.Equal(at) && c.ID > bestID) {
			headSHA, at, ok, bestID = sha, ts, true, c.ID
		}
	}
	return headSHA, at, ok
}

// containsMarker reports whether the comment body carries the cooldown sentinel.
func containsMarker(text string) bool {
	return strings.Contains(text, GardenerDeliveryCooldownMarker)
}

// shortSHA (bot_unreleased.go) truncates a commit SHA to a readable prefix for the
// log/reason text; reused here so the cooldown reasons read like the rest of the
// bridge's SHA output.

// roundDur rounds a duration to the nearest minute for stable, readable reason text
// (so a 49m59s elapsed reads "50m", not "49m59.001s").
func roundDur(d time.Duration) time.Duration {
	if d < 0 {
		d = -d
	}
	return d.Round(time.Minute)
}
