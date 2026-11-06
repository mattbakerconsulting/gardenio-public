package podmanager

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// shellQuote wraps s in single quotes, escaping any embedded single quotes.
// This prevents shell injection when interpolating values into shell scripts.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// hostFromHTTPSURL extracts the host component from an https:// URL, mirroring
// the manual parsing in the agent-side repoHosts() helper
// (cmd/gb/agent_k8s_workspace.go). It returns "" for empty or non-https inputs.
// Any port is retained (e.g. "gitlab.example.com:8443") so the emitted
// credential entry matches the URL git will actually authenticate against.
func hostFromHTTPSURL(rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	const scheme = "https://"
	if !strings.HasPrefix(rawURL, scheme) {
		return ""
	}
	host := strings.SplitN(rawURL[len(scheme):], "/", 2)[0]
	return host
}

// gitlabCredentialHosts returns the distinct, deterministically-ordered set of
// GitLab hosts the pod spec's repos reference (the primary GitURL plus any
// reference repos), for which a `https://oauth2:<token>@<host>` line must be
// written into the init-clone credential store.
//
// It mirrors the agent-side repoHosts() helper, deriving hosts from each
// https:// repo URL instead of hardcoding gitlab.com — which previously broke
// the FIRST clone of a private repo on any other GitLab host (self-hosted /
// gitlab.example.com), since no credential entry existed for it. github.com is
// excluded because the github credential block writes its own line. If no host
// can be derived (no GitURL, no reference repos, or unparseable URLs), it falls
// back to the single host "gitlab.com" so existing gitlab.com-only projects
// keep working with no behavioral change (ka-0000000552).
func gitlabCredentialHosts(spec AgentPodSpec) []string {
	seen := map[string]bool{}
	add := func(rawURL string) {
		h := hostFromHTTPSURL(rawURL)
		if h == "" || h == "github.com" {
			return
		}
		seen[h] = true
	}
	add(spec.GitURL)
	for _, ref := range spec.ReferenceRepos {
		add(ref.URL)
	}
	if len(seen) == 0 {
		return []string{"gitlab.com"}
	}
	hosts := make([]string, 0, len(seen))
	for h := range seen {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	return hosts
}

// buildInitCloneContainer creates an init container that clones the project's repo
// into the workspace from GitURL, plus any reference repos.
// Returns nil if no clone source is configured.
//
// The container is admissible under PSA "restricted":
//   - runs as non-root (AgentUID/AgentGID, matching the agent container so
//     files written to the shared workspace volume are readable);
//   - drops ALL capabilities;
//   - allowPrivilegeEscalation=false;
//   - seccompProfile=RuntimeDefault.
//
// It relies on the init-clone image having git already installed (no `apk
// add` at runtime) and on the pod-level FSGroup recursively chgrp'ing the
// workspace volume so the non-root UID can write to it.
func (m *K8sManager) buildInitCloneContainer(spec AgentPodSpec) *corev1.Container {
	if spec.GitURL == "" && len(spec.ReferenceRepos) == 0 {
		return nil
	}

	branch := spec.GitDefaultBranch
	if branch == "" {
		branch = "main"
	}

	// Disable git-lfs smudge and hooks. The init container doesn't have
	// git-lfs installed (not needed for code — only large binary assets),
	// and the post-checkout hook in repos with LFS configured exits 1 when
	// git-lfs is missing, breaking the init container with set -e.
	script := "set -e\nexport GIT_LFS_SKIP_SMUDGE=1\n"

	// Cold-CNI-netns race (ka-0000005535): on a freshly-created pod — notably
	// the freeze/thaw placeholder Pod B, which keeps this run-to-completion
	// init-clone container so its workspace is rehydrated before the graft —
	// the kubelet may start init-clone before the AWS VPC CNI has finished
	// route-programming the pod's netns. The first network git operation then
	// hits an unrouted netns, exits 128, and (under set -e) the pod enters
	// terminal phase Failed before Ready — fail-closing the thaw BEFORE the
	// barrier restore ever runs. git_retry wraps the network git operations
	// with bounded backoff (~60s) so a cold netns self-heals within the window
	// the CNI takes to program routes; a genuine auth/URL/ref error still
	// exhausts the budget and fails terminally, preserving the old behavior.
	script += `git_retry() {
  _n=0
  _max=12
  until "$@"; do
    _rc=$?
    _n=$((_n+1))
    if [ "$_n" -ge "$_max" ]; then
      echo "git_retry: '$*' still failing after $_n attempts (rc=$_rc); giving up" >&2
      return "$_rc"
    fi
    echo "git_retry: '$*' failed (rc=$_rc), attempt $_n/$_max; retrying in 5s (waiting for CNI netns to be route-programmed)..." >&2
    sleep 5
  done
}
`

	// Credential helper setup (when configured) must come before any git
	// operation that may need to authenticate to a private remote.
	//
	// Credentials are read from the projected /etc/git-creds/ mount instead
	// of GIT_USERNAME / GIT_TOKEN / GITLAB_TOKEN env vars. Env-var-based
	// secrets leak through `ps eww`, `kubectl describe pod`, and admin log
	// dumps; file-mounted secrets do not. See W6 (ka-0000002787).
	//
	// The helper file (/tmp/.git-credentials) is the standard format
	// consumed by `git credential-store --file=...`. It lives on the tmpfs
	// /tmp inside the container — a side-effect of materialization, not a
	// new disclosure surface.
	//
	// `cat` and `printf` are busybox builtins (alpine/git), so secret
	// values never appear in `ps` argv listings during construction.
	if spec.GitCredentialsSecret != "" || spec.GitlabTokenSecret != "" {
		// Derive the GitLab host(s) the project's repos actually reference and
		// emit one credential line per host, instead of hardcoding gitlab.com.
		// Hardcoding broke the FIRST clone of a private repo on any other
		// GitLab host (self-hosted, gitlab.example.com, …): no credential entry
		// existed for it. This mirrors the agent-side repoHosts() helper in
		// cmd/gb/agent_k8s_workspace.go, which already derives hosts from the
		// project's repo URLs. github.com is handled by the github block below;
		// when no GitLab host can be derived we fall back to gitlab.com so
		// existing gitlab.com-only projects keep working (ka-0000000552).
		var glHostLines, ghFallbackHostLines strings.Builder
		for _, h := range gitlabCredentialHosts(spec) {
			// Pass the host as a printf ARGUMENT (not interpolated into the
			// format string) and shell-quote it, so a host carrying '%' or a
			// quote can neither corrupt the printf format nor inject shell.
			qh := shellQuote(h)
			glHostLines.WriteString("  printf 'https://oauth2:%s@%s\\n' \"$GL_TOKEN\" " + qh + " >> /tmp/.git-credentials\n")
			ghFallbackHostLines.WriteString("  printf 'https://%s:%s@%s\\n' \"$GH_USER\" \"$GH_TOKEN\" " + qh + " >> /tmp/.git-credentials\n")
		}

		// Kubernetes Secret files have no trailing newline, so `read -r`
		// exits non-zero on EOF — which under `set -e` kills the script.
		// Use $(cat …) to capture the full file contents regardless of
		// terminator. `cat` is a busybox builtin in alpine/git, so secret
		// values still never appear in `ps` argv listings.
		script += `# Configure git credentials for private repos (W6: read from
# /etc/git-creds/ instead of env vars to avoid process-listing leaks).
git config --global credential.helper 'store --file=/tmp/.git-credentials'
: > /tmp/.git-credentials
chmod 0600 /tmp/.git-credentials
GH_USER=""
GH_TOKEN=""
if [ -r /etc/git-creds/github/username ] && [ -r /etc/git-creds/github/token ]; then
  GH_USER=$(cat /etc/git-creds/github/username)
  GH_TOKEN=$(cat /etc/git-creds/github/token)
  printf 'https://%s:%s@github.com\n' "$GH_USER" "$GH_TOKEN" >> /tmp/.git-credentials
fi
if [ -r /etc/git-creds/gitlab/token ]; then
  GL_TOKEN=$(cat /etc/git-creds/gitlab/token)
` + glHostLines.String() + `  unset GL_TOKEN
elif [ -n "$GH_USER" ] && [ -n "$GH_TOKEN" ]; then
` + ghFallbackHostLines.String() + `fi
unset GH_USER GH_TOKEN
`
	}

	// Clone-storm stagger (ka-0000000995, follow-up to ka-0000001925): before a
	// COLD network clone, sleep a random 0..N seconds so a batch of pods
	// scheduled together does not all saturate NAT egress at the same instant.
	// This runs in the init container BEFORE coop starts, so it never consumes
	// the agent StartupProbe budget, and is emitted only on the cold-clone path
	// (a warm PVC that just fetches skips it). od reads 2 bytes of /dev/urandom
	// for per-pod entropy so pods starting in the same second still get distinct
	// delays; 'od'/'tr'/'sleep' are busybox builtins in the alpine/git image.
	// The read is defended with a :-0 fallback so a (theoretical) empty read
	// cannot make the `$(( ))` arithmetic fail under `set -e`. <= 0 disables.
	jitterSnippet := ""
	if m.InitCloneJitterMaxSeconds > 0 {
		jitterSnippet = fmt.Sprintf(`  _JR=$(od -An -N2 -tu2 /dev/urandom | tr -d ' ')
  _J=$(( ${_JR:-0} %% %d ))
  echo "init-clone: clone-storm jitter — sleeping ${_J}s before cold clone (max %ds)" >&2
  sleep "$_J"
`, m.InitCloneJitterMaxSeconds, m.InitCloneJitterMaxSeconds)
	}

	// Clone primary repo if configured.
	// All user-controlled values are shell-quoted to prevent injection.
	if spec.GitURL != "" {
		qProject := shellQuote(spec.Project)
		qBranch := shellQuote(branch)
		qGitURL := shellQuote(spec.GitURL)
		qAgent := shellQuote(spec.AgentName)
		qAgentEmail := shellQuote(spec.AgentName + "@gardenio")

		script += fmt.Sprintf(`git config --global --add safe.directory %s/%s/work
WORK_DIR=%s/%s/work
if [ -d "$WORK_DIR/.git" ]; then
  echo "Repo already cloned, fetching updates..."
  cd "$WORK_DIR"
  if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
    echo "WARNING: PVC has uncommitted changes from previous agent run, stashing..."
    git stash push -m "init-clone: auto-stash from previous agent $(date -u +%%Y-%%m-%%dT%%H:%%M:%%SZ)" --include-untracked || {
      echo "WARNING: git stash failed, discarding changes to unblock checkout"
      git checkout -- . || true
      git clean -fd || true
    }
  fi
  # One-time shallow->blobless PVC migration (ka-0000006480): a workspace cloned
  # by an older controller is shallow (--depth 1 --single-branch --no-tags), so
  # git merge-base / main-vs-tag / ancestry checks return "no common history"
  # even though fresh pods now get a blobless partial clone (--filter=blob:none).
  # If this PVC's repo is still shallow, upgrade it IN PLACE to blobless: mark the
  # origin as a promisor with a blob:none filter, then --unshallow to backfill the
  # full commit+tree history and all tags on demand. Idempotent: once the repo is
  # deep, --is-shallow-repository is false and this whole block is skipped, so
  # subsequent boots pay nothing. Benchmarked ~2s (one-time per PVC).
  if [ "$(git rev-parse --is-shallow-repository 2>/dev/null)" = "true" ]; then
    echo "Shallow PVC detected; upgrading in place to a blobless partial clone..."
    git config remote.origin.promisor true
    git config remote.origin.partialclonefilter blob:none
    git_retry git fetch --unshallow --filter=blob:none --tags
  fi
  git_retry git fetch --all --prune
  git -c core.hooksPath=/dev/null checkout %s
  git pull --ff-only || true
else
  echo "Cloning..."
%s  mkdir -p "$(dirname "$WORK_DIR")"
  # Blobless (NOT shallow) clone: the primary repo's full history (1.5G / 10k
  # commits on the monorepo) dominated cold-start admit (~95s of a ~200s
  # create->Ready) -- almost all of it downloading file *contents* (blobs) for
  # historical commits the agent never reads. --filter=blob:none downloads the
  # full commit + tree graph (all branches, all tags -- cheap: ~75-90M) but
  # defers blob download until a blob is actually needed, at which point git
  # lazily fetches it on demand. This keeps cold-start fast WITHOUT the sharp
  # edges of a --depth 1 shallow clone: git log sees the whole history,
  # git merge-base / rebase against any branch works, all tags are present, and
  # git status / git describe behave normally. A shallow clone (--depth 1)
  # broke all of those and forced agents to run git fetch --unshallow; a
  # blobless clone needs no such escape hatch. is-shallow stays false.
  git_retry git clone --filter=blob:none -b %s %s "$WORK_DIR"
  cd "$WORK_DIR"
fi
`, MountWorkspace, qProject, MountWorkspace, qProject, qBranch, jitterSnippet, qBranch, qGitURL)

		// Configure git identity from agent env vars. Written to the cloned
		// repo's local .git/config (no --global) so it persists on the
		// workspace volume for the agent container to use.
		script += fmt.Sprintf("git config user.name %s\ngit config user.email %s\n", qAgent, qAgentEmail)
	}

	// Clone reference repos.
	// All user-controlled values are shell-quoted to prevent injection.
	for _, ref := range spec.ReferenceRepos {
		refBranch := ref.Branch
		if refBranch == "" {
			refBranch = "main"
		}
		qRefDir := shellQuote(fmt.Sprintf("%s/%s/repos/%s", MountWorkspace, spec.Project, ref.Name))
		qRefBranch := shellQuote(refBranch)
		qRefName := shellQuote(ref.Name)
		qRefURL := shellQuote(ref.URL)
		script += fmt.Sprintf(`
REFDIR=%s
if [ -d "$REFDIR/.git" ]; then
  echo "Reference repo" %s "already present, updating..."
  cd "$REFDIR"
  if [ -n "$(git status --porcelain 2>/dev/null)" ]; then
    echo "WARNING: Reference repo has uncommitted changes, discarding..."
    # Best-effort cleanup: never fatal. git clean exits non-zero when it cannot
    # unlink a path (e.g. a root-owned or mode-500 untracked dir left by a prior
    # agent), and under set -e that killed init-clone for good -- the workspace
    # emptyDir outlives container restarts, so restartPolicy: Always re-ran
    # init-clone against the same tree and it died at this same line every
    # time. Two pods crashlooped that way for 24h and 7d, holding
    # their triggers' max_concurrent lease open and leaving them silently dark
    # (ka-0000005271). If the tree is still dirty the fetch/checkout/pull below
    # is already "|| true", so we proceed with a stale reference repo rather
    # than never starting at all.
    git checkout -- . 2>/dev/null || true
    git clean -fd 2>/dev/null || true
  fi
  git_retry git fetch --all --prune && git -c core.hooksPath=/dev/null checkout %s && git pull --ff-only || true
else
  echo "Cloning reference repo" %s "..."
  mkdir -p "$(dirname "$REFDIR")"
  # Blobless (NOT shallow) clone -- see the primary-clone comment above for the
  # rationale. --filter=blob:none keeps the initial fetch small while leaving
  # full history + all tags + all branches available for on-demand blob fetch,
  # so cross-branch diffs and git log in a reference repo behave normally.
  git_retry git clone --filter=blob:none -b %s %s "$REFDIR"
fi
`, qRefDir, qRefName, qRefBranch, qRefName, qRefBranch, qRefURL)
	}

	// Build env vars for the init container.
	// HOME=/tmp lets `git config --global` (writes to ~/.gitconfig) work
	// when the container image lacks an /etc/passwd entry for AgentUID.
	//
	// NOTE: We deliberately do NOT inject GIT_USERNAME / GIT_TOKEN /
	// GITLAB_TOKEN as env vars. Env-var secrets are readable from
	// /proc/<pid>/environ, `ps eww`, and `kubectl describe pod`, which is
	// the leak path W6 closes. Credentials are projected as files under
	// /etc/git-creds/ (see buildVolumes) and consumed by the init script
	// above. See ka-0000002787.
	initEnv := []corev1.EnvVar{
		{Name: "HOME", Value: "/tmp"},
	}

	// Mount the projected git-creds volume (when configured) so the init
	// script can read username/token from files instead of env vars.
	initMounts := []corev1.VolumeMount{
		{Name: VolumeWorkspace, MountPath: MountWorkspace},
	}
	if spec.GitCredentialsSecret != "" || spec.GitlabTokenSecret != "" {
		initMounts = append(initMounts, corev1.VolumeMount{
			Name:      VolumeGitCreds,
			MountPath: MountGitCreds,
			ReadOnly:  true,
		})
	}

	image := InitCloneImage
	if m.InitCloneImageOverride != "" {
		image = m.InitCloneImageOverride
	}

	uid := AgentUID
	gid := AgentGID
	return &corev1.Container{
		Name:            InitCloneName,
		Image:           image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		Command:         []string{"/bin/sh", "-c", script},
		Env:             initEnv,
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                &uid,
			RunAsGroup:               &gid,
			RunAsNonRoot:             boolPtr(true),
			AllowPrivilegeEscalation: boolPtr(false),
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
			},
			SeccompProfile: &corev1.SeccompProfile{
				Type: corev1.SeccompProfileTypeRuntimeDefault,
			},
		},
		VolumeMounts: initMounts,
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("128Mi"),
			},
			Limits: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("500m"),
				corev1.ResourceMemory: resource.MustParse("512Mi"),
			},
		},
	}
}
