> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — header-incident-summary → header-incident-summary-htmx (Phase 4)

Bead: ka-0000005304 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md))

Third pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334 and related-tickets in MR !2346. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`header-incident-summary`,
`widgets_phase4.go::renderHeaderIncidentSummary`) and the htmx port
(`header-incident-summary-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing
`widget_type: "header-incident-summary-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat top-level payload — title,
severity, status, started_at, plus the optional incident_id and
primary_action_{url,label} pair the action buttons consume. Real
instances on rich-report beads follow the Go renderer's contract today;
matching that contract is what unlocks side-by-side soak.

```text
{
  "title":                "study-web 502 spike",
  "severity":             "p1",                          // "p1" | "p2" | "p3"
  "status":               "open",                        // "open" | "ack" | ...
  "started_at":           "2026-05-06T12:00:00Z",
  "incident_id":          "ka-incident-001",             // optional; powers Acknowledge
  "primary_action_url":   "https://gitlab.com/...",      // optional; powers Investigate
  "primary_action_label": "Open MR"                       // optional; button label override
}
```

`severity` and `status` are emitted verbatim — the Go renderer
`strings.ToLower`s them server-side, but the htmx template intentionally
does not (same approach as activity-log-htmx — see ka-0000004584 §3).
Producers should emit lowercase strings.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/header-incident-summary-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestHeaderIncidentSummaryParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same payload through
`renderHeaderIncidentSummary` (Go) and through the htmx-compiled
template, then asserts:

- both outputs carry the `<div class="ii-w-header">` /
  `<div class="ii-w-header-top">` chrome
- both outputs carry the same `data-severity` and `data-status` pill
  attributes (lowercased)
- both outputs render the title under one `<h3 class="ii-w-title">`
- both outputs render the started_at as a single
  `<time class="ii-relative-time" datetime="…">` element
- auto-escaping is identical (the test asserts `<bug>` in `title`
  escapes to `&lt;bug&gt;` in both paths)

### Parity gap — action buttons

The Go renderer wraps each header with two
`<button class="ii-action-btn">` elements: an Investigate button built
from `OpenMRAction(primary_action_label, primary_action_url)`, and an
Acknowledge button built from `AckIncidentAction(incident_id, "")`.
Those helpers serialize an `ActionDescriptor` into JSON for
`data-ii-action`; they live server-side in `incident_intel_actions.go`
and have no template-only equivalent (sprig `fnRegistry` does not
expose them).

The htmx template body therefore omits the buttons.
`TestHeaderIncidentSummaryParity` locks the gap in place — Go path
emits two `ii-action-btn`s, htmx path emits zero. Two paths to close
the gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders
   each button HTML once and ships it as a `primary_action_html` /
   `ack_action_html` field; the template emits
   `{{.primary_action_html | safehtml}}` (requires a trusted-html
   sanitizer registration).
2. **Lift the helpers into the template scope**: register
   `OpenMRAction` / `AckIncidentAction` as template funcs at compile
   time (research §3.3 — deferred until a second port asks for them).

Neither is required for header-incident-summary — the header chrome
carries enough information for the soak; the action buttons remain a
Go-renderer-only affordance for now.

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
(`templates/partials/widgets_phase4.html` /
`widget_header_incident_summary`) modulo the field-access shape
(`.Severity` vs `index .Payload "severity"`) and the button parity gap
above. That means:

- `<div class="ii-w-header">` + `<div class="ii-w-header-top">`
  semantic chrome preserved
- `<h3 class="ii-w-title">` heading-level preserved
- `data-severity` / `data-status` pill attributes preserved for
  high-contrast theming
- `<time class="ii-relative-time" datetime="…">` semantics preserved
  for the relative-time enhancer
- "Started" copy preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/header-incident-summary-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: header-incident-summary-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/header-incident-summary-htmx.json
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
   `widget_type: header-incident-summary-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `header-incident-summary` instance on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on headers missing the action buttons
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `header-incident-summary-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
