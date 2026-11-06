package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

// resetListOperationalFlags clears the flags this file mutates so a prior
// subtest's setting never leaks across RunE calls within one process. Mirrors
// resetListProjectFlags (which this file also leans on for --project/--type).
func resetListOperationalFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		case "type", "kind", "status", "label":
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			}
		case "all-types", "all-projects", "project", "parent", "include-operational":
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
	listProjectFlag = ""
	listAllProjectsFlag = false
}

// withStdoutTTY forces isStdoutTTY to the given value for the test's duration,
// restoring the real hook on cleanup. The nudge is TTY-gated, so tests that
// assert its presence must claim a TTY; the default (piped) path is what keeps
// the ~12 `len(ListBeadsCalls)==1` assertions elsewhere green.
func withStdoutTTY(t *testing.T, tty bool) {
	t.Helper()
	prev := isStdoutTTY
	t.Cleanup(func() { isStdoutTTY = prev })
	isStdoutTTY = func() bool { return tty }
}

// TestList_IncludeOperational_AppendsTypes asserts --include-operational
// appends escalation+decision to the type filter (the working `-t escalation
// -t decision` path) so kind=data operational beads become eligible, and does
// NOT inject the default kind=issue clause.
func TestList_IncludeOperational_AppendsTypes(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	withStdoutTTY(t, false) // no nudge; single ListBeads call

	resetListOperationalFlags(t)
	if err := listCmd.Flags().Set("include-operational", "true"); err != nil {
		t.Fatalf("setting --include-operational: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call (nudge TTY-gated off), got %d", len(mc.ListBeadsCalls))
	}
	call := mc.ListBeadsCalls[0]
	if !slices.Contains(call.Type, "escalation") || !slices.Contains(call.Type, "decision") {
		t.Errorf("expected Type to include escalation+decision, got %v", call.Type)
	}
	if slices.Contains(call.Kind, "issue") {
		t.Errorf("--include-operational must NOT inject kind=issue, got Kind=%v", call.Kind)
	}
}

// TestList_IncludeOperational_SuppressesProjectScope asserts the flag skips the
// implicit project-label filter (mirroring --all-types), so operational beads —
// which may not carry a project: label — are not silently dropped.
func TestList_IncludeOperational_SuppressesProjectScope(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListOperationalFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("include-operational", "true"); err != nil {
		t.Fatalf("setting --include-operational: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if slices.Contains(call.Labels, "project:gardenio") {
		t.Errorf("--include-operational must suppress the implicit project filter, got Labels=%v", call.Labels)
	}
}

// TestList_IncludeOperational_AllTypesWins asserts that under --all-types the
// operational types are NOT appended: --all-types already returns every type,
// and appending a type filter would instead NARROW to just escalation/decision.
func TestList_IncludeOperational_AllTypesWins(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListOperationalFlags(t)
	if err := listCmd.Flags().Set("all-types", "true"); err != nil {
		t.Fatalf("setting --all-types: %v", err)
	}
	if err := listCmd.Flags().Set("include-operational", "true"); err != nil {
		t.Fatalf("setting --include-operational: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if len(call.Type) != 0 {
		t.Errorf("under --all-types no type filter should be appended, got Type=%v", call.Type)
	}
	if slices.Contains(call.Kind, "issue") {
		t.Errorf("--all-types must not inject kind=issue, got Kind=%v", call.Kind)
	}
}

// TestList_OperationalNudge_FiresOnTTY is the happy path: a plain interactive
// `kd list` that hid open escalation/decision beads prints the one-line stderr
// nudge, sized by a second count-only query, and names --include-operational.
func TestList_OperationalNudge_FiresOnTTY(t *testing.T) {
	mc := newMockClient()
	// Call 0 = main issue list (1 task). Call 1 = count-only (2 operational).
	mc.ListBeadsPages = [][]*model.Bead{
		{{ID: "gd-task1", Title: "a task", Status: "open", Type: "task"}},
		{
			{ID: "gd-esc1", Title: "esc", Status: "open", Type: "escalation"},
			{ID: "gd-dec1", Title: "dec", Status: "open", Type: "decision"},
		},
	}
	withMockClient(t, mc)
	withStdoutTTY(t, true)

	resetListOperationalFlags(t)

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) != 2 {
		t.Fatalf("expected 2 ListBeads calls (list + count), got %d", len(mc.ListBeadsCalls))
	}
	// The second call must be the count-only query for open escalation/decision.
	count := mc.ListBeadsCalls[1]
	if !count.CountOnly {
		t.Errorf("nudge count query must set CountOnly, got %+v", count)
	}
	if !slices.Contains(count.Type, "escalation") || !slices.Contains(count.Type, "decision") {
		t.Errorf("nudge count query Type = %v, want escalation+decision", count.Type)
	}
	if !slices.Contains(count.Status, "open") {
		t.Errorf("nudge count query Status = %v, want open", count.Status)
	}
	if !strings.Contains(stderr, "2 open escalation/decision beads hidden") {
		t.Errorf("expected nudge with count 2, got stderr:\n%s", stderr)
	}
	if !strings.Contains(stderr, "--include-operational") {
		t.Errorf("nudge must point at --include-operational, got:\n%s", stderr)
	}
}

// TestList_OperationalNudge_SilentWhenNone asserts no nudge (and no wasted
// count round-trip surfacing text) when the count is zero — the count call
// still happens but prints nothing.
func TestList_OperationalNudge_SilentWhenNone(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{{ID: "gd-task1", Title: "a task", Status: "open", Type: "task"}},
		{}, // count-only: zero operational beads
	}
	withMockClient(t, mc)
	withStdoutTTY(t, true)

	resetListOperationalFlags(t)

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if strings.Contains(stderr, "hidden") {
		t.Errorf("expected no nudge when count is zero, got stderr:\n%s", stderr)
	}
}

// TestList_OperationalNudge_SilentNonTTY asserts a piped (non-TTY) stdout —
// the agent poll-loop / script shape — pays for neither the count round-trip
// nor the nudge. This is also what keeps the many `len(ListBeadsCalls)==1`
// assertions elsewhere green.
func TestList_OperationalNudge_SilentNonTTY(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-task1", Title: "t", Status: "open", Type: "task"}}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListOperationalFlags(t)

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("non-TTY must not issue the count query; expected 1 call, got %d", len(mc.ListBeadsCalls))
	}
	if strings.Contains(stderr, "hidden") {
		t.Errorf("non-TTY must not print the nudge, got stderr:\n%s", stderr)
	}
}

// TestList_OperationalNudge_SilentUnderJSON asserts --json suppresses the nudge
// (and its count query) even on a TTY: machine-parsed output must not carry the
// interactive affordance, and the JSON path never reaches the nudge branch.
func TestList_OperationalNudge_SilentUnderJSON(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-task1", Title: "t", Status: "open", Type: "task"}}
	withMockClient(t, mc)
	withStdoutTTY(t, true)
	// withMockClient resets jsonOutput on cleanup; set it after the swap.
	jsonOutput = true

	resetListOperationalFlags(t)

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("--json must not issue the count query; expected 1 call, got %d", len(mc.ListBeadsCalls))
	}
	if strings.Contains(stderr, "hidden") {
		t.Errorf("--json must not print the nudge, got stderr:\n%s", stderr)
	}
}

// TestList_OperationalNudge_SilentWhenShown asserts that when the reader ALREADY
// passed --include-operational (so nothing is hidden) the nudge does not fire.
func TestList_OperationalNudge_SilentWhenShown(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-esc1", Title: "esc", Status: "open", Type: "escalation"}}
	withMockClient(t, mc)
	withStdoutTTY(t, true)

	resetListOperationalFlags(t)
	if err := listCmd.Flags().Set("include-operational", "true"); err != nil {
		t.Fatalf("setting --include-operational: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("--include-operational must not issue a count query; expected 1 call, got %d", len(mc.ListBeadsCalls))
	}
	if strings.Contains(stderr, "hidden") {
		t.Errorf("no nudge when operational beads are already shown, got:\n%s", stderr)
	}
}

// TestList_OperationalNudge_SilentWithExplicitType asserts that an explicit
// -t bug (defaultIssueMode false, so nothing was hidden by the kind=issue
// injection) suppresses the nudge — the reader chose a specific type filter.
func TestList_OperationalNudge_SilentWithExplicitType(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-bug1", Title: "b", Status: "open", Type: "bug"}}
	withMockClient(t, mc)
	withStdoutTTY(t, true)

	resetListOperationalFlags(t)
	if err := listCmd.Flags().Set("type", "bug"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("explicit --type must not issue a count query; expected 1 call, got %d", len(mc.ListBeadsCalls))
	}
	if strings.Contains(stderr, "hidden") {
		t.Errorf("explicit --type must suppress the nudge, got:\n%s", stderr)
	}
}

// TestList_OperationalNudge_PropagatesFleet asserts the count-only nudge query
// carries the SAME fleet scope as the main list, so an admin `kd list
// --all-fleets` gets a cross-fleet count rather than a single-fleet undercount
// under a "fleet-wide" note.
func TestList_OperationalNudge_PropagatesFleet(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{
		{{ID: "gd-task1", Title: "t", Status: "open", Type: "task"}},
		{{ID: "gd-esc1", Title: "e", Status: "open", Type: "escalation"}},
	}
	withMockClient(t, mc)
	withStdoutTTY(t, true)

	resetListOperationalFlags(t)
	origAllFleets := listAllFleetsFlag
	t.Cleanup(func() { listAllFleetsFlag = origAllFleets })
	listAllFleetsFlag = true

	captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) != 2 {
		t.Fatalf("expected 2 ListBeads calls, got %d", len(mc.ListBeadsCalls))
	}
	if got := mc.ListBeadsCalls[1].Fleet; got != model.FleetAll {
		t.Errorf("nudge count query Fleet = %q, want %q (matching --all-fleets main list)", got, model.FleetAll)
	}
}

// TestList_OperationalNudge_ScopeQualifier asserts the "fleet-wide" qualifier
// appears only when the MAIN list was project-scoped (so the reader is not
// misled that the fleet-wide operational count shares the narrower scope), and
// is omitted when the main list was itself fleet-wide.
func TestList_OperationalNudge_ScopeQualifier(t *testing.T) {
	t.Run("scoped main list gets fleet-wide qualifier", func(t *testing.T) {
		mc := newMockClient()
		mc.ListBeadsPages = [][]*model.Bead{
			{{ID: "gd-task1", Title: "t", Status: "open", Type: "task"}},
			{{ID: "gd-esc1", Title: "e", Status: "open", Type: "escalation"}},
		}
		withMockClient(t, mc)
		withStdoutTTY(t, true)

		resetListOperationalFlags(t)
		if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
			t.Fatalf("setting --project: %v", err)
		}

		stderr := captureStderr(t, func() {
			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})
		})
		if !strings.Contains(stderr, "hidden fleet-wide") {
			t.Errorf("expected 'fleet-wide' qualifier for a project-scoped main list, got:\n%s", stderr)
		}
		// Singular noun for count 1.
		if !strings.Contains(stderr, "1 open escalation/decision bead ") {
			t.Errorf("expected singular 'bead' for count 1, got:\n%s", stderr)
		}
	})

	t.Run("unscoped main list omits qualifier", func(t *testing.T) {
		mc := newMockClient()
		mc.ListBeadsPages = [][]*model.Bead{
			{{ID: "gd-task1", Title: "t", Status: "open", Type: "task"}},
			{{ID: "gd-esc1", Title: "e", Status: "open", Type: "escalation"}},
		}
		withMockClient(t, mc)
		withStdoutTTY(t, true)

		resetListOperationalFlags(t)
		if err := listCmd.Flags().Set("all-projects", "true"); err != nil {
			t.Fatalf("setting --all-projects: %v", err)
		}

		stderr := captureStderr(t, func() {
			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})
		})
		if strings.Contains(stderr, "fleet-wide") {
			t.Errorf("unscoped main list should not add the 'fleet-wide' qualifier, got:\n%s", stderr)
		}
		if !strings.Contains(stderr, "hidden — kd list --include-operational") {
			t.Errorf("expected the base nudge without qualifier, got:\n%s", stderr)
		}
	})
}
