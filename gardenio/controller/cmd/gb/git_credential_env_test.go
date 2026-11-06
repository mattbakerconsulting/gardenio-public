package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Bug ka-0000000218: env-var credential helper — the stateless fallback that
// survives the credential-store file being truncated by `git credential
// reject` (a refused project-scoped token in MicroVM minting mode).

// TestMain lets the TEST BINARY stand in for the gb binary when git invokes
// it as a credential helper: the TestWriteGitCredentials* tests run
// writeGitCredentials, whose env-helper entry points at os.Executable() — in
// a test process that IS the test binary. Without this guard a real `git
// credential` call would re-execute the entire test suite recursively.
func TestMain(m *testing.M) {
	runAsGbIfRequested() // hook_stopgate_gemini_realbinary_test.go
	if len(os.Args) > 1 && os.Args[1] == "git-credential-env" {
		if len(os.Args) > 2 && os.Args[2] == "get" {
			if resp := gitCredentialEnvResponse(parseGitCredentialAttrs(os.Stdin), os.Getenv); resp != "" {
				fmt.Print(resp)
			}
		}
		os.Exit(0)
	}
	if len(os.Args) > 1 && os.Args[1] == projectedHelperName {
		// Same stand-in for `gb git-credential-projected`; the projected dir
		// and host are test-injected (production hardcodes /etc/git-creds and
		// github.com).
		if len(os.Args) > 2 && os.Args[2] == "get" {
			resp := gitCredentialProjectedResponse(parseGitCredentialAttrs(os.Stdin),
				os.Getenv("GB_TEST_PROJECTED_DIR"), os.Getenv("GB_TEST_PROJECTED_HOST"))
			if resp != "" {
				fmt.Print(resp)
			}
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func envFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestParseGitCredentialAttrs(t *testing.T) {
	attrs := parseGitCredentialAttrs(strings.NewReader(
		"protocol=https\nhost=gitlab.com\npath=group/repo.git\n\nignored=after-blank\n"))
	if attrs["protocol"] != "https" || attrs["host"] != "gitlab.com" {
		t.Errorf("unexpected attrs: %v", attrs)
	}
	if _, ok := attrs["ignored"]; ok {
		t.Errorf("attributes after the blank-line terminator must be ignored: %v", attrs)
	}
	if len(parseGitCredentialAttrs(strings.NewReader(""))) != 0 {
		t.Error("empty input should produce no attrs")
	}
}

func TestGitCredentialEnvResponse(t *testing.T) {
	env := envFrom(map[string]string{
		"BOAT_PROJECTS": "gardenio=https://gitlab.com/acme/gardenio:gb,other=https://github.com/org/repo:o",
		"GITLAB_TOKEN":  "gl-tok",
		"GIT_USERNAME":  "u",
		"GIT_TOKEN":     "gh-tok",
	})

	// GitLab host → oauth2 + GITLAB_TOKEN (mirrors writeGitCredentials).
	got := gitCredentialEnvResponse(map[string]string{"protocol": "https", "host": "gitlab.com"}, env)
	if got != "username=oauth2\npassword=gl-tok\n" {
		t.Errorf("gitlab response: %q", got)
	}

	// Non-GitLab host in BOAT_PROJECTS → GIT_USERNAME/GIT_TOKEN.
	got = gitCredentialEnvResponse(map[string]string{"protocol": "https", "host": "github.com"}, env)
	if got != "username=u\npassword=gh-tok\n" {
		t.Errorf("github response: %q", got)
	}

	// Host outside BOAT_PROJECTS → nothing (never widen where tokens go).
	if got = gitCredentialEnvResponse(map[string]string{"protocol": "https", "host": "evil.example.com"}, env); got != "" {
		t.Errorf("unknown host must get no credential, got %q", got)
	}

	// Non-https protocol → nothing.
	if got = gitCredentialEnvResponse(map[string]string{"protocol": "http", "host": "gitlab.com"}, env); got != "" {
		t.Errorf("http must get no credential, got %q", got)
	}

	// Missing host → nothing.
	if got = gitCredentialEnvResponse(map[string]string{"protocol": "https"}, env); got != "" {
		t.Errorf("hostless request must get no credential, got %q", got)
	}

	// GitLab host but no GITLAB_TOKEN → falls back to user/token pair.
	envNoGL := envFrom(map[string]string{
		"BOAT_PROJECTS": "p=https://gitlab.com/x/y:p",
		"GIT_USERNAME":  "u",
		"GIT_TOKEN":     "t",
	})
	got = gitCredentialEnvResponse(map[string]string{"host": "gitlab.com"}, envNoGL)
	if got != "username=u\npassword=t\n" {
		t.Errorf("gitlab-without-GITLAB_TOKEN fallback: %q", got)
	}

	// No tokens at all → nothing.
	empty := envFrom(map[string]string{"BOAT_PROJECTS": "p=https://gitlab.com/x/y:p"})
	if got = gitCredentialEnvResponse(map[string]string{"host": "gitlab.com"}, empty); got != "" {
		t.Errorf("tokenless env must get no credential, got %q", got)
	}
}

func TestCredentialHosts(t *testing.T) {
	// BOAT_PROJECTS present → derived hosts only, no defaults appended.
	hosts := credentialHosts("p=https://gitlab.example.com/x/y:p", "u", "t", "gl")
	if len(hosts) != 1 || hosts[0] != "gitlab.example.com" {
		t.Errorf("derived hosts must win: %v", hosts)
	}
	// No BOAT_PROJECTS + GITLAB_TOKEN → gitlab.com fallback (the MicroVM
	// minting-mode shape from ka-0000000218).
	hosts = credentialHosts("", "", "", "gl")
	if len(hosts) != 1 || hosts[0] != "gitlab.com" {
		t.Errorf("gitlab fallback: %v", hosts)
	}
	// No BOAT_PROJECTS + user/token pair → github.com fallback.
	hosts = credentialHosts("", "u", "t", "")
	if len(hosts) != 1 || hosts[0] != "github.com" {
		t.Errorf("github fallback: %v", hosts)
	}
	// Both token kinds, no BOAT_PROJECTS → both defaults.
	hosts = credentialHosts("", "u", "t", "gl")
	if len(hosts) != 2 {
		t.Errorf("both fallbacks expected: %v", hosts)
	}
	// Nothing delivered → nothing to cover.
	if hosts = credentialHosts("", "", "", ""); len(hosts) != 0 {
		t.Errorf("expected empty, got %v", hosts)
	}
}

func TestGitCredentialEnvResponseFallbackHosts(t *testing.T) {
	// MicroVM minting-mode shape: GITLAB_TOKEN delivered, BOAT_PROJECTS absent.
	env := envFrom(map[string]string{"GITLAB_TOKEN": "minted"})
	got := gitCredentialEnvResponse(map[string]string{"protocol": "https", "host": "gitlab.com"}, env)
	if got != "username=oauth2\npassword=minted\n" {
		t.Errorf("fallback-host gitlab response: %q", got)
	}
	// Still refuses hosts outside the fallback set.
	if got = gitCredentialEnvResponse(map[string]string{"protocol": "https", "host": "github.com"}, env); got != "" {
		t.Errorf("no github credential should be served without a user/token pair, got %q", got)
	}
}

func TestGitCredentialEnvHelperQuoting(t *testing.T) {
	h := gitCredentialEnvHelper()
	if !strings.HasPrefix(h, "!\"") || !strings.HasSuffix(h, "\" git-credential-env") {
		t.Errorf("helper value must be a shell-quoted executable invocation, got %q", h)
	}
}

// TestWriteGitCredentialsSurvivesReject is the end-to-end regression for
// ka-0000000218 against the REAL git binary: after writeGitCredentials, a
// `git credential reject` (what git does internally on a 401/403) truncates
// the store file — the helper chain must still resolve the credential from
// env on the next fill.
func TestWriteGitCredentialsSurvivesReject(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Neutralize any ambient git config redirection.
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("BOAT_PROJECTS", "gardenio=https://gitlab.com/acme/gardenio:gb")
	t.Setenv("GITLAB_TOKEN", "env-token")

	if err := writeGitCredentials("", "", "env-token", []string{"gitlab.com"}); err != nil {
		t.Fatalf("writeGitCredentials: %v", err)
	}

	// The env-helper config entry points at os.Executable() — in this process
	// that is the test binary, which TestMain teaches to serve the helper
	// protocol, so `git credential fill` below exercises the REAL chain.
	credFile := filepath.Join(home, ".git-credentials")
	data, err := os.ReadFile(credFile)
	if err != nil {
		t.Fatalf("store not written: %v", err)
	}
	if !strings.Contains(string(data), "oauth2:env-token@gitlab.com") {
		t.Fatalf("store missing credential line: %s", data)
	}

	// git credential reject — the truncation the bug report observed.
	rej := exec.Command(gitPath, "credential", "reject")
	rej.Env = os.Environ()
	rej.Stdin = strings.NewReader("protocol=https\nhost=gitlab.com\n\n")
	if out, err := rej.CombinedOutput(); err != nil {
		t.Fatalf("git credential reject: %v (%s)", err, out)
	}
	data, _ = os.ReadFile(credFile)
	if strings.Contains(string(data), "env-token") {
		t.Fatalf("precondition failed: reject did not erase the store line: %s", data)
	}

	// End-to-end: `git credential fill` must STILL resolve the credential —
	// the store is empty, so git falls through to the env helper (served by
	// the test binary via TestMain).
	fill := exec.Command(gitPath, "credential", "fill")
	fill.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=")
	fill.Stdin = strings.NewReader("protocol=https\nhost=gitlab.com\n\n")
	out2, err := fill.Output()
	if err != nil {
		t.Fatalf("git credential fill after reject: %v", err)
	}
	filled := string(out2)
	if !strings.Contains(filled, "username=oauth2") || !strings.Contains(filled, "password=env-token") {
		t.Fatalf("env helper must re-serve the credential after store truncation, got %q", filled)
	}

	// And the config chain must actually contain both helpers, in order.
	cfg := exec.Command(gitPath, "config", "--global", "--get-all", "credential.helper")
	cfg.Env = os.Environ()
	out, err := cfg.Output()
	if err != nil {
		t.Fatalf("git config --get-all credential.helper: %v", err)
	}
	helpers := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(helpers) != 2 {
		t.Fatalf("expected exactly 2 credential helpers, got %d: %v", len(helpers), helpers)
	}
	if !strings.HasPrefix(helpers[0], "store --file=") {
		t.Errorf("first helper must be the store: %q", helpers[0])
	}
	if !strings.Contains(helpers[1], "git-credential-env") {
		t.Errorf("second helper must be the env fallback: %q", helpers[1])
	}
}

// TestWriteGitCredentialsIdempotent re-runs writeGitCredentials (pod restart /
// resume path) and asserts the helper chain does not accumulate duplicates.
func TestWriteGitCredentialsIdempotent(t *testing.T) {
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not on PATH")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(home, ".gitconfig"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	for i := 0; i < 3; i++ {
		if err := writeGitCredentials("u", "t", "gl", []string{"gitlab.com"}); err != nil {
			t.Fatalf("writeGitCredentials run %d: %v", i, err)
		}
	}
	cfg := exec.Command(gitPath, "config", "--global", "--get-all", "credential.helper")
	cfg.Env = os.Environ()
	out, err := cfg.Output()
	if err != nil {
		t.Fatalf("git config: %v", err)
	}
	helpers := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(helpers) != 2 {
		t.Errorf("helper chain must stay at 2 entries across re-runs, got %d: %v", len(helpers), helpers)
	}
}
