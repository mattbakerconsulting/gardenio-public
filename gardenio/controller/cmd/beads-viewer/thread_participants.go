package main

import (
	"html/template"
	"strconv"
	"strings"
)

// thread_participants.go — the render half of the /threads index-card enrichment
// (ka-0000000064): the participant-avatar row, the "+N" overflow avatar, and the
// last-message preview line. The DATA half (threadParticipant, threadLastMessage,
// threadParticipantsOf, threadLastMessageOf) lives in threads.go; these are the
// html/template helpers the index card invokes so threads.html stays declarative.
//
// Avatars reuse the shared owner-initials chrome (colored circular monogram,
// deterministic hue per person via ownerInitialsHue, /u/{slug} link when a slug
// resolves) so a participant reads identically to every other person avatar in
// the viewer. The initials are already resolved on the threadParticipant (via
// operatorInitials over the shared chip), so this layer only paints them.

// renderThreadParticipantAvatar renders one participant as a colored circular
// initials avatar, linking to /u/{slug} when a slug is present. The full display
// name rides on title= for hover / screen readers. Returns "" for a participant
// with no initials (an unresolved identity) so the row can invoke it
// unconditionally and simply skip empties.
func renderThreadParticipantAvatar(p threadParticipant, basePath string) template.HTML {
	initials := strings.TrimSpace(p.Initials)
	if initials == "" {
		return ""
	}
	display := strings.TrimSpace(p.Display)
	if display == "" {
		display = strings.TrimSpace(p.RawID)
	}
	safeInitials := template.HTMLEscapeString(initials)
	safeName := template.HTMLEscapeString(display)
	// Hue keys on the resolved display so the same person lands on the same color
	// across every card (matches the owner-initials badge).
	style := "background:" + ownerInitialsHue(display)

	var b strings.Builder
	slug := strings.TrimSpace(p.Slug)
	if slug != "" {
		href := template.HTMLEscapeString(basePath + "/u/" + slug)
		b.WriteString(`<a class="thread-avatar" href="`)
		b.WriteString(href)
		b.WriteString(`" style="`)
		b.WriteString(style)
		b.WriteString(`" title="`)
		b.WriteString(safeName)
		b.WriteString(`" aria-label="participant `)
		b.WriteString(safeName)
		b.WriteString(`">`)
		b.WriteString(safeInitials)
		b.WriteString(`</a>`)
		return template.HTML(b.String())
	}
	b.WriteString(`<span class="thread-avatar" style="`)
	b.WriteString(style)
	b.WriteString(`" title="`)
	b.WriteString(safeName)
	b.WriteString(`" aria-label="participant `)
	b.WriteString(safeName)
	b.WriteString(`">`)
	b.WriteString(safeInitials)
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}

// renderThreadParticipantOverflow renders the "+N" avatar that stands in for the
// participants beyond the avatar-row cap (threadParticipantCap). Returns "" when
// n<=0 so the template can invoke it unconditionally.
func renderThreadParticipantOverflow(n int) template.HTML {
	if n <= 0 {
		return ""
	}
	label := "+" + strconv.Itoa(n)
	var b strings.Builder
	b.WriteString(`<span class="thread-avatar thread-avatar-overflow" title="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(` more participant`)
	if n != 1 {
		b.WriteString("s")
	}
	b.WriteString(`" aria-label="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(` more participants">`)
	b.WriteString(template.HTMLEscapeString(label))
	b.WriteString(`</span>`)
	return template.HTML(b.String())
}
