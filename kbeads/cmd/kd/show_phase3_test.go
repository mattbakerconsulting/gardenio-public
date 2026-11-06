package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Phase 3 of E3 typed-mentions (gd-m7r3YorWyH):
//   - TTY linkification of gd-IDs in Mentions / Mentioned-by sections.
//   - --mentions-only flag for `kd show` that suppresses header/comments/deps.
//
// These tests pin down the byte-level rendering: non-TTY stays plain (so
// existing pipelines that grep/awk the output keep working), TTY emits OSC 8
// hyperlinks pointing at the configured beads-viewer base URL, and
// mentions-only mode emits *only* the two mention sections.

// TestNewIDLinker_DisabledByDefault locks in the safety default: no TTY or
// no viewer URL → no hyperlinks. We do not want OSC 8 escape sequences to
// leak into pipes, redirected files, or CI logs.
func TestNewIDLinker_DisabledByDefault(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		tty     bool
		want    bool
	}{
		{"non-tty plain url", "https://beads.example.com", false, false},
		{"tty without url", "", true, false},
		{"tty with whitespace url", "   ", true, false},
		{"non-tty no url", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("NO_COLOR", "")
			t.Setenv("CLICOLOR", "")
			t.Setenv("CLICOLOR_FORCE", "")
			l := newIDLinker(tc.baseURL, tc.tty)
			if l.enabled != tc.want {
				t.Fatalf("enabled = %v, want %v", l.enabled, tc.want)
			}
		})
	}
}

// TestIDLinker_WrapPlain locks in that a disabled linker (the default for
// non-TTY callers) returns the bare ID. Golden-file expectations across the
// rest of the suite depend on this.
func TestIDLinker_WrapPlain(t *testing.T) {
	l := idLinker{}
	if got := l.wrap("gd-AAA"); got != "gd-AAA" {
		t.Fatalf("disabled linker should return id verbatim; got %q", got)
	}
}

// TestIDLinker_WrapOSC8 verifies the OSC 8 byte sequence is exactly the
// canonical form so terminal emulators that follow the spec render the link.
// Reference: https://gist.github.com/egmontkob/eb114294efbcd5adb1944c9f3cb5feda
func TestIDLinker_WrapOSC8(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1") // force color/link enable without a real TTY
	l := newIDLinker("https://beads.example.com/", true)
	got := l.wrap("gd-AAA")
	want := "\x1b]8;;https://beads.example.com/gd-AAA\x1b\\gd-AAA\x1b]8;;\x1b\\"
	if got != want {
		t.Fatalf("OSC 8 wrap mismatch:\n got %q\nwant %q", got, want)
	}
}

// TestIDLinker_NoColorSuppressesLinks confirms NO_COLOR=1 disables OSC 8
// links even on a TTY. Linking is part of the same "decoration" budget as
// ANSI color; users who suppress one suppress the other.
func TestIDLinker_NoColorSuppressesLinks(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Setenv("CLICOLOR_FORCE", "")
	t.Setenv("CLICOLOR", "")
	l := newIDLinker("https://beads.example.com", true)
	if l.enabled {
		t.Fatalf("NO_COLOR=1 must disable OSC 8 links")
	}
	if got := l.wrap("gd-AAA"); got != "gd-AAA" {
		t.Fatalf("NO_COLOR linker should return id verbatim; got %q", got)
	}
}

// TestPrintMentionSections_TTYLinkified renders the canonical groups with a
// linker enabled and verifies that *every* gd-ID in the output is wrapped
// in an OSC 8 hyperlink while the surrounding scaffolding (verb labels,
// counts, parens, commas) stays plain. This is the TTY golden behavior
// called out in the acceptance criteria for gd-m7r3YorWyH.
func TestPrintMentionSections_TTYLinkified(t *testing.T) {
	mc := newMockClient()
	beadID := "gd-Focus0002"
	blocks := "blocks"
	mc.Mentions[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-AAA", SourceField: "description", RelationshipType: &blocks},
		{ID: "gd-BBB", SourceField: "description", RelationshipType: nil},
	}

	t.Setenv("NO_COLOR", "")
	t.Setenv("CLICOLOR", "")
	t.Setenv("CLICOLOR_FORCE", "1")
	linker := newIDLinker("https://beads.example.com", true)
	if !linker.enabled {
		t.Fatalf("precondition: linker must be enabled when CLICOLOR_FORCE=1")
	}

	out := &strings.Builder{}
	printMentionSections(t.Context(), mc, beadID, out, linker)
	got := out.String()

	// Each gd-ID must appear inside an OSC 8 sequence pointing at the
	// configured viewer URL.
	for _, id := range []string{"gd-AAA", "gd-BBB"} {
		want := "\x1b]8;;https://beads.example.com/" + id + "\x1b\\" + id + "\x1b]8;;\x1b\\"
		if !strings.Contains(got, want) {
			t.Errorf("missing OSC 8 link for %s in output:\n%q", id, got)
		}
	}
	// Verb labels and structural punctuation must NOT be wrapped — only IDs
	// are clickable. Catch the mistake of accidentally linkifying everything.
	if strings.Contains(got, "\x1b]8;;https://beads.example.com/blocks") {
		t.Errorf("verb label was linkified; only gd-IDs should be wrapped:\n%q", got)
	}
}

// TestPrintMentionSections_NonTTYNoEscapes is the inverse golden: with a
// disabled linker (the default when stdout is not a TTY), no ESC bytes
// appear in the output, period. This is what makes `kd show … | grep gd-`
// work — see the acceptance criteria for gd-m7r3YorWyH.
func TestPrintMentionSections_NonTTYNoEscapes(t *testing.T) {
	mc := newMockClient()
	beadID := "gd-Focus0003"
	related := "related"
	mc.Mentions[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-CCC", SourceField: "description", RelationshipType: &related},
	}
	mc.MentionedBy[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-DDD", SourceField: "description", RelationshipType: nil},
	}

	out := &strings.Builder{}
	printMentionSections(t.Context(), mc, beadID, out, idLinker{})
	got := out.String()

	if strings.ContainsRune(got, '\x1b') {
		t.Errorf("non-TTY output must contain no ESC bytes; got:\n%q", got)
	}
	// Sanity: the structural format from Phase 1 is preserved verbatim.
	for _, want := range []string{
		"Mentions:\n",
		"  related: 1 (gd-CCC)\n",
		"Mentioned by:\n",
		"  untyped: 1 (gd-DDD)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in plain output:\n%q", want, got)
		}
	}
}

// TestShowMentionsOnly_SuppressesEverythingElse covers the --mentions-only
// flag end-to-end: header, deps, and comments are dropped; only the two
// mention sections survive. Acceptance for gd-m7r3YorWyH:
//
//	`kd show gd-AAA --mentions-only` prints only the two mention sections.
//
// We invoke the command's RunE directly so we exercise the same control
// flow real callers hit (flag parsing through cobra would also work but
// adds plumbing; the flag's effect is the only thing under test here).
func TestShowMentionsOnly_SuppressesEverythingElse(t *testing.T) {
	mc := newMockClient()
	beadID := "gd-UES79pg4W5"
	blocks := "blocks"
	mc.Beads[beadID] = &model.Bead{
		ID:          beadID,
		Title:       "Should NOT appear",
		Description: "Description body that must be suppressed",
		Status:      model.StatusOpen,
	}
	mc.Mentions[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-MMM", SourceField: "description", RelationshipType: &blocks},
	}
	mc.MentionedBy[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-NNN", SourceField: "description", RelationshipType: nil},
	}

	// Wire the package-level client and flag, then capture stdout while the
	// RunE executes. Restore both on exit so test ordering doesn't matter.
	prevClient := beadsClient
	prevOnly := showMentionsOnly
	prevTTY := isStdoutTTY
	t.Cleanup(func() {
		beadsClient = prevClient
		showMentionsOnly = prevOnly
		isStdoutTTY = prevTTY
	})
	beadsClient = mc
	showMentionsOnly = true
	isStdoutTTY = func() bool { return false }

	got := captureShowStdout(t, func() {
		if err := showCmd.RunE(showCmd, []string{beadID}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	// What must be present.
	for _, want := range []string{
		"Mentions:",
		"  blocks: 1 (gd-MMM)",
		"Mentioned by:",
		"  untyped: 1 (gd-NNN)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("--mentions-only output missing %q:\n%s", want, got)
		}
	}
	// What must be absent — the header/title block, deps, comments, and
	// the bead description body. Notably we should NOT have called GetBead
	// either; verify that as a stronger contract so flat lookups stay
	// cheap when piped.
	for _, banned := range []string{
		"Should NOT appear",
		"Description body",
		"Depends On:",
		"Comments:",
		"ID:",
		"Title:",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("--mentions-only output unexpectedly contains %q:\n%s", banned, got)
		}
	}
	if len(mc.GetBeadCalls) != 0 {
		t.Errorf("--mentions-only should not call GetBead; got %d calls", len(mc.GetBeadCalls))
	}
}

// captureShowStdout swaps os.Stdout for a pipe, runs fn, and returns
// everything written. Reused only by phase-3 tests; the wider suite uses
// helpers_test.go's captureStdout, but that variant relies on a different
// invocation pattern.
func captureShowStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	prev := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = prev })

	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = buf.ReadFrom(r)
		close(done)
	}()

	fn()
	_ = w.Close()
	<-done
	return buf.String()
}
