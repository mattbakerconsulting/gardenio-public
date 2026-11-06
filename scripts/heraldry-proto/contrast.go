package main

import (
	"fmt"
	"image"
	"image/color"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// lum is WCAG 2's relative luminance of an sRGB colour.
func lum(c color.NRGBA) float64 {
	lin := func(v uint8) float64 {
		s := float64(v) / 255
		if s <= 0.04045 {
			return s / 12.92
		}
		return math.Pow((s+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.R) + 0.7152*lin(c.G) + 0.0722*lin(c.B)
}

// contrast is WCAG 2's contrast ratio between two relative luminances.
func contrast(a, b float64) float64 {
	if a < b {
		a, b = b, a
	}
	return (a + 0.05) / (b + 0.05)
}

// Threshold is the 1.4.11 non-text contrast the spec asks of every charge
// against its field (§6.6).
const Threshold = 3.0

// SinkShare and SinkDistance are round 1's sinking measure (rubric G1): a
// charge sinks when more than 35 % of its opaque pixels lie within redmean
// 110 of the field.
const (
	SinkShare    = 0.35
	SinkDistance = 110.0
)

// ChargeStats is what the matrix needs of one charge's artwork: the
// luminance of its opaque pixels at the median and at the darkest and
// lightest deciles (spec §6.6), its opaque area, its median colour (for G4's
// colour family) and its opaque pixels, kept for the sinking measure.
type ChargeStats struct {
	Emoji        string
	Median       float64
	P10, P90     float64
	Area         float64 // opaque share of the 72×72 tile
	MedianColour color.NRGBA
	opaque       []color.NRGBA
}

func Stats(emoji string, art image.Image) ChargeStats {
	b := art.Bounds()
	var ls []float64
	var px []color.NRGBA
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := color.NRGBAModel.Convert(art.At(x, y)).(color.NRGBA)
			if c.A < 128 {
				continue
			}
			c.A = 255
			px = append(px, c)
			ls = append(ls, lum(c))
		}
	}
	st := ChargeStats{Emoji: emoji, opaque: px, Area: float64(len(px)) / float64(b.Dx()*b.Dy())}
	if len(ls) == 0 {
		return st
	}
	idx := make([]int, len(ls))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return ls[idx[a]] < ls[idx[b]] })
	at := func(q float64) int { return idx[min(len(idx)-1, int(q*float64(len(idx))))] }
	st.P10, st.Median, st.P90 = ls[at(0.1)], ls[at(0.5)], ls[at(0.9)]
	st.MedianColour = px[at(0.5)]
	return st
}

// Verdict is one (charge, field) cell of the contrast matrix.
type Verdict struct {
	Median, Far float64 // contrast of the median and of the decile on the far side of the field
	Pass        bool    // rubric G2's rule: median ≥ 3, or the far decile ≥ 3
	Sink        float64 // share of opaque pixels near the field colour (G1)
}

// Judge measures a charge against a field. The far decile is whichever of
// the darkest and lightest deciles lies farther from the field's luminance:
// the edge a charge keeps even when its body sits near the field (rubric §7
// point 4).
func (st ChargeStats) Judge(field color.NRGBA) Verdict {
	lf := lum(field)
	v := Verdict{Median: contrast(st.Median, lf), Far: math.Max(contrast(st.P10, lf), contrast(st.P90, lf))}
	v.Pass = v.Median >= Threshold || v.Far >= Threshold
	near := 0
	for _, c := range st.opaque {
		if redmean(c, field) < SinkDistance {
			near++
		}
	}
	if len(st.opaque) > 0 {
		v.Sink = float64(near) / float64(len(st.opaque))
	}
	return v
}

func redmean(a, b color.NRGBA) float64 {
	rm := (float64(a.R) + float64(b.R)) / 2
	dr, dg, db := float64(a.R)-float64(b.R), float64(a.G)-float64(b.G), float64(a.B)-float64(b.B)
	return math.Sqrt((2+rm/256)*dr*dr + 4*dg*dg + (2+(255-rm)/256)*db*db)
}

// Family buckets a colour for G4's pre-filter: grey when it has little
// chroma, else one of eight hue sectors.
func Family(c color.NRGBA) string {
	r, g, b := float64(c.R)/255, float64(c.G)/255, float64(c.B)/255
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	if mx-mn < 0.15 {
		switch {
		case mx < 0.3:
			return "black"
		case mn > 0.75:
			return "white"
		}
		return "grey"
	}
	var hue float64
	switch mx {
	case r:
		hue = math.Mod((g-b)/(mx-mn), 6)
	case g:
		hue = (b-r)/(mx-mn) + 2
	default:
		hue = (r-g)/(mx-mn) + 4
	}
	hue = math.Mod(hue*60+360, 360)
	return []string{"red", "orange", "yellow", "green", "teal", "blue", "purple", "pink"}[int(hue/45)%8]
}

// Matrix is every library charge measured against every tincture in both
// palettes.
type Matrix struct {
	Charges []Charge
	Stats   map[string]ChargeStats
	Cell    map[string]map[string]Verdict // emoji → "tincture/mode" → verdict
}

func cellKey(t string, m Mode) string { return t + "/" + m.Name }

func BuildMatrix(lib []Charge, src Charges) (*Matrix, error) {
	mx := &Matrix{Charges: lib, Stats: map[string]ChargeStats{}, Cell: map[string]map[string]Verdict{}}
	for _, c := range lib {
		art, err := src.PNG72(c.Emoji)
		if err != nil {
			return nil, err
		}
		st := Stats(c.Emoji, art)
		mx.Stats[c.Emoji] = st
		row := map[string]Verdict{}
		for _, t := range TinctureOrder {
			for _, m := range []Mode{Day, Night} {
				row[cellKey(t, m)] = st.Judge(Tinctures[t].Colour(m))
			}
		}
		mx.Cell[c.Emoji] = row
	}
	return mx, nil
}

// StatsFor measures a charge outside the library (it should not happen; the
// round's validator refuses such charges first).
func (mx *Matrix) StatsFor(e string, src Charges) (ChargeStats, error) {
	if st, ok := mx.Stats[e]; ok {
		return st, nil
	}
	art, err := src.PNG72(e)
	if err != nil {
		return ChargeStats{}, err
	}
	st := Stats(e, art)
	mx.Stats[e] = st
	return st, nil
}

// WriteTSV writes the whole matrix: one row per charge, per (tincture, mode)
// the median and far-decile ratios and the verdict.
func (mx *Matrix) WriteTSV(path string, name func(string) string) error {
	var sb strings.Builder
	sb.WriteString("# emoji\tname")
	for _, t := range TinctureOrder {
		for _, m := range []Mode{Day, Night} {
			fmt.Fprintf(&sb, "\t%s/%s", t, m.Name)
		}
	}
	sb.WriteString("\n# each cell: median ratio / far-decile ratio, then P (passes 3:1 by rubric G2's rule) or F\n")
	for _, c := range mx.Charges {
		fmt.Fprintf(&sb, "%s\t%s", c.Emoji, name(c.Emoji))
		for _, t := range TinctureOrder {
			for _, m := range []Mode{Day, Night} {
				v := mx.Cell[c.Emoji][cellKey(t, m)]
				pf := "F"
				if v.Pass {
					pf = "P"
				}
				fmt.Fprintf(&sb, "\t%.2f/%.2f %s", v.Median, v.Far, pf)
			}
		}
		sb.WriteString("\n")
	}
	return os.WriteFile(path, []byte(sb.String()), 0o644)
}

// Fails counts, per (tincture, mode), the charges that fail G2 raw.
func (mx *Matrix) Fails() map[string]int {
	out := map[string]int{}
	for _, row := range mx.Cell {
		for k, v := range row {
			if !v.Pass {
				out[k]++
			}
		}
	}
	return out
}

// matrixSheets writes, per tincture, the library on that field in four
// panels: day raw with the refused charges marked (Q14 (b)), day with the
// keyline (Q14 (a)), and the same by night. Charges at 16 CSS px, 1×.
func matrixSheets(mx *Matrix, src Charges, out string) error {
	const cols, cell, top, panelGap = 37, 22, 40, 16
	rows := (len(mx.Charges) + cols - 1) / cols
	pw, ph := cols*cell, rows*cell
	for _, tn := range TinctureOrder {
		t := Tinctures[tn]
		sh := newSheet(2*pw+3*panelGap, 2*(ph+top)+panelGap, Day.Surface)
		for r, m := range []Mode{Day, Night} {
			field := t.Colour(m)
			kl := KeylineFor(field)
			for k, keyline := range []bool{false, true} {
				x0, y0 := panelGap+k*(pw+panelGap), r*(ph+top+panelGap/2)
				sh.fill(image.Rect(x0-panelGap/2, y0, x0+pw+panelGap/2, y0+ph+top), m.Surface)
				fails := 0
				var body string
				if keyline {
					body = fmt.Sprintf("Q14 (a) keyline (%s): every charge outlined, so every pair passes", hexOf(kl))
				}
				sh.fill(image.Rect(x0, y0+top-4, x0+pw, y0+top+ph), field)
				for i, c := range mx.Charges {
					x, y := x0+(i%cols)*cell+3, y0+top+(i/cols)*cell+3
					img, err := chargeTile(c.Emoji, 16, field, keyline, src)
					if err != nil {
						return err
					}
					v := mx.Cell[c.Emoji][cellKey(tn, m)]
					if !keyline && !v.Pass {
						fails++
						sh.put(fade(img, field, 0.35), x, y, 1)
						// Refused: a red bar under the charge.
						sh.fill(image.Rect(x-2, y+16, x+18, y+19), mustHex("#e8202a"))
						continue
					}
					sh.put(img, x, y, 1)
				}
				if !keyline {
					body = fmt.Sprintf("Q14 (b) refusal: %d of %d charges fail 3:1 and are refused (faded, red bar)", fails, len(mx.Charges))
				}
				sh.label(x0, y0+16, fmt.Sprintf("%s %s, field %s", tn, m.Name, hexOf(field)), m.Ink)
				sh.label(x0, y0+30, body, m.Ink)
			}
		}
		if err := writePNG(filepath.Join(out, "matrix-"+tn+".png"), sh.img); err != nil {
			return err
		}
	}
	return nil
}

// chargeTile is one charge at n device px on a square of the field, with or
// without the keyline: the library contact sheet's cell.
func chargeTile(e string, n int, field color.NRGBA, keyline bool, src Charges) (*image.NRGBA, error) {
	art, err := src.PNG72(e)
	if err != nil {
		return nil, err
	}
	layer := resize(art, n)
	img := image.NewNRGBA(layer.Bounds())
	if keyline {
		paint(img, dilate(layer, math.Max(float64(n)/24, 1)), KeylineFor(field))
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if c := layer.NRGBAAt(x, y); c.A > 0 {
				over(img, x, y, c)
			}
		}
	}
	return img, nil
}

// fade blends img toward the field by k (refused charges on the matrix).
func fade(img *image.NRGBA, field color.NRGBA, k float64) *image.NRGBA {
	out := image.NewNRGBA(img.Bounds())
	for i := 0; i+3 < len(img.Pix); i += 4 {
		out.Pix[i] = uint8(float64(img.Pix[i])*k + float64(field.R)*(1-k))
		out.Pix[i+1] = uint8(float64(img.Pix[i+1])*k + float64(field.G)*(1-k))
		out.Pix[i+2] = uint8(float64(img.Pix[i+2])*k + float64(field.B)*(1-k))
		out.Pix[i+3] = img.Pix[i+3]
	}
	return out
}

func hexOf(c color.NRGBA) string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }
