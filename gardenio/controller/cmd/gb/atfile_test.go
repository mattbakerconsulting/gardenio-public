package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveTextValue covers the curl-style @file / @- / @@ contract that
// `gb` must keep byte-for-byte compatible with `kd`'s resolveTextValue
// (kbeads/cmd/kd/fields.go). Same prefixes, same escape, same fail-loud
// behaviour so an agent's muscle memory carries across the two CLIs
// (ka-0000003861).
func TestResolveTextValue(t *testing.T) {
	dir := t.TempDir()
	body := "# Report body\n\nActual content read from a file.\n"
	path := filepath.Join(dir, "report.md")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write body: %v", err)
	}

	t.Run("plain value is verbatim", func(t *testing.T) {
		got, err := resolveTextValue("just some inline text")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != "just some inline text" {
			t.Errorf("got %q, want the value unchanged", got)
		}
	})

	t.Run("@file reads the file body", func(t *testing.T) {
		got, err := resolveTextValue("@" + path)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != body {
			t.Errorf("got %q, want the file body %q", got, body)
		}
	})

	t.Run("@@ escapes a literal leading @", func(t *testing.T) {
		got, err := resolveTextValue("@@mention this is a literal")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if want := "@mention this is a literal"; got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("missing @file fails loud", func(t *testing.T) {
		_, err := resolveTextValue("@" + filepath.Join(dir, "does-not-exist.md"))
		if err == nil {
			t.Fatal("expected an error for @missing-file, got nil")
		}
	})
}
