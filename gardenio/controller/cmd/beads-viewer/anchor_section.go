package main

import (
	"regexp"
	"strings"
)

// Anchored preview section slicing (A5, epic ka-0000006913).
//
// When a hover-preview is requested for an anchored bead ref
// (`/b/{id}/preview?anchor=c1-1`), we show the anchored SECTION — the matching
// heading plus the body that follows it — instead of the whole-bead excerpt.
// This is the operator ask (ka-0000003033): "mouseover preview should display
// the anchored text + what follows."

// atxHeadingRE matches an ATX markdown heading line, capturing the `#` run
// (for level) and the title text. A trailing run of `#` (closed ATX form,
// `## Title ##`) is trimmed from the title separately.
var atxHeadingRE = regexp.MustCompile(`^(#{1,6})[ \t]+(.*?)[ \t]*$`)

// codeFenceRE matches a fenced-code-block delimiter, capturing the marker run
// so the slicer can apply CommonMark's close rule: a fence closes only on a
// later line whose marker is the SAME character and at least as long. Captures
// group 1 = the fence run (backticks or tildes), optionally indented up to 3
// spaces. Heading-looking lines INSIDE a fence (e.g. a `# comment` in a shell
// snippet) must not be treated as headings.
var codeFenceRE = regexp.MustCompile("^[ ]{0,3}(`{3,}|~{3,})")

// closingHashRE matches a CommonMark closed-ATX trailing `#` run: a run of `#`
// at end-of-string preceded by whitespace. Only this form is a closer; a
// content-internal or content-terminal `#` (e.g. "Notes on C#") is kept.
var closingHashRE = regexp.MustCompile(`\s+#+\s*$`)

// anchorSection scans markdown source for the heading whose label slug equals
// the requested anchor and returns that heading's title plus the body that
// follows it, up to (but excluding) the next heading at the same or shallower
// level. ok is false when no heading matches — callers fall back to the
// whole-bead preview.
//
// Matching is done via slugifyAnchor on both sides, so a heading `### C1.1 —
// TTL field` (slug c1-1) is found by anchor `c1-1`, `C1.1`, or `c1.1`. ATX
// headings only (the universal form in the plan/ADR corpus); setext headings
// are not sliced and fall through to whole-bead.
func anchorSection(src, anchor string) (heading, body string, ok bool) {
	want := slugifyAnchor(anchor)
	if want == "" {
		return "", "", false
	}
	lines := strings.Split(src, "\n")
	var fence string // the open fence's marker run ("" when not in a fence)
	matchLevel := 0
	matchStart := -1 // index of first BODY line (line after the matched heading)
	for i, line := range lines {
		if fm := codeFenceRE.FindStringSubmatch(line); fm != nil {
			marker := fm[1]
			switch {
			case fence == "":
				fence = marker // open
			case marker[0] == fence[0] && len(marker) >= len(fence):
				fence = "" // close: same char, run >= opener
			}
			continue
		}
		if fence != "" {
			continue
		}
		m := atxHeadingRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		level := len(m[1])
		// Strip a CommonMark closed-ATX trailing `#` run (must be preceded by
		// whitespace) without clipping a content-terminal `#` like "C#".
		title := closingHashRE.ReplaceAllString(m[2], "")
		title = strings.TrimSpace(title)
		if matchStart < 0 {
			if slugifyAnchor(title) == want {
				heading = title
				matchLevel = level
				matchStart = i + 1
			}
			continue
		}
		// Already inside the matched section: a heading at the same or a
		// shallower level ends it.
		if level <= matchLevel {
			return heading, strings.TrimSpace(strings.Join(lines[matchStart:i], "\n")), true
		}
	}
	if matchStart < 0 {
		return "", "", false
	}
	return heading, strings.TrimSpace(strings.Join(lines[matchStart:], "\n")), true
}
