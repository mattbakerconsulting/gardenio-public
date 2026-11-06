package main

import (
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// TestWishViewerShoot is not a check: it shoots the viewer's two renderings of a Slack wish with its whole family
// (commonstest.WishFamily, the one the gardenhub card world serves; plan gd-gO1fTT7oGC step 1), its bead page /<head>
// and its fused thread /threads/<head>, at a phone's size and a desktop's, full page, into WISH_SHOTS, for the plan's
// before and after pictures beside TestCommonsUXShoot's of /garden. Runs only when WISH_SHOTS is set; same browser needs
// as TestReleasePagesUnderChromium.
func TestWishViewerShoot(t *testing.T) {
	out := os.Getenv("WISH_SHOTS")
	if out == "" {
		t.Skip("shoots the viewer's wish pages: WISH_SHOTS=<dir>")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	const head = "gd-wishSlack"
	f := newFake()
	wish := commonstest.WishFamily(head, "gardenio", "gd-chGeneral", time.Now().Add(-26*time.Hour))
	for _, b := range wish.Beads {
		f.beads[b.ID] = b
	}
	f.revDeps = map[string][]beadsapi.Dependency{}
	for _, d := range wish.Deps {
		f.deps[d.BeadID] = append(f.deps[d.BeadID], d)
		f.revDeps[d.DependsOnID] = append(f.revDeps[d.DependsOnID], d)
	}
	for id, cs := range wish.Comments {
		f.comments[id] = cs
	}
	hs := httptest.NewServer(mkServer(t, f))
	t.Cleanup(hs.Close)
	out, _ = filepath.Abs(out)
	script, _ := filepath.Abs(filepath.Join("testdata", "wish_shoot_browser.js"))
	b, err := exec.Command(node, script, hs.URL, mods, out, head).CombinedOutput()
	t.Logf("%s", strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatalf("wish_shoot_browser.js: %v", err)
	}
}
