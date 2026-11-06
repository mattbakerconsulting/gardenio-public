// Package golden is the comparison half of the golden-screenshot gates (gd-D953GccBTY): a browser writes one PNG per shot,
// and Check holds each against the committed golden of the same name under a per-surface pixel tolerance. Both gates use
// it: /garden (internal/gardenhub TestGardenGoldens) and the beads viewer's empty composer (cmd/beads-viewer
// TestComposerGoldens). How to read and update a failure: gardenio/tests/gardenhub-browser/GOLDENS.md.
package golden

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// Tol is how far a shot may drift from its golden. A pixel differs when any of its RGBA channels moves by more than
// Channel (0-255), which absorbs anti-aliasing noise; the shot fails when more than Pixels of them differ. A budget in
// pixels, not a share of the frame: one letter changed in the /garden card ("Gardener" to "Gardenet") is about 120 pixels,
// which is 0.012% of the desktop frame and would hide under any ratio loose enough to sound safe. Measured drift between an
// agent pod and the CI runner is 0 on every shot (ci.yml run 39208096382), so 16 is headroom, not slack.
type Tol struct {
	Channel int
	Pixels  int
}

// Default is the tolerance every surface starts at. Loosen one only with a measured reason in its comment.
var Default = Tol{Channel: 40, Pixels: 16}

// Options says where the shots and goldens are and how to judge them.
type Options struct {
	Shots     string // the PNGs the browser just wrote
	Goldens   string // the committed goldens, same file names
	Artifacts string // where actual/expected/diff PNGs of each failing shot go
	Update    bool   // rewrite the goldens that fail and remove stale ones instead of failing
	// Surfaces gives each shot's tolerance, keyed by its file name's first word (<surface>-<device>-<theme>.png). A shot
	// whose surface has no row fails, so a new surface cannot land without choosing its tolerance.
	Surfaces map[string]Tol
	// Only, when set, names the surfaces this run shot (a developer regenerating one surface on a small machine,
	// GARDENHUB_GOLDEN_ONLY): a golden of another surface is neither compared nor stale. Empty is every surface.
	Only map[string]bool
	// Cells, when set, names the cells this run shot (<device>-<theme>, GARDENHUB_GOLDEN_CELLS): as Only, a golden of
	// another cell is neither compared nor stale. Empty is every cell.
	Cells map[string]bool
	// Unpinned, per shot file, says why some of its text is drawn by a system font (Pinned.Unpinned): its pixels are this
	// machine's, not CI's. Such a shot that fails is not rewritten under Update; it fails, pointing at CI's render.
	Unpinned map[string]string
}

// Surface is a shot's or golden's surface: its file name's first word (<surface>-<device>-<theme>.png).
func Surface(name string) string {
	return strings.SplitN(strings.TrimSuffix(name, ".png"), "-", 2)[0]
}

// Cell is a shot's or golden's cell: its file name's device and theme (<surface>-<device>-<theme>.png).
func Cell(name string) string {
	parts := strings.SplitN(strings.TrimSuffix(name, ".png"), "-", 2)
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}

// OnlySet parses a GARDENHUB_GOLDEN_ONLY value: surface names, comma separated; nil when empty (every surface).
func OnlySet(v string) map[string]bool {
	var out map[string]bool
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[s] = true
		}
	}
	return out
}

// Report is what Check found: Logs is one line per shot compared or updated, Failed one line per failing shot (sorted).
// Quarantined is the failing shots .github/quarantine.d names: logged, their artifacts saved, but not in Failed.
type Report struct {
	Shots       int
	Logs        []string
	Failed      []string
	Quarantined []string
}

// Quarantine reads the golden rows of a known-failing tests list (.github/quarantine.d/*.tsv, which scripts/quarantine.sh fetch
// concatenates into the file CI puts in GARDENIO_QUARANTINE): shot file name to bug bead. An unset path is no quarantine, so a laptop run sees every failure.
func Quarantine(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	q := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Split(l, "\t"); !strings.HasPrefix(l, "#") && len(f) >= 3 && f[0] == "golden" {
			q[f[1]] = f[2]
		}
	}
	return q, nil
}

// Check compares every Shots/*.png with Goldens/<same name>. A shot over its tolerance, a shot with no golden and a golden
// with no shot all fail unless quarantined, and each failing shot's actual, expected and diff PNGs land in Artifacts. With Update those
// goldens are rewritten (or removed) instead: an unchanged shot keeps its bytes, so a PR does not churn PNGs it did not
// touch. The error is for broken plumbing (unreadable files, no shots at all), not for drift.
func Check(o Options) (Report, error) {
	var r Report
	if err := os.RemoveAll(o.Artifacts); err != nil {
		return r, err
	}
	quarantined, err := Quarantine(os.Getenv("GARDENIO_QUARANTINE"))
	if err != nil {
		return r, err
	}
	got, _ := filepath.Glob(filepath.Join(o.Shots, "*.png"))
	want, _ := filepath.Glob(filepath.Join(o.Goldens, "*.png"))
	if len(got) == 0 {
		return r, fmt.Errorf("the capture wrote no shots to %s", o.Shots)
	}
	r.Shots = len(got)
	shot := map[string]bool{}
	for _, a := range got {
		name := filepath.Base(a)
		shot[name] = true
		tol, ok := o.Surfaces[Surface(name)]
		if !ok {
			r.Failed = append(r.Failed, name+": no tolerance row for its surface")
			continue
		}
		actual, err := os.ReadFile(a)
		if err != nil {
			return r, err
		}
		g := filepath.Join(o.Goldens, name)
		expected, err := os.ReadFile(g)
		var verdict string
		var diff image.Image
		switch {
		case os.IsNotExist(err):
			verdict = "no golden"
		case err != nil:
			return r, err
		default:
			n, total, d, err := PixelDiff(expected, actual, tol.Channel)
			if err != nil {
				verdict = err.Error()
				break
			}
			r.Logs = append(r.Logs, fmt.Sprintf("%-28s %7d of %7d px differ (tolerance %d)", name, n, total, tol.Pixels))
			if n <= tol.Pixels {
				if bead := quarantined[name]; bead != "" && !o.Update {
					r.Logs = append(r.Logs, fmt.Sprintf("quarantined %s passed; delete .github/quarantine.d/golden-%s.tsv once %s is fixed", name, name, bead))
				}
				continue
			}
			verdict, diff = fmt.Sprintf("%d of %d pixels differ, tolerance %d", n, total, tol.Pixels), d
		}
		if why := o.Unpinned[name]; why != "" {
			verdict += "; it " + why + ", so its pixels depend on this machine's fonts"
			if o.Update {
				verdict += ": not rewritten, take CI's render (gardenio/scripts/garden-goldens-from-ci.sh)"
			}
		}
		if o.Update && o.Unpinned[name] == "" {
			if err := os.WriteFile(g, actual, 0o644); err != nil {
				return r, err
			}
			r.Logs = append(r.Logs, fmt.Sprintf("%s: updated (%s)", name, verdict))
			continue
		}
		if bead := quarantined[name]; bead != "" {
			r.Quarantined = append(r.Quarantined, name)
			r.Logs = append(r.Logs, fmt.Sprintf("QUARANTINED %s: %s (%s, .github/quarantine.d); not failing the test", name, verdict, bead))
		} else {
			r.Failed = append(r.Failed, name+": "+verdict)
		}
		if err := saveArtifacts(o.Artifacts, name, actual, expected, diff); err != nil {
			return r, err
		}
	}
	for _, g := range want {
		name := filepath.Base(g)
		if shot[name] || (o.Only != nil && !o.Only[Surface(name)]) || (o.Cells != nil && !o.Cells[Cell(name)]) {
			continue
		}
		if o.Update {
			if err := os.Remove(g); err != nil {
				return r, err
			}
			r.Logs = append(r.Logs, name+": removed, no surface shoots it any more")
			continue
		}
		verdict := "golden with no shot (a surface was renamed or removed)"
		if bead := quarantined[name]; bead != "" {
			r.Quarantined = append(r.Quarantined, name)
			r.Logs = append(r.Logs, fmt.Sprintf("QUARANTINED %s: %s (%s, .github/quarantine.d); not failing the test", name, verdict, bead))
			continue
		}
		r.Failed = append(r.Failed, name+": "+verdict)
	}
	sort.Strings(r.Failed)
	return r, nil
}

// PixelDiff counts the pixels of actual whose RGBA moved by more than channel from expected, and paints a diff: differing
// pixels red over a faded copy of the golden. Images of different sizes are an error, not a count.
func PixelDiff(expected, actual []byte, channel int) (n, total int, diff image.Image, err error) {
	e, err := decodeNRGBA(expected)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("golden: %w", err)
	}
	a, err := decodeNRGBA(actual)
	if err != nil {
		return 0, 0, nil, fmt.Errorf("shot: %w", err)
	}
	if e.Rect.Size() != a.Rect.Size() {
		return 0, 0, nil, fmt.Errorf("size %v, golden %v", a.Rect.Size(), e.Rect.Size())
	}
	d := image.NewNRGBA(e.Rect)
	for i := 0; i < len(e.Pix); i += 4 {
		far := false
		for c := 0; c < 4; c++ {
			if delta := int(e.Pix[i+c]) - int(a.Pix[i+c]); delta > channel || -delta > channel {
				far = true
			}
		}
		if far {
			n++
			copy(d.Pix[i:i+4], []byte{255, 0, 0, 255})
			continue
		}
		y := (299*int(e.Pix[i]) + 587*int(e.Pix[i+1]) + 114*int(e.Pix[i+2])) / 1000
		g := uint8(255 - (255-y)/4)
		copy(d.Pix[i:i+4], []byte{g, g, g, 255})
	}
	return n, len(e.Pix) / 4, d, nil
}

func decodeNRGBA(b []byte) (*image.NRGBA, error) {
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	if n, ok := img.(*image.NRGBA); ok && n.Rect.Min == (image.Point{}) {
		return n, nil
	}
	n := image.NewNRGBA(image.Rectangle{Max: img.Bounds().Size()})
	draw.Draw(n, n.Rect, img, img.Bounds().Min, draw.Src)
	return n, nil
}

// saveArtifacts writes <name>.actual.png, .expected.png (when there is a golden) and .diff.png (when the sizes agree).
func saveArtifacts(dir, name string, actual, expected []byte, diff image.Image) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(dir, strings.TrimSuffix(name, ".png"))
	if err := os.WriteFile(base+".actual.png", actual, 0o644); err != nil {
		return err
	}
	if expected != nil {
		if err := os.WriteFile(base+".expected.png", expected, 0o644); err != nil {
			return err
		}
	}
	if diff == nil {
		return nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, diff); err != nil {
		return err
	}
	return os.WriteFile(base+".diff.png", buf.Bytes(), 0o644)
}

// Browser finds what a capture script needs, for a test two directories under gardenio/controller (internal/gardenhub,
// cmd/beads-viewer): node on PATH and playwright-core in GARDENHUB_PW_MODULES (default
// gardenio/tests/gardenhub-browser/node_modules). skip is why the gate cannot run here, "" when it can. The gates run only
// when GARDENHUB_GOLDENS=1 (`make test-garden-goldens` sets it) and only on Linux, where the goldens were rendered.
func Browser() (node, mods, skip string) {
	if os.Getenv("GARDENHUB_GOLDENS") != "1" {
		return "", "", "golden screenshots run under `make test-garden-goldens` (GARDENHUB_GOLDENS=1)"
	}
	if runtime.GOOS != "linux" {
		return "", "", fmt.Sprintf("the goldens are rendered on linux; on %s use gardenio/scripts/garden-goldens-from-ci.sh", runtime.GOOS)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		return "", "", "node not installed"
	}
	mods = os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		return "", "", fmt.Sprintf("playwright-core not installed under %s (make test-garden-goldens)", mods)
	}
	return node, mods, ""
}

// ArtifactsDir is where a gate's failing shots go: <GARDENHUB_GOLDEN_ARTIFACTS>/<gate> (default
// gardenio/tests/gardenhub-browser/golden-diffs/<gate>). One subdirectory per gate, so the gates cannot clear each other's
// and garden-goldens-from-ci.sh knows which golden directory each actual belongs to.
func ArtifactsDir(gate string) string {
	root := os.Getenv("GARDENHUB_GOLDEN_ARTIFACTS")
	if root == "" {
		root, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "golden-diffs"))
	}
	return filepath.Join(root, gate)
}
