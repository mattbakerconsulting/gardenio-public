package heraldry

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"strings"
)

// SVGOptions shape the SVG form.
type SVGOptions struct {
	// ChargeHref, when set, is the URL a charge's artwork is linked from
	// (a surface that serves ChargePNG once per charge and lets the browser
	// cache it). When nil the artwork is embedded as data: URIs, so the SVG
	// stands alone: about 6 KB per charge.
	ChargeHref func(Charge) string
	// Small embeds each charge's 72 px Noto PNG instead of its 128 px one,
	// at a bit over half the bytes: enough for a slot up to 24 CSS px high
	// (72 device px at 3×), too soft above that. ChargeHref, when set, wins.
	Small bool
}

// SVG is the same drawing as Image, resolution-free (spec §6.4): the
// charges as <image>s in a row, each in its 24-unit square inset by the
// keyline, an eighth of a square between squares, and the keyline as a
// dilate filter under them, on a transparent ground. Its width and height
// attributes are its design units (24 high, as wide as the run); CSS sizes it
// to any slot by its height. The keyline is one unit, so at a 24 px slot it is
// one CSS pixel. It carries role="img", an aria-label and a <title> in plain
// words (spec §8), and its ids are prefixed by the arms' hash so several
// pictures can sit inline on one page.
func (a Arms) SVG(th Theme, o SVGOptions) []byte {
	name := html.EscapeString(a.PlainName())
	sum := sha256.Sum256([]byte(a.Canonical()))
	id := "arms-" + hex.EncodeToString(sum[:5])
	n := float64(len(a.Charges))
	w := n*unit + max(n-1, 0)*unit/8

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %g %g" width="%g" height="%g" role="img" aria-label="%s">`, w, unit, w, unit, name)
	fmt.Fprintf(&b, `<title>%s</title>`, name)
	// The keyline: the charges' alpha dilated by one unit, flooded with the
	// theme's keyline colour, under the charges.
	fmt.Fprintf(&b, `<defs><filter id="%s-key" x="-10%%" y="-10%%" width="120%%" height="120%%" color-interpolation-filters="sRGB">`+
		`<feMorphology in="SourceAlpha" operator="dilate" radius="%g" result="grown"/>`+
		`<feFlood flood-color="%s"/><feComposite in2="grown" operator="in"/>`+
		`<feMerge><feMergeNode/><feMergeNode in="SourceGraphic"/></feMerge></filter></defs>`, id, keyline, hexOf(keylineFor(th)))
	fmt.Fprintf(&b, `<g filter="url(#%s-key)">`, id)
	for i, c := range a.Charges {
		var href string
		if o.ChargeHref != nil {
			href = html.EscapeString(o.ChargeHref(c))
		} else {
			art, _ := ChargePNG(c) // every library charge has artwork (TestEveryChargeHasArtwork)
			if o.Small {
				art, _ = smallChargePNG(c)
			}
			href = "data:image/png;base64," + base64.StdEncoding.EncodeToString(art)
		}
		x := float64(i)*(unit+unit/8) + keyline
		fmt.Fprintf(&b, `<image href="%s" x="%g" y="%g" width="%g" height="%g"/>`, href, x, keyline, unit-2*keyline, unit-2*keyline)
	}
	b.WriteString(`</g></svg>`)
	return []byte(b.String())
}
