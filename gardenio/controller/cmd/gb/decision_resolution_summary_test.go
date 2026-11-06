package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// A resolved decision must surface chosen + who + when on ONE scannable line
// so agents never fall back to guessing jq field names on the raw bead.
// The real fields are chosen / responded_by / responded_at — the common
// guesses (resolution/chosen_option/answer/decided_by) do not exist
// (ka-0000000648).

func TestDecisionResolutionLine_UnresolvedIsEmpty(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-D", Fields: map[string]string{
		"prompt":  "deploy or hold?",
		"options": `[{"id":"deploy","short":"Deploy now"}]`,
	}}
	if got := decisionResolutionLine(b); got != "" {
		t.Errorf("unresolved decision should yield empty line, got %q", got)
	}
}

func TestDecisionResolutionLine_ChosenWithLabelByAndWhen(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-D", Fields: map[string]string{
		"options":      `[{"id":"deploy","short":"Deploy now","label":"Ship it"},{"id":"hold","short":"Hold"}]`,
		"chosen":       "deploy",
		"responded_by": "slack:U0OPERATOR1",
		"responded_at": "2026-07-27T14:00:00Z",
	}}
	got := decisionResolutionLine(b)
	for _, want := range []string{"deploy", "Deploy now", "slack:U0OPERATOR1", "2026-07-27T14:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("resolution line %q missing %q", got, want)
		}
	}
}

func TestDecisionResolutionLine_ChosenNoOptionsLabel(t *testing.T) {
	// chosen has no matching option entry — fall back to the bare id, no crash.
	b := &beadsapi.BeadDetail{ID: "gd-D", Fields: map[string]string{
		"chosen":       "deploy",
		"responded_by": "alice",
	}}
	got := decisionResolutionLine(b)
	if !strings.Contains(got, "deploy") || !strings.Contains(got, "by alice") {
		t.Errorf("line = %q, want chosen id + responder", got)
	}
	// No parenthetical label when there is no options JSON to resolve one.
	if strings.Contains(got, "(deploy)") {
		t.Errorf("line = %q should not duplicate the id as a label", got)
	}
}

func TestDecisionResolutionLine_FreeTextOnly(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-D", Fields: map[string]string{
		"response_text": "just go with plan C",
		"responded_by":  "bob",
		"responded_at":  "2026-07-27T15:00:00Z",
	}}
	got := decisionResolutionLine(b)
	for _, want := range []string{"just go with plan C", "bob", "2026-07-27T15:00:00Z"} {
		if !strings.Contains(got, want) {
			t.Errorf("free-text line %q missing %q", got, want)
		}
	}
}

func TestDecisionResolutionLine_ChosenPlusFreeText(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-D", Fields: map[string]string{
		"options":       `[{"id":"deploy","short":"Deploy now"}]`,
		"chosen":        "deploy",
		"response_text": "but wait for the on-call to ack",
		"responded_by":  "carol",
	}}
	got := decisionResolutionLine(b)
	for _, want := range []string{"deploy", "Deploy now", "but wait for the on-call to ack", "carol"} {
		if !strings.Contains(got, want) {
			t.Errorf("line %q missing %q", got, want)
		}
	}
}

func TestDecisionResolutionLine_ByOnlyNoWhen(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-D", Fields: map[string]string{
		"chosen":       "hold",
		"responded_by": "dave",
	}}
	got := decisionResolutionLine(b)
	if !strings.Contains(got, "by dave") {
		t.Errorf("line %q should carry the responder", got)
	}
	if strings.Contains(got, " at ") {
		t.Errorf("line %q should not mention a time when responded_at is absent", got)
	}
}

func TestOptionLabelForID(t *testing.T) {
	opts := `[{"id":"a","short":"Alpha","label":"Do alpha"},{"id":"b","label":"Only label"},{"id":"c"}]`
	cases := []struct {
		id, want string
	}{
		{"a", "Alpha"},      // short wins over label
		{"b", "Only label"}, // label used when short absent
		{"c", ""},           // neither -> empty
		{"z", ""},           // no match -> empty
	}
	for _, tc := range cases {
		if got := optionLabelForID(opts, tc.id); got != tc.want {
			t.Errorf("optionLabelForID(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
	// Malformed / empty inputs never panic and yield "".
	if got := optionLabelForID("not json", "a"); got != "" {
		t.Errorf("malformed options should yield empty, got %q", got)
	}
	if got := optionLabelForID("", "a"); got != "" {
		t.Errorf("empty options should yield empty, got %q", got)
	}
}

// printDecisionDetail must lead with the one-line "Resolved:" summary AND still
// print the "At:" (responded_at) field — the missing "when" that pushed agents
// into jq field-guessing (ka-0000000648).
func TestPrintDecisionDetail_ResolvedSummaryAndAtLine(t *testing.T) {
	out := string(captureStdout(t, func() error {
		printDecisionDetail(&beadsapi.BeadDetail{
			ID:     "gd-DD",
			Status: "closed",
			Fields: map[string]string{
				"prompt":       "deploy or hold?",
				"options":      `[{"id":"deploy","short":"Deploy now"}]`,
				"chosen":       "deploy",
				"responded_by": "slack:U0OPERATOR1",
				"responded_at": "2026-07-27T14:00:00Z",
			},
		})
		return nil
	}))
	if !strings.Contains(out, "Resolved: deploy") {
		t.Errorf("output missing one-line Resolved summary:\n%s", out)
	}
	if !strings.Contains(out, "At:       2026-07-27T14:00:00Z") {
		t.Errorf("output missing responded_at (At:) line:\n%s", out)
	}
	if !strings.Contains(out, "By:       slack:U0OPERATOR1") {
		t.Errorf("output missing responder (By:) line:\n%s", out)
	}
}

// An unresolved decision must NOT print a "Resolved:" line.
func TestPrintDecisionDetail_UnresolvedHasNoResolvedLine(t *testing.T) {
	out := string(captureStdout(t, func() error {
		printDecisionDetail(&beadsapi.BeadDetail{
			ID:     "gd-DD",
			Status: "open",
			Fields: map[string]string{
				"prompt":  "deploy or hold?",
				"options": `[{"id":"deploy","short":"Deploy now"}]`,
			},
		})
		return nil
	}))
	if strings.Contains(out, "Resolved:") {
		t.Errorf("unresolved decision should not print a Resolved line:\n%s", out)
	}
}
