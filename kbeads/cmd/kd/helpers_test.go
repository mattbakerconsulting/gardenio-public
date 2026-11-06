package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// --- hasTargetingLabel ---

func TestHasTargetingLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   bool
	}{
		{"nil labels", nil, false},
		{"empty labels", []string{}, false},
		{"no targeting", []string{"tag:foo", "priority:high"}, false},
		{"project prefix", []string{"project:foo"}, true},
		{"global", []string{"tag:foo", "global"}, true},
		{"role prefix", []string{"role:crew"}, true},
		{"agent prefix", []string{"agent:my-agent"}, true},
		{"rig no longer recognized", []string{"rig:my-rig"}, false},
		{"partial role (too short)", []string{"role:"}, false},
		{"partial agent (too short)", []string{"agent"}, false},
		{"global among others", []string{"project:x", "global", "tag:y"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasTargetingLabel(tt.labels)
			if got != tt.want {
				t.Errorf("hasTargetingLabel(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

// --- filterJackLabels ---

func TestFilterJackLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
		want   int
	}{
		{"nil", nil, 0},
		{"no jack labels", []string{"project:foo", "role:crew"}, 0},
		{"one jack label", []string{"jack:my-jack", "project:foo"}, 1},
		{"multiple jack labels", []string{"jack:a", "jack:b", "other"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterJackLabels(tt.labels)
			if len(got) != tt.want {
				t.Errorf("filterJackLabels returned %d labels, want %d", len(got), tt.want)
			}
			for _, l := range got {
				if !strings.HasPrefix(l, "jack:") {
					t.Errorf("expected jack: prefix, got %q", l)
				}
			}
		})
	}
}

// --- truncate ---

func TestTruncate(t *testing.T) {
	tests := []struct {
		input string
		max   int
		want  string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello world", 8, "hello..."},
		{"abcdefgh", 5, "ab..."},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := truncate(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncate(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

// --- strPtr / intPtr ---

func TestStrPtr(t *testing.T) {
	p := strPtr("hello")
	if *p != "hello" {
		t.Errorf("*strPtr = %q, want hello", *p)
	}
}

func TestIntPtr(t *testing.T) {
	p := intPtr(42)
	if *p != 42 {
		t.Errorf("*intPtr = %d, want 42", *p)
	}
}

// --- agentProject / defaultProject ---

func TestDefaultProject(t *testing.T) {
	t.Run("KD_PROJECT set", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "myproj")
		t.Setenv("BOAT_PROJECT", "other")
		if got := defaultProject(); got != "myproj" {
			t.Errorf("defaultProject() = %q, want myproj", got)
		}
	})
	t.Run("BOAT_PROJECT fallback", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "")
		t.Setenv("BOAT_PROJECT", "boatproj")
		if got := defaultProject(); got != "boatproj" {
			t.Errorf("defaultProject() = %q, want boatproj", got)
		}
	})
	t.Run("both empty", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "")
		t.Setenv("BOAT_PROJECT", "")
		if got := defaultProject(); got != "" {
			t.Errorf("defaultProject() = %q, want empty", got)
		}
	})
}

func TestAgentProject(t *testing.T) {
	t.Run("from KD_PROJECT", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "proj1")
		t.Setenv("BOAT_PROJECT", "")
		t.Setenv("BEADS_AGENT_NAME", "")
		if got := agentProject(); got != "proj1" {
			t.Errorf("agentProject() = %q, want proj1", got)
		}
	})
	t.Run("from BEADS_AGENT_NAME", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "")
		t.Setenv("BOAT_PROJECT", "")
		t.Setenv("BEADS_AGENT_NAME", "gardenio/gb-zeta")
		if got := agentProject(); got != "gardenio" {
			t.Errorf("agentProject() = %q, want gardenio", got)
		}
	})
	t.Run("BEADS_AGENT_NAME no slash", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "")
		t.Setenv("BOAT_PROJECT", "")
		t.Setenv("BEADS_AGENT_NAME", "standalone")
		if got := agentProject(); got != "" {
			t.Errorf("agentProject() = %q, want empty (no slash)", got)
		}
	})
	t.Run("all empty", func(t *testing.T) {
		t.Setenv("KD_PROJECT", "")
		t.Setenv("BOAT_PROJECT", "")
		t.Setenv("BEADS_AGENT_NAME", "")
		if got := agentProject(); got != "" {
			t.Errorf("agentProject() = %q, want empty", got)
		}
	})
}

// --- filterDepsByType ---

func TestFilterDepsByType(t *testing.T) {
	deps := []*model.Dependency{
		{BeadID: "a", DependsOnID: "b", Type: "blocks"},
		{BeadID: "a", DependsOnID: "c", Type: "parent-child"},
		{BeadID: "a", DependsOnID: "d", Type: "related"},
	}

	t.Run("filter blocks", func(t *testing.T) {
		got := filterDepsByType(deps, []string{"blocks"})
		if len(got) != 1 {
			t.Fatalf("expected 1, got %d", len(got))
		}
		if got[0].DependsOnID != "b" {
			t.Errorf("expected dep on b, got %s", got[0].DependsOnID)
		}
	})

	t.Run("filter multiple types", func(t *testing.T) {
		got := filterDepsByType(deps, []string{"blocks", "related"})
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("no match", func(t *testing.T) {
		got := filterDepsByType(deps, []string{"nonexistent"})
		if len(got) != 0 {
			t.Errorf("expected 0, got %d", len(got))
		}
	})
}

// --- printBeadJSON ---

func TestPrintBeadJSON(t *testing.T) {
	bead := &model.Bead{
		ID:    "gd-abc",
		Title: "Test bead",
		Type:  "task",
	}
	out := captureStdout(t, func() {
		printBeadJSON(bead)
	})
	if !strings.Contains(out, `"id": "gd-abc"`) {
		t.Errorf("printBeadJSON output missing id, got:\n%s", out)
	}
	if !strings.Contains(out, `"title": "Test bead"`) {
		t.Errorf("printBeadJSON output missing title, got:\n%s", out)
	}
}

// --- printBeadListJSON ---

func TestPrintBeadListJSON(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	out := captureStdout(t, func() {
		printBeadListJSON(beads)
	})
	if !strings.Contains(out, "gd-1") || !strings.Contains(out, "gd-2") {
		t.Errorf("printBeadListJSON missing bead IDs, got:\n%s", out)
	}
}

// --- printBeadListJSONWithTotal ---

// Under --json (jsonOutput=true) a truncated page stays a bare JSON array on
// stdout with NO stderr hint: the hint on stderr breaks the live agent idiom
// `kd list --json 2>&1 | jq` by feeding jq a non-JSON "note:" line (exit 5).
// Machine-readable output is silent; `kd count` is the scriptable way to learn
// the true total (ka-0000008174 / design ka-0000000863 §4).
func TestPrintBeadListJSONWithTotalTruncatedJSONSilent(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			printBeadListJSONWithTotal(beads, 55550, true)
		})
	})

	// stdout must remain the bare array — parseable and unchanged.
	if !strings.Contains(stdout, "gd-1") || !strings.Contains(stdout, "gd-2") {
		t.Errorf("stdout missing bead IDs, got:\n%s", stdout)
	}
	var arr []map[string]any
	if err := json.Unmarshal([]byte(stdout), &arr); err != nil {
		t.Fatalf("stdout is not a valid JSON array: %v\n%s", err, stdout)
	}
	if len(arr) != 2 {
		t.Errorf("stdout array len = %d, want 2", len(arr))
	}
	// No truncation hint on EITHER stream under --json — that is the fix.
	if strings.Contains(stdout, "returned") || strings.Contains(stdout, "55550") {
		t.Errorf("truncation hint leaked into stdout, got:\n%s", stdout)
	}
	if stderr != "" {
		t.Errorf("under --json the truncation hint must be suppressed, got stderr:\n%s", stderr)
	}
}

// In non-JSON mode (jsonOutput=false) the defensive truncation hint still rides
// stderr when the page is truncated, so a hypothetical non-JSON caller of this
// JSON printer keeps the signal. stdout stays a bare array either way.
func TestPrintBeadListJSONWithTotalTruncatedNonJSONHints(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	var stdout string
	stderr := captureStderr(t, func() {
		stdout = captureStdout(t, func() {
			printBeadListJSONWithTotal(beads, 55550, false)
		})
	})

	if strings.Contains(stdout, "returned") || strings.Contains(stdout, "55550") {
		t.Errorf("truncation hint leaked into stdout, got:\n%s", stdout)
	}
	if !strings.Contains(stderr, "returned 2 of 55550") {
		t.Errorf("stderr missing truncation hint in non-JSON mode, got:\n%s", stderr)
	}
}

// When the result fits (total == len(beads)), no stderr hint is emitted so
// non-truncated pages and `--limit 0` runs stay quiet — regardless of mode.
func TestPrintBeadListJSONWithTotalFits(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-1", Title: "First"},
		{ID: "gd-2", Title: "Second"},
	}
	for _, jsonMode := range []bool{true, false} {
		var stdout string
		stderr := captureStderr(t, func() {
			stdout = captureStdout(t, func() {
				printBeadListJSONWithTotal(beads, 2, jsonMode)
			})
		})
		if !strings.Contains(stdout, "gd-1") || !strings.Contains(stdout, "gd-2") {
			t.Errorf("jsonMode=%v: stdout missing bead IDs, got:\n%s", jsonMode, stdout)
		}
		if stderr != "" {
			t.Errorf("jsonMode=%v: expected no stderr hint when result fits, got:\n%s", jsonMode, stderr)
		}
	}
}

// --- printBeadListTable ---

func TestPrintBeadListTable(t *testing.T) {
	beads := []*model.Bead{
		{ID: "gd-1", Title: "First", Status: "open", Type: "task", Priority: 2},
		{ID: "gd-2", Title: "Second", Status: "closed", Type: "bug", Priority: 1},
	}
	out := captureStdout(t, func() {
		printBeadListTable(beads, 10)
	})
	if !strings.Contains(out, "gd-1") {
		t.Errorf("output missing gd-1, got:\n%s", out)
	}
	if !strings.Contains(out, "2 beads (10 total)") {
		t.Errorf("output missing total count, got:\n%s", out)
	}
}

func TestPrintBeadListTableTruncatesLongTitle(t *testing.T) {
	longTitle := strings.Repeat("x", 60)
	beads := []*model.Bead{
		{ID: "gd-1", Title: longTitle, Status: "open", Type: "task"},
	}
	out := captureStdout(t, func() {
		printBeadListTable(beads, 1)
	})
	if strings.Contains(out, longTitle) {
		t.Error("expected long title to be truncated")
	}
	if !strings.Contains(out, "...") {
		t.Error("expected truncated title to have ellipsis")
	}
}

// --- printBeadTable ---

func TestPrintBeadTable(t *testing.T) {
	now := time.Now()
	bead := &model.Bead{
		ID:          "gd-abc",
		Title:       "Test bead",
		Type:        "task",
		Kind:        "issue",
		Status:      "open",
		Priority:    2,
		Assignee:    "alice",
		Description: "A description",
		Labels:      []string{"project:foo"},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	out := captureStdout(t, func() {
		printBeadTable(bead)
	})
	for _, want := range []string{"gd-abc", "Test bead", "task", "issue", "open", "alice", "A description", "project:foo"} {
		if !strings.Contains(out, want) {
			t.Errorf("printBeadTable output missing %q", want)
		}
	}
}

// --- printConfigJSON ---

func TestPrintConfigJSON(t *testing.T) {
	c := &model.Config{
		Key:       "test.key",
		Value:     []byte(`{"enabled":true}`),
		CreatedAt: time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC),
	}
	out := captureStdout(t, func() {
		printConfigJSON(c)
	})
	if !strings.Contains(out, "test.key") {
		t.Errorf("output missing key, got:\n%s", out)
	}
	if !strings.Contains(out, "enabled") {
		t.Errorf("output missing value, got:\n%s", out)
	}
	if !strings.Contains(out, "2025-06-01") {
		t.Errorf("output missing created_at, got:\n%s", out)
	}
}

// --- printComments ---

func TestPrintCommentsNilEmpty(t *testing.T) {
	out := captureStdout(t, func() {
		printComments(nil)
	})
	if out != "" {
		t.Errorf("expected empty output for nil comments, got %q", out)
	}
}

func TestPrintCommentsOutput(t *testing.T) {
	comments := []*model.Comment{
		{Author: "alice", Text: "looks good", CreatedAt: time.Date(2025, 6, 1, 12, 0, 0, 0, time.UTC)},
		{Author: "bob", Text: "needs work"},
	}
	out := captureStdout(t, func() {
		printComments(comments)
	})
	if !strings.Contains(out, "Comments:") {
		t.Error("expected Comments: header")
	}
	if !strings.Contains(out, "alice: looks good") {
		t.Error("expected alice's comment")
	}
	if !strings.Contains(out, "bob: needs work") {
		t.Error("expected bob's comment")
	}
}

// --- printDepSubSection ---

func TestPrintDepSubSectionEmpty(t *testing.T) {
	out := captureStdout(t, func() {
		printDepSubSection(nil, nil)
	})
	if out != "" {
		t.Errorf("expected empty output for nil deps, got %q", out)
	}
}

func TestPrintDepSubSectionResolved(t *testing.T) {
	deps := []resolvedDep{
		{
			Dep:  &model.Dependency{Type: "blocks", DependsOnID: "gd-target"},
			Bead: &model.Bead{ID: "gd-target", Title: "Target bead", Status: "open"},
		},
	}
	out := captureStdout(t, func() {
		printDepSubSection(deps, []string{"id", "title", "status"})
	})
	if !strings.Contains(out, "blocks:") {
		t.Error("expected blocks: prefix")
	}
	if !strings.Contains(out, "gd-target") {
		t.Error("expected target bead ID")
	}
}

func TestPrintDepSubSectionUnresolved(t *testing.T) {
	deps := []resolvedDep{
		{
			Dep:  &model.Dependency{Type: "parent-child", DependsOnID: "gd-missing"},
			Bead: nil,
		},
	}
	out := captureStdout(t, func() {
		printDepSubSection(deps, nil)
	})
	if !strings.Contains(out, "unresolved") {
		t.Error("expected (unresolved) for nil bead")
	}
	if !strings.Contains(out, "gd-missing") {
		t.Error("expected missing bead ID")
	}
}
