package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

// resetCountFlags clears the countCmd filter flags a subtest mutates so state
// never leaks across RunE calls within one process. Mirrors
// resetListOperationalFlags for the sibling listCmd; countCmd shares the same
// filter surface (registerListFilterFlags) plus the same package globals.
//
// It clears BOTH up-front and on t.Cleanup. The trailing clear matters because
// pflag state on these package-level cobra commands is global: a `--field` value
// left set leaks into every later test in the process, and there it is invisible
// — with a `-f` predicate attached and a 0-row mock, `kd count`/`kd list` emit
// the hidden-kind nudge, whose probe is a SECOND ListBeads call that breaks the
// many `len(ListBeadsCalls) == 1` assertions elsewhere, far from the cause.
func resetCountFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		countCmd.Flags().VisitAll(func(f *pflag.Flag) {
			switch f.Name {
			case "type", "kind", "status", "label", "label-any", "exclude-label", "field":
				f.Changed = false
				if sv, ok := f.Value.(pflag.SliceValue); ok {
					_ = sv.Replace(nil)
				}
			case "all-types", "all-projects", "project", "parent", "include-operational",
				"all-fleets", "fleet", "assignee", "unassigned", "no-blockers":
				f.Changed = false
				_ = f.Value.Set(f.DefValue)
			}
		})
		listProjectFlag = ""
		listAllProjectsFlag = false
		listFleetFlag = ""
		listAllFleetsFlag = false
	}
	clear()
	t.Cleanup(clear)
}

// TestCount_SetsCountOnly asserts `kd count` flips CountOnly=true so the daemon
// takes the bare SELECT COUNT(*) fast path and transfers no bead bodies.
func TestCount_SetsCountOnly(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetCountFlags(t)

	captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	if !mc.ListBeadsCalls[0].CountOnly {
		t.Errorf("kd count must set CountOnly=true, got %+v", mc.ListBeadsCalls[0])
	}
}

// TestCount_PrintsBareInteger asserts stdout is exactly the total integer plus a
// newline — a jq-parseable number, scriptable as N=$(kd count ...). No bead
// bodies, no footer, no envelope.
func TestCount_PrintsBareInteger(t *testing.T) {
	mc := newMockClient()
	// Mock returns Total = len(ListBeadsResult); use 3 dummy rows so Total=3.
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-1", Title: "a", Status: "open", Type: "bug"},
		{ID: "gd-2", Title: "b", Status: "open", Type: "bug"},
		{ID: "gd-3", Title: "c", Status: "open", Type: "bug"},
	}
	withMockClient(t, mc)

	resetCountFlags(t)

	out := captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if strings.TrimRight(out, "\n") != "3" {
		t.Errorf("kd count stdout = %q, want bare \"3\\n\"", out)
	}
	// It must be ONLY the integer — no bead IDs leaked into stdout.
	if strings.Contains(out, "gd-1") {
		t.Errorf("kd count must not print bead bodies, got:\n%s", out)
	}
}

// TestCount_ForwardsFilterFlags asserts kd count threads kd list's filter flags
// onto the wire request identically (it reuses buildListRequest), so a count and
// a list over the same flags agree by construction.
func TestCount_ForwardsFilterFlags(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("status", "open"); err != nil {
		t.Fatalf("setting --status: %v", err)
	}
	if err := countCmd.Flags().Set("type", "bug"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}
	if err := countCmd.Flags().Set("assignee", "alice"); err != nil {
		t.Fatalf("setting --assignee: %v", err)
	}

	captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if !slices.Contains(call.Status, "open") {
		t.Errorf("Status = %v, want to contain open", call.Status)
	}
	if !slices.Contains(call.Type, "bug") {
		t.Errorf("Type = %v, want to contain bug", call.Type)
	}
	if call.Assignee != "alice" {
		t.Errorf("Assignee = %q, want alice", call.Assignee)
	}
}

// TestCount_DefaultIssueScope asserts a plain `kd count` (no filters) reproduces
// `kd list`'s default eligibility: it injects kind=issue and applies the
// implicit project label — so `kd count` counts exactly what a default `kd list`
// would page, never drifting from the list footer's total.
func TestCount_DefaultIssueScope(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}

	captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if !slices.Contains(call.Kind, "issue") {
		t.Errorf("default kd count must inject kind=issue, got Kind=%v", call.Kind)
	}
	if !slices.Contains(call.Labels, "project:gardenio") {
		t.Errorf("default kd count must apply the implicit project label, got Labels=%v", call.Labels)
	}
}

// TestCount_IncludeOperational asserts --include-operational on `kd count`
// behaves exactly as on `kd list` (shared buildListRequest): it appends
// escalation+decision, drops the kind=issue injection, and skips project
// scoping — so `kd count --include-operational` sizes the operational beads a
// plain default count hides.
func TestCount_IncludeOperational(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := countCmd.Flags().Set("include-operational", "true"); err != nil {
		t.Fatalf("setting --include-operational: %v", err)
	}

	captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if !slices.Contains(call.Type, "escalation") || !slices.Contains(call.Type, "decision") {
		t.Errorf("Type = %v, want escalation+decision", call.Type)
	}
	if slices.Contains(call.Kind, "issue") {
		t.Errorf("--include-operational must not inject kind=issue, got Kind=%v", call.Kind)
	}
	if slices.Contains(call.Labels, "project:gardenio") {
		t.Errorf("--include-operational must skip project scoping, got Labels=%v", call.Labels)
	}
}

// TestCount_UnknownTypeFailsLoud asserts kd count inherits kd list's fail-loud
// --type validation (buildListRequest → validateTypeFilter): an unknown type is
// a hard error, not a silent "0" that reads as a false all-clear (ka-0000006244).
func TestCount_UnknownTypeFailsLoud(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("type", "zzznotatype"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}

	err := countCmd.RunE(countCmd, nil)
	if err == nil {
		t.Fatalf("expected an error for an unknown --type, got nil")
	}
	if !strings.Contains(err.Error(), "unknown bead type") {
		t.Errorf("error = %q, want it to name the unknown type", err.Error())
	}
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("kd count must not query the daemon on invalid --type, got %d calls", len(mc.ListBeadsCalls))
	}
}
