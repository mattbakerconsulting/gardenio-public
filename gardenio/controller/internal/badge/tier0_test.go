package badge

import (
	"encoding/xml"
	"strings"
	"testing"
)

// wellFormedSVG asserts the string parses as XML and has an <svg> root — the
// minimum bar for "we can safely inline this".
func wellFormedSVG(t *testing.T, svg string) {
	t.Helper()
	if !strings.HasPrefix(strings.TrimSpace(svg), "<svg") {
		t.Fatalf("badge does not start with <svg>: %q", svg[:clamp40(len(svg))])
	}
	dec := xml.NewDecoder(strings.NewReader(svg))
	var sawSVG bool
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "svg" {
			sawSVG = true
		}
	}
	if !sawSVG {
		t.Fatalf("no <svg> element parsed from: %q", svg)
	}
}

func clamp40(n int) int {
	if n < 40 {
		return n
	}
	return 40
}

func TestTier0_DeterministicAndWellFormed(t *testing.T) {
	a := Tier0("ka-0000002437", "Every gardenio plan gets its own badge")
	b := Tier0("ka-0000002437", "Every gardenio plan gets its own badge")
	if a.SVG != b.SVG {
		t.Fatal("Tier0 is not deterministic: same input produced different SVG")
	}
	if a.Tier != TierDeterministic {
		t.Fatalf("tier = %q, want %q", a.Tier, TierDeterministic)
	}
	wellFormedSVG(t, a.SVG)
	if !strings.Contains(a.SVG, `viewBox="0 0 100 100"`) {
		t.Errorf("badge missing shared viewBox: %s", a.SVG)
	}
	// The ROOT <svg> must carry no width/height (scaled by CSS via viewBox);
	// inner elements legitimately use stroke-width etc., so check only the
	// root tag.
	root := a.SVG[:strings.IndexByte(a.SVG, '>')+1]
	if strings.Contains(root, " width=") || strings.Contains(root, " height=") {
		t.Errorf("root <svg> should carry no width/height (scaled by CSS): %s", root)
	}
}

func TestTier0_DifferentIDsDiffer(t *testing.T) {
	// Two plans with the SAME title but different ids must not render
	// identically — this is the "[E2E fixture] -> EF" collision the
	// monogram-only prototype failed. The shape/hue vary on id, so the
	// badges differ even when the monogram matches.
	x := Tier0("gd-fnhtkrbdiq", "[E2E fixture]")
	y := Tier0("gd-hkbgwaothb", "[E2E fixture]")
	if x.SVG == y.SVG {
		t.Fatal("distinct plan ids produced identical badges")
	}
}

func TestMonogramFor(t *testing.T) {
	cases := []struct {
		title string
		want  string
	}{
		{"Every gardenio plan gets its own icon", "EG"}, // first two significant words: Every, gardenio
		{"The scanner plan", "SP"},                      // leading stop-word dropped -> scanner/plan
		{"Scanner", "SC"},                               // single word -> first two
		{"a the of", "AT"},                              // all stop-words -> keep them
		{"", "?"},                                       // empty
		{"   ", "?"},                                    // whitespace only
		{"!!! ???", "?"},                                // no letters
		{"Deep Clones", "DC"},
		{"X", "X"}, // single letter
	}
	for _, c := range cases {
		if got := monogramFor(c.title); got != c.want {
			t.Errorf("monogramFor(%q) = %q, want %q", c.title, got, c.want)
		}
	}
}

func TestTier0_EscapesXML(t *testing.T) {
	b := Tier0("gd-x", `Tom & Jerry <"'>`)
	if strings.Contains(b.SVG, "<\"") || strings.Contains(b.SVG, "& ") {
		t.Errorf("unescaped XML metachar leaked into badge: %s", b.SVG)
	}
	wellFormedSVG(t, b.SVG)
	// aria-label carries the raw title (escaped); confirm the ampersand was
	// entity-encoded, not left bare.
	if !strings.Contains(b.SVG, "&amp;") {
		t.Errorf("ampersand not escaped in badge: %s", b.SVG)
	}
}

func TestTier0_AllShapesWellFormed(t *testing.T) {
	// Sweep enough ids to exercise every shape + palette branch and confirm
	// none of the geometry produces malformed XML.
	seen := map[tier0Shape]bool{}
	for i := 0; i < 200; i++ {
		id := "gd-" + strings.Repeat("z", i%12) + string(rune('a'+i%26))
		b := Tier0(id, "Coverage sweep title")
		wellFormedSVG(t, b.SVG)
		if len(b.SVG) > 4096 {
			t.Errorf("tier0 badge unexpectedly large (%d bytes): %s", len(b.SVG), id)
		}
	}
	_ = seen // shapes exercised via the id sweep + explicit loop below
	// Directly render each shape to guarantee coverage of every branch.
	for s := shapeCircle; s < numShapes; s++ {
		out := backgroundShape(s, "#123456")
		if !strings.Contains(out, "#123456") {
			t.Errorf("backgroundShape(%d) did not use fill: %q", s, out)
		}
	}
}
