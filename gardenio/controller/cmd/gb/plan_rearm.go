// `gb plan rearm` — reopen a plan's own sealed gardener instead of binding a
// new one (gd-miuZqFACwL).
//
// The engine closes a plan's gardener when its budget is spent or it hard
// stops, and the trigger keeps the seal its owner gave it. The home steward
// re-arms it by reopening the SAME trigger with enabled=true and nothing else;
// the daemon admits that write for the home steward only (row steward-rearm,
// kbeads spawn_lineage_rearm.go) and keeps the seal, so the next fire passes
// the lineage gate. The engine then refills the run budget on its next cycle
// (gardenerRearmFromOperatorReopen), keyed on the terminate marker the write
// leaves in place.
package main

import (
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/plangardener"
	"gardenio/controller/internal/steward/gardenerwatch"

	"github.com/spf13/cobra"
)

var planRearmCmd = &cobra.Command{
	Use:   "rearm <plan-epic-id>",
	Short: "Reopen the plan's last gardener, keeping its owner seal",
	Long: `Reopen and re-enable the plan gardener the engine closed last, so the
plan keeps the trigger its owner sealed instead of getting a new one.

Refused when the plan already has an open gardener (there is nothing to
re-arm), when its last gardener was closed by hand rather than by the
engine, or when that gardener carries no valid owner seal: bind a new one
with 'gb plan gardener <plan>' then. The daemon admits the reopen for the
home garden's steward (and an admin's own session).

After it succeeds, fire it: gb gardener fire-now <gardener>.`,
	Args: cobra.ExactArgs(1),
	RunE: runPlanRearm,
}

func init() {
	planCmd.AddCommand(planRearmCmd)
	planRearmCmd.SilenceUsage = true
}

func runPlanRearm(cmd *cobra.Command, args []string) error {
	planID := strings.TrimSpace(args[0])
	if planID == "" {
		return fmt.Errorf("plan epic id is required")
	}
	ctx := cmd.Context()
	if open, err := plangardener.FindAnyGardenerForEpic(ctx, daemon, planID); err != nil {
		return err
	} else if open != nil {
		if jsonOutput {
			printJSON(map[string]any{"plan": planID, "id": open.ID, "already_open": true})
			return nil
		}
		fmt.Printf("plan %s already has an open gardener: %s (nothing to re-arm)\n", planID, open.ID)
		return nil
	}
	last, err := gardenerwatch.LastGardener(ctx, daemon, planID)
	if err != nil {
		return err
	}
	if last == nil {
		return fmt.Errorf("plan %s has no closed gardener to re-arm; bind one: gb plan gardener %s", planID, planID)
	}
	// GET by id: a list row does not carry the daemon's spawn_lineage_digest.
	g, err := daemon.GetBead(ctx, last.ID)
	if err != nil {
		return fmt.Errorf("reading gardener %s: %w", last.ID, err)
	}
	if reason := rearmRefusal(g); reason != "" {
		return fmt.Errorf("cannot re-arm gardener %s of plan %s: %s; bind a new one: gb plan gardener %s", g.ID, planID, reason, planID)
	}
	open := "open"
	if err := daemon.UpdateBead(ctx, g.ID, beadsapi.UpdateBeadRequest{Status: &open, Fields: map[string]any{"enabled": true}}); err != nil {
		return fmt.Errorf("re-arming gardener %s: %w", g.ID, err)
	}
	kind := strings.TrimSpace(g.Fields["gardener_terminated_kind"])
	if jsonOutput {
		printJSON(map[string]any{"plan": planID, "id": g.ID, "rearmed": true, "stopped_on": kind})
		return nil
	}
	fmt.Printf("Re-armed plan gardener %s for %s (it stopped on %s); seal kept\n", g.ID, planID, defaultIfEmpty(kind, "an engine stop"))
	fmt.Printf("  fire it: gb gardener fire-now %s\n", g.ID)
	return nil
}

// rearmRefusal is why the daemon would refuse to re-arm g, or "". It mirrors
// the bead side of kbeads homeStewardRearmsPlanGardener that a client can see.
func rearmRefusal(g *beadsapi.BeadDetail) string {
	switch {
	case strings.TrimSpace(g.Fields["role"]) != plangardener.SpawnRole:
		return "it does not spawn " + plangardener.SpawnRole
	case !engineClosed(g):
		return "it was closed by hand, not by the engine"
	case !spawnSealValid(g):
		return "it carries no valid owner seal"
	}
	return ""
}

// engineClosed reports whether g carries a terminate marker, which the engine
// stamps only in the write that closes a gardener.
func engineClosed(g *beadsapi.BeadDetail) bool {
	for _, k := range []string{"gardener_terminated_kind", "gardener_terminated_slot", "gardener_stuck_escalated"} {
		if v := strings.TrimSpace(g.Fields[k]); v != "" && v != "false" {
			return true
		}
	}
	return false
}
