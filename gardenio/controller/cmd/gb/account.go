package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var accountCmd = &cobra.Command{
	Use:     "account",
	Short:   "Manage LLM accounts (rotate, status, reset)",
	GroupID: "orchestration",
}

// ── account status ──────────────────────────────────────────────────

var accountStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show all accounts with their current status",
	RunE:  runAccountStatus,
}

func runAccountStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()

	accounts, err := daemon.ListAccountBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing accounts: %w", err)
	}

	if len(accounts) == 0 {
		fmt.Fprintln(os.Stdout, "No account beads found.")
		return nil
	}

	// Build reverse map: account name → projects authorized for it, derived
	// from each account bead's project:<name> labels. Accounts with no
	// project labels are global/fallback and show no project mapping.
	accountProjects := make(map[string][]string)
	for _, acct := range accounts {
		var projects []string
		for _, label := range acct.Labels {
			if p, ok := strings.CutPrefix(label, "project:"); ok && p != "" {
				projects = append(projects, p)
			}
		}
		sort.Strings(projects)
		accountProjects[acct.Name] = projects
	}

	if jsonOutput {
		type accountEntry struct {
			Name     string   `json:"name"`
			Provider string   `json:"provider"`
			Status   string   `json:"status"`
			Model    string   `json:"model,omitempty"`
			Fallback string   `json:"fallback,omitempty"`
			Projects []string `json:"projects,omitempty"`
		}
		var entries []accountEntry
		for _, acct := range accounts {
			status := acct.Status
			if status == "" {
				status = "active"
			}
			entries = append(entries, accountEntry{
				Name:     acct.Name,
				Provider: acct.Provider,
				Status:   status,
				Model:    acct.Model,
				Fallback: acct.Fallback,
				Projects: accountProjects[acct.Name],
			})
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tPROVIDER\tSTATUS\tMODEL\tFALLBACK\tPROJECTS")
	for _, acct := range accounts {
		status := acct.Status
		if status == "" {
			status = "active"
		}
		projects := strings.Join(accountProjects[acct.Name], ",")
		if projects == "" {
			projects = "-"
		}
		fallback := acct.Fallback
		if fallback == "" {
			fallback = "-"
		}
		model := acct.Model
		if model == "" {
			model = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
			acct.Name, acct.Provider, status, model, fallback, projects)
	}
	w.Flush()

	return nil
}

// ── account rotate ──────────────────────────────────────────────────

var accountRotateCmd = &cobra.Command{
	Use:   "rotate <project>",
	Short: "Rotate to fallback account for a project",
	Long: `Triggers the same rotation logic as automatic budget-exhaustion failover:
marks the project's currently-active account bead as exhausted so the next
project:<name>-labeled active account (alphabetically) takes over. The
current account's fallback field is used to validate that a designated
successor exists. Running agents are restarted to pick up the new
credentials.`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountRotate,
}

func runAccountRotate(cmd *cobra.Command, args []string) error {
	project := args[0]
	ctx := context.Background()

	// Step 1: Find the project's currently-active account (alphabetically-first
	// account labeled project:<name> with status "active" or empty).
	pool, err := daemon.ListAccountBeadsByProject(ctx, project)
	if err != nil {
		return fmt.Errorf("listing account beads for project %q: %w", project, err)
	}
	if len(pool) == 0 {
		return fmt.Errorf("no active account beads labeled project:%s", project)
	}
	currentName := pool[0].Name

	// Step 2: Look up the account bead (need ID + fields) and validate fallback.
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    50,
	})
	if err != nil {
		return fmt.Errorf("listing account beads: %w", err)
	}
	currentBead := findBeadByTitle(result.Beads, currentName)
	if currentBead == nil {
		return fmt.Errorf("current account bead %q not found", currentName)
	}

	fallback := currentBead.Fields["fallback"]
	if fallback == "" {
		return fmt.Errorf("account %q has no fallback configured", currentName)
	}
	fallbackBead := findBeadByTitle(result.Beads, fallback)
	if fallbackBead == nil {
		return fmt.Errorf("fallback account bead %q not found", fallback)
	}

	// Step 3: Mark the current account as exhausted. The label-aware pool
	// automatically routes new pods to the next active sibling.
	now := time.Now().UTC().Format(time.RFC3339)
	rotCount, _ := strconv.Atoi(currentBead.Fields["rotation_count"])
	if err := daemon.UpdateBeadFieldsTyped(ctx, currentBead.ID, map[string]any{
		"status":          "exhausted",
		"exhausted_at":    now,
		"rotation_count":  rotCount + 1,
		"last_rotated_at": now,
	}); err != nil {
		return fmt.Errorf("updating account status: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Rotated project %q: %s -> %s\n", project, currentName, fallback)
	fmt.Fprintf(os.Stdout, "  Account bead: %s\n", currentBead.ID)
	fmt.Fprintln(os.Stdout, "\nThe AccountUpdated event will trigger a rolling restart of running agents.")
	return nil
}

// ── account reset ───────────────────────────────────────────────────

var accountResetCmd = &cobra.Command{
	Use:   "reset <account-name>",
	Short: "Mark an exhausted account as active again",
	Long: `Manually reset an account's status from "exhausted" back to "active"
and clear the exhausted_at timestamp. Use this after a daily spend reset
or when an account has recovered from rate limiting. Any project labeled
on the account will immediately see it back in their active pool (and,
if it sorts alphabetically-first, take over as the project default).`,
	Args: cobra.ExactArgs(1),
	RunE: runAccountReset,
}

func runAccountReset(cmd *cobra.Command, args []string) error {
	accountName := args[0]
	ctx := context.Background()

	// Find the account bead.
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"account"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred"},
		Limit:    50,
	})
	if err != nil {
		return fmt.Errorf("listing account beads: %w", err)
	}

	accountBead := findBeadByTitle(result.Beads, accountName)
	if accountBead == nil {
		return fmt.Errorf("account %q not found", accountName)
	}

	// Reset the essential status fields first — these must always succeed.
	essentialFields := map[string]string{
		"status":       "active",
		"exhausted_at": "",
	}
	if err := daemon.UpdateBeadFields(ctx, accountBead.ID, essentialFields); err != nil {
		return fmt.Errorf("updating account status: %w", err)
	}

	fmt.Fprintf(os.Stdout, "Account %q reset to active (bead %s)\n", accountName, accountBead.ID)

	// Set reset metadata (last_reset_at, spend snapshot) as best-effort.
	// These fields may not be in the type schema on older kbeads servers.
	metaFields := map[string]string{
		"last_reset_at": time.Now().UTC().Format(time.RFC3339),
	}
	if spendSnapshot := snapshotLiteLLMSpend(ctx, accountBead); spendSnapshot != "" {
		metaFields["spend_at_last_reset"] = spendSnapshot
		fmt.Fprintf(os.Stdout, "  Spend snapshot: $%s (API-equivalent)\n", spendSnapshot)
	}
	if err := daemon.UpdateBeadFields(ctx, accountBead.ID, metaFields); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set reset metadata: %v\n", err)
	}

	return nil
}

// ── helpers ─────────────────────────────────────────────────────────

// findBeadByTitle returns the first bead with the given title, or nil.
func findBeadByTitle(beads []*beadsapi.BeadDetail, title string) *beadsapi.BeadDetail {
	for _, b := range beads {
		if b.Title == title {
			return b
		}
	}
	return nil
}

// snapshotLiteLLMSpend queries LiteLLM for the current spend of the account's
// virtual key and returns it as a string (e.g. "142.30"). Returns "" if the
// LiteLLM URL/key aren't configured or the query fails — callers should treat
// this as best-effort.
func snapshotLiteLLMSpend(ctx context.Context, bead *beadsapi.BeadDetail) string {
	litellmURL := os.Getenv("LITELLM_URL")
	litellmKey := os.Getenv("LITELLM_MASTER_KEY")
	virtualKey := bead.Fields["litellm_key"]
	if litellmURL == "" || litellmKey == "" || virtualKey == "" {
		return ""
	}

	client := &http.Client{Timeout: 10 * time.Second}
	info, err := queryLiteLLMKeyInfo(ctx, client, litellmURL, litellmKey, virtualKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not snapshot LiteLLM spend: %v\n", err)
		return ""
	}
	return fmt.Sprintf("%.2f", info.Spend)
}

func init() {
	accountCmd.AddCommand(accountStatusCmd)
	accountCmd.AddCommand(accountRotateCmd)
	accountCmd.AddCommand(accountResetCmd)
}
