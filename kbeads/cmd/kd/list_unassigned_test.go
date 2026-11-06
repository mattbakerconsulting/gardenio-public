package main

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// resetUnassignedFlags clears the --unassigned / --assignee pair on both
// listCmd and countCmd between subtests. These are package-level cobra commands,
// so a bool left Changed leaks into every later test in the process — and a
// leaked --unassigned is invisible yet devastating: it silently HIDES every
// claimed bead from an unrelated assertion.
func resetUnassignedFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		for _, cmd := range []*pflag.FlagSet{listCmd.Flags(), countCmd.Flags()} {
			cmd.VisitAll(func(f *pflag.Flag) {
				switch f.Name {
				case "unassigned", "assignee":
					f.Changed = false
					_ = f.Value.Set(f.DefValue)
				}
			})
		}
	}
	clear()
	t.Cleanup(clear)
}

// TestList_UnassignedFlag pins the end-to-end CLI wiring of `kd list
// --unassigned`: the flag must land in req.Unassigned so the server applies its
// `assignee IS NULL OR assignee = <empty>` predicate SERVER-SIDE.
//
// The bug this closes (ka-0000007209): there was no way to ask for unclaimed
// beads at all. `--assignee ""` is dropped as "no filter" and returns every bead
// including claimed ones, and `-f assignee=` does not apply because assignee is
// a first-class column, not a JSONB custom field. So the single
// highest-frequency work-seeking query degraded to a full-table pull plus
// hand-rolled jq, where one wrong field name silently yields a false-zero "no
// work" — the same hazard class as sibling --exclude-label (ka-0000007103).
func TestList_UnassignedFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"unset leaves the filter off", nil, false},
		{"--unassigned sets it", []string{"--unassigned"}, true},
		{"--unassigned=false leaves it off", []string{"--unassigned=false"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			withMockClient(t, mc)
			resetUnassignedFlags(t)

			if err := listCmd.Flags().Parse(tc.args); err != nil {
				t.Fatalf("Parse(%v): %v", tc.args, err)
			}
			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			if got := mc.ListBeadsCalls[0].Unassigned; got != tc.want {
				t.Errorf("Unassigned = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCount_UnassignedFlag pins that `kd count --unassigned` forwards the same
// predicate. count reuses buildListRequest verbatim, so the count and the list
// derive one WHERE clause — but the flag has to be REGISTERED on countCmd for
// that to hold, which is what this asserts (registerListFilterFlags is shared).
// Without it `kd count --unassigned` would be an unknown-flag error while
// `kd list --unassigned` worked, so no scriptable count of the ready queue.
func TestCount_UnassignedFlag(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)
	resetCountFlags(t)
	resetUnassignedFlags(t)

	if err := countCmd.Flags().Set("unassigned", "true"); err != nil {
		t.Fatalf("setting --unassigned: %v", err)
	}
	captureStdout(t, func() {
		if err := countCmd.RunE(countCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if !call.Unassigned {
		t.Errorf("kd count --unassigned must set Unassigned=true, got %+v", call)
	}
	// The count-only fast path must still be taken — --unassigned narrows WHICH
	// rows match, it must not knock the caller off the bare-COUNT(*) path.
	if !call.CountOnly {
		t.Error("kd count --unassigned must still set CountOnly=true")
	}
}

// TestList_UnassignedAndAssigneeConflict pins that the contradictory pair fails
// LOUDLY. "assigned to alice AND assigned to nobody" matches nothing by
// construction, and a silent empty result is indistinguishable from "no work
// matches" — precisely the false-zero this flag was added to remove. So the
// combination must error rather than print an honest-looking zero.
func TestList_UnassignedAndAssigneeConflict(t *testing.T) {
	for _, cmdName := range []string{"list", "count"} {
		t.Run(cmdName, func(t *testing.T) {
			mc := newMockClient()
			withMockClient(t, mc)
			resetCountFlags(t)
			resetUnassignedFlags(t)

			cmd := listCmd
			if cmdName == "count" {
				cmd = countCmd
			}
			if err := cmd.Flags().Set("unassigned", "true"); err != nil {
				t.Fatalf("setting --unassigned: %v", err)
			}
			if err := cmd.Flags().Set("assignee", "alice"); err != nil {
				t.Fatalf("setting --assignee: %v", err)
			}

			var err error
			captureStdout(t, func() { err = cmd.RunE(cmd, nil) })
			if err == nil {
				t.Fatal("--unassigned with --assignee must error, got nil")
			}
			if !strings.Contains(err.Error(), "mutually exclusive") {
				t.Errorf("error %q should say the flags are mutually exclusive", err)
			}
			// It must fail BEFORE issuing a query — a request that can only
			// return 0 should never reach the daemon.
			if len(mc.ListBeadsCalls) != 0 {
				t.Errorf("expected no ListBeads call on a rejected flag combo, got %d", len(mc.ListBeadsCalls))
			}
		})
	}
}
