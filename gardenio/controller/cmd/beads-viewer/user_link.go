package main

import (
	"html/template"
	"strings"
)

// renderUserLink wraps a person's display name in an `<a href="/u/{slug}">`
// link to their per-user homepage when slug is non-empty. Falls back to
// HTML-escaped plain text when slug is empty so templates can call it
// unconditionally.
//
// ka-0000007502: meta strips like "op {{.OperatedByDisplay}}" on /agents,
// "requested by {{.RequestedBy}}" on /decisions, and the bold author header
// on comment rows all rendered user names as plain text. With slug now
// plumbed end-to-end (see operator_slug_cache.go), those call sites switch
// to `{{userLink .Display .Slug}}` so user names become the entry-point to
// the operator's homepage.
//
// Returns the empty HTML when display is empty so the surrounding template
// fragment can be omitted via `{{if userLink …}}` without a separate
// emptiness check on the display.
func renderUserLink(display, slug, basePath string) template.HTML {
	display = strings.TrimSpace(display)
	if display == "" {
		return ""
	}
	safe := template.HTMLEscapeString(display)
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return template.HTML(safe)
	}
	href := template.HTMLEscapeString(basePath + "/u/" + slug)
	return template.HTML(`<a class="user-link" href="` + href + `" title="Open ` + safe + `&#39;s homepage">` + safe + `</a>`)
}
