package tests

import (
	"strings"
	"testing"
)

// The Lands plot application's chart wiring on beads-viewer (decision
// gd-j3EiWECejv "chart"; step 7.7 gd-LJoqthyf2w, rollout gd-NgnBO6s1uc;
// runbook gardenio/docs/lands/github-app.md).
//
// Invariants locked here:
//   - Everything is off by default: no LANDS_APPLY_*, LANDS_APPROVER,
//     LANDS_GITHUB_APP_* or MAILER_* env on the viewer, no lands or mailer
//     volumes.
//   - beadsViewer.landsApply.enabled renders LANDS_APPLY_ENABLED=true plus the
//     optional approver, daily limit and default credits, and FAILS the render
//     without beadsViewer.machinePrincipal.enabled (the Gate writes only as
//     that principal).
//   - beadsViewer.landsGithubApp.enabled renders the App id, client id, slug
//     and the two secret PATHS; the key and client secret are a read-only file
//     mount of the Secret, never env (no secretKeyRef). Missing appId or
//     clientId fails the render.
//   - With the Gate on and agents.mailer.enabled, the viewer gets the same
//     MAILER_* env as the controller and, with credentialsSecret.name, the
//     same read-only SES key files. The Lands App alone never pulls the
//     mailer in.

const (
	viewerPrincipalOn = "beadsViewer.enabled=true,beadsViewer.machinePrincipal.enabled=true"
	landsApplyOn      = viewerPrincipalOn + ",beadsViewer.landsApply.enabled=true"
	landsAppOn        = "beadsViewer.enabled=true,beadsViewer.landsGithubApp.enabled=true," +
		"beadsViewer.landsGithubApp.appId=4242,beadsViewer.landsGithubApp.clientId=Iv1.abc"
	landsAppDir     = "/etc/gardenio/lands-github-app"
	viewerMailerOn  = "agents.mailer.enabled=true,agents.mailer.from=lands@x.example,agents.mailer.baseUrl=https://x.example"
	viewerDeployTpl = "templates/beads-viewer/deployment.yaml"
)

func TestLandsApply_OffByDefault(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, viewerDeployTpl, viewerPrincipalOn)
	for _, forbidden := range []string{"LANDS_APPLY_", "LANDS_APPROVER", "LANDS_GITHUB_", "LANDS_MAX_REPO", "MAILER_", "lands-github-app", "mailer-ses-credentials"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("viewer: Lands apply off by default, but %q is rendered:\n%s", forbidden, out)
		}
	}
}

func TestLandsApply_RendersGateEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, viewerDeployTpl, landsApplyOn,
		"beadsViewer.landsApply.approver=oidc:owner@x.example",
		"beadsViewer.landsApply.dailyLimit=5", "beadsViewer.landsApply.defaultCredits=10")
	want := map[string]string{
		"LANDS_APPLY_ENABLED":         "true",
		"LANDS_APPROVER":              "oidc:owner@x.example",
		"LANDS_APPLY_DAILY_LIMIT":     "5",
		"LANDS_APPLY_DEFAULT_CREDITS": "10",
	}
	for k, wantV := range want {
		if got, ok := envValueAfter(out, k); !ok || got != wantV {
			t.Errorf("viewer %s = %q (present=%v), want %q", k, got, ok, wantV)
		}
	}
}

// Empty approver, limit and credits leave the env out: unset LANDS_APPROVER
// means only gardenio-admins count, the others take the binary defaults.
func TestLandsApply_EmptyOptionalsUseBinaryDefaults(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, viewerDeployTpl, landsApplyOn)
	if got, ok := envValueAfter(out, "LANDS_APPLY_ENABLED"); !ok || got != "true" {
		t.Fatalf("LANDS_APPLY_ENABLED = %q (present=%v), want true", got, ok)
	}
	for _, k := range []string{"LANDS_APPROVER", "LANDS_APPLY_DAILY_LIMIT", "LANDS_APPLY_DEFAULT_CREDITS"} {
		if _, ok := envValueAfter(out, k); ok {
			t.Errorf("viewer %s rendered for an empty value; the binary default must hold", k)
		}
	}
}

func TestLandsApply_RequiresMachinePrincipal(t *testing.T) {
	t.Parallel()
	out := helmTemplateExpectError(t, viewerDeployTpl, "--set", "beadsViewer.enabled=true,beadsViewer.landsApply.enabled=true")
	if !strings.Contains(out, "beadsViewer.landsApply.enabled requires beadsViewer.machinePrincipal.enabled") {
		t.Errorf("render must fail naming the machine principal, got:\n%s", out)
	}
}

func TestLandsGithubApp_RendersEnvAndFileMount(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, viewerDeployTpl, landsAppOn)
	want := map[string]string{
		"LANDS_GITHUB_APP_ID":                 "4242",
		"LANDS_GITHUB_APP_CLIENT_ID":          "Iv1.abc",
		"LANDS_GITHUB_APP_SLUG":               "gardenio-lands",
		"LANDS_GITHUB_APP_PRIVATE_KEY_PATH":   landsAppDir + "/private-key",
		"LANDS_GITHUB_APP_CLIENT_SECRET_PATH": landsAppDir + "/client-secret",
	}
	for k, wantV := range want {
		if got, ok := envValueAfter(out, k); !ok || got != wantV {
			t.Errorf("viewer %s = %q (present=%v), want %q", k, got, ok, wantV)
		}
	}
	for _, k := range []string{"LANDS_GITHUB_APP_API_URL", "LANDS_GITHUB_APP_WEB_URL", "LANDS_GITHUB_ALLOW_PRIVATE_REPOS", "LANDS_MAX_REPO_SIZE_MB"} {
		if _, ok := envValueAfter(out, k); ok {
			t.Errorf("viewer %s rendered at the defaults; the binary default must hold", k)
		}
	}
	if strings.Contains(out, "secretKeyRef") {
		t.Errorf("the Lands App key and client secret must be files, never env (secretKeyRef):\n%s", out)
	}
	for _, needle := range []string{
		"- name: lands-github-app\n              mountPath: \"" + landsAppDir + "\"\n              readOnly: true",
		"secretName: \"gardenio-lands-github-app\"",
		"- key: private-key\n                path: private-key",
		"- key: client-secret\n                path: client-secret",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("viewer Deployment missing %q:\n%s", needle, out)
		}
	}
	// The App alone never pulls the mailer in: that rides the Gate.
	if strings.Contains(out, "MAILER_") {
		t.Errorf("landsGithubApp must not render MAILER_* env:\n%s", out)
	}
}

func TestLandsGithubApp_Overrides(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, viewerDeployTpl, landsAppOn,
		"beadsViewer.landsGithubApp.allowPrivateRepos=false", "beadsViewer.landsGithubApp.maxRepoSizeMB=0",
		"beadsViewer.landsGithubApp.apiUrl=https://api.example", "beadsViewer.landsGithubApp.webUrl=https://web.example",
		"beadsViewer.landsGithubApp.mountPath=/mnt/app/")
	want := map[string]string{
		"LANDS_GITHUB_ALLOW_PRIVATE_REPOS":  "false",
		"LANDS_MAX_REPO_SIZE_MB":            "0",
		"LANDS_GITHUB_APP_API_URL":          "https://api.example",
		"LANDS_GITHUB_APP_WEB_URL":          "https://web.example",
		"LANDS_GITHUB_APP_PRIVATE_KEY_PATH": "/mnt/app/private-key",
	}
	for k, wantV := range want {
		if got, ok := envValueAfter(out, k); !ok || got != wantV {
			t.Errorf("viewer %s = %q (present=%v), want %q", k, got, ok, wantV)
		}
	}
}

func TestLandsGithubApp_RequiresIds(t *testing.T) {
	t.Parallel()
	for name, set := range map[string]string{
		"no appId":    "beadsViewer.enabled=true,beadsViewer.landsGithubApp.enabled=true,beadsViewer.landsGithubApp.clientId=Iv1.abc",
		"no clientId": "beadsViewer.enabled=true,beadsViewer.landsGithubApp.enabled=true,beadsViewer.landsGithubApp.appId=4242",
	} {
		t.Run(name, func(t *testing.T) {
			out := helmTemplateExpectError(t, viewerDeployTpl, "--set", set)
			if !strings.Contains(out, "beadsViewer.landsGithubApp.") {
				t.Errorf("render must fail naming the missing landsGithubApp key, got:\n%s", out)
			}
		})
	}
}

func TestLandsApply_MailerRidesTheGate(t *testing.T) {
	t.Parallel()
	// Mailer on, Gate off: nothing on the viewer (the controller keeps it).
	out := helmTemplate(t, viewerDeployTpl, viewerPrincipalOn, viewerMailerOn, "agents.mailer.credentialsSecret.name=gardenio-mailer-ses")
	for _, forbidden := range []string{"MAILER_", "mailer-ses-credentials"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("viewer must not render %q while the Gate is off:\n%s", forbidden, out)
		}
	}

	// Gate on, mailer on, default chain: the env, no SES files.
	out = helmTemplate(t, viewerDeployTpl, landsApplyOn, viewerMailerOn, "agents.mailer.sesRegion=us-east-1")
	want := map[string]string{
		"MAILER_ENABLED":    "true",
		"MAILER_FROM":       "lands@x.example",
		"MAILER_BASE_URL":   "https://x.example",
		"MAILER_SES_REGION": "us-east-1",
	}
	for k, wantV := range want {
		if got, ok := envValueAfter(out, k); !ok || got != wantV {
			t.Errorf("viewer %s = %q (present=%v), want %q", k, got, ok, wantV)
		}
	}
	for _, forbidden := range []string{"MAILER_SES_CREDENTIALS_DIR", "mailer-ses-credentials"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("viewer must not render %q without credentialsSecret.name:\n%s", forbidden, out)
		}
	}

	// Gate on, mailer on, its own SES keys: the controller's file contract.
	out = helmTemplate(t, viewerDeployTpl, landsApplyOn, viewerMailerOn, "agents.mailer.credentialsSecret.name=gardenio-mailer-ses")
	if got, ok := envValueAfter(out, "MAILER_SES_CREDENTIALS_DIR"); !ok || got != "/var/run/secrets/gardenio-mailer-ses" {
		t.Errorf("MAILER_SES_CREDENTIALS_DIR = %q (present=%v)", got, ok)
	}
	for _, needle := range []string{
		"- name: mailer-ses-credentials\n              mountPath: /var/run/secrets/gardenio-mailer-ses\n              readOnly: true",
		"secretName: \"gardenio-mailer-ses\"",
		"path: accessKeyId",
		"path: secretAccessKey",
	} {
		if !strings.Contains(out, needle) {
			t.Errorf("viewer Deployment missing %q:\n%s", needle, out)
		}
	}
	if strings.Contains(out, "secretKeyRef") {
		t.Errorf("the SES keys must be files, never env (secretKeyRef):\n%s", out)
	}
}
