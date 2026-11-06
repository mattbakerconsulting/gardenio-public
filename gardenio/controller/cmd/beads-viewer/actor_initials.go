package main

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// actorInitials returns a short 1–4 character avatar/initial collapse of an
// actor display name for narrow-row rendering on /inbox.
//
// Rules:
//   - Multi-segment (hyphen/underscore/space/dot separators): first rune of
//     up to four segments, uppercased. e.g. "gardenio-inbox-pushdense-condense-71z"
//     → "GIPC".
//   - Single segment: first two runes uppercased. e.g. "alice" → "AL".
//   - Empty / whitespace-only input: empty string (caller decides what to do).
//
// The full actor string is preserved on the surrounding span via title=, so
// the abbreviated form is purely a visual densification — screen readers and
// hover still surface the full value. Used by templates/partials/inbox_row.html
// to render an .actor-initials span alongside .actor-full; CSS in inbox.html
// swaps which is visible at narrow viewport widths.
func actorInitials(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.' || r == '/'
	})
	if len(parts) <= 1 {
		// Single segment (or no separators): first two letters/digits. Skipping
		// leading punctuation keeps a name like "(scoped)" from collapsing to "((".
		var b strings.Builder
		count := 0
		for _, r := range name {
			if !isInitialRune(r) {
				continue
			}
			b.WriteRune(unicode.ToUpper(r))
			count++
			if count >= 2 {
				break
			}
		}
		return b.String()
	}
	var b strings.Builder
	for _, p := range parts {
		// Use each segment's first letter/digit, skipping leading punctuation so a
		// parenthesised segment like "(gardenio," contributes "G", not "(". Without
		// this, names like "Bug-Response Gardener (gardenio, aggressive)" produced a
		// trailing "(" in the avatar initials.
		r := firstInitialRune(p)
		if r == utf8.RuneError {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
		if utf8.RuneCountInString(b.String()) >= 4 {
			break
		}
	}
	return b.String()
}

// isInitialRune reports whether r is eligible to appear in an initials collapse:
// a letter or digit. Punctuation, brackets, and symbols are skipped so they never
// leak into an avatar/initials string.
func isInitialRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsDigit(r)
}

// firstInitialRune returns the first letter/digit in s, or utf8.RuneError when s
// has none (e.g. a pure-punctuation segment), so the caller can skip it.
func firstInitialRune(s string) rune {
	for _, r := range s {
		if isInitialRune(r) {
			return r
		}
	}
	return utf8.RuneError
}
