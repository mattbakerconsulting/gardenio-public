package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The command writes the three files for an origin, and refuses an -app-origin that is not one.
func TestRun(t *testing.T) {
	dir := t.TempDir()
	if err := run(dir, "https://gardenio.clonk.tv/"); err != nil {
		t.Fatal(err)
	}
	page, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), `href="https://gardenio.clonk.tv/lands/"`) {
		t.Errorf("index.html's invite is not on the origin")
	}
	for _, f := range []string{"three.module.min.js", "still.png"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	for _, bad := range []string{"", "gardenio.clonk.tv", "ftp://gardenio.clonk.tv", "https://gardenio.clonk.tv/app", "https://gardenio.clonk.tv?x=1"} {
		if err := run(t.TempDir(), bad); err == nil {
			t.Errorf("-app-origin %q accepted", bad)
		}
	}
}
