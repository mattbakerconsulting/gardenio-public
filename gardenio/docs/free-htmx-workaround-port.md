> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — workaround → workaround-htmx (Phase 4)

Bead: ka-0000001237 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000007835 ([reproduction-steps port doc](free-htmx-reproduction-steps-port.md))

Eleventh free-htmx port — completes the Phase-4 markdown-body widget pair
(reproduction-steps shipped first in ka-0000007835). Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A — port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`workaround`,
`widgets_phase4.go::renderWorkaround`) and the htmx port
(`workaround-htmx`) coexist as different slugs. Existing widget-instance
beads keep rendering through the Go path; new instances opt into the
htmx path by writing `widget_type: "workaround-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the `headline` + `markdown` shape the Go renderer
already accepts. Real instances on rich-report beads cutting the htmx
path emit both fields; the headline is optional and the body falls
back to the empty-state placeholder when missing.

```jsonc
{
  "headline": "Use the legacy upload path",
  "markdown": "Append `?legacy=1` to the upload URL. Logged in users are auto-redirected back."
}
```

The Go renderer feeds the `markdown` field through goldmark. The htmx
template body has no goldmark hook, so the field is rendered verbatim
inside a `<p>` (HTML-escaped by html/template) — the parity gap is
documented below. Producers that need rich markup keep writing
`widget_type: workaround` through the Go path.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/workaround-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestWorkaroundParity` in `widgets_htmx_test.go` is the regression bar.
It feeds the same `headline` + `markdown` payload through
`renderWorkaround` (Go) and through the htmx-compiled template, then
asserts:

- both outputs carry the `<div class="ii-w-markdown ii-w-workaround">`
  chrome
- both outputs carry the `ii-w-workaround-headline` paragraph with the
  bold headline text
- the markdown body text appears verbatim in both outputs
- the empty-state copy fires in both paths when the payload is `{}`

### Parity gap — markdown rendering

The Go renderer reads the `markdown` field and feeds it through
goldmark via `widgetRenderCtx.md`. Goldmark turns backtick-delimited
spans into `<code>…</code>`, `**bold**` into `<strong>…</strong>`,
`[links](#)` into `<a href>` anchors, etc.

The htmx template body cannot run goldmark — html/template has no
markdown hook, and the renderer compiles under the substrate
established in ka-0000007359 / MR !2334. The htmx template therefore
wraps the raw `markdown` string in a single `<p>` with html/template's
default HTML-escaping (so `?legacy=1` survives intact while `<` becomes
`&lt;`).

`TestWorkaroundParity` locks the gap in place: when `markdown` carries
"`` `?legacy=1` ``", the Go path emits `<code>?legacy=1</code>` while
the htmx path emits the literal text with the backticks escaped. Two
paths to close the gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders the
   markdown to HTML once and ships it as a `body_html` field; the
   template emits `{{.body_html | safehtml}}` (requires a trusted-html
   sanitizer registration).
2. **Lift goldmark into the template scope**: register a `markdown`
   template func at compile time (research §3.3 — deferred until a
   second port asks for it; same gap as reproduction-steps-htmx).

Neither is required for workaround — operator-authored workaround
payloads on real rich-reports tend to be a single sentence, and the
chrome carries enough information for the soak.

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
(`templates/partials/widgets_phase4.html` / `widget_workaround`) modulo
the field-access shape (`.Headline` / `.HTML` named struct fields vs
`index .Payload "headline"` / `index .Payload "markdown"`) and the
markdown parity gap above. That means:

- `<div class="ii-w-markdown ii-w-workaround">` semantic chrome preserved
- optional `<p class="ii-w-workaround-headline"><strong>…</strong></p>`
  preserved
- empty-state copy ("No workaround documented.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/workaround-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: workaround-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/workaround-htmx.json
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
   `widget_type: workaround-htmx` parented to a real rich-report bead.
   Visually verify against the matching `workaround` instance on the
   same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on workaround cards missing markdown-rendered
     formatting
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `workaround-htmx` widget-instance on a
   real rich-report bead in prod beads-viewer. Update ka-0000005157
   and the gardener rich-report ka-0000003788 with the prod
   confirmation.
