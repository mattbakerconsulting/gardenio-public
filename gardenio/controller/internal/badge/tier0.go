// Package badge turns a plan's (bead id, title, prose) into a small SVG
// badge. There are two tiers, and the whole point of the package is that they
// compose into a single always-succeeds call:
//
//   - Tier-0 (this file) is a pure function of the bead id + title: no network,
//     no model, no cost, stable forever. hash(id) picks a hue, a background
//     shape, and a ring; a 2-letter monogram is derived from the title. It is
//     the guaranteed floor — every plan always has SOMETHING, so /plans has no
//     empty state and no generation-latency hole (plan ka-0000005980, the
//     "there is no empty state" bet).
//
//   - Tier-2 (model.go) asks a model to AUTHOR an SVG glyph for the plan's
//     subject. It is more recognizable but can fail (no key, API error, model
//     returns junk); on ANY failure it falls back to Tier-0. See Generate.
//
// A badge is ~1KB of text: diffable, reviewable in an MR, hand-editable, and
// infinitely scalable (the same asset must read at 128px on a detail header and
// 24px in a cmdk row). Tier-0 is deliberately geometric so it stays legible at
// 24px, which is exactly where the monogram-only prototype and the "metric
// triad" semantic glyph both failed (plan narrative ka-0000005497).
package badge

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode"
)

// Badge is the result of a generation: the SVG bytes plus which tier produced
// them, so callers (and the content-address cache) can tell a model-authored
// badge from the deterministic fallback.
type Badge struct {
	// SVG is a complete, standalone <svg>…</svg> document.
	SVG string
	// Tier is "tier0" (deterministic fallback) or "tier2" (model-authored).
	Tier string
}

// Tier names, exported so callers can compare without magic strings.
const (
	TierDeterministic = "tier0"
	TierModel         = "tier2"
)

// canvas is the intrinsic coordinate system every badge is authored in. The SVG
// has no width/height attributes — only viewBox — so the consumer scales it to
// 128 / 40 / 24px purely with CSS. 100x100 keeps the numbers round and the
// model prompt (model.go) authors into the SAME box.
const canvas = 100

// tier0Palette is a fixed set of hue/shape/ring triples. hash(id) indexes into
// it, so the choice is deterministic per plan yet visually varied across a
// board of ~20 plans. Colors are mid-saturation so a wall of them reads as a
// system rather than noise (the "wall of stickers" taste risk). Each entry is a
// (background, foreground) pair; the monogram is drawn in the foreground.
var tier0Palette = []struct {
	bg string // background fill
	fg string // monogram + ring stroke
}{
	{"#2563eb", "#eff6ff"}, // blue
	{"#059669", "#ecfdf5"}, // emerald
	{"#d97706", "#fffbeb"}, // amber
	{"#dc2626", "#fef2f2"}, // red
	{"#7c3aed", "#f5f3ff"}, // violet
	{"#0891b2", "#ecfeff"}, // cyan
	{"#db2777", "#fdf2f8"}, // pink
	{"#4b5563", "#f9fafb"}, // slate
	{"#65a30d", "#f7fee7"}, // lime
	{"#ea580c", "#fff7ed"}, // orange
	{"#0d9488", "#f0fdfa"}, // teal
	{"#9333ea", "#faf5ff"}, // purple
}

// tier0Shape is the background silhouette. hash(id) picks one; the variety
// helps two plans with the same monogram (the "[E2E fixture]" -> "EF" collision
// that broke the monogram-only prototype) still look distinct.
type tier0Shape int

const (
	shapeCircle tier0Shape = iota
	shapeRoundedSquare
	shapeHexagon
	shapeShield
	numShapes
)

// Tier0 renders the deterministic fallback badge for a plan. It never fails: id
// alone is sufficient (title only feeds the monogram, which degrades to "?"
// when the title has no letters). Given the same id+title it returns byte-
// identical SVG forever, which is what lets the cache be content-addressed on
// (id, title) and a retitle invalidate naturally.
func Tier0(id, title string) Badge {
	// A single hash drives every deterministic choice. Different byte
	// positions select palette / shape / ring so the dimensions vary
	// independently rather than moving in lockstep.
	sum := sha256.Sum256([]byte(id))
	pal := tier0Palette[int(sum[0])%len(tier0Palette)]
	shape := tier0Shape(int(sum[1]) % int(numShapes))
	ring := sum[2]%2 == 0
	monogram := monogramFor(title)

	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" role="img" aria-label="%s">`,
		canvas, canvas, escapeXML(title))
	b.WriteString(backgroundShape(shape, pal.bg))
	if ring {
		// A concentric ring inset from the edge — extra deterministic
		// distinctiveness at 128px, invisible-but-harmless at 24px.
		fmt.Fprintf(&b, `<circle cx="50" cy="50" r="40" fill="none" stroke="%s" stroke-opacity="0.35" stroke-width="3"/>`, pal.fg)
	}
	// The monogram, centered. font-size scales with letter count so two
	// letters do not overflow a single-letter's box.
	fontSize := 52
	if len([]rune(monogram)) >= 2 {
		fontSize = 40
	}
	fmt.Fprintf(&b,
		`<text x="50" y="50" fill="%s" font-family="-apple-system,Segoe UI,Roboto,sans-serif" font-weight="700" font-size="%d" text-anchor="middle" dominant-baseline="central">%s</text>`,
		pal.fg, fontSize, escapeXML(monogram))
	b.WriteString(`</svg>`)

	return Badge{SVG: b.String(), Tier: TierDeterministic}
}

// backgroundShape emits the filled silhouette for the chosen shape, sized to
// the 100x100 canvas with a small inset so nothing clips at the viewBox edge.
func backgroundShape(shape tier0Shape, fill string) string {
	switch shape {
	case shapeRoundedSquare:
		return fmt.Sprintf(`<rect x="6" y="6" width="88" height="88" rx="20" fill="%s"/>`, fill)
	case shapeHexagon:
		// A flat-top hexagon inscribed in the canvas.
		return fmt.Sprintf(`<polygon points="50,6 90,28 90,72 50,94 10,72 10,28" fill="%s"/>`, fill)
	case shapeShield:
		// A rounded shield: flat top, tapering to a point at the bottom.
		return fmt.Sprintf(`<path d="M50 6 L90 20 L90 52 Q90 82 50 94 Q10 82 10 52 L10 20 Z" fill="%s"/>`, fill)
	default: // shapeCircle
		return fmt.Sprintf(`<circle cx="50" cy="50" r="46" fill="%s"/>`, fill)
	}
}

// monogramFor derives a 1-2 letter uppercase monogram from a plan title. It
// prefers the initials of the first two significant words; if the title is a
// single word it takes its first two letters; if there are no letters at all it
// returns "?" so the badge never renders blank.
func monogramFor(title string) string {
	words := significantWords(title)
	switch {
	case len(words) >= 2:
		return firstLetter(words[0]) + firstLetter(words[1])
	case len(words) == 1:
		return firstTwoLetters(words[0])
	default:
		return "?"
	}
}

// stopWords are skipped when picking the two words that seed the monogram, so
// "Every gardenio plan gets its own icon" yields "GP" (gardenio/plan), not "EG".
var stopWords = map[string]bool{
	"a": true, "an": true, "the": true, "of": true, "for": true, "to": true,
	"and": true, "or": true, "in": true, "on": true, "at": true, "by": true,
	"with": true, "its": true, "our": true, "we": true, "is": true, "are": true,
}

// significantWords splits a title into words that carry meaning: it drops
// stop-words and any token with no letters/digits, but never returns empty when
// the title had SOME word — if every word was a stop-word it keeps them, so a
// title like "The One" still yields a monogram.
func significantWords(title string) []string {
	raw := strings.FieldsFunc(title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	var kept []string
	for _, w := range raw {
		if !stopWords[strings.ToLower(w)] {
			kept = append(kept, w)
		}
	}
	if len(kept) == 0 {
		return raw // title was all stop-words; better than nothing
	}
	return kept
}

// firstLetter returns the first letter/digit of w, uppercased, or "" if none.
func firstLetter(w string) string {
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return strings.ToUpper(string(r))
		}
	}
	return ""
}

// firstTwoLetters returns up to the first two letters/digits of w, uppercased.
func firstTwoLetters(w string) string {
	var out []rune
	for _, r := range w {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToUpper(r))
			if len(out) == 2 {
				break
			}
		}
	}
	if len(out) == 0 {
		return "?"
	}
	return string(out)
}

// escapeXML escapes the five XML metacharacters so a title with & or < cannot
// break the SVG (both in the aria-label and in the rendered monogram text).
func escapeXML(s string) string {
	return strings.NewReplacer(
		"&", "&amp;",
		"<", "&lt;",
		">", "&gt;",
		`"`, "&quot;",
		"'", "&apos;",
	).Replace(s)
}
