package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

// TestIsIssueOnlyQuery covers the helper that decides whether a kd list
// query is constrained to issue-kind beads (and therefore safe to scope
// by project label).
func TestIsIssueOnlyQuery(t *testing.T) {
	cases := []struct {
		name     string
		beadType []string
		kind     []string
		want     bool
	}{
		{"both empty", nil, nil, false},
		{"kind=issue only", nil, []string{"issue"}, true},
		{"kind=config", nil, []string{"config"}, false},
		{"kind=data", nil, []string{"data"}, false},
		{"type=bug", []string{"bug"}, nil, true},
		{"type=task,bug", []string{"task", "bug"}, nil, true},
		{"type=epic", []string{"epic"}, nil, true},
		{"type=feature", []string{"feature"}, nil, true},
		{"type=chore", []string{"chore"}, nil, true},
		{"type=formula", []string{"formula"}, nil, false},
		{"type=molecule", []string{"molecule"}, nil, false},
		{"type=agent", []string{"agent"}, nil, false},
		{"type=bug + type=formula", []string{"bug", "formula"}, nil, false},
		{"kind=issue + type=bug", []string{"bug"}, []string{"issue"}, true},
		{"kind=issue + type=formula", []string{"formula"}, []string{"issue"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isIssueOnlyQuery(tc.beadType, tc.kind)
			if got != tc.want {
				t.Errorf("isIssueOnlyQuery(%v, %v) = %v, want %v", tc.beadType, tc.kind, got, tc.want)
			}
		})
	}
}

// TestProjectScopeDescriptor covers the footer note that makes the implicit
// project filter visible (ka-0000007254). Applied → names the project + opt-out
// flag; not applied → "all projects".
func TestProjectScopeDescriptor(t *testing.T) {
	cases := []struct {
		name    string
		applied bool
		project string
		want    string
	}{
		{"applied gardenio", true, "gardenio", "project=gardenio — --all-projects to widen"},
		{"applied monorepo", true, "monorepo", "project=monorepo — --all-projects to widen"},
		{"not applied", false, "gardenio", "all projects"},
		{"not applied empty project", false, "", "all projects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := projectScopeDescriptor(tc.applied, tc.project); got != tc.want {
				t.Errorf("projectScopeDescriptor(%v, %q) = %q, want %q", tc.applied, tc.project, got, tc.want)
			}
		})
	}
}

// TestListFooter covers the footer formatter: the scope note is only added
// when non-empty, preserving the legacy "N beads (M total)" shape everywhere a
// caller passes no scope (search, view, fleet, watch).
func TestListFooter(t *testing.T) {
	if got := listFooter(2, 10, ""); got != "2 beads (10 total)" {
		t.Errorf("listFooter with empty scope = %q, want plain footer", got)
	}
	want := "2 beads (10 total; project=gardenio — --all-projects to widen)"
	if got := listFooter(2, 10, "project=gardenio — --all-projects to widen"); got != want {
		t.Errorf("listFooter with scope = %q, want %q", got, want)
	}
}

// TestList_ScopeFooter_Scoped asserts the end-to-end footer for an
// issue-scoped query names the project and opt-out flag.
func TestList_ScopeFooter_Scoped(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-1", Title: "First", Status: "open", Type: "task"}}
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("type", "task"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}

	out := captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "1 beads (1 total; project=gardenio — --all-projects to widen)") {
		t.Errorf("expected scoped footer, got:\n%s", out)
	}
}

// TestList_ScopeFooter_Unscoped asserts the footer for a query that does NOT
// apply the implicit project filter (--type=config) reads "all projects" —
// the tell that distinguishes it from a silently-scoped query.
func TestList_ScopeFooter_Unscoped(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-1", Title: "First", Status: "open", Type: "config"}}
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("type", "config"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}

	out := captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "1 beads (1 total; all projects)") {
		t.Errorf("expected unscoped footer, got:\n%s", out)
	}
}

// resetListProjectFlags clears flags that this test file mutates, so a
// previous subtest's setting doesn't leak into the next one. Cobra carries
// the Changed bit and the value across RunE calls within one process.
//
// Note: slice flags (StringSliceP) cannot be reset via f.Value.Set(f.DefValue)
// because pflag's DefValue for a slice is the literal string "[]" — feeding
// that back through Set yields a slice containing the string "[]". Reset
// slice flags via the SliceValue interface instead.
func resetListProjectFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		case "type", "kind", "status", "label":
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace(nil)
			}
		// "unassigned"/"assignee" are cleared defensively. They are mutually
		// exclusive, so a stale --unassigned would make RunE return the conflict
		// error instead of listing, and the assertions here would then see EMPTY
		// output — a failure whose message points nowhere near the real cause.
		// No current test leaks it (the --unassigned tests all clear via
		// t.Cleanup); this keeps a future one from being baffling.
		case "all-types", "all-projects", "project", "parent", "unassigned", "assignee":
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
	// Package-level vars used for --project / --all-projects are bound via
	// StringVar/BoolVar, so the Set() above resets them. Belt and braces:
	listProjectFlag = ""
	listAllProjectsFlag = false
}

// TestList_ProjectFilter_AppliesToTypeBug is the regression test for
// ka-0000005814: `kd list --type=bug --project=<name>` must scope by
// project, not silently drop the filter.
func TestList_ProjectFilter_AppliesToTypeBug(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("type", "bug"); err != nil {
		t.Fatalf("setting --type: %v", err)
	}
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	got := mc.ListBeadsCalls[0].Labels
	if !slices.Contains(got, "project:gardenio") {
		t.Errorf("expected Labels to include project:gardenio, got %v", got)
	}
}

// TestList_ProjectFilter_DefaultMode covers the existing behavior where
// `kd list` (no flags) applies the project filter via the implicit
// kind=issue path. The fix must not regress this.
func TestList_ProjectFilter_DefaultMode(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	got := mc.ListBeadsCalls[0].Labels
	if !slices.Contains(got, "project:gardenio") {
		t.Errorf("expected Labels to include project:gardenio, got %v", got)
	}
}

// TestList_ProjectFilter_AllProjects covers `--all-projects` opt-out:
// the project filter must NOT be applied even when --project is set.
func TestList_ProjectFilter_AllProjects(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("all-projects", "true"); err != nil {
		t.Fatalf("setting --all-projects: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	got := mc.ListBeadsCalls[0].Labels
	if slices.Contains(got, "project:gardenio") {
		t.Errorf("expected Labels NOT to include project:gardenio under --all-projects, got %v", got)
	}
}

// TestList_ProjectFilter_AllTypes covers `--all-types`: project filter
// must be skipped because the result set may include infrastructure
// beads that don't carry project labels.
func TestList_ProjectFilter_AllTypes(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("all-types", "true"); err != nil {
		t.Fatalf("setting --all-types: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	got := mc.ListBeadsCalls[0].Labels
	if slices.Contains(got, "project:gardenio") {
		t.Errorf("expected Labels NOT to include project:gardenio under --all-types, got %v", got)
	}
}

// TestList_ProjectFilter_NonIssueType covers `--type=formula` (a data-kind
// bead): project filter must be skipped, matching the prior behavior of
// not silently excluding rows that may not carry project labels.
func TestList_ProjectFilter_NonIssueType(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("type", "formula"); err != nil {
		t.Fatalf("setting --type=formula: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	got := mc.ListBeadsCalls[0].Labels
	if slices.Contains(got, "project:gardenio") {
		t.Errorf("expected Labels NOT to include project:gardenio for --type=formula, got %v", got)
	}
}

// TestList_ProjectFilter_KindIssue covers `--kind=issue --type=bug`:
// explicit issue-kind narrowing should still apply the project filter.
func TestList_ProjectFilter_KindIssue(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("kind", "issue"); err != nil {
		t.Fatalf("setting --kind=issue: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	got := mc.ListBeadsCalls[0].Labels
	if !slices.Contains(got, "project:gardenio") {
		t.Errorf("expected Labels to include project:gardenio for --kind=issue, got %v", got)
	}
}

// TestList_ParentSubtree_SuppressesImplicitProject is the regression test for
// the meta-gardener stop-decision undercount (ka-0000008241). An ID-anchored
// subtree query (`kd list --parent <epic>`) must NOT silently AND the caller's
// implicit project label onto the subtree — that dropped cross-project
// children (e.g. a gardenio plan's project:monorepo tasks) so the epic read as
// childless and the scorer proposed stopping a still-active gardener. The
// query must forward Parent and carry NO project:gardenio label.
func TestList_ParentSubtree_SuppressesImplicitProject(t *testing.T) {
	mc := newMockClient()
	// A cross-project subtree: the plan is project:gardenio but the child is
	// project:monorepo. Both should be countable via --parent.
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-child", Title: "monorepo child", Status: "open", Type: "task"},
	}
	withMockClient(t, mc)

	resetListProjectFlags(t)
	// Simulate the IMPLICIT project: set the bound var directly (as the flag
	// default would), WITHOUT calling Set — so cmd.Flags().Changed("project")
	// stays false. This is the real agent shape: `kd list --parent <epic>`
	// with KD_PROJECT=gardenio in the environment and no explicit --project.
	listProjectFlag = "gardenio"
	if err := listCmd.Flags().Set("parent", "gd-EPIC1"); err != nil {
		t.Fatalf("setting --parent: %v", err)
	}
	if err := listCmd.Flags().Set("status", "open,in_progress,blocked"); err != nil {
		t.Fatalf("setting --status: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	call := mc.ListBeadsCalls[0]
	if call.Parent != "gd-EPIC1" {
		t.Errorf("expected Parent=gd-EPIC1 forwarded, got %q", call.Parent)
	}
	if slices.Contains(call.Labels, "project:gardenio") {
		t.Errorf("--parent must suppress the implicit project filter, but Labels=%v included project:gardenio", call.Labels)
	}
}

// TestList_ParentSubtree_ExplicitProjectHonored covers the escape hatch: an
// EXPLICIT --project alongside --parent is still honored, so an operator can
// deliberately narrow the subtree to one project's slice. The suppression only
// targets the IMPLICIT (default) project label.
func TestList_ParentSubtree_ExplicitProjectHonored(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	resetListProjectFlags(t)
	// Explicitly set --project (Changed=true) plus --parent.
	if err := listCmd.Flags().Set("project", "monorepo"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("parent", "gd-EPIC1"); err != nil {
		t.Fatalf("setting --parent: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	call := mc.ListBeadsCalls[0]
	if call.Parent != "gd-EPIC1" {
		t.Errorf("expected Parent=gd-EPIC1 forwarded, got %q", call.Parent)
	}
	if !slices.Contains(call.Labels, "project:monorepo") {
		t.Errorf("an explicit --project must still be honored under --parent, got Labels=%v", call.Labels)
	}
}
