// Package shorttitle derives the short human name a bead is shown under from
// its type and title. It is the one source of truth for "a short name from a
// title": the bridge's render-time fallback (bridge/bead_display_name.go) and
// the create-time fills for beads that arrive without an authored short_title
// call the same function (plan gd-YZATAIOhfW, gd-V5nGseRS5f).
//
// Bead titles run 70–130 characters and are front-loaded with machine noise:
// `[2026-10-11T15:07] Schedule: …`, `beads-viewer: …`, `P3: …`, a self-
// referencing bead id. A naive 40-char cut of such a title yields junk like
// `[2026-10-11T15:07] Schedule: Merge desk`. So Derive applies a type-aware
// de-noising of the title and cuts it at a word boundary. An authored
// short_title is always preferred over Derive; the heuristics are the
// fallback, not the source of truth (thread gd-8Gxghn6hkt).
package shorttitle

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Max is the longest short title, in runes. It matches the daemon's
// short_title validator (kbeads, D1 of plan gd-YZATAIOhfW).
const Max = 40

var (
	// beadID matches a bare bead id (`gd-…`, `ka-…`) anywhere in a title.
	beadID = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]+\b`)
	// scheduleRunTitle matches the titles `gb schedule` stamps on run beads:
	// `[2026-10-11T15:20] Schedule: cicd watchdog: 20-min …`.
	scheduleRunTitle = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}T(\d{2}:\d{2})\]\s*Schedule:\s*(.+)$`)
	// leadingBracketTag strips any other leading `[…]` tag.
	leadingBracketTag = regexp.MustCompile(`^\[[^\]]*\]\s*`)
	// leadingOrdinal strips plan-child ordinals: `P0:`, `P12 -`.
	leadingOrdinal = regexp.MustCompile(`^P\d+\s*[:\-–—]\s*`)
	// leadingComponent strips a single-token component prefix such as
	// `beads-viewer:` or `gardenio/bridge:`. The token has no spaces, so a
	// multi-word prefix like `Merge PR #209:` is left alone (it is the name).
	leadingComponent = regexp.MustCompile(`^[a-z0-9][a-z0-9/_.\-]*:\s+`)
	// trailingParen drops a trailing parenthetical: `… (gd-XXX)`, `… (atomic rollback applied)`.
	trailingParen = regexp.MustCompile(`\s*\([^()]*\)\s*$`)
	// refPrefix recognises a `<ref>: <rest>` title whose prefix is itself the
	// name: `Merge PR #209: P4 per-session …` → `Merge PR #209`. The prefix
	// must carry a number or `#` so `Slack thread: …` does not collapse to
	// `Slack thread`.
	refPrefix = regexp.MustCompile(`^([^:]{3,40}[#\d][^:]{0,40}):\s+\S`)
	// clauseSplit splits on a spaced dash of any flavour.
	clauseSplit = regexp.MustCompile(`\s+[-–—]\s+`)
	// knownNoisePrefixes are prefixes the bridge's own creators stamp that
	// carry no meaning for a reader who is already in the Slack thread.
	knownNoisePrefixes = []string{
		"Slack thread:",
		"Plan transcript turn for",
		"Plan-gardener",
	}
)

// gatedTypes are the bead types whose creates the daemon's short_title gate
// watches (kbeads BEADS_SHORT_TITLE_GATE: task, bug, feature, epic, decision,
// escalation), plus agent, whose short title is its handle. In-repo creators
// fill these at the beadsapi create choke point; every other type (session,
// message, trigger, schedule, mail, bundle, config) is left to the bridge's
// render-time heuristics.
var gatedTypes = map[string]bool{
	"task":       true,
	"bug":        true,
	"feature":    true,
	"epic":       true,
	"decision":   true,
	"escalation": true,
	"agent":      true,
}

// Gated reports whether a create of beadType should carry a short_title.
func Gated(beadType string) bool {
	return gatedTypes[beadType]
}

// Handle returns the stored short title for an agent named name: the handle
// without a leading `@` (the badge is added at render time, like every other
// glyph), clipped to Max. Empty in, empty out.
func Handle(name string) string {
	return Clip(strings.TrimPrefix(strings.TrimSpace(name), "@"), Max)
}

// Derive returns a short name for a bead of beadType titled title, at most Max
// runes. It returns "" when nothing usable survives de-noising; the caller
// picks its own fallback (the bridge shows the id).
func Derive(beadType, title string) string {
	if beadType == "agent" {
		// Agent bead titles are the handle; that is the name people know.
		if h := strings.TrimPrefix(strings.TrimSpace(title), "@"); h != "" {
			return Clip("@"+h, Max)
		}
		return ""
	}
	if m := scheduleRunTitle.FindStringSubmatch(strings.TrimSpace(title)); m != nil {
		return scheduleRunName(m[1], m[2])
	}
	return heuristic(title)
}

// scheduleRunName renders a schedule run as `<schedule name> HH:MM`. The
// schedule name is the segment before the first colon of the rest of the
// title (`cicd watchdog: 20-min …` → `cicd watchdog`).
func scheduleRunName(hhmm, rest string) string {
	name := rest
	if i := strings.Index(rest, ":"); i > 0 {
		name = rest[:i]
	}
	name = Clip(name, Max-len(hhmm)-1)
	return name + " " + hhmm
}

// heuristic de-noises a free-form title. Each step is a rule a human would
// apply by eye; they are ordered so that the cheapest, most certain ones run
// first.
func heuristic(title string) string {
	t := beadID.ReplaceAllString(title, "")
	t = collapseSpaces(t)
	t = leadingBracketTag.ReplaceAllString(t, "")
	for _, p := range knownNoisePrefixes {
		if strings.HasPrefix(t, p) {
			t = collapseSpaces(strings.TrimPrefix(t, p))
		}
	}
	t = leadingOrdinal.ReplaceAllString(t, "")
	t = leadingComponent.ReplaceAllString(t, "")
	t = trailingParen.ReplaceAllString(t, "")
	t = collapseSpaces(t)

	// `Merge PR #209: …` — the reference prefix is the name.
	if m := refPrefix.FindStringSubmatch(t); m != nil {
		return Clip(m[1], Max)
	}
	// `… — extend watchdog coverage?` — a trailing question is the point of a
	// decision title; otherwise the first clause carries the subject.
	if parts := clauseSplit.Split(t, -1); len(parts) > 1 {
		last := strings.TrimSpace(parts[len(parts)-1])
		if strings.HasSuffix(last, "?") {
			t = last
		} else {
			t = strings.TrimSpace(parts[0])
		}
	}
	t = strings.Trim(t, ":;,-–— ")
	return Clip(t, Max)
}

// Clip collapses whitespace runs (including newlines) to one space and cuts
// the result to at most max runes at a word boundary, appending a single
// ellipsis rune only when it cut anything. A string with no usable boundary in
// range is cut hard so a long single token still fits. Empty in, empty out.
func Clip(s string, max int) string {
	s = collapseSpaces(s)
	if max < 1 {
		return ""
	}
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	limit := max - 1 // leave room for the ellipsis
	cut := limit
	for i := limit; i > limit/2; i-- {
		if runes[i] == ' ' {
			cut = i
			break
		}
	}
	out := strings.TrimRight(string(runes[:cut]), " ,;:-–—(+&/")
	return out + "…"
}

// collapseSpaces folds all whitespace runs (including newlines) to one space.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
