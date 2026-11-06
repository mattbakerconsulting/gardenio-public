package main

import (
	"bytes"
	"compress/gzip"
	"io/fs"
	"testing"

	"gardenio/controller/internal/gardenhub"
)

// xtermBundleGzipBudget caps the served xterm.js bundle at 125 KB gzipped.
// Phase 1 plan-of-record: a stricter budget than the typical xterm payload
// (~66 KB gz today) catches regressions before they ship to clients on
// slow links. Bumping this requires updating the plan-of-record bead.
const xtermBundleGzipBudget = 125 * 1024

// TestXtermBundleSize asserts the vendored xterm.bundle.js compresses
// under the budget. It is gardenhub's (web/vendor), embedded, which both
// the Glass and the agent page serve.
func TestXtermBundleSize(t *testing.T) {
	raw, err := fs.ReadFile(gardenhub.XtermFS, "xterm.bundle.js")
	if err != nil {
		t.Fatalf("read xterm.bundle.js: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("xterm.bundle.js is empty — vendor regression?")
	}

	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	gz := buf.Len()
	if gz > xtermBundleGzipBudget {
		t.Errorf("xterm.bundle.js gzipped = %d bytes, budget = %d bytes (%.1f KB over)",
			gz, xtermBundleGzipBudget, float64(gz-xtermBundleGzipBudget)/1024)
	}
	t.Logf("xterm.bundle.js: raw=%d B (%.1f KB) gz=%d B (%.1f KB) budget=%d B",
		len(raw), float64(len(raw))/1024, gz, float64(gz)/1024, xtermBundleGzipBudget)
}
