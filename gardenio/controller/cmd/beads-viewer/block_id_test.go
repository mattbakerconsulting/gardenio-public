package main

import (
	"bytes"
	"regexp"
	"strings"
	"testing"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/yuin/goldmark"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"
)

// newBlockIDMD builds a goldmark instance mirroring the production `md` builder
// in server.go: GFM + highlighting (with the block-id hooks) + heading anchors
// + block ids + the custom block-id node renderer.
func newBlockIDMD(hardWraps bool) goldmark.Markdown {
	hlOpts := append([]highlighting.Option{
		highlighting.WithStyle("github-dark"),
		highlighting.WithFormatOptions(chromahtml.WithClasses(false)),
	}, blockIDHighlightingOptions()...)
	hl := highlighting.NewHighlighting(hlOpts...)
	if hardWraps {
		return goldmark.New(
			goldmark.WithExtensions(extension.GFM, hl),
			goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs()),
			goldmark.WithRendererOptions(html.WithHardWraps(), withBlockIDRenderers()),
		)
	}
	return goldmark.New(
		goldmark.WithExtensions(extension.GFM, hl),
		goldmark.WithParserOptions(withHeadingAnchors(), withBlockIDs()),
		goldmark.WithRendererOptions(withBlockIDRenderers()),
	)
}

func renderBlockIDMD(t *testing.T, src string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := newBlockIDMD(false).Convert([]byte(src), &buf); err != nil {
		t.Fatalf("convert: %v", err)
	}
	return buf.String()
}

// TestBlockID_Paragraph confirms a plain paragraph gets the first id, rendered
// natively by the default renderer (data- prefix bypasses its attr filter).
func TestBlockID_Paragraph(t *testing.T) {
	out := renderBlockIDMD(t, "hello world")
	if !strings.Contains(out, `<p data-block-id="blk-1">hello world</p>`) {
		t.Errorf("paragraph missing data-block-id\n%s", out)
	}
}

// TestBlockID_AllBlockKinds confirms every one of the seven anchored block
// kinds carries a data-block-id in the rendered HTML.
func TestBlockID_AllBlockKinds(t *testing.T) {
	src := strings.Join([]string{
		"para one",
		"",
		"```go",
		`fmt.Println("hi")`,
		"```",
		"",
		"    indented code",
		"",
		"> a blockquote",
		"",
		"- item one",
		"- item two",
	}, "\n")
	out := renderBlockIDMD(t, src)

	// Paragraph.
	if !strings.Contains(out, `<p data-block-id="blk-1">para one</p>`) {
		t.Errorf("paragraph missing id\n%s", out)
	}
	// Fenced code (highlighted path): id lands on the chroma <pre>.
	if m := regexp.MustCompile(`<pre[^>]*\bdata-block-id="blk-\d+"[^>]*><code>`).FindString(out); m == "" {
		t.Errorf("fenced code block missing data-block-id on <pre>\n%s", out)
	}
	// Indented code block: custom renderer stamps the <pre>.
	if !strings.Contains(out, `<pre data-block-id=`) {
		t.Errorf("indented code block missing data-block-id\n%s", out)
	}
	if !strings.Contains(out, "indented code") {
		t.Errorf("indented code content missing\n%s", out)
	}
	// Blockquote.
	if !regexp.MustCompile(`<blockquote data-block-id="blk-\d+">`).MatchString(out) {
		t.Errorf("blockquote missing data-block-id\n%s", out)
	}
	// List + list items.
	if !regexp.MustCompile(`<ul data-block-id="blk-\d+">`).MatchString(out) {
		t.Errorf("list missing data-block-id\n%s", out)
	}
	if n := strings.Count(out, `<li data-block-id=`); n != 2 {
		t.Errorf("expected 2 list items with data-block-id, got %d\n%s", n, out)
	}
}

// TestBlockID_FencedNoLanguage exercises the non-highlighted fallback path
// (no info string => no lexer => WrapperRenderer supplies <pre><code>).
func TestBlockID_FencedNoLanguage(t *testing.T) {
	out := renderBlockIDMD(t, "```\nplain text\n```")
	if m := regexp.MustCompile(`<pre data-block-id="blk-\d+"><code>`).FindString(out); m == "" {
		t.Errorf("no-language fenced block missing data-block-id on <pre>\n%s", out)
	}
	if !strings.Contains(out, "plain text") {
		t.Errorf("fenced content missing\n%s", out)
	}
}

// TestBlockID_FencedUnknownLanguage exercises the fallback path for an
// unrecognised language token, which must still carry the id and preserve the
// language class the default renderer emits.
func TestBlockID_FencedUnknownLanguage(t *testing.T) {
	out := renderBlockIDMD(t, "```nosuchlang\nabc\n```")
	if m := regexp.MustCompile(`<pre data-block-id="blk-\d+"><code class="language-nosuchlang">`).FindString(out); m == "" {
		t.Errorf("unknown-language fenced block missing id/class\n%s", out)
	}
}

// TestBlockID_HTMLBlock confirms a raw HTML block is wrapped in an anchorable
// div. Production runs Unsafe=false, so the body is the escaped source.
func TestBlockID_HTMLBlock(t *testing.T) {
	out := renderBlockIDMD(t, "<div>raw</div>")
	if m := regexp.MustCompile(`<div data-block-id="blk-\d+">`).FindString(out); m == "" {
		t.Errorf("HTML block not wrapped with data-block-id\n%s", out)
	}
	if !strings.Contains(out, "&lt;div&gt;raw&lt;/div&gt;") {
		t.Errorf("expected safe-mode escaped source\n%s", out)
	}
	if strings.Contains(out, "raw HTML omitted") {
		t.Errorf("raw HTML block was dropped, not escaped\n%s", out)
	}
}

// TestRawHTML_RendersEscapedSource covers gd-oNoa0zf55W: angle-bracket text
// that goldmark parses as raw HTML (a `<n>` placeholder, a `<script>` tag)
// renders as its escaped source, inline and block, never as a tag and never
// as the omission comment that read "credits= or model=opus".
func TestRawHTML_RendersEscapedSource(t *testing.T) {
	cases := []struct {
		name, src, want string
	}{
		{"placeholder", "reply credits=<n> and/or model=opus", "reply credits=&lt;n&gt; and/or model=opus"},
		{"inline script", "say <script>alert(1)</script> here", "say &lt;script&gt;alert(1)&lt;/script&gt; here"},
		{"inline handler", "x <img src=x onerror=alert(2)> y", "x &lt;img src=x onerror=alert(2)&gt; y"},
		{"block script", "<script>alert(1)</script>", "&lt;script&gt;alert(1)&lt;/script&gt;"},
		{"block closure", "<!--\nnote\n-->", "&lt;!--\nnote\n--&gt;"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, hard := range []bool{false, true} {
				var buf bytes.Buffer
				if err := newBlockIDMD(hard).Convert([]byte(tc.src), &buf); err != nil {
					t.Fatalf("convert: %v", err)
				}
				out := buf.String()
				if !strings.Contains(out, tc.want) {
					t.Errorf("hardWraps=%v: missing %q\n%s", hard, tc.want, out)
				}
				for _, bad := range []string{"<script", "<img", "<n>", "<!--", "raw HTML omitted"} {
					if strings.Contains(out, bad) {
						t.Errorf("hardWraps=%v: output contains %q\n%s", hard, bad, out)
					}
				}
			}
		})
	}
}

// TestDecisionMarkdown_KeepsPlaceholder runs the production decision path
// (s.md + LinkifyHTML) over the Gate's approve label, so every decision
// surface shows credits=<n> verbatim.
func TestDecisionMarkdown_KeepsPlaceholder(t *testing.T) {
	s := newTestServer(t)
	got := string(s.decisionMarkdown("Approve: provision the land. To change either, reply credits=<n> and/or model=opus."))
	if !strings.Contains(got, "reply credits=&lt;n&gt; and/or model=opus") {
		t.Errorf("decision markdown lost the <n> placeholder: %s", got)
	}
}

// TestBlockID_MonotonicDocumentOrder confirms ids increase in depth-first
// document order and start at 1.
func TestBlockID_MonotonicDocumentOrder(t *testing.T) {
	out := renderBlockIDMD(t, "first\n\nsecond\n\nthird")
	ids := regexp.MustCompile(`data-block-id="(blk-\d+)"`).FindAllStringSubmatch(out, -1)
	got := make([]string, 0, len(ids))
	for _, m := range ids {
		got = append(got, m[1])
	}
	want := []string{"blk-1", "blk-2", "blk-3"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("ids = %v, want %v\n%s", got, want, out)
	}
}

// TestBlockID_StableAcrossRerenders is the load-bearing invariant: an unchanged
// document renders identical ids on every pass (the anchor resolver's tier-1
// lookup depends on it).
func TestBlockID_StableAcrossRerenders(t *testing.T) {
	src := strings.Join([]string{
		"intro paragraph",
		"",
		"```go",
		"x := 1",
		"```",
		"",
		"> quote",
		"",
		"- a",
		"- b",
	}, "\n")
	first := renderBlockIDMD(t, src)
	for range 5 {
		if got := renderBlockIDMD(t, src); got != first {
			t.Fatalf("re-render differs from first render\n--- first ---\n%s\n--- got ---\n%s", first, got)
		}
	}
}

// TestBlockID_HeadingSlugsUnchanged confirms block-id stamping does not disturb
// the heading-anchor ids (which live on a different node kind).
func TestBlockID_HeadingSlugsUnchanged(t *testing.T) {
	out := renderBlockIDMD(t, "### C1.1 — TTL field\n\nbody\n")
	if !strings.Contains(out, `id="c1-1"`) {
		t.Errorf("heading slug id=c1-1 missing\n%s", out)
	}
	// Headings are NOT anchored blocks, so they must not gain a data-block-id.
	if regexp.MustCompile(`<h3[^>]*data-block-id`).MatchString(out) {
		t.Errorf("heading unexpectedly carries data-block-id\n%s", out)
	}
}

// TestBlockID_FencedInfoStringAttributesPreserved is the regression guard for
// the shadowing hazard: stamping data-block-id must NOT stop the highlighting
// extension from honouring info-string chroma attributes. hl_lines=[2] wraps
// line 2 in chroma's highlight span (background-color), so its presence proves
// the directive survived.
func TestBlockID_FencedInfoStringAttributesPreserved(t *testing.T) {
	// With hl_lines requested, chroma switches the PreWrapper to a grid and
	// wraps the highlighted line; the surest signal is the extra <span
	// style=...> around the highlighted line. Compare highlighted vs plain.
	withHL := renderBlockIDMD(t, "```go {hl_lines=[\"2\"]}\na := 1\nb := 2\nc := 3\n```")
	plain := renderBlockIDMD(t, "```go\na := 1\nb := 2\nc := 3\n```")
	if withHL == plain {
		t.Errorf("hl_lines info-string attribute had no effect — it was shadowed by data-block-id stamping\n--- withHL ---\n%s", withHL)
	}
	// The id must still be present on the highlighted block.
	if !regexp.MustCompile(`data-block-id="blk-\d+"`).MatchString(withHL) {
		t.Errorf("highlighted block with hl_lines lost its data-block-id\n%s", withHL)
	}
}

// TestBlockID_NestedListItemParagraphs confirms nested blocks each get their
// own id (loose list => inner paragraphs are real *ast.Paragraph nodes).
func TestBlockID_NestedListItemParagraphs(t *testing.T) {
	// Blank lines between items make the list "loose", so each item wraps its
	// text in a <p>.
	out := renderBlockIDMD(t, "- item one\n\n- item two\n")
	if !regexp.MustCompile(`<ul data-block-id=`).MatchString(out) {
		t.Errorf("list missing id\n%s", out)
	}
	if n := strings.Count(out, `<li data-block-id=`); n != 2 {
		t.Errorf("expected 2 <li> ids, got %d\n%s", n, out)
	}
	if n := strings.Count(out, `<p data-block-id=`); n != 2 {
		t.Errorf("expected 2 loose-list <p> ids, got %d\n%s", n, out)
	}
}

// TestBlockID_ConcurrentConvertSafe confirms the shared goldmark instance (the
// production pattern — one md serves every request) produces correct,
// non-interleaved ids under concurrent Convert calls, since all per-block state
// lives on the per-parse AST rather than the shared option closures.
func TestBlockID_ConcurrentConvertSafe(t *testing.T) {
	md := newBlockIDMD(false)
	src := "para\n\n```go\nx := 1\n```\n\n> q\n"
	// Render once serially for the expected output.
	var want bytes.Buffer
	if err := md.Convert([]byte(src), &want); err != nil {
		t.Fatalf("convert: %v", err)
	}
	const n = 32
	results := make([]string, n)
	done := make(chan int, n)
	for i := range n {
		go func(i int) {
			var buf bytes.Buffer
			_ = md.Convert([]byte(src), &buf)
			results[i] = buf.String()
			done <- i
		}(i)
	}
	for range n {
		<-done
	}
	for i, got := range results {
		if got != want.String() {
			t.Fatalf("concurrent render %d differs from serial render\n--- want ---\n%s\n--- got ---\n%s", i, want.String(), got)
		}
	}
}
