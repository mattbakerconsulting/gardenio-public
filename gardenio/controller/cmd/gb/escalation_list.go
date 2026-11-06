package main

import (
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── escalation list ─────────────────────────────────────────────────────

var escalationListCmd = &cobra.Command{
	Use:   "list",
	Short: "List escalation requests",
	Long: `List escalation requests.

By default EVERY matching escalation is returned (the daemon's paged list
endpoint is walked to exhaustion), so the command can never silently hide the
oldest / most-starved escalations behind a fixed cap — the exact defect this
replaced (bug ka-0000001245: a hardcoded newest-50 cap structurally hid the 39
oldest of 89 open escalations, including operator-gating P0s).

Pass --limit N for a bounded preview; when the match set exceeds N a truncation
banner reports the true open count (mirroring the beads-viewer /escalations
page), so a cap is always announced, never silent.

Scope to your OWN open gates with --mine, mirroring gb decision list --mine:
it matches the durable lane owner (owner_seat = your seat_key) with a
created_by = actor fallback, so it survives respawn. --lane <seat> inspects
another lane by its seat_key.

  gb escalation list --mine          # escalations MY lane filed that are open
  gb escalation list --lane <seat>   # escalations ANOTHER lane filed that are open`,
	RunE: func(cmd *cobra.Command, args []string) error {
		statusFilter, _ := cmd.Flags().GetString("status")
		limit, _ := cmd.Flags().GetInt("limit")

		statuses := []string{"open", "in_progress"}
		if statusFilter != "" {
			statuses = strings.Split(statusFilter, ",")
		}

		heldBy, _ := cmd.Flags().GetString("held-by")
		needsMe, _ := cmd.Flags().GetBool("needs-me")
		if heldBy != "" || needsMe {
			if heldBy != "" && needsMe {
				return fmt.Errorf("--held-by and --needs-me are mutually exclusive")
			}
			mine, _ := cmd.Flags().GetBool("mine")
			laneSeat, _ := cmd.Flags().GetString("lane")
			if mine || laneSeat != "" {
				return fmt.Errorf("--held-by/--needs-me cannot be combined with --mine or --lane")
			}
			refs := strings.Split(heldBy, ",")
			if needsMe {
				refs = []string{"@me"}
			}
			// The daemon filters held_by in Go and cuts to limit, defaulting to
			// 50; with no --limit ask for its cap (maxListBeadsLimit) so the
			// default listing stays "every match", as the ownership path does
			// (ka-0000001245).
			heldLimit := limit
			if heldLimit == 0 {
				heldLimit = 10000
			}
			beads, err := daemon.ListEscalationsHeldBy(cmd.Context(), refs, statuses, heldLimit)
			if err != nil {
				return fmt.Errorf("listing escalations: %w", err)
			}
			return printEscalationList(&beadsapi.ListBeadsResult{Beads: beads, Total: len(beads)}, limit)
		}

		// --mine scopes to escalations THIS lane owns; --lane <seat> inspects
		// another lane. Ownership matches the DURABLE owner_seat with a
		// created_by == actor fallback, keeping "mine" to one meaning across the
		// decision/escalation pair (ka-0000005138). Escalations carry no assignee
		// (gb escalation create stamps none), so created_by is the only
		// instance-level column; owner_seat is the durable one that survives
		// respawn.
		//
		// NOTE: escalation create does not yet stamp owner_seat, so today the
		// fallback (created_by == actor) is what a --mine listing resolves on.
		// The seat predicate is wired now so that once escalations carry
		// owner_seat, --mine survives respawn here too with no further change.
		scope, err := resolveOwnershipScope(cmd)
		if err != nil {
			return err
		}

		query := beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: statuses,
			Sort:     "-created_at",
		}

		var result *beadsapi.ListBeadsResult
		if limit > 0 {
			// Bounded preview: cap the rows but keep the daemon's full COUNT(*)
			// in Total so we can announce the truncation below.
			query.Limit = limit
			result, err = listOwnedBeads(cmd.Context(), query, scope, false)
		} else {
			// Default: page-walk the daemon to exhaustion and return EVERY
			// matching escalation. ListAllBeadsFiltered owns paging (Limit must
			// stay zero) and pins a stable sort internally.
			result, err = listOwnedBeads(cmd.Context(), query, scope, true)
		}
		if err != nil {
			return fmt.Errorf("listing escalations: %w", err)
		}

		return printEscalationList(result, limit)
	},
}

// printEscalationList renders an escalation listing (JSON or the table) and
// the truncation banner. Shared by the ownership and held_by paths.
func printEscalationList(result *beadsapi.ListBeadsResult, limit int) error {
	if jsonOutput {
		// Emit the raw bead list so `gb escalation list --json | jq …`
		// works — previously this printed the human table and jq exited 5
		// (bug ka-0000007267). Bare array, matching kd/gb list --json shape.
		printJSON(result.Beads)
		return nil
	}

	if len(result.Beads) == 0 {
		fmt.Println("No escalations found.")
		return nil
	}

	// Header.
	fmt.Printf("%-14s %-12s %-4s %-20s %-8s %-16s %s\n",
		"ID", "STATUS", "PRI", "TYPE", "AGE", "AGENT", "TITLE")
	fmt.Println(strings.Repeat("─", 110))

	for _, b := range result.Beads {
		opType := b.Fields["operation_type"]
		sens := b.Fields["sensitivity"]
		sensTag := ""
		if sens != "" && sens != "public" {
			sensTag = " [" + sens + "]"
		}
		if aud := beadsapi.EscalationAudience(b.Fields); aud != beadsapi.AudienceOwner {
			sensTag += " [" + aud + "]"
		}

		// Format age.
		age := escalationAge(b.CreatedAt)

		// Format priority.
		pri := fmt.Sprintf("P%d", b.Priority)

		// Format requesting agent (truncated).
		agent := b.Fields["requesting_agent"]
		if agent != "" {
			// Show just the bead ID prefix for compactness.
			if len(agent) > 14 {
				agent = agent[:14]
			}
		} else {
			agent = "-"
		}

		// Truncate title if needed.
		title := b.Title + sensTag
		if len(title) > 60 {
			title = title[:57] + "..."
		}

		fmt.Printf("%-14s %-12s %-4s %-20s %-8s %-16s %s\n",
			b.ID, b.Status, pri, truncField(opType, 20), age, agent, title)
	}

	// Announce truncation loudly when a bounded --limit hid rows. Total is
	// the daemon's full COUNT(*) of the match; len(result.Beads) is what we
	// printed. Never let a cap be silent — that inversion (a newest-first
	// sort + a silent cap hiding the oldest, starved escalations) is exactly
	// what bug ka-0000001245 was about.
	if limit > 0 && result.Total > len(result.Beads) {
		fmt.Printf("\n… showing %d of %d matching escalations (--limit %d). Re-run without --limit to see all, or raise --limit.\n",
			len(result.Beads), result.Total, limit)
	}

	return nil
}

// escalationAge returns a human-readable age string like "2h", "3d", "1w".
// isE2EFixture reports whether an escalation was created by an E2E test
// agent. Fixture closures should be excluded from the stats denominator
// because the harness doesn't capture result_status — they would otherwise
// drown out signal from real operator/gardener closures.
func isE2EFixture(createdBy string) bool {
	switch createdBy {
	case "escalation-analyze", "e2e-agent-v2":
		return true
	}
	return strings.HasPrefix(createdBy, "e2e-v")
}

func escalationAge(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dw", int(d.Hours()/(24*7)))
	}
}

// truncField truncates a string to maxLen.
func truncField(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}
