package advice

// render.go owns the CANONICAL prime-advice item rendering — the exact markdown
// `gb prime` emits for each matched advice bead. It lives here (not in cmd/gb)
// so the controller metrics collector can measure the abbreviated-render size of
// a real archetype's matched pool through the SAME code path that produces the
// bytes an agent actually receives. `gb prime` delegates to these functions, so
// the R4 tripwire metric (gardenio_advice_prime_render_bytes, ka-0000002748) and
// the live render can never diverge — the same measure-one-way discipline the
// AuditCatalog byte gauge already follows.

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// DefaultSummaryMaxChars is the default per-item abbreviated-summary rune cap
// (gb prime's primeAdviceSummaryMaxChars). Kept here so the metrics collector
// sizes the render with the same default the CLI uses when no override is set.
const DefaultSummaryMaxChars = 500

// DefaultRenderByteBudget is the default ceiling on the rendered size of the
// non-inline abbreviated advice section in gb prime output (gb prime's
// primeAdviceByteBudget). The R4 tripwire watches the abbreviated-render demand
// against this budget: alert before a real archetype's demand crosses ~80% of
// it, so pressure is visible before silent truncation can recur (ka-0000002748).
const DefaultRenderByteBudget = 60000

// IsInline reports whether the bead carries the "inline:true" label, opting it
// into full-text rendering in gb prime output. Inline beads bypass the
// abbreviated byte budget entirely, so the R4 render-demand measure excludes
// them (they are the irreducible must-inline policy, not budget pressure).
func IsInline(b *beadsapi.BeadDetail) bool {
	for _, l := range b.Labels {
		if l == "inline:true" {
			return true
		}
	}
	return false
}

// RenderAbbrevItem writes the abbreviated pointer rendering of an advice bead:
// title + one-line summary (if distinct) + `gb advice show` pointer. This is
// the default rendering and the form whose bytes count against the abbreviated
// budget. summaryMax caps the per-item summary excerpt.
func RenderAbbrevItem(w io.Writer, header string, b *beadsapi.BeadDetail, summaryMax int) {
	summary := Excerpt(CleanDescription(b.Title, b.Description), summaryMax)
	if summary == "" || summary == b.Title {
		fmt.Fprintf(w, "**[%s]** %s — `gb advice show %s`\n", header, b.Title, b.ID)
	} else {
		fmt.Fprintf(w, "**[%s]** %s — %s `gb advice show %s`\n", header, b.Title, summary, b.ID)
	}
	fmt.Fprintln(w)
}

// RenderInlineItem writes the full-text (inline) rendering of an advice bead:
// title line plus the cleaned description indented two spaces.
func RenderInlineItem(w io.Writer, header string, b *beadsapi.BeadDetail) {
	fmt.Fprintf(w, "**[%s]** %s\n", header, b.Title)
	desc := CleanDescription(b.Title, b.Description)
	if desc != "" {
		for _, line := range strings.Split(desc, "\n") {
			fmt.Fprintf(w, "  %s\n", line)
		}
	}
	fmt.Fprintln(w)
}

// RenderTitleOnlyItem writes the most compact rendering: title + `gb advice
// show` pointer, no summary line. Used as the overflow form once the
// abbreviated budget is exhausted.
func RenderTitleOnlyItem(w io.Writer, header string, b *beadsapi.BeadDetail) {
	fmt.Fprintf(w, "**[%s]** %s — `gb advice show %s`\n\n", header, b.Title, b.ID)
}

// AbbrevRenderBytes returns the abbreviated-render demand for a matched pool:
// the total bytes the NON-inline items would emit through RenderAbbrevItem — the
// exact quantity that accumulates against the abbreviated byte budget in gb
// prime. Inline items are excluded because they bypass that budget. The header
// is derived per item via BuildScopeHeader(Scope, ScopeTarget), matching the
// header gb prime renders. This is the number the R4 tripwire compares to ~80%
// of DefaultRenderByteBudget.
func AbbrevRenderBytes(matched []MatchedAdvice, summaryMax int) int {
	var buf bytes.Buffer
	total := 0
	for _, m := range matched {
		if m.Bead == nil || IsInline(m.Bead) {
			continue
		}
		buf.Reset()
		RenderAbbrevItem(&buf, BuildScopeHeader(m.Scope, m.ScopeTarget), m.Bead, summaryMax)
		total += buf.Len()
	}
	return total
}

// Excerpt renders a one-line preview of an advice body up to max runes. The
// body is flattened (newlines and runs of whitespace collapse to single spaces)
// so multi-line advice reads cleanly inline; if the result exceeds max it is cut
// on a sentence boundary when one falls in the latter part of the budget,
// otherwise on a word boundary, and an ellipsis is appended. A non-truncated
// excerpt ends with a period so it joins cleanly with the trailing `gb advice
// show` pointer. Returns "" for empty input.
func Excerpt(s string, max int) string {
	flat := strings.Join(strings.Fields(s), " ")
	if flat == "" {
		return ""
	}
	runes := []rune(flat)
	if len(runes) <= max {
		if r := runes[len(runes)-1]; r != '.' && r != '!' && r != '?' && r != ':' {
			return flat + "."
		}
		return flat
	}
	// Prefer a sentence boundary in the back half of the budget; fall back to
	// the last word boundary; never cut mid-word.
	window := string(runes[:max])
	cut := -1
	for _, sep := range []string{". ", "! ", "? "} {
		if i := strings.LastIndex(window, sep); i > max/2 && i+1 > cut {
			cut = i + 1 // keep the punctuation
		}
	}
	if cut > 0 {
		return strings.TrimRight(window[:cut], " ") + " …"
	}
	if i := strings.LastIndexByte(window, ' '); i > 0 {
		return window[:i] + " …"
	}
	return window + " …"
}

// CleanDescription prepares an advice description for rendering by dropping a
// title line already emitted on the preceding line and collapsing runs of
// identical consecutive lines. Returns "" when the description is entirely
// redundant (empty, equal to the title, or only repeats the title).
//
// This prevents the "title renders twice" (and for malformed mirror beads, up
// to 3×) bug in gb prime output where descriptions that begin with their own
// title duplicated content under each item.
func CleanDescription(title, description string) string {
	if description == "" || description == title {
		return ""
	}
	// Collapse duplicates first so that a title repeated N× at the top of the
	// body is reduced to a single occurrence that StripTitlePrefix can then
	// remove.
	desc := CollapseConsecutiveDuplicateLines(description)
	desc = StripTitlePrefix(title, desc)
	if strings.TrimSpace(desc) == "" {
		return ""
	}
	return desc
}

// StripTitlePrefix returns description with a leading title prefix removed when
// description begins with title followed by a newline. Both direct and per-line
// whitespace-tolerant matches are supported, and multi-line titles are handled.
// Leading blank lines after the stripped prefix are also removed.
func StripTitlePrefix(title, description string) string {
	if title == "" {
		return description
	}
	if strings.HasPrefix(description, title+"\n") {
		return trimLeadingBlankLines(description[len(title)+1:])
	}
	titleLines := strings.Split(title, "\n")
	descLines := strings.Split(description, "\n")
	if len(descLines) < len(titleLines) {
		return description
	}
	for i, tl := range titleLines {
		if strings.TrimSpace(tl) != strings.TrimSpace(descLines[i]) {
			return description
		}
	}
	rest := descLines[len(titleLines):]
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	return strings.Join(rest, "\n")
}

// trimLeadingBlankLines drops any leading lines that are empty or whitespace-only.
func trimLeadingBlankLines(s string) string {
	for {
		idx := strings.IndexByte(s, '\n')
		if idx < 0 {
			return s
		}
		if strings.TrimSpace(s[:idx]) != "" {
			return s
		}
		s = s[idx+1:]
	}
}

// CollapseConsecutiveDuplicateLines collapses runs of identical consecutive
// lines to a single line, preserving order.
func CollapseConsecutiveDuplicateLines(s string) string {
	if s == "" {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	var prev string
	for i, line := range lines {
		if i > 0 && line == prev {
			continue
		}
		out = append(out, line)
		prev = line
	}
	return strings.Join(out, "\n")
}
