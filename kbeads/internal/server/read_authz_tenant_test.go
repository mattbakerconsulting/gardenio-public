package server

import (
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

const (
	tokClAgent       = "cl-agent-tok"
	tokGardenioAgent = "gd-agent-tok"
	tenantSecretKey  = "sk-tenant-secret"
)

// seedLandAccounts seeds claimlayer as a tenant land with its lands-claimlayer
// account bead, a gardenio account bead, a per-agent token on each project,
// and returns the fixture.
func seedLandAccounts(t *testing.T) *scopedFixture {
	t.Helper()
	f := newScopedFixture(t)
	f.seedLandProject(t, "false")
	for id, project := range map[string]string{"acct-cl": "claimlayer", "acct-gd": "gardenio"} {
		f.seedTyped(id, model.TypeAccount, "project:"+project)
		f.ms.beads[id].Kind = model.KindConfig
		f.ms.beads[id].Fields = []byte(`{"litellm_key":"` + tenantSecretKey + `","total_cost_usd":"12"}`)
	}
	f.seedLandAgent("cl-agent-1", "steward", "claimlayer", tokClAgent)
	f.seedLandAgent("gd-agent-1", "crew", "gardenio", tokGardenioAgent)
	return f
}

// A land member, the land principal and an agent on a tenant land read no
// account bead, their own land's included: not by id, not in a list, not in a
// search or a batch, and not through its comments or events (gd-q8jegjvKI3).
// They still read the land's other beads.
func TestTenantReadsNoAccountBead(t *testing.T) {
	f := seedLandAccounts(t)
	for _, tok := range []string{tokScopedWriter, tokScopedClaimlayer, tokLandClaimlayer, tokClAgent} {
		for _, path := range []string{
			"/v1/beads/acct-cl",
			"/v1/beads/acct-cl/comments",
			"/v1/beads/acct-cl/events",
		} {
			code, body := f.do(t, tok, http.MethodGet, path, nil)
			if code != http.StatusForbidden || strings.Contains(body, tenantSecretKey) {
				t.Errorf("%s GET %s: %d %s, want 403", tok, path, code, body)
			}
		}
		for _, path := range []string{
			"/v1/beads?type=account",
			"/v1/beads?limit=100",
			"/v1/events?limit=100",
		} {
			code, body := f.do(t, tok, http.MethodGet, path, nil)
			if code != http.StatusOK || strings.Contains(body, "acct-cl") || strings.Contains(body, tenantSecretKey) {
				t.Errorf("%s GET %s: %d %s, want 200 without the account bead", tok, path, code, body)
			}
		}
		// The batch answers a hidden id as not found, as it does any id the
		// caller may not read.
		code, body := f.do(t, tok, http.MethodGet, "/v1/batch/beads?ids=acct-cl,"+beadClaimlayer, nil)
		if code != http.StatusOK || !strings.Contains(body, `{"id":"acct-cl","error":"not found"}`) || strings.Contains(body, tenantSecretKey) {
			t.Errorf("%s batch: %d %s, want acct-cl not found", tok, code, body)
		}
		if code, body := f.do(t, tok, http.MethodGet, "/v1/beads/"+beadClaimlayer, nil); code != http.StatusOK {
			t.Errorf("%s GET its land's task: %d %s", tok, code, body)
		}
	}
}

// The gate is for tenants only: an agent on an operator project, an operator
// and an admin read account beads as before.
func TestNonTenantReadsAccountBead(t *testing.T) {
	f := seedLandAccounts(t)
	for _, c := range []struct{ tok, id string }{
		{tokGardenioAgent, "acct-gd"},
		{tokOwnerAdmin, "acct-cl"},
		{scopedAdminToken, "acct-cl"},
	} {
		code, body := f.do(t, c.tok, http.MethodGet, "/v1/beads/"+c.id, nil)
		if code != http.StatusOK || !strings.Contains(body, tenantSecretKey) {
			t.Errorf("%s GET %s: %d %s, want 200", c.tok, c.id, code, body)
		}
		code, body = f.do(t, c.tok, http.MethodGet, "/v1/beads?type=account", nil)
		if code != http.StatusOK || !strings.Contains(body, c.id) {
			t.Errorf("%s list accounts: %d %s, want %s", c.tok, code, body, c.id)
		}
	}
}

// A tenant agent may not tail the unscoped event bus; an agent on an operator
// project reaches the handler (503 here: no bus is configured).
func TestTenantAgentRefusedEventBus(t *testing.T) {
	f := seedLandAccounts(t)
	if code, body := f.do(t, tokClAgent, http.MethodGet, "/v1/bus/events", nil); code != http.StatusForbidden {
		t.Errorf("tenant agent: %d %s, want 403", code, body)
	}
	if code, body := f.do(t, tokGardenioAgent, http.MethodGet, "/v1/bus/events", nil); code != http.StatusServiceUnavailable {
		t.Errorf("operator-project agent: %d %s, want 503", code, body)
	}
}

// A land that is not a tenant land does not hide account beads from its
// agents; flipping it to a tenant land does (after the cache window).
func TestTenantAgentGateFollowsLandKind(t *testing.T) {
	f := seedLandAccounts(t)
	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"operator"}`)
	if code, body := f.do(t, tokClAgent, http.MethodGet, "/v1/beads/acct-cl", nil); code != http.StatusOK {
		t.Fatalf("agent on an operator land: %d %s, want 200", code, body)
	}
	f.srv.tenantLands = tenantLandCache{}
	f.ms.beads["proj-claimlayer"].Fields = []byte(`{"land_kind":"tenant"}`)
	if code, body := f.do(t, tokClAgent, http.MethodGet, "/v1/beads/acct-cl", nil); code != http.StatusForbidden {
		t.Fatalf("agent on a tenant land: %d %s, want 403", code, body)
	}
}

// A tenant's SSE client drops an account bead's events, and one whose bead
// type did not resolve; a client that hides nothing passes both.
func TestSSEClientHidesTenantTypes(t *testing.T) {
	tenant := &sseClient{hiddenTypes: tenantHiddenTypes}
	tenant.setScope([]string{"claimlayer"}, beadTaintGateState{exempt: true})
	open := &sseClient{}
	open.setScope(nil, beadTaintGateState{exempt: true})
	labels := []string{"project:claimlayer"}
	if tenant.matchesScope("acct-cl", string(model.TypeAccount), "", labels, nil) {
		t.Error("a tenant client matched an account bead's event")
	}
	if !tenant.matchesScope(beadClaimlayer, "task", "", labels, nil) {
		t.Error("a tenant client refused its land's task")
	}
	if tenant.matchesTypeScope("x-1", "") {
		t.Error("a tenant client matched a bead whose type did not resolve")
	}
	if !tenant.matchesTypeScope("x-1", "task") || !tenant.matchesTypeScope("", "") {
		t.Error("a tenant client refused a task or a bead-less event")
	}
	if !open.matchesScope("acct-cl", string(model.TypeAccount), "", labels, nil) {
		t.Error("an unscoped client refused an account bead's event")
	}
}

// A person and a land principal read no gatehouse_request (gd-R7meWqNxqr):
// not by id, not in a list, not in a batch, not through its events. The
// steward's per-agent token, which polls the requests it files, and an admin
// still read it.
func TestScopedPrincipalReadsNoGatehouseRequest(t *testing.T) {
	f := seedLandAccounts(t)
	f.seedTyped("ghr-1", typeGatehouseRequest, "project:claimlayer")
	f.ms.beads["ghr-1"].Fields = []byte(`{"kind":"apply","land_name":"secret-patch","github_login":"pat","state":"drafted"}`)
	for _, tok := range []string{tokScopedWriter, tokScopedClaimlayer, tokLandClaimlayer} {
		for _, path := range []string{"/v1/beads/ghr-1", "/v1/beads/ghr-1/events"} {
			if code, body := f.do(t, tok, http.MethodGet, path, nil); code != http.StatusForbidden || strings.Contains(body, "secret-patch") {
				t.Errorf("%s GET %s: %d %s, want 403", tok, path, code, body)
			}
		}
		for _, path := range []string{"/v1/beads?type=gatehouse_request", "/v1/beads?limit=100", "/v1/events?limit=100"} {
			if code, body := f.do(t, tok, http.MethodGet, path, nil); code != http.StatusOK || strings.Contains(body, "ghr-1") {
				t.Errorf("%s GET %s: %d %s, want 200 without the request", tok, path, code, body)
			}
		}
		if code, body := f.do(t, tok, http.MethodGet, "/v1/batch/beads?ids=ghr-1", nil); code != http.StatusOK || !strings.Contains(body, `{"id":"ghr-1","error":"not found"}`) {
			t.Errorf("%s batch: %d %s, want ghr-1 not found", tok, code, body)
		}
	}
	for _, tok := range []string{tokClAgent, tokOwnerAdmin, scopedAdminToken} {
		if code, body := f.do(t, tok, http.MethodGet, "/v1/beads/ghr-1", nil); code != http.StatusOK || !strings.Contains(body, "secret-patch") {
			t.Errorf("%s GET ghr-1: %d %s, want 200", tok, code, body)
		}
		if code, body := f.do(t, tok, http.MethodGet, "/v1/beads?type=gatehouse_request", nil); code != http.StatusOK || !strings.Contains(body, "ghr-1") {
			t.Errorf("%s list requests: %d %s, want ghr-1", tok, code, body)
		}
	}
}
