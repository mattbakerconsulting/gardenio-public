package embedding

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"gardenio/kbeads/internal/model"
)

// BeadText builds the text document to embed for a bead. It concatenates
// Title, Type, Description, Notes and Labels with field prefixes. Optional
// fields (Description, Notes, Labels) are omitted when empty so the hash is
// stable for beads that never had them set.
func BeadText(b *model.Bead) string {
	if b == nil {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("Title: ")
	sb.WriteString(b.Title)
	sb.WriteString("\nType: ")
	sb.WriteString(string(b.Type))
	if b.Description != "" {
		sb.WriteString("\nDescription: ")
		sb.WriteString(b.Description)
	}
	if b.Notes != "" {
		sb.WriteString("\nNotes: ")
		sb.WriteString(b.Notes)
	}
	if len(b.Labels) > 0 {
		sb.WriteString("\nLabels: ")
		sb.WriteString(strings.Join(b.Labels, ", "))
	}
	return sb.String()
}

// TextHash returns a hex-encoded SHA-256 of the text. Used to skip re-embedding
// when a bead's text content hasn't changed.
func TextHash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:])
}
