package badge

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// The raster goldens pin what Rasterize draws at each size it is asked for,
// up to the 512px poster size (gd-RcmmFKWytW). They are compared pixel by
// pixel, not byte by byte, so a change in the PNG encoder's compression does
// not red them; a change in what is drawn does. Regenerate after an
// INTENTIONAL raster change, then look at every changed PNG:
//
//	GB_UPDATE_GOLDEN=1 go test ./internal/badge/ -run TestRasterize_Golden
//
// poster-sample.svg is a poster for plan gd-pQZO9ie11D (arms 🏅🖌️🔎 on brown),
// drawn to the poster prompt: gradients, opacity, a transformed group, and no
// <text>, so it exercises the geometry path at size; the Tier-0 flagship
// exercises the text overlay at size.
var rasterGoldenFixtures = []struct {
	name string
	size int
}{
	{"tier0", 24},
	{"tier0", 128},
	{"tier0", 256},
	{"tier0", 512},
	{"poster", 128},
	{"poster", 512},
}

// rasterGoldenBadge returns the badge a raster fixture draws.
func rasterGoldenBadge(t *testing.T, name string) Badge {
	t.Helper()
	switch name {
	case "tier0":
		return Tier0("ka-0000005980", "Every gardenio plan gets its own icon/badge")
	case "poster":
		raw, err := os.ReadFile(filepath.Join(goldenDir, "poster-sample.svg"))
		if err != nil {
			t.Fatal(err)
		}
		// The sample must pass the gate a model poster passes.
		svg, err := sanitizeSVG(string(raw), maxPosterSVGBytes)
		if err != nil {
			t.Fatalf("poster-sample.svg fails the poster sanitizer: %v", err)
		}
		return Badge{SVG: svg, Tier: TierModel}
	}
	t.Fatalf("unknown raster fixture %q", name)
	return Badge{}
}

func TestRasterize_Golden(t *testing.T) {
	update := os.Getenv("GB_UPDATE_GOLDEN") != ""
	for _, f := range rasterGoldenFixtures {
		name := f.name + "-" + strconv.Itoa(f.size)
		t.Run(name, func(t *testing.T) {
			r, err := rasterGoldenBadge(t, f.name).Rasterize(f.size)
			if err != nil {
				t.Fatalf("Rasterize(%d): %v", f.size, err)
			}
			got := decode(t, r.PNG)
			if got.Bounds().Dx() != f.size || got.Bounds().Dy() != f.size {
				t.Fatalf("bounds = %v, want %dx%d", got.Bounds(), f.size, f.size)
			}
			path := filepath.Join(goldenDir, "raster-"+name+".golden.png")
			if update {
				if err := os.WriteFile(path, r.PNG, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			fh, err := os.Open(path)
			if err != nil {
				t.Fatalf("open golden %s: %v (set GB_UPDATE_GOLDEN=1 to create it)", path, err)
			}
			defer fh.Close()
			want, err := png.Decode(fh)
			if err != nil {
				t.Fatalf("decode golden %s: %v", path, err)
			}
			if n := pixelsDiffering(got, want); n > 0 {
				t.Errorf("%d pixels differ from %s", n, path)
			}
		})
	}
}

// pixelsDiffering counts the pixels where a and b differ by more than a small
// per-channel tolerance (anti-aliasing round-off), or every pixel when the
// sizes differ.
func pixelsDiffering(a, b image.Image) int {
	if a.Bounds() != b.Bounds() {
		return a.Bounds().Dx() * a.Bounds().Dy()
	}
	const tol = 2
	n := 0
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			ca := color.NRGBAModel.Convert(a.At(x, y)).(color.NRGBA)
			cb := color.NRGBAModel.Convert(b.At(x, y)).(color.NRGBA)
			if absDiff(ca.R, cb.R) > tol || absDiff(ca.G, cb.G) > tol ||
				absDiff(ca.B, cb.B) > tol || absDiff(ca.A, cb.A) > tol {
				n++
			}
		}
	}
	return n
}

func absDiff(a, b uint8) uint8 {
	if a > b {
		return a - b
	}
	return b - a
}

// The monogram overlay scales with the canvas: the ink box of the Tier-0
// monogram at 512px is four times its box at 128px, to within a few pixels of
// glyph hinting. Without the scale the 512px monogram would stay 128px-sized
// in the middle of the disc.
func TestRasterize_OverlayScales(t *testing.T) {
	b := Tier0("ka-0000007031", "Every gardenio plan gets its own icon badge")
	tx, ok := extractText(b.SVG)
	if !ok {
		t.Fatal("no monogram in the Tier-0 badge")
	}
	fg := color.NRGBAModel.Convert(tx.fill).(color.NRGBA)
	box := func(size int) image.Rectangle {
		r, err := b.Rasterize(size)
		if err != nil {
			t.Fatalf("Rasterize(%d): %v", size, err)
		}
		img := decode(t, r.PNG)
		var ink image.Rectangle
		// The monogram sits in the middle; the ring, also in fg, lies outside
		// the central half, so only the central half is scanned.
		for y := size / 4; y < size*3/4; y++ {
			for x := size / 4; x < size*3/4; x++ {
				c := color.NRGBAModel.Convert(img.At(x, y)).(color.NRGBA)
				if absDiff(c.R, fg.R) < 24 && absDiff(c.G, fg.G) < 24 && absDiff(c.B, fg.B) < 24 {
					ink = ink.Union(image.Rect(x, y, x+1, y+1))
				}
			}
		}
		if ink.Empty() {
			t.Fatalf("no monogram ink at %dpx", size)
		}
		return ink
	}
	small, large := box(128), box(512)
	for _, d := range []struct {
		name         string
		small, large int
	}{
		{"width", small.Dx(), large.Dx()},
		{"height", small.Dy(), large.Dy()},
	} {
		if diff := d.large - 4*d.small; diff < -8 || diff > 8 {
			t.Errorf("monogram %s = %dpx at 512 vs %dpx at 128, want about 4x", d.name, d.large, d.small)
		}
	}
}

func TestRasterize_RejectsOverMax(t *testing.T) {
	b := Tier0("gd-x", "Title")
	if _, err := b.Rasterize(MaxRasterSize + 1); err == nil {
		t.Errorf("Rasterize(%d) succeeded, want an error over MaxRasterSize", MaxRasterSize+1)
	}
	if _, err := b.Rasterize(MaxRasterSize); err != nil {
		t.Errorf("Rasterize(%d): %v", MaxRasterSize, err)
	}
}
