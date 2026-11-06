package main

import (
	"io/fs"
	"regexp"
	"strconv"
	"testing"
)

// Type chips (gd-kgfqnbAjo3): every bead type layout.html tints keeps that
// tint on the Homestead and takes a Homestead ink, and the ink clears 5.2:1
// on the tint laid over each skin's paper, surface and cream, day and night.
// Baseline's fix was the Home panels' task and decision chips only; the
// viewer's own inks fall to 4.1:1 on the cream bands and to 1.4-2.1:1 at night.
func TestHomesteadCSS_TypeChipInks(t *testing.T) {
	layout, err := fs.ReadFile(templateFS, "templates/layout.html")
	if err != nil {
		t.Fatal(err)
	}
	type tint struct {
		rgb [3]float64
		a   float64
	}
	tints := map[string]tint{}
	decl := regexp.MustCompile(`(?m)^  \.chip-type-([a-z]+)\s*\{ background: rgba\((\d+),(\d+),(\d+),([0-9.]+)\);[^}]*color: ([^;]+);`)
	for _, m := range decl.FindAllStringSubmatch(string(layout), -1) {
		if m[6] == "inherit" {
			continue // chore: the text ink already
		}
		var tn tint
		for i := range tn.rgb {
			tn.rgb[i], _ = strconv.ParseFloat(m[2+i], 64)
		}
		tn.a, _ = strconv.ParseFloat(m[5], 64)
		tints[m[1]] = tn
	}
	if len(tints) < 10 {
		t.Fatalf("found %d tinted types in layout.html; the parser or the file changed shape", len(tints))
	}

	css := regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(readHomesteadCSS(t), "")
	inkOf := map[string]string{}
	rule := regexp.MustCompile(`\[data-skin="dark-homestead"\]\) (:is\([^)]*\)|\.chip-type-[a-z]+) \{ color: var\((--hs-ink-[a-z]+)\); \}`)
	for _, m := range rule.FindAllStringSubmatch(css, -1) {
		for _, c := range regexp.MustCompile(`\.chip-type-([a-z]+)`).FindAllStringSubmatch(m[1], -1) {
			inkOf[c[1]] = m[2]
		}
	}

	for _, skin := range []string{"homestead", "dark-homestead"} {
		head := `html[data-skin="` + skin + `"] {`
		grounds := homesteadTokens(t, css, head)
		inks := homesteadTokens(t, css, head+"\n  --hs-pill-tint")
		for typ, tn := range tints {
			tok, ok := inkOf[typ]
			if !ok {
				t.Errorf(".chip-type-%s has no Homestead ink", typ)
				continue
			}
			if inks[tok] == "" {
				t.Errorf("%s does not define %s (for .chip-type-%s)", skin, tok, typ)
				continue
			}
			for _, g := range []string{"--bg", "--gd-surface", "--gd-bg"} {
				px := blendOver(tn.rgb, tn.a, hexRGB(t, grounds[g]))
				if c := contrast(hexRGB(t, inks[tok]), px); c < 5.2 {
					t.Errorf("%s .chip-type-%s (%s %s) on %s is %.2f:1, under 5.2", skin, typ, tok, inks[tok], g, c)
				}
			}
		}
	}
}
