package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// orphanBead represents a bead referenced in git commits but still open.
type orphanBead struct {
	ID                  string `json:"id"`
	Title               string `json:"title"`
	Status              string `json:"status"`
	LatestCommit        string `json:"latest_commit,omitempty"`
	LatestCommitMessage string `json:"latest_commit_message,omitempty"`
}

// gitLogRunner is the function used to get git log output. Replaceable for testing.
var gitLogRunner = func(dir string) (string, error) {
	cmd := exec.Command("git", "log", "--oneline", "--all")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// beadCloser is the function used to close a bead. Replaceable for testing.
var beadCloser = func(ctx context.Context, c client.BeadsClient, id, closedBy string) error {
	_, err := c.CloseBead(ctx, id, closedBy)
	return err
}

var orphansCmd = &cobra.Command{
	Use:     "orphans",
	Short:   "Identify orphaned beads (referenced in commits but still open)",
	GroupID: "views",
	Long: `Identify orphaned beads - beads that are referenced in commit messages
but remain open or in_progress in the database.

This helps identify work that has been implemented but not formally closed.

Examples:
  kd orphans              # Show orphaned beads
  kd orphans --json       # Machine-readable output
  kd orphans --details    # Show full commit information
  kd orphans --fix        # Close orphaned beads with confirmation
  kd orphans --prefix bd  # Use a different prefix (default: kd)`,
	RunE: runOrphans,
}

func init() {
	orphansCmd.Flags().BoolP("fix", "f", false, "Close orphaned beads with confirmation")
	orphansCmd.Flags().Bool("details", false, "Show full commit information")
	orphansCmd.Flags().String("prefix", "kd", "Issue ID prefix to match in commit messages")
}

func runOrphans(cmd *cobra.Command, args []string) error {
	fix, _ := cmd.Flags().GetBool("fix")
	details, _ := cmd.Flags().GetBool("details")
	prefix, _ := cmd.Flags().GetString("prefix")

	// Check that we're in a git repo.
	gitCheck := exec.Command("git", "rev-parse", "--git-dir")
	if err := gitCheck.Run(); err != nil {
		if jsonOutput {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("Not a git repository — nothing to check.")
		return nil
	}

	// Fetch open/in_progress beads from the server.
	ctx := context.Background()
	resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
		Status: []string{"open", "in_progress"},
		Limit:  5000,
	})
	if err != nil {
		return fmt.Errorf("listing beads: %w", err)
	}

	if len(resp.Beads) == 0 {
		if jsonOutput {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("No open beads to check.")
		return nil
	}

	// Build lookup map of open beads.
	openBeads := make(map[string]*orphanBead, len(resp.Beads))
	for _, b := range resp.Beads {
		openBeads[b.ID] = &orphanBead{
			ID:     b.ID,
			Title:  b.Title,
			Status: string(b.Status),
		}
	}

	// Scan git log for bead ID references.
	logOutput, err := gitLogRunner(".")
	if err != nil {
		if jsonOutput {
			fmt.Println("[]")
			return nil
		}
		fmt.Println("Could not read git log — skipping orphan check.")
		return nil
	}

	// Match pattern like (ka-xxx) or (ka-xxx.1) including hierarchical IDs.
	pattern := fmt.Sprintf(`\(%s-[a-zA-Z0-9._]+\)`, regexp.QuoteMeta(prefix))
	re := regexp.MustCompile(pattern)

	for _, line := range strings.Split(logOutput, "\n") {
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, " ", 2)
		commitHash := parts[0]
		commitMsg := ""
		if len(parts) > 1 {
			commitMsg = parts[1]
		}

		for _, match := range re.FindAllString(line, -1) {
			beadID := strings.Trim(match, "()")
			if orphan, exists := openBeads[beadID]; exists {
				// Only record first (most recent) commit per bead.
				if orphan.LatestCommit == "" {
					orphan.LatestCommit = commitHash
					orphan.LatestCommitMessage = commitMsg
				}
			}
		}
	}

	// Collect beads that have commit references.
	orphans := make([]orphanBead, 0)
	for _, ob := range openBeads {
		if ob.LatestCommit != "" {
			orphans = append(orphans, *ob)
		}
	}

	sort.Slice(orphans, func(i, j int) bool {
		return orphans[i].ID < orphans[j].ID
	})

	// Output.
	if jsonOutput {
		data, err := json.MarshalIndent(orphans, "", "  ")
		if err != nil {
			return fmt.Errorf("marshaling JSON: %w", err)
		}
		fmt.Println(string(data))
		return nil
	}

	if len(orphans) == 0 {
		fmt.Println("No orphaned beads found.")
		return nil
	}

	fmt.Printf("\nFound %d orphaned bead(s):\n\n", len(orphans))

	for i, orphan := range orphans {
		fmt.Printf("%d. %s: %s\n", i+1, orphan.ID, orphan.Title)
		fmt.Printf("   Status: %s\n", orphan.Status)
		if details && orphan.LatestCommit != "" {
			fmt.Printf("   Latest commit: %s - %s\n", orphan.LatestCommit, orphan.LatestCommitMessage)
		}
	}

	if fix {
		fmt.Println()
		fmt.Printf("This will close %d orphaned bead(s). Continue? (Y/n): ", len(orphans))
		var response string
		_, _ = fmt.Scanln(&response)
		response = strings.ToLower(strings.TrimSpace(response))
		if response != "" && response != "y" && response != "yes" {
			fmt.Println("Canceled.")
			return nil
		}

		closedCount := 0
		for _, orphan := range orphans {
			if err := beadCloser(ctx, beadsClient, orphan.ID, actor); err != nil {
				fmt.Fprintf(os.Stderr, "Error closing %s: %v\n", orphan.ID, err)
			} else {
				fmt.Printf("Closed %s\n", orphan.ID)
				closedCount++
			}
		}
		fmt.Printf("\nClosed %d bead(s)\n", closedCount)
	}

	return nil
}
