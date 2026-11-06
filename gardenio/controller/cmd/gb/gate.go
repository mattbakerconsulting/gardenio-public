package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

var gateAgentID string

var gateCmd = &cobra.Command{
	Use:     "gate",
	Short:   "Manage session gates",
	GroupID: "orchestration",
}

var gateStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show gate state for the current agent",
	RunE: func(cmd *cobra.Command, args []string) error {
		agentID, err := resolveGateAgentID(cmd)
		if err != nil {
			return err
		}

		gates, err := daemon.ListGates(cmd.Context(), agentID)
		if err != nil {
			return fmt.Errorf("listing gates: %w", err)
		}

		if jsonOutput {
			printJSON(gates)
			return nil
		}

		if len(gates) == 0 {
			fmt.Printf("No gates found for agent %s.\n", agentID)
			return nil
		}

		fmt.Printf("Session gates for agent %s:\n", agentID)
		for _, g := range gates {
			var bullet string
			var detail string
			if g.Status == "satisfied" {
				bullet = "●"
				if g.SatisfiedAt != nil {
					detail = fmt.Sprintf(" (%s)", g.SatisfiedAt.Format("2006-01-02 15:04:05"))
				}
			} else {
				bullet = "○"
			}
			fmt.Printf("  %s %s: %s%s\n", bullet, g.GateID, g.Status, detail)
		}
		return nil
	},
}

var gateMarkForce bool

var gateMarkCmd = &cobra.Command{
	Use:   "mark <gate-id>",
	Short: "Manually mark a gate as satisfied (operator use only for decision gate)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gateID := args[0]

		// Reject manual marking of the decision gate without --force.
		// The decision gate must be satisfied via 'gb yield' (after 'gb decision create')
		// so the Slack bridge always has a decision bead handle to re-engage the agent.
		if gateID == "decision" && !gateMarkForce {
			fmt.Fprintf(os.Stderr, "ERROR: 'gb gate mark decision' bypasses the stop gate without a decision bead.\n")
			fmt.Fprintf(os.Stderr, "       This silently cuts off the Slack operator's re-entry handle.\n")
			fmt.Fprintf(os.Stderr, "       Use 'gb yield' instead (requires an open decision bead).\n")
			fmt.Fprintf(os.Stderr, "       Operators only: use --force to override.\n")
			return fmt.Errorf("decision gate requires --force to mark manually (use 'gb yield' instead)")
		}

		agentID, err := resolveGateAgentID(cmd)
		if err != nil {
			return err
		}

		if gateID == "decision" && gateMarkForce {
			fmt.Fprintln(os.Stderr, "WARNING: manually satisfying decision gate with --force.")
			fmt.Fprintln(os.Stderr, "The Slack bridge will not have a re-entry handle until the next decision bead is created.")
		}

		if err := daemon.SatisfyGate(cmd.Context(), agentID, gateID); err != nil {
			return fmt.Errorf("satisfying gate: %w", err)
		}

		// For the decision gate with --force, record the method so stop-gate.sh
		// can recognize this as an authorized operator override.
		if gateID == "decision" && gateMarkForce {
			markCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := daemon.UpdateBeadFields(markCtx, agentID, map[string]string{
				"gate_satisfied_by": "operator",
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to set gate_satisfied_by: %v\n", err)
			}
		}

		fmt.Printf("✓ Gate %s marked as satisfied\n", gateID)
		return nil
	},
}

var gateClearCmd = &cobra.Command{
	Use:   "clear <gate-id>",
	Short: "Clear a gate (satisfy as operator so stop hook allows through persistently)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		gateID := args[0]
		agentID, err := resolveGateAgentID(cmd)
		if err != nil {
			return err
		}

		// Satisfy the gate instead of resetting to pending. The stop hook handler
		// in http_hooks.go skips gate consumption for operator-satisfied gates,
		// so this persists across repeated stop attempts (e.g. idle thread agents).
		if err := daemon.SatisfyGate(cmd.Context(), agentID, gateID); err != nil {
			return fmt.Errorf("clearing gate: %w", err)
		}

		// Set gate_satisfied_by=operator so the stop hook handler recognizes this
		// as an authorized override and does not consume the gate.
		if gateID == "decision" {
			clearCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := daemon.UpdateBeadFields(clearCtx, agentID, map[string]string{
				"gate_satisfied_by": "operator",
			}); err != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to set gate_satisfied_by: %v\n", err)
			}
		}

		fmt.Printf("○ Gate %s cleared (pending)\n", gateID)
		return nil
	},
}

// gateSatisfiedByCmd checks how the decision gate was satisfied.
// Exits 0 if satisfied via 'gb yield' (gate_satisfied_by=yield) or operator --force
// (gate_satisfied_by=operator). Exits 1 if not set or unrecognised value.
// Used by stop-gate.sh to distinguish proper yield from manual bypass.
var gateSatisfiedByCmd = &cobra.Command{
	Use:   "satisfied-by",
	Short: "Check how the decision gate was satisfied (exit 1 if not via yield/operator)",
	RunE: func(cmd *cobra.Command, args []string) error {
		agentID, err := resolveGateAgentID(cmd)
		if err != nil {
			return err
		}

		bead, err := daemon.GetBead(cmd.Context(), agentID)
		if err != nil {
			return fmt.Errorf("fetching agent bead: %w", err)
		}

		method := bead.Fields["gate_satisfied_by"]
		if method == "yield" || method == "operator" || method == "ready" {
			fmt.Println(method)
			return nil
		}

		// Not satisfied via a recognized method.
		if method == "" {
			fmt.Fprintln(os.Stderr, "gate_satisfied_by not set — gate was not satisfied via 'gb yield'")
		} else {
			fmt.Fprintf(os.Stderr, "gate_satisfied_by=%s — unrecognised method\n", method)
		}
		os.Exit(1)
		return nil
	},
}

// resolveGateAgentID resolves the agent bead ID for gate operations.
func resolveGateAgentID(cmd *cobra.Command) (string, error) {
	return resolveAgentIDWithFallback(cmd.Context(), gateAgentID)
}

func init() {
	gateCmd.PersistentFlags().StringVar(&gateAgentID, "agent-id", "", "agent bead ID (default: KD_AGENT_ID env)")

	gateMarkCmd.Flags().BoolVar(&gateMarkForce, "force", false, "bypass decision-gate protection (operator use only)")

	gateCmd.AddCommand(gateStatusCmd)
	gateCmd.AddCommand(gateMarkCmd)
	gateCmd.AddCommand(gateClearCmd)
	gateCmd.AddCommand(gateSatisfiedByCmd)
}
