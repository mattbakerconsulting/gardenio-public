package formula

import (
	"strings"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
)

// ShortTitle returns the short_title a poured bead is created with (plan
// gd-YZATAIOhfW, gd-4Nrk1IDadk): the declared one when the formula author set
// one, else one cut from title. Both are clipped to model.ShortTitleMax, so a
// {{var}} that expands a declared short_title past the limit cannot fail the
// pour at the daemon's validator. Every caller that creates a step or molecule
// bead from a formula (the daemon's HTTP pour, kd formula apply/expand) goes
// through here, so a pour under BEADS_SHORT_TITLE_GATE=enforce never strands a
// step without one.
//
// Formula step titles are authored ("Review the {{component}} PR"), not the
// machine-stamped titles the controller's shorttitle.Derive de-noises, so a
// word-boundary clip is enough here.
func ShortTitle(declared, title string) string {
	if s := clipShortTitle(declared); s != "" {
		return s
	}
	return clipShortTitle(title)
}

// clipShortTitle collapses whitespace runs (including line breaks) to one
// space and cuts the result to at most model.ShortTitleMax runes at a word
// boundary, appending a single ellipsis rune only when it cut anything. A
// string with no usable boundary in range is cut hard. It mirrors the
// controller's shorttitle.Clip.
func clipShortTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= model.ShortTitleMax {
		return s
	}
	runes := []rune(s)
	limit := model.ShortTitleMax - 1 // leave room for the ellipsis
	cut := limit
	for i := limit; i > limit/2; i-- {
		if runes[i] == ' ' {
			cut = i
			break
		}
	}
	return strings.TrimRight(string(runes[:cut]), " ,;:-–—(+&/") + "…"
}
