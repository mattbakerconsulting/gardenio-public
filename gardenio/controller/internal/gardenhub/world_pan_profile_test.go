package gardenhub

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestWorldPanProfile is the World's render profile on the 10x farm (Lands performance pass, step P2.1 gd-PoCdbE1SJX):
// it serves the goldens' world and runs testdata/world_pan_profile.js against it, which swaps the page's /garden/farm
// document for testdata/world_tenx.js's and reads idle, a 10 s pan and a zoom in Chromium on the throttled phone, with the
// gate that no long task in the pan runs over 100 ms. The script's header lists what it measures and the knobs to dig in
// with; gardenio/docs/lands-perf-world-render.md has the last results and how to read them.
//
// Runs only when WORLD_PAN_OUT is set (`make world-pan-profile OUT=<dir>`): it is a measurement, not a check, as phone CPU
// numbers on a shared runner move with the node's load. Same browser needs as TestLandsPerfBaseline.
func TestWorldPanProfile(t *testing.T) {
	out := os.Getenv("WORLD_PAN_OUT")
	if out == "" {
		t.Skip("profiles the World on the 10x farm for the Lands performance pass: `make world-pan-profile OUT=<dir>` (WORLD_PAN_OUT)")
	}
	srv := cardWorld(t, func() time.Time { return goldenNow }, goldenPorch()...)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		t.Fatalf("playwright-core not installed under %s (make gardenhub-browser-deps)", mods)
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "world_pan_profile.js"))
	out, _ = filepath.Abs(out)
	cmd := exec.Command(node, script, srv.URL, mods, out)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("world_pan_profile.js: %v (report in %s)", err, filepath.Join(out, "report.json"))
	}
}
