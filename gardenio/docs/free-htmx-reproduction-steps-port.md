> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — reproduction-steps → reproduction-steps-htmx (Phase 4)

Bead: ka-0000007835 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md))

Fourth pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346, and header-incident-summary
in MR !2347. Same dispatcher (`widgets_htmx.go::lookupWidgetForReport`),
same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`reproduction-steps`,
`widgets_phase4.go::renderReproductionSteps`) and the htmx port
(`reproduction-steps-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new
instances opt into the htmx path by writing
`widget_type: "reproduction-steps-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the structured `steps[]` shape the Go renderer
already accepts as a fallback for callers that do not want to ship
markdown. Real instances on rich-report beads cutting the htmx path
emit `steps[]`; matching that contract is what unlocks side-by-side
soak with the existing Go renderer.

```jsonc
{
  "steps": [
    "Open the upload dialog",
    "Pick a 50 MB PDF",
    "Click Upload — observe the spinner hang"
  ]
}
```

The Go renderer also accepts a `markdown` field and runs it through
goldmark. The htmx template body has no goldmark hook, so the
`markdown` field is intentionally a parity gap (see below). Producers
that need rich markup keep writing `widget_type: reproduction-steps`
through the Go path.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/reproduction-steps-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestReproductionStepsParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same `steps[]` payload through
`renderReproductionSteps` (Go) and through the htmx-compiled template,
then asserts:

- both outputs carry the `<div class="ii-w-markdown ii-w-repro">`
  chrome
- both outputs carry an `<ol>` and exactly one `<li>` per step
- every step's text appears verbatim in both outputs

### Parity gap — markdown rendering

The Go renderer reads the `markdown` field, falls back to formatting
`steps[]` as `1. step\n2. step\n…`, and feeds the result through
goldmark via `widgetRenderCtx.md`. Goldmark turns the numbered list
into an `<ol>` of `<li>`s, renders `**bold**` and `[links](#)`,
strips raw HTML, etc.

The htmx template body cannot run goldmark — html/template has no
markdown hook, and the renderer compiles under the substrate
established in ka-0000007359 / MR !2334. The htmx template therefore
accepts only the structured `steps[]` shape and renders the steps as
plain `<li>`s.

`TestReproductionStepsParity` locks the gap in place: when only
`markdown` is supplied, the Go path emits `<strong>Upload</strong>`
while the htmx path emits the empty-state copy. Two paths to close
the gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders
   the markdown to HTML once and ships it as a `steps_html` field;
   the template emits `{{.steps_html | safehtml}}` (requires a
   trusted-html sanitizer registration).
2. **Lift goldmark into the template scope**: register a
   `markdown` template func at compile time (research §3.3 — deferred
   until a second port asks for it).

Neither is required for reproduction-steps — the structured `steps[]`
shape is the dominant authoring pattern on real rich-report beads,
and the chrome carries enough information for the soak.

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
`widget_reproduction_steps`) modulo the field-access shape (`.HTML`
goldmark output vs `range .Payload.steps`) and the markdown parity
gap above. That means:

- `<div class="ii-w-markdown ii-w-repro">` semantic chrome preserved
- `<ol>` numbered-list semantics preserved (the Go path emits the same
  shape via goldmark's numbered-list rendering)
- empty-state copy ("No reproduction steps recorded.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/reproduction-steps-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: reproduction-steps-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/reproduction-steps-htmx.json
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
   `widget_type: reproduction-steps-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `reproduction-steps` instance on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on lists missing markdown-rendered formatting
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `reproduction-steps-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
