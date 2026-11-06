package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// bigJsonl returns a .jsonl payload > 256 bytes so it's considered resumable.
func bigJsonl() []byte {
	return []byte(fmt.Sprintf("{%q: %q}\n", "data", strings.Repeat("x", 300)))
}

func TestFindResumeSession_SkipsTinyJsonl(t *testing.T) {
	// A tiny .jsonl (< 256 bytes) is skipped — likely just the init message.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	logPath := filepath.Join(projectDir, "tiny-session.jsonl")
	os.WriteFile(logPath, []byte("{}\n"), 0o644)

	result := findResumeSession(dir, true, false)
	if result != "" {
		t.Errorf("findResumeSession returned %q for tiny .jsonl, want empty", result)
	}
}

func TestFindResumeSession_FindsLargeJsonlWithoutCompanionDir(t *testing.T) {
	// A .jsonl with enough content is resumable even without a companion dir.
	// Claude Code creates companion dirs lazily (only for subagents/tool-results).
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	logPath := filepath.Join(projectDir, "valid-session.jsonl")
	os.WriteFile(logPath, bigJsonl(), 0o644)

	result := findResumeSession(dir, true, false)
	if result != logPath {
		t.Errorf("findResumeSession = %q, want %q", result, logPath)
	}
}

func TestFindResumeSession_OldStaleFilesDoNotBlockResume(t *testing.T) {
	// Old stale files (>1h) from previous restart cycles should NOT count
	// toward the maxStaleRetries threshold. Only recent stale files indicate
	// a rapid failure loop in the current restart cycle.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	// Create 5 old stale files (simulating previous restart cycles).
	for i := 0; i < 5; i++ {
		stalePath := filepath.Join(projectDir, fmt.Sprintf("old-session-%d.jsonl.stale", i))
		os.WriteFile(stalePath, []byte("{}\n"), 0o644)
		// Set mtime to 2 hours ago so they count as old.
		oldTime := time.Now().Add(-2 * time.Hour)
		os.Chtimes(stalePath, oldTime, oldTime)
	}

	// Create a valid resumable session.
	logPath := filepath.Join(projectDir, "current-session.jsonl")
	os.WriteFile(logPath, bigJsonl(), 0o644)

	// Resume should still work — old stale files don't block it.
	result := findResumeSession(dir, true, false)
	if result != logPath {
		t.Errorf("findResumeSession = %q, want %q (old stale files should not block resume)", result, logPath)
	}
}

func TestFindResumeSession_StaleDisableBypassesStaleCount(t *testing.T) {
	// When staleDisable=true, sessions are found even with >= 3 stale files.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	// Create 5 stale files (well above maxStaleRetries=3).
	for i := 0; i < 5; i++ {
		name := filepath.Join(projectDir, fmt.Sprintf("stale-%d.jsonl.stale", i))
		os.WriteFile(name, []byte("{}\n"), 0o644)
	}

	// Create a valid resumable session (large enough, no companion dir needed).
	logPath := filepath.Join(projectDir, "valid.jsonl")
	os.WriteFile(logPath, bigJsonl(), 0o644)

	// With staleDisable=true, the stale count is ignored.
	result := findResumeSession(dir, true, true)
	if result != logPath {
		t.Errorf("findResumeSession with staleDisable=true = %q, want %q", result, logPath)
	}
}

func TestRetireStaleSession_RetiresCompanionDir(t *testing.T) {
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	// Create session log + companion dir + session-env.
	logPath := filepath.Join(projectDir, "abc-123.jsonl")
	os.WriteFile(logPath, []byte("{}\n"), 0o644)
	companionDir := filepath.Join(projectDir, "abc-123")
	os.MkdirAll(companionDir, 0o755)
	os.WriteFile(filepath.Join(companionDir, "db.sqlite"), []byte{}, 0o644)
	sessionEnvDir := filepath.Join(dir, "session-env", "abc-123")
	os.MkdirAll(sessionEnvDir, 0o755)
	os.WriteFile(filepath.Join(sessionEnvDir, "state.json"), []byte("{}"), 0o644)

	retireStaleSession(logPath)

	// .jsonl should be renamed to .stale
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Error("session log should be renamed to .stale")
	}
	if _, err := os.Stat(logPath + ".stale"); err != nil {
		t.Error("stale session log should exist")
	}

	// Companion dir should be renamed to .stale
	if _, err := os.Stat(companionDir); !os.IsNotExist(err) {
		t.Error("companion dir should be renamed to .stale")
	}
	if _, err := os.Stat(companionDir + ".stale"); err != nil {
		t.Error("stale companion dir should exist")
	}

	// Session-env dir should be renamed to .stale
	if _, err := os.Stat(sessionEnvDir); !os.IsNotExist(err) {
		t.Error("session-env dir should be renamed to .stale")
	}
	if _, err := os.Stat(sessionEnvDir + ".stale"); err != nil {
		t.Error("stale session-env dir should exist")
	}
}

func TestFindResumeSession_SkipsMissingSessionEnv(t *testing.T) {
	// When session-env/ root exists (Claude v2.1+), a session without a
	// matching session-env/<uuid>/ directory should not be resumable.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	// Create a .jsonl large enough to be resumable.
	logPath := filepath.Join(projectDir, "abc-123.jsonl")
	os.WriteFile(logPath, bigJsonl(), 0o644)

	// Create session-env root WITHOUT a matching entry for this session.
	os.MkdirAll(filepath.Join(dir, "session-env"), 0o755)
	// session-env/abc-123/ does NOT exist.

	result := findResumeSession(dir, true, false)
	if result != "" {
		t.Errorf("findResumeSession returned %q, want empty (missing session-env)", result)
	}
}

func TestFindResumeSession_AcceptsSessionWithEnv(t *testing.T) {
	// When session-env/ root exists and the session has a matching
	// session-env/<uuid>/ directory, it should be resumable.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	logPath := filepath.Join(projectDir, "def-456.jsonl")
	os.WriteFile(logPath, bigJsonl(), 0o644)

	// Create matching session-env entry.
	os.MkdirAll(filepath.Join(dir, "session-env", "def-456"), 0o755)

	result := findResumeSession(dir, true, false)
	if result != logPath {
		t.Errorf("findResumeSession = %q, want %q", result, logPath)
	}
}

func TestFindResumeSession_CapsValidationToMostRecentK(t *testing.T) {
	// G7: walk must be bounded. With N > maxResumeCandidates files and a
	// session-env root that lacks every entry, validation tries to retire each
	// processed .jsonl to .jsonl.stale. Under the cap, only the K most recent
	// are processed/retired; the older N-K remain as .jsonl untouched.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create session-env root but no per-session entries → every processed
	// file fails the env check and gets renamed to .jsonl.stale.
	if err := os.MkdirAll(filepath.Join(dir, "session-env"), 0o755); err != nil {
		t.Fatal(err)
	}

	n := maxResumeCandidates + 100
	base := time.Now().Add(-time.Duration(n) * time.Second)
	for i := range n {
		p := filepath.Join(projectDir, fmt.Sprintf("session-%05d.jsonl", i))
		if err := os.WriteFile(p, bigJsonl(), 0o644); err != nil {
			t.Fatal(err)
		}
		mt := base.Add(time.Duration(i) * time.Second) // higher i = more recent
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}

	// staleDisable=true: the early stale-count walk would otherwise cap retries.
	if got := findResumeSession(dir, true, true); got != "" {
		t.Errorf("expected no resumable session (all fail env check), got %q", got)
	}

	jsonl, stale := 0, 0
	entries, err := os.ReadDir(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		switch {
		case strings.HasSuffix(e.Name(), ".jsonl.stale"):
			stale++
		case strings.HasSuffix(e.Name(), ".jsonl"):
			jsonl++
		}
	}
	if stale != maxResumeCandidates {
		t.Errorf("retired %d files, want exactly maxResumeCandidates=%d", stale, maxResumeCandidates)
	}
	if jsonl != n-maxResumeCandidates {
		t.Errorf("untouched .jsonl count = %d, want %d (n - K)", jsonl, n-maxResumeCandidates)
	}
}

func TestFindResumeSession_NoSessionEnvRootSkipsCheck(t *testing.T) {
	// When session-env/ root does not exist (pre-v2.1 Claude Code),
	// session-env validation is skipped entirely.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	os.MkdirAll(projectDir, 0o755)

	logPath := filepath.Join(projectDir, "old-session.jsonl")
	os.WriteFile(logPath, bigJsonl(), 0o644)

	// No session-env/ directory at all — pre-v2.1 environment.
	result := findResumeSession(dir, true, false)
	if result != logPath {
		t.Errorf("findResumeSession = %q, want %q (session-env check should be skipped)", result, logPath)
	}
}

func TestCleanAllSessionLogs_RemovesEverythingForFresh(t *testing.T) {
	// ka-0000004439: when gb init has decided fresh, every .jsonl in
	// projects/ (and its companion + session-env entry) must be removed
	// so Claude Code cannot auto-resume from leftover transcript state.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Live transcript + companion + session-env entry.
	liveLog := filepath.Join(projectDir, "abc-123.jsonl")
	os.WriteFile(liveLog, bigJsonl(), 0o644)
	liveCompanion := filepath.Join(projectDir, "abc-123")
	os.MkdirAll(liveCompanion, 0o755)
	os.WriteFile(filepath.Join(liveCompanion, "x"), []byte("x"), 0o644)
	liveEnv := filepath.Join(dir, "session-env", "abc-123")
	os.MkdirAll(liveEnv, 0o755)
	os.WriteFile(filepath.Join(liveEnv, "state.json"), []byte("{}"), 0o644)

	// Retired transcript (.jsonl.stale) + retired companion + retired env.
	staleLog := filepath.Join(projectDir, "def-456.jsonl.stale")
	os.WriteFile(staleLog, []byte("{}\n"), 0o644)
	staleCompanion := filepath.Join(projectDir, "def-456.stale")
	os.MkdirAll(staleCompanion, 0o755)
	staleEnv := filepath.Join(dir, "session-env", "def-456.stale")
	os.MkdirAll(staleEnv, 0o755)

	cleanAllSessionLogs(dir)

	// Every transcript and its companion/env must be gone.
	for _, p := range []string{liveLog, staleLog, liveCompanion, staleCompanion, liveEnv, staleEnv} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("expected %q removed, still present", p)
		}
	}

	// projects/<hash>/ itself is left intact.
	if _, err := os.Stat(projectDir); err != nil {
		t.Errorf("projects/<hash>/ should remain: %v", err)
	}
}

func TestCleanAllSessionLogs_NoProjectsDirIsNoOp(t *testing.T) {
	// Fresh PVC: projects/ doesn't exist yet. Must not crash or create state.
	dir := t.TempDir()
	cleanAllSessionLogs(dir) // must not panic
	if _, err := os.Stat(filepath.Join(dir, "projects")); !os.IsNotExist(err) {
		t.Errorf("projects/ should not have been created")
	}
}

func TestCleanAllSessionLogs_LeavesUnrelatedFiles(t *testing.T) {
	// Only .jsonl[.stale] should be touched. Other files (e.g. settings)
	// must survive so unrelated PVC state isn't collateral damage.
	dir := t.TempDir()
	projectDir := filepath.Join(dir, "projects", "-test-project")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(projectDir, "settings.json")
	os.WriteFile(keep, []byte("{}"), 0o644)
	jsonl := filepath.Join(projectDir, "abc-123.jsonl")
	os.WriteFile(jsonl, bigJsonl(), 0o644)

	cleanAllSessionLogs(dir)

	if _, err := os.Stat(jsonl); !os.IsNotExist(err) {
		t.Errorf(".jsonl should be removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("unrelated settings.json was removed: %v", err)
	}
}

func TestS3ResumeIntended_RequiresBucketAndKey(t *testing.T) {
	// ka-0000004439: gb init only intends to resume when both the S3
	// bucket and an agent-scoped session key are configured. Anything else
	// is treated as fresh, and the local session log purge runs.
	cases := []struct {
		name   string
		bucket string
		agent  string
		want   bool
	}{
		{"both set", "my-bucket", "gd-AGENT", true},
		{"no bucket", "", "gd-AGENT", false},
		{"no agent", "my-bucket", "", false},
		{"neither", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COOP_S3_BUCKET", tc.bucket)
			t.Setenv("KD_AGENT_ID", tc.agent)
			t.Setenv("BOAT_AGENT_BEAD_ID", "")
			if got := s3ResumeIntended(); got != tc.want {
				t.Errorf("s3ResumeIntended() = %v, want %v", got, tc.want)
			}
		})
	}
}
