// heraldry-proto is the Phase 0 renderer prototype for heraldry (spec
// gardenio/docs/superpowers/specs/2026-10-25-heraldry-design.md §6, §11 0.1).
// It is throwaway, not product code: it exists so the sample sheet is drawn by
// the drawing rule the product's internal/heraldry will use, and to record
// whether Noto's SVGs render under oksvg. See README.md.
//
//	go run . svg    -docs ../../gardenio/docs/heraldry
//	go run . render -noto DIR -out OUT
//	go run . survey -noto DIR -out OUT
//	go run . round2 -noto DIR -out OUT [-arms FILE] [-grades FILE] [-lib FILE]
package main

import (
	"flag"
	"fmt"
	"image"
	"os"
	"path/filepath"
	"strings"
)

// Sample arms for the prototype sheet: no theme glyphs (R4), each kind of
// layout once, and a worst case of square artwork filling every place.
type sample struct {
	Name     string // ASCII, for labels and file names
	Arms     Arms
	Tincture string
}

var samples = []sample{
	{"owl", Arms{"", "", "🦉", "", ""}, "blue"},
	{"compass-anchor", Arms{"🧭", "", "", "", "⚓"}, "red"},
	{"fire-package-key", Arms{"🔥", "", "📦", "", "🔑"}, "green"},
	{"die-kite-magnet-canoe", Arms{"🎲", "🧲", "", "🪁", "🛶"}, "gold"},
	{"fox-wave-star-apple-bell", Arms{"🦊", "🌊", "⭐", "🍎", "🔔"}, "blue"},
	{"five-squares", Arms{"🟦", "🟨", "🟥", "🟩", "🟪"}, "sand"},
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	fs := flag.NewFlagSet(os.Args[1], flag.ExitOnError)
	docs := fs.String("docs", "../../gardenio/docs/heraldry", "where svg writes shield.svg and shield-grid.svg")
	noto := fs.String("noto", "", "noto-emoji checkout at "+NotoTag+" (see fetch-noto.sh)")
	out := fs.String("out", "out", "output directory for render, survey and proposals")
	arms := fs.String("arms", "../../gardenio/docs/heraldry/samples/round-2/arms.tsv", "round 2's arms, for round2")
	grades := fs.String("grades", "../../gardenio/docs/heraldry/samples/round-2/grades.tsv", "the reviewer's grades of round 2's arms (optional)")
	lib := fs.String("lib", "../../gardenio/docs/heraldry/library.md", "the library list, for round2")
	_ = fs.Parse(os.Args[2:])

	var err error
	switch os.Args[1] {
	case "svg":
		err = writeSVGs(*docs)
	case "render":
		err = needNoto(*noto, *out, func(n Noto) error { return renderAll(n, *out) })
	case "survey":
		err = needNoto(*noto, *out, func(n Noto) error {
			if err := survey(n, *out); err != nil {
				return err
			}
			return compareSheet(n, []string{"🦉", "🧭", "🔥", "📦", "🦊", "🌊", "🔔", "🟥"}, *out)
		})
	case "round2":
		err = needNoto(*noto, *out, func(n Noto) error { return round2(n, *arms, *grades, *lib, *out) })
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "heraldry-proto:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: heraldry-proto svg|render|survey|round2 [-docs DIR] [-noto DIR] [-out DIR] [-arms FILE] [-grades FILE] [-lib FILE]")
	os.Exit(2)
}

func needNoto(dir, out string, f func(Noto) error) error {
	if dir == "" {
		return fmt.Errorf("-noto is required (./fetch-noto.sh DIR)")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	return f(Noto{dir})
}

func writeSVGs(dir string) error {
	if err := os.WriteFile(filepath.Join(dir, "shield.svg"), []byte(ShieldSVG()), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "shield-grid.svg"), []byte(ShieldGridSVG()), 0o644)
}

// renderAll writes every picture (OUT/pictures/<arms>-<mode>-<w>@<dpr>x.png),
// the sample sheet, and a tincture strip.
func renderAll(n Noto, out string) error {
	pics := filepath.Join(out, "pictures")
	if err := os.MkdirAll(pics, 0o755); err != nil {
		return err
	}
	for _, sm := range samples {
		for _, m := range []Mode{Day, Night} {
			for _, w := range Sizes {
				for _, d := range DPRs {
					img, err := Render(sm.Arms, w, m, Tinctures[sm.Tincture], Style{DPR: d}, n)
					if err != nil {
						return err
					}
					if err := writePNG(filepath.Join(pics, fmt.Sprintf("%s-%s-%d@%dx.png", sm.Name, m.Name, w, d)), img); err != nil {
						return err
					}
				}
			}
		}
	}
	sh, err := sampleSheet(n)
	if err != nil {
		return err
	}
	if err := writePNG(filepath.Join(out, "sheet.png"), sh); err != nil {
		return err
	}
	ts, err := tinctureSheet(n)
	if err != nil {
		return err
	}
	return writePNG(filepath.Join(out, "tinctures.png"), ts)
}

// sampleSheet: one row per (arms, mode), the four sizes at 1×, 2× and 3×,
// each at its device pixels.
func sampleSheet(n Noto) (*image.NRGBA, error) {
	const gap, labelW = 8, 230
	rowH := heightFor(64)*3 + 16
	width := labelW
	for _, d := range DPRs {
		for _, w := range Sizes {
			width += w*d + gap
		}
		width += gap
	}
	height := 24 + len(samples)*2*rowH
	sh := newSheet(width, height, Day.Surface)
	x := labelW
	for _, d := range DPRs {
		sh.label(x, 16, fmt.Sprintf("64 40 24 16 at %dx", d), Day.Ink)
		for _, w := range Sizes {
			x += w*d + gap
		}
		x += gap
	}
	y := 24
	for _, sm := range samples {
		for _, m := range []Mode{Day, Night} {
			sh.fill(image.Rect(0, y, width, y+rowH), m.Surface)
			sh.label(6, y+18, sm.Name, m.Ink)
			for i, line := range describe(sm.Arms) {
				sh.label(6, y+34+14*i, line, m.Ink)
			}
			sh.label(6, y+80, m.Name+", "+sm.Tincture, m.Ink)
			x := labelW
			for _, d := range DPRs {
				for _, w := range Sizes {
					img, err := Render(sm.Arms, w, m, Tinctures[sm.Tincture], Style{DPR: d}, n)
					if err != nil {
						return nil, err
					}
					sh.put(img, x, y+8, 1)
					x += w*d + gap
				}
				x += gap
			}
			y += rowH
		}
	}
	return sh.img, nil
}

// tinctureSheet: the five-charge arms on every tincture, day and night, at
// 40 and 16 CSS px at 2×, without and with the keyline.
func tinctureSheet(n Noto) (*image.NRGBA, error) {
	arms := samples[4].Arms
	const colW, rowH = 260, 110
	sh := newSheet(70+len(TinctureOrder)*colW, 2*rowH+20, Day.Surface)
	for i, t := range TinctureOrder {
		sh.label(70+i*colW, 14, t+" (plain, keyline)", Day.Ink)
	}
	for r, m := range []Mode{Day, Night} {
		y := 20 + r*rowH
		sh.fill(image.Rect(0, y, 70+len(TinctureOrder)*colW, y+rowH), m.Surface)
		sh.label(6, y+20, m.Name, m.Ink)
		for i, t := range TinctureOrder {
			x := 70 + i*colW
			for _, kl := range []bool{false, true} {
				for _, w := range []int{40, 16} {
					img, err := Render(arms, w, m, Tinctures[t], Style{DPR: 2, Keyline: kl}, n)
					if err != nil {
						return nil, err
					}
					sh.put(img, x, y+6, 1)
					x += 2*w + 4
				}
			}
		}
	}
	return sh.img, nil
}

// names are the sample charges' Unicode names, lower case (the library's
// names come from 1.1a's generator; the prototype needs only these).
var names = map[string]string{
	"🦉": "owl", "🧭": "compass", "⚓": "anchor", "🔥": "fire", "📦": "package", "🔑": "key",
	"🎲": "game die", "🧲": "magnet", "🪁": "kite", "🛶": "canoe", "🦊": "fox", "🌊": "water wave",
	"⭐": "star", "🍎": "red apple", "🔔": "bell", "🟦": "blue square", "🟨": "yellow square",
	"🟥": "red square", "🟩": "green square", "🟪": "purple square",
}

// describe names the filled places in ASCII (the sheet font has no emoji),
// wrapped two places to a line.
func describe(a Arms) []string {
	var lines []string
	var cur []string
	for i, c := range a {
		if c == "" {
			continue
		}
		cur = append(cur, Places[i].Name+":"+names[c])
		if len(cur) == 2 {
			lines = append(lines, strings.Join(cur, " "))
			cur = nil
		}
	}
	if len(cur) > 0 {
		lines = append(lines, strings.Join(cur, " "))
	}
	return lines
}
