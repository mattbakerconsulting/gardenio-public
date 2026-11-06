package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRenderStopGateBlock_Claude(t *testing.T) {
	stdout, stderr, code := renderStopGateBlock(stopGateFormatClaude, "<system-reminder>STOP</system-reminder>", "decision gate unsatisfied")
	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if stdout != "<system-reminder>STOP</system-reminder>" {
		t.Errorf("stdout = %q", stdout)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(stderr), &got); err != nil {
		t.Fatalf("stderr is not JSON: %q", stderr)
	}
	if got["decision"] != "block" || got["reason"] != "decision gate unsatisfied" {
		t.Errorf("stderr JSON = %v", got)
	}
}

// Codex needs {"decision":"block","reason":...} on stdout and exit 0; stdout
// must be exactly one JSON document, so the checkpoint prose goes in the reason.
func TestRenderStopGateBlock_Codex(t *testing.T) {
	stdout, stderr, code := renderStopGateBlock(stopGateFormatCodex, "<system-reminder>STOP</system-reminder>\n", "decision gate unsatisfied")
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (exit 2 is untested on Codex)", code)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
	var got map[string]string
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("stdout is not JSON: %q", stdout)
	}
	if dec.More() {
		t.Errorf("stdout has trailing content: %q", stdout)
	}
	if got["decision"] != "block" {
		t.Errorf("decision = %q", got["decision"])
	}
	for _, want := range []string{"<system-reminder>STOP</system-reminder>", "decision gate unsatisfied"} {
		if !strings.Contains(got["reason"], want) {
			t.Errorf("reason %q missing %q", got["reason"], want)
		}
	}
	if len(got) != 2 {
		t.Errorf("unexpected keys: %v", got)
	}
}

func TestRenderStopGateBlock_CodexEmptyText(t *testing.T) {
	stdout, _, _ := renderStopGateBlock(stopGateFormatCodex, "", "wrap-up required")
	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatal(err)
	}
	if got["reason"] != "wrap-up required" {
		t.Errorf("reason = %q (no stray leading newline expected)", got["reason"])
	}
}

func TestResolveStopGateFormat(t *testing.T) {
	t.Setenv(envGardenioHarness, "")
	for flag, want := range map[string]string{"": "claude", "claude": "claude", "codex": "codex", " CODEX ": "codex"} {
		got, err := resolveStopGateFormat(flag)
		if err != nil || got != want {
			t.Errorf("resolveStopGateFormat(%q) = %q, %v; want %q", flag, got, err, want)
		}
	}
	if _, err := resolveStopGateFormat("xml"); err == nil {
		t.Error("expected error for unknown format")
	}
	t.Setenv(envGardenioHarness, "codex")
	if got, _ := resolveStopGateFormat(""); got != "codex" {
		t.Errorf("default under GARDENIO_HARNESS=codex = %q", got)
	}
}

func TestStopGateOutput_CodexBuffersProse(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // no stop-gate-text.md: fallback text
	out := &stopGateOutput{format: stopGateFormatCodex}
	out.print("<system-reminder>a</system-reminder>\n")
	injectStopGateTextTo(out.print)
	if !strings.Contains(out.pending.String(), "STOP BLOCKED") {
		t.Errorf("codex output did not buffer the checkpoint text: %q", out.pending.String())
	}
}

// Gemini's AfterAgent hook continues a turn on the same JSON-only stdout form as
// Codex ({"decision":"block","reason":...}, exit 0). {"continue":true} does not
// (ADR 0005 §11.3), so the format must never degrade to Claude's exit 2.
func TestRenderStopGateBlock_Gemini(t *testing.T) {
	stdout, stderr, code := renderStopGateBlock(stopGateFormatGemini, "<system-reminder>STOP</system-reminder>\n", "decision gate unsatisfied")
	if code != 0 || stderr != "" {
		t.Errorf("exit = %d stderr = %q, want 0 and empty", code, stderr)
	}
	var got map[string]string
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not a single JSON document: %q", stdout)
	}
	if got["decision"] != "block" || len(got) != 2 {
		t.Errorf("stdout JSON = %v", got)
	}
	for _, want := range []string{"<system-reminder>STOP</system-reminder>", "decision gate unsatisfied"} {
		if !strings.Contains(got["reason"], want) {
			t.Errorf("reason %q missing %q", got["reason"], want)
		}
	}
}

func TestResolveStopGateFormat_Gemini(t *testing.T) {
	t.Setenv(envGardenioHarness, "")
	if got, err := resolveStopGateFormat(" Gemini "); err != nil || got != "gemini" {
		t.Errorf("resolveStopGateFormat(gemini) = %q, %v", got, err)
	}
	t.Setenv(envGardenioHarness, "gemini")
	if got, _ := resolveStopGateFormat(""); got != "gemini" {
		t.Errorf("default under GARDENIO_HARNESS=gemini = %q", got)
	}
}

func TestStopGateOutput_GeminiBuffersProse(t *testing.T) {
	out := &stopGateOutput{format: stopGateFormatGemini}
	out.print("prose")
	if out.pending.String() != "prose" {
		t.Errorf("pending = %q, want the prose buffered so stdout stays one JSON document", out.pending.String())
	}
}
