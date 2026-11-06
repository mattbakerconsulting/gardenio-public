// Raster output for plan badges. The badge is authored ONCE as SVG (tier0.go /
// model.go); this file turns that same SVG into a PNG so consumers that cannot
// render SVG inline — Slack unfurls, GitLab MR/Jira <img> tags, OpenGraph
// preview cards, favicons — still get the plan's glyph. SVG stays the source of
// truth; the raster is a derived, content-addressed artifact.
//
// Toolchain choice (documented cost/benefit in docs/badge-art-spec.md, Raster):
// rasterization is done with the PURE-GO pair github.com/srwiley/oksvg +
// rasterx — no CGo, no external binary (rsvg-convert / inkscape / imagemagick),
// so it builds and runs anywhere the controller does, including the in-box
// (Gasbox) image with zero extra system packages. The one real limitation of
// that pair is that it does NOT render SVG <text> — so the Tier-0 monogram,
// which IS the badge's identity, would silently vanish (a bare colored disc).
// We therefore rasterize the geometry with oksvg and OVERLAY the <text> glyph
// ourselves with a bundled Go font (golang.org/x/image/font + gofont), scaled
// out of the badge's 100x100 viewBox. Tier-2 (model-authored) badges are line
// art built from <path>/<circle>/<polygon>, which oksvg renders natively, so
// the overlay is a no-op there.
//
// oksvg's SetTarget maps path GEOMETRY from the viewBox to pixels but leaves two
// things in viewBox units, which is only right when the target is 100px: a
// stroke-width is used as pixels, and a userSpaceOnUse gradient's coordinates
// are too. At 512px a 6-unit stroke would draw 6px wide instead of 31px and a
// background gradient would sit in the top-left corner. Rasterize corrects both
// (scaleStrokes, targetUserSpaceGradients), so every size is the same picture.
package badge

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"regexp"
	"strconv"
	"strings"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// DefaultRasterSize is the default square edge, in pixels, for a rendered
// badge. 128px is the plan-detail header size (tier0.go's comment) and a clean
// power-of-two that downsizes crisply to 40/24px on the client.
const DefaultRasterSize = 128

// MaxRasterSize is the largest square edge Rasterize renders: 512px, the
// poster size (gd-RcmmFKWytW). The geometry and the text overlay both scale
// out of the 100x100 viewBox, so every size up to it is the same picture; the
// cap only stops a stray --size from allocating a huge canvas.
const MaxRasterSize = 512

// RasterResult is a rendered PNG plus the metadata a content-addressed cache
// and an uploader need: the raw bytes, their SHA-256 (hex), and the square
// pixel size they were rendered at.
type RasterResult struct {
	// PNG is the encoded image bytes.
	PNG []byte
	// SHA256 is the lowercase hex SHA-256 of PNG — the content address. Two
	// renders of byte-identical SVG at the same size share this hash, so an
	// upload keyed on it is idempotent.
	SHA256 string
	// Size is the square edge in pixels the badge was rendered at.
	Size int
}

// Rasterize renders a badge's SVG to a square PNG of the given edge in pixels
// (<=0 selects DefaultRasterSize, above MaxRasterSize is an error). It rasterizes the vector geometry with the
// pure-Go oksvg/rasterx pair, then overlays any <text> element with a bundled
// font because that pair does not draw SVG text (see the package comment).
func (b Badge) Rasterize(size int) (RasterResult, error) {
	if size <= 0 {
		size = DefaultRasterSize
	}
	if size > MaxRasterSize {
		return RasterResult{}, fmt.Errorf("raster size %dpx is over the %dpx maximum", size, MaxRasterSize)
	}

	icon, err := readIcon(b.SVG, size)
	if err != nil {
		return RasterResult{}, err
	}
	// userSpaceOnUse gradients are fixed in the source, so the SVG is read a
	// second time with them mapped to the target. A badge without one (every
	// Tier-0 badge) is read once.
	if svg, ok := targetUserSpaceGradients(b.SVG, icon.Transform); ok {
		if icon, err = readIcon(svg, size); err != nil {
			return RasterResult{}, err
		}
	}
	scaleStrokes(icon, float64(size)/icon.ViewBox.W)

	rgba := image.NewRGBA(image.Rect(0, 0, size, size))
	scanner := rasterx.NewScannerGV(size, size, rgba, rgba.Bounds())
	raster := rasterx.NewDasher(size, size, scanner)
	icon.Draw(raster, 1.0)

	// Overlay <text> (the Tier-0 monogram). Tier-2 line art has no <text>, so
	// extractText returns ok=false and this is skipped.
	if t, ok := extractText(b.SVG); ok {
		if err := overlayText(rgba, t, icon.ViewBox.W, size); err != nil {
			return RasterResult{}, fmt.Errorf("overlaying badge text: %w", err)
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, rgba); err != nil {
		return RasterResult{}, fmt.Errorf("encoding PNG: %w", err)
	}
	sum := sha256.Sum256(buf.Bytes())
	return RasterResult{
		PNG:    buf.Bytes(),
		SHA256: hex.EncodeToString(sum[:]),
		Size:   size,
	}, nil
}

// readIcon parses a badge SVG and targets it at a size x size canvas.
func readIcon(svg string, size int) (*oksvg.SvgIcon, error) {
	icon, err := oksvg.ReadIconStream(strings.NewReader(svg))
	if err != nil {
		return nil, fmt.Errorf("parsing badge SVG: %w", err)
	}
	// The badge is authored with a viewBox but no width/height; if a parse ever
	// yields a zero viewBox, fall back to the known 100x100 canvas so SetTarget
	// cannot divide by zero.
	if icon.ViewBox.W == 0 || icon.ViewBox.H == 0 {
		icon.ViewBox.W = canvas
		icon.ViewBox.H = canvas
	}
	icon.SetTarget(0, 0, float64(size), float64(size))
	return icon, nil
}

// scaleStrokes multiplies every path's stroke width and dash pattern by scale
// (pixels per viewBox unit), which SetTarget does not do. The miter limit is a
// ratio of the width, so it is left alone.
func scaleStrokes(icon *oksvg.SvgIcon, scale float64) {
	for i := range icon.SVGPaths {
		p := &icon.SVGPaths[i]
		p.LineWidth *= scale
		p.DashOffset *= scale
		for j := range p.Dash {
			p.Dash[j] *= scale
		}
	}
}

var (
	gradientTagRe       = regexp.MustCompile(`(?s)<(?:linearGradient|radialGradient)\b[^>]*>`)
	userSpaceOnUseRe    = regexp.MustCompile(`gradientUnits\s*=\s*["']userSpaceOnUse["']`)
	gradientTransformRe = regexp.MustCompile(`gradientTransform\s*=\s*(?:"([^"]*)"|'([^']*)')`)
)

// targetUserSpaceGradients rewrites every userSpaceOnUse gradient in svg so its
// gradientTransform starts with t, the viewBox-to-pixel transform: oksvg reads
// such a gradient's coordinates as pixels, so without this it is drawn at the
// viewBox's scale whatever the target size. An objectBoundingBox gradient is
// measured on the drawn path and needs nothing. ok is false when svg has no
// userSpaceOnUse gradient.
func targetUserSpaceGradients(svg string, t rasterx.Matrix2D) (string, bool) {
	f := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	m := "matrix(" + strings.Join([]string{f(t.A), f(t.B), f(t.C), f(t.D), f(t.E), f(t.F)}, " ") + ")"
	changed := false
	out := gradientTagRe.ReplaceAllStringFunc(svg, func(tag string) string {
		if !userSpaceOnUseRe.MatchString(tag) {
			return tag
		}
		changed = true
		if loc := gradientTransformRe.FindStringSubmatchIndex(tag); loc != nil {
			lo, hi := loc[2], loc[3] // double-quoted value
			if lo < 0 {
				lo, hi = loc[4], loc[5] // single-quoted
			}
			old := tag[lo:hi]
			return tag[:loc[0]] + `gradientTransform="` + m + " " + old + `"` + tag[loc[1]:]
		}
		// Insert after the element name: "<linearGradient" or "<radialGradient".
		name := len("<linearGradient")
		if strings.HasPrefix(tag, "<radialGradient") {
			name = len("<radialGradient")
		}
		return tag[:name] + ` gradientTransform="` + m + `"` + tag[name:]
	})
	return out, changed
}

// svgText is a parsed <text> element from a badge SVG: the string to draw, its
// fill color, the font-size in viewBox units, and the (x,y) anchor in viewBox
// units. Tier-0 authors exactly one such element, centered.
type svgText struct {
	content  string
	fill     color.Color
	fontSize float64
	x, y     float64
}

var (
	textElemRe = regexp.MustCompile(`(?s)<text\b([^>]*)>(.*?)</text>`)
	attrRe     = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
)

// extractText pulls the first <text> element out of an SVG and unpacks the
// attributes Tier-0 sets (x, y, fill, font-size, text-anchor). It returns
// ok=false when there is no <text> (e.g. a Tier-2 line-art badge), so callers
// skip the overlay entirely.
func extractText(svg string) (svgText, bool) {
	m := textElemRe.FindStringSubmatch(svg)
	if m == nil {
		return svgText{}, false
	}
	t := svgText{
		content:  unescapeXML(strings.TrimSpace(m[2])),
		fill:     color.White,
		fontSize: 40,
		x:        canvas / 2.0,
		y:        canvas / 2.0,
	}
	for _, a := range attrRe.FindAllStringSubmatch(m[1], -1) {
		switch a[1] {
		case "x":
			t.x = atof(a[2], t.x)
		case "y":
			t.y = atof(a[2], t.y)
		case "font-size":
			t.fontSize = atof(a[2], t.fontSize)
		case "fill":
			if c, ok := parseHexColor(a[2]); ok {
				t.fill = c
			}
		}
	}
	if t.content == "" {
		return svgText{}, false
	}
	return t, true
}

// overlayText draws t onto img. The badge's coordinates are in viewBox units
// (viewBox is a square of edge viewBoxW); we scale by size/viewBoxW to reach
// pixels. Tier-0 centers the text with text-anchor="middle" and
// dominant-baseline="central", so we center horizontally on t.x and vertically
// on t.y using the glyph metrics.
func overlayText(img *image.RGBA, t svgText, viewBoxW float64, size int) error {
	if viewBoxW == 0 {
		viewBoxW = canvas
	}
	scale := float64(size) / viewBoxW

	fnt, err := opentype.Parse(gobold.TTF)
	if err != nil {
		return fmt.Errorf("parsing bundled font: %w", err)
	}
	face, err := opentype.NewFace(fnt, &opentype.FaceOptions{
		Size:    t.fontSize * scale,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return fmt.Errorf("building font face: %w", err)
	}
	defer face.Close()

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(t.fill),
		Face: face,
	}

	adv := d.MeasureString(t.content)
	metrics := face.Metrics()
	// Center horizontally: anchor.x*scale is the midpoint; subtract half the
	// advance. Center vertically on the cap/x-mid: dominant-baseline="central"
	// puts the geometric center of the text on t.y, so the baseline sits below
	// t.y by roughly half the cap height. (Ascent+Descent)/2 - Descent is a
	// good approximation that keeps a two-letter monogram optically centered.
	cx := t.x * scale
	cy := t.y * scale
	baseline := cy + (float64(metrics.Ascent-metrics.Descent)/2)/64.0
	d.Dot = fixed.Point26_6{
		X: fixed.Int26_6(cx*64) - adv/2,
		Y: fixed.Int26_6(baseline * 64),
	}
	d.DrawString(t.content)
	return nil
}

// parseHexColor parses #rgb / #rrggbb into a color.Color. Named colors and
// other forms return ok=false (the caller keeps the default white).
func parseHexColor(s string) (color.Color, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "#") {
		return nil, false
	}
	hexStr := s[1:]
	switch len(hexStr) {
	case 3:
		// #rgb -> #rrggbb
		hexStr = string([]byte{hexStr[0], hexStr[0], hexStr[1], hexStr[1], hexStr[2], hexStr[2]})
	case 6:
	default:
		return nil, false
	}
	v, err := hex.DecodeString(hexStr)
	if err != nil {
		return nil, false
	}
	return color.RGBA{R: v[0], G: v[1], B: v[2], A: 0xff}, true
}

// atof parses a float, tolerating a trailing unit (e.g. "40px"), and returns
// def on failure.
func atof(s string, def float64) float64 {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "px")
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return def
}

// unescapeXML reverses escapeXML so an overlaid monogram shows "&" not "&amp;".
func unescapeXML(s string) string {
	return strings.NewReplacer(
		"&amp;", "&",
		"&lt;", "<",
		"&gt;", ">",
		"&quot;", `"`,
		"&apos;", "'",
	).Replace(s)
}
