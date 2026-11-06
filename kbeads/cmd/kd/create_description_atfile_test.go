package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCreateCmd_DescriptionAtFileExpands is the create-path counterpart to the
// ka-0000003715 regression guard: `kd create <title> -d @file` must send the
// file's CONTENTS as the description, not the literal "@/path" pointer.
func TestCreateCmd_DescriptionAtFileExpands(t *testing.T) {
	dir := t.TempDir()
	body := "# New bead body\n\nActual content read from a file.\n"
	path := filepath.Join(dir, "body.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := createCmd.Flags().Set("description", "@"+path); err != nil {
		t.Fatalf("set -d: %v", err)
	}
	if err := createCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	defer func() {
		_ = createCmd.Flags().Set("description", "")
		_ = createCmd.Flags().Set("project", "")
	}()

	_ = captureStdout(t, func() {
		if err := createCmd.RunE(createCmd, []string{"a bead"}); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.CreateBeadCalls) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(mc.CreateBeadCalls))
	}
	if got := mc.CreateBeadCalls[0].Description; got != body {
		t.Errorf("Description = %q, want the file body %q", got, body)
	}
}

// TestCreateCmd_DescriptionMissingAtFileFailsLoud asserts create refuses to
// write when -d points at a missing file, so a stale pointer can never become
// a bead body.
func TestCreateCmd_DescriptionMissingAtFileFailsLoud(t *testing.T) {
	dir := t.TempDir()
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := createCmd.Flags().Set("description", "@"+filepath.Join(dir, "gone.md")); err != nil {
		t.Fatalf("set -d: %v", err)
	}
	if err := createCmd.Flags().Set("project", "gardenio"); err != nil {
		t.Fatalf("set project: %v", err)
	}
	defer func() {
		_ = createCmd.Flags().Set("description", "")
		_ = createCmd.Flags().Set("project", "")
	}()

	err := createCmd.RunE(createCmd, []string{"a bead"})
	if err == nil {
		t.Fatal("expected error for -d @missing-file, got nil")
	}
	if len(mc.CreateBeadCalls) != 0 {
		t.Errorf("CreateBead must not be called when -d @file fails to read, got %d calls", len(mc.CreateBeadCalls))
	}
}
