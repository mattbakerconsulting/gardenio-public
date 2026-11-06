package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// gitCtx runs `git -C dir <args...>` under ctx with credential prompts disabled,
// returning trimmed stdout. Errors include the git stderr for diagnosis.
func gitCtx(ctx context.Context, dir string, args ...string) (string, error) {
	return gitEnv(ctx, dir, append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), args...)
}

// gitEnv is gitCtx with an explicit environment (used to inject GIT_INDEX_FILE
// / a fixed commit identity for the throwaway-index snapshot).
func gitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	full := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// worktreeHead returns (sha, true) for a born HEAD, ("", false) when unborn.
func worktreeHead(ctx context.Context, dir string) (string, bool) {
	sha, err := gitCtx(ctx, dir, "rev-parse", "--verify", "-q", "HEAD")
	if err != nil || sha == "" {
		return "", false
	}
	return sha, true
}

// worktreeBranch returns the checked-out branch name, or a "(detached)"/
// "(unborn)" sentinel. Used only for the manifest's human context.
func worktreeBranch(ctx context.Context, dir string) string {
	b, err := gitCtx(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD")
	if err != nil || b == "" {
		if _, hasHead := worktreeHead(ctx, dir); hasHead {
			return "(detached)"
		}
		return "(unborn)"
	}
	return b
}

// worktreeDirtyFiles returns `git status --porcelain` lines for the
// manifest. Captures raw stdout (only the trailing newline trimmed) rather
// than going through gitCtx: porcelain lines are fixed-format (2-char status
// + space + path), and gitCtx's blanket strings.TrimSpace over the whole
// multi-line output eats the leading status byte of the first line whenever
// that byte is a space (e.g. a plain tracked edit, " M base.txt"), which
// then corrupts every path reapDiffFiles derives from it.
func worktreeDirtyFiles(ctx context.Context, dir string) []string {
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "--no-optional-locks", "status", "--porcelain")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil
	}
	out := strings.TrimRight(stdout.String(), "\n")
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// commitWorktreeSnapshot captures a worktree's content into a DETACHED commit
// built in a throwaway index — never touching the real index, HEAD, or branch
// pointer — and returns its sha. It is the one snapshot recipe behind both the
// full Tier-1 snapshot (addFlag "-A": tracked edits + untracked files) and the
// Tier-2/Tier-3 pushable snapshot (addFlag "-u": tracked edits/deletes ONLY, the
// leak-surface guard that keeps untracked files off the remote). Steps:
// read-tree HEAD (when HEAD is born) → add <addFlag> → write-tree →
// commit-tree [-p HEAD]. With an unborn HEAD the commit is parentless.
func commitWorktreeSnapshot(ctx context.Context, path, head string, hasHead bool, addFlag, msg string) (string, error) {
	idx, err := os.CreateTemp("", "salvage-index-*")
	if err != nil {
		return "", err
	}
	idxPath := idx.Name()
	idx.Close()
	// git REJECTS a 0-byte index file ("fatal: index file smaller than
	// expected"), so os.CreateTemp's empty file must be removed first: git then
	// initializes a fresh, valid empty index at idxPath on the first write. This
	// is load-bearing for an unborn HEAD (no read-tree to populate the index) —
	// with the 0-byte file in place `add`/`write-tree` fail and ALL WIP is
	// silently dropped (the exact data loss this command exists to prevent).
	if err := os.Remove(idxPath); err != nil {
		return "", err
	}
	defer os.Remove(idxPath)
	// GIT_INDEX_FILE/GIT_TERMINAL_PROMPT go LAST so they win over any ambient
	// value — Go's os/exec resolves duplicate env keys last-wins.
	env := append(os.Environ(), "GIT_INDEX_FILE="+idxPath, "GIT_TERMINAL_PROMPT=0")

	if hasHead {
		if _, err := gitEnv(ctx, path, env, "--no-optional-locks", "read-tree", "HEAD"); err != nil {
			return "", err
		}
	}
	if _, err := gitEnv(ctx, path, env, "--no-optional-locks", "add", addFlag); err != nil {
		return "", err
	}
	tree, err := gitEnv(ctx, path, env, "--no-optional-locks", "write-tree")
	if err != nil || strings.TrimSpace(tree) == "" {
		return "", fmt.Errorf("write-tree: %w", err)
	}
	tree = strings.TrimSpace(tree)

	// The salvage commit is built with a fixed identity so it never depends on
	// the ambient git user config (which may be unset on a teardown path). The
	// overrides go LAST: pods inject GIT_AUTHOR_NAME=<agent>, and Go's os/exec
	// resolves duplicate env keys last-wins, so prepending would let the ambient
	// value shadow the fixed identity.
	commitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=gardenio-salvage", "GIT_AUTHOR_EMAIL=salvage@gardenio",
		"GIT_COMMITTER_NAME=gardenio-salvage", "GIT_COMMITTER_EMAIL=salvage@gardenio",
		"GIT_TERMINAL_PROMPT=0",
	)
	args := []string{"commit-tree", tree, "-m", msg}
	if hasHead {
		args = []string{"commit-tree", tree, "-p", head, "-m", msg}
	}
	sha, err := gitEnv(ctx, path, commitEnv, args...)
	if err != nil || strings.TrimSpace(sha) == "" {
		return "", fmt.Errorf("commit-tree: %w", err)
	}
	return strings.TrimSpace(sha), nil
}

// durableAnchorRefs is the ordered list of remote refs to try as a bundle
// anchor. The first that resolves keeps the bundle incremental yet recoverable
// from a fresh clone.
var durableAnchorRefs = []string{"origin/main", "origin/master", "origin/HEAD"}

// durableAnchor returns the (ref, sha) of the first durableAnchorRefs entry that
// resolves in dir, or ("","") when none do (→ caller emits a self-contained
// full bundle).
func durableAnchor(ctx context.Context, dir string) (string, string) {
	for _, ref := range durableAnchorRefs {
		if sha, err := gitCtx(ctx, dir, "rev-parse", "--verify", "-q", ref); err == nil && sha != "" {
			return ref, sha
		}
	}
	return "", ""
}

// ── S3 key layout + manifest helpers ────────────────────────────────────────

// salvagePrefix builds the S3 key prefix for salvage artifacts, mirroring the
// stdout-tail per-spawn layout (<prefix>/<agent-bead>/<session>/) with a
// trailing /salvage segment. Falls back to a flat <prefix>/<session>/salvage
// when no agent bead ID is in scope.
func salvagePrefix(prefix, agentBeadID, session string) string {
	if agentBeadID == "" {
		return fmt.Sprintf("%s/%s/salvage", prefix, session)
	}
	return fmt.Sprintf("%s/%s/%s/salvage", prefix, agentBeadID, session)
}

// salvageRecoverRecipe returns a copy-pasteable recovery command sequence.
func salvageRecoverRecipe(agentBead, session string) string {
	arg := agentBead
	if arg == "" {
		arg = "<agent-bead-id>"
	}
	sess := ""
	if session != "" && session != agentBead {
		sess = " --session " + session
	}
	return fmt.Sprintf("gb salvage restore --agent-bead %s%s", arg, sess)
}

// salvageTimestamp returns an RFC3339 timestamp, or "" if the clock is
// unavailable. Isolated so the manifest write does not hard-depend on it.
func salvageTimestamp() string {
	return time.Now().UTC().Format(time.RFC3339)
}
