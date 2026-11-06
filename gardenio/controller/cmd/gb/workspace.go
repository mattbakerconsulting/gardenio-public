package main

// workspace.go — gb workspace subcommands for per-bead git worktree isolation.
//
// Problem: Agents carry over git state between beads when the shared PVC
// workspace has a stale feature branch. This creates MRs that bundle
// unrelated changes.
//
// Solution: Each bead gets its own git worktree at:
//   {workspace}/../worktrees/{bead-id}/
//
// The worktree lives as a SIBLING of the primary checkout — not nested inside
// it — so the Claude Code harness (which scans memory files DOWN from the
// primary cwd) never re-discovers the worktree's byte-identical CLAUDE.md and
// double-loads it as nested_memory (bug ka-0000007049). The sibling layout also
// mirrors how reference repos are cloned (../repos/<name>, see initclone.go),
// and salvage.go already scans that sibling location for dirty WIP.
//
// Phase 1: setup, teardown, list, audit + Fields metadata, dep-chain base.
// Phase 2: sync (stacked rebase), reaper, PVC monitoring.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
)

// WorkspaceInfo is stored as a JSON string in Bead.Fields["workspace"].
type WorkspaceInfo struct {
	Branch       string `json:"branch"`
	WorktreePath string `json:"worktree_path"`
	BaseBranch   string `json:"base_branch"`
}

// worktreesDir returns the per-bead worktree parent directory: a SIBLING of the
// primary checkout at {workspace}/../worktrees. Keeping worktrees OUTSIDE the
// primary cwd stops the Claude Code harness from double-loading each worktree's
// byte-identical CLAUDE.md as nested_memory (bug ka-0000007049) and mirrors the
// sibling reference-repo layout (../repos) that salvage.go already scans.
func worktreesDir(workspace string) string {
	return filepath.Join(filepath.Dir(workspace), "worktrees")
}

// worktreePathFor returns the on-disk path of a bead's worktree.
func worktreePathFor(workspace, beadID string) string {
	return filepath.Join(worktreesDir(workspace), beadID)
}

// worktreeRelPath is the workspace-relative form stored in
// WorkspaceInfo.WorktreePath for post-restart recovery display.
func worktreeRelPath(beadID string) string {
	return filepath.Join("..", "worktrees", beadID)
}

var workspaceCmd = &cobra.Command{
	Use:     "workspace",
	Short:   "Per-bead git worktree isolation",
	GroupID: "session",
}

// ── gb workspace setup ────────────────────────────────────────────────────

var workspaceSetupCmd = &cobra.Command{
	Use:   "setup <bead-id>",
	Short: "Create a git worktree for a bead",
	Long: `Creates a git worktree at {workspace}/../worktrees/{bead-id}/ (a sibling
of the primary checkout, NOT nested inside it) with a new branch derived from
the bead title (or --branch). The base branch is auto-resolved from the dep
chain (or --base).

Workspace info is stored in Bead.Fields["workspace"] so agents can recover
the worktree path after a pod restart.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		workspace := workspaceFlagOrCwd(cmd)
		baseBranch, _ := cmd.Flags().GetString("base")
		branchName, _ := cmd.Flags().GetString("branch")
		ctx := cmd.Context()

		// Fetch bead to get title for branch derivation.
		bead, err := daemon.GetBead(ctx, beadID)
		if err != nil {
			return fmt.Errorf("fetching bead %s: %w", beadID, err)
		}

		if branchName == "" {
			branchName = deriveBranchName(bead.Title, beadID)
		}
		if baseBranch == "" {
			baseBranch = resolveBaseBranch(ctx, beadID)
		}

		worktreePath := worktreePathFor(workspace, beadID)
		relPath := worktreeRelPath(beadID)

		// Ensure the sibling worktrees/ parent dir exists — unlike the old
		// nested .beads/ path it is not created as a side effect of other
		// workspace setup, so `git worktree add` would otherwise fail.
		if err := os.MkdirAll(worktreesDir(workspace), 0o755); err != nil {
			return fmt.Errorf("creating worktrees dir: %w", err)
		}

		// Check if worktree already exists.
		if _, err := os.Stat(worktreePath); err == nil {
			fmt.Fprintf(os.Stderr, "[workspace] worktree already exists at %s\n", worktreePath)
			return nil
		}

		// Fetch latest from remote so base is current. When the base is a
		// remote ref (origin/*), a stale base is the root cause of grafted
		// branches — fail loudly rather than silently cutting from an
		// out-of-date or missing base. For a local base (stacked worktrees off
		// a parent bead's branch) a fetch failure is non-fatal.
		if out, err := exec.CommandContext(ctx, "git", "-C", workspace, "fetch", "origin").CombinedOutput(); err != nil {
			if strings.HasPrefix(baseBranch, "origin/") {
				return fmt.Errorf("git fetch origin failed but base %q requires an up-to-date remote: %s",
					baseBranch, strings.TrimSpace(string(out)))
			}
			fmt.Fprintf(os.Stderr, "[workspace] warning: git fetch: %s\n", strings.TrimSpace(string(out)))
		}

		// Create the worktree, guaranteeing the branch shares a merge-base with
		// the freshly-fetched base (see setupWorktree).
		if err := setupWorktree(ctx, workspace, worktreePath, branchName, baseBranch); err != nil {
			return err
		}

		// Store workspace info in bead fields for recovery after pod restart.
		info := WorkspaceInfo{
			Branch:       branchName,
			WorktreePath: relPath,
			BaseBranch:   baseBranch,
		}
		if err := storeWorkspaceInfo(ctx, beadID, info); err != nil {
			fmt.Fprintf(os.Stderr, "[workspace] warning: storing workspace info: %v\n", err)
		}

		fmt.Printf("[workspace] created worktree at %s\n  branch:  %s\n  base:    %s\n", worktreePath, branchName, baseBranch)
		fmt.Printf("[workspace] switch to worktree: cd %s\n", worktreePath)
		return nil
	},
}

// ── gb workspace teardown ─────────────────────────────────────────────────

var workspaceTeardownCmd = &cobra.Command{
	Use:   "teardown <bead-id>",
	Short: "Remove the git worktree for a bead",
	Long:  `Removes the git worktree and clears workspace metadata from the bead.`,
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		beadID := args[0]
		workspace := workspaceFlagOrCwd(cmd)
		ctx := cmd.Context()

		worktreePath := worktreePathFor(workspace, beadID)

		if _, err := os.Stat(worktreePath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "[workspace] no worktree found at %s\n", worktreePath)
			return nil
		}

		// Remove via git worktree remove (handles .git cleanup).
		out, err := exec.CommandContext(ctx, "git", "-C", workspace,
			"worktree", "remove", "--force", worktreePath).CombinedOutput()
		if err != nil {
			// Fallback: manual removal if git doesn't know about it.
			fmt.Fprintf(os.Stderr, "[workspace] git worktree remove: %s — falling back to rm\n", strings.TrimSpace(string(out)))
			if err2 := os.RemoveAll(worktreePath); err2 != nil {
				return fmt.Errorf("removing worktree %s: %w", worktreePath, err2)
			}
		}

		// Prune stale worktree refs.
		_ = exec.CommandContext(ctx, "git", "-C", workspace, "worktree", "prune").Run()

		// Clear workspace info from bead fields.
		if err := storeWorkspaceInfo(ctx, beadID, WorkspaceInfo{}); err != nil {
			fmt.Fprintf(os.Stderr, "[workspace] warning: clearing workspace info: %v\n", err)
		}

		fmt.Printf("[workspace] removed worktree at %s\n", worktreePath)
		return nil
	},
}

// ── gb workspace list ─────────────────────────────────────────────────────

var workspaceListCmd = &cobra.Command{
	Use:   "list",
	Short: "Show active git worktrees",
	RunE: func(cmd *cobra.Command, args []string) error {
		workspace := workspaceFlagOrCwd(cmd)
		out, err := exec.CommandContext(cmd.Context(), "git", "-C", workspace, "worktree", "list").Output()
		if err != nil {
			return fmt.Errorf("listing worktrees: %w", err)
		}
		fmt.Print(string(out))
		return nil
	},
}

// ── gb workspace audit ────────────────────────────────────────────────────

var workspaceAuditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Scan worktrees for dirty state (Phase 1: warn only)",
	Long: `Scans all per-bead worktrees under {workspace}/../worktrees/.
Warns about dirty worktrees (uncommitted changes or untracked files).
Exits 0 even when dirty — non-blocking by design for SessionStart hooks.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		workspace := workspaceFlagOrCwd(cmd)
		wtDir := worktreesDir(workspace)

		entries, err := os.ReadDir(wtDir)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // No worktrees yet — OK.
			}
			return fmt.Errorf("reading worktrees dir: %w", err)
		}

		dirty := 0
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			wtPath := filepath.Join(wtDir, e.Name())
			if isDirtyWorktree(wtPath) {
				fmt.Printf("<system-reminder>[workspace] WARNING: worktree %s has uncommitted changes (bead: %s)</system-reminder>\n",
					wtPath, e.Name())
				dirty++
			}
		}

		if dirty > 0 {
			fmt.Printf("<system-reminder>[workspace] %d worktree(s) have uncommitted changes — commit or stash before switching beads</system-reminder>\n", dirty)
		}

		return nil
	},
}

func init() {
	for _, sub := range []*cobra.Command{workspaceSetupCmd, workspaceTeardownCmd, workspaceListCmd, workspaceAuditCmd} {
		sub.Flags().String("workspace", os.Getenv("KD_WORKSPACE"), "workspace root directory")
	}
	workspaceSetupCmd.Flags().String("base", "", "base branch (default: auto-resolve from dep chain, fallback: origin/main)")
	workspaceSetupCmd.Flags().String("branch", "", "branch name (default: derived from bead title)")

	workspaceCmd.AddCommand(workspaceSetupCmd)
	workspaceCmd.AddCommand(workspaceTeardownCmd)
	workspaceCmd.AddCommand(workspaceListCmd)
	workspaceCmd.AddCommand(workspaceAuditCmd)
}

// ── helpers ───────────────────────────────────────────────────────────────

// setupWorktree creates a git worktree at worktreePath on branchName, based on
// baseBranch, and guarantees the resulting branch shares a merge-base with
// baseBranch.
//
// The happy path is `git worktree add -b <branch> <path> <base>`, which cuts a
// fresh branch off the (freshly-fetched) base — these always share history. The
// default flow hits this path and is unchanged.
//
// The hazard is the fallback path. When `-b` fails because the branch already
// exists locally, the previous code ran a naive `git worktree add <path>
// <branch>`, reusing the stale local branch verbatim. If that branch was
// grafted / has an unrelated history (no merge-base with the base), the agent
// ended up on an "unrelated-histories" branch — exactly the MR !4991 failure
// (root cause ka-0000001637 / follow-up ka-0000000926). So before reusing an
// existing local branch we verify it shares a merge-base with the base, and
// when it does not we pick the SAFEST repair:
//
//   - If the branch has NO local-only commits — every commit it carries is
//     already on a remote-tracking ref — then resetting the local ref onto the
//     base discards nothing recoverable, so we hard-reset it onto the base.
//     This is the preferred self-heal path so a routine stale/grafted branch
//     does not block the agent.
//   - If the branch DOES carry local-only commits, resetting would silently
//     discard unpushed work, so we refuse and return a clear, actionable error
//     telling the agent the branch is grafted and how to recover.
//
// Either way we NEVER silently attach a worktree to an unrelated-history
// branch. A branch that already shares history with the base is reused as
// before (a later `gb workspace sync` / rebase handles "behind base").
func setupWorktree(ctx context.Context, workspace, worktreePath, branchName, baseBranch string) error {
	// Happy path: cut a brand-new branch off the base.
	out, err := exec.CommandContext(ctx, "git", "-C", workspace,
		"worktree", "add", "-b", branchName, worktreePath, baseBranch).CombinedOutput()
	if err == nil {
		return nil
	}
	firstErr := strings.TrimSpace(string(out))

	// `-b` failed — most commonly because branchName already exists locally.
	// Before reusing it, verify it shares history with the base.
	if !gitBranchExists(ctx, workspace, branchName) {
		// The failure was not "branch exists" — surface the original error.
		return fmt.Errorf("creating worktree: %s", firstErr)
	}

	if !gitMergeBaseExists(ctx, workspace, branchName, baseBranch) {
		// Unrelated histories (grafted branch). Decide whether it is safe to
		// reset by checking for local-only commits that a reset would lose.
		unpushed, uErr := gitBranchHasLocalOnlyCommits(ctx, workspace, branchName)
		if uErr != nil {
			return fmt.Errorf("local branch %q has unrelated history with base %q and its commits could not be inspected (%v); "+
				"delete it (git branch -D %s) and re-run", branchName, baseBranch, uErr, branchName)
		}
		if unpushed {
			return fmt.Errorf("local branch %q is grafted: it shares no merge-base with base %q and carries local-only commits, "+
				"so resetting it would discard unpushed work. Inspect with 'git log %s --not --remotes', then rebase those commits "+
				"onto %s or delete the branch (git branch -D %s) and re-run", branchName, baseBranch, branchName, baseBranch, branchName)
		}
		// No local-only commits — safe to re-point the stale branch at the base
		// so the worktree starts from a branch that shares history with main.
		if rOut, rErr := exec.CommandContext(ctx, "git", "-C", workspace,
			"branch", "-f", branchName, baseBranch).CombinedOutput(); rErr != nil {
			return fmt.Errorf("resetting grafted branch %q onto %q: %s", branchName, baseBranch, strings.TrimSpace(string(rOut)))
		}
		fmt.Fprintf(os.Stderr, "[workspace] reset stale grafted branch %s onto %s (no local-only commits to preserve)\n", branchName, baseBranch)
	}

	// Branch now shares history with the base (either it always did, or we just
	// reset it) — attach the worktree to it.
	out2, err2 := exec.CommandContext(ctx, "git", "-C", workspace,
		"worktree", "add", worktreePath, branchName).CombinedOutput()
	if err2 != nil {
		return fmt.Errorf("creating worktree: %s\n%s", firstErr, strings.TrimSpace(string(out2)))
	}
	return nil
}

// gitBranchExists reports whether branchName is a local branch in workspace.
func gitBranchExists(ctx context.Context, workspace, branchName string) bool {
	err := exec.CommandContext(ctx, "git", "-C", workspace,
		"show-ref", "--verify", "--quiet", "refs/heads/"+branchName).Run()
	return err == nil
}

// gitMergeBaseExists reports whether branchName and baseBranch share a common
// ancestor (i.e. `git merge-base` prints a commit). Empty output / non-zero
// exit means unrelated histories.
func gitMergeBaseExists(ctx context.Context, workspace, branchName, baseBranch string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", workspace,
		"merge-base", branchName, baseBranch).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}

// gitBranchHasLocalOnlyCommits reports whether branchName has any commits that
// are not reachable from any remote-tracking ref
// (`git rev-list --count <branch> --not --remotes`). A non-zero count means the
// branch carries unpushed work that a reset-to-base would discard.
func gitBranchHasLocalOnlyCommits(ctx context.Context, workspace, branchName string) (bool, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", workspace,
		"rev-list", "--count", branchName, "--not", "--remotes").Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) != "0", nil
}

var issueRe = regexp.MustCompile(`\[([A-Z]+-\d+)\]`)

// deriveBranchName returns a branch name like "fix/pe-6916" from the bead title.
// Falls back to "fix/{bead-id}" if no issue number is found.
//
// When falling back to the bead ID, the ID's case is preserved: bead IDs are
// case-sensitive (e.g. ka-AbCdEf and ka-abcdef are distinct beads), so the
// gitlab-sync branch→bead resolver must be able to recover the exact ID from
// the source branch. Lowercasing here previously orphaned MRs whose bead ID
// contained uppercase letters. See fix ka-0000004161 / root-cause ka-0000003609.
func deriveBranchName(title, beadID string) string {
	if m := issueRe.FindStringSubmatch(title); len(m) >= 2 {
		return "fix/" + strings.ToLower(m[1])
	}
	// Sanitize bead ID to a valid branch name component. Preserve case — the
	// bead ID round-trips through the branch name and must match exactly.
	safe := strings.ReplaceAll(beadID, " ", "-")
	return "fix/" + safe
}

// resolveBaseBranch walks the direct dependencies of beadID looking for a
// parent that already has workspace metadata. Returns that parent's branch
// so stacked worktrees are based on each other. Falls back to "origin/main".
func resolveBaseBranch(ctx context.Context, beadID string) string {
	deps, err := daemon.GetDependencies(ctx, beadID)
	if err != nil || len(deps) == 0 {
		return "origin/main"
	}
	for _, dep := range deps {
		parent, err := daemon.GetBead(ctx, dep.DependsOnID)
		if err != nil {
			continue
		}
		if wsStr, ok := parent.Fields["workspace"]; ok && wsStr != "" {
			var ws WorkspaceInfo
			if err := json.Unmarshal([]byte(wsStr), &ws); err == nil && ws.Branch != "" {
				fmt.Fprintf(os.Stderr, "[workspace] resolved base from dep %s: %s\n", dep.DependsOnID, ws.Branch)
				return ws.Branch
			}
		}
	}
	return "origin/main"
}

// storeWorkspaceInfo writes WorkspaceInfo (as a JSON string) into Bead.Fields["workspace"].
// An empty WorkspaceInfo clears the field.
func storeWorkspaceInfo(ctx context.Context, beadID string, info WorkspaceInfo) error {
	var value string
	if info.Branch != "" {
		b, err := json.Marshal(info)
		if err != nil {
			return err
		}
		value = string(b)
	}
	return daemon.UpdateBeadFields(ctx, beadID, map[string]string{"workspace": value})
}

// workspaceFlagOrCwd returns the --workspace flag value or the current directory.
func workspaceFlagOrCwd(cmd *cobra.Command) string {
	if ws, _ := cmd.Flags().GetString("workspace"); ws != "" {
		return ws
	}
	cwd, _ := os.Getwd()
	return cwd
}

// isDirtyWorktree returns true if the given worktree path has uncommitted
// changes or untracked files.
func isDirtyWorktree(path string) bool {
	out, err := exec.Command("git", "-C", path, "status", "--porcelain").Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}
