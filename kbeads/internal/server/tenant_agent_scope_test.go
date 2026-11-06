package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/presence"
)

// A tenant land's per-agent token is gated like a scoped principal on the bare
// daemon (gd-c1DOWNYy7W): every classAdminOnly route but tenantAgentRoutes
// answers 403, so it reads no operator, fleet stat, taint, health snapshot,
// bus status or chart. An agent on an operator project is unchanged.
func TestTenantAgentRefusedAdminOnlyRoutes(t *testing.T) {
	f := seedLandAccounts(t)
	for _, pattern := range registeredRoutePatterns() {
		if class, ok := scopedRouteClasses[pattern]; (ok && class != classAdminOnly) || tenantAgentRoutes[pattern] {
			continue
		}
		method, path, _ := crossLandPath(pattern)
		var body any
		if method != http.MethodGet && method != http.MethodDelete {
			body = map[string]any{}
		}
		code, resp := f.do(t, tokClAgent, method, path, body)
		if code != http.StatusForbidden || !strings.Contains(resp, "tenant land's agents") {
			t.Errorf("tenant agent %s %s: %d %.200s, want the tenant route gate's 403", method, path, code, resp)
		}
	}
	for _, path := range []string{"/v1/operators", "/v1/stats", "/v1/taints"} {
		if code, resp := f.do(t, tokGardenioAgent, http.MethodGet, path, nil); strings.Contains(resp, "tenant land's agents") {
			t.Errorf("operator-project agent GET %s: %d %s, want past the tenant gate", path, code, resp)
		}
	}
}

// tenantAgentRoutes holds only admin-only routes: a classFiltered or
// classScopedSafe route needs no exemption, and an unregistered one is stale.
func TestTenantAgentRoutes_AreAdminOnlyAndRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, p := range registeredRoutePatterns() {
		registered[p] = true
	}
	for pattern := range tenantAgentRoutes {
		if !registered[pattern] {
			t.Errorf("tenantAgentRoutes names %q, which is not registered", pattern)
		}
		if scopedRouteClasses[pattern] != classAdminOnly {
			t.Errorf("tenantAgentRoutes names %q, which is not classAdminOnly", pattern)
		}
	}
}

// A tenant agent emits hooks only for its own agent bead, and its presence is
// recorded under its own name whatever the body's actor says. Another land's
// agent and a missing one answer alike. An operator-project agent is unchanged.
func TestTenantAgentHookEmitOwnOnly(t *testing.T) {
	f := seedCrossLands(t)
	f.srv.Presence = presence.New()
	for _, id := range []string{bvAgent, "bv-gone-agent"} {
		code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/hooks/emit", map[string]any{"agent_bead_id": id, "hook_type": "PreToolUse", "actor": "x"})
		if code != http.StatusNotFound {
			t.Errorf("A agent emits for %s: %d %s, want 404", id, code, body)
		}
	}
	code, body := f.do(t, tokClaimAgent, http.MethodPost, "/v1/hooks/emit", map[string]any{"agent_bead_id": "cl-agent-1", "hook_type": "PreToolUse", "actor": bvAgent})
	if code != http.StatusOK {
		t.Fatalf("A agent emits for itself: %d %s, want 200", code, body)
	}
	for _, e := range f.srv.Presence.Roster(time.Hour) {
		if e.Actor != "cl-agent-1" {
			t.Errorf("presence recorded for %q, want only the token's own name", e.Actor)
		}
	}

	f.seedLandAgent("gd-agent-1", "crew", "gardenio", tokGardenioAgent)
	if code, body := f.do(t, tokGardenioAgent, http.MethodPost, "/v1/hooks/emit", map[string]any{"hook_type": "PreToolUse", "actor": "someone"}); code != http.StatusOK {
		t.Errorf("operator-project agent emit: %d %s, want 200", code, body)
	}
}

// seedLabelLessConfig seeds the fleet's label-less advice and config: one
// config bead opted in with tenantSafeLabel, a role-only config bead and a
// Global advice bead that are not, and one config bead of claimlayer's own.
func seedLabelLessConfig(f *scopedFixture) {
	add := func(id, title string, typ model.BeadType, labels ...string) {
		f.ms.beads[id] = &model.Bead{ID: id, Title: title, Type: typ, Kind: model.KindConfig, Status: model.StatusOpen, Fleet: "default", Labels: labels}
		f.ms.labels[id] = labels
	}
	add("cfg-safe", "claude-instructions", "config", "global", tenantSafeLabel)
	add("cfg-crew", "claude-instructions", "config", "role:crew")
	add("adv-global", "Escalate for redress", "advice", "global")
	add("adv-fleet-group", "No admin merge", "advice", "g0:project:gardenio", "g0:role:crew")
	add("cfg-own", "claude-settings", "config", "project:claimlayer")
}

// A tenant agent reads only the tenant-safe label-less advice and config, by
// list and by id, beside its own land's; an operator-project agent reads every
// label-less one as before.
func TestTenantAgentLabelLessConfigOnlyTenantSafe(t *testing.T) {
	f := seedLandAccounts(t)
	seedLabelLessConfig(f)
	hidden := []string{"cfg-crew", "adv-global", "adv-fleet-group"}

	for _, path := range []string{"/v1/beads?type=config", "/v1/beads?type=config,advice", "/v1/beads?type=advice"} {
		code, body := f.do(t, tokClAgent, http.MethodGet, path+"&limit=100", nil)
		if code != http.StatusOK {
			t.Fatalf("tenant agent GET %s: %d %s", path, code, body)
		}
		for _, id := range hidden {
			if strings.Contains(body, `"`+id+`"`) {
				t.Errorf("tenant agent GET %s lists %s: %.300s", path, id, body)
			}
		}
		if strings.Contains(path, "config") && (!strings.Contains(body, `"cfg-safe"`) || !strings.Contains(body, `"cfg-own"`)) {
			t.Errorf("tenant agent GET %s: want cfg-safe and cfg-own, got %.300s", path, body)
		}
	}
	for _, id := range hidden {
		if code, body := f.do(t, tokClAgent, http.MethodGet, "/v1/beads/"+id, nil); code/100 == 2 {
			t.Errorf("tenant agent GET %s: %d %.200s, want refused", id, code, body)
		}
	}
	if code, body := f.do(t, tokClAgent, http.MethodGet, "/v1/beads/cfg-safe", nil); code != http.StatusOK {
		t.Errorf("tenant agent GET cfg-safe: %d %s, want 200", code, body)
	}

	code, body := f.do(t, tokGardenioAgent, http.MethodGet, "/v1/beads?type=config,advice&limit=100", nil)
	for _, id := range append(hidden, "cfg-safe") {
		if code != http.StatusOK || !strings.Contains(body, `"`+id+`"`) {
			t.Errorf("operator-project agent list: %d, want %s in %.300s", code, id, body)
		}
	}
}
