> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — reporter-info → reporter-info-htmx (Phase 4)

Bead: ka-0000004065 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md))

Fourth pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346, and
header-incident-summary in MR !2347. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`reporter-info`,
`widgets_phase4.go::renderReporterInfo`) and the htmx port
(`reporter-info-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing `widget_type: "reporter-info-htmx"`.
No flag-day, no rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat top-level payload — name,
email, org, role. Real instances on rich-report beads follow the Go
renderer's contract today; matching that contract is what unlocks
side-by-side soak.

```jsonc
{
  "name":  "Perry Tate",
  "email": "paulo@example.com",   // optional; powers the Copy-email button (Go path)
  "org":   "Acme",
  "role":  "Site coordinator"
}
```

All four fields are optional — empty payload renders the dl shell with
zero rows. Unlike the previous three ports, reporter-info has no
`ToLower` normalization on the Go side, so values are emitted verbatim
in both paths.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/reporter-info-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestReporterInfoParity` in `widgets_htmx_test.go` is the regression
bar. It feeds the same payload through `renderReporterInfo` (Go) and
through the htmx-compiled template, then asserts:

- both outputs carry the `<div class="ii-w-reporter">` wrapper and the
  `<dl class="field-list">` shell
- both outputs render four `<dt>`/`<dd>` pairs (Name, Email, Org, Role)
- both outputs wrap the email in `<code>…</code>`
- auto-escaping is identical (the test asserts `<ops>` in `name`
  escapes to `&lt;ops&gt;` in both paths)

### Parity gap — Copy-email button

The Go renderer wraps the email row with a
`<button class="ii-action-btn">Copy email</button>` element built from
`CopyToClipboardAction("Copy email", email)`. That helper serializes an
`ActionDescriptor` into JSON for `data-ii-action`; it lives server-side
in `incident_intel_actions.go` and has no template-only equivalent
(sprig `fnRegistry` does not expose it).

The htmx template body therefore omits the button.
`TestReporterInfoParity` locks the gap in place — Go path emits one
`ii-action-btn`, htmx path emits zero. Two paths to close the gap if a
future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders the
   button HTML once and ships it as a `copy_email_button_html` field;
   the template emits `{{.copy_email_button_html | safehtml}}`
   (requires a trusted-html sanitizer registration).
2. **Lift the helper into the template scope**: register
   `CopyToClipboardAction` as a template func at compile time
   (research §3.3 — deferred until a second port asks for it).

Neither is required for reporter-info — the dl chrome carries enough
information for the soak; the Copy-email affordance remains a
Go-renderer-only convenience for now.

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
(`templates/partials/widgets_phase4.html` / `widget_reporter_info`)
modulo the field-access shape (`.Name` vs `index .Payload "name"`) and
the button parity gap above. That means:

- `<div class="ii-w-reporter">` semantic chrome preserved
- `<dl class="field-list">` definition-list semantics preserved
  (screen readers announce term/definition pairs)
- `<code>` wrapping for the email address preserved
- field labels (Name / Email / Org / Role) preserved verbatim

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/reporter-info-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: reporter-info-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/reporter-info-htmx.json
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
   `widget_type: reporter-info-htmx` parented to a real rich-report
   bead. Visually verify against the matching `reporter-info` instance
   on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows missing the Copy-email button
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `reporter-info-htmx` widget-instance on
   a real rich-report bead in prod beads-viewer. Update ka-0000005157
   and the gardener rich-report ka-0000003788 with the prod confirmation.
