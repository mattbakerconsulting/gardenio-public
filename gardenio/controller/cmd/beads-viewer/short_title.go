package main

import (
	"html/template"
	"strings"
)

// displayTitle renders a bead's name for a list row, card or plan step: the
// short title when the bead carries one, in a span whose title= keeps the full
// title on hover, otherwise the full title as before (gd-V5nGseRS5f). The bead
// page header still shows the full title, with the short title under it
// (partials/title_block.html).
func displayTitle(short, full string) template.HTML {
	short = strings.TrimSpace(short)
	if short == "" {
		return template.HTML(template.HTMLEscapeString(full))
	}
	return template.HTML(`<span class="short-title" title="` + template.HTMLEscapeString(full) + `">` +
		template.HTMLEscapeString(short) + `</span>`)
}
