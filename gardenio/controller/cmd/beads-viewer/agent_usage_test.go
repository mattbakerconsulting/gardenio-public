package main

import (
	"strings"
	"testing"
)

func TestBuildAgentUsageView_LiveContextAndCost(t *testing.T) {
	v := buildAgentUsageView(map[string]string{
		"context_tokens": "128000",
		"live_cost_usd":  "0.50",
		"total_cost_usd": "1.00",
	}, true)

	if !v.HasContext {
		t.Fatal("expected HasContext true")
	}
	if v.ContextLabel != "128.0k" {
		t.Errorf("ContextLabel = %q, want 128.0k", v.ContextLabel)
	}
	// 128k against the default 200k window's ~167k auto-compact threshold → 76%.
	if v.ContextPct != 76 {
		t.Errorf("ContextPct = %d, want 76", v.ContextPct)
	}
	// Live cost = total (1.00) + live estimate (0.50) = 1.50.
	if v.CostLabel != "$1.50" {
		t.Errorf("CostLabel = %q, want $1.50", v.CostLabel)
	}
}

// TestBuildAgentUsageView_ClosedKeepsContextAtExitAndOmitsLiveCost pins the
// completed-agent contract (ka-0000003171): a closed agent still gets its context
// figure — the frozen last reading before teardown — flagged ContextAtExit so the
// chip qualifies itself instead of implying a live occupancy. Cost stays the
// reconciled total_cost_usd ALONE, never the stale live estimate (which would
// double-count what the session-close reconciler already folded in).
func TestBuildAgentUsageView_ClosedKeepsContextAtExitAndOmitsLiveCost(t *testing.T) {
	v := buildAgentUsageView(map[string]string{
		"context_tokens": "128000",
		"live_cost_usd":  "0.50",
		"total_cost_usd": "1.00",
	}, false)

	if !v.HasContext {
		t.Error("closed agent should keep its final context reading")
	}
	if !v.ContextAtExit {
		t.Error("a closed agent's context must be flagged ContextAtExit so the chip can qualify it")
	}
	if v.ContextLabel != "128.0k" {
		t.Errorf("ContextLabel = %q, want 128.0k", v.ContextLabel)
	}
	if !strings.Contains(v.ContextTitle, "at exit") {
		t.Errorf("ContextTitle should mark the reading as final, got %q", v.ContextTitle)
	}
	if v.CostLabel != "$1.00" {
		t.Errorf("CostLabel = %q, want $1.00 (total only)", v.CostLabel)
	}
}

// TestBuildAgentUsageView_LiveContextIsNotFlaggedAtExit is the other half of the
// contract: a running agent's context is a live figure and must NOT be qualified.
func TestBuildAgentUsageView_LiveContextIsNotFlaggedAtExit(t *testing.T) {
	v := buildAgentUsageView(map[string]string{"context_tokens": "128000"}, true)
	if !v.HasContext {
		t.Fatal("live agent should show a context size")
	}
	if v.ContextAtExit {
		t.Error("a live agent's context must not be flagged ContextAtExit")
	}
	if strings.Contains(v.ContextTitle, "at exit") {
		t.Errorf("live ContextTitle should not say 'at exit', got %q", v.ContextTitle)
	}
}

func TestBuildAgentUsageView_EmptyRendersNothing(t *testing.T) {
	v := buildAgentUsageView(map[string]string{}, true)
	if v.HasAny() {
		t.Errorf("expected no usage for empty fields, got %+v", v)
	}
}

func TestBuildAgentUsageView_Turns(t *testing.T) {
	cases := []struct {
		name      string
		fields    map[string]string
		live      bool
		wantHas   bool
		wantLabel string
	}{
		{
			// Live agent prefers live_turns over total_request_count.
			name:      "live prefers live_turns",
			fields:    map[string]string{"live_turns": "47", "total_request_count": "12"},
			live:      true,
			wantHas:   true,
			wantLabel: "47",
		},
		{
			// Live agent falls back to total_request_count when live_turns is absent.
			name:      "live falls back to total_request_count",
			fields:    map[string]string{"total_request_count": "12"},
			live:      true,
			wantHas:   true,
			wantLabel: "12",
		},
		{
			// Live agent falls back when live_turns is zero.
			name:      "live falls back on zero live_turns",
			fields:    map[string]string{"live_turns": "0", "total_request_count": "9"},
			live:      true,
			wantHas:   true,
			wantLabel: "9",
		},
		{
			// Closed agent always uses total_request_count, ignoring any stale live_turns.
			name:      "closed uses total_request_count",
			fields:    map[string]string{"live_turns": "47", "total_request_count": "50"},
			live:      false,
			wantHas:   true,
			wantLabel: "50",
		},
		{
			// No turn counts → no chip.
			name:    "absent renders nothing",
			fields:  map[string]string{},
			live:    true,
			wantHas: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := buildAgentUsageView(tc.fields, tc.live)
			if v.HasTurns != tc.wantHas {
				t.Errorf("HasTurns = %v, want %v", v.HasTurns, tc.wantHas)
			}
			if v.TurnsLabel != tc.wantLabel {
				t.Errorf("TurnsLabel = %q, want %q", v.TurnsLabel, tc.wantLabel)
			}
		})
	}
}

func TestBuildAgentUsageView_ContextThresholds(t *testing.T) {
	// 160k against the default 200k window's ~167k auto-compact threshold → 95%
	// → high band (compaction is imminent).
	v := buildAgentUsageView(map[string]string{"context_tokens": "160000"}, true)
	if v.ContextPct != 95 {
		t.Errorf("ContextPct = %d, want 95", v.ContextPct)
	}
}

// TestBuildAgentUsageView_PerModelWindow pins the per-model denominator: the same
// raw context reading is a far smaller fraction of a long-context (1M-window)
// model than of the 200k default, so the percentage must reflect the model's
// real window rather than a fixed 200k. 200k of a 1M window's ~967k auto-compact
// threshold → 20%, versus reading as over-full against a fixed 200k.
func TestBuildAgentUsageView_PerModelWindow(t *testing.T) {
	v := buildAgentUsageView(map[string]string{
		"context_tokens": "200000",
		"model":          "claude-fable-5",
	}, true)
	if v.ContextPct != 20 {
		t.Errorf("ContextPct = %d, want 20 (200k of a 1M-window model)", v.ContextPct)
	}
	if !strings.Contains(v.ContextTitle, "1000k window") {
		t.Errorf("ContextTitle should name the 1M window, got %q", v.ContextTitle)
	}
}

func TestHumanizeTokenCount(t *testing.T) {
	cases := map[int64]string{
		500:       "500",
		1234:      "1.2k",
		128000:    "128.0k",
		1_234_567: "1.2M",
	}
	for in, want := range cases {
		if got := humanizeTokenCount(in); got != want {
			t.Errorf("humanizeTokenCount(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestContextLevel pins the warn/high band thresholds shared by the compact
// usage chip and the context gauge (ka-0000006399) so the two surfaces colour
// identically.
func TestContextLevel(t *testing.T) {
	cases := []struct {
		pct        int
		hasContext bool
		want       string
	}{
		{0, false, ""},  // no context reported → no band
		{0, true, "ok"}, // reported but empty occupancy still classifies ok
		{59, true, "ok"},
		{60, true, "warn"},
		{79, true, "warn"},
		{80, true, "high"},
		{130, true, "high"},
	}
	for _, c := range cases {
		v := agentUsageView{HasContext: c.hasContext, ContextPct: c.pct}
		if got := v.ContextLevel(); got != c.want {
			t.Errorf("ContextLevel(pct=%d, has=%v) = %q, want %q", c.pct, c.hasContext, got, c.want)
		}
	}
}

// TestContextBarPct clamps the gauge fill width to 0..100 even when occupancy
// reads slightly over the auto-compact threshold.
func TestContextBarPct(t *testing.T) {
	cases := map[int]int{-5: 0, 0: 0, 69: 69, 100: 100, 130: 100}
	for in, want := range cases {
		v := agentUsageView{ContextPct: in}
		if got := v.ContextBarPct(); got != want {
			t.Errorf("ContextBarPct(%d) = %d, want %d", in, got, want)
		}
	}
}

func TestGroupThousands(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		999:     "999",
		1000:    "1,000",
		128000:  "128,000",
		1234567: "1,234,567",
	}
	for in, want := range cases {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildAgentUsageView_CodexWindowUnknownShowsNoPercent(t *testing.T) {
	v := buildAgentUsageView(map[string]string{
		"context_tokens": "128000", "harness": "codex", "model": "gpt-5-codex",
	}, true)
	if !v.HasContext || v.ContextLabel != "128.0k" {
		t.Fatalf("view = %+v", v)
	}
	if v.ContextPct != 0 {
		t.Errorf("ContextPct = %d, want 0 (unknown, not a Claude-window guess)", v.ContextPct)
	}
	if !strings.Contains(v.ContextTitle, "window unknown") || strings.Contains(v.ContextTitle, "%") {
		t.Errorf("ContextTitle = %q", v.ContextTitle)
	}
}

func TestBuildAgentUsageView_CodexReportedWindow(t *testing.T) {
	v := buildAgentUsageView(map[string]string{
		"context_tokens": "128000", "harness": "codex", "model_context_window": "256000",
	}, true)
	if v.ContextPct != 50 || !strings.Contains(v.ContextTitle, "256k window") {
		t.Errorf("view = %+v", v)
	}
}
