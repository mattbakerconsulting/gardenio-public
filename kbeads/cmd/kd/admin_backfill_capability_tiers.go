package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// adminBackfillCapabilityTiersCmd stamps capability_tiers onto every live
// type:role bead from its privileged flag (P9 gd-Bcqj2r4m7E). The daemon keeps
// the two in step on every role write from now on
// (model.NormalizeRoleCapabilityTiers); this reaches the role beads nobody has
// written since. privileged stays the source of truth for the backfill: a role
// with privileged=true gets the privileged tier, any other tier it already
// holds is kept.
//
// DRY-RUN BY DEFAULT, like gardener-owner: it writes only with --apply. Role
// beads are admin-gated config, so --apply needs an admin login.
var adminBackfillCapabilityTiersCmd = &cobra.Command{
	Use:   "capability-tiers",
	Short: "Stamp capability_tiers onto live role beads from their privileged flag",
	Long: `Walks every live type:role bead (open, in_progress, blocked, deferred) and
stamps capability_tiers from its privileged flag: ["privileged"] for a
privileged role, [] for any other. Tiers a role already holds are kept; only
the privileged tier is added or removed to match the flag.

A role whose capability_tiers is already set and agrees with privileged is
skipped (SKIP_IDEMPOTENT), so a re-run after an apply writes nothing. A role
whose stored capability_tiers is not an array of tier names is reported as
ERROR and left for a human.

DRY-RUN IS THE DEFAULT: with no flags the command prints the
BEAD_ID/ROLE/PRIVILEGED/TIERS/STATUS rows and writes nothing. Pass --apply to
write (needs gardenio-admin). Exit is non-zero on any ERROR row.`,
	RunE: runAdminBackfillCapabilityTiers,
}

func init() {
	adminBackfillCapabilityTiersCmd.Flags().Bool("apply", false, "write the tiers (default: dry run)")
	adminBackfillCapabilityTiersCmd.Flags().Int("page-size", 200, "role beads per ListBeads page")
	adminBackfillCmd.AddCommand(adminBackfillCapabilityTiersCmd)
}

type capabilityTiersStatus string

const (
	capTiersWouldUpdate    capabilityTiersStatus = "WOULD_UPDATE"
	capTiersUpdated        capabilityTiersStatus = "UPDATED"
	capTiersSkipIdempotent capabilityTiersStatus = "SKIP_IDEMPOTENT"
	capTiersError          capabilityTiersStatus = "ERROR"
)

// capabilityTiersPlan is one role bead's row.
type capabilityTiersPlan struct {
	BeadID     string
	Role       string
	Privileged bool
	Tiers      []string
	Status     capabilityTiersStatus
	Err        string
	patch      json.RawMessage
}

func runAdminBackfillCapabilityTiers(cmd *cobra.Command, _ []string) error {
	apply, _ := cmd.Flags().GetBool("apply")
	pageSize, _ := cmd.Flags().GetInt("page-size")
	if pageSize <= 0 {
		pageSize = 200
	}
	ctx := context.Background()

	var plans []capabilityTiersPlan
	for offset := 0; ; {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:   []string{"role"},
			Status: []string{string(model.StatusOpen), string(model.StatusInProgress), string(model.StatusBlocked), string(model.StatusDeferred)},
			Limit:  pageSize,
			Offset: offset,
			Sort:   "created_at",
		})
		if err != nil {
			return fmt.Errorf("listing role beads (offset=%d): %w", offset, err)
		}
		for _, b := range resp.Beads {
			plans = append(plans, planCapabilityTiers(b))
		}
		// A write never moves a role out of the listed statuses, so the
		// offset advances by the full page in both modes.
		offset += len(resp.Beads)
		if len(resp.Beads) < pageSize {
			break
		}
	}

	failed := 0
	for i := range plans {
		p := &plans[i]
		if p.Status != capTiersWouldUpdate || !apply {
			if p.Status == capTiersError {
				failed++
			}
			continue
		}
		if _, err := beadsClient.UpdateBead(ctx, p.BeadID, &client.UpdateBeadRequest{Fields: p.patch}); err != nil {
			p.Status = capTiersError
			p.Err = err.Error()
			failed++
			continue
		}
		p.Status = capTiersUpdated
	}

	printCapabilityTiersPlans(plans, apply)
	if failed > 0 {
		return fmt.Errorf("%d role bead(s) not backfilled", failed)
	}
	return nil
}

// planCapabilityTiers decides one role bead's row: the tier set its privileged
// flag implies (keeping any other tier it holds), and whether writing it would
// change anything.
func planCapabilityTiers(b *model.Bead) capabilityTiersPlan {
	fields := decodeFields(b.Fields)
	privileged := model.RolePrivilegedFlag(fields)
	plan := capabilityTiersPlan{BeadID: b.ID, Role: b.Title, Privileged: privileged}

	// The daemon's own rule computes the set: a write that sets only
	// privileged adds or removes the privileged tier on the stored set.
	merged := make(map[string]any, len(fields))
	for k, v := range fields {
		merged[k] = v
	}
	if err := model.NormalizeRoleCapabilityTiers(merged, map[string]any{model.RolePrivilegedField: privileged}); err != nil {
		plan.Status = capTiersError
		plan.Err = err.Error()
		return plan
	}
	tiers, _ := model.ParseCapabilityTiers(merged[model.RoleCapabilityTiersField])
	plan.Tiers = tiers

	if stored, ok := fields[model.RoleCapabilityTiersField]; ok {
		if have, ok := model.ParseCapabilityTiers(stored); ok && strings.Join(have, ",") == strings.Join(tiers, ",") {
			plan.Status = capTiersSkipIdempotent
			return plan
		}
	}
	patch, err := json.Marshal(map[string]any{
		model.RoleCapabilityTiersField: tiers,
		model.RolePrivilegedField:      privileged,
	})
	if err != nil {
		plan.Status = capTiersError
		plan.Err = err.Error()
		return plan
	}
	plan.patch = patch
	plan.Status = capTiersWouldUpdate
	return plan
}

func printCapabilityTiersPlans(plans []capabilityTiersPlan, apply bool) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "BEAD_ID\tROLE\tPRIVILEGED\tTIERS\tSTATUS")
	counts := map[capabilityTiersStatus]int{}
	for _, p := range plans {
		counts[p.Status]++
		tiers := "[" + strings.Join(p.Tiers, ",") + "]"
		status := string(p.Status)
		if p.Err != "" {
			status += ": " + p.Err
		}
		fmt.Fprintf(tw, "%s\t%s\t%t\t%s\t%s\n", p.BeadID, p.Role, p.Privileged, tiers, status)
	}
	_ = tw.Flush()
	mode := "dry run (pass --apply to write)"
	if apply {
		mode = "applied"
	}
	fmt.Printf("\n%s: %d role bead(s); %d would update, %d updated, %d already set, %d error\n",
		mode, len(plans), counts[capTiersWouldUpdate], counts[capTiersUpdated], counts[capTiersSkipIdempotent], counts[capTiersError])
}
