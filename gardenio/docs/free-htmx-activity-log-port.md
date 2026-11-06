> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — activity-log → activity-log-htmx (Phase 6)

Bead: ka-0000004584 · Epic: ka-0000008085 · Research: ka-0000007457
([port-prototype.md](research/free-htmx-widget-port-prototype.md))

This is the first port done end-to-end through the read-side dispatcher
landed in this MR. It establishes the verification methodology every
subsequent port will follow.

## Strategy A — port-then-soak

The Go renderer (`activity-log`) and the htmx port (`activity-log-htmx`)
coexist as different slugs. Existing widget-instance beads keep rendering
through the Go path; new instances opt into the htmx path by writing
`widget_type: "activity-log-htmx"`. No flag-day, no rewrite-on-read.

The dispatcher in `widgets_htmx.go::lookupWidgetForReport` consults the
builtin registry first, then falls through to a daemon lookup for an
active `type=widget-type` bead with `renderer_kind=htmx`. A miss in both
paths renders the placeholder card (current Phase-0 behaviour).

## Verification methodology

### Visual fidelity

`TestActivityLogParity` in `widgets_htmx_test.go` is the regression-bar
for the port. It feeds the same payload through `renderActivityLog`
(Go) and through the htmx-compiled template, then asserts:

- both outputs contain every payload datetime as `<time datetime="…">`
  (this is the contract the relative-time enhancer in
  `static/js/relative-time.js` reads)
- both outputs contain every actor / text string verbatim
- both outputs carry the same `data-kind` and `data-state` attributes
  (lowercased) — the kind enum is what powers the existing CSS pill
  styling
- both outputs produce exactly one `<li>` per payload entry

Auto-escaping is identical because both paths run under
`html/template`. The parity test asserts `<500ms>` in payload `text`
escapes to `&lt;500ms&gt;` in both outputs.

### Performance

Per research §6.3 the read-time budget is 4 s for the full rich-report
page. The dispatcher's daemon round-trip is bounded by
`htmxResolveTimeoutSecs = 4`; on timeout the htmx fallback returns the
placeholder, never a half-rendered page.

The compiled template is cached on `(bead.ID, bead.UpdatedAt)`. First
hit per process pays one `template.Parse`; subsequent renders pay a
read-locked map lookup. Editing the widget-type bead bumps `updated_at`,
which invalidates the cache on the next dispatch (covered by
`TestCompileHtmxRenderer_CacheInvalidatesOnUpdate`).

### Accessibility

The htmx template body is byte-equivalent to the Go template
(`templates/partials/widgets_phase4.html` / `widget_activity_log`)
modulo the field-access shape (`.Entries[i]` vs `.Payload.entries[i]`).
That means:

- `<ol class="ii-w-activity-list">` semantic list preserved
- `<time datetime>` machine-readable timestamps preserved (screen-reader
  announces the relative-time enhancer's output)
- `data-state` pill attribute preserved for high-contrast theming
- empty-state copy ("No activity recorded.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/activity-log-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: activity-log-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/activity-log-htmx.json
```

Pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject the create if `template_source` does not parse, and reject any
later `widget-instance` write whose `data` does not validate against
the schema.

## Soak plan

1. Land this MR. Daemon-side validation is already live (ka-0000007359,
   commit 88251da2).
2. Post-merge: `kd create` the widget-type bead from the fixture against
   the gardenio-rwx daemon. Cut a smoke widget-instance with
   `widget_type: activity-log-htmx` parented to a real rich-report bead.
   Visually verify against the matching `activity-log` instance on the
   same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs (`widget-htmx: list
     widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
4. If clean, port the next two highest-traffic widgets per the epic
   schedule (ka-0000008085 children).
