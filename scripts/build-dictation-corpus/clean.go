package main

import (
	"regexp"
	"strings"
)

// Regexes for stripping noise from the corpus. Compiled once at init.
var (
	// Fenced code blocks: ``` optionally with a language hint, then any body, then ``` on its own fence.
	// Non-greedy body match so multiple blocks don't collapse into one.
	reCodeFence = regexp.MustCompile("(?s)```[^\n]*\n.*?```")

	// Inline backtick code: `...`. Do not cross a newline.
	reInlineCode = regexp.MustCompile("`[^`\n]+`")

	// URLs: http(s)://..., up to the first whitespace or closing paren/angle bracket.
	reURL = regexp.MustCompile(`https?://[^\s)>\]]+`)

	// Markdown image/link references [text](...) — keep the visible text, drop the target.
	reMDLink = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)

	// HTML-ish tags like <foo>, <br />, <a href="...">.
	reHTMLTag = regexp.MustCompile(`<[^<>\n]{1,200}>`)

	// Collapse runs of whitespace.
	reWhitespace = regexp.MustCompile(`[ \t]+`)

	// Collapse 3+ blank lines to two.
	reBlankLines = regexp.MustCompile(`\n{3,}`)
)

// cleanText strips code fences, URLs, HTML tags, and collapses whitespace.
// It preserves bead IDs (ka-0000001713) so downstream stages can find them.
func cleanText(s string) string {
	if s == "" {
		return ""
	}
	s = reCodeFence.ReplaceAllString(s, " ")
	s = reInlineCode.ReplaceAllString(s, " ")
	s = reMDLink.ReplaceAllString(s, "$1 ")
	s = reURL.ReplaceAllString(s, " ")
	s = reHTMLTag.ReplaceAllString(s, " ")

	// Normalize line endings and whitespace.
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = reWhitespace.ReplaceAllString(s, " ")
	s = reBlankLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}
