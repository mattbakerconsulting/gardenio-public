package tests

import (
	"strings"
	"testing"
)

const oidcDeployment = "templates/grafana/deployment.yaml"

func oidcRender(t *testing.T, extra ...string) string {
	t.Helper()
	args := append([]string{
		"grafana.enabled=true",
		"grafana.persistence.enabled=true",
	}, extra...)
	return helmTemplate(t, oidcDeployment, args...)
}

// oidcMinimal is the smallest oidc block that passes the render gates.
func oidcMinimal(extra ...string) []string {
	return append([]string{
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=s",
		"grafana.auth.oidc.authURL=http://auth.example/auth",
		"grafana.auth.oidc.tokenURL=http://token.example/token",
		"grafana.auth.oidc.apiURL=http://api.example/userinfo",
	}, extra...)
}

// oidcEnvValue returns the rendered `value:` line for the named env var, or "" if
// the env var is absent.
func oidcEnvValue(out, name string) string {
	i := strings.Index(out, "- name: "+name+"\n")
	if i < 0 {
		return ""
	}
	rest := out[i:]
	lines := strings.SplitN(rest, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[1]), "value:"))
}

// Off by default: enabling grafana must not silently turn on an auth flow.
func TestGrafanaOIDC_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out := oidcRender(t)
	if strings.Contains(out, "GF_AUTH_GENERIC_OAUTH_") {
		t.Errorf("OIDC env must not render unless explicitly enabled:\n%s", out)
	}
}

func TestGrafanaOIDC_RendersEnv(t *testing.T) {
	t.Parallel()
	out := oidcRender(t,
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=gardenio-grafana-oidc",
		"grafana.auth.oidc.authURL=http://gardenio.keycloak.ziti:8080/auth/realms/gardenio/protocol/openid-connect/auth",
		"grafana.auth.oidc.tokenURL=http://gardenio-keycloak-runtime:8080/auth/realms/gardenio/protocol/openid-connect/token",
		"grafana.auth.oidc.apiURL=http://gardenio-keycloak-runtime:8080/auth/realms/gardenio/protocol/openid-connect/userinfo",
		"grafana.rootURL=http://gardenio.grafana.ziti:3000",
	)
	for _, want := range []string{
		"GF_AUTH_GENERIC_OAUTH_ENABLED",
		"GF_AUTH_GENERIC_OAUTH_CLIENT_ID",
		"GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET",
		"GF_AUTH_GENERIC_OAUTH_AUTH_URL",
		"GF_AUTH_GENERIC_OAUTH_TOKEN_URL",
		"GF_AUTH_GENERIC_OAUTH_API_URL",
		"GF_SERVER_ROOT_URL",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in:\n%s", want, out)
		}
	}
	// The browser follows auth_url, so it must be the ziti host, while the
	// back-channel token/userinfo calls stay in-cluster (spec G4, §3).
	if got := oidcEnvValue(out, "GF_AUTH_GENERIC_OAUTH_AUTH_URL"); !strings.Contains(got, "gardenio.keycloak.ziti") {
		t.Errorf("auth_url must be the browser-reachable ziti hostname, got %q", got)
	}
	for _, name := range []string{"GF_AUTH_GENERIC_OAUTH_TOKEN_URL", "GF_AUTH_GENERIC_OAUTH_API_URL"} {
		if got := oidcEnvValue(out, name); !strings.Contains(got, "gardenio-keycloak-runtime") {
			t.Errorf("%s must stay on the in-cluster Service, got %q", name, got)
		}
	}
	if got := oidcEnvValue(out, "GF_SERVER_ROOT_URL"); !strings.Contains(got, "gardenio.grafana.ziti:3000") {
		t.Errorf("GF_SERVER_ROOT_URL must come from grafana.rootURL, got %q", got)
	}
}

// Every new values key must change the render, so a typo'd key (which the
// schema does not catch) fails here instead of being silently ignored.
func TestGrafanaOIDC_EveryKeyIsWired(t *testing.T) {
	t.Parallel()
	out := oidcRender(t,
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.name=KC-Test",
		"grafana.auth.oidc.clientId=cid-test",
		"grafana.auth.oidc.clientSecretSecret=sec-test",
		"grafana.auth.oidc.clientSecretKey=key-test",
		"grafana.auth.oidc.scopes=openid groups-test",
		"grafana.auth.oidc.authURL=http://auth-test/a",
		"grafana.auth.oidc.tokenURL=http://token-test/t",
		"grafana.auth.oidc.apiURL=http://api-test/u",
		"grafana.auth.oidc.roleAttributePath=role-path-test",
		"grafana.auth.oidc.roleAttributeStrict=true",
		"grafana.auth.oidc.allowAssignGrafanaAdmin=true",
		"grafana.auth.oidc.allowSignUp=true",
		"grafana.auth.oidc.usePKCE=false",
		"grafana.auth.oidc.autoLogin=true",
	)
	for env, want := range map[string]string{
		"GF_AUTH_GENERIC_OAUTH_NAME":                       "KC-Test",
		"GF_AUTH_GENERIC_OAUTH_CLIENT_ID":                  "cid-test",
		"GF_AUTH_GENERIC_OAUTH_SCOPES":                     "openid groups-test",
		"GF_AUTH_GENERIC_OAUTH_ROLE_ATTRIBUTE_PATH":        "role-path-test",
		"GF_AUTH_GENERIC_OAUTH_AUTH_URL":                   "http://auth-test/a",
		"GF_AUTH_GENERIC_OAUTH_TOKEN_URL":                  "http://token-test/t",
		"GF_AUTH_GENERIC_OAUTH_API_URL":                    "http://api-test/u",
		"GF_AUTH_GENERIC_OAUTH_ROLE_ATTRIBUTE_STRICT":      "true",
		"GF_AUTH_GENERIC_OAUTH_ALLOW_ASSIGN_GRAFANA_ADMIN": "true",
		"GF_AUTH_GENERIC_OAUTH_ALLOW_SIGN_UP":              "true",
		"GF_AUTH_GENERIC_OAUTH_USE_PKCE":                   "false",
		"GF_AUTH_GENERIC_OAUTH_AUTO_LOGIN":                 "true",
	} {
		if got := oidcEnvValue(out, env); !strings.Contains(got, want) {
			t.Errorf("%s = %q, want it to contain %q", env, got, want)
		}
	}
	i := strings.Index(out, "GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET")
	if i < 0 {
		t.Fatal("client secret env not rendered")
	}
	blk := out[i:min(i+250, len(out))]
	if !strings.Contains(blk, "sec-test") || !strings.Contains(blk, "key-test") {
		t.Errorf("secretKeyRef must use clientSecretSecret/clientSecretKey:\n%s", blk)
	}
}

// Secure defaults (task gd-1xgYeguTI5): no auto sign-up, Keycloak role mapping
// (gardenio-admin -> Admin, else Viewer), explicit PKCE, no server-admin grant.
// The client secret key defaults to 'client-secret'.
func TestGrafanaOIDC_Defaults(t *testing.T) {
	t.Parallel()
	out := oidcRender(t, oidcMinimal()...)
	i := strings.Index(out, "GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET")
	if i < 0 || !strings.Contains(out[i:min(i+250, len(out))], "key: client-secret") {
		t.Errorf("clientSecretKey must default to client-secret:\n%s", out)
	}
	rolePath := oidcEnvValue(out, "GF_AUTH_GENERIC_OAUTH_ROLE_ATTRIBUTE_PATH")
	for _, want := range []string{"realm_access.roles", "gardenio-admin", "'Admin'", "'Viewer'"} {
		if !strings.Contains(rolePath, want) {
			t.Errorf("default role_attribute_path must map realm role gardenio-admin->Admin else Viewer; missing %q in %s", want, rolePath)
		}
	}
	for env, want := range map[string]string{
		"GF_AUTH_GENERIC_OAUTH_ALLOW_SIGN_UP":              `"false"`,
		"GF_AUTH_GENERIC_OAUTH_USE_PKCE":                   `"true"`,
		"GF_AUTH_GENERIC_OAUTH_ROLE_ATTRIBUTE_STRICT":      `"false"`,
		"GF_AUTH_GENERIC_OAUTH_ALLOW_ASSIGN_GRAFANA_ADMIN": `"false"`,
	} {
		if got := oidcEnvValue(out, env); got != want {
			t.Errorf("%s default = %q, want %s", env, got, want)
		}
	}
	if got := oidcEnvValue(out, "GF_AUTH_GENERIC_OAUTH_SCOPES"); !strings.Contains(got, "openid profile email") {
		t.Errorf("default scopes wrong: %q", got)
	}
	if got := oidcEnvValue(out, "GF_AUTH_GENERIC_OAUTH_AUTO_LOGIN"); !strings.Contains(got, "false") {
		t.Errorf("autoLogin must default to false: %q", got)
	}
}

// The client secret is a credential: secretKeyRef only, never an inline value.
func TestGrafanaOIDC_ClientSecretFromSecretRef(t *testing.T) {
	t.Parallel()
	out := oidcRender(t, oidcMinimal("grafana.auth.oidc.clientSecretSecret=gardenio-grafana-oidc")...)
	i := strings.Index(out, "GF_AUTH_GENERIC_OAUTH_CLIENT_SECRET")
	if i < 0 {
		t.Fatal("client secret env not rendered")
	}
	if !strings.Contains(out[i:min(i+250, len(out))], "secretKeyRef") {
		t.Errorf("client secret must come from secretKeyRef:\n%s", out[i:])
	}
}

// A misconfigured OIDC block must fail the RENDER, not boot a Grafana that
// silently falls back to local login. values.schema.json validates none of
// this (spec §2 item 8), so the template gate is the only guard.
func TestGrafanaOIDC_RequiresClientIdAndSecret(t *testing.T) {
	t.Parallel()
	for name, extra := range map[string][]string{
		"neither":      nil,
		"no secret":    {"grafana.auth.oidc.clientId=grafana"},
		"no client id": {"grafana.auth.oidc.clientSecretSecret=s"},
	} {
		args := append([]string{
			"grafana.enabled=true",
			"grafana.persistence.enabled=true",
			"grafana.auth.oidc.enabled=true",
		}, extra...)
		if _, err := helmTemplateAllowFail(t, oidcDeployment, args...); err == nil {
			t.Errorf("%s: oidc.enabled without clientId+clientSecretSecret must fail the render", name)
		}
	}
}

// Two auth mechanisms at once is a misconfiguration, not a feature.
func TestGrafanaOIDC_MutuallyExclusiveWithAuthProxy(t *testing.T) {
	t.Parallel()
	_, err := helmTemplateAllowFail(t, oidcDeployment,
		"grafana.enabled=true",
		"grafana.persistence.enabled=true",
		"grafana.auth.oidc.enabled=true",
		"grafana.auth.oidc.clientId=grafana",
		"grafana.auth.oidc.clientSecretSecret=s",
		"grafana.auth.oidc.authURL=a", "grafana.auth.oidc.tokenURL=t", "grafana.auth.oidc.apiURL=u",
		"grafana.auth.proxy.enabled=true",
	)
	if err == nil {
		t.Error("oidc and the header auth-proxy must not both be enabled")
	}
}

// The local admin account is BREAK-GLASS. The login form must therefore still
// be reachable by default even with OIDC on, and hiding it must be a
// deliberate, tested choice rather than an accident of enabling OIDC.
func TestGrafanaOIDC_LoginFormStaysByDefault(t *testing.T) {
	t.Parallel()
	out := oidcRender(t, oidcMinimal()...)
	if got := oidcEnvValue(out, "GF_AUTH_DISABLE_LOGIN_FORM"); got != `"false"` {
		t.Errorf("OIDC on must keep the break-glass login form (GF_AUTH_DISABLE_LOGIN_FORM=\"false\"), got %q", got)
	}
}

func TestGrafanaOIDC_LoginFormCanBeDisabled(t *testing.T) {
	t.Parallel()
	out := oidcRender(t, oidcMinimal("grafana.auth.disableLoginForm=true")...)
	if got := oidcEnvValue(out, "GF_AUTH_DISABLE_LOGIN_FORM"); got != `"true"` {
		t.Errorf("disableLoginForm=true must render GF_AUTH_DISABLE_LOGIN_FORM=\"true\", got %q", got)
	}
}

// An empty authURL/tokenURL/apiURL boots a Grafana with a broken login redirect
// or back-channel; each must fail the render on its own (spec G3).
func TestGrafanaOIDC_RequiresURLs(t *testing.T) {
	t.Parallel()
	urls := map[string]string{
		"authURL":  "grafana.auth.oidc.authURL=http://auth.example/auth",
		"tokenURL": "grafana.auth.oidc.tokenURL=http://token.example/token",
		"apiURL":   "grafana.auth.oidc.apiURL=http://api.example/userinfo",
	}
	for missing := range urls {
		args := []string{
			"grafana.enabled=true",
			"grafana.persistence.enabled=true",
			"grafana.auth.oidc.enabled=true",
			"grafana.auth.oidc.clientId=grafana",
			"grafana.auth.oidc.clientSecretSecret=s",
		}
		for k, v := range urls {
			if k != missing {
				args = append(args, v)
			}
		}
		out, err := helmTemplateAllowFail(t, oidcDeployment, args...)
		if err == nil {
			t.Errorf("oidc.enabled without %s must fail the render", missing)
		} else if !strings.Contains(out, "oidc."+missing) {
			t.Errorf("failure for missing %s should name it, got: %s", missing, out)
		}
	}
	// The gate must not fire when OIDC is off (URLs are empty by default).
	if _, err := helmTemplateAllowFail(t, oidcDeployment,
		"grafana.enabled=true", "grafana.persistence.enabled=true"); err != nil {
		t.Errorf("empty OIDC URLs must be fine while oidc is disabled: %v", err)
	}
}
