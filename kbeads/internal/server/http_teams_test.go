package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// --- GET /v1/teams/{id}/taint-summary ---

func TestHandleTeamTaintSummary_NotFound(t *testing.T) {
	_, _, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-nope/taint-summary", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusNotFound)
}

func TestHandleTeamTaintSummary_NonTeamBeadNotFound(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	ms.beads["gd-not-a-team"] = &model.Bead{ID: "gd-not-a-team", Kind: model.KindIssue, Type: model.TypeTask, Title: "t", Fleet: "gardenio"}

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-not-a-team/taint-summary", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusNotFound)
}

// TestHandleTeamTaintSummary_HeldGrantsExcludesOwnedTaints pins that
// held_grants reflects only explicit taint_grants rows for the team, not
// taints the team owns outright (ADR docs/adr/taints.md §5.1/§9.1: ownership
// implies its own separate grant, not a taint_grants row).
func TestHandleTeamTaintSummary_HeldGrantsExcludesOwnedTaints(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":[]}`)
	putTaint(ms, &model.Taint{ID: 1, Name: "owned-by-prod", OwnerTeam: "gd-team-prod"})
	putTaint(ms, &model.Taint{ID: 2, Name: "granted-to-prod", OwnerTeam: "gd-team-infra"})
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{
		TaintID: 2, PrincipalKind: model.PrincipalTeam, Principal: "gd-team-prod", GrantedBy: 7,
	}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-team-prod/taint-summary", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var got teamTaintSummary
	decodeJSON(t, rec, &got)
	if len(got.HeldGrants) != 1 || got.HeldGrants[0].TaintName != "granted-to-prod" {
		t.Fatalf("expected held_grants to contain only the explicitly granted taint, got %+v", got.HeldGrants)
	}
}

// TestHandleTeamTaintSummary_HeldGrantsOmitsExpired pins that an expired
// grant no longer appears in held_grants.
func TestHandleTeamTaintSummary_HeldGrantsOmitsExpired(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":[]}`)
	putTaint(ms, &model.Taint{ID: 1, Name: "expired-grant", OwnerTeam: "gd-team-infra"})
	expired := time.Now().Add(-time.Hour)
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalTeam, Principal: "gd-team-prod", GrantedBy: 7, ExpiresAt: &expired,
	}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-team-prod/taint-summary", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var got teamTaintSummary
	decodeJSON(t, rec, &got)
	if len(got.HeldGrants) != 0 {
		t.Fatalf("expected no held_grants for an expired grant, got %+v", got.HeldGrants)
	}
}

// TestHandleTeamTaintSummary_HeldGrantsScopedToCallerVisibility pins the
// operator-review fix on PR #145: held_grants must not reveal which taints an
// arbitrary team holds grants for to every viewer. A grant on a taint owned
// by gd-team-infra is included only for a caller who can see that taint's
// detail (a gd-team-infra member, or an admin) — an unrelated viewer gets an
// empty list for the very same team/grant.
func TestHandleTeamTaintSummary_HeldGrantsScopedToCallerVisibility(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{
		"outsider@example.com":     {RoleViewer},
		"infra-member@example.com": {RoleViewer},
	})
	ms.operatorAliases = map[string]int64{"oidc\x00infra-member@example.com": 7}
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":[]}`)
	seedTeamBead(ms, "gd-team-infra", `{"team":"acme-infra","members":["oidc:infra-member@example.com"]}`)
	putTaint(ms, &model.Taint{ID: 1, Name: "granted-to-prod", OwnerTeam: "gd-team-infra"})
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{
		TaintID: 1, PrincipalKind: model.PrincipalTeam, Principal: "gd-team-prod", GrantedBy: 7,
	}); err != nil {
		t.Fatalf("GrantTaint: %v", err)
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-team-prod/taint-summary", nil, "outsider@example.com")
	requireStatus(t, rec, http.StatusOK)
	var outsiderGot teamTaintSummary
	decodeJSON(t, rec, &outsiderGot)
	if len(outsiderGot.HeldGrants) != 0 {
		t.Fatalf("expected an unrelated viewer to see no held_grants, got %+v", outsiderGot.HeldGrants)
	}

	rec = taintDoJSONAs(t, h, "GET", "/v1/teams/gd-team-prod/taint-summary", nil, "infra-member@example.com")
	requireStatus(t, rec, http.StatusOK)
	var memberGot teamTaintSummary
	decodeJSON(t, rec, &memberGot)
	if len(memberGot.HeldGrants) != 1 || memberGot.HeldGrants[0].TaintName != "granted-to-prod" {
		t.Fatalf("expected the owner team's own member to see the held grant, got %+v", memberGot.HeldGrants)
	}
}

// TestHandleTeamTaintSummary_OpenApprovalsMatchesNamedApprover pins that
// open_approvals lists an open scrub-request whose resolved policy approvers
// (the same resolveScrubRuleForApproval evaluator handleApproveScrubRequest
// runs) name this team, and excludes one that names a different team.
func TestHandleTeamTaintSummary_OpenApprovalsMatchesNamedApprover(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":[]}`)
	policy := model.ScrubPolicy{Default: model.ScrubModeHuman, Approvers: []string{"team:gd-team-prod"}}
	policyJSON, _ := json.Marshal(policy)
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod", ScrubPolicy: policyJSON})

	otherPolicy := model.ScrubPolicy{Default: model.ScrubModeHuman, Approvers: []string{"team:gd-team-other"}}
	otherPolicyJSON, _ := json.Marshal(otherPolicy)
	putTaint(ms, &model.Taint{ID: 2, Name: "other-taint", OwnerTeam: "gd-team-other", ScrubPolicy: otherPolicyJSON})

	ms.beads["gd-open1"] = &model.Bead{
		ID: "gd-open1", Kind: model.KindIssue, Type: model.TypeScrubRequest, Title: "detaint pii",
		Fleet: "gardenio", Status: model.StatusOpen,
		Fields: []byte(`{"taint":"customer-pii","kind":"detaint","requested_by":"oidc:alice@example.com"}`),
	}
	ms.beads["gd-open2"] = &model.Bead{
		ID: "gd-open2", Kind: model.KindIssue, Type: model.TypeScrubRequest, Title: "detaint other",
		Fleet: "gardenio", Status: model.StatusOpen,
		Fields: []byte(`{"taint":"other-taint","kind":"detaint","requested_by":"oidc:bob@example.com"}`),
	}
	ms.beads["gd-closed1"] = &model.Bead{
		ID: "gd-closed1", Kind: model.KindIssue, Type: model.TypeScrubRequest, Title: "already closed",
		Fleet: "gardenio", Status: model.StatusClosed,
		Fields: []byte(`{"taint":"customer-pii","kind":"detaint","requested_by":"oidc:alice@example.com"}`),
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-team-prod/taint-summary", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var got teamTaintSummary
	decodeJSON(t, rec, &got)
	if len(got.OpenApprovals) != 1 {
		t.Fatalf("expected exactly 1 open approval, got %+v", got.OpenApprovals)
	}
	if got.OpenApprovals[0].ID != "gd-open1" || got.OpenApprovals[0].TaintName != "customer-pii" {
		t.Fatalf("unexpected open approval: %+v", got.OpenApprovals[0])
	}
}

// TestHandleTeamTaintSummary_OpenApprovalsIncludesOverCapAutoFallenThrough is
// the merge-desk fix for PR #182 (gd-d3ZZbjU9gH): a request matching an
// "auto" rule that is over its max_per_day cap must still surface in
// open_approvals for whichever team the request actually falls through to
// (here, a human rule naming gd-team-prod) — teamOpenApprovals used to
// resolve mode/approvers through the uncapped resolveScrubRule, which would
// resolve the over-cap auto rule's own (empty) approvers and never show the
// request in gd-team-prod's inbox at all, even though approving it for real
// falls through to gd-team-prod exactly as this list should reflect.
func TestHandleTeamTaintSummary_OpenApprovalsIncludesOverCapAutoFallenThrough(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	seedTeamBead(ms, "gd-team-prod", `{"team":"acme-prod","members":[]}`)
	maxBeads := 10
	maxPerDay := 2
	policy := model.ScrubPolicy{
		Rules: []model.ScrubRule{
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeAuto, MaxBeads: &maxBeads, MaxPerDay: &maxPerDay},
			{When: json.RawMessage(`{"kind":"detaint"}`), Mode: model.ScrubModeHuman, Approvers: []string{"team:gd-team-prod"}},
		},
		Default: model.ScrubModeHumanFourEyes,
	}
	policyJSON, _ := json.Marshal(policy)
	putTaint(ms, &model.Taint{ID: 1, Name: "customer-pii", OwnerTeam: "gd-team-prod", ScrubPolicy: policyJSON})
	// The taint has already hit its max_per_day of 2 today, so the auto
	// rule is over cap and must fall through to the human rule below it.
	seedTaintAuditScrubExecuteRows(ms, 1, 2)

	ms.beads["gd-open-overcap"] = &model.Bead{
		ID: "gd-open-overcap", Kind: model.KindIssue, Type: model.TypeScrubRequest, Title: "detaint pii",
		Fleet: "gardenio", Status: model.StatusOpen,
		Fields: []byte(`{"taint":"customer-pii","kind":"detaint","requested_by":"oidc:alice@example.com","resolved_bead_ids":["gd-target1"]}`),
	}

	rec := taintDoJSONAs(t, h, "GET", "/v1/teams/gd-team-prod/taint-summary", nil, "admin@example.com")
	requireStatus(t, rec, http.StatusOK)

	var got teamTaintSummary
	decodeJSON(t, rec, &got)
	if len(got.OpenApprovals) != 1 || got.OpenApprovals[0].ID != "gd-open-overcap" {
		t.Fatalf("expected the over-cap request to appear in gd-team-prod's open_approvals (fell through to the human rule naming it), got %+v", got.OpenApprovals)
	}
}
