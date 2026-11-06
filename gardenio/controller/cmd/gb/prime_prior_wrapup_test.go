package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestOutputPriorWrapupOnResume_EmptyAgentID_Empty(t *testing.T) {
	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty agentID, got: %q", buf.String())
	}
}

func TestOutputPriorWrapupOnResume_NoArchive_Empty(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when bead carries no prior_wrapup, got: %q", buf.String())
	}
}

func TestOutputPriorWrapupOnResume_NullArchive_Empty(t *testing.T) {
	// The revive path nil-deletes the live wrapup by writing a JSON null; a
	// prior_wrapup literally "null" (or empty) must not render a block.
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{
		"prior_wrapup": "null",
	}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for a null archive, got: %q", buf.String())
	}
}

func TestOutputPriorWrapupOnResume_MalformedArchive_Empty(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{
		"prior_wrapup": "not json",
	}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for a malformed archive, got: %q", buf.String())
	}
}

func TestOutputPriorWrapupOnResume_NoUsableFields_Empty(t *testing.T) {
	// A well-formed archive that carries none of tldr/accomplishments/handoff
	// (e.g. only blockers) has nothing to continue from — render nothing.
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{
		"prior_wrapup": wrapJSON(t, map[string]any{"blockers": "waiting on review"}),
	}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output when archive has no usable summary, got: %q", buf.String())
	}
}

func TestOutputPriorWrapupOnResume_RendersArchivedTurn(t *testing.T) {
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{
		"prior_wrapup": wrapJSON(t, map[string]any{
			"tldr":            "Landed the schema fix; MR !7468 merged.",
			"accomplishments": "Registered prior_wrapup on type:agent.\nAll CI statuses green.",
			"handoff_notes":   "Deploy lag: prod still 400s until the next kbeads roll.",
		}),
	}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "gd-agent")
	out := buf.String()

	checks := []string{
		"## Your prior turn",
		"resumed in place",
		"**Prior tl;dr:**",
		"> Landed the schema fix; MR !7468 merged.",
		"**What you accomplished last turn:**",
		"> Registered prior_wrapup on type:agent.",
		"> All CI statuses green.",
		"**Your prior handoff notes:**",
		"> Deploy lag: prod still 400s until the next kbeads roll.",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestOutputPriorWrapupOnResume_TLDROnly(t *testing.T) {
	// A minimal archive with only a tldr still renders — and must not print the
	// accomplishments/handoff sub-headers with empty bodies.
	agent := &mockBead{ID: "gd-agent", Type: "agent", Fields: map[string]string{
		"prior_wrapup": wrapJSON(t, map[string]any{"tldr": "Half done."}),
	}}
	cleanup := setupPriorWorkTestDaemon(t, map[string]*mockBead{"gd-agent": agent}, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputPriorWrapupOnResume(&buf, "gd-agent")
	out := buf.String()

	if !strings.Contains(out, "> Half done.") {
		t.Errorf("output missing the tldr line\n---\n%s", out)
	}
	if strings.Contains(out, "**What you accomplished last turn:**") {
		t.Errorf("accomplishments header should be absent when empty\n---\n%s", out)
	}
	if strings.Contains(out, "**Your prior handoff notes:**") {
		t.Errorf("handoff header should be absent when empty\n---\n%s", out)
	}
}

func TestParsePriorWrapup(t *testing.T) {
	cases := []struct {
		name                        string
		raw                         string
		wantTLDR, wantAcc, wantHand string
	}{
		{"empty", "", "", "", ""},
		{"whitespace", "   ", "", "", ""},
		{"null", "null", "", "", ""},
		{"malformed", "{not json", "", "", ""},
		{"tldr only", `{"tldr":"x"}`, "x", "", ""},
		{"trims", `{"tldr":"  y  ","accomplishments":"\ta\n"}`, "y", "a", ""},
		{"all three", `{"tldr":"t","accomplishments":"a","handoff_notes":"h"}`, "t", "a", "h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tldr, acc, hand := parsePriorWrapup(tc.raw)
			if tldr != tc.wantTLDR || acc != tc.wantAcc || hand != tc.wantHand {
				t.Errorf("parsePriorWrapup(%q) = (%q,%q,%q), want (%q,%q,%q)",
					tc.raw, tldr, acc, hand, tc.wantTLDR, tc.wantAcc, tc.wantHand)
			}
		})
	}
}
