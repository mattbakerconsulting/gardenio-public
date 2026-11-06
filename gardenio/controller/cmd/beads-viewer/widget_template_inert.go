package main

import (
	"strings"

	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// inertWidgetHTML makes the output of a widget-type's template_source unable
// to run script or send a request before it is marked template.HTML
// (gd-a2s0GZExxr). html/template trusts the template text itself, so a literal
// <script>, onclick= or hx-post= in template_source survives Execute; this
// pass removes them while keeping the structure, class, data-* and style= the
// ported widgets' CSS reads (freehtmx.Sanitize's w- namespace would strip
// those). The viewer's pages carry inline scripts, so a page CSP cannot do
// this job; the preview fragment route also gets freehtmx.StrictCSP.
//
// It drops: elements that run, load or submit (widgetDroppedElements) with
// their content; on* event handlers; hx-* / data-hx-* (htmx would issue the
// request with the operator's cookies); srcdoc, formaction, ping; and a URL
// attribute whose scheme is not http, https or mailto. A fragment that does
// not parse renders as nothing.
func inertWidgetHTML(in string) string {
	if in == "" {
		return ""
	}
	nodes, err := html.ParseFragment(strings.NewReader(in), &html.Node{
		Type:     html.ElementNode,
		Data:     "body",
		DataAtom: atom.Body,
	})
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, n := range nodes {
		if dropWidgetNode(n) {
			continue
		}
		scrubWidgetNode(n)
		if err := html.Render(&b, n); err != nil {
			return ""
		}
	}
	return b.String()
}

// widgetDroppedElements run script, load a document or plugin, change how
// relative URLs or the page resolve, or submit a request. svg and math are
// dropped whole: foreign content re-parses differently (mXSS).
var widgetDroppedElements = map[atom.Atom]bool{
	atom.Script: true, atom.Noscript: true, atom.Template: true, atom.Style: true,
	atom.Iframe: true, atom.Frame: true, atom.Frameset: true,
	atom.Object: true, atom.Embed: true, atom.Applet: true,
	atom.Base: true, atom.Meta: true, atom.Link: true, atom.Form: true,
	atom.Svg: true, atom.Math: true,
}

// widgetURLAttrs carry a URL the browser may navigate to or fetch.
var widgetURLAttrs = map[string]bool{
	"href": true, "src": true, "action": true, "xlink:href": true, "poster": true,
	"background": true, "cite": true, "data": true, "codebase": true, "manifest": true,
}

func dropWidgetNode(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	if widgetDroppedElements[n.DataAtom] {
		return true
	}
	// Foreign or unknown elements parsed without an atom, by name.
	switch strings.ToLower(n.Data) {
	case "svg", "math", "portal":
		return true
	}
	return false
}

func scrubWidgetNode(n *html.Node) {
	if n.Type == html.ElementNode && len(n.Attr) > 0 {
		kept := n.Attr[:0]
		for _, a := range n.Attr {
			if keepWidgetAttr(a) {
				kept = append(kept, a)
			}
		}
		n.Attr = kept
	}
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if dropWidgetNode(c) {
			n.RemoveChild(c)
		} else {
			scrubWidgetNode(c)
		}
		c = next
	}
}

func keepWidgetAttr(a html.Attribute) bool {
	key := strings.ToLower(a.Key)
	if a.Namespace != "" {
		key = strings.ToLower(a.Namespace) + ":" + key
	}
	switch {
	case strings.HasPrefix(key, "on"),
		strings.HasPrefix(key, "hx-"),
		strings.HasPrefix(key, "data-hx-"),
		key == "srcdoc", key == "formaction", key == "ping":
		return false
	case widgetURLAttrs[key]:
		return safeWidgetURL(a.Val)
	}
	return true
}

// safeWidgetURL reports whether a URL attribute value is relative or uses an
// http, https or mailto scheme. Whitespace and control characters, which
// browsers ignore inside a scheme ("java\tscript:"), are removed first.
func safeWidgetURL(v string) bool {
	v = strings.Map(func(r rune) rune {
		if r <= ' ' || r == 0x7f {
			return -1
		}
		return r
	}, v)
	colon := strings.IndexByte(v, ':')
	if colon < 0 {
		return true
	}
	if i := strings.IndexAny(v, "/?#"); i >= 0 && i < colon {
		return true
	}
	switch strings.ToLower(v[:colon]) {
	case "http", "https", "mailto":
		return true
	}
	return false
}
