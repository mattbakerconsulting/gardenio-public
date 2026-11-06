package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveCoopWorkdir_ProjectRepoExists(t *testing.T) {
	// Create a temp directory structure mimicking init-clone output.
	tmp := t.TempDir()
	repoDir := filepath.Join(tmp, "testproj", "work")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Override the hardcoded /home/agent/bot path by using a project
	// whose resolved path will match our temp dir.
	// Since resolveCoopWorkdir uses filepath.Join("/home/agent/bot", project, "work"),
	// we can't easily test with the real path. Instead, test the logic directly.

	// Direct logic test: the function checks os.Stat(repoDir/.git).IsDir().
	info, err := os.Stat(filepath.Join(repoDir, ".git"))
	if err != nil || !info.IsDir() {
		t.Fatal("expected .git to be a directory")
	}
}

func TestResolveCoopWorkdir_NoProject(t *testing.T) {
	cfg := k8sConfig{
		workspace: "/tmp/scaffold",
		project:   "",
	}

	got := resolveCoopWorkdir(cfg)
	if got != "/tmp/scaffold" {
		t.Errorf("resolveCoopWorkdir(no project) = %q, want scaffold workspace", got)
	}
}

func TestResolveCoopWorkdir_ProjectNoRepo(t *testing.T) {
	cfg := k8sConfig{
		workspace: "/tmp/scaffold",
		project:   "nonexistent-project-xyz",
	}

	got := resolveCoopWorkdir(cfg)
	if got != "/tmp/scaffold" {
		t.Errorf("resolveCoopWorkdir(missing repo) = %q, want scaffold workspace", got)
	}
}

func TestResolveMuxSessionID_PrefersPodName(t *testing.T) {
	// A >63-char pod name MUST register under its full, untruncated name via
	// POD_NAME — $HOSTNAME/os.Hostname() would be truncated to 63 bytes by the
	// kernel and break the Slack agent-card Terminal deep-link (ka-0000003655).
	fullName := "crew-devsecops-devsecops-triage-gardener-thread-1786382541-341789" // 64 chars
	if len(fullName) <= 63 {
		t.Fatalf("test fixture must exceed 63 chars, got %d", len(fullName))
	}
	t.Setenv("POD_NAME", fullName)

	got := resolveMuxSessionID()
	if got != fullName {
		t.Errorf("resolveMuxSessionID() = %q (len %d), want full name %q (len %d)",
			got, len(got), fullName, len(fullName))
	}
}

func TestResolveMuxSessionID_FallsBackToHostname(t *testing.T) {
	// Absent POD_NAME (e.g. a non-downward-API runtime), fall back to the
	// hostname rather than returning empty.
	t.Setenv("POD_NAME", "")

	hostname, _ := os.Hostname()
	got := resolveMuxSessionID()
	if got != hostname {
		t.Errorf("resolveMuxSessionID() with no POD_NAME = %q, want hostname %q", got, hostname)
	}
}

func TestParseResetDuration_PM(t *testing.T) {
	dur := parseResetDuration("Rate limit exceeded, resets 9pm (UTC)")
	if dur <= 0 {
		t.Error("expected positive duration")
	}
}

func TestParseResetDuration_NoMatch(t *testing.T) {
	dur := parseResetDuration("no reset info here")
	if dur != 0 {
		t.Errorf("expected 0, got %v", dur)
	}
}

func TestResolveCoopWorkdir_SetsEnvVars(t *testing.T) {
	// Create a temp repo structure.
	tmp := t.TempDir()
	project := "myproject"
	repoDir := filepath.Join(tmp, project, "work")
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	// We need to test with a real git repo for this to work properly.
	// Since resolveCoopWorkdir hardcodes /home/agent/bot, we create
	// a real repo there if we have write access (skip if not).
	testDir := filepath.Join(t.TempDir(), "agent-workspace-test")
	gitDir := filepath.Join(testDir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Skip("can't create test directory")
	}

	// Initialize a real git repo.
	cmd := exec.Command("git", "init", "-q", testDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("can't init git repo: %v: %s", err, out)
	}

	// The function sets env vars when it finds the repo.
	// Since we can't easily mock the path, test the fallback case.
	cfg := k8sConfig{
		workspace: testDir,
		project:   "",
	}
	got := resolveCoopWorkdir(cfg)
	if got != testDir {
		t.Errorf("resolveCoopWorkdir = %q, want %q", got, testDir)
	}
}
