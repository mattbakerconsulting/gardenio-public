package hooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCheckCommitPushEmptyCWD(t *testing.T) {
	if got := CheckCommitPush(""); got != "" {
		t.Errorf("CheckCommitPush(\"\") = %q, want empty", got)
	}
}

func TestCheckCommitPushNonexistentDir(t *testing.T) {
	// Non-existent directory — git will fail, so should return empty.
	if got := CheckCommitPush("/nonexistent/path/12345"); got != "" {
		t.Errorf("CheckCommitPush(nonexistent) = %q, want empty", got)
	}
}

func TestCheckCommitPushCleanRepo(t *testing.T) {
	// Create a clean git repo with one commit.
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")

	// Create a file and commit it.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")

	got := CheckCommitPush(dir)
	if got != "" {
		t.Errorf("CheckCommitPush(clean repo) = %q, want empty", got)
	}
}

func TestCheckCommitPushDirtyRepo(t *testing.T) {
	// Create a git repo with uncommitted changes.
	dir := t.TempDir()
	runGit(t, dir, "init")
	runGit(t, dir, "config", "user.email", "test@test.com")
	runGit(t, dir, "config", "user.name", "Test")

	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-m", "initial")

	// Create an uncommitted file.
	if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("dirty"), 0644); err != nil {
		t.Fatal(err)
	}

	got := CheckCommitPush(dir)
	if got == "" {
		t.Error("CheckCommitPush(dirty repo) = empty, want warning")
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE=2025-01-01T00:00:00Z", "GIT_COMMITTER_DATE=2025-01-01T00:00:00Z")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
}
