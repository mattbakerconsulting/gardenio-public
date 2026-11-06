// Package bridge — gardener bounded-stop guard (ka-0000006744).
//
// The load-bearing safety invariant for EVERY gardener (advice ka-0000000974) is
// that it "MUST declare a self-block: run_budget + a hard wall-clock stop." An
// unbounded gardener is the single biggest cost/RSI footgun in the fleet: it can
// spin forever, spawn crew forever, and burn tokens + CI minutes + namespaces
// with no ceiling. That invariant is GLOBAL — it applies to all gardeners, not a
// label-scoped subset. Historically it was honored only by DISCIPLINE; the
// pre-existing gardener-gate guard (gardener_gate_guard.go) only WARNS.
//
// This file is the FAIL-CLOSED enforcement, applied to ALL gardeners. A gardener
// MUST carry BOTH:
//
//   - run_budget > 0 (a positive run cap — out of runs ⇒ stop), AND
//   - a hard_stop wall_clock entry that parses to a bounded, positive duration
//     no larger than GardenerMaxWallClock (the absolute wall-clock ceiling).
//
// Validation is SEMANTIC, not field-presence: run_budget must be a positive
// int and the wall_clock duration must actually parse (time.ParseDuration) to a
// value in (0, GardenerMaxWallClock]. A wall_clock of "0s", "-1h", "forever", or
// "100000h" is rejected exactly like a missing one.
//
// Rollout is asymmetric by design (supersedes the dark-only MR !4702; operator
// declined that special-casing in decision ka-0000003439):
//
//   - AUTHORING time — `gb gardener start` and `gb trigger create` exit non-zero
//     and write nothing when a gardener blob is unbounded. New gardeners are
//     fail-closed globally. (The default `gb gardener start` blob is always
//     bounded — run_budget=max_runs, hard_stop wall_clock=loop-wall-clock — so
//     this only bites a hand-overridden unbounded value.)
//   - FIRING time — the engine WARNS (warn-with-grace, once per trigger) when a
//     live gardener bead is unbounded but still lets it fire, so the existing
//     live gardeners that predate this guard are NOT disabled mid-flight. The
//     only firing-time hard stop is the explicit, operator-engaged
//     `gardeners-pause` kill-switch gate below — never a bounds auto-disable.
//
// There is intentionally NO label/project discriminator: the rule is universal,
// so there is nothing to mis-scope and no fleet-wide auto-disable keyed on a
// label match.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/pausegate"
	"gardenio/controller/internal/scheduler"
)

// GardenerMaxWallClock is the absolute upper bound a gardener's hard_stop
// wall_clock may declare. A "bounded" wall clock that says 100000h is not a
// safety guarantee — it is an unbounded loop with extra steps. 720h (30 days)
// is generously above the longest legitimate gardener program while still being
// a real ceiling. The default `gb gardener start` loop wall-clock is 168h (1
// week), well under this. Exported so call sites that must name the ceiling in
// operator-facing text (e.g. `gb gardener status`'s expiry hint, ka-0000007024)
// share this ONE constant rather than hardcoding "720h" and drifting from it.
const GardenerMaxWallClock = 720 * time.Hour

// GardenerBoundsViolation describes why a gardener failed the bounded-stop guard.
// A nil *GardenerBoundsViolation means the gardener is properly bounded.
type GardenerBoundsViolation struct {
	// MissingRunBudget is true when run_budget is absent or <= 0.
	MissingRunBudget bool
	// RunBudget is the offending run_budget value (for the message).
	RunBudget int
	// MissingWallClock is true when no hard_stop wall_clock entry parses to a
	// bounded, positive duration <= GardenerMaxWallClock.
	MissingWallClock bool
	// WallClockReason explains the wall_clock failure (absent / unparseable /
	// non-positive / exceeds-ceiling). Empty when the wall_clock is fine.
	WallClockReason string
}

// Error renders an actionable, operator-facing rejection message naming every
// missing constraint and how to fix it. Implements error so callers can return
// it directly.
func (v *GardenerBoundsViolation) Error() string {
	if v == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("gardener is UNBOUNDED — refusing (advice ka-0000000974).\n")
	b.WriteString("  A gardener MUST declare BOTH a positive run_budget AND a hard_stop wall_clock.\n")
	if v.MissingRunBudget {
		fmt.Fprintf(&b, "    • run_budget is %d — must be a positive integer (e.g. run_budget=20).\n", v.RunBudget)
	}
	if v.MissingWallClock {
		reason := v.WallClockReason
		if reason == "" {
			reason = "absent"
		}
		fmt.Fprintf(&b, "    • hard_stop wall_clock is %s — must parse to a bounded duration in (0, %s].\n", reason, GardenerMaxWallClock)
		b.WriteString("      Add a hard_stop wall_clock, e.g. {\"hard_stop\":{\"any\":[{\"kind\":\"wall_clock\",\"duration\":\"168h\"}]}}.\n")
		// The 168h example is safe for a FRESH gardener (baseline = created_at =
		// now) but a footgun when REVIVING an old one: a gardener older than the
		// wall_clock grandfather epoch measures its window from that epoch, not
		// its creation time (scheduler.gardenerWallClockBaseline), so a short
		// duration can already be expired and re-close it on the next reconcile —
		// a "bounded" verdict that is an instant self-close (ka-0000007024). Point
		// the operator at `gb gardener status`, which prints the real baseline +
		// expiry from the bead's created_at, rather than hardcoding a date here.
		b.WriteString("      NOTE: a gardener older than the wall_clock grandfather epoch measures this window from that\n")
		b.WriteString("      epoch, not its creation time, so a short duration may already be EXPIRED (an instant self-close on\n")
		b.WriteString("      the next reconcile). Confirm the real baseline + expiry with `gb gardener status <bead>` and size the\n")
		b.WriteString("      duration so the expiry lands in the future.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// hasBoundedHardStopWallClock reports whether the parsed stop spec carries at
// least one hard_stop wall_clock entry whose duration parses to a positive
// value no larger than GardenerMaxWallClock. It returns a human reason
// describing the failure when no such entry exists.
//
// Only hard_stop counts: a wall_clock in run_end is a per-run timeout, and a
// wall_clock in loop_end can be raced/converted by the resume-or-escalate path
// (premature kinds re-arm). The hard_stop slot is the emergency brake that the
// scheduler/reaper enforces unconditionally — it is the only slot that is a
// true ceiling, so it is the only one that satisfies the guard.
func hasBoundedHardStopWallClock(spec scheduler.StopSpec) (ok bool, reason string) {
	sawWallClock := false
	for _, e := range spec.HardStop.Any {
		if e.Kind != scheduler.StopKindWallClock {
			continue
		}
		sawWallClock = true
		d, err := time.ParseDuration(strings.TrimSpace(e.Duration))
		if err != nil {
			reason = fmt.Sprintf("unparseable (%q)", e.Duration)
			continue
		}
		if d <= 0 {
			reason = fmt.Sprintf("non-positive (%q)", e.Duration)
			continue
		}
		if d > GardenerMaxWallClock {
			reason = fmt.Sprintf("exceeds the %s ceiling (%q)", GardenerMaxWallClock, e.Duration)
			continue
		}
		return true, ""
	}
	if !sawWallClock {
		return false, "absent"
	}
	return false, reason
}

// ValidateGardenerBounds is the fail-closed check. Given a gardener's run_budget
// and its raw `stop` JSON blob, it returns a non-nil *GardenerBoundsViolation
// unless BOTH a positive run_budget and a bounded hard_stop wall_clock are
// present. A malformed stop blob is treated as a missing wall_clock
// (fail-closed — an unparseable spec is NOT a ceiling).
//
// This is the single source of truth shared by the authoring-time CLI guards
// and the firing-time engine warning, so the two can never disagree on what
// "bounded" means.
func ValidateGardenerBounds(runBudget int, stopRaw string) *GardenerBoundsViolation {
	v := &GardenerBoundsViolation{RunBudget: runBudget}
	if runBudget <= 0 {
		v.MissingRunBudget = true
	}

	spec, err := scheduler.ParseStopSpec(strings.TrimSpace(stopRaw))
	if err != nil {
		v.MissingWallClock = true
		v.WallClockReason = fmt.Sprintf("in an unparseable stop blob (%v)", err)
	} else if ok, reason := hasBoundedHardStopWallClock(spec); !ok {
		v.MissingWallClock = true
		v.WallClockReason = reason
	}

	if v.MissingRunBudget || v.MissingWallClock {
		return v
	}
	return nil
}

// ValidateGardenerSpec applies ValidateGardenerBounds to a parsed GardenerSpec,
// reading run_budget and the stop blob off the spec. It is the firing-time
// entry point (the engine has already decoded the gardener blob into a
// GardenerSpec via ParseTriggerDef). Returns nil when the spec is bounded or
// when spec is nil (a nil gardener is not a gardener — nothing to bound).
func ValidateGardenerSpec(spec *GardenerSpec) *GardenerBoundsViolation {
	if spec == nil {
		return nil
	}
	return ValidateGardenerBounds(spec.RunBudget, string(spec.Stop))
}

// ValidateGardenerBlob applies the bounded-stop guard to a raw `gardener` JSON
// blob (the artifact `gb gardener start` / `gb trigger create` persist). It
// decodes the blob into a GardenerSpec and validates run_budget + the hard_stop
// wall_clock end-to-end, so the authoring-time CLI guard validates the EXACT
// bytes that will be written — not a separate model that could drift from them.
//
// An empty blob means "no gardener spec" — callers gate on the blob being
// non-empty before calling this (a source-only trigger with no gardener block is
// not a gardener). When called with an empty blob it is treated as unbounded and
// rejected with both constraints flagged. A non-empty but unparseable blob is
// likewise fail-closed (an unparseable spec is not a ceiling).
func ValidateGardenerBlob(raw string) *GardenerBoundsViolation {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return &GardenerBoundsViolation{
			MissingRunBudget: true,
			MissingWallClock: true,
			WallClockReason:  "absent (no gardener loop spec)",
		}
	}
	var spec GardenerSpec
	if err := json.Unmarshal([]byte(raw), &spec); err != nil {
		return &GardenerBoundsViolation{
			MissingRunBudget: true,
			MissingWallClock: true,
			WallClockReason:  fmt.Sprintf("in an unparseable gardener blob (%v)", err),
		}
	}
	return ValidateGardenerSpec(&spec)
}

// decodeGardenerSpec decodes a raw `gardener` JSON blob into a *GardenerSpec. It
// returns ok=false for an empty blob (not a gardener — nothing to decode) or an
// unparseable one, so a caller can cleanly treat "no spec to inspect" as a
// no-op. It is the shared decode used by the standing-marker guard so that
// guard inspects the EXACT bytes that will be persisted, decoded identically to
// the bounded-stop guard above.
func decodeGardenerSpec(raw string) (spec *GardenerSpec, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, false
	}
	var s GardenerSpec
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, false
	}
	return &s, true
}

// gardenersPauseTitle is the well-known title of a gate bead that, when open,
// pauses ALL gardener firing fleet-wide — the kill-switch. It is the gardener
// analogue of triggersPauseTitle: an operator can halt every gardener in every
// project without freezing every non-gardener trigger. Open a gate bead with
// this exact title (`kd create "<title>" --type gate`) to engage; close it to
// release. This is the ONLY firing-time hard stop the bounded guard adds — it
// is operator-engaged, never an automatic label-keyed disable.
//
// Aliased to pausegate.GardenersPauseTitle so the /schedule command, the /plans
// master switch, and this fire-time read all key on ONE string constant and can
// never drift (ka-0000004048). The value is unchanged.
const gardenersPauseTitle = pausegate.GardenersPauseTitle

// gardenersPauseEngaged reports whether an open `gardeners-pause` gate bead
// exists. When true, every gardener's fire is suppressed fleet-wide (the
// kill-switch). Mirrors loadGateState's pause-gate lookup: errors are
// downgraded to "not engaged" so a daemon hiccup does not silently freeze every
// gardener (fail-OPEN on the kill-switch lookup is the safe default — the
// bounded-stop guard still independently warns about every unbounded gardener).
func gardenersPauseEngaged(ctx context.Context, daemon gardenerDaemon, logger *slog.Logger) bool {
	if logger == nil {
		logger = slog.Default()
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"gate"},
		Statuses: []string{"open", "in_progress"},
		Search:   gardenersPauseTitle,
		Limit:    5,
	})
	if err != nil {
		logger.Warn("gardeners-pause: ListBeadsFiltered failed, treating as disengaged",
			"error", err)
		return false
	}
	if res == nil {
		return false
	}
	for _, b := range res.Beads {
		if b != nil && b.Title == gardenersPauseTitle {
			return true
		}
	}
	return false
}
