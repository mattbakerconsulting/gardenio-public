package main

// Free-htmx widget dispatcher (Phase 6 — port-and-soak substrate).
//
// Plan-of-record: gardenio/docs/research/free-htmx-widget-port-prototype.md
// Bead: ka-0000004584 (epic ka-0000008085). Schema + write-time validation
// landed in ka-0000007359; this file provides the read-side dispatcher so the
// rich-report can render a widget-instance whose widget_type slug resolves to
// a type=widget-type bead with renderer_kind=htmx instead of a registered Go
// renderer.
//
// Strategy A — parallel registry, port-then-soak, deprecate. Builtin slugs
// (registered via init() in widgets_phase4.go and friends) win the lookup;
// only when the registry has no entry do we fall through to the daemon for
// an active htmx widget-type bead. Existing widget-instance beads with
// widget_type=activity-log keep rendering through the Go renderer; new
// instances emitted with widget_type=activity-log-htmx pick up the htmx
// path.
//
// Compiled templates are cached on (slug, widget_version, updated_at) so a
// type-bead edit invalidates immediately on the next dispatch. The cache is
// process-local and rebuilds from kbeads on miss; bead deletion is not
// invalidated explicitly (research §6.5 — the cache holds at most a stale
// compile, which is harmless because no instance will dispatch to a deleted
// type).

import (
	"context"
	"fmt"
	"html/template"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	htmxRendererKind        = "htmx"
	htmxBuiltinKind         = "builtin"
	htmxTemplateEngineGoV1  = "go-html-template-v1"
	htmxResolveTimeoutSecs  = 4 // matches the rich-report 4 s render budget
	htmxResolveListPageSize = 25
)

// htmxResolver is the slim subset of beadAPI the dispatcher needs to find
// the active widget-type bead by slug. Splitting it out keeps the test
// fixture small and lets us pass server.daemon (which already implements
// ListBeadsFiltered) without dragging the full API surface into tests.
type htmxResolver interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// htmxTrustLookup is the project registry widgetTypeTrusted reads. A resolver
// without it resolves no htmx widget-type (fail closed); server.daemon has it.
type htmxTrustLookup interface {
	ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error)
}

// widgetTypeTrusted reports whether a widget-type bead's template_source may
// run in an operator's viewer session: every project: label it carries names a
// registered operator land (gd-a2s0GZExxr). A lands-tenant writer's bead always
// carries its own land's label (kbeads stamps the agent token's project at
// create and refuses a mismatch), so a tenant-authored widget-type never
// passes; a label naming an unregistered project fails closed. An unlabelled
// bead is fleet-global and passes.
func widgetTypeTrusted(b *beadsapi.BeadDetail, projects map[string]beadsapi.ProjectInfo) bool {
	if b == nil {
		return false
	}
	for _, l := range b.Labels {
		name, ok := strings.CutPrefix(l, "project:")
		if !ok {
			continue
		}
		info, known := projects[name]
		if !known || !info.IsOperatorLand() {
			return false
		}
	}
	return true
}

// htmxRendererCache memoises compiled htmx widget templates. Keyed on the
// type-bead id so multiple slug versions (or concurrent edits) keep their
// own compile. Entries store the type-bead's updated_at so we can detect
// stale entries on the next lookup.
type htmxRendererCache struct {
	mu      sync.RWMutex
	entries map[string]htmxRendererCacheEntry
}

type htmxRendererCacheEntry struct {
	updatedAt time.Time
	renderer  widgetRenderer
}

// htmxCache is the package-level cache. Tests reset it via
// resetHtmxRendererCache so a malformed-template test doesn't poison
// subsequent runs.
var htmxCache = &htmxRendererCache{entries: map[string]htmxRendererCacheEntry{}}

// lookupWidgetForReport resolves a widget renderer for the rich-report
// dispatcher. Builtin renderers (Go-coded, registered at init time) win;
// when no builtin matches, the resolver is consulted for an active
// type=widget-type bead with renderer_kind=htmx. Returns ok=false when
// neither path resolves — the caller renders the placeholder card.
//
// resolver may be nil; in that case the function degrades to lookupWidget
// (pure builtin lookup). This keeps tests and dev-mode renders that don't
// wire a daemon working unchanged.
func lookupWidgetForReport(ctx context.Context, slug string, resolver htmxResolver) (widgetRenderer, bool) {
	if r, ok := lookupWidget(slug); ok {
		return r, true
	}
	if resolver == nil || strings.TrimSpace(slug) == "" {
		return nil, false
	}
	r, err := resolveHtmxRenderer(ctx, slug, resolver)
	if err != nil || r == nil {
		return nil, false
	}
	return r, true
}

// resolveHtmxRenderer fetches the active widget-type bead for `slug` from
// the daemon, compiles its template_source on cache miss, and returns the
// resulting widgetRenderer. Returns nil with no error when no matching
// type bead exists or when the bead is not htmx — both are "fall through to
// placeholder" outcomes and the caller treats them identically.
func resolveHtmxRenderer(ctx context.Context, slug string, resolver htmxResolver) (widgetRenderer, error) {
	if resolver == nil {
		return nil, nil
	}
	// Bound the daemon round-trip; the rich-report page render runs under
	// the same budget (research §6.3) and a hung resolver mustn't starve it.
	rctx, cancel := context.WithTimeout(ctx, htmxResolveTimeoutSecs*time.Second)
	defer cancel()

	bead, err := findActiveHtmxWidgetType(rctx, slug, resolver)
	if err != nil {
		return nil, err
	}
	if bead == nil {
		return nil, nil
	}
	return compileHtmxRenderer(bead)
}

// findActiveHtmxWidgetType returns the freshest open, trusted
// (widgetTypeTrusted) widget-type bead with a slug field equal to `slug` and
// renderer_kind=htmx. Untrusted beads are dropped before the freshest is
// picked, so a newer tenant bead cannot shadow a fleet one. Returns (nil, nil)
// when the daemon has no matching bead — that's a normal "no port yet"
// signal, not an error.
//
// The daemon doesn't index beads by Fields, so we list type=widget-type
// (a small set in practice) and filter client-side. The page-size cap
// keeps memory bounded even when the catalog grows; widget-type bead
// counts are O(catalog), which is dozens, not thousands.
func findActiveHtmxWidgetType(ctx context.Context, slug string, resolver htmxResolver) (*beadsapi.BeadDetail, error) {
	res, err := resolver.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"widget-type"},
		Statuses: []string{"open", "in_progress"},
		Limit:    htmxResolveListPageSize,
	})
	if err != nil {
		return nil, fmt.Errorf("widget-htmx: list widget-type beads: %w", err)
	}
	if res == nil {
		return nil, nil
	}
	var candidates []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if b.Fields["slug"] != slug {
			continue
		}
		rk := b.Fields["renderer_kind"]
		if rk == "" {
			rk = htmxBuiltinKind
		}
		if rk != htmxRendererKind {
			continue
		}
		if strings.TrimSpace(b.Fields["template_source"]) == "" {
			continue
		}
		candidates = append(candidates, b)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	trust, ok := resolver.(htmxTrustLookup)
	if !ok {
		return nil, nil
	}
	projects, err := trust.ListProjectBeads(ctx)
	if err != nil {
		return nil, fmt.Errorf("widget-htmx: list projects: %w", err)
	}
	var best *beadsapi.BeadDetail
	for _, b := range candidates {
		if !widgetTypeTrusted(b, projects) {
			continue
		}
		// Prefer the most recently-updated match. UpdatedAt may be the zero
		// time when the daemon strips it; treat that as "older than anything"
		// so explicit timestamps win.
		if best == nil || b.UpdatedAt.After(best.UpdatedAt) {
			best = b
		}
	}
	return best, nil
}

// compileHtmxRenderer parses the type-bead's template_source and returns a
// widgetRenderer that decodes the instance payload, runs the template, and
// emits the rendered HTML. Cached on (bead.ID, bead.UpdatedAt) so subsequent
// lookups hit the cache; a re-parse on type-bead edits is automatic.
//
// Returned errors describe template-parse failures. The dispatcher in
// rich_report.go treats those identically to "no renderer" — the placeholder
// card renders. Daemon startup also walks every htmx widget-type bead
// (CompileAllWidgetTemplates in kbeads/internal/server/widget_validation.go),
// so a malformed template-source never reaches the read path silently.
func compileHtmxRenderer(bead *beadsapi.BeadDetail) (widgetRenderer, error) {
	if bead == nil {
		return nil, fmt.Errorf("widget-htmx: nil widget-type bead")
	}
	slug := strings.TrimSpace(bead.Fields["slug"])
	if slug == "" {
		return nil, fmt.Errorf("widget-htmx: widget-type %s missing slug", bead.ID)
	}
	src := bead.Fields["template_source"]
	if strings.TrimSpace(src) == "" {
		return nil, fmt.Errorf("widget-htmx: widget-type %s (%s) has empty template_source", bead.ID, slug)
	}
	engine := bead.Fields["template_engine"]
	if engine == "" {
		engine = htmxTemplateEngineGoV1
	}
	if engine != htmxTemplateEngineGoV1 {
		return nil, fmt.Errorf("widget-htmx: widget-type %s (%s) template_engine=%q not supported (only %q)",
			bead.ID, slug, engine, htmxTemplateEngineGoV1)
	}

	htmxCache.mu.RLock()
	if entry, ok := htmxCache.entries[bead.ID]; ok && !entry.updatedAt.Before(bead.UpdatedAt) && entry.renderer != nil {
		htmxCache.mu.RUnlock()
		return entry.renderer, nil
	}
	htmxCache.mu.RUnlock()

	tpl, err := template.New(slug).Parse(src)
	if err != nil {
		return nil, fmt.Errorf("widget-htmx: parse template_source for %s (%s): %w", bead.ID, slug, err)
	}

	beadID := bead.ID
	r := func(rawJSON string, _ *widgetRenderCtx) (template.HTML, error) {
		payload, err := decodePayload(rawJSON)
		if err != nil {
			return "", err
		}
		// Mirror the ka-0000007136 §5.1 scope: free-htmx templates always
		// see a `.Payload` field, regardless of the underlying shape. The
		// Phase 1 port (activity-log-htmx) uses `.Payload.entries`. Future
		// ports add `.Bead`, `.Widget`, `.URLs` etc. — same call site.
		scope := htmxTemplateScope{
			Payload: payload,
			Widget:  htmxWidgetMeta{ID: beadID},
		}
		var buf strings.Builder
		if err := tpl.Execute(&buf, scope); err != nil {
			return "", fmt.Errorf("widget-htmx: render %s: %w", slug, err)
		}
		// html/template trusts template_source itself; inertWidgetHTML
		// strips what would run before the output is marked safe.
		return template.HTML(inertWidgetHTML(buf.String())), nil
	}

	htmxCache.mu.Lock()
	htmxCache.entries[beadID] = htmxRendererCacheEntry{updatedAt: bead.UpdatedAt, renderer: r}
	htmxCache.mu.Unlock()
	return r, nil
}

// htmxTemplateScope is the compile-time scope every free-htmx template runs
// against. v1 exposes only Payload + Widget metadata; later phases extend
// with .Bead, .Parent, .Siblings, .Viewer, .URLs (research §3.2). Adding a
// field here is a Phase-3+ change; the port intentionally locks the scope
// down to the smallest set the activity-log-htmx template body needs.
type htmxTemplateScope struct {
	Payload map[string]any
	Widget  htmxWidgetMeta
}

// htmxWidgetMeta carries the widget-instance bead id so a template can emit
// stable target ids for OOB swaps (research §3.1). The id is the bead-id of
// the widget-instance, not the widget-type bead.
type htmxWidgetMeta struct {
	ID string
}

