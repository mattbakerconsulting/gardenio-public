package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

var setupReposCmd = &cobra.Command{
	Use:   "repos",
	Short: "Clone project repos into the workspace",
	Long: `Queries the project bead for the current BOAT_PROJECT and clones its
repos into {workspace}/repos/{name}.

The project bead's "repos" field is the source of truth. Each repo entry
has a url, optional name, optional branch, and a role (primary or reference).
All repos are cloned; the primary repo is also available at repos/{project}.

Repos that already exist (have a .git directory) are skipped.
Uses blobless partial clone (--filter=blob:none) for speed. Requires git credentials
to be configured (gb agent start or entrypoint handles this).`,
	RunE: func(cmd *cobra.Command, args []string) error {
		workspace, _ := cmd.Flags().GetString("workspace")
		if workspace == "" {
			workspace, _ = os.Getwd()
		}
		return runSetupRepos(cmd.Context(), workspace)
	},
}

func init() {
	setupReposCmd.Flags().String("workspace", os.Getenv("KD_WORKSPACE"), "workspace directory")
	setupCmd.AddCommand(setupReposCmd)
}

// repoCloneEntry is a resolved repo ready to clone.
type repoCloneEntry struct {
	Name string
	URL  string
}

// repoNameFromURL extracts the repository name from a URL.
// "https://github.com/org/my-repo.git" → "my-repo"
func repoNameFromURL(rawURL string) string {
	u := strings.TrimSuffix(rawURL, ".git")
	parts := strings.Split(u, "/")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return "repo"
}

// resolveReposFromProjectBead queries the daemon for the project bead and
// returns the repos to clone. Returns nil if no project bead is found.
func resolveReposFromProjectBead(ctx context.Context, project string) []repoCloneEntry {
	if daemon == nil {
		return nil
	}

	projects, err := daemon.ListProjectBeads(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[setup] warning: listing project beads: %v\n", err)
		return nil
	}

	info, ok := projects[project]
	if !ok {
		fmt.Fprintf(os.Stderr, "[setup] no project bead found for %q\n", project)
		return nil
	}

	var entries []repoCloneEntry

	if len(info.Repos) > 0 {
		// Multi-repo project: use the repos field.
		for _, r := range info.Repos {
			name := r.Name
			if name == "" {
				if r.Role == "primary" {
					name = project
				} else {
					name = repoNameFromURL(r.URL)
				}
			}
			entries = append(entries, repoCloneEntry{Name: name, URL: r.URL})
		}
	} else if info.GitURL != "" {
		// Legacy single-repo project: use git_url field.
		entries = append(entries, repoCloneEntry{Name: project, URL: info.GitURL})
	}

	return entries
}

// parseBoatProjectsEnv parses BOAT_PROJECTS as a fallback when no project
// beads exist. Format: "name=https://host/path:prefix,..."
func parseBoatProjectsEnv(raw string) []repoCloneEntry {
	var entries []repoCloneEntry
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		eqIdx := strings.Index(part, "=")
		if eqIdx < 0 {
			continue
		}
		name := part[:eqIdx]
		rawURL := part[eqIdx+1:]
		// Strip trailing :prefix (e.g. ":PE") if present after the URL.
		if lastColon := strings.LastIndex(rawURL, ":"); lastColon > strings.Index(rawURL, "//") {
			rawURL = rawURL[:lastColon]
		}
		if name != "" && rawURL != "" {
			entries = append(entries, repoCloneEntry{Name: name, URL: rawURL})
		}
	}
	return entries
}

func runSetupRepos(ctx context.Context, workspace string) error {
	project := resolveProject("")
	if project == "" {
		fmt.Fprintf(os.Stderr, "[setup] no BOAT_PROJECT set — nothing to clone\n")
		return nil
	}

	// init-clone already ran? Then skip the entire workspace re-clone.
	//
	// On init-clone-backed pods the init-clone init container has ALREADY
	// materialized the primary repo at /home/agent/bot/{project}/work and every
	// reference repo at /home/agent/bot/{project}/repos/{name} BEFORE this
	// (agent-container) code runs. Re-cloning those same repos into
	// {workspace}/repos here is pure redundancy:
	//   - the {workspace}/repos/{primary} clone is never consumed — the agent
	//     cwd is /home/agent/bot/{project}/work (resolveCoopWorkdir) and
	//     reference repos are read from /home/agent/bot/{project}/repos as
	//     `../repos` siblings (nudge_prompt.go), not from {workspace}/repos;
	//   - worse, the primary re-clone is a bare network `git clone` of a
	//     multi-GB repo (~1.2–2.7G for gardenio/monorepo). It runs BLOCKING,
	//     inside gb init, BEFORE coop starts — i.e. on coop's 300s StartupProbe
	//     critical path. On a slow- or contended-egress cluster (a fresh prod
	//     cluster, or during a multi-pod clone-storm) it cannot finish in 300s,
	//     so kubelet kills the container mid-clone and the pod crash-loops
	//     forever, never registering with coopmux (ka-0000006474, ka-0000000649).
	//     The earlier --reference-if-able fix (ka-0000006474) only accelerated
	//     REFERENCE repos: its cache probe (initCloneCacheDir) looks under
	//     .../repos/{name}, which never exists for the PRIMARY (init-clone puts
	//     the primary at .../work), so the primary still fell to a full network
	//     clone. This guard removes that whole redundant phase.
	//
	// Guard on the PRIMARY checkout (.../work/.git), NOT on .../repos: init-clone
	// only creates .../repos inside its reference-repo loop, so a single-repo /
	// primary-only project has NO .../repos dir even though init-clone ran and
	// materialized the (possibly multi-GB) primary. Mirror resolveCoopWorkdir's
	// exact probe so both agree on "init-clone gave us the primary".
	if workDir := initCloneWorkDir(project); workDir != "" {
		if info, err := os.Stat(filepath.Join(workDir, ".git")); err == nil && info.IsDir() {
			fmt.Fprintf(os.Stderr, "[setup] init-clone already materialized the workspace at %s — skipping redundant workspace re-clone\n", workDir)
			return nil
		}
	}

	// Primary source of truth: the project bead.
	entries := resolveReposFromProjectBead(ctx, project)

	// Fallback: parse BOAT_PROJECTS env var when no project bead exists.
	if len(entries) == 0 {
		if raw := os.Getenv("BOAT_PROJECTS"); raw != "" {
			fmt.Fprintf(os.Stderr, "[setup] no project bead repos — falling back to BOAT_PROJECTS env\n")
			entries = parseBoatProjectsEnv(raw)
		}
	}

	if len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "[setup] no repos to clone for project %q\n", project)
		return nil
	}

	reposDir := filepath.Join(workspace, "repos")
	if err := os.MkdirAll(reposDir, 0o755); err != nil {
		return fmt.Errorf("creating repos dir: %w", err)
	}

	var cloned, skipped, failed int
	for _, e := range entries {
		dest := filepath.Join(reposDir, e.Name)
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			fmt.Fprintf(os.Stderr, "[setup] repo already present: %s (skipping)\n", dest)
			skipped++
			continue
		}

		if err := cloneRepoWithRetry(ctx, e, project, dest); err != nil {
			fmt.Fprintf(os.Stderr, "[setup] WARNING: clone failed for %s: %v\n", e.URL, err)
			failed++
			continue
		}
		cloned++
	}

	fmt.Fprintf(os.Stderr, "[setup] repos: %d cloned, %d skipped, %d failed\n", cloned, skipped, failed)
	if failed > 0 {
		return fmt.Errorf("%d repo(s) failed to clone", failed)
	}
	return nil
}

// initCloneWorkDir is where the init-clone init container clones the PRIMARY
// project repo: /home/agent/bot/{project}/work. Its existence (checked via
// .git) is the canonical "init-clone ran and gave us the primary" signal —
// mirroring resolveCoopWorkdir, which picks the same path as the agent cwd.
// Returns "" when project is empty. NOTE this is deliberately the .../work
// sibling, not .../repos/{name}: init-clone only creates .../repos in its
// reference-repo loop, so gating on .../repos would miss single-repo projects.
func initCloneWorkDir(project string) string {
	if project == "" {
		return ""
	}
	return filepath.Join("/home/agent/bot", project, "work")
}

// initCloneCacheDir is where the init-clone init container pre-clones project
// repos on PVC-backed pods: /home/agent/bot/{project}/repos/{name}. The agent
// container later re-clones into {workspace}/repos/{name}; serving that clone
// from this local cache (via git clone --reference) avoids re-downloading
// multi-GB repos over the network — the redundant network re-clone could
// exceed the pod's 300s StartupProbe budget and crash-loop the pod before coop
// ever started (ka-0000006474). Returns "" when no usable cache exists.
func initCloneCacheDir(project, name string) string {
	if project == "" || name == "" {
		return ""
	}
	cache := filepath.Join("/home/agent/bot", project, "repos", name)
	if info, err := os.Stat(filepath.Join(cache, ".git")); err == nil && info.IsDir() {
		return cache
	}
	return ""
}

// cloneRepoWithRetry clones e into dest with a bounded retry/backoff. When the
// init-clone cache for this repo exists on the local PVC, it is used as a
// --reference source with --dissociate so git copies objects from local disk
// instead of re-fetching them from the remote; the resulting clone is
// standalone (no dependency on the cache path surviving). A bare network clone
// is used as the fallback when no cache is present.
func cloneRepoWithRetry(ctx context.Context, e repoCloneEntry, project, dest string) error {
	args := []string{"clone", "--filter=blob:none", "--quiet"}
	if cache := initCloneCacheDir(project, e.Name); cache != "" {
		fmt.Fprintf(os.Stderr, "[setup] cloning %s → %s (via local reference %s)\n", e.URL, dest, cache)
		args = append(args, "--reference-if-able", cache, "--dissociate")
	} else {
		fmt.Fprintf(os.Stderr, "[setup] cloning %s → %s\n", e.URL, dest)
	}
	args = append(args, e.URL, dest)

	const maxAttempts = 5
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		out, err := exec.CommandContext(ctx, "git", args...).CombinedOutput()
		if err == nil {
			return nil
		}
		lastErr = fmt.Errorf("attempt %d/%d: %s", attempt, maxAttempts, strings.TrimSpace(string(out)))
		if ctx.Err() != nil {
			return lastErr
		}
		// A failed clone leaves dest in a half-written state that blocks the
		// retry; remove it before trying again.
		_ = os.RemoveAll(dest)
		if attempt < maxAttempts {
			fmt.Fprintf(os.Stderr, "[setup] clone of %s failed (%v); retrying in 5s\n", e.URL, lastErr)
			select {
			case <-ctx.Done():
				return lastErr
			case <-time.After(5 * time.Second):
			}
		}
	}
	return lastErr
}
