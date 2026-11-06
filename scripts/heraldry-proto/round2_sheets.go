package main

import (
	"fmt"
	"image"
	"path/filepath"
	"strings"

	xdraw "golang.org/x/image/draw"
	"golang.org/x/image/font"
)

// rowLines is the text beside a row's pictures: kind and tincture, the
// places with their glosses, the reason line, the score and every FAIL.
func (r *Round2) rowLines(e *Entry, body, small font.Face) []textLine {
	const w = textW - 2*pad
	var out []textLine
	kind := e.Kind
	if e.Kind == "phase" {
		kind = "phase of " + e.Parent
	}
	tr := e.TReason
	if e.TinctureNote != "" {
		tr = e.TinctureNote
	}
	r1 := ""
	if e.Round1 != "new" {
		r1 = ", round 1 " + e.Round1
	}
	out = append(out, textLine{text: fmt.Sprintf("%s%s. Field %s: %s", kind, r1, e.Tincture, tr), colour: mutedInk, small: true})
	for i, c := range e.Arms {
		if c == "" {
			continue
		}
		g := e.Gloss[string(baseRunes(c))]
		for j, l := range wrap(fmt.Sprintf("%s  %s = %s", strings.ToUpper(Places[i].Name), r.Name(c), g), w-40, body) {
			in := 0
			if j > 0 {
				in = 40
			}
			out = append(out, textLine{text: l, indent: in, colour: Day.Ink})
		}
	}
	for _, l := range wrap("Why: "+e.Reason, w, body) {
		out = append(out, textLine{text: l, colour: Day.Ink})
	}
	sc := "Score " + e.scoreText(false)
	if e.G2Raw.V == "F" {
		sc += "; raw " + e.scoreText(true)
	}
	col := passInk
	if _, _, _, b := e.Score(false); b != "accept" {
		col = failRed
	}
	out = append(out, textLine{text: sc, colour: col})
	for _, k := range append(append([]string{}, Checks...), "G2raw") {
		g := e.Grades[k]
		if k == "G2raw" {
			g = e.G2Raw
		}
		if g.V != "F" && g.V != "U" {
			continue
		}
		label := k
		if k == "G2raw" {
			label = "G2 raw"
		}
		for j, l := range wrap(fmt.Sprintf("%s %s: %s", label, g.V, r.words(g.Note)), w-24, small) {
			in := 12
			if j > 0 {
				in = 36
			}
			c := failRed
			if g.V == "U" || k == "G2raw" {
				c = mutedInk
			}
			out = append(out, textLine{text: l, indent: in, small: true, colour: c})
		}
	}
	return out
}

// words replaces each emoji in s with its library name in brackets: the
// sheet's font (Go Regular) has no emoji, and the notes quote charges.
func (r *Round2) words(s string) string {
	var b strings.Builder
	rs := []rune(s)
	for i := 0; i < len(rs); i++ {
		c := rs[i]
		if c < 0x2190 || c == 0xFE0F || (c >= 0x2010 && c <= 0x2027) {
			if c != 0xFE0F {
				b.WriteRune(c)
			}
			continue
		}
		e := string(c)
		if i+1 < len(rs) && rs[i+1] == 0xFE0F {
			e += string(rs[i+1])
			i++
		}
		b.WriteString("[" + r.Name(e) + "]")
	}
	return b.String()
}

// page24 is every arms at 24 CSS px, the second slot (Commons card header,
// plot gate): at 2× day and night, and at 1× day and night shown doubled (how
// a 1× image looks on a 2× screen). The sheet is at 2×.
func (r *Round2) page24(path string) error {
	const cols, cellW, cellH = 6, 260, 118
	var all []*Entry
	for _, g := range r.groups() {
		all = append(all, g...)
	}
	rows := (len(all) + cols - 1) / cols
	sh := newSheet(cols*cellW+2*pad, header+rows*cellH+pad, Day.Surface)
	sh.text(pad, 36, "Heraldry round 2: every open epic at 24 CSS px (the Commons card header, the plan's plot gate). Sheet at 2x.", Day.Ink, face(true, 26))
	sh.text(pad, 68, "Each cell: 2x day, 2x night, then 1x day and 1x night shown doubled (a 1x bitmap on a 2x screen). Rubric G3: the centre must still read here.", Day.Ink, face(false, 19))
	sh.text(pad, 96, "Order: each plan then its phases, then the other epics, as on the 64/40 pages.", Day.Ink, face(false, 19))
	fs := face(false, 16)
	for i, e := range all {
		x, y := pad+(i%cols)*cellW, header+(i/cols)*cellH
		xx := x
		for _, spec := range []struct {
			m Mode
			d int
		}{{Day, 2}, {Night, 2}, {Day, 1}, {Night, 1}} {
			sh.fill(image.Rect(xx, y, xx+60, y+66), spec.m.Surface)
			img, err := Render(e.Arms, 24, spec.m, Tinctures[e.Tincture], Style{DPR: spec.d}, r.Src)
			if err != nil {
				return err
			}
			if spec.d == 1 {
				img = magnify(img, 2)
			}
			sh.put(img, xx+6, y+6, 1)
			xx += 62
		}
		sh.text(x, y+86, e.ID+" "+e.Tincture, Day.Ink, fs)
		short := e.Short
		for font.MeasureString(fs, short).Ceil() > cellW-12 && len(short) > 4 {
			short = short[:len(short)-2]
		}
		sh.text(x, y+106, short, mutedInk, fs)
	}
	return writePNG(path, sh.img)
}

// page40 is every arms at 40 CSS px, the first slots with room (viewer cards
// and lists, the Slack card image): 2× day and night. Rubric G5: every
// charge must be nameable here. The sheet is at 2×.
func (r *Round2) page40(path string) error {
	const cols, cellW, cellH = 7, 226, 150
	var all []*Entry
	for _, g := range r.groups() {
		all = append(all, g...)
	}
	rows := (len(all) + cols - 1) / cols
	sh := newSheet(cols*cellW+2*pad, header+rows*cellH+pad, Day.Surface)
	sh.text(pad, 36, "Heraldry round 2: every open epic at 40 CSS px (viewer cards and lists, the Slack card image). Sheet at 2x.", Day.Ink, face(true, 26))
	sh.text(pad, 68, "Each cell: day, then night. Rubric G5: every charge must still be nameable at this size.", Day.Ink, face(false, 19))
	sh.text(pad, 96, "Order: each plan then its phases, then the other epics, as on the 64/40 pages.", Day.Ink, face(false, 19))
	fs := face(false, 16)
	h40 := heightFor(80)
	for i, e := range all {
		x, y := pad+(i%cols)*cellW, header+(i/cols)*cellH
		xx := x
		for _, m := range []Mode{Day, Night} {
			sh.fill(image.Rect(xx, y, xx+96, y+h40+16), m.Surface)
			img, err := Render(e.Arms, 40, m, Tinctures[e.Tincture], Style{DPR: 2}, r.Src)
			if err != nil {
				return err
			}
			sh.put(img, xx+8, y+8, 1)
			xx += 100
		}
		sh.text(x, y+h40+36, e.ID+" "+e.Tincture, Day.Ink, fs)
		short := e.Short
		for font.MeasureString(fs, short).Ceil() > cellW-12 && len(short) > 4 {
			short = short[:len(short)-2]
		}
		sh.text(x, y+h40+56, short, mutedInk, fs)
	}
	return writePNG(path, sh.img)
}

// The farm's grass behind a phase stake: art.js's DAY "g" and its night()
// (gardenhub/web/art.js), so the stake is judged on the ground it stands on.
var (
	grassDay   = mustHex("#5fae3e")
	grassNight = mustHex("#39706a")
)

// stakePage is Q16's evidence: every phase's arms at the 16 px stake, at
// 1×, 2× and 3×, day and night, on the farm's grass. The sheet is at 3×: a
// 3× bitmap is shown at its pixels, a 2× one resampled by 1.5 and a 1× one
// tripled, which is what a 3× phone does with each.
func (r *Round2) stakePage(path string) error {
	const cols, k = 3, 3
	cw := 16 * k
	ch := heightFor(16 * k)
	cellW := 6*(cw+24) + 40
	cellH := ch + 80
	var phases []*Entry
	for _, g := range r.groups() {
		for _, e := range g {
			if e.Kind == "phase" {
				phases = append(phases, e)
			}
		}
	}
	rows := (len(phases) + cols - 1) / cols
	sh := newSheet(cols*cellW+2*pad, 170+rows*cellH+pad, Day.Surface)
	sh.text(pad, 50, "Heraldry round 2, Q16: every phase's arms at the World's 16 px phase stake, on the farm's grass. Sheet at 3x.", Day.Ink, face(true, 36))
	sh.text(pad, 96, "Each cell, day then night: 1x, 2x, 3x. On a 3x phone, 48 sheet pixels are the stake's 16 CSS px. Q16: keep the stake only if this clearly reads as the arms.", Day.Ink, face(false, 27))
	sh.text(pad, 138, "1x is tripled (what a phone does with a 1x bitmap), 2x is resampled by 1.5, 3x is shown at its own pixels.", Day.Ink, face(false, 27))
	fs := face(false, 24)
	for i, e := range phases {
		x, y := pad+(i%cols)*cellW, 170+(i/cols)*cellH
		xx := x
		for _, m := range []Mode{Day, Night} {
			g := grassDay
			if m.Night {
				g = grassNight
			}
			for _, d := range DPRs {
				sh.fill(image.Rect(xx, y, xx+cw+24, y+ch+24), g)
				img, err := Render(e.Arms, 16, m, Tinctures[e.Tincture], Style{DPR: d}, r.Src)
				if err != nil {
					return err
				}
				var shown *image.NRGBA
				switch d {
				case 1:
					shown = magnify(img, k)
				case 2:
					shown = image.NewNRGBA(image.Rect(0, 0, cw, ch))
					xdraw.CatmullRom.Scale(shown, shown.Bounds(), img, img.Bounds(), xdraw.Over, nil)
				default:
					shown = img
				}
				sh.put(shown, xx+12, y+12, 1)
				xx += cw + 24
			}
			xx += 20
		}
		sh.text(x, y+ch+56, fmt.Sprintf("%s  %s", e.ID, e.Short), Day.Ink, fs)
	}
	return writePNG(path, sh.img)
}

// TinctureSample is the arms the tincture pages show on all eight
// tinctures: round 1's plans that are still open, and a five-charge arms.
var TinctureSample = []string{
	"gd-N6I3Oa7uDg", "gd-obL67RJ1td", "gd-EGpbDpqsec", "gd-vmw5s07A1F",
	"gd-aD7jOGmAWW", "gd-QXq1fjYuRD", "gd-y28dPqgbgg", "gd-gh5J0vVbJP",
}

// tincturePages: the same arms on all eight tinctures (R13, Q13), day and
// night, at 64 and 24 CSS px at 2×, once raw and once with the keyline.
// A red bar under a raw picture marks a (charge, tincture) pair below 3:1,
// which Q14 (b) would refuse.
func (r *Round2) tincturePages(out string) error {
	for _, kl := range []bool{false, true} {
		const d = 2
		cell := 64*d + 24*d + 3*pad
		h64 := heightFor(64 * d)
		rowH := h64 + 2*pad + 12
		var rows []*Entry
		for _, id := range TinctureSample {
			if e := r.ByID[id]; e != nil {
				rows = append(rows, e)
			}
		}
		labelW := 340
		sh := newSheet(labelW+len(TinctureOrder)*cell+pad, header+len(rows)*2*rowH+pad, Day.Surface)
		what, file := "raw: a red bar marks a charge under 3:1 on that field, which Q14 (b) would refuse", "r2-5-tinctures-raw@2x.png"
		if kl {
			what, file = "with the keyline of Q14 (a): every charge outlined in the luminance opposite the field", "r2-5-tinctures-keyline@2x.png"
		}
		sh.text(pad, 36, "Heraldry round 2: the same arms on all eight tinctures, day and night, at 64 and 24 CSS px. Sheet at 2x.", Day.Ink, face(true, 26))
		sh.text(pad, 68, "This page: "+what+".", Day.Ink, face(false, 19))
		for i, tn := range TinctureOrder {
			t := Tinctures[tn]
			sh.text(labelW+i*cell+pad, 104, fmt.Sprintf("%s %s / %s", tn, t.DayHex, t.NightHex), Day.Ink, face(true, 18))
		}
		fb, fs := face(true, 19), face(false, 17)
		y := header
		for _, e := range rows {
			for _, m := range []Mode{Day, Night} {
				sh.fill(image.Rect(0, y, sh.img.Bounds().Dx(), y+rowH), m.Surface)
				sh.text(pad, y+30, e.ID, m.Ink, fb)
				for j, l := range wrap(e.Short, labelW-2*pad, fs) {
					if j < 2 {
						sh.text(pad, y+56+22*j, l, m.Ink, fs)
					}
				}
				sh.text(pad, y+110, m.Name+", own field: "+e.Tincture, m.Ink, fs)
				for i, tn := range TinctureOrder {
					x := labelW + i*cell + pad
					for _, w := range []int{64, 24} {
						img, err := Render(e.Arms, w, m, Tinctures[tn], Style{DPR: d, Keyline: kl}, r.Src)
						if err != nil {
							return err
						}
						sh.put(img, x, y+pad, 1)
						x += w*d + pad
					}
					if !kl {
						fails := 0
						for _, c := range e.charges() {
							if !r.verdict(c, tn, m).Pass {
								fails++
							}
						}
						if fails > 0 {
							x0 := labelW + i*cell + pad
							sh.fill(image.Rect(x0, y+pad+h64+4, x0+fails*28, y+pad+h64+10), mustHex("#e8202a"))
						}
					}
				}
				y += rowH
			}
		}
		if err := writePNG(filepath.Join(out, file), sh.img); err != nil {
			return err
		}
	}
	return nil
}

// pairsPage: G4's closest pairs, side by side at
// 24 CSS px at 2×, day and night, with their distance. The reviewer judges
// these by eye.
func (r *Round2) pairsPage(path string) error {
	var pairs []Pair
	for i, p := range r.Pairs {
		if i < G4Closest {
			pairs = append(pairs, p)
		}
	}
	const cols = 2
	cellW, cellH := 820, 150
	rows := (len(pairs) + cols - 1) / cols
	sh := newSheet(cols*cellW+2*pad, header+rows*cellH+pad, Day.Surface)
	sh.text(pad, 36, fmt.Sprintf("Heraldry round 2, G4: the %d closest pairs of the register, at 24 CSS px, 2x.", G4Closest), Day.Ink, face(true, 26))
	sh.text(pad, 68, "Distance: mean redmean over the two 24 px pictures, day and night averaged (lower is closer). Each pair: A and B by day, then by night.", Day.Ink, face(false, 19))
	fs := face(false, 17)
	for i, p := range pairs {
		x, y := pad+(i%cols)*cellW, header+(i/cols)*cellH
		xx := x
		for _, m := range []Mode{Day, Night} {
			for _, e := range []*Entry{p.A, p.B} {
				sh.fill(image.Rect(xx, y, xx+60, y+66), m.Surface)
				img, err := Render(e.Arms, 24, m, Tinctures[e.Tincture], Style{DPR: 2}, r.Src)
				if err != nil {
					return err
				}
				sh.put(img, xx+6, y+6, 1)
				xx += 62
			}
			xx += 16
		}
		pre := ""
		if p.Prefilter {
			pre = ", pre-filter flag"
		}
		sh.text(xx+8, y+24, fmt.Sprintf("#%d  distance %.1f%s", i+1, p.Dist, pre), Day.Ink, face(true, 18))
		sh.text(xx+8, y+50, fmt.Sprintf("A %s (%s)", p.A.ID, p.A.Tincture), Day.Ink, fs)
		sh.text(xx+8, y+72, trim(p.A.Short, 40), mutedInk, fs)
		sh.text(xx+8, y+96, fmt.Sprintf("B %s (%s)", p.B.ID, p.B.Tincture), Day.Ink, fs)
		sh.text(xx+8, y+118, trim(p.B.Short, 40), mutedInk, fs)
	}
	return writePNG(path, sh.img)
}

func trim(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
