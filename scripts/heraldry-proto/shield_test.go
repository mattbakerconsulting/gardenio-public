package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The docs SVGs are generated from shield.go; a hand edit or a geometry
// change without `go run . svg` fails here.
func TestDocsSVGsUpToDate(t *testing.T) {
	for name, want := range map[string]string{"shield.svg": ShieldSVG(), "shield-grid.svg": ShieldGridSVG()} {
		got, err := os.ReadFile(filepath.Join("..", "..", "gardenio", "docs", "heraldry", name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("gardenio/docs/heraldry/%s is stale: run `GOWORK=off go run . svg` in scripts/heraldry-proto", name)
		}
	}
}

// fieldClearance is the distance from (x, y) to the field's edge (the
// outline's inner side), positive inside.
func fieldClearance(x, y float64) float64 {
	d := math.Min(y-Outline, math.Min(x-Outline, GridW-Outline-x))
	if y > SideY {
		r := ArcR - Outline
		d = math.Min(d, r-math.Hypot(x-ArcR, y-SideY))
		d = math.Min(d, r-math.Hypot(x-(GridW-ArcR), y-SideY))
	}
	return d
}

// Noto draws a typical round emoji inside ~64/72 of its tile, so a round
// charge in a 24-unit place has a radius of ~10.7 units. The geometry
// promises it clears the outline by 2 units and its neighbours by 1.9.
func TestRoundChargesClearOutlineAndEachOther(t *testing.T) {
	const r = 24.0 * 32 / 72
	type pt struct{ x, y float64 }
	var c []pt
	for _, p := range Places {
		c = append(c, pt{p.X + PlaceSz/2, p.Y + PlaceSz/2})
		if p.X < 0 || p.Y < 0 || p.X+PlaceSz > GridW || p.Y+PlaceSz > GridH {
			t.Errorf("place %s leaves the grid", p.Name)
		}
	}
	if c[2] != (pt{FessX, FessY}) {
		t.Errorf("centre place %v is not on the fess point", c[2])
	}
	for i, p := range c {
		if got := fieldClearance(p.x, p.y) - r; got < 1 {
			t.Errorf("place %s: a round charge clears the outline by %.2f units, want ≥ 1", Places[i].Name, got)
		}
		for j := i + 1; j < len(c); j++ {
			if got := math.Hypot(p.x-c[j].x, p.y-c[j].y) - 2*r; got < 1.5 {
				t.Errorf("places %s/%s: round charges %.2f units apart, want ≥ 1.5", Places[i].Name, Places[j].Name, got)
			}
		}
	}
}

func TestArcLeavesSideTangentially(t *testing.T) {
	if got := math.Hypot(32-ArcR, GridH-SideY); math.Abs(got-ArcR) > 1e-9 {
		t.Fatalf("arc from (0,%g) radius %g misses the point: |c-p| = %g", SideY, ArcR, got)
	}
}

// disc is a stand-in for Noto artwork: a radial gradient disc, so the tests
// need no network and exercise blending.
type disc struct{}

func (disc) PNG72(string) (image.Image, error) {
	img := image.NewNRGBA(image.Rect(0, 0, 72, 72))
	for y := 0; y < 72; y++ {
		for x := 0; x < 72; x++ {
			d := math.Hypot(float64(x)-35.5, float64(y)-35.5)
			if d < 32 {
				img.SetNRGBA(x, y, color.NRGBA{uint8(255 - 6*d), uint8(80 + 4*d), 40, 255})
			}
		}
	}
	return img, nil
}

func TestRenderDeterministic(t *testing.T) {
	arms := Arms{"a", "", "c", "", "e"}
	for _, kl := range []bool{false, true} {
		a, _ := Render(arms, 40, Night, Tinctures["red"], Style{DPR: 2, Keyline: kl}, disc{})
		b, _ := Render(arms, 40, Night, Tinctures["red"], Style{DPR: 2, Keyline: kl}, disc{})
		if string(a.Pix) != string(b.Pix) {
			t.Fatalf("keyline=%v: two renders differ", kl)
		}
	}
}

// A picture is the slot times the density, at every size and density.
func TestRenderAtDeviceResolution(t *testing.T) {
	for _, w := range Sizes {
		for _, d := range DPRs {
			img, err := Render(Arms{"", "", "c", "", ""}, w, Day, Tinctures["blue"], Style{DPR: d}, disc{})
			if err != nil {
				t.Fatal(err)
			}
			if got, want := img.Bounds().Size(), image.Pt(w*d, heightFor(w*d)); got != want {
				t.Errorf("%d@%dx: %v, want %v", w, d, got, want)
			}
		}
	}
}

// The keyline draws only where the charges are not, inside the field, in the
// colour opposite the field.
func TestKeylineOutlinesCharges(t *testing.T) {
	arms := Arms{"", "", "c", "", ""}
	for _, tn := range TinctureOrder {
		tc := Tinctures[tn]
		plain, _ := Render(arms, 64, Day, tc, Style{DPR: 2}, disc{})
		lined, _ := Render(arms, 64, Day, tc, Style{DPR: 2, Keyline: true}, disc{})
		kl, field := KeylineFor(tc.Colour(Day)), tc.Colour(Day)
		if contrast(lum(kl), lum(field)) < Threshold {
			t.Errorf("%s: keyline %s on field %s is under 3:1", tn, hexOf(kl), hexOf(field))
		}
		changed := 0
		for i := 0; i < len(plain.Pix); i += 4 {
			if plain.Pix[i] != lined.Pix[i] || plain.Pix[i+1] != lined.Pix[i+1] || plain.Pix[i+2] != lined.Pix[i+2] {
				changed++
			}
		}
		if changed == 0 {
			t.Errorf("%s: the keyline changed no pixel", tn)
		}
	}
}

// WCAG 2's anchors: black on white is 21:1, a colour on itself 1:1.
func TestContrastAnchors(t *testing.T) {
	w, k := lum(mustHex("#ffffff")), lum(mustHex("#000000"))
	if r := contrast(w, k); math.Abs(r-21) > 0.01 {
		t.Errorf("white/black = %.3f, want 21", r)
	}
	if r := contrast(k, w); math.Abs(r-21) > 0.01 {
		t.Errorf("black/white = %.3f, want 21 (order-free)", r)
	}
	if r := contrast(w, w); r != 1 {
		t.Errorf("white/white = %.3f, want 1", r)
	}
}

// The far decile carries a charge whose body sits near the field.
func TestJudgeFarDecile(t *testing.T) {
	st := ChargeStats{Median: lum(mustHex("#3d6cc0")), P10: lum(mustHex("#000000")), P90: lum(mustHex("#4070c0"))}
	v := st.Judge(mustHex("#3d6cc0"))
	if v.Median >= Threshold || !v.Pass {
		t.Errorf("median %.2f far %.2f pass %v: want a pass on the dark decile", v.Median, v.Far, v.Pass)
	}
}

func TestPaletteHexParses(t *testing.T) {
	all := map[string]string{"ink day": "#33261b", "ink night": "#f1ead2", "surface day": "#fbf6e4", "surface night": "#1a2540",
		"keyline dark": "#1b1410", "keyline light": "#fffaf0"}
	for _, tc := range Tinctures {
		all[tc.Name+" day"], all[tc.Name+" night"] = tc.DayHex, tc.NightHex
	}
	for name, h := range all {
		c := mustHex(h)
		if len(h) != 7 || fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) != strings.ToLower(h) {
			t.Errorf("%s: %q does not parse as #rrggbb", name, h)
		}
	}
	if len(Tinctures) != len(TinctureOrder) {
		t.Errorf("%d tinctures, %d in order", len(Tinctures), len(TinctureOrder))
	}
}

// Every Q11 shape: the field lies inside the outline, and each place's centre
// is on the field at 64 px (a charge has somewhere to go).
func TestShapesFieldInsideOutline(t *testing.T) {
	for _, sh := range Shapes {
		for _, w := range Sizes {
			h := heightFor(w)
			s := float64(w) / GridW
			outer, field := sh.masks(w, h, s, float64(h)/GridH)
			for i := range field.Pix {
				if field.Pix[i] > outer.Pix[i] {
					t.Fatalf("%s %d: field outside the outline at %d", sh.Name, w, i)
				}
			}
			if w != 64 {
				continue
			}
			for _, p := range sh.Places {
				cx, cy := int(p.X+sh.placeSz()/2), int((p.Y+sh.placeSz()/2)*float64(h)/GridH)
				if field.AlphaAt(cx, cy).A != 255 {
					t.Fatalf("%s: place %s centre (%d,%d) is off the field", sh.Name, p.Name, cx, cy)
				}
			}
		}
	}
}

// library.md is kbeads' generated list; round 2 reads its charges and names.
func TestLoadLibrary(t *testing.T) {
	lib, err := LoadLibrary("../../gardenio/docs/heraldry/library.md")
	if err != nil {
		t.Fatal(err)
	}
	if len(lib) < 1000 {
		t.Fatalf("library has %d charges, want ~1,333", len(lib))
	}
	name := nameOf(lib)
	for e, want := range map[string]string{"🎲": "game die", "🛡️": "shield", "🛡": "shield"} {
		if got := name(e); got != want {
			t.Errorf("name(%s) = %q, want %q", e, got, want)
		}
	}
}
