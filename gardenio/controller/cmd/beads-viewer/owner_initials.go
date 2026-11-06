package main

import (
	"hash/fnv"
	"html/template"
	"strings"
)

// ownerInitialsPalette is a small set of accessible background hues for the
// owner-initials avatar badge. The hue is picked deterministically from the
// owner display string (see ownerInitialsHue) so the same owner always lands
// on the same color across rows and reloads — scannable at a glance without a
// legend. All hues pair with white text at the chosen lightness.
var ownerInitialsPalette = []string{
	"#7851A9", // royal purple (also the viewer-self accent)
	"#1f7a8c", // teal
	"#b5651d", // ochre
	"#2e7d32", // green
	"#c2185b", // magenta
	"#3949ab", // indigo
	"#00838f", // cyan
	"#6d4c41", // brown
	"#ad1457", // deep pink
	"#00695c", // dark teal
	"#5d4037", // dark brown
	"#827717", // olive
}

// ownerInitialsHue returns a deterministic palette color for an owner display
// string. Empty input maps to a neutral gray so unresolved owners render a
// visible-but-muted badge rather than picking up an arbitrary hue.
func ownerInitialsHue(display string) string {
	display = strings.TrimSpace(display)
	if display == "" {
		return "#8a8a8a"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(strings.ToLower(display)))
	return ownerInitialsPalette[int(h.Sum32())%len(ownerInitialsPalette)]
}

// renderOwnerInitialsBadge renders a compact circular avatar carrying the
// owner's initials for an agent row. It reuses actorInitials (the same
// densification helper used by /inbox) so the initials rule is shared, and
// preserves the full owner name via title= for hover and screen readers
// (ka-0000001514 / bug ka-0000007078).
//
// When slug is non-empty the badge links to the owner's /u/{slug} homepage,
// matching the userLink behavior of the adjacent "op …" text; otherwise it
// renders as a non-interactive <span>. Returns the empty HTML for an empty
// display so callers can invoke it unconditionally (the surrounding
// fragment collapses via `{{if .OperatedByDisplay}}`).
func renderOwnerInitialsBadge(display, slug, basePath string) template.HTML {
	display = strings.TrimSpace(display)
	if display == "" {
		return ""
	}
	initials := actorInitials(display)
	if initials == "" {
		return ""
	}
	safeName := template.HTMLEscapeString(display)
	safeInitials := template.HTMLEscapeString(initials)
	style := "background:" + ownerInitialsHue(display)

	var b strings.Builder
	slug = strings.TrimSpace(slug)
	if slug != "" {
		href := template.HTMLEscapeString(basePath + "/u/" + slug)
		b.WriteString(`<a class="owner-initials" href="`)
		b.WriteString(href)
		b.WriteString(`" style="`)
		b.WriteString(style)
		b.WriteString(`" title="owner: `)
		b.WriteString(safeName)
		b.WriteString(`" aria-label="owner `)
		b.WriteString(safeName)
		b.WriteString(`">`)
		b.WriteString(safeInitials)
		b.WriteString(`</a>`)
		return template.HTML(b.String())
	}
	b.WriteString(`<span class="owner-initials" style="`)
	b.WriteString(style)
	b.WriteString(`" title="owner: `)
	b.WriteString(safeName)
	b.WriteString(`" aria-label="owner `)
	b.WriteString(safeName)
	b.WriteString(`">`)
	b.WriteString(safeInitials)
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}
