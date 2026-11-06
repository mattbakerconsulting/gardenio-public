package main

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yuin/goldmark"
)

// fieldValueMaxJSONDepth bounds the nested key/value expansion of a JSON field
// value. Beyond it the sub-tree renders as pretty-printed JSON text rather than
// yet another nested <dl> — a runaway-nesting backstop, not a real-data limit
// (operation_spec / stop specs are 1–3 levels).
const fieldValueMaxJSONDepth = 4

// humanizeFieldKey turns a snake_case bead field key into a human label:
// "operation_spec" → "Operation spec", "mr_url" → "MR URL". Acronyms that
// operators read as words (MR, URL, ID, …) are upcased so the label reads the
// way the team writes it in Slack. The raw key stays available as the <dt>
// title attribute so nobody loses the exact field name to grep for.
func humanizeFieldKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	parts := strings.Split(strings.ReplaceAll(key, "-", "_"), "_")
	out := make([]string, 0, len(parts))
	for i, p := range parts {
		if p == "" {
			continue
		}
		if up, ok := fieldKeyAcronyms[strings.ToLower(p)]; ok {
			out = append(out, up)
			continue
		}
		if i == 0 {
			out = append(out, strings.ToUpper(p[:1])+p[1:])
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, " ")
}

// fieldKeyAcronyms are the key segments rendered upper-case in a humanized
// field label instead of being title-cased like an ordinary word.
var fieldKeyAcronyms = map[string]string{
	"mr":    "MR",
	"url":   "URL",
	"id":    "ID",
	"ids":   "IDs",
	"iid":   "IID",
	"api":   "API",
	"aws":   "AWS",
	"ci":    "CI",
	"cpu":   "CPU",
	"dm":    "DM",
	"eks":   "EKS",
	"iam":   "IAM",
	"json":  "JSON",
	"jira":  "JIRA",
	"k8s":   "K8s",
	"llm":   "LLM",
	"pr":    "PR",
	"sa":    "SA",
	"sha":   "SHA",
	"sse":   "SSE",
	"tldr":  "TLDR",
	"ts":    "TS",
	"ui":    "UI",
	"uri":   "URI",
	"uuid":  "UUID",
	"vpc":   "VPC",
	"yaml":  "YAML",
	"rfc":   "RFC",
	"http":  "HTTP",
	"https": "HTTPS",
}

// renderFieldValue renders one bead field value as rich HTML instead of the
// raw <pre> dump the generic field list used to emit (ka-0000006884). The
// operator complaint this fixes: an escalation's `operation_spec` carried a
// perfectly good `mr_url` that was not clickable, and every other field read
// as an unformatted blob.
//
// Dispatch, in order:
//
//   - empty → an em-dash "empty" span.
//   - JSON object/array → a nested key/value <dl> (fieldJSONHTML), so every
//     leaf string gets the same URL/bead-id treatment as a top-level value.
//   - bare URL (the whole value is one absolute http(s) URL) → an anchor.
//   - RFC3339 timestamp → humanized relative time with the exact stamp as the
//     title attribute.
//   - anything else → goldmark markdown + the shared Linkify pipeline (bead
//     ids, MR !refs, JIRA keys, GFM autolinks).
//
// md may be nil, in which case the markdown branch degrades to LinkifyText.
func renderFieldValue(md goldmark.Markdown, value, basePath string, now time.Time) template.HTML {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return template.HTML(`<span class="empty">&mdash;</span>`) //nolint:gosec // constant markup
	}
	if h, ok := fieldJSONHTML(trimmed, basePath, 0); ok {
		return h
	}
	if href, ok := bareFieldURL(trimmed); ok {
		return fieldURLAnchor(href, trimmed)
	}
	if h, ok := fieldTimestampHTML(trimmed, now); ok {
		return h
	}
	return fieldMarkdownHTML(md, trimmed, basePath)
}

// fieldMarkdownHTML renders a field value through goldmark + LinkifyHTML, then
// unwraps a lone <p>…</p> so a one-line scalar (a role name, an agent id) stays
// inline in its <dd> instead of gaining paragraph margins. Multi-block values
// (a real prose field like `context`) keep their full block structure.
func fieldMarkdownHTML(md goldmark.Markdown, text, basePath string) template.HTML {
	if md == nil {
		return LinkifyText(text, basePath)
	}
	var buf bytes.Buffer
	if err := md.Convert([]byte(text), &buf); err != nil {
		return LinkifyText(text, basePath)
	}
	return LinkifyHTML(unwrapSoleParagraph(buf.String()), basePath)
}

// soleParagraphOpenRe matches an opening <p> tag at the very start of a
// rendered fragment, WITH its attributes — the viewer's goldmark stamps
// `data-block-id` on every block, so a naive `<p>` prefix check never fires and
// every one-line field value keeps paragraph margins (which reads as a broken
// two-row grid cell in a <dl>).
var soleParagraphOpenRe = regexp.MustCompile(`^<p(\s[^>]*)?>`)

// unwrapSoleParagraph strips the wrapping <p …>…</p> when the rendered fragment
// is exactly one paragraph and contains no other block element. Keeps a short
// scalar field value visually inline.
func unwrapSoleParagraph(h string) string {
	t := strings.TrimSpace(h)
	loc := soleParagraphOpenRe.FindStringIndex(t)
	if loc == nil || !strings.HasSuffix(t, "</p>") {
		return h
	}
	inner := strings.TrimSuffix(t[loc[1]:], "</p>")
	if strings.Contains(inner, "<p>") || strings.Contains(inner, "<p ") || strings.Contains(inner, "</p>") {
		return h
	}
	return inner
}

// bareFieldURL reports whether the whole trimmed value is a single absolute
// http(s) URL — the case that must become a clickable link (an escalation's
// mr_url, a dashboard link, a run URL). Deliberately strict: a value with any
// whitespace goes down the markdown path instead, where GFM autolinks handle
// embedded URLs.
func bareFieldURL(v string) (string, bool) {
	if strings.ContainsAny(v, " \t\n\r") {
		return "", false
	}
	if !strings.HasPrefix(v, "http://") && !strings.HasPrefix(v, "https://") {
		return "", false
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" {
		return "", false
	}
	return v, true
}

// fieldURLAnchor renders an external link with the safety attributes every
// off-site anchor in the viewer carries. label is shown; href is the target.
func fieldURLAnchor(href, label string) template.HTML {
	var b strings.Builder
	b.WriteString(`<a class="field-link" href="`)
	b.WriteString(template.HTMLEscapeString(href))
	b.WriteString(`" target="_blank" rel="noopener noreferrer">`)
	b.WriteString(template.HTMLEscapeString(label))
	b.WriteString(`</a>`)
	return template.HTML(b.String()) //nolint:gosec // href + label are HTML-escaped above.
}

// fieldTimestampHTML renders an RFC3339 field value ("2026-08-27T17:10:49Z" —
// notified_at, approved_at, operator_dm_at, …) as a humanized relative time
// with the exact stamp on hover. Non-timestamps fall through.
func fieldTimestampHTML(v string, now time.Time) (template.HTML, bool) {
	ts, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return "", false
	}
	var b strings.Builder
	b.WriteString(`<span class="field-time" title="`)
	b.WriteString(template.HTMLEscapeString(ts.UTC().Format("2006-01-02 15:04:05 MST")))
	b.WriteString(`">`)
	b.WriteString(template.HTMLEscapeString(fmtTimeAgo(ts, now)))
	b.WriteString(`</span>`)
	return template.HTML(b.String()), true //nolint:gosec // both interpolations are HTML-escaped.
}

// fieldJSONHTML renders a JSON object/array field value as a nested key/value
// list. Returns ok=false for anything that is not a JSON container so callers
// fall through to the scalar paths — notably a bare number or a quoted string
// is NOT treated as JSON (an `mr_iid: 1274` field should read as 1274, not as
// a JSON document).
func fieldJSONHTML(v, basePath string, depth int) (template.HTML, bool) {
	if len(v) == 0 || (v[0] != '{' && v[0] != '[') {
		return "", false
	}
	var any1 any
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	if err := dec.Decode(&any1); err != nil {
		return "", false
	}
	// Trailing garbage after a valid document means this was never JSON.
	if dec.More() {
		return "", false
	}
	return fieldJSONNode(any1, basePath, depth), true
}

// fieldJSONNode renders one decoded JSON node. Objects become a <dl>, arrays a
// <ul>, and leaves go through fieldJSONLeaf so a nested `mr_url` is as
// clickable as a top-level one.
func fieldJSONNode(n any, basePath string, depth int) template.HTML {
	switch t := n.(type) {
	case map[string]any:
		if len(t) == 0 {
			return template.HTML(`<span class="empty">{}</span>`) //nolint:gosec // constant markup
		}
		if depth >= fieldValueMaxJSONDepth {
			return fieldJSONPre(n)
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		b.WriteString(`<dl class="field-json">`)
		for _, k := range keys {
			b.WriteString(`<dt title="`)
			b.WriteString(template.HTMLEscapeString(k))
			b.WriteString(`">`)
			b.WriteString(template.HTMLEscapeString(humanizeFieldKey(k)))
			b.WriteString(`</dt><dd>`)
			b.WriteString(string(fieldJSONNode(t[k], basePath, depth+1)))
			b.WriteString(`</dd>`)
		}
		b.WriteString(`</dl>`)
		return template.HTML(b.String()) //nolint:gosec // keys escaped; leaves escaped in fieldJSONLeaf.
	case []any:
		if len(t) == 0 {
			return template.HTML(`<span class="empty">[]</span>`) //nolint:gosec // constant markup
		}
		if depth >= fieldValueMaxJSONDepth {
			return fieldJSONPre(n)
		}
		var b strings.Builder
		b.WriteString(`<ul class="field-json-list">`)
		for _, item := range t {
			b.WriteString(`<li>`)
			b.WriteString(string(fieldJSONNode(item, basePath, depth+1)))
			b.WriteString(`</li>`)
		}
		b.WriteString(`</ul>`)
		return template.HTML(b.String()) //nolint:gosec // leaves escaped in fieldJSONLeaf.
	default:
		return fieldJSONLeaf(n, basePath)
	}
}

// fieldJSONLeaf renders a scalar JSON value: a URL becomes an anchor, a string
// gets bead-id / MR-ref / JIRA linkification, and numbers/bools/null render as
// <code>.
func fieldJSONLeaf(n any, basePath string) template.HTML {
	switch t := n.(type) {
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return template.HTML(`<span class="empty">&mdash;</span>`) //nolint:gosec // constant markup
		}
		if href, ok := bareFieldURL(s); ok {
			return fieldURLAnchor(href, s)
		}
		return LinkifyText(s, basePath)
	case json.Number:
		return template.HTML(`<code>` + template.HTMLEscapeString(t.String()) + `</code>`) //nolint:gosec // escaped
	case bool:
		return template.HTML(`<code>` + strconv.FormatBool(t) + `</code>`) //nolint:gosec // bool literal
	case nil:
		return template.HTML(`<span class="empty">null</span>`) //nolint:gosec // constant markup
	default:
		return fieldJSONPre(n)
	}
}

// fieldJSONPre is the depth-cap / unknown-shape fallback: indented JSON in a
// <pre>. Still readable, just not expanded into nested lists.
func fieldJSONPre(n any) template.HTML {
	b, err := json.MarshalIndent(n, "", "  ")
	if err != nil {
		return template.HTML(`<span class="empty">&mdash;</span>`) //nolint:gosec // constant markup
	}
	return template.HTML(`<pre class="field-json-raw">` + template.HTMLEscapeString(string(b)) + `</pre>`) //nolint:gosec // escaped
}
