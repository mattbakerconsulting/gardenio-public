package decisionview

import (
	"bytes"
	"html/template"
	"strconv"
	"strings"
	"unicode/utf8"

	"gardenio/controller/internal/gardenmd"
)

// The evidence strip (gd-Pi7C83262N) is how every surface that shows a
// decision's context shows its evidence: a row of thumbnails under the
// context, each with its alt text and a "from <source bead title>" link, that
// opens whole in a lightbox. One renderer serves the beads viewer, the
// /garden Mailbox and the Commons card, so the three say the same; the bead
// panel builds the same markup in web/workbench.js evidenceStrip.
//
// A picture loads only through the site's image proxy (gardenmd.PicSrc, the
// /garden rule): an image link the proxy does not serve is left out of the
// strip, never fetched from its host. The lightbox is the browser's popover,
// so it needs no script: the thumbnail is its invoker, Escape or a click
// outside closes it.

// StripOptions says how a surface draws the strip.
type StripOptions struct {
	// Base is the site's base path; pictures load from Base + "/image/...".
	Base string
	// Slot names the surface in the strip's DOM ids, so a page that shows the
	// same decision twice (a Mailbox row and a bead panel) keeps them unique.
	Slot string
	// Href is a source bead's address on this surface; nil links nothing.
	Href func(id string) string
}

type stripFig struct {
	Src, Alt, Box, Href, From string
}

type stripView struct {
	ID   string
	N    int
	Figs []stripFig
}

// stripAltMax clips an image's alt text in its caption (the whole alt stays
// on the picture).
const stripAltMax = 80

// evidenceStripTmpl is the strip's markup. Styles: .dec-ev in the viewer's
// layout.html and in gardenhub's garden.css.
var evidenceStripTmpl = template.Must(template.New("dec-ev").Funcs(template.FuncMap{"clip": func(s string) string { return clipRunes(s, stripAltMax) }}).Parse(
	`<section class="dec-ev" aria-label="Screenshots from the beads this decision is about" data-decision-evidence="{{.ID}}" data-deck-no-swipe>` +
		`<h4 class="dec-ev-head">Screenshots <span class="dec-ev-n">{{.N}}</span></h4>` +
		`<ul class="dec-ev-list">{{range .Figs}}<li class="dec-ev-fig"><figure>` +
		`<button type="button" class="dec-ev-thumb" popovertarget="{{.Box}}" title="Enlarge: {{.Alt}}">` +
		`<img src="{{.Src}}" alt="{{.Alt}}" loading="lazy" decoding="async" referrerpolicy="no-referrer"></button>` +
		`<figcaption><span class="dec-ev-alt">{{clip .Alt}}</span>{{if .Href}} <span class="dec-ev-from">from <a href="{{.Href}}">{{.From}}</a></span>{{end}}</figcaption>` +
		`</figure>` +
		`<div class="dec-ev-box" id="{{.Box}}" popover>` +
		`<img src="{{.Src}}" alt="{{.Alt}}" loading="lazy" decoding="async" referrerpolicy="no-referrer">` +
		`<p class="dec-ev-box-cap"><span>{{.Alt}}</span>{{if .Href}} · from <a href="{{.Href}}">{{.From}}</a>{{end}} · <a href="{{.Src}}" target="_blank" rel="noopener noreferrer">Open full size</a></p>` +
		`<button type="button" class="dec-ev-close" popovertarget="{{.Box}}" popovertargetaction="hide">Close</button>` +
		`</div></li>{{end}}</ul></section>`))

// EvidenceStrip renders decision id's evidence images as the strip, or ""
// when none of them loads through the image proxy (a decision with no
// evidence shows no strip).
func EvidenceStrip(id string, imgs []Image, o StripOptions) template.HTML {
	v := stripView{ID: id}
	slot := o.Slot
	if slot == "" {
		slot = "ev"
	}
	for i, im := range imgs {
		src := gardenmd.PicSrc(o.Base, im.URL)
		if src == "" {
			continue
		}
		f := stripFig{Src: src, Alt: im.Alt, Box: "dec-ev-" + slot + "-" + id + "-" + strconv.Itoa(i)}
		if o.Href != nil && im.Source != "" {
			f.Href, f.From = o.Href(im.Source), firstNonBlank(im.SourceTitle, im.Source)
		}
		v.Figs = append(v.Figs, f)
	}
	if len(v.Figs) == 0 {
		return ""
	}
	v.N = len(v.Figs)
	var buf bytes.Buffer
	if err := evidenceStripTmpl.Execute(&buf, v); err != nil {
		return ""
	}
	return template.HTML(buf.String()) //nolint:gosec // produced by html/template above
}

// SourceCount is how many of a decision's evidence images one bead holds.
type SourceCount struct {
	ID, Title string
	N         int
}

// EvidenceBySource counts imgs per source bead, in the order the sources
// first appear: what a surface that cannot show the pictures says instead
// (the Slack card, which fetches image blocks anonymously).
func EvidenceBySource(imgs []Image) []SourceCount {
	var out []SourceCount
	at := map[string]int{}
	for _, im := range imgs {
		i, ok := at[im.Source]
		if !ok {
			i = len(out)
			at[im.Source] = i
			out = append(out, SourceCount{ID: im.Source, Title: im.SourceTitle})
		}
		out[i].N++
	}
	return out
}

func firstNonBlank(ss ...string) string {
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			return s
		}
	}
	return ""
}

func clipRunes(s string, n int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n-1])) + "…"
}
