package main

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// sheet is a contact sheet: pictures placed at whole pixels, labelled in
// basicfont (ASCII only, so labels name charges in words).
type sheet struct{ img *image.NRGBA }

func newSheet(w, h int, bg color.NRGBA) *sheet {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), &image.Uniform{bg}, image.Point{}, draw.Src)
	return &sheet{img}
}

func (s *sheet) fill(r image.Rectangle, c color.NRGBA) {
	draw.Draw(s.img, r, &image.Uniform{c}, image.Point{}, draw.Src)
}

func (s *sheet) label(x, y int, text string, c color.NRGBA) {
	d := font.Drawer{Dst: s.img, Src: &image.Uniform{c}, Face: basicfont.Face7x13, Dot: fixed.P(x, y)}
	d.DrawString(text)
}

// put draws img at (x, y), magnified k× with nearest-neighbour so every
// pixel of a small picture shows.
func (s *sheet) put(img image.Image, x, y, k int) {
	b := img.Bounds()
	r := image.Rect(x, y, x+b.Dx()*k, y+b.Dy()*k)
	xdraw.NearestNeighbor.Scale(s.img, r, img, b, xdraw.Over, nil)
}

func resize(img image.Image, n int) *image.NRGBA {
	out := image.NewNRGBA(image.Rect(0, 0, n, n))
	xdraw.CatmullRom.Scale(out, out.Bounds(), img, img.Bounds(), xdraw.Over, nil)
	return out
}

// magnify returns img scaled k× nearest-neighbour (for the @3x copies).
func magnify(img image.Image, k int) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx()*k, b.Dy()*k))
	xdraw.NearestNeighbor.Scale(out, out.Bounds(), img, b, xdraw.Src, nil)
	return out
}

// Faces for the round-2 sheets, which are drawn at 2× or 3× and so need
// labels larger than basicfont's 7×13: Go Regular and Go Bold (both in
// x/image, BSD-licensed), parsed once by loadFonts.
var (
	goFonts   [2]*opentype.Font // regular, bold
	faceCache = map[string]font.Face{}
)

// loadFonts parses Go Regular and Go Bold; round2 calls it before drawing.
func loadFonts() error {
	for i, ttf := range [][]byte{goregular.TTF, gobold.TTF} {
		ft, err := opentype.Parse(ttf)
		if err != nil {
			return fmt.Errorf("parse Go font: %w", err)
		}
		goFonts[i] = ft
	}
	return nil
}

// face is Go Regular or Go Bold at px. Before loadFonts, or if NewFace
// fails (x/image returns a nil error for every option), it falls back to
// basicfont, so a mistake shows as small labels rather than a crash.
func face(bold bool, px float64) font.Face {
	k := fmt.Sprintf("%v/%g", bold, px)
	if f, ok := faceCache[k]; ok {
		return f
	}
	ft := goFonts[0]
	if bold {
		ft = goFonts[1]
	}
	if ft == nil {
		return basicfont.Face7x13
	}
	f, err := opentype.NewFace(ft, &opentype.FaceOptions{Size: px, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return basicfont.Face7x13
	}
	faceCache[k] = f
	return f
}

// text draws one line in f with its baseline at y.
func (s *sheet) text(x, y int, t string, c color.NRGBA, f font.Face) {
	d := font.Drawer{Dst: s.img, Src: &image.Uniform{c}, Face: f, Dot: fixed.P(x, y)}
	d.DrawString(t)
}

// wrap breaks t into lines no wider than w pixels in f.
func wrap(t string, w int, f font.Face) []string {
	var lines []string
	cur := ""
	for _, word := range strings.Fields(t) {
		try := word
		if cur != "" {
			try = cur + " " + word
		}
		if cur != "" && font.MeasureString(f, try).Ceil() > w {
			lines = append(lines, cur)
			cur = word
			continue
		}
		cur = try
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}
