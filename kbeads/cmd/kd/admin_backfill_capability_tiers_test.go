package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func capTiersRoleBead(id, title, fields string) *model.Bead {
	return &model.Bead{ID: id, Title: title, Type: "role", Kind: model.KindConfig, Status: model.StatusOpen, Fields: json.RawMessage(fields)}
}

func TestPlanCapabilityTiers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		fields    string
		status    capabilityTiersStatus
		wantTiers []string
	}{
		{"privileged legacy", `{"privileged":true}`, capTiersWouldUpdate, []string{"privileged"}},
		{"privileged text legacy", `{"privileged":"true"}`, capTiersWouldUpdate, []string{"privileged"}},
		{"unprivileged legacy", `{"image":"x"}`, capTiersWouldUpdate, []string{}},
		{"already set", `{"privileged":true,"capability_tiers":["privileged"]}`, capTiersSkipIdempotent, []string{"privileged"}},
		{"already empty", `{"capability_tiers":[]}`, capTiersSkipIdempotent, []string{}},
		// privileged is the source of truth for the backfill; other tiers stay.
		{"set missing the flag's tier", `{"privileged":true,"capability_tiers":["lands-tenant"]}`, capTiersWouldUpdate, []string{"lands-tenant", "privileged"}},
		{"set holding a tier the flag denies", `{"privileged":false,"capability_tiers":["privileged","lands-tenant"]}`, capTiersWouldUpdate, []string{"lands-tenant"}},
		{"malformed set", `{"privileged":true,"capability_tiers":"privileged"}`, capTiersError, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := planCapabilityTiers(capTiersRoleBead("gd-r", "crew", tc.fields))
			if p.Status != tc.status {
				t.Fatalf("status %s, want %s (%s)", p.Status, tc.status, p.Err)
			}
			if tc.status != capTiersError && !reflect.DeepEqual(p.Tiers, tc.wantTiers) {
				t.Fatalf("tiers %v, want %v", p.Tiers, tc.wantTiers)
			}
			if tc.status == capTiersWouldUpdate {
				var patch map[string]any
				if err := json.Unmarshal(p.patch, &patch); err != nil {
					t.Fatalf("patch: %v", err)
				}
				tiers, _ := model.ParseCapabilityTiers(patch[model.RoleCapabilityTiersField])
				if patch[model.RolePrivilegedField] != model.HasCapabilityTier(tiers, model.CapabilityTierPrivileged) {
					t.Fatalf("patch disagrees with itself: %s", p.patch)
				}
			}
		})
	}
}

func TestAdminBackfillCapabilityTiers_DryRunWritesNothing(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{
		capTiersRoleBead("gd-infra", "infra", `{"privileged":true}`),
		capTiersRoleBead("gd-crew", "crew", `{"capability_tiers":[]}`),
	}}
	withMockClient(t, mc)
	t.Cleanup(func() { _ = adminBackfillCapabilityTiersCmd.Flags().Set("apply", "false") })

	out := captureStdout(t, func() {
		if err := adminBackfillCapabilityTiersCmd.RunE(adminBackfillCapabilityTiersCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if len(mc.UpdateBeadCalls) != 0 {
		t.Fatalf("dry run issued %d UpdateBead calls", len(mc.UpdateBeadCalls))
	}
	if !strings.Contains(out, string(capTiersWouldUpdate)) || !strings.Contains(out, string(capTiersSkipIdempotent)) {
		t.Fatalf("missing rows:\n%s", out)
	}
}

func TestAdminBackfillCapabilityTiers_Apply(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsPages = [][]*model.Bead{{
		capTiersRoleBead("gd-infra", "infra", `{"privileged":true}`),
		capTiersRoleBead("gd-crew", "crew", `{"capability_tiers":[]}`),
	}}
	withMockClient(t, mc)
	t.Cleanup(func() { _ = adminBackfillCapabilityTiersCmd.Flags().Set("apply", "false") })
	_ = adminBackfillCapabilityTiersCmd.Flags().Set("apply", "true")

	out := captureStdout(t, func() {
		if err := adminBackfillCapabilityTiersCmd.RunE(adminBackfillCapabilityTiersCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if len(mc.UpdateBeadCalls) != 1 || mc.UpdateBeadCalls[0].ID != "gd-infra" {
		t.Fatalf("want one UpdateBead on gd-infra, got %+v", mc.UpdateBeadCalls)
	}
	if got := string(mc.UpdateBeadCalls[0].Req.Fields); got != `{"capability_tiers":["privileged"],"privileged":true}` {
		t.Fatalf("patch = %s", got)
	}
	if !strings.Contains(out, string(capTiersUpdated)) {
		t.Fatalf("missing UPDATED row:\n%s", out)
	}
}
