package main

// prime_size_by_source.go implements `gb prime --size --by-source` — the P1
// "Snapshot boot-context by source" breakdown for the Opus/Fable 5 context
// audit (epic ka-0000005473, task ka-0000004541).
//
// Where the plain `gb prime --size` line reports ONE number (the rendered
// prime body for the current agent), --by-source decomposes the boot context
// into the six injection surfaces the audit rubric (bundle ka-0000000454)
// tracks, and does it for EACH (project, role) archetype — not just the
// current agent — so the operator sees the whole grid in one shot:
//
//	system_prompt       gb-authored durable system-prompt subset
//	                    (`gb prime --system-prompt`, advice excluded)
//	claude_md           the repo CLAUDE.md files an agent in that project reads
//	advice              matched advice catalog for the archetype (= gb advice audit)
//	role_nudge          the spawn nudge template resolved for the archetype
//	tool_descriptions   the gb CLI cobra help surface (Short/Long/flags)
//	mcp                 the resolved MCP config (.mcp.json / claude-mcp beads)
//
// All columns are reported in approximate tokens (bytes / advice.TokenBytesRatio,
// the same 4-bytes/token convention gb advice audit uses).
//
// Honest-measurement caveats (also printed in the table footer):
//   - system_prompt measures only the gb-INJECTED slice. The base Claude Code
//     harness system prompt is provider-side and NOT visible to gb.
//   - tool_descriptions measures the gb cobra help tree. kd is a sibling binary
//     of comparable size; the harness's own tool schemas (Bash/Read/Edit/…) are
//     provider-side.
//   - mcp measures the MCP *config* (which servers connect), not the tool-schema
//     blob those servers serve into context at runtime — that is assembled by
//     the coop/Claude process and gb structurally cannot see it (same limitation
//     documented on gardenio_prime_hook_bytes in prime_metrics_push.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// archetypeBootRow is the per-source token breakdown for one (project, role)
// archetype. All *Tokens fields are approximate tokens (bytes/TokenBytesRatio).
type archetypeBootRow struct {
	Project               string `json:"project"`
	Role                  string `json:"role"`
	SystemPromptTokens    int    `json:"system_prompt_tokens"`
	ClaudeMDTokens        int    `json:"claude_md_tokens"`
	AdviceTokens          int    `json:"advice_tokens"`
	RoleNudgeTokens       int    `json:"role_nudge_tokens"`
	ToolDescriptionTokens int    `json:"tool_descriptions_tokens"`
	MCPTokens             int    `json:"mcp_tokens"`
	TotalTokens           int    `json:"total_tokens"`
}

// tokensOf converts a byte count to approximate tokens using the shared
// 4-bytes/token convention (advice.TokenBytesRatio).
func tokensOf(nbytes int) int { return nbytes / advice.TokenBytesRatio }

// runPrimeBySource renders the per-archetype boot-context breakdown. It does
// NOT depend on a live agent identity — like `gb advice audit`, it derives the
// archetype grid from the advice catalog (filterable by --project/--role) so an
// operator can run it ad hoc.
func runPrimeBySource(stdout io.Writer) error {
	ctx := context.Background()

	beads, err := advice.ListOpenAdvice(ctx, daemon)
	if err != nil {
		return fmt.Errorf("listing advice: %w", err)
	}

	// Derive the archetype grid from the real fleet (agent beads), not the
	// advice-corpus cross-product (ka-0000002153).
	archetypes, err := advice.ListFleetArchetypes(ctx, daemon, time.Now())
	if err != nil {
		return fmt.Errorf("deriving fleet archetypes: %w", err)
	}
	// Narrow with the existing --project flag and the by-source --role flag.
	if primeProject != "" || primeBySourceRole != "" {
		filtered := archetypes[:0]
		for _, a := range archetypes {
			if primeProject != "" && a.Project != primeProject {
				continue
			}
			if primeBySourceRole != "" && a.Role != primeBySourceRole {
				continue
			}
			filtered = append(filtered, a)
		}
		archetypes = filtered
		// Both flags name an exact archetype the fleet may not have run yet.
		if len(archetypes) == 0 && primeProject != "" && primeBySourceRole != "" {
			archetypes = []advice.Archetype{{Project: primeProject, Role: primeBySourceRole}}
		}
	}
	if len(archetypes) == 0 {
		return fmt.Errorf("no archetypes to measure (no agent beads in the last %s; use --project and --role to name one)", advice.FleetArchetypeWindow)
	}

	// Sources that don't vary by archetype (or vary only by project) are
	// measured once and reused, so a full grid stays cheap.
	repoRoot := findRepoRootForSizing()
	toolDescTokens := tokensOf(measureGbToolDescriptionBytes())

	rows := make([]archetypeBootRow, 0, len(archetypes))
	for _, a := range archetypes {
		row := archetypeBootRow{Project: a.Project, Role: a.Role}
		row.SystemPromptTokens = tokensOf(measureSystemPromptBytes(a.Project, a.Role))
		row.ClaudeMDTokens = tokensOf(measureClaudeMDBytes(repoRoot, a.Project))
		row.AdviceTokens = measureAdviceTokens(a.Project, a.Role, beads)
		row.RoleNudgeTokens = tokensOf(measureRoleNudgeBytes(a.Project, a.Role))
		row.ToolDescriptionTokens = toolDescTokens
		row.MCPTokens = tokensOf(measureMCPBytes(repoRoot, a.Project, a.Role))
		row.TotalTokens = row.SystemPromptTokens + row.ClaudeMDTokens + row.AdviceTokens +
			row.RoleNudgeTokens + row.ToolDescriptionTokens + row.MCPTokens
		rows = append(rows, row)
	}

	// Heaviest boot first — the archetype an operator most needs to see.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TotalTokens > rows[j].TotalTokens })

	if jsonOutput {
		printJSON(rows)
		return nil
	}
	if primeBySourceCSV {
		printPrimeBySourceCSV(stdout, rows)
		return nil
	}
	printPrimeBySourceTable(stdout, rows)
	return nil
}

// printPrimeBySourceTable renders the aligned token table + a measurement-basis
// footer so the numbers are never read without their caveats.
func printPrimeBySourceTable(stdout io.Writer, rows []archetypeBootRow) {
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PROJECT\tROLE\tSYSTEM_PROMPT\tCLAUDE_MD\tADVICE\tROLE_NUDGE\tTOOL_DESC\tMCP\tTOTAL")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\t%d\n",
			r.Project, r.Role, r.SystemPromptTokens, r.ClaudeMDTokens, r.AdviceTokens,
			r.RoleNudgeTokens, r.ToolDescriptionTokens, r.MCPTokens, r.TotalTokens)
	}
	w.Flush()

	// No cross-archetype total row: tool_descriptions/claude_md are
	// archetype-invariant, so summing them down the column just multiplies a
	// constant and no single agent boots every role. Each row IS the per-agent
	// boot total for that archetype.
	fmt.Fprintf(stdout, "\n%d archetype(s); tokens ≈ bytes/%d.\n", len(rows), advice.TokenBytesRatio)
	fmt.Fprintln(stdout, "Sources: system_prompt = gb-injected `gb prime --system-prompt` subset (advice excluded);")
	fmt.Fprintln(stdout, "  claude_md = repo CLAUDE.md files read for the project; advice = matched catalog (= gb advice audit);")
	fmt.Fprintln(stdout, "  role_nudge = resolved spawn-nudge template; tool_descriptions = gb cobra help tree; mcp = MCP config.")
	fmt.Fprintln(stdout, "Not counted (provider-side, not visible to gb): the base Claude Code harness system prompt,")
	fmt.Fprintln(stdout, "  the harness tool schemas (Bash/Read/Edit/…), skills, and the MCP tool-schema blob servers serve at runtime.")
}

// printPrimeBySourceCSV renders the same data as RFC-4180-ish CSV for
// spreadsheet ingestion (the task allows "CSV or table"; --csv opts into CSV).
func printPrimeBySourceCSV(stdout io.Writer, rows []archetypeBootRow) {
	fmt.Fprintln(stdout, "project,role,system_prompt_tokens,claude_md_tokens,advice_tokens,role_nudge_tokens,tool_descriptions_tokens,mcp_tokens,total_tokens")
	for _, r := range rows {
		fmt.Fprintf(stdout, "%s,%s,%d,%d,%d,%d,%d,%d,%d\n",
			r.Project, r.Role, r.SystemPromptTokens, r.ClaudeMDTokens, r.AdviceTokens,
			r.RoleNudgeTokens, r.ToolDescriptionTokens, r.MCPTokens, r.TotalTokens)
	}
}

// ── per-source measurers ────────────────────────────────────────────────────

// measureSystemPromptBytes renders the gb-injected system-prompt subset for the
// archetype (identity + workflow context + wrap-up + memory pointer) with advice
// suppressed — advice is its own column — and returns its byte size.
func measureSystemPromptBytes(project, role string) int {
	var n int
	withArchetypeEnv(project, role, func() {
		oldNoAdvice := primeNoAdvice
		primeNoAdvice = true
		defer func() { primeNoAdvice = oldNoAdvice }()

		// Synthetic agent path (project/rolePlural/probe) so subscription
		// parsing and the identity/workflow/wrap-up renderers fire exactly as
		// they do for a real agent. GetBead 404s on the probe id, so operator/
		// schedule/thread lookups no-op — matching a fresh spawn.
		agentID := project + "/" + role + "s/size-probe"
		var buf bytes.Buffer
		buildSystemPromptBody(&buf, agentID, role)
		n = buf.Len()
	})
	return n
}

// measureRoleNudgeBytes resolves the spawn-nudge template for the archetype and
// returns its rendered byte size. Vars are archetype-level (Project/Role) only;
// per-task hints (TaskHint/BoatPrompt) are left empty because they are not part
// of the archetype boot cost.
func measureRoleNudgeBytes(project, role string) int {
	var n int
	withArchetypeEnv(project, role, func() {
		promptType := nudgeTypeForRole(role)
		rendered := resolveNudgeFromConfig(promptType, nudgeVars{Project: project, Role: role})
		n = len(rendered)
	})
	return n
}

// nudgeTypeForRole picks which spawn-nudge template an archetype boots with:
// gardeners get the gardener orchestrator nudge, everything else gets the standard
// task-spawn ("adhoc") nudge.
func nudgeTypeForRole(role string) string {
	switch {
	case containsFold(role, "gardener"):
		return "gardener"
	case role == "thread":
		return "thread"
	default:
		return "adhoc"
	}
}

// measureAdviceTokens returns the matched-advice token cost for the archetype,
// reusing the exact pipeline gb advice audit / gb prime deliver on (AuditCatalog).
func measureAdviceTokens(project, role string, beads []*beadsapi.BeadDetail) int {
	res := advice.AuditCatalog(beads, []advice.Archetype{{Project: project, Role: role}})
	if len(res) == 0 {
		return 0
	}
	return res[0].Tokens
}

// measureClaudeMDBytes sums the CLAUDE.md files a project's agent reads: the repo
// root CLAUDE.md (every agent) plus a component CLAUDE.md named after the project
// when one exists on disk (e.g. project=gardenio → gardenio/CLAUDE.md).
func measureClaudeMDBytes(repoRoot, project string) int {
	total := fileBytes(filepath.Join(repoRoot, "CLAUDE.md"))
	if project != "" {
		total += fileBytes(filepath.Join(repoRoot, project, "CLAUDE.md"))
	}
	return total
}

// measureMCPBytes returns the byte size of the MCP config the archetype boots
// with. Prefers the resolved claude-mcp config beads (the source of truth the
// controller writes into .mcp.json); falls back to the on-disk .mcp.json when
// no config beads resolve (dev shells / detached runs).
func measureMCPBytes(repoRoot, project, role string) int {
	var n int
	withArchetypeEnv(project, role, func() {
		subs := buildSubscriptions(role)
		merged, count, _ := ResolveConfigBeads(context.Background(), daemon, "claude-mcp", subs)
		if count > 0 && merged != nil {
			if data, err := json.Marshal(merged); err == nil {
				n = len(data)
				return
			}
		}
		n = fileBytes(filepath.Join(repoRoot, ".mcp.json"))
	})
	return n
}

// measureGbToolDescriptionBytes walks the gb cobra command tree and sums the
// help text (Short + Long + per-flag usage) of every non-hidden command — the
// gb CLI "tool description" surface the audit rubric tracks. Archetype-invariant.
func measureGbToolDescriptionBytes() int {
	return commandHelpBytes(rootCmd)
}

// commandHelpBytes recursively sums a command's Short/Long help plus its local
// flag usage strings, then its subcommands'. Hidden commands are skipped — they
// are not part of the discoverable help surface.
func commandHelpBytes(cmd *cobra.Command) int {
	if cmd == nil || cmd.Hidden {
		return 0
	}
	n := len(cmd.Short) + len(cmd.Long) + len(cmd.Example) + len(cmd.Use)
	cmd.LocalFlags().VisitAll(func(f *pflag.Flag) {
		n += len(f.Name) + len(f.Usage)
	})
	for _, sub := range cmd.Commands() {
		n += commandHelpBytes(sub)
	}
	return n
}

// ── shared helpers ──────────────────────────────────────────────────────────

// withArchetypeEnv sets the process env/flags the prime renderers read
// (primeProject, BOAT_PROJECT, BOAT_ROLE, KD_PROJECT) to the given archetype,
// runs fn, then restores every value. Single-threaded by construction — the
// by-source loop is sequential — so the transient global mutation is safe.
func withArchetypeEnv(project, role string, fn func()) {
	oldPrimeProject := primeProject
	oldBoatProject, hadBoatProject := os.LookupEnv("BOAT_PROJECT")
	oldKDProject, hadKDProject := os.LookupEnv("KD_PROJECT")
	oldRole, hadRole := os.LookupEnv("BOAT_ROLE")
	defer func() {
		primeProject = oldPrimeProject
		restoreEnv("BOAT_PROJECT", oldBoatProject, hadBoatProject)
		restoreEnv("KD_PROJECT", oldKDProject, hadKDProject)
		restoreEnv("BOAT_ROLE", oldRole, hadRole)
	}()

	primeProject = project
	_ = os.Setenv("BOAT_PROJECT", project)
	_ = os.Unsetenv("KD_PROJECT") // primeProject/BOAT_PROJECT win; keep resolution unambiguous
	_ = os.Setenv("BOAT_ROLE", role)
	fn()
}

// restoreEnv restores an env var to its captured prior state.
func restoreEnv(key, val string, had bool) {
	if had {
		_ = os.Setenv(key, val)
	} else {
		_ = os.Unsetenv(key)
	}
}

// fileBytes returns the byte length of a file, or 0 if it can't be read.
func fileBytes(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return int(info.Size())
}

// findRepoRootForSizing walks up from the current working directory looking for
// a repo root marker (go.work or .git). Falls back to cwd. Used to locate the
// CLAUDE.md files and .mcp.json regardless of which subdir gb runs from.
func findRepoRootForSizing() string {
	dir, err := os.Getwd()
	if err != nil {
		return "."
	}
	for {
		if fileExists(filepath.Join(dir, "go.work")) || pathExists(filepath.Join(dir, ".git")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return mustGetwd()
		}
		dir = parent
	}
}

// mustGetwd returns the cwd or "." on error.
func mustGetwd() string {
	if d, err := os.Getwd(); err == nil {
		return d
	}
	return "."
}

// containsFold reports whether s contains sub, case-insensitively.
func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}
