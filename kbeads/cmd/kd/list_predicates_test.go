package main

import (
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/spf13/pflag"
)

// TestList_HasReportFlag pins the three-state semantics of --has-report:
//
//	flag unset       → req.HasReport == nil
//	--has-report     → &true
//	--has-report=false → &false
func TestList_HasReportFlag(t *testing.T) {
	cases := []struct {
		name    string
		setFlag bool
		flagVal string
		want    *bool
	}{
		{"unset", false, "", nil},
		{"true_short", true, "true", boolP(true)},
		{"false_explicit", true, "false", boolP(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetListPredicateFlags(t)
			if tc.setFlag {
				if err := listCmd.Flags().Set("has-report", tc.flagVal); err != nil {
					t.Fatalf("setting --has-report: %v", err)
				}
			}

			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := mc.ListBeadsCalls[0].HasReport
			if !boolPtrEq(got, tc.want) {
				t.Errorf("HasReport = %v, want %v", boolPtrFmt(got), boolPtrFmt(tc.want))
			}
		})
	}
}

// TestList_BlockedOnDecisionFlag mirrors TestList_HasReportFlag for the R2
// predicate. Same three-state shape, different field.
func TestList_BlockedOnDecisionFlag(t *testing.T) {
	cases := []struct {
		name    string
		setFlag bool
		flagVal string
		want    *bool
	}{
		{"unset", false, "", nil},
		{"true_short", true, "true", boolP(true)},
		{"false_explicit", true, "false", boolP(false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetListPredicateFlags(t)
			if tc.setFlag {
				if err := listCmd.Flags().Set("blocked-on-decision", tc.flagVal); err != nil {
					t.Fatalf("setting --blocked-on-decision: %v", err)
				}
			}

			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := mc.ListBeadsCalls[0].BlockedOnDecision
			if !boolPtrEq(got, tc.want) {
				t.Errorf("BlockedOnDecision = %v, want %v", boolPtrFmt(got), boolPtrFmt(tc.want))
			}
		})
	}
}

// TestList_WithBlockersFlag pins the --with-blockers plumbing (ka-0000000945).
// Unlike the two predicates above this is a plain opt-in enrichment, not a
// three-state filter: absent means "don't enrich", never "filter to unblocked".
func TestList_WithBlockersFlag(t *testing.T) {
	cases := []struct {
		name    string
		setFlag bool
		want    bool
	}{
		{"unset_stays_off", false, false},
		{"set_opts_in", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetListPredicateFlags(t)
			if tc.setFlag {
				if err := listCmd.Flags().Set("with-blockers", "true"); err != nil {
					t.Fatalf("setting --with-blockers: %v", err)
				}
			}

			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			if got := mc.ListBeadsCalls[0].WithOpenBlockers; got != tc.want {
				t.Errorf("WithOpenBlockers = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestList_WithBlockersIsNotAFilter guards the one way this flag could become
// dangerous: --with-blockers must never touch NoBlockers (or any other
// predicate). If enriching ever started filtering, `kd list --with-blockers`
// would quietly return a DIFFERENT row set than `kd list` — and a triage
// caller would draw conclusions about a backlog it never actually saw.
func TestList_WithBlockersIsNotAFilter(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListPredicateFlags(t)
	if err := listCmd.Flags().Set("with-blockers", "true"); err != nil {
		t.Fatalf("setting --with-blockers: %v", err)
	}
	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	req := mc.ListBeadsCalls[0]
	if !req.WithOpenBlockers {
		t.Fatal("WithOpenBlockers should be set")
	}
	if req.NoOpenDeps {
		t.Error("--with-blockers must not imply --no-blockers: it enriches rows, it does not filter them")
	}
	if req.HasReport != nil || req.BlockedOnDecision != nil {
		t.Error("--with-blockers must not set any other predicate")
	}
}

// TestList_WithBlockersDetectsOldDaemon is the version-skew guard
// (ka-0000000945). A daemon predating with_open_blockers ignores the unknown
// param and returns rows with open_blockers absent — which is indistinguishable
// from "nothing is blocked" unless the client checks. Printing that to a triage
// caller would be a fleet-wide false all-clear, so `kd list --with-blockers`
// must ERROR instead.
func TestList_WithBlockersDetectsOldDaemon(t *testing.T) {
	mc := newMockClient()
	// An old daemon: rows come back, but no row carries blocker data.
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-a", Title: "a", Status: model.StatusOpen, Type: model.TypeTask},
		{ID: "gd-b", Title: "b", Status: model.StatusOpen, Type: model.TypeTask},
	}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListPredicateFlags(t)
	if err := listCmd.Flags().Set("with-blockers", "true"); err != nil {
		t.Fatalf("setting --with-blockers: %v", err)
	}

	var err error
	captureStdout(t, func() { err = listCmd.RunE(listCmd, nil) })
	if err == nil {
		t.Fatal("--with-blockers against a daemon that returned no blocker data must error, not print an unblocked-looking list")
	}
	if !strings.Contains(err.Error(), "too old") {
		t.Errorf("error %q should explain the daemon is too old", err)
	}
}

// TestList_WithBlockersAcceptsGenuineAllClear is the other half of the guard:
// a CURRENT daemon reporting that nothing is blocked sends present-but-empty
// lists, and that must print normally. If this ever starts erroring, the skew
// check has become a false positive on a healthy, genuinely-unblocked backlog.
func TestList_WithBlockersAcceptsGenuineAllClear(t *testing.T) {
	mc := newMockClient()
	empty := []*model.OpenBlocker{}
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-a", Title: "a", Status: model.StatusOpen, Type: model.TypeTask, OpenBlockers: &empty},
	}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListPredicateFlags(t)
	if err := listCmd.Flags().Set("with-blockers", "true"); err != nil {
		t.Fatalf("setting --with-blockers: %v", err)
	}

	var err error
	out := captureStdout(t, func() { err = listCmd.RunE(listCmd, nil) })
	if err != nil {
		t.Fatalf("a genuine all-clear from a current daemon must not error: %v", err)
	}
	// The column must still render, so the reader can see the check happened.
	if !strings.Contains(out, "BLOCKED BY") {
		t.Errorf("expected the BLOCKED BY column on an answered --with-blockers page, got:\n%s", out)
	}
}

// TestList_PredicatesCompose locks in that --has-report and
// --blocked-on-decision compose with each other and with the existing
// project label filter.
func TestList_PredicatesCompose(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListPredicateFlags(t)
	if err := listCmd.Flags().Set("has-report", "true"); err != nil {
		t.Fatalf("setting --has-report: %v", err)
	}
	if err := listCmd.Flags().Set("blocked-on-decision", "false"); err != nil {
		t.Fatalf("setting --blocked-on-decision: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if !boolPtrEq(req.HasReport, boolP(true)) {
		t.Errorf("HasReport = %v, want &true", boolPtrFmt(req.HasReport))
	}
	if !boolPtrEq(req.BlockedOnDecision, boolP(false)) {
		t.Errorf("BlockedOnDecision = %v, want &false", boolPtrFmt(req.BlockedOnDecision))
	}
}

func boolP(b bool) *bool { return &b }

func boolPtrEq(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolPtrFmt(p *bool) any {
	if p == nil {
		return nil
	}
	return *p
}

// resetListPredicateFlags clears the listCmd flags this file mutates so a
// previous subtest's setting doesn't leak into the next one. Cobra carries
// the Changed bit and the value across RunE calls within one process; both
// must be reset for the three-state predicate tests to behave.
func resetListPredicateFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "has-report" || f.Name == "blocked-on-decision" || f.Name == "with-blockers" {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
}
