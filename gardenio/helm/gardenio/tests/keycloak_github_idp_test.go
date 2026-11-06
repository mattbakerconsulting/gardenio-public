package tests

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Lands Track B 7.3 (gd-0o4Ggf9ngP): keycloak.githubIdp renders the GitHub IdP
// without trustEmail, its own first broker login flow that never links an
// existing user, a post broker login flow that refuses the admin role, the
// github_id / github_login IdP mappers, the github-identity
// client scope that puts both in the access token on every client, and
// admin-only user-profile attributes. Off by default.

const githubIdpK3s = "../../../deploy-values/gardenio-k3s.yaml"

func githubIdpOn(extra ...string) []string {
	return append([]string{
		"keycloak.githubIdp.enabled=true",
		"keycloak.githubIdp.clientId=Iv1.test",
	}, extra...)
}

// renderedObjects parses a multi-doc helm template output.
func renderedObjects(t *testing.T, out string) []map[string]any {
	t.Helper()
	var objs []map[string]any
	for doc := range strings.SplitSeq(out, "\n---") {
		var o map[string]any
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("parse rendered doc: %v\n%s", err, doc)
		}
		if o != nil {
			objs = append(objs, o)
		}
	}
	return objs
}

func objectsOfKind(objs []map[string]any, kind string) []map[string]any {
	var out []map[string]any
	for _, o := range objs {
		if o["kind"] == kind {
			out = append(out, o)
		}
	}
	return out
}

func specOf(o map[string]any) map[string]any {
	s, _ := o["spec"].(map[string]any)
	return s
}

func listOf(v any) []any {
	l, _ := v.([]any)
	return l
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func githubIdpSpec(t *testing.T, objs []map[string]any) map[string]any {
	t.Helper()
	for _, o := range objectsOfKind(objs, "KeycloakRealmIdentityProvider") {
		if s := specOf(o); s["alias"] == "github" {
			return s
		}
	}
	t.Fatal("no KeycloakRealmIdentityProvider with alias github rendered")
	return nil
}

// The chart default is off; gardenio-k3s turns it on, so its case switches it
// off again and checks that off renders nothing over those values.
func TestGitHubIdp_OffByDefault(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		files []string
		sets  []string
	}{
		"base": {nil, []string{"keycloak.enabled=true"}},
		"k3s":  {[]string{githubIdpK3s}, []string{"keycloak.enabled=true", "keycloak.githubIdp.enabled=false"}},
	} {
		t.Run(name, func(t *testing.T) {
			out := helmTemplateFull(t, tc.files, tc.sets...)
			for _, forbidden := range []string{`alias: "github"`, "github-identity", "userProfileConfig", "github_id", "github_login", "gardenio-github-first-broker-login", "gardenio-github-post-broker"} {
				if strings.Contains(out, forbidden) {
					t.Errorf("githubIdp is off by default, but the render contains %q", forbidden)
				}
			}
		})
	}
}

func TestGitHubIdp_RendersTheProvider(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...))
	idp := githubIdpSpec(t, objs)

	for field, want := range map[string]any{
		"providerId":                "github",
		"enabled":                   true,
		"trustEmail":                false,
		"firstBrokerLoginFlowAlias": "gardenio-github-first-broker-login",
		"postBrokerLoginFlowAlias":  "gardenio-github-post-broker-login",
	} {
		if idp[field] != want {
			t.Errorf("spec.%s = %v, want %v", field, idp[field], want)
		}
	}
	cfg := mapOf(idp["config"])
	for key, want := range map[string]string{
		"clientId":     "Iv1.test",
		"clientSecret": "$gardenio-github-idp:client-secret",
		"syncMode":     "IMPORT",
		"defaultScope": "read:user user:email",
	} {
		if cfg[key] != want {
			t.Errorf("spec.config.%s = %v, want %q", key, cfg[key], want)
		}
	}

	mapped := map[string]map[string]any{}
	for _, m := range listOf(idp["mappers"]) {
		mm := mapOf(m)
		if mm["identityProviderMapper"] != "github-user-attribute-mapper" || mm["identityProviderAlias"] != "github" {
			t.Errorf("mapper %v: want a github-user-attribute-mapper on alias github", mm["name"])
		}
		c := mapOf(mm["config"])
		mapped[c["userAttribute"].(string)] = c
	}
	for attr, field := range map[string]string{"github_id": "id", "github_login": "login"} {
		c, ok := mapped[attr]
		if !ok {
			t.Errorf("no IdP mapper fills user attribute %s", attr)
			continue
		}
		if c["jsonField"] != field {
			t.Errorf("%s mapper reads GitHub field %v, want %q", attr, c["jsonField"], field)
		}
	}
	// The login can be renamed on GitHub, so it is re-synced. The id is too:
	// under the IdP's IMPORT an INHERIT mapper runs only when a sign-in creates
	// the user, so a user who linked GitHub from the account console would
	// never get github_id.
	for _, attr := range []string{"github_id", "github_login"} {
		if mapped[attr]["syncMode"] != "FORCE" {
			t.Errorf("%s mapper syncMode = %v, want FORCE", attr, mapped[attr]["syncMode"])
		}
	}
}

// Keycloak's github provider never reads GitHub's `verified` flag, so no IdP in
// the live stack trusts email: an unverified address must not become
// email_verified=true.
func TestGitHubIdp_NoIdpTrustsEmail(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...))
	for _, o := range objectsOfKind(objs, "KeycloakRealmIdentityProvider") {
		s := specOf(o)
		if s["trustEmail"] != false && s["trustEmail"] != nil {
			t.Errorf("IdP %v has trustEmail %v; no IdP in the live stack may trust email", s["alias"], s["trustEmail"])
		}
	}
}

// The github IdP's first broker login flow creates a new user or stops: no
// review form can relink, no "verify existing account by email" or re-auth
// step can hand an existing user to a GitHub account.
func TestGitHubIdp_FirstBrokerLoginNeverLinks(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...))
	var flow map[string]any
	for _, o := range objectsOfKind(objs, "KeycloakAuthFlow") {
		if s := specOf(o); s["alias"] == githubIdpSpec(t, objs)["firstBrokerLoginFlowAlias"] {
			flow = s
		}
	}
	if flow == nil {
		t.Fatal("no KeycloakAuthFlow renders the github IdP's firstBrokerLoginFlowAlias")
	}
	if flow["topLevel"] != true || flow["providerId"] != "basic-flow" {
		t.Errorf("flow topLevel/providerId = %v/%v, want true/basic-flow", flow["topLevel"], flow["providerId"])
	}
	realmName := mapOf(objectsOfKind(objs, "KeycloakRealm")[0]["metadata"])["name"]
	if ref := mapOf(flow["realmRef"]); ref["kind"] != "KeycloakRealm" || ref["name"] != realmName {
		t.Errorf("flow realmRef = %v, want the chart's KeycloakRealm %v", ref, realmName)
	}

	// Exactly these executions, all REQUIRED, and no subflow: a subflow is
	// where the stock flow hides its linking branch.
	want := []string{"idp-review-profile", "idp-create-user-if-unique"}
	execs := listOf(flow["authenticationExecutions"])
	if len(execs) != len(want) {
		t.Fatalf("flow has %d executions, want %v: %v", len(execs), want, execs)
	}
	for i, e := range execs {
		em := mapOf(e)
		if em["authenticator"] != want[i] || em["requirement"] != "REQUIRED" {
			t.Errorf("execution %d = %v %v, want %s REQUIRED", i, em["authenticator"], em["requirement"], want[i])
		}
		if em["authenticatorFlow"] == true {
			t.Errorf("execution %d is a subflow; the flow must have none", i)
		}
	}
	for _, o := range objectsOfKind(objs, "KeycloakAuthFlow") {
		if specOf(o)["parentName"] == flow["alias"] {
			t.Errorf("KeycloakAuthFlow %v is a subflow of the github first broker login flow", specOf(o)["alias"])
		}
	}
	if c := mapOf(mapOf(mapOf(execs[0])["authenticatorConfig"])["config"]); c["update.profile.on.first.login"] != "missing" {
		t.Errorf("idp-review-profile update.profile.on.first.login = %v, want missing", c["update.profile.on.first.login"])
	}

	// No linking authenticator anywhere in the render.
	out := helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...)
	for _, linker := range []string{"idp-confirm-link", "idp-email-verification", "idp-username-password-form", "idp-auto-link", "idp-detect-existing-broker-user"} {
		if strings.Contains(out, linker) {
			t.Errorf("render contains account-linking authenticator %s", linker)
		}
	}
}

func authFlowByAlias(objs []map[string]any, alias any) map[string]any {
	for _, o := range objectsOfKind(objs, "KeycloakAuthFlow") {
		if s := specOf(o); s["alias"] == alias {
			return s
		}
	}
	return nil
}

// The admin passkey is asked only inside gardenio-mfa-browser's
// username/password subflow, which a GitHub sign-in never runs. The github IdP's
// post broker login flow lets everyone else in and refuses the MFA admin role.
func TestGitHubIdp_PostBrokerLoginRefusesAdmins(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		extra []string
		role  string
	}{
		"default admin role":    {nil, "gardenio-admin"},
		"mfa.adminRole renamed": {[]string{"keycloak.mfa.adminRole=realm-boss"}, "realm-boss"},
	} {
		t.Run(name, func(t *testing.T) {
			objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn(tc.extra...)...))
			alias := githubIdpSpec(t, objs)["postBrokerLoginFlowAlias"]
			flow := authFlowByAlias(objs, alias)
			if flow == nil {
				t.Fatalf("no KeycloakAuthFlow renders the github IdP's postBrokerLoginFlowAlias %v", alias)
			}
			if flow["topLevel"] != true || flow["providerId"] != "basic-flow" {
				t.Errorf("flow topLevel/providerId = %v/%v, want true/basic-flow", flow["topLevel"], flow["providerId"])
			}
			realmName := mapOf(objectsOfKind(objs, "KeycloakRealm")[0]["metadata"])["name"]
			if ref := mapOf(flow["realmRef"]); ref["kind"] != "KeycloakRealm" || ref["name"] != realmName {
				t.Errorf("flow realmRef = %v, want the chart's KeycloakRealm %v", ref, realmName)
			}

			// A flow whose only step is a skipped conditional subflow never
			// succeeds in Keycloak, so a REQUIRED allow-access is what lets a
			// non-admin through.
			execs := listOf(flow["authenticationExecutions"])
			if len(execs) != 1 || mapOf(execs[0])["authenticator"] != "allow-access-authenticator" || mapOf(execs[0])["requirement"] != "REQUIRED" {
				t.Errorf("post broker flow executions = %v, want one REQUIRED allow-access-authenticator", execs)
			}

			var subs []map[string]any
			for _, o := range objectsOfKind(objs, "KeycloakAuthFlow") {
				if s := specOf(o); s["parentName"] == alias {
					subs = append(subs, s)
				}
			}
			if len(subs) != 1 {
				t.Fatalf("want 1 subflow of %v, got %d", alias, len(subs))
			}
			sub := subs[0]
			if sub["topLevel"] != false || sub["childType"] != "basic-flow" || sub["childRequirement"] != "CONDITIONAL" {
				t.Errorf("subflow topLevel/childType/childRequirement = %v/%v/%v, want false/basic-flow/CONDITIONAL",
					sub["topLevel"], sub["childType"], sub["childRequirement"])
			}
			if ref := mapOf(sub["realmRef"]); ref["kind"] != "KeycloakRealm" || ref["name"] != realmName {
				t.Errorf("subflow realmRef = %v, want the chart's KeycloakRealm %v", ref, realmName)
			}
			sexecs := listOf(sub["authenticationExecutions"])
			if len(sexecs) != 2 {
				t.Fatalf("subflow has %d executions, want conditional-user-role then deny-access-authenticator: %v", len(sexecs), sexecs)
			}
			cond, deny := mapOf(sexecs[0]), mapOf(sexecs[1])
			if cond["authenticator"] != "conditional-user-role" || cond["requirement"] != "REQUIRED" {
				t.Errorf("subflow execution 0 = %v %v, want conditional-user-role REQUIRED", cond["authenticator"], cond["requirement"])
			}
			if c := mapOf(mapOf(cond["authenticatorConfig"])["config"]); c["condUserRole"] != tc.role || c["negate"] != "false" {
				t.Errorf("conditional-user-role config = %v, want condUserRole %s, negate false", c, tc.role)
			}
			if deny["authenticator"] != "deny-access-authenticator" || deny["requirement"] != "REQUIRED" {
				t.Errorf("subflow execution 1 = %v %v, want deny-access-authenticator REQUIRED", deny["authenticator"], deny["requirement"])
			}
			if p0, p1 := cond["priority"].(float64), deny["priority"].(float64); p0 >= p1 {
				t.Errorf("conditional-user-role priority %v must run before deny-access-authenticator %v", p0, p1)
			}
		})
	}
}

// The MFA flow guards the same role the post broker flow refuses: the k3s
// overlay turns MFA on, and both resolve keycloak.mfa.adminRole.
func TestGitHubIdp_PostBrokerRoleMatchesMFARole(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...))
	roleOf := func(alias string) any {
		f := authFlowByAlias(objs, alias)
		if f == nil {
			t.Fatalf("no KeycloakAuthFlow %s rendered", alias)
		}
		for _, e := range listOf(f["authenticationExecutions"]) {
			if em := mapOf(e); em["authenticator"] == "conditional-user-role" {
				return mapOf(mapOf(em["authenticatorConfig"])["config"])["condUserRole"]
			}
		}
		t.Fatalf("flow %s has no conditional-user-role", alias)
		return nil
	}
	if mfa, gh := roleOf("gardenio-mfa-browser-conditional-mfa"), roleOf("gardenio-github-post-broker-admin"); mfa != gh {
		t.Errorf("MFA guards role %v but the GitHub post broker flow refuses %v", mfa, gh)
	}
}

func TestGitHubIdp_ClaimsReachEveryClientsAccessToken(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...))

	var scope map[string]any
	for _, o := range objectsOfKind(objs, "KeycloakClientScope") {
		if s := specOf(o); s["name"] == "github-identity" {
			scope = s
		}
	}
	if scope == nil {
		t.Fatal("no github-identity KeycloakClientScope rendered")
	}
	if scope["type"] != "default" || scope["protocol"] != "openid-connect" {
		t.Errorf("github-identity scope type/protocol = %v/%v, want default/openid-connect", scope["type"], scope["protocol"])
	}
	claims := map[string]map[string]any{}
	for _, pm := range listOf(scope["protocolMappers"]) {
		m := mapOf(pm)
		if m["protocolMapper"] != "oidc-usermodel-attribute-mapper" {
			t.Errorf("protocol mapper %v is %v, want oidc-usermodel-attribute-mapper", m["name"], m["protocolMapper"])
		}
		c := mapOf(m["config"])
		claims[c["claim.name"].(string)] = c
	}
	for claim, jsonType := range map[string]string{"github_id": "long", "github_login": "String"} {
		c, ok := claims[claim]
		if !ok {
			t.Errorf("no protocol mapper emits claim %s", claim)
			continue
		}
		if c["user.attribute"] != claim || c["access.token.claim"] != "true" || c["jsonType.label"] != jsonType {
			t.Errorf("claim %s mapper config = %v; want user.attribute %s, access.token.claim true, jsonType.label %s", claim, c, claim, jsonType)
		}
	}

	clients := objectsOfKind(objs, "KeycloakClient")
	if len(clients) == 0 {
		t.Fatal("the k3s overlay renders no KeycloakClient")
	}
	for _, o := range clients {
		s := specOf(o)
		found := false
		for _, sc := range listOf(s["defaultClientScopes"]) {
			found = found || sc == "github-identity"
		}
		if !found {
			t.Errorf("client %v does not carry the github-identity default scope", s["clientId"])
		}
	}
}

// Users can never set their own github_id: both attributes are declared in the
// realm user profile and only admins may edit or view them.
func TestGitHubIdp_UserProfileAttributesAreAdminOnly(t *testing.T) {
	t.Parallel()
	objs := renderedObjects(t, helmTemplateFull(t, []string{githubIdpK3s}, githubIdpOn()...))
	realms := objectsOfKind(objs, "KeycloakRealm")
	if len(realms) != 1 {
		t.Fatalf("want 1 KeycloakRealm, got %d", len(realms))
	}
	up := mapOf(specOf(realms[0])["userProfileConfig"])
	if _, set := up["unmanagedAttributePolicy"]; set {
		t.Errorf("userProfileConfig must not change unmanagedAttributePolicy, got %v", up["unmanagedAttributePolicy"])
	}
	declared := map[string]bool{}
	for _, a := range listOf(up["attributes"]) {
		am := mapOf(a)
		name, _ := am["name"].(string)
		perms := mapOf(am["permissions"])
		for _, op := range []string{"edit", "view"} {
			got := listOf(perms[op])
			if len(got) != 1 || got[0] != "admin" {
				t.Errorf("attribute %s %s permission = %v, want [admin]", name, op, got)
			}
		}
		declared[name] = true
	}
	for _, want := range []string{"github_id", "github_login"} {
		if !declared[want] {
			t.Errorf("user profile does not declare %s", want)
		}
	}
}

func TestGitHubIdp_RefusesUnsafeOrIncompleteValues(t *testing.T) {
	t.Parallel()
	k3s := []string{"-f", chartDir(t) + "/" + githubIdpK3s}
	set := func(flags ...string) []string {
		args := append([]string{}, k3s...)
		for _, f := range flags {
			args = append(args, "--set", f)
		}
		return args
	}
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no client id": {
			set("keycloak.githubIdp.enabled=true", "keycloak.githubIdp.clientId="),
			"keycloak.githubIdp.clientId is required",
		},
		// Base defaults put every new user in gardenio-viewers: with public
		// GitHub registration that is read access for anyone.
		"realm default group": {
			[]string{"--set", "keycloak.enabled=true", "--set", "keycloak.githubIdp.enabled=true", "--set", "keycloak.githubIdp.clientId=Iv1.test"},
			"every registrant would join it",
		},
		// The admin-only user profile and the flow live on the chart's realm.
		"realm not created by the chart": {
			set(githubIdpOn("keycloak.realm.create=false")...),
			"needs keycloak.realm.create",
		},
		"github in identityProviders too": {
			set(githubIdpOn("keycloak.identityProviders[0].alias=github", "keycloak.identityProviders[0].providerId=github")...),
			"render the same IdP",
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := helmTemplateExpectError(t, "", tc.args...)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("error does not mention %q:\n%s", tc.want, out)
			}
		})
	}
}
