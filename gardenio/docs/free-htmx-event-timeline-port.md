> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — event-timeline → event-timeline-htmx (Phase 4)

Bead: ka-0000000243 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md))

Third pre-defined widget on the free-htmx substrate, after activity-log
(MR !2334) and related-tickets (MR !2346). Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`event-timeline`,
`widgets_phase7.go::renderEventTimeline`) and the htmx port
(`event-timeline-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing `widget_type: "event-timeline-htmx"`.
No flag-day, no rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat `events[]` payload with an
optional `lanes[]` array for the chip header. Real instances on
rich-report beads follow the Go renderer's contract today; matching
that contract is what unlocks side-by-side soak.

```text
{
  "title":              "Event timeline",          // optional
  "subtitle":           "incident-001",            // optional
  "group_id":           "ii-ts-incident-001",      // hover-sync group
  "cross_filter_badge": "informational",           // optional D4 badge
  "events": [
    {
      "when":   "2026-05-06T11:50:00Z",            // RFC3339
      "kind":   "gitlab",                          // pre-sanitized source kind
      "title":  "deploy !2185",
      "detail": "1204 events",                     // optional
      "url":    "https://gitlab.com/..."           // optional
    }
  ],
  "lanes": [                                       // optional chip header
    {"kind": "gitlab", "label": "Code (GitLab)", "count": 1, "index": 0}
  ]
}
```

`kind` is emitted verbatim — the Go renderer collapses unknown kinds to
`"code"` via `sanitizeSourceKind`, but the htmx template intentionally
does not (same approach as activity-log-htmx and related-tickets-htmx).
Producers should pre-sanitize to one of the eight Phase-1 source kinds:
`mezmo`, `victoria-metrics`, `victoria-traces`, `sentry`, `jira`,
`gitlab`, `code`, `slack`.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/event-timeline-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestEventTimelineParity` in `widgets_htmx_test.go` is the regression
bar. It feeds the same payload through `renderEventTimeline` (Go) and
through the htmx-compiled template, then asserts:

- both outputs carry the wrapper (`data-ii-ts-group`), header
  (`<h4 class="ii-w-event-timeline-title">`), and `<ol class="ii-w-event-timeline-list">`
- both outputs carry per-row `data-source-kind`,
  `data-ii-crossfilter-keys="source-kind"`,
  `data-ii-crossfilter-source-kind="<kind>"`,
  `<time class="ii-relative-time" datetime="...">`, the title chrome,
  the detail chrome, and the `<a class="ii-w-event-timeline-event-link">`
- both outputs produce exactly one `<li>` per event
- auto-escaping is identical (the test asserts `<bug>` in `title`
  escapes to `&lt;bug&gt;` in both paths)

### Parity gaps

Three intentional gaps are locked in by `TestEventTimelineParity`:

1. **uPlot canvas (`data-uplot-config`)** — the Go renderer emits a
   `<div class="ii-w-event-timeline-canvas" data-uplot-config='...'
   data-uplot-kind="timeline"></div>` whose JSON config is built by
   `MarshalTimelineConfig`. That helper lives server-side and has no
   template-scope analogue (sprig `fnRegistry` does not expose it). The
   htmx body emits the static `<ol>` fallback only — uPlot interactivity
   is not part of the htmx port. The test asserts the htmx output does
   NOT contain `data-uplot-kind=`.

2. **Lanes header** — the Go path derives lanes from sorted events
   server-side (`renderEventTimeline` → `Lanes []timelineLane`). The
   htmx template renders a lanes header only when the payload supplies
   a pre-computed `lanes[]` array; producers who want chip header
   parity opt in by emitting it. `TestCompileHtmxRenderer_EventTimelineLanes`
   exercises that opt-in path.

3. **`data-lane` on each `<li>`** — the Go path emits `data-lane="N"`
   on every row using the computed lane index. The htmx body omits the
   attribute. The test asserts the Go output contains ≥2 `data-lane`
   attrs and the htmx output contains zero. (This trades a small CSS
   cross-filter affordance for template-scope simplicity; reopen if a
   future port surfaces lane indices into the wire contract.)

Two paths to close the canvas gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders the
   uPlot config blob once and ships it as a `uplot_config` field; the
   template emits `<div data-uplot-config="{{.uplot_config}}">`.
2. **Lift the helper into the template scope**: register
   `MarshalTimelineConfig` as a template func at compile time
   (research §3.3 — deferred until a second port asks for it).

Neither is required for event-timeline — the row chrome carries enough
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

The htmx template body is byte-equivalent to the Go template
(`templates/partials/widgets_phase7.html` / `widget_event_timeline`)
modulo the field-access shape (`.Events[i]` vs `.Payload.events[i]`)
and the canvas / lane parity gaps above. That means:

- `<ol class="ii-w-event-timeline-list">` semantic list preserved
- `<time class="ii-relative-time" datetime="...">` relative-time hook
  preserved
- `<span class="ii-source ii-w-event-timeline-source"
  data-source-kind="...">` source-icon hook preserved (same CSS / icon)
- title / detail / open-link chrome preserved
- empty-state copy ("No timeline events.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/event-timeline-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: event-timeline-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/event-timeline-htmx.json
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
   `widget_type: event-timeline-htmx` parented to a real rich-report
   bead. Visually verify against the matching `event-timeline` instance
   on the same report (uPlot canvas absent in htmx — expected gap).
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows lacking `data-lane`
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `event-timeline-htmx` widget-instance on
   a real rich-report bead in prod beads-viewer. Update ka-0000005157
   and the gardener rich-report ka-0000003788 with the prod confirmation.
