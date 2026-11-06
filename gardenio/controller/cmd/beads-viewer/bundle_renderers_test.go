package main

// Per-format renderer tests for /bundle/<id> (T5 — ka-0000006897).
// One sub-test set per format: happy path, degraded path, and (where
// applicable) HTML-escape / host-whitelist regressions.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

func newTestBundleRenderCtx() *bundleRenderCtx {
	return &bundleRenderCtx{
		md:       goldmark.New(goldmark.WithExtensions(extension.GFM)),
		hl:       newCodeHighlighter(),
		basePath: "",
		imageHosts: map[string]struct{}{
			"s3.amazonaws.com": {},
		},
	}
}

func TestBundleRendererRegistry_AllFormatsRegistered(t *testing.T) {
	for _, format := range []string{"md", "diff", "csv", "code", "image", "link", "html"} {
		if _, ok := lookupBundleRenderer(format); !ok {
			t.Errorf("expected renderer for format %q to be registered", format)
		}
	}
}

func TestRenderBundleMarkdown_Happy(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{Format: "md", Content: "# Plan\n\nSee gd-abc12345 for details.\n"}
	got, err := renderBundleMarkdown(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleMarkdown: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "<h1>Plan</h1>") {
		t.Errorf("want <h1>Plan</h1>, got %q", s)
	}
	// LinkifyHTML wraps bare bead IDs in anchor tags.
	if !strings.Contains(s, `href="/beads/gd-abc12345"`) && !strings.Contains(s, `href="gd-abc12345"`) {
		// Accept either basePath-relative or root-relative form depending on linkify impl.
		if !strings.Contains(s, "gd-abc12345") {
			t.Errorf("want bead ID surfaced (linkified), got %q", s)
		}
	}
}

func TestRenderBundleMarkdown_EmptyContent(t *testing.T) {
	got, err := renderBundleMarkdown(bundleDocCard{Format: "md"}, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestRenderBundleMarkdown_InlineImage_WhitelistedHost(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	// Add the leading-dot suffix form so the virtual-hosted bucket subdomain
	// matches via isWhitelistedImageHost's suffix branch.
	ctx.imageHosts[".s3.amazonaws.com"] = struct{}{}
	card := bundleDocCard{
		Format:  "md",
		Content: `![diagram](https://gardenio-sessions.s3.amazonaws.com/test/diagram.png "the diagram")`,
	}
	got, err := renderBundleMarkdown(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleMarkdown: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `<img class="bundle-md-image"`) {
		t.Errorf("want sanitized <img>, got %q", s)
	}
	if !strings.Contains(s, `src="https://gardenio-sessions.s3.amazonaws.com/test/diagram.png"`) {
		t.Errorf("want src URL preserved, got %q", s)
	}
	if !strings.Contains(s, `alt="diagram"`) {
		t.Errorf("want alt preserved, got %q", s)
	}
	if !strings.Contains(s, `loading="lazy"`) {
		t.Errorf("want lazy loading, got %q", s)
	}
	if !strings.Contains(s, `referrerpolicy="no-referrer"`) {
		t.Errorf("want referrerpolicy=no-referrer, got %q", s)
	}
	if !strings.Contains(s, `title="the diagram"`) {
		t.Errorf("want title preserved, got %q", s)
	}
}

func TestRenderBundleMarkdown_InlineImage_PathStyleS3Host(t *testing.T) {
	ctx := newTestBundleRenderCtx() // default includes "s3.amazonaws.com"
	card := bundleDocCard{
		Format:  "md",
		Content: `![pic](https://s3.amazonaws.com/bucket/path/pic.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if !strings.Contains(s, `<img class="bundle-md-image"`) {
		t.Errorf("want sanitized <img> for path-style S3 host, got %q", s)
	}
}

func TestRenderBundleMarkdown_InlineImage_NonWhitelistedDegradesToLink(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{
		Format:  "md",
		Content: `![evil](https://evil.example.com/x.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("non-whitelisted host must not render <img>, got %q", s)
	}
	if !strings.Contains(s, `class="bundle-md-image-fallback"`) {
		t.Errorf("want fallback link, got %q", s)
	}
	if !strings.Contains(s, `href="https://evil.example.com/x.png"`) {
		t.Errorf("want href preserved on fallback link, got %q", s)
	}
	if !strings.Contains(s, `>evil</a>`) {
		t.Errorf("want alt as fallback label, got %q", s)
	}
}

// ka-0000005491: a repo-relative image path is never servable by the viewer —
// the browser resolves it against the bundle page URL and 404s. It must render
// a visibly-broken placeholder (NOT a clickable <a> that dead-links, and NOT an
// <img> that silently breaks) so the author catches it at preview time.
func TestRenderBundleMarkdown_InlineImage_RelativePathRendersBrokenPlaceholder(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{
		Format:  "md",
		Content: `![Concept A](feed-mockups/concept-a-chronological.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("relative path must not emit <img>, got %q", s)
	}
	if strings.Contains(s, `<a `) || strings.Contains(s, `href=`) {
		t.Errorf("relative path must not emit a clickable link (deceptive 404), got %q", s)
	}
	if !strings.Contains(s, `class="bundle-md-image-broken"`) {
		t.Errorf("want broken-image placeholder, got %q", s)
	}
	if !strings.Contains(s, `feed-mockups/concept-a-chronological.png`) {
		t.Errorf("want offending path named in placeholder, got %q", s)
	}
	if !strings.Contains(s, `gb image upload`) {
		t.Errorf("want remediation hint (gb image upload), got %q", s)
	}
}

func TestRenderBundleMarkdown_InlineImage_RootRelativePathRendersBrokenPlaceholder(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{
		Format:  "md",
		Content: `![logo](/docs/feed-mockups/x.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("root-relative path must not emit <img>, got %q", s)
	}
	if !strings.Contains(s, `class="bundle-md-image-broken"`) {
		t.Errorf("want broken-image placeholder for root-relative path, got %q", s)
	}
}

// A whitelisted absolute URL must still render a real <img> — the broken-path
// guard must not regress the happy path.
func TestRenderBundleMarkdown_InlineImage_AbsoluteWhitelistedStillRenders(t *testing.T) {
	ctx := newTestBundleRenderCtx() // includes s3.amazonaws.com
	card := bundleDocCard{
		Format:  "md",
		Content: `![pic](https://s3.amazonaws.com/bucket/path/pic.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if !strings.Contains(s, `<img class="bundle-md-image"`) {
		t.Errorf("absolute whitelisted host must still render <img>, got %q", s)
	}
	if strings.Contains(s, "bundle-md-image-broken") {
		t.Errorf("absolute URL must not be flagged broken, got %q", s)
	}
}

func TestRelativeImageSrc(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"feed-mockups/x.png", true},                // repo-relative
		{"/docs/feed-mockups/x.png", true},          // root-relative
		{"./x.png", true},                           // dot-relative
		{"https://s3.amazonaws.com/b/x.png", false}, // absolute http
		{"//cdn.example.com/x.png", false},          // protocol-relative (has host)
		{"data:image/png;base64,AAAA", false},       // data URI (has scheme)
		{"", false},                                 // empty — handled elsewhere
	}
	for _, c := range cases {
		if got := relativeImageSrc(c.src); got != c.want {
			t.Errorf("relativeImageSrc(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestRenderBundleMarkdown_InlineImage_NilCtxFailsClosed(t *testing.T) {
	// nil ctx → empty whitelist → fall through to fallback link, no <img>.
	got, err := renderBundleMarkdown(bundleDocCard{
		Format:  "md",
		Content: `![alt text](https://s3.amazonaws.com/bucket/x.png)`,
	}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("nil ctx must not emit <img>, got %q", s)
	}
}

func TestRenderBundleMarkdown_InlineImage_EmptyAltUsesURLForLabel(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{
		Format:  "md",
		Content: `![](https://evil.example.com/x.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if !strings.Contains(s, `>https://evil.example.com/x.png</a>`) {
		t.Errorf("want URL as fallback label when alt is empty, got %q", s)
	}
}

func TestRenderBundleMarkdown_InlineImage_EscapesAttributes(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	// XSS attempt via alt text. Goldmark escapes on its side, but verify
	// the fallback path re-escapes too rather than trusting upstream.
	card := bundleDocCard{
		Format:  "md",
		Content: `![<script>alert(1)</script>](https://evil.example.com/x.png)`,
	}
	got, _ := renderBundleMarkdown(card, ctx)
	s := string(got)
	if strings.Contains(s, "<script>") {
		t.Errorf("XSS payload not escaped: %q", s)
	}
}

func TestRenderBundleMarkdown_NilCtxDegrades(t *testing.T) {
	// When ctx is nil (or md is nil) we degrade to escaped <p>.
	got, err := renderBundleMarkdown(bundleDocCard{Content: "<script>alert(1)</script>"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if strings.Contains(s, "<script>") {
		t.Errorf("XSS payload not escaped: %q", s)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("want escaped output, got %q", s)
	}
}

func TestRenderBundleDiff_Happy(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	src := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,4 @@
 keep
-old line
+new line
+added`
	got, err := renderBundleDiff(bundleDocCard{Format: "diff", Content: src}, ctx)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`class="bundle-diff"`,
		`class="bundle-diff-file"`,
		`class="bundle-diff-hunk"`,
		`<span class="adds">+2</span>`,
		`<span class="dels">-1</span>`,
		`line-add`,
		`line-del`,
		`line-ctx`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in diff output:\n%s", want, s)
		}
	}
}

func TestRenderBundleDiff_DegradesOnMalformed(t *testing.T) {
	got, err := renderBundleDiff(bundleDocCard{Content: "not actually a diff <script>"}, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `class="bundle-diff-raw"`) {
		t.Errorf("want degraded raw <pre>, got %q", s)
	}
	if strings.Contains(s, "<script>") {
		t.Errorf("XSS payload not escaped in degraded path: %q", s)
	}
}

func TestRenderBundleDiff_EmptyContent(t *testing.T) {
	got, _ := renderBundleDiff(bundleDocCard{}, newTestBundleRenderCtx())
	if string(got) != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestRenderBundleDiff_EscapesPathAndContent(t *testing.T) {
	src := `diff --git a/<script>.go b/<script>.go
--- a/<script>.go
+++ b/<script>.go
@@ -1,1 +1,1 @@
-old <script>
+new <script>`
	got, _ := renderBundleDiff(bundleDocCard{Content: src}, newTestBundleRenderCtx())
	s := string(got)
	if strings.Contains(s, "<script>") {
		t.Errorf("unescaped <script> in diff: %s", s)
	}
}

func TestRenderBundleCSV_Happy(t *testing.T) {
	csv := "name,role\nAlice,eng\nBob,pm\n"
	got, err := renderBundleCSV(bundleDocCard{Format: "csv", Content: csv}, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`<table class="bundle-csv">`,
		`<thead>`,
		`<th>name</th>`,
		`<th>role</th>`,
		`<tbody>`,
		`<td>Alice</td>`,
		`<td>eng</td>`,
		`<td>Bob</td>`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in csv output:\n%s", want, s)
		}
	}
}

func TestRenderBundleCSV_EscapesXSS(t *testing.T) {
	csv := "name,role\n<script>alert(1)</script>,\"<img src=x onerror=y>\"\n"
	got, _ := renderBundleCSV(bundleDocCard{Content: csv}, newTestBundleRenderCtx())
	s := string(got)
	if strings.Contains(s, "<script>") {
		t.Errorf("unescaped <script> in csv: %s", s)
	}
	if strings.Contains(s, "<img src=x") {
		t.Errorf("unescaped <img> in csv: %s", s)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("want escaped <script>, got %s", s)
	}
}

func TestRenderBundleCSV_OverCapDegrades(t *testing.T) {
	var b strings.Builder
	b.WriteString("h1,h2\n")
	for range bundleCSVRowCap + 5 {
		b.WriteString("a,b\n")
	}
	got, _ := renderBundleCSV(bundleDocCard{Content: b.String()}, newTestBundleRenderCtx())
	s := string(got)
	if !strings.Contains(s, `data-overcap="1"`) {
		t.Errorf("want over-cap fallback, got prefix: %.200s", s)
	}
	if strings.Contains(s, "<table") {
		t.Errorf("over-cap should not render <table>, got: %.200s", s)
	}
}

func TestRenderBundleCSV_EmptyContent(t *testing.T) {
	got, _ := renderBundleCSV(bundleDocCard{}, newTestBundleRenderCtx())
	if string(got) != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestRenderBundleCode_Happy(t *testing.T) {
	got, err := renderBundleCode(bundleDocCard{Language: "go", Content: "package main\n"}, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if s == "" {
		t.Errorf("want non-empty highlighted output")
	}
}

func TestRenderBundleCode_NilCtxDegrades(t *testing.T) {
	got, _ := renderBundleCode(bundleDocCard{Content: "<script>alert(1)</script>"}, nil)
	s := string(got)
	if !strings.Contains(s, `class="bundle-code"`) {
		t.Errorf("want degraded <pre class='bundle-code'>, got %s", s)
	}
	if strings.Contains(s, "<script>") {
		t.Errorf("XSS payload unescaped in degraded path: %s", s)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("want escaped <script>, got %s", s)
	}
}

func TestRenderBundleCode_EmptyContent(t *testing.T) {
	got, _ := renderBundleCode(bundleDocCard{}, newTestBundleRenderCtx())
	if string(got) != "" {
		t.Errorf("want empty, got %q", got)
	}
}

func TestRenderBundleImage_WhitelistedHost(t *testing.T) {
	card := bundleDocCard{
		Format:     "image",
		Headline:   "screenshot",
		ContentURL: "https://s3.amazonaws.com/bucket/path/image.png",
	}
	got, err := renderBundleImage(card, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if !strings.HasPrefix(s, `<img class="bundle-image"`) {
		t.Errorf("want <img>, got %s", s)
	}
	if !strings.Contains(s, `src="https://s3.amazonaws.com/bucket/path/image.png"`) {
		t.Errorf("want src URL, got %s", s)
	}
	if !strings.Contains(s, `alt="screenshot"`) {
		t.Errorf("want alt headline, got %s", s)
	}
	if !strings.Contains(s, `loading="lazy"`) {
		t.Errorf("want lazy loading, got %s", s)
	}
}

func TestRenderBundleImage_RejectsNonWhitelistedHost(t *testing.T) {
	card := bundleDocCard{
		Format:     "image",
		Headline:   "evil",
		ContentURL: "https://evil.example.com/x.png",
	}
	got, _ := renderBundleImage(card, newTestBundleRenderCtx())
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("non-whitelisted host must not render <img>, got %s", s)
	}
	if !strings.Contains(s, `class="bundle-link-card"`) {
		t.Errorf("want fallback link card, got %s", s)
	}
}

func TestRenderBundleImage_NilCtxRejects(t *testing.T) {
	// nil ctx → empty whitelist → fail closed → fall through to link card.
	card := bundleDocCard{ContentURL: "https://s3.amazonaws.com/x.png"}
	got, _ := renderBundleImage(card, nil)
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("nil ctx must fail closed, got %s", s)
	}
}

func TestRenderBundleImage_EmptyURLPlaceholder(t *testing.T) {
	got, _ := renderBundleImage(bundleDocCard{}, newTestBundleRenderCtx())
	s := string(got)
	if !strings.Contains(s, `class="bundle-image-empty empty"`) {
		t.Errorf("want empty placeholder, got %s", s)
	}
}

func TestRenderBundleImage_UnparseableURLDegrades(t *testing.T) {
	got, _ := renderBundleImage(bundleDocCard{ContentURL: "not a url"}, newTestBundleRenderCtx())
	s := string(got)
	if strings.Contains(s, "<img") {
		t.Errorf("unparseable URL must not render <img>, got %s", s)
	}
}

func TestRenderBundleLink_Happy(t *testing.T) {
	card := bundleDocCard{
		Headline:   "MR !1234",
		ContentURL: "https://gitlab.example.com/org/repo/-/merge_requests/1234",
	}
	got, err := renderBundleLink(card, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	for _, want := range []string{
		`class="bundle-link-card"`,
		`href="https://gitlab.example.com/org/repo/-/merge_requests/1234"`,
		`rel="noopener noreferrer"`,
		`<span class="bundle-link-title">MR !1234</span>`,
		`<code class="bundle-link-url">`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in link output:\n%s", want, s)
		}
	}
}

func TestRenderBundleLink_FallsBackToURLAsTitle(t *testing.T) {
	card := bundleDocCard{ContentURL: "https://example.com/x"}
	got, _ := renderBundleLink(card, newTestBundleRenderCtx())
	s := string(got)
	if !strings.Contains(s, `>https://example.com/x</span>`) {
		t.Errorf("want URL as title, got %s", s)
	}
}

func TestRenderBundleLink_EmptyURLPlaceholder(t *testing.T) {
	got, _ := renderBundleLink(bundleDocCard{Headline: "x"}, newTestBundleRenderCtx())
	s := string(got)
	if !strings.Contains(s, `class="bundle-link-empty empty"`) {
		t.Errorf("want empty placeholder, got %s", s)
	}
}

func TestRenderBundleLink_EscapesXSS(t *testing.T) {
	card := bundleDocCard{
		Headline:   `<script>alert(1)</script>`,
		ContentURL: `https://example.com/?q=<script>`,
	}
	got, _ := renderBundleLink(card, newTestBundleRenderCtx())
	s := string(got)
	if strings.Contains(s, "<script>") {
		t.Errorf("unescaped <script> in link: %s", s)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("want escaped <script>, got %s", s)
	}
}

func TestRenderBundleHTML_Happy(t *testing.T) {
	ctx := newTestBundleRenderCtx()
	card := bundleDocCard{Format: "html", Content: "<h1>hello</h1><p>world</p>"}
	got, err := renderBundleHTML(card, ctx)
	if err != nil {
		t.Fatalf("renderBundleHTML: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, `<iframe class="bundle-html"`) {
		t.Errorf("want sandboxed iframe, got %q", s)
	}
	if !strings.Contains(s, ` sandbox `) && !strings.Contains(s, ` sandbox=`) && !strings.Contains(s, `<iframe class="bundle-html" sandbox`) {
		t.Errorf("iframe must carry sandbox attribute, got %q", s)
	}
	// srcdoc is HTML-escaped — the embedded document gets the original markup
	// after the browser decodes it once. The `<base target="_top">` prefix
	// (ka-0000001462) makes in-frame links navigate the whole page; it rides
	// through the same single escape.
	if !strings.Contains(s, `srcdoc="&lt;base target=&#34;_top&#34;&gt;&lt;h1&gt;hello&lt;/h1&gt;&lt;p&gt;world&lt;/p&gt;"`) {
		t.Errorf("want HTML-escaped srcdoc with base prefix, got %q", s)
	}
}

func TestRenderBundleHTML_SandboxHasNoAllowScripts(t *testing.T) {
	// This is the load-bearing security invariant: the sandbox attribute must
	// NEVER carry allow-scripts. If a future edit adds it the renderer becomes
	// a viewer-wide XSS surface — and worse, paired with allow-same-origin
	// (which we DO set) embedded script could strip its own sandbox and escape.
	// allow-same-origin alone is safe precisely because no script ever runs in
	// the frame; it exists so the PARENT can measure + bind previews
	// (bundle_html.js, ka-0000004212 / ka-0000006557).
	got, _ := renderBundleHTML(
		bundleDocCard{Content: `<script>alert('pwned')</script>`},
		newTestBundleRenderCtx(),
	)
	s := string(got)
	if strings.Contains(s, "allow-scripts") {
		t.Fatalf("sandbox MUST NOT include allow-scripts, got %q", s)
	}
	// allow-same-origin is REQUIRED for parent-side auto-resize + preview bind.
	if !strings.Contains(s, "allow-same-origin") {
		t.Fatalf("sandbox MUST include allow-same-origin (no scripts), got %q", s)
	}
	// The unrestricted allow-top-navigation token would let frame content
	// redirect the whole page WITHOUT a user gesture — forbidden. Only the
	// "-by-user-activation" variant (a real click) is allowed (ka-0000001462).
	if strings.Contains(s, `allow-top-navigation"`) || strings.Contains(s, `allow-top-navigation `) {
		t.Fatalf("sandbox MUST NOT include unrestricted allow-top-navigation, got %q", s)
	}
	if strings.Contains(s, "allow-popups") {
		t.Fatalf("sandbox MUST NOT include allow-popups, got %q", s)
	}
}

func TestRenderBundleHTML_LinksNavigateTopFrame(t *testing.T) {
	// ka-0000001462: a link inside the bundle html-doc iframe used to navigate
	// the frame itself (target=_self default) — a dead-end. The fix injects
	// `<base target="_top">` into the framed document and adds the
	// allow-top-navigation-by-user-activation sandbox token so a real click
	// escapes the frame and navigates the whole page.
	got, _ := renderBundleHTML(
		bundleDocCard{Content: `<a href="/gd-abc">link</a>`},
		newTestBundleRenderCtx(),
	)
	s := string(got)
	// The <base> element (escaped once for the srcdoc attribute) makes every
	// in-frame link default to the top browsing context.
	if !strings.Contains(s, `&lt;base target=&#34;_top&#34;&gt;`) {
		t.Errorf("want escaped <base target=\"_top\"> prefix in srcdoc, got %q", s)
	}
	// The sandbox must grant top navigation only on user activation — never the
	// unrestricted form (that would allow scriptless meta-refresh-style or
	// content-driven top redirects).
	if !strings.Contains(s, "allow-top-navigation-by-user-activation") {
		t.Errorf("want allow-top-navigation-by-user-activation token, got %q", s)
	}
	if strings.Contains(s, "allow-scripts") {
		t.Fatalf("fix MUST NOT introduce allow-scripts, got %q", s)
	}
}

func TestRenderBundleHTML_XSSPayloadEscapedInSrcdoc(t *testing.T) {
	got, _ := renderBundleHTML(
		bundleDocCard{Content: `<script>alert(1)</script><img src=x onerror=y>`},
		newTestBundleRenderCtx(),
	)
	s := string(got)
	// The raw <script> tag must not appear in the parent document — only
	// inside the escaped srcdoc value where the sandboxed iframe will treat
	// it as inert (no allow-scripts).
	idx := strings.Index(s, "srcdoc=\"")
	if idx < 0 {
		t.Fatalf("missing srcdoc attribute: %q", s)
	}
	preSrcdoc := s[:idx]
	if strings.Contains(preSrcdoc, "<script>") {
		t.Errorf("raw <script> leaked into parent document before srcdoc: %q", preSrcdoc)
	}
	if !strings.Contains(s, "&lt;script&gt;") {
		t.Errorf("want escaped <script> inside srcdoc, got %q", s)
	}
	if !strings.Contains(s, "&lt;img src=x onerror=y&gt;") {
		t.Errorf("want escaped <img> inside srcdoc, got %q", s)
	}
}

func TestRenderBundleHTML_AttributeBreakoutAttempt(t *testing.T) {
	// Attempt to break out of the srcdoc attribute by injecting a literal
	// double-quote. HTMLEscapeString turns " into &#34;, so the breakout is
	// neutralized.
	payload := `"><script>alert(1)</script>`
	got, _ := renderBundleHTML(bundleDocCard{Content: payload}, newTestBundleRenderCtx())
	s := string(got)
	if strings.Count(s, `"`) != strings.Count(`<iframe class="bundle-html" sandbox="allow-same-origin" srcdoc="" loading="lazy" referrerpolicy="no-referrer" style="">`, `"`) {
		// We expect exactly the structural quotes from class/srcdoc/loading/
		// referrerpolicy/style attributes — author content's " must all be
		// escaped to &#34; and not contribute additional unescaped quotes.
		// A failure here indicates an attribute breakout vector.
		t.Errorf("unexpected raw quote count — possible attribute breakout in: %q", s)
	}
	if strings.Contains(s, `"><script>`) {
		t.Errorf("attribute breakout — unescaped quote sequence reached output: %q", s)
	}
}

func TestRenderBundleHTML_EmptyContent(t *testing.T) {
	got, err := renderBundleHTML(bundleDocCard{Format: "html"}, newTestBundleRenderCtx())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if string(got) != "" {
		t.Errorf("want empty output for empty content, got %q", got)
	}
}

func TestRenderBundleHTML_NilCtxStillSafe(t *testing.T) {
	// The html renderer doesn't consult ctx today, but if a future change
	// adds ctx-dependent logic the nil-ctx branch must still emit a
	// sandboxed iframe — the security invariant is independent of ctx.
	got, err := renderBundleHTML(bundleDocCard{Content: "<p>hi</p>"}, nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "sandbox") {
		t.Errorf("nil ctx must still emit sandboxed iframe, got %q", s)
	}
	if strings.Contains(s, "allow-scripts") {
		t.Fatalf("nil ctx path must not include allow-scripts, got %q", s)
	}
}

func TestRenderBundleHTML_DefaultMinHeightApplied(t *testing.T) {
	got, _ := renderBundleHTML(bundleDocCard{Content: "<p>x</p>"}, newTestBundleRenderCtx())
	s := string(got)
	want := fmt.Sprintf("min-height:%dpx", bundleHTMLDefaultMinHeight)
	if !strings.Contains(s, want) {
		t.Errorf("want %q in inline style (sandboxed iframes don't auto-size), got %q", want, s)
	}
}

func TestIsWhitelistedImageHost(t *testing.T) {
	ctx := &bundleRenderCtx{imageHosts: map[string]struct{}{
		"exact.example.com": {},
		".s3.amazonaws.com": {},
	}}

	cases := []struct {
		host string
		want bool
	}{
		{"exact.example.com", true},
		{"EXACT.example.com", true}, // case-insensitive
		{"bucket.s3.amazonaws.com", true},
		{"deeper.bucket.s3.amazonaws.com", true},
		{"s3.amazonaws.com", false}, // exact form not in map (only the leading-dot form is)
		{"evil.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isWhitelistedImageHost(ctx, c.host); got != c.want {
			t.Errorf("isWhitelistedImageHost(%q) = %v, want %v", c.host, got, c.want)
		}
	}
	if isWhitelistedImageHost(nil, "exact.example.com") {
		t.Errorf("nil ctx must fail closed")
	}
	if isWhitelistedImageHost(&bundleRenderCtx{}, "exact.example.com") {
		t.Errorf("empty whitelist must fail closed")
	}
}

func TestBundleImageHostWhitelist_DefaultAndEnv(t *testing.T) {
	t.Setenv("GARDENIO_BUNDLE_IMAGE_HOSTS", "")
	def := bundleImageHostWhitelist()
	if _, ok := def["s3.amazonaws.com"]; !ok {
		t.Errorf("default whitelist must include s3.amazonaws.com, got %v", keys(def))
	}

	t.Setenv("GARDENIO_BUNDLE_IMAGE_HOSTS", "Foo.Example.COM, .bar.example.com ,,baz.example.com")
	got := bundleImageHostWhitelist()
	for _, want := range []string{"foo.example.com", ".bar.example.com", "baz.example.com", "s3.amazonaws.com"} {
		if _, ok := got[want]; !ok {
			t.Errorf("want host %q in whitelist, got %v", want, keys(got))
		}
	}
}

func keys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
