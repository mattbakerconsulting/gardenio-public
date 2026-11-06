> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — vm-sparklines → vm-sparklines-htmx (Phase 4)

Bead: ka-0000000715 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000000243 ([event-timeline port doc](free-htmx-event-timeline-port.md))

Sixth pre-defined widget on the free-htmx substrate, after activity-log
(MR !2334), related-tickets (MR !2346), header-incident-summary
(MR !2347), reproduction-steps (ka-0000007835), reporter-info
(ka-0000004065), and event-timeline (ka-0000000243). Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A —
port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`vm-sparklines`,
`widgets_phase7.go::renderVMSparklines`) and the htmx port
(`vm-sparklines-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing `widget_type: "vm-sparklines-htmx"`.
No flag-day, no rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat `series[]` payload but
requires producers to ship pre-computed `latest`, `min`, and `max`
numbers per series. The Go path derives those from `points[]` (or a
parallel `x`/`y` array) inside `renderVMSparklines`; replicating that
arithmetic inside an html/template would require helpers the template
scope does not have. Real producers already compute those values
upstream — VictoriaMetrics queries return min/max/latest as dedicated
fields — so the htmx wire shape is what the data already looks like.

```text
{
  "title":              "VM sparklines",            // optional
  "subtitle":           "incident-001",             // optional
  "group_id":           "ii-ts-incident-001",       // hover-sync group
  "cross_filter_badge": "informational",            // optional D4 badge
  "series": [
    {
      "service": "study-web",                       // row label
      "metric":  "p95",                             // row sublabel
      "unit":    "ms",                              // optional
      "state":   "warn",                            // healthy / warn / critical
      "latest":  315.0,                             // pre-computed
      "min":     220.0,                             // pre-computed
      "max":     315.0                              // pre-computed
    }
  ]
}
```

`state` is emitted verbatim — the Go renderer lowercases it via
`strings.ToLower`, but the htmx template intentionally does not (same
approach as activity-log-htmx and related-tickets-htmx). Producers
should pre-lower to one of `healthy`, `warn`, `critical`.

`latest`, `min`, and `max` are formatted with `{{printf "%.2f" ...}}`
inside the template so the rendered chrome (`<strong>315.00</strong>`,
`min 220.00 · max 315.00`) byte-matches the Go renderer.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/vm-sparklines-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestVMSparklinesParity` in `widgets_htmx_test.go` is the regression
bar. It feeds the same payload (containing both `points[]` and
pre-computed `latest`/`min`/`max`) through `renderVMSparklines` (Go)
and through the htmx-compiled template, then asserts:

- both outputs carry the wrapper (`data-ii-ts-group`), header
  (`<h4 class="ii-w-sparklines-title">`), and `<ul class="ii-w-sparklines-rows">`
- both outputs carry per-row `data-state` pills, the
  `<span class="ii-w-sparkline-service">…</span>` row label,
  `<span class="ii-w-sparkline-metric">…</span>` sublabel, and the
  formatted `latest <strong>NNN.NN</strong>` + `min NNN.NN · max NNN.NN`
  readings strings
- both outputs produce exactly one `<li>` per series

### Parity gaps

Three intentional gaps are locked in by `TestVMSparklinesParity`:

1. **uPlot canvas (`data-uplot-config`)** — the Go renderer emits a
   `<div class="ii-w-sparkline-canvas" id="…" data-uplot-config='…'
   data-uplot-kind="sparkline"></div>` per series whose JSON config is
   built by `MarshalSparkConfig`. That helper lives server-side and has
   no template-scope analogue. The htmx body emits the static `<ul>`
   fallback only — uPlot interactivity is not part of the htmx port.
   The test asserts the htmx output does NOT contain `data-uplot-kind=`
   or `data-uplot-config=`.

2. **`latest`/`min`/`max` derivation** — the Go path computes these
   from `points[]`/`x[]`/`y[]` inside `renderVMSparklines` (uses
   `parseTimePoints` + `minMax` helpers). The htmx template requires
   producers to ship them pre-computed; the parity test supplies both
   shapes in the same payload so the Go renderer's computed values
   match the htmx template's pre-computed values.

3. **`state` sanitization** — the Go path lowercases the state via
   `strings.ToLower`. The htmx template emits state verbatim — payloads
   here use pre-lowered state strings to match.

Two paths to close the canvas gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders the
   uPlot config blob once and ships it as a `uplot_config` field; the
   template emits `<div data-uplot-config="{{.uplot_config}}">`.
2. **Lift the helper into the template scope**: register
   `MarshalSparkConfig` as a template func at compile time
   (research §3.3 — deferred until a second port asks for it).

Neither is required for vm-sparklines — the row chrome carries enough
information for the soak.

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
(`templates/partials/widgets_phase7.html` / `widget_vm_sparklines`)
modulo the field-access shape (`.Series[i].Latest` vs
`.Payload.series[i].latest`) and the canvas parity gap above. That
means:

- `<ul class="ii-w-sparklines-rows">` semantic list preserved
- `<span class="ii-pill" data-state="…">` state-pill hook preserved
  (same CSS / icon)
- `<span class="ii-w-sparkline-service">…</span>` /
  `<span class="ii-w-sparkline-metric">…</span>` row chrome preserved
- formatted `latest` + `min · max` readings line preserved
- empty-state copy ("No metric series available.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/vm-sparklines-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: vm-sparklines-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/vm-sparklines-htmx.json
```

Pre-write hooks parse `template_source` under `html/template` and reject
malformed inputs at createBead/updateBead time, so a bad fixture
surfaces immediately. Schema validation runs against widget-instance
writes per the same pre-write hook.

## Soak plan

1. Land this MR. Daemon-side validation has been live since
   ka-0000007359 (commit 88251da2).
2. Post-merge: `kd create` the widget-type bead from the fixture against
   the gardenio-rwx daemon. Cut a smoke widget-instance with
   `widget_type: vm-sparklines-htmx` parented to a real rich-report
   bead. Visually verify against the matching `vm-sparklines` instance
   on the same report (uPlot canvas absent in htmx — expected gap).
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows lacking the canvas
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `vm-sparklines-htmx` widget-instance on
   a real rich-report bead in prod beads-viewer. Update ka-0000005157
   and the gardener rich-report ka-0000003788 with the prod confirmation.
