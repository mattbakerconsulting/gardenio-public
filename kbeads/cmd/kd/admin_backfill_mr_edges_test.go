package main

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/model"
)

func mrBeadWithFields(id string, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{
		ID:     id,
		Type:   "merge-request",
		Kind:   model.KindIssue,
		Status: model.StatusClosed,
		Fields: raw,
	}
}

func selfLoopDeps(mrID string) []*model.Dependency {
	return []*model.Dependency{
		{BeadID: mrID, DependsOnID: mrID, Type: "produces"},
		{BeadID: mrID, DependsOnID: mrID, Type: "triggered-by"},
	}
}

func resetMREdgesFlags(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"dry-run": "true", "apply": "false", "limit": "0", "clean-self-loops": "true",
	} {
		if err := adminBackfillMREdgesCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("reset %s: %v", k, err)
		}
	}
}

// withMRURL is the canonical MR URL used across these tests.
const testMRURL = "https://gitlab.com/Acme/Platform/gardenio/-/merge_requests/3636"

// resolveVia drives one MR bead through the command in --apply mode and
// returns the mock so the caller can assert on the writes.
func runMREdges(t *testing.T, mc *mockBeadsClient) {
	t.Helper()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMREdgesFlags(t)
	_ = adminBackfillMREdgesCmd.Flags().Set("apply", "true")
	_ = adminBackfillMREdgesCmd.Flags().Set("dry-run", "false")
	captureStdout(t, func() {
		if err := adminBackfillMREdgesCmd.RunE(adminBackfillMREdgesCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
}

// Resolution path A: existing cross triggered-by(MR→task) edge. The command
// must add the missing produces(task→MR) and remove the self-loops, while
// leaving the existing cross triggered-by alone.
func TestMREdges_ResolveViaTriggeredBy(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr1", map[string]any{"mr_url": testMRURL})
	mc.ListBeadsPages = [][]*model.Bead{{mr}, {}}
	mc.Deps = map[string][]*model.Dependency{
		"gd-mr1": append(selfLoopDeps("gd-mr1"),
			&model.Dependency{BeadID: "gd-mr1", DependsOnID: "gd-task1", Type: "triggered-by"}),
		"gd-task1": nil, // task carries no produces edge yet
	}

	runMREdges(t, mc)

	// produces(task→MR) added exactly once.
	var produces int
	for _, d := range mc.AddDepCalls {
		if d.BeadID == "gd-task1" && d.DependsOnID == "gd-mr1" && d.Type == "produces" {
			produces++
		}
	}
	if produces != 1 {
		t.Errorf("produces(task→MR) added %d times, want 1", produces)
	}
	// existing cross triggered-by must NOT be re-added.
	for _, d := range mc.AddDepCalls {
		if d.BeadID == "gd-mr1" && d.DependsOnID == "gd-task1" && d.Type == "triggered-by" {
			t.Errorf("re-added an already-present triggered-by edge")
		}
	}
	// self-loops removed (produces + triggered-by on the MR bead itself).
	var loops int
	for _, r := range mc.RemoveDepCalls {
		if r.BeadID == "gd-mr1" && r.DependsOnID == "gd-mr1" {
			loops++
		}
	}
	if loops != 2 {
		t.Errorf("self-loop removals = %d, want 2", loops)
	}
}

// Resolution path B: no cross edge, but a task bead carries the MR's mr_url.
// The command must resolve via the field filter and add BOTH canonical edges.
func TestMREdges_ResolveViaMRURLCarrier(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr2", map[string]any{"mr_url": testMRURL})
	carrier := taskWithMRFields("gd-task2", map[string]any{"mr_url": testMRURL})
	// page 0: MR list; page 1: field-filter carrier lookup; page 2: terminate.
	mc.ListBeadsPages = [][]*model.Bead{{mr}, {carrier}, {}}
	mc.Deps = map[string][]*model.Dependency{
		"gd-mr2":   selfLoopDeps("gd-mr2"),
		"gd-task2": nil,
	}

	runMREdges(t, mc)

	var produces, trig int
	for _, d := range mc.AddDepCalls {
		switch {
		case d.BeadID == "gd-task2" && d.DependsOnID == "gd-mr2" && d.Type == "produces":
			produces++
		case d.BeadID == "gd-mr2" && d.DependsOnID == "gd-task2" && d.Type == "triggered-by":
			trig++
		}
	}
	if produces != 1 || trig != 1 {
		t.Errorf("edges added: produces=%d triggered-by=%d, want 1/1", produces, trig)
	}
}

// Resolution path C: no cross edge and no carrier, but the source branch
// embeds a case-sensitive gd-id that resolves to an existing bead. The branch
// substring must be used verbatim (NOT lowercased).
func TestMREdges_ResolveViaSourceBranchCasePreserved(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr3", map[string]any{
		"mr_url":        testMRURL,
		"source_branch": "fix/gd-AbCdEf12-something",
	})
	mc.ListBeadsPages = [][]*model.Bead{
		{mr},
		{}, // field-filter carrier lookup: empty
		{}, // terminate MR pagination
	}
	mc.Deps = map[string][]*model.Dependency{"gd-mr3": selfLoopDeps("gd-mr3")}
	// The mixed-case bead exists; a lowercased lookup would miss.
	mc.Beads["gd-AbCdEf12"] = &model.Bead{ID: "gd-AbCdEf12", Type: "task", Status: model.StatusClosed}

	runMREdges(t, mc)

	var produces int
	for _, d := range mc.AddDepCalls {
		if d.BeadID == "gd-AbCdEf12" && d.DependsOnID == "gd-mr3" && d.Type == "produces" {
			produces++
		}
	}
	if produces != 1 {
		t.Errorf("produces from case-preserved branch id = %d, want 1", produces)
	}
	// Verify the verbatim (mixed-case) id was the one looked up.
	sawMixed := false
	for _, id := range mc.GetBeadCalls {
		if id == "gd-AbCdEf12" {
			sawMixed = true
		}
		if id == "gd-abcdef12" {
			t.Errorf("looked up lowercased branch id; case was not preserved")
		}
	}
	if !sawMixed {
		t.Errorf("never looked up the mixed-case branch id")
	}
}

// Idempotency: when both canonical edges already exist, the command adds
// nothing (but still strips any residual self-loops).
func TestMREdges_IdempotentWhenLinked(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr4", map[string]any{"mr_url": testMRURL})
	mc.ListBeadsPages = [][]*model.Bead{{mr}, {}}
	mc.Deps = map[string][]*model.Dependency{
		"gd-mr4": {
			{BeadID: "gd-mr4", DependsOnID: "gd-task4", Type: "triggered-by"},
		},
		"gd-task4": {
			{BeadID: "gd-task4", DependsOnID: "gd-mr4", Type: "produces"},
		},
	}

	runMREdges(t, mc)

	if len(mc.AddDepCalls) != 0 {
		t.Errorf("added %d edges on already-linked MR, want 0", len(mc.AddDepCalls))
	}
}

// Unresolvable: no cross edge, no carrier, no gd-id in branch. The command
// must add no edges and not crash; self-loops are still cleaned.
func TestMREdges_UnresolvableLeavesEdgesAlone(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr5", map[string]any{
		"mr_url":        testMRURL,
		"source_branch": "fix/no-gd-id-here",
	})
	mc.ListBeadsPages = [][]*model.Bead{{mr}, {}, {}}
	mc.Deps = map[string][]*model.Dependency{"gd-mr5": selfLoopDeps("gd-mr5")}

	runMREdges(t, mc)

	if len(mc.AddDepCalls) != 0 {
		t.Errorf("added %d edges for unresolvable MR, want 0", len(mc.AddDepCalls))
	}
	// self-loops still removed.
	var loops int
	for _, r := range mc.RemoveDepCalls {
		if r.BeadID == "gd-mr5" && r.DependsOnID == "gd-mr5" {
			loops++
		}
	}
	if loops != 2 {
		t.Errorf("self-loop removals = %d, want 2", loops)
	}
}

// Dry-run must not write.
func TestMREdges_DryRunNoWrites(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr6", map[string]any{"mr_url": testMRURL})
	mc.ListBeadsPages = [][]*model.Bead{{mr}, {}}
	mc.Deps = map[string][]*model.Dependency{
		"gd-mr6": append(selfLoopDeps("gd-mr6"),
			&model.Dependency{BeadID: "gd-mr6", DependsOnID: "gd-task6", Type: "triggered-by"}),
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	defer resetMREdgesFlags(t)
	// default dry-run=true.
	captureStdout(t, func() {
		if err := adminBackfillMREdgesCmd.RunE(adminBackfillMREdgesCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.AddDepCalls) != 0 || len(mc.RemoveDepCalls) != 0 {
		t.Errorf("dry-run wrote: adds=%d removes=%d, want 0/0",
			len(mc.AddDepCalls), len(mc.RemoveDepCalls))
	}
}

// TestMREdges_PartialSelfLoopToleratesNoMatch guards the idempotent-caller
// contract from ka-0000001966. applyMREdges removes BOTH candidate self-loop
// types unconditionally, so when only one is present the other remove is a
// guaranteed 0-match. That must stay non-fatal: the command has to complete and
// still add the canonical edges. (The other self-loop tests seed both types, so
// they never exercise the 0-match branch.)
func TestMREdges_PartialSelfLoopToleratesNoMatch(t *testing.T) {
	mc := newMockClient()
	mr := mrBeadWithFields("gd-mr7", map[string]any{"mr_url": testMRURL})
	carrier := taskWithMRFields("gd-task7", map[string]any{"mr_url": testMRURL})
	mc.ListBeadsPages = [][]*model.Bead{{mr}, {carrier}, {}}
	// ONLY the produces self-loop exists — removing triggered-by matches nothing.
	mc.Deps = map[string][]*model.Dependency{
		"gd-mr7":   {{BeadID: "gd-mr7", DependsOnID: "gd-mr7", Type: "produces"}},
		"gd-task7": nil,
	}

	runMREdges(t, mc) // fails the test if RunE returns an error

	// Both removes were attempted, and the 0-match one did not abort the run.
	var loops int
	for _, r := range mc.RemoveDepCalls {
		if r.BeadID == "gd-mr7" && r.DependsOnID == "gd-mr7" {
			loops++
		}
	}
	if loops != 2 {
		t.Errorf("self-loop remove attempts = %d, want 2 (both types tried)", loops)
	}
	// The run continued past the no-op and still repaired the canonical edges.
	if len(mc.AddDepCalls) == 0 {
		t.Error("no canonical edges added — the 0-match remove aborted the repair")
	}
}
