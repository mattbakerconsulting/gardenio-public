package heraldry

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func linked(c Charge) string { return "/arms/charge/" + c.Codepoints + ".png" }

// TestSVGGoldens pins the SVG form, with the charges linked so the goldens
// stay small (TestSVGEmbedsArtwork covers the stand-alone form).
// HERALDRY_UPDATE_GOLDEN=1 regenerates them with the PNG goldens.
func TestSVGGoldens(t *testing.T) {
	update := os.Getenv("HERALDRY_UPDATE_GOLDEN") != ""
	for _, g := range goldenArms {
		a, _ := Parse(g.stored)
		for _, th := range []Theme{Day, Night} {
			path := filepath.Join("testdata", "golden", fmt.Sprintf("%s-%s.svg", g.slug, th))
			got := a.SVG(th, SVGOptions{ChargeHref: linked})
			if update {
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
				continue
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (regenerate with HERALDRY_UPDATE_GOLDEN=1)", err)
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s differs from the golden:\n%s", path, got)
			}
		}
	}
}

// TestSVGWellFormed: the SVG parses as XML, names itself in plain words,
// draws one <image> per charge in a row of 24-unit squares an eighth apart,
// draws the keyline in the theme's colour, and draws nothing else: no
// shield path, no field, no fill.
func TestSVGWellFormed(t *testing.T) {
	for _, g := range goldenArms {
		a, _ := Parse(g.stored)
		for _, th := range []Theme{Day, Night} {
			svg := a.SVG(th, SVGOptions{})
			d := xml.NewDecoder(bytes.NewReader(svg))
			var xs []string
			for {
				tok, err := d.Token()
				if err != nil {
					if err != io.EOF {
						t.Fatalf("%s %s: not well-formed XML: %v", g.slug, th, err)
					}
					break
				}
				se, ok := tok.(xml.StartElement)
				if !ok {
					continue
				}
				switch se.Name.Local {
				case "path", "clipPath", "rect", "circle", "polygon":
					t.Errorf("%s %s: draws a <%s>; the row is the charges alone", g.slug, th, se.Name.Local)
				case "image":
					for _, at := range se.Attr {
						switch {
						case at.Name.Local == "x":
							xs = append(xs, at.Value)
						case at.Name.Local == "y" && at.Value != "1", (at.Name.Local == "width" || at.Name.Local == "height") && at.Value != "22":
							t.Errorf("%s %s: a charge's %s is %s", g.slug, th, at.Name.Local, at.Value)
						}
					}
				}
			}
			var want []string
			for i := range a.Charges {
				want = append(want, fmt.Sprint(1+27*i))
			}
			if fmt.Sprint(xs) != fmt.Sprint(want) {
				t.Errorf("%s %s: charges at x %v, want %v", g.slug, th, xs, want)
			}
			s := string(svg)
			w := fmt.Sprint(27*len(a.Charges) - 3)
			for _, want := range []string{
				`viewBox="0 0 ` + w + ` 24" width="` + w + `" height="24"`,
				`role="img"`,
				`aria-label="` + a.PlainName() + `"`,
				`<title>` + a.PlainName() + `</title>`,
				`flood-color="` + hexOf(keylineFor(th)) + `"`,
			} {
				if !strings.Contains(s, want) {
					t.Errorf("%s %s: SVG lacks %s", g.slug, th, want)
				}
			}
			if strings.Contains(s, "fill=") {
				t.Errorf("%s %s: SVG fills something; the ground is transparent", g.slug, th)
			}
		}
	}
}

// TestSVGEmbedsArtwork: without ChargeHref the SVG stands alone, each charge
// the library's own artwork as a data: URI, in order: the 128 px PNG, or the
// 72 px one with Small.
func TestSVGEmbedsArtwork(t *testing.T) {
	a, _ := Parse(goldenArms[0].stored)
	for _, small := range []bool{false, true} {
		art, px := ChargePNG, 128
		if small {
			art, px = smallChargePNG, 72
		}
		hrefs := regexp.MustCompile(`href="data:image/png;base64,([^"]+)"`).FindAllSubmatch(a.SVG(Day, SVGOptions{Small: small}), -1)
		if len(hrefs) != len(a.Charges) {
			t.Fatalf("Small %v: %d embedded charges, want %d", small, len(hrefs), len(a.Charges))
		}
		for i, m := range hrefs {
			got, err := base64.StdEncoding.DecodeString(string(m[1]))
			if err != nil {
				t.Fatal(err)
			}
			if want, _ := art(a.Charges[i]); !bytes.Equal(got, want) {
				t.Errorf("Small %v: charge %d is not %s's %d px artwork", small, i, a.Charges[i].Emoji, px)
			}
		}
	}
}

// TestSVGIDsPerArms: two different arms inline on one page do not share an
// id, and the same arms always use the same ones.
func TestSVGIDsPerArms(t *testing.T) {
	ids := regexp.MustCompile(`id="([^"]+)"`)
	seen := map[string]string{}
	for _, g := range goldenArms {
		a, _ := Parse(g.stored)
		for _, m := range ids.FindAllSubmatch(a.SVG(Day, SVGOptions{}), -1) {
			if other, ok := seen[string(m[1])]; ok && other != g.slug {
				t.Errorf("id %s is used by %s and %s", m[1], other, g.slug)
			}
			seen[string(m[1])] = g.slug
		}
	}
	a, _ := Parse(goldenArms[0].stored)
	if !bytes.Equal(a.SVG(Night, SVGOptions{}), a.SVG(Night, SVGOptions{})) {
		t.Error("two SVGs of the same arms differ")
	}
}

// TestSVGEscapesHref: a surface's charge URL is escaped into the attribute.
func TestSVGEscapesHref(t *testing.T) {
	a, _ := Parse("🧭🔥📦")
	svg := string(a.SVG(Day, SVGOptions{ChargeHref: func(Charge) string { return `/c?a=1&b="2"` }}))
	if !strings.Contains(svg, `href="/c?a=1&amp;b=&#34;2&#34;"`) {
		t.Errorf("href not escaped: %s", svg)
	}
}
