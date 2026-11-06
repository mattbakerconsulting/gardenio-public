package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminLinkDecisionReportsCmd backfills the parent-child edge from
// decision-artifact type=report beads to the originating task. The edge is
// added at creation time by `gb decision report` (see
// gardenio/docs/research/sequencer-report-decision-visibility.md, R3 row);
// this one-shot command repairs reports created before that change shipped.
//
// Idempotent: skips reports that already have a parent-child edge to the
// resolved originating task. Bounded by --limit to honor the kbeads migration
// policy (no eager full-table walks; agents iterate in --limit batches).
var adminLinkDecisionReportsCmd = &cobra.Command{
	Use:   "link-decision-reports",
	Short: "Backfill parent-child edges from decision-artifact reports to originating tasks",
	Long: `Walks closed type=decision beads in --page-size pages, finds child
type=report beads, resolves the originating task (parent of the decision, or
parent of the requesting_agent_bead_id agent), and adds the missing
parent-child edge from the report to the task.

Idempotent: a report that already has a parent-child edge to its originating
task is skipped. Decisions with no inferable originating task (operator-created
standalone) are skipped with no error.

Bounded by --limit to honor the kbeads migration policy: this is a one-shot
operator command, not an automatic startup walk. Use --dry-run first to
estimate the edge count.`,
	RunE: runAdminLinkDecisionReports,
}

func init() {
	adminLinkDecisionReportsCmd.Flags().Bool("dry-run", false, "report edges that would be added without writing")
	adminLinkDecisionReportsCmd.Flags().Int("limit", 1000, "max decisions to scan in this run (0 = unbounded)")
	adminLinkDecisionReportsCmd.Flags().Int("page-size", 100, "decisions per ListBeads page")
	adminCmd.AddCommand(adminLinkDecisionReportsCmd)
}

func runAdminLinkDecisionReports(cmd *cobra.Command, _ []string) error {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	limit, _ := cmd.Flags().GetInt("limit")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 100
	}

	ctx := context.Background()
	start := time.Now()

	var (
		decisionsScanned int
		reportsScanned   int
		edgesAdded       int
		edgesSkipped     int
		standalone       int
		offset           int
	)

	if dryRun {
		fmt.Println("dry-run: counting edges that would be added (no writes)")
	}

	for {
		req := &client.ListBeadsRequest{
			Type:   []string{"decision"},
			Status: []string{string(model.StatusClosed)},
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		}
		resp, err := beadsClient.ListBeads(ctx, req)
		if err != nil {
			return fmt.Errorf("listing decisions (offset=%d): %w", offset, err)
		}
		if len(resp.Beads) == 0 {
			break
		}

		for _, dec := range resp.Beads {
			decisionsScanned++

			// Resolve the originating task once per decision.
			origTaskID, err := resolveOriginatingTaskAdmin(ctx, dec)
			if err != nil {
				fmt.Printf("warn: %s: %v\n", dec.ID, err)
				continue
			}
			if origTaskID == "" {
				standalone++
				continue
			}

			// Find type=report children of this decision.
			revDeps, err := beadsClient.GetReverseDependencies(ctx, dec.ID)
			if err != nil {
				fmt.Printf("warn: %s: getting reverse deps: %v\n", dec.ID, err)
				continue
			}
			for _, rd := range revDeps {
				if rd.Type != model.DepParentChild {
					continue
				}
				// rd.BeadID is the child (report); rd.DependsOnID is the parent (decision).
				reportID := rd.BeadID
				report, err := beadsClient.GetBead(ctx, reportID)
				if err != nil {
					fmt.Printf("warn: %s: get child %s: %v\n", dec.ID, reportID, err)
					continue
				}
				if string(report.Type) != "report" {
					continue
				}
				reportsScanned++

				// Idempotency: skip if the report already has a parent-child
				// edge to the originating task.
				existing, err := beadsClient.GetDependencies(ctx, reportID)
				if err != nil {
					fmt.Printf("warn: %s: get deps for report %s: %v\n", dec.ID, reportID, err)
					continue
				}
				already := false
				for _, d := range existing {
					if d.Type == model.DepParentChild && d.DependsOnID == origTaskID {
						already = true
						break
					}
				}
				if already {
					edgesSkipped++
					continue
				}

				if dryRun {
					edgesAdded++
					fmt.Printf("would link: %s → %s (decision %s)\n", reportID, origTaskID, dec.ID)
					continue
				}
				_, err = beadsClient.AddDependency(ctx, &client.AddDependencyRequest{
					BeadID:      reportID,
					DependsOnID: origTaskID,
					Type:        string(model.DepParentChild),
				})
				if err != nil {
					fmt.Printf("warn: %s: add edge %s→%s: %v\n", dec.ID, reportID, origTaskID, err)
					continue
				}
				edgesAdded++
				fmt.Printf("linked: %s → %s (decision %s)\n", reportID, origTaskID, dec.ID)
			}

			if limit > 0 && decisionsScanned >= limit {
				break
			}
		}

		if limit > 0 && decisionsScanned >= limit {
			break
		}
		if len(resp.Beads) < pageSize {
			break
		}
		offset += len(resp.Beads)
	}

	verb := "added"
	if dryRun {
		verb = "would add"
	}
	fmt.Printf("\ndecisions scanned: %d\n", decisionsScanned)
	fmt.Printf("standalone (no originating task): %d\n", standalone)
	fmt.Printf("reports scanned:   %d\n", reportsScanned)
	fmt.Printf("edges %s:          %d\n", verb, edgesAdded)
	fmt.Printf("edges skipped (already linked): %d\n", edgesSkipped)
	fmt.Printf("duration: %s\n", time.Since(start).Round(time.Millisecond))
	return nil
}

// resolveOriginatingTaskAdmin mirrors gb's resolveOriginatingTask but operates
// on a *model.Bead loaded from the kd client. Pathways: decision parent → agent
// parent. Empty string means "no inferable task; skip".
func resolveOriginatingTaskAdmin(ctx context.Context, dec *model.Bead) (string, error) {
	deps, err := beadsClient.GetDependencies(ctx, dec.ID)
	if err != nil {
		return "", fmt.Errorf("get deps for decision %s: %w", dec.ID, err)
	}
	for _, d := range deps {
		if d.Type == model.DepParentChild && d.DependsOnID != "" {
			return d.DependsOnID, nil
		}
	}
	// Fallback: requesting_agent_bead_id → agent's parent.
	agentID := ""
	if len(dec.Fields) > 0 {
		var f map[string]any
		if err := json.Unmarshal(dec.Fields, &f); err == nil {
			if v, ok := f["requesting_agent_bead_id"].(string); ok {
				agentID = v
			}
		}
	}
	if agentID == "" {
		return "", nil
	}
	adeps, err := beadsClient.GetDependencies(ctx, agentID)
	if err != nil {
		return "", fmt.Errorf("get deps for agent %s: %w", agentID, err)
	}
	for _, d := range adeps {
		if d.Type == model.DepParentChild && d.DependsOnID != "" {
			return d.DependsOnID, nil
		}
	}
	return "", nil
}
