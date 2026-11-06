package main

// git_creds_refresh.go — in-pod refresh of GitHub credentials for rotating
// tokens (gd-uPJ6AEsfyn, epic gd-BHLDnjX1Hw; plan github-app-onboarding.md
// §2.1 and §4.2 step 5).
//
// Why it exists: GitHub App installation tokens live 60 minutes, so the
// controller re-mints one and rewrites the projected Secret
// (/etc/git-creds/github/{username,token}; the kubelet re-projects it in ~60s).
// Both agent-side consumers used to be one-shot materializations of that file
// and never saw the rotation:
//
//   - the git credential store (~/.git-credentials, written once by
//     writeGitCredentials), and
//   - gh's $GH_CONFIG_DIR/hosts.yml (written once by setupGhAuth).
//
// Three pieces close the gap:
//
//   - refreshGitHubCreds rewrites both files from the projected mount, only
//     when the token actually changed, atomically (git and gh may read them at
//     any moment);
//   - gitCredsRefreshLoop runs it every gitCredsRefreshInterval for the life of
//     the pod's supervisor (`gb init` on agent pods, `gb agent start`; both
//     via startGitCredsRefresh); `gb token refresh github` is the same thing
//     on demand;
//   - `gb git-credential-projected` is a git credential helper that answers
//     github.com straight from the projected files, installed FIRST in the
//     helper chain, so git sees the current token even between refreshes and
//     even after a `credential reject` erased the store line.
//
// Secret hygiene: the token never appears in an error string, a log line, an
// env var or argv. Errors from this file carry paths and static text only; in
// particular url.Parse errors (which quote their input, token included) are
// never propagated.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/podmanager"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const (
	// gitCredsRefreshInterval is how often the loop re-reads the projected
	// mount. Tokens last 60 min and are re-minted at ~50, and the kubelet
	// re-projects within ~60-120s, so 30s keeps the worst-case lag far inside
	// the 10 min margin while costing two tiny file reads.
	gitCredsRefreshInterval = 30 * time.Second

	githubHost            = "github.com"
	githubDefaultUsername = "x-access-token"

	// projectedHelperName is the subcommand git invokes as a credential helper.
	projectedHelperName = "git-credential-projected"
)

// githubCreds is the GitHub credential projected at
// <gitCredsDir>/github/{username,token}.
type githubCreds struct {
	User  string
	Token string
}

// readProjectedGitHubCreds reads the projected GitHub credential. ok is false
// when no token is mounted (the common case for projects without a GitHub
// secret) or the file is empty. A missing username defaults to GitHub's
// documented HTTPS form for installation-token auth.
func readProjectedGitHubCreds(gitCredsDir string) (githubCreds, bool) {
	tokenBytes, err := os.ReadFile(filepath.Join(gitCredsDir, "github", "token"))
	if err != nil {
		return githubCreds{}, false
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return githubCreds{}, false
	}
	user := githubDefaultUsername
	if userBytes, err := os.ReadFile(filepath.Join(gitCredsDir, "github", "username")); err == nil {
		if u := strings.TrimSpace(string(userBytes)); u != "" {
			user = u
		}
	}
	return githubCreds{User: user, Token: token}, true
}

// writeFileAtomic writes data to path via a same-directory temp file + rename,
// so a concurrent reader (git, gh) sees the old or the new content, never a
// truncated file. Skips the write and returns false when the content is
// already identical. The temp file is created 0600; perm is applied before the
// rename.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (bool, error) {
	if old, err := os.ReadFile(path); err == nil && bytes.Equal(old, data) {
		return false, nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return false, fmt.Errorf("create temp for %s: %w", path, err)
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return false, fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return false, fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return false, fmt.Errorf("rename to %s: %w", path, err)
	}
	return true, nil
}

// ghConfigDir resolves where gh keeps hosts.yml: $GH_CONFIG_DIR, else
// $XDG_CONFIG_HOME/gh, else ~/.config/gh (gh's own precedence).
func ghConfigDir() string {
	if d := os.Getenv("GH_CONFIG_DIR"); d != "" {
		return d
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(xdg, "gh")
}

// writeGhHosts merges the github.com entry for c into $cfgDir/hosts.yml
// (atomically, 0600, only on change) and returns whether the file changed.
// Every other host and every other key under github.com (gh's users: block,
// extra settings) is preserved; only user, oauth_token and git_protocol are
// set. An absent or unparsable file is replaced by a fresh one holding just
// github.com: a stale token matters more than a corrupt config.
func writeGhHosts(cfgDir string, c githubCreds) (bool, error) {
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", cfgDir, err)
	}
	path := filepath.Join(cfgDir, "hosts.yml")
	var doc yaml.Node
	if old, err := os.ReadFile(path); err == nil {
		// The parse error is dropped: never risk it quoting file content.
		if yaml.Unmarshal(old, &doc) != nil {
			doc = yaml.Node{}
		}
	}
	root := &doc
	if root.Kind == yaml.DocumentNode && len(root.Content) == 1 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
		root = doc.Content[0]
	}
	entry := yamlMapValue(root, githubHost)
	if entry.Kind != yaml.MappingNode {
		*entry = yaml.Node{Kind: yaml.MappingNode}
	}
	if c.User != "" {
		yamlSetScalar(entry, "user", c.User)
	}
	yamlSetScalar(entry, "oauth_token", c.Token)
	yamlSetScalar(entry, "git_protocol", "https")
	data, err := yaml.Marshal(&doc)
	if err != nil {
		// yaml errors do not echo field values, but never risk it.
		return false, fmt.Errorf("marshal hosts.yml")
	}
	return writeFileAtomic(path, data, 0o600)
}

// yamlMapValue returns the value node for key in mapping m, appending an empty
// one when the key is absent.
func yamlMapValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	k := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	v := &yaml.Node{}
	m.Content = append(m.Content, k, v)
	return v
}

// yamlSetScalar sets key to the string val in mapping m, keeping the key's
// position and comments when it already exists.
func yamlSetScalar(m *yaml.Node, key, val string) {
	v := yamlMapValue(m, key)
	v.Kind, v.Tag, v.Value, v.Style, v.Content, v.Alias = yaml.ScalarNode, "!!str", val, 0, nil, nil
}

// isGitHubStoreLine reports whether a git-credential-store line is for
// github.com. A line that does not parse as a URL is treated as foreign and
// kept untouched; the parse error is deliberately dropped because it quotes
// the line, token included.
func isGitHubStoreLine(line string) bool {
	u, err := url.Parse(strings.TrimSpace(line))
	return err == nil && u.Hostname() == githubHost
}

// githubStoreLine renders the git-credential-store line for c, URL-escaping
// the userinfo as the store format requires.
func githubStoreLine(c githubCreds) string {
	u := url.URL{Scheme: "https", User: url.UserPassword(c.User, c.Token), Host: githubHost}
	return u.String()
}

// updateGitHubStoreFile replaces the github.com line of the credential store
// at path with c, keeping every other line (e.g. a GitLab entry) as is. It
// only updates a store that already exists: when none was written there is no
// store helper reading it, and the projected helper serves git directly.
func updateGitHubStoreFile(path string, c githubCreds) (bool, error) {
	old, err := os.ReadFile(path)
	if err != nil {
		return false, nil
	}
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(old))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || isGitHubStoreLine(line) {
			continue
		}
		out = append(out, line)
	}
	out = append(out, githubStoreLine(c))
	return writeFileAtomic(path, []byte(strings.Join(out, "\n")+"\n"), 0o600)
}

// refreshGitHubCreds re-reads the projected GitHub credential and rewrites the
// consumers that cached it: the git credential store under home and gh's
// hosts.yml under ghCfgDir. changed reports whether any file was rewritten
// (false when nothing is mounted or the token did not rotate).
func refreshGitHubCreds(gitCredsDir, home, ghCfgDir string) (changed bool, err error) {
	c, ok := readProjectedGitHubCreds(gitCredsDir)
	if !ok {
		return false, nil
	}
	storeChanged, storeErr := updateGitHubStoreFile(filepath.Join(home, ".git-credentials"), c)
	hostsChanged, hostsErr := writeGhHosts(ghCfgDir, c)
	switch {
	case storeErr != nil && hostsErr != nil:
		err = fmt.Errorf("%v; %v", storeErr, hostsErr)
	case storeErr != nil:
		err = storeErr
	case hostsErr != nil:
		err = hostsErr
	}
	return storeChanged || hostsChanged, err
}

// startGitCredsRefresh runs gitCredsRefreshLoop on the projected mount in the
// background until ctx ends. Every pod supervisor must call it: without it
// hosts.yml keeps the spawn-time token and gh fails with 401 once that token
// expires, ~60 min in (gd-jfo7R99vgj). logf takes one line, no newline.
func startGitCredsRefresh(ctx context.Context, logf func(format string, args ...any)) {
	go gitCredsRefreshLoop(ctx, podmanager.MountGitCreds, gitCredsRefreshInterval, logf)
}

// gitCredsRefreshLoop keeps the cached GitHub credentials current for the life
// of the agent. It never exits the process on failure — a failed refresh only
// matters if the token has actually expired, and the next tick retries.
func gitCredsRefreshLoop(ctx context.Context, gitCredsDir string, interval time.Duration, logf func(format string, args ...any)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failing := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			changed, err := refreshGitHubCreds(gitCredsDir, homeDir(), ghConfigDir())
			switch {
			case err != nil:
				// Log once per failure streak, not every tick.
				if !failing {
					logf("WARNING: GitHub credential refresh failed: %v", err)
				}
				failing = true
			case changed:
				failing = false
				logf("GitHub credentials refreshed from projected Secret")
			default:
				failing = false
			}
		}
	}
}

// projectedHelperConfigValue returns the credential.helper config value
// invoking `gb git-credential-projected` via this very gb binary (absolute
// path, same reasoning as gitCredentialEnvHelper).
func projectedHelperConfigValue() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = "gb"
	}
	return fmt.Sprintf("!%q %s", exe, projectedHelperName)
}

// installProjectedGitHelper makes `gb git-credential-projected` the FIRST
// global git credential helper, preserving the rest of the chain in order.
// Git asks helpers in order and stops at the first that answers, so the
// projected files win over the (possibly stale) store and env fallback.
// Idempotent. No-op when no GitHub credential is mounted.
func installProjectedGitHelper(gitCredsDir string) error {
	if _, ok := readProjectedGitHubCreds(gitCredsDir); !ok {
		return nil
	}
	desired := projectedHelperConfigValue()
	var cur, rest []string
	// `git config --get-all` exits 1 when the key is unset; treat as empty.
	if out, err := exec.Command("git", "config", "--global", "--get-all", "credential.helper").Output(); err == nil {
		for _, h := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if h == "" {
				continue
			}
			cur = append(cur, h)
			if !strings.Contains(h, projectedHelperName) {
				rest = append(rest, h)
			}
		}
	}
	want := append([]string{desired}, rest...)
	if slices.Equal(cur, want) {
		return nil
	}
	// Exit 5 = nothing to unset; fine.
	_ = exec.Command("git", "config", "--global", "--unset-all", "credential.helper").Run()
	for _, h := range want {
		if err := exec.Command("git", "config", "--global", "--add", "credential.helper", h).Run(); err != nil {
			return fmt.Errorf("git config credential.helper: %w", err)
		}
	}
	return nil
}

// gitCredentialProjectedResponse returns the credential-helper `get` response
// for attrs, or "" when this helper has nothing to serve. It answers only for
// host (github.com in production) over https, so the token is never offered to
// another host.
func gitCredentialProjectedResponse(attrs map[string]string, gitCredsDir, host string) string {
	if attrs["host"] != host {
		return ""
	}
	if proto := attrs["protocol"]; proto != "" && proto != "https" {
		return ""
	}
	c, ok := readProjectedGitHubCreds(gitCredsDir)
	if !ok {
		return ""
	}
	return "username=" + c.User + "\npassword=" + c.Token + "\n"
}

// helperNoDaemon overrides the root PersistentPreRunE: git invokes credential
// helpers on every authenticated remote operation, so they must not construct
// (or fail on) a beads daemon client.
func helperNoDaemon(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true
	return nil
}

var gitCredentialProjectedCmd = &cobra.Command{
	Use:               projectedHelperName + " [operation]",
	Short:             "Git credential helper serving the projected GitHub token (always current)",
	Hidden:            true,
	Args:              cobra.ArbitraryArgs,
	PersistentPreRunE: helperNoDaemon,
	RunE: func(cmd *cobra.Command, args []string) error {
		// store/erase are deliberate no-ops: the projected Secret is the
		// source of truth and is not ours to mutate.
		if len(args) == 0 || args[0] != "get" {
			return nil
		}
		attrs := parseGitCredentialAttrs(cmd.InOrStdin())
		if resp := gitCredentialProjectedResponse(attrs, podmanager.MountGitCreds, githubHost); resp != "" {
			fmt.Fprint(cmd.OutOrStdout(), resp)
		}
		return nil
	},
}

var tokenCmd = &cobra.Command{
	Use:   "token",
	Short: "Credential token maintenance inside an agent pod",
}

var tokenRefreshCmd = &cobra.Command{
	Use:   "refresh",
	Short: "Refresh cached credentials from the projected Secrets",
}

var tokenRefreshGithubCmd = &cobra.Command{
	Use:   "github",
	Short: "Re-read the projected GitHub token and rewrite the git credential store and gh hosts.yml now",
	Long: `Re-read /etc/git-creds/github/{username,token} (the kubelet re-projects the
Secret within ~60s of the controller rotating the installation token) and
rewrite ~/.git-credentials and $GH_CONFIG_DIR/hosts.yml.

` + "`gb agent start`" + ` does this every 30s automatically; run it by hand to force an
immediate refresh. The token is never printed.`,
	Args:              cobra.NoArgs,
	PersistentPreRunE: helperNoDaemon,
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, ok := readProjectedGitHubCreds(podmanager.MountGitCreds); !ok {
			fmt.Fprintln(cmd.OutOrStdout(), "no GitHub credential projected at "+podmanager.MountGitCreds+"/github; nothing to refresh")
			return nil
		}
		changed, err := refreshGitHubCreds(podmanager.MountGitCreds, homeDir(), ghConfigDir())
		if err != nil {
			return err
		}
		if changed {
			fmt.Fprintln(cmd.OutOrStdout(), "GitHub credentials refreshed")
		} else {
			fmt.Fprintln(cmd.OutOrStdout(), "GitHub credentials already current")
		}
		return nil
	},
}

func init() {
	tokenRefreshCmd.AddCommand(tokenRefreshGithubCmd)
	tokenCmd.AddCommand(tokenRefreshCmd)
}
