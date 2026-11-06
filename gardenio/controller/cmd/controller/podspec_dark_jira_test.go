package main

// Regression guards for the dark-Jira write-token fence, pod layer
// (task ka-0000005242, R7 ka-0000004670). Two invariants are enforced here BY
// CONSTRUCTION:
//
//  1. A DARK agent pod (metadata[beadsapi.MetaDarkPod]=="true" ⇒ spec.Dark)
//     never carries the Jira WRITE credential JIRA_API_TOKEN — no matter which
//     injection layer sets it: project plaintext env, role plaintext env,
//     project/role secret, or the account-credential path. The plaintext-env
//     paths have NO name filtering (unlike the prefix-gated secrets path), which
//     is exactly the unguarded route R7 found; scrubDarkForbiddenEnv closes it
//     and FAILS CLOSED.
//  2. A NON-dark agent pod is untouched — it keeps JIRA_API_TOKEN and gets no
//     dark marker env / label. The fence is dark-scoped, not a global change.
//
// If any of these go red, a change has re-opened the path for the Jira write
// token to reach a dark pod — do not "fix" the test; fix the wiring.

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// hasSecretEnv reports whether the spec mounts a secret under the given env name.
func hasSecretEnv(spec *podmanager.AgentPodSpec, env string) bool {
	for _, e := range spec.SecretEnv {
		if e.EnvName == env {
			return true
		}
	}
	return false
}

// --- Unit: the deny-list predicate ------------------------------------------

func TestIsDarkForbiddenEnv(t *testing.T) {
	forbidden := []string{"JIRA_API_TOKEN", "jira_api_token", " JIRA_API_TOKEN "}
	for _, name := range forbidden {
		if !isDarkForbiddenEnv(name) {
			t.Errorf("isDarkForbiddenEnv(%q) = false, want true (Jira write token must be fenced from dark pods)", name)
		}
	}
	// JIRA_EMAIL / JIRA_BASE_URL are harmless without the token — a dark agent
	// may still know which Jira exists and read it; only the write cred is fenced.
	allowed := []string{"JIRA_EMAIL", "JIRA_BASE_URL", "JIRA_BRIDGE_URL", "GIT_TOKEN", ""}
	for _, name := range allowed {
		if isDarkForbiddenEnv(name) {
			t.Errorf("isDarkForbiddenEnv(%q) = true, want false (only the write token is fenced)", name)
		}
	}
}

// --- scrubDarkForbiddenEnv (injection-site agnostic) ------------------------

func TestScrubDarkForbiddenEnv_DarkPodStripsPlaintextAndSecret(t *testing.T) {
	spec := &podmanager.AgentPodSpec{
		Project: "gardenio",
		Role:    "crew",
		Dark:    true,
		Env: map[string]string{
			"JIRA_API_TOKEN": "plaintext-write-token", // the unguarded route R7 found
			"JIRA_EMAIL":     "gardenio@acme.example",
			"JIRA_BASE_URL":  "https://acme.atlassian.net",
		},
		SecretEnv: []podmanager.SecretEnvSource{
			{EnvName: "JIRA_API_TOKEN", SecretName: "monorepo-jira-api-token", SecretKey: "api-token"},
			{EnvName: "GIT_TOKEN", SecretName: "gardenio-git-credentials", SecretKey: "token"},
		},
	}
	scrubDarkForbiddenEnv(spec)

	if _, ok := spec.Env["JIRA_API_TOKEN"]; ok {
		t.Error("SECURITY REGRESSION: JIRA_API_TOKEN survived in a dark pod's plaintext Env")
	}
	if hasSecretEnv(spec, "JIRA_API_TOKEN") {
		t.Error("SECURITY REGRESSION: JIRA_API_TOKEN secret survived in a dark pod's SecretEnv")
	}
	// The harmless read-only identity + unrelated git token survive.
	if spec.Env["JIRA_EMAIL"] != "gardenio@acme.example" {
		t.Error("JIRA_EMAIL wrongly scrubbed — a dark agent may still read Jira")
	}
	if spec.Env["JIRA_BASE_URL"] != "https://acme.atlassian.net" {
		t.Error("JIRA_BASE_URL wrongly scrubbed — a dark agent may still know which Jira exists")
	}
	if !hasSecretEnv(spec, "GIT_TOKEN") {
		t.Error("unrelated GIT_TOKEN secret wrongly scrubbed")
	}
}

func TestScrubDarkForbiddenEnv_NonDarkPodUntouched(t *testing.T) {
	spec := &podmanager.AgentPodSpec{
		Project: "monorepo",
		Dark:    false,
		Env:     map[string]string{"JIRA_API_TOKEN": "keep-me"},
		SecretEnv: []podmanager.SecretEnvSource{
			{EnvName: "JIRA_API_TOKEN", SecretName: "monorepo-jira-api-token", SecretKey: "api-token"},
		},
	}
	scrubDarkForbiddenEnv(spec)
	if spec.Env["JIRA_API_TOKEN"] != "keep-me" {
		t.Error("non-dark pod lost its plaintext JIRA_API_TOKEN — the fence must be dark-scoped")
	}
	if !hasSecretEnv(spec, "JIRA_API_TOKEN") {
		t.Error("non-dark pod lost its JIRA_API_TOKEN secret — the fence must be dark-scoped")
	}
}

// --- End-to-end through applyCommonConfig -----------------------------------

// TestApplyCommonConfig_DarkPodFencesPlaintextJiraToken proves a project that
// injects JIRA_API_TOKEN as a PLAINTEXT env var (the exact unguarded path R7
// flagged) cannot leak it into a dark pod, while JIRA_EMAIL/BASE_URL survive.
func TestApplyCommonConfig_DarkPodFencesPlaintextJiraToken(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_API_TOKEN", Value: "plaintext-write-token"},
					{Name: "JIRA_EMAIL", Value: "gardenio@acme.example"},
					{Name: "JIRA_BASE_URL", Value: "https://acme.atlassian.net"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Dark: true, Env: map[string]string{}}
	applyCommonConfig(cfg, spec)

	if _, ok := spec.Env["JIRA_API_TOKEN"]; ok {
		t.Error("SECURITY REGRESSION: dark pod carries plaintext JIRA_API_TOKEN after applyCommonConfig")
	}
	if spec.Env["JIRA_EMAIL"] != "gardenio@acme.example" {
		t.Error("JIRA_EMAIL should survive on a dark pod (read-only identity is harmless)")
	}
	// The dark marker env is set so the gb CLI builds a read-only Jira client.
	if spec.Env[beadsapi.EnvGardenioDark] != "true" {
		t.Errorf("expected %s=true on a dark pod, got %q", beadsapi.EnvGardenioDark, spec.Env[beadsapi.EnvGardenioDark])
	}
}

// TestApplyCommonConfig_DarkPodFencesSecretJiraToken proves the monorepo-style
// SECRET path (JIRA_API_TOKEN <- monorepo-jira-api-token) is also fenced from a
// dark pod — the token must not reach it by ANY route.
func TestApplyCommonConfig_DarkPodFencesSecretJiraToken(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				Secrets: []beadsapi.SecretEntry{
					{Env: "JIRA_API_TOKEN", Secret: "gardenio-jira-api-token", Key: "api-token"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Dark: true, Env: map[string]string{}}
	applyCommonConfig(cfg, spec)

	if hasSecretEnv(spec, "JIRA_API_TOKEN") {
		t.Error("SECURITY REGRESSION: dark pod carries JIRA_API_TOKEN secret after applyCommonConfig")
	}
}

// TestApplyCommonConfig_NonDarkKeepsJiraTokenAndNoMarker is the negative case:
// a non-dark pod keeps the token and gets no dark marker env.
func TestApplyCommonConfig_NonDarkKeepsJiraTokenAndNoMarker(t *testing.T) {
	cfg := &config.Config{
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_API_TOKEN", Value: "keep-me"},
				},
			},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Dark: false, Env: map[string]string{}}
	applyCommonConfig(cfg, spec)

	if spec.Env["JIRA_API_TOKEN"] != "keep-me" {
		t.Error("non-dark pod must keep JIRA_API_TOKEN")
	}
	if _, ok := spec.Env[beadsapi.EnvGardenioDark]; ok {
		t.Errorf("non-dark pod must not carry %s", beadsapi.EnvGardenioDark)
	}
}

// --- Full spec builder (reconciler path) ------------------------------------

// TestBuildSpecFromBeadInfo_DarkMetadataFencesToken exercises the public spec
// builder end-to-end: a dark-marked agent bead yields a pod with no
// JIRA_API_TOKEN, the GARDENIO_DARK marker, and the gardenio.io/dark pod label.
func TestBuildSpecFromBeadInfo_DarkMetadataFencesToken(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_API_TOKEN", Value: "plaintext-write-token"},
					{Name: "JIRA_EMAIL", Value: "gardenio@acme.example"},
				},
			},
		},
	}
	meta := map[string]string{beadsapi.MetaDarkPod: "true"}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "darkagent1", meta)

	if !spec.Dark {
		t.Fatal("spec.Dark should be true when metadata carries the dark marker")
	}
	if _, ok := spec.Env["JIRA_API_TOKEN"]; ok {
		t.Error("SECURITY REGRESSION: dark pod (reconciler path) carries JIRA_API_TOKEN")
	}
	if spec.Env[beadsapi.EnvGardenioDark] != "true" {
		t.Errorf("expected %s=true, got %q", beadsapi.EnvGardenioDark, spec.Env[beadsapi.EnvGardenioDark])
	}
	if spec.Labels()[podmanager.LabelDark] != "true" {
		t.Errorf("expected pod label %s=true on a dark pod", podmanager.LabelDark)
	}
}

// TestBuildSpecFromBeadInfo_NonDarkNoFence is the negative full-path case.
func TestBuildSpecFromBeadInfo_NonDarkNoFence(t *testing.T) {
	cfg := &config.Config{
		Namespace: "test",
		CoopImage: "agent:latest",
		ProjectCache: map[string]config.ProjectCacheEntry{
			"gardenio": {
				EnvVars: []beadsapi.EnvEntry{
					{Name: "JIRA_API_TOKEN", Value: "keep-me"},
				},
			},
		},
	}
	spec := BuildSpecFromBeadInfo(cfg, "gardenio", "crew", "crew", "agent1", map[string]string{})

	if spec.Dark {
		t.Fatal("spec.Dark should be false without the dark marker")
	}
	if spec.Env["JIRA_API_TOKEN"] != "keep-me" {
		t.Error("non-dark pod must keep JIRA_API_TOKEN")
	}
	if _, ok := spec.Labels()[podmanager.LabelDark]; ok {
		t.Errorf("non-dark pod must not carry the %s label", podmanager.LabelDark)
	}
}
