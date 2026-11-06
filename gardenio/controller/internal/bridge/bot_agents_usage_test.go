package bridge

import (
	"strings"
	"testing"
)

func TestFormatAgentContextChip(t *testing.T) {
	got := formatAgentContextChip(Garden, map[string]string{"context_tokens": "128000"}, true)
	want := "🧺 128.0k ctx (76%)"
	if got != want {
		t.Errorf("formatAgentContextChip(live) = %q, want %q", got, want)
	}

	// Closed: same reading, qualified so it cannot be misread as live
	// (ka-0000003171).
	got = formatAgentContextChip(Garden, map[string]string{"context_tokens": "128000"}, false)
	want = "🧺 128.0k ctx at exit (76%)"
	if got != want {
		t.Errorf("formatAgentContextChip(closed) = %q, want %q", got, want)
	}

	// Absent/zero/unparseable render nothing, live or closed — a run that never
	// reported context gets no chip rather than a bogus "0 ctx (0%)".
	for _, live := range []bool{true, false} {
		if s := formatAgentContextChip(Garden, map[string]string{}, live); s != "" {
			t.Errorf("live=%v: expected empty for missing context_tokens, got %q", live, s)
		}
		if s := formatAgentContextChip(Garden, map[string]string{"context_tokens": "0"}, live); s != "" {
			t.Errorf("live=%v: expected empty for zero context_tokens, got %q", live, s)
		}
		if s := formatAgentContextChip(Garden, map[string]string{"context_tokens": "abc"}, live); s != "" {
			t.Errorf("live=%v: expected empty for unparseable context_tokens, got %q", live, s)
		}
	}
}

func TestFormatAgentUsageLine_Live(t *testing.T) {
	got := formatAgentUsageLine(Garden, map[string]string{
		"context_tokens": "100000",
		"total_cost_usd": "1.00",
		"live_cost_usd":  "0.50",
	}, true)
	// 100k → 59% of the ~167k auto-compact threshold, cost = total(1.00) + live(0.50) = 1.50.
	want := "🧺 100.0k ctx (59%) · 🍯 $1.50"
	if got != want {
		t.Errorf("formatAgentUsageLine(live) = %q, want %q", got, want)
	}
}

// TestFormatAgentUsageLine_ClosedKeepsContextAtExitAndOmitsLiveCost pins the
// completed-agent contract (ka-0000003171): the rollup MUST still render for a
// closed agent — that is when the operator most wants it — with two differences
// from the live line. Context renders as the frozen last reading, labelled "at
// exit" so it is not mistaken for a live figure, and cost is the reconciled
// total_cost_usd ALONE (the stale live_cost_usd estimate would double-count what
// the session-close reconciler already folded in).
func TestFormatAgentUsageLine_ClosedKeepsContextAtExitAndOmitsLiveCost(t *testing.T) {
	got := formatAgentUsageLine(Garden, map[string]string{
		"context_tokens": "100000",
		"total_cost_usd": "1.00",
		"live_cost_usd":  "0.50",
	}, false)
	want := "🧺 100.0k ctx at exit (59%) · 🍯 $1.00"
	if got != want {
		t.Errorf("formatAgentUsageLine(closed) = %q, want %q", got, want)
	}
}

func TestFormatAgentUsageLine_EmptyRendersNothing(t *testing.T) {
	if s := formatAgentUsageLine(Garden, map[string]string{}, true); s != "" {
		t.Errorf("expected empty usage line, got %q", s)
	}
	if s := formatAgentUsageLine(Garden, nil, true); s != "" {
		t.Errorf("expected empty usage line for nil fields, got %q", s)
	}
}

// TestFormatAgentTurns exercises the live-vs-closed field selection and the
// omission rules for the ♻️ turns chip in isolation.
func TestFormatAgentTurns(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		live   bool
		want   string
	}{
		{
			name:   "live reads live_turns",
			fields: map[string]string{"live_turns": "42", "total_request_count": "99"},
			live:   true,
			want:   "♻️ 42",
		},
		{
			name:   "closed reads total_request_count",
			fields: map[string]string{"live_turns": "42", "total_request_count": "99"},
			live:   false,
			want:   "♻️ 99",
		},
		{
			// Parity with the viewer seam: a live agent with no live_turns yet
			// falls back to the authoritative total_request_count instead of
			// dropping the chip (ka-0000003042).
			name:   "live falls back to total_request_count when live_turns absent",
			fields: map[string]string{"total_request_count": "99"},
			live:   true,
			want:   "♻️ 99",
		},
		{
			// Fallback also fires when live_turns is present but zero (not-yet-
			// mirrored), matching buildAgentUsageView's `lt > 0` guard.
			name:   "live falls back on zero live_turns",
			fields: map[string]string{"live_turns": "0", "total_request_count": "99"},
			live:   true,
			want:   "♻️ 99",
		},
		{
			name:   "closed ignores live_turns when total_request_count absent",
			fields: map[string]string{"live_turns": "42"},
			live:   false,
			want:   "",
		},
		{
			name:   "large count is humanized",
			fields: map[string]string{"live_turns": "1500"},
			live:   true,
			want:   "♻️ 1.5k",
		},
		{
			name:   "zero renders nothing",
			fields: map[string]string{"live_turns": "0"},
			live:   true,
			want:   "",
		},
		{
			name:   "negative renders nothing",
			fields: map[string]string{"total_request_count": "-3"},
			live:   false,
			want:   "",
		},
		{
			name:   "unparseable renders nothing",
			fields: map[string]string{"live_turns": "abc"},
			live:   true,
			want:   "",
		},
		{
			name:   "missing renders nothing",
			fields: map[string]string{},
			live:   true,
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAgentTurns(Garden, tc.fields, tc.live); got != tc.want {
				t.Errorf("formatAgentTurns(%v, live=%v) = %q, want %q", tc.fields, tc.live, got, tc.want)
			}
		})
	}
}

// TestFormatAgentUsageLine_TurnsMatrix covers turns present/absent × live/closed
// against the full usage line, confirming the ♻️ chip appends after cost
// (the 🧠 · 💰 · 🔁 triad order) and that cost rendering is unchanged.
func TestFormatAgentUsageLine_TurnsMatrix(t *testing.T) {
	tests := []struct {
		name   string
		fields map[string]string
		live   bool
		want   string
	}{
		{
			name: "live: ctx + cost + live turns (full triad)",
			fields: map[string]string{
				"context_tokens": "100000",
				"total_cost_usd": "1.00",
				"live_cost_usd":  "0.50",
				"live_turns":     "12",
			},
			live: true,
			want: "🧺 100.0k ctx (59%) · 🍯 $1.50 · ♻️ 12",
		},
		{
			// Closed keeps the FULL triad — ctx (frozen, "at exit"), reconciled
			// cost only, authoritative total turns (ka-0000003171).
			name: "closed: ctx at exit + reconciled cost + total turns (full triad)",
			fields: map[string]string{
				"context_tokens":      "100000",
				"total_cost_usd":      "1.00",
				"live_cost_usd":       "0.50",
				"live_turns":          "12",
				"total_request_count": "20",
			},
			live: false,
			want: "🧺 100.0k ctx at exit (59%) · 🍯 $1.00 · ♻️ 20",
		},
		{
			name: "live: turns present, cost absent — chip still shows without a leading separator",
			fields: map[string]string{
				"context_tokens": "100000",
				"live_turns":     "7",
			},
			live: true,
			want: "🧺 100.0k ctx (59%) · ♻️ 7",
		},
		{
			name: "live: turns absent leaves the pre-turns line unchanged",
			fields: map[string]string{
				"context_tokens": "100000",
				"total_cost_usd": "1.00",
				"live_cost_usd":  "0.50",
			},
			live: true,
			want: "🧺 100.0k ctx (59%) · 🍯 $1.50",
		},
		{
			name: "closed: turns absent leaves cost-only line unchanged",
			fields: map[string]string{
				"total_cost_usd": "1.00",
			},
			live: false,
			want: "🍯 $1.00",
		},
		{
			name:   "live: turns only (no ctx, no cost) renders a bare chip",
			fields: map[string]string{"live_turns": "3"},
			live:   true,
			want:   "♻️ 3",
		},
		{
			name:   "live: live_turns zero with no total renders nothing extra",
			fields: map[string]string{"total_cost_usd": "1.00", "live_turns": "0"},
			live:   true,
			want:   "🍯 $1.00",
		},
		{
			// The parity fix in context: a live multi-session agent whose
			// live_turns has not been mirrored yet still shows its accumulated
			// total_request_count on the Slack card (ka-0000003042), so the 🔁
			// chip no longer blinks out vs the viewer.
			name: "live: no live_turns falls back to total in the full line",
			fields: map[string]string{
				"context_tokens":      "100000",
				"total_cost_usd":      "1.00",
				"live_cost_usd":       "0.50",
				"total_request_count": "20",
			},
			live: true,
			want: "🧺 100.0k ctx (59%) · 🍯 $1.50 · ♻️ 20",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatAgentUsageLine(Garden, tc.fields, tc.live); got != tc.want {
				t.Errorf("formatAgentUsageLine(%v, live=%v) = %q, want %q", tc.fields, tc.live, got, tc.want)
			}
		})
	}
}

// TestFormatAgentTurns_ViewerParity locks the Slack seam's turn-count selection
// to the beads-viewer seam's (buildAgentUsageView in cmd/beads-viewer/
// agent_usage.go). The two render functions live in different packages and are
// maintained independently, so this table restates the viewer's documented rule
// — live → live_turns if > 0 else total_request_count; closed → total_request_count
// — as the expected raw count and asserts formatAgentTurns renders exactly that.
// If either seam's selection drifts, this test (and its viewer twin,
// TestBuildAgentUsageView_Turns) diverge, surfacing the Phase 4 cross-check
// regression before it ships (gd-iz3kCe6twr / ka-0000003042).
func TestFormatAgentTurns_ViewerParity(t *testing.T) {
	// wantCount encodes the shared selection rule; -1 means "no chip".
	viewerWant := func(fields map[string]string, live bool) int64 {
		total := parseTurnField(fields["total_request_count"])
		n := total
		if live {
			if lt := parseTurnField(fields["live_turns"]); lt > 0 {
				n = lt
			}
		}
		if n <= 0 {
			return -1
		}
		return n
	}
	cases := []struct {
		live               bool
		liveTurns, totalRC string
	}{
		{true, "47", "12"},  // live prefers live_turns
		{true, "", "12"},    // live falls back to total_request_count
		{true, "0", "9"},    // live falls back on zero live_turns
		{false, "47", "50"}, // closed uses total_request_count, ignores stale live_turns
		{false, "47", ""},   // closed with no total → no chip even though live_turns set
		{true, "", ""},      // nothing → no chip
	}
	for _, c := range cases {
		fields := map[string]string{}
		if c.liveTurns != "" {
			fields["live_turns"] = c.liveTurns
		}
		if c.totalRC != "" {
			fields["total_request_count"] = c.totalRC
		}
		want := ""
		if n := viewerWant(fields, c.live); n >= 0 {
			want = "♻️ " + formatTokenCount(n)
		}
		if got := formatAgentTurns(Garden, fields, c.live); got != want {
			t.Errorf("parity drift: formatAgentTurns(%v, live=%v) = %q, viewer would render %q",
				fields, c.live, got, want)
		}
	}
}

func TestAgentStateIsLive(t *testing.T) {
	live := []string{"working", "spawning", "yielding", "idle", "rate_limited"}
	for _, s := range live {
		if !agentStateIsLive(s) {
			t.Errorf("agentStateIsLive(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"done", "failed", ""} {
		if agentStateIsLive(s) {
			t.Errorf("agentStateIsLive(%q) = true, want false", s)
		}
	}
}

// TestFormatAgentTTFT pins the boot→first-token chip (ka-0000003911): sub-10s
// keeps one decimal, larger cold-boot values round to whole seconds, and an
// absent / zero / unparseable ttft_ms renders nothing (never a bogus "0s").
func TestFormatAgentTTFT(t *testing.T) {
	cases := []struct {
		ms   string
		want string
	}{
		{"1200", "🐇 TTFT 1.2s"},
		{"500", "🐇 TTFT 0.5s"},
		{"9900", "🐇 TTFT 9.9s"},
		{"10000", "🐇 TTFT 10s"},
		{"12400", "🐇 TTFT 12s"},
		{"", ""},
		{"0", ""},
		{"-5", ""},
		{"abc", ""},
	}
	for _, c := range cases {
		got := formatAgentTTFT(Garden, map[string]string{"ttft_ms": c.ms})
		if got != c.want {
			t.Errorf("formatAgentTTFT(ttft_ms=%q) = %q, want %q", c.ms, got, c.want)
		}
	}
}

// TestFormatAgentTTFT_RendersOnUsageLines verifies the TTFT chip is appended to
// both card rollup lines (the compact usage triad and the full-card context/turns
// line), for both live and closed agents.
func TestFormatAgentTTFT_RendersOnUsageLines(t *testing.T) {
	fields := map[string]string{
		"context_tokens":      "100000",
		"total_request_count": "7",
		"ttft_ms":             "1200",
	}
	for _, live := range []bool{true, false} {
		if got := formatAgentUsageLine(Garden, fields, live); !strings.Contains(got, "🐇 TTFT 1.2s") {
			t.Errorf("live=%v: usage line missing TTFT chip, got %q", live, got)
		}
		if got := formatAgentContextTurns(Garden, fields, live); !strings.Contains(got, "🐇 TTFT 1.2s") {
			t.Errorf("live=%v: context/turns line missing TTFT chip, got %q", live, got)
		}
	}
	// A bead that never measured TTFT gets no chip on either line.
	noTTFT := map[string]string{"context_tokens": "100000"}
	if got := formatAgentUsageLine(Garden, noTTFT, true); strings.Contains(got, "TTFT") {
		t.Errorf("expected no TTFT chip when ttft_ms absent, got %q", got)
	}
	if got := formatAgentContextTurns(Garden, noTTFT, true); strings.Contains(got, "TTFT") {
		t.Errorf("expected no TTFT chip when ttft_ms absent, got %q", got)
	}
}

// TestUsageChips_BusinessTheme asserts the usage chips resolve through the
// theme: a Business channel gets bracket tags in place of Garden's glyphs, each
// glued to its verbal label, and never a bare legacy shortcode (gd-wKyW84WezG).
func TestUsageChips_BusinessTheme(t *testing.T) {
	fields := map[string]string{
		"context_tokens":      "100000",
		"total_cost_usd":      "1.50",
		"total_request_count": "12",
		"ttft_ms":             "1200",
	}
	got := formatAgentUsageLine(Business, fields, false)
	want := "[CTX] 100.0k ctx at exit (59%) · [COST] $1.50 · [TURNS] 12 · [TTFT] TTFT 1.2s"
	if got != want {
		t.Errorf("Business usage line = %q, want %q", got, want)
	}
	got = formatAgentContextTurns(Business, fields, false)
	want = "[CTX] 100.0k ctx at exit (59%) · [TURNS] 12 · [TTFT] TTFT 1.2s"
	if got != want {
		t.Errorf("Business context/turns = %q, want %q", got, want)
	}
	if strings.Contains(got, ":brain:") || strings.Contains(got, ":repeat:") || strings.Contains(got, ":moneybag:") {
		t.Errorf("Business output leaked a legacy shortcode: %q", got)
	}
}

// A Codex agent has no Claude window table: with no reported window the chip
// shows the raw count and no percentage (ADR 0004 R7); a reported window gives
// a real percentage.
func TestFormatAgentContextChip_Codex(t *testing.T) {
	codex := map[string]string{"context_tokens": "128000", "harness": "codex", "model": "gpt-5-codex"}
	if got, want := formatAgentContextChip(Garden, codex, true), "🧺 128.0k ctx"; got != want {
		t.Errorf("codex live, unknown window = %q, want %q", got, want)
	}
	if got, want := formatAgentContextChip(Garden, codex, false), "🧺 128.0k ctx at exit"; got != want {
		t.Errorf("codex closed, unknown window = %q, want %q", got, want)
	}
	codex["model_context_window"] = "256000"
	if got, want := formatAgentContextChip(Garden, codex, true), "🧺 128.0k ctx (50%)"; got != want {
		t.Errorf("codex live, reported window = %q, want %q", got, want)
	}
}
