package main

import (
	"regexp"
	"strings"
)

// Same-document auto-linking (bead anchors L2 — epic ka-0000006913 A4,
// design ka-0000007428, operator OK ka-0000006898 Q3).
//
// A1 stamps a stable, label-derived id onto every rendered heading
// (`### C1.1 — TTL field` -> `<h3 id="c1-1">`); A2 lets an explicit
// `ka-X#c1-1` ref deep-link to it. L2 closes the last gap: within a SINGLE
// rendered fragment, a BARE inline label token — the way agents already write
// `as described in R3 above` — that exactly matches a heading id present in
// the SAME fragment silently becomes a link to `#<slug>`. No new syntax, no
// author effort.
//
// The whole point is that it CANNOT misfire across documents (the `§6.3`
// ambiguity that kept L3 out of the MVP): the target set is collected from the
// fragment being rendered and nothing else, so a bare `C1` only links when
// THIS fragment actually contains a `#c1` heading.
//
// Conservatism (three independent gates):
//  1. Matcher is `^[A-Z][0-9]` — an uppercase letter immediately followed by a
//     digit. This is strictly narrower than anchorLabelRE (A1): prose words,
//     lowercase tokens, and pure-number ADR sections never match, so ordinary
//     text is untouched unless it looks like a label AND names a real heading.
//  2. The token's slug (via the shared slugifyAnchor) must be an id actually
//     present in the fragment. No heading, no link.
//  3. It runs only over body text — heading blocks, existing <a> spans, raw
//     tag markup, and mermaid diagram source are all skipped (sameDocSkipRe),
//     so a label inside its own heading is never self-linked and existing
//     links are never double-wrapped.

// headingIDRe extracts the id attribute of a rendered heading tag (<h1>..<h6>).
// Only headings carry the anchors L2 targets, so scanning them (rather than any
// id="…") keeps the target set to genuine section anchors.
var headingIDRe = regexp.MustCompile(`(?is)<h[1-6]\b[^>]*\bid="([^"]+)"`)

// sameDocLabelRe matches a conservative bare inline label token: an uppercase
// letter immediately followed by a digit, then optional trailing alphanumerics
// and dotted/dashed segments — `C1`, `R3`, `C1.1`, `P2`, `D10`, `T2.3`. The
// leading `[A-Z][0-9]` shape is what makes this safe: it admits the numbered
// label families in the corpus while excluding prose ("Overview"), lowercase
// slugs ("c1-1"), and bare ADR section numbers ("6.3") — none of which start
// uppercase-then-digit. Word-bounded so it never fires on a substring.
var sameDocLabelRe = regexp.MustCompile(`\b[A-Z][0-9][A-Za-z0-9]*(?:[.-][A-Za-z0-9]+)*\b`)

// sameDocSkipRe marks the HTML regions the L2 pass must NOT rewrite. Beyond
// the regions skipRe already protects (mermaid source, existing <a> spans, raw
// tag markup) it additionally skips whole heading blocks: a label token inside
// its own heading is the link TARGET, not a reference to it, so self-links are
// suppressed. Alternation is leftmost-first — the mermaid and heading block
// variants are tried before the broad `<[^>]+>` branch so each is skipped as a
// single unit.
var sameDocSkipRe = regexp.MustCompile(`(?is)` + mermaidBlockPattern +
	`|<h[1-6]\b[^>]*>.*?</h[1-6]>|<a\b[^>]*>.*?</a>|<[^>]+>`)

// autoLinkSameDocLabels is the post-render L2 pass. It collects the heading ids
// present in the rendered fragment and wraps any bare, conservative label token
// in body text whose slug matches one of them in an anchor to `#<slug>`. When
// the fragment has no labeled headings (the overwhelmingly common case) it is a
// cheap no-op: nothing to target, nothing to scan.
//
// Runs LAST in the Linkify* pipeline, after bead-id/MR/JIRA linkification, so
// the anchors those passes just inserted are seen by sameDocSkipRe and never
// double-wrapped.
func autoLinkSameDocLabels(s string) string {
	ids := collectHeadingIDs(s)
	if len(ids) == 0 {
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range sameDocSkipRe.FindAllStringIndex(s, -1) {
		b.WriteString(replaceSameDocLabels(s[last:loc[0]], ids))
		b.WriteString(s[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(replaceSameDocLabels(s[last:], ids))
	return b.String()
}

// collectHeadingIDs returns the set of heading ids in the fragment, or nil when
// there are none. The ids were minted by A1's slugifyAnchor, so they are the
// exact strings a matching label token slugs to.
func collectHeadingIDs(s string) map[string]bool {
	ms := headingIDRe.FindAllStringSubmatch(s, -1)
	if len(ms) == 0 {
		return nil
	}
	ids := make(map[string]bool, len(ms))
	for _, m := range ms {
		ids[m[1]] = true
	}
	return ids
}

// replaceSameDocLabels wraps each conservative label token in s whose slug is a
// heading id in the same fragment. The chunk must already be free of the skip
// regions (autoLinkSameDocLabels segments around sameDocSkipRe before calling).
// The slug is `[a-z0-9-]` from slugifyAnchor, so the `#<slug>` href needs no
// escaping.
func replaceSameDocLabels(s string, ids map[string]bool) string {
	if s == "" {
		return s
	}
	return sameDocLabelRe.ReplaceAllStringFunc(s, func(tok string) string {
		slug := slugifyAnchor(tok)
		if slug == "" || !ids[slug] {
			return tok
		}
		return `<a href="#` + slug + `" class="samedoc-anchor">` + tok + `</a>`
	})
}
