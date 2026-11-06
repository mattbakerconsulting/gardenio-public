package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemPromptFlag(t *testing.T) {
	if got := systemPromptFlag(""); got != "" {
		t.Errorf("empty path should yield empty flag, got %q", got)
	}
	if got := systemPromptFlag("/tmp/x.md"); got != "--append-system-prompt-file=/tmp/x.md" {
		t.Errorf("unexpected flag: %q", got)
	}
}

func TestInjectSystemPromptArgs(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		path string
		want []string
	}{
		{"empty path returns input", []string{"claude", "--foo"}, "", []string{"claude", "--foo"}},
		{"empty argv returns input", nil, "/x.md", nil},
		{"flag inserted after binary", []string{"claude", "--dangerously-skip-permissions"}, "/x.md",
			[]string{"claude", "--append-system-prompt-file=/x.md", "--dangerously-skip-permissions"}},
		{"single-token argv", []string{"claude"}, "/x.md",
			[]string{"claude", "--append-system-prompt-file=/x.md"}},
		// claudeless 0.4.0 exits 2 on the flag (gd-K77LsUtqhK).
		{"claudeless argv unchanged", []string{"claudeless", "--scenario", "/scenarios/mail-reader.toml"}, "/x.md",
			[]string{"claudeless", "--scenario", "/scenarios/mail-reader.toml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := injectSystemPromptArgs(tt.in, tt.path)
			if len(got) != len(tt.want) {
				t.Fatalf("len mismatch: got %v want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("argv[%d]: got %q want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestInjectSystemPromptShellCommand(t *testing.T) {
	tests := []struct {
		name    string
		command string
		path    string
		want    string
	}{
		{"empty path returns input", "claude --foo", "", "claude --foo"},
		{"empty command returns input", "", "/x.md", ""},
		{"flag inserted after binary", "claude --dangerously-skip-permissions", "/x.md",
			"claude --append-system-prompt-file=/x.md --dangerously-skip-permissions"},
		{"single-token command", "claude", "/x.md",
			"claude --append-system-prompt-file=/x.md"},
		{"tab-separated", "claude\t--foo", "/x.md",
			"claude --append-system-prompt-file=/x.md\t--foo"},
		{"claudeless command unchanged", "claudeless --scenario /scenarios/mail-reader.toml", "/x.md",
			"claudeless --scenario /scenarios/mail-reader.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := injectSystemPromptShellCommand(tt.command, tt.path); got != tt.want {
				t.Errorf("got %q want %q", got, tt.want)
			}
		})
	}
}

// TestWriteSystemPromptFile_EmptyWorkdir documents the no-op contract for an
// empty workdir — callers must treat this as best-effort.
func TestWriteSystemPromptFile_EmptyWorkdir(t *testing.T) {
	path, ok := writeSystemPromptFile("", nil)
	if ok || path != "" {
		t.Errorf("expected (\"\", false) for empty workdir, got (%q, %v)", path, ok)
	}
}

// TestWriteSystemPromptFile_GbCommandFails verifies that a failed `gb prime`
// invocation yields a clean (empty path, false) result rather than crashing.
// We point PATH at a bin dir containing a stub `gb` that exits non-zero, so
// the subprocess fails fast without invoking the real binary (or the test
// binary, which would recurse).
func TestWriteSystemPromptFile_GbCommandFails(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "gb")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	t.Setenv("PATH", binDir)

	logged := false
	logf := func(string, ...any) { logged = true }
	path, ok := writeSystemPromptFile(dir, logf)
	if ok {
		t.Errorf("expected ok=false when gb prime fails, got path=%q", path)
	}
	if !logged {
		t.Error("expected logf to be invoked on failure")
	}
	// .gb dir should still be created (mkdir succeeds before exec).
	if _, err := os.Stat(filepath.Join(dir, ".gb")); err != nil {
		t.Errorf("expected .gb dir to exist after mkdir: %v", err)
	}
}

// TestSystemPromptFileName documents the on-disk filename contract — coop
// resume must read the same file the previous session wrote.
func TestSystemPromptFileName(t *testing.T) {
	if !strings.HasSuffix(systemPromptFileName, ".md") {
		t.Errorf("filename should be markdown: %q", systemPromptFileName)
	}
	if strings.ContainsAny(systemPromptFileName, "/\\") {
		t.Errorf("filename should be a basename, not a path: %q", systemPromptFileName)
	}
}
