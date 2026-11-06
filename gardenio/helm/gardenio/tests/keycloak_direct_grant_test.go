package tests

import (
	"slices"
	"strings"
	"testing"
)

// The password grant (directAccess) skips the admin passkey, which lives in
// the browser flow. A public client with it takes any realm user's password
// from anyone, so the chart makes such a client name a directGrantRole, and
// the k3s overlay has exactly one: the land-door verifier (gd-Hq1PnUVOUC,
// merge desk send-backs on PR #2019).

const (
	verifierClient = "gardenio-land-verifier"
	verifierRole   = "gardenio-land-verifier"
	verifierUser   = "land-verifier-scopetest@lands.clonk.tv"
)

func executionsOf(flow map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range listOf(flow["authenticationExecutions"]) {
		out = append(out, mapOf(e))
	}
	return out
}

func TestKeycloakDirectGrant_K3sOnlyTheVerifierHasDirectAccess(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, "keycloak.enabled=true"))
	clients := clientSpecs(objs)
	if _, ok := clients[verifierClient]; !ok {
		t.Fatalf("k3s renders no %s client", verifierClient)
	}
	for id, c := range clients {
		if c["directAccess"] == true && id != verifierClient {
			t.Errorf("client %s has directAccess: true; only %s may", id, verifierClient)
		}
	}
}

// The verifier's password grant runs a flow that refuses every user without
// gardenio-land-verifier before the password is checked, and its token
// carries no realm role but gardenio-project-member.
func TestKeycloakDirectGrant_K3sVerifierIsLockedToItsRole(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, "keycloak.enabled=true"))
	c := clientSpecs(objs)[verifierClient]
	if c == nil {
		t.Fatalf("k3s renders no %s client", verifierClient)
	}
	if c["directAccess"] != true || c["standardFlowEnabled"] != false {
		t.Errorf("%s directAccess/standardFlowEnabled = %v/%v, want true/false", verifierClient, c["directAccess"], c["standardFlowEnabled"])
	}
	if c["fullScopeAllowed"] != false {
		t.Errorf("%s fullScopeAllowed = %v, want false (a full-scope token carries gardenio-admin)", verifierClient, c["fullScopeAllowed"])
	}
	var hardcoded []any
	for _, pm := range listOf(c["protocolMappers"]) {
		if m := mapOf(pm); m["protocolMapper"] == "oidc-hardcoded-role-mapper" {
			hardcoded = append(hardcoded, mapOf(m["config"])["role"])
		}
	}
	if !slices.Equal(hardcoded, []any{"gardenio-project-member"}) {
		t.Errorf("%s hardcoded roles = %v, want only gardenio-project-member", verifierClient, hardcoded)
	}

	alias := verifierClient + "-direct-grant"
	if got := mapOf(c["authenticationFlowBindingOverrides"])["directGrant"]; got != alias {
		t.Fatalf("%s direct grant flow = %v, want %s", verifierClient, got, alias)
	}
	flow := authFlowByAlias(objs, alias)
	if flow == nil {
		t.Fatalf("no KeycloakAuthFlow %s rendered", alias)
	}
	if flow["topLevel"] != true || flow["providerId"] != "basic-flow" {
		t.Errorf("flow %s topLevel/providerId = %v/%v, want true/basic-flow", alias, flow["topLevel"], flow["providerId"])
	}
	gateAlias := alias + "-role-gate"
	type step struct {
		name, requirement string
		priority          int
	}
	var got []step
	for _, e := range executionsOf(flow) {
		name, _ := e["authenticator"].(string)
		if e["authenticatorFlow"] == true {
			name, _ = e["alias"].(string)
		}
		prio, _ := e["priority"].(int)
		if f, ok := e["priority"].(float64); ok {
			prio = int(f)
		}
		got = append(got, step{name, e["requirement"].(string), prio})
	}
	want := []step{
		{"direct-grant-validate-username", "REQUIRED", 10},
		{gateAlias, "CONDITIONAL", 20},
		{"direct-grant-validate-password", "REQUIRED", 30},
	}
	if !slices.Equal(got, want) {
		t.Errorf("flow %s executions = %v, want %v (the role gate between the username and the password check)", alias, got, want)
	}

	gate := authFlowByAlias(objs, gateAlias)
	if gate == nil {
		t.Fatalf("no KeycloakAuthFlow %s rendered", gateAlias)
	}
	if gate["parentName"] != alias || gate["childRequirement"] != "CONDITIONAL" || gate["topLevel"] != false {
		t.Errorf("gate %s parentName/childRequirement/topLevel = %v/%v/%v, want %s/CONDITIONAL/false",
			gateAlias, gate["parentName"], gate["childRequirement"], gate["topLevel"], alias)
	}
	ge := executionsOf(gate)
	if len(ge) != 2 {
		t.Fatalf("gate %s has %d executions, want 2: %v", gateAlias, len(ge), ge)
	}
	cond := mapOf(mapOf(ge[0]["authenticatorConfig"])["config"])
	if ge[0]["authenticator"] != "conditional-user-role" || cond["condUserRole"] != verifierRole || cond["negate"] != "true" {
		t.Errorf("gate condition = %v, want conditional-user-role on NOT %s", ge[0], verifierRole)
	}
	if ge[1]["authenticator"] != "deny-access-authenticator" || ge[1]["requirement"] != "REQUIRED" {
		t.Errorf("gate action = %v, want deny-access-authenticator REQUIRED", ge[1])
	}
}

// The role the verifier's flow admits is held by its test owner and nobody
// else: no other realm user, no group, no composite.
func TestKeycloakDirectGrant_K3sOnlyTheTestOwnerHoldsTheRole(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, "keycloak.enabled=true"))
	var holders []string
	for _, o := range objectsOfKind(objs, "KeycloakRealmUser") {
		s := specOf(o)
		if slices.Contains(stringsOf(s["roles"]), verifierRole) {
			holders = append(holders, s["username"].(string))
		}
	}
	if !slices.Equal(holders, []string{verifierUser}) {
		t.Errorf("realm users holding %s = %v, want only %s", verifierRole, holders, verifierUser)
	}
	for _, o := range objectsOfKind(objs, "KeycloakRealmGroup") {
		if s := specOf(o); slices.Contains(stringsOf(s["realmRoles"]), verifierRole) {
			t.Errorf("group %v grants %s", s["name"], verifierRole)
		}
	}
	for _, o := range objectsOfKind(objs, "KeycloakRealmRole") {
		s := specOf(o)
		for _, comp := range listOf(s["composites"]) {
			if mapOf(comp)["name"] == verifierRole {
				t.Errorf("role %v composes %s", s["name"], verifierRole)
			}
		}
	}
}

func TestKeycloakDirectGrant_ChartRefusesAnUngatedPasswordGrant(t *testing.T) {
	t.Parallel()
	const tpl = "templates/keycloak/clients.yaml"
	base := []string{"keycloak.enabled=true", "keycloak.clients[0].clientId=pw-client"}
	for _, tc := range []struct {
		name, wantErr string
		set           []string
	}{
		{"public password grant without a role", "a public client with directAccess must set directGrantRole",
			[]string{"keycloak.clients[0].public=true", "keycloak.clients[0].directAccess=true"}},
		{"role without the password grant", "directGrantRole needs directAccess: true",
			[]string{"keycloak.clients[0].public=true", "keycloak.clients[0].directGrantRole=pw-role"}},
	} {
		out, err := helmTemplateGardenio(t, tpl, append(base, tc.set...)...)
		if err == nil || !strings.Contains(out, tc.wantErr) {
			t.Errorf("%s: err = %v, want a fail with %q:\n%s", tc.name, err, tc.wantErr, out)
		}
	}
	out, err := helmTemplateGardenio(t, "templates/keycloak/auth-flow-direct-grant.yaml",
		append(base, "keycloak.clients[0].public=true", "keycloak.clients[0].directAccess=true", "keycloak.clients[0].directGrantRole=Bad_Role")...)
	if err == nil || !strings.Contains(out, "must be a lowercase role name") {
		t.Errorf("bad directGrantRole: err = %v, want a fail:\n%s", err, out)
	}
	// A confidential client's password grant needs its secret, so it may
	// keep the default flow (gardenio-beads-api in the base values).
	if out, err := helmTemplateGardenio(t, tpl, append(base, "keycloak.clients[0].directAccess=true")...); err != nil {
		t.Errorf("confidential directAccess client failed to render: %v\n%s", err, out)
	}
}
