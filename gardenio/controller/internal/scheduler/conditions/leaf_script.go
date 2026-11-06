package conditions

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// scriptProbeClient is the optional capability the "script" daemon leaf needs:
// fetch a single bead by id so the leaf can read the verdict a probe pod
// stamped on the type=script bead. *beadsapi.Client satisfies it; a stub that
// does not is treated as "cannot read the probe" and FAILS OPEN (see below).
//
// This is deliberately a narrow, single-method capability — the script leaf
// reads ONE bead and does nothing else, so it must not require the full
// BeadClient surface. It follows the same optional-capability pattern as
// beadStatusClient / AcceptanceMetricsClient.
type scriptProbeClient interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// Compile-time proof that the production daemon client can back the script
// leaf. If a refactor drops GetBead, evalScript would silently fail open for
// EVERY script leaf in production (never suppress, always spawn) — this
// assertion turns that regression into a build failure instead.
var _ scriptProbeClient = (*beadsapi.Client)(nil)

// scriptDefaultMaxAge bounds how old a completed probe verdict may be before
// the leaf stops trusting it and fails open. The canonical caller (the
// release-owner cron, 13,43 — every 30m) re-runs its probe each cadence, so a
// fresh verdict is at most one cadence old. One hour tolerates a single missed
// refresh, then fails open: a probe pod that silently stops refreshing must NOT
// leave a frozen "exit 0 / nothing to do" verdict suppressing the gardener
// forever — that is exactly the untraceable-release-outage failure mode this
// increment exists to avoid. Overridable per-leaf via Leaf.Value (a Go
// duration); Value<=0 disables the staleness gate.
const scriptDefaultMaxAge = time.Hour

// evalScript evaluates the "script" daemon leaf (probe-pods plan
// ka-0000000544, increment 4 / ka-0000006663).
//
// SEMANTICS. The leaf does NOT execute anything — it reads the verdict a probe
// pod (increment 1, `gb probe run`) stamped on the type=script bead named by
// Leaf.ScriptBead. `gb probe run` runs the script out-of-band in a cheap 50m /
// 128Mi pod and, on completion, CLOSES the script bead stamping exit_code and
// agent_state (done for exit 0, failed for non-zero). This leaf reads that
// completed verdict. Keeping execution out-of-band is what lets the conditions
// package stay pure (no os/exec, no net/http) and non-blocking — the two hard
// constraints on this task.
//
// The only supported op is "nonzero" (empty defaults to it): the condition is
// MET (⇒ the caller fires ⇒ the gardener spawns) when the probe exited NON-ZERO
// ("something changed, wake the gardener"), and UNMET (⇒ the fire is suppressed
// ⇒ no gardener) when the probe exited ZERO ("nothing to do").
//
// FAIL-OPEN. This inverts the fail-closed convention of the surrounding gate
// (trigger_source_schedule.go: an evaluation error skips the fire). Here, any
// uncertainty about the verdict — the bead is missing/deleted, not yet closed
// (no completed verdict), carries no parseable exit code, is stale, or the
// client cannot read it — returns (true, nil): the gardener SPAWNS. A degraded
// probe costs one no-op run (the status quo); it must NEVER silently stop
// releases. The ONLY path that returns (false, nil) — i.e. suppresses — is a
// fresh, closed probe with a parseable exit code of exactly 0.
func evalScript(ctx context.Context, client BeadClient, l Leaf) (bool, error) {
	// Only "nonzero" (or empty) is supported. An unknown op is a
	// misconfiguration; fail open rather than silently invert the gate.
	if op := strings.TrimSpace(l.Op); op != "" && op != "nonzero" {
		slog.Warn("script leaf: unsupported op, failing open (spawning)",
			"script_bead", l.ScriptBead, "op", l.Op)
		return true, nil
	}

	if strings.TrimSpace(l.ScriptBead) == "" {
		slog.Warn("script leaf: no script_bead configured, failing open (spawning)")
		return true, nil
	}

	probeClient, ok := client.(scriptProbeClient)
	if !ok {
		slog.Warn("script leaf: client cannot fetch beads, failing open (spawning)",
			"script_bead", l.ScriptBead)
		return true, nil
	}

	bead, err := probeClient.GetBead(ctx, l.ScriptBead)
	if err != nil {
		// Missing/deleted probe bead or a transport error: cannot read the
		// verdict → fail open. (A deleted probe MUST wake the gardener.)
		slog.Warn("script leaf: could not read probe bead, failing open (spawning)",
			"script_bead", l.ScriptBead, "error", err)
		return true, nil
	}
	if bead == nil {
		slog.Warn("script leaf: probe bead not found, failing open (spawning)",
			"script_bead", l.ScriptBead)
		return true, nil
	}

	// A completed probe is a CLOSED bead. An open bead means the probe has not
	// run (or is still running) — there is no completed verdict to trust.
	if bead.Status != "closed" {
		slog.Warn("script leaf: probe has no completed verdict (bead not closed), failing open (spawning)",
			"script_bead", l.ScriptBead, "status", bead.Status)
		return true, nil
	}

	// Staleness: a frozen verdict from a probe that stopped refreshing must not
	// suppress forever. If the verdict is older than the max age, fail open.
	if maxAge := scriptMaxAge(l.Value); maxAge > 0 {
		verdictAt, ok := scriptVerdictTime(bead)
		if !ok {
			slog.Warn("script leaf: probe verdict has no usable timestamp, failing open (spawning)",
				"script_bead", l.ScriptBead)
			return true, nil
		}
		if age := scriptNow().Sub(verdictAt); age > maxAge {
			slog.Warn("script leaf: probe verdict is stale, failing open (spawning)",
				"script_bead", l.ScriptBead, "age", age.String(), "max_age", maxAge.String())
			return true, nil
		}
	}

	// The authoritative verdict is the stamped exit code (present for both the
	// done and failed cases). Unparseable ⇒ cannot interpret ⇒ fail open.
	raw, present := bead.Fields["exit_code"]
	if !present || strings.TrimSpace(raw) == "" {
		slog.Warn("script leaf: probe has no exit_code, failing open (spawning)",
			"script_bead", l.ScriptBead)
		return true, nil
	}
	exit, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		slog.Warn("script leaf: probe exit_code unparseable, failing open (spawning)",
			"script_bead", l.ScriptBead, "exit_code", raw, "error", err)
		return true, nil
	}

	// The one suppression path: a fresh, closed probe that exited exactly 0.
	if exit == 0 {
		return false, nil
	}
	// Non-zero ⇒ wake the gardener.
	return true, nil
}

// scriptNow is the clock the staleness check uses. Overridable in tests via
// scriptNowFunc; production uses time.Now.
func scriptNow() time.Time {
	if scriptNowFunc != nil {
		return scriptNowFunc()
	}
	return time.Now()
}

// scriptNowFunc, when non-nil, overrides scriptNow (tests only). It is a
// package-level hook rather than an Env field so the existing Env construction
// sites (EvaluateCondition, the gate) need no change — production always uses
// the real clock.
var scriptNowFunc func() time.Time

// scriptMaxAge parses the per-leaf staleness window from Leaf.Value. Empty ⇒
// the default; a parseable Go duration ⇒ that; <=0 ⇒ staleness gate disabled;
// unparseable ⇒ the default (a bad value must not accidentally disable the
// gate).
func scriptMaxAge(value string) time.Duration {
	v := strings.TrimSpace(value)
	if v == "" {
		return scriptDefaultMaxAge
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		slog.Warn("script leaf: unparseable max-age value, using default",
			"value", value, "default", scriptDefaultMaxAge.String())
		return scriptDefaultMaxAge
	}
	return d
}

// scriptVerdictTime returns the time the probe verdict was produced — the
// bead's close time, falling back to its last update — as the anchor for the
// staleness check. Returns ok=false when neither is usable.
func scriptVerdictTime(bead *beadsapi.BeadDetail) (time.Time, bool) {
	if s := strings.TrimSpace(bead.ClosedAt); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, true
		}
	}
	if !bead.UpdatedAt.IsZero() {
		return bead.UpdatedAt, true
	}
	return time.Time{}, false
}
