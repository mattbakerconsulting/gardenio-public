package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var escalationClaimCmd = &cobra.Command{
	Use:   "claim <id>",
	Short: "Claim an escalation your lane holds (held_by)",
	Long: `Claim an escalation so the other members of its held_by lane know you are on it.
Only a member of the held_by lane may claim. The status does not change.

Examples:
  gb escalation claim gd-abc123`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		b, err := daemon.ClaimEscalation(cmd.Context(), args[0])
		if err != nil {
			return fmt.Errorf("claiming escalation: %w", err)
		}
		fmt.Printf("Claimed: %s\n", b.ID)
		fmt.Printf("  Held by: %s\n", b.Fields["held_by"])
		fmt.Printf("  Claimed by: %s\n", b.Fields["claimed_by"])
		return nil
	},
}

var escalationHandUpCmd = &cobra.Command{
	Use:   "hand-up <id>",
	Short: "Hand an escalation up to the operator (or the land owner)",
	Long: `Hand an escalation your lane holds up one level: to the operator, or for a tenant
land to its owner. The reason is stamped on the bead and re-notifies the new holder.

--operator-class 1|2|3 is the merge desk's OPERATOR-ONLY class on a PR merge
(docs/merge-desk.md, "Sensitive paths"): the merge goes to the operator on the
owner floor (policy_rule floor:operator_class_merge), so only the owner approves
it, and the desk merges once they do.

Examples:
  gb escalation hand-up gd-abc123 --reason "touches deploy-values; needs the owner"
  gb escalation hand-up gd-abc123 --operator-class 2 --reason "OPERATOR-ONLY (2): widens an approver lane"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		reason, _ := cmd.Flags().GetString("reason")
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("--reason is required")
		}
		class, _ := cmd.Flags().GetString("operator-class")
		b, err := daemon.HandUpEscalation(cmd.Context(), args[0], beadsapi.HandUpEscalationRequest{Reason: reason, OperatorClass: class})
		if err != nil {
			return fmt.Errorf("handing up escalation: %w", err)
		}
		fmt.Printf("Handed up: %s\n", b.ID)
		fmt.Printf("  Now held by: %s\n", b.Fields["held_by"])
		return nil
	},
}

var escalationRerouteCmd = &cobra.Command{
	Use:   "reroute --approved-unexecuted",
	Short: "Re-target approved escalations an idle hand-up left with no executor (admin)",
	Long: `One-shot backfill for approved escalations the idle sweep left with the operator.
For every open, approved, unclaimed escalation the operator holds: one the sweep moved
from a staffed lane to operator goes back to that lane, a coordinator lane's bead-only
item off the floor moves to coordinator:cluster when that lane is staffed, and one the
sweep handed up from a staffed executor goes back to it. Manual hand-ups stay put.
Needs gardenio-admin on your own sign-in. Use --dry-run to preview.

Examples:
  gb escalation reroute --approved-unexecuted --dry-run
  gb escalation reroute --approved-unexecuted`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		approved, _ := cmd.Flags().GetBool("approved-unexecuted")
		if !approved {
			return fmt.Errorf("--approved-unexecuted is required")
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		res, err := daemon.RerouteEscalations(cmd.Context(), beadsapi.RerouteEscalationsRequest{ApprovedUnexecuted: true, DryRun: dryRun})
		if err != nil {
			return err
		}
		verb := "Rerouted"
		if res.DryRun {
			verb = "Would reroute"
		}
		for _, r := range res.Rerouted {
			fmt.Printf("%s %s: execute_by %s -> %s (held by %s)\n", verb, r.ID, r.From, r.ExecuteBy, r.HeldBy)
		}
		fmt.Printf("%s %d of %d open escalations\n", verb, len(res.Rerouted), res.Considered)
		if res.Truncated {
			fmt.Println("The daemon's working set was capped; run it again for the rest.")
		}
		return nil
	},
}
