package main

// Phase 5 — code-highlighted widgets.
// Plan-of-record: docs/research/incident-intelligence/synthesis.md §Phase 5.
// Bead: ka-0000004643.
//
// Two widgets land here:
//   #4  rca-evidence-cards    — per-evidence-item cards with snippet,
//                               source-kind chip, confidence band,
//                               drill-down drawer.
//   #13 source-code-analysis  — file/function-level findings with
//                               highlighted offending lines + jump-to-line
//                               action descriptor.
//
// Both surface a `code` + `language` payload field. The widgetRenderCtx
// gained an `hl` field in this phase; it's the shared chroma highlighter
// (server-side, github-dark, inline styles, cached). Plain-HTML widgets in
// Phase 4 are unaffected.

import (
	"fmt"
	"html/template"
	"strconv"
	"strings"

	"github.com/yuin/goldmark"
)

func init() {
	registerWidget("rca-evidence-cards", renderRCAEvidenceCards)
	registerWidget("source-code-analysis", renderSourceCodeAnalysis)
}

// ---- helpers --------------------------------------------------------------

// widgetInt pulls an integer payload field. JSON numbers decode as float64,
// so we accept both. Returns 0 when missing/invalid.
func widgetInt(payload map[string]any, key string) int {
	switch v := payload[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(v))
		return n
	}
	return 0
}

// renderHighlight is the bridge between Phase 5 widgets and the shared
// code highlighter. Returns "" when the context lacks a highlighter (test
// path that doesn't exercise highlighting) — the template handles the
// empty case as a non-highlighted <pre> fallback.
func renderHighlight(ctx *widgetRenderCtx, language, code string) template.HTML {
	if ctx == nil || ctx.hl == nil {
		if code == "" {
			return ""
		}
		return plainTextPre(code)
	}
	return ctx.hl.highlight(language, code)
}

// ---- W4 — rca-evidence-cards ---------------------------------------------

type rcaEvidenceCard struct {
	ID         string
	Headline   string
	Detail     string
	SourceKind string // "mezmo" | "victoria-metrics" | … (Phase 1 chip palette)
	SourceText string // human label rendered inside the chip
	Confidence string // band only — D2
	Language   string
	CodeHTML   template.HTML // chroma-highlighted snippet
	OpenAction template.HTML // primary action button (open-mezmo / open-mr / open-ide …)
	DrillKey   string        // unique key used to scope the drill-down drawer
	DrillTitle string
	DrillHTML  template.HTML // optional pre-rendered drawer body markdown
}

type rcaEvidenceCards struct {
	Cards []rcaEvidenceCard
}

func renderRCAEvidenceCards(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := rcaEvidenceCards{}
	for i, e := range widgetObjectSlice(p, "cards") {
		card := rcaEvidenceCard{
			ID:         widgetString(e, "id"),
			Headline:   widgetString(e, "headline"),
			Detail:     widgetString(e, "detail"),
			SourceKind: strings.ToLower(widgetString(e, "source_kind")),
			SourceText: widgetString(e, "source_label"),
			Confidence: confidenceBand(e),
			Language:   widgetString(e, "language"),
		}
		if card.SourceText == "" {
			card.SourceText = card.SourceKind
		}
		card.CodeHTML = renderHighlight(ctx, card.Language, widgetString(e, "code"))
		card.OpenAction = evidenceCardAction(e)

		// Drill-down drawer: optional. If `drilldown_markdown` is set we
		// pre-render it here so the drawer body can be inlined as a
		// data-attribute (kept off the main DOM until the user opens it).
		if md := widgetString(e, "drilldown_markdown"); md != "" {
			var mdRenderer goldmark.Markdown
			if ctx != nil {
				mdRenderer = ctx.md
			}
			card.DrillHTML = renderMarkdown(mdRenderer, md)
			card.DrillTitle = widgetString(e, "drilldown_title")
			if card.DrillTitle == "" {
				card.DrillTitle = "Evidence detail"
			}
			// Drawer-key collisions would let one card's keystroke open
			// another's drawer. ID + index keeps it stable across renders.
			card.DrillKey = fmt.Sprintf("rca-evidence-%d-%s", i, card.ID)
		}
		v.Cards = append(v.Cards, card)
	}
	return renderWidgetTemplate(ctx, "widget_rca_evidence_cards", v)
}

// evidenceCardAction picks the right action descriptor based on the payload
// fields a card carries. We accept the three most common evidence shapes:
// mezmo query, MR link, and IDE jump. Returns empty HTML when no recognised
// link is present.
func evidenceCardAction(e map[string]any) template.HTML {
	if q := widgetString(e, "open_mezmo"); q != "" {
		return template.HTML(actionButtonJSON("Open in Mezmo", OpenMezmoAction("", q)))
	}
	if mrURL := widgetString(e, "open_mr"); mrURL != "" {
		return template.HTML(actionButtonJSON("Open MR", OpenMRAction("", mrURL)))
	}
	if path := widgetString(e, "ide_path"); path != "" {
		return template.HTML(actionButtonJSON("Open in IDE",
			OpenIDEAction(path, widgetInt(e, "ide_line"))))
	}
	if ticket := widgetString(e, "open_jira"); ticket != "" {
		return template.HTML(actionButtonJSON("Open ticket", OpenJiraAction("", ticket)))
	}
	return ""
}

// ---- W13 — source-code-analysis -------------------------------------------

type sourceFinding struct {
	Path       string
	Line       int
	HasLine    bool
	Function   string
	Severity   string // "error" | "warn" | "info" — re-uses Phase 1 state-pill data-state
	Headline   string
	Detail     string
	Language   string
	CodeHTML   template.HTML
	JumpAction template.HTML
}

type sourceCodeAnalysis struct {
	Title    string
	Findings []sourceFinding
}

func renderSourceCodeAnalysis(rawJSON string, ctx *widgetRenderCtx) (template.HTML, error) {
	p, err := decodePayload(rawJSON)
	if err != nil {
		return "", err
	}
	v := sourceCodeAnalysis{Title: widgetString(p, "title")}
	for _, f := range widgetObjectSlice(p, "findings") {
		row := sourceFinding{
			Path:     widgetString(f, "path"),
			Line:     widgetInt(f, "line"),
			Function: widgetString(f, "function"),
			Severity: strings.ToLower(widgetString(f, "severity")),
			Headline: widgetString(f, "headline"),
			Detail:   widgetString(f, "detail"),
			Language: widgetString(f, "language"),
		}
		row.HasLine = row.Line > 0
		row.CodeHTML = renderHighlight(ctx, row.Language, widgetString(f, "code"))
		// Jump-to-line is the canonical action for #13 — every finding gets
		// one when we have a path. Falls through to OpenIDEAction's no-line
		// branch when line is absent.
		if row.Path != "" {
			row.JumpAction = template.HTML(actionButtonJSON("Open in IDE",
				OpenIDEAction(row.Path, row.Line)))
		}
		v.Findings = append(v.Findings, row)
	}
	return renderWidgetTemplate(ctx, "widget_source_code_analysis", v)
}
