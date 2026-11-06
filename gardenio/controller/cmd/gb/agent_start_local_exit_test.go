package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/coopapi"
)

// A laptop whose ~/.claude.json has never seen --dir: preTrustClaudeDir marks
// it trusted and keeps every other key and project entry (gd-bJeLOYSWZG).
func TestPreTrustClaudeDirMergesIntoOperatorFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	existing := `{"preferredTheme":"light","projects":{"/other":{"allowedTools":["Bash"]}}}`
	if err := os.WriteFile(path, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	if err := preTrustClaudeDir(dir); err != nil {
		t.Fatalf("preTrustClaudeDir: %v", err)
	}

	cfg := readClaudeJSON(t, home)
	if !projectTrusted(t, cfg, dir) {
		t.Errorf("%s not pre-trusted", dir)
	}
	if cfg["preferredTheme"] != "light" {
		t.Errorf("preferredTheme clobbered: %v", cfg["preferredTheme"])
	}
	if _, ok := cfg["hasCompletedOnboarding"]; ok {
		t.Errorf("operator file gained pod-only onboarding keys")
	}
	other := cfg["projects"].(map[string]any)["/other"].(map[string]any)
	if tools, _ := other["allowedTools"].([]any); len(tools) != 1 {
		t.Errorf("/other entry lost allowedTools: %v", other)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode not kept: %v %v", fi.Mode(), err)
	}
}

// Claude Code keys projects by its getcwd, the symlink-resolved path, so a
// --dir reached through a symlink must be trusted under both spellings.
func TestPreTrustClaudeDirTrustsResolvedPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}

	if err := preTrustClaudeDir(link); err != nil {
		t.Fatalf("preTrustClaudeDir: %v", err)
	}

	cfg := readClaudeJSON(t, home)
	for _, d := range []string{link, resolved} {
		if !projectTrusted(t, cfg, d) {
			t.Errorf("%s not pre-trusted", d)
		}
	}
}

// An already-trusted dir is a no-op: the file is not rewritten.
func TestPreTrustClaudeDirNoopWhenTrusted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()
	resolved, _ := filepath.EvalSymlinks(dir)
	path := filepath.Join(home, ".claude.json")
	body := `{"projects":{"` + dir + `":{"hasTrustDialogAccepted":true},"` + resolved + `":{"hasTrustDialogAccepted":true}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := preTrustClaudeDir(dir); err != nil {
		t.Fatalf("preTrustClaudeDir: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != body {
		t.Errorf("trusted file rewritten:\n%s", got)
	}
}

// The operator's file is never clobbered: an unparseable ~/.claude.json is an
// error and stays byte-for-byte as it was.
func TestPreTrustClaudeDirRefusesCorruptFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := preTrustClaudeDir(t.TempDir()); err == nil {
		t.Fatal("expected an error for a corrupt ~/.claude.json")
	}
	if got, _ := os.ReadFile(path); string(got) != "{not json" {
		t.Errorf("corrupt file rewritten: %q", got)
	}
}

// No ~/.claude.json yet (claude never run on this host): it is created.
func TestPreTrustClaudeDirCreatesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := t.TempDir()

	if err := preTrustClaudeDir(dir); err != nil {
		t.Fatalf("preTrustClaudeDir: %v", err)
	}
	if !projectTrusted(t, readClaudeJSON(t, home), dir) {
		t.Errorf("%s not pre-trusted", dir)
	}
}

type fakeLocalExitCoop struct {
	states    []string
	polls     atomic.Int32
	exitCode  int
	shutdowns atomic.Int32
}

func (f *fakeLocalExitCoop) GetAgentState(ctx context.Context) (*coopapi.AgentState, error) {
	i := int(f.polls.Add(1)) - 1
	if i >= len(f.states) {
		i = len(f.states) - 1
	}
	return &coopapi.AgentState{State: f.states[i]}, nil
}

func (f *fakeLocalExitCoop) GetSessionStatus(ctx context.Context) (*coopapi.SessionStatus, error) {
	return &coopapi.SessionStatus{ExitCode: &f.exitCode}, nil
}

func (f *fakeLocalExitCoop) Shutdown(ctx context.Context) error {
	f.shutdowns.Add(1)
	return nil
}

// claude exits at startup (the unanswered trust dialog): coop is asked to shut
// down exactly once, so runLocal returns and deregisters the agent.
func TestStopCoopWhenAgentExitsShutsDownOnExit(t *testing.T) {
	coop := &fakeLocalExitCoop{states: []string{"starting", "exited"}, exitCode: 1}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if !stopCoopWhenAgentExits(ctx, coop, time.Millisecond) {
		t.Fatal("watcher returned without shutting coop down")
	}
	if n := coop.shutdowns.Load(); n != 1 {
		t.Errorf("Shutdown called %d times, want 1", n)
	}
}

// A live agent is left alone until ctx ends (Ctrl-C, control:kill).
func TestStopCoopWhenAgentExitsLeavesLiveAgent(t *testing.T) {
	coop := &fakeLocalExitCoop{states: []string{"idle", "working", "idle"}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if stopCoopWhenAgentExits(ctx, coop, time.Millisecond) {
		t.Fatal("watcher shut down a live agent")
	}
	if n := coop.shutdowns.Load(); n != 0 {
		t.Errorf("Shutdown called %d times, want 0", n)
	}
}
