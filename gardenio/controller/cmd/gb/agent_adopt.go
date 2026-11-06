package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/scheduler"
	"github.com/spf13/cobra"
)

// gb agent adopt (persistent-agents Phase 3, task ka-0000005121, design memo
// ka-0000001420 §3.2). Manual override for gardener ownership: it rewrites the
// agent bead's canonical gardener_id AND reconciles the live card/lineage in the
// same operation.
//
// Ownership is derived from the agent bead's gardener_id field (beadsapi
// DeriveGardenerID / the viewer's ?group=gardener buckets), so rewriting that one
// field re-parents the agent under the new gardener everywhere it is read; the
// daemon's field-update event drives the Slack card refresh (the bridge has no
// K8s deps and observes the bead change). This is the consuming half of the
// needs-gardener backstop sweep: adopting an agent that the sweep surfaced
// removes the needs-gardener label so it drops out of the queryable leak set.
func init() {
	agentAdoptCmd.Flags().String("gardener", "", "The gardener (trigger) bead ID to adopt the agent under (required)")
	agentAdoptCmd.Flags().String("label", scheduler.DefaultNeedsGardenerLabel, "The needs-gardener backstop label to clear on adoption")
	_ = agentAdoptCmd.MarkFlagRequired("gardener")
	agentCmd.AddCommand(agentAdoptCmd)
}

var agentAdoptCmd = &cobra.Command{
	Use:   "adopt <agent-bead-id> --gardener <gardener-bead-id>",
	Short: "Adopt an agent under a gardener: rewrite gardener_id and reconcile the card/lineage",
	Long: "Rewrite an agent bead's canonical gardener_id to the given gardener and reconcile " +
		"the live card/lineage. Clears the needs-gardener backstop label (if present) so the " +
		"agent drops out of the leak set, and records an audit comment. This is the manual " +
		"override the needs-gardener backstop sweep is designed to feed.",
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := cmd.Context()
		agentID := strings.TrimSpace(args[0])
		if agentID == "" {
			return fmt.Errorf("agent bead ID is required")
		}
		gardener, _ := cmd.Flags().GetString("gardener")
		gardener = strings.TrimSpace(gardener)
		if gardener == "" {
			return fmt.Errorf("--gardener is required and must be non-empty")
		}
		backstopLabelRaw, _ := cmd.Flags().GetString("label")
		backstopLabel := strings.TrimSpace(backstopLabelRaw)

		// Validate the target is an agent bead and read its current ownership +
		// labels so the operation is idempotent and auditable.
		bead, err := daemon.GetBead(ctx, agentID)
		if err != nil {
			return fmt.Errorf("reading agent bead %s: %w", agentID, err)
		}
		if bead.Type != "agent" {
			return fmt.Errorf("%s is a %q bead, not an agent — adopt only rewrites agent gardener ownership", agentID, bead.Type)
		}
		prev := strings.TrimSpace(bead.Fields["gardener_id"])
		if prev == gardener {
			cmd.Printf("Agent %s is already owned by gardener %s — nothing to do.\n", agentID, gardener)
			// Still clear a stale backstop label if it is somehow present.
			if backstopLabel != "" && labelSetContains(bead.Labels, backstopLabel) {
				if err := daemon.RemoveLabel(ctx, agentID, backstopLabel); err != nil {
					cmd.PrintErrf("warning: clearing %q label failed: %v\n", backstopLabel, err)
				} else {
					cmd.Printf("Cleared stale %q label.\n", backstopLabel)
				}
			}
			return nil
		}

		// Rewrite the canonical gardener_id first — this is the authoritative
		// ownership write, and the daemon event it emits reconciles the live card
		// and the viewer's gardener lineage buckets.
		if err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{"gardener_id": gardener}); err != nil {
			return fmt.Errorf("rewriting gardener_id on %s: %w", agentID, err)
		}

		// Clear the needs-gardener backstop label so the adopted agent drops out of
		// the queryable leak set. Best-effort: the ownership write above already
		// stands, so a label-remove failure must not fail the adoption.
		if backstopLabel != "" && labelSetContains(bead.Labels, backstopLabel) {
			if err := daemon.RemoveLabel(ctx, agentID, backstopLabel); err != nil {
				cmd.PrintErrf("warning: clearing %q label failed (gardener_id was still rewritten): %v\n", backstopLabel, err)
			}
		}

		// Audit trail: record who reparented the agent and from/to which gardener.
		comment := fmt.Sprintf("gb agent adopt: gardener_id %s → %s", displayGardener(prev), gardener)
		if err := daemon.AddComment(ctx, agentID, defaultActor(), comment); err != nil {
			cmd.PrintErrf("warning: recording adopt audit comment failed: %v\n", err)
		}

		cmd.Printf("Adopted agent %s under gardener %s (was %s).\n", agentID, gardener, displayGardener(prev))
		return nil
	},
}

// labelSetContains reports whether labels contains target (case-insensitive, trimmed).
func labelSetContains(labels []string, target string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), target) {
			return true
		}
	}
	return false
}

// displayGardener renders a gardener_id for human output, mapping an empty prior
// value to a readable placeholder.
func displayGardener(id string) string {
	if strings.TrimSpace(id) == "" {
		return "(unset)"
	}
	return id
}
