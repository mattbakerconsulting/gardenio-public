package main

// agent_k8s_workspace.go — K8s agent workspace setup.
//
// Handles: platform version, git global config, git credentials, workspace git
// init / stale-branch reset, daemon config, PVC persistence (symlinks), Claude
// user settings.json, hook materialization, CLAUDE.md generation, and
// onboarding skip.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"gardenio/controller/internal/podmanager"
)

// setupWorkspace performs all one-time workspace setup that runs before coop starts.
func setupWorkspace(cfg k8sConfig) error {
	// 1. Platform version.
	if data, err := os.ReadFile("/etc/platform-version"); err == nil {
		if v := strings.TrimSpace(string(data)); v != "" {
			os.Setenv("BEADS_PLATFORM_VERSION", v)
			fmt.Printf("[gb agent start] platform version: %s\n", v)
		}
	}

	// 2. Git global config.
	authorName := envOr("GIT_AUTHOR_NAME", cfg.role)
	runGitGlobal("config", "--global", "user.name", authorName)
	runGitGlobal("config", "--global", "user.email", cfg.role+"@gardenio.local")
	runGitGlobal("config", "--global", "--add", "safe.directory", "*")
	fmt.Printf("[gb agent start] git global config set (user: %s)\n", authorName)

	// 3. Git credentials — derive hosts from BOAT_PROJECTS repo URLs, falling
	// back to gitlab.com/github.com when none are derivable (ka-0000000218:
	// a missing/unparseable BOAT_PROJECTS must not leave delivered tokens
	// unregistered — mirrors init-clone's ka-0000000552 fallback).
	gitUser := os.Getenv("GIT_USERNAME")
	gitToken := os.Getenv("GIT_TOKEN")
	gitlabToken := os.Getenv("GITLAB_TOKEN")
	if gitUser != "" && gitToken != "" || gitlabToken != "" {
		hosts := credentialHosts(os.Getenv("BOAT_PROJECTS"), gitUser, gitToken, gitlabToken)
		if err := writeGitCredentials(gitUser, gitToken, gitlabToken, hosts); err != nil {
			fmt.Printf("[gb agent start] warning: git credentials: %v\n", err)
		} else {
			fmt.Printf("[gb agent start] git credentials configured for %v\n", hosts)
		}
	}

	// 3b. GitHub CLI auth — read the token from the same projected
	// /etc/git-creds file the init container uses (W6, ka-0000002787)
	// instead of a GH_TOKEN env var, so a crew pod can `gh pr create`
	// without adding a new secret readable via /proc/<pid>/environ, `ps
	// eww`, or `kubectl describe pod` (gd-koIbfse9OE). No-op when the file
	// isn't mounted (no GitHub credentials secret configured for this
	// project/role).
	if configured, err := setupGhAuth(); err != nil {
		fmt.Printf("[gb agent start] warning: gh auth: %v\n", err)
	} else if configured {
		fmt.Printf("[gb agent start] gh CLI authenticated for github.com\n")
	}

	// 3c. Rotating tokens (gd-uPJ6AEsfyn): make git read the projected token
	// live, ahead of the once-written store. The periodic refresh of the store
	// and hosts.yml runs from the pod supervisor (startGitCredsRefresh).
	if err := installProjectedGitHelper(podmanager.MountGitCreds); err != nil {
		fmt.Printf("[gb agent start] warning: projected git credential helper: %v\n", err)
	}

	// 4. Workspace git init or stale-branch reset.
	if _, err := os.Stat(filepath.Join(cfg.workspace, ".git")); os.IsNotExist(err) {
		fmt.Printf("[gb agent start] initialising git repo in %s\n", cfg.workspace)
		if err := runGitIn(cfg.workspace, "init", "-q"); err != nil {
			fmt.Printf("[gb agent start] warning: git init: %v\n", err)
		}
		_ = runGitIn(cfg.workspace, "config", "user.name", authorName)
		_ = runGitIn(cfg.workspace, "config", "user.email", cfg.role+"@gardenio.local")
	} else {
		fmt.Printf("[gb agent start] git repo already exists in %s\n", cfg.workspace)
		resetStaleBranch(cfg.workspace)
		warnInitCloneStash(cfg.workspace)
	}

	// 5. Daemon config (.beads/config.yaml).
	if host := os.Getenv("BEADS_DAEMON_HOST"); host != "" {
		port := envOr("BEADS_DAEMON_HTTP_PORT", "9080")
		daemonURL := fmt.Sprintf("http://%s:%s", host, port)
		fmt.Printf("[gb agent start] configuring daemon at %s\n", daemonURL)
		if err := writeDaemonConfig(cfg.workspace, daemonURL); err != nil {
			fmt.Printf("[gb agent start] warning: daemon config: %v\n", err)
		}
	}

	// 6. Clone project repos (source of truth: project bead, fallback: BOAT_PROJECTS env).
	if err := runSetupRepos(context.Background(), cfg.workspace); err != nil {
		fmt.Printf("[gb agent start] warning: repo clone: %v\n", err)
	}

	return nil
}

// setupPVC creates the .state/{claude,coop} directories on the PVC and
// symlinks ~/.claude into .state/claude so Claude state survives pod restarts.
// Returns the resolved claude state directory path — this is ~/.claude when it
// is a direct PVC subPath mount, or .state/claude when using symlinks.
func setupPVC(cfg k8sConfig) (string, error) {
	stateDir := filepath.Join(cfg.workspace, ".state")
	claudeState := filepath.Join(stateDir, "claude")
	coopState := filepath.Join(stateDir, "coop")

	for _, d := range []string{claudeState, coopState} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", d, err)
		}
	}

	claudeDir := filepath.Join(homeDir(), ".claude")
	if isMountpoint(claudeDir) {
		fmt.Printf("[gb agent start] %s is a mount point (subPath) — already PVC-backed\n", claudeDir)
		// When ~/.claude is a direct mount, session logs live there, not in .state/claude.
		claudeState = claudeDir
	} else {
		os.RemoveAll(claudeDir)
		if err := os.Symlink(claudeState, claudeDir); err != nil {
			return "", fmt.Errorf("symlink %s -> %s: %w", claudeDir, claudeState, err)
		}
		fmt.Printf("[gb agent start] linked %s -> %s\n", claudeDir, claudeState)
	}

	os.Setenv("XDG_STATE_HOME", stateDir)
	fmt.Printf("[gb agent start] XDG_STATE_HOME=%s\n", stateDir)

	// Dev tools PATH.
	if _, err := os.Stat("/usr/local/go/bin"); err == nil {
		os.Setenv("PATH", "/usr/local/go/bin:"+os.Getenv("PATH"))
		fmt.Printf("[gb agent start] added /usr/local/go/bin to PATH\n")
	}

	return claudeState, nil
}

// claudeExtensionSubdirs are the .claude subdirectories whose entries Claude
// Code discovers: sub-agents, skills, and slash commands.
var claudeExtensionSubdirs = []string{"agents", "skills", "commands"}

// extensionFilter decides which discovered extension entries of one subdir are
// linked into the workspace, derived from the agent's role bead(s) toggle
// fields: enabled_skills / disabled_skills for the "skills" subdir (Adam
// feedback #1, epic ka-0000004989) and enabled_agents / disabled_agents for the
// "agents" subdir (ka-0000006908). Commands are always linked. The names it
// matches are the LINKED names Claude Code discovers: primary-repo entries
// bare, sibling entries prefixed "{repo}-" (see linkExtensionSubdir). A zero
// extensionFilter (empty maps) allows everything — the default when no role
// sets the toggle.
type extensionFilter struct {
	// enabled is the allow-list: when non-empty, ONLY entries whose linked name
	// appears here are linked. Empty = no allow-list restriction (everything
	// enabled by default), mirroring account_whitelist's empty=unrestricted.
	enabled map[string]bool
	// disabled is the deny-list: an entry whose linked name appears here is
	// never linked, even if it also appears in enabled (deny wins).
	disabled map[string]bool
}

// allows reports whether an entry with the given LINKED name should be linked.
// Deny-list wins over allow-list; an empty allow-list imposes no restriction.
func (f extensionFilter) allows(linkedName string) bool {
	if f.disabled[linkedName] {
		return false
	}
	if len(f.enabled) > 0 && !f.enabled[linkedName] {
		return false
	}
	return true
}

// active reports whether the filter would ever exclude an entry — used purely
// to gate a log line so a no-op filter stays quiet.
func (f extensionFilter) active() bool {
	return len(f.enabled) > 0 || len(f.disabled) > 0
}

// extensionFilters bundles the per-subdir role toggles consumed by
// linkClaudeExtensions: skills gates the "skills" subdir, agents gates the
// "agents" subdir. Commands have no toggle and always link. A zero value
// (both filters empty) links everything — the pre-toggle behaviour.
type extensionFilters struct {
	skills extensionFilter
	agents extensionFilter
}

// symlinkClaudeExtensions makes custom Claude Code agents, skills, and commands
// from the PRIMARY project repo AND every sibling reference repo discoverable in
// the agent workspace.
//
// The primary repo is cloned by init-clone to /home/agent/bot/{project}/work;
// reference repos are cloned to /home/agent/bot/{project}/repos/{name}
// (setupReferenceRepos / setup_repos.go). Each may carry .claude/{agents,skills,
// commands}. Historically only the primary's directories were symlinked wholesale
// into the workspace, so sibling-repo extensions were invisible.
//
// We now merge PER ENTRY into a real workspace directory so entries from multiple
// repos coexist. Claude Code discovers skills/agents/commands by their one-level
// name under .claude/<sub>/, so sibling entries are NAMESPACED with a "{repo}-"
// prefix to avoid clobbering identically-named primary entries. PRIMARY WINS: the
// primary repo is linked first under bare names, and any later entry (including a
// sibling whose namespaced name still somehow collides) is skipped rather than
// overwriting an existing destination.
//
// The extensionFilters (derived from the agent's role bead enabled_skills /
// disabled_skills and enabled_agents / disabled_agents) gate the "skills" and
// "agents" subdirs respectively — commands are always linked. Zero filters
// allow everything (the default when no role sets a toggle).
func symlinkClaudeExtensions(workspace string, filters extensionFilters) {
	project := os.Getenv("BOAT_PROJECT")
	if project == "" {
		return
	}

	botRoot := filepath.Join("/home/agent/bot", project)
	primary := filepath.Join(botRoot, "work")
	if _, err := os.Stat(primary); os.IsNotExist(err) {
		return
	}

	claudeDir := filepath.Join(workspace, ".claude")
	linkClaudeExtensions(claudeDir, primary, siblingRepoDirs(botRoot), filters)
}

// linkClaudeExtensions links every extension subdir (agents/skills/commands)
// from the primary + sibling repos into claudeDir, applying the role skill
// toggle to the "skills" subdir and the role agent toggle to the "agents"
// subdir — commands always link (a zero extensionFilter allows everything).
// Split out from symlinkClaudeExtensions so the per-subdir filter wiring is
// unit-testable with explicit paths, since the exported function resolves the
// bot root from a hardcoded /home/agent/bot path.
func linkClaudeExtensions(claudeDir, primary string, siblings []string, filters extensionFilters) {
	for _, sub := range claudeExtensionSubdirs {
		// Each subdir gets its own role toggle; commands have none and always
		// link (an empty filter allows everything).
		f := extensionFilter{}
		switch sub {
		case "skills":
			f = filters.skills
		case "agents":
			f = filters.agents
		}
		linkExtensionSubdir(claudeDir, sub, primary, siblings, f)
	}
}

// siblingRepoDirs returns the reference-repo checkout directories under
// {botRoot}/repos, sorted by name for deterministic namespacing/link order.
// Returns nil when the repos directory is absent (primary-only projects) so the
// behaviour collapses to "primary only" exactly as before.
func siblingRepoDirs(botRoot string) []string {
	reposDir := filepath.Join(botRoot, "repos")
	entries, err := os.ReadDir(reposDir)
	if err != nil {
		return nil
	}
	var dirs []string
	for _, e := range entries {
		// A repo checkout is a directory; skip stray files. Symlinked repo
		// dirs report as symlinks from ReadDir, so accept those too.
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 {
			dirs = append(dirs, filepath.Join(reposDir, e.Name()))
		}
	}
	sort.Strings(dirs)
	return dirs
}

// linkExtensionSubdir populates {claudeDir}/{sub} with per-entry symlinks drawn
// from the primary repo (bare names) and each sibling repo (namespaced with a
// "{repo}-" prefix). Primary entries win: a destination that already exists is
// never overwritten. No-op when neither the primary nor any sibling carries the
// subdirectory, so an empty workspace stays empty.
//
// filter gates which entries are linked by their LINKED name (post-namespacing).
// Callers pass a zero extensionFilter for subdirs with no role toggle
// (commands), which allows every entry.
func linkExtensionSubdir(claudeDir, sub, primary string, siblings []string, filter extensionFilter) {
	// Ordered list of (label, source .claude/<sub> dir, namePrefix) to merge.
	// Primary first so it wins on any name collision.
	type source struct {
		label  string
		dir    string
		prefix string
	}
	sources := []source{{label: "primary", dir: filepath.Join(primary, ".claude", sub)}}
	for _, repo := range siblings {
		sources = append(sources, source{
			label:  filepath.Base(repo),
			dir:    filepath.Join(repo, ".claude", sub),
			prefix: filepath.Base(repo) + "-",
		})
	}

	// Skip entirely if no source directory exists — avoids creating an empty
	// {claudeDir}/{sub} on projects that ship none of these extensions.
	anySource := false
	for _, s := range sources {
		if info, err := os.Stat(s.dir); err == nil && info.IsDir() {
			anySource = true
			break
		}
	}
	if !anySource {
		return
	}

	dstBase := filepath.Join(claudeDir, sub)
	if err := ensureRealDir(dstBase); err != nil {
		fmt.Printf("[gb agent start] warning: prepare %s: %v\n", dstBase, err)
		return
	}

	for _, s := range sources {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			continue // source subdir absent for this repo
		}
		for _, e := range entries {
			name := s.prefix + e.Name()
			src := filepath.Join(s.dir, e.Name())
			dst := filepath.Join(dstBase, name)

			// Role toggle: skip entries the role disabled or left out of a
			// non-empty allow-list. Matched on the LINKED (namespaced) name so
			// a role bead's enabled_/disabled_ lists use the same name Claude
			// Code discovers. A zero filter (commands, or no role toggle)
			// allows everything.
			if !filter.allows(name) {
				fmt.Printf("[gb agent start] skipping %s %s (role %s toggle)\n",
					sub, name, strings.TrimSuffix(sub, "s"))
				continue
			}

			// Skip if destination already exists (primary wins; earlier
			// sibling wins over a later one on a namespaced collision).
			if _, err := os.Lstat(dst); err == nil {
				continue
			}

			if err := os.Symlink(src, dst); err != nil {
				fmt.Printf("[gb agent start] warning: symlink %s -> %s: %v\n", dst, src, err)
			} else {
				fmt.Printf("[gb agent start] linked %s -> %s (%s)\n", dst, src, s.label)
			}
		}
	}
}

// ensureRealDir makes path a real directory. If path is currently a symlink
// (the legacy whole-directory link created by earlier gb versions), it is
// replaced by a real directory that re-links the old target's entries under
// their bare names — so a pod restart onto new gb code migrates cleanly without
// losing the previously-linked primary extensions.
func ensureRealDir(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(path, 0o755)
		}
		return err
	}

	if fi.Mode()&os.ModeSymlink != 0 {
		// Legacy whole-directory symlink: re-materialize as a real dir,
		// preserving the old target's entries under bare names.
		oldTarget, rerr := os.Readlink(path)
		if rerr != nil {
			oldTarget = ""
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		if oldTarget != "" {
			if !filepath.IsAbs(oldTarget) {
				oldTarget = filepath.Join(filepath.Dir(path), oldTarget)
			}
			if entries, rerr := os.ReadDir(oldTarget); rerr == nil {
				for _, e := range entries {
					_ = os.Symlink(filepath.Join(oldTarget, e.Name()), filepath.Join(path, e.Name()))
				}
			}
		}
		return nil
	}

	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", path)
	}
	return nil
}


// writeOnboardingSkip ensures ~/.claude.json bypasses the global onboarding
// wizard AND pre-trusts the agent's working directories, so Claude Code never
// renders the per-project "Do you trust this folder?" dialog on (re)start.
//
// ~/.claude.json lives on the pod's ephemeral overlay fs (only ~/.claude is
// PVC-backed), so it is recreated from scratch on every restart. Writing only
// the global onboarding keys left projects[<cwd>].hasTrustDialogAccepted unset,
// re-arming the trust dialog each restart. coop's groom races to auto-dismiss
// it (driver maps trust -> "1"); when it loses the race, coop exits code 1
// mid-startup and the next session inherits a cancelled context, so the agent
// bead is closed as context_cancelled before any analysis posts (ka-0000005485).
//
// We merge into any existing file rather than clobbering, and mark every passed
// directory as trusted so the prompt cannot appear.
func writeOnboardingSkip(trustDirs ...string) {
	path := filepath.Join(homeDir(), ".claude.json")

	cfg := map[string]any{}
	if existing, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(existing, &cfg); err != nil {
			fmt.Printf("[gb agent start] warning: parse existing .claude.json (%v) — rewriting\n", err)
			cfg = map[string]any{}
		}
	}

	cfg["hasCompletedOnboarding"] = true
	cfg["lastOnboardingVersion"] = claudeOnboardingVersion()
	cfg["preferredTheme"] = "dark"
	cfg["bypassPermissionsModeAccepted"] = true

	projects, _ := cfg["projects"].(map[string]any)
	if projects == nil {
		projects = map[string]any{}
	}
	for _, dir := range trustDirs {
		if dir == "" {
			continue
		}
		entry, _ := projects[dir].(map[string]any)
		if entry == nil {
			entry = map[string]any{}
		}
		entry["hasTrustDialogAccepted"] = true
		projects[dir] = entry
	}
	cfg["projects"] = projects

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		fmt.Printf("[gb agent start] warning: marshal .claude.json: %v\n", err)
		return
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		fmt.Printf("[gb agent start] warning: write .claude.json: %v\n", err)
	}
}

// onboardingVersionFallback is the lastOnboardingVersion written when the
// installed Claude Code version cannot be resolved at runtime. Any value that
// is >= the version Claude Code last gated onboarding UI on is sufficient to
// suppress the wizard; we keep it loosely current as a defensive floor.
const onboardingVersionFallback = "2.1.37"

// claudeOnboardingVersion resolves the installed Claude Code version so
// writeOnboardingSkip can stamp lastOnboardingVersion to match the binary
// actually running in the image. Claude Code re-shows onboarding UI when its
// version is newer than lastOnboardingVersion; a hardcoded value silently goes
// stale on every image bump and re-arms the very startup prompt this race is
// about (ka-0000005485). We read it from `claude --version` (output like
// "2.1.154 (Claude Code)") with the CLAUDE_CODE_VERSION env var and a static
// floor as fallbacks, so the value tracks the image automatically.
func claudeOnboardingVersion() string {
	if v := os.Getenv("CLAUDE_CODE_VERSION"); v != "" {
		return v
	}
	out, err := exec.Command("claude", "--version").Output()
	if err == nil {
		if v := parseClaudeVersion(string(out)); v != "" {
			return v
		}
	}
	return onboardingVersionFallback
}

// parseClaudeVersion extracts the semver token from `claude --version` output,
// e.g. "2.1.154 (Claude Code)" -> "2.1.154". Returns "" if no version-looking
// leading token is found.
func parseClaudeVersion(out string) string {
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) == 0 {
		return ""
	}
	tok := fields[0]
	// Must look like a dotted version (digits + dots), not a stray word.
	if !strings.ContainsRune(tok, '.') {
		return ""
	}
	for _, r := range tok {
		if r != '.' && (r < '0' || r > '9') {
			return ""
		}
	}
	return tok
}

// ── helpers ───────────────────────────────────────────────────────────────

func runGitGlobal(args ...string) {
	cmd := exec.Command("git", args...)
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func runGitIn(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// resetStaleBranch resets the main workspace to main/master if it is on a
// stale feature branch — but only when no per-bead worktrees are present.
// When worktrees exist the main repo acts as a coordinator and should not be
// reset: agents work inside individual worktrees under ../worktrees/.
func resetStaleBranch(workspace string) {
	out, err := exec.Command("git", "-C", workspace, "branch", "--show-current").Output()
	if err != nil {
		return
	}
	branch := strings.TrimSpace(string(out))
	if branch == "" || branch == "main" || branch == "master" {
		return
	}

	// If per-bead worktrees exist, warn but do not destroy the main repo state.
	// Agents working in worktrees manage their own branches independently.
	wtDir := worktreesDir(workspace)
	if entries, err := os.ReadDir(wtDir); err == nil && len(entries) > 0 {
		fmt.Printf("[gb agent start] workspace on branch '%s' with %d active worktree(s) — skipping reset\n",
			branch, len(entries))
		return
	}

	fmt.Printf("[gb agent start] WARNING: workspace on stale branch '%s', resetting to main\n", branch)
	_ = runGitIn(workspace, "checkout", "--", ".")
	_ = runGitIn(workspace, "clean", "-fd")
	if runGitIn(workspace, "checkout", "main") != nil {
		_ = runGitIn(workspace, "checkout", "-b", "main")
	}
	out, _ = exec.Command("git", "-C", workspace, "branch", "--show-current").Output()
	fmt.Printf("[gb agent start] workspace now on branch: %s\n", strings.TrimSpace(string(out)))
}

// warnInitCloneStash checks for stashes created by init-clone and warns the agent.
func warnInitCloneStash(workspace string) {
	out, err := exec.Command("git", "-C", workspace, "stash", "list").Output()
	if err != nil || len(out) == 0 {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.Contains(line, "init-clone:") {
			fmt.Printf("[gb agent start] WARNING: %s\n", line)
			fmt.Println("[gb agent start] Previous agent left uncommitted changes. Use 'git stash show -p' to inspect, 'git stash pop' to restore.")
		}
	}
}

// repoHosts extracts unique hostnames from BOAT_PROJECTS.
// Format: "name=https://host/path:prefix,name2=https://host2/path:prefix"
func repoHosts(boatProjects string) []string {
	seen := map[string]bool{}
	for _, entry := range strings.Split(boatProjects, ",") {
		entry = strings.TrimSpace(entry)
		// name=https://host/path:prefix — URL is between '=' and last ':'
		eqIdx := strings.Index(entry, "=")
		if eqIdx < 0 {
			continue
		}
		rawURL := entry[eqIdx+1:]
		// Strip trailing :prefix if present.
		if lastColon := strings.LastIndex(rawURL, ":"); lastColon > strings.Index(rawURL, "//") {
			rawURL = rawURL[:lastColon]
		}
		// Extract host from URL.
		if strings.HasPrefix(rawURL, "https://") {
			host := strings.SplitN(rawURL[len("https://"):], "/", 2)[0]
			if host != "" {
				seen[host] = true
			}
		}
	}
	var hosts []string
	for h := range seen {
		hosts = append(hosts, h)
	}
	return hosts
}

// credentialHosts returns the host set writeGitCredentials should cover:
// the hosts derived from BOAT_PROJECTS, plus a per-token default host when
// derivation yields nothing for a token that WAS delivered (ka-0000000218 —
// e.g. the MicroVM path, whose entrypoint clones via BOAT_GIT_URL and where
// BOAT_PROJECTS may be absent while GITLAB_TOKEN is present). Without the
// fallback the delivered token is never written to the store and the first
// in-VM clone fails with "could not read Username".
func credentialHosts(boatProjects, user, token, gitlabToken string) []string {
	hosts := repoHosts(boatProjects)
	if len(hosts) > 0 {
		return hosts
	}
	if gitlabToken != "" {
		hosts = append(hosts, "gitlab.com")
	}
	if user != "" && token != "" {
		hosts = append(hosts, "github.com")
	}
	return hosts
}

func writeGitCredentials(user, token, gitlabToken string, hosts []string) error {
	home := homeDir()
	credFile := filepath.Join(home, ".git-credentials")
	var lines string
	for _, host := range hosts {
		// Use GITLAB_TOKEN for gitlab.com, GIT_USERNAME/GIT_TOKEN for everything else.
		if strings.Contains(host, "gitlab") && gitlabToken != "" {
			lines += fmt.Sprintf("https://oauth2:%s@%s\n", gitlabToken, host)
		} else if user != "" && token != "" {
			lines += fmt.Sprintf("https://%s:%s@%s\n", user, token, host)
		}
	}
	if lines == "" {
		return nil
	}
	if err := os.WriteFile(credFile, []byte(lines), 0o600); err != nil {
		return err
	}
	// Two-helper chain (bug ka-0000000218). When a remote REFUSES a credential
	// (401/403 — e.g. a MicroVM minting-mode project-scoped token offered to a
	// reference repo on the same host), git runs `credential reject` and the
	// store helper ERASES the host's line, truncating the file — every later
	// git operation then degrades to "could not read Username" even where the
	// token IS valid. The second, stateless helper re-serves the credential
	// from env (git_credential_env.go), so a rejected store line is never
	// fatal. --replace-all + --add keeps re-runs idempotent: exactly one store
	// entry followed by one env entry.
	if err := exec.Command("git", "config", "--global", "--replace-all",
		"credential.helper", "store --file="+credFile).Run(); err != nil {
		return err
	}
	return exec.Command("git", "config", "--global", "--add",
		"credential.helper", gitCredentialEnvHelper()).Run()
}

// gitCredentialEnvHelper returns the credential.helper config value invoking
// `gb git-credential-env` via this very gb binary. The absolute path (rather
// than a bare "gb") keeps the helper working for git subprocesses whose PATH
// differs from the agent session's. %q shell-quotes a path with spaces.
func gitCredentialEnvHelper() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "gb"
	}
	return fmt.Sprintf("!%q git-credential-env", exe)
}

// setupGhAuth authenticates the `gh` CLI for github.com from the token
// projected at podmanager.MountGitCreds/github/{username,token} — the same
// file-based credential the init container reads (W6, ka-0000002787) —
// rather than a GH_TOKEN env var, so `gh auth status`/`gh pr create` work in
// the agent container without adding a secret readable via
// /proc/<pid>/environ, `ps eww`, or `kubectl describe pod` (gd-koIbfse9OE).
//
// Writes $GH_CONFIG_DIR/hosts.yml directly rather than shelling out to `gh
// auth login --with-token`, which would spend a network round-trip
// validating the token against the GitHub API on every pod start. Returns
// (false, nil) — not an error — when no GitHub credentials file is mounted,
// which is the common case for projects/roles without a GitHub secret.
func setupGhAuth() (bool, error) {
	return setupGhAuthFrom(podmanager.MountGitCreds)
}

// setupGhAuthFrom is setupGhAuth with the git-creds base directory injected,
// so tests can point it at a tempdir instead of the real /etc/git-creds
// mount.
func setupGhAuthFrom(gitCredsDir string) (bool, error) {
	c, ok := readProjectedGitHubCreds(gitCredsDir)
	if !ok {
		return false, nil
	}
	// Fine-grained PATs authenticate over HTTPS as any non-empty username;
	// gh itself defaults to "x-access-token" for token-only auth.
	// readProjectedGitHubCreds prefers the username file when the secret
	// carries one (matches the git credential line written for the same host
	// by writeGitCredentials).
	if _, err := writeGhHosts(ghConfigDir(), c); err != nil {
		return false, err
	}
	return true, nil
}

func writeDaemonConfig(workspace, daemonURL string) error {
	dir := filepath.Join(workspace, ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf("daemon-host: %q\ndaemon-token: %q\n",
		daemonURL, os.Getenv("BEADS_DAEMON_TOKEN"))
	return os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(content), 0o600)
}

// isMountpoint checks /proc/mounts to see if path is a mount point.
func isMountpoint(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	f, err := os.Open("/proc/mounts")
	if err != nil {
		return false
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[1] == abs {
			return true
		}
	}
	return false
}


func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/home/agent"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
