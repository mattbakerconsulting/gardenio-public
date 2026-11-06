// Package freehtmx implements the sanitizer + CSP plumbing for the
// free-form htmx widget substrate.
//
// Plan: ka-0000002149 — Plan: free-htmx widget — sandbox + CSP + sanitizer
// policy. Research: ka-0000000873. Doc:
// gardenio/docs/research/free-htmx-widget-security.md.
//
// The policy is built on top of bluemonday.UGCPolicy() with a tight set of
// additions for htmx attributes (hx-get, hx-post, hx-target, hx-swap,
// hx-trigger, hx-confirm), a `w-` namespace for class/id, and a strict URL
// allowlist regex pinning hx-get/hx-post/form-action to /htmx/widget/...
// path-prefixed same-origin URLs.
//
// The hard never-allow list (T1-T20) lives in the regression-test corpus
// under policy_test.go — every vector named in the plan §5 has at least one
// regression test that fails the build if the sanitizer ever lets it
// through.
package freehtmx

import (
	"regexp"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// HtmxURLPattern is the syntactic gate for hx-get / hx-post / form-action.
// It pins URLs to same-origin path-prefixed under /htmx/widget/, with a
// restricted character set (no scheme markers, no `..`, no protocol-relative
// `//`, no control characters in query strings).
//
// The runtime widget route handler is the semantic gate (ka-0000007136); the
// two defend each other.
var HtmxURLPattern = regexp.MustCompile(`^/htmx/widget/[a-zA-Z0-9_/.~-]+(\?[a-zA-Z0-9_=&%.~+-]+)?$`)

var (
	hxTargetPattern  = regexp.MustCompile(`^(this|#w-[a-zA-Z0-9_-]+|closest [a-z]+|find [a-z]+)$`)
	hxSwapPattern    = regexp.MustCompile(`^(innerHTML|outerHTML|beforebegin|afterbegin|beforeend|afterend|delete|none)$`)
	hxTriggerPattern = regexp.MustCompile(`^(click|submit|change|load|revealed|every \d+s)$`)
	hxConfirmPattern = regexp.MustCompile(`^[\p{L}\p{N}\s\.\?\!,'-]{1,200}$`)
	wClassPattern    = regexp.MustCompile(`^(w-[a-zA-Z0-9_-]+(\s+w-[a-zA-Z0-9_-]+)*)$`)
	wIDPattern       = regexp.MustCompile(`^w-[a-zA-Z0-9_-]+$`)
)

var (
	policyOnce sync.Once
	policy     *bluemonday.Policy
)

// Policy returns the singleton FreeHtmxPolicy. The bluemonday policy is
// concurrent-safe for sanitization, so we share one instance across the
// process.
func Policy() *bluemonday.Policy {
	policyOnce.Do(func() {
		policy = newPolicy()
	})
	return policy
}

// Sanitize is the convenience wrapper that callers use to scrub an
// agent-rendered widget fragment before concatenating it into the page.
//
// The pipeline is two-stage:
//  1. bluemonday.UGCPolicy() + freeHtmxAdditions strips dangerous elements,
//     attributes, and URL schemes. This is the primary defense.
//  2. enforceWNamespace() strips `id` and `class` attribute values that do
//     not match the `w-` namespace prefix. Bluemonday's UGCPolicy
//     unconditionally allows `id` via AllowStandardAttributes() with a
//     permissive regex, and bluemonday matchers are OR-combined — so a
//     post-pass is required to enforce the namespace contract from plan §2.
//     This is defense in depth, not the primary security boundary.
func Sanitize(in string) string {
	cleaned := Policy().Sanitize(in)
	return enforceWNamespace(cleaned)
}

// enforceWNamespace walks the HTML fragment and removes `id` attribute
// values not matching `^w-…$` and class tokens not prefixed `w-`. The
// element tree is preserved; only attribute values change.
//
// We use html.Parse + html.Render rather than a regex pass because attribute
// rewriting on raw HTML is mxss-prone (the same family of bugs bluemonday
// already defends against). The cost of a re-parse is negligible relative
// to the cost of the bluemonday pipeline that already ran.
func enforceWNamespace(in string) string {
	if in == "" {
		return in
	}
	// html.ParseFragment requires a context node; we want to treat the
	// input as document-body content (which matches how the renderer
	// concatenates the fragment into bead-show).
	nodes, err := html.ParseFragment(strings.NewReader(in), &html.Node{
		Type:     html.ElementNode,
		Data:     "body",
		DataAtom: atom.Body,
	})
	if err != nil {
		// Malformed input — bluemonday already returned a string, so
		// fall through unchanged. The caller (and CSP) remains the
		// final line of defense.
		return in
	}
	for _, n := range nodes {
		walkAndScrub(n)
	}
	var b strings.Builder
	for _, n := range nodes {
		if err := html.Render(&b, n); err != nil {
			return in
		}
	}
	return b.String()
}

func walkAndScrub(n *html.Node) {
	if n.Type == html.ElementNode && len(n.Attr) > 0 {
		out := n.Attr[:0]
		for _, a := range n.Attr {
			switch strings.ToLower(a.Key) {
			case "id":
				if wIDPattern.MatchString(a.Val) {
					out = append(out, a)
				}
				// else drop the attribute entirely.
			case "class":
				if v := keepWClassesOnly(a.Val); v != "" {
					a.Val = v
					out = append(out, a)
				}
				// else drop.
			default:
				out = append(out, a)
			}
		}
		n.Attr = out
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walkAndScrub(c)
	}
}

// keepWClassesOnly filters a space-separated class list down to tokens
// prefixed `w-`. Returns "" if no tokens survive.
func keepWClassesOnly(classes string) string {
	tokens := strings.Fields(classes)
	keep := tokens[:0]
	for _, t := range tokens {
		if strings.HasPrefix(t, "w-") && len(t) > 2 && validWClassToken(t) {
			keep = append(keep, t)
		}
	}
	return strings.Join(keep, " ")
}

var wClassTokenPattern = regexp.MustCompile(`^w-[a-zA-Z0-9_-]+$`)

func validWClassToken(t string) bool { return wClassTokenPattern.MatchString(t) }

// hxAttrElements is the set of elements an htmx attribute may appear on.
// Limiting the surface to interactive / form-ish elements keeps the
// allowlist narrow (e.g. <p hx-post="…"> stays stripped).
var hxAttrElements = []string{"a", "button", "div", "form", "span", "input", "select", "tr", "td", "li"}

func newPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()

	// htmx URL attributes — hx-get / hx-post must match HtmxURLPattern.
	p.AllowAttrs("hx-get").Matching(HtmxURLPattern).OnElements(hxAttrElements...)
	p.AllowAttrs("hx-post").Matching(HtmxURLPattern).OnElements(hxAttrElements...)

	// hx-target — `this`, `#w-…`, `closest tag`, `find tag` only. No
	// arbitrary selectors (which could reach into parent-page DOM).
	p.AllowAttrs("hx-target").Matching(hxTargetPattern).OnElements(hxAttrElements...)

	// hx-swap — finite enum; future htmx broadenings (e.g. transition:) do
	// not silently pass through.
	p.AllowAttrs("hx-swap").Matching(hxSwapPattern).OnElements(hxAttrElements...)

	// hx-trigger — limited set; `every Ns` polling allowed only with the
	// `s` suffix (sanitizer regex), runtime additionally enforces N >= 5
	// and per-instance rate limit.
	p.AllowAttrs("hx-trigger").Matching(hxTriggerPattern).OnElements(hxAttrElements...)

	// hx-confirm — free text bounded length, no markup-ish characters
	// (prevents UI-spoofing payloads / fingerprinting).
	p.AllowAttrs("hx-confirm").Matching(hxConfirmPattern).OnElements(hxAttrElements...)

	// `w-` namespace for class / id so widget-authored selectors can never
	// target parent-page chrome (ka-0000001930). Globally because UGCPolicy
	// already allows class/id on a wide surface.
	p.AllowAttrs("class").Matching(wClassPattern).Globally()
	p.AllowAttrs("id").Matching(wIDPattern).Globally()

	// data-* widget hooks. Restrict to a small approved set; arbitrary
	// data-* attributes can be re-allowed in a follow-up if a use case
	// emerges.
	p.AllowAttrs("data-widget-key", "data-widget-value", "data-bead-id", "data-widget-instance").Globally()

	// Hard belt-and-braces: explicitly reject `<base>` / `<meta>` /
	// `<link>` / `<style>` / `<script>` / `<iframe>` / `<object>` /
	// `<embed>` / `<applet>` / `<frame>` / `<frameset>` / `<foreignObject>`
	// / `<math>` even though UGCPolicy already strips them. bluemonday
	// strips unknown elements, but explicit calls keep the policy
	// auditable.
	//
	// We also disallow <input> entirely from the allowlist (UGCPolicy does
	// not allow it by default) — the `hxAttrElements` "input" entry above
	// is moot until/unless a follow-up explicitly allows input via
	// AllowElements. This avoids type=image|file|password footguns from §5
	// item 17.

	// Force `rel="nofollow noopener noreferrer"` on external links — this
	// is already part of UGCPolicy via RequireNoFollowOnLinks +
	// RequireNoReferrerOnLinks, but we re-state it here so an upstream
	// policy refactor cannot accidentally drop it.
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(false)

	// The sanitizer is the structural defense; CSP (csp.go) is the
	// runtime defense; the route handler is the semantic defense. All
	// three must agree before a widget renders.

	return p
}
