package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStateSetGetDelete(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)

	// Set a value.
	if err := stateSetCmd.RunE(stateSetCmd, []string{"MY_KEY", "my-value"}); err != nil {
		t.Fatalf("state set: %v", err)
	}

	// Verify file exists.
	path := filepath.Join(dir, ".state", "learned.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("state file not created: %v", err)
	}

	// Read it back.
	state, err := readState()
	if err != nil {
		t.Fatalf("readState: %v", err)
	}
	if state["MY_KEY"] != "my-value" {
		t.Fatalf("expected 'my-value', got %q", state["MY_KEY"])
	}

	// Set another value with multi-word value.
	if err := stateSetCmd.RunE(stateSetCmd, []string{"OTHER", "hello", "world"}); err != nil {
		t.Fatalf("state set (multi-word): %v", err)
	}
	state, _ = readState()
	if state["OTHER"] != "hello world" {
		t.Fatalf("expected 'hello world', got %q", state["OTHER"])
	}

	// Delete a value.
	if err := stateDeleteCmd.RunE(stateDeleteCmd, []string{"MY_KEY"}); err != nil {
		t.Fatalf("state delete: %v", err)
	}
	state, _ = readState()
	if _, ok := state["MY_KEY"]; ok {
		t.Fatal("MY_KEY should have been deleted")
	}
	if state["OTHER"] != "hello world" {
		t.Fatal("OTHER should still exist after deleting MY_KEY")
	}
}

func TestStateClear(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)

	// Set a value then clear.
	_ = stateSetCmd.RunE(stateSetCmd, []string{"KEY", "val"})
	if err := stateClearCmd.RunE(stateClearCmd, nil); err != nil {
		t.Fatalf("state clear: %v", err)
	}

	state, err := readState()
	if err != nil {
		t.Fatalf("readState after clear: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %d entries", len(state))
	}
}

func TestStateGetNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)

	err := stateGetCmd.RunE(stateGetCmd, []string{"NONEXISTENT"})
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestStateDeleteNotFound(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)

	err := stateDeleteCmd.RunE(stateDeleteCmd, []string{"NONEXISTENT"})
	if err == nil {
		t.Fatal("expected error for missing key")
	}
}

func TestFormatStateForInjection(t *testing.T) {
	// Empty state returns empty string.
	if got := formatStateForInjection(map[string]string{}); got != "" {
		t.Fatalf("expected empty string for empty state, got %q", got)
	}

	// Non-empty state produces formatted output with sorted keys.
	state := map[string]string{
		"API_KEY": "sk-test-123",
		"CWD":    "/home/agent/workspace",
	}
	got := formatStateForInjection(state)
	if got == "" {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(got, "- **API_KEY**: sk-test-123") {
		t.Fatalf("missing API_KEY in output: %s", got)
	}
	if !strings.Contains(got, "- **CWD**: /home/agent/workspace") {
		t.Fatalf("missing CWD in output: %s", got)
	}
	// Verify sorted order: API_KEY before CWD.
	apiIdx := strings.Index(got, "API_KEY")
	cwdIdx := strings.Index(got, "CWD")
	if apiIdx >= cwdIdx {
		t.Fatalf("expected API_KEY before CWD in sorted output")
	}
}

func TestReadStateNoFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)

	state, err := readState()
	if err != nil {
		t.Fatalf("readState with no file: %v", err)
	}
	if len(state) != 0 {
		t.Fatalf("expected empty state, got %d entries", len(state))
	}
}

func TestStateOverwrite(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KD_WORKSPACE", dir)

	// Set a value then overwrite it.
	_ = stateSetCmd.RunE(stateSetCmd, []string{"KEY", "first"})
	_ = stateSetCmd.RunE(stateSetCmd, []string{"KEY", "second"})

	state, _ := readState()
	if state["KEY"] != "second" {
		t.Fatalf("expected 'second', got %q", state["KEY"])
	}
}
