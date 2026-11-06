package bridge

import (
	"testing"

	"gardenio/controller/internal/heraldry"
)

// TestBeadTypeFallbackIsNotACharge extends R4 (spec 2026-10-25-heraldry-design)
// to the glyph a bead type outside Theme.BeadType draws. That glyph shows in
// unfurls, prose and the References footer beside emoji runs, so it may not
// be a charge either; it used to be a hard-coded emoji in link_unfurl.go
// (📦 📄 💬 💡 📊 📍) that the library held (gd-vQvHXp4Rl3).
func TestBeadTypeFallbackIsNotACharge(t *testing.T) {
	types := []string{"", "rating", "human_survey", "investigation-request", "no-such-type"}
	for k := range Garden.BeadType {
		types = append(types, k)
	}
	for _, th := range []Theme{Garden, Business} {
		if c, ok := heraldry.Lookup(th.BeadTypeFallback); ok {
			t.Errorf("%s BeadTypeFallback = %s is the library charge %q (spec R4)", th.Name, th.BeadTypeFallback, c.Name)
		}
		for _, bt := range types {
			g := th.BeadTypeGlyph(bt)
			if g == "" {
				t.Errorf("%s BeadTypeGlyph(%q) is empty", th.Name, bt)
			}
			if c, ok := heraldry.Lookup(g); ok {
				t.Errorf("%s BeadTypeGlyph(%q) = %s is the library charge %q (spec R4)", th.Name, bt, g, c.Name)
			}
		}
	}
}

// TestBeadTypeGlyphFallbacks pins the fallback for an unthemed type: Garden's
// BeadTypeFallback, and Business's bracket tag of the type word.
func TestBeadTypeGlyphFallbacks(t *testing.T) {
	cases := []struct {
		th       Theme
		beadType string
		glyph    string
		badge    string
	}{
		{Garden, "bundle", "🪺", "🪺 `bundle`"},
		{Garden, "rating", "🪹", "🪹 `rating`"},
		{Garden, "", "🪹", "🪹 `bead`"},
		{Business, "bundle", "[BUNDLE]", "[BUNDLE]"},
		{Business, "rating", "[RATING]", "[RATING]"},
		{Business, "", "[BEAD]", "[BEAD]"},
	}
	for _, c := range cases {
		if g := c.th.BeadTypeGlyph(c.beadType); g != c.glyph {
			t.Errorf("%s BeadTypeGlyph(%q) = %q, want %q", c.th.Name, c.beadType, g, c.glyph)
		}
		if b := c.th.BeadTypeBadge(c.beadType); b != c.badge {
			t.Errorf("%s BeadTypeBadge(%q) = %q, want %q", c.th.Name, c.beadType, b, c.badge)
		}
	}
}
