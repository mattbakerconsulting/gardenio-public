package beadsapi

import "strings"

// HeraldryRun is the emoji run of arms given in the canonical stored form
// (BeadDetail.Heraldry): the emoji in place order with nothing between them,
// "blue 🌱 . 🔥 . 📦" → "🌱🔥📦". It is the form a terminal or a line of prose
// shows, always followed by a space and the title (heraldry spec §4.2).
// "" gives "".
//
// It mirrors kbeads/internal/heraldry.Run, which the controller cannot import:
// the canonical form is the colour word (lower-case ASCII, absent in the
// five-token form) and then the five places, "." for an empty one. No emoji
// is ASCII, so dropping the ASCII tokens drops the colour and the gaps.
func HeraldryRun(canonical string) string {
	var sb strings.Builder
	for _, tok := range strings.Fields(canonical) {
		if isASCII(tok) {
			continue
		}
		sb.WriteString(tok)
	}
	return sb.String()
}

// HeraldryRun is the emoji run of the bead's arms, "" when it bears none.
func (b *BeadDetail) HeraldryRun() string {
	if b == nil {
		return ""
	}
	return HeraldryRun(b.Heraldry)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
