package main

import (
	"image"
	"image/color"
	"math"
)

// Shape is a shield outline on the 64×72 grid and its five places (spec
// §9 Q11: round 1 shows the same arms on four shapes once). The heater is the
// standard drawing (shield.go); the other three exist only for the sheet.
type Shape struct {
	Name string
	// Trace draws the outline inset by `in` units (shield.go tracePath's
	// contract). When Exact is false the inset is ignored and the field is the
	// outer mask eroded by one pixel instead.
	Trace  func(p pather, in, s, ox, oy float64)
	Exact  bool
	Places []Place
	Size   float64 // place size in units; 0 is PlaceSz
}

func (sh Shape) placeSz() float64 {
	if sh.Size > 0 {
		return sh.Size
	}
	return PlaceSz
}

// masks returns the outer silhouette and the field (the cloth inside the
// outline). The outline is never thinner than 1 px (render.go).
func (sh Shape) masks(w, h int, s, sy float64) (outer, field *image.Alpha) {
	outer = traceMask(sh.Trace, w, h, 0, s, sy)
	if sh.Exact {
		return outer, traceMask(sh.Trace, w, h, math.Max(Outline, 1/s), s, sy)
	}
	// Every size here is ≤ 64 px, where one unit is at most one pixel, so
	// the outline is exactly one pixel: erode by one.
	return outer, erode(outer)
}

// erode is the 4-neighbour minimum of an alpha mask.
func erode(m *image.Alpha) *image.Alpha {
	b := m.Bounds()
	at := func(x, y int) uint8 {
		if !image.Pt(x, y).In(b) {
			return 0
		}
		return m.AlphaAt(x, y).A
	}
	out := image.NewAlpha(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			v := at(x, y)
			for _, n := range []uint8{at(x-1, y), at(x+1, y), at(x, y-1), at(x, y+1)} {
				v = min(v, n)
			}
			out.SetAlpha(x, y, color.Alpha{v})
		}
	}
	return out
}

var Heater = Shape{Name: "heater", Trace: tracePath, Exact: true, Places: Places}

// Shapes are Q11's four candidates, the standard heater first.
var Shapes = []Shape{
	Heater,
	// Round: an oval filling the grid (a circle would leave 8 units of the
	// 64×72 slot empty). The corners are pulled in and the places shrunk to
	// 22 units so a round charge clears the rim by ~1.5 units, as on the heater.
	{Name: "round", Trace: traceOval, Exact: true, Size: 22, Places: []Place{
		{"tl", 6, 8}, {"tr", 36, 8}, {"c", 21, 25}, {"bl", 6, 42}, {"br", 36, 42},
	}},
	// Square with a point: straight sides to y = 56, then straight to the
	// point. Roomier at the bottom than the heater, so the heater's places fit.
	{Name: "square-point", Trace: polyTrace([][2]float64{{0, 0}, {64, 0}, {64, 56}, {32, 72}, {0, 56}}), Places: Places},
	// The first draft's gonfalon: a banner with three tails.
	{Name: "banner", Trace: polyTrace([][2]float64{{0, 0}, {64, 0}, {64, 62}, {53.3, 72}, {42.7, 62}, {32, 72}, {21.3, 62}, {10.7, 72}, {0, 62}}), Places: Places},
}

// traceOval traces an ellipse filling the grid, inset by `in` (an exact
// parallel offset only for a circle; close enough for a 1-unit line).
func traceOval(p pather, in, s, ox, oy float64) {
	cx, cy, rx, ry := GridW/2, GridH/2, GridW/2-in, GridH/2-in
	pt := func(x, y float64) (float32, float32) { return float32(ox + (cx+x*rx)*s), float32(oy + (cy+y*ry)*s) }
	p.MoveTo(pt(1, 0))
	for q := range 4 {
		a0 := float64(q) * math.Pi / 2
		arcTo(p, pt, 0, 0, 1, a0, a0+math.Pi/2)
	}
	p.ClosePath()
}

// polyTrace traces a polygon in grid units (no exact inset: Exact is false).
func polyTrace(pts [][2]float64) func(p pather, in, s, ox, oy float64) {
	return func(p pather, _, s, ox, oy float64) {
		pt := func(v [2]float64) (float32, float32) { return float32(ox + v[0]*s), float32(oy + v[1]*s) }
		p.MoveTo(pt(pts[0]))
		for _, v := range pts[1:] {
			p.LineTo(pt(v))
		}
		p.ClosePath()
	}
}
