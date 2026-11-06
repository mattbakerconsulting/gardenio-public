package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitRun runs a git command in dir with a deterministic author/committer
// identity and fails the test on error.
func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test",
		"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}

// gitOut runs a git command in dir and returns trimmed stdout (ignoring error,
// for read-only inspection like merge-base / rev-parse).
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// makeOrphanBranch creates branchName as an orphan commit (unrelated history,
// no merge-base with main) carrying a single distinct file, then returns the
// working tree to main.
func makeOrphanBranch(t *testing.T, work, branchName, fileName string) {
	t.Helper()
	gitRun(t, work, "checkout", "--orphan", branchName)
	gitRun(t, work, "rm", "-rf", ".")
	if err := os.WriteFile(filepath.Join(work, fileName), []byte("orphan\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", fileName)
	gitRun(t, work, "commit", "-m", "orphan root for "+branchName)
	gitRun(t, work, "checkout", "main")
}

// TestSetupWorktree_HappyPathNewBranch verifies the unchanged default flow: a
// brand-new branch is cut off the base and shares history with it.
func TestSetupWorktree_HappyPathNewBranch(t *testing.T) {
	work, _ := initGitRepo(t)
	ctx := context.Background()

	wt := filepath.Join(t.TempDir(), "wt-new")
	if err := setupWorktree(ctx, work, wt, "fix/brand-new", "origin/main"); err != nil {
		t.Fatalf("setupWorktree: %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
	if mb := gitOut(t, work, "merge-base", "fix/brand-new", "origin/main"); mb == "" {
		t.Fatalf("new branch must share a merge-base with origin/main, got none")
	}
}

// TestSetupWorktree_GraftedNoLocalCommitsResets is the core MR !4991 scenario:
// an existing local branch with unrelated history but NO local-only commits is
// reset onto the base (not silently grafted), so the worktree shares history
// with main.
func TestSetupWorktree_GraftedNoLocalCommitsResets(t *testing.T) {
	work, _ := initGitRepo(t)
	ctx := context.Background()

	// Orphan branch, then push it so all its commits live on a remote ref —
	// i.e. nothing local-only that a reset would discard.
	makeOrphanBranch(t, work, "fix/grafted", "orphan.txt")
	gitRun(t, work, "push", "origin", "fix/grafted")
	gitRun(t, work, "fetch", "origin")

	// Precondition: unrelated histories.
	if mb := gitOut(t, work, "merge-base", "fix/grafted", "origin/main"); mb != "" {
		t.Fatalf("test setup wrong: expected no merge-base, got %q", mb)
	}

	wt := filepath.Join(t.TempDir(), "wt-grafted")
	if err := setupWorktree(ctx, work, wt, "fix/grafted", "origin/main"); err != nil {
		t.Fatalf("setupWorktree should have reset the grafted branch, got error: %v", err)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("worktree not created: %v", err)
	}
	// The branch must now share history with the base.
	if mb := gitOut(t, work, "merge-base", "fix/grafted", "origin/main"); mb == "" {
		t.Fatalf("branch was NOT reset onto base — still unrelated history (silent graft)")
	}
	// And it must point at the base tip (reset, not merge).
	base := gitOut(t, work, "rev-parse", "origin/main")
	tip := gitOut(t, work, "rev-parse", "fix/grafted")
	if base != tip {
		t.Fatalf("branch tip %q != base %q after reset", tip, base)
	}
}

// TestSetupWorktree_GraftedWithLocalCommitsErrors verifies the safety guard:
// a grafted branch carrying unpushed (local-only) commits is NOT silently
// reused and NOT reset (which would lose work) — setupWorktree errors clearly.
func TestSetupWorktree_GraftedWithLocalCommitsErrors(t *testing.T) {
	work, _ := initGitRepo(t)
	ctx := context.Background()

	// Orphan branch left local — its commit is reachable from no remote ref.
	makeOrphanBranch(t, work, "fix/grafted-local", "orphan.txt")

	// Precondition: unrelated histories and a local-only commit.
	if mb := gitOut(t, work, "merge-base", "fix/grafted-local", "origin/main"); mb != "" {
		t.Fatalf("test setup wrong: expected no merge-base, got %q", mb)
	}

	wt := filepath.Join(t.TempDir(), "wt-grafted-local")
	err := setupWorktree(ctx, work, wt, "fix/grafted-local", "origin/main")
	if err == nil {
		t.Fatalf("expected error for grafted branch with local-only commits, got nil")
	}
	if !strings.Contains(err.Error(), "grafted") {
		t.Fatalf("error should explain the branch is grafted, got: %v", err)
	}
	// Must NOT have silently created the worktree.
	if _, statErr := os.Stat(wt); statErr == nil {
		t.Fatalf("worktree should not be created when refusing a grafted branch")
	}
	// Must NOT have discarded the unpushed commit (branch still orphan).
	if mb := gitOut(t, work, "merge-base", "fix/grafted-local", "origin/main"); mb != "" {
		t.Fatalf("branch was reset/mutated despite carrying local-only work")
	}
}

// TestSetupWorktree_ExistingRelatedBranchPreserved verifies backward compat:
// an existing local branch that DOES share history with the base is reused
// as-is — its unique commits are preserved, never reset.
func TestSetupWorktree_ExistingRelatedBranchPreserved(t *testing.T) {
	work, _ := initGitRepo(t)
	ctx := context.Background()

	// Branch off main with an extra commit (shares history with origin/main).
	gitRun(t, work, "checkout", "-b", "fix/related", "origin/main")
	if err := os.WriteFile(filepath.Join(work, "feature.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, work, "add", "feature.txt")
	gitRun(t, work, "commit", "-m", "feature work")
	wantTip := gitOut(t, work, "rev-parse", "fix/related")
	gitRun(t, work, "checkout", "main")

	wt := filepath.Join(t.TempDir(), "wt-related")
	if err := setupWorktree(ctx, work, wt, "fix/related", "origin/main"); err != nil {
		t.Fatalf("setupWorktree on a related branch should succeed: %v", err)
	}
	// The unique commit must be preserved (branch NOT reset onto base).
	if gotTip := gitOut(t, work, "rev-parse", "fix/related"); gotTip != wantTip {
		t.Fatalf("related branch tip changed: got %q want %q (unique commit was discarded)", gotTip, wantTip)
	}
}
