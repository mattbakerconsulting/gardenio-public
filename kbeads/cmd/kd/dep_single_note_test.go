package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// countTrailingNotes returns how many stdout lines begin with "NOTE:". The
// non-JSON `dep add` success output promises exactly one such line, printed
// last so `... 2>&1 | tail -N` keeps it (ka-0000007975). Two would mean one
// silently stole the other's last line and voided a `tail` guarantee.
func countTrailingNotes(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "NOTE:") {
			n++
		}
	}
	return n
}

// TestTrailingDepNote_AtMostOne is the STRUCTURAL lock for ka-0000002186: the
// two "printed last" notes (the parent-child flip note and the wait-edge
// direction echo) used to be mutually exclusive only by prose invariant spread
// across depWasCanonicalized and isWaitEdgeType. trailingDepNote collapses the
// choice into one return value, so at most one note can ever exist regardless of
// dep type or flip state — asserted here directly on the helper across the whole
// cross-product, including the future-hazard combination (flipped AND a
// wait-edge type) the old two-if form could not survive.
func TestTrailingDepNote_AtMostOne(t *testing.T) {
	types := []model.DependencyType{
		model.DepBlocks, "blocked-by", "depends-on",
		model.DepParentChild, model.DepRelated, model.DepDuplicates,
		model.DepSupersedes, model.DepTriggeredBy, model.DepChainsTo, model.DepRates, "",
	}
	for _, typ := range types {
		for _, flipped := range []bool{false, true} {
			dep := &model.Dependency{BeadID: "gd-child", DependsOnID: "gd-parent", Type: typ}
			note := trailingDepNote(dep, flipped)
			// A single string is one-or-zero notes by construction; guard that
			// it never smuggles a second note in as an embedded newline.
			if strings.Contains(note, "\n") {
				t.Errorf("trailingDepNote(type=%q, flipped=%v) must be a single line; got:\n%q", typ, flipped, note)
			}
			if got := countTrailingNotes(note); got > 1 {
				t.Errorf("trailingDepNote(type=%q, flipped=%v) emitted %d NOTE lines, want at most 1; got:\n%q",
					typ, flipped, got, note)
			}
		}
	}
}

// TestTrailingDepNote_FlipWinsOverEcho pins the priority made explicit by the
// switch ordering: if a future change ever makes a canonicalized edge ALSO a
// wait-edge type, exactly the flip note prints (the surprise the caller did not
// ask for), never both and never the echo alone.
func TestTrailingDepNote_FlipWinsOverEcho(t *testing.T) {
	// blocks is a wait-edge type; flipped=true simulates the hazard where a
	// non-parent-child edge is also reported canonicalized.
	dep := &model.Dependency{BeadID: "gd-child", DependsOnID: "gd-parent", Type: model.DepBlocks}
	note := trailingDepNote(dep, true)
	if !strings.HasPrefix(note, "NOTE: orientation auto-canonicalized") {
		t.Errorf("flip note must win when both would apply; got:\n%q", note)
	}
	if strings.Contains(note, "BLOCKED BY") {
		t.Errorf("only one note may print — the echo must not also appear; got:\n%q", note)
	}
}

// TestDepAdd_EmitsAtMostOneTrailingNote is the end-to-end lock over the real
// command output: for every dep type, a successful non-JSON add prints at most
// one NOTE line, and when present it is the LAST non-empty line. This is the
// guarantee the two per-note last-line tests each assert only within a scenario
// where the other note cannot fire.
func TestDepAdd_EmitsAtMostOneTrailingNote(t *testing.T) {
	for _, typ := range []string{
		"blocks", "blocked-by", "depends-on",
		"parent-child", "related", "duplicates", "supersedes", "triggered-by",
	} {
		t.Run(typ, func(t *testing.T) {
			mc := newMockClient() // default AddDependency echoes the request unchanged
			withMockClient(t, mc)
			setDepType(t, typ)

			out := captureStdout(t, func() {
				if err := depAddCmd.RunE(depAddCmd, []string{"gd-dependent", "gd-blocker"}); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if got := countTrailingNotes(out); got > 1 {
				t.Fatalf("--type %s printed %d NOTE lines, want at most 1; got:\n%s", typ, got, out)
			}
			lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
			last := lines[len(lines)-1]
			// Any NOTE that printed must be the last line; if none printed the
			// last line is a normal field and must not start with NOTE:.
			for _, line := range lines[:len(lines)-1] {
				if strings.HasPrefix(line, "NOTE:") {
					t.Errorf("--type %s: a NOTE line appeared before the last line (would be lost to `tail -1`); got:\n%s", typ, out)
				}
			}
			_ = last
		})
	}
}
