package tests

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

// Literal admin impersonation, V1 (gd-NJkG7x9wb7, gd-X2xwS9MoBP): Keycloak's
// own Users > <user> > Impersonate, with no gardenio code. The chart's part is
// two settings: gardenio-admins holds the realm-management client roles
// impersonation + view-users (and no other group holds any client role), and
// the realm records admin events, so every IMPERSONATE is on the record. A
// later helm upgrade must not revert either one, on the chart default or on
// the k3s overlay (which re-declares the whole groups list).

var impersonationClientRoles = []map[string]any{
	{"clientId": "realm-management", "roles": []any{"impersonation", "view-users"}},
}

type impersonationGroup struct {
	Spec struct {
		Name        string           `yaml:"name"`
		ClientRoles []map[string]any `yaml:"clientRoles"`
	} `yaml:"spec"`
}

type eventsRealm struct {
	Spec map[string]any `yaml:"spec"`
}

func renderWithK3sOverlay(t *testing.T, tpl string) string {
	t.Helper()
	out, err := exec.Command("helm", "template", "gardenio", chartDir(t),
		"--show-only", tpl, "-f", deployValuesK3s(t)).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template %s with k3s overlay failed: %v\n%s", tpl, err, out)
	}
	return string(out)
}

// assertOnlyAdminsImpersonate: gardenio-admins carries exactly the two
// realm-management roles, and every other group carries no client role.
func assertOnlyAdminsImpersonate(t *testing.T, rendered string) {
	t.Helper()
	sawAdmins := false
	for _, g := range decodeDocs[impersonationGroup](t, rendered) {
		if g.Spec.Name == "gardenio-admins" {
			sawAdmins = true
			if !reflect.DeepEqual(g.Spec.ClientRoles, impersonationClientRoles) {
				t.Errorf("gardenio-admins clientRoles = %v, want %v", g.Spec.ClientRoles, impersonationClientRoles)
			}
			continue
		}
		if len(g.Spec.ClientRoles) != 0 {
			t.Errorf("group %s must carry no clientRoles (impersonation is admins only), got %v", g.Spec.Name, g.Spec.ClientRoles)
		}
	}
	if !sawAdmins {
		t.Fatalf("no gardenio-admins group rendered:\n%s", rendered)
	}
}

func TestKeycloakImpersonation_ChartDefaultGrantsAdminsOnly(t *testing.T) {
	t.Parallel()
	assertOnlyAdminsImpersonate(t, helmTemplate(t, groupsTpl, "keycloak.enabled=true"))
}

func TestKeycloakImpersonation_K3sOverlayGrantsAdminsOnly(t *testing.T) {
	t.Parallel()
	assertOnlyAdminsImpersonate(t, renderWithK3sOverlay(t, groupsTpl))
}

func realmEventConfig(t *testing.T, rendered string) map[string]any {
	t.Helper()
	docs := decodeDocs[eventsRealm](t, rendered)
	if len(docs) != 1 {
		t.Fatalf("want 1 KeycloakRealm, got %d:\n%s", len(docs), rendered)
	}
	cfg, _ := docs[0].Spec["realmEventConfig"].(map[string]any)
	return cfg
}

// assertAdminEventsOn pins the whole block: the operator PUTs it as is, and
// Keycloak empties any field the body leaves out (eventsListeners would lose
// jboss-logging). User events are IMPERSONATE only: an empty
// enabledEventTypes would record every login too.
func assertAdminEventsOn(t *testing.T, cfg map[string]any) {
	t.Helper()
	want := map[string]any{
		"adminEventsEnabled":        true,
		"adminEventsDetailsEnabled": true,
		"adminEventsExpiration":     7776000,
		"eventsEnabled":             true,
		"enabledEventTypes":         []any{"IMPERSONATE"},
		"eventsExpiration":          7776000,
		"eventsListeners":           []any{"jboss-logging"},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("realmEventConfig = %v, want %v", cfg, want)
	}
}

func TestKeycloakRealmEvents_ChartDefaultAdminEventsOn(t *testing.T) {
	t.Parallel()
	assertAdminEventsOn(t, realmEventConfig(t, helmTemplate(t, realmTemplate, "keycloak.enabled=true")))
}

func TestKeycloakRealmEvents_K3sOverlayAdminEventsOn(t *testing.T) {
	t.Parallel()
	assertAdminEventsOn(t, realmEventConfig(t, renderWithK3sOverlay(t, realmTemplate)))
}

func TestKeycloakRealmEvents_DisabledRendersNone(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, realmTemplate, "keycloak.enabled=true", "keycloak.realm.events.enabled=false")
	if cfg := realmEventConfig(t, out); cfg != nil {
		t.Errorf("events.enabled=false must render no realmEventConfig, got %v", cfg)
	}
}

// One-click Become (plan gd-0SUIdKXWV3, step gd-rbML2txvGG). Two Keycloak
// pieces: a public client gardenio-become whose token can call the admin API's
// impersonation endpoint (only the three realm-management roles in scope), and
// two session-note mappers on the forwardauth client so a token minted for an
// impersonated session names the admin behind it.

type renderedClient struct {
	Spec struct {
		ClientID            string            `yaml:"clientId"`
		Public              bool              `yaml:"public"`
		StandardFlowEnabled bool              `yaml:"standardFlowEnabled"`
		DirectAccess        bool              `yaml:"directAccess"`
		FullScopeAllowed    bool              `yaml:"fullScopeAllowed"`
		ImplicitFlowEnabled bool              `yaml:"implicitFlowEnabled"`
		RedirectUris        []string          `yaml:"redirectUris"`
		WebOrigins          []string          `yaml:"webOrigins"`
		Attributes          map[string]string `yaml:"attributes"`
		ServiceAccount      map[string]any    `yaml:"serviceAccount"`
		ProtocolMappers     []struct {
			Name           string            `yaml:"name"`
			Protocol       string            `yaml:"protocol"`
			ProtocolMapper string            `yaml:"protocolMapper"`
			Config         map[string]string `yaml:"config"`
		} `yaml:"protocolMappers"`
	} `yaml:"spec"`
}

func renderedClientByID(t *testing.T, rendered, clientID string) renderedClient {
	t.Helper()
	for _, c := range decodeDocs[renderedClient](t, rendered) {
		if c.Spec.ClientID == clientID {
			return c
		}
	}
	t.Fatalf("no KeycloakClient %s rendered:\n%s", clientID, rendered)
	return renderedClient{}
}

func TestKeycloakBecome_K3sOverlayClient(t *testing.T) {
	t.Parallel()
	out := renderWithK3sOverlay(t, keycloakClients)
	c := renderedClientByID(t, out, "gardenio-become").Spec
	if !c.Public || !c.StandardFlowEnabled {
		t.Errorf("gardenio-become must be public with the standard flow on (public=%v standardFlow=%v)", c.Public, c.StandardFlowEnabled)
	}
	if c.DirectAccess || c.ImplicitFlowEnabled || c.ServiceAccount != nil {
		t.Errorf("gardenio-become must have no direct grants, implicit flow or service account (direct=%v implicit=%v sa=%v)",
			c.DirectAccess, c.ImplicitFlowEnabled, c.ServiceAccount)
	}
	if c.FullScopeAllowed {
		t.Error("gardenio-become must render fullScopeAllowed: false (its token carries only the scope-mapped roles)")
	}
	if want := []string{"https://auth.clonk.tv/gardenio/become/*"}; !reflect.DeepEqual(c.RedirectUris, want) {
		t.Errorf("redirectUris = %v, want %v", c.RedirectUris, want)
	}
	if want := []string{"https://auth.clonk.tv"}; !reflect.DeepEqual(c.WebOrigins, want) {
		t.Errorf("webOrigins = %v, want %v", c.WebOrigins, want)
	}
	if got := c.Attributes["pkce.code.challenge.method"]; got != "S256" {
		t.Errorf("pkce.code.challenge.method = %q, want S256", got)
	}
	if got := c.Attributes["oauth2.device.authorization.grant.enabled"]; got == "true" {
		t.Error("gardenio-become must not enable the device grant")
	}
	// scopeMappings is chart-only input for the hook, not a KeycloakClient field.
	if doc := clientDoc(out, "gardenio-become"); strings.Contains(doc, "scopeMappings") {
		t.Errorf("scopeMappings must not reach the KeycloakClient CR:\n%s", doc)
	}
}

// The hook adds exactly the three realm-management roles to the client's
// scope, in its non-fatal scope-mappings block.
func TestKeycloakBecome_K3sOverlayHookMapsScope(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, firstAdminJob, []string{k3sOverlay})
	b := strings.Index(out, "# --- scope-mappings:begin")
	e := strings.Index(out, "# --- scope-mappings:end")
	if b < 0 || e < b {
		t.Fatalf("scope-mappings block markers missing (begin=%d end=%d):\n%s", b, e, out)
	}
	block := out[b:e]
	if !strings.Contains(block, "scope_map gardenio-become realm-management impersonation view-users query-users\n") {
		t.Errorf("block must map exactly impersonation, view-users and query-users onto gardenio-become:\n%s", block)
	}
	if n := strings.Count(block, "\n              scope_map "); n != 1 {
		t.Errorf("want 1 scope_map call (only gardenio-become has scopeMappings), got %d", n)
	}
	for _, w := range []string{
		`kcadm create "clients/${SM_CID}/scope-mappings/clients/${SM_TID}"`,
		"WARNING: could not add ${SM_CLIENT} scope mappings",
	} {
		if !strings.Contains(block, w) {
			t.Errorf("block missing %q", w)
		}
	}
	if strings.Contains(block, "die ") {
		t.Error("the scope-mappings step must warn, never die: the admin bootstrap after it still has to run")
	}
	realm := strings.Index(out, "realm ${KC_REALM} present")
	grp := strings.Index(out, "# 3) Wait for the target group")
	if !(realm >= 0 && realm < b && e < grp) {
		t.Errorf("scope-mappings step must sit between the realm wait and the group wait (realm=%d begin=%d group=%d)", realm, b, grp)
	}
}

func TestKeycloakBecome_ChartDefaultHasNoScopeMappingStep(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, firstAdminJob, nil,
		"keycloak.enabled=true", "keycloak.runtime.enabled=true")
	if !strings.Contains(out, "[first-admin] DONE") {
		t.Fatalf("first-admin hook did not render:\n%s", out)
	}
	if strings.Contains(out, "scope-mappings:begin") || strings.Contains(out, "scope_map") {
		t.Error("no chart-default client has scopeMappings: the hook must carry no scope-mappings step")
	}
}

func TestKeycloakBecome_ScopeMappingsValidated(t *testing.T) {
	t.Parallel()
	base := []string{
		"--set", "keycloak.enabled=true", "--set", "keycloak.runtime.enabled=true",
		"--set", "keycloak.clients[0].clientId=probe",
		"--set", "keycloak.clients[0].scopeMappings[0].clientId=realm-management",
	}
	for name, tc := range map[string]struct {
		extra []string
		want  string
	}{
		"full scope": {
			[]string{"--set", "keycloak.clients[0].scopeMappings[0].roles={view-users}"},
			"scopeMappings needs fullScopeAllowed: false",
		},
		"shell in a role": {
			[]string{"--set", "keycloak.clients[0].fullScopeAllowed=false",
				"--set", "keycloak.clients[0].scopeMappings[0].roles={view-users;id}"},
			"must be a lowercase role name",
		},
		"no roles": {
			[]string{"--set", "keycloak.clients[0].fullScopeAllowed=false"},
			"must list at least one role",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := helmTemplateExpectError(t, firstAdminJob, append(append([]string{}, base...), tc.extra...)...)
			if !strings.Contains(out, tc.want) {
				t.Errorf("want error %q, got:\n%s", tc.want, out)
			}
		})
	}
}

// assertImpersonatorMappers: the forwardauth client maps both session notes
// into the access and ID tokens.
func assertImpersonatorMappers(t *testing.T, rendered string) {
	t.Helper()
	c := renderedClientByID(t, rendered, "gardenio-traefik-forwardauth").Spec
	want := map[string]map[string]string{
		"impersonator":    {"user.session.note": "IMPERSONATOR_USERNAME", "claim.name": "impersonator"},
		"impersonator-id": {"user.session.note": "IMPERSONATOR_ID", "claim.name": "impersonator_id"},
	}
	for _, m := range c.ProtocolMappers {
		w, ok := want[m.Name]
		if !ok {
			continue
		}
		delete(want, m.Name)
		if m.Protocol != "openid-connect" || m.ProtocolMapper != "oidc-usersessionmodel-note-mapper" {
			t.Errorf("mapper %s: protocol=%q protocolMapper=%q, want openid-connect / oidc-usersessionmodel-note-mapper", m.Name, m.Protocol, m.ProtocolMapper)
		}
		for k, v := range map[string]string{
			"user.session.note":  w["user.session.note"],
			"claim.name":         w["claim.name"],
			"jsonType.label":     "String",
			"id.token.claim":     "true",
			"access.token.claim": "true",
		} {
			if m.Config[k] != v {
				t.Errorf("mapper %s: config %s = %q, want %q", m.Name, k, m.Config[k], v)
			}
		}
	}
	for name := range want {
		t.Errorf("gardenio-traefik-forwardauth has no %s mapper", name)
	}
}

func TestKeycloakImpersonatorClaim_ChartDefault(t *testing.T) {
	t.Parallel()
	assertImpersonatorMappers(t, helmTemplate(t, keycloakClients, "keycloak.enabled=true"))
}

func TestKeycloakImpersonatorClaim_K3sOverlay(t *testing.T) {
	t.Parallel()
	assertImpersonatorMappers(t, renderWithK3sOverlay(t, keycloakClients))
}
