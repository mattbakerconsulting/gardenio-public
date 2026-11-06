> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — suggested-test-cases → suggested-test-cases-htmx (Phase 4)

Bead: ka-0000001809 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000007835 ([reproduction-steps port doc](free-htmx-reproduction-steps-port.md)),
ka-0000000272 ([ai-recommended-actions port doc](free-htmx-ai-recommended-actions-port.md))

Twelfth pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346,
header-incident-summary in MR !2347, reproduction-steps in MR !2350,
ai-recommended-actions in MR !2351, and the remainder of the Phase 4–7
backlog (ka-0000005157). Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A —
port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`suggested-test-cases`,
`widgets_phase4.go::renderSuggestedTestCases`) and the htmx port
(`suggested-test-cases-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new
instances opt into the htmx path by writing
`widget_type: "suggested-test-cases-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the cases[] shape the Go renderer accepts —
flat array of case objects with `title`, optional `detail`, and optional
`coverage` band. Real instances on rich-report beads cutting the htmx
path emit the same shape as the Go path.

```jsonc
{
  "cases": [
    {
      "title": "Upload a 50 MB PDF",
      "detail": "Asserts the spinner clears within 30 s.",
      "coverage": "regression"
    },
    {
      "title": "Upload a malformed file",
      "coverage": "edge"
    }
  ]
}
```

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/suggested-test-cases-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestSuggestedTestCasesParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same cases through `renderSuggestedTestCases`
(Go) and through the htmx-compiled template, then asserts:

- both outputs carry the `<div class="ii-w-tests">` chrome
- both outputs carry a `<ul class="ii-w-tests-list">`
- both outputs carry one `<li class="ii-w-test-row">` per case with the
  disabled-checkbox label
- both outputs carry the `<span class="ii-w-test-title">` per case
- both outputs carry the `data-state` coverage pill when `coverage` is
  present
- both outputs carry the `<p class="ii-w-test-detail">` paragraph
  when `detail` is present
- both outputs fall into the empty-state arm ("No test cases
  suggested.") when the payload is `{}`

### Parity gaps

**None.** The Go renderer at
`widgets_phase4.go::renderSuggestedTestCases` does no normalisation, no
markdown rendering, and no button construction — it just lifts
`title` / `detail` / `coverage` off each case object via
`widgetString` and hands them to the template. Both paths render the
payload verbatim through `html/template` auto-escaping.

This makes suggested-test-cases the simplest port to date: pure
structural parity with no D2 carve-outs, no markdown side-channel, and
no Go-only helpers. Where ports like ai-recommended-actions or
fix-available-pending-branch require pre-banding or pre-rendered
markdown server-side, suggested-test-cases-htmx is bit-for-bit
equivalent to the Go path modulo the field-access shape (`.Title` Go
struct vs `.title` map index).

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
`widget_suggested_test_cases`) modulo the field-access shape
(`.Cases` Go struct vs `range .Payload.cases` map). That means:

- `<div class="ii-w-tests">` semantic chrome preserved
- `<ul class="ii-w-tests-list">` list semantics preserved
- per-row `<input type="checkbox" disabled />` decorative-checkbox
  semantics preserved (the disabled attribute keeps the control out of
  the tab order — operators read the row, they don't toggle it)
- per-row `<span class="ii-w-test-title">` and `data-state` coverage
  pill preserved
- empty-state copy ("No test cases suggested.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/suggested-test-cases-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: suggested-test-cases-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/suggested-test-cases-htmx.json
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
   `widget_type: suggested-test-cases-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `suggested-test-cases` instance on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows missing `data-state` coverage pills
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `suggested-test-cases-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
