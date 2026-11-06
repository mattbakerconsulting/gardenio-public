package main

import (
	"strings"
	"testing"

	"gardenio/controller/internal/bridge"
)

func up(fullPath string) *bridge.GitLabUpload {
	return &bridge.GitLabUpload{FullPath: fullPath, URL: fullPath}
}

// TestSpliceEvidenceBlock_IsIdempotent is the one that matters: a naive append
// would stack a fresh gallery on every re-capture, and agents retry proofs.
func TestSpliceEvidenceBlock_IsIdempotent(t *testing.T) {
	desc := "Fixes the 403 on save.\n\nSee the linked bead."
	first := spliceEvidenceBlock(desc, buildEvidenceBlock("Evidence", []string{"a"}, []*bridge.GitLabUpload{up("/g/p/uploads/h1/a.png")}, false))
	second := spliceEvidenceBlock(first, buildEvidenceBlock("Evidence", []string{"b"}, []*bridge.GitLabUpload{up("/g/p/uploads/h2/b.png")}, false))

	if n := strings.Count(second, mrEvidenceMarkerStart); n != 1 {
		t.Errorf("want exactly 1 evidence block after a re-embed, got %d:\n%s", n, second)
	}
	if strings.Contains(second, "h1/a.png") {
		t.Errorf("the stale gallery should be REPLACED, not kept:\n%s", second)
	}
	if !strings.Contains(second, "h2/b.png") {
		t.Errorf("the new gallery should be present:\n%s", second)
	}
	if !strings.Contains(second, "Fixes the 403 on save.") {
		t.Errorf("the author's own prose must survive the splice:\n%s", second)
	}
}

func TestSpliceEvidenceBlock_AppendsWhenAbsent(t *testing.T) {
	got := spliceEvidenceBlock("Some description.", "BLOCK")
	if !strings.HasPrefix(got, "Some description.") {
		t.Errorf("original description should lead: %q", got)
	}
	if !strings.HasSuffix(got, "BLOCK") {
		t.Errorf("block should be appended: %q", got)
	}
}

func TestSpliceEvidenceBlock_EmptyDescription(t *testing.T) {
	if got := spliceEvidenceBlock("", "BLOCK"); got != "BLOCK" {
		t.Errorf("want just the block for an empty description, got %q", got)
	}
	if got := spliceEvidenceBlock("   \n\n ", "BLOCK"); got != "BLOCK" {
		t.Errorf("want just the block for a whitespace-only description, got %q", got)
	}
}

// TestSpliceEvidenceBlock_PreservesTrailingContent: a PROOF block or Operator
// trailer written AFTER the evidence must not be eaten by the swap.
func TestSpliceEvidenceBlock_PreservesTrailingContent(t *testing.T) {
	desc := "Intro.\n\n" + mrEvidenceMarkerStart + "\nold\n" + mrEvidenceMarkerEnd + "\n\n<!-- PROOF:BEGIN -->\n## Proof\n<!-- PROOF:END -->"
	got := spliceEvidenceBlock(desc, mrEvidenceMarkerStart+"\nnew\n"+mrEvidenceMarkerEnd)

	if !strings.Contains(got, "<!-- PROOF:BEGIN -->") {
		t.Errorf("the PROOF block after the evidence must survive — losing it reds lint-mr-proof:\n%s", got)
	}
	if !strings.Contains(got, "Intro.") {
		t.Errorf("leading prose must survive:\n%s", got)
	}
	if strings.Contains(got, "old") {
		t.Errorf("stale evidence should be gone:\n%s", got)
	}
}

// TestSpliceEvidenceBlock_MalformedMarkersAppend: an end marker with no start
// (or reversed order) must not corrupt the description by slicing on it.
func TestSpliceEvidenceBlock_MalformedMarkersAppend(t *testing.T) {
	for _, desc := range []string{
		"text " + mrEvidenceMarkerEnd + " more",             // end with no start
		mrEvidenceMarkerEnd + " x " + mrEvidenceMarkerStart, // reversed
	} {
		got := spliceEvidenceBlock(desc, "BLOCK")
		if !strings.Contains(got, "BLOCK") {
			t.Errorf("want the block added for malformed markers %q, got %q", desc, got)
		}
		if !strings.Contains(got, "x") && strings.Contains(desc, "x") {
			t.Errorf("must not drop existing content on malformed markers: %q -> %q", desc, got)
		}
	}
}

// --- Rendering. --------------------------------------------------------------

func TestBuildEvidenceBlock_PairRendersTable(t *testing.T) {
	got := buildEvidenceBlock("Fix proof",
		[]string{"before", "after"},
		[]*bridge.GitLabUpload{up("/g/p/uploads/h/before.png"), up("/g/p/uploads/h/after.png")},
		true)

	if !strings.Contains(got, "| Before | After |") {
		t.Errorf("want a two-column table header:\n%s", got)
	}
	if !strings.Contains(got, "| ![before](/g/p/uploads/h/before.png) | ![after](/g/p/uploads/h/after.png) |") {
		t.Errorf("want both images on one table row:\n%s", got)
	}
	if !strings.Contains(got, "## Fix proof") {
		t.Errorf("want the custom heading:\n%s", got)
	}
	// A raw newline inside a cell breaks GitLab's table parser.
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "|") && strings.Count(line, "|") < 3 {
			t.Errorf("malformed table row %q in:\n%s", line, got)
		}
	}
}

func TestBuildEvidenceBlock_UnpairedStacksImages(t *testing.T) {
	got := buildEvidenceBlock("Evidence",
		[]string{"one", "two"},
		[]*bridge.GitLabUpload{up("/g/p/uploads/h/1.png"), up("/g/p/uploads/h/2.png")},
		false)

	if strings.Contains(got, "| Before | After |") {
		t.Errorf("unpaired mode should not render a table:\n%s", got)
	}
	for _, want := range []string{"![one](/g/p/uploads/h/1.png)", "![two](/g/p/uploads/h/2.png)"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s:\n%s", want, got)
		}
	}
}

func TestBuildEvidenceBlock_AlwaysCarriesBothMarkers(t *testing.T) {
	got := buildEvidenceBlock("Evidence", nil, nil, false)
	if !strings.Contains(got, mrEvidenceMarkerStart) || !strings.Contains(got, mrEvidenceMarkerEnd) {
		t.Errorf("both markers are required or the next splice cannot find the block:\n%s", got)
	}
}

func TestAltsFor_StripsPathAndExtension(t *testing.T) {
	got := altsFor([]string{"/tmp/x/before-403.png", "after.jpeg", "noext"})
	want := []string{"before-403", "after", "noext"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("altsFor[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestAltAt_OutOfRangeIsEmpty(t *testing.T) {
	if got := altAt([]string{"a"}, 5); got != "" {
		t.Errorf("want empty alt out of range, got %q", got)
	}
}
