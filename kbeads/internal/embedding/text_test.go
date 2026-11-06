package embedding

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestBeadText_AllFields(t *testing.T) {
	b := &model.Bead{
		Title:       "Fix auth bug",
		Type:        model.TypeBug,
		Description: "Login flow breaks on refresh.",
		Notes:       "Investigate middleware.",
		Labels:      []string{"project:gardenio", "area:auth"},
	}
	got := BeadText(b)
	want := "Title: Fix auth bug\n" +
		"Type: bug\n" +
		"Description: Login flow breaks on refresh.\n" +
		"Notes: Investigate middleware.\n" +
		"Labels: project:gardenio, area:auth"
	if got != want {
		t.Errorf("BeadText mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestBeadText_OmitsEmptyOptionals(t *testing.T) {
	b := &model.Bead{Title: "t", Type: model.TypeTask}
	got := BeadText(b)
	want := "Title: t\nType: task"
	if got != want {
		t.Errorf("BeadText mismatch\n got: %q\nwant: %q", got, want)
	}
	for _, s := range []string{"Description:", "Notes:", "Labels:"} {
		if strings.Contains(got, s) {
			t.Errorf("BeadText should omit %q for empty field, got: %q", s, got)
		}
	}
}

func TestBeadText_NilBead(t *testing.T) {
	if got := BeadText(nil); got != "" {
		t.Errorf("BeadText(nil) = %q, want empty", got)
	}
}

func TestBeadText_PartialOptionals(t *testing.T) {
	// Description only — verify ordering: Description appears before Notes/Labels slots.
	b := &model.Bead{
		Title:       "t",
		Type:        model.TypeTask,
		Description: "desc",
	}
	got := BeadText(b)
	want := "Title: t\nType: task\nDescription: desc"
	if got != want {
		t.Errorf("BeadText mismatch\n got: %q\nwant: %q", got, want)
	}

	// Labels only — the "Labels:" prefix should still be present once.
	b = &model.Bead{
		Title:  "t",
		Type:   model.TypeTask,
		Labels: []string{"a"},
	}
	got = BeadText(b)
	want = "Title: t\nType: task\nLabels: a"
	if got != want {
		t.Errorf("BeadText mismatch\n got: %q\nwant: %q", got, want)
	}
}

func TestBeadText_LabelOrderPreserved(t *testing.T) {
	// Hash stability depends on deterministic label ordering — the helper must
	// not reorder labels silently.
	b := &model.Bead{
		Title:  "t",
		Type:   model.TypeTask,
		Labels: []string{"z", "a", "m"},
	}
	got := BeadText(b)
	if !strings.HasSuffix(got, "Labels: z, a, m") {
		t.Errorf("BeadText should preserve label order, got: %q", got)
	}
}

func TestTextHash_Deterministic(t *testing.T) {
	a := TextHash("hello world")
	b := TextHash("hello world")
	if a != b {
		t.Errorf("TextHash not deterministic: %q vs %q", a, b)
	}
	if len(a) != 64 {
		t.Errorf("TextHash length = %d, want 64 (hex sha-256)", len(a))
	}
	if TextHash("x") == TextHash("y") {
		t.Error("TextHash collision on distinct inputs")
	}
}

func TestTextHash_Empty(t *testing.T) {
	// The SHA-256 of the empty string is a known constant.
	const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got := TextHash(""); got != emptySHA256 {
		t.Errorf("TextHash(\"\") = %q, want %q", got, emptySHA256)
	}
}

func TestTextHash_StableAcrossIdenticalBeadText(t *testing.T) {
	// Two equivalent beads should produce the same BeadText -> same hash.
	// This is the invariant we rely on to skip re-embedding.
	b1 := &model.Bead{Title: "t", Type: model.TypeTask, Description: "d"}
	b2 := &model.Bead{Title: "t", Type: model.TypeTask, Description: "d"}
	if TextHash(BeadText(b1)) != TextHash(BeadText(b2)) {
		t.Error("identical beads produced different text hashes")
	}

	// Changing Description should change the hash.
	b2.Description = "different"
	if TextHash(BeadText(b1)) == TextHash(BeadText(b2)) {
		t.Error("distinct beads produced same text hash")
	}
}
