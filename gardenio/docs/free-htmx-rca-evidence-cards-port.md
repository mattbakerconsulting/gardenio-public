> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — rca-evidence-cards → rca-evidence-cards-htmx (Phase 5)

Bead: ka-0000004029 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000007835 ([reproduction-steps port doc](free-htmx-reproduction-steps-port.md)),
ka-0000004065 ([reporter-info port doc](free-htmx-reporter-info-port.md)),
ka-0000000243 ([event-timeline port doc](free-htmx-event-timeline-port.md)),
ka-0000000272 ([ai-recommended-actions port doc](free-htmx-ai-recommended-actions-port.md)),
ka-0000005506 ([fix-available-pending-branch port doc](free-htmx-fix-available-pending-branch-port.md)),
ka-0000000715 ([vm-sparklines port doc](free-htmx-vm-sparklines-port.md)),
ka-0000006619 ([source-code-analysis port doc](free-htmx-source-code-analysis-port.md))

Tenth pre-defined widget on the free-htmx substrate, **second Phase-5
port** — rca-evidence-cards is a code-highlighted widget
(`widgets_phase5.go`) so the parity-gap surface is wider than the
Phase-4 ports. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A —
port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`rca-evidence-cards`,
`widgets_phase5.go::renderRCAEvidenceCards`) and the htmx port
(`rca-evidence-cards-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new
instances opt into the htmx path by writing
`widget_type: "rca-evidence-cards-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the cards[] shape the Go renderer accepts —
flat array of card objects with `id`, `headline`, optional `detail`,
`source_kind` (pre-lower-cased), optional `source_label`,
`confidence` (pre-banded), optional `language`, and `code`. Real
instances on rich-report beads cutting the htmx path emit pre-banded
confidence strings and pre-lower-cased source-kind strings.

```jsonc
{
  "cards": [
    {
      "id": "ev-1",
      "headline": "Auth middleware drops sessions",
      "detail": "401 spike correlates with !2185 deploy",
      "source_kind": "mezmo",
      "source_label": "mezmo: auth-svc",
      "confidence": "high",
      "language": "go",
      "code": "func auth() { panic(0) }"
    }
  ]
}
```

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/rca-evidence-cards-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestRCAEvidenceCardsParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same cards through `renderRCAEvidenceCards`
(Go) and through the htmx-compiled template, then asserts:

- both outputs carry the `<div class="ii-w-rca">` chrome
- both outputs carry an `<ol class="ii-w-rca-list">`
- both outputs carry one `<li class="ii-w-rca-card">` per card with
  the optional `data-confidence` attribute
- both outputs carry the `<header class="ii-w-rca-card-head">` row
- both outputs carry the `<span class="ii-source" data-source-kind="…">`
  chip with lower-cased `source_kind`
- both outputs carry the `<h4 class="ii-w-rca-headline">` when
  `headline` is set
- both outputs carry the `<span class="ii-pill" data-confidence="…">`
  with banded confidence values
- both outputs carry the `<p class="ii-w-rca-detail">` when `detail`
  is set
- both outputs carry the `<div class="ii-w-rca-code" data-language="…">`
  wrapper when `code` is present

### Parity gaps

Five intentional gaps vs. the Go renderer, all locked in
`TestRCAEvidenceCardsParity`:

#### 1. Chroma syntax highlighting

The Go path runs each card's `code` field through the shared
`codeHighlighter` (chroma, github-dark theme, server-side, cached) and
emits chroma's coloured `<span style="color:…">` token spans inside
`<pre class="chroma">`. The htmx template body has no Go-side hook into
chroma, so the rendered `<pre>` carries plain escaped text only — the
same fidelity the `plainTextPre` fallback produces when chroma fails.

The wrapper chrome is preserved on both paths:
`<div class="ii-w-rca-code" data-language="…">` is identical, so the
Phase-5 CSS that targets `.ii-w-rca-code` still applies.

Same gap as `source-code-analysis-htmx`. A future research call
(ka-0000002102 §3.3) may register a template-side highlighter fn at
compile time. Until that lands, widget-instances that need chroma
highlighting should keep using the `rca-evidence-cards` Go slug.

#### 2. Confidence normalisation

The Go path runs `payload.confidence` through `confidenceBand()`:
numeric scores are bucketed (`>=0.8` → `"high"`, `>=0.5` → `"medium"`,
otherwise `"low"`) and string bands are trimmed/lower-cased and
validated against `{high, medium, low}`. The htmx template body has no
Go-side hook, so it renders the `confidence` field verbatim. Producers
cutting an rca-evidence-cards-htmx widget-instance MUST emit
confidence as a pre-banded lower-case string (`"high"` / `"medium"` /
`"low"`) — a numeric score would render verbatim and violate the D2
rule that numbers must not surface.

The parity test's Go-side payload uses a numeric `0.92` to exercise
`confidenceBand`; the htmx-side payload pre-bands to `"high"`. Both
renders must produce identical `data-confidence="high"` chrome. Same
approach as `TestAIRecommendedActionsParity`.

#### 3. Source-kind normalisation

The Go path runs `payload.source_kind` through `strings.ToLower(...)`
before emitting `data-source-kind="…"`. The htmx template body has
no Go-side hook, so it renders the `source_kind` field verbatim.
Producers cutting an rca-evidence-cards-htmx widget-instance MUST emit
source_kind as a pre-lower-cased string (e.g. `"mezmo"`,
`"victoria-metrics"`, `"gitlab"`) for the Phase-1 source-chip CSS
palette to match.

The parity test's Go-side payload uses `"MEZMO"` to exercise the Go
normalisation; the htmx-side payload pre-lower-cases. Both renders
must produce identical `data-source-kind="mezmo"` chrome.

#### 4. Action buttons

The Go renderer constructs `<button class="ii-action-btn">` elements
via `OpenMezmoAction` / `OpenMRAction` / `OpenIDEAction` /
`OpenJiraAction` descriptors keyed off `open_mezmo` / `open_mr` /
`ide_path` / `open_jira`. Those helpers live in Go and have no
template-side equivalent on the substrate established in
ka-0000007359 / MR !2334. The htmx template therefore emits zero
buttons; the four button-construction fields are silently ignored.
The `<div class="ii-w-rca-actions">` wrapper is present but empty —
this preserves the CSS hook so Phase-1 spacing rules still apply.

This is the same gap as in
[`free-htmx-ai-recommended-actions-port.md`](free-htmx-ai-recommended-actions-port.md),
[`free-htmx-header-incident-summary-port.md`](free-htmx-header-incident-summary-port.md),
and [`free-htmx-source-code-analysis-port.md`](free-htmx-source-code-analysis-port.md) —
button rendering blocks on a future research call (see ka-0000002102
§3.3 — register a button-builder template func at compile time).
Until that lands, widget-instances that need actionable buttons
should keep using the `rca-evidence-cards` Go slug.

`TestRCAEvidenceCardsParity` locks the gap in place: the Go output
carries exactly two `ii-action-btn` (one per card with an
`open_mezmo` / `open_mr` field); the htmx output carries zero.
Change with eyes-open.

#### 5. Drill-down drawer

The Go renderer pre-renders `payload.drilldown_markdown` via goldmark
(`renderMarkdown`) and emits a `<button class="ii-w-rca-drill">` plus
a `<template class="ii-w-rca-drill-body">` carrying the rendered
HTML. The drawer body is kept off the main DOM until the user opens
it, scoped via a stable `data-ii-drawer-key` derived from the card
ID and index.

The htmx template has no goldmark helper in scope and no template-side
button-construction hook, so the drawer button + template pair is
omitted entirely; the `drilldown_markdown` and `drilldown_title`
fields are silently ignored. Producers who want a drawer should keep
using the `rca-evidence-cards` Go slug.

`TestRCAEvidenceCardsParity` locks the htmx-side absence:
`ii-w-rca-drill` must not appear in the htmx output.

### Performance

Per research §6.3 the read-time budget is 4 s for the full rich-report
page. Dispatcher round-trip is bounded by `htmxResolveTimeoutSecs = 4`;
the compiled template is cached on `(bead.ID, bead.UpdatedAt)`. First
hit per process pays one `template.Parse`; subsequent renders pay a
read-locked map lookup. Editing the widget-type bead bumps `updated_at`,
which invalidates the cache on the next dispatch (already covered by
`TestCompileHtmxRenderer_CacheInvalidatesOnUpdate`).

### Accessibility

The htmx template body is structurally equivalent to the Go template
(`templates/partials/widgets_phase5.html` /
`widget_rca_evidence_cards`) modulo the field-access shape
(`.Cards` Go struct vs `range .Payload.cards` map) and the five
parity gaps above. That means:

- `<div class="ii-w-rca">` semantic chrome preserved
- `<ol class="ii-w-rca-list">` numbered-list semantics preserved
- per-card `data-confidence` attribute preserved
- `<span class="ii-source" data-source-kind="…">` chip preserved
  (with `aria-hidden="true"` icon span)
- `<h4 class="ii-w-rca-headline">` headline level preserved
- empty-state copy ("No RCA evidence collected.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/rca-evidence-cards-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: rca-evidence-cards-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/rca-evidence-cards-htmx.json
```

Pre-write hooks parse `template_source` under `html/template` and
reject malformed inputs at createBead/updateBead time, so a bad
fixture surfaces immediately. Schema validation runs against
widget-instance writes per the same pre-write hook.

## Soak plan

1. Land this MR. Daemon-side validation has been live since
   ka-0000007359 (commit 88251da2).
2. Post-merge: `kd create` the widget-type bead from the fixture
   against the gardenio-rwx daemon. Cut a smoke widget-instance with
   `widget_type: rca-evidence-cards-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `rca-evidence-cards` instance on the same report — confirm the
   chrome shells match modulo the documented parity gaps (chroma
   colours, action buttons, drill-down drawer).
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on cards missing `data-source-kind` or
     `data-confidence` attributes (e.g. producers forgetting to
     pre-band a confidence)
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `rca-evidence-cards-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
