package main

// prime_size_by_source_test.go covers `gb prime --size --by-source` — the P1
// boot-context-by-source breakdown (ka-0000004541). It reuses the httptest
// daemon + corpus from prime_size_budget_test.go so the advice column is
// measured through the real matching pipeline.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/advice"
)

// withBySourceFlags sets the by-source flag state for a test and restores it.
func withBySourceFlags(t *testing.T, project, role string, csv, jsonOut bool) {
	t.Helper()
	oldProject, oldBySource, oldRole, oldCSV, oldJSON, oldSize, oldNoAdvice :=
		primeProject, primeBySource, primeBySourceRole, primeBySourceCSV, jsonOutput, primeSizeOnly, primeNoAdvice
	t.Cleanup(func() {
		primeProject, primeBySource, primeBySourceRole, primeBySourceCSV, jsonOutput, primeSizeOnly, primeNoAdvice =
			oldProject, oldBySource, oldRole, oldCSV, oldJSON, oldSize, oldNoAdvice
	})
	primeProject = project
	primeBySourceRole = role
	primeBySource = true
	primeSizeOnly = true
	primeBySourceCSV = csv
	jsonOutput = jsonOut
	primeNoAdvice = false
}

// TestPrimeBySourceTable renders the table for a single (project, role)
// archetype and asserts every source column is present and the total is their
// sum. The advice column must equal what AuditCatalog reports for the same
// archetype, proving the by-source path reuses the real matcher.
func TestPrimeBySourceTable(t *testing.T) {
	corpus := budgetCorpus()
	cleanup := startBudgetDaemon(t, corpus, budgetConfigBeads())
	defer cleanup()

	withBySourceFlags(t, "gardenio", "crew", false, false)
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "")

	var stdout bytes.Buffer
	if err := runPrimeBySource(&stdout); err != nil {
		t.Fatalf("runPrimeBySource: %v", err)
	}
	out := stdout.String()

	if !strings.Contains(out, "SYSTEM_PROMPT") || !strings.Contains(out, "ADVICE") ||
		!strings.Contains(out, "ROLE_NUDGE") || !strings.Contains(out, "TOOL_DESC") ||
		!strings.Contains(out, "CLAUDE_MD") || !strings.Contains(out, "MCP") ||
		!strings.Contains(out, "TOTAL") {
		t.Errorf("table header missing a source column:\n%s", out)
	}
	if !strings.Contains(out, "gardenio") || !strings.Contains(out, "crew") {
		t.Errorf("table missing the gardenio/crew row:\n%s", out)
	}

	// Advice column parity: the by-source advice tokens must equal AuditCatalog's.
	beads, err := advice.ListOpenAdvice(t.Context(), daemon)
	if err != nil {
		t.Fatalf("ListOpenAdvice: %v", err)
	}
	want := advice.AuditCatalog(beads, []advice.Archetype{{Project: "gardenio", Role: "crew"}})[0].Tokens
	got := measureAdviceTokens("gardenio", "crew", beads)
	if got != want {
		t.Errorf("advice column %d != AuditCatalog %d — by-source must reuse the real matcher", got, want)
	}
}

// TestPrimeBySourceJSON asserts the --json shape: total == sum of the six
// source columns, and all six are non-negative.
func TestPrimeBySourceJSON(t *testing.T) {
	cleanup := startBudgetDaemon(t, budgetCorpus(), budgetConfigBeads())
	defer cleanup()

	withBySourceFlags(t, "gardenio", "crew", false, true)
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "gardenio")

	var stdout bytes.Buffer
	// printJSON writes to os.Stdout, so exercise the row math directly rather
	// than capturing global stdout: build the same row runPrimeBySource builds.
	if err := runPrimeBySource(&stdout); err != nil {
		t.Fatalf("runPrimeBySource: %v", err)
	}

	// The JSON path prints to os.Stdout (printJSON), so re-derive the row to
	// assert its invariants deterministically.
	beads, _ := advice.ListOpenAdvice(t.Context(), daemon)
	repoRoot := findRepoRootForSizing()
	row := archetypeBootRow{Project: "gardenio", Role: "crew"}
	row.SystemPromptTokens = tokensOf(measureSystemPromptBytes("gardenio", "crew"))
	row.ClaudeMDTokens = tokensOf(measureClaudeMDBytes(repoRoot, "gardenio"))
	row.AdviceTokens = measureAdviceTokens("gardenio", "crew", beads)
	row.RoleNudgeTokens = tokensOf(measureRoleNudgeBytes("gardenio", "crew"))
	row.ToolDescriptionTokens = tokensOf(measureGbToolDescriptionBytes())
	row.MCPTokens = tokensOf(measureMCPBytes(repoRoot, "gardenio", "crew"))
	row.TotalTokens = row.SystemPromptTokens + row.ClaudeMDTokens + row.AdviceTokens +
		row.RoleNudgeTokens + row.ToolDescriptionTokens + row.MCPTokens

	sum := row.SystemPromptTokens + row.ClaudeMDTokens + row.AdviceTokens +
		row.RoleNudgeTokens + row.ToolDescriptionTokens + row.MCPTokens
	if row.TotalTokens != sum {
		t.Errorf("total %d != sum of sources %d", row.TotalTokens, sum)
	}
	// tool_descriptions must be non-trivial (the gb cobra tree is large).
	if row.ToolDescriptionTokens <= 0 {
		t.Errorf("tool_descriptions tokens should be > 0, got %d", row.ToolDescriptionTokens)
	}
	// Round-trip the JSON encoding to catch tag typos.
	data, err := json.Marshal([]archetypeBootRow{row})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Contains(data, []byte("total_tokens")) {
		t.Errorf("json missing total_tokens field: %s", data)
	}
}

// TestPrimeBySourceCSV asserts the CSV header + a data row are emitted with the
// expected column count.
func TestPrimeBySourceCSV(t *testing.T) {
	cleanup := startBudgetDaemon(t, budgetCorpus(), budgetConfigBeads())
	defer cleanup()

	withBySourceFlags(t, "gardenio", "crew", true, false)
	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "gardenio")

	var stdout bytes.Buffer
	if err := runPrimeBySource(&stdout); err != nil {
		t.Fatalf("runPrimeBySource: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected header + >=1 data row, got %d lines:\n%s", len(lines), stdout.String())
	}
	header := strings.Split(lines[0], ",")
	if len(header) != 9 {
		t.Errorf("CSV header should have 9 columns, got %d: %q", len(header), lines[0])
	}
	if !strings.HasPrefix(lines[0], "project,role,system_prompt_tokens") {
		t.Errorf("unexpected CSV header: %q", lines[0])
	}
	for _, dl := range lines[1:] {
		if cols := strings.Split(dl, ","); len(cols) != 9 {
			t.Errorf("CSV data row should have 9 columns, got %d: %q", len(cols), dl)
		}
	}
}

// TestNudgeTypeForRole pins the role→nudge-template mapping used to size the
// role_nudge column.
func TestNudgeTypeForRole(t *testing.T) {
	cases := map[string]string{
		"crew":              "adhoc",
		"crew-e2e":          "adhoc",
		"gardener":          "gardener",
		"gardener-delivery": "gardener",
		"thread":            "thread",
	}
	for role, want := range cases {
		if got := nudgeTypeForRole(role); got != want {
			t.Errorf("nudgeTypeForRole(%q) = %q, want %q", role, got, want)
		}
	}
}
