// Package bridge — markdown → Atlassian Document Format rendering.
//
// Agents write markdown: the F4 spawn prompt hands them markdown history and
// `gb jira reply` bodies routinely carry **bold**, lists, fences and links.
// The inbound direction already flattens ADF to markdown (adfToMarkdown); this
// file is the missing outbound half (H1, epic ka-0000007401) so a reply
// renders as rich text in Jira instead of literal punctuation.
//
// The converter is deliberately conservative: it maps only the block and
// inline constructs ADF has a direct equivalent for, and reports ok=false on
// anything else (images, raw HTML, tables, …). Callers MUST treat ok=false as
// "post the plain-text form" — the fallback keeps every reply at least as
// readable as before this renderer existed.
package bridge

import (
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	gmtext "github.com/yuin/goldmark/text"
)

// adfBlocksFromMarkdown converts CommonMark markdown into ADF top-level block
// nodes (paragraph, heading, bulletList, orderedList, codeBlock, blockquote,
// rule). ok=false means the input used a construct with no ADF mapping (or
// produced no content) and the caller must fall back to plain rendering.
func adfBlocksFromMarkdown(markdown string) ([]any, bool) {
	return adfBlocksFromMarkdownWithMedia(markdown, nil)
}

// adfBlocksFromMarkdownWithMedia is adfBlocksFromMarkdown plus inline-image
// support (ka-0000001730).
//
// media maps a markdown image destination to a Jira ATTACHMENT ID (from
// AddAttachment). An image whose destination resolves becomes a real ADF
// mediaSingle block that renders inline; an image that does NOT resolve
// degrades to a clickable link rather than aborting the whole conversion.
//
// That degradation is the point. Before this, ANY image in the body made
// adfBlocksFromMarkdown return ok=false, and the caller's documented fallback
// is "post the plain-text form" — so one `![shot](url)` silently turned an
// entire rich comment into literal markdown source. Losing the inline
// rendering of one unresolvable image is a far better failure than losing the
// formatting of everything around it.
//
// A nil map keeps the historical behaviour for non-image input and still
// degrades images to links instead of aborting.
func adfBlocksFromMarkdownWithMedia(markdown string, media map[string]string) ([]any, bool) {
	source := []byte(markdown)
	root := goldmark.New().Parser().Parse(gmtext.NewReader(source))
	conv := &adfConverter{source: source, media: media}
	blocks, ok := conv.blocks(root)
	if !ok || len(blocks) == 0 {
		return nil, false
	}
	return blocks, true
}

// adfConverter walks a goldmark AST, carrying the original source for text
// segment extraction.
type adfConverter struct {
	source []byte
	// media maps an image destination → Jira attachment id. See
	// adfBlocksFromMarkdownWithMedia.
	media map[string]string
	// pendingMedia collects mediaSingle blocks produced by images encountered
	// while converting the CURRENT block's inline content. ADF has no inline
	// image node — media is block-level — so an image inside a paragraph is
	// hoisted out and emitted as a sibling block immediately after it. Drained
	// by blocks() after each child.
	pendingMedia []any
}

// resolveMedia returns the attachment id registered for an image destination.
// Accepts a bare destination and the explicit `attachment:<id>` form, so a
// caller that already knows the id can reference it without round-tripping
// through a URL.
func (c *adfConverter) resolveMedia(dest string) string {
	if id, ok := strings.CutPrefix(dest, "attachment:"); ok {
		return strings.TrimSpace(id)
	}
	return c.media[dest]
}

// blocks converts parent's children to ADF block nodes. Empty blocks (e.g. a
// paragraph whose inline content collapsed to nothing) are skipped.
func (c *adfConverter) blocks(parent ast.Node) ([]any, bool) {
	var out []any
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		block, ok := c.block(n)
		if !ok {
			return nil, false
		}
		if block != nil {
			out = append(out, block)
		}
		// Hoist any images found inside that block's inline content out as
		// sibling blocks: ADF media is block-level, so an image in a paragraph
		// cannot stay nested. Emitting immediately after keeps the reading
		// order the author wrote.
		if len(c.pendingMedia) > 0 {
			out = append(out, c.pendingMedia...)
			c.pendingMedia = nil
		}
	}
	return out, true
}

// block converts a single block-level node. A nil, true return means "skip
// this node" (it rendered empty); nil, false aborts the whole conversion.
func (c *adfConverter) block(n ast.Node) (map[string]any, bool) {
	switch node := n.(type) {
	case *ast.Paragraph, *ast.TextBlock:
		// TextBlock is the tight-list-item variant of Paragraph.
		inline, ok := c.inlineChildren(n, nil)
		if !ok {
			return nil, false
		}
		if len(inline) == 0 {
			return nil, true
		}
		return map[string]any{"type": "paragraph", "content": inline}, true

	case *ast.Heading:
		inline, ok := c.inlineChildren(n, nil)
		if !ok {
			return nil, false
		}
		if len(inline) == 0 {
			return nil, true
		}
		return map[string]any{
			"type":    "heading",
			"attrs":   map[string]any{"level": node.Level},
			"content": inline,
		}, true

	case *ast.FencedCodeBlock:
		return c.codeBlock(string(node.Language(c.source)), n), true

	case *ast.CodeBlock:
		return c.codeBlock("", n), true

	case *ast.List:
		return c.list(node)

	case *ast.Blockquote:
		children, ok := c.blocks(n)
		if !ok {
			return nil, false
		}
		if len(children) == 0 {
			return nil, true
		}
		return map[string]any{"type": "blockquote", "content": children}, true

	case *ast.ThematicBreak:
		return map[string]any{"type": "rule"}, true

	default:
		// HTMLBlock, tables, and anything future goldmark versions add: no ADF
		// mapping — abort so the caller degrades to plain text.
		return nil, false
	}
}

// codeBlock renders a (fenced or indented) code block. ADF codeBlock content
// is a single unmarked text node; the language rides in attrs when present.
func (c *adfConverter) codeBlock(lang string, n ast.Node) map[string]any {
	var sb strings.Builder
	lines := n.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		sb.Write(seg.Value(c.source))
	}
	code := strings.TrimRight(sb.String(), "\n")

	block := map[string]any{"type": "codeBlock"}
	if lang != "" {
		block["attrs"] = map[string]any{"language": lang}
	}
	// An empty text node is invalid ADF; an empty code block simply carries no
	// content array.
	if code != "" {
		block["content"] = []any{map[string]any{"type": "text", "text": code}}
	}
	return block
}

// list renders an ordered or bullet list. Items whose content collapsed to
// nothing are dropped (an ADF listItem requires at least one block); a list
// with no surviving items is skipped entirely.
func (c *adfConverter) list(node *ast.List) (map[string]any, bool) {
	listType := "bulletList"
	var attrs map[string]any
	if node.IsOrdered() {
		listType = "orderedList"
		attrs = map[string]any{"order": node.Start}
	}

	var items []any
	for li := node.FirstChild(); li != nil; li = li.NextSibling() {
		blocks, ok := c.blocks(li)
		if !ok {
			return nil, false
		}
		if len(blocks) == 0 {
			continue
		}
		items = append(items, map[string]any{"type": "listItem", "content": blocks})
	}
	if len(items) == 0 {
		return nil, true
	}

	out := map[string]any{"type": listType, "content": items}
	if attrs != nil {
		out["attrs"] = attrs
	}
	return out, true
}

// inlineChildren converts parent's children to ADF inline nodes, applying
// marks to every produced text node.
func (c *adfConverter) inlineChildren(parent ast.Node, marks []any) ([]any, bool) {
	var out []any
	for n := parent.FirstChild(); n != nil; n = n.NextSibling() {
		nodes, ok := c.inline(n, marks)
		if !ok {
			return nil, false
		}
		out = append(out, nodes...)
	}
	return out, true
}

// inline converts a single inline node under the given active marks.
func (c *adfConverter) inline(n ast.Node, marks []any) ([]any, bool) {
	switch node := n.(type) {
	case *ast.Text:
		var out []any
		if s := string(node.Segment.Value(c.source)); s != "" {
			out = append(out, adfMarkedText(s, marks))
		}
		// Both soft and hard markdown line breaks become ADF hardBreaks,
		// matching adfInlineText's newline promotion so line structure inside a
		// paragraph is preserved either way.
		if node.SoftLineBreak() || node.HardLineBreak() {
			out = append(out, map[string]any{"type": "hardBreak"})
		}
		return out, true

	case *ast.String:
		if len(node.Value) == 0 {
			return nil, true
		}
		return []any{adfMarkedText(string(node.Value), marks)}, true

	case *ast.Emphasis:
		markType := "em"
		if node.Level >= 2 {
			markType = "strong"
		}
		return c.inlineChildren(n, appendMark(marks, map[string]any{"type": markType}))

	case *ast.CodeSpan:
		var sb strings.Builder
		for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
			if t, isText := ch.(*ast.Text); isText {
				sb.Write(t.Segment.Value(c.source))
			}
		}
		s := sb.String()
		if s == "" {
			return nil, true
		}
		return []any{adfMarkedText(s, appendCodeMark(marks))}, true

	case *ast.Link:
		return c.inlineChildren(n, appendMark(marks, adfLinkMark(string(node.Destination))))

	case *ast.AutoLink:
		u := string(node.URL(c.source))
		label := string(node.Label(c.source))
		if node.AutoLinkType == ast.AutoLinkEmail && !strings.HasPrefix(u, "mailto:") {
			u = "mailto:" + u
		}
		if label == "" {
			return nil, true
		}
		return []any{adfMarkedText(label, appendMark(marks, adfLinkMark(u)))}, true

	case *ast.Image:
		return c.image(node, marks)

	default:
		// RawHTML, footnotes, …: no ADF inline mapping — abort.
		return nil, false
	}
}

// image converts a markdown image. ADF has no inline image node, so a
// resolvable image is queued as a block-level mediaSingle (drained by blocks())
// and contributes nothing inline. An UNRESOLVABLE image degrades to a link so
// the reader can still reach it — see adfBlocksFromMarkdownWithMedia for why
// degrading beats aborting.
func (c *adfConverter) image(node *ast.Image, marks []any) ([]any, bool) {
	dest := string(node.Destination)
	alt := c.getNodeText(node)

	if id := c.resolveMedia(dest); id != "" {
		c.pendingMedia = append(c.pendingMedia, adfMediaSingle(id))
		return nil, true
	}

	if dest == "" {
		return nil, true
	}
	if alt == "" {
		alt = dest
	}
	return []any{adfMarkedText(alt, appendMark(marks, adfLinkMark(dest)))}, true
}

// getNodeText extracts plain text from a node's children (e.g. image alt text).
func (c *adfConverter) getNodeText(n ast.Node) string {
	var sb strings.Builder
	for ch := n.FirstChild(); ch != nil; ch = ch.NextSibling() {
		if t, isText := ch.(*ast.Text); isText {
			sb.Write(t.Segment.Value(c.source))
		}
	}
	return sb.String()
}

// adfMarkedText builds an ADF text node carrying the given marks (omitted when
// empty — ADF rejects an empty marks array on some surfaces).
func adfMarkedText(text string, marks []any) map[string]any {
	node := map[string]any{"type": "text", "text": text}
	if len(marks) > 0 {
		node["marks"] = marks
	}
	return node
}

// adfLinkMark builds an ADF link mark for href (the mark shape adfLinkText
// inlines; factored here so the converter can stack it with em/strong/code).
func adfLinkMark(href string) map[string]any {
	return map[string]any{
		"type":  "link",
		"attrs": map[string]any{"href": href},
	}
}

// appendMark copies marks and appends one more. A plain append would alias the
// backing array across sibling branches of the walk and cross-contaminate
// their mark stacks.
func appendMark(marks []any, mark map[string]any) []any {
	out := make([]any, 0, len(marks)+1)
	out = append(out, marks...)
	return append(out, mark)
}

// appendCodeMark adds the ADF `code` mark to a text node's active marks,
// dropping any mark `code` may not co-occur with first.
//
// In the ADF schema the `code` mark is exclusive with every other formatting
// mark EXCEPT `link` (see the "code" mark's `excludes` in Atlassian's schema:
// em, strong, strike, subsup, textColor, underline). So an inline code span
// nested under emphasis — the entirely valid markdown for a bold code span,
// bold-asterisks around a backtick-quoted word — otherwise produced a text node
// carrying both `strong` and `code`, which Jira Cloud rejects with a 400
// ("INVALID_INPUT ... marks") and the reply path surfaced as a bare 502.
// Keeping only `link` alongside `code` yields a node Jira accepts; the code
// span simply loses the surrounding bold/italic, which is a far better failure
// than the whole comment 400ing (ka-0000000661).
func appendCodeMark(marks []any) []any {
	out := make([]any, 0, len(marks)+1)
	for _, m := range marks {
		if markType(m) == "link" {
			out = append(out, m)
		}
	}
	return append(out, map[string]any{"type": "code"})
}

// markType returns the "type" of an ADF mark node, or "" if it is not a
// mark-shaped map (defensive — the converter only ever builds
// map[string]any marks).
func markType(m any) string {
	mm, ok := m.(map[string]any)
	if !ok {
		return ""
	}
	t, _ := mm["type"].(string)
	return t
}
