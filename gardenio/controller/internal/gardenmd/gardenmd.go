// Package gardenmd is /garden's one Markdown renderer: the text an agent
// writes (a decision's context above all) rendered as safe HTML for the
// gardenhub Mailbox and the Commons cards, so every surface shows the same
// headings, lists, code and links (epic gd-hmwSI9lwKe, step 3:
// gd-OSI1uK35Wa). The bead panel builds the same Markdown node by node in
// web/workbench.js markdown(); this is its server-side twin.
//
// Safe means: goldmark with GFM and hard wraps, never HTML from the text (raw
// HTML shows as the text it is, and a javascript: or data: link loses its
// href), a link out
// opens in a new tab, headings start at h4 so a card never grows a second
// page heading (rubric C11), a picture loads only through the viewer's
// /image proxy (any other is a link, as on the bead page), and a bead id in
// the text is a link to the garden's bead view (rubric C12).
package gardenmd

import (
	"bytes"
	"html/template"
	"net/url"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

// FoldAt is the length, in runes, past which Block folds the text behind
// "Show more" (commons-ux-rubric C11: long context folds after ~600
// characters, never by a hard clamp).
const FoldAt = 600

// Options are what a surface supplies.
type Options struct {
	// Base is the site's base path ("" or "/x"): bead links and proxied
	// pictures hang off it.
	Base string
	// BeadLink renders a bead id named in the text as its link (the Commons
	// labels it by the bead's title); nil links it to /garden?bead=<id> by
	// its id, which web/titles.js dresses as the title on the page.
	BeadLink func(id string) template.HTML
}

var optsKey = parser.NewContextKey()

var md = goldmark.New(
	goldmark.WithExtensions(extension.GFM),
	goldmark.WithParserOptions(parser.WithASTTransformers(util.Prioritized(transformer{}, 999))),
	goldmark.WithRendererOptions(html.WithHardWraps(), renderer.WithNodeRenderers(util.Prioritized(beadRenderer{}, 100))),
)

// Render renders text as safe HTML. Empty text is "".
func Render(src string, o Options) template.HTML {
	src = strings.TrimSpace(src)
	if src == "" {
		return ""
	}
	pc := parser.NewContext()
	pc.Set(optsKey, &o)
	var buf bytes.Buffer
	if err := md.Convert([]byte(src), &buf, parser.WithContext(pc)); err != nil {
		return template.HTML("<p>" + template.HTMLEscapeString(src) + "</p>")
	}
	return template.HTML(buf.String())
}

// Block renders text in a <div class="cls md">, and when it is longer than
// FoldAt runes folds it: the head shows, then a "Show more" that opens the
// whole in place (a <details>, so it needs no script and survives the
// Mailbox's row clones and stream patches; garden.css .md-fold).
func Block(cls, src string, o Options) template.HTML {
	h := Render(src, o)
	if h == "" {
		return ""
	}
	cls = strings.TrimSpace(cls + " md")
	if utf8.RuneCountInString(strings.TrimSpace(src)) <= FoldAt {
		return template.HTML(`<div class="` + template.HTMLEscapeString(cls) + `">` + string(h) + `</div>`)
	}
	return template.HTML(`<div class="` + template.HTMLEscapeString(cls) + ` md-fold"><div class="md-body">` + string(h) +
		`</div><details class="md-more"><summary><span class="md-more-l">Show more</span><span class="md-less-l">Show less</span></summary></details></div>`)
}

// beadID is the rule of web/titles.js split() and gardenhub's mentionIDs: two
// lowercase letters, a dash and ten letters or digits, at least one of them a
// capital or a digit ("re-introduced" stays a word), not glued to a '/' (the
// last segment of a branch path is the branch's).
var beadID = regexp.MustCompile(`\b[a-z]{2}-[A-Za-z0-9]{10}\b`)

func isBeadID(s string, i, j int) bool {
	if !strings.ContainsAny(s[i+3:j], "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789") {
		return false
	}
	return !(i > 0 && s[i-1] == '/') && !(j < len(s) && s[j] == '/')
}

// kindBead is a bead id cut out of a text node, rendered by the surface's
// BeadLink.
var kindBead = ast.NewNodeKind("GardenBead")

type beadNode struct {
	ast.BaseInline
	ID string
	o  *Options
}

func (n *beadNode) Kind() ast.NodeKind { return kindBead }
func (n *beadNode) Dump(source []byte, level int) {
	ast.DumpHelper(n, source, level, map[string]string{"ID": n.ID}, nil)
}

type beadRenderer struct{}

// RegisterFuncs renders bead ids, and raw HTML as the text it is (an agent's
// "<placeholder>" reads as written, as in web/workbench.js markdown()), where
// goldmark's safe mode would drop it. Its priority (100) is over the html
// renderer's (1000; a lower number wins), so these win.
func (beadRenderer) RegisterFuncs(r renderer.NodeRendererFuncRegisterer) {
	r.Register(kindBead, renderBead)
	r.Register(ast.KindRawHTML, renderRawHTML)
	r.Register(ast.KindHTMLBlock, renderHTMLBlock)
}

func renderRawHTML(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if entering {
		segs := n.(*ast.RawHTML).Segments
		for i := range segs.Len() {
			seg := segs.At(i)
			_, _ = w.WriteString(template.HTMLEscapeString(string(seg.Value(src))))
		}
	}
	return ast.WalkSkipChildren, nil
}

func renderHTMLBlock(w util.BufWriter, src []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	b := n.(*ast.HTMLBlock)
	var lines []string
	for i := range b.Lines().Len() {
		line := b.Lines().At(i)
		lines = append(lines, string(line.Value(src)))
	}
	if b.HasClosure() {
		lines = append(lines, string(b.ClosureLine.Value(src)))
	}
	text := strings.TrimRight(strings.Join(lines, ""), "\n")
	_, _ = w.WriteString("<p>" + strings.ReplaceAll(template.HTMLEscapeString(text), "\n", "<br>\n") + "</p>\n")
	return ast.WalkContinue, nil
}

func renderBead(w util.BufWriter, _ []byte, n ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	b := n.(*beadNode)
	if b.o.BeadLink != nil {
		_, _ = w.WriteString(string(b.o.BeadLink(b.ID)))
		return ast.WalkContinue, nil
	}
	e := template.HTMLEscapeString(b.ID)
	_, _ = w.WriteString(`<a class="bl" href="` + template.HTMLEscapeString(b.o.Base+"/garden?bead="+url.QueryEscape(b.ID)) +
		`" data-bead="` + e + `" title="` + e + `">` + e + `</a>`)
	return ast.WalkContinue, nil
}

type transformer struct{}

// Transform shifts headings down to h4, routes pictures through the image
// proxy, opens links out in a new tab and cuts bead ids out of text.
func (transformer) Transform(doc *ast.Document, reader text.Reader, pc parser.Context) {
	o, _ := pc.Get(optsKey).(*Options)
	if o == nil {
		o = &Options{}
	}
	src := reader.Source()
	var texts []*ast.Text
	var images []*ast.Image
	var autos []*ast.AutoLink
	var unsafe []ast.Node
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *ast.Heading:
			v.Level = min(6, v.Level+3)
		case *ast.Link:
			if !safeHref(string(v.Destination)) {
				unsafe = append(unsafe, v)
			} else if external(string(v.Destination)) {
				v.SetAttributeString("target", "_blank")
				v.SetAttributeString("rel", "noopener noreferrer")
			}
			return ast.WalkSkipChildren, nil // a link's own words are never a second link
		case *ast.AutoLink:
			if v.AutoLinkType == ast.AutoLinkURL && !safeHref(string(v.URL(src))) {
				unsafe = append(unsafe, v)
			} else if v.AutoLinkType == ast.AutoLinkURL {
				autos = append(autos, v)
			}
			return ast.WalkSkipChildren, nil
		case *ast.Image:
			images = append(images, v)
			return ast.WalkSkipChildren, nil
		case *ast.CodeSpan, *ast.CodeBlock, *ast.FencedCodeBlock, *ast.HTMLBlock, *ast.RawHTML:
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			texts = append(texts, v)
		}
		return ast.WalkContinue, nil
	})
	for _, img := range images {
		picture(img, o.Base)
	}
	for _, a := range autos {
		shortLink(a, src)
	}
	for _, t := range texts {
		splitBeads(t, src, o)
	}
	for _, n := range unsafe {
		unlink(n, src)
	}
}

// safeHref is web/workbench.js safeHref: an http(s) or mailto: URL, a path on
// this site or a fragment. goldmark only blanks the scripts it knows of, so
// tel:, vscode: or data:image/svg+xml would link; any such link reads as its words.
func safeHref(u string) bool {
	l := strings.ToLower(u)
	return external(u) || strings.HasPrefix(l, "mailto:") || (strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//")) || strings.HasPrefix(u, "#")
}

// unlink replaces a link (or autolink) by its words, unlinked.
func unlink(n ast.Node, src []byte) {
	parent := n.Parent()
	if parent == nil {
		return
	}
	if a, ok := n.(*ast.AutoLink); ok {
		parent.ReplaceChild(parent, n, ast.NewString(a.Label(src)))
		return
	}
	for c := n.FirstChild(); c != nil; {
		next := c.NextSibling()
		parent.InsertBefore(parent, n, c)
		c = next
	}
	parent.RemoveChild(parent, n)
}

// external reports a link that leaves the site: an absolute http(s) URL.
func external(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "https://") || strings.HasPrefix(l, "http://")
}

// shortLink turns a bare URL into a link out labelled by its host and last two
// path segments, the whole URL in its tooltip (the Commons linkify's label), so
// a 120-character URL does not run across a card (rubric C2).
func shortLink(a *ast.AutoLink, src []byte) {
	raw := string(a.URL(src))
	u, err := url.Parse(raw)
	parent := a.Parent()
	if err != nil || u.Host == "" || parent == nil || !external(raw) {
		return
	}
	l := ast.NewLink()
	l.Destination = []byte(raw)
	l.Title = []byte(raw)
	l.SetAttributeString("target", "_blank")
	l.SetAttributeString("rel", "noopener noreferrer")
	l.AppendChild(l, ast.NewString([]byte(urlLabel(u))))
	parent.ReplaceChild(parent, a, l)
}

// urlLabel is a URL as a link reads it: its host, and its last two path
// segments after "…" when there are more (commons render.go urlLabel).
func urlLabel(u *url.URL) string {
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	switch {
	case len(segs) == 0:
		return u.Host
	case len(segs) > 2:
		return u.Host + "/…/" + strings.Join(segs[len(segs)-2:], "/")
	}
	return u.Host + "/" + strings.Join(segs, "/")
}

// splitBeads cuts each bead id out of t into a bead link, keeping the text
// around it, and t's line break on the last piece.
func splitBeads(t *ast.Text, src []byte, o *Options) {
	seg, parent := t.Segment, t.Parent()
	s := string(seg.Value(src))
	if parent == nil || !strings.Contains(s, "-") {
		return
	}
	var ms [][]int
	for _, m := range beadID.FindAllStringIndex(s, -1) {
		if isBeadID(s, m[0], m[1]) {
			ms = append(ms, m)
		}
	}
	if len(ms) == 0 {
		return
	}
	soft, hard := t.SoftLineBreak(), t.HardLineBreak()
	var prev ast.Node = t
	add := func(n ast.Node) { parent.InsertAfter(parent, prev, n); prev = n }
	t.Segment = text.NewSegment(seg.Start, seg.Start+ms[0][0])
	t.SetSoftLineBreak(false)
	t.SetHardLineBreak(false)
	at := ms[0][0]
	for _, m := range ms {
		if m[0] > at {
			add(ast.NewTextSegment(text.NewSegment(seg.Start+at, seg.Start+m[0])))
		}
		add(&beadNode{ID: s[m[0]:m[1]], o: o})
		at = m[1]
	}
	last := ast.NewTextSegment(text.NewSegment(seg.Start+at, seg.Stop))
	add(last)
	last.SetSoftLineBreak(soft)
	last.SetHardLineBreak(hard)
}

// picture points img at the viewer's image proxy, or, for a picture the proxy
// does not serve, turns it into a link to it ("picture: <alt>"), so the page
// never loads a picture from anywhere else (web/workbench.js mdInto's rule).
func picture(img *ast.Image, base string) {
	if p := PicSrc(base, string(img.Destination)); p != "" {
		img.Destination = []byte(p)
		img.SetAttributeString("loading", "lazy")
		img.SetAttributeString("decoding", "async")
		img.SetAttributeString("referrerpolicy", "no-referrer")
		return
	}
	parent := img.Parent()
	if parent == nil {
		return
	}
	var repl ast.Node
	if external(string(img.Destination)) {
		l := ast.NewLink()
		l.Destination = img.Destination
		l.SetAttributeString("target", "_blank")
		l.SetAttributeString("rel", "noopener noreferrer")
		repl = l
	} else {
		repl = ast.NewEmphasis(1)
	}
	repl.AppendChild(repl, ast.NewString([]byte("picture: ")))
	for c := img.FirstChild(); c != nil; {
		next := c.NextSibling()
		repl.AppendChild(repl, c)
		c = next
	}
	parent.ReplaceChild(parent, img, repl)
}

var s3Host = regexp.MustCompile(`^[a-z0-9.-]+\.s3(?:\.[a-z0-9-]+)?\.amazonaws\.com$`)

// PicSrc is web/workbench.js picSrc / mdPicSrc: the /image/ path of a
// picture the viewer's proxy serves (absolute, or root-relative on this
// site), under base, or "" for any other. The uploader's raw private-bucket
// S3 URL is the same key, so it goes through the proxy too.
func PicSrc(base, u string) string {
	if strings.HasPrefix(u, "/") && !strings.HasPrefix(u, "//") {
		u = "https://site.invalid" + u
	}
	x, err := url.Parse(u)
	if err != nil || (x.Scheme != "https" && x.Scheme != "http") {
		return ""
	}
	x.Path = path.Clean(x.Path) // /image/../garden is /garden to the browser (new URL in the JS does the same)
	i := strings.Index(x.Path, "/image/")
	if i < 0 && s3Host.MatchString(x.Hostname()) && strings.Index(x.Path, "/images/") > 0 {
		return base + "/image" + x.Path
	}
	if i < 0 || len(x.Path) <= i+7 {
		return ""
	}
	return base + x.Path[i:]
}
