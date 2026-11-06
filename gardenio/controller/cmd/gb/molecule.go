package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// moleculeCmd groups molecule-level (formula-instance) operator surfaces.
// Today the only subcommand is `cost`, which rolls up host-session spend +
// reported workflow_spend across a molecule's parent-child subtree (design
// ka-0000007305 §3.3).
var moleculeCmd = &cobra.Command{
	Use:     "molecule",
	Short:   "Molecule (formula-instance) operator surfaces",
	GroupID: "orchestration",
}

var moleculeCostCmd = &cobra.Command{
	Use:   "cost <molecule-id>",
	Short: "Roll up cost (host-session + workflow_spend) across a molecule subtree",
	Long: `Walks the parent-child subtree rooted at <molecule-id> and sums two
non-overlapping token pools (design ka-0000007305 §3.2-3.3):

  - host-session usage — the orchestrator/driver tokens already attached to
    agent/task/session beads, and
  - workflow_spend — the in-session dynamic-workflow subagent pool that has no
    beads of its own, reported explicitly by the host pod.

The molecule total is the SUM of the two (not a prefer-one). A 'partial' flag
is surfaced when ANY descendant's workflow_spend is non-final — i.e. an
in-flight dynamic-workflow run that may still be growing.

--json emits the full rolled-up MoleculeUsage (totals, both pools, and the
per-step breakdown).`,
	Args: cobra.ExactArgs(1),
	RunE: runMoleculeCost,
}

func init() {
	moleculeCmd.AddCommand(moleculeCostCmd)
}

func runMoleculeCost(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	molID := args[0]

	usage, err := daemon.RollupMoleculeUsage(ctx, molID)
	if err != nil {
		return fmt.Errorf("rolling up molecule cost: %w", err)
	}

	if jsonOutput {
		printJSON(usage)
		return nil
	}

	printMoleculeCost(usage)
	return nil
}

// printMoleculeCost renders the human-readable cost summary modeled on the
// design ka-0000007305 §3.3 example.
func printMoleculeCost(u *beadsapi.MoleculeUsage) {
	partial := ""
	if u.Partial {
		partial = "  ⚠ partial (a descendant workflow_spend is non-final)"
	}
	fmt.Printf("molecule %s%s\n", u.MoleculeID, partial)
	fmt.Printf("  beads: %d   sessions: %d   workflow-runs: %d (%s agents, cumulative)\n",
		u.BeadCount, u.SessionCount, u.WorkflowRunCount, fmtTokens(u.WorkflowSpend.AgentsSpawned))

	fmt.Printf("  cost:  %s  (host sessions %s + workflow_spend %s)\n",
		fmtSpend(u.TotalCostUSD), fmtSpend(u.SessionUsage.TotalCostUSD), fmtSpend(u.WorkflowSpend.TotalCostUSD))

	// Combined token pools (host session + workflow_spend) — the two are
	// non-overlapping so the per-line sum is the molecule total.
	// thinking(est) is a chars/4 estimate already counted inside output — it is
	// labeled and NOT summed into the total token pools.
	fmt.Printf("    input %s  output %s  thinking(est) %s  cache-read %s  cache-write %s\n",
		fmtTokens(u.SessionUsage.InputTokens+u.WorkflowSpend.InputTokens),
		fmtTokens(u.SessionUsage.OutputTokens+u.WorkflowSpend.OutputTokens),
		fmtTokens(u.SessionUsage.ThinkingTokens+u.WorkflowSpend.ThinkingTokens),
		fmtTokens(u.SessionUsage.CacheReadTokens+u.WorkflowSpend.CacheReadTokens),
		fmtTokens(u.SessionUsage.CacheWriteTokens+u.WorkflowSpend.CacheWriteTokens),
	)

	if len(u.Steps) == 0 {
		return
	}

	fmt.Println("  by step:")
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, s := range u.Steps {
		// Skip beads with no cost contribution — molecule roots and inert
		// container beads would otherwise pad the list with $- rows.
		if s.TotalCostUSD == 0 && s.WorkflowSpend == nil && !stepHasUsage(s) {
			continue
		}
		detail := ""
		if s.WorkflowSpend != nil {
			ws := s.WorkflowSpend
			finalNote := ""
			if !ws.Final {
				finalNote = ", non-final"
			}
			detail = fmt.Sprintf("\thost-session %s + workflow_spend %s (%s agents%s)",
				fmtSpend(s.SessionUsage.TotalCostUSD), fmtSpend(ws.TotalCostUSD),
				fmtTokens(ws.AgentsSpawned), finalNote)
		} else {
			detail = "\t1 session"
		}
		fmt.Fprintf(w, "    %s\t%s\t%s%s\n", s.Title, s.BeadID, fmtSpend(s.TotalCostUSD), detail)
	}
	_ = w.Flush()
}

// stepHasUsage reports whether a step's own session usage carries any
// non-zero token/cost/request signal. Mirrors the unexported
// SessionUsage.hasAny in beadsapi, which we can't call across the package
// boundary.
func stepHasUsage(s beadsapi.MoleculeStepUsage) bool {
	u := s.SessionUsage
	return u.InputTokens != 0 || u.OutputTokens != 0 || u.ThinkingTokens != 0 ||
		u.CacheReadTokens != 0 || u.CacheWriteTokens != 0 || u.TotalCostUSD != 0 ||
		u.RequestCount != 0 || u.ToolCallCount != 0 || u.ErrorCount != 0 ||
		u.CompactionCount != 0
}

// fmtTokens renders a token count compactly: 4_100_000 → "4.1M", 600_000 →
// "600.0K", small counts as-is. Matches the design §3.3 example shape.
func fmtTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}
