package main

import (
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The hidden-kind field nudge (maybePrintFieldKindNudge, list_nudges.go) closes the
// silent-zero footgun ka-0000004030: `kd count --all-projects -f spawned_by=*`
// printed a confident bare `0` while the same predicate under --all-types
// reported 40k+, because spawned_by lives only on kind=config agent beads and
// the default issue-kind injection hid every one of them. Zero was
// indistinguishable from "no such beads".
//
// The tests below pin the four properties that make the note trustworthy:
//   - it fires only when the answer is PROVABLY misleading (0 here, >0 without
//     the implicit kind clause), and reports the --all-types total;
//   - it stays silent when the predicate is genuinely empty, when the reader
//     scoped the kind themselves, and when no -f was given at all;
//   - the probe drops the IMPLICIT project label (which --all-types also drops)
//     but PRESERVES explicit -l labels, so the note cannot over-promise rows
//     the suggested command would not surface;
//   - stdout stays a bare integer — the note is stderr-only, so
//     `N=$(kd count …)` is byte-identical.

// oneProbeRow returns a single matching row, used to make a probe response
// non-empty. A function (not a shared package-level slice) so no test can mutate
// the fixture out from under another.
func oneProbeRow() []*model.Bead {
	return []*model.Bead{{ID: "gd-1", Title: "a", Status: "open", Type: "task"}}
}

// TestCount_FieldKindNudge_FiresOnHiddenKindZero is the ka-0000004030 repro:
// `-f spawned_by=*` counts 0 under the implicit issue-kind filter, the probe
// (kind cleared) finds rows, so a stderr note names the predicate, reports the
// --all-types total, and points at the flag that fixes it.
func TestCount_FieldKindNudge_FiresOnHiddenKindZero(t *testing.T) {
	mc := newMockClient()
	// Call 1 = the count itself (0 rows). Call 2 = the nudge probe (rows exist
	// once the implicit kind=issue clause is dropped).
	mc.ListBeadsPages = [][]*model.Bead{{}, oneProbeRow()}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("all-projects", "true"); err != nil {
		t.Fatalf("setting --all-projects: %v", err)
	}
	if err := countCmd.Flags().Set("field", "spawned_by=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	// stdout contract: a bare integer, so N=$(kd count …) is unaffected.
	if strings.TrimSpace(stdout) != "0" {
		t.Errorf("stdout = %q, want a bare %q — the note must not reach stdout", stdout, "0")
	}
	if !strings.Contains(stderr, "spawned_by=*") {
		t.Errorf("stderr must name the predicate verbatim so it is actionable, got %q", stderr)
	}
	if !strings.Contains(stderr, "--all-types") {
		t.Errorf("stderr must point at --all-types, got %q", stderr)
	}
	if !strings.Contains(stderr, "1") {
		t.Errorf("stderr must report the --all-types total (1), got %q", stderr)
	}

	// The probe must re-ask the SAME predicate with only the kind clause dropped.
	if len(mc.ListBeadsCalls) != 2 {
		t.Fatalf("expected 2 ListBeads calls (count + probe), got %d", len(mc.ListBeadsCalls))
	}
	probe := mc.ListBeadsCalls[1]
	if len(probe.Kind) != 0 {
		t.Errorf("probe must clear the kind filter, got Kind=%v", probe.Kind)
	}
	if probe.FieldFilters["spawned_by"] != "*" {
		t.Errorf("probe must reuse the same field predicate, got %v", probe.FieldFilters)
	}
	if !probe.CountOnly {
		t.Error("probe must be count-only — it needs a total, not bead bodies")
	}

	// The probe MUST carry a deadline. Nothing else bounds it: the HTTP client
	// sets no Timeout and cobra's root context has none, so an unbounded probe
	// could hang the CLI indefinitely — after stdout had already printed the
	// answer, which reads as a wedged command. The primary call is deliberately
	// NOT required to have one; only the discardable diagnostic is.
	if len(mc.ListBeadsCtxs) != 2 {
		t.Fatalf("expected 2 captured contexts, got %d", len(mc.ListBeadsCtxs))
	}
	if _, ok := mc.ListBeadsCtxs[1].Deadline(); !ok {
		t.Error("probe context must carry a deadline so a best-effort hint can never hang the CLI")
	}
}

// TestCount_FieldKindNudge_SilentWhenGenuinelyEmpty asserts the note does NOT
// fire when the predicate matches nothing anywhere: a truthful 0 must stay
// quiet, or the hint becomes noise that trains readers to ignore it.
func TestCount_FieldKindNudge_SilentWhenGenuinelyEmpty(t *testing.T) {
	mc := newMockClient()
	// Both the count and the probe come back empty.
	mc.ListBeadsPages = [][]*model.Bead{{}, {}}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("all-projects", "true"); err != nil {
		t.Fatalf("setting --all-projects: %v", err)
	}
	if err := countCmd.Flags().Set("field", "nosuchfield=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if stderr != "" {
		t.Errorf("a genuinely-empty predicate must print no note, got %q", stderr)
	}
}

// TestCount_FieldKindNudge_SilentWhenKindExplicit asserts an explicit
// --all-types / -k / -t suppresses the note: the reader already chose the kind
// scope, so there is no implicit clause to reveal.
func TestCount_FieldKindNudge_SilentWhenKindExplicit(t *testing.T) {
	for _, tc := range []struct{ flag, value string }{
		{"all-types", "true"},
		{"kind", "agent"},
		{"type", "bug"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			mc := newMockClient()
			// Probe WOULD find rows — only the explicit scoping keeps it quiet.
			mc.ListBeadsPages = [][]*model.Bead{{}, oneProbeRow()}
			withMockClient(t, mc)

			resetCountFlags(t)
			if err := countCmd.Flags().Set("all-projects", "true"); err != nil {
				t.Fatalf("setting --all-projects: %v", err)
			}
			if err := countCmd.Flags().Set("field", "spawned_by=*"); err != nil {
				t.Fatalf("setting --field: %v", err)
			}
			if err := countCmd.Flags().Set(tc.flag, tc.value); err != nil {
				t.Fatalf("setting --%s: %v", tc.flag, err)
			}

			stderr := captureStderr(t, func() {
				captureStdout(t, func() {
					if err := countCmd.RunE(countCmd, nil); err != nil {
						t.Fatalf("RunE: %v", err)
					}
				})
			})

			if stderr != "" {
				t.Errorf("--%s is explicit kind scoping; expected no note, got %q", tc.flag, stderr)
			}
			if len(mc.ListBeadsCalls) != 1 {
				t.Errorf("explicit kind scoping must not pay for a probe, got %d calls", len(mc.ListBeadsCalls))
			}
		})
	}
}

// TestCount_FieldKindNudge_SilentWithoutFieldFilter asserts a plain zero-result
// count (no -f at all) neither prints the note nor pays for a probe — the
// footgun is specific to field predicates.
func TestCount_FieldKindNudge_SilentWithoutFieldFilter(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{}, oneProbeRow()}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("all-projects", "true"); err != nil {
		t.Fatalf("setting --all-projects: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if stderr != "" {
		t.Errorf("no -f predicate means no note, got %q", stderr)
	}
	if len(mc.ListBeadsCalls) != 1 {
		t.Errorf("a count with no -f must not probe, got %d calls", len(mc.ListBeadsCalls))
	}
}

// TestCount_FieldKindNudge_ProbeDropsImplicitProjectKeepsExplicitLabel pins the
// probe's label handling. --all-types suppresses the implicit project label
// (agent/config beads may carry none), so the probe must drop it too — keeping
// it would make the probe itself return 0 and swallow the hint. An explicit
// -l label, by contrast, is preserved: the suggested `--all-types` command
// would still apply it, so the reported total must not include rows that
// command would not surface.
func TestCount_FieldKindNudge_ProbeDropsImplicitProjectKeepsExplicitLabel(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{}, oneProbeRow()}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := countCmd.Flags().Set("label", "kind:ced-audit"); err != nil {
		t.Fatalf("setting --label: %v", err)
	}
	if err := countCmd.Flags().Set("field", "spawned_by=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	captureStderr(t, func() {
		captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	// 3 calls: the count, the probe, and — because the probe lifted the project
	// label as well as the kind clause — one disambiguation probe (kind restored)
	// so the note can tell "not issue-kind" from "issue-kind, other project".
	if len(mc.ListBeadsCalls) != 3 {
		t.Fatalf("expected 3 ListBeads calls (count + probe + kind-disambiguation), got %d",
			len(mc.ListBeadsCalls))
	}
	// Sanity: the primary count DID carry the implicit project label.
	if !slices.Contains(mc.ListBeadsCalls[0].Labels, "project:gardenio") {
		t.Fatalf("precondition: primary count should be project-scoped, got Labels=%v",
			mc.ListBeadsCalls[0].Labels)
	}
	probe := mc.ListBeadsCalls[1]
	if slices.Contains(probe.Labels, "project:gardenio") {
		t.Errorf("probe must drop the implicit project label (--all-types does), got Labels=%v", probe.Labels)
	}
	if !slices.Contains(probe.Labels, "kind:ced-audit") {
		t.Errorf("probe must keep explicit -l labels so the total cannot over-promise, got Labels=%v", probe.Labels)
	}
	// The disambiguation probe restores the kind clause while keeping the label
	// dropped — that is the only pair that isolates which clause hid the rows.
	disambig := mc.ListBeadsCalls[2]
	if !slices.Contains(disambig.Kind, "issue") {
		t.Errorf("disambiguation probe must restore kind=issue, got Kind=%v", disambig.Kind)
	}
	if slices.Contains(disambig.Labels, "project:gardenio") {
		t.Errorf("disambiguation probe must keep the project label dropped, got Labels=%v", disambig.Labels)
	}
}

// TestCount_FieldKindNudge_DoesNotBlameKindWhenProjectScopingHidTheRows is the
// mis-diagnosis guard. The probe lifts TWO clauses (kind=issue AND the injected
// project label), so attributing the whole delta to kind is an outright lie when
// the rows are ordinary issue-kind beads in a DIFFERENT project — observed on
// prod: `kd count --project gardenio -f jira_status=*` returns 0 while 2221 beads
// carry that field and every one is type=task, kind=issue. Here the
// disambiguation probe (call 3, kind restored) finds rows, so the note must blame
// project scoping and point at --all-projects, never say "not issue-kind".
func TestCount_FieldKindNudge_DoesNotBlameKindWhenProjectScopingHidTheRows(t *testing.T) {
	mc := newMockClient()
	// 1: the count (0). 2: probe, both clauses lifted (rows). 3: disambiguation,
	// kind=issue restored and STILL rows → the project label was the real cause.
	mc.ListBeadsPages = [][]*model.Bead{{}, oneProbeRow(), oneProbeRow()}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := countCmd.Flags().Set("field", "jira_status=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if strings.Contains(stderr, "not issue-kind") {
		t.Errorf("note must NOT blame kind when the rows are issue-kind in other projects, got %q", stderr)
	}
	if !strings.Contains(stderr, "--all-projects") {
		t.Errorf("note must point at --all-projects, the flag that actually lifts the cause, got %q", stderr)
	}
	if !strings.Contains(stderr, "project:gardenio") {
		t.Errorf("note must name the scoping label it dropped, got %q", stderr)
	}
}

// TestCount_FieldKindNudge_ReportsTheTotalItsRemedyReturns guards an
// over-promise: the number in the note must be the count the SUGGESTED command
// returns, not whatever the widest probe found. The both-clauses-lifted probe
// (kind AND project label) is necessarily >= the issue-kind-only one, so pairing
// its total with an --all-projects remedy inflates the promise. Observed on prod
// before the fix: `-f jira_project=*` under --project gardenio promised 1733 while
// `--all-projects -f jira_project=*` returns 1730 — three of the rows are
// non-issue-kind, so --all-projects alone never surfaces them.
func TestCount_FieldKindNudge_ReportsTheTotalItsRemedyReturns(t *testing.T) {
	mc := newMockClient()
	// 1: the count (0). 2: both clauses lifted → 3 rows. 3: kind restored → 2
	// rows, i.e. what --all-projects actually returns. The note must say 2.
	mc.ListBeadsPages = [][]*model.Bead{
		{},
		{{ID: "gd-a", Type: "task"}, {ID: "gd-b", Type: "task"}, {ID: "gd-c", Type: "agent"}},
		{{ID: "gd-a", Type: "task"}, {ID: "gd-b", Type: "task"}},
	}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := countCmd.Flags().Set("field", "jira_project=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	stderr := captureStderr(t, func() {
		captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if !strings.Contains(stderr, "--all-projects") {
		t.Fatalf("precondition: expected the project-scoping remedy, got %q", stderr)
	}
	if !strings.Contains(stderr, "but 2 without") {
		t.Errorf("note must report 2 (what --all-projects returns), not 3 (the wider probe); got %q", stderr)
	}
}

// TestCount_FieldKindNudge_DropsOnlyOneInjectedProjectLabel guards the
// over-promise the docstring forbids. buildListRequest appends the injected label
// and THEN the caller's -l flags, so `--project X -l project:X` carries it twice;
// removing every match would strip the reader's own label and make the probe
// count rows the suggested command could never return.
func TestCount_FieldKindNudge_DropsOnlyOneInjectedProjectLabel(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{}, oneProbeRow(), {}}
	withMockClient(t, mc)

	resetCountFlags(t)
	if err := countCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := countCmd.Flags().Set("label", "project:gardenio"); err != nil {
		t.Fatalf("setting --label: %v", err)
	}
	if err := countCmd.Flags().Set("field", "spawned_by=*"); err != nil {
		t.Fatalf("setting --field: %v", err)
	}

	captureStderr(t, func() {
		captureStdout(t, func() {
			if err := countCmd.RunE(countCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
	})

	if len(mc.ListBeadsCalls) < 2 {
		t.Fatalf("expected at least 2 ListBeads calls, got %d", len(mc.ListBeadsCalls))
	}
	// Precondition: the label really was duplicated on the primary request.
	if got := countLabel(mc.ListBeadsCalls[0].Labels, "project:gardenio"); got != 2 {
		t.Fatalf("precondition: primary should carry the label twice (injected + explicit), got %d in %v",
			got, mc.ListBeadsCalls[0].Labels)
	}
	if got := countLabel(mc.ListBeadsCalls[1].Labels, "project:gardenio"); got != 1 {
		t.Errorf("probe must drop exactly ONE occurrence, keeping the reader's explicit -l; got %d in %v",
			got, mc.ListBeadsCalls[1].Labels)
	}
}

func countLabel(labels []string, want string) int {
	n := 0
	for _, l := range labels {
		if l == want {
			n++
		}
	}
	return n
}
