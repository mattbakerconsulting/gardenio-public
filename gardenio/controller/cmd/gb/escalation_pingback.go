package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// ── escalation pingback ─────────────────────────────────────────────────

// pingbackMinInterval is the minimum spacing between pingbacks on the same
// escalation. Bounds alert fatigue at the source: ka-0000003694 collected 13+
// ad-hoc nudges in ~1.5 days because nothing rate-limited the re-surfacing
// lever. Override with --force when a genuine emergency justifies it.
const pingbackMinInterval = 1 * time.Hour

var escalationPingbackCmd = &cobra.Command{
	Use:   "pingback <id>",
	Short: "Re-notify the operator about a stalled escalation (Slack re-post)",
	Long: `Request a Slack re-notification for an open escalation that the operator has
not acted on. The slack-bridge picks up the request and re-posts the escalation
card (as a broadcast reply on the original message when it is still tracked,
or a fresh card otherwise).

Unlike "gb escalation reflag", pingback does NOT change priority — it only
re-surfaces the escalation in Slack. Use reflag when the escalation has become
more urgent; use pingback when it is merely stalled.

Pingbacks are rate-limited to one per hour per escalation (manual or
automatic) to bound alert fatigue; --force overrides the limit.

The engine also fires automatic pingbacks on a backoff cadence (~12h, ~24h,
then doubling) and alerts the operator queue channel past 48h — you only need
manual pingback when an escalation is urgent ahead of that cadence.

Examples:
  gb escalation pingback ka-abc123
  gb escalation pingback ka-abc123 --reason="still blocking plan ka-xyz789"
  gb escalation pingback ka-abc123 --force`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		id := args[0]
		reason, _ := cmd.Flags().GetString("reason")
		force, _ := cmd.Flags().GetBool("force")

		bead, err := daemon.GetBead(cmd.Context(), id)
		if err != nil {
			return fmt.Errorf("getting escalation %s: %w", id, err)
		}
		if bead.Type != "escalation" {
			return fmt.Errorf("%s is not an escalation (type=%s)", id, bead.Type)
		}
		if bead.Status == "closed" {
			return fmt.Errorf("escalation %s is already closed; pingback has no effect", id)
		}

		// Rate limit: refuse when the last pingback (manual or auto) is
		// fresher than pingbackMinInterval, unless --force.
		if last := bead.Fields["pingback_requested_at"]; last != "" && !force {
			if t, err := time.Parse(time.RFC3339, last); err == nil {
				if age := time.Since(t); age < pingbackMinInterval {
					return fmt.Errorf(
						"last pingback was %s ago (< %s); use --force to override",
						age.Round(time.Minute), pingbackMinInterval)
				}
			}
		}

		count := 0
		if v := bead.Fields["pingback_count"]; v != "" {
			fmt.Sscanf(v, "%d", &count)
		}
		count++
		now := time.Now().UTC().Format(time.RFC3339)

		fields := map[string]string{
			"pingback_requested_at": now,
			"pingback_count":        fmt.Sprintf("%d", count),
			"pingback_by":           actor,
			"pingback_source":       "manual",
		}
		if reason != "" {
			fields["pingback_reason"] = reason
		}
		if err := daemon.UpdateBeadFields(cmd.Context(), id, fields); err != nil {
			return fmt.Errorf("updating pingback fields on %s: %w", id, err)
		}

		// Audit comment.
		auditMsg := fmt.Sprintf("pingback #%d requested by %s", count, actor)
		if reason != "" {
			auditMsg += fmt.Sprintf("\nreason: %s", reason)
		}
		if err := daemon.AddComment(cmd.Context(), id, actor, auditMsg); err != nil {
			// Non-fatal — the pingback request already landed.
			fmt.Fprintf(os.Stderr, "warning: failed to record audit comment: %v\n", err)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"id":                    id,
				"pingback_count":        count,
				"pingback_requested_at": now,
			})
		} else {
			fmt.Printf("Pingback #%d requested for %s — the bridge will re-post to Slack\n", count, id)
		}
		return nil
	},
}
