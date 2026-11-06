package tests

import (
	"strings"
	"testing"
)

// GitLab-SSO / login-picker chart wiring verification (T3 — ka-0000002649,
// epic ka-0000002083 "beads-viewer → public internet").
//
// Context / why this test exists
// ------------------------------
// T3 was written (April 2026) as a *live* GitLab-SSO click-through on the
// RWX-era staging instance: visit the Keycloak account page, click the
// "GitLab" button, complete the OAuth round-trip, land authenticated on the
// viewer, and confirm the picker flips when the provider flags flip.
//
// The live edge has since moved on: beads-viewer's public edge is now gated by
// AWS Verified Access (templates/beads-viewer/ava-ingressroute.yaml *omits*
// Keycloak forwardauth on purpose — see ka-0000005862), the Keycloak realm
// moved off auth.e2e.dev.acme.example, and `beadsViewer.loginPicker.enabled`
// defaults false in every deploy — so a literal button click cannot be driven
// end-to-end today. What remains verifiable, durable, and (until now)
// *untested at the chart layer* is the seam that actually decides whether the
// GitLab SSO path is reachable: the chart → Deployment env → picker-handler
// contract.
//
// The login-picker handler itself (buildLoginRedirect, handleLogin, the
// kc_idp_hint=gitlab query param, the hide-when-disabled branches) is unit
// tested in gardenio/controller/cmd/beads-viewer/login_test.go. What those Go
// tests cannot see is whether the Helm chart hands the handler the right env
// under each flag combination. This file closes that gap: it renders the
// beads-viewer Deployment across the T3 acceptance matrix and asserts the
// LOGIN_PICKER_* env the binary reads (main.go) is emitted correctly, so the
// GitLab button appears / disappears exactly when an operator flips
// beadsViewer.loginPicker.gitlab.enabled — which is T3 step 4 ("verify the
// picker flips correctly when a provider flag flips"), verified as a
// deterministic chart contract rather than a one-off manual click.

// baseViewerPickerFlags returns the minimal --set flags to render the
// beads-viewer Deployment with the picker enabled. Callers append the
// per-scenario provider toggles.
func baseViewerPickerFlags(extra ...string) []string {
	base := []string{
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.loginPicker.enabled=true",
		"beadsViewer.loginPicker.forwardAuthURL=https://gardenio.example.com",
	}
	return append(base, extra...)
}

// envValueAfter finds `- name: <key>` in a rendered Deployment and returns the
// quoted value on the following `value:` line. Returns ("", false) when the
// env var is absent. Mirrors how the binary reads a discrete env var, so a
// test failure points straight at the operator-facing symptom.
func envValueAfter(rendered, key string) (string, bool) {
	lines := strings.Split(rendered, "\n")
	needle := "- name: " + key
	for i, ln := range lines {
		if strings.TrimSpace(ln) == needle {
			for j := i + 1; j < len(lines) && j < i+3; j++ {
				t := strings.TrimSpace(lines[j])
				if after, ok := strings.CutPrefix(t, "value:"); ok {
					v := strings.Trim(strings.TrimSpace(after), `"`)
					return v, true
				}
			}
			return "", true // env present but no scalar value line
		}
	}
	return "", false
}

// T3 step 4 (both providers on): picker enabled with GitLab + Keycloak-local
// both enabled must emit the full LOGIN_PICKER_* env block the handler reads,
// including the GitLab button toggle set to "true". This is the "GitLab SSO
// path is reachable from the picker" contract.
func TestBeadsViewerPicker_BothProviders_EmitsFullEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml", baseViewerPickerFlags()...)

	want := map[string]string{
		"LOGIN_PICKER_ENABLED":                "true",
		"LOGIN_PICKER_FORWARDAUTH_URL":        "https://gardenio.example.com",
		"LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED": "true",
		"LOGIN_PICKER_KEYCLOAK_LOCAL_LABEL":   "Keycloak (local)",
		"LOGIN_PICKER_GITLAB_ENABLED":         "true",
		"LOGIN_PICKER_GITLAB_LABEL":           "GitLab",
	}
	for k, wantV := range want {
		gotV, present := envValueAfter(out, k)
		if !present {
			t.Errorf("picker-on: env %s must be rendered, but it is absent:\n%s", k, out)
			continue
		}
		if gotV != wantV {
			t.Errorf("picker-on: env %s = %q, want %q", k, gotV, wantV)
		}
	}
}

// T3 step 4 (flip GitLab off): flipping beadsViewer.loginPicker.gitlab.enabled
// to false must render LOGIN_PICKER_GITLAB_ENABLED=false while leaving the
// Keycloak-local button enabled. The binary reads that env as
// gitlabEnabled=false and hides the GitLab button (login_test.go
// TestHandleLogin_OnlyKeycloakLocal), so this asserts the chart end of the
// "disable GitLab" flag path.
func TestBeadsViewerPicker_GitLabDisabled_FlipsEnvOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		baseViewerPickerFlags("beadsViewer.loginPicker.gitlab.enabled=false")...)

	if v, ok := envValueAfter(out, "LOGIN_PICKER_GITLAB_ENABLED"); !ok || v != "false" {
		t.Errorf("gitlab-disabled: LOGIN_PICKER_GITLAB_ENABLED = %q (present=%v), want \"false\"", v, ok)
	}
	// Keycloak-local button stays reachable — only GitLab was flipped off.
	if v, ok := envValueAfter(out, "LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED"); !ok || v != "true" {
		t.Errorf("gitlab-disabled: LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED = %q (present=%v), want \"true\"", v, ok)
	}
}

// T3 step 4 (flip Keycloak-local off): the mirror case — disabling the local
// button must leave the GitLab SSO button enabled. Together with the previous
// test this proves each provider flips independently ("one at a time", per the
// T3 acceptance list).
func TestBeadsViewerPicker_KeycloakLocalDisabled_FlipsEnvOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		baseViewerPickerFlags("beadsViewer.loginPicker.keycloakLocal.enabled=false")...)

	if v, ok := envValueAfter(out, "LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED"); !ok || v != "false" {
		t.Errorf("keycloak-local-disabled: LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED = %q (present=%v), want \"false\"", v, ok)
	}
	if v, ok := envValueAfter(out, "LOGIN_PICKER_GITLAB_ENABLED"); !ok || v != "true" {
		t.Errorf("keycloak-local-disabled: LOGIN_PICKER_GITLAB_ENABLED = %q (present=%v), want \"true\"", v, ok)
	}
}

// Picker disabled (the shipped default): the whole LOGIN_PICKER_* env block
// must be suppressed. main.go reads LOGIN_PICKER_ENABLED != "true" and takes
// the straight-passthrough branch (302 to oauth2-proxy with no kc_idp_hint),
// preserving pre-picker behavior. Asserting the env is *absent* (not merely
// "false") locks the chart's `{{- if .Values.beadsViewer.loginPicker.enabled }}`
// guard.
func TestBeadsViewerPicker_DisabledByDefault_SuppressesEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
	)
	for _, k := range []string{
		"LOGIN_PICKER_ENABLED",
		"LOGIN_PICKER_FORWARDAUTH_URL",
		"LOGIN_PICKER_KEYCLOAK_LOCAL_ENABLED",
		"LOGIN_PICKER_GITLAB_ENABLED",
	} {
		if _, present := envValueAfter(out, k); present {
			t.Errorf("picker default-off: env %s must NOT be rendered, but it is present:\n%s", k, out)
		}
	}
}

// forwardAuthURL auto-derivation: when beadsViewer.loginPicker.forwardAuthURL
// is left empty, the chart derives it from keycloak.forwardAuth.ingress.host
// (helper gardenio.beadsViewer.loginPicker.forwardAuthURL). That URL is the
// oauth2-proxy /oauth2/start base every picker button (including GitLab)
// redirects to — an empty/wrong value silently breaks the whole SSO round
// trip, so pin the derivation.
func TestBeadsViewerPicker_ForwardAuthURL_DerivedFromKeycloakHost(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.loginPicker.enabled=true",
		"keycloak.forwardAuth.ingress.host=fa.example.com",
	)
	if v, ok := envValueAfter(out, "LOGIN_PICKER_FORWARDAUTH_URL"); !ok || v != "https://fa.example.com" {
		t.Errorf("forwardAuthURL derive: LOGIN_PICKER_FORWARDAUTH_URL = %q (present=%v), want \"https://fa.example.com\"", v, ok)
	}
}

// forwardAuthURL explicit override beats the derived Keycloak host: an operator
// setting beadsViewer.loginPicker.forwardAuthURL must win over the
// keycloak.forwardAuth.ingress.host fallback. Guards the helper's
// if/else-if precedence.
func TestBeadsViewerPicker_ForwardAuthURL_ExplicitOverrideWins(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true",
		"beadsViewer.oidc.issuerURL=https://auth.example.com/realms/x",
		"beadsViewer.oidc.clientID=viewer",
		"beadsViewer.loginPicker.enabled=true",
		"beadsViewer.loginPicker.forwardAuthURL=https://explicit.example.com",
		"keycloak.forwardAuth.ingress.host=fa.example.com",
	)
	if v, ok := envValueAfter(out, "LOGIN_PICKER_FORWARDAUTH_URL"); !ok || v != "https://explicit.example.com" {
		t.Errorf("forwardAuthURL override: LOGIN_PICKER_FORWARDAUTH_URL = %q (present=%v), want \"https://explicit.example.com\"", v, ok)
	}
}

// Custom provider labels flow through the chart verbatim. Matt's spec allows
// relabeling either button; a mangled label is a visible regression on the
// login page, so assert the operator-set strings reach the handler env.
func TestBeadsViewerPicker_CustomLabels_FlowThrough(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads-viewer/deployment.yaml",
		baseViewerPickerFlags(
			"beadsViewer.loginPicker.gitlab.label=Sign in with GitLab",
			"beadsViewer.loginPicker.keycloakLocal.label=Local account",
		)...)

	if v, ok := envValueAfter(out, "LOGIN_PICKER_GITLAB_LABEL"); !ok || v != "Sign in with GitLab" {
		t.Errorf("gitlab label: LOGIN_PICKER_GITLAB_LABEL = %q (present=%v), want \"Sign in with GitLab\"", v, ok)
	}
	if v, ok := envValueAfter(out, "LOGIN_PICKER_KEYCLOAK_LOCAL_LABEL"); !ok || v != "Local account" {
		t.Errorf("keycloak label: LOGIN_PICKER_KEYCLOAK_LOCAL_LABEL = %q (present=%v), want \"Local account\"", v, ok)
	}
}
