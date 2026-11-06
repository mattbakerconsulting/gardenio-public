package main

import (
	"html/template"
	"strings"
	"unicode"
	"unicode/utf8"
)

// operatorInitials returns a 1–3 char monogram for an operatorChip suitable
// for narrow-row densification (project list rows, search rows).
//
// The full chip's Display is the source of truth — we never emit raw
// `slack:U…` / `oidc:…` identifiers as initials. If Display is empty or is
// only an unresolved raw id (e.g. matches RawID and starts with a known
// scheme prefix), the fallback "??" is returned so callers can render a
// visible-but-clearly-unresolved monogram.
//
// Rules (mirror actor_initials.go):
//   - Multi-segment (separators: space, hyphen, underscore, dot, slash):
//     first rune of up to three segments, uppercased.
//     "Matt Baker"          → "MB"
//     "gardenio-gardener-foo" → "GCF"
//   - Single segment: first two runes uppercased. "alice" → "AL".
//   - Empty / unresolved Display: "??".
//
// The full operator display is kept on the surrounding span via title= so
// hover and screen readers still surface the un-abbreviated value.
func operatorInitials(chip operatorChip) string {
	display := strings.TrimSpace(chip.Display)
	if display == "" {
		return "??"
	}
	// For human chips, Display falls back to RawID when the daemon could
	// not resolve a human-readable name. We must not emit raw slack:U… /
	// oidc:… identifiers as a monogram — render the unresolved sentinel
	// instead. Agent chips always carry their agent name as Display and
	// RawID together by construction (see resolveOperatorChip), so the
	// equality is not a signal there.
	if chip.Kind == "human" {
		lower := strings.ToLower(display)
		if strings.HasPrefix(lower, "slack:") ||
			strings.HasPrefix(lower, "oidc:") ||
			strings.HasPrefix(lower, "email:") {
			return "??"
		}
	}
	parts := strings.FieldsFunc(display, func(r rune) bool {
		return r == '-' || r == '_' || r == ' ' || r == '.' || r == '/'
	})
	if len(parts) <= 1 {
		var b strings.Builder
		count := 0
		for _, r := range display {
			if unicode.IsSpace(r) {
				continue
			}
			b.WriteRune(unicode.ToUpper(r))
			count++
			if count >= 2 {
				break
			}
		}
		out := b.String()
		if out == "" {
			return "??"
		}
		return out
	}
	var b strings.Builder
	written := 0
	for _, p := range parts {
		if written >= 3 {
			break
		}
		r, _ := utf8.DecodeRuneInString(p)
		if r == utf8.RuneError {
			continue
		}
		b.WriteRune(unicode.ToUpper(r))
		written++
	}
	out := b.String()
	if out == "" {
		return "??"
	}
	return out
}

// renderOperatorChipCompact renders a narrow inline chip — initials only —
// suitable for list-row densification. Mirrors the marker classes from the
// full chip so the same skin rules (mine-vs-not-mine, human-vs-agent) apply.
//
// HTML shape:
//
//	<a class="operator-chip-compact is-viewer-self" data-operated-by="…" title="…" href="…">XX</a>
//
// or, when no FilterHref is set:
//
//	<span class="operator-chip-compact is-agent" data-operated-by="…" title="…">XX</span>
//
// Kinds "scheduler" / "empty" / "self" / "suppressed" return the empty string
// — the compact chip is only useful for human / machine / agent attribution.
// The scheduler sentinel and em-dash already render fine in their existing
// shapes; the comment-row "self" / "suppressed" cases drop their chip
// entirely.
//
// A "machine" chip (ka-0000003465: fleet:/system: sentinels) renders a muted,
// non-linked monogram with the is-machine skin so a narrow row shows an
// unmistakably-automated badge rather than a fake-human initials avatar.
func renderOperatorChipCompact(chip operatorChip) template.HTML {
	switch chip.Kind {
	case "human", "agent", "machine":
		// ok
	default:
		return ""
	}
	initials := operatorInitials(chip)
	var classes []string
	classes = append(classes, "operator-chip-compact")
	if chip.IsViewerSelf {
		classes = append(classes, "is-viewer-self")
	}
	if chip.Kind == "agent" {
		classes = append(classes, "is-agent")
	}
	if chip.Kind == "machine" {
		classes = append(classes, "is-machine")
	}
	className := strings.Join(classes, " ")

	title := strings.TrimSpace(chip.Display)
	if title == "" {
		title = strings.TrimSpace(chip.RawID)
	}

	// Per-user-homepage M2 §6 (ka-0000007787): when the resolver populated a
	// canonical slug, point the compact chip at /u/{slug} instead of the
	// legacy /search?operated_by=… filter href. The slug-bearing path is
	// preferred for human chips; agent chips fall through to FilterHref.
	href := ""
	isSlugLink := false
	switch {
	case chip.Slug != "":
		href = "/u/" + chip.Slug
		isSlugLink = true
	case chip.FilterHref != "":
		href = chip.FilterHref
	}
	var b strings.Builder
	if href != "" {
		if isSlugLink {
			classes = append(classes, "is-slug-link")
			className = strings.Join(classes, " ")
		}
		b.WriteString(`<a class="`)
		b.WriteString(template.HTMLEscapeString(className))
		b.WriteString(`" data-operated-by="`)
		b.WriteString(template.HTMLEscapeString(chip.RawID))
		if isSlugLink {
			b.WriteString(`" data-slug="`)
			b.WriteString(template.HTMLEscapeString(chip.Slug))
		}
		b.WriteString(`" title="`)
		b.WriteString(template.HTMLEscapeString(title))
		b.WriteString(`" href="`)
		b.WriteString(template.HTMLEscapeString(href))
		b.WriteString(`">`)
		b.WriteString(template.HTMLEscapeString(initials))
		b.WriteString(`</a>`)
	} else {
		b.WriteString(`<span class="`)
		b.WriteString(template.HTMLEscapeString(className))
		b.WriteString(`" data-operated-by="`)
		b.WriteString(template.HTMLEscapeString(chip.RawID))
		b.WriteString(`" title="`)
		b.WriteString(template.HTMLEscapeString(title))
		b.WriteString(`">`)
		b.WriteString(template.HTMLEscapeString(initials))
		b.WriteString(`</span>`)
	}
	return template.HTML(b.String())
}
