package main

import (
	"bytes"
	"strings"
	"testing"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"
)

// newMermaidHighlighting builds the highlighting extension exactly as the
// production md builder (and block_id_test.go) does — GFM + github-dark chroma +
// the block-id hooks — so mermaid tests exercise the real interaction (the
// transformer must intercept a mermaid fence before highlighting renders it).
func newMermaidHighlighting() goldmark.Extender {
	hlOpts := append([]highlighting.Option{
		highlighting.WithStyle("github-dark"),
		highlighting.WithFormatOptions(chromahtml.WithClasses(false)),
	}, blockIDHighlightingOptions()...)
	return highlighting.NewHighlighting(hlOpts...)
}

// newMermaidTestMD builds a goldmark instance wired exactly like the production
// md renderer's relevant pieces (GFM + highlighting + mermaid).
func newMermaidTestMD(t *testing.T) goldmark.Markdown {
	t.Helper()
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM, newMermaidHighlighting()),
		goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs(), withMermaid()),
		goldmark.WithRendererOptions(html.WithHardWraps(), withBlockIDRenderers(), withMermaidRenderer()),
	)
}

func convertMD(t *testing.T, md goldmark.Markdown, src string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf); err != nil {
		t.Fatalf("convert: %v", err)
	}
	return buf.String()
}

func TestMermaid_EmitsExpectedHTML(t *testing.T) {
	md := newMermaidTestMD(t)
	got := convertMD(t, md, "```mermaid\ngraph TD; A-->B\n```\n")
	if !strings.Contains(got, `<pre class="mermaid-diagram" data-mermaid="1"><code class="language-mermaid">`) {
		t.Errorf("missing mermaid pre/code wrapper.\ngot: %s", got)
	}
	if !strings.Contains(got, "graph TD; A--&gt;B") {
		t.Errorf("source not present/escaped (--> should become --&gt;).\ngot: %s", got)
	}
	if !strings.Contains(got, "</code></pre>") {
		t.Errorf("missing close tags.\ngot: %s", got)
	}
}

func TestMermaid_EscapesMaliciousSource(t *testing.T) {
	md := newMermaidTestMD(t)
	// A node label carrying an <img onerror> plus a naive close-tag breakout.
	src := "```mermaid\ngraph TD\n  A[\"<img src=x onerror=alert(1)>\"]\n  B[\"</code></pre><script>evil()</script>\"]\n```\n"
	got := convertMD(t, md, src)

	// The payload must be entity-escaped, never live markup.
	if strings.Contains(got, "<img src=x onerror=") {
		t.Errorf("live <img> reached output — XSS.\ngot: %s", got)
	}
	if strings.Contains(got, "<script>evil()") {
		t.Errorf("live <script> reached output — XSS.\ngot: %s", got)
	}
	if !strings.Contains(got, "&lt;img src=x onerror=alert(1)&gt;") {
		t.Errorf("expected escaped <img>.\ngot: %s", got)
	}
	if !strings.Contains(got, "&lt;/code&gt;&lt;/pre&gt;&lt;script&gt;") {
		t.Errorf("expected escaped close-tag breakout.\ngot: %s", got)
	}
}

func TestMermaid_NotTouchedByHighlighting(t *testing.T) {
	md := newMermaidTestMD(t)
	// One mermaid fence + one go fence in the same doc.
	got := convertMD(t, md, "```mermaid\ngraph TD; A-->B\n```\n\n```go\nfunc main() {}\n```\n")

	// The mermaid block must NOT carry chroma inline-style spans.
	mermaidStart := strings.Index(got, `class="mermaid-diagram"`)
	if mermaidStart < 0 {
		t.Fatalf("mermaid block missing.\ngot: %s", got)
	}
	mermaidEnd := strings.Index(got[mermaidStart:], "</pre>")
	if mermaidEnd < 0 {
		t.Fatalf("mermaid block not closed.\ngot: %s", got)
	}
	mermaidHTML := got[mermaidStart : mermaidStart+mermaidEnd]
	if strings.Contains(mermaidHTML, "<span style=") {
		t.Errorf("mermaid block was syntax-highlighted (chroma span present).\ngot: %s", mermaidHTML)
	}

	// The go fence SHOULD still be highlighted by chroma (inline-style spans).
	if !strings.Contains(got, "<span style=") {
		t.Errorf("go fence lost syntax highlighting — mermaid transformer over-reached.\ngot: %s", got)
	}
}

func TestMermaid_LanguageGateExact(t *testing.T) {
	md := newMermaidTestMD(t)

	// Case-insensitive + info-string trailing content still match on first word.
	for _, src := range []string{
		"```Mermaid\ngraph TD; A-->B\n```\n",
		"```mermaid {theme=dark}\ngraph TD; A-->B\n```\n",
	} {
		got := convertMD(t, md, src)
		if !strings.Contains(got, `class="mermaid-diagram"`) {
			t.Errorf("expected mermaid rendering for %q.\ngot: %s", src, got)
		}
	}

	// Near-misses must NOT match — they render as ordinary (highlighted) code.
	for _, src := range []string{
		"```mermaidjs\ngraph TD; A-->B\n```\n",
		"```mermaidish\ngraph TD; A-->B\n```\n",
	} {
		got := convertMD(t, md, src)
		if strings.Contains(got, `class="mermaid-diagram"`) {
			t.Errorf("false-positive mermaid match for %q.\ngot: %s", src, got)
		}
	}
}

func TestMermaid_JSOffDegradation(t *testing.T) {
	md := newMermaidTestMD(t)
	got := convertMD(t, md, "```mermaid\nsequenceDiagram\n  A->>B: hi\n```\n")
	// Source lives in element text (a readable <pre><code>), NOT a data-attribute.
	if strings.Contains(got, "data-mermaid-src") {
		t.Errorf("source must live in element text, not a data-mermaid-src attribute.\ngot: %s", got)
	}
	if !strings.Contains(got, "sequenceDiagram") || !strings.Contains(got, "A-&gt;&gt;B: hi") {
		t.Errorf("no-JS fallback lost the diagram source.\ngot: %s", got)
	}
	// No spinner / loading placeholder — the source IS the pre-render state.
	if strings.Contains(got, "spinner") || strings.Contains(got, "loading") {
		t.Errorf("unexpected spinner/loading state in server output.\ngot: %s", got)
	}
}

func TestMermaid_NoBlockID(t *testing.T) {
	md := newMermaidTestMD(t)
	got := convertMD(t, md, "A paragraph.\n\n```mermaid\ngraph TD; A-->B\n```\n")
	// The surrounding paragraph gets a data-block-id...
	if !strings.Contains(got, "data-block-id") {
		t.Fatalf("expected a data-block-id on the paragraph.\ngot: %s", got)
	}
	// ...but the mermaid <pre> must NOT (it is excluded from critique anchoring
	// because the transformer replaces the fence before blockIDTransformer runs).
	mermaidStart := strings.Index(got, `class="mermaid-diagram"`)
	if mermaidStart < 0 {
		t.Fatalf("mermaid block missing.\ngot: %s", got)
	}
	mermaidEnd := strings.Index(got[mermaidStart:], "</pre>")
	mermaidHTML := got[mermaidStart : mermaidStart+mermaidEnd]
	if strings.Contains(mermaidHTML, "data-block-id") {
		t.Errorf("mermaid block should not carry data-block-id.\ngot: %s", mermaidHTML)
	}
}

func TestMermaid_MultipleDiagrams(t *testing.T) {
	md := newMermaidTestMD(t)
	got := convertMD(t, md, "```mermaid\ngraph TD; A-->B\n```\n\n```mermaid\ngraph LR; C-->D\n```\n")
	if n := strings.Count(got, `class="mermaid-diagram"`); n != 2 {
		t.Errorf("expected 2 mermaid blocks, got %d.\ngot: %s", n, got)
	}
	// Document order preserved.
	if strings.Index(got, "A--&gt;B") > strings.Index(got, "C--&gt;D") {
		t.Errorf("diagram order not preserved.\ngot: %s", got)
	}
}

// TestMermaid_LinkifyDoesNotCorruptSource guards the must-fix: a bead-ID / MR-ref
// literal inside a diagram label must survive byte-for-byte through the
// LinkifyHTML post-pass (skipRe must skip the whole mermaid <pre> region).
func TestMermaid_LinkifyDoesNotCorruptSource(t *testing.T) {
	md := newMermaidTestMD(t)
	src := "```mermaid\ngraph TD\n  A[\"see gd-abc12345 and !12\"] --> B\n```\n"
	rendered := convertMD(t, md, src)

	linked := string(LinkifyHTML(rendered, "/beads"))

	// Inside the mermaid block, the gd-id and !12 must NOT be wrapped in an <a>.
	mermaidStart := strings.Index(linked, `class="mermaid-diagram"`)
	if mermaidStart < 0 {
		t.Fatalf("mermaid block missing after linkify.\ngot: %s", linked)
	}
	mermaidEnd := strings.Index(linked[mermaidStart:], "</pre>")
	mermaidHTML := linked[mermaidStart : mermaidStart+mermaidEnd]
	if strings.Contains(mermaidHTML, "<a ") {
		t.Errorf("linkify corrupted the diagram source with an <a> tag.\nblock: %s", mermaidHTML)
	}
	if !strings.Contains(mermaidHTML, "see gd-abc12345 and !12") {
		t.Errorf("diagram source altered by linkify.\nblock: %s", mermaidHTML)
	}
}

// TestMermaid_LinkifyDoesNotExpandGrafana guards the second post-render rewriter:
// embedGrafanaDirectives runs BEFORE skipRe protects the block, so it must skip
// mermaid regions itself. A `:::grafana` line inside diagram source must survive
// verbatim, not expand into a grafana embed (which would corrupt the diagram).
func TestMermaid_LinkifyDoesNotExpandGrafana(t *testing.T) {
	md := newMermaidTestMD(t)
	src := "```mermaid\ngraph TD\n:::grafana somedash\nA-->B\n```\n"
	rendered := convertMD(t, md, src)
	linked := string(LinkifyHTML(rendered, "/beads"))
	if strings.Contains(linked, "grafana-embed") {
		t.Errorf("grafana directive was expanded inside a mermaid diagram.\ngot: %s", linked)
	}
	if !strings.Contains(linked, ":::grafana somedash") {
		t.Errorf("mermaid source line lost.\ngot: %s", linked)
	}
	// A genuine :::grafana directive OUTSIDE the diagram must still expand.
	src2 := "Some text.\n\n:::grafana realdash\n\n```mermaid\ngraph TD; A-->B\n```\n"
	linked2 := string(LinkifyHTML(convertMD(t, md, src2), "/beads"))
	if !strings.Contains(linked2, "grafana-embed") {
		t.Errorf("grafana directive outside the diagram should still expand.\ngot: %s", linked2)
	}
}

// TestMermaid_BothInstances confirms both goldmark instances (description +
// comment) render diagrams identically once wired the same way.
func TestMermaid_BothInstances(t *testing.T) {
	mdBlock := goldmark.New(
		goldmark.WithExtensions(extension.GFM, newMermaidHighlighting()),
		goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs(), withMermaid()),
		goldmark.WithRendererOptions(withBlockIDRenderers(), withMermaidRenderer()),
	)
	mdInline := goldmark.New(
		goldmark.WithExtensions(extension.GFM, newMermaidHighlighting()),
		goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs(), withMermaid()),
		goldmark.WithRendererOptions(html.WithHardWraps(), withBlockIDRenderers(), withMermaidRenderer()),
	)
	src := "```mermaid\ngraph TD; A-->B\n```\n"
	if !strings.Contains(convertMD(t, mdBlock, src), `class="mermaid-diagram"`) {
		t.Errorf("description instance did not render mermaid")
	}
	if !strings.Contains(convertMD(t, mdInline, src), `class="mermaid-diagram"`) {
		t.Errorf("comment instance did not render mermaid")
	}
}

// TestMermaid_EscapeRoundTrips checks the escaper matches util.EscapeHTML byte
// for byte, so the client's code.textContent decodes to the exact source.
func TestMermaid_EscapeRoundTrips(t *testing.T) {
	md := newMermaidTestMD(t)
	line := `A["<b>&'\"quoted\""] --> B`
	got := convertMD(t, md, "```mermaid\n"+line+"\n```\n")
	want := string(util.EscapeHTML([]byte(line)))
	if !strings.Contains(got, want) {
		t.Errorf("escaped source mismatch.\nwant substring: %s\ngot: %s", want, got)
	}
}
