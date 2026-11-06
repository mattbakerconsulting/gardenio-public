package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func landBead(title string, fields map[string]any) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{ID: "proj-" + title, Type: model.TypeProject, Title: title, Fields: raw}
}

// The backfill sets the default only where the key is absent: an explicit []
// (the owner's revoke) and any other value survive, and an operator land is
// never touched.
func TestAdminBackfillLandStewardApproves(t *testing.T) {
	beads := []*model.Bead{
		landBead("phonobaloon", map[string]any{"land_kind": "tenant"}),
		landBead("revoked", map[string]any{"land_kind": "tenant", "land_steward_approves": []string{}}),
		landBead("wider", map[string]any{"land_kind": "tenant", "land_steward_approves": []string{"github_pr_merge", "create"}}),
		landBead("Project: oldstyle", map[string]any{"land_kind": "tenant"}),
		landBead("gardenio", map[string]any{"land_kind": "operator"}),
		landBead("legacy", map[string]any{}),
	}
	for _, apply := range []bool{false, true} {
		mc := newMockClient()
		mc.ListBeadsResult = beads
		origClient := beadsClient
		beadsClient = mc
		_ = adminBackfillLandStewardApprovesCmd.Flags().Set("apply", map[bool]string{false: "false", true: "true"}[apply])
		out := captureStdout(t, func() {
			if err := adminBackfillLandStewardApprovesCmd.RunE(adminBackfillLandStewardApprovesCmd, nil); err != nil {
				t.Fatalf("RunE: %v", err)
			}
		})
		beadsClient = origClient

		if got := mc.ListBeadsCalls[0].FieldFilters["land_kind"]; got != "tenant" {
			t.Errorf("list filter land_kind = %q, want tenant", got)
		}
		var want []string
		if apply {
			want = []string{"oldstyle=github_pr_merge", "phonobaloon=github_pr_merge"}
		}
		if !slices.Equal(mc.SetStewardDelegationCalls, want) {
			t.Errorf("apply=%v: delegation writes = %v, want %v", apply, mc.SetStewardDelegationCalls, want)
		}
		for _, s := range []string{"phonobaloon", "oldstyle", "2 land(s)", "4 already have a value"} {
			if !strings.Contains(out, s) {
				t.Errorf("apply=%v: output lacks %q:\n%s", apply, s, out)
			}
		}
		for _, s := range []string{"revoked", "wider", "gardenio", "legacy"} {
			if strings.Contains(out, s+":") {
				t.Errorf("apply=%v: output names %s, which keeps its value:\n%s", apply, s, out)
			}
		}
	}
	_ = adminBackfillLandStewardApprovesCmd.Flags().Set("apply", "false")
}

func TestLandNeedsStewardDefault_CaseVariantKey(t *testing.T) {
	b := landBead("x", map[string]any{"land_kind": "tenant", "Land_Steward_Approves": []string{}})
	if landNeedsStewardDefault(b) {
		t.Error("a case-variant key is a value the daemon reads; the land is left alone")
	}
}
