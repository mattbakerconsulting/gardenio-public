package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestDarkBeadOperator_Precedence proves the operator identity is picked in the
// most-human-readable order: display > email > raw id > field.
func TestDarkBeadOperator_Precedence(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want string
	}{
		{"display wins", &beadsapi.BeadDetail{OperatedByDisplay: "Matt Baker", OperatedByEmail: "m@x", OperatedBy: "slack:U1"}, "Matt Baker"},
		{"email next", &beadsapi.BeadDetail{OperatedByEmail: "m@x", OperatedBy: "slack:U1"}, "m@x"},
		{"raw id next", &beadsapi.BeadDetail{OperatedBy: "slack:U1"}, "slack:U1"},
		{"field fallback", &beadsapi.BeadDetail{Fields: map[string]string{"operated_by": "slack:U9"}}, "slack:U9"},
		{"none", &beadsapi.BeadDetail{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := darkBeadOperator(tc.bead); got != tc.want {
				t.Errorf("darkBeadOperator = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDarkMRIIDFromBead proves the dark MR IID is extracted from mr_url and
// anchors on the integer IID, never a SHA-shaped ref.
func TestDarkMRIIDFromBead(t *testing.T) {
	cases := []struct {
		name string
		url  string
		want string
	}{
		{"gitlab url", "https://gitlab.example.com/org/repo/-/merge_requests/42", "42"},
		{"empty", "", ""},
		{"garbage", "not a url", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bead := &beadsapi.BeadDetail{Fields: map[string]string{"mr_url": tc.url}}
			if got := darkMRIIDFromBead(bead); got != tc.want {
				t.Errorf("darkMRIIDFromBead(%q) = %q, want %q", tc.url, got, tc.want)
			}
		})
	}
}

// TestRunMRPeel_OfflineTextOutput drives the full command with daemon unset (so
// the dark-bead fetch degrades) and only flag-supplied facts, asserting the
// text output carries the suggested branch, the pre-filled body, and the
// human-guided next steps — and that nothing was pushed (there is no code path
// that could).
func TestRunMRPeel_OfflineTextOutput(t *testing.T) {
	resetPeelFlags(t)
	mrPeelConcern = "scaffolder"
	mrPeelOperator = "Matt Baker"
	mrPeelDarkMR = "13"

	cmd := mrPeelCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	t.Cleanup(func() { cmd.SetOut(nil); cmd.SetErr(nil) })

	if err := runMRPeel(cmd, []string{"gd-DARK42"}); err != nil {
		t.Fatalf("runMRPeel error: %v", err)
	}
	s := out.String()
	for _, want := range []string{
		"suggested branch : peel/scaffolder-dark42",
		"<!-- DOSSIER:BEGIN -->",
		"Dark-Bead: gd-DARK42",
		"Operator: Matt Baker",
		"Human-guided next steps",
		"peel-mr-cmd.sh",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("output missing %q\n---\n%s", want, s)
		}
	}
}

// TestRunMRPeel_OutputFile proves --output writes the body to a file and the
// stdout points at it instead of dumping the body.
func TestRunMRPeel_OutputFile(t *testing.T) {
	resetPeelFlags(t)
	mrPeelConcern = "viewer"
	dir := t.TempDir()
	path := filepath.Join(dir, "peel-body.md")
	mrPeelOutput = path

	cmd := mrPeelCmd
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	t.Cleanup(func() { cmd.SetOut(nil); cmd.SetErr(nil) })

	if err := runMRPeel(cmd, []string{"gd-Z"}); err != nil {
		t.Fatalf("runMRPeel error: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading output file: %v", err)
	}
	if !strings.Contains(string(body), "<!-- DOSSIER:BEGIN -->") {
		t.Error("output file missing dossier body")
	}
	if !strings.Contains(out.String(), "MR body written to "+path) {
		t.Errorf("stdout should point at the output file, got:\n%s", out.String())
	}
}

// TestRunMRPeel_RequiresConcern proves the required-concern guard.
func TestRunMRPeel_RequiresConcern(t *testing.T) {
	resetPeelFlags(t)
	if err := runMRPeel(mrPeelCmd, []string{"gd-X"}); err == nil {
		t.Fatal("expected error when --concern is empty")
	}
}

// resetPeelFlags zeroes the package-level peel flag vars and restores them after
// the test, so the flag-driven command tests do not bleed into each other.
func resetPeelFlags(t *testing.T) {
	t.Helper()
	old := []string{mrPeelConcern, mrPeelOperator, mrPeelBead, mrPeelSlug, mrPeelDarkMR, mrPeelTitle, mrPeelOutput}
	oldJSON := jsonOutput
	mrPeelConcern, mrPeelOperator, mrPeelBead, mrPeelSlug, mrPeelDarkMR, mrPeelTitle, mrPeelOutput = "", "", "", "", "", "", ""
	jsonOutput = false
	t.Cleanup(func() {
		mrPeelConcern, mrPeelOperator, mrPeelBead, mrPeelSlug, mrPeelDarkMR, mrPeelTitle, mrPeelOutput =
			old[0], old[1], old[2], old[3], old[4], old[5], old[6]
		jsonOutput = oldJSON
	})
}
