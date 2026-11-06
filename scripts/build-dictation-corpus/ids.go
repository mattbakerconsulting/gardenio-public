package main

import (
	"regexp"
	"strings"
)

// Bead IDs are nanoid-style: prefix "gd-" followed by exactly 10 alphanumerics.
// We anchor both ends with \b so a prefix "gd-" embedded in a longer token
// (e.g. "ka-TOOLONGIDENT123") is not matched as a valid id.
var reBeadID = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]{10}\b`)

// digitWords expands "0".."9" to english words so dictation catches numeric chars
// inside a bead id without having to say "zero" vs "oh". Apple Speech generally
// tokenizes letter-by-letter dictation better when given written-out words.
var digitWords = map[byte]string{
	'0': "zero",
	'1': "one",
	'2': "two",
	'3': "three",
	'4': "four",
	'5': "five",
	'6': "six",
	'7': "seven",
	'8': "eight",
	'9': "nine",
}

// findBeadIDs returns every unique bead ID referenced in text, in order of
// first appearance. Empty input returns an empty slice (never nil).
func findBeadIDs(text string) []string {
	matches := reBeadID.FindAllString(text, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if _, ok := seen[m]; ok {
			continue
		}
		seen[m] = struct{}{}
		out = append(out, m)
	}
	return out
}

// spellID returns the spelling template for a single bead id. Example:
//
//	spellID("ka-0000000316") -> "k d dash U v W U b O k C x P"
//	spellID("ka-0000005040") -> "k d dash Q y six Z one z t W two S"
//
// Case is preserved on letters (so Apple Speech picks up the capitalization
// hint for contextual biasing). Digits are expanded to english words.
// Dash becomes the spoken word "dash".
func spellID(id string) string {
	if id == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(id) * 3)
	for i := 0; i < len(id); i++ {
		c := id[i]
		if i > 0 {
			b.WriteByte(' ')
		}
		switch {
		case c == '-':
			b.WriteString("dash")
		case c >= '0' && c <= '9':
			b.WriteString(digitWords[c])
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// spellingTemplates returns phrase variants for a bead id that we want the
// speech recognizer to prefer over homophones. We emit three variants:
//
//	"bead <spell>"   — explicit lead-in (most common in meeting speech)
//	"<spell>"        — bare form (people often just read the id)
//	"see <spell>"    — common mid-sentence form ("see kd dash ...")
//
// Keeping a small fixed set of variants helps the custom LM learn the
// n-gram pattern without bloating phrases.yaml.
func spellingTemplates(id string) []string {
	sp := spellID(id)
	if sp == "" {
		return nil
	}
	return []string{
		"bead " + sp,
		sp,
		"see " + sp,
	}
}
