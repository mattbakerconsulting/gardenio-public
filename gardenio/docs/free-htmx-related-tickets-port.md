> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — related-tickets → related-tickets-htmx (Phase 4)

Bead: ka-0000005540 · Migration backlog: ka-0000005157 ·
Reference port: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md))

Second pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`related-tickets`,
`widgets_phase4.go::renderRelatedTickets`) and the htmx port
(`related-tickets-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing `widget_type: "related-tickets-htmx"`.
No flag-day, no rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat `tickets[]` payload — not
the grouped shape sketched in the source widget-type bead
(ka-0000001154). Real instances on rich-report beads follow the Go
renderer's contract today; matching that contract is what unlocks
side-by-side soak.

```jsonc
{
  "tickets": [
    {
      "id":    "FSM-12345",      // jira ticket / MR iid / bead id
      "kind":  "jira",            // "jira" | "mr" | "bead"
      "title": "TMF upload broken",
      "url":   "https://...",     // optional; mr rows use it for the button
      "state": "open"
    }
  ]
}
```

`kind` and `state` are emitted verbatim — the Go renderer
`strings.ToLower`s them server-side, but the htmx template intentionally
does not (same approach as activity-log-htmx — see ka-0000004584 §3).
Producers should emit lowercase strings.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/related-tickets-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestRelatedTicketsParity` in `widgets_htmx_test.go` is the regression
bar. It feeds the same payload through `renderRelatedTickets` (Go) and
through the htmx-compiled template, then asserts:

- both outputs carry `data-kind`, `data-source-kind` (with the
  `jira → jira` / `mr → gitlab` mapping the existing
  `[data-source-kind]` CSS targets), `id`, and `title` strings
- both outputs carry the same `data-state` pill attribute (lowercased)
- both outputs produce exactly one `<li>` per ticket
- auto-escaping is identical (the test asserts `<bug>` in `title`
  escapes to `&lt;bug&gt;` in both paths)

### Parity gap — action button

The Go renderer wraps each row with a `<button class="ii-action-btn">`
built from `OpenJiraAction` / `OpenMRAction` descriptors. Those helpers
serialize an `ActionDescriptor` into JSON for `data-ii-action`; they
live server-side in `incident_intel_actions.go` and have no
template-only equivalent (sprig `fnRegistry` does not expose them).

The htmx template body therefore omits the button. `TestRelatedTicketsParity`
locks the gap in place — Go path emits two `ii-action-btn`s, htmx path
emits zero. Two paths to close the gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders the
   button HTML once and ships it as a `tickets[i].button_html` field;
   the template emits `{{.button_html | safehtml}}` (requires a
   trusted-html sanitizer registration).
2. **Lift the helpers into the template scope**: register
   `OpenJiraAction` / `OpenMRAction` as template funcs at compile time
   (research §3.3 — deferred until a second port asks for them).

Neither is required for related-tickets — the row chrome carries enough
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
(`templates/partials/widgets_phase4.html` / `widget_related_tickets`)
modulo the field-access shape (`.Rows[i]` vs `.Payload.tickets[i]`) and
the button parity gap above. That means:

- `<ul class="ii-w-related-list">` semantic list preserved
- `<code class="ii-w-related-id">` machine-readable id preserved
- `data-source-kind` source-icon hook preserved (same CSS / icon)
- `data-state` pill attribute preserved for high-contrast theming
- empty-state copy ("No related tickets.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/related-tickets-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: related-tickets-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/related-tickets-htmx.json
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
   `widget_type: related-tickets-htmx` parented to a real rich-report
   bead. Visually verify against the matching `related-tickets` instance
   on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows missing the action button
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `related-tickets-htmx` widget-instance on
   a real rich-report bead in prod beads-viewer. Update ka-0000005157
   and the gardener rich-report ka-0000003788 with the prod confirmation.
