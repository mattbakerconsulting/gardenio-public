package main

import (
	"bytes"
	"compress/gzip"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
)

// mermaidBundleGzipBudget caps the vendored mermaid library at 1100 KB gzipped.
// mermaid v11.16.0 UMD is ~975 KB gz today; the budget leaves ~12% headroom so
// an accidental un-minified or source-map-bloated re-vendor trips CI. The lib is
// LAZY-loaded (mermaid_render.js fetches it only on pages that contain a
// diagram), so the raw ~3.5 MB is never served to diagram-free pages. Bumping
// this budget requires updating the plan-of-record bead (ka-0000005977).
const mermaidBundleGzipBudget = 1100 * 1024

// requireVendoredJS fails with the fix when fetch-vendored.sh has not run:
// mermaid.min.js and echarts.min.js are fetched at build time, not committed
// (gd-wNQzR9ptZI), and a 404 or a missing-file error does not say so.
func requireVendoredJS(t *testing.T) {
	t.Helper()
	if missing := missingVendoredJS(); len(missing) > 0 {
		t.Fatalf("%v not embedded: run cmd/beads-viewer/fetch-vendored.sh (or make fetch-viewer-vendor) and re-run", missing)
	}
}

// TestMermaidAssets_ServedStatic asserts both the vendored library and the
// driver are embedded and served, or diagram rendering regresses silently.
// Mirrors TestEChartsAssets_ServedStatic.
func TestMermaidAssets_ServedStatic(t *testing.T) {
	requireVendoredJS(t)
	mux := mkServer(t, newFake())
	cases := []struct {
		path    string
		minSize int
	}{
		{"/assets/static/mermaid.min.js", 500000},  // vendored lib is ~3.5 MB
		{"/assets/static/mermaid_render.js", 1000}, // the driver
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Errorf("%s: got %d want 200", tc.path, w.Code)
			continue
		}
		if got := w.Body.Len(); got < tc.minSize {
			t.Errorf("%s suspiciously small: %d bytes (want >= %d)", tc.path, got, tc.minSize)
		}
	}
}

// TestMermaidBundleSize asserts the vendored mermaid.min.js compresses under the
// budget. Reads from disk so a CI run that regenerates the bundle still gates on
// size. Copies TestXtermBundleSize.
func TestMermaidBundleSize(t *testing.T) {
	requireVendoredJS(t)
	raw, err := os.ReadFile("templates/static/mermaid.min.js")
	if err != nil {
		t.Fatalf("read mermaid.min.js: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("mermaid.min.js is empty — vendor regression?")
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
	if gz > mermaidBundleGzipBudget {
		t.Errorf("mermaid.min.js gzipped = %d bytes, budget = %d bytes (%.1f KB over)",
			gz, mermaidBundleGzipBudget, float64(gz-mermaidBundleGzipBudget)/1024)
	}
	t.Logf("mermaid.min.js: raw=%d B (%.1f KB) gz=%d B (%.1f KB) budget=%d B",
		len(raw), float64(len(raw))/1024, gz, float64(gz)/1024, mermaidBundleGzipBudget)
}

// mermaidSecureArrayRe matches mermaid's default-config `secure:[...]` array
// literal and requires `securityLevel` to be one of its members. That deny-list
// is what prevents a diagram-embedded %%{init:{securityLevel:'loose'}}%%
// directive from downgrading strict mode — the entire non-override guarantee is
// a property of THIS exact vendored version.
var mermaidSecureArrayRe = regexp.MustCompile(`secure:\[[^\]]*"securityLevel"[^\]]*\]`)

// TestMermaidVersionPin (must-fix) fails if the vendored bundle no longer pins
// `securityLevel` inside its `secure:[...]` deny-list, or drops the runtime
// guard that enforces it. A vendor bump that loosens directive sanitization must
// break CI, since strict-mode-cannot-be-overridden is the core of the XSS
// argument (design of record on ka-0000005977).
func TestMermaidVersionPin(t *testing.T) {
	requireVendoredJS(t)
	raw, err := os.ReadFile("templates/static/mermaid.min.js")
	if err != nil {
		t.Fatalf("read mermaid.min.js: %v", err)
	}
	if !mermaidSecureArrayRe.Match(raw) {
		t.Error(`vendored mermaid.min.js no longer pins "securityLevel" inside a secure:[...] array — ` +
			`a diagram %%{init:{securityLevel:'loose'}}%% could now downgrade strict mode. ` +
			`Re-audit before bumping the vendored version (ka-0000005977).`)
	}
	if !bytes.Contains(raw, []byte("Denied attempt to modify a secure key")) {
		t.Error(`vendored mermaid.min.js no longer contains the "Denied attempt to modify a secure key" ` +
			`guard — the secure-key enforcement path may have changed. Re-audit before bumping.`)
	}
}

// TestMermaidNotEagerlyLinked (lazy-load invariant) asserts layout.html loads the
// tiny driver but NEVER script-tags the ~975 KB-gz library — the driver fetches
// it on demand only when a diagram is present, so diagram-free pages pay nothing.
func TestMermaidNotEagerlyLinked(t *testing.T) {
	raw, err := templateFS.ReadFile("templates/layout.html")
	if err != nil {
		t.Fatalf("read layout.html: %v", err)
	}
	if !bytes.Contains(raw, []byte(`/assets/static/mermaid_render.js`)) {
		t.Error("layout.html should <script>-tag the mermaid_render.js driver")
	}
	// The heavy lib must NOT appear inside a <script src=...> — a naive grep for
	// the filename would false-match the driver's own lazy-load path string in a
	// comment, so assert specifically on the script-src form.
	if bytes.Contains(raw, []byte(`static/mermaid.min.js" defer`)) ||
		bytes.Contains(raw, []byte(`static/mermaid.min.js" async`)) ||
		bytes.Contains(raw, []byte(`static/mermaid.min.js"></script>`)) {
		t.Error("layout.html must NOT eagerly script-tag mermaid.min.js — it is lazy-loaded by the driver")
	}
}
