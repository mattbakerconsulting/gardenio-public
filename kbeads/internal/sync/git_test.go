package sync

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitDestination(t *testing.T) {
	// Check git is available.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	// Create a bare remote repo.
	remoteDir := t.TempDir()
	run(t, remoteDir, "git", "init", "--bare")

	// Clone it to a working copy.
	workDir := t.TempDir()
	run(t, workDir, "git", "clone", remoteDir, "repo")
	repoDir := filepath.Join(workDir, "repo")

	// Git needs user identity for commits.
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "branch", "-m", "main")

	// Create an initial commit so the branch exists.
	initFile := filepath.Join(repoDir, ".gitkeep")
	if err := os.WriteFile(initFile, []byte(""), 0o644); err != nil {
		t.Fatalf("write .gitkeep: %v", err)
	}
	run(t, repoDir, "git", "add", ".")
	run(t, repoDir, "git", "commit", "-m", "init")
	run(t, repoDir, "git", "push", "origin", "main")

	dest := NewGitDestination(repoDir, "beads.jsonl", "main")

	// First write.
	data1 := []byte(`{"version":"1","type":"header"}` + "\n")
	if err := dest.Write(context.Background(), data1); err != nil {
		t.Fatalf("first write: %v", err)
	}

	// Verify file exists.
	got, err := os.ReadFile(filepath.Join(repoDir, "beads.jsonl"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != string(data1) {
		t.Fatalf("file content mismatch: got %q", string(got))
	}

	// Second write with same data should be a no-op (no commit).
	if err := dest.Write(context.Background(), data1); err != nil {
		t.Fatalf("second write (no-op): %v", err)
	}

	// Third write with different data should commit.
	data2 := []byte(`{"version":"1","type":"header","bead_count":1}` + "\n")
	if err := dest.Write(context.Background(), data2); err != nil {
		t.Fatalf("third write: %v", err)
	}

	got, err = os.ReadFile(filepath.Join(repoDir, "beads.jsonl"))
	if err != nil {
		t.Fatalf("read file after update: %v", err)
	}
	if string(got) != string(data2) {
		t.Fatalf("file content mismatch after update: got %q", string(got))
	}
}

func TestGitDestination_SubDirectory(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found in PATH")
	}

	remoteDir := t.TempDir()
	run(t, remoteDir, "git", "init", "--bare")

	workDir := t.TempDir()
	run(t, workDir, "git", "clone", remoteDir, "repo")
	repoDir := filepath.Join(workDir, "repo")

	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "branch", "-m", "main")

	initFile := filepath.Join(repoDir, ".gitkeep")
	if err := os.WriteFile(initFile, []byte(""), 0o644); err != nil {
		t.Fatalf("write .gitkeep: %v", err)
	}
	run(t, repoDir, "git", "add", ".")
	run(t, repoDir, "git", "commit", "-m", "init")
	run(t, repoDir, "git", "push", "origin", "main")

	dest := NewGitDestination(repoDir, "data/beads.jsonl", "main")

	data := []byte(`{"type":"header"}` + "\n")
	if err := dest.Write(context.Background(), data); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(repoDir, "data", "beads.jsonl"))
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("content mismatch: got %q", string(got))
	}
}

func run(t *testing.T, dir string, name string, args ...string) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v failed: %v", name, args, err)
	}
}
