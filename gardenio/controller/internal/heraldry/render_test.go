package heraldry

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	xdraw "golang.org/x/image/draw"
)

// goldenArms are the arms the goldens draw (spec §6.6): round 1's goats and
// heraldry plans as runs of three, the heraldry plan as its stored six-token
// form, and a full run of five.
var goldenArms = []struct{ slug, stored string }{
	{"goats", "🌠🐐🪦"},
	{"heraldry", "slate 🎲 . 🛡️ . 🖼️"},
	{"five", "🚪📜🏡🛂🎉"},
}

// goldenSlots are the CSS slots (spec §6.3), drawn at 2×.
var goldenSlots = []int{64, 40, 24}

// TestGoldens: the renderer is deterministic and draws what was reviewed.
// Each golden is decoded and compared pixel by pixel, allowing ±2 per
// channel so a fused multiply-add on another architecture cannot fail it.
// After a deliberate change to the drawing, bump Version and regenerate:
//
//	HERALDRY_UPDATE_GOLDEN=1 go test ./internal/heraldry/ -run TestGoldens
//
// then look at every changed PNG before committing it.
func TestGoldens(t *testing.T) {
	update := os.Getenv("HERALDRY_UPDATE_GOLDEN") != ""
	for _, g := range goldenArms {
		a, err := Parse(g.stored)
		if err != nil {
			t.Fatal(err)
		}
		for _, slot := range goldenSlots {
			for _, th := range []Theme{Day, Night} {
				name := fmt.Sprintf("%s-%d@2x-%s.png", g.slug, slot, th)
				path := filepath.Join("testdata", "golden", name)
				got, err := a.PNG(DeviceHeight(slot, 2), th)
				if err != nil {
					t.Fatal(err)
				}
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
				if msg := diff(t, got, want); msg != "" {
					t.Errorf("%s: %s", name, msg)
				}
			}
		}
	}
}

func diff(t *testing.T, got, want []byte) string {
	g, err := png.Decode(bytes.NewReader(got))
	if err != nil {
		t.Fatal(err)
	}
	w, err := png.Decode(bytes.NewReader(want))
	if err != nil {
		t.Fatal(err)
	}
	if g.Bounds() != w.Bounds() {
		return fmt.Sprintf("size %v, golden %v", g.Bounds().Size(), w.Bounds().Size())
	}
	bad := 0
	b := g.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r1, g1, b1, a1 := g.At(x, y).RGBA()
			r2, g2, b2, a2 := w.At(x, y).RGBA()
			for _, d := range [][2]uint32{{r1, r2}, {g1, g2}, {b1, b2}, {a1, a2}} {
				if dd := int(d[0]>>8) - int(d[1]>>8); dd > 2 || dd < -2 {
					bad++
					break
				}
			}
		}
	}
	if bad > 0 {
		return fmt.Sprintf("%d of %d pixels differ from the golden", bad, b.Dx()*b.Dy())
	}
	return ""
}

func TestRenderDeterministic(t *testing.T) {
	a, _ := Parse(goldenArms[2].stored)
	one, err := a.PNG(120, Night)
	if err != nil {
		t.Fatal(err)
	}
	two, _ := a.PNG(120, Night)
	if !bytes.Equal(one, two) {
		t.Error("two renders of the same arms differ")
	}
}

func TestRenderSizes(t *testing.T) {
	for _, g := range goldenArms {
		a, _ := Parse(g.stored)
		for _, h := range []int{MinHeight, 17, 24, 48, 120, MaxHeight} {
			img, err := a.Image(h, Day)
			if err != nil {
				t.Fatalf("height %d: %v", h, err)
			}
			if got := img.Bounds().Size(); got != image.Pt(Width(len(a.Charges), h), h) {
				t.Errorf("%s at height %d: drew %v", g.slug, h, got)
			}
		}
	}
	a, _ := Parse(goldenArms[0].stored)
	for _, h := range []int{MinHeight - 1, MaxHeight + 1, 0, -40} {
		if _, err := a.Image(h, Day); err == nil {
			t.Errorf("height %d: no error", h)
		}
	}
	if _, err := (Arms{}).Image(48, Day); err == nil {
		t.Error("no charges: no error")
	}
}

// TestRowShape: the picture is a row of charges on a transparent ground,
// with nothing drawn but the charges and their keyline. The gaps between
// the squares are clear, every square holds its charge, and the corners of
// the picture (where a shield's outline and field were) are transparent.
func TestRowShape(t *testing.T) {
	for _, g := range goldenArms {
		a, _ := Parse(g.stored)
		for _, h := range []int{48, 80, 128} {
			for _, th := range []Theme{Day, Night} {
				img, err := a.Image(h, th)
				if err != nil {
					t.Fatal(err)
				}
				b := img.Bounds()
				for _, p := range []image.Point{{0, 0}, {b.Dx() - 1, 0}, {0, h - 1}, {b.Dx() - 1, h - 1}} {
					if c := img.RGBAAt(p.X, p.Y); c.A != 0 {
						t.Errorf("%s %d %s: corner %v is not transparent: %v", g.slug, h, th, p, c)
					}
				}
				gap := Gap(h)
				for i := range a.Charges {
					x0 := i * (h + gap)
					opaque := 0
					for y := 0; y < h; y++ {
						for x := x0; x < x0+h; x++ {
							if img.RGBAAt(x, y).A > 200 {
								opaque++
							}
						}
						// The middle column of the gap after this square is clear.
						if i < len(a.Charges)-1 {
							if c := img.RGBAAt(x0+h+gap/2, y); c.A != 0 {
								t.Fatalf("%s %d %s: the gap after charge %d is drawn on at y=%d: %v", g.slug, h, th, i, y, c)
							}
						}
					}
					if opaque < h*h/8 {
						t.Errorf("%s %d %s: charge %d's square is nearly empty (%d opaque px)", g.slug, h, th, i, opaque)
					}
				}
			}
		}
	}
}

// TestKeylineDrawn: the keyline is really in the picture, in the theme's
// keyline colour, just outside each charge's own footprint.
func TestKeylineDrawn(t *testing.T) {
	const h = 96
	a, _ := Parse("🖤⬛🔲") // dark charges, which need the keyline by night
	for _, th := range []Theme{Day, Night} {
		img, err := a.Image(h, th)
		if err != nil {
			t.Fatal(err)
		}
		foot := image.NewRGBA(img.Bounds())
		for i, c := range a.Charges {
			art, _ := chargeImage(c)
			xdraw.CatmullRom.Scale(foot, chargeBox(i, h, keylinePx(h)), art, art.Bounds(), xdraw.Over, nil)
		}
		kl, edge := keylineFor(th), 0
		for y := 0; y < h; y++ {
			for x := 0; x < img.Bounds().Dx(); x++ {
				if p := img.RGBAAt(x, y); foot.RGBAAt(x, y).A == 0 && p.A == 255 && nearColour(p, kl) {
					edge++
				}
			}
		}
		// Four px around three charges is well over 300 px.
		if edge < 300 {
			t.Errorf("%s: only %d px of %s keyline around the charges", th, edge, hexOf(kl))
		}
	}
}

// TestKeylineContrast: the keyline clears 3:1 against the surface it is
// drawn for, day and night (spec §6.6, Q14), so every charge keeps its edge
// there.
func TestKeylineContrast(t *testing.T) {
	for _, th := range []Theme{Day, Night} {
		if r := contrast(luminance(keylineFor(th)), luminance(surface(th))); r < 3 {
			t.Errorf("%s keyline %s on surface %s: %.2f:1", th, hexOf(keylineFor(th)), hexOf(surface(th)), r)
		}
	}
}

func nearColour(p interface{ RGBA() (r, g, b, a uint32) }, c interface{ RGBA() (r, g, b, a uint32) }) bool {
	r1, g1, b1, _ := p.RGBA()
	r2, g2, b2, _ := c.RGBA()
	d := func(a, b uint32) bool { x := int(a>>8) - int(b>>8); return x <= 3 && x >= -3 }
	return d(r1, r2) && d(g1, g2) && d(b1, b2)
}
