package main

import (
	"bytes"
	"context"
	"html"
	"html/template"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/decisionview"
)

// decisionContextClampChars is the context length above which the block
// clamps to a few lines behind a "Show more" toggle. Shorter context shows in
// full; longer context still shows its opening lines expanded, so the
// options below stay in reach (gd-PNtilz1bgs: "expanded, with show-more
// after ~600 chars").
const decisionContextClampChars = 600

// decisionContextView is the context block every viewer surface that can
// resolve a decision renders ABOVE its options: the bead page card, /up, a
// plan's clarifying questions, the deck and its swapped-in card, and the
// inbox (epic gd-hmwSI9lwKe, step 2). The context is what the agent wrote to
// make the decision decidable; a resolve control is never armed over a
// surface that does not show it.
type decisionContextView struct {
	// ID is the decision bead's id, which keeps the show-more toggle's DOM id
	// unique when a page lists several decisions.
	ID string
	// HTML is the context rendered through decisionMarkdown; "" when the
	// decision has none. A block that is not Present says so out loud.
	HTML template.HTML
	// Long is true past decisionContextClampChars.
	Long bool
}

// decisionContextOf reads a decision's context through the shared read model
// (decisionview.Context: the context field, else the description) and
// renders it as Markdown.
func (s *Server) decisionContextOf(b *beadsapi.BeadDetail) decisionContextView {
	return newDecisionContextView(b, s.decisionMarkdown)
}

// newDecisionContextView is decisionContextOf for a caller holding only the
// Markdown renderer (buildDecisionView).
func newDecisionContextView(b *beadsapi.BeadDetail, renderMarkdown func(string) template.HTML) decisionContextView {
	if b == nil {
		return decisionContextView{}
	}
	v := decisionContextView{ID: b.ID}
	if text, _ := decisionview.Context(b); text != "" && renderMarkdown != nil {
		v.HTML = renderMarkdown(text)
		v.Long = len(text) > decisionContextClampChars
	}
	return v
}

// Present is true when the context shows the reader something: text left
// once the markup is stripped, or a picture. A context of only raw HTML
// (which renders as a comment, or as escaped text once gd-oNoa0zf55W lands)
// or only whitespace is not present, so /up never arms Enter over a blank
// block (gd-ZwnwonBxGS).
func (c decisionContextView) Present() bool { return hasVisibleContent(c.HTML) }

var htmlTagRe = regexp.MustCompile(`<[^>]*>`)

// hasVisibleContent reports whether rendered HTML shows any text or picture.
func hasVisibleContent(h template.HTML) bool {
	s := string(h)
	if strings.Contains(s, "<img ") {
		return true
	}
	text := html.UnescapeString(htmlTagRe.ReplaceAllString(s, ""))
	return strings.TrimFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) }) != ""
}

// decisionContextTmpl is the block's markup. It lives here rather than in a
// partial so the lean deck card (which includes no partials) and every
// standalone htmx fragment render it without a companion parse. The
// show-more is a CSS checkbox toggle, no script: the clamp lifts when the
// visually hidden checkbox is checked, and the label is the visible control.
// Styles: .decision-ctx-block in layout.html.
var decisionContextTmpl = template.Must(template.New("decision-ctx").Parse(
	`<section class="decision-ctx-block{{if .Long}} decision-ctx-block-long{{end}}{{if not .Present}} decision-ctx-block-empty{{end}}" aria-label="Decision context" data-decision-context="{{.ID}}">` +
		`{{if .Present}}` +
		`<h4 class="decision-ctx-block-head">Context</h4>` +
		`{{if .Long}}<input type="checkbox" class="decision-ctx-block-toggle" id="decision-ctx-more-{{.ID}}" aria-label="Show the whole context">{{end}}` +
		`<div class="decision-ctx-block-body markdown-body">{{.HTML}}</div>` +
		`{{if .Long}}<label class="decision-ctx-block-more" for="decision-ctx-more-{{.ID}}" data-deck-no-swipe><span class="decision-ctx-block-more-open">Show more</span><span class="decision-ctx-block-more-close">Show less</span></label>{{end}}` +
		`{{else}}` +
		`<p class="decision-ctx-block-none">No context was given for this decision. Open the bead and read its comments before answering.</p>` +
		`{{end}}` +
		`</section>`))

// Block renders the context block.
func (c decisionContextView) Block() template.HTML {
	var buf bytes.Buffer
	if err := decisionContextTmpl.Execute(&buf, c); err != nil {
		return ""
	}
	return template.HTML(buf.String())
}

// decisionEvidenceOf is the decision's evidence strip (gd-Pi7C83262N): the
// screenshots on the beads it is about (decisionview.LoadEvidence, read on
// the request's bearer through the short cache), drawn under the context on
// every viewer surface that resolves one. "" when there is none or the reads
// fail: a decision always renders, with or without its evidence.
func (s *Server) decisionEvidenceOf(ctx context.Context, b *beadsapi.BeadDetail) template.HTML {
	if b == nil {
		return ""
	}
	return s.evidenceStrip(b.ID, s.evidence.Load(ctx, s.daemon, b, 0))
}

// decisionEvidenceAll is decisionEvidenceOf for a list surface, reading the
// decisions' evidence a few at a time, keyed by decision id.
func (s *Server) decisionEvidenceAll(ctx context.Context, bs []*beadsapi.BeadDetail) map[string]template.HTML {
	out := map[string]template.HTML{}
	for id, imgs := range s.evidence.LoadAll(ctx, s.daemon, bs, 0) {
		if h := s.evidenceStrip(id, imgs); h != "" {
			out[id] = h
		}
	}
	return out
}

// evidenceStrip draws imgs as the shared strip, a source bead linking to its
// page here.
func (s *Server) evidenceStrip(id string, imgs []decisionview.Image) template.HTML {
	return decisionview.EvidenceStrip(id, imgs, decisionview.StripOptions{
		Base: s.basePath, Slot: "bv",
		Href: func(src string) string { return s.basePath + "/" + url.PathEscape(src) },
	})
}
