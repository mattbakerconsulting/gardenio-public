package main

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestExtractSessionDuration(t *testing.T) {
	lines := []string{
		`{"type":"user","timestamp":"2026-03-20T10:00:00.000Z","message":{"role":"user"}}`,
		`{"type":"assistant","timestamp":"2026-03-20T10:05:00.000Z","message":{"role":"assistant"}}`,
		`{"type":"assistant","timestamp":"2026-03-20T10:30:00.000Z","message":{"role":"assistant"}}`,
	}

	f := writeTempJSONL(t, lines)
	defer os.Remove(f)

	dur, err := extractSessionDuration(f)
	if err != nil {
		t.Fatalf("extractSessionDuration: %v", err)
	}

	expected := 30 * time.Minute
	if dur != expected {
		t.Errorf("expected %v, got %v", expected, dur)
	}
}

func TestExtractSessionDuration_NoTimestamps(t *testing.T) {
	lines := []string{
		`{"type":"user"}`,
		`{"type":"assistant"}`,
	}

	f := writeTempJSONL(t, lines)
	defer os.Remove(f)

	_, err := extractSessionDuration(f)
	if err == nil {
		t.Error("expected error for log with no timestamps")
	}
}

func TestExtractSessionDuration_Empty(t *testing.T) {
	f := writeTempJSONL(t, nil)
	defer os.Remove(f)

	_, err := extractSessionDuration(f)
	if err == nil {
		t.Error("expected error for empty log")
	}
}

func TestExtractTimestamp(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantZero bool
	}{
		{"valid", `{"timestamp":"2026-03-20T10:00:00.000Z"}`, false},
		{"no timestamp", `{"type":"result"}`, true},
		{"invalid json", `not json`, true},
		{"empty timestamp", `{"timestamp":""}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := extractTimestamp([]byte(tt.line))
			if tt.wantZero && !ts.IsZero() {
				t.Errorf("expected zero time, got %v", ts)
			}
			if !tt.wantZero && ts.IsZero() {
				t.Error("expected non-zero time")
			}
		})
	}
}

func TestBuildAnalyzeResult(t *testing.T) {
	usage := &beadsapi.SessionUsage{
		InputTokens:      1000,
		OutputTokens:     200,
		CacheReadTokens:  500,
		CacheWriteTokens: 100,
		TotalCostUSD:     1.50,
		RequestCount:     5,
		ToolCallCount:    10,
		ErrorCount:       2,
		CompactionCount:  1,
	}

	result := buildAnalyzeResult("/tmp/test.jsonl", usage, 30*time.Minute)

	if result.File != "/tmp/test.jsonl" {
		t.Errorf("file: expected /tmp/test.jsonl, got %s", result.File)
	}
	if result.DurationSeconds != 1800 {
		t.Errorf("duration_seconds: expected 1800, got %f", result.DurationSeconds)
	}
	if result.InputTokens != 1000 {
		t.Errorf("input_tokens: expected 1000, got %d", result.InputTokens)
	}
	if result.ErrorRate != 20.0 {
		t.Errorf("error_rate: expected 20.0, got %f", result.ErrorRate)
	}
}

func TestBuildAnalyzeResult_ZeroToolCalls(t *testing.T) {
	usage := &beadsapi.SessionUsage{
		ToolCallCount: 0,
		ErrorCount:    0,
	}

	result := buildAnalyzeResult("test.jsonl", usage, 0)
	if result.ErrorRate != 0 {
		t.Errorf("error_rate: expected 0 with no tool calls, got %f", result.ErrorRate)
	}
}

func TestBuildAnalyzeResult_JSONRoundtrip(t *testing.T) {
	usage := &beadsapi.SessionUsage{
		InputTokens:  5000,
		OutputTokens: 1000,
		TotalCostUSD: 12.50,
		RequestCount: 10,
		ToolCallCount: 20,
		ErrorCount:    3,
	}

	result := buildAnalyzeResult("test.jsonl", usage, 2*time.Hour)

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	var decoded sessionAnalyzeResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	if decoded.InputTokens != 5000 {
		t.Errorf("input_tokens after roundtrip: expected 5000, got %d", decoded.InputTokens)
	}
	if math.Abs(decoded.TotalCostUSD-12.50) > 0.001 {
		t.Errorf("total_cost_usd after roundtrip: expected 12.50, got %f", decoded.TotalCostUSD)
	}
	if decoded.Duration != "2h0m0s" {
		t.Errorf("duration after roundtrip: expected 2h0m0s, got %s", decoded.Duration)
	}
}

func TestResolveSessionLog_NoArgs(t *testing.T) {
	// With no args and no session logs in default locations, should error.
	path, cleanup, err := resolveSessionLog(nil)
	if cleanup != nil {
		defer cleanup()
	}
	// This may or may not error depending on whether session logs exist.
	// We just verify it doesn't panic.
	_ = path
	_ = err
}

func TestResolveSessionLog_ExplicitPath(t *testing.T) {
	f := writeTempJSONL(t, []string{`{"type":"user"}`})
	defer os.Remove(f)

	path, cleanup, err := resolveSessionLog([]string{f})
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil {
		t.Fatalf("resolveSessionLog: %v", err)
	}
	if path != f {
		t.Errorf("expected %s, got %s", f, path)
	}
}

func TestSessionAnalyzeResult_JSONOmitsSignalsByDefault(t *testing.T) {
	// Without --token-signals, TokenSignals is nil and must not appear in JSON.
	result := buildAnalyzeResult("test.jsonl", &beadsapi.SessionUsage{InputTokens: 100}, 0)
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(data), "token_signals") {
		t.Errorf("token_signals should be omitted when nil, got: %s", data)
	}
}

func TestSessionAnalyzeResult_JSONIncludesSignalsWhenSet(t *testing.T) {
	result := buildAnalyzeResult("test.jsonl", &beadsapi.SessionUsage{}, 0)
	result.TokenSignals = buildTokenSignals([]turnUsage{
		{Input: 100, Output: 50, CacheRead: 900, CacheCreate: 0},
	}, 0)

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(data), "token_signals") {
		t.Errorf("token_signals should be present when set, got: %s", data)
	}

	// Roundtrip preserves the derived signals.
	var decoded sessionAnalyzeResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if decoded.TokenSignals == nil {
		t.Fatal("token_signals missing after roundtrip")
	}
	if decoded.TokenSignals.TurnCount != 1 {
		t.Errorf("turn_count after roundtrip: expected 1, got %d", decoded.TokenSignals.TurnCount)
	}
	// cache_read=900, input=100, cache_creation=0 => 900/1000 = 0.9.
	if math.Abs(decoded.TokenSignals.CacheHitRatio-0.9) > 0.0001 {
		t.Errorf("cache_hit_ratio after roundtrip: expected 0.9, got %f", decoded.TokenSignals.CacheHitRatio)
	}
}

func TestRunSessionAnalyze_TokenSignalsFlag(t *testing.T) {
	// End-to-end through runSessionAnalyze with --token-signals + --json: the
	// emitted JSON must carry a token_signals block with the expected fields.
	lines := []string{
		`{"type":"assistant","timestamp":"2026-07-03T10:00:00.000Z","message":{"role":"assistant","usage":{"input_tokens":1000,"output_tokens":200,"cache_read_input_tokens":8000,"cache_creation_input_tokens":1000,"cache_creation":{"ephemeral_1h_input_tokens":1000,"ephemeral_5m_input_tokens":0}}}}`,
		`{"type":"result","timestamp":"2026-07-03T10:05:00.000Z","usage":{"input_tokens":0,"output_tokens":100,"cache_read_input_tokens":9000,"cache_creation_input_tokens":0}}`,
	}
	f := writeTempJSONL(t, lines)
	defer os.Remove(f)

	// Flip the package-level flags for the duration of this test.
	prevSignals, prevJSON := sessionTokenSignals, jsonOutput
	sessionTokenSignals, jsonOutput = true, true
	defer func() { sessionTokenSignals, jsonOutput = prevSignals, prevJSON }()

	out := captureStdout(t, func() error {
		return runSessionAnalyze(sessionAnalyzeCmd, []string{f})
	})

	var result sessionAnalyzeResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decoding analyze output %q: %v", out, err)
	}
	if result.TokenSignals == nil {
		t.Fatal("expected token_signals in output")
	}
	if result.TokenSignals.TurnCount != 2 {
		t.Errorf("turn_count: expected 2, got %d", result.TokenSignals.TurnCount)
	}
	if result.TokenSignals.PeakContextTokens != 10000 {
		t.Errorf("peak_context_tokens: expected 10000, got %d", result.TokenSignals.PeakContextTokens)
	}
	if result.TokenSignals.EphemeralCacheCreation.Ephemeral1hInputTokens != 1000 {
		t.Errorf("ephemeral_1h: expected 1000, got %d", result.TokenSignals.EphemeralCacheCreation.Ephemeral1hInputTokens)
	}
}

// TestRunSessionAnalyze_EfficiencyCapFlag exercises the --efficiency-cap flag
// end-to-end: it implies --token-signals (so token_signals is present) AND
// attaches an efficiency_cap verdict. A bloated captured session must cap.
func TestRunSessionAnalyze_EfficiencyCapFlag(t *testing.T) {
	// Flip the package-level flags for the duration of this test. Note we set
	// sessionTokenSignals=false to prove --efficiency-cap alone pulls the signals.
	prevSignals, prevCap, prevJSON := sessionTokenSignals, sessionEfficiencyCap, jsonOutput
	sessionTokenSignals, sessionEfficiencyCap, jsonOutput = false, true, true
	defer func() {
		sessionTokenSignals, sessionEfficiencyCap, jsonOutput = prevSignals, prevCap, prevJSON
	}()

	out := captureStdout(t, func() error {
		return runSessionAnalyze(sessionAnalyzeCmd, []string{
			filepath.Join("testdata", "ced", "token_cost_bloated.jsonl"),
		})
	})

	var result sessionAnalyzeResult
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("decoding analyze output %q: %v", out, err)
	}
	// --efficiency-cap implies --token-signals.
	if result.TokenSignals == nil {
		t.Fatal("expected token_signals in output (--efficiency-cap implies --token-signals)")
	}
	if result.EfficiencyCap == nil {
		t.Fatal("expected efficiency_cap verdict in output")
	}
	if !result.EfficiencyCap.Capped {
		t.Errorf("bloated fixture must cap, got %+v", result.EfficiencyCap)
	}
}

// TestRunSessionAnalyze_NoEfficiencyCapByDefault confirms the measure path stays
// objective: without --efficiency-cap the verdict is omitted entirely.
func TestRunSessionAnalyze_NoEfficiencyCapByDefault(t *testing.T) {
	prevSignals, prevCap, prevJSON := sessionTokenSignals, sessionEfficiencyCap, jsonOutput
	sessionTokenSignals, sessionEfficiencyCap, jsonOutput = true, false, true
	defer func() {
		sessionTokenSignals, sessionEfficiencyCap, jsonOutput = prevSignals, prevCap, prevJSON
	}()

	out := captureStdout(t, func() error {
		return runSessionAnalyze(sessionAnalyzeCmd, []string{
			filepath.Join("testdata", "ced", "token_cost_bloated.jsonl"),
		})
	})

	if strings.Contains(string(out), "efficiency_cap") {
		t.Errorf("efficiency_cap must be omitted without --efficiency-cap, got: %s", out)
	}
}

func writeTempJSONL(t *testing.T, lines []string) string {
	t.Helper()
	f, err := os.CreateTemp("", "session-analyze-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) > 0 {
		if _, err := f.WriteString(strings.Join(lines, "\n")); err != nil {
			t.Fatal(err)
		}
	}
	f.Close()
	return f.Name()
}
