package heraldry

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strconv"
	"strings"

	xdraw "golang.org/x/image/draw"
)

// The row in design units: each charge sits on a 24-unit square, an eighth
// of it (3 units) between squares, and is drawn inset by the keyline so its
// keyline stays inside its square. A slot's height is one square.
const (
	unit    = 24.0 // a charge's square
	keyline = 1.0  // the keyline around each charge (Q14 (a)); never under 1 device px
)

// MinHeight and MaxHeight bound a picture's height in device pixels: 8 is
// under the smallest slot at 1×, 256 a 64 px slot at 4×, past any screen's
// density.
const (
	MinHeight = 8
	MaxHeight = 256
)

// Gap is the space between two charges in a picture h device pixels high:
// an eighth of the height, rounded (24→3, 40→5, 48→6, 64→8).
func Gap(h int) int { return int(math.Round(float64(h) / 8)) }

// Width is the width in device pixels of n charges h device pixels high:
// n squares and the gaps between them (three at 24 are 78, five at 64 are
// 352).
func Width(n, h int) int {
	if n < 1 {
		return 0
	}
	return n*h + (n-1)*Gap(h)
}

// DeviceHeight is the bitmap height for a CSS slot at a device pixel ratio
// (spec §6.3): the slot times the ratio, rounded up, at least 1×. A 24 px
// slot at 3× is 72.
func DeviceHeight(slot int, dpr float64) int {
	return int(math.Ceil(float64(slot) * math.Max(dpr, 1)))
}

// The surfaces are garden.css --surface, the backgrounds the keyline is
// measured against (TestKeylineContrast).
var (
	surfaceDay   = mustHex("#fbf6e4")
	surfaceNight = mustHex("#1a2540")
)

// The keyline (spec §6.2, Q14 (a)) is drawn in whichever of these clears the
// theme's surface by more: dark by day, light by night.
var (
	keylineDark  = mustHex("#1b1410")
	keylineLight = mustHex("#fffaf0")
)

func surface(th Theme) color.NRGBA {
	if th == Night {
		return surfaceNight
	}
	return surfaceDay
}

// keylineFor is the keyline colour on a theme's surface.
func keylineFor(th Theme) color.NRGBA {
	ls := luminance(surface(th))
	if contrast(luminance(keylineDark), ls) >= contrast(luminance(keylineLight), ls) {
		return keylineDark
	}
	return keylineLight
}

// chargeBox is the device-pixel square charge i is drawn into in a picture
// h high: its slot square inset by the keyline k on every side.
func chargeBox(i, h, k int) image.Rectangle {
	x := i * (h + Gap(h))
	return image.Rect(x+k, k, x+h-k, h-k)
}

// keylinePx is the keyline's width in device pixels at height h: one unit,
// never under one pixel.
func keylinePx(h int) int { return max(int(math.Round(keyline*float64(h)/unit)), 1) }

// Image draws the arms h device pixels high, as wide as Width(n, h), on a
// transparent ground for theme th's surface: each charge resampled from its
// 128 px artwork into its square with Catmull-Rom (the kernel widens when
// downscaling), and under the charges their keyline.
func (a Arms) Image(h int, th Theme) (*image.RGBA, error) {
	if h < MinHeight || h > MaxHeight {
		return nil, fmt.Errorf("heraldry: height %d is outside %d to %d device pixels", h, MinHeight, MaxHeight)
	}
	if len(a.Charges) == 0 {
		return nil, ErrNoArms
	}
	img := image.NewRGBA(image.Rect(0, 0, Width(len(a.Charges), h), h))
	layer := image.NewRGBA(img.Bounds())
	k := keylinePx(h)
	for i, c := range a.Charges {
		art, err := chargeImage(c)
		if err != nil {
			return nil, err
		}
		xdraw.CatmullRom.Scale(layer, chargeBox(i, h, k), art, art.Bounds(), xdraw.Over, nil)
	}
	draw.DrawMask(img, img.Bounds(), image.NewUniform(keylineFor(th)), image.Point{}, dilate(layer, float64(k)), image.Point{}, draw.Over)
	draw.Draw(img, img.Bounds(), layer, image.Point{}, draw.Over)
	return img, nil
}

// PNG is Image encoded as PNG.
func (a Arms) PNG(h int, th Theme) ([]byte, error) {
	img, err := a.Image(h, th)
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	if err := (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&b, img); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// chargeImage decodes a charge's artwork.
func chargeImage(c Charge) (image.Image, error) {
	b, ok := ChargePNG(c)
	if !ok {
		return nil, fmt.Errorf("heraldry: %s has no artwork", c.Emoji)
	}
	return png.Decode(bytes.NewReader(b))
}

// dilate is the layer's alpha grown by radius r device pixels, a disc
// maximum antialiased over its last pixel: the keyline's coverage. The
// layer's alpha counts as full from 50%, so the keyline's r pixels are
// measured from the charge's visible edge, not from inside its soft rim.
func dilate(layer *image.RGBA, r float64) *image.Alpha {
	type tap struct {
		dx, dy int
		wgt    float64
	}
	ri := int(math.Ceil(r))
	var taps []tap
	for dy := -ri; dy <= ri; dy++ {
		for dx := -ri; dx <= ri; dx++ {
			if wgt := math.Min(1, r+0.5-math.Hypot(float64(dx), float64(dy))); wgt > 0 {
				taps = append(taps, tap{dx, dy, wgt})
			}
		}
	}
	b := layer.Bounds()
	w, h := b.Dx(), b.Dy()
	out := image.NewAlpha(b)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var best float64
			for _, t := range taps {
				px, py := x+t.dx, y+t.dy
				if px < 0 || py < 0 || px >= w || py >= h {
					continue
				}
				if a := layer.Pix[py*layer.Stride+px*4+3]; a != 0 {
					best = math.Max(best, math.Min(255, 2*float64(a))*t.wgt)
				}
			}
			out.Pix[y*out.Stride+x] = uint8(math.Round(best))
		}
	}
	return out
}

// luminance is the WCAG 2 relative luminance of an opaque sRGB colour.
func luminance(c color.NRGBA) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// contrast is the WCAG 2 contrast ratio between two relative luminances.
func contrast(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// mustHex reads a "#rrggbb" colour constant.
func mustHex(s string) color.NRGBA {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}

func hexOf(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }
