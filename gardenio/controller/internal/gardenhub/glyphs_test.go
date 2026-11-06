package gardenhub

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/image/font/sfnt"
)

// TestPageTextInGoldenFonts guards gd-SgTgIGaeKk, the page-wide twin of the
// Commons' TestRenderedTextInGoldenFonts (internal/commons/glyphs_test.go):
// every character garden.html writes (its entities decoded) and every
// content: string in web/css/*.css (its escapes decoded) must be in the Noto Sans
// subsets the goldens pin (testdata/golden/fonts). A character missing there
// falls back to a system font, which differs between an agent pod and the CI
// runner, so the goldens drift by machine instead of failing here. Draw a
// symbol as a CSS mask in currentColor (.dsum::before), or re-subset the
// fonts (GOLDENS.md).
func TestPageTextInGoldenFonts(t *testing.T) {
	names := []string{"NotoSans-Regular", "NotoSans-Bold"}
	var faces []*sfnt.Font
	for _, f := range names {
		b, err := os.ReadFile(filepath.Join("testdata", "golden", "fonts", f+".ttf"))
		if err != nil {
			t.Fatal(err)
		}
		face, err := sfnt.Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		faces = append(faces, face)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("web", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	texts := map[string]string{}
	for i, line := range strings.Split(read("garden.html"), "\n") {
		texts["garden.html:"+strconv.Itoa(i+1)] = html.UnescapeString(line)
	}
	cssEscape := regexp.MustCompile(`\\([0-9a-fA-F]{1,6}) ?`)
	content := regexp.MustCompile(`content:\s*"((?:[^"\\]|\\.)*)"`)
	sheets, _ := filepath.Glob(filepath.Join("web", "css", "*.css"))
	for _, f := range sheets {
		name := "css/" + filepath.Base(f)
		for i, line := range strings.Split(read(name), "\n") {
			for _, m := range content.FindAllStringSubmatch(line, -1) {
				texts[name+":"+strconv.Itoa(i+1)+" content:\""+m[1]+"\""] = cssEscape.ReplaceAllStringFunc(m[1], func(e string) string {
					n, _ := strconv.ParseUint(strings.TrimSpace(e[1:]), 16, 32)
					return string(rune(n))
				})
			}
		}
	}

	var buf sfnt.Buffer
	for where, text := range texts {
		for _, r := range text {
			if r < 0x20 {
				continue
			}
			for i, face := range faces {
				if g, err := face.GlyphIndex(&buf, r); err != nil || g == 0 {
					t.Errorf("%s: %q (U+%04X) is not in the pinned %s", where, r, r, names[i])
				}
			}
		}
	}
}
