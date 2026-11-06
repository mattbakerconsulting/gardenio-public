package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// This file pins the three E2E config-instructions failure modes that
// repeatedly blocked golden release promotion (bug gd-9MN6ADguHq, epic
// ka-0000003885, task gd-zga44XF0tz):
//
//  1. "test tag"  — a role-scoped claude_md's content tag reaches CLAUDE.md
//  2. "header"    — a role-scoped claude_md's markdown header reaches CLAUDE.md
//  3. "empty claude_md handling" — an empty-string claude_md at role scope
//     both wins the merge over a leaked non-empty global AND suppresses the
//     CLAUDE.md write.
//
// The recurring root cause was a leaked *global* claude-instructions bead in
// the shared e2e daemon (from sibling config-* modules) contaminating merge
// resolution: scenarios 2/13 originally created global beads, so the leaked
// global and the test's own bead were both global and the merge tiebreak was
// nondeterministic. MR !1160 role-scoped the scenarios so the test bead wins
// on specificity (role 2: beats global 0:), regardless of any leaked global.
//
// These are product-layer (deterministic, daemon-free) regressions for the
// exact resolve→write chain `gb setup claude` runs — they can never flake on
// shared-daemon contamination the way the E2E module can, so they catch a
// reintroduced merge/write regression on every unit-test run rather than only
// on a release-tag E2E pipeline.

// resolveAndWrite mirrors runSetupClaude's claude-instructions leg: resolve the
// merged config for the given subscriptions, then materialize CLAUDE.md via
// writeInstructionFiles. Returns the CLAUDE.md path (which may or may not exist).
func resolveAndWrite(t *testing.T, lister configBeadLister, subs []string, workspace string) string {
	t.Helper()
	merged, _, err := ResolveConfigBeads(context.Background(), lister, "claude-instructions", subs)
	if err != nil {
		t.Fatalf("ResolveConfigBeads: %v", err)
	}
	if merged != nil {
		writeInstructionFiles(workspace, merged, false)
	}
	return filepath.Join(workspace, "CLAUDE.md")
}

// TestConfigInstructions_RoleClaudeMDBeatsLeakedGlobal pins E2E scenarios 1/2:
// a role-scoped claude_md must win the merge over a leaked *global*
// claude-instructions bead, and its exact content — markdown header AND content
// tag — must land verbatim in CLAUDE.md. Reproduces the "test tag" + "header"
// assertions of test-config-instructions.sh scenario 2.
func TestConfigInstructions_RoleClaudeMDBeatsLeakedGlobal(t *testing.T) {
	const (
		e2eRole = "e2e-instr-role-42"
		header  = "# E2E Test CLAUDE.md"
		tag     = "e2e-instr-12345-content-tag"
	)
	// A leaked global bead from a sibling config-* module, exactly the shape
	// that historically contaminated the merge.
	leakedGlobal := &beadsapi.BeadDetail{
		Title:       "claude-instructions",
		Labels:      []string{"global"},
		Description: `{"claude_md":"# LEAKED GLOBAL SHOULD NOT WIN"}`,
	}
	roleBead := &beadsapi.BeadDetail{
		Title:       "claude-instructions",
		Labels:      []string{"role:" + e2eRole},
		Description: `{"claude_md":"` + header + `\nContent tag: ` + tag + `"}`,
	}
	// Deliberately list the leaked global FIRST to prove ordering is by
	// specificity, not input order.
	lister := &mockConfigBeadLister{beads: []*beadsapi.BeadDetail{leakedGlobal, roleBead}}

	subs := []string{"global", "role:" + e2eRole, "project:gardenio", "agent:gardenio-x"}
	workspace := t.TempDir()
	path := resolveAndWrite(t, lister, subs, workspace)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected CLAUDE.md written: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, header) {
		t.Errorf("CLAUDE.md missing role header %q; got: %q", header, got)
	}
	if !strings.Contains(got, tag) {
		t.Errorf("CLAUDE.md missing role content tag %q; got: %q", tag, got)
	}
	if strings.Contains(got, "LEAKED GLOBAL") {
		t.Errorf("leaked global claude_md won the merge; got: %q", got)
	}
}

// TestConfigInstructions_EmptyClaudeMDSuppressesWrite pins E2E scenario 13
// ("empty claude_md handling"): an empty-string claude_md at role scope must
// (a) win the MergeOverride over a leaked non-empty global claude_md, and
// (b) suppress the CLAUDE.md write entirely — writeInstructionFiles only writes
// when the resolved claude_md is a non-empty string.
func TestConfigInstructions_EmptyClaudeMDSuppressesWrite(t *testing.T) {
	const e2eRole = "e2e-instr-role-13"
	leakedGlobal := &beadsapi.BeadDetail{
		Title:       "claude-instructions",
		Labels:      []string{"global"},
		Description: `{"claude_md":"# LEAKED GLOBAL NON-EMPTY"}`,
	}
	emptyRoleBead := &beadsapi.BeadDetail{
		Title:       "claude-instructions",
		Labels:      []string{"role:" + e2eRole},
		Description: `{"claude_md":""}`,
	}
	lister := &mockConfigBeadLister{beads: []*beadsapi.BeadDetail{leakedGlobal, emptyRoleBead}}

	// First assert the merge itself: empty role value overrides the leaked
	// global (MergeOverride, role sorts last → wins).
	subs := []string{"global", "role:" + e2eRole, "project:gardenio", "agent:gardenio-x"}
	merged, count, err := ResolveConfigBeads(context.Background(), lister, "claude-instructions", subs)
	if err != nil {
		t.Fatalf("ResolveConfigBeads: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 layers (global + role), got %d", count)
	}
	if cm, ok := merged["claude_md"].(string); !ok || cm != "" {
		t.Fatalf("expected empty-string claude_md to win the merge, got %q (ok=%v)", cm, ok)
	}

	// Then assert the write is suppressed for the empty resolved value.
	workspace := t.TempDir()
	writeInstructionFiles(workspace, merged, false)
	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("expected no CLAUDE.md for empty claude_md, stat err=%v", err)
	}
}

// TestWriteInstructionFiles_EmptyClaudeMDNoFile is the narrow write-layer unit
// for failure mode 3: an explicit empty-string claude_md value must not create
// CLAUDE.md. Complements TestWriteInstructionFiles_EmptyConfig (key absent)
// with the key-present-but-empty case the E2E scenario 13 actually exercises.
func TestWriteInstructionFiles_EmptyClaudeMDNoFile(t *testing.T) {
	workspace := t.TempDir()
	writeInstructionFiles(workspace, map[string]any{"claude_md": ""}, false)
	if _, err := os.Stat(filepath.Join(workspace, "CLAUDE.md")); !os.IsNotExist(err) {
		t.Errorf("expected no CLAUDE.md for empty-string claude_md, stat err=%v", err)
	}
}
