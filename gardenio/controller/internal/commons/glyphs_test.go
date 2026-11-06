package commons

import (
	"html"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"text/template/parse"
	"unicode"

	"golang.org/x/image/font/sfnt"
)

// TestRenderedTextInGoldenFonts guards gd-icRpoTdPhD: every character the
// Commons renderers and the pane's own web code (paneTexts) write must be in the Noto Sans subsets the garden goldens
// pin (gardenhub/testdata/golden/fonts), which is the face the pane's text
// renders in (--font-body, --font-display). A character missing there falls
// back to a system font, which differs between an agent pod and the CI
// runner, so the goldens drift by machine instead of failing here. Draw a
// symbol as SVG (stateMarks), or re-subset the fonts (GOLDENS.md).
func TestRenderedTextInGoldenFonts(t *testing.T) {
	var faces []*sfnt.Font
	for _, f := range []string{"NotoSans-Regular", "NotoSans-Bold"} {
		b, err := os.ReadFile(filepath.Join("..", "gardenhub", "testdata", "golden", "fonts", f+".ttf"))
		if err != nil {
			t.Fatal(err)
		}
		face, err := sfnt.Parse(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		faces = append(faces, face)
	}

	// The text the renderers write themselves: the fragments' literal text,
	// the agent states' words, and the separators and ellipses the Go side adds.
	texts := map[string]string{"stream separator": " · ", "clip ellipsis": "…", "url ellipsis": "/…/"}
	for _, tpl := range fragments.Templates() {
		if tpl.Tree == nil {
			continue
		}
		var walk func(parse.Node)
		walk = func(n parse.Node) {
			switch n := n.(type) {
			case *parse.ListNode:
				if n != nil {
					for _, c := range n.Nodes {
						walk(c)
					}
				}
			case *parse.TextNode:
				texts["template "+tpl.Name()+": "+strings.TrimSpace(string(n.Text))] = string(n.Text)
			case *parse.IfNode:
				walk(n.List)
				walk(n.ElseList)
			case *parse.RangeNode:
				walk(n.List)
				walk(n.ElseList)
			case *parse.WithNode:
				walk(n.List)
				walk(n.ElseList)
			}
		}
		walk(tpl.Tree.Root)
	}
	for state, s := range agentStates {
		texts["agentStates["+state+"].stream"] = s.stream
		if _, ok := stateMarks[s.mark]; !ok {
			t.Errorf("agentStates[%s]: no stateMarks[%q]", state, s.mark)
		}
		if _, ok := glassWords[s.glass]; !ok {
			t.Errorf("agentStates[%s]: no glassWords[%q]", state, s.glass)
		}
	}
	for k, w := range glassWords {
		texts["glassWords["+k+"]"] = w
	}
	for name, m := range stateMarks {
		if strings.ContainsFunc(stripTags(string(m)), func(r rune) bool { return !unicode.IsSpace(r) }) {
			t.Errorf("stateMarks[%s] draws text, which a font renders: %s", name, m)
		}
	}

	for where, text := range paneTexts(t) {
		texts[where] = text
	}

	var buf sfnt.Buffer
	for where, text := range texts {
		for _, r := range text {
			if r < 0x20 {
				continue
			}
			for i, face := range faces {
				if g, err := face.GlyphIndex(&buf, r); err != nil || g == 0 {
					t.Errorf("%s: %q (U+%04X) is not in the pinned %s", where, r, r, []string{"NotoSans-Regular", "NotoSans-Bold"}[i])
				}
			}
		}
	}
}

func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// paneTexts is the text the Commons pane writes outside the renderers
// (gd-PGuWWmb23a, the back button's U+2190): every non-ASCII character in
// commons2.js (the workbench Up link's U+2191, gd-2gyVEEp16B), costs.js and
// project.js (whose cost cards' Open links drew
// U+2197, gd-zLPoTcMCST), string literal or \u escape alike (a comment counts
// too, which is stricter than needed but needs no JS parser); the pane's page,
// node.html, with its entities decoded; and the content: of the cm- rules in
// web/css/*.css with their escapes decoded.
func paneTexts(t *testing.T) map[string]string {
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "gardenhub", "web", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	out := map[string]string{}

	for _, name := range []string{"commons2.js", "costs.js", "project.js"} {
		for i, line := range strings.Split(read(name), "\n") {
			var b strings.Builder
			for _, r := range line {
				if r > 0x7e {
					b.WriteRune(r)
				}
			}
			for _, m := range regexp.MustCompile(`\\u([0-9a-fA-F]{4})`).FindAllStringSubmatch(line, -1) {
				n, _ := strconv.ParseUint(m[1], 16, 32)
				b.WriteRune(rune(n))
			}
			if b.Len() > 0 {
				out[name+":"+strconv.Itoa(i+1)] = b.String()
			}
		}
	}

	out["node.html"] = html.UnescapeString(read("node.html"))

	cssEscape := regexp.MustCompile(`\\([0-9a-fA-F]{1,6}) ?`)
	sheets, _ := filepath.Glob(filepath.Join("..", "gardenhub", "web", "css", "*.css"))
	var css strings.Builder
	for _, f := range sheets {
		css.WriteString(read(filepath.Join("css", filepath.Base(f))))
	}
	for _, m := range regexp.MustCompile(`([^{}]*)\{[^{}]*content:"((?:[^"\\]|\\.)*)"`).FindAllStringSubmatch(css.String(), -1) {
		if sel := strings.TrimSpace(m[1]); strings.Contains(sel, ".cm-") || strings.Contains(sel, ".commons") {
			out["web/css "+sel+" content"] = cssEscape.ReplaceAllStringFunc(m[2], func(e string) string {
				n, _ := strconv.ParseUint(strings.TrimSpace(e[1:]), 16, 32)
				return string(rune(n))
			})
		}
	}
	return out
}
