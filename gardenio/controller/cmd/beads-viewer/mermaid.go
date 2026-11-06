package main

// Mermaid diagram rendering in markdown (bundle docs, bead descriptions,
// comments) — plan step 2e (ka-0000005977), decision D7 resolved in-scope.
//
// A fenced code block tagged `mermaid` is turned into a dedicated AST node so
// the diagram source survives to the client verbatim (HTML-escaped) instead of
// being syntax-highlighted as a code listing. The client driver
// (templates/static/mermaid_render.js) lazy-loads the vendored mermaid library
// and promotes each `<pre class="mermaid-diagram">` into inline SVG.
//
// SECURITY — two independent layers, both must be defeated to execute script in
// the viewer's same-origin context (see the design of record on ka-0000005977):
//
//	Layer 1 (server-side, JS-independent): the renderer emits the diagram
//	source through util.EscapeHTML into a <pre><code>. A malicious label like
//	A["<img src=x onerror=alert(document.cookie)>"] reaches the DOM as inert
//	escaped text — never a live element — on every path (JS off, lib-load
//	failure, parse error). goldmark html.WithUnsafe stays OFF, so there is no
//	raw-HTML bypass.
//
//	Layer 2 (client-side): mermaid runs in securityLevel:'strict' with
//	htmlLabels:false, so the generated SVG is DOMPurify-sanitised, HTML-in-
//	labels is disabled, and `click NODE javascript:…` directives are refused.
//	A diagram-embedded %%{init:{securityLevel:'loose'}}%% directive CANNOT
//	downgrade strict mode: `securityLevel` sits in mermaid's immutable
//	`secure` deny-list and is stripped before every directive merge (verified
//	against the vendored v11.16.0 bundle; TestMermaidVersionPin guards it).
//
// This mirrors the hand-rolled goldmark AST-transformer + custom NodeRenderer
// pattern in anchor.go (headingAnchorTransformer / blockIDRenderer) — no new Go
// dependency is added.

import (
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// mermaidLang is the fenced-code info-string language that selects diagram
// rendering. Matching is case-insensitive and compares only the first
// space-delimited info word (fc.Language), so ```mermaid, ```Mermaid, and
// ```mermaid {theme=dark} all match while ```mermaidjs does NOT.
const mermaidLang = "mermaid"

// KindMermaidBlock is the NodeKind for a fenced block whose language is
// "mermaid". It replaces the FencedCodeBlock in the tree so the highlighting
// extension (which only registers KindFencedCodeBlock) never touches it.
var KindMermaidBlock = ast.NewNodeKind("MermaidBlock")

// mermaidBlock is the AST node a mermaid fence becomes. It embeds BaseBlock so
// it carries the fenced block's source Lines() (the diagram text) verbatim.
type mermaidBlock struct {
	ast.BaseBlock
}

// Kind implements ast.Node.
func (*mermaidBlock) Kind() ast.NodeKind { return KindMermaidBlock }

// IsRaw reports that the node's content is raw text (the diagram source),
// matching FencedCodeBlock — the renderer emits the lines directly, children
// are not walked.
func (*mermaidBlock) IsRaw() bool { return true }

// Dump implements ast.Node.
func (n *mermaidBlock) Dump(src []byte, level int) {
	ast.DumpHelper(n, src, level, nil, nil)
}

// mermaidTransformer rewrites every fenced code block whose language is
// "mermaid" into a mermaidBlock. Running as an AST transformer (rather than
// out-prioritising the highlighting renderer) removes the FencedCodeBlock kind
// the highlighting extension owns, so there is no renderer-priority contention.
type mermaidTransformer struct{}

// Transform implements parser.ASTTransformer. It collects the target fenced
// blocks first, then replaces them, so the tree is not mutated mid-walk.
func (mermaidTransformer) Transform(doc *ast.Document, reader text.Reader, _ parser.Context) {
	src := reader.Source()
	var targets []*ast.FencedCodeBlock
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		fc, ok := n.(*ast.FencedCodeBlock)
		if !ok {
			return ast.WalkContinue, nil
		}
		if isMermaidLang(fc.Language(src)) {
			targets = append(targets, fc)
		}
		return ast.WalkContinue, nil
	})
	for _, fc := range targets {
		mb := &mermaidBlock{}
		// Carry the fence body (the diagram source) onto the new node. The
		// segments reference the same source buffer, so no copy is made.
		mb.SetLines(fc.Lines())
		if parent := fc.Parent(); parent != nil {
			parent.ReplaceChild(parent, fc, mb)
		}
	}
}

// isMermaidLang reports whether a fenced block's language token selects mermaid
// rendering. lang is fc.Language(src) — the first space-delimited info word —
// so this is an exact, case-insensitive match on "mermaid" (no "mermaidjs"
// false positives).
func isMermaidLang(lang []byte) bool {
	if len(lang) != len(mermaidLang) {
		return false
	}
	for i := range lang {
		c := lang[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != mermaidLang[i] {
			return false
		}
	}
	return true
}

// withMermaid returns the parser option that runs mermaidTransformer. Priority
// 90 sequences it BEFORE headingAnchorTransformer (100) and blockIDTransformer
// (110): the FencedCodeBlock is replaced before block-IDs are stamped, so a
// diagram never receives a data-block-id (it is not a critique-anchor target)
// and the highlighting renderer (priority 200) never sees the node.
func withMermaid() parser.Option {
	return parser.WithASTTransformers(
		util.Prioritized(mermaidTransformer{}, 90),
	)
}

// mermaidRenderer is the custom renderer.NodeRenderer for KindMermaidBlock. It
// is the only renderer registered for that kind, so there is no contention with
// the default html renderer (1000) or the highlighting renderer (200).
type mermaidRenderer struct{}

// RegisterFuncs implements renderer.NodeRenderer.
func (r *mermaidRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(KindMermaidBlock, r.render)
}

// render emits the diagram as:
//
//		<pre class="mermaid-diagram" data-mermaid="1"><code class="language-mermaid">ESCAPED_SOURCE</code></pre>
//
//	  - class="mermaid-diagram" (not the bare "mermaid" mermaid's own startOnLoad
//	    scan looks for) is the driver's selector — mermaid can never auto-grab an
//	    un-driven node, and startOnLoad:false in the driver double-guards.
//	  - data-mermaid="1" is the explicit JS presence marker so a CSS rename cannot
//	    silently break detection.
//	  - The <pre><code> is BOTH the JS-off degradation (a readable code block,
//	    never blank, never a spinner) AND the single source of truth: the driver
//	    reads code.textContent (browser-decoded to the literal source) — there is
//	    no data-mermaid-src attribute and no source duplication.
//	  - Every source line is written through util.EscapeHTML exactly once (the
//	    same escaper anchor.go/block_id.go use), so `<`, `>`, `&`, `"` become
//	    entities — Layer 1 of the XSS defense.
func (r *mermaidRenderer) render(w util.BufWriter, source []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	_, _ = w.WriteString(`<pre class="mermaid-diagram" data-mermaid="1"><code class="language-mermaid">`)
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		line := lines.At(i)
		_, _ = w.Write(util.EscapeHTML(line.Value(source)))
	}
	_, _ = w.WriteString("</code></pre>\n")
	// The node is raw; its (absent) children must not be walked.
	return ast.WalkSkipChildren, nil
}

// withMermaidRenderer returns the renderer option registering mermaidRenderer.
// Priority 100 matches blockIDRenderer; it owns a disjoint node kind, so there
// is no contention.
func withMermaidRenderer() renderer.Option {
	return renderer.WithNodeRenderers(
		util.Prioritized(&mermaidRenderer{}, 100),
	)
}
