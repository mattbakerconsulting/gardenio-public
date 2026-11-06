package golden

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"golang.org/x/image/font/sfnt"
)

// A golden is only portable between an agent pod and the CI runner when both draw it with the same browser and the same
// fonts (gd-VJWqCUihlQ). The capture pins both, but a machine can still slip past the pins: playwright-core from another
// node_modules (GARDENHUB_PW_MODULES) brings another Chromium, and a character no pinned face has falls back to whatever
// system font the machine has (the cost cards' "Open ↗" draws in Noto Sans Math on a pod, another face on the runner,
// 526-550 px apart). This file is how a gate finds out, so a bless that would turn CI red is refused instead of committed.

// Capture is what a capture script says about its run on stdout, besides its shots:
//
//	golden browser <chromium version> <playwright-core version>
//	golden text <shot file> {"glyphs":"...","families":["..."]}
//
// Text holds, per shot, the text it draws that may not be in a pinned face: its characters outside printable ASCII, and
// the font families of text whose stack starts with no pinned face.
type Capture struct {
	Chromium, Playwright string
	Text                 map[string]Drawn
}

// Drawn is one shot's line of text.
type Drawn struct {
	Glyphs   string   `json:"glyphs"`
	Families []string `json:"families"`
}

// ParseCapture reads a capture script's golden lines out of its output; other lines are ignored.
func ParseCapture(out []byte) Capture {
	c := Capture{Text: map[string]Drawn{}}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), " ", 4)
		switch {
		case len(f) == 4 && f[0] == "golden" && f[1] == "browser":
			c.Chromium, c.Playwright = f[2], f[3]
		case len(f) == 4 && f[0] == "golden" && f[1] == "text":
			var d Drawn
			if json.Unmarshal([]byte(f[3]), &d) == nil {
				c.Text[f[2]] = d
			}
		}
	}
	return c
}

// PinnedPlaywright is the playwright-core version CI installs: the one gardenio/tests/gardenhub-browser/package-lock.json
// pins, which in turn pins the Chromium build. lock is that file's path.
func PinnedPlaywright(lock string) (string, error) {
	b, err := os.ReadFile(lock)
	if err != nil {
		return "", err
	}
	var l struct {
		Packages map[string]struct{ Version string } `json:"packages"`
	}
	if err := json.Unmarshal(b, &l); err != nil {
		return "", fmt.Errorf("%s: %w", lock, err)
	}
	v := l.Packages["node_modules/playwright-core"].Version
	if v == "" {
		return "", fmt.Errorf("%s pins no playwright-core", lock)
	}
	return v, nil
}

// LockFile is package-lock.json beside the default playwright-core (Browser), for a test two directories under
// gardenio/controller.
func LockFile() string {
	p, _ := filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "package-lock.json"))
	return p
}

// BrowserSkew is why c's browser is not the one CI shoots with, "" when it is (or when the capture did not say).
func BrowserSkew(c Capture, pinned string) string {
	if c.Playwright == "" || c.Playwright == pinned {
		return ""
	}
	return fmt.Sprintf("this run's playwright-core is %s (Chromium %s), CI's is %s (package-lock.json): its Chromium draws differently, so its goldens would not match CI's",
		c.Playwright, c.Chromium, pinned)
}

// Pinned is the faces a capture splices into the page. Both of /garden's stacks list both families, so a character any
// face has is drawn by a pinned face; one none has falls back to a system font, which differs by machine.
type Pinned []*sfnt.Font

// LoadPinned parses every .ttf in dir.
func LoadPinned(dir string) (Pinned, error) {
	files, _ := filepath.Glob(filepath.Join(dir, "*.ttf"))
	if len(files) == 0 {
		return nil, fmt.Errorf("no .ttf in %s", dir)
	}
	var p Pinned
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		face, err := sfnt.Parse(b)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", f, err)
		}
		p = append(p, face)
	}
	return p, nil
}

// Missing is the characters of s no pinned face has, in order, each once. Characters that draw nothing (spaces,
// controls, format characters, variation selectors) are not missing.
func (p Pinned) Missing(s string) []rune {
	var out []rune
	var buf sfnt.Buffer
	seen := map[rune]bool{}
	for _, r := range s {
		if seen[r] || unicode.IsSpace(r) || unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Variation_Selector) {
			continue
		}
		seen[r] = true
		has := false
		for _, f := range p {
			if g, err := f.GlyphIndex(&buf, r); err == nil && g != 0 {
				has = true
				break
			}
		}
		if !has {
			out = append(out, r)
		}
	}
	return out
}

// Unpinned is, per shot of c, why some of its text is drawn by a system font rather than a pinned face; a shot whose text
// is all pinned has no entry. Such a shot's pixels depend on the machine's fonts.
func (p Pinned) Unpinned(c Capture) map[string]string {
	out := map[string]string{}
	for shot, d := range c.Text {
		var why []string
		if m := p.Missing(d.Glyphs); len(m) > 0 {
			var cs []string
			for _, r := range m {
				cs = append(cs, fmt.Sprintf("%c (U+%04X)", r, r))
			}
			why = append(why, "draws "+strings.Join(cs, ", ")+", in no pinned font")
		}
		if len(d.Families) > 0 {
			fs := append([]string(nil), d.Families...)
			sort.Strings(fs)
			why = append(why, "sets text in "+strings.Join(fs, "; ")+", no pinned face first")
		}
		if len(why) > 0 {
			out[shot] = strings.Join(why, "; and ")
		}
	}
	return out
}
