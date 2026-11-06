package main

// init_agent_setup.go — one-time agent pod setup for gb init.
//
// Consolidates all setup that entrypoint.sh currently does in bash into Go.
// Called once at the start of gb init before the restart loop.
// All functions are idempotent — safe to re-run on pod restart.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// runSetup performs all one-time setup before the restart loop.
// This is the Go equivalent of entrypoint.sh lines 54–467.
func runSetup(cfg initConfig) error {
	setupSSHKnownHosts()
	setupWorkspace(initToK8sConfig(cfg))

	claudeStateDir, err := setupPVC(initToK8sConfig(cfg))
	if err != nil {
		return fmt.Errorf("PVC setup: %w", err)
	}
	cfg.claudeStateDir = claudeStateDir

	setupGitLabEmail()
	setupReferenceRepos(cfg)

	credMode := provisionCredentials(claudeStateDir)
	_ = credMode

	// Account resolution is the controller's job, not gb init's. The controller
	// (applyAccountConfig → selectAccount) is the single authoritative
	// resolver: it picks the project account by live-pod-count spread, enforces
	// the role account_whitelist (ka-0000003061), bakes that account's credential
	// into the pod, and pins spec.Env["BOAT_ACCOUNT"] to it. gb init runs inside
	// the pod and has no access to the controller's live pod counts / account
	// probe cache, so it simply consumes that pin — coop/coopmux registration
	// reads BOAT_ACCOUNT from the env the controller already set. The old
	// gb-init FNV-hash resolver (resolveProjectAccount) computed a SECOND,
	// divergent pick and was removed (ka-0000002572): for controller-spawned
	// pods BOAT_ACCOUNT is always pre-set, so it only ever early-returned.

	setupDevToolsPath()
	setupCustomCACerts()

	cwd := resolveCoopWorkdir(initToK8sConfig(cfg))
	setupGardenioChannelSymlink(cwd)
	setupClaudeSettingsFn(cwd, cfg.role, cfg.mockMode)
	writeOnboardingSkip(cwd, cfg.workspace)

	// Disable interactive features that interrupt autonomous agents.
	if os.Getenv("CLAUDE_CODE_ENABLE_TASKS") == "" {
		os.Setenv("CLAUDE_CODE_ENABLE_TASKS", "false")
	}

	return nil
}

// setupSSHKnownHosts populates ~/.ssh/known_hosts for github.com and gitlab.com.
// Tries ssh-keyscan first; falls back to baked-in published keys.
func setupSSHKnownHosts() {
	sshDir := filepath.Join(homeDir(), ".ssh")
	knownHosts := filepath.Join(sshDir, "known_hosts")

	// Skip if already populated.
	if data, err := os.ReadFile(knownHosts); err == nil && strings.Contains(string(data), "github.com ") {
		return
	}

	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		logInit("warning: mkdir .ssh: %v", err)
		return
	}

	// Try ssh-keyscan first.
	cmd := exec.Command("ssh-keyscan", "-t", "ed25519,ecdsa,rsa", "github.com", "gitlab.com")
	if output, err := cmd.Output(); err == nil && len(output) > 0 {
		if err := appendFile(knownHosts, output, 0o644); err == nil {
			logInit("SSH known_hosts populated via ssh-keyscan")
			return
		}
	}

	// Fallback: baked-in keys.
	fallbackKeys := `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf
gitlab.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFSMqzJeV9rUzU4kWitGjeR4PWSa29SPqJ1fVkhtj3Hw9xjLVXVYrU9QlYWrOLXBpQ6KWjbjTDTdDkoohFzgbEY=
gitlab.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQCsj2bNKTBSpIYDEGk9KxsGh3mySTRgMtXL583qmBpzeQ+jqCMRgBqB98u3z++Jzuwa7FR2gpol/a7elJCEPDcCVirYHn91Dm7bSSa4wzkFCQbRpLh4K/JaBUQjjltUbCXbQNGQ8OFVDdNHvSH5NLX9+j5IeOz08g6D3Z1D6cpQ+/Zy2gLxZ5K8Xc/qVnqnYZp0SmZ2tARcaUhBxP8KIWf1H3tYS+OgWLyE91oy7Xm+hqn1NM/10bMnDHXPUBG7nS3bPb+G+0rS29Mj0+7xCC4OFdWv1IUWAMB7VGVPYu0G+oiOC+LKVnZ7s/3rauN4j8Ka6dGJ7M4dlKVpBjOd7N
`
	if err := appendFile(knownHosts, []byte(fallbackKeys), 0o644); err != nil {
		logInit("warning: write known_hosts: %v", err)
	} else {
		logInit("SSH known_hosts populated with fallback keys")
	}
}

// setupGitLabEmail resolves the git committer email from the GitLab account
// when GIT_AUTHOR_EMAIL is not explicitly set. GitLab rejects pushes if the
// committer email doesn't match a verified address.
func setupGitLabEmail() {
	if os.Getenv("GIT_AUTHOR_EMAIL") != "" {
		return // explicitly set, don't override
	}
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	if gitlabToken == "" {
		return
	}

	glHost := envOr("GITLAB_HOST", "gitlab.com")
	client := &http.Client{Timeout: 10 * time.Second}

	// Try /user/emails first (verified addresses).
	email := fetchGitLabEmail(client, glHost, gitlabToken)
	if email == "" {
		// Fallback: derive noreply email from /user.
		email = fetchGitLabNoreplyEmail(client, glHost, gitlabToken)
	}

	if email != "" {
		runGitGlobal("config", "--global", "user.email", email)
		logInit("git committer email resolved from GitLab: %s", email)
	} else {
		logInit("WARNING: could not resolve GitLab committer email")
	}
}

func fetchGitLabEmail(client *http.Client, host, token string) string {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/api/v4/user/emails", host), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var emails []struct {
		Email       string  `json:"email"`
		ConfirmedAt *string `json:"confirmed_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&emails); err != nil {
		return ""
	}
	for _, e := range emails {
		if e.ConfirmedAt != nil {
			return e.Email
		}
	}
	return ""
}

func fetchGitLabNoreplyEmail(client *http.Client, host, token string) string {
	req, err := http.NewRequest("GET", fmt.Sprintf("https://%s/api/v4/user", host), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := client.Do(req)
	if err != nil || resp.StatusCode != 200 {
		return ""
	}
	defer resp.Body.Close()

	var user struct {
		ID int `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil || user.ID == 0 {
		return ""
	}
	return fmt.Sprintf("%d-noreply@%s", user.ID, host)
}

// setupReferenceRepos clones reference repos declared on the project bead.
// These are cloned by the init container for PVC-backed pods; this is the
// fallback for job mode (EmptyDir) where no init container runs.
func setupReferenceRepos(cfg initConfig) {
	repoSpec := os.Getenv("BOAT_REFERENCE_REPOS")
	if repoSpec == "" {
		return
	}

	// Skip if init container already cloned them.
	initReposDir := filepath.Join("/home/agent/bot", cfg.project, "repos")
	if _, err := os.Stat(initReposDir); err == nil {
		return
	}

	reposDir := filepath.Join(cfg.workspace, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		logInit("warning: mkdir repos: %v", err)
		return
	}

	for _, entry := range strings.Split(repoSpec, ",") {
		entry = strings.TrimSpace(entry)
		eqIdx := strings.Index(entry, "=")
		if eqIdx < 0 {
			continue
		}
		name := entry[:eqIdx]
		rest := entry[eqIdx+1:]
		// Strip trailing :branch suffix.
		url := rest
		if lastColon := strings.LastIndex(rest, ":"); lastColon > strings.Index(rest, "//") {
			url = rest[:lastColon]
		}
		// Guard: if stripping produced a bare scheme, use full value.
		if url == "https" || url == "http" {
			url = rest
		}

		dest := filepath.Join(reposDir, name)
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			logInit("repo already present: %s", dest)
			continue
		}
		logInit("cloning %s → %s", url, dest)
		cmd := exec.Command("git", "clone", "--filter=blob:none", "--quiet", url, dest)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			logInit("WARNING: clone failed for %s: %v", url, err)
		}
	}
}

// setupDevToolsPath adds dev tool directories to PATH if they exist.
func setupDevToolsPath() {
	for _, dir := range []string{"/usr/local/go/bin", "/usr/local/cargo/bin"} {
		if _, err := os.Stat(dir); err == nil {
			os.Setenv("PATH", dir+":"+os.Getenv("PATH"))
		}
	}
	// Playwright browsers path.
	if _, err := os.Stat("/ms-playwright"); err == nil && os.Getenv("PLAYWRIGHT_BROWSERS_PATH") == "" {
		os.Setenv("PLAYWRIGHT_BROWSERS_PATH", "/ms-playwright")
	}
}

// setupCustomCACerts updates the system CA trust store if custom certs are mounted.
func setupCustomCACerts() {
	caDir := "/usr/local/share/ca-certificates"
	entries, err := filepath.Glob(filepath.Join(caDir, "*.crt"))
	if err != nil || len(entries) == 0 {
		return
	}
	logInit("custom CA certificates found, updating trust store")
	cmd := exec.Command("update-ca-certificates")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		logInit("WARNING: update-ca-certificates failed: %v", err)
	}
}

// setupGardenioChannelSymlink creates the channel/ symlink in the coop working
// directory so that gb setup claude can discover the gardenio-channel MCP server.
func setupGardenioChannelSymlink(coopWorkdir string) {
	if os.Getenv("GARDENIO_CHANNEL") != "1" {
		return
	}
	channelDir := "/channel"
	if _, err := os.Stat(filepath.Join(channelDir, "gardenio-channel.ts")); err != nil {
		return
	}
	dest := filepath.Join(coopWorkdir, "channel")
	if _, err := os.Lstat(dest); err == nil {
		return // already exists
	}
	if err := os.Symlink(channelDir, dest); err != nil {
		logInit("warning: symlink channel: %v", err)
	} else {
		logInit("symlinked channel/ → %s", channelDir)
	}
}

// setupClaudeSettingsFn indirects setupClaudeSettings so the pool
// re-materialization path can be tested without writing a real ~/.claude tree.
var setupClaudeSettingsFn = setupClaudeSettings

// setupClaudeSettings materializes user settings and workspace hooks from
// config beads via `gb setup claude`.
func setupClaudeSettings(coopWorkdir, role string, mockMode bool) {
	if isCodexHarness() {
		// harness=codex pods get $CODEX_HOME/config.toml, not ~/.claude (ADR 0004 §E).
		setupCodexSettings(coopWorkdir, role, mockMode)
		return
	}
	if isGeminiHarness() {
		// harness=gemini pods get ~/.gemini/settings.json, not ~/.claude (ADR 0005 §5 E).
		setupGeminiSettings(coopWorkdir, role, mockMode)
		return
	}
	if mockMode {
		logInit("mock mode — skipping Claude settings")
		claudeDir := filepath.Join(homeDir(), ".claude")
		os.MkdirAll(claudeDir, 0o755)
		_ = os.WriteFile(filepath.Join(claudeDir, "settings.json"), []byte("{}\n"), 0o600)
		return
	}

	if err := runSetupClaude(context.Background(), coopWorkdir, role, false); err != nil {
		logInit("config beads not found, installing defaults")
		if err2 := runSetupClaudeDefaults(coopWorkdir, role, false); err2 != nil {
			logInit("warning: could not write settings: %v", err2)
		}
	}
}

// appendFile appends data to a file, creating it if it doesn't exist.
func appendFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
