package deathreport

import (
	"fmt"
	"time"
)

// StopEvent is one reconstructed stop-gate block: the Nth consecutive block,
// the time it fired, the gap since the previous block, and the cooldown window
// that was in force at that moment. The DEATH-relevant field is InCooldown —
// whether this block landed inside the still-active cooldown window of the
// previous block. A block that lands in-cooldown is the one the stop-gate
// force-exits (closeAgentBeadOnExit → *_involuntary).
type StopEvent struct {
	// BlockNumber is the 1-based consecutive block index (resets are not modeled
	// here — a death-report transcript is a single un-reset run).
	BlockNumber int `json:"block_number"`
	// At is the wall-clock time the block fired (zero if the transcript carried
	// no timestamp on it).
	At time.Time `json:"at,omitzero"`
	// GapSecs is the seconds elapsed since the previous block (0 for the first).
	GapSecs int64 `json:"gap_secs"`
	// CooldownWindowSecs is the cooldown window that the PREVIOUS block armed —
	// i.e. the window this block is tested against (base * 2^(prevCount-1),
	// capped). Computed by the shared CooldownWindowSecs so it is the exact
	// window the live gate used.
	CooldownWindowSecs int `json:"cooldown_window_secs"`
	// InCooldown is true when GapSecs < CooldownWindowSecs — this block landed
	// inside the previous block's still-active cooldown, so the live stop-gate
	// would have force-exited here. This is the timing anomaly the report
	// surfaces.
	InCooldown bool `json:"in_cooldown"`
}

// CadenceDiff is the Stop-hook-cadence-vs-cooldown-math diff: the reconstructed
// per-block timeline plus the derived verdict about WHICH block (if any) the
// cooldown math would have killed the agent on.
type CadenceDiff struct {
	// BaseCooldownSecs is the base cooldown the diff was computed with (default
	// or STOP_GATE_COOLDOWN_SECS override).
	BaseCooldownSecs int `json:"base_cooldown_secs"`
	// Events is the per-block timeline, oldest first.
	Events []StopEvent `json:"events"`
	// KillBlock is the 1-based BlockNumber of the FIRST block that landed inside
	// cooldown — the block the cooldown math would force-exit on. 0 when no
	// block landed in cooldown (no cooldown-timing kill is implicated).
	KillBlock int `json:"kill_block"`
	// EscapeHatchBlock is the 1-based block at which the escape hatch would fire
	// (block count reaching StopGateMaxBlocks), or 0 if the run never reached it.
	// When a kill would happen via cooldown BEFORE the escape hatch, KillBlock <
	// EscapeHatchBlock — the report can then say "died on cooldown timing, not
	// the escape hatch", the exact distinction the ka-0000002026 RCA drew.
	EscapeHatchBlock int `json:"escape_hatch_block"`
	// Anomaly is a one-line human summary of the timing finding.
	Anomaly string `json:"anomaly"`
}

// StopHookCadenceDiff reconstructs the Stop-hook cadence from the observed
// block timestamps and diffs it against the stop-gate cooldown math, using the
// SHARED CooldownWindowSecs so the window it reports is byte-for-byte the window
// the live gate applied.
//
// The model mirrors the production gate (cmd/gb/hook.go): each stop-gate block
// increments a consecutive-block counter and re-arms an exponential-backoff
// cooldown window (base * 2^(count-1), capped). The NEXT block is force-exited
// as involuntary if it fires while the previous block's window is still active
// (gap < window). We reconstruct that arithmetic from the transcript-observed
// block times so the report can pinpoint the anomaly: three identical quiet
// turn-ends survive because their gaps exceed the (then-small) window, and the
// fourth kills purely because its gap fell inside the (now-larger) window.
//
// baseCooldownSecs <= 0 falls back to StopGateDefaultCooldownSecs.
func StopHookCadenceDiff(blockTimes []time.Time, baseCooldownSecs int) CadenceDiff {
	if baseCooldownSecs <= 0 {
		baseCooldownSecs = StopGateDefaultCooldownSecs
	}
	d := CadenceDiff{BaseCooldownSecs: baseCooldownSecs}

	var prev time.Time
	for i, at := range blockTimes {
		blockNum := i + 1
		ev := StopEvent{BlockNumber: blockNum, At: at}

		// The window this block is tested against is the one the PREVIOUS block
		// armed: the previous block took the consecutive count to (blockNum-1),
		// so its window is CooldownWindowSecs(blockNum-1, base).
		if blockNum > 1 {
			ev.CooldownWindowSecs = CooldownWindowSecs(blockNum-1, baseCooldownSecs)
			if !prev.IsZero() && !at.IsZero() {
				gap := int64(at.Sub(prev).Seconds())
				if gap < 0 {
					gap = 0
				}
				ev.GapSecs = gap
				ev.InCooldown = gap < int64(ev.CooldownWindowSecs)
			}
		}

		if ev.InCooldown && d.KillBlock == 0 {
			d.KillBlock = blockNum
		}
		d.Events = append(d.Events, ev)
		prev = at
	}

	// The escape hatch fires once the consecutive-block count reaches
	// StopGateMaxBlocks. If the run produced at least that many blocks, that is
	// the block the escape hatch would have allowed the stop on.
	if len(blockTimes) >= StopGateMaxBlocks {
		d.EscapeHatchBlock = StopGateMaxBlocks
	}

	d.Anomaly = describeAnomaly(d)
	return d
}

// describeAnomaly renders the one-line human finding for the cadence diff.
func describeAnomaly(d CadenceDiff) string {
	switch {
	case len(d.Events) == 0:
		return "no stop-gate blocks observed in the transcript — this death is not a stop-gate cooldown/escape-hatch timing kill"
	case d.KillBlock > 0 && d.EscapeHatchBlock > 0 && d.KillBlock < d.EscapeHatchBlock:
		return fmt.Sprintf("cooldown-timing kill: block #%d landed inside the %ds cooldown window (gap %ds) and would force-exit the agent — BEFORE the escape hatch at block #%d; the earlier blocks survived only because their gaps exceeded the then-smaller window",
			d.KillBlock, windowAt(d, d.KillBlock), gapAt(d, d.KillBlock), d.EscapeHatchBlock)
	case d.KillBlock > 0:
		return fmt.Sprintf("cooldown-timing kill: block #%d landed inside the %ds cooldown window (gap %ds) and would force-exit the agent; earlier blocks survived because their gaps exceeded the then-smaller window",
			d.KillBlock, windowAt(d, d.KillBlock), gapAt(d, d.KillBlock))
	case d.EscapeHatchBlock > 0:
		return fmt.Sprintf("escape-hatch kill: %d consecutive blocks reached the escape-hatch ceiling (max %d) without any single block landing in-cooldown; the gate allowed the stop and closed the bead",
			len(d.Events), StopGateMaxBlocks)
	default:
		return fmt.Sprintf("%d stop-gate block(s) observed, none landing inside its cooldown window and none reaching the escape-hatch ceiling — no cooldown/escape-hatch timing kill is implicated",
			len(d.Events))
	}
}

func windowAt(d CadenceDiff, blockNum int) int {
	if blockNum >= 1 && blockNum <= len(d.Events) {
		return d.Events[blockNum-1].CooldownWindowSecs
	}
	return 0
}

func gapAt(d CadenceDiff, blockNum int) int64 {
	if blockNum >= 1 && blockNum <= len(d.Events) {
		return d.Events[blockNum-1].GapSecs
	}
	return 0
}
