package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/pflag"

	"gardenio/kbeads/internal/model"
)

func withTeamClient(t *testing.T, mc *mockBeadsClient) {
	t.Helper()
	orig := beadsClient
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
}

// resetTeamSliceFlags clears the repeatable --member/--owner/--oidc-group/
// --project StringSlice flags on cmd between tests. StringSlice accumulates
// across Set calls and carries its Changed bit between RunE invocations
// within one process, and f.Value.Set(f.DefValue) does not reset a slice
// flag — so the underlying slice must be Replace'd (ka-0000007843 pattern).
func resetTeamSliceFlags(t *testing.T, cmd interface{ Flags() *pflag.FlagSet }) {
	t.Helper()
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		switch f.Name {
		case "member", "owner", "oidc-group", "project", "display":
			f.Changed = false
			if sv, ok := f.Value.(pflag.SliceValue); ok {
				_ = sv.Replace([]string{})
			} else {
				_ = f.Value.Set(f.DefValue)
			}
		}
	})
}

// TestTeamCreate_ResolvesAliasesAndBuildsFields covers acceptance criterion 2
// (a bare email is resolved through operator_aliases before storing) for
// `kd team create`, and confirms the numeric-id shortcut is stored verbatim.
func TestTeamCreate_ResolvesAliasesAndBuildsFields(t *testing.T) {
	mc := newMockClient()
	mc.OperatorAliases = map[string]int64{"oidc:alice@example.com": 42}
	withTeamClient(t, mc)

	teamCreateCmd.Flags().Set("display", "Acme Prod")
	teamCreateCmd.Flags().Set("member", "alice@example.com")
	teamCreateCmd.Flags().Set("owner", "7")
	teamCreateCmd.Flags().Set("oidc-group", "/platform-team")
	teamCreateCmd.Flags().Set("project", "acme")
	t.Cleanup(func() { resetTeamSliceFlags(t, teamCreateCmd) })

	if err := teamCreateCmd.RunE(teamCreateCmd, []string{"acme-prod"}); err != nil {
		t.Fatalf("teamCreateCmd: %v", err)
	}

	if len(mc.CreateBeadCalls) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(mc.CreateBeadCalls))
	}
	req := mc.CreateBeadCalls[0]
	if req.Type != "team" {
		t.Errorf("Type = %q, want %q", req.Type, "team")
	}
	if req.Kind != "" {
		t.Errorf("Kind = %q, want empty (server derives kind from type config)", req.Kind)
	}

	var fields map[string]any
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("unmarshaling fields: %v", err)
	}
	if fields["team"] != "acme-prod" {
		t.Errorf("fields.team = %v, want acme-prod", fields["team"])
	}
	if fields["display"] != "Acme Prod" {
		t.Errorf("fields.display = %v, want %q", fields["display"], "Acme Prod")
	}
	members, _ := fields["members"].([]any)
	if len(members) != 1 || members[0].(float64) != 42 {
		t.Errorf("fields.members = %v, want [42] (resolved from alias)", fields["members"])
	}
	owners, _ := fields["owners"].([]any)
	if len(owners) != 1 || owners[0].(float64) != 7 {
		t.Errorf("fields.owners = %v, want [7] (numeric passthrough)", fields["owners"])
	}
	groups, _ := fields["oidc_groups"].([]any)
	if len(groups) != 1 || groups[0] != "/platform-team" {
		t.Errorf("fields.oidc_groups = %v, want [/platform-team]", fields["oidc_groups"])
	}
}

// TestTeamCreate_ServerRejectionSurfacedVerbatim proves acceptance criterion
// 1: a non-admin creator is rejected SERVER-SIDE, and the CLI does nothing
// but surface that error — it must not swallow, reinterpret, or pre-empt it
// with its own role check.
func TestTeamCreate_ServerRejectionSurfacedVerbatim(t *testing.T) {
	mc := newMockClient()
	mc.CreateErr = errors.New(`write denied: role "crew" is below required role "gardenio-admin" for creating type:team`)
	withTeamClient(t, mc)

	err := teamCreateCmd.RunE(teamCreateCmd, []string{"acme-prod"})
	if err == nil {
		t.Fatal("expected an error from a denied create, got nil")
	}
	if !strings.Contains(err.Error(), "gardenio-admin") {
		t.Errorf("error = %q; expected the server's role-denial message to pass through unchanged", err.Error())
	}
}

func seedTeamBeadForTest(mc *mockBeadsClient, id, name, fieldsJSON string) *model.Bead {
	b := &model.Bead{ID: id, Kind: model.KindConfig, Type: model.TypeTeam, Title: id, Fields: json.RawMessage(fieldsJSON)}
	mc.Beads[id] = b
	mc.ListBeadsResult = append(mc.ListBeadsResult, b)
	_ = name
	return b
}

// TestTeamAdd_ResolvesAliasAndPatchesOnlyMembers covers acceptance criterion
// 2 for `kd team add` and confirms the update patch carries ONLY the changed
// "members" key (relying on the server's merge-patch semantics), not a full
// field replacement.
func TestTeamAdd_ResolvesAliasAndPatchesOnlyMembers(t *testing.T) {
	mc := newMockClient()
	mc.OperatorAliases = map[string]int64{"oidc:bob@example.com": 99}
	seedTeamBeadForTest(mc, "gd-team1", "acme-prod", `{"team":"acme-prod","display":"Acme","members":[1]}`)
	withTeamClient(t, mc)

	teamAddCmd.Flags().Set("member", "bob@example.com")
	t.Cleanup(func() { resetTeamSliceFlags(t, teamAddCmd) })

	if err := teamAddCmd.RunE(teamAddCmd, []string{"acme-prod"}); err != nil {
		t.Fatalf("teamAddCmd: %v", err)
	}

	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("expected 1 UpdateBead call, got %d", len(mc.UpdateBeadCalls))
	}
	call := mc.UpdateBeadCalls[0]
	if call.ID != "gd-team1" {
		t.Errorf("UpdateBead id = %q, want gd-team1", call.ID)
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(call.Req.Fields, &patch); err != nil {
		t.Fatalf("unmarshaling patch: %v", err)
	}
	if len(patch) != 1 {
		t.Fatalf("patch = %v; want exactly the changed \"members\" key (merge-patch semantics)", patch)
	}
	var members []any
	if err := json.Unmarshal(patch["members"], &members); err != nil {
		t.Fatalf("unmarshaling members patch: %v", err)
	}
	if len(members) != 2 || members[0].(float64) != 1 || members[1].(float64) != 99 {
		t.Errorf("members = %v, want [1, 99] (existing id preserved, alias resolved and appended)", members)
	}
}

// TestTeamAdd_UnresolvedAliasStoredAsPendingReference covers the fallback
// half of criterion 2: an alias with no operator row yet is stored as a
// pending "kind:subject" string rather than dropped or erroring, matching
// the same lazy-resolution tolerance teamMatchesMember applies at read time.
func TestTeamAdd_UnresolvedAliasStoredAsPendingReference(t *testing.T) {
	mc := newMockClient()
	seedTeamBeadForTest(mc, "gd-team1", "acme-prod", `{"team":"acme-prod"}`)
	withTeamClient(t, mc)

	teamAddCmd.Flags().Set("member", "newperson@example.com")
	t.Cleanup(func() { resetTeamSliceFlags(t, teamAddCmd) })

	if err := teamAddCmd.RunE(teamAddCmd, []string{"acme-prod"}); err != nil {
		t.Fatalf("teamAddCmd: %v", err)
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(mc.UpdateBeadCalls[0].Req.Fields, &patch); err != nil {
		t.Fatalf("unmarshaling patch: %v", err)
	}
	var members []string
	if err := json.Unmarshal(patch["members"], &members); err != nil {
		t.Fatalf("unmarshaling members patch: %v", err)
	}
	if len(members) != 1 || members[0] != "oidc:newperson@example.com" {
		t.Errorf("members = %v, want [\"oidc:newperson@example.com\"]", members)
	}
}

// TestTeamRemove_MatchesAcrossAliasEquivalence proves `kd team remove` can
// remove a member listed under one alias (Slack) by passing an equivalent
// alias (email) for the same operator — mirroring the read-time equivalence
// teamMatchesMember already guarantees server-side.
func TestTeamRemove_MatchesAcrossAliasEquivalence(t *testing.T) {
	mc := newMockClient()
	mc.OperatorAliases = map[string]int64{
		"oidc:alice@example.com": 42,
		"slack:U12345":           42,
	}
	seedTeamBeadForTest(mc, "gd-team1", "acme-prod", `{"team":"acme-prod","members":["slack:U12345"]}`)
	withTeamClient(t, mc)

	teamRemoveCmd.Flags().Set("member", "alice@example.com")
	t.Cleanup(func() { resetTeamSliceFlags(t, teamRemoveCmd) })

	if err := teamRemoveCmd.RunE(teamRemoveCmd, []string{"acme-prod"}); err != nil {
		t.Fatalf("teamRemoveCmd: %v", err)
	}
	var patch map[string]json.RawMessage
	if err := json.Unmarshal(mc.UpdateBeadCalls[0].Req.Fields, &patch); err != nil {
		t.Fatalf("unmarshaling patch: %v", err)
	}
	var members []any
	if err := json.Unmarshal(patch["members"], &members); err != nil {
		t.Fatalf("unmarshaling members patch: %v", err)
	}
	if len(members) != 0 {
		t.Errorf("members = %v, want [] (removed via alias equivalence)", members)
	}
}

// TestTeamShow_RendersDerivedMembershipCount covers acceptance criterion 3's
// show half: all fields render, plus a derived membership count, in both
// table and JSON modes.
func TestTeamShow_RendersDerivedMembershipCount(t *testing.T) {
	mc := newMockClient()
	seedTeamBeadForTest(mc, "gd-team1", "acme-prod", `{"team":"acme-prod","members":[1,2,3],"display":"Acme"}`)
	withTeamClient(t, mc)

	out := captureStdout(t, func() {
		if err := teamShowCmd.RunE(teamShowCmd, []string{"acme-prod"}); err != nil {
			t.Fatalf("teamShowCmd: %v", err)
		}
	})
	if !strings.Contains(out, "Membership Count: 3") {
		t.Errorf("table output missing membership count, got:\n%s", out)
	}

	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })
	out = captureStdout(t, func() {
		if err := teamShowCmd.RunE(teamShowCmd, []string{"acme-prod"}); err != nil {
			t.Fatalf("teamShowCmd (json): %v", err)
		}
	})
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshaling json output: %v\n%s", err, out)
	}
	if result["member_count"].(float64) != 3 {
		t.Errorf("member_count = %v, want 3", result["member_count"])
	}
	if result["id"] != "gd-team1" {
		t.Errorf("json output missing bead fields, got: %v", result)
	}
}

// TestTeamList_Paginates covers acceptance criterion 3's list half: `kd team
// list` accepts --limit/--offset and forwards them like other `kd list`
// commands, rather than hardcoding a fixed page size.
func TestTeamList_Paginates(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{
		{ID: "gd-team1", Type: model.TypeTeam, Kind: model.KindConfig, Title: "gd-team1"},
	}
	mc.ListBeadsTotalOverride = 42
	withTeamClient(t, mc)

	teamListCmd.Flags().Set("limit", "5")
	teamListCmd.Flags().Set("offset", "10")
	t.Cleanup(func() {
		teamListCmd.Flags().Set("limit", "20")
		teamListCmd.Flags().Set("offset", "0")
	})

	out := captureStdout(t, func() {
		if err := teamListCmd.RunE(teamListCmd, nil); err != nil {
			t.Fatalf("teamListCmd: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if req.Limit != 5 || req.Offset != 10 {
		t.Errorf("ListBeads limit/offset = %d/%d, want 5/10", req.Limit, req.Offset)
	}
	if !strings.Contains(out, "1 beads (42 total)") {
		t.Errorf("expected pagination footer with total, got:\n%s", out)
	}
}
