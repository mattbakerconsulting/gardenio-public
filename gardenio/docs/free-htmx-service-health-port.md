> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — service-health → service-health-htmx (Phase 4)

Bead: ka-0000006096 · Migration backlog: ka-0000005157 ·
Reference port: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md))

Tenth pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346, header-incident-summary
in MR !2347, reproduction-steps (ka-0000007835), reporter-info
(ka-0000004065), event-timeline (ka-0000000243), ai-recommended-actions
(ka-0000000272), fix-available-pending-branch (ka-0000005506), and
vm-sparklines (ka-0000000715). Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`service-health`,
`widgets_phase4.go::renderServiceHealth`) and the htmx port
(`service-health-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing `widget_type: "service-health-htmx"`.
No flag-day, no rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat `services[]` payload — the
shape `incident_intel_gallery.go` already emits today. Real instances on
rich-report beads follow the Go renderer's contract; matching that
contract is what unlocks side-by-side soak.

```jsonc
{
  "services": [
    {
      "service": "study-web",            // service name (rendered as <code>)
      "state":   "critical",              // "healthy" | "warn" | "critical"
      "detail":  "5xx 12% (last 5m)"     // free-text detail cell
    }
  ]
}
```

`state` is emitted verbatim — the Go renderer `strings.ToLower`s it
server-side, but the htmx template intentionally does not (same approach
as activity-log-htmx — see ka-0000004584 §3 — and related-tickets-htmx).
Producers should emit lowercase strings to match the
`[data-state="healthy|warn|critical"]` CSS selectors.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/service-health-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestServiceHealthParity` in `widgets_htmx_test.go` is the regression
bar. It feeds the same payload through `renderServiceHealth` (Go) and
through the htmx-compiled template, then asserts:

- both outputs carry `data-ii-crossfilter-group="service-health"` on the
  wrapper (the cross-filter coordinator the existing CSS / JS targets)
- both outputs carry `data-ii-crossfilter-keys="service"` and
  `data-ii-crossfilter-service="<name>"` per row (per-row cross-filter
  identity)
- both outputs carry `<code>{service}</code>` and the `data-state` pill
  per row (lowercased)
- both outputs produce exactly three rows for a three-service payload
- auto-escaping is identical (the test asserts `<bug>` in `detail`
  escapes to `&lt;bug&gt;` in both paths)

### Parity gap — state sanitization

The Go renderer ToLowers state via `strings.ToLower`
(`widgets_phase4.go::renderServiceHealth`); the htmx body emits state
verbatim. Producers must pre-lower state to one of `healthy`, `warn`,
or `critical` to match the existing `[data-state="…"]` CSS selectors.

`TestServiceHealthParity` mirrors the Go normalization in the htmx
payload via `strings.NewReplacer` — same approach as
`TestRelatedTicketsParity` and `TestVMSparklinesParity`. Closing this
gap (lifting `strings.ToLower` into a `lower` template func) was
deferred in research §3.3 until a port asks for it. Service-health does
not — pre-lowering at producer time is cheap.

### Performance

Per research §6.3 the read-time budget is 4 s for the full rich-report
page. Dispatcher round-trip is bounded by `htmxResolveTimeoutSecs = 4`;
the compiled template is cached on `(bead.ID, bead.UpdatedAt)`. First
hit per process pays one `template.Parse`; subsequent renders pay a
read-locked map lookup. Editing the widget-type bead bumps `updated_at`,
which invalidates the cache on the next dispatch (already covered by
`TestCompileHtmxRenderer_CacheInvalidatesOnUpdate`).

### Accessibility

The htmx template body is byte-equivalent to the Go template
(`templates/partials/widgets_phase4.html` / `widget_service_health`)
modulo the field-access shape (`.Rows[i].Service` vs
`.Payload.services[i].service`). That means:

- `<table class="ii-w-table">` semantic table preserved with `<thead>` /
  `<tbody>` and column headers (Service / State / Detail)
- `<code>{service}</code>` machine-readable service name preserved
- `data-state` pill attribute preserved for high-contrast theming
- empty-state copy ("No service rows.") preserved
- empty-state em-dash ("—") preserved when `state` is missing

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/service-health-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: service-health-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/service-health-htmx.json
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
   `widget_type: service-health-htmx` parented to a real rich-report
   bead. Visually verify against the matching `service-health` instance
   on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows missing the data-state pill (e.g. when
     state is empty — the htmx template emits "—" identically)
   - cross-filter regressions (the wrapper still emits
     `data-ii-crossfilter-group="service-health"` and rows still emit
     per-service `data-ii-crossfilter-service`, so the existing
     `applyServerCrossFilter` path keeps working unchanged)
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `service-health-htmx` widget-instance on
   a real rich-report bead in prod beads-viewer. Update ka-0000005157
   and the gardener rich-report ka-0000003788 with the prod confirmation.
