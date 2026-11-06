> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — ai-recommended-actions → ai-recommended-actions-htmx (Phase 4)

Bead: ka-0000000272 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000007835 ([reproduction-steps port doc](free-htmx-reproduction-steps-port.md))

Fifth pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346,
header-incident-summary in MR !2347, and reproduction-steps in
MR !2350. Same dispatcher (`widgets_htmx.go::lookupWidgetForReport`),
same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`ai-recommended-actions`,
`widgets_phase4.go::renderAIRecommendedActions`) and the htmx port
(`ai-recommended-actions-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new
instances opt into the htmx path by writing
`widget_type: "ai-recommended-actions-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the actions[] shape the Go renderer accepts —
flat array of action objects with `headline`, optional `detail`,
`confidence` band, optional `hotkey`. Real instances on rich-report
beads cutting the htmx path emit pre-banded confidence strings.

```jsonc
{
  "actions": [
    {
      "headline": "Roll back !2185",
      "detail": "Probable cause is the auth refactor",
      "confidence": "high",
      "hotkey": "r"
    },
    {
      "headline": "Check Mezmo for 401 spikes",
      "confidence": "medium"
    }
  ]
}
```

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/ai-recommended-actions-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestAIRecommendedActionsParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same actions through `renderAIRecommendedActions`
(Go) and through the htmx-compiled template, then asserts:

- both outputs carry the `<div class="ii-w-actions-list">` chrome
- both outputs carry an `<ol class="ii-w-actions-rows">`
- both outputs carry one `<li class="ii-w-action-row">` per action with
  `data-confidence` attribute
- both outputs carry the `<div class="ii-w-action-head">` row with the
  hotkey `<kbd>`, headline span, and confidence pill
- both outputs carry the `<p class="ii-w-action-detail">` paragraph
  when `detail` is present
- D2 holds in both paths: numeric confidence scores never surface in
  the rendered HTML

### Parity gaps

Two intentional gaps vs. the Go renderer, both locked in
`TestAIRecommendedActionsParity`:

#### 1. Confidence normalisation

The Go path runs `payload.confidence` through `confidenceBand()`
(`widgets_phase4.go::confidenceBand`) which:

- maps `float64` JSON numbers to band strings: `>=0.8 → "high"`,
  `>=0.5 → "medium"`, otherwise `"low"`
- trims/lower-cases string bands and validates against
  `{high, medium, low}`; anything else collapses to `""`

The htmx template body has no Go-side hook, so it renders the
`confidence` field verbatim. Producers cutting an
ai-recommended-actions-htmx widget-instance MUST emit confidence as
a pre-banded lower-case string (`"high"` / `"medium"` / `"low"`).
A numeric score would render verbatim and violate the D2 rule that
numbers must not surface.

The parity test's Go-side payload uses a numeric `0.9` and an
upper-case `"MEDIUM"` (both normalised by the Go path); the htmx-side
payload pre-bands to `"high"` and `"medium"`. Both renders must
produce identical chrome — and neither must contain `0.9` (D2).

The same approach is used in `TestActivityLogParity` /
`TestRelatedTicketsParity` / `TestHeaderIncidentSummaryParity`:
pre-normalise the htmx payload to the wire shape the Go path produces
server-side.

#### 2. Action buttons

The Go renderer emits one `<button class="ii-action-btn">` per action
that carries `open_mr` / `open_jira` / `open_mezmo`, built via
`OpenMRAction` / `OpenJiraAction` / `OpenMezmoAction` descriptors.
Those helpers live in Go and have no template-side equivalent on the
substrate established in ka-0000007359 / MR !2334. The htmx template
therefore omits the button block entirely; the optional
button-construction fields are silently ignored.

This is the same gap as in
[`free-htmx-header-incident-summary-port.md`](free-htmx-header-incident-summary-port.md) —
button rendering blocks on a future research call (see ka-0000002102
§3.3 — register a button-builder template func at compile time).
Until that lands, widget-instances that need actionable buttons should
keep using the `ai-recommended-actions` Go slug.

`TestAIRecommendedActionsParity` locks the gap in place: the Go output
carries exactly two `ii-action-btn` (one for `open_mr`, one for
`open_mezmo`); the htmx output carries zero. Change with eyes-open.

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
(`templates/partials/widgets_phase4.html` /
`widget_ai_recommended_actions`) modulo the field-access shape
(`.Actions` Go struct vs `range .Payload.actions` map) and the two
parity gaps above. That means:

- `<div class="ii-w-actions-list">` semantic chrome preserved
- `<ol class="ii-w-actions-rows">` numbered-list semantics preserved
- per-row `data-confidence` attribute preserved
- `<kbd class="ii-key">` keyboard-hint semantics preserved
- empty-state copy ("No recommended actions.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/ai-recommended-actions-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: ai-recommended-actions-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/ai-recommended-actions-htmx.json
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
   `widget_type: ai-recommended-actions-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `ai-recommended-actions` instance on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows missing `data-confidence` attributes
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `ai-recommended-actions-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
