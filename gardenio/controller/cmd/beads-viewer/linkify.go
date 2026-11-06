package main

import (
	"html/template"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// beadIDRe matches a bead ID reference with an OPTIONAL `#label` anchor
// suffix (bead anchors — epic ka-0000006913). Examples:
//
//	ka-abc123            -> id only (unchanged legacy behavior)
//	ka-0000002402#c1-1   -> id + anchor label "c1-1"
//	ka-0000002402#C1.1   -> id + anchor label "C1.1" (slugified to c1-1)
//
// The label starts and ends with an alphanumeric and may carry interior
// `.`/`-`/`_`, so a trailing sentence period (`…ka-X#c1-1.`) is NOT consumed.
// The bare-id arm is identical to the previous pattern, so refs without a
// `#` linkify exactly as before.
var beadIDRe = regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]+(?:#[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?)?`)

// splitBeadAnchor splits a beadIDRe match into the bare bead id and the raw
// anchor label (empty when there is no `#suffix`). The id alone is what keys
// the typed-mention lookup, per-render mention counter, and data-bead-id —
// the anchor is a render-time href concern only.
func splitBeadAnchor(match string) (id, label string) {
	id, label, _ = strings.Cut(match, "#")
	return id, label
}

// anchorHrefSuffix slugifies a raw anchor label through the SAME slugifyAnchor
// used to mint heading ids (anchor.go), so `#C1.1`, `#c1-1`, and `#c1.1` all
// resolve to the heading rendered with id="c1-1". Returns "" (no fragment)
// for an empty or unsluggable label.
func anchorHrefSuffix(label string) string {
	if label == "" {
		return ""
	}
	slug := slugifyAnchor(label)
	if slug == "" {
		return ""
	}
	return "#" + slug
}

const (
	// defaultJiraBaseURL is the JIRA Cloud instance JIRA keys deep-link into.
	// Overridable via BEADS_VIEWER_JIRA_BASE_URL.
	defaultJiraBaseURL = "https://acme.atlassian.net"
	// defaultJiraProjectKeys is the comma-separated allowlist of real JIRA
	// project keys whose `<KEY>-<num>` refs get linkified. Overridable via
	// BEADS_VIEWER_JIRA_PROJECT_KEYS. An explicit allowlist (rather than a
	// permissive `[A-Z][A-Z0-9]+` matcher) keeps us from linkifying
	// ALL-CAPS-number tokens that are not tickets — version strings,
	// HTTP-200, RFC-1234, etc.
	defaultJiraProjectKeys = "FSM,PE,DEVOPS"
)

// jiraBaseURL returns the JIRA base URL used to construct issue-key anchors.
// Overridable via BEADS_VIEWER_JIRA_BASE_URL. A trailing slash is trimmed so
// callers can append `/browse/<KEY>` cleanly.
func jiraBaseURL() string {
	if v := strings.TrimSpace(os.Getenv("BEADS_VIEWER_JIRA_BASE_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultJiraBaseURL
}

// jiraReMu guards the memoized JIRA key regex. The compiled regex is cached
// keyed on the resolved project-keys string so a process compiles it once,
// while tests that flip BEADS_VIEWER_JIRA_PROJECT_KEYS via t.Setenv still get
// a fresh compile when the key set changes.
var (
	jiraReMu    sync.Mutex
	jiraReKey   string
	jiraReCache *regexp.Regexp
)

// jiraKeyRe returns the regex matching an allowlisted JIRA issue key (e.g.
// `FSM-371`). The alternation is built from the resolved project-key allowlist
// and is case-sensitive: only UPPERCASE project keys match, so lowercase
// `gd-` bead IDs can never collide. Returns nil when the allowlist is empty
// (JIRA linkification disabled).
func jiraKeyRe() *regexp.Regexp {
	keysCSV := os.Getenv("BEADS_VIEWER_JIRA_PROJECT_KEYS")
	if strings.TrimSpace(keysCSV) == "" {
		keysCSV = defaultJiraProjectKeys
	}

	jiraReMu.Lock()
	defer jiraReMu.Unlock()
	if jiraReCache != nil && jiraReKey == keysCSV {
		return jiraReCache
	}

	parts := make([]string, 0, 4)
	for _, k := range strings.Split(keysCSV, ",") {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k != "" {
			parts = append(parts, regexp.QuoteMeta(k))
		}
	}
	jiraReKey = keysCSV
	if len(parts) == 0 {
		jiraReCache = nil
		return nil
	}
	// (?-i) forces case-sensitivity; \b boundaries keep us off substrings
	// like `XFSM-1` or `FSM-1a`.
	jiraReCache = regexp.MustCompile(`(?-i)\b(?:` + strings.Join(parts, "|") + `)-\d+\b`)
	return jiraReCache
}

// replaceJiraKeys wraps allowlisted JIRA issue keys (e.g. `FSM-371`) in the
// given chunk in anchor tags pointing at <jiraBaseURL>/browse/<KEY>. The chunk
// must not contain HTML regions that should be skipped — the Linkify* entry
// points segment around anchorRe before calling, so already-linked spans and
// tag markup are never passed here. JIRA keys are uppercase while bead IDs are
// lowercase `gd-`, so the two matchers never collide.
func replaceJiraKeys(s string) string {
	re := jiraKeyRe()
	if re == nil {
		return s
	}
	base := jiraBaseURL()
	return re.ReplaceAllStringFunc(s, func(key string) string {
		href := template.HTMLEscapeString(base + "/browse/" + key)
		return `<a href="` + href + `" target="_blank" rel="noopener">` + key + `</a>`
	})
}

// mrIIDRe matches a GitLab-style MR reference of the form `!N` (e.g. `!2948`).
// The leading char class restricts matches to MR refs that follow a real
// separator — start-of-input, whitespace (incl. newline), `(`, or `[`. Patterns
// like `text!2948` (no separator) and `<code>!2948</code>` (preceding `>` is
// not in the class) are deliberately NOT matched. N must be ≥2 digits to keep
// the false-positive rate low (e.g. `!1` will not link).
var mrIIDRe = regexp.MustCompile(`(^|[\s(\[])!(\d{2,})\b`)

const defaultGitLabProjectURL = "https://gitlab.com/example-org/gardenio"

// gitlabProjectURL returns the GitLab project URL used to construct MR-ref
// anchors. Overridable via BEADS_VIEWER_DEFAULT_GITLAB_PROJECT_URL. A trailing
// slash is trimmed so callers can append `/-/merge_requests/N` cleanly.
func gitlabProjectURL() string {
	if v := os.Getenv("BEADS_VIEWER_DEFAULT_GITLAB_PROJECT_URL"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return defaultGitLabProjectURL
}

// skipRe matches HTML regions where bead-ID linkification must NOT run:
//   - <pre class="mermaid-diagram">...</pre>: a mermaid diagram's escaped source
//     (ka-0000005977). A `ka-XXX` / `!N` / JIRA-key literal in a diagram label
//     must survive byte-for-byte: the client driver reads the source via
//     code.textContent, and wrapping part of it in an <a> would corrupt the
//     diagram (and litter the no-JS fallback with spurious links).
//   - <a ...>...</a>: already-linked text (don't double-wrap)
//   - any other tag's own markup: attribute values like src="/image/.../ka-XXX/foo.png"
//     would otherwise have the `ka-XXX` inside the URL wrapped in an anchor,
//     producing broken HTML. ka-0000006131.
//
// Alternation order is leftmost-first: the mermaid-block variant is tried before
// the anchor-span and bare-tag branches so the whole diagram region (opening
// tag through </pre>) is skipped as one unit rather than its inner <code> tag
// being matched alone by the broader `<[^>]+>` branch.
var skipRe = regexp.MustCompile(`(?is)` + mermaidBlockPattern + `|<a\b[^>]*>.*?</a>|<[^>]+>`)

// mermaidBlockPattern matches a rendered mermaid diagram block (opening
// `<pre class="mermaid-diagram" …>` through its `</pre>`). Its escaped source
// must survive every post-render rewrite byte-for-byte — the client driver
// feeds code.textContent straight to mermaid, so a `ka-XXX` / `!N` / `:::grafana`
// literal in a diagram label must NOT be linkified or embedded (ka-0000005977).
// skipRe uses it for bead-ID/MR/JIRA linkification; the grafana-directive
// pass skips every <pre> (preBlockRe), a diagram among them.
const mermaidBlockPattern = `<pre class="mermaid-diagram"[^>]*>.*?</pre>`

// preBlockRe matches any rendered <pre> block, a code block or a mermaid
// diagram, used by embedGrafanaDirectives to leave its text alone: a
// `:::grafana` line in a code block is quoted text, never an embed
// (gd-ZwnwonBxGS). (?is): dotall so `.*?` spans the multi-line source,
// case-insensitive to match defensively.
var preBlockRe = regexp.MustCompile(`(?is)<pre\b[^>]*>.*?</pre>`)

// anchorRe is kept as an alias for skipRe so existing call sites don't churn.
var anchorRe = skipRe

// LinkifyOpts controls optional behavior of the Linkify* family.
type LinkifyOpts struct {
	// SkipMRRefs disables bare `!N` MR-ref linkification. Set when the
	// bead's project has more than one repo (e.g. project:monorepo): a bare
	// `!N` is fundamentally ambiguous between the repos[] entries and the
	// linkifier can't pick the correct one, so it's safer to render the ref
	// as plain text than to silently point at the wrong repo. See
	// ka-0000004231.
	SkipMRRefs bool

	// ProjectURL is the GitLab project web URL that bare `!N` MR refs on
	// this bead resolve against (no trailing slash, no `.git` suffix), e.g.
	// "https://gitlab.com/example-org/monorepo". It is resolved from
	// the bead's OWN project bead, so a `project:devsecops` bead links to
	// monorepo rather than to the process-wide gardenio default. Empty
	// falls back to gitlabProjectURL(). Ignored when SkipMRRefs is set.
	// See ka-0000005612.
	ProjectURL string
}

// LinkifyHTML wraps bare bead ID references in the given HTML fragment in
// anchor tags pointing to basePath + "/" + id. Bead IDs that already appear
// inside an existing <a>...</a> span are left untouched so we don't
// double-wrap markdown-rendered links. basePath is used verbatim (typically
// "" or "/some/prefix").
func LinkifyHTML(htmlIn, basePath string) template.HTML {
	return LinkifyHTMLWithOpts(htmlIn, basePath, LinkifyOpts{})
}

// LinkifyHTMLWithOpts is LinkifyHTML with caller-supplied behavior gates.
func LinkifyHTMLWithOpts(htmlIn, basePath string, opts LinkifyOpts) template.HTML {
	// Expand any :::grafana dashboard-embed directives first, so the directive
	// line is replaced by iframe/link-card HTML before bead-ID linkification
	// runs over the result (W2c — ka-0000001078).
	htmlIn = embedGrafanaDirectives(htmlIn, grafanaEmbedBaseURL())
	var b strings.Builder
	last := 0
	for _, loc := range anchorRe.FindAllStringIndex(htmlIn, -1) {
		chunk := replaceBeadIDs(htmlIn[last:loc[0]], basePath)
		chunk = replaceMRIIDs(chunk, opts)
		chunk = replaceJiraKeys(chunk)
		b.WriteString(chunk)
		b.WriteString(htmlIn[loc[0]:loc[1]])
		last = loc[1]
	}
	chunk := replaceBeadIDs(htmlIn[last:], basePath)
	chunk = replaceMRIIDs(chunk, opts)
	chunk = replaceJiraKeys(chunk)
	b.WriteString(chunk)
	// L2 same-doc anchors run last, over the fully linkified HTML, so the
	// bead/MR/JIRA anchors just inserted are skipped rather than double-wrapped
	// (A4 — ka-0000007811).
	return template.HTML(autoLinkSameDocLabels(b.String())) //nolint:gosec // caller supplies HTML; we only insert anchor tags with escaped hrefs.
}

// LinkifyText HTML-escapes a plain-text string and then wraps bead ID
// references in anchor tags. Use this for user-supplied text (comment body,
// title, typed fields) that would otherwise go through template auto-escape.
func LinkifyText(text, basePath string) template.HTML {
	return LinkifyTextWithOpts(text, basePath, LinkifyOpts{})
}

// LinkifyTextWithOpts is LinkifyText with caller-supplied behavior gates.
func LinkifyTextWithOpts(text, basePath string, opts LinkifyOpts) template.HTML {
	escaped := template.HTMLEscapeString(text)
	return LinkifyHTMLWithOpts(escaped, basePath, opts)
}

// LinkifyDescriptionHTML wraps bare bead ID references with rich anchors
// suitable for the highlight pipeline (E2 — ka-0000000608): each anchor
// carries class="mention", data-bead-id, and a per-render running counter
// id="mention-ka-XXX-N" so multi-mentions of the same target each get a
// unique anchor that the highlight script can scroll to. typedByID maps
// target bead ID → typed-mention verb (e.g. "blocks") for typed mentions
// found in this field; matching anchors get data-relationship-type=<verb>
// so the highlight script can prefer the typed match when ?rel=<verb> is
// in the URL.
//
// Note: typedByID is keyed by target ID alone, so a bead authored with
// multiple distinct verbs targeting the same bead inside one field will
// have all of its anchors tagged with the *first* verb's value. This
// matches the daemon's "first relationship_type wins per (target, field)"
// dedup at the API surface and keeps the renderer simple — the highlight
// script also falls back to "any match" when ?rel doesn't line up.
func LinkifyDescriptionHTML(htmlIn, basePath string, typedByID map[string]string) template.HTML {
	return LinkifyDescriptionHTMLWithOpts(htmlIn, basePath, typedByID, LinkifyOpts{})
}

// LinkifyDescriptionHTMLWithOpts is LinkifyDescriptionHTML with caller-supplied
// behavior gates.
func LinkifyDescriptionHTMLWithOpts(htmlIn, basePath string, typedByID map[string]string, opts LinkifyOpts) template.HTML {
	// Expand :::grafana dashboard-embed directives before bead-ID linkification
	// (W2c — ka-0000001078), same as LinkifyHTMLWithOpts.
	htmlIn = embedGrafanaDirectives(htmlIn, grafanaEmbedBaseURL())
	counts := make(map[string]int)
	repl := func(s string) string {
		s = replaceBeadIDsRich(s, basePath, typedByID, counts)
		s = replaceMRIIDs(s, opts)
		s = replaceJiraKeys(s)
		return s
	}
	var b strings.Builder
	last := 0
	for _, loc := range anchorRe.FindAllStringIndex(htmlIn, -1) {
		b.WriteString(repl(htmlIn[last:loc[0]]))
		b.WriteString(htmlIn[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(repl(htmlIn[last:]))
	// L2 same-doc anchors run last, over the fully linkified HTML (A4 —
	// ka-0000007811). See LinkifyHTMLWithOpts.
	return template.HTML(autoLinkSameDocLabels(b.String())) //nolint:gosec // caller supplies HTML; we only insert anchor tags with escaped hrefs.
}

func replaceBeadIDs(s, basePath string) string {
	return beadIDRe.ReplaceAllStringFunc(s, func(match string) string {
		id, label := splitBeadAnchor(match)
		href := template.HTMLEscapeString(basePath+"/"+id) + anchorHrefSuffix(label)
		return `<a href="` + href + `">` + match + `</a>`
	})
}

// typedMentionsByID extracts a target-bead-id → verb map from a slice of
// BeadReferenceSummary rows, restricted to refs in the given source field
// (e.g. "description"). Untyped refs (RelationshipType == nil) and refs
// from other fields are skipped. The first typed verb seen for each target
// wins — see LinkifyDescriptionHTML for the rationale.
func typedMentionsByID(refs []beadsapi.BeadReferenceSummary, sourceField string) map[string]string {
	if len(refs) == 0 {
		return nil
	}
	out := make(map[string]string, len(refs))
	for _, r := range refs {
		if r.RelationshipType == nil || *r.RelationshipType == "" {
			continue
		}
		if sourceField != "" && r.SourceField != sourceField {
			continue
		}
		if r.ID == "" {
			continue
		}
		if _, dupe := out[r.ID]; dupe {
			continue
		}
		out[r.ID] = *r.RelationshipType
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func replaceBeadIDsRich(s, basePath string, typedByID map[string]string, counts map[string]int) string {
	return beadIDRe.ReplaceAllStringFunc(s, func(match string) string {
		// The bare id keys the mention counter, the per-render anchor id, the
		// data-bead-id, and the typed-mention lookup — the `#label` is a href
		// concern only, so a `ka-X#c1-1` ref still participates in highlight
		// scroll/typed-mention exactly like a bare `ka-X`.
		id, label := splitBeadAnchor(match)
		counts[id]++
		n := counts[id]
		href := template.HTMLEscapeString(basePath+"/"+id) + anchorHrefSuffix(label)
		anchorID := template.HTMLEscapeString("mention-" + id + "-" + strconv.Itoa(n))
		var rel string
		if v, ok := typedByID[id]; ok && v != "" {
			rel = ` data-relationship-type="` + template.HTMLEscapeString(v) + `"`
		}
		return `<a href="` + href + `" id="` + anchorID + `" data-bead-id="` + id + `" class="mention"` + rel + `>` + match + `</a>`
	})
}

// replaceMRIIDs wraps `!N` MR references in anchors pointing at the bead's own
// GitLab project (opts.ProjectURL), falling back to the process-wide default
// (gitlabProjectURL) only when the caller could not resolve one. The leading
// separator captured by mrIIDRe is preserved verbatim so adjacent punctuation
// (parens, brackets, whitespace) is not consumed. Caller is responsible for
// ensuring s does not contain HTML regions that should be skipped — the
// Linkify* entry points handle that by segmenting around anchorRe before
// calling.
func replaceMRIIDs(s string, opts LinkifyOpts) string {
	if opts.SkipMRRefs {
		return s
	}
	matches := mrIIDRe.FindAllStringSubmatchIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	projectURL := opts.ProjectURL
	if projectURL == "" {
		projectURL = gitlabProjectURL()
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		// m = [matchStart, matchEnd, leadStart, leadEnd, digitsStart, digitsEnd]
		b.WriteString(s[last:m[0]])
		b.WriteString(s[m[2]:m[3]])
		n := s[m[4]:m[5]]
		href := template.HTMLEscapeString(projectURL + "/-/merge_requests/" + n)
		b.WriteString(`<a href="`)
		b.WriteString(href)
		b.WriteString(`" target="_blank" rel="noopener">!`)
		b.WriteString(n)
		b.WriteString(`</a>`)
		last = m[1]
	}
	b.WriteString(s[last:])
	return b.String()
}
