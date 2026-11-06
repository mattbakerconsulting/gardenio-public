package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// linkRule wraps a regex with a function that builds the destination URL for
// each match. The full match is preserved as the user-visible link label —
// only the URL is computed from submatches.
type linkRule struct {
	re  *regexp.Regexp
	url func(submatches []string) string
}

// replace applies the rule's regex to s, wrapping each match in a Slack mrkdwn
// hyperlink (`<url|match>`). The match text becomes the visible label so
// `ka-abc` stays readable while clicking jumps to the viewer.
func (r linkRule) replace(s string) string {
	return r.re.ReplaceAllStringFunc(s, func(match string) string {
		sub := r.re.FindStringSubmatch(match)
		u := r.url(sub)
		if u == "" {
			return match
		}
		return fmt.Sprintf("<%s|%s>", u, match)
	})
}

// bareURLPattern matches a bare http(s) URL up to the next whitespace or
// Slack mrkdwn delimiter. We use it to keep linkify rules from rewriting
// matches that happen to fall inside a URL path/query (e.g. ka-XXX inside
// `https://viewer/ka-XXX` would otherwise be wrapped, producing nested
// mrkdwn that Slack normalises into `<https://A/<https://A/ka-X|ka-X>>`).
var bareURLPattern = regexp.MustCompile(`https?://[^\s<>|]+`)

// applyRuleAvoidingSpans applies a single rule to s, but never inside an
// existing `<...>` Slack mrkdwn span (links, user/channel mentions) or a
// bare http(s) URL. This is what lets us run rules in sequence: rule 1 may
// have inserted `<url|ka-X>` spans which rule 2 must leave alone, and lets
// `ka-XXX` inside a URL path stay untouched.
func applyRuleAvoidingSpans(s string, r linkRule) string {
	if s == "" {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for len(s) > 0 {
		// Find the next protected region: either a `<...>` Slack span or a
		// bare URL. Whichever starts earliest wins.
		spanStart, spanLen := nextProtectedSpan(s)
		if spanStart < 0 {
			b.WriteString(r.replace(s))
			break
		}
		b.WriteString(r.replace(s[:spanStart]))
		b.WriteString(s[spanStart : spanStart+spanLen])
		s = s[spanStart+spanLen:]
	}
	return b.String()
}

// nextProtectedSpan returns the start index and length of the next region
// in s that linkify rules must skip. Protected regions are existing Slack
// mrkdwn spans (`<...>`) and bare http(s) URLs. Returns (-1, 0) when none.
func nextProtectedSpan(s string) (int, int) {
	lt := strings.IndexByte(s, '<')
	urlLoc := bareURLPattern.FindStringIndex(s)

	// Whichever protected region starts earliest wins.
	if lt >= 0 && (urlLoc == nil || lt <= urlLoc[0]) {
		// Slack mrkdwn never nests `<...>`, so the next `>` closes this span.
		gt := strings.IndexByte(s[lt:], '>')
		if gt < 0 {
			return lt, len(s) - lt
		}
		return lt, gt + 1
	}
	if urlLoc != nil {
		return urlLoc[0], urlLoc[1] - urlLoc[0]
	}
	return -1, 0
}

// linkifyText applies every rule in order to text. Each rule walks chunks
// outside existing `<...>` spans, so rules layered on top of one another do
// not corrupt links emitted by earlier rules.
func linkifyText(text string, rules []linkRule) string {
	if text == "" || len(rules) == 0 {
		return text
	}
	out := text
	for _, r := range rules {
		out = applyRuleAvoidingSpans(out, r)
	}
	return out
}

// LinkifyConfig collects the base URLs and project-key allowlists used to
// build the default link rules.
type LinkifyConfig struct {
	// BeadsViewerURL — base URL for the beads-viewer (ka-XXX → /<id>).
	BeadsViewerURL string
	// JiraBaseURL — base URL for JIRA (e.g. https://acme.atlassian.net).
	// Issue links resolve to <JiraBaseURL>/browse/<KEY>-<num>.
	JiraBaseURL string
	// JiraProjectKeys narrows the JIRA matcher to a specific allowlist of
	// project keys (case-insensitive). When empty, any 2–10 character
	// uppercase project key matches — broader, but more prone to false
	// positives like RFC-1234 or HTTP-200.
	JiraProjectKeys []string
	// GitlabBaseURL — base URL for GitLab (e.g. https://gitlab.com). Merge
	// request refs of the form `group/project!123` (or deeper paths) resolve
	// to <GitlabBaseURL>/<path>/-/merge_requests/<num>.
	GitlabBaseURL string
}

// jiraKeyPattern returns the regex source matching a JIRA project key. With
// an explicit allowlist we anchor to those exact keys (case-insensitive).
// Without one we accept any 2–10 character uppercase key — the standard
// JIRA convention — but this is a coarser net.
func jiraKeyPattern(keys []string) string {
	if len(keys) == 0 {
		return `[A-Z][A-Z0-9]{1,9}`
	}
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = regexp.QuoteMeta(strings.ToUpper(strings.TrimSpace(k)))
	}
	return `(?:` + strings.Join(parts, "|") + `)`
}

// buildLinkRules constructs the set of rules given a config. Rules whose
// base URL is unset are skipped, so the matrix of enabled providers is
// purely env-driven.
func buildLinkRules(cfg LinkifyConfig) []linkRule {
	var rules []linkRule

	if v := strings.TrimRight(cfg.BeadsViewerURL, "/"); v != "" {
		// Bare kbeads IDs in free text: `ka-XXX`.
		re := regexp.MustCompile(`\b(?:gd|ka)-[A-Za-z0-9]+\b`)
		rules = append(rules, linkRule{
			re:  re,
			url: func(sub []string) string { return v + "/" + sub[0] },
		})
	}

	if v := strings.TrimRight(cfg.JiraBaseURL, "/"); v != "" {
		// JIRA issue keys: `<KEY>-<num>` where <KEY> matches the configured
		// allowlist (or any 2–10 char uppercase token when no list is set).
		// (?-i) keeps the alternation case-sensitive even though strings.ToUpper
		// normalises the allowlist input.
		re := regexp.MustCompile(`(?-i)\b` + jiraKeyPattern(cfg.JiraProjectKeys) + `-\d+\b`)
		rules = append(rules, linkRule{
			re:  re,
			url: func(sub []string) string { return v + "/browse/" + sub[0] },
		})
	}

	if v := strings.TrimRight(cfg.GitlabBaseURL, "/"); v != "" {
		// GitLab MR refs in fully-qualified form: `group/project!123` or
		// nested groups `group/sub/project!123`. We require at least one slash
		// to avoid matching bare `!123` in chat (too ambiguous).
		re := regexp.MustCompile(`\b([a-zA-Z0-9_][a-zA-Z0-9_-]*(?:/[a-zA-Z0-9_][a-zA-Z0-9_-]*)+)!(\d+)\b`)
		rules = append(rules, linkRule{
			re: re,
			url: func(sub []string) string {
				return v + "/" + sub[1] + "/-/merge_requests/" + sub[2]
			},
		})
	}

	return rules
}

// linkifyBlocksJSON walks a JSON string carrying Slack BlockKit blocks (or
// attachments) and linkifies refs found inside any object whose `type` is
// `mrkdwn`. Returns the original string unchanged if it does not parse as
// JSON or has no mrkdwn nodes worth rewriting.
func linkifyBlocksJSON(jsonStr string, rules []linkRule) string {
	if jsonStr == "" || len(rules) == 0 {
		return jsonStr
	}
	var data any
	if err := json.Unmarshal([]byte(jsonStr), &data); err != nil {
		return jsonStr
	}
	if !walkLinkify(data, rules) {
		return jsonStr
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return jsonStr
	}
	// json.Encoder.Encode appends a trailing newline; strip it so the output
	// matches the wire shape Slack expects.
	out := buf.Bytes()
	if n := len(out); n > 0 && out[n-1] == '\n' {
		out = out[:n-1]
	}
	return string(out)
}

// walkLinkify traverses a decoded JSON tree and rewrites any `text` field on
// an object whose `type` is `mrkdwn`. Returns true if any rewrite happened.
//
// Each link adds the whole URL to the text (`gd-abc` becomes
// `<viewer/gd-abc|gd-abc>`, about 45 more chars), so a node the card builder
// clamped under its Block Kit limit could cross it here, and Slack rejects
// the whole message with invalid_blocks. A node whose linkified text would
// exceed its limit is left as it was, the same limits the hydration pass uses
// (walkMrkdwnLimit): decision gd-Jsbg1Re436's context section was clamped to
// 2900 chars, 12 bead ids made it 3470, and the decision never reached Slack
// (gd-fthcee65T7).
func walkLinkify(node any, rules []linkRule) bool {
	return walkMrkdwnLimit(node, hydrateMaxContext, func(s string, limit int) string {
		nt := linkifyText(s, rules)
		if utf8.RuneCountInString(nt) > limit {
			return s
		}
		return nt
	})
}

// slackLinkifyTransport implements slack-go's httpClient interface (has a
// `Do` method on *http.Request). It intercepts the message-posting endpoints
// and rewrites text/blocks/attachments to embed hyperlinks for any
// recognised refs (ka-XXX bead IDs, JIRA keys, GitLab MR refs) the agents
// (or anyone) put in the body.
//
// All other requests pass through untouched.
type slackLinkifyTransport struct {
	base  *http.Client
	rules []linkRule
	// hydrate, when non-nil and ready, runs the bead-link hydration pass
	// (slack_link_hydrate.go) ahead of the regex rules.
	hydrate *linkHydrator
}

// newSlackLinkifyClient wraps an *http.Client with a transport that linkifies
// outgoing chat.* message bodies. The rules slice controls which refs get
// rewritten and where they point.
func newSlackLinkifyClient(base *http.Client, rules []linkRule) *slackLinkifyTransport {
	if base == nil {
		base = http.DefaultClient
	}
	return &slackLinkifyTransport{base: base, rules: rules}
}

// chatLinkifyMethods lists the Slack Web API methods whose request bodies we
// rewrite. These are the methods that put user-visible text into Slack.
var chatLinkifyMethods = map[string]struct{}{
	"chat.postMessage":     {},
	"chat.update":          {},
	"chat.postEphemeral":   {},
	"chat.scheduleMessage": {},
}

func (t *slackLinkifyTransport) Do(req *http.Request) (*http.Response, error) {
	if len(t.rules) == 0 || req == nil || req.Body == nil || req.Method != http.MethodPost {
		return t.base.Do(req)
	}
	method := slackMethodFromPath(req.URL.Path)
	if method == "" {
		return t.base.Do(req)
	}
	if _, ok := chatLinkifyMethods[method]; !ok {
		return t.base.Do(req)
	}
	ct := req.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		return t.base.Do(req)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return t.base.Do(req)
	}
	_ = req.Body.Close()

	rewritten, ok := rewriteFormBodyHydrated(req.Context(), body, t.rules, t.hydrate)
	if !ok {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return t.base.Do(req)
	}
	req.Body = io.NopCloser(strings.NewReader(rewritten))
	req.ContentLength = int64(len(rewritten))
	req.Header.Set("Content-Length", strconv.Itoa(len(rewritten)))
	return t.base.Do(req)
}

// slackMethodFromPath extracts the Slack API method name from the request path.
// e.g. `/api/chat.postMessage` → `chat.postMessage`. Returns "" if none.
func slackMethodFromPath(path string) string {
	if path == "" {
		return ""
	}
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

// rewriteFormBodyHydrated is rewriteFormBody with the optional hydration pass
// run first on the parsed form. Hydration rewrites resolvable bead ids into
// labelled links (protected spans), so the regex rules then only touch what
// hydration left alone: unresolved ids, JIRA keys, MR refs.
func rewriteFormBodyHydrated(ctx context.Context, body []byte, rules []linkRule, h *linkHydrator) (string, bool) {
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return "", false
	}
	changed := false
	if h.ready() {
		changed = h.hydrateForm(ctx, vals)
	}
	if rewriteFormValues(vals, rules) {
		changed = true
	}
	if !changed {
		return "", false
	}
	return vals.Encode(), true
}

// rewriteFormValues linkifies the `text`, `blocks`, and `attachments` fields
// of a parsed form in place. Returns true when anything changed.
func rewriteFormValues(vals url.Values, rules []linkRule) bool {
	changed := false
	if text := vals.Get("text"); text != "" {
		if nt := linkifyText(text, rules); nt != text {
			vals.Set("text", nt)
			changed = true
		}
	}
	if blocks := vals.Get("blocks"); blocks != "" {
		if nb := linkifyBlocksJSON(blocks, rules); nb != blocks {
			vals.Set("blocks", nb)
			changed = true
		}
	}
	if atts := vals.Get("attachments"); atts != "" {
		if na := linkifyBlocksJSON(atts, rules); na != atts {
			vals.Set("attachments", na)
			changed = true
		}
	}
	return changed
}
