package deathreport

import (
	"testing"
	"time"
)

func TestCooldownWindowSecs_Curve(t *testing.T) {
	// base * 2^(blockCount-1), capped at 300.
	cases := []struct {
		blockCount int
		base       int
		want       int
	}{
		{0, 30, 30},  // non-positive count → base
		{1, 30, 30},  // first block → base, no doubling
		{2, 30, 60},  // base*2
		{3, 30, 120}, // base*4
		{4, 30, 240}, // base*8
		{5, 30, 300}, // base*16=480 → capped at 300
		{6, 30, 300}, // stays capped
		{3, 0, 120},  // non-positive base → default 30 → *4
	}
	for _, c := range cases {
		if got := CooldownWindowSecs(c.blockCount, c.base); got != c.want {
			t.Errorf("CooldownWindowSecs(%d, %d) = %d, want %d", c.blockCount, c.base, got, c.want)
		}
	}
}

// TestStopHookCadenceDiff_RCADeathArithmetic replays the exact cadence from the
// ka-0000002026 hand RCA (thread-1784851806-182809): four consecutive stop-gate
// blocks with inter-block deltas 0 / +40s / +177s / +67s. Stops #1–#3 survive
// because each gap exceeds the (then-smaller) cooldown window; only Stop#4 lands
// inside the count=3 (120s) window (gap 67 < 120) and is force-exited. The diff
// must pinpoint block #4 as the kill and NOT attribute it to the escape hatch
// (only 4 blocks, escape hatch is at 5).
func TestStopHookCadenceDiff_RCADeathArithmetic(t *testing.T) {
	base := time.Date(2026, 7, 24, 1, 0, 5, 0, time.UTC)
	// Deltas from the RCA: Stop#1 t0, #2 +40s, #3 +177s, #4 +67s.
	blockTimes := []time.Time{
		base,                                    // #1
		base.Add(40 * time.Second),              // #2  gap 40  vs window 30 (count1) → survives
		base.Add((40 + 177) * time.Second),      // #3  gap 177 vs window 60 (count2) → survives
		base.Add((40 + 177 + 67) * time.Second), // #4 gap 67 vs window 120 (count3) → IN COOLDOWN
	}

	d := StopHookCadenceDiff(blockTimes, 30)

	if d.KillBlock != 4 {
		t.Errorf("KillBlock = %d, want 4 (Stop#4 lands in the 120s window)", d.KillBlock)
	}
	if d.EscapeHatchBlock != 0 {
		t.Errorf("EscapeHatchBlock = %d, want 0 (only 4 blocks, escape hatch is at 5)", d.EscapeHatchBlock)
	}
	if len(d.Events) != 4 {
		t.Fatalf("len(Events) = %d, want 4", len(d.Events))
	}
	// Blocks #2 and #3 must survive; #4 must be in cooldown.
	if d.Events[1].InCooldown {
		t.Errorf("block #2 (gap 40 vs window 30) should survive, got InCooldown")
	}
	if d.Events[2].InCooldown {
		t.Errorf("block #3 (gap 177 vs window 60) should survive, got InCooldown")
	}
	if !d.Events[3].InCooldown {
		t.Errorf("block #4 (gap 67 vs window 120) should be InCooldown")
	}
	// Verify the window each block was tested against matches the shared math.
	if got, want := d.Events[3].CooldownWindowSecs, 120; got != want {
		t.Errorf("block #4 window = %d, want %d", got, want)
	}
	if got, want := d.Events[3].GapSecs, int64(67); got != want {
		t.Errorf("block #4 gap = %d, want %d", got, want)
	}
}

func TestStopHookCadenceDiff_NoBlocks(t *testing.T) {
	d := StopHookCadenceDiff(nil, 30)
	if d.KillBlock != 0 || d.EscapeHatchBlock != 0 || len(d.Events) != 0 {
		t.Errorf("empty cadence should have no kill/escape/events, got %+v", d)
	}
	if d.Anomaly == "" {
		t.Error("Anomaly should still describe the no-blocks case")
	}
}

// TestStopHookCadenceDiff_AllSurvive: gaps always exceed the window, so no block
// is force-exited and (with <5 blocks) the escape hatch never fires.
func TestStopHookCadenceDiff_AllSurvive(t *testing.T) {
	base := time.Date(2026, 7, 24, 2, 0, 0, 0, time.UTC)
	blockTimes := []time.Time{
		base,
		base.Add(400 * time.Second), // gap 400 vs window 30
		base.Add(800 * time.Second), // gap 400 vs window 60
	}
	d := StopHookCadenceDiff(blockTimes, 30)
	if d.KillBlock != 0 {
		t.Errorf("KillBlock = %d, want 0 (all gaps exceed windows)", d.KillBlock)
	}
	if d.EscapeHatchBlock != 0 {
		t.Errorf("EscapeHatchBlock = %d, want 0", d.EscapeHatchBlock)
	}
}

// TestStopHookCadenceDiff_EscapeHatch: five rapid blocks. Even though the first
// in-cooldown block is the kill, the escape hatch is also reached at block 5.
func TestStopHookCadenceDiff_EscapeHatch(t *testing.T) {
	base := time.Date(2026, 7, 24, 3, 0, 0, 0, time.UTC)
	var blockTimes []time.Time
	for i := 0; i < 5; i++ {
		// 5s apart — always well inside any window.
		blockTimes = append(blockTimes, base.Add(time.Duration(i*5)*time.Second))
	}
	d := StopHookCadenceDiff(blockTimes, 30)
	if d.EscapeHatchBlock != 5 {
		t.Errorf("EscapeHatchBlock = %d, want 5", d.EscapeHatchBlock)
	}
	// The first rapid block after #1 is in cooldown → kill at #2.
	if d.KillBlock != 2 {
		t.Errorf("KillBlock = %d, want 2 (block #2 at gap 5 < window 30)", d.KillBlock)
	}
}

// TestStopHookCadenceDiff_MissingTimestamps: blocks with zero timestamps cannot
// be diffed (no gap), so none are flagged in-cooldown.
func TestStopHookCadenceDiff_MissingTimestamps(t *testing.T) {
	d := StopHookCadenceDiff([]time.Time{{}, {}, {}}, 30)
	if d.KillBlock != 0 {
		t.Errorf("KillBlock = %d, want 0 with no timestamps", d.KillBlock)
	}
	if len(d.Events) != 3 {
		t.Errorf("len(Events) = %d, want 3", len(d.Events))
	}
}
