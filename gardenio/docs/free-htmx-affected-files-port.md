> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — affected-files → affected-files-htmx (Phase 4)

Bead: ka-0000005289 · Migration backlog: ka-0000005157 ·
Reference ports: ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000004065 ([reporter-info port doc](free-htmx-reporter-info-port.md))

Seventh pre-defined widget on the free-htmx substrate, after activity-log
shipped in MR !2334, related-tickets in MR !2346, header-incident-summary
in MR !2347, reproduction-steps in MR !2350, reporter-info, ai-recommended-actions,
event-timeline, fix-available-pending-branch, and vm-sparklines. Same
dispatcher (`widgets_htmx.go::lookupWidgetForReport`), same Strategy A —
port-then-soak.

## Strategy A — port-then-soak

The Go renderer (`affected-files`,
`widgets_phase4.go::renderAffectedFiles`) and the htmx port
(`affected-files-htmx`) coexist as different slugs. Existing
widget-instance beads keep rendering through the Go path; new instances
opt into the htmx path by writing `widget_type: "affected-files-htmx"`.
No flag-day, no rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

The htmx port mirrors the Go renderer's flat top-level payload — a
`files[]` array, each row carrying `path` (string), `line` (number),
and `reason` (string). Real instances on rich-report beads follow the
Go renderer's contract today; matching that contract is what unlocks
side-by-side soak.

```jsonc
{
  "files": [
    {
      "path":   "gardenio/controller/cmd/beads-viewer/server.go",
      "line":   442,                                     // optional; 0 / missing → suppress `:line` suffix
      "reason": "new auth middleware introduced"         // optional
    }
  ]
}
```

`files[]` is optional — empty / missing array renders the empty-state
copy ("No affected files."). Within a row, `path` is the only field
that drives layout; `line` and `reason` are both suppressed when zero
or missing. Unlike the previous header-incident-summary or
ai-recommended-actions ports, affected-files has no `ToLower`
normalisation on the Go side, so values are emitted verbatim in both
paths (modulo HTML auto-escaping).

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/affected-files-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

### Note on numeric `line` values

JSON numbers arrive in the template as `float64`. Go's `html/template`
formats integer-valued floats without a decimal point — `442` not
`442.0` — so the rendered `path:line` string matches the Go renderer's
`int`-formatted output byte-for-byte. The `{{if .line}}` truthy check
naturally suppresses the `:line` suffix when `line` is zero or missing,
mirroring the Go renderer's `{{if gt .Line 0}}:{{.Line}}{{end}}`
behaviour. `TestCompileHtmxRenderer_AffectedFilesLineless` locks both
arms of that behaviour.

## Verification methodology

### Visual fidelity

`TestAffectedFilesParity` in `widgets_htmx_test.go` is the regression
bar. It feeds the same payload through `renderAffectedFiles` (Go) and
through the htmx-compiled template, then asserts:

- both outputs carry the `<div class="ii-w-files">` wrapper and the
  `<ul class="ii-w-files-list">` shell
- both outputs render two `<li class="ii-w-file-row">` rows
- both outputs wrap the path (with optional `:line` suffix) in
  `<code class="ii-w-file-path">…</code>`
- both outputs wrap the reason in
  `<span class="ii-w-file-reason">…</span>`
- auto-escaping is identical (the test asserts `<introduced>` in
  `reason` escapes to `&lt;introduced&gt;` in both paths)

`TestCompileHtmxRenderer_AffectedFiles`,
`TestCompileHtmxRenderer_AffectedFilesEmpty`, and
`TestCompileHtmxRenderer_AffectedFilesLineless` round out the
htmx-only happy-path / empty-state / lineless coverage.

### Parity gap — Open-in-IDE button

The Go renderer wraps each row with a
`<button class="ii-action-btn">Open in IDE</button>` element built from
`OpenIDEAction(path, line)`. That helper serializes an
`ActionDescriptor` into JSON for `data-ii-action`; it lives server-side
in `incident_intel_actions.go` and has no template-only equivalent
(sprig `fnRegistry` does not expose it).

The htmx template body therefore omits the button.
`TestAffectedFilesParity` locks the gap in place — Go path emits one
`ii-action-btn` per file (two here), htmx path emits zero. Two paths
to close the gap if a future port needs it:

1. **Pre-render server-side**: the widget-instance producer renders the
   button HTML once per file and ships it as an `open_ide_button_html`
   field; the template emits `{{.open_ide_button_html | safehtml}}`
   (requires a trusted-html sanitizer registration).
2. **Lift the helper into the template scope**: register
   `OpenIDEAction` as a template func at compile time
   (research §3.3 — deferred until a second port asks for it).

Neither is required for affected-files — the file-list chrome carries
enough information for the soak; the Open-in-IDE affordance remains a
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
(`templates/partials/widgets_phase4.html` / `widget_affected_files`)
modulo the field-access shape (`.Path` vs `.path`, `gt .Line 0` vs
`if .line`) and the button parity gap above. That means:

- `<div class="ii-w-files">` semantic chrome preserved
- `<ul class="ii-w-files-list">` list semantics preserved
  (screen readers announce list-of-N affected files)
- `<code>` wrapping for the file path preserved
- reason text rendered as a sibling `<span>` rather than nested in the
  code element, matching the Go template's structure

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/affected-files-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: affected-files-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/affected-files-htmx.json
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
   `widget_type: affected-files-htmx` parented to a real rich-report
   bead. Visually verify against the matching `affected-files`
   instance on the same report.
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows missing the Open-in-IDE button
4. **Stage B (gated on ka-0000004464 / ka-0000003243)**: once the prod
   helm rollback lands, render at least one `affected-files-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788 with
   the prod confirmation.
