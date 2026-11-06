package commons

import (
	"html/template"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/heraldry"
)

// Arms on a card (heraldry spec §5, steps 3.3 and 5.5b): when a card's subject
// bears arms, its header shows the row picture as inline SVG 24 px high and as
// wide as its emoji (garden.css .cm-arms), never the emoji run. The picture is
// pixels, not text, so the font tests (TestRenderedTextInGoldenFonts,
// gardenhub's TestPageTextInGoldenFonts) and C7's glyph budget are untouched;
// its accessible name is the arms' plain words (spec §8).

// armsSVG is the card picture for a subject's arms (the canonical form on
// BeadDetail.Heraldry); "" when it bears none or the value does not parse,
// so a bad value costs the card its picture and nothing else.
//
// It is always drawn by night: the Commons pane sits on --box, which is a
// dark slate under either sky (garden.css), so the night outline and field
// values are the ones that clear it. One drawing per arms also keeps the
// SVG's ids (prefixed by the arms' hash) meaning the same thing everywhere
// on a page that shows the same arms twice. The charges' artwork is embedded,
// so the picture stands alone in a fragment; only epics bear arms in v1
// (spec R6), so a timeline carries few. It is the 72 px artwork, which the
// 24 px slot needs at 3× and which weighs about half the 128 px
// (gd-5ZVgasQaHh).
func armsSVG(canonical string) template.HTML {
	a, err := heraldry.Parse(canonical)
	if err != nil {
		return ""
	}
	return template.HTML(a.SVG(heraldry.Night, heraldry.SVGOptions{Small: true}))
}

// faceless is the picture for a card with no face (an epic's card, say),
// when its subject is in the card's land, so another land's arms never show.
// A face reader has already checked its subject's land.
func faceless(b *beadsapi.BeadDetail, land string) template.HTML {
	if b == nil || b.Heraldry == "" || landOf(b.Labels) != land {
		return ""
	}
	return armsSVG(b.Heraldry)
}
