package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// GitDestination writes JSONL data to a file in a git repo and pushes.
type GitDestination struct {
	repo   string // path to the local clone
	file   string // file path within the repo
	branch string // branch to commit and push to
}

// NewGitDestination creates a git destination. repo is the path to an
// existing local clone.
func NewGitDestination(repo, file, branch string) *GitDestination {
	return &GitDestination{
		repo:   repo,
		file:   file,
		branch: branch,
	}
}

// Write writes data to the configured file, commits, and pushes.
func (d *GitDestination) Write(ctx context.Context, data []byte) error {
	// Ensure we're on the right branch.
	if err := d.git(ctx, "checkout", d.branch); err != nil {
		return fmt.Errorf("git checkout: %w", err)
	}

	// Pull latest to minimize conflicts.
	// Ignore errors since the remote might not have the branch yet.
	_ = d.git(ctx, "pull", "--ff-only", "origin", d.branch)

	// Write the file.
	filePath := filepath.Join(d.repo, d.file)
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0o644); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	// Stage, commit, push.
	if err := d.git(ctx, "add", d.file); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	// Check if there are changes to commit.
	if err := d.git(ctx, "diff", "--cached", "--quiet"); err == nil {
		// No changes — nothing to commit.
		return nil
	}

	if err := d.git(ctx, "commit", "-m", "sync: update beads export"); err != nil {
		return fmt.Errorf("git commit: %w", err)
	}

	if err := d.git(ctx, "push", "origin", d.branch); err != nil {
		return fmt.Errorf("git push: %w", err)
	}

	return nil
}

func (d *GitDestination) git(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = d.repo
	cmd.Stdout = os.Stderr // redirect to stderr so it's visible in logs
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
