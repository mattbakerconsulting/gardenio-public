package scheduler

import "testing"

// --- ParseStopSpec ---

func TestParseStopSpec_Empty(t *testing.T) {
	spec, err := ParseStopSpec("")
	if err != nil {
		t.Fatalf("empty input should not error: %v", err)
	}
	if !spec.IsZero() {
		t.Errorf("empty input should yield zero spec, got %+v", spec)
	}
}

func TestParseStopSpec_AllSlots(t *testing.T) {
	raw := `{
		"loop_end":  {"any": [{"kind":"max_runs"}]},
		"run_end":   {"any": [{"kind":"wall_clock","duration":"30m"}]},
		"hard_stop": {"any": [{"kind":"consecutive_failures","value":"5"}]},
		"on_loop_end": "close"
	}`
	spec, err := ParseStopSpec(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.LoopEnd.Any) != 1 || spec.LoopEnd.Any[0].Kind != StopKindMaxRuns {
		t.Errorf("loop_end: %+v", spec.LoopEnd)
	}
	if len(spec.RunEnd.Any) != 1 || spec.RunEnd.Any[0].Duration != "30m" {
		t.Errorf("run_end: %+v", spec.RunEnd)
	}
	if len(spec.HardStop.Any) != 1 || spec.HardStop.Any[0].Value != "5" {
		t.Errorf("hard_stop: %+v", spec.HardStop)
	}
	if spec.OnLoopEnd != "close" {
		t.Errorf("on_loop_end: %q", spec.OnLoopEnd)
	}
	if spec.IsZero() {
		t.Error("non-empty spec should not be zero")
	}
}

// Regression for gd-Afhs2rwmQk: schedule authors sometimes write
// `"value": 5` (JSON number) instead of `"value": "5"`. Accept both so the
// entry is not silently dropped with a WARN every reconcile tick.
func TestParseStopSpec_NumericValueAccepted(t *testing.T) {
	raw := `{
		"hard_stop": {"any": [{"kind":"consecutive_failures","value":5}]}
	}`
	spec, err := ParseStopSpec(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.HardStop.Any) != 1 || spec.HardStop.Any[0].Value != "5" {
		t.Errorf("numeric value should canonicalize to \"5\", got %+v", spec.HardStop.Any)
	}
}

func TestParseStopSpec_NumericValueWithFraction(t *testing.T) {
	raw := `{"hard_stop":{"any":[{"kind":"consecutive_failures","value":2.5}]}}`
	spec, err := ParseStopSpec(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.HardStop.Any) != 1 || spec.HardStop.Any[0].Value != "2.5" {
		t.Errorf("fractional value should canonicalize to \"2.5\", got %+v", spec.HardStop.Any)
	}
}

func TestParseStopSpec_StringValueStillWorks(t *testing.T) {
	raw := `{"hard_stop":{"any":[{"kind":"consecutive_failures","value":"7"}]}}`
	spec, err := ParseStopSpec(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(spec.HardStop.Any) != 1 || spec.HardStop.Any[0].Value != "7" {
		t.Errorf("string value should round-trip, got %+v", spec.HardStop.Any)
	}
}

func TestParseStopSpec_Malformed(t *testing.T) {
	if _, err := ParseStopSpec(`{not valid`); err == nil {
		t.Error("malformed JSON should error")
	}
}
