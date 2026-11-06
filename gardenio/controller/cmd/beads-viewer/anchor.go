package main

import (
	"bytes"
	"regexp"
	"strings"

	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	highlighting "github.com/yuin/goldmark-highlighting/v2"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// Bead anchors (epic ka-0000006913, design ka-0000007428).
//
// Planning artifacts label their headings with short tokens — `### C1.1 — TTL
// field`, `### R3. cycle-budget cap`, `## C1 Shape C — …`, ADR `## 6.3 …` —
// and those labels get referenced as though self-evident (`P.D1`, `§6.3`). To
// make a label resolve to a location we give every rendered heading a STABLE
// id derived from its label token alone, so the ref `ka-0000001713#C1.1`
// deep-links to `id="c1-1"`.
//
// The key contract: slugifyAnchor() is the SINGLE source of truth for the
// slug. The AST transformer below stamps it onto heading nodes (the link
// TARGET); linkify.go runs the SAME function over the `#label` suffix of a
// bead ref (the link SOURCE). Target and source must agree, so both call this.

// anchorLeadingJunkRE strips a leading run of non-alphanumeric characters
// (e.g. the `§` in `§6.3`, a stray `#`, or surrounding punctuation) before the
// label token is matched, so section-symbol refs resolve.
var anchorLeadingJunkRE = regexp.MustCompile(`^[^A-Za-z0-9]+`)

// anchorLabelRE matches a leading planning-label token: one or more
// alphanumeric segments joined by `.` or `-`. This admits every label family
// in the corpus — `C1`, `R3`, `P2`, `D10`, `T2`, dotted `C1.1`/`3.5.1`,
// composite `P.D1`/`P2.D1` (plan→decision), and bare ADR sections `6`/`6.3`.
// The interior separator alternates between `.` (as authored, `C1.1`) and `-`
// (as slugged, `c1-1`), so slugifyAnchor is idempotent on its own output. A
// segment boundary is a single `.`/`-`; a space ends the token, so
// "R4 - Narrow infra" stops at `R4` (space before the dash) rather than
// swallowing the title. Case-insensitive. A digit-presence check in
// slugifyAnchor (below) rejects pure-prose first words like "Overview".
var anchorLabelRE = regexp.MustCompile(`^[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*`)

// slugifyAnchor reduces a heading's text to its stable label-token slug.
//
// Rule (operator decision ka-0000006898 Q1 — "strip at first separator"): the
// slug is the LABEL TOKEN only, not the whole heading text. We strip any
// leading `§`/`#`/punctuation, take the leading label token (anchorLabelRE),
// and slugify it, mapping the dotted separator to a dash so `C1.1` -> `c1-1`,
// `6.3` -> `6-3`, `P.D1` -> `p-d1`. A label token is only accepted when it
// contains a digit — that is what separates a real label (always numbered)
// from a prose word. When a heading has no recognizable leading label (e.g.
// "## Overview") we fall back to the full-text slugify so every heading still
// gets *some* id.
//
//	"C1.1 — TTL field"     -> "c1-1"
//	"R3. cycle-budget cap" -> "r3"
//	"C1 Shape C — core"    -> "c1"
//	"6.3 Retention"        -> "6-3"
//	"§6.3 Retention"       -> "6-3"
//	"P.D1 — decision"      -> "p-d1"
//	"P2.D1"                -> "p2-d1"
//	"Overview"             -> "overview"
//
// It is idempotent on its own slugged output: slugifyAnchor("c1-1") == "c1-1"
// and slugifyAnchor("p-d1") == "p-d1", which lets linkify.go run a ref's
// `#c1-1` / `#P.D1` suffix through the same function and still hit the heading
// whose id was minted from the heading text.
func slugifyAnchor(headingText string) string {
	s := strings.TrimSpace(headingText)
	s = anchorLeadingJunkRE.ReplaceAllString(s, "")
	if m := anchorLabelRE.FindString(s); m != "" && strings.ContainsAny(m, "0123456789") {
		return slugify(m)
	}
	return slugify(s)
}

// headingAnchorTransformer stamps a stable, label-derived id onto every
// heading in the document. It runs as an AST transformer so we control the
// slug (via slugifyAnchor) rather than goldmark's whole-text default — which
// would render `## 6.3 Title` as `63-title` (digits run together), breaking
// the `#6-3` ref form. An author-supplied `{#explicit}` id (heading-attribute
// syntax) is respected and never overwritten.
type headingAnchorTransformer struct{}

// Transform implements parser.ASTTransformer.
func (headingAnchorTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	// used is keyed on the FINAL emitted id (not the base slug) and is local
	// to this one document parse, so the disambiguation loop can never mint a
	// duplicate id even when a deduped slug (`c1-2`) collides with a literal
	// label token (`C1-2`). Mirrors goldmark's util.IDs.Generate.
	used := map[string]bool{}
	src := reader.Source()
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}
		// Respect an explicit author-supplied id (WithHeadingAttribute).
		if _, has := h.AttributeString("id"); has {
			return ast.WalkSkipChildren, nil
		}
		slug := slugifyAnchor(headingText(h, src))
		if slug == "" {
			return ast.WalkSkipChildren, nil
		}
		id := slug
		for i := 2; used[id]; i++ {
			id = slug + "-" + itoa(i)
		}
		used[id] = true
		h.SetAttributeString("id", []byte(id))
		return ast.WalkSkipChildren, nil
	})
}

// headingText reconstructs a heading's plain-text content from its source
// lines. ast.Heading.Text is deprecated, so we read the heading's raw source
// segments directly — this is the text the label token is parsed from.
func headingText(h *ast.Heading, src []byte) string {
	lines := h.Lines()
	if lines.Len() == 0 {
		return ""
	}
	var b strings.Builder
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(src))
	}
	return strings.TrimSpace(b.String())
}

// withHeadingAnchors returns the parser option that enables our stable heading
// anchors. Registered on every goldmark renderer that renders bead-authored
// markdown so anchors exist uniformly across description, comment, and doc
// surfaces. Priority 100 runs it after the inline parser has populated
// heading lines.
func withHeadingAnchors() parser.Option {
	return parser.WithASTTransformers(
		util.Prioritized(headingAnchorTransformer{}, 100),
	)
}

// ---------------------------------------------------------------------------
// Block IDs (crit-critique T1, epic ka-0000002316).
//
// Line-precise inline critique anchors a comment to a rendered block by a
// STABLE, per-document `data-block-id="blk-N"`. blockIDTransformer walks the
// AST after headingAnchorTransformer and stamps that attribute on every block
// node in document (depth-first) order. The counter is STRUCTURAL, not
// content-derived: paragraph text is not stable across edits, but the block's
// position in the tree is, so an unchanged document re-renders to identical
// ids — the invariant the anchor resolver (T6) relies on for its tier-1
// `querySelector('[data-block-id="blk-N"]')` lookup.
//
// Stamping is only half the job. goldmark v1.8.2 renders `data-` attributes on
// only a subset of blocks; the rest silently drop them, so this file also
// carries the two renderer-side pieces that make the id actually appear:
//
//   - Paragraph / Blockquote / List / ListItem — the default html renderer
//     calls RenderAttributes, and a `data-` prefix bypasses its attribute
//     allow-list (renderer/html dataPrefix), so the stamped id renders
//     natively. Nothing extra needed.
//   - FencedCodeBlock — the highlighting extension owns this kind and emits
//     its own `<pre><code>` with no node attributes. We inject the id through
//     the extension's two hooks: a chroma PreWrapper (highlighted path) and a
//     WrapperRenderer (non-highlighted fallback path). Wired on the shared
//     highlighting extension via blockIDHighlightingOptions().
//   - CodeBlock (indented) / HTMLBlock — neither the default renderer nor the
//     highlighting extension renders node attributes here, so blockIDRenderer
//     (a custom renderer.NodeRenderer) reproduces their output with the id
//     added. Wired via withBlockIDRenderers().
//
// blockIDBlock reports whether a node is one of the seven block kinds the
// critique feature anchors to.
func blockIDBlock(n ast.Node) bool {
	switch n.(type) {
	case *ast.Paragraph, *ast.FencedCodeBlock, *ast.CodeBlock,
		*ast.Blockquote, *ast.List, *ast.ListItem, *ast.HTMLBlock:
		return true
	default:
		return false
	}
}

// blockIDAttr is the attribute name stamped onto every anchored block node.
var blockIDAttr = []byte("data-block-id")

// blockIDTransformer stamps `data-block-id="blk-N"` onto every block node in
// depth-first document order. N is a per-document monotonic counter starting
// at 1.
type blockIDTransformer struct{}

// Transform implements parser.ASTTransformer.
func (blockIDTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	src := reader.Source()
	n := 0
	_ = ast.Walk(doc, func(node ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering || !blockIDBlock(node) {
			return ast.WalkContinue, nil
		}
		n++
		// A fenced code block's info string may carry chroma attributes
		// (` ```go {hl_lines=[2]} `). The highlighting extension only parses
		// them lazily, and only when the node has no attributes yet — so
		// stamping data-block-id first would shadow them. Merge them onto the
		// node up front so both survive.
		if fc, ok := node.(*ast.FencedCodeBlock); ok {
			mergeFencedInfoAttributes(fc, src)
		}
		node.SetAttributeString(string(blockIDAttr), []byte("blk-"+itoa(n)))
		return ast.WalkContinue, nil
	})
}

// mergeFencedInfoAttributes parses a fenced code block's `{...}` info-string
// attributes (hl_lines, linenos, hl_style, nohl, linenostart) and sets them on
// the node. The highlighting extension parses these lazily via getAttributes,
// but bails the moment the node already has any attribute — which
// blockIDTransformer is about to give it. Doing the parse here preserves the
// info-string highlighting directives that would otherwise be dropped. No-op
// when the info string has no `{`.
func mergeFencedInfoAttributes(fc *ast.FencedCodeBlock, src []byte) {
	if fc.Info == nil {
		return
	}
	info := fc.Info.Segment.Value(src)
	brace := bytes.IndexByte(info, '{')
	if brace < 0 {
		return
	}
	attrs, ok := parser.ParseAttributes(text.NewReader(info[brace:]))
	if !ok {
		return
	}
	for _, attr := range attrs {
		fc.SetAttribute(attr.Name, attr.Value)
	}
}

// withBlockIDs returns the parser option that runs blockIDTransformer.
// Priority 110 sequences it after headingAnchorTransformer (100) so heading
// ids are assigned first; the two never touch the same node kinds, so the
// ordering is for clarity rather than correctness.
func withBlockIDs() parser.Option {
	return parser.WithASTTransformers(
		util.Prioritized(blockIDTransformer{}, 110),
	)
}

// blockIDOf returns the stamped `blk-N` id for a node, or "" if absent.
func blockIDOf(n ast.Node) []byte {
	if v, ok := n.AttributeString(string(blockIDAttr)); ok {
		if b, ok := v.([]byte); ok {
			return b
		}
	}
	return nil
}

// blockIDFromAttrs pulls the stamped id out of a highlighting CodeBlockContext.
func blockIDFromAttrs(attrs highlighting.ImmutableAttributes) []byte {
	if attrs == nil {
		return nil
	}
	if v, ok := attrs.Get(blockIDAttr); ok {
		if b, ok := v.([]byte); ok {
			return b
		}
	}
	return nil
}

// writeOpenTag writes `<tag` plus an optional ` data-block-id="…"` attribute
// (id already has fixed `blk-N` shape, but it is escaped for defence in depth).
func writeBlockIDAttr(w util.BufWriter, id []byte) {
	if len(id) == 0 {
		return
	}
	_, _ = w.WriteString(` data-block-id="`)
	_, _ = w.Write(util.EscapeHTML(id))
	_ = w.WriteByte('"')
}

// blockIDPreWrapper is a chroma PreWrapper that reproduces chroma's default
// `<pre STYLE><code>` / `</code></pre>` wrapping and adds data-block-id to the
// code `<pre>`. It is installed per-block (via blockIDHighlightingOptions ->
// WithCodeBlockOptions) so it can close over that block's id.
type blockIDPreWrapper struct{ id []byte }

func (p blockIDPreWrapper) Start(code bool, styleAttr string) string {
	if code {
		return "<pre" + styleAttr + string(dataBlockIDInline(p.id)) + "><code>"
	}
	// Line-number pre (only emitted when linenos is requested); no id, matches
	// chroma's default so the code <pre> below carries the single anchor.
	return "<pre" + styleAttr + ">"
}

func (p blockIDPreWrapper) End(code bool) string {
	if code {
		return "</code></pre>"
	}
	return "</pre>"
}

// dataBlockIDInline renders ` data-block-id="…"` (or "") for string assembly.
func dataBlockIDInline(id []byte) []byte {
	if len(id) == 0 {
		return nil
	}
	var b bytes.Buffer
	b.WriteString(` data-block-id="`)
	b.Write(util.EscapeHTML(id))
	b.WriteByte('"')
	return b.Bytes()
}

// blockIDWrapperRenderer is the highlighting WrapperRenderer. It covers the
// NON-highlighted fallback path (unknown/absent language, or a tokeniser
// error) where the extension emits no `<pre><code>` of its own. In the
// highlighted path (ctx.Highlighted()), blockIDPreWrapper already stamped the
// `<pre>`, so this renders nothing to avoid a redundant wrapper element.
func blockIDWrapperRenderer(w util.BufWriter, ctx highlighting.CodeBlockContext, entering bool) {
	if ctx.Highlighted() {
		return
	}
	if entering {
		id := blockIDFromAttrs(ctx.Attributes())
		_, _ = w.WriteString("<pre")
		writeBlockIDAttr(w, id)
		_, _ = w.WriteString("><code")
		if lang, ok := ctx.Language(); ok && lang != nil {
			_, _ = w.WriteString(` class="language-`)
			_, _ = w.Write(util.EscapeHTML(lang))
			_ = w.WriteByte('"')
		}
		_ = w.WriteByte('>')
		return
	}
	_, _ = w.WriteString("</code></pre>\n")
}

// blockIDHighlightingOptions returns the highlighting-extension options that
// inject data-block-id into fenced code blocks. Both hooks read the id the
// transformer stamped on the node (surfaced through the CodeBlockContext), so
// they are safe on the shared, concurrently-used extension instance — all
// per-block state lives on the AST, never on the option closures.
func blockIDHighlightingOptions() []highlighting.Option {
	return []highlighting.Option{
		highlighting.WithCodeBlockOptions(func(ctx highlighting.CodeBlockContext) []chromahtml.Option {
			return []chromahtml.Option{
				chromahtml.WithPreWrapper(blockIDPreWrapper{id: blockIDFromAttrs(ctx.Attributes())}),
			}
		}),
		highlighting.WithWrapperRenderer(blockIDWrapperRenderer),
	}
}

// blockIDRenderer is a custom renderer.NodeRenderer for the two block kinds
// whose attributes neither the default html renderer nor the highlighting
// extension emit: indented CodeBlock and HTMLBlock. It reproduces their
// standard output with data-block-id added. Registered at priority 100 so it
// wins over the default html renderer (1000) for exactly these kinds; it does
// NOT register FencedCodeBlock, leaving that to the highlighting extension.
//
// It also owns inline RawHTML. In safe mode the default renderer drops raw
// HTML for `<!-- raw HTML omitted -->`, which ate placeholder text like the
// Gate's "reply credits=<n>" (it read "credits= or model=opus"). Here raw HTML,
// inline or block, renders as its escaped source instead: the reader sees
// exactly what the writer typed and no tag reaches the page (gd-oNoa0zf55W).
type blockIDRenderer struct {
	unsafe bool
	writer html.Writer
}

// RegisterFuncs implements renderer.NodeRenderer.
func (r *blockIDRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindCodeBlock, r.renderCodeBlock)
	reg.Register(ast.KindHTMLBlock, r.renderHTMLBlock)
	reg.Register(ast.KindRawHTML, r.renderRawHTML)
}

// renderRawHTML mirrors renderer/html's renderRawHTML, except that safe mode
// writes the escaped source in place of the omission comment.
func (r *blockIDRenderer) renderRawHTML(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkSkipChildren, nil
	}
	n := node.(*ast.RawHTML)
	for i := range n.Segments.Len() {
		segment := n.Segments.At(i)
		v := segment.Value(source)
		if r.unsafe {
			_, _ = w.Write(v)
		} else {
			_, _ = w.Write(util.EscapeHTML(v))
		}
	}
	return ast.WalkSkipChildren, nil
}

// renderCodeBlock mirrors renderer/html's renderCodeBlock, adding the id.
func (r *blockIDRenderer) renderCodeBlock(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		_, _ = w.WriteString("<pre")
		writeBlockIDAttr(w, blockIDOf(n))
		_, _ = w.WriteString("><code>")
		r.writeLines(w, source, n)
	} else {
		_, _ = w.WriteString("</code></pre>\n")
	}
	return ast.WalkContinue, nil
}

// renderHTMLBlock mirrors renderer/html's renderHTMLBlock, wrapping the block
// in a `<div data-block-id>` so even a raw-HTML block is anchorable. In safe
// mode (the production default) the body is the block's escaped source.
func (r *blockIDRenderer) renderHTMLBlock(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	n := node.(*ast.HTMLBlock)
	if entering {
		_, _ = w.WriteString("<div")
		writeBlockIDAttr(w, blockIDOf(n))
		_ = w.WriteByte('>')
		_ = w.WriteByte('\n')
		if r.unsafe {
			l := n.Lines().Len()
			for i := range l {
				line := n.Lines().At(i)
				r.writer.SecureWrite(w, line.Value(source))
			}
		} else {
			r.writeEscapedLines(w, source, n)
		}
	} else {
		if n.HasClosure() {
			if r.unsafe {
				closure := n.ClosureLine
				r.writer.SecureWrite(w, closure.Value(source))
			} else {
				_, _ = w.Write(util.EscapeHTML(n.ClosureLine.Value(source)))
			}
		}
		_, _ = w.WriteString("</div>\n")
	}
	return ast.WalkContinue, nil
}

// writeLines mirrors renderer/html's (*Renderer).writeLines.
func (r *blockIDRenderer) writeLines(w util.BufWriter, source []byte, n ast.Node) {
	l := n.Lines().Len()
	for i := range l {
		line := n.Lines().At(i)
		r.writer.RawWrite(w, line.Value(source))
	}
}

// writeEscapedLines writes a node's lines as escaped text, for safe mode.
func (r *blockIDRenderer) writeEscapedLines(w util.BufWriter, source []byte, n ast.Node) {
	l := n.Lines().Len()
	for i := range l {
		line := n.Lines().At(i)
		_, _ = w.Write(util.EscapeHTML(line.Value(source)))
	}
}

// withBlockIDRenderers returns the renderer option registering blockIDRenderer.
// Both production goldmark instances run with Unsafe=false, so unsafe defaults
// off; the flag exists so the renderer stays faithful if Unsafe is ever set.
func withBlockIDRenderers() renderer.Option {
	return renderer.WithNodeRenderers(
		util.Prioritized(&blockIDRenderer{unsafe: false, writer: html.DefaultWriter}, 100),
	)
}
