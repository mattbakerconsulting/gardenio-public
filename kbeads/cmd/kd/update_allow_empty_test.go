package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runUpdateWith sets each flag on the package-global updateCmd, runs it
// against a seeded mock bead and returns the RunE error and the mock.
func runUpdateWith(t *testing.T, flags [][2]string) (*mockBeadsClient, error) {
	t.Helper()
	mc := mockClientWithBead("gd-empty1")
	withMockClient(t, mc)
	resetUpdateFlags(t)
	t.Cleanup(func() { resetUpdateFlags(t) })
	for _, f := range flags {
		if err := updateCmd.Flags().Set(f[0], f[1]); err != nil {
			t.Fatalf("set --%s: %v", f[0], err)
		}
	}
	var err error
	_ = captureStdout(t, func() {
		err = updateCmd.RunE(updateCmd, []string{"gd-empty1"})
	})
	return mc, err
}

// TestUpdate_RefusesEmptyContent is the gd-EgvTGAozYB regression guard: a
// `-d "$(cat missing.md)"` reaches kd as -d "" and used to blank the bead's
// description. Every empty or blank content value is now refused before any
// write, unless --allow-empty says it is meant.
func TestUpdate_RefusesEmptyContent(t *testing.T) {
	dir := t.TempDir()
	emptyFile := filepath.Join(dir, "empty.md")
	if err := os.WriteFile(emptyFile, []byte("\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cases := []struct {
		name  string
		flags [][2]string
		want  string
	}{
		{"empty description", [][2]string{{"description", ""}}, "--description"},
		{"blank description", [][2]string{{"description", "  \n"}}, "--description"},
		{"description from empty file", [][2]string{{"description", "@" + emptyFile}}, "--description"},
		{"empty notes", [][2]string{{"notes", ""}}, "--notes"},
		{"empty field", [][2]string{{"field", "content="}}, `field "content"`},
		{"field from empty file", [][2]string{{"field", "content=@" + emptyFile}}, `field "content"`},
		{"one empty field among several", [][2]string{{"field", "model=claude"}, {"field", "prompt="}}, `field "prompt"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc, err := runUpdateWith(t, tc.flags)
			if err == nil {
				t.Fatal("expected an error refusing the empty value, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "--allow-empty") {
				t.Errorf("error %q should name %s and --allow-empty", err, tc.want)
			}
			if len(mc.UpdateBeadCalls) != 0 {
				t.Errorf("UpdateBead must not be called, got %d calls", len(mc.UpdateBeadCalls))
			}
		})
	}
}

// TestUpdate_AllowEmptyWritesEmpty asserts --allow-empty is the deliberate
// path to blank a description or store an empty field.
func TestUpdate_AllowEmptyWritesEmpty(t *testing.T) {
	mc, err := runUpdateWith(t, [][2]string{
		{"description", ""},
		{"field", "proxy_url="},
		{"allow-empty", "true"},
	})
	if err != nil {
		t.Fatalf("RunE with --allow-empty: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times, want 1", len(mc.UpdateBeadCalls))
	}
	req := mc.UpdateBeadCalls[0].Req
	if req.Description == nil || *req.Description != "" {
		t.Errorf("Description = %v, want an explicit empty string", req.Description)
	}
	var m map[string]any
	if err := json.Unmarshal(req.Fields, &m); err != nil {
		t.Fatalf("req.Fields not valid JSON: %s", req.Fields)
	}
	if v, ok := m["proxy_url"]; !ok || v != "" {
		t.Errorf("fields[proxy_url] = %v (present %v), want empty string", v, ok)
	}
}

// TestUpdate_NonEmptyContentStillWrites guards against the check catching
// ordinary updates: a real description, a real field, a JSON non-string field
// and --unset-field (the way to delete a field) all pass.
func TestUpdate_NonEmptyContentStillWrites(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "body.md")
	if err := os.WriteFile(body, []byte("# Plan\n\nnarrative\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	mc, err := runUpdateWith(t, [][2]string{
		{"description", "@" + body},
		{"field", "content=@" + body},
		{"field", "max_runs=0"},
		{"unset-field", "last_session_id"},
	})
	if err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(mc.UpdateBeadCalls) != 1 {
		t.Fatalf("UpdateBead called %d times, want 1", len(mc.UpdateBeadCalls))
	}
	if got := *mc.UpdateBeadCalls[0].Req.Description; got != "# Plan\n\nnarrative\n" {
		t.Errorf("Description = %q, want the file body", got)
	}
}
