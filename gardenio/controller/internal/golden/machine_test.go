package golden

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseCapture(t *testing.T) {
	c := ParseCapture([]byte("golden browser 145.0.7632.6 1.58.2\nshot costs-phone-light.png\n" +
		`golden text costs-phone-light.png {"glyphs":"↗·","families":["system-ui"]}` + "\ngolden text broken.png {\ngolden shots taken\n"))
	if c.Chromium != "145.0.7632.6" || c.Playwright != "1.58.2" {
		t.Errorf("browser = %q %q", c.Chromium, c.Playwright)
	}
	if d := c.Text["costs-phone-light.png"]; d.Glyphs != "↗·" || len(d.Families) != 1 || d.Families[0] != "system-ui" {
		t.Errorf("text = %+v", d)
	}
	if _, ok := c.Text["broken.png"]; ok || len(c.Text) != 1 {
		t.Errorf("a line that is not JSON is ignored, got %v", c.Text)
	}
}

func TestPinnedPlaywrightAndSkew(t *testing.T) {
	v, err := PinnedPlaywright(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "package-lock.json"))
	if err != nil || v == "" {
		t.Fatalf("PinnedPlaywright = %q, %v", v, err)
	}
	if s := BrowserSkew(Capture{Chromium: "1", Playwright: v}, v); s != "" {
		t.Errorf("the pinned playwright-core is no skew, got %q", s)
	}
	if s := BrowserSkew(Capture{}, v); s != "" {
		t.Errorf("a capture that said nothing is no skew, got %q", s)
	}
	if s := BrowserSkew(Capture{Chromium: "147.0.1", Playwright: "1.60.0"}, v); !strings.Contains(s, "1.60.0") || !strings.Contains(s, "147.0.1") || !strings.Contains(s, v) {
		t.Errorf("skew = %q, want both versions and Chromium's", s)
	}
	if _, err := PinnedPlaywright(filepath.Join(t.TempDir(), "none.json")); err == nil {
		t.Error("a missing lock file is an error")
	}
}

func TestPinnedUnpinned(t *testing.T) {
	faces, err := LoadPinned(filepath.Join("..", "gardenhub", "testdata", "golden", "fonts"))
	if err != nil {
		t.Fatal(err)
	}
	// ↗ is in no pinned face (the cost cards' "Open ↗", gd-VJWqCUihlQ); → is in Noto Sans Mono; · × – are in Noto Sans;
	// a space, a zero-width space and a variation selector draw nothing
	if m := faces.Missing("Open ↗ → · × – ​️ ↗"); string(m) != "↗" {
		t.Errorf("Missing = %q, want ↗ once", string(m))
	}
	got := faces.Unpinned(Capture{Text: map[string]Drawn{
		"costs-phone-light.png": {Glyphs: "↗·"},
		"glass-phone-light.png": {Families: []string{"monospace", "Menlo"}},
		"bead-phone-light.png":  {Glyphs: "·→"},
	}})
	if len(got) != 2 {
		t.Fatalf("Unpinned = %v, want the costs and glass shots only", got)
	}
	if !strings.Contains(got["costs-phone-light.png"], "↗ (U+2197)") || strings.Contains(got["costs-phone-light.png"], "·") {
		t.Errorf("costs: %q", got["costs-phone-light.png"])
	}
	if !strings.Contains(got["glass-phone-light.png"], "Menlo; monospace") {
		t.Errorf("glass: %q", got["glass-phone-light.png"])
	}
}

// A shot whose text a system font draws is not rewritten under Update when it differs: its pixels are this machine's.
func TestCheckUnpinnedNotUpdated(t *testing.T) {
	dir := t.TempDir()
	shots, goldens := filepath.Join(dir, "shots"), filepath.Join(dir, "goldens")
	for _, d := range []string{shots, goldens} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p string, c color.Color) []byte {
		img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
		for i := 0; i < 64; i++ {
			img.Set(i%8, i/8, c)
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	black, white := color.NRGBA{A: 255}, color.NRGBA{R: 255, G: 255, B: 255, A: 255}
	for _, n := range []string{"costs-phone-light.png", "bead-phone-light.png", "costs-phone-dark.png"} {
		write(filepath.Join(goldens, n), black)
		write(filepath.Join(shots, n), white)
	}
	write(filepath.Join(shots, "costs-phone-dark.png"), black)    // unpinned, but matches: nothing to say
	write(filepath.Join(shots, "costs-desktop-light.png"), white) // unpinned and new: no golden written either
	unpinned := map[string]string{"costs-phone-light.png": "draws ↗ (U+2197), in no pinned font", "costs-phone-dark.png": "draws ↗ (U+2197), in no pinned font",
		"costs-desktop-light.png": "draws ↗ (U+2197), in no pinned font"}
	r, err := Check(Options{Shots: shots, Goldens: goldens, Artifacts: filepath.Join(dir, "arts"), Update: true,
		Surfaces: map[string]Tol{"costs": Default, "bead": Default}, Unpinned: unpinned})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failed) != 2 || !strings.HasPrefix(r.Failed[0], "costs-desktop-light.png: no golden; it draws ↗") ||
		!strings.HasPrefix(r.Failed[1], "costs-phone-light.png: 64 of 64 pixels differ") || !strings.Contains(r.Failed[1], "garden-goldens-from-ci.sh") {
		t.Errorf("Failed = %q", r.Failed)
	}
	if b, _ := os.ReadFile(filepath.Join(goldens, "costs-phone-light.png")); bytes.Equal(b, mustRead(t, filepath.Join(shots, "costs-phone-light.png"))) {
		t.Error("the unpinned golden was rewritten")
	}
	if _, err := os.Stat(filepath.Join(goldens, "costs-desktop-light.png")); err == nil {
		t.Error("an unpinned new shot got a golden")
	}
	if b, _ := os.ReadFile(filepath.Join(goldens, "bead-phone-light.png")); !bytes.Equal(b, mustRead(t, filepath.Join(shots, "bead-phone-light.png"))) {
		t.Error("a pinned shot that differs is still rewritten")
	}
	if _, err := os.Stat(filepath.Join(dir, "arts", "costs-phone-light.actual.png")); err != nil {
		t.Errorf("the refused shot's artifacts: %v", err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A failing shot (or a golden with no shot) .github/quarantine.d names is logged and keeps its artifacts but does not fail; any other still fails.
func TestCheckQuarantine(t *testing.T) {
	dir := t.TempDir()
	shots, goldens := filepath.Join(dir, "shots"), filepath.Join(dir, "goldens")
	for _, d := range []string{shots, goldens} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	flat := func(p string, v uint8) {
		img := image.NewNRGBA(image.Rect(0, 0, 8, 8))
		for i := 0; i < 64; i++ {
			img.Set(i%8, i/8, color.NRGBA{R: v, G: v, B: v, A: 255})
		}
		var b bytes.Buffer
		if err := png.Encode(&b, img); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"land-phone-dark.png", "land-phone-light.png", "wall-desktop-dark.png"} {
		flat(filepath.Join(goldens, n), 0)
		flat(filepath.Join(shots, n), 255)
	}
	flat(filepath.Join(shots, "wall-desktop-dark.png"), 0)   // quarantined, but passes
	flat(filepath.Join(goldens, "land-desktop-dark.png"), 0) // quarantined, and no surface shoots it any more
	tsv := filepath.Join(dir, "quarantine.tsv")
	rows := "# kind\tname\tbead\n# golden\tland-phone-light.png\tgd-commented\n" +
		"golden\tland-phone-dark.png\tgd-x\t2026-11-01\t2026-11-15\trun\ngolden\twall-desktop-dark.png\tgd-y\ngo\tland-phone-light.png\tgd-z\n" +
		"golden\tland-desktop-dark.png\tgd-w\n"
	if err := os.WriteFile(tsv, []byte(rows), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARDENIO_QUARANTINE", tsv)
	r, err := Check(Options{Shots: shots, Goldens: goldens, Artifacts: filepath.Join(dir, "arts"), Surfaces: map[string]Tol{"land": Default, "wall": Default}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Failed) != 1 || !strings.HasPrefix(r.Failed[0], "land-phone-light.png: 64 of 64") {
		t.Errorf("Failed = %q, want only the unquarantined shot", r.Failed)
	}
	if !slices.Equal(r.Quarantined, []string{"land-phone-dark.png", "land-desktop-dark.png"}) {
		t.Errorf("Quarantined = %q", r.Quarantined)
	}
	logs := strings.Join(r.Logs, "\n")
	if !strings.Contains(logs, "QUARANTINED land-phone-dark.png: 64 of 64 pixels differ, tolerance 16 (gd-x,") ||
		!strings.Contains(logs, "quarantined wall-desktop-dark.png passed; delete .github/quarantine.d/golden-wall-desktop-dark.png.tsv once gd-y is fixed") ||
		!strings.Contains(logs, "QUARANTINED land-desktop-dark.png: golden with no shot (a surface was renamed or removed) (gd-w,") {
		t.Errorf("Logs:\n%s", logs)
	}
	if _, err := os.Stat(filepath.Join(dir, "arts", "land-phone-dark.actual.png")); err != nil {
		t.Errorf("the quarantined shot's artifacts: %v", err)
	}
	t.Setenv("GARDENIO_QUARANTINE", filepath.Join(dir, "missing.tsv"))
	if _, err := Check(Options{Shots: shots, Goldens: goldens, Artifacts: filepath.Join(dir, "arts"), Surfaces: map[string]Tol{"land": Default, "wall": Default}}); err == nil {
		t.Error("a named but missing quarantine file did not error")
	}
}

// GARDENHUB_GOLDEN_CELLS (Options.Cells): a golden of a cell the run did not shoot is neither compared nor stale, so blessing
// the phone turned sideways alone (M4.1 gd-T3JPWCD9xM) does not remove every other cell's golden; one of a cell it did
// shoot, with no shot, still is.
func TestCheckCells(t *testing.T) {
	dir := t.TempDir()
	shots, goldens := filepath.Join(dir, "shots"), filepath.Join(dir, "goldens")
	for _, d := range []string{shots, goldens} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewNRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(shots, "bead-phoneland-light.png"), filepath.Join(goldens, "bead-phoneland-light.png"),
		filepath.Join(goldens, "bead-phone-light.png"), filepath.Join(goldens, "bead-desktop-dark.png"), filepath.Join(goldens, "pad-phoneland-light.png")} {
		if err := os.WriteFile(p, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Check(Options{Shots: shots, Goldens: goldens, Artifacts: filepath.Join(dir, "arts"), Update: true,
		Surfaces: map[string]Tol{"bead": Default, "pad": Default}, Cells: OnlySet("phoneland-light")})
	if err != nil {
		t.Fatal(err)
	}
	if r.Shots != 1 || len(r.Failed) != 0 {
		t.Errorf("Shots = %d, Failed = %q", r.Shots, r.Failed)
	}
	for _, n := range []string{"bead-phone-light.png", "bead-desktop-dark.png"} {
		if _, err := os.Stat(filepath.Join(goldens, n)); err != nil {
			t.Errorf("%s, a cell the run did not shoot, was removed", n)
		}
	}
	if _, err := os.Stat(filepath.Join(goldens, "pad-phoneland-light.png")); err == nil {
		t.Error("pad-phoneland-light.png, in a shot cell with no shot, was kept")
	}
	if got := Cell("commonsfacelive-phoneland-light.png"); got != "phoneland-light" {
		t.Errorf("Cell = %q", got)
	}
}
