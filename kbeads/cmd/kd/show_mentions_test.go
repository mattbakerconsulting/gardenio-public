package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestPrintMentionSections_GroupsByVerb verifies the Mentions / Mentioned by
// summary in `kd show` groups references by relationship_type, dedupes IDs
// within a group (a target referenced via two source_fields collapses to
// one entry), sorts typed verbs alphabetically with "untyped" last, and
// renders the canonical layout from gardenio/plans/typed-mentions-graph-edges-and-cli.md.
func TestPrintMentionSections_GroupsByVerb(t *testing.T) {
	mc := newMockClient()
	beadID := "gd-Focus0001"

	// Forward (Mentions): two typed verbs and a few untyped, with a duplicate
	// target across description+notes that should collapse to one ID under
	// the same verb bucket.
	parent := "parent"
	blocks := "blocks"
	mc.Mentions[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-AAA", Type: "task", Status: "open", SourceField: "description", RelationshipType: &blocks},
		{ID: "gd-BBB", Type: "task", Status: "open", SourceField: "description", RelationshipType: &blocks},
		{ID: "gd-CCC", Type: "epic", Status: "open", SourceField: "description", RelationshipType: &parent},
		{ID: "gd-DDD", Type: "task", Status: "open", SourceField: "description", RelationshipType: nil},
		{ID: "gd-DDD", Type: "task", Status: "open", SourceField: "notes", RelationshipType: nil}, // dup target
		{ID: "gd-EEE", Type: "task", Status: "open", SourceField: "description", RelationshipType: nil},
	}
	// Reverse (Mentioned by): one typed, two untyped.
	blockedBy := "blocked-by"
	mc.MentionedBy[beadID] = []*model.BeadReferenceSummary{
		{ID: "gd-XXX", Type: "task", Status: "open", SourceField: "description", RelationshipType: &blockedBy},
		{ID: "gd-YYY", Type: "task", Status: "open", SourceField: "description", RelationshipType: nil},
		{ID: "gd-ZZZ", Type: "task", Status: "open", SourceField: "description", RelationshipType: nil},
	}

	out := &strings.Builder{}
	printMentionSections(t.Context(), mc, beadID, out, idLinker{})

	got := out.String()

	// Section headers.
	for _, want := range []string{"Mentions:", "Mentioned by:"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in output:\n%s", want, got)
		}
	}

	// Forward groups: blocks (2), parent (1), untyped (2 after dedup).
	for _, want := range []string{
		"  blocks: 2 (gd-AAA, gd-BBB)",
		"  parent: 1 (gd-CCC)",
		"  untyped: 2 (gd-DDD, gd-EEE)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing forward group line %q in output:\n%s", want, got)
		}
	}

	// Reverse groups.
	for _, want := range []string{
		"  blocked-by: 1 (gd-XXX)",
		"  untyped: 2 (gd-YYY, gd-ZZZ)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing reverse group line %q in output:\n%s", want, got)
		}
	}

	// Ordering: typed verbs sort alphabetically before untyped within each
	// section. Verify by index in the rendered string.
	mentionsIdx := strings.Index(got, "Mentions:")
	blocksIdx := strings.Index(got[mentionsIdx:], "blocks:")
	parentIdx := strings.Index(got[mentionsIdx:], "parent:")
	untypedIdx := strings.Index(got[mentionsIdx:], "untyped:")
	if !(blocksIdx < parentIdx && parentIdx < untypedIdx) {
		t.Errorf("verb ordering wrong: blocks=%d parent=%d untyped=%d (alphabetical, untyped last expected)",
			blocksIdx, parentIdx, untypedIdx)
	}
}

// TestPrintMentionSections_Empty verifies that beads with no mentions
// produce no output (no leading blank lines, no section headers). This is
// the dominant case for fresh beads and we don't want to clutter `kd show`.
func TestPrintMentionSections_Empty(t *testing.T) {
	mc := newMockClient()
	out := &strings.Builder{}
	printMentionSections(t.Context(), mc, "gd-Empty0001", out, idLinker{})
	if got := out.String(); got != "" {
		t.Errorf("expected empty output for bead with no mentions, got: %q", got)
	}
}

// TestPrintMentionSections_TruncatesLongLists locks in that groups with
// more than mentionExemplarsPerGroup IDs render only the first N exemplars
// followed by "..." while still showing the full count.
func TestPrintMentionSections_TruncatesLongLists(t *testing.T) {
	mc := newMockClient()
	beadID := "gd-Wide00001"

	related := "related"
	refs := make([]*model.BeadReferenceSummary, 0, mentionExemplarsPerGroup+3)
	for i := range mentionExemplarsPerGroup + 3 {
		// Synthetic but valid-looking gd-IDs (10 chars after prefix).
		refs = append(refs, &model.BeadReferenceSummary{
			ID:               kdID(i),
			Type:             "task",
			Status:           "open",
			SourceField:      "description",
			RelationshipType: &related,
		})
	}
	mc.Mentions[beadID] = refs

	out := &strings.Builder{}
	printMentionSections(t.Context(), mc, beadID, out, idLinker{})
	got := out.String()

	if !strings.Contains(got, "related: 8 (") {
		t.Errorf("expected count to reflect full bucket size (8), got:\n%s", got)
	}
	if !strings.Contains(got, ", ...)") {
		t.Errorf("expected truncation marker '...' for oversized group, got:\n%s", got)
	}
}

// kdID emits a deterministic 10-char suffix matching the ka-0000001713
// shape, varying only the first byte so each ID is distinct. Test-only —
// real ID generation lives in internal/idgen.
func kdID(i int) string {
	suffix := []byte("AbCdEfGhIj")
	suffix[0] = byte('A' + (i % 26))
	return "gd-" + string(suffix)
}
