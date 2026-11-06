package server

// escalation_login_resolve_e2e_test.go
//
// Repro + e2e verification for epic gd-OCkwfqlnMn / task gd-mmvoPn7X8z:
// "a freshly-logged-in user should be able to resolve an escalation".
//
// This file has TWO halves, and they are deliberately kept together so the
// before/after is legible in one place:
//
//   1. REPRODUCTION (the bug as it exists TODAY, pre-flat-grant). A `gb login`
//      OIDC identity that carries NO gardenio-* realm role — or only the
//      viewer role a burn-in edge session gets — is walled off from BOTH
//      resolve paths:
//        - Gate #1 (ownership): the generic close/update path on a
//          type:escalation bead filed by someone else -> 403 "ownership
//          denied". This is the literal denial hit on gd-zPklZK9xkN.
//        - Gate #2 (role): the proper POST /v1/escalations/{id}/approve|reject
//          endpoints are wrapped in requireRole(RoleOperator) -> 403
//          "insufficient permissions: requires gardenio-operator".
//      These two subtests LOCK IN the denial. When the flat grant lands
//      (deriveOIDCRoles synthesizing RoleOperator for any verified login at
//      the two AuthMiddleware WithRoles sites — see recommendation bundle
//      gd-8y1oGtjV4A), the *fresh-login* identity in these subtests will begin
//      passing the operator predicate. At that point flip freshLoginRoles from
//      viewer/nil to the granted role (one-line change, noted inline) and the
//      denial subtests will correctly start proving "no longer denied".
//
//   2. GREEN PATH (what "resolved end-to-end" must look like once an identity
//      DOES clear the operator predicate). Driven here with an explicit
//      operator-role token so the assertion is independent of whether the flat
//      grant has shipped: an operator lists the open escalation, resolves it
//      (approve -> operation_spec replay fires + auto-close; and, in a second
//      instance, reject -> close with result_status=failure). This is the
//      behavior the flat grant makes reachable for a fresh login.
//
// Runs fully in-process through AuthMiddleware -> requireRole -> handler via
// the shared newEscalationE2EServer harness (no cluster, no Keycloak), so it
// stays green in the claudeless/e2e shard and as a plain `go test`.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// freshLoginRoles models what a `gb login` OIDC identity carries at the daemon
// TODAY, pre-flat-grant: realm_access.roles is empty for a freshly-federated
// GitLab login (nil), and a burn-in-trusted-but-unverified edge session is
// pinned to viewer (edge_auth.go). Neither clears requireRole(RoleOperator)
// nor the escalation ownership carve-out.
//
// AFTER the flat grant (gd-8y1oGtjV4A) lands, a verified login is synthesized
// up to RoleOperator at the AuthMiddleware WithRoles sites. To convert the
// denial subtests below into "no-longer-denied" assertions, change this to
// []string{RoleOperator} and update the wantDenied expectations noted inline.
var freshLoginRoles = []string{RoleViewer}

// makeFreshLoginEscalation stands up a full-stack server and pre-seeds an OPEN
// escalation bead created by SOME OTHER agent (mimicking the real shape: the
// escalation was filed by the agent whose write was denied, not by the human
// now trying to resolve it). Returns the server harness, the BeadsServer (for
// direct authz probes), the mock store, and the escalation id.
func makeFreshLoginEscalation(t *testing.T, adminToken, freshTok, operatorTok, fleetName string) (*httptest.Server, *BeadsServer, *mockStore, string) {
	t.Helper()

	oidcMock := &escOIDCMock{
		tokens: map[string]escTokenInfo{
			// The freshly-logged-in human: verified identity, but no operator role.
			freshTok: {email: "newlogin@acme.example", roles: freshLoginRoles},
			// An explicit operator, used to drive the green path independent of
			// whether the flat grant has shipped.
			operatorTok: {email: "operator@acme.example", roles: []string{RoleOperator}},
		},
	}

	fc := NewFleetConfig(fleetName, nil)
	ts, srv, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)

	// Pre-seed an OPEN escalation filed by a DIFFERENT agent. Seeding directly
	// (vs the operator X-Escalate path) guarantees created_by != the resolver
	// so the ownership gate is genuinely exercised, not masked by the human
	// happening to be the creator.
	esc := escBead("gd-sk6c6exzw4-esc1", "some-denied-agent")
	esc.Fields = json.RawMessage(`{"operation_type":"comment","operation_spec":{}}`)
	ms.beads[esc.ID] = esc

	return ts, srv, ms, esc.ID
}

// TestEscalationFreshLoginDenied_Repro locks in the CURRENT denial: a
// freshly-logged-in OIDC user (no operator role) cannot resolve an escalation
// through either gate. This is the reproduction half of gd-mmvoPn7X8z.
func TestEscalationFreshLoginDenied_Repro(t *testing.T) {
	const (
		adminToken  = "admin-freshlogin-e2e"
		freshTok    = "oidc-freshlogin"
		operatorTok = "oidc-operator-ctrl"
		fleetName   = "freshlogin-fleet"
	)

	ts, srv, ms, escID := makeFreshLoginEscalation(t, adminToken, freshTok, operatorTok, fleetName)

	// Whether the fresh login is EXPECTED to still be denied. TODAY: true.
	// After the flat grant lands (freshLoginRoles -> RoleOperator), set the
	// var and these subtests flip to proving "no longer denied" instead of
	// proving the denial.
	wantDenied := !hasMinimumRole(freshLoginRoles, RoleOperator)

	// ── Gate #2 (role): the proper resolve endpoints ─────────────────
	// The correct resolution path is POST /v1/escalations/{id}/approve|reject,
	// each wrapped in requireRole(RoleOperator) at the mux (http.go:180-182).
	// A fresh login below operator is rejected BEFORE the handler runs, with
	// "insufficient permissions: requires gardenio-operator". This is the wall
	// on the *intended* resolve surface.
	for _, verb := range []struct {
		name, path string
		body       map[string]any
	}{
		{"approve", "/approve", map[string]any{"approved_by": "newlogin@acme.example", "approval_method": "web_ui"}},
		{"reject", "/reject", map[string]any{"rejected_by": "newlogin@acme.example", "reason": "test"}},
	} {
		t.Run("Gate2_RoleDenies_"+verb.name, func(t *testing.T) {
			resp := escRequest(t, ts, "POST", "/v1/escalations/"+escID+verb.path,
				freshTok, verb.body, nil)
			defer resp.Body.Close()

			body, _ := io.ReadAll(resp.Body)
			if wantDenied {
				if resp.StatusCode != http.StatusForbidden {
					t.Fatalf("fresh login %s: expected 403 role denial, got %d: %s",
						verb.name, resp.StatusCode, body)
				}
				if !strings.Contains(string(body), RoleOperator) {
					t.Errorf("expected role-denial message naming %s, got: %s", RoleOperator, body)
				}
				t.Logf("REPRO %s: HTTP %d %s", verb.name, resp.StatusCode, strings.TrimSpace(string(body)))
			} else if resp.StatusCode == http.StatusForbidden {
				t.Fatalf("post-grant fresh login should clear requireRole(operator) on %s, got 403: %s",
					verb.name, body)
			}
		})
	}

	// The denied resolve calls must not have moved the escalation.
	if wantDenied {
		if got := ms.beads[escID].Status; got != model.StatusOpen {
			t.Errorf("denied resolve calls must leave escalation open, got status=%s", got)
		}
	}

	// ── Gate #2 (role) also walls the CLI generic-close path ─────────
	// The generic close route (POST /v1/beads/{id}/close) is likewise
	// requireRole(RoleOperator)-gated at the mux (http.go:159), so the same
	// role denial appears here first — a fresh login cannot even reach the
	// escalation resolve-surface divert (409) that operators see.
	t.Run("Gate2_RoleDenies_GenericClose", func(t *testing.T) {
		resp := escRequest(t, ts, "POST", "/v1/beads/"+escID+"/close",
			freshTok, map[string]any{}, nil)
		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		if wantDenied {
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("fresh login generic-close: expected 403 role denial, got %d: %s",
					resp.StatusCode, body)
			}
			if !strings.Contains(string(body), RoleOperator) {
				t.Errorf("expected role-denial message naming %s, got: %s", RoleOperator, body)
			}
		} else if resp.StatusCode == http.StatusForbidden &&
			strings.Contains(string(body), RoleOperator) {
			t.Fatalf("post-grant fresh login should clear the close role gate, got 403: %s", body)
		}
	})

	// ── Gate #1 (ownership): the second, independent wall ────────────
	// Even if a fresh login somehow cleared the role gate, the escalation
	// ownership carve-out (write_authz.go:225) admits ONLY operator+ OIDC
	// callers — a viewer/no-role identity is still denied. requireRole is a
	// mux wrapper, so we exercise the ownership predicate directly to prove it
	// is a distinct wall, not the same one. (This is the gate whose message
	// "ownership denied" was the literal 403 recorded on gd-zPklZK9xkN before
	// the carve-out; it now fails a below-operator OIDC caller here.)
	t.Run("Gate1_OwnershipDeniesBelowOperator", func(t *testing.T) {
		ctx := oidcCtx("newlogin@acme.example", freshLoginRoles)
		d := srv.authorizeOwnership(ctx, "close", ms.beads[escID])
		if wantDenied {
			if d == nil {
				t.Fatal("a below-operator OIDC login must be denied by the escalation ownership carve-out")
			}
			if !strings.Contains(d.Message, "ownership denied") {
				t.Errorf("expected 'ownership denied' message, got: %s", d.Message)
			}
			if !d.Escalatable {
				t.Error("ownership denial on an escalation should be escalatable")
			}
			t.Logf("REPRO ownership: %s", d.Message)
		} else if d != nil {
			t.Fatalf("post-grant operator-equivalent login must clear the ownership carve-out, got: %v", d)
		}
	})
}

// TestEscalationOperatorResolvesEndToEnd_GreenPath proves the resolve behavior
// the flat grant makes reachable for a fresh login: an identity that clears the
// operator predicate can LIST an open escalation, RESOLVE it, and see it close
// with operation_spec replay firing (approve) or result_status=failure
// (reject). Driven with an explicit operator token so the green-path assertion
// holds whether or not the flat grant has shipped.
func TestEscalationOperatorResolvesEndToEnd_GreenPath(t *testing.T) {
	const (
		adminToken  = "admin-greenpath-e2e"
		freshTok    = "oidc-greenpath-fresh"
		operatorTok = "oidc-greenpath-op"
		fleetName   = "greenpath-fleet"
	)

	// ── Approve path: list -> approve -> replay + auto-close ─────────
	t.Run("ApproveResolvesAndReplays", func(t *testing.T) {
		oidcMock := &escOIDCMock{
			tokens: map[string]escTokenInfo{
				operatorTok: {email: "operator@acme.example", roles: []string{RoleOperator}},
			},
		}
		fc := NewFleetConfig(fleetName, nil)
		ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)
		makeUserBead(ms, "gd-op", "operator@acme.example", "monorepo", "crew")

		// Seed a resolvable escalation with a replayable operation_spec: an
		// operator (dave) was denied creating a comment on a foreign bead;
		// approving must replay that create.
		esc := escBead("gd-greenpath-approve", "denied-agent")
		esc.Fields = json.RawMessage(`{
			"operation_type":"create",
			"operation_spec":{"title":"Replayed by approve","type":"task","labels":["project:monorepo"]}
		}`)
		ms.beads[esc.ID] = esc
		// The escalation is a monorepo escalation (its replay creates a
		// project:monorepo bead), so carry the project: label a real escalation
		// gets via its --requesting-task link. Without it the project-scoped
		// operator's own read-authz gate (ka-0000008132) correctly hides an
		// unlabeled escalation from a monorepo-restricted user.
		ms.labels[esc.ID] = []string{"project:monorepo"}

		// 1. LIST — the operator can see the open escalation.
		listResp := escRequest(t, ts, "GET", "/v1/escalations", operatorTok, nil, nil)
		defer listResp.Body.Close()
		if listResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(listResp.Body)
			t.Fatalf("list escalations: expected 200, got %d: %s", listResp.StatusCode, body)
		}
		var list struct {
			Escalations []map[string]any `json:"escalations"`
		}
		escDecode(t, listResp, &list)
		found := false
		for _, e := range list.Escalations {
			issue, _ := e["issue"].(map[string]any)
			if issue["id"] == esc.ID {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("open escalation %s not visible to operator in list", esc.ID)
		}

		// 2. RESOLVE — approve.
		apResp := escRequest(t, ts, "POST", "/v1/escalations/"+esc.ID+"/approve", operatorTok, map[string]any{
			"approved_by":     "operator@acme.example",
			"approval_method": "web_ui",
		}, nil)
		defer apResp.Body.Close()
		if apResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(apResp.Body)
			t.Fatalf("operator approve: expected 200, got %d: %s", apResp.StatusCode, body)
		}
		var apBody map[string]any
		escDecode(t, apResp, &apBody)

		// 3a. operation_spec replay fired.
		replay, ok := apBody["replay"].(map[string]any)
		if !ok {
			t.Fatalf("expected replay in approve response, got: %v", apBody)
		}
		if replay["replayed"] != true || replay["status"] != "success" {
			t.Fatalf("expected successful replay, got: %v", replay)
		}
		replayedID, _ := replay["bead_id"].(string)
		if replayedID == "" {
			t.Fatal("replay should return the created bead_id")
		}
		if rb := ms.beads[replayedID]; rb == nil || rb.Title != "Replayed by approve" {
			t.Errorf("replayed bead missing/incorrect: %+v", rb)
		}

		// 3b. escalation auto-closed on replay success.
		if got := ms.beads[esc.ID].Status; got != model.StatusClosed {
			t.Errorf("escalation should be auto-closed after approve+replay, got %s", got)
		}
	})

	// ── Reject path: resolve -> close with failure ──────────────────
	t.Run("RejectResolvesAndCloses", func(t *testing.T) {
		oidcMock := &escOIDCMock{
			tokens: map[string]escTokenInfo{
				operatorTok: {email: "operator2@acme.example", roles: []string{RoleOperator}},
			},
		}
		fc := NewFleetConfig(fleetName+"-rej", nil)
		ts, _, ms := newEscalationE2EServer(t, adminToken, fc, oidcMock)

		esc := escBead("gd-greenpath-reject", "denied-agent")
		esc.Fields = json.RawMessage(`{"operation_type":"comment","operation_spec":{}}`)
		ms.beads[esc.ID] = esc

		rejResp := escRequest(t, ts, "POST", "/v1/escalations/"+esc.ID+"/reject", operatorTok, map[string]any{
			"rejected_by": "operator2@acme.example",
			"reason":      "not approved",
		}, nil)
		defer rejResp.Body.Close()
		if rejResp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(rejResp.Body)
			t.Fatalf("operator reject: expected 200, got %d: %s", rejResp.StatusCode, body)
		}

		b := ms.beads[esc.ID]
		if b.Status != model.StatusClosed {
			t.Errorf("rejected escalation should be closed, got %s", b.Status)
		}
		var fields map[string]json.RawMessage
		json.Unmarshal(b.Fields, &fields)
		var resultStatus string
		json.Unmarshal(fields["result_status"], &resultStatus)
		if resultStatus != "failure" {
			t.Errorf("rejected escalation result_status=%q, want failure", resultStatus)
		}
	})
}
