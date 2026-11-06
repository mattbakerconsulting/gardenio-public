> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — source-code-analysis → source-code-analysis-htmx (Phase 5)

Bead: ka-0000006619 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000007835 ([reproduction-steps port doc](free-htmx-reproduction-steps-port.md)),
ka-0000004065 ([reporter-info port doc](free-htmx-reporter-info-port.md)),
ka-0000000243 ([event-timeline port doc](free-htmx-event-timeline-port.md)),
ka-0000000272 ([ai-recommended-actions port doc](free-htmx-ai-recommended-actions-port.md))

Sixth pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346,
header-incident-summary in MR !2347, reproduction-steps in
MR !2350, reporter-info, event-timeline, and
ai-recommended-actions. **First Phase-5 port** — source-code-analysis is a
code-highlighted widget (`widgets_phase5.go`) so the parity-gap surface is
wider than the Phase-4 ports. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A —
port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`source-code-analysis`,
`widgets_phase5.go::renderSourceCodeAnalysis`) and the htmx port
(`source-code-analysis-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new
instances opt into the htmx path by writing
`widget_type: "source-code-analysis-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the findings[] shape the Go renderer accepts —
flat array of finding objects with `path`, optional `line`,
optional `function`, `severity` band, optional `headline`, optional
`detail`, `language`, and `code`. Real instances on rich-report
beads cutting the htmx path emit pre-lower-cased severity strings.

```jsonc
{
  "title": "Top hot lines",
  "findings": [
    {
      "path": "controller/main.go",
      "line": 42,
      "function": "run",
      "severity": "error",
      "headline": "nil deref on missing config",
      "detail": "Hit in 3/3 traces",
      "language": "go",
      "code": "if cfg.X == nil { panic(0) }"
    }
  ]
}
```

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/source-code-analysis-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestSourceCodeAnalysisParity` in `widgets_htmx_test.go` is the
regression bar. It feeds the same findings through `renderSourceCodeAnalysis`
(Go) and through the htmx-compiled template, then asserts:

- both outputs carry the `<div class="ii-w-sca">` chrome
- both outputs carry the `<h4 class="ii-w-sca-title">` when `title` is set
- both outputs carry an `<ol class="ii-w-sca-list">`
- both outputs carry one `<li class="ii-w-sca-finding">` per finding with
  the `<header class="ii-w-sca-head">` row
- both outputs carry the severity `<span class="ii-pill" data-state="…">`
  with lower-cased severity values
- both outputs carry the `<code class="ii-w-sca-path">` element with
  optional `:line` suffix
- both outputs carry the `<span class="ii-w-sca-fn">in <code>fn()</code></span>`
  span when `function` is present
- both outputs carry the `<p class="ii-w-sca-headline">` and
  `<p class="ii-w-sca-detail">` paragraphs
- both outputs carry the `<div class="ii-w-sca-code" data-language="…">`
  wrapper when `code` is present

### Parity gaps

Three intentional gaps vs. the Go renderer, all locked in
`TestSourceCodeAnalysisParity`:

#### 1. Chroma syntax highlighting

The Go path runs each finding's `code` field through the shared
`codeHighlighter` (chroma, github-dark theme, server-side, cached) and
emits chroma's coloured `<span style="color:…">` token spans inside
`<pre class="chroma">`. The htmx template body has no Go-side hook into
chroma, so the rendered `<pre>` carries plain escaped text only — the
same fidelity the `plainTextPre` fallback produces when chroma fails.

The wrapper chrome is preserved on both paths:
`<div class="ii-w-sca-code" data-language="…">` is identical, so the
Phase-5 CSS that targets `.ii-w-sca-code` still applies. Producers
cutting a source-code-analysis-htmx widget-instance get readable code
without the colourised tokens.

A future research call (ka-0000002102 §3.3) may register a
template-side highlighter fn at compile time. Until that lands,
widget-instances that need chroma highlighting should keep using the
`source-code-analysis` Go slug.

#### 2. Severity normalisation

The Go path runs `payload.severity` through `strings.ToLower(...)`
before emitting `data-state="…"`. The htmx template body has no Go-side
hook, so it renders the `severity` field verbatim. Producers cutting a
source-code-analysis-htmx widget-instance MUST emit severity as a
pre-lower-cased string (`"error"` / `"warn"` / `"info"`) for the
Phase-1 state-pill CSS to match.

The parity test's Go-side payload uses an upper-case `"ERROR"` to
exercise the Go normalisation; the htmx-side payload pre-lower-cases.
Both renders must produce identical `data-state="error"` chrome.

The same approach is used in `TestActivityLogParity` /
`TestRelatedTicketsParity` / `TestHeaderIncidentSummaryParity`:
pre-normalise the htmx payload to the wire shape the Go path produces
server-side.

#### 3. Jump-to-IDE buttons

The Go renderer emits one `<button class="ii-action-btn">` per finding
that carries a `path`, built via the `OpenIDEAction` descriptor. That
helper lives in Go and has no template-side equivalent on the
substrate established in ka-0000007359 / MR !2334. The htmx template
therefore omits the button block entirely; the optional jump-to-line
action is silently ignored.

This is the same gap as in
[`free-htmx-header-incident-summary-port.md`](free-htmx-header-incident-summary-port.md)
and [`free-htmx-ai-recommended-actions-port.md`](free-htmx-ai-recommended-actions-port.md) —
button rendering blocks on a future research call (see ka-0000002102
§3.3 — register a button-builder template func at compile time).
Until that lands, widget-instances that need actionable jump-to-line
buttons should keep using the `source-code-analysis` Go slug.

`TestSourceCodeAnalysisParity` locks the gap in place: the Go output
carries exactly two `ii-action-btn` (one per finding with a `path`);
the htmx output carries zero. Change with eyes-open.

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
(`templates/partials/widgets_phase5.html` /
`widget_source_code_analysis`) modulo the field-access shape
(`.Findings` Go struct vs `range .Payload.findings` map) and the three
parity gaps above. That means:

- `<div class="ii-w-sca">` semantic chrome preserved
- `<ol class="ii-w-sca-list">` numbered-list semantics preserved
- per-finding severity `data-state` attribute preserved
- `<code>` wrappers on path and function name preserved
- empty-state copy ("No source-code findings.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/source-code-analysis-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: source-code-analysis-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/source-code-analysis-htmx.json
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
   `widget_type: source-code-analysis-htmx` parented to a real
   rich-report bead. Visually verify against the matching
   `source-code-analysis` instance on the same report — confirm the
   chrome shells match modulo the documented parity gaps (chroma
   colours, jump-to-IDE button).
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on findings missing `data-state` attributes
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `source-code-analysis-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788
   with the prod confirmation.
