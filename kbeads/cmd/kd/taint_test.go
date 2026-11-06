package main

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
)

func TestTaintGrant_TableOutput(t *testing.T) {
	mc := newMockClient()
	expires := time.Date(2026, 12, 19, 0, 0, 0, 0, time.UTC)
	mc.GrantTaintResult = &model.TaintGrant{
		PrincipalKind: "operator",
		Principal:     "42",
		GrantedBy:     7,
		GrantedAt:     time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC),
		ExpiresAt:     &expires,
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := taintGrantCmd.RunE(taintGrantCmd, []string{"acme-production", "operator:42"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.GrantTaintCalls) != 1 {
		t.Fatalf("expected 1 GrantTaint call, got %d", len(mc.GrantTaintCalls))
	}
	call := mc.GrantTaintCalls[0]
	if call.Name != "acme-production" || call.Req.Principal != "operator:42" {
		t.Errorf("unexpected call: %+v", call)
	}
	if call.Req.Until != nil {
		t.Errorf("expected no --until, req carried %v", call.Req.Until)
	}
	for _, want := range []string{"acme-production", "operator:42", "2026-12-19"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestTaintGrant_UntilParsedAndForwarded(t *testing.T) {
	mc := newMockClient()
	mc.GrantTaintResult = &model.TaintGrant{PrincipalKind: "team", Principal: "gd-owner"}
	withMockClient(t, mc)

	t.Cleanup(func() { _ = taintGrantCmd.Flags().Set("until", "") })
	if err := taintGrantCmd.Flags().Set("until", "2027-01-01T00:00:00Z"); err != nil {
		t.Fatalf("setting --until: %v", err)
	}

	captureStdout(t, func() {
		if err := taintGrantCmd.RunE(taintGrantCmd, []string{"acme-production", "team:gd-owner"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.GrantTaintCalls) != 1 {
		t.Fatalf("expected 1 GrantTaint call, got %d", len(mc.GrantTaintCalls))
	}
	got := mc.GrantTaintCalls[0].Req.Until
	if got == nil || !got.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("expected Until 2027-01-01T00:00:00Z, got %v", got)
	}
}

func TestTaintGrant_InvalidUntil(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	t.Cleanup(func() { _ = taintGrantCmd.Flags().Set("until", "") })
	if err := taintGrantCmd.Flags().Set("until", "not-a-date"); err != nil {
		t.Fatalf("setting --until: %v", err)
	}

	err := taintGrantCmd.RunE(taintGrantCmd, []string{"acme-production", "operator:42"})
	if err == nil {
		t.Fatal("expected an error for an unparseable --until")
	}
	if len(mc.GrantTaintCalls) != 0 {
		t.Errorf("expected no GrantTaint call on parse failure, got %d", len(mc.GrantTaintCalls))
	}
}

func TestTaintGrant_ServerRejection(t *testing.T) {
	mc := newMockClient()
	mc.GrantTaintErr = errors.New("403: not owner or admin")
	withMockClient(t, mc)

	err := taintGrantCmd.RunE(taintGrantCmd, []string{"acme-production", "operator:42"})
	if err == nil {
		t.Fatal("expected an error to propagate from the server")
	}
	if !strings.Contains(err.Error(), "not owner or admin") {
		t.Errorf("expected server error text to propagate, got: %v", err)
	}
}

// TestTaintGrantRequest_NoOwnerFlag proves `kd taint grant-request <name>
// [--reason]` files the request without the caller passing --owner (ADR
// docs/adr/taints.md §9.2, gd-KT7mKtvYwt) — there is no such flag to begin
// with, and the command reports the escalation the server resolved.
func TestTaintGrantRequest_NoOwnerFlag(t *testing.T) {
	mc := newMockClient()
	mc.RequestTaintGrantResult = &client.EscalationCreated{
		EscalationID: "gd-esc-grant-1",
		Message:      "grant request created: pending owner-team approval",
		DeniedAction: "grant",
		BeadType:     "taint",
	}
	withMockClient(t, mc)

	taintGrantRequestCmd.Flags().Set("reason", "need to patch prod config")
	defer taintGrantRequestCmd.Flags().Set("reason", "")

	out := captureStdout(t, func() {
		if err := taintGrantRequestCmd.RunE(taintGrantRequestCmd, []string{"acme-production"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.RequestTaintGrantCalls) != 1 {
		t.Fatalf("expected 1 RequestTaintGrant call, got %d", len(mc.RequestTaintGrantCalls))
	}
	call := mc.RequestTaintGrantCalls[0]
	if call.Name != "acme-production" {
		t.Errorf("Name = %q; want acme-production", call.Name)
	}
	if call.Req.Reason != "need to patch prod config" {
		t.Errorf("Req.Reason = %q; want the --reason text", call.Req.Reason)
	}
	for _, want := range []string{"gd-esc-grant-1", "acme-production", "pending owner-team approval"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

// TestTaintGrantRequest_ServerRejection proves a server-side failure (e.g.
// the taint does not exist) propagates the server's own message rather than
// being swallowed or replaced with a generic error.
func TestTaintGrantRequest_ServerRejection(t *testing.T) {
	mc := newMockClient()
	mc.RequestTaintGrantErr = errors.New("404: taint not found")
	withMockClient(t, mc)

	err := taintGrantRequestCmd.RunE(taintGrantRequestCmd, []string{"nonexistent"})
	if err == nil {
		t.Fatal("expected an error to propagate from the server")
	}
	if !strings.Contains(err.Error(), "taint not found") {
		t.Errorf("expected server error text to propagate, got: %v", err)
	}
}

func TestTaintRevoke_Removed(t *testing.T) {
	mc := newMockClient()
	mc.RevokeTaintResult = true
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := taintRevokeCmd.RunE(taintRevokeCmd, []string{"acme-production", "operator:42"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.RevokeTaintCalls) != 1 {
		t.Fatalf("expected 1 RevokeTaint call, got %d", len(mc.RevokeTaintCalls))
	}
	if !strings.Contains(out, "Revoked acme-production from operator:42") {
		t.Errorf("unexpected output:\n%s", out)
	}
}

func TestTaintRevoke_NoOp(t *testing.T) {
	mc := newMockClient()
	mc.RevokeTaintResult = false
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := taintRevokeCmd.RunE(taintRevokeCmd, []string{"acme-production", "operator:99"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "nothing to revoke") {
		t.Errorf("expected a nothing-to-revoke notice, got:\n%s", out)
	}
}

func TestTaintApply_TableOutput(t *testing.T) {
	mc := newMockClient()
	mc.ApplyTaintResult = &model.BeadTaint{
		BeadID:    "gd-x",
		TaintName: "acme-production",
		Source:    model.TaintSourceExplicit,
		AppliedBy: "operator:42",
		AppliedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := taintApplyCmd.RunE(taintApplyCmd, []string{"gd-x", "acme-production"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ApplyTaintCalls) != 1 {
		t.Fatalf("expected 1 ApplyTaint call, got %d", len(mc.ApplyTaintCalls))
	}
	call := mc.ApplyTaintCalls[0]
	if call.BeadID != "gd-x" || call.TaintName != "acme-production" {
		t.Errorf("unexpected call: %+v", call)
	}
	for _, want := range []string{"gd-x", "acme-production", "explicit", "operator:42"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestTaintApply_NoGrant_Forbidden(t *testing.T) {
	mc := newMockClient()
	mc.ApplyTaintErr = errors.New("403: no grant for taint")
	withMockClient(t, mc)

	err := taintApplyCmd.RunE(taintApplyCmd, []string{"gd-x", "acme-production"})
	if err == nil {
		t.Fatal("expected an error to propagate from the server")
	}
	if !strings.Contains(err.Error(), "no grant for taint") {
		t.Errorf("expected server error text to propagate, got: %v", err)
	}
}

func TestTaintShow_ListsSourceAndSourceBeadID(t *testing.T) {
	mc := newMockClient()
	mc.BeadTaints = map[string][]*model.BeadTaint{
		"gd-x": {
			{
				BeadID:    "gd-x",
				TaintName: "acme-production",
				Source:    model.TaintSourceExplicit,
				AppliedBy: "operator:42",
				AppliedAt: time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC),
			},
			{
				BeadID:       "gd-x",
				TaintName:    "pii",
				Source:       model.TaintSourceInheritParent,
				SourceBeadID: "gd-parent",
				AppliedBy:    "system:inherit",
				AppliedAt:    time.Date(2026, 9, 20, 13, 0, 0, 0, time.UTC),
			},
		},
	}
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := taintShowCmd.RunE(taintShowCmd, []string{"gd-x"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.GetBeadTaintsCalls) != 1 || mc.GetBeadTaintsCalls[0] != "gd-x" {
		t.Fatalf("expected 1 GetBeadTaints call for gd-x, got %v", mc.GetBeadTaintsCalls)
	}
	for _, want := range []string{"acme-production", "explicit", "pii", "inherit:parent", "gd-parent"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestTaintShow_Empty(t *testing.T) {
	mc := newMockClient()
	withMockClient(t, mc)

	out := captureStdout(t, func() {
		if err := taintShowCmd.RunE(taintShowCmd, []string{"gd-empty"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if !strings.Contains(out, "No taints on gd-empty") {
		t.Errorf("expected an empty notice, got:\n%s", out)
	}
}

func TestTaintCmd_NoArgsPrintsHelp(t *testing.T) {
	if err := taintCmd.RunE(taintCmd, nil); err != nil {
		t.Fatalf("expected help to print without error, got: %v", err)
	}
}

// resetTaintCreateFlags clears --owner/--description/--preset/--policy state
// between tests; cobra commands are package-level singletons so Changed()
// bookkeeping bleeds across cases (mirrors resetCommentListFlags).
func resetTaintCreateFlags(t *testing.T) {
	t.Helper()
	_ = taintCreateCmd.Flags().Set("description", "")
	_ = taintCreateCmd.Flags().Set("owner", "")
	_ = taintCreateCmd.Flags().Set("preset", "")
	_ = taintCreateCmd.Flags().Set("policy", "")
	jsonOutput = false
	t.Cleanup(func() { jsonOutput = false })
}

func runTaintCreate(t *testing.T, args []string) (string, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		runErr = taintCreateCmd.RunE(taintCreateCmd, args)
	})
	return out, runErr
}

func TestTaintCreate_RequiresOwner(t *testing.T) {
	orig := beadsClient
	beadsClient = &mockBeadsClient{}
	t.Cleanup(func() { beadsClient = orig })
	resetTaintCreateFlags(t)

	_, err := runTaintCreate(t, []string{"customer-pii"})
	if err == nil || !strings.Contains(err.Error(), "--owner is required") {
		t.Fatalf("expected --owner required error, got %v", err)
	}
}

func TestTaintCreate_PresetAndPolicyMutuallyExclusive(t *testing.T) {
	orig := beadsClient
	beadsClient = &mockBeadsClient{}
	t.Cleanup(func() { beadsClient = orig })
	resetTaintCreateFlags(t)
	_ = taintCreateCmd.Flags().Set("owner", "gd-team-prod")
	_ = taintCreateCmd.Flags().Set("preset", "production")
	_ = taintCreateCmd.Flags().Set("policy", "/tmp/does-not-matter.json")

	_, err := runTaintCreate(t, []string{"customer-pii"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

// TestTaintCreate_ResolvesOwnerByRawBeadID covers the fallback path in
// resolveTaintOwnerTeam: when --owner does not match any team's Fields.team
// (findTeamByName finds nothing, since ListBeadsResult is empty here), it
// falls back to GetBead + a type:team check.
func TestTaintCreate_ResolvesOwnerByRawBeadID(t *testing.T) {
	orig := beadsClient
	mc := &mockBeadsClient{
		Beads: map[string]*model.Bead{
			"gd-team-prod": {ID: "gd-team-prod", Type: "team", Kind: model.KindConfig},
		},
	}
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
	resetTaintCreateFlags(t)
	_ = taintCreateCmd.Flags().Set("owner", "gd-team-prod")
	_ = taintCreateCmd.Flags().Set("description", "customer PII")

	out, err := runTaintCreate(t, []string{"customer-pii"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.CreateTaintCalls) != 1 {
		t.Fatalf("expected 1 CreateTaint call, got %d", len(mc.CreateTaintCalls))
	}
	req := mc.CreateTaintCalls[0]
	if req.Name != "customer-pii" || req.OwnerTeam != "gd-team-prod" || req.Description != "customer PII" {
		t.Fatalf("unexpected CreateTaintRequest: %+v", req)
	}
	if !strings.Contains(out, "Created taint customer-pii") {
		t.Fatalf("expected creation summary in output, got %q", out)
	}
}

// TestTaintCreate_ResolvesOwnerByTeamName covers the primary path: --owner
// matches a team's Fields.team, resolved via findTeamByName / ListBeads.
func TestTaintCreate_ResolvesOwnerByTeamName(t *testing.T) {
	orig := beadsClient
	teamFields, _ := json.Marshal(map[string]any{"team": "acme-prod-owners"})
	mc := &mockBeadsClient{
		ListBeadsResult: []*model.Bead{
			{ID: "gd-team-prod", Type: "team", Kind: model.KindConfig, Fields: teamFields},
		},
	}
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
	resetTaintCreateFlags(t)
	_ = taintCreateCmd.Flags().Set("owner", "acme-prod-owners")

	_, err := runTaintCreate(t, []string{"customer-pii"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.CreateTaintCalls) != 1 || mc.CreateTaintCalls[0].OwnerTeam != "gd-team-prod" {
		t.Fatalf("expected OwnerTeam resolved to gd-team-prod, got %+v", mc.CreateTaintCalls)
	}
}

func TestTaintCreate_PresetFlagPassedThrough(t *testing.T) {
	orig := beadsClient
	mc := &mockBeadsClient{
		Beads: map[string]*model.Bead{
			"gd-team-prod": {ID: "gd-team-prod", Type: "team", Kind: model.KindConfig},
		},
	}
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
	resetTaintCreateFlags(t)
	_ = taintCreateCmd.Flags().Set("owner", "gd-team-prod")
	_ = taintCreateCmd.Flags().Set("preset", "internal")

	_, err := runTaintCreate(t, []string{"internal-only"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mc.CreateTaintCalls) != 1 || mc.CreateTaintCalls[0].Preset != "internal" {
		t.Fatalf("expected preset=internal passed through, got %+v", mc.CreateTaintCalls)
	}
}

func TestTaintCreate_JSONOutput(t *testing.T) {
	orig := beadsClient
	mc := &mockBeadsClient{
		Beads: map[string]*model.Bead{
			"gd-team-prod": {ID: "gd-team-prod", Type: "team", Kind: model.KindConfig},
		},
	}
	beadsClient = mc
	t.Cleanup(func() { beadsClient = orig })
	resetTaintCreateFlags(t)
	jsonOutput = true
	_ = taintCreateCmd.Flags().Set("owner", "gd-team-prod")

	out, err := runTaintCreate(t, []string{"customer-pii"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var got client.TaintView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", out, err)
	}
	if got.Name != "customer-pii" {
		t.Fatalf("expected name customer-pii, got %+v", got)
	}
}

func TestTaintList_TableOutput(t *testing.T) {
	orig := beadsClient
	retired := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	beadsClient = &mockBeadsClient{
		TaintList: []client.TaintView{
			{Name: "customer-pii", OwnerTeam: "gd-team-prod", Description: "PII"},
			{Name: "old-taint", OwnerTeam: "gd-team-infra", Description: "retired", RetiredAt: &retired},
		},
	}
	t.Cleanup(func() { beadsClient = orig })
	jsonOutput = false

	out := captureStdout(t, func() {
		if err := taintListCmd.RunE(taintListCmd, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, "customer-pii") || !strings.Contains(out, "gd-team-prod") {
		t.Fatalf("expected customer-pii row, got %q", out)
	}
	if !strings.Contains(out, "2026-01-02") {
		t.Fatalf("expected retired date formatted, got %q", out)
	}
}

func TestTaintList_JSONOutput(t *testing.T) {
	orig := beadsClient
	beadsClient = &mockBeadsClient{
		TaintList: []client.TaintView{{Name: "customer-pii", OwnerTeam: "gd-team-prod"}},
	}
	t.Cleanup(func() { beadsClient = orig })
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = false })

	out := captureStdout(t, func() {
		if err := taintListCmd.RunE(taintListCmd, nil); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	var got []client.TaintView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("expected JSON array output, got %q: %v", out, err)
	}
	if len(got) != 1 || got[0].Name != "customer-pii" {
		t.Fatalf("unexpected taints: %+v", got)
	}
}

func TestTaintDescribe_FullDetailForOwnerOrAdmin(t *testing.T) {
	orig := beadsClient
	beadsClient = &mockBeadsClient{
		TaintsByName: map[string]*client.TaintView{
			"customer-pii": {
				Name: "customer-pii", Description: "PII", OwnerTeam: "gd-team-prod",
				ScrubPolicy: json.RawMessage(`{"default":"human-four-eyes"}`),
			},
		},
	}
	t.Cleanup(func() { beadsClient = orig })
	jsonOutput = false

	out := captureStdout(t, func() {
		if err := taintDescribeCmd.RunE(taintDescribeCmd, []string{"customer-pii"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, "Owner team:  gd-team-prod") {
		t.Fatalf("expected owner team line, got %q", out)
	}
	if !strings.Contains(out, "Scrub policy:") {
		t.Fatalf("expected scrub policy section, got %q", out)
	}
}

func TestTaintDescribe_RestrictedViewOmitsOwnerAndPolicy(t *testing.T) {
	orig := beadsClient
	beadsClient = &mockBeadsClient{
		TaintsByName: map[string]*client.TaintView{
			"customer-pii": {Name: "customer-pii", Description: "PII"},
		},
	}
	t.Cleanup(func() { beadsClient = orig })
	jsonOutput = false

	out := captureStdout(t, func() {
		if err := taintDescribeCmd.RunE(taintDescribeCmd, []string{"customer-pii"}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
	if !strings.Contains(out, "Name:        customer-pii") || !strings.Contains(out, "Description: PII") {
		t.Fatalf("expected name/description, got %q", out)
	}
	if strings.Contains(out, "Owner team:") || strings.Contains(out, "Scrub policy:") {
		t.Fatalf("restricted view must not reveal owner team or policy, got %q", out)
	}
}

func TestTaintDescribe_NotFound(t *testing.T) {
	orig := beadsClient
	beadsClient = &mockBeadsClient{
		GetTaintErr: &client.APIError{StatusCode: 404, Message: "taint not found"},
	}
	t.Cleanup(func() { beadsClient = orig })
	jsonOutput = false

	_ = captureStdout(t, func() {
		err := taintDescribeCmd.RunE(taintDescribeCmd, []string{"does-not-exist"})
		if err == nil {
			t.Fatalf("expected error for not-found taint")
		}
	})
}
