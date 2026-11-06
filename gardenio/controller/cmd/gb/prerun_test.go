package main

// prerun_test.go — unit tests for the per-role prerun subsystem (ka-0000007894).
//
// Coverage:
//   - isReadOnlyPrerunCmd / splitShellSegments: the safety guard that keeps a
//     prerun from ever mutating state (it re-fires on every resume).
//   - runPrerunCommand: execution, byte cap, timeout, fail-open, and the
//     {{.AssignedBead}} substitution.
//   - resolvePrerunEntries: config-bead resolution, layer concat + id de-dup,
//     read-only filtering, and the no-assigned-bead skip.
//   - outputPrerunResults: rendered-section shape, fail-open (no section), and
//     the whole-section byte cap.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestIsReadOnlyPrerunCmd(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"kd show gd-ABC123", true},
		{"gb news", true},
		{"gb news --active-only --ids-only", true},
		{"kd list --status=open", true},
		{"ls ../repos/", true},
		{"git status --short", true},
		// Mutating verbs — must be rejected.
		{"kd create 'x' --type=task", false},
		{"kd claim gd-ABC", false},
		{"kd close gd-ABC", false},
		{"kd update gd-ABC --status=open", false},
		{"gb spawn --task gd-ABC --role crew", false},
		{"gb squawk 'hi'", false},
		{"gb stop", false},
		{"gb decision create --prompt x", false},
		// A read-only lead command must not smuggle a mutating one past a separator.
		{"gb news && kd close gd-ABC", false},
		{"kd show gd-ABC | gb squawk hi", false},
		{"kd show gd-ABC; kd create y", false},
		// Whitespace normalization: "kd   create" still matches "kd create".
		{"kd   create  z", false},
		// A read-only pipeline stays read-only.
		{"kd show gd-ABC | grep title", true},
		{"gb news | head -20", true},
	}
	for _, c := range cases {
		if got := isReadOnlyPrerunCmd(c.cmd); got != c.want {
			t.Errorf("isReadOnlyPrerunCmd(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestRunPrerunCommand_Basic(t *testing.T) {
	out, ok := runPrerunCommand(context.Background(), prerunEntry{ID: "echo", Cmd: "echo hello"}, "")
	if !ok {
		t.Fatal("expected ok for a successful command")
	}
	if out != "hello" {
		t.Errorf("output = %q, want %q", out, "hello")
	}
}

func TestRunPrerunCommand_AssignedBeadSubstitution(t *testing.T) {
	out, ok := runPrerunCommand(context.Background(),
		prerunEntry{ID: "show", Cmd: "echo " + prerunAssignedBeadPlaceholder}, "gd-ZZZ999")
	if !ok {
		t.Fatal("expected ok")
	}
	if out != "gd-ZZZ999" {
		t.Errorf("substitution failed: output = %q, want %q", out, "gd-ZZZ999")
	}
}

func TestRunPrerunCommand_FailOpenOnNonZeroExit(t *testing.T) {
	if _, ok := runPrerunCommand(context.Background(), prerunEntry{ID: "fail", Cmd: "exit 3"}, ""); ok {
		t.Error("expected !ok for a non-zero exit (fail-open)")
	}
}

func TestRunPrerunCommand_FailOpenOnEmptyOutput(t *testing.T) {
	if _, ok := runPrerunCommand(context.Background(), prerunEntry{ID: "empty", Cmd: "true"}, ""); ok {
		t.Error("expected !ok for empty output")
	}
}

func TestRunPrerunCommand_Timeout(t *testing.T) {
	start := time.Now()
	_, ok := runPrerunCommand(context.Background(),
		prerunEntry{ID: "slow", Cmd: "sleep 5", TimeoutMS: 100}, "")
	if ok {
		t.Error("expected !ok for a timed-out command")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout not enforced: took %s", elapsed)
	}
}

func TestRunPrerunCommand_ByteCap(t *testing.T) {
	// Emit ~1000 chars but cap at 100.
	out, ok := runPrerunCommand(context.Background(),
		prerunEntry{ID: "big", Cmd: "printf 'x%.0s' $(seq 1 1000)", MaxBytes: 100}, "")
	if !ok {
		t.Fatal("expected ok")
	}
	if !strings.Contains(out, "prerun output truncated") {
		t.Errorf("expected truncation marker, got %d bytes: %q", len(out), out)
	}
	// The kept prefix must be exactly the cap (plus the marker).
	if !strings.HasPrefix(out, strings.Repeat("x", 100)) {
		t.Errorf("expected 100-char prefix, got %q", out[:min(120, len(out))])
	}
}

// (min is a Go builtin as of go1.21 — no local helper needed.)

// prerunConfigBead builds a budgetBead carrying a prerun config for one label
// scope (e.g. "role:gardener"). entries is marshalled into the {"prerun":[...]}
// description shape the resolver decodes.
func prerunConfigBead(id, label string, entries []map[string]any) budgetBead {
	raw, _ := json.Marshal(map[string]any{"prerun": entries})
	return budgetBead{
		ID: id, Title: "prerun",
		Description: string(raw),
		Type:        "config", Kind: "config", Status: "open",
		Labels: []string{label},
	}
}

func TestResolvePrerunEntries_GardenerRole(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "assigned", "cmd": "kd show {{.AssignedBead}}", "heading": "task"},
			{"id": "news", "cmd": "gb news", "heading": "news"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	entries := resolvePrerunEntries(context.Background(), daemon, "gardener", "gd-TASK1")
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].ID != "assigned" || entries[1].ID != "news" {
		t.Errorf("unexpected order/ids: %q, %q", entries[0].ID, entries[1].ID)
	}
}

func TestResolvePrerunEntries_CrewGetsNothing(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "news", "cmd": "gb news"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	// A crew agent does not subscribe to role:gardener, so the gardener prerun
	// bead must not resolve for it.
	if entries := resolvePrerunEntries(context.Background(), daemon, "crew", "gd-TASK1"); len(entries) != 0 {
		t.Errorf("crew should get no prerun entries, got %d: %+v", len(entries), entries)
	}
}

// TestResolvePrerunEntries_GlobalReachesThreadAndCrew verifies a GLOBAL prerun
// bead (the capability probe, ka-0000003174) resolves for roles that have NO
// per-role prerun of their own — thread and crew — which previously got
// "(no prerun entries for role ...)". This is the delivery half of the
// escalation-awareness fix: the roles that file the most stale escalations must
// actually boot with the probe.
func TestResolvePrerunEntries_GlobalReachesThreadAndCrew(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-global", "global", []map[string]any{
			{"id": "capability-probe", "cmd": "echo probe-output", "heading": "caps"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	for _, role := range []string{"thread", "crew", "gardener"} {
		entries := resolvePrerunEntries(context.Background(), daemon, role, "gd-TASK1")
		if len(entries) != 1 || entries[0].ID != "capability-probe" {
			t.Errorf("role %s: global capability-probe should resolve, got %+v", role, entries)
		}
	}
}

// TestOutputPrerunResults_GlobalCapabilityProbeRenders verifies the global
// capability probe renders into the injected `## Prerun results` section for a
// crew agent (a role with no per-role prerun) — the end-to-end "agent boots and
// sees its capabilities" contract for ka-0000003174.
func TestOutputPrerunResults_GlobalCapabilityProbeRenders(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-global", "global", []map[string]any{
			{"id": "capability-probe", "cmd": "echo you-can-do-X", "heading": "Your capabilities"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_TASK_ID", "gd-TASK1")

	var buf strings.Builder
	outputPrerunResults(&buf, daemon, "crew")
	out := buf.String()

	if !strings.Contains(out, "## Prerun results") {
		t.Errorf("crew should now get a prerun section from the global probe:\n%s", out)
	}
	if !strings.Contains(out, "### Your capabilities") {
		t.Errorf("missing capability heading:\n%s", out)
	}
	if !strings.Contains(out, "you-can-do-X") {
		t.Errorf("capability probe output not rendered:\n%s", out)
	}
}

func TestResolvePrerunEntries_SkipsNonReadOnly(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "safe", "cmd": "gb news"},
			{"id": "danger", "cmd": "kd create 'x' --type=task"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	entries := resolvePrerunEntries(context.Background(), daemon, "gardener", "gd-TASK1")
	if len(entries) != 1 || entries[0].ID != "safe" {
		t.Fatalf("expected only the read-only entry, got %+v", entries)
	}
}

func TestResolvePrerunEntries_SkipsAssignedPlaceholderWhenUnassigned(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "assigned", "cmd": "kd show {{.AssignedBead}}"},
			{"id": "news", "cmd": "gb news"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	// No assigned bead (warm-pool / pre-assignment): the {{.AssignedBead}} entry
	// is skipped, but gb news still resolves.
	entries := resolvePrerunEntries(context.Background(), daemon, "gardener", "")
	if len(entries) != 1 || entries[0].ID != "news" {
		t.Fatalf("expected only the news entry when unassigned, got %+v", entries)
	}
}

func TestResolvePrerunEntries_LayerConcatAndDedup(t *testing.T) {
	// A global layer contributes an entry; a role layer ADDS one and OVERRIDES
	// the global entry by re-declaring its id.
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-global", "global", []map[string]any{
			{"id": "news", "cmd": "gb news OLD"},
			{"id": "globalonly", "cmd": "gb ready"},
		}),
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "news", "cmd": "gb news NEW"},
			{"id": "assigned", "cmd": "kd show {{.AssignedBead}}"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	entries := resolvePrerunEntries(context.Background(), daemon, "gardener", "gd-TASK1")

	byID := map[string]string{}
	for _, e := range entries {
		byID[e.ID] = e.Cmd
	}
	if len(entries) != 3 {
		t.Fatalf("expected 3 deduped entries (news, globalonly, assigned), got %d: %+v", len(entries), entries)
	}
	if byID["news"] != "gb news NEW" {
		t.Errorf("role layer should override global entry by id: news = %q, want %q", byID["news"], "gb news NEW")
	}
	if byID["globalonly"] != "gb ready" {
		t.Errorf("global-only entry should survive concat: %q", byID["globalonly"])
	}
	if _, ok := byID["assigned"]; !ok {
		t.Error("role-only entry should be present")
	}
	// The overridden 'news' entry keeps the global layer's slot (order preserved).
	if entries[0].ID != "news" {
		t.Errorf("overridden entry should keep its original order slot; got first id %q", entries[0].ID)
	}
}

func TestOutputPrerunResults_RendersSection(t *testing.T) {
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "greet", "cmd": "echo hello-from-prerun", "heading": "greeting"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_TASK_ID", "gd-TASK1")

	var buf strings.Builder
	outputPrerunResults(&buf, daemon, "gardener")
	out := buf.String()

	if !strings.Contains(out, "## Prerun results") {
		t.Errorf("missing section header:\n%s", out)
	}
	if !strings.Contains(out, "### greeting") {
		t.Errorf("missing entry heading:\n%s", out)
	}
	if !strings.Contains(out, "hello-from-prerun") {
		t.Errorf("missing command output:\n%s", out)
	}
	if !strings.Contains(out, "You do NOT need to re-run them") {
		t.Errorf("missing the do-not-rerun instruction:\n%s", out)
	}
}

func TestOutputPrerunResults_FailOpenNoSection(t *testing.T) {
	// Every entry fails → no section at all (must not emit an empty header).
	cfg := []budgetBead{
		prerunConfigBead("gd-pr-gardener", "role:gardener", []map[string]any{
			{"id": "boom", "cmd": "exit 1"},
		}),
	}
	cleanup := startBudgetDaemon(t, nil, cfg)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("BOAT_TASK_ID", "gd-TASK1")

	var buf strings.Builder
	outputPrerunResults(&buf, daemon, "gardener")
	if out := buf.String(); out != "" {
		t.Errorf("expected no output when every entry fails, got:\n%s", out)
	}
}

func TestOutputPrerunResults_NoConfigNoSection(t *testing.T) {
	cleanup := startBudgetDaemon(t, nil, nil)
	defer cleanup()

	t.Setenv("BOAT_PROJECT", "gardenio")
	var buf strings.Builder
	outputPrerunResults(&buf, daemon, "crew")
	if out := buf.String(); out != "" {
		t.Errorf("expected no output for a role with no prerun config, got:\n%s", out)
	}
}
