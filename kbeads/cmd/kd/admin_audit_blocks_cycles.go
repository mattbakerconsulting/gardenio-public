package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// adminAuditBlocksCyclesCmd drives ka-0000007984's read-only audit of the
// blocks-edge dependency graph. A cycle in the blocks graph (e.g. the mutual
// pair A blocked-by B, B blocked-by A) makes every bead in and downstream of
// the cycle permanently undispatchable — no dispatcher lane can ever pick them
// up, and nothing else surfaces the condition: each bead individually looks
// like an ordinary, legitimately-blocked bead. A concrete 2-cycle pinned six
// gardenio beads for 12 days before it was found by hand.
//
// The server scans the whole blocks graph and returns every cycle
// (strongly-connected tangle) with its member bead IDs, so an operator can
// break the stale edge with `kd dep remove <blocked> <blocker> -t blocks`.
// The write-time guard (same bead) already refuses NEW cycle-forming edges;
// this command finds the ones already in the live graph.
var adminAuditBlocksCyclesCmd = &cobra.Command{
	Use:   "audit-blocks-cycles",
	Short: "Report cycles in the blocks dependency graph",
	Long: `Scans the entire blocks-edge dependency graph and reports every cycle
present. A blocks cycle — the simplest being the mutual pair A blocked-by B,
B blocked-by A — pins every bead in and downstream of the cycle as
permanently undispatchable, and nothing else surfaces the condition (each
bead individually looks like an ordinary, legitimately-blocked bead).

Detection uses Tarjan's strongly-connected-components algorithm, so each
reported cycle is a maximal tangle of mutually-blocking beads (one stable
group per tangle, not an exponential enumeration of simple cycles). Members
are the bead IDs to inspect; break the stale edge with:

  kd dep remove <blocked-id> <blocker-id> -t blocks

Output is a JSON manifest: edge/node/cycle counts plus each cycle's sorted
member set. Exit code is 0 when the graph is clean and 2 when one or more
cycles are found (unless --exit-zero), so a CI/cron gate can fail on a dirty
graph. The companion write-time guard already refuses any NEW blocks edge
that would close a cycle; this command finds cycles already in the graph.`,
	RunE: runAdminAuditBlocksCycles,
}

func init() {
	adminAuditBlocksCyclesCmd.Flags().Bool("exit-zero", false, "always exit 0, even when cycles are found (default: exit 2 on cycles)")
	adminCmd.AddCommand(adminAuditBlocksCyclesCmd)
}

func runAdminAuditBlocksCycles(cmd *cobra.Command, _ []string) error {
	exitZero, _ := cmd.Flags().GetBool("exit-zero")

	ctx := context.Background()
	start := time.Now()

	resp, err := beadsClient.AuditBlocksCycles(ctx)
	if err != nil {
		return fmt.Errorf("audit-blocks-cycles: %w", err)
	}

	manifest := map[string]any{
		"edge_count":     resp.EdgeCount,
		"node_count":     resp.NodeCount,
		"cycle_count":    resp.CycleCount,
		"cycles":         resp.Cycles,
		"client_wall_ms": time.Since(start).Milliseconds(),
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}

	// A dirty graph exits non-zero so a cron/CI gate can act on it, unless the
	// caller opted out. SilenceUsage/SilenceErrors keep the JSON manifest the
	// sole stdout artifact — no cobra usage spew on the non-zero exit.
	if resp.CycleCount > 0 && !exitZero {
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		return &exitCodeError{code: 2, msg: fmt.Sprintf("%d blocks cycle(s) found", resp.CycleCount)}
	}
	return nil
}
