// Package deathreport builds a "why did this agent die" forensic report from an
// involuntarily-dead agent's Claude Code session transcript.
//
// It is the SUMMARIZER stage of the auto-forensics pipeline (epic ka-0000005447,
// task ka-0000001381): given the last N turns of a dead session plus the dead
// agent bead's close_reason, it produces the structured content a human needs to
// understand the death —
//
//   - the last N model turns leading up to the involuntary exit,
//   - the agent's LAST WORDS (final assistant text) and the recorded close_reason,
//   - a diff of the observed Stop-hook cadence against the stop-gate cooldown
//     math, surfacing the timing anomaly that a cooldown-family death hinges on,
//   - a death-FAMILY classification (stop-gate cooldown / escape-hatch /
//     rate-limited / context-cancelled / OOM / pod-evict / reaper / idle-out /
//     timeout / …).
//
// The package is deliberately PURE: every function is a total function of its
// inputs (the parsed transcript + the close_reason string), with no I/O, no
// clock, and no daemon calls. That keeps the forensic logic unit-testable and
// lets the same code back both the `gb session death-report` CLI and any
// controller-side auto-forensics caller. Transcript loading (local file / S3)
// and the daemon bead fetch live in the CLI layer (cmd/gb), not here.
package deathreport

// StopGateDefaultCooldownSecs is the base cooldown, in seconds, between stop-gate
// text injections. It doubles with each consecutive block (exponential backoff),
// capped at StopGateMaxCooldownSecs. This mirrors the production stop-gate
// (cmd/gb/hook.go stopGateDefaultCooldownSecs) — the hook re-exports its const
// from here so the death-report cadence diff is computed against the EXACT math
// that killed the agent, never a re-derivation that could drift.
const StopGateDefaultCooldownSecs = 30

// StopGateMaxCooldownSecs caps the exponential backoff. The stop-gate never
// waits more than five minutes between injections regardless of block count.
const StopGateMaxCooldownSecs = 300

// StopGateMaxBlocks is the number of consecutive stop-gate blocks before the
// escape hatch fires (allowing the stop and closing the bead). Mirrors the
// production stop-gate (cmd/gb/hook.go stopGateMaxBlocks).
const StopGateMaxBlocks = 5

// CooldownWindowSecs returns the stop-gate cooldown window, in seconds, that
// applies when the current consecutive-block count is blockCount and the base
// cooldown is baseCooldownSecs.
//
// This is the SINGLE SOURCE OF TRUTH for the stop-gate backoff curve: the
// production hook (stopGateInCooldown, cmd/gb/hook.go) calls this, and the
// death-report cadence diff (StopHookCadenceDiff) calls this, so the window the
// report shows an operator is byte-for-byte the window that decided the death.
//
// The curve is base * 2^(blockCount-1), capped at StopGateMaxCooldownSecs:
//
//	blockCount 1 -> base            (e.g. 30s)
//	blockCount 2 -> base*2          (60s)
//	blockCount 3 -> base*4          (120s)
//	blockCount 4 -> base*8          (240s)
//	blockCount 5 -> capped at 300s
//
// A blockCount <= 1 yields the base cooldown (no doubling yet). A non-positive
// baseCooldownSecs falls back to StopGateDefaultCooldownSecs so a misconfigured
// STOP_GATE_COOLDOWN_SECS can never produce a zero or negative window.
func CooldownWindowSecs(blockCount, baseCooldownSecs int) int {
	if baseCooldownSecs <= 0 {
		baseCooldownSecs = StopGateDefaultCooldownSecs
	}
	cooldown := baseCooldownSecs
	for i := 1; i < blockCount; i++ {
		cooldown *= 2
		if cooldown > StopGateMaxCooldownSecs {
			return StopGateMaxCooldownSecs
		}
	}
	return cooldown
}
