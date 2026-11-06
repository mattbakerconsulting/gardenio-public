package contextwindow

import "testing"

func TestWindowTokensForModel(t *testing.T) {
	cases := map[string]int64{
		"":                        DefaultWindowTokens, // account default → 200k
		"claude-opus-4-8":         DefaultWindowTokens,
		"claude-sonnet-5":         DefaultWindowTokens,
		"claude-haiku-4-5":        DefaultWindowTokens,
		"opus":                    DefaultWindowTokens,
		"sonnet":                  DefaultWindowTokens,
		"glm-5":                   DefaultWindowTokens, // unrecognized → fail-safe default
		"claude-fable-5":          LargeWindowTokens,   // 1M-context model
		"fable":                   LargeWindowTokens,
		"claude-fable-5-20260609": LargeWindowTokens, // future dated snapshot flows through
		"claude-opus-4-8-1m":      LargeWindowTokens, // opt-in 1M beta marker
		"  Claude-Fable-5  ":      LargeWindowTokens, // trimmed + case-insensitive
	}
	for model, want := range cases {
		if got := WindowTokensForModel(model); got != want {
			t.Errorf("WindowTokensForModel(%q) = %d, want %d", model, got, want)
		}
	}
}

func TestEffectiveWindowTokens(t *testing.T) {
	// Absolute reserve: window − 20k output − 13k summary = window − 33k.
	// 200k → 167000 (~83.5%); 1M → 967000 (~96.7%) — same reserve, different fraction.
	if got := EffectiveWindowTokens(""); got != 167_000 {
		t.Errorf("EffectiveWindowTokens(default) = %d, want 167000", got)
	}
	if got := EffectiveWindowTokens("claude-fable-5"); got != 967_000 {
		t.Errorf("EffectiveWindowTokens(fable) = %d, want 967000", got)
	}
}

func TestOccupancyPct(t *testing.T) {
	cases := []struct {
		name  string
		ctx   int64
		model string
		want  int
	}{
		{"128k on default 200k window", 128_000, "", 76},     // 128000*100/167000
		{"160k on default 200k window", 160_000, "opus", 95}, // near compaction
		{"at the threshold reads 100", 167_000, "", 100},
		{"overshoot exceeds 100 (not clamped)", 190_000, "", 113},              // 190000*100/167000
		{"same reading is small on a 1M model", 200_000, "claude-fable-5", 20}, // 200000*100/967000
		{"zero reading → 0", 0, "opus", 0},
		{"negative reading → 0", -5, "opus", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OccupancyPct(tc.ctx, tc.model); got != tc.want {
				t.Errorf("OccupancyPct(%d, %q) = %d, want %d", tc.ctx, tc.model, got, tc.want)
			}
		})
	}
}

func TestOccupancyPctForAgent(t *testing.T) {
	tests := []struct {
		name      string
		fields    map[string]string
		used      int64
		wantPct   int
		wantKnown bool
	}{
		{"claude default matches OccupancyPct", map[string]string{"model": "claude-opus-4-8"}, 83_500, 50, true},
		{"claude no model", map[string]string{}, 167_000, 100, true},
		{"claude fable 1M", map[string]string{"model": "claude-fable-5"}, 483_500, 50, true},
		{"explicit claude harness", map[string]string{"harness": "claude", "model": "opus"}, 83_500, 50, true},
		{"codex without window: no estimate", map[string]string{"harness": "codex", "model": "gpt-5-codex"}, 90_000, 0, false},
		{"codex with bad window: no estimate", map[string]string{"harness": "codex", FieldModelContextWindow: "lots"}, 90_000, 0, false},
		{"codex zero window: no estimate", map[string]string{"harness": "codex", FieldModelContextWindow: "0"}, 90_000, 0, false},
		{"codex with reported window", map[string]string{"harness": "codex", FieldModelContextWindow: "272000"}, 136_000, 50, true},
		{"codex reported window, nothing used", map[string]string{"harness": "codex", FieldModelContextWindow: "272000"}, 0, 0, true},
		{"gemini without window: no estimate", map[string]string{"harness": "gemini", "model": "gemini-2.5-pro"}, 90_000, 0, false},
		{"gemini with reported window", map[string]string{"harness": "gemini", FieldModelContextWindow: "1048576"}, 524_288, 50, true},
		{"claude window field ignored", map[string]string{FieldModelContextWindow: "1000"}, 83_500, 50, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pct, known := OccupancyPctForAgent(tc.fields, tc.used)
			if pct != tc.wantPct || known != tc.wantKnown {
				t.Errorf("OccupancyPctForAgent = (%d,%v), want (%d,%v)", pct, known, tc.wantPct, tc.wantKnown)
			}
		})
	}
}

// Claude agents must see byte-identical numbers to the pre-harness helpers (gd-MolQ67L3xX).
func TestAgentWindow_ClaudeParity(t *testing.T) {
	for _, model := range []string{"", "opus", "claude-fable-5", "claude-opus-4-8-1m"} {
		raw, thr, known := AgentWindow(map[string]string{"model": model})
		if !known || raw != WindowTokensForModel(model) || thr != EffectiveWindowTokens(model) {
			t.Errorf("model %q: AgentWindow = (%d,%d,%v)", model, raw, thr, known)
		}
		if got, _ := OccupancyPctForAgent(map[string]string{"model": model}, 123_456); got != OccupancyPct(123_456, model) {
			t.Errorf("model %q: pct %d != OccupancyPct %d", model, got, OccupancyPct(123_456, model))
		}
	}
}
