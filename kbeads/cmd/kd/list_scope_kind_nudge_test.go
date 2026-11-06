package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

// The scope-kind nudge (maybePrintScopeKindNudge, list_nudges.go) closes two
// variants of the same false-answer footgun that the default kind=issue scoping
// creates:
//
//   - `-f/--field` false-EMPTY (ka-0000004030) — covered by
//     count_field_kind_nudge_test.go and list_field_filter_test.go.
//   - `--created-by`/`--assignee` false-UNDERCOUNT (ka-0000008078) — an actor's
//     non-issue-kind beads (agent/session/message) are hidden by kind=issue, so
//     `kd list --created-by X` shows 1 of the 6 beads X created and the footer's
//     `(1 total)` reads as the whole count.
//
// It also fires in --json / --json-envelope mode (ka-0000005220): the note is
// stderr-only, so stdout stays a clean bare array / envelope, and --json is the
// mode agents script against.
//
// The tests below pin the actor-undercount behavior and the JSON-mode firing;
// the field-filter behavior is unchanged and pinned by its existing suites.

// resetListScopeKindFlags clears every flag these tests mutate so one subtest's
// setting never leaks into the next RunE call within this process. Cobra carries
// the Changed bit and the value across RunE calls in one process.
func resetListScopeKindFlags(t *testing.T) {
	t.Helper()
	clear := func() {
		listCmd.Flags().VisitAll(func(f *pflag.Flag) {
			switch f.Name {
			case "field", "type", "kind", "status", "label":
				f.Changed = false
				if sv, ok := f.Value.(pflag.SliceValue); ok {
					_ = sv.Replace(nil)
				}
			case "created-by", "assignee", "all-types", "all-projects", "project",
				"offset", "json-envelope":
				f.Changed = false
				_ = f.Value.Set(f.DefValue)
			}
		})
		listProjectFlag = ""
		listAllProjectsFlag = false
	}
	clear()
	t.Cleanup(clear)
}

// TestList_ScopeKindNudge_CreatedByUndercount is the ka-0000008078 repro: a
// `--created-by X` default-scoped list SHOWS some issue-kind beads (1 here) but
// the kind=issue clause hid the rest (6 total). The probe (kind cleared) finds
// more than the primary query returned, so the note must name the actor
// predicate, report the --all-types total, and point at --all-types — even
// though the primary answer was non-empty.
func TestList_ScopeKindNudge_CreatedByUndercount(t *testing.T) {
	mc := newMockClient()
	// Call 1 = the list itself (1 shown row). Call 2 = the probe (6 without the
	// implicit kind=issue clause). No project label is injected here (no
	// --project), so there is no disambiguation probe.
	shown := []*model.Bead{{ID: "ka-0000002189", Title: "Iteration", Status: "closed", Type: "task"}}
	six := make([]*model.Bead, 6)
	for i := range six {
		six[i] = &model.Bead{ID: "gd-x", Type: "task"}
	}
	mc.ListBeadsPages = [][]*model.Bead{shown, six}
	withMockClient(t, mc)
	withStdoutTTY(t, false) // keep the TTY-gated operational nudge out of the way

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("created-by", "gardenio-continual-datadensity-improvement-lql"); err != nil {
		t.Fatalf("setting --created-by: %v", err)
	}

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	// The footer (stdout) still shows the in-scope total; the correction rides
	// stderr so scripted stdout consumers are untouched.
	if !strings.Contains(stdout, "1 beads") {
		t.Errorf("stdout footer must show the in-scope count, got %q", stdout)
	}
	if !strings.Contains(stderr, "--created-by gardenio-continual-datadensity-improvement-lql") {
		t.Errorf("note must name the actor predicate verbatim, got %q", stderr)
	}
	if !strings.Contains(stderr, "matched 1 under") {
		t.Errorf("note must report the count the reader actually saw (matched 1), got %q", stderr)
	}
	if !strings.Contains(stderr, "but 6 without") || !strings.Contains(stderr, "--all-types") {
		t.Errorf("note must report the --all-types total (6) and name the flag, got %q", stderr)
	}
	if len(mc.ListBeadsCalls) != 2 {
		t.Fatalf("expected 2 ListBeads calls (list + probe), got %d", len(mc.ListBeadsCalls))
	}
	probe := mc.ListBeadsCalls[1]
	if len(probe.Kind) != 0 || !probe.CountOnly {
		t.Errorf("probe must clear the kind clause and be count-only, got Kind=%v CountOnly=%v", probe.Kind, probe.CountOnly)
	}
	if probe.CreatedBy != "gardenio-continual-datadensity-improvement-lql" {
		t.Errorf("probe must reuse the same actor predicate, got CreatedBy=%q", probe.CreatedBy)
	}
	if _, ok := mc.ListBeadsCtxs[1].Deadline(); !ok {
		t.Error("probe context must carry a deadline so a best-effort hint can never hang the CLI")
	}
}

// TestList_ScopeKindNudge_AssigneeUndercount pins the sibling actor column:
// `--assignee X` gets the same undercount safety net.
func TestList_ScopeKindNudge_AssigneeUndercount(t *testing.T) {
	mc := newMockClient()
	shown := []*model.Bead{{ID: "gd-1", Type: "task"}}
	four := make([]*model.Bead, 4)
	for i := range four {
		four[i] = &model.Bead{ID: "gd-y", Type: "task"}
	}
	mc.ListBeadsPages = [][]*model.Bead{shown, four}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("assignee", "alice"); err != nil {
		t.Fatalf("setting --assignee: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "--assignee alice") {
		t.Errorf("note must name the assignee predicate, got %q", stderr)
	}
	if !strings.Contains(stderr, "but 4 without") || !strings.Contains(stderr, "--all-types") {
		t.Errorf("note must report the --all-types total (4), got %q", stderr)
	}
}

// TestList_ScopeKindNudge_SilentWhenNothingHidden is the no-undercount guard:
// when the probe returns exactly what the primary query returned, the actor
// filter did NOT truncate, so the note must stay quiet — otherwise the hint
// becomes noise on every actor-filtered list.
func TestList_ScopeKindNudge_SilentWhenNothingHidden(t *testing.T) {
	mc := newMockClient()
	three := make([]*model.Bead, 3)
	for i := range three {
		three[i] = &model.Bead{ID: "gd-z", Type: "task"}
	}
	// Primary shows 3; the probe (kind cleared) also finds 3 → nothing hidden.
	mc.ListBeadsPages = [][]*model.Bead{three, three}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("created-by", "alice"); err != nil {
		t.Fatalf("setting --created-by: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if stderr != "" {
		t.Errorf("no undercount (probe == shown) must print no note, got %q", stderr)
	}
}

// TestList_ScopeKindNudge_SilentUnderAllTypes asserts an explicit --all-types
// suppresses the note and the probe: the reader already chose to lift the kind
// scope, so there is no implicit clause to reveal (and nothing to pay a probe
// for).
func TestList_ScopeKindNudge_SilentUnderAllTypes(t *testing.T) {
	mc := newMockClient()
	// The probe WOULD find more — only the explicit scoping keeps it quiet.
	mc.ListBeadsPages = [][]*model.Bead{
		{{ID: "gd-1", Type: "task"}},
		{{ID: "gd-a"}, {ID: "gd-b"}, {ID: "gd-c"}},
	}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("created-by", "alice"); err != nil {
		t.Fatalf("setting --created-by: %v", err)
	}
	if err := listCmd.Flags().Set("all-types", "true"); err != nil {
		t.Fatalf("setting --all-types: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if stderr != "" {
		t.Errorf("--all-types is explicit kind scoping; expected no note, got %q", stderr)
	}
	if len(mc.ListBeadsCalls) != 1 {
		t.Errorf("explicit --all-types must not pay for a probe, got %d calls", len(mc.ListBeadsCalls))
	}
}

// TestList_ScopeKindNudge_CreatedByFiresInJSON is the ka-0000005220 fix for the
// actor variant: `kd list --created-by X --json` must still print the note on
// STDERR (the mode agents script against), while STDOUT stays a clean bare JSON
// array a `2>/dev/null | jq` consumer can parse.
func TestList_ScopeKindNudge_CreatedByFiresInJSON(t *testing.T) {
	mc := newMockClient()
	shown := []*model.Bead{{ID: "ka-0000002189", Title: "Iteration", Status: "closed", Type: "task"}}
	six := make([]*model.Bead, 6)
	for i := range six {
		six[i] = &model.Bead{ID: "gd-x", Type: "task"}
	}
	mc.ListBeadsPages = [][]*model.Bead{shown, six}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("created-by", "lql"); err != nil {
		t.Fatalf("setting --created-by: %v", err)
	}

	origJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = origJSON })

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	// stdout must be a clean bare JSON array — no "note:" prose merged in.
	trimmed := strings.TrimSpace(stdout)
	if !strings.HasPrefix(trimmed, "[") {
		t.Errorf("--json stdout must stay a bare array, got %q", stdout)
	}
	if strings.Contains(stdout, "note:") {
		t.Errorf("the note must never reach stdout under --json, got %q", stdout)
	}
	// stderr must carry the correction that the bare page would otherwise hide.
	if !strings.Contains(stderr, "--created-by lql") || !strings.Contains(stderr, "but 6 without") {
		t.Errorf("--json mode must still emit the scope note on stderr, got %q", stderr)
	}
}

// TestList_ScopeKindNudge_FieldFilterFiresInJSON is the ka-0000005220 repro for
// the field variant: `kd list -f agent=X --json` returned a bare `[]` with an
// empty stderr, hiding the very false-empty the note exists to catch. The note
// must now ride stderr in --json too, leaving stdout a parseable bare array.
func TestList_ScopeKindNudge_FieldFilterFiresInJSON(t *testing.T) {
	mc := newMockClient()
	// Call 1 = the list (0 rows). Call 2 = the probe (rows once kind is dropped).
	mc.ListBeadsPages = [][]*model.Bead{{}, {{ID: "gd-1", Type: "agent"}, {ID: "gd-2", Type: "session"}}}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("field", "agent=lql"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	origJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = origJSON })

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if strings.TrimSpace(stdout) != "[]" {
		t.Errorf("empty --json list must stay a bare [] on stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "-f agent=lql") || !strings.Contains(stderr, "--all-types") {
		t.Errorf("--json mode must emit the field scope note on stderr, got %q", stderr)
	}
}

// TestList_ScopeKindNudge_CreatedByProjectDisambiguation pins that the actor
// nudge inherits the two-clause disambiguation: when the implicit project label
// is ALSO dropped and restoring kind=issue STILL surfaces more than the primary
// query returned, the note blames project scoping and points at --all-projects,
// never "not issue-kind".
func TestList_ScopeKindNudge_CreatedByProjectDisambiguation(t *testing.T) {
	mc := newMockClient()
	// 1: primary (1 shown, project-scoped). 2: both clauses lifted → 3.
	// 3: kind=issue restored, project still off → 3 (> shown 1) → project hid them.
	shown := []*model.Bead{{ID: "gd-1", Type: "task"}}
	three := []*model.Bead{{ID: "gd-a", Type: "task"}, {ID: "gd-b", Type: "task"}, {ID: "gd-c", Type: "task"}}
	mc.ListBeadsPages = [][]*model.Bead{shown, three, three}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	// Set --project (Changed) so the implicit project label is injected and the
	// probe drops it, triggering the disambiguation branch.
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("created-by", "alice"); err != nil {
		t.Fatalf("setting --created-by: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if strings.Contains(stderr, "not issue-kind") {
		t.Errorf("note must not blame kind when the rows are issue-kind in other projects, got %q", stderr)
	}
	if !strings.Contains(stderr, "--all-projects") {
		t.Errorf("note must point at --all-projects, the flag that lifts the real cause, got %q", stderr)
	}
	if !strings.Contains(stderr, "project:gardenio") {
		t.Errorf("note must name the scoping label it dropped, got %q", stderr)
	}
	// Precondition: the primary query really was project-scoped.
	if !slices.Contains(mc.ListBeadsCalls[0].Labels, "project:gardenio") {
		t.Fatalf("precondition: primary list should carry the implicit project label, got %v", mc.ListBeadsCalls[0].Labels)
	}
}

// TestList_ScopeKindNudge_KeepsProjectRemedyWhenKindHidThem is the mirror guard
// for the actor undercount: the project label is dropped by the probe, but
// restoring kind=issue returns exactly what the primary query showed (1),
// proving project scoping hid NOTHING — the kind clause hid the rest. The note
// must keep pointing at --all-types (r.Total must be compared against the shown
// total, not against 0). Regression test for the bare `r.Total > 0` mis-blame.
func TestList_ScopeKindNudge_KeepsProjectRemedyWhenKindHidThem(t *testing.T) {
	mc := newMockClient()
	// 1: primary (1 shown, project-scoped). 2: both clauses lifted → 6.
	// 3: kind=issue restored, project off → 1 (== shown) → project hid nothing.
	shown := []*model.Bead{{ID: "gd-1", Type: "task"}}
	six := make([]*model.Bead, 6)
	for i := range six {
		six[i] = &model.Bead{ID: "gd-x", Type: "task"}
	}
	mc.ListBeadsPages = [][]*model.Bead{shown, six, shown}
	withMockClient(t, mc)
	withStdoutTTY(t, false)

	resetListScopeKindFlags(t)
	if err := listCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := listCmd.Flags().Set("created-by", "alice"); err != nil {
		t.Fatalf("setting --created-by: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := listCmd.RunE(listCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "--all-types") {
		t.Errorf("kind hid the rows (project restored still == shown), note must point at --all-types, got %q", stderr)
	}
	if strings.Contains(stderr, "the matching beads ARE issue-kind") {
		t.Errorf("note must not switch to the --all-projects wording when project scoping hid nothing, got %q", stderr)
	}
	if !strings.Contains(stderr, "but 6 without") {
		t.Errorf("note must report the --all-types total (6), got %q", stderr)
	}
}
