> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — user-reported-issue-header → user-reported-issue-header-htmx (Phase 4)

Bead: ka-0000000993 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000001237 ([workaround port doc](free-htmx-workaround-port.md))

Twelfth free-htmx port — closes the Phase-4 user-reporting widget cluster
alongside reporter-info-htmx and reproduction-steps-htmx. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`user-reported-issue-header`,
`widgets_phase4.go::renderUserReportedIssueHeader`) and the htmx port
(`user-reported-issue-header-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing
`widget_type: "user-reported-issue-header-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's payload shape — five optional
string fields. Real instances on rich-report beads cutting the htmx path
typically emit `title` + `ticket_id` + `severity`; `ticket_url` and
`reported_at` are populated when the upstream Jira / form provides them.

```text
{
  "title":       "Cannot upload TMF document",
  "ticket_id":   "FSM-12345",
  "ticket_url":  "https://acme.atlassian.net/browse/FSM-12345",
  "severity":    "p2",
  "reported_at": "2026-05-05T10:00:00Z"
}
```

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/user-reported-issue-header-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestUserReportedIssueHeaderParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same payload through
`renderUserReportedIssueHeader` (Go) and through the htmx-compiled
template, then asserts:

- both outputs carry the `<div class="ii-w-user-header">` chrome
- both outputs carry the `<div class="ii-w-header-top">` row
- both outputs carry the lowercased `data-severity="p2"` pill
- both outputs HTML-escape `<50MB>` identically in the title
- both outputs link the ticket via the Jira URL with
  `target="_blank" rel="noopener"` and wrap the ticket id in
  `<code>FSM-12345</code>`
- both outputs carry the reported `<time class="ii-relative-time"
  datetime="…">` element
- the empty payload renders the chrome shell with the unconditional
  `<p class="ii-w-meta">` paragraph and no pill / title / ticket / time
  elements in either path

### Parity gap — "Open ticket" action button

The Go renderer constructs an action button when `ticket_id` is set:

```go
v.Button = template.HTML(actionButtonJSON("Open ticket",
    OpenJiraAction("", v.TicketID)))
```

`actionButtonJSON` and `OpenJiraAction` are Go-side helpers that emit a
`<button class="ii-action-btn">…</button>` carrying a JSON action
descriptor for the rich-report client to wire up. html/template has no
template-only equivalent, so the htmx template body omits the button —
the same gap as `header-incident-summary-htmx` (Investigate /
Acknowledge buttons) and `related-tickets-htmx`.

`TestUserReportedIssueHeaderParity` locks the gap in place: it asserts
"Open ticket" appears in the Go output and is absent from the htmx
output. Closing the gap requires either pre-rendering the button
HTML server-side and shipping it as a `body_html` field, or registering
a template func that builds the descriptor from a Jira ticket id —
deferred until a second port asks for it (research §3.3).

### Parity gap — severity case-folding

The Go renderer normalises severity via `strings.ToLower` so the
existing CSS rules (`[data-severity="p1"]`, `[data-severity="p2"]`,
etc.) match regardless of producer case. html/template has no `lower`
template func registered (research §3.2), so the htmx template emits
the severity verbatim. Producers cutting a `user-reported-issue-header-htmx`
widget-instance must pre-lowercase severity in the payload.

`TestUserReportedIssueHeaderParity` mirrors the Go behaviour by
substituting `"P2"` → `"p2"` in the htmx payload (same approach as
`TestHeaderIncidentSummaryParity`). The rich-report producers
(jira-bridge, fixture authors) already emit lowercase severity in
practice, and the schema does not enumerate values, so the normalisation
gap has no observable user impact today.

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
(`templates/partials/widgets_phase4.html` / `widget_user_reported_issue_header`)
modulo the field-access shape (`.Title` / `.TicketID` named struct
fields vs `index .Payload "title"` / `index .Payload "ticket_id"`) and
the two parity gaps above. That means:

- `<div class="ii-w-user-header">` semantic chrome preserved
- `<div class="ii-w-header-top">` row preserved
- `<span class="ii-pill" data-severity="…">` pill preserved
- optional `<h3 class="ii-w-title">…</h3>` preserved
- `<p class="ii-w-meta">` paragraph preserved (unconditional in both paths)
- ticket anchor (`target="_blank" rel="noopener"`) preserved
- `<time class="ii-relative-time" datetime="…">` preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/user-reported-issue-header-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: user-reported-issue-header-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/user-reported-issue-header-htmx.json
```

Pre-write hooks parse `template_source` under `html/template` and
reject malformed inputs at createBead/updateBead time, so a bad fixture
surfaces immediately. Schema validation runs against widget-instance
writes per the same pre-write hook.

## Soak plan

1. Land this MR. Daemon-side validation has been live since
   ka-0000007359 (commit 88251da2).
2. Post-merge: `kd create` the widget-type bead from the fixture
   against the gardenio-rwx daemon. Cut a smoke widget-instance with
   `widget_type: user-reported-issue-header-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `user-reported-issue-header` instance on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on user-reported-issue cards missing the action
     button or the lowercased severity pill
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `user-reported-issue-header-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
