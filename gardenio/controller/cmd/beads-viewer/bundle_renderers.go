package main

// Per-format renderers for /bundle/<id> (T5 — ka-0000006897).
// Plan-of-record: gardenio/plans/agent-artifact-bundles-multi-doc-viewer.md §3.3.3.
//
// Each renderer is registered against a `format` slug at init time; the
// dispatch in buildBundleView (bundle.go) looks up by `fields.format` on the
// type=doc child bead and falls back to the placeholder card in the template
// when no renderer is registered.
//
// Renderers reuse the existing infrastructure rather than re-implementing it:
//   - md     → s.md (goldmark) + LinkifyHTML (same path as bead descriptions).
//   - diff   → parseUnifiedDiff (diff_parser.go) + chroma per line.
//   - csv    → encoding/csv → <table>; cap on rows, over-cap → escaped <pre>.
//   - code   → codeHighlighter.highlight (chroma).
//   - image  → host-whitelisted <img>; non-whitelisted host degrades to link card.
//   - link   → escaped title + URL card; never embeds, never fetches.
//   - html   → sandboxed <iframe srcdoc>; XSS-safe by construction (ka-0000001859).
//
// Security: csv/code/diff/link all HTML-escape user content before assembling
// HTML. The image renderer rejects URLs whose host is not on the whitelist
// (env-extensible via GARDENIO_BUNDLE_IMAGE_HOSTS) so a malicious doc can't
// turn the bundle viewer into an open <img src="…"> proxy. The html renderer
// emits author content into an <iframe sandbox srcdoc="…"> with no
// allow-scripts / allow-same-origin tokens — the sandbox is the entire
// defense, do not loosen it without a security review. Do NOT enable goldmark
// html.WithUnsafe() on the md path; that would silently widen XSS surface
// for every existing markdown doc.

import (
	"encoding/csv"
	"fmt"
	"html/template"
	"net/url"
	"os"
	"strings"

	"github.com/yuin/goldmark"
	xhtml "golang.org/x/net/html"
)

// bundleCSVRowCap caps the number of CSV rows we render as a real <table>
// (header + body). Past this cap the renderer degrades to an escaped <pre>
// dump — a human reader is going to scroll past 5000 rows anyway, and the
// table layout starts to hurt page load. Tune as needed; tests target this
// constant directly.
const bundleCSVRowCap = 5000

func init() {
	registerBundleRenderer("md", renderBundleMarkdown)
	registerBundleRenderer("diff", renderBundleDiff)
	registerBundleRenderer("csv", renderBundleCSV)
	registerBundleRenderer("code", renderBundleCode)
	registerBundleRenderer("image", renderBundleImage)
	registerBundleRenderer("link", renderBundleLink)
	registerBundleRenderer("html", renderBundleHTML)
}

// renderBundleMarkdown renders fields.content as goldmark HTML, then runs the
// result through LinkifyHTML so bare bead IDs become anchors. Mirrors the
// description-render path on bead-show. Empty content → empty output.
func renderBundleMarkdown(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.Content == "" {
		return "", nil
	}
	var md goldmark.Markdown
	var basePath string
	if ctx != nil {
		md = ctx.md
		basePath = ctx.basePath
	}
	if md == nil {
		// Defensive: tests may pass a nil ctx. Escape and wrap so we still
		// emit safe HTML rather than panic.
		return template.HTML("<p>" + template.HTMLEscapeString(card.Content) + "</p>"), nil
	}
	var buf strings.Builder
	if err := md.Convert([]byte(card.Content), &buf); err != nil {
		// Goldmark failed on author-supplied markdown — degrade to escaped
		// plain text rather than fail the whole bundle render.
		return template.HTML("<p>" + template.HTMLEscapeString(card.Content) + "</p>"), nil
	}
	// Goldmark renders `![alt](url)` to `<img src="url" alt="alt">` without
	// applying any host whitelist — same as a raw markdown renderer would.
	// Pass the output through filterMarkdownImages so inline images respect
	// the same image-host whitelist as the dedicated image-card renderer.
	filtered := filterMarkdownImages(ctx, buf.String())
	return LinkifyHTML(filtered, basePath), nil
}

// filterMarkdownImages walks goldmark-rendered HTML and rewrites every
// `<img>` tag based on the image-host whitelist. Whitelisted hosts get
// safe-by-default attributes (`loading="lazy"`, `referrerpolicy="no-referrer"`)
// to mirror the dedicated image-card renderer. Non-whitelisted (or unparseable)
// URLs degrade to a visible inline link so the doc surfaces a clickable
// reference instead of silently dropping the image. Non-`<img>` tokens are
// written through unmodified.
//
// We tokenize rather than regex-match goldmark's output so attribute order
// and quoting variations stay safe (goldmark may add a `title` attribute
// when the markdown supplies one).
func filterMarkdownImages(ctx *bundleRenderCtx, input string) string {
	if !strings.Contains(input, "<img") {
		return input
	}
	z := xhtml.NewTokenizer(strings.NewReader(input))
	var buf strings.Builder
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			return buf.String()
		}
		if tt == xhtml.StartTagToken || tt == xhtml.SelfClosingTagToken {
			name, hasAttr := z.TagName()
			if string(name) == "img" {
				buf.WriteString(rewriteMarkdownImage(ctx, z, hasAttr))
				continue
			}
		}
		buf.Write(z.Raw())
	}
}

// rewriteMarkdownImage consumes the attributes of an `<img>` tag from the
// tokenizer and emits either a sanitized `<img>` (whitelisted host) or an
// `<a class="bundle-md-image-fallback">` link card (everything else). The
// fallback label prefers the original alt text; on empty alt we fall back to
// the URL so the link is never empty.
func rewriteMarkdownImage(ctx *bundleRenderCtx, z *xhtml.Tokenizer, hasAttr bool) string {
	var src, alt, title string
	if hasAttr {
		for {
			k, v, more := z.TagAttr()
			switch string(k) {
			case "src":
				src = string(v)
			case "alt":
				alt = string(v)
			case "title":
				title = string(v)
			}
			if !more {
				break
			}
		}
	}
	// ka-0000006131: rewrite gardenio-sessions S3 URLs to the /image/* proxy
	// route before the host whitelist runs. A rewritten URL is same-origin
	// and pre-authorised (the handler re-checks the key prefix), so we skip
	// the whitelist branch and emit a direct <img>.
	if proxied := rewriteSessionImageURL(ctx, src); proxied != "" {
		return renderProxiedMarkdownImg(proxied, alt, title)
	}
	// A relative or root-relative image src (no scheme + no host) is never
	// servable by the viewer: the browser resolves it against the bundle page
	// URL (e.g. /beads/bundle/feed-mockups/x.png) and 404s. The old fallback
	// emitted a clickable <a href="feed-mockups/x.png"> that *looked* fine but
	// dead-links — a deceptive 404 (ka-0000005491). Authors who write a
	// repo-relative path mistakenly assume the viewer serves files committed to
	// the repo; it does not. Surface a visibly-broken placeholder naming the
	// offending path so the mistake is caught at preview time, and point at the
	// correct path (gb image upload → absolute /image/* proxy URL).
	if relativeImageSrc(src) {
		return renderBrokenImagePlaceholder(src, alt)
	}
	u, err := url.Parse(src)
	if err != nil || u.Scheme == "" || u.Host == "" || !isWhitelistedImageHost(ctx, u.Host) {
		label := alt
		if label == "" {
			label = src
		}
		if label == "" {
			// No src, no alt — nothing useful to surface; drop the tag.
			return ""
		}
		return `<a class="bundle-md-image-fallback" href="` +
			template.HTMLEscapeString(src) +
			`" rel="noopener noreferrer">` +
			template.HTMLEscapeString(label) + `</a>`
	}
	out := `<img class="bundle-md-image" src="` + template.HTMLEscapeString(src) +
		`" alt="` + template.HTMLEscapeString(alt) + `"`
	if title != "" {
		out += ` title="` + template.HTMLEscapeString(title) + `"`
	}
	out += ` loading="lazy" referrerpolicy="no-referrer">`
	return out
}

// relativeImageSrc reports whether an <img> src is a relative or root-relative
// path that the viewer cannot serve — i.e. it has no scheme and no host, so the
// browser would resolve it against the current bundle page URL and 404. This
// catches the common author mistake of embedding a repo-committed file by its
// repo path (e.g. `feed-mockups/x.png` or `/docs/feed-mockups/x.png`); the
// viewer is not a file server for the repo, so such paths are always dead.
//
// Same-origin proxy URLs produced by rewriteSessionImageURL (`/image/<key>` or
// `<basePath>/image/<key>`) are intentionally NOT considered relative here:
// they are rewritten to a servable route before this function is reached, so by
// construction any path that survives to this point is not a proxy URL.
//
// Data URIs (`data:image/...`) and protocol-relative URLs (`//host/...`) are
// handled by url.Parse downstream — a data URI has a scheme, and a
// protocol-relative URL has a host — so neither is misclassified here.
func relativeImageSrc(src string) bool {
	src = strings.TrimSpace(src)
	if src == "" {
		return false // empty src is dropped by the caller, not a broken path
	}
	if strings.HasPrefix(src, "//") {
		return false // protocol-relative URL — has a host, parsed downstream
	}
	u, err := url.Parse(src)
	if err != nil {
		return false // unparseable — let the downstream fallback handle it
	}
	return u.Scheme == "" && u.Host == ""
}

// renderBrokenImagePlaceholder emits a self-evidently-broken inline marker for
// a non-servable relative image path. It names the offending src (and alt, when
// present) so an author previewing the bundle sees exactly which embed is dead
// and why, instead of a link that 404s only when clicked. Deliberately NOT an
// <img> or <a>: there is nothing valid to fetch or navigate to.
func renderBrokenImagePlaceholder(src, alt string) string {
	label := alt
	if label == "" {
		label = "image"
	}
	return `<span class="bundle-md-image-broken" role="img" aria-label="broken image: ` +
		template.HTMLEscapeString(label) + `">` +
		`⚠ broken image — relative path not servable by the viewer: ` +
		`<code>` + template.HTMLEscapeString(src) + `</code>` +
		` (upload via <code>gb image upload</code> and use the returned absolute URL)` +
		`</span>`
}

// renderBundleDiff parses fields.content as a unified diff and renders each
// hunk with chroma per-line highlighting. Malformed input (no diff headers)
// degrades to an HTML-escaped <pre> so the doc still surfaces — the bundle
// is a reading view, not a parser-correctness gate.
func renderBundleDiff(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.Content == "" {
		return "", nil
	}
	files := parseUnifiedDiff(card.Content)
	if len(files) == 0 {
		// Degraded: the parser found no `diff --git` / `--- ` headers. Emit
		// the raw text so the reader can still see what was attached.
		return template.HTML(`<pre class="bundle-diff-raw">` + template.HTMLEscapeString(card.Content) + `</pre>`), nil
	}

	var buf strings.Builder
	buf.WriteString(`<div class="bundle-diff">`)
	if row := renderBundleSourceMRRow(ctx); row != "" {
		buf.WriteString(row)
	}
	for _, f := range files {
		path := f.NewPath
		if path == "" {
			path = f.OldPath
		}
		lang := f.Language
		if lang == "" {
			lang = card.Language
		}
		fmt.Fprintf(&buf,
			`<div class="bundle-diff-file" data-language="%s"><div class="bundle-diff-file-header"><span class="path">%s</span> <span class="counts"><span class="adds">+%d</span> <span class="dels">-%d</span></span></div>`,
			template.HTMLEscapeString(lang),
			template.HTMLEscapeString(path),
			f.Adds, f.Dels,
		)
		for _, h := range f.Hunks {
			fmt.Fprintf(&buf,
				`<pre class="bundle-diff-hunk"><span class="hunk-header">%s</span>`,
				template.HTMLEscapeString(h.Header),
			)
			for _, ln := range h.Lines {
				marker := " "
				switch ln.Kind {
				case "add":
					marker = "+"
				case "del":
					marker = "-"
				}
				rendered := bundleHighlightDiffLine(ctx, lang, ln.Text)
				fmt.Fprintf(&buf,
					`<span class="line line-%s">%s%s</span>`+"\n",
					template.HTMLEscapeString(ln.Kind),
					marker,
					rendered,
				)
			}
			buf.WriteString(`</pre>`)
		}
		buf.WriteString(`</div>`)
	}
	buf.WriteString(`</div>`)
	return template.HTML(buf.String()), nil
}

// renderBundleSourceMRRow emits the "Source: !iid · state" pill that links a
// bundle diff card back to the producing MR (ka-0000006690). buildBundleView
// stamps ctx.SourceMR by reverse-walking `produces` edges; renderers call
// this once per card to surface the linkage. Returns "" when ctx (or its
// SourceMR / mr_url) is unset so bundles without an originating MR render
// identically to before.
func renderBundleSourceMRRow(ctx *bundleRenderCtx) string {
	if ctx == nil || ctx.SourceMR == nil {
		return ""
	}
	mr := ctx.SourceMR
	if mr.MRURL == "" {
		return ""
	}
	label := "!"
	if mr.MRIID != "" {
		label += mr.MRIID
	} else {
		label += "?"
	}
	if mr.MRState != "" {
		label += " · " + mr.MRState
	}
	stateClass := mr.MRState
	if stateClass == "" {
		stateClass = "unknown"
	}
	return `<div class="bundle-diff-source"><span class="bundle-diff-source-label">Source:</span> <a class="bundle-diff-source-link bundle-diff-source-state-` +
		template.HTMLEscapeString(stateClass) +
		`" href="` +
		template.HTMLEscapeString(mr.MRURL) +
		`" rel="noopener noreferrer">` +
		template.HTMLEscapeString(label) +
		`</a></div>`
}

// bundleHighlightDiffLine runs a single diff line through the chroma
// highlighter when available; otherwise hand-escapes. Mirrors
// highlightDiffLine in widgets_phase6.go so a bundle-rendered diff and a
// widget-rendered diff produce the same visual output for the same input.
func bundleHighlightDiffLine(ctx *bundleRenderCtx, language, text string) template.HTML {
	if text == "" {
		return ""
	}
	if ctx == nil || ctx.hl == nil {
		return template.HTML(template.HTMLEscapeString(text))
	}
	wrapped := string(ctx.hl.highlight(language, text))
	return template.HTML(stripChromaWrap(wrapped))
}

// renderBundleCSV parses fields.content with encoding/csv and emits a
// <table> with the first row treated as <th>. Malformed input or row counts
// past bundleCSVRowCap degrade to an HTML-escaped <pre> so the doc still
// renders something readable.
func renderBundleCSV(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.Content == "" {
		return "", nil
	}
	r := csv.NewReader(strings.NewReader(card.Content))
	// Tolerate ragged CSVs — author-supplied data is rarely perfectly
	// rectangular and we'd rather show what we can than fail the page.
	r.FieldsPerRecord = -1
	rows, err := r.ReadAll()
	if err != nil || len(rows) == 0 {
		return template.HTML(`<pre class="bundle-csv-raw">` + template.HTMLEscapeString(card.Content) + `</pre>`), nil
	}
	if len(rows) > bundleCSVRowCap {
		// Over-cap: fall back to the raw text so the reader can still inspect
		// the first few KB without us pumping a 100k-row table into the DOM.
		return template.HTML(`<pre class="bundle-csv-raw" data-overcap="1">` + template.HTMLEscapeString(card.Content) + `</pre>`), nil
	}

	var buf strings.Builder
	buf.WriteString(`<table class="bundle-csv"><thead><tr>`)
	for _, h := range rows[0] {
		fmt.Fprintf(&buf, `<th>%s</th>`, template.HTMLEscapeString(h))
	}
	buf.WriteString(`</tr></thead><tbody>`)
	for _, row := range rows[1:] {
		buf.WriteString(`<tr>`)
		for _, c := range row {
			fmt.Fprintf(&buf, `<td>%s</td>`, template.HTMLEscapeString(c))
		}
		buf.WriteString(`</tr>`)
	}
	buf.WriteString(`</tbody></table>`)
	return template.HTML(buf.String()), nil
}

// renderBundleCode pipes fields.content through the chroma highlighter,
// language-keyed by fields.language. Empty content → empty output. Without
// a highlighter (test contexts) or for unknown languages we degrade to an
// escaped <pre> — chroma already does this internally too.
func renderBundleCode(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.Content == "" {
		return "", nil
	}
	if ctx == nil || ctx.hl == nil {
		return template.HTML(`<pre class="bundle-code">` + template.HTMLEscapeString(card.Content) + `</pre>`), nil
	}
	return ctx.hl.highlight(card.Language, card.Content), nil
}

// renderBundleImage emits an <img> only when the URL host is on the
// whitelist (env-extensible). Non-whitelisted hosts and unparseable URLs
// degrade to a link card so the doc surfaces a clickable reference rather
// than silently failing or, worse, fetching arbitrary external bytes.
func renderBundleImage(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.ContentURL == "" {
		return template.HTML(`<p class="bundle-image-empty empty">No image URL on this doc.</p>`), nil
	}
	// ka-0000006131: gardenio-sessions S3 URLs get rewritten to the /image/*
	// proxy so they stream through viewerOnly rather than relying on bucket
	// ACLs. Done before url.Parse / whitelist so the whitelist stays as
	// pure defense-in-depth for non-S3 hosts.
	if proxied := rewriteSessionImageURL(ctx, card.ContentURL); proxied != "" {
		src := template.HTMLEscapeString(proxied)
		alt := template.HTMLEscapeString(card.Headline)
		return template.HTML(`<img class="bundle-image" src="` + src + `" alt="` + alt + `" loading="lazy" referrerpolicy="no-referrer">`), nil
	}
	u, err := url.Parse(card.ContentURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		// Not a usable URL — degrade to the link card path so the bundle
		// still surfaces something for the reader.
		return renderBundleLink(card, ctx)
	}
	if !isWhitelistedImageHost(ctx, u.Host) {
		// Whitelist miss: the plan explicitly forbids embedding arbitrary
		// hosts. Fall through to the link card so the doc remains visible
		// but unfetched.
		return renderBundleLink(card, ctx)
	}
	src := template.HTMLEscapeString(card.ContentURL)
	alt := template.HTMLEscapeString(card.Headline)
	return template.HTML(`<img class="bundle-image" src="` + src + `" alt="` + alt + `" loading="lazy" referrerpolicy="no-referrer">`), nil
}

// renderBundleLink emits a card with the doc's headline and content URL.
// Never embeds, never fetches — used for "this doc is over there" pointers
// (MR descriptions, Confluence pages, …) and as the degraded path for the
// image renderer when the host isn't on the whitelist.
func renderBundleLink(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.ContentURL == "" {
		return template.HTML(`<p class="bundle-link-empty empty">No URL on this doc.</p>`), nil
	}
	href := template.HTMLEscapeString(card.ContentURL)
	title := card.Headline
	if title == "" {
		title = card.ContentURL
	}
	label := template.HTMLEscapeString(title)
	urlText := template.HTMLEscapeString(card.ContentURL)
	return template.HTML(
		`<a class="bundle-link-card" href="` + href + `" rel="noopener noreferrer">` +
			`<span class="bundle-link-title">` + label + `</span> ` +
			`<code class="bundle-link-url">` + urlText + `</code></a>`,
	), nil
}

// bundleHTMLDefaultMinHeight is the inline min-height (in px) used for the
// sandboxed iframe before bundle_html.js auto-sizes it to its content. It is a
// floor for the no-JS / pre-load case (and the thin-sliver guard), not a cap —
// the resize helper grows the frame to its document's scrollHeight on load so
// the page scrolls as one document with no inner scrollbar (ka-0000004212).
// Tests target this constant directly.
const bundleHTMLDefaultMinHeight = 720

// bundleHTMLFrameBase is prepended to the framed document so links inside the
// bundle navigate the WHOLE page instead of reloading the frame. Without it a
// link in the (target=_self by default) frame loads inside the iframe — a
// dead-end the user reported (ka-0000001462). `<base target="_top">` makes
// every in-frame <a>/<form> default to the top browsing context; the
// allow-top-navigation-by-user-activation sandbox token (below) is what lets
// that navigation actually reach the top frame. It sets only `target`, not
// `href`, so it does not change how relative URLs (or the preview popover's
// a.href reads in bundle_html.js) resolve.
const bundleHTMLFrameBase = `<base target="_top">`

// renderBundleHTML emits fields.content as a sandboxed <iframe srcdoc="…">.
//
// The sandbox carries exactly TWO tokens: allow-same-origin and
// allow-top-navigation-by-user-activation. It deliberately does NOT carry
// allow-scripts, so author HTML can lay itself out and load its own inline
// <style> but CANNOT run JavaScript or submit forms. No-allow-scripts is the
// entire XSS defense — DO NOT add allow-scripts here. Adding it alongside
// allow-same-origin would let embedded script remove its own sandbox attribute
// and escape; keeping scripts off means no code ever runs inside the frame, so
// there is nothing to exploit the same-origin grant.
//
// allow-top-navigation-by-user-activation lets a link the user actually CLICKS
// navigate the whole page (paired with the injected `<base target="_top">`).
// The "-by-user-activation" qualifier is load-bearing: navigation only happens
// in response to a genuine user gesture, and since no script runs in the frame
// there is no way to trigger a top-frame navigation programmatically. We must
// NOT use the unrestricted allow-top-navigation token, which would let content
// redirect the top frame without a click.
//
// allow-same-origin exists so the PARENT page can read the frame's
// contentDocument (same-origin) to (a) auto-size the iframe to its content,
// killing the inner scrollbar, and (b) bind the bead-link hover popover onto
// links inside the frame. Both run in parent JS (bundle_html.js); the frame
// itself stays script-free. See plans/daily-bundle-productionization.md.
//
// Author content is HTML-escaped before being placed in the srcdoc attribute
// (browsers decode srcdoc once when parsing the iframe document, so a single
// escape is correct — the embedded document itself receives the original
// markup). The `<base target="_top">` prefix rides through the same single
// escape so the framed document receives a real <base> element. Empty content
// → empty output, mirroring the other renderers.
func renderBundleHTML(card bundleDocCard, ctx *bundleRenderCtx) (template.HTML, error) {
	if card.Content == "" {
		return "", nil
	}
	srcdoc := template.HTMLEscapeString(bundleHTMLFrameBase + card.Content)
	return template.HTML(fmt.Sprintf(
		`<iframe class="bundle-html" sandbox="allow-same-origin allow-top-navigation-by-user-activation" srcdoc="%s" loading="lazy" referrerpolicy="no-referrer" style="width:100%%;min-height:%dpx;border:1px solid var(--line,#e3e6ea);border-radius:4px;background:#fff;"></iframe>`,
		srcdoc, bundleHTMLDefaultMinHeight,
	)), nil
}

// bundleImageHostWhitelist returns the per-process whitelist of hosts whose
// URLs may render as <img>. The default set is intentionally narrow —
// `s3.amazonaws.com` (path-style coop transcript bucket); the gardenio
// origin host and any virtual-hosted S3 bucket should be added via the
// GARDENIO_BUNDLE_IMAGE_HOSTS env var (comma-separated).
//
// Read once per ctx build (newBundleRenderCtx) — env changes require a
// process restart, which is intentional: the whitelist is security-load-
// bearing and we don't want hot-reload races to widen it.
func bundleImageHostWhitelist() map[string]struct{} {
	out := map[string]struct{}{
		"s3.amazonaws.com": {},
	}
	if v := os.Getenv("GARDENIO_BUNDLE_IMAGE_HOSTS"); v != "" {
		for h := range strings.SplitSeq(v, ",") {
			h = strings.TrimSpace(strings.ToLower(h))
			if h != "" {
				out[h] = struct{}{}
			}
		}
	}
	return out
}

// renderProxiedMarkdownImg emits an `<img>` tag for a same-origin proxy URL
// (e.g. `/image/<key>`). Mirrors the whitelisted-host branch in
// rewriteMarkdownImage so a rewritten URL still gets `loading="lazy"` and
// `referrerpolicy="no-referrer"`. Used by the ka-0000006131 proxy rewrite.
func renderProxiedMarkdownImg(src, alt, title string) string {
	out := `<img class="bundle-md-image" src="` + template.HTMLEscapeString(src) +
		`" alt="` + template.HTMLEscapeString(alt) + `"`
	if title != "" {
		out += ` title="` + template.HTMLEscapeString(title) + `"`
	}
	out += ` loading="lazy" referrerpolicy="no-referrer">`
	return out
}

// rewriteSessionImageURL maps a gardenio-sessions S3 URL (virtual-hosted or
// path-style) to a same-origin `<basePath>/image/<encoded-key>` proxy path.
// Returns the rewritten URL on success, or "" when the input doesn't look like
// a session-bucket URL we can serve through the proxy. Implements step 2 of
// ka-0000006131 — keep the URL shape stable so a later CloudFront swap doesn't
// churn already-rendered HTML.
//
// Recognized URL shapes (case-insensitive on host):
//   - https://<bucket>.s3.amazonaws.com/<key>
//   - https://<bucket>.s3.<region>.amazonaws.com/<key>
//   - https://s3.amazonaws.com/<bucket>/<key>
//   - https://s3.<region>.amazonaws.com/<bucket>/<key>
//
// The key must live under `<imagePrefix>/images/` (default
// `coop/sessions/images/`) — same authz the proxy handler enforces. Mismatched
// bucket / out-of-prefix keys fall through to the existing whitelist path.
func rewriteSessionImageURL(ctx *bundleRenderCtx, raw string) string {
	if ctx == nil || ctx.imageBucket == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	if out := rewriteBucketImageURL(ctx, u, ctx.imageBucket); out != "" {
		return out
	}
	if ctx.imageFallbackBucket != "" {
		return rewriteBucketImageURL(ctx, u, ctx.imageFallbackBucket)
	}
	return ""
}

// rewriteBucketImageURL is rewriteSessionImageURL for one bucket.
func rewriteBucketImageURL(ctx *bundleRenderCtx, u *url.URL, bucket string) string {
	host := strings.ToLower(u.Host)
	bucket = strings.ToLower(bucket)
	// EscapedPath preserves percent-encoding from the source URL; we
	// PathUnescape below so the prefix check sees raw bytes.
	rawPath := strings.TrimPrefix(u.EscapedPath(), "/")
	var encodedKey string
	switch {
	case host == bucket+".s3.amazonaws.com":
		encodedKey = rawPath
	case strings.HasPrefix(host, bucket+".s3.") && strings.HasSuffix(host, ".amazonaws.com"):
		encodedKey = rawPath
	case host == "s3.amazonaws.com" || (strings.HasPrefix(host, "s3.") && strings.HasSuffix(host, ".amazonaws.com")):
		// Path-style: first path segment is the bucket.
		prefix := bucket + "/"
		if !strings.HasPrefix(rawPath, prefix) {
			return ""
		}
		encodedKey = strings.TrimPrefix(rawPath, prefix)
	default:
		return ""
	}
	if encodedKey == "" {
		return ""
	}
	decodedKey, err := url.PathUnescape(encodedKey)
	if err != nil {
		return ""
	}
	prefix := imageProxyAllowedPrefix(ctx.imagePrefix)
	if !strings.HasPrefix(decodedKey, prefix) {
		return ""
	}
	return ctx.basePath + "/image/" + encodeImageProxyKey(decodedKey)
}

// imageProxyAllowedPrefix returns the path prefix the /image/* proxy will
// serve, with a guaranteed trailing slash. Mirrors the handler-side check —
// kept here so the renderer and the handler agree on what is proxyable.
func imageProxyAllowedPrefix(imagePrefix string) string {
	p := strings.TrimRight(imagePrefix, "/")
	if p == "" {
		p = "coop/sessions"
	}
	return p + "/images/"
}

// encodeImageProxyKey percent-encodes each `/`-delimited segment of an S3
// key for safe inclusion in a URL path. Mirrors gb image upload's
// encodeS3Key (gb/image.go:236) so the rendered URL is symmetric with the
// upload-time URL shape.
func encodeImageProxyKey(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// isWhitelistedImageHost compares host against ctx.imageHosts. Exact match
// or prefix-leading-dot suffix match (e.g. ".s3.amazonaws.com" matches
// "bucket.s3.amazonaws.com"). When ctx is nil or its whitelist is unset
// the answer is always false — fail closed.
func isWhitelistedImageHost(ctx *bundleRenderCtx, host string) bool {
	if ctx == nil || len(ctx.imageHosts) == 0 {
		return false
	}
	host = strings.ToLower(host)
	if _, ok := ctx.imageHosts[host]; ok {
		return true
	}
	for h := range ctx.imageHosts {
		if strings.HasPrefix(h, ".") && strings.HasSuffix(host, h) {
			return true
		}
	}
	return false
}
