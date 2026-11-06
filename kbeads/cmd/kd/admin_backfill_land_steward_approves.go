package main

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/cobra"
)

// defaultLandStewardApproves mirrors the daemon's defaultLandStewardApproves
// (kbeads/internal/server/land_steward_delegation.go), which it stamps on a
// tenant land's create (owner decision gd-fBSlZfEvmo, task gd-oIa8qydN1w).
var defaultLandStewardApproves = []string{"github_pr_merge"}

// adminBackfillLandStewardApprovesCmd gives the lands created before the
// daemon stamped the default the same default, once. A land whose project
// bead carries the key at all, an explicit [] included, is the owner's choice
// and is left alone; the write goes through the owner endpoint, so it is
// validated and audited like an owner's.
var adminBackfillLandStewardApprovesCmd = &cobra.Command{
	Use:   "backfill-land-steward-approves",
	Short: "Give tenant lands without land_steward_approves the default [github_pr_merge]",
	Long: `Lists the open type=project beads with land_kind=tenant and, for each one whose
fields have no land_steward_approves key, sets it to ["github_pr_merge"] through
POST /v1/lands/{land}/steward-delegation. A land with any value, [] included, is
skipped. Needs a gardenio-admin bearer (the endpoint admits the land owner or an
admin). Dry run by default; --apply writes. Idempotent: a re-run finds nothing.`,
	RunE: runAdminBackfillLandStewardApproves,
}

func init() {
	adminBackfillLandStewardApprovesCmd.Flags().Bool("apply", false, "write the default (default: print what would change)")
	adminCmd.AddCommand(adminBackfillLandStewardApprovesCmd)
}

// landNeedsStewardDefault reports whether a project bead is a tenant land with
// no land_steward_approves key. The key is matched in any case, as the
// daemon's encoding/json decode of it is.
func landNeedsStewardDefault(b *model.Bead) bool {
	if b == nil || b.Type != model.TypeProject || len(b.Fields) == 0 {
		return false
	}
	var f map[string]json.RawMessage
	if err := json.Unmarshal(b.Fields, &f); err != nil {
		return false
	}
	var kind string
	if err := json.Unmarshal(f["land_kind"], &kind); err != nil || kind != "tenant" {
		return false
	}
	for k := range f {
		if strings.EqualFold(k, "land_steward_approves") {
			return false
		}
	}
	return true
}

func runAdminBackfillLandStewardApproves(cmd *cobra.Command, _ []string) error {
	apply, _ := cmd.Flags().GetBool("apply")
	ctx := context.Background()

	const pageSize = 200
	var projects []*model.Bead
	for offset := 0; ; {
		resp, err := beadsClient.ListBeads(ctx, &client.ListBeadsRequest{
			Type:         []string{string(model.TypeProject)},
			Status:       []string{string(model.StatusOpen)},
			FieldFilters: map[string]string{"land_kind": "tenant"},
			Limit:        pageSize,
			Offset:       offset,
		})
		if err != nil {
			return fmt.Errorf("listing tenant lands: %w", err)
		}
		projects = append(projects, resp.Beads...)
		if len(resp.Beads) < pageSize {
			break
		}
		offset += len(resp.Beads)
	}

	var lands []string
	skipped := 0
	for _, b := range projects {
		if landNeedsStewardDefault(b) {
			lands = append(lands, strings.TrimPrefix(b.Title, "Project: "))
		} else {
			skipped++
		}
	}
	slices.Sort(lands)

	ops := strings.Join(defaultLandStewardApproves, ",")
	for _, land := range lands {
		if !apply {
			fmt.Printf("would set %s: land_steward_approves=[%s]\n", land, ops)
			continue
		}
		if err := beadsClient.SetStewardDelegation(ctx, land, defaultLandStewardApproves); err != nil {
			return fmt.Errorf("land %s: %w", land, err)
		}
		fmt.Printf("set %s: land_steward_approves=[%s]\n", land, ops)
	}
	verb := "would set"
	if apply {
		verb = "set"
	}
	fmt.Printf("%s %d land(s); %d already have a value (left alone)\n", verb, len(lands), skipped)
	return nil
}
