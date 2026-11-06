package main

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

// renderSVG72 rasterises one Noto SVG to 72×72 with oksvg. strict reports
// whether oksvg parsed it in StrictErrorMode (an unsupported element or
// attribute is an error); a file that fails strict is retried in
// IgnoreErrorMode, which drops what oksvg does not know.
func renderSVG72(path string) (img *image.NRGBA, strict bool, strictErr, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false, nil, err
	}
	icon, strictErr := oksvg.ReadIconStream(bytes.NewReader(data), oksvg.StrictErrorMode)
	strict = strictErr == nil
	if !strict {
		icon, err = oksvg.ReadIconStream(bytes.NewReader(data), oksvg.IgnoreErrorMode)
		if err != nil {
			return nil, false, strictErr, err
		}
	}
	icon.SetTarget(0, 0, 72, 72)
	rgba := image.NewRGBA(image.Rect(0, 0, 72, 72))
	sc := rasterx.NewScannerGV(72, 72, rgba, rgba.Bounds())
	icon.Draw(rasterx.NewDasher(72, 72, sc), 1)
	out := image.NewNRGBA(rgba.Bounds())
	for y := 0; y < 72; y++ {
		for x := 0; x < 72; x++ {
			out.Set(x, y, rgba.At(x, y))
		}
	}
	return out, strict, strictErr, nil
}

// diff is the mean absolute per-channel difference between two 72×72
// pictures composited on white, in 0..1. Noto's 72-px PNGs are rendered from
// the same SVGs by the release's own pipeline (resvg-class quality), so this
// is how far oksvg is from the reference.
func diff(a, b image.Image) float64 {
	var sum float64
	for y := 0; y < 72; y++ {
		for x := 0; x < 72; x++ {
			ar, ag, ab := onWhite(a, x, y)
			br, bg, bb := onWhite(b, x, y)
			sum += math.Abs(ar-br) + math.Abs(ag-bg) + math.Abs(ab-bb)
		}
	}
	return sum / (72 * 72 * 3)
}

func onWhite(img image.Image, x, y int) (float64, float64, float64) {
	r, g, b, a := img.At(x, y).RGBA() // premultiplied, 0..65535
	w := 65535 - float64(a)
	return (float64(r) + w) / 65535, (float64(g) + w) / 65535, (float64(b) + w) / 65535
}

type surveyRow struct {
	Emoji     string
	File      string
	Strict    bool
	StrictErr string
	Err       string
	Diff      float64
}

var singleCP = regexp.MustCompile(`^emoji_u([0-9a-f]{4,6})\.svg$`)

// survey renders every single-codepoint Noto SVG through oksvg and compares
// it with the release's 72-px PNG of the same emoji. Regional-indicator
// letters, skin-tone modifiers and the keycap bases are skipped: none can be
// a charge (spec R2/R3).
func survey(n Noto, outDir string) error {
	ents, err := os.ReadDir(filepath.Join(n.Dir, "2D", "svg"))
	if err != nil {
		return err
	}
	var rows []surveyRow
	for _, e := range ents {
		m := singleCP.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		cp, _ := strconv.ParseUint(m[1], 16, 32)
		if skipCP(rune(cp)) {
			continue
		}
		emoji := string(rune(cp))
		row := surveyRow{Emoji: emoji, File: e.Name()}
		img, strict, serr, err := renderSVG72(filepath.Join(n.Dir, "2D", "svg", e.Name()))
		row.Strict = strict
		if serr != nil {
			row.StrictErr = firstLine(serr.Error())
		}
		if err != nil {
			row.Err = firstLine(err.Error())
			rows = append(rows, row)
			continue
		}
		ref, err := n.PNG72(emoji)
		if err != nil {
			row.Err = "no PNG: " + firstLine(err.Error())
			rows = append(rows, row)
			continue
		}
		row.Diff = diff(img, ref)
		rows = append(rows, row)
	}
	return writeSurvey(rows, outDir)
}

func skipCP(r rune) bool {
	switch {
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators (flags)
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin-tone modifiers
		return true
	case r == '#' || r == '*' || (r >= '0' && r <= '9'): // keycap bases
		return true
	case r >= 0x1F9B0 && r <= 0x1F9B3: // hair components
		return true
	}
	return false
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}

// Diff bands: ≤2% is indistinguishable at 64 px and below; >8% is visibly
// wrong (a missing gradient or a dropped shape) even at 24 px.
const (
	diffGood = 0.02
	diffBad  = 0.08
)

func writeSurvey(rows []surveyRow, outDir string) error {
	var strict, ignored, failed, good, fair, bad int
	var diffs []float64
	for _, r := range rows {
		switch {
		case r.Err != "":
			failed++
			continue
		case r.Strict:
			strict++
		default:
			ignored++
		}
		diffs = append(diffs, r.Diff)
		switch {
		case r.Diff <= diffGood:
			good++
		case r.Diff <= diffBad:
			fair++
		default:
			bad++
		}
	}
	sort.Float64s(diffs)
	pct := func(p float64) float64 {
		if len(diffs) == 0 {
			return 0
		}
		return diffs[int(p*float64(len(diffs)-1))] * 100
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# oksvg survey: Noto Emoji %s, single-codepoint SVGs\n\n", NotoTag)
	fmt.Fprintf(&b, "Each SVG rasterised by oksvg at 72×72 and compared, on white, with the release's own 72-px PNG of the same emoji (mean absolute channel difference).\n\n")
	fmt.Fprintf(&b, "| | count |\n|---|---|\n")
	fmt.Fprintf(&b, "| SVGs surveyed | %d |\n| parsed in StrictErrorMode | %d |\n| parsed only in IgnoreErrorMode (something dropped) | %d |\n| failed to parse at all | %d |\n", len(rows), strict, ignored, failed)
	fmt.Fprintf(&b, "| rendered, diff ≤ %.0f%% (matches the PNG) | %d |\n| rendered, diff %.0f–%.0f%% (visibly off at 64 px) | %d |\n| rendered, diff > %.0f%% (wrong) | %d |\n",
		diffGood*100, good, diffGood*100, diffBad*100, fair, diffBad*100, bad)
	fmt.Fprintf(&b, "\nDiff percentiles: p50 %.2f%%, p90 %.2f%%, p99 %.2f%%, max %.2f%%.\n", pct(.5), pct(.9), pct(.99), pct(1))

	errs := map[string]int{}
	for _, r := range rows {
		if r.StrictErr != "" {
			errs[r.StrictErr]++
		}
	}
	if len(errs) > 0 {
		type kv struct {
			k string
			v int
		}
		var kvs []kv
		for k, v := range errs {
			kvs = append(kvs, kv{k, v})
		}
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v || (kvs[i].v == kvs[j].v && kvs[i].k < kvs[j].k) })
		b.WriteString("\n## Strict-mode errors, by message\n\n| count | message |\n|---|---|\n")
		for _, e := range kvs {
			fmt.Fprintf(&b, "| %d | `%s` |\n", e.v, strings.ReplaceAll(e.k, "|", `\|`))
		}
	}

	sort.Slice(rows, func(i, j int) bool { return rows[i].Diff > rows[j].Diff })
	b.WriteString("\n## Worst 25 by diff\n\n| emoji | file | strict | diff |\n|---|---|---|---|\n")
	for i, r := range rows {
		if i == 25 {
			break
		}
		fmt.Fprintf(&b, "| %s | %s | %v | %.1f%% |\n", r.Emoji, r.File, r.Strict, r.Diff*100)
	}
	if failed > 0 {
		b.WriteString("\n## Failed to parse\n\n")
		for _, r := range rows {
			if r.Err != "" {
				fmt.Fprintf(&b, "- %s %s: `%s`\n", r.Emoji, r.File, r.Err)
			}
		}
	}
	return os.WriteFile(filepath.Join(outDir, "oksvg-survey.md"), []byte(b.String()), 0o644)
}

// compareSheet puts, for each emoji, the Noto PNG and the oksvg render side by
// side at 72 px and at the 24- and 8-px charge sizes, for the eye.
func compareSheet(n Noto, emojis []string, outDir string) error {
	const cell = 72
	cols := []string{"Noto PNG 72", "oksvg 72", "PNG→24", "SVG→24", "PNG→8 x3", "SVG→8 x3"}
	lw := 8
	w := lw + len(cols)*(cell+8)
	h := 20 + len(emojis)*(cell+8)
	sheet := newSheet(w, h, Day.Surface)
	for i, c := range cols {
		sheet.label(lw+i*(cell+8), 14, c, Day.Ink)
	}
	for row, e := range emojis {
		y := 20 + row*(cell+8)
		ref, err := n.PNG72(e)
		if err != nil {
			return err
		}
		svg, _, _, err := renderSVG72(n.SVGPath(e))
		if err != nil {
			return fmt.Errorf("%s: %w", e, err)
		}
		x := lw
		sheet.put(ref, x, y, 1)
		sheet.put(svg, x+(cell+8), y, 1)
		sheet.put(resize(ref, 24), x+2*(cell+8), y, 1)
		sheet.put(resize(svg, 24), x+3*(cell+8), y, 1)
		sheet.put(resize(ref, 8), x+4*(cell+8), y, 3)
		sheet.put(resize(svg, 8), x+5*(cell+8), y, 3)
	}
	return writePNG(filepath.Join(outDir, "oksvg-compare.png"), sheet.img)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
