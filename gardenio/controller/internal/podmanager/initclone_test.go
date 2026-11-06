package podmanager

import (
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "main", "'main'"},
		{"empty", "", "''"},
		{"with spaces", "my project", "'my project'"},
		{"with single quote", "it's", `'it'\''s'`},
		{"with double quote", `say "hello"`, `'say "hello"'`},
		{"with backtick", "foo`bar`", "'foo`bar`'"},
		{"with dollar", "foo$bar", "'foo$bar'"},
		{"with semicolon", "foo; rm -rf /", "'foo; rm -rf /'"},
		{"with newline", "foo\nbar", "'foo\nbar'"},
		{"with backslash", `foo\bar`, `'foo\bar'`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shellQuote(tt.in)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestBuildInitCloneContainer_NilWhenNoSources(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		AgentName: "agent",
	}
	if c := m.buildInitCloneContainer(spec); c != nil {
		t.Error("expected nil container when no GitURL or ReferenceRepos")
	}
}

func TestBuildInitCloneContainer_QuotesProjectName(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "my project",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent-1",
		GitURL:    "https://github.com/example/repo.git",
	}
	c := m.buildInitCloneContainer(spec)
	if c == nil {
		t.Fatal("expected non-nil container")
	}

	script := c.Command[2] // /bin/sh -c <script>
	// The project name should be single-quoted to prevent shell word splitting.
	if !strings.Contains(script, "'my project'") {
		t.Errorf("script does not contain quoted project name:\n%s", script)
	}
}

func TestBuildInitCloneContainer_QuotesMaliciousURL(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://evil.com/repo.git'; echo pwned; '",
	}
	c := m.buildInitCloneContainer(spec)
	if c == nil {
		t.Fatal("expected non-nil container")
	}

	script := c.Command[2]
	// The URL with embedded shell commands should be safely quoted.
	// It should NOT contain unquoted "echo pwned".
	if strings.Contains(script, "echo pwned") && !strings.Contains(script, `'\''echo pwned`) {
		// Check that "echo pwned" only appears inside quotes
		if !strings.Contains(script, `'; echo pwned; '`) {
			t.Errorf("script may be vulnerable to injection:\n%s", script)
		}
	}
}

func TestBuildInitCloneContainer_DefaultBranch(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://github.com/example/repo.git",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	// Default branch should be "main", quoted.
	if !strings.Contains(script, "checkout 'main'") {
		t.Errorf("script does not checkout default branch 'main':\n%s", script)
	}
}

func TestBuildInitCloneContainer_CustomBranch(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:          "test",
		Mode:             "crew",
		Role:             "dev",
		AgentName:        "agent",
		GitURL:           "https://github.com/example/repo.git",
		GitDefaultBranch: "develop",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	if !strings.Contains(script, "checkout 'develop'") {
		t.Errorf("script does not use custom branch:\n%s", script)
	}
}

func TestBuildInitCloneContainer_PrimaryCloneIsBlobless(t *testing.T) {
	// The primary repo clone must be blobless (--filter=blob:none), NOT shallow:
	// full history download (1.5G on the monorepo) was ~95s of the ~200s
	// cold-start admit -- almost all of it historical blob contents. A blobless
	// clone keeps the initial fetch small while leaving full history, all tags,
	// and all branches available for on-demand blob fetch, so it avoids the
	// sharp edges of a --depth 1 shallow clone (broken git log / merge-base /
	// describe, forced git fetch --unshallow). is-shallow stays false.
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://github.com/example/repo.git",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	// The primary `git clone ... -b 'main'` line must use --filter=blob:none.
	if !strings.Contains(script, "git clone --filter=blob:none -b 'main'") {
		t.Errorf("primary clone line is not blobless (expected `git clone --filter=blob:none -b 'main'`):\n%s", script)
	}
	// The old shallow flags must be gone from every executable `git clone` line
	// -- they reintroduce the shallow-clone sharp edges this change removed. We
	// check the clone command lines only, not the whole script: the explanatory
	// comment block legitimately references `--depth 1` to document WHY we moved
	// away from it.
	for _, ln := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(ln)
		if !strings.Contains(trimmed, "git clone") {
			continue
		}
		for _, flag := range []string{"--depth 1", "--single-branch", "--no-tags"} {
			if strings.Contains(trimmed, flag) {
				t.Errorf("clone line must not carry shallow flag %q (blobless replaces shallow): %q", flag, trimmed)
			}
		}
	}
}

func TestBuildInitCloneContainer_ReferenceRepos(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		ReferenceRepos: []RepoRef{
			{URL: "https://github.com/ref/repo.git", Name: "ref-repo", Branch: "v2"},
		},
	}
	c := m.buildInitCloneContainer(spec)
	if c == nil {
		t.Fatal("expected non-nil container for reference repos")
	}
	script := c.Command[2]
	if !strings.Contains(script, "'ref-repo'") {
		t.Errorf("script does not contain quoted ref name:\n%s", script)
	}
	if !strings.Contains(script, "'v2'") {
		t.Errorf("script does not contain quoted ref branch:\n%s", script)
	}
}

func TestBuildInitCloneContainer_ReferenceRepoDiscardIsNonFatal(t *testing.T) {
	// The script runs under `set -e`. `git clean` exits non-zero when it cannot
	// unlink a path (a root-owned or mode-500 untracked dir left by a prior
	// agent), so an unguarded discard killed init-clone permanently: the path
	// lives on the persistent workspace volume, so every restart died at the
	// same line, holding the trigger's max_concurrent lease and leaving it
	// silently dark (ka-0000005271). Both discard commands must be best-effort.
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		ReferenceRepos: []RepoRef{
			{URL: "https://github.com/ref/repo.git", Name: "ref-repo", Branch: "v2"},
		},
	}
	script := m.buildInitCloneContainer(spec).Command[2]
	for _, want := range []string{
		"git checkout -- . 2>/dev/null || true",
		"git clean -fd 2>/dev/null || true",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("reference-repo discard is fatal under set -e; missing %q:\n%s", want, script)
		}
	}
}

func TestBuildInitCloneContainer_StashesDirtyPVC(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://github.com/example/repo.git",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	// When repo is already cloned, the script must stash uncommitted changes
	// before checkout to prevent CrashLoopBackOff on dirty PVCs.
	if !strings.Contains(script, "git stash push") {
		t.Errorf("script missing 'git stash push' for dirty PVC preservation:\n%s", script)
	}
	if !strings.Contains(script, "init-clone: auto-stash") {
		t.Errorf("script missing init-clone stash message:\n%s", script)
	}
	// Falls back to hard discard if stash fails.
	if !strings.Contains(script, "git checkout -- .") {
		t.Errorf("script missing fallback 'git checkout -- .' for stash failure:\n%s", script)
	}
}

func TestBuildInitCloneContainer_UpgradesShallowPVCToBlobless(t *testing.T) {
	// A PVC cloned by an older controller is shallow (--depth 1). Fresh pods now
	// get a blobless clone, but a reused shallow PVC keeps the broken merge-base /
	// ancestry behavior until it is upgraded. The "already cloned" branch must
	// carry an idempotent guard that, when the repo IS shallow, marks origin as a
	// promisor with a blob:none filter and --unshallow-fetches the full history
	// (ka-0000006480).
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://github.com/example/repo.git",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	// Guarded on the shallow check so it is a no-op (idempotent) once the repo is
	// already deep.
	if !strings.Contains(script, "git rev-parse --is-shallow-repository") {
		t.Errorf("script missing '--is-shallow-repository' guard for PVC migration:\n%s", script)
	}
	// The one-time upgrade must mark origin as a blob:none promisor and unshallow
	// through the retry wrapper (cold-netns tolerant).
	for _, want := range []string{
		"git config remote.origin.promisor true",
		"git config remote.origin.partialclonefilter blob:none",
		"git_retry git fetch --unshallow --filter=blob:none --tags",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing PVC blobless-migration line %q:\n%s", want, script)
		}
	}
}

func TestBuildInitCloneContainer_RefRepoCleansDirtyPVC(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		ReferenceRepos: []RepoRef{
			{URL: "https://github.com/ref/repo.git", Name: "ref-repo", Branch: "v2"},
		},
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	// Reference repos discard dirty state before checkout.
	if !strings.Contains(script, "git checkout -- .") {
		t.Errorf("ref repo script missing dirty PVC cleanup:\n%s", script)
	}
}

func TestBuildInitCloneContainer_CredentialSetup(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:              "test",
		Mode:                 "crew",
		Role:                 "dev",
		AgentName:            "agent",
		GitURL:               "https://github.com/example/repo.git",
		GitCredentialsSecret: "git-creds",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]
	if !strings.Contains(script, "credential.helper") {
		t.Errorf("script missing credential helper setup:\n%s", script)
	}

	// W6 (ka-0000002787): credentials MUST come from the projected mount
	// under /etc/git-creds/, NOT from GIT_USERNAME / GIT_TOKEN env vars.
	envNames := map[string]bool{}
	for _, e := range c.Env {
		envNames[e.Name] = true
	}
	if envNames["GIT_USERNAME"] || envNames["GIT_TOKEN"] {
		t.Errorf("init container must not receive GIT_USERNAME/GIT_TOKEN env vars; got: %v", envNames)
	}

	// Script should read username/token from the file mount, not env vars.
	if !strings.Contains(script, "/etc/git-creds/github/username") {
		t.Errorf("script must read username from /etc/git-creds/github/username:\n%s", script)
	}
	if !strings.Contains(script, "/etc/git-creds/github/token") {
		t.Errorf("script must read token from /etc/git-creds/github/token:\n%s", script)
	}
	if strings.Contains(script, "$GIT_USERNAME") || strings.Contains(script, "${GIT_USERNAME}") {
		t.Errorf("script must not reference $GIT_USERNAME env var:\n%s", script)
	}
	if strings.Contains(script, "$GIT_TOKEN") || strings.Contains(script, "${GIT_TOKEN}") {
		t.Errorf("script must not reference $GIT_TOKEN env var:\n%s", script)
	}

	// Init container must mount the projected git-creds volume.
	var sawGitCredsMount bool
	for _, mnt := range c.VolumeMounts {
		if mnt.Name == VolumeGitCreds {
			sawGitCredsMount = true
			if mnt.MountPath != MountGitCreds {
				t.Errorf("git-creds mount path = %q, want %q", mnt.MountPath, MountGitCreds)
			}
			if !mnt.ReadOnly {
				t.Error("git-creds mount must be read-only")
			}
		}
	}
	if !sawGitCredsMount {
		t.Errorf("init container missing git-creds volume mount, got: %+v", c.VolumeMounts)
	}
}

// TestGitlabCredentialHosts pins the host-derivation helper behind the
// init-clone credential block (ka-0000000552): hosts come from the spec's repo
// URLs (primary + reference repos), github.com is excluded (the github block
// writes its own line), results are de-duplicated and sorted, and an empty
// derivation falls back to gitlab.com for back-compat.
func TestGitlabCredentialHosts(t *testing.T) {
	tests := []struct {
		name string
		spec AgentPodSpec
		want []string
	}{
		{
			name: "gitlab.com primary only",
			spec: AgentPodSpec{GitURL: "https://gitlab.com/example/repo.git"},
			want: []string{"gitlab.com"},
		},
		{
			name: "self-hosted gitlab host",
			spec: AgentPodSpec{GitURL: "https://gitlab.example.com/team/repo.git"},
			want: []string{"gitlab.example.com"},
		},
		{
			name: "host with port retained",
			spec: AgentPodSpec{GitURL: "https://gitlab.internal:8443/team/repo.git"},
			want: []string{"gitlab.internal:8443"},
		},
		{
			name: "multi-host across primary + reference repos, deduped + sorted",
			spec: AgentPodSpec{
				GitURL: "https://gitlab.example.com/team/repo.git",
				ReferenceRepos: []RepoRef{
					{URL: "https://gitlab.example.com/team/lib.git", Name: "lib"},
					{URL: "https://git.acme.dev/infra/charts.git", Name: "charts"},
				},
			},
			want: []string{"git.acme.dev", "gitlab.example.com"},
		},
		{
			name: "github.com excluded (handled by github block)",
			spec: AgentPodSpec{
				GitURL: "https://github.com/example/repo.git",
				ReferenceRepos: []RepoRef{
					{URL: "https://gitlab.example.com/team/lib.git", Name: "lib"},
				},
			},
			want: []string{"gitlab.example.com"},
		},
		{
			name: "github-only falls back to gitlab.com",
			spec: AgentPodSpec{GitURL: "https://github.com/example/repo.git"},
			want: []string{"gitlab.com"},
		},
		{
			name: "no derivable host falls back to gitlab.com",
			spec: AgentPodSpec{ReferenceRepos: []RepoRef{{URL: "git@gitlab.com:team/repo.git", Name: "ssh"}}},
			want: []string{"gitlab.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gitlabCredentialHosts(tt.spec)
			if len(got) != len(tt.want) {
				t.Fatalf("gitlabCredentialHosts() = %v, want %v", got, tt.want)
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Fatalf("gitlabCredentialHosts() = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

// TestBuildInitCloneContainer_DerivesGitlabHostFromRepoURL verifies the
// init-clone credential helper writes an oauth2 credential line for the
// project's ACTUAL GitLab host(s), not a hardcoded gitlab.com (ka-0000000552).
// Previously the first clone of a private repo on any other GitLab host failed
// because no credential entry existed for it.
func TestBuildInitCloneContainer_DerivesGitlabHostFromRepoURL(t *testing.T) {
	m := &K8sManager{}

	// Multi-host spec: a self-hosted GitLab primary + a second self-hosted
	// reference repo. Both hosts must get an oauth2 credential line; gitlab.com
	// must NOT appear (it isn't referenced).
	t.Run("non-gitlab.com multi-host", func(t *testing.T) {
		spec := AgentPodSpec{
			Project:           "acme",
			Mode:              "crew",
			Role:              "dev",
			AgentName:         "agent",
			GitURL:            "https://gitlab.example.com/team/repo.git",
			GitlabTokenSecret: "gitlab-token",
			ReferenceRepos: []RepoRef{
				{URL: "https://git.acme.dev/infra/charts.git", Name: "charts"},
			},
		}
		script := m.buildInitCloneContainer(spec).Command[2]

		for _, host := range []string{"gitlab.example.com", "git.acme.dev"} {
			want := `printf 'https://oauth2:%s@%s\n' "$GL_TOKEN" '` + host + `' >> /tmp/.git-credentials`
			if !strings.Contains(script, want) {
				t.Errorf("script missing derived-host credential line for %q:\nwant substring: %s\ngot:\n%s", host, want, script)
			}
		}
		// gitlab.com is not referenced by this spec, so no gitlab.com oauth2
		// line should be emitted.
		if strings.Contains(script, `"$GL_TOKEN" 'gitlab.com'`) {
			t.Errorf("script must not emit a gitlab.com credential line for a non-gitlab.com-only spec:\n%s", script)
		}
	})

	// Back-compat: a gitlab.com-only spec still gets the gitlab.com line.
	t.Run("gitlab.com back-compat", func(t *testing.T) {
		spec := AgentPodSpec{
			Project:           "test",
			Mode:              "crew",
			Role:              "dev",
			AgentName:         "agent",
			GitURL:            "https://gitlab.com/example/repo.git",
			GitlabTokenSecret: "gitlab-token",
		}
		script := m.buildInitCloneContainer(spec).Command[2]
		want := `printf 'https://oauth2:%s@%s\n' "$GL_TOKEN" 'gitlab.com' >> /tmp/.git-credentials`
		if !strings.Contains(script, want) {
			t.Errorf("gitlab.com-only spec lost its credential line (back-compat regression):\nwant substring: %s\ngot:\n%s", want, script)
		}
	})

	// Fallback: when only a github credentials secret is configured (no
	// GitlabTokenSecret) and no GitLab host is derivable, the github-token
	// fallback line must still target gitlab.com so existing behavior holds.
	t.Run("github-token fallback to gitlab.com", func(t *testing.T) {
		spec := AgentPodSpec{
			Project:              "test",
			Mode:                 "crew",
			Role:                 "dev",
			AgentName:            "agent",
			GitURL:               "https://github.com/example/repo.git",
			GitCredentialsSecret: "git-creds",
		}
		script := m.buildInitCloneContainer(spec).Command[2]
		want := `printf 'https://%s:%s@%s\n' "$GH_USER" "$GH_TOKEN" 'gitlab.com' >> /tmp/.git-credentials`
		if !strings.Contains(script, want) {
			t.Errorf("github-token fallback must target gitlab.com when no GitLab host derivable:\nwant substring: %s\ngot:\n%s", want, script)
		}
	})
}

// TestBuildInitCloneContainer_GitlabTokenNoEnvLeak verifies that the GitLab
// token is also consumed from the file mount, not GITLAB_TOKEN env var.
// W6 (ka-0000002787).
func TestBuildInitCloneContainer_GitlabTokenNoEnvLeak(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:           "test",
		Mode:              "crew",
		Role:              "dev",
		AgentName:         "agent",
		GitURL:            "https://gitlab.com/example/repo.git",
		GitlabTokenSecret: "gitlab-token",
	}
	c := m.buildInitCloneContainer(spec)
	script := c.Command[2]

	for _, e := range c.Env {
		if e.Name == "GITLAB_TOKEN" {
			t.Errorf("init container must not receive GITLAB_TOKEN env var; got: %+v", e)
		}
	}
	if !strings.Contains(script, "/etc/git-creds/gitlab/token") {
		t.Errorf("script must read GitLab token from /etc/git-creds/gitlab/token:\n%s", script)
	}
	if strings.Contains(script, "$GITLAB_TOKEN") || strings.Contains(script, "${GITLAB_TOKEN}") {
		t.Errorf("script must not reference $GITLAB_TOKEN env var:\n%s", script)
	}
}

// TestBuildInitCloneContainer_CredsReadTolerateNoNewline guards against using
// `read -r VAR < file` on the projected git-creds mount. Kubernetes Secret
// files have no trailing newline, so `read` exits non-zero on EOF, which
// kills the script under `set -e` with no log output (the failure mode that
// crashlooped every crew pod in 2026.145.4 — gd-… W6 regression).
func TestBuildInitCloneContainer_CredsReadTolerateNoNewline(t *testing.T) {
	m := &K8sManager{}
	for name, spec := range map[string]AgentPodSpec{
		"github-only": {
			Project: "test", Mode: "crew", Role: "dev", AgentName: "agent",
			GitURL: "https://github.com/example/repo.git", GitCredentialsSecret: "git-creds",
		},
		"gitlab-only": {
			Project: "test", Mode: "crew", Role: "dev", AgentName: "agent",
			GitURL: "https://gitlab.com/example/repo.git", GitlabTokenSecret: "gitlab-token",
		},
		"both": {
			Project: "test", Mode: "crew", Role: "dev", AgentName: "agent",
			GitURL:               "https://gitlab.com/example/repo.git",
			GitCredentialsSecret: "git-creds", GitlabTokenSecret: "gitlab-token",
		},
	} {
		t.Run(name, func(t *testing.T) {
			script := m.buildInitCloneContainer(spec).Command[2]
			for _, banned := range []string{
				"read -r GH_USER < /etc/git-creds",
				"read -r GH_TOKEN < /etc/git-creds",
				"read -r GL_TOKEN < /etc/git-creds",
			} {
				if strings.Contains(script, banned) {
					t.Errorf("script must not %q (K8s Secret files lack trailing newline; use $(cat ...) instead):\n%s", banned, script)
				}
			}
		})
	}
}

// TestBuildInitCloneContainer_RetriesNetworkOpsForColdNetns verifies the
// init-clone script wraps its network git operations in the bounded git_retry
// helper (ka-0000005535). On a freshly-created freeze/thaw placeholder Pod B the
// kubelet may start init-clone before the AWS VPC CNI has route-programmed the
// netns; the first network git op then exits 128 and (under set -e) the pod
// enters terminal phase Failed before Ready, fail-closing the thaw before the
// barrier restore. The retry lets a cold netns self-heal within the CNI window.
func TestBuildInitCloneContainer_RetriesNetworkOpsForColdNetns(t *testing.T) {
	m := &K8sManager{}
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://github.com/example/repo.git",
		ReferenceRepos: []RepoRef{
			{Name: "ref1", URL: "https://github.com/example/ref1.git"},
		},
	}
	c := m.buildInitCloneContainer(spec)
	if c == nil {
		t.Fatal("expected non-nil container")
	}
	script := c.Command[2]

	// The retry helper must be defined.
	if !strings.Contains(script, "git_retry() {") {
		t.Errorf("script must define git_retry() helper:\n%s", script)
	}

	// Every network git op that hard-fails under set -e must be retried so a
	// cold/unrouted CNI netns self-heals instead of fail-closing the thaw.
	for _, want := range []string{
		"git_retry git clone --filter=blob:none -b", // primary + reference repo clone
		"git_retry git fetch --all --prune",         // primary repo already-cloned path
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script must wrap network op with retry: missing %q:\n%s", want, script)
		}
	}

	// A bare (unwrapped) clone of the primary repo must not remain — it would
	// still exit 128 on a cold netns. The only `git clone` occurrences should be
	// the git_retry-prefixed ones.
	for _, ln := range strings.Split(script, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "git clone") {
			t.Errorf("found unwrapped network git clone (should be git_retry git clone): %q", trimmed)
		}
	}
}

// TestBuildInitCloneContainer_CloneStormJitter asserts the clone-storm stagger
// (ka-0000000995, follow-up to ka-0000001925): when InitCloneJitterMaxSeconds is
// > 0 the init-clone script sleeps a random 0..N seconds on the COLD-clone path
// only, never on the warm fetch/checkout path, and never when disabled.
func TestBuildInitCloneContainer_CloneStormJitter(t *testing.T) {
	spec := AgentPodSpec{
		Project:   "test",
		Mode:      "crew",
		Role:      "dev",
		AgentName: "agent",
		GitURL:    "https://github.com/example/repo.git",
	}

	// Enabled: the jitter block is emitted, uses urandom entropy (so pods
	// starting in the same second differ), and honors the configured window.
	enabled := (&K8sManager{InitCloneJitterMaxSeconds: 30}).buildInitCloneContainer(spec).Command[2]
	for _, want := range []string{
		"od -An -N2 -tu2 /dev/urandom", // high-entropy per-pod source
		"_J=$(( ${_JR:-0} % 30 ))",     // window applied; :-0 guards empty read
		`sleep "$_J"`,                  // actually sleeps
		"clone-storm jitter",           // log line for observability
	} {
		if !strings.Contains(enabled, want) {
			t.Errorf("jitter enabled: missing %q:\n%s", want, enabled)
		}
	}

	// The sleep must sit on the COLD-clone path (before the network clone),
	// never on the warm already-cloned fetch path — a warm PVC pod must not pay
	// the stagger. Assert the sleep appears between "Cloning..." and the clone.
	cloningIdx := strings.Index(enabled, `echo "Cloning..."`)
	sleepIdx := strings.Index(enabled, `sleep "$_J"`)
	cloneIdx := strings.Index(enabled, "git_retry git clone")
	if cloningIdx < 0 || sleepIdx < 0 || cloneIdx < 0 {
		t.Fatalf("expected Cloning banner, sleep, and clone all present:\n%s", enabled)
	}
	// The sleep sits strictly inside the cold `else` block: after the "Cloning..."
	// banner and before the network clone. (The warm already-cloned `if` branch —
	// with its `git fetch --all --prune` — precedes the `else` in text order, so
	// the sleep landing after that fetch is exactly right: it is in the else, not
	// the if.)
	if !(cloningIdx < sleepIdx && sleepIdx < cloneIdx) {
		t.Errorf("jitter sleep must be on the cold-clone path (after %q, before the clone): cloning=%d sleep=%d clone=%d",
			`echo "Cloning..."`, cloningIdx, sleepIdx, cloneIdx)
	}

	// Disabled (0 and negative): no sleep / jitter block at all — zero behavior
	// change for clusters that opt out.
	for _, max := range []int{0, -1} {
		script := (&K8sManager{InitCloneJitterMaxSeconds: max}).buildInitCloneContainer(spec).Command[2]
		if strings.Contains(script, "clone-storm jitter") || strings.Contains(script, `sleep "$_J"`) {
			t.Errorf("jitter must be absent when InitCloneJitterMaxSeconds=%d:\n%s", max, script)
		}
	}
}
