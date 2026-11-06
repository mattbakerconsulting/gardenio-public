package tests

import (
	"slices"
	"strings"
	"testing"
)

// Per-land audiences (gd-eAfptUBh9Z, owner decision gd-zf6eceweED): every land
// the chart knows (keycloak.landScopes, keycloak.projectGroups) gets an
// optional client scope land-<name> whose only mapper adds aud land-<name> to
// the access token, attached to keycloak.landScopeClients and nothing else.

func landScopeSpecs(t *testing.T, objs []map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, o := range objectsOfKind(objs, "KeycloakClientScope") {
		s := specOf(o)
		if name, _ := s["name"].(string); strings.HasPrefix(name, "land-") {
			out[name] = s
		}
	}
	return out
}

func clientSpecs(objs []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, o := range objectsOfKind(objs, "KeycloakClient") {
		s := specOf(o)
		if id, _ := s["clientId"].(string); id != "" {
			out[id] = s
		}
	}
	return out
}

func stringsOf(v any) []string {
	var out []string
	for _, x := range listOf(v) {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func TestKeycloakLandScopes_NoneByDefault(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, nil, "keycloak.enabled=true"))
	if got := landScopeSpecs(t, objs); len(got) != 0 {
		t.Errorf("base renders land scopes %v, want none", got)
	}
}

func TestKeycloakLandScopes_K3sRendersChartLandsForLandScopeClientsOnly(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, "keycloak.enabled=true"))
	scopes := landScopeSpecs(t, objs)
	for _, land := range []string{"claimlayer", "scopetest"} {
		name := "land-" + land
		s, ok := scopes[name]
		if !ok {
			t.Errorf("k3s renders no client scope %s", name)
			continue
		}
		if s["type"] != "optional" || s["protocol"] != "openid-connect" {
			t.Errorf("%s: type %v protocol %v, want an optional openid-connect scope", name, s["type"], s["protocol"])
		}
		if mapOf(s["attributes"])["gardenio_land"] != land {
			t.Errorf("%s: no gardenio_land=%s marker", name, land)
		}
		mappers := listOf(s["protocolMappers"])
		if len(mappers) != 1 {
			t.Errorf("%s: %d mappers, want exactly the audience mapper", name, len(mappers))
			continue
		}
		m := mapOf(mappers[0])
		cfg := mapOf(m["config"])
		if m["protocolMapper"] != "oidc-audience-mapper" || cfg["included.custom.audience"] != name ||
			cfg["access.token.claim"] != "true" || cfg["included.client.audience"] != nil {
			t.Errorf("%s: mapper %v, want an access-token audience mapper adding %s", name, m, name)
		}
	}
	clients := clientSpecs(objs)
	// The CLI's device grant and the land-door verifier's password grant
	// (gd-Hq1PnUVOUC) mint land-door bearers; no other client may.
	landClients := []string{"gardenio-cli", "gardenio-land-verifier"}
	for _, id := range landClients {
		c, ok := clients[id]
		if !ok {
			t.Fatalf("k3s renders no %s client", id)
		}
		opt := stringsOf(c["optionalClientScopes"])
		for _, name := range []string{"land-claimlayer", "land-scopetest"} {
			if !slices.Contains(opt, name) {
				t.Errorf("%s optionalClientScopes %v lack %s", id, opt, name)
			}
		}
	}
	for id, c := range clients {
		if slices.Contains(landClients, id) {
			continue
		}
		for _, s := range append(stringsOf(c["optionalClientScopes"]), stringsOf(c["defaultClientScopes"])...) {
			if strings.HasPrefix(s, "land-") {
				t.Errorf("client %s carries land scope %s; only keycloak.landScopeClients may", id, s)
			}
		}
	}
}

func TestKeycloakLandScopes_ProjectGroupsAndExplicitListDeduped(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil, "keycloak.enabled=true",
		"keycloak.projectGroups[0].name=acme", "keycloak.landScopes={acme,beta}",
		"keycloak.clients[0].clientId=gardenio-cli", "keycloak.clients[1].clientId=other")
	objs := renderedObjects(t, out)
	scopes := landScopeSpecs(t, objs)
	if len(scopes) != 2 || scopes["land-acme"] == nil || scopes["land-beta"] == nil {
		t.Fatalf("land scopes = %v, want land-acme and land-beta once each", scopes)
	}
	clients := clientSpecs(objs)
	if got := stringsOf(clients["gardenio-cli"]["optionalClientScopes"]); !slices.Equal(got, []string{"land-acme", "land-beta"}) {
		t.Errorf("gardenio-cli optional scopes = %v", got)
	}
	if got := stringsOf(clients["other"]["optionalClientScopes"]); len(got) != 0 {
		t.Errorf("other client optional scopes = %v, want none", got)
	}
}

func TestKeycloakLandScopes_RefusesBadLandName(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"Acme", "a_b", strings.Repeat("a", 59)} {
		if _, err := helmTemplateGardenio(t, "templates/keycloak/client-scopes.yaml", "keycloak.enabled=true", "keycloak.landScopes={"+bad+"}"); err == nil {
			t.Errorf("land scope %q rendered", bad)
		}
	}
}
