package main

import (
	"context"
	"encoding/json"
	"html/template"
	"sort"
	"strconv"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// widgetHydrationConcurrency caps the parallel GetBead fan-out used by
// buildRichReportView. A typical rich-report has well under this many
// widget children, but concurrent dashboards (Phase 8 incident wall) push
// us toward dozens of cards — bounded fan-out keeps daemon contention sane
// while still removing the serial RTT chain that dominated page-load.
const widgetHydrationConcurrency = 8

// richReportView is the per-type view model for `rich-report` beads — the
// Incident Intelligence user-facing surface. It composes a list of widget
// cards rendered by the registry in widget_renderer.go (Phase 0 ships the
// placeholder renderer only; later phases register real renderers under
// well-known slugs).
type richReportView struct {
	ReportKind string
	Headline   string
	Severity   string
	SourceRef  string
	Widgets    []widgetCard
	// BackLink points to the originating intake bead — the first parent-child
	// typed mention emitted by the rich-report. Phase 9 surfaces this as a
	// "← Back to …" link in the header so a user landing on a report can
	// always reach the bead the report was filed against. nil when the
	// rich-report has no parent-child mention (e.g. standalone reports).
	BackLink *richReportBackLink
}

// richReportBackLink is the minimal view model for the header back-link to
// the originating intake bead.
type richReportBackLink struct {
	ID    string
	Title string
}

// widgetCard is one rendered widget on a rich-report page. WidgetType is the
// renderer-registry slug; HeadlinedData is the prettified JSON payload the
// placeholder renderer drops into a <pre> block. RenderedHTML is filled by a
// registered renderer; when empty the template falls back to the placeholder.
type widgetCard struct {
	BeadID     string
	WidgetType string
	Headline   string
	Position   int
	DataJSON   string
	// RenderedHTML is populated when a Phase 4+ renderer is registered for
	// WidgetType. Empty → template falls back to the placeholder <pre>.
	RenderedHTML template.HTML

	// LiveIntervalSecs is the htmx polling cadence for this card. Non-zero
	// renders `hx-get/hx-trigger` attrs; zero suppresses polling. Sourced
	// from the Phase 8 allowlist for legacy widgets, or from the mezmo-
	// source path for widgets carrying live data via ka-0000002519.
	LiveIntervalSecs int

	// HasMezmoSource is true when the widget-instance bead carried a
	// non-empty `mezmo_source` field. Drives the per-widget Refresh button
	// in the template — refresh is only meaningful when there's a live data
	// path to re-fetch.
	HasMezmoSource bool

	// RefreshButton is the rendered <button> markup for the per-widget
	// refresh affordance. Empty when HasMezmoSource is false.
	RefreshButton template.HTML

	// Stage / StageChip / StageClass surface the lifecycle state of the
	// widget-type bead this instance points at (P5, ka-0000002154). Sourced
	// from the matching widget-type bead's `widget-stage:*` label. Empty
	// when no widget-type bead matches the slug or no stage label is set —
	// the template hides the chip in those cases.
	Stage      string
	StageChip  string
	StageClass string

	// Span is the rendered grid-column hint sourced from the widget-instance
	// bead's `widget_span` field (epic ka-0000003810). Empty (== "auto") lets
	// the auto-fill grid place the card. "half" and "full" claim row width
	// regardless of the grid's natural fit. Validated by normalizeWidgetSpan;
	// unknown values render as empty.
	Span string
}

// normalizeWidgetSpan validates a widget_span field value. Accepted values are
// "auto", "half", "full". Anything else (including empty) returns "" so the
// template falls through to the default auto-fill grid placement.
func normalizeWidgetSpan(raw string) string {
	switch raw {
	case "half", "full":
		return raw
	}
	return ""
}

// widgetChildFetcher is the subset of beadAPI buildRichReportView needs.
// Splitting it out keeps the test fixture small. The optional htmxResolver
// surface (ListBeadsFiltered) is asserted at call time so test fixtures
// that don't exercise the free-htmx fallback can keep their tiny mocks.
type widgetChildFetcher interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
}

// widgetTypeStageLookup is the optional capability the rich-report builder
// uses to resolve widget-type slugs → lifecycle stage so it can render the
// DRAFT/DEPRECATED chip on each card. Production wires it via beadAPI.ListBeadsFiltered;
// test fixtures that don't implement it simply skip stage chips.
type widgetTypeStageLookup interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// buildRichReportView walks the parent-child mentions pointing at the
// rich-report bead, picks out widget-instance children, fetches each child's
// fields, and assembles the ordered widget list. Daemon errors on a single
// child degrade that card to a placeholder rather than failing the page.
func buildRichReportView(
	ctx context.Context,
	bead *beadsapi.BeadDetail,
	mentions []beadsapi.BeadReferenceSummary,
	mentionedBy []beadsapi.BeadReferenceSummary,
	fetcher widgetChildFetcher,
	wctx *widgetRenderCtx,
) *richReportView {
	if bead == nil {
		return nil
	}
	v := &richReportView{
		ReportKind: bead.Fields["report_kind"],
		Headline:   bead.Fields["headline"],
		Severity:   bead.Fields["severity"],
		SourceRef:  bead.Fields["source_ref"],
		BackLink:   firstParentBackLink(mentions),
	}

	stageBySlug := loadWidgetTypeStages(ctx, fetcher)

	// Phase 1: collect widget-instance + parent-child refs into a slice,
	// preserving order. Cheap, single-threaded.
	widgetRefs := make([]beadsapi.BeadReferenceSummary, 0, len(mentionedBy))
	for _, ref := range mentionedBy {
		if ref.Type != "widget-instance" {
			continue
		}
		// Only treat parent-child typed mentions as widget children. Untyped
		// mentions or other relationship types are deliberately ignored so a
		// stray reference can't insert itself into the page. Accepts legacy
		// "parent-child" plus the new surface verbs "parent" / "child-of"
		// (M0 of ka-0000000933).
		if !isParentChildMention(ref.RelationshipType) {
			continue
		}
		widgetRefs = append(widgetRefs, ref)
	}

	// Phase 2: fan out one GetBead per ref with bounded concurrency. Each
	// goroutine writes into a pre-allocated cards[i] slot keyed by the ref
	// position, so the slice walk below stays deterministic. The mezmoCache
	// (`wctx.mezmoCache`) carries its own sync.Mutex, so concurrent
	// applyMezmoSource calls are safe; lookupWidgetForReport is a read-only
	// registry after init and stageBySlug is read-only here.
	cards := make([]widgetCard, len(widgetRefs))
	if len(widgetRefs) > 0 {
		sem := make(chan struct{}, widgetHydrationConcurrency)
		var wg sync.WaitGroup
		for i := range widgetRefs {
			wg.Add(1)
			sem <- struct{}{}
			go func(i int) {
				defer wg.Done()
				defer func() { <-sem }()
				cards[i] = hydrateWidgetCard(ctx, widgetRefs[i], fetcher, wctx, stageBySlug)
			}(i)
		}
		wg.Wait()
	}
	v.Widgets = append(v.Widgets, cards...)

	sort.SliceStable(v.Widgets, func(i, j int) bool {
		return v.Widgets[i].Position < v.Widgets[j].Position
	})
	return v
}

// loadWidgetTypeStages builds a slug → normalized-stage map by listing
// every widget-type bead once. Returns an empty map when the fetcher does
// not implement widgetTypeStageLookup or the daemon errors — callers fall
// back to no chip rather than failing the whole rich-report page.
func loadWidgetTypeStages(ctx context.Context, fetcher widgetChildFetcher) map[string]string {
	out := map[string]string{}
	lookup, ok := fetcher.(widgetTypeStageLookup)
	if !ok {
		return out
	}
	res, err := lookup.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"widget-type"},
		Kinds: []string{"data"},
		Limit: widgetTypesListLimit,
	})
	if err != nil || res == nil {
		return out
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		slug := b.Fields["slug"]
		if slug == "" {
			continue
		}
		stage := widgetStageFromLabels(b.Labels)
		if stage == "" {
			continue
		}
		// Latest write wins per slug (versioning ignored at chip level).
		out[slug] = stage
	}
	return out
}

// hydrateWidgetCard fetches a single widget-instance child and assembles
// the card. On daemon error the card degrades to a placeholder carrying
// just the ref's bead-id and title — same behavior as the pre-fan-out
// loop, so a flaky daemon can't take down the whole rich-report page.
// stageBySlug is the read-only slug→stage map from loadWidgetTypeStages,
// used to populate the per-widget stage chip.
func hydrateWidgetCard(
	ctx context.Context,
	ref beadsapi.BeadReferenceSummary,
	fetcher widgetChildFetcher,
	wctx *widgetRenderCtx,
	stageBySlug map[string]string,
) widgetCard {
	card := widgetCard{
		BeadID:   ref.ID,
		Headline: ref.Title,
	}
	child, err := fetcher.GetBead(ctx, ref.ID)
	if err != nil || child == nil {
		return card
	}
	card.WidgetType = child.Fields["widget_type"]
	if h := child.Fields["headline"]; h != "" {
		card.Headline = h
	}
	if pos := child.Fields["position"]; pos != "" {
		if n, perr := strconv.Atoi(pos); perr == nil {
			card.Position = n
		}
	}
	card.Span = normalizeWidgetSpan(child.Fields["widget_span"])
	if raw := child.Fields["data"]; raw != "" {
		if src, ok := parseMezmoSource(child.Fields["mezmo_source"]); ok {
			card.HasMezmoSource = true
			if wctx != nil {
				raw = applyMezmoSource(ctx, raw, src, card.WidgetType, wctx.mezmo, wctx.mezmoCache)
			}
		}
		card.DataJSON = prettyJSON(raw)
		// Phase-6 dispatcher: builtin Go renderers win first (parallel-
		// registry per ka-0000004584 / research §7), then fall through
		// to a free-htmx widget-type bead resolved off the daemon.
		resolver, _ := fetcher.(htmxResolver)
		if r, ok := lookupWidgetForReport(ctx, card.WidgetType, resolver); ok {
			if html, err := r(raw, wctx); err == nil {
				card.RenderedHTML = html
			}
		}
	}
	card.LiveIntervalSecs = liveIntervalForCard(card.WidgetType, card.HasMezmoSource, mezmoLiveActive)
	if card.HasMezmoSource && card.WidgetType != "" {
		swapInto := "#widget-body-" + card.BeadID
		card.RefreshButton = template.HTML(actionButtonJSON(
			"Refresh",
			RefreshWidgetAction(card.WidgetType, card.BeadID, swapInto),
		))
	}
	if card.WidgetType != "" {
		if stage, ok := stageBySlug[card.WidgetType]; ok && stage != "" {
			card.Stage = stage
			card.StageChip = widgetStageChipText(stage)
			card.StageClass = widgetStageChipClass(stage)
		}
	}
	return card
}

// firstParentBackLink scans forward typed mentions for the first
// parent-child reference. The originating intake bead is conventionally
// the rich-report's parent (e.g. a Jira-imported task). Returns nil when
// no parent is found.
func firstParentBackLink(mentions []beadsapi.BeadReferenceSummary) *richReportBackLink {
	for _, m := range mentions {
		if !isParentChildMention(m.RelationshipType) {
			continue
		}
		title := strings.TrimSpace(m.Title)
		if title == "" {
			title = m.ID
		}
		return &richReportBackLink{ID: m.ID, Title: title}
	}
	return nil
}

// prettyJSON re-indents a JSON string with two-space indentation. Invalid
// JSON falls through verbatim — the placeholder renderer wraps it in <pre>
// either way, so the user can still see what's in the bead.
func prettyJSON(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return raw
	}
	return string(out)
}
