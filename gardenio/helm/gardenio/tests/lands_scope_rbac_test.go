package tests

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Per-land audience scopes made at runtime (gd-eAfptUBh9Z, PR #862 review):
// templates/controller/lands-scope-rbac.yaml lets the controller write
// KeycloakClientScope resources and holds every such write to the land scope
// shape internal/lands/landscope renders, so the provisioner's Keycloak
// credential never needs manage-clients.

const landScopeRBAC = "templates/controller/lands-scope-rbac.yaml"

func renderLandScopeRBAC(t *testing.T, setFlags ...string) string {
	t.Helper()
	return helmTemplateNS(t, "gardenio", landScopeRBAC, append([]string{landsProvOn, "keycloak.enabled=true"}, setFlags...)...)
}

func TestLandScopes_OffWithoutProvisionerOrKeycloak(t *testing.T) {
	t.Parallel()
	for _, flags := range [][]string{{"agents.enabled=true", "keycloak.enabled=true"}, {landsProvOn, "keycloak.enabled=false"}} {
		out, err := helmTemplateGardenio(t, landScopeRBAC, flags...)
		if err == nil && strings.Contains(out, "kind:") {
			t.Errorf("%v renders land scope RBAC:\n%s", flags, out)
		}
	}
}

// The grant is get/create/update on keycloakclientscopes in the release
// namespace only: no delete, no status, no keycloakclients (attaching scopes
// to clients stays the chart's).
func TestLandScopes_GrantIsNarrow(t *testing.T) {
	t.Parallel()
	out := renderLandScopeRBAC(t)
	role := landDoc(t, out, "Role", "test-agents-land-scopes")
	for _, want := range []string{"namespace: gardenio", `apiGroups: ["v1.edp.epam.com"]`, `resources: ["keycloakclientscopes"]`, `verbs: ["get", "create", "update"]`} {
		if !strings.Contains(role, want) {
			t.Errorf("Role lacks %q:\n%s", want, role)
		}
	}
	for _, bad := range []string{"delete", "keycloakclients\"", "status", "*"} {
		if strings.Contains(role, bad) {
			t.Errorf("Role grants %q:\n%s", bad, role)
		}
	}
	rb := landDoc(t, out, "RoleBinding", "test-agents-land-scopes")
	if !strings.Contains(rb, "name: test-agents-sa") || !strings.Contains(rb, "kind: Role\n") {
		t.Errorf("RoleBinding does not bind the Role to the controller SA:\n%s", rb)
	}
	b := landDoc(t, out, "ValidatingAdmissionPolicyBinding", "test-agents-land-scopes")
	if !strings.Contains(b, `validationActions: ["Deny"]`) {
		t.Errorf("binding does not deny:\n%s", b)
	}
}

// landScopeObject is what internal/lands/landscope Render builds for acme.
func landScopeObject() map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name": "gardenio-land-scope-acme",
			"labels": map[string]any{
				"gardenio.io/land": "acme", "gardenio.io/land-scope": "true", "app.kubernetes.io/managed-by": "gardenio-controller",
			},
		},
		"spec": map[string]any{
			"name":        "land-acme",
			"realmRef":    map[string]any{"name": "test-realm", "kind": "KeycloakRealm"},
			"protocol":    "openid-connect",
			"type":        "optional",
			"description": "Mints a token for the land acme alone (aud land-acme)",
			"attributes": map[string]any{
				"gardenio_land": "acme", "include.in.token.scope": "true", "display.on.consent.screen": "false",
			},
			"protocolMappers": []any{map[string]any{
				"name": "land-audience", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper",
				"config": map[string]any{
					"included.custom.audience": "land-acme", "access.token.claim": "true",
					"id.token.claim": "false", "introspection.token.claim": "true",
				},
			}},
		},
	}
}

// deepCopyJSON copies a JSON-shaped value so a case can change its object.
func deepCopyJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, e := range x {
			out[k] = deepCopyJSON(e)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = deepCopyJSON(e)
		}
		return out
	}
	return v
}

// The policy admits exactly the land scope shape from the controller, and
// refuses every way a scope could carry more than a land audience: a role or
// claim mapper, a second mapper, a client audience, another realm, a default
// scope, a foreign attribute or resource.
func TestLandScopes_AdmissionPolicyCEL(t *testing.T) {
	t.Parallel()
	var p vapPolicy
	if err := yaml.Unmarshal([]byte(landDoc(t, renderLandScopeRBAC(t), "ValidatingAdmissionPolicy", "test-agents-land-scopes")), &p); err != nil {
		t.Fatal(err)
	}
	if p.Spec.FailurePolicy != "Fail" {
		t.Errorf("failurePolicy = %q, want Fail", p.Spec.FailurePolicy)
	}
	const ctrl = "system:serviceaccount:gardenio:test-agents-sa"
	edit := func(f func(o map[string]any)) map[string]any {
		o := deepCopyJSON(landScopeObject()).(map[string]any)
		f(o)
		return o
	}
	spec := func(o map[string]any) map[string]any { return o["spec"].(map[string]any) }
	mapper := func(o map[string]any) map[string]any {
		return spec(o)["protocolMappers"].([]any)[0].(map[string]any)
	}
	mcfg := func(o map[string]any) map[string]any { return mapper(o)["config"].(map[string]any) }

	cases := []landVAPRequest{
		{name: "the land scope", user: ctrl, op: "CREATE", object: landScopeObject(), applied: true, wantAllowed: true},
		{name: "the land scope, updated", user: ctrl, op: "UPDATE", object: landScopeObject(), oldObject: landScopeObject(), applied: true, wantAllowed: true},
		{name: "no attributes", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { delete(spec(o), "attributes") }), applied: true, wantAllowed: true},
		{name: "another principal (helm)", user: "kubernetes-admin", op: "CREATE", object: edit(func(o map[string]any) { spec(o)["name"] = "github-identity" })},

		{name: "a non-land scope", user: ctrl, op: "UPDATE", object: edit(func(o map[string]any) { spec(o)["name"] = "github-identity" }), applied: true},
		{name: "bad land name", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { spec(o)["name"] = "land-Acme" }), applied: true},
		{name: "resource name for another land", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			o["metadata"].(map[string]any)["name"] = "gardenio-land-scope-beta"
		}), applied: true},
		{name: "unlabelled", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { delete(o["metadata"].(map[string]any), "labels") }), applied: true},
		{name: "a default scope", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { spec(o)["type"] = "default" }), applied: true},
		{name: "the deprecated default flag", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { spec(o)["default"] = true }), applied: true},
		{name: "saml", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { spec(o)["protocol"] = "saml" }), applied: true},
		{name: "another realm", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			spec(o)["realmRef"] = map[string]any{"name": "master", "kind": "ClusterKeycloakRealm"}
		}), applied: true},
		{name: "foreign attribute", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			spec(o)["attributes"].(map[string]any)["consent.screen.text"] = "x"
		}), applied: true},
		{name: "attribute names another land", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			spec(o)["attributes"].(map[string]any)["gardenio_land"] = "beta"
		}), applied: true},
		{name: "no mapper", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) { spec(o)["protocolMappers"] = []any{} }), applied: true},
		{name: "a second mapper", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			spec(o)["protocolMappers"] = append(spec(o)["protocolMappers"].([]any), deepCopyJSON(mapper(o)))
		}), applied: true},
		{name: "a hardcoded realm_roles claim", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			mapper(o)["protocolMapper"] = "oidc-hardcoded-claim-mapper"
			mapper(o)["config"] = map[string]any{"claim.name": "realm_roles", "claim.value": `["gardenio-admin"]`, "jsonType.label": "JSON", "access.token.claim": "true"}
		}), applied: true},
		{name: "a client audience", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			mcfg(o)["included.client.audience"] = "gardenio-traefik-forwardauth"
		}), applied: true},
		{name: "another land's audience", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			mcfg(o)["included.custom.audience"] = "land-beta"
		}), applied: true},
		{name: "audience mapper with no audience", user: ctrl, op: "CREATE", object: edit(func(o map[string]any) {
			delete(mcfg(o), "included.custom.audience")
		}), applied: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			applied, denied := evalLandVAP(t, p, c)
			if applied != c.applied {
				t.Fatalf("applied = %v, want %v", applied, c.applied)
			}
			if applied && (len(denied) == 0) != c.wantAllowed {
				t.Errorf("denied = %v, want allowed %v", denied, c.wantAllowed)
			}
		})
	}
}

func TestLandScopes_ControllerEnvNamesTheRealmResource(t *testing.T) {
	t.Parallel()
	_, d := renderControllerPodSpec(t, landsProvOn, "keycloak.enabled=true")
	env := map[string]string{}
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if got := env["LANDS_KEYCLOAK_REALM_RESOURCE"]; got != "test-realm" {
		t.Errorf("LANDS_KEYCLOAK_REALM_RESOURCE = %q, want test-realm (client-scopes.yaml's realmRef)", got)
	}
	if _, ok := env["LANDS_KEYCLOAK_LAND_SCOPE_CLIENTS"]; ok {
		t.Error("the controller still gets LANDS_KEYCLOAK_LAND_SCOPE_CLIENTS; it attaches nothing")
	}
	_, d = renderControllerPodSpec(t, landsProvOn, "keycloak.enabled=false")
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "LANDS_KEYCLOAK_REALM_RESOURCE" {
			t.Error("LANDS_KEYCLOAK_REALM_RESOURCE rendered without keycloak.enabled")
		}
	}
}
