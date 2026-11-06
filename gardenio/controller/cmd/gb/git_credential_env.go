package main

// git_credential_env.go — `gb git-credential-env`, a stateless git credential
// helper that serves tokens from the agent environment (bug ka-0000000218).
//
// Why it exists: agents authenticate git-over-https through
// `credential.helper = store --file=~/.git-credentials`, populated by
// writeGitCredentials (agent_k8s_workspace.go) from GITLAB_TOKEN /
// GIT_USERNAME / GIT_TOKEN. When a remote REFUSES a credential (HTTP 401/403
// — e.g. a MicroVM minting-mode per-run token, which is project-scoped, being
// offered to a reference repo on the same host), git invokes `credential
// reject` and the store helper ERASES the matching line. One refused clone
// therefore wipes the host's credential for every later git operation —
// including ones the token IS valid for — degrading them from "Access denied"
// to "could not read Username", while the still-valid env tokens sit shadowed
// by the empty store.
//
// This helper closes that hole: writeGitCredentials registers it as a SECOND
// credential.helper after the store. On `get`, git consults helpers in order
// — the store answers on the fast path; when a store line has been erased,
// this helper re-supplies the credential straight from the environment. Its
// `store`/`erase` operations are deliberate no-ops (it has no state to
// mutate), so the credential can never be lost for the lifetime of the agent
// process tree.
//
// Scope: it only answers for the credentialHosts() set — the hosts derived
// from BOAT_PROJECTS plus the per-token default hosts, exactly what
// writeGitCredentials writes into the store — so it never widens where env
// tokens are sent.

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

var gitCredentialEnvCmd = &cobra.Command{
	Use:    "git-credential-env [operation]",
	Short:  "Git credential helper serving agent env tokens (fallback after the store)",
	Hidden: true,
	Args:   cobra.ArbitraryArgs,
	// Overrides the root PersistentPreRunE: git invokes this helper on every
	// authenticated remote operation, so it must not construct (or fail on) a
	// beads daemon client.
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cmd.SilenceUsage = true
		return nil
	},
	RunE: func(cmd *cobra.Command, args []string) error {
		// store/erase are deliberate no-ops — statelessness is the point.
		if len(args) == 0 || args[0] != "get" {
			return nil
		}
		attrs := parseGitCredentialAttrs(cmd.InOrStdin())
		if resp := gitCredentialEnvResponse(attrs, os.Getenv); resp != "" {
			fmt.Fprint(cmd.OutOrStdout(), resp)
		}
		return nil
	},
}

// parseGitCredentialAttrs reads the `key=value` attribute block git writes to
// a credential helper's stdin (terminated by a blank line or EOF).
func parseGitCredentialAttrs(r io.Reader) map[string]string {
	attrs := map[string]string{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			attrs[k] = v
		}
	}
	return attrs
}

// gitCredentialEnvResponse returns the credential-helper `get` response for
// the given request attributes, or "" when this helper has nothing to serve.
// The host must be one writeGitCredentials would cover (credentialHosts —
// BOAT_PROJECTS-derived plus per-token defaults), and the gitlab-vs-
// user/token selection mirrors the store lines it writes so the two helpers
// can never disagree.
func gitCredentialEnvResponse(attrs map[string]string, getenv func(string) string) string {
	host := attrs["host"]
	if host == "" {
		return ""
	}
	if proto := attrs["protocol"]; proto != "" && proto != "https" {
		return ""
	}
	user, token := getenv("GIT_USERNAME"), getenv("GIT_TOKEN")
	gitlabToken := getenv("GITLAB_TOKEN")
	if !slices.Contains(credentialHosts(getenv("BOAT_PROJECTS"), user, token, gitlabToken), host) {
		return ""
	}
	if strings.Contains(host, "gitlab") && gitlabToken != "" {
		return "username=oauth2\npassword=" + gitlabToken + "\n"
	}
	if user != "" && token != "" {
		return "username=" + user + "\npassword=" + token + "\n"
	}
	return ""
}
