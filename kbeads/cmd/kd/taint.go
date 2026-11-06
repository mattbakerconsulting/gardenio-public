package main

import (
	"context"
	"fmt"
	"time"

	"gardenio/kbeads/internal/client"
	"github.com/spf13/cobra"
)

// taintCmd is a parent group, but it MUST be runnable (have a RunE) so that
// cobra reaches the Args validator below — a non-runnable parent
// short-circuits to flag.ErrHelp BEFORE ValidateArgs runs, printing help to
// STDOUT and exiting 0 (ka-0000005587 / ka-0000003820 shape). With a RunE
// present, cobra.NoArgs rejects any UNKNOWN subcommand with a non-zero exit;
// a bare `kd taint` passes NoArgs and falls through to RunE → help.
var taintCmd = &cobra.Command{
	Use:     "taint",
	Short:   "Manage taints: grants, explicit apply, per-bead show",
	GroupID: "beads",
	Args:    cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var taintGrantCmd = &cobra.Command{
	Use:   "grant <name> <principal>",
	Short: "Grant a taint to a principal (team:<id>, operator:<id-or-alias>, agent:<bead-id>, fleet:<name>)",
	Long: `Grant a taint to a principal, authorizing it to read beads carrying the
taint (and to explicitly apply the taint). Restricted to the taint's owner
team or an admin.

<principal> is "<kind>:<value>", one of:
  team:<team-bead-id>       no expiry unless --until is given
  operator:<id-or-alias>    defaults to a 90-day expiry, renewable
  agent:<agent-bead-id>     ALWAYS expires at the agent bead's close;
                            any --until is ignored (docs/adr/taints.md §5.2)
  fleet:<name>              no expiry unless --until is given

granted_by is always the authenticated caller — never client-supplied.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		name := args[0]
		principal := args[1]
		until, _ := cmd.Flags().GetString("until")

		req := &client.GrantTaintRequest{Principal: principal}
		if until != "" {
			t, err := time.Parse(time.RFC3339, until)
			if err != nil {
				return fmt.Errorf("parsing --until: %w", err)
			}
			req.Until = &t
		}

		grant, err := beadsClient.GrantTaint(ctx, name, req)
		if err != nil {
			return fmt.Errorf("granting taint %q to %s: %w", name, principal, err)
		}

		if jsonOutput {
			emitJSON(grant)
		} else {
			fmt.Printf("Taint:        %s\n", name)
			fmt.Printf("Principal:    %s:%s\n", grant.PrincipalKind, grant.Principal)
			fmt.Printf("Granted By:   %d\n", grant.GrantedBy)
			fmt.Printf("Granted At:   %s\n", grant.GrantedAt.Format("2006-01-02 15:04:05"))
			if grant.ExpiresAt != nil {
				fmt.Printf("Expires At:   %s\n", grant.ExpiresAt.Format("2006-01-02 15:04:05"))
			} else {
				fmt.Printf("Expires At:   (never)\n")
			}
		}
		return nil
	},
}

var taintRevokeCmd = &cobra.Command{
	Use:   "revoke <name> <principal>",
	Short: "Revoke a taint grant from a principal",
	Long: `Revoke a taint grant. Restricted to the taint's owner team or an admin.

<principal> is "<kind>:<value>", matching what was passed to grant.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		name := args[0]
		principal := args[1]

		removed, err := beadsClient.RevokeTaint(ctx, name, principal)
		if err != nil {
			return fmt.Errorf("revoking taint %q from %s: %w", name, principal, err)
		}

		if jsonOutput {
			emitJSON(map[string]any{"taint": name, "principal": principal, "removed": removed})
			return nil
		}
		if removed {
			fmt.Printf("Revoked %s from %s\n", name, principal)
		} else {
			fmt.Printf("No grant for %s on %s (nothing to revoke)\n", principal, name)
		}
		return nil
	},
}

var taintApplyCmd = &cobra.Command{
	Use:   "apply <bead-id> <name>",
	Short: "Explicitly apply a taint to a bead",
	Long: `Explicitly apply a taint to a bead. Requires the caller hold a grant for
the taint (or the taint owner team's implicit grant). Writes a bead_taints
row with source=explicit; applied_by is server-derived, never client-supplied.`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		beadID := args[0]
		name := args[1]

		bt, err := beadsClient.ApplyTaint(ctx, beadID, name)
		if err != nil {
			return fmt.Errorf("applying taint %q to %s: %w", name, beadID, err)
		}

		if jsonOutput {
			emitJSON(bt)
		} else {
			fmt.Printf("Bead:        %s\n", bt.BeadID)
			fmt.Printf("Taint:       %s\n", bt.TaintName)
			fmt.Printf("Source:      %s\n", bt.Source)
			if bt.SourceBeadID != "" {
				fmt.Printf("Source Bead: %s\n", bt.SourceBeadID)
			}
			fmt.Printf("Applied By:  %s\n", bt.AppliedBy)
			fmt.Printf("Applied At:  %s\n", bt.AppliedAt.Format("2006-01-02 15:04:05"))
		}
		return nil
	},
}

var taintGrantRequestCmd = &cobra.Command{
	Use:   "grant-request <name>",
	Short: "File a grant request for a taint, addressed to its owner team",
	Long: `File a grant request for a taint without needing to know who owns it
(ADR docs/adr/taints.md §9.2/§9.4). Creates a "grant"-type escalation
addressed to the taint's owner team; approving it writes a taint_grants row
for the requester with the expiry the approver chooses, defaulting to 7 days.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		name := args[0]
		reason, _ := cmd.Flags().GetString("reason")

		esc, err := beadsClient.RequestTaintGrant(ctx, name, &client.RequestTaintGrantRequest{Reason: reason})
		if err != nil {
			return fmt.Errorf("requesting grant for taint %q: %w", name, err)
		}

		if jsonOutput {
			emitJSON(esc)
			return nil
		}
		fmt.Printf("Grant request created: %s\n", esc.EscalationID)
		fmt.Printf("  Taint:  %s\n", name)
		fmt.Printf("  Status: pending owner-team approval\n")
		return nil
	},
}

var taintShowCmd = &cobra.Command{
	Use:   "show <bead-id>",
	Short: "List every taint applied to a bead",
	Long: `List every taint applied to a bead, with its source and source_bead_id.
Available to any principal that can already read the bead — not just grant
holders.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx := context.Background()
		beadID := args[0]

		taints, err := beadsClient.GetBeadTaints(ctx, beadID)
		if err != nil {
			return fmt.Errorf("getting taints for %s: %w", beadID, err)
		}

		if jsonOutput {
			emitJSON(taints)
			return nil
		}
		if len(taints) == 0 {
			fmt.Printf("No taints on %s\n", beadID)
			return nil
		}
		for i, bt := range taints {
			fmt.Printf("Taint:       %s\n", bt.TaintName)
			fmt.Printf("Source:      %s\n", bt.Source)
			if bt.SourceBeadID != "" {
				fmt.Printf("Source Bead: %s\n", bt.SourceBeadID)
			}
			fmt.Printf("Applied By:  %s\n", bt.AppliedBy)
			fmt.Printf("Applied At:  %s\n", bt.AppliedAt.Format("2006-01-02 15:04:05"))
			if i < len(taints)-1 {
				fmt.Println()
			}
		}
		return nil
	},
}

func init() {
	taintCmd.AddCommand(taintGrantCmd)
	taintCmd.AddCommand(taintGrantRequestCmd)
	taintCmd.AddCommand(taintRevokeCmd)
	taintCmd.AddCommand(taintApplyCmd)
	taintCmd.AddCommand(taintShowCmd)
	// Registry commands (task gd-sygX3b8TAg). `show` was ALSO the name this
	// task gave its by-name view of a taint definition; ADR §7 listed both
	// meanings under `show`, which is how two crews shipped the same command.
	// Resolved in favour of §9's everyday question -- `show <bead-id>` answers
	// "what is on this bead" -- and the definition view is `describe <name>`.
	taintCmd.AddCommand(taintCreateCmd)
	taintCmd.AddCommand(taintListCmd)
	taintCmd.AddCommand(taintDescribeCmd)

	taintGrantCmd.Flags().String("until", "",
		"grant expiry (RFC3339, e.g. 2025-03-01T00:00:00Z); defaults per docs/adr/taints.md §10.4; always ignored for an agent principal")

	taintGrantRequestCmd.Flags().String("reason", "", "optional free text recorded on the escalation for the approving team's context")
}

// resolveTaintOwnerTeam resolves a --owner value to the type:team bead id
// SetTaint expects as owner_team, accepting either the team's human-facing
// name (Fields.team, as `kd team create <name>` sets it) or the bead's raw
// id directly.
func resolveTaintOwnerTeam(ctx context.Context, ref string) (string, error) {
	if bead, err := findTeamByName(ctx, ref); err == nil {
		return bead.ID, nil
	}
	bead, err := beadsClient.GetBead(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolving --owner %q: no team found by that name or id", ref)
	}
	if bead.Type != "team" {
		return "", fmt.Errorf("resolving --owner %q: bead %s is type %q, not team", ref, bead.ID, bead.Type)
	}
	return bead.ID, nil
}
