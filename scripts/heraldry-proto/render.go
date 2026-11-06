package main

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/vector"
)

// Sizes are the CSS slot widths the spec names (§6.3). Height keeps the
// grid's 64:72, rounded. The 8 px task cell is gone with task arms (R6).
var Sizes = []int{64, 40, 24, 16}

// DPRs are the device pixel ratios a slot is drawn at (round 2: 1×, 2× and
// 3×). A picture is always the full drawing at slot × DPR device pixels,
// resampled smoothly; nothing is quantised (owner, round 1).
var DPRs = []int{1, 2, 3}

func heightFor(w int) int { return int(math.Round(float64(w) * GridH / GridW)) }

// Mode is day or night: the outline and field colours (spec §6.2: "Day and
// night are the outline colour and each tincture's day and night value; the
// charges are the same").
type Mode struct {
	Name    string
	Ink     color.NRGBA // garden.css --ink for the scheme
	Surface color.NRGBA // garden.css --surface, the sheet's background
	Night   bool
}

var (
	Day   = Mode{"day", mustHex("#33261b"), mustHex("#fbf6e4"), false}
	Night = Mode{"night", mustHex("#f1ead2"), mustHex("#1a2540"), true}
)

// Tincture is one of R13's eight field colours, with a day and a night value
// of the same hue (night a shade deeper, not inverted, spec §6.2). The values
// are round 2's proposal; Q13 names the eight but no values exist anywhere
// else yet.
type Tincture struct {
	Name, DayHex, NightHex string
}

// TinctureOrder is R13's palette in the spec's order (§6.2, Q13).
var TinctureOrder = []string{"blue", "red", "green", "gold", "purple", "brown", "slate", "sand"}

var Tinctures = map[string]Tincture{
	"blue":   {"blue", "#3d6cc0", "#2b4e8f"},
	"red":    {"red", "#c0443a", "#8e3029"},
	"green":  {"green", "#3a8f47", "#2a6834"},
	"gold":   {"gold", "#d9a933", "#a67e22"},
	"purple": {"purple", "#8556b8", "#613f88"},
	"brown":  {"brown", "#8f5f3a", "#69452a"},
	"slate":  {"slate", "#627284", "#475363"},
	"sand":   {"sand", "#d6c29a", "#a69572"},
}

func (t Tincture) Colour(m Mode) color.NRGBA {
	if m.Night {
		return mustHex(t.NightHex)
	}
	return mustHex(t.DayHex)
}

// The keyline (spec §6.2, Q14 (a)): a 1-unit line around every charge in the
// luminance opposite the field's, whichever of these two clears the field
// by more.
var (
	KeylineDark  = mustHex("#1b1410")
	KeylineLight = mustHex("#fffaf0")
)

func KeylineFor(field color.NRGBA) color.NRGBA {
	if contrast(lum(KeylineDark), lum(field)) >= contrast(lum(KeylineLight), lum(field)) {
		return KeylineDark
	}
	return KeylineLight
}

// Arms is a blazon split into its five places (spec §4.1); "" is empty.
type Arms [5]string

// Charges loads artwork: PNG72 returns the Noto 72-px PNG for one emoji. 72 px
// is exactly a 64 px slot's 24-unit place at 3×, so no larger asset is needed
// for round 2's densities.
type Charges interface {
	PNG72(emoji string) (image.Image, error)
}

// Style is how a picture is drawn: its density and whether charges carry the
// keyline.
type Style struct {
	DPR     int
	Keyline bool
}

// Render draws arms in a CSS slot w wide, at st.DPR device pixels per CSS
// pixel, in mode m on tincture t. It is the drawing rule the product's
// internal/heraldry would use (spec §6.4): the outline and field rasterised
// from the shield path, each charge resampled from its 72-px PNG into its
// place with Catmull-Rom (x/image/draw widens the kernel when downscaling),
// clipped to the field, composited in place order with the centre last so it
// sits on top where squares overlap.
func Render(arms Arms, w int, m Mode, t Tincture, st Style, src Charges) (*image.NRGBA, error) {
	return RenderShape(arms, w*max(st.DPR, 1), m, t, st.Keyline, src, Heater)
}

// RenderShape is Render at a device width px on any shield shape (round 1's
// Q11 comparison left the heater as the standard; shapes.go keeps the rest).
func RenderShape(arms Arms, px int, m Mode, t Tincture, keyline bool, src Charges, sh Shape) (*image.NRGBA, error) {
	w := px
	h := heightFor(w)
	s := float64(w) / GridW
	sy := float64(h) / GridH // ≈ s; rounding the height stretches at most 1/18 at 16 px
	// The outline is 1 unit, but never thinner than 1 px: below 64 px a
	// 1-unit line is a fraction of a pixel and the shield's edge vanishes
	// against a surface of its own tone (sand on the day surface).
	outer, field := sh.masks(w, h, s, sy)

	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	cloth := t.Colour(m)
	paint(img, outer, m.Ink)
	paint(img, field, cloth)

	charges := image.NewNRGBA(img.Bounds())
	order := []int{0, 1, 3, 4, 2} // corners, then the centre on top
	for _, i := range order {
		if arms[i] == "" {
			continue
		}
		art, err := src.PNG72(arms[i])
		if err != nil {
			return nil, err
		}
		pl := sh.Places[i]
		x0, y0 := int(math.Round(pl.X*s)), int(math.Round(pl.Y*sy))
		sz := max(int(math.Round(sh.placeSz()*s)), 1)
		xdraw.CatmullRom.Scale(charges, image.Rect(x0, y0, x0+sz, y0+sz), art, art.Bounds(), xdraw.Over, nil)
	}
	if keyline {
		// One unit, never under one device pixel, so the line exists at 16 px.
		paint(img, clip(dilate(charges, math.Max(s, 1)), field), KeylineFor(cloth))
	}
	// Clip the charge layer to the field (spec §6.2: a charge never crosses
	// the outline), then lay it on the shield.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := charges.NRGBAAt(x, y)
			if c.A == 0 {
				continue
			}
			c.A = uint8(uint32(c.A) * uint32(field.AlphaAt(x, y).A) / 255)
			over(img, x, y, c)
		}
	}
	return img, nil
}

// dilate is the charge layer's alpha grown by radius r device pixels (a disc
// maximum, antialiased on its rim): the keyline's coverage.
func dilate(layer *image.NRGBA, r float64) *image.Alpha {
	b := layer.Bounds()
	out := image.NewAlpha(b)
	ri := int(math.Ceil(r))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			var best float64
			for dy := -ri; dy <= ri; dy++ {
				for dx := -ri; dx <= ri; dx++ {
					p := image.Pt(x+dx, y+dy)
					if !p.In(b) {
						continue
					}
					a := float64(layer.NRGBAAt(p.X, p.Y).A)
					if a == 0 {
						continue
					}
					// Full weight inside r, fading over the last pixel.
					wgt := math.Max(0, math.Min(1, r+0.5-math.Hypot(float64(dx), float64(dy))))
					best = math.Max(best, a*wgt)
				}
			}
			out.SetAlpha(x, y, color.Alpha{uint8(math.Round(best))})
		}
	}
	return out
}

// clip multiplies coverage a by coverage m.
func clip(a, m *image.Alpha) *image.Alpha {
	for i := range a.Pix {
		a.Pix[i] = uint8(uint32(a.Pix[i]) * uint32(m.Pix[i]) / 255)
	}
	return a
}

// traceMask rasterises a shield outline inset by `in` units into an alpha
// coverage mask.
func traceMask(trace func(p pather, in, s, ox, oy float64), w, h int, in, sx, sy float64) *image.Alpha {
	r := vector.NewRasterizer(w, h)
	trace(scaled{r, sy / sx}, in, sx, 0, 0)
	a := image.NewAlpha(image.Rect(0, 0, w, h))
	r.Draw(a, a.Bounds(), image.Opaque, image.Point{})
	return a
}

// scaled stretches y by k so the path fills a height rounded to whole pixels.
type scaled struct {
	r *vector.Rasterizer
	k float64
}

func (p scaled) y(v float32) float32 { return float32(float64(v) * p.k) }
func (p scaled) MoveTo(x, y float32) { p.r.MoveTo(x, p.y(y)) }
func (p scaled) LineTo(x, y float32) { p.r.LineTo(x, p.y(y)) }
func (p scaled) ClosePath()          { p.r.ClosePath() }
func (p scaled) CubeTo(a, b, c, d, e, f float32) {
	p.r.CubeTo(a, p.y(b), c, p.y(d), e, p.y(f))
}

// paint fills colour c through coverage m.
func paint(dst *image.NRGBA, m *image.Alpha, c color.NRGBA) {
	b := dst.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if a := m.AlphaAt(x, y).A; a > 0 {
				cc := c
				cc.A = a
				over(dst, x, y, cc)
			}
		}
	}
}

// over composites one non-premultiplied pixel onto dst.
func over(dst *image.NRGBA, x, y int, c color.NRGBA) {
	var u image.Uniform
	u.C = c
	draw.Draw(dst, image.Rect(x, y, x+1, y+1), &u, image.Point{}, draw.Over)
}

// mustHex parses a "#rrggbb" constant; TestPaletteHexParses covers every
// colour this file names, so a typo fails the tests rather than drawing black.
func mustHex(s string) color.NRGBA {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return color.NRGBA{uint8(v >> 16), uint8(v >> 8), uint8(v), 255}
}
