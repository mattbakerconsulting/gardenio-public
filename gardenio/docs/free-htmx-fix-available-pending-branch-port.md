> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx port — fix-available-pending-branch → fix-available-pending-branch-htmx (Phase 4)

Bead: ka-0000005506 · Migration backlog: ka-0000005157 ·
Reference ports:
ka-0000004584 ([activity-log port doc](free-htmx-activity-log-port.md)),
ka-0000005540 ([related-tickets port doc](free-htmx-related-tickets-port.md)),
ka-0000005304 ([header-incident-summary port doc](free-htmx-header-incident-summary-port.md)),
ka-0000004065 ([reporter-info port doc](free-htmx-reporter-info-port.md)),
ka-0000000243 ([event-timeline port doc](free-htmx-event-timeline-port.md))

Sixth pre-defined widget on the free-htmx substrate. Same dispatcher
(`widgets_htmx.go::lookupWidgetForReport`), same Strategy A —
port-then-soak. Notable for being the first port whose wire contract
intentionally does NOT mirror the Go renderer 1:1: the Go path parses a
raw unified diff string server-side, while the htmx template requires
producers to ship pre-structured `files[]` (see "Wire contract" below).

## Strategy A — port-then-soak

The Go renderer (`fix-available-pending-branch`,
`widgets_phase6.go::renderFixAvailablePendingBranch`) and the htmx port
(`fix-available-pending-branch-htmx`) coexist as different slugs.
Existing widget-instance beads keep rendering through the Go path; new
instances opt into the htmx path by writing
`widget_type: "fix-available-pending-branch-htmx"`. No flag-day, no
rewrite-on-read.

The dispatcher consults the builtin registry first, then falls through
to a daemon lookup for an active `type=widget-type` bead with
`renderer_kind=htmx`. A miss in both paths renders the placeholder card.

## Wire contract

```text
{
  "title":        "Fix available",                         // optional
  "branch":       "agent/fix-stale-cookies",
  "base_branch":  "main",
  "commit_sha":   "9c2a3b7e1f4d8c0a5b6e9f2d4c1a3e5b7d9c0a2f",
  "commit_short": "9c2a3b7",                               // pre-truncated
  "author":       "gardenio",
  "ci_status":    "passed",                                // pre-normalized
  "adds":         2,
  "dels":         1,
  "files": [
    {
      "path":     "auth/session.go",
      "language": "go",
      "adds":     2,
      "dels":     1,
      "hunks": [
        {
          "header":  "@@ -42,7 +42,8 @@",
          "context": "func rotateSession",                  // optional
          "lines": [
            {"kind": "ctx", "old_num": 42, "new_num": 42, "text": "\tcookie := r.Cookies()"},
            {"kind": "del", "old_num": 43, "new_num": 0,  "text": "-\tif cookie.Stale() { return cookie }"},
            {"kind": "add", "old_num": 0,  "new_num": 43, "text": "+\tif cookie.Stale() {"},
            {"kind": "add", "old_num": 0,  "new_num": 44, "text": "+\t\treturn nil"}
          ]
        }
      ]
    }
  ]
}
```

`kind` is one of `ctx` / `add` / `del`. `old_num` / `new_num` are
1-indexed line numbers; emit 0 (or omit) for the side that doesn't apply
to that kind. `ci_status` should be one of `passed` / `failed` /
`running` / `pending` — the htmx template emits the value verbatim into
`data-state="..."`, so producers should pre-normalize.

The schema in
`gardenio/controller/cmd/beads-viewer/fixtures/fix-available-pending-branch-htmx.json`
is the authoritative wire contract; pre-write hooks
([`kbeads/internal/server/widget_validation.go`](https://github.com/mattbakerconsulting/gardenio/blob/main/kbeads/internal/server/widget_validation.go))
reject widget-instance writes that do not validate.

## Verification methodology

### Visual fidelity

`TestFixAvailablePendingBranchParity` in `widgets_htmx_test.go` is the
regression bar. Unlike the previous five ports, it feeds two payloads
(one raw-diff for the Go renderer, one structured-files for the htmx
template) describing the same content, then asserts both outputs carry:

- the wrapper (`<div class="ii-w-pending-branch">`)
- header chrome (`<h4 class="ii-w-pending-title">`,
  `<code class="ii-w-pending-branch-name">`, `ii-w-pending-base`,
  `ii-w-pending-author`, `<span class="ii-pill" data-state="passed">CI passed</span>`)
- per-file chrome (`<ol class="ii-w-pending-files">`,
  `<li class="ii-w-diff-file" data-language="go">`,
  `<code class="ii-w-diff-path">foo.go</code>`)
- per-line chrome (`<table class="ii-w-diff-table">`,
  `ii-w-diff-add` / `ii-w-diff-del` / `ii-w-diff-ctx` row classes)
- the 7-char short SHA in the meta block

Both renders produce exactly one `<li class="ii-w-diff-file">` for the
single-file payload.

### Parity gaps

Five intentional gaps are locked in by `TestFixAvailablePendingBranchParity`:

1. **Diff parsing** — the Go renderer parses a raw unified `diff` string
   via `parseUnifiedDiff` and produces `renderedDiffFile` /
   `renderedHunk` / `renderedDiffLine`. The htmx template has no diff
   parser in scope, so it requires producers to ship pre-structured
   `files[]`. The gardener that emits the widget-instance bead is best
   placed to do this — `parseUnifiedDiff` lives in `diff_parser.go` and
   is exported within the package; downstream producers can either call
   it via a helper or generate equivalent structure from upstream
   sources (e.g. GitLab MR diff API).

2. **Syntax highlighting** — the Go renderer runs each diff line
   through chroma via `highlightDiffLine` and emits the resulting
   per-token `<span class="...">` markup as `template.HTML`. The htmx
   template has no chroma in scope, so it emits the line text as plain
   auto-escaped text. The diff CSS classes (row-level
   `ii-w-diff-{add,del,ctx}`, gutter, marker, code cell) still apply —
   only the per-token colour is missing. The test asserts the htmx
   output does NOT contain `<span class="line` chroma markup.

3. **Markdown summary** — the Go path renders `summary_markdown`
   through goldmark via `renderMarkdown`. The htmx template has no
   markdown helper (sprig `fnRegistry` does not expose goldmark). The
   field is dropped on the floor; producers who need a summary should
   pre-render to plain text and ship via a different field (e.g.
   embed in `title` or extend the schema). The test asserts the
   summary body appears in the Go output but NOT in the htmx output.

4. **Action buttons** — the Go renderer emits a
   `<button class="ii-action-btn">Fast-track merge</button>` (built
   from `FastTrackMergeAction`) and an `Open MR` button (built from
   `OpenMRAction`). Both helpers live server-side and have no
   template-scope analogue. The htmx body emits an empty
   `<footer class="ii-w-pending-actions"></footer>` so the surrounding
   layout stays stable; the buttons themselves are absent. The test
   asserts the htmx output emits zero `class="ii-action-btn"`.

5. **Swap-target div** — the Go path emits
   `<div id="ii-merge-result-..." class="ii-w-pending-result">` as the
   placeholder for the fast-track-merge OOB swap response. With no
   merge button to drive the swap, the htmx body omits the
   placeholder. The test asserts the htmx output does NOT contain
   `ii-w-pending-result`.

Three paths to close the parsing/highlighting gaps if a future port
needs them:

1. **Pre-render server-side** — producers (the gardener that emits the
   widget-instance bead) call `parseUnifiedDiff` + `highlightDiffLine`
   themselves and ship structured + highlighted files[]. The
   widget-instance schema would extend each line with an `html` field
   (template.HTML) the template emits raw.
2. **Lift helpers into template scope** — register `parseUnifiedDiff`
   and `highlightDiffLine` as template funcs at compile time. Research
   §3.3 — deferred until a second port asks for it.
3. **Use the Go renderer for diff bodies, htmx wrapper for chrome** —
   composite render. Out of scope for this port.

For the action-button and markdown gaps, the existing route is "ship
two widget-instances on the same parent": one carries the diff +
chrome via `fix-available-pending-branch-htmx`, another carries the
merge action via a future `fix-available-actions-htmx` slug or via
the Go-coded `fix-available-pending-branch` slug.

### Performance

Per research §6.3 the read-time budget is 4 s for the full rich-report
page. Dispatcher round-trip is bounded by `htmxResolveTimeoutSecs = 4`;
the compiled template is cached on `(bead.ID, bead.UpdatedAt)`. First
hit per process pays one `template.Parse`; subsequent renders pay a
read-locked map lookup. Editing the widget-type bead bumps `updated_at`,
which invalidates the cache on the next dispatch (already covered by
`TestCompileHtmxRenderer_CacheInvalidatesOnUpdate`).

The structured-files contract removes a server-side parse + chroma
highlighter from the read path — for large diffs this can be a net
performance win (parsing happens once at producer time, not on every
render). The flip side is that producers carry the cost; for gardeners
that emit a widget-instance once and re-render the rich-report often,
the trade is favourable.

### Accessibility

The htmx template body is byte-equivalent to the Go template
(`templates/partials/widgets_phase6.html` /
`widget_fix_available_pending_branch`) modulo the field-access shape
(`.Files[i]` vs `.Payload.files[i]`) and the parity gaps above. That
means:

- `<table class="ii-w-diff-table">` semantic table preserved
- `<code>` wrapping for paths, branch names, SHAs, hunk headers
- Add/del/ctx semantic row classes preserved (same CSS targets)
- Empty-state copy ("No diff available.") preserved

No new ARIA attributes were added, so the
[`docs/beads-viewer-a11y-baseline.md`](beads-viewer-a11y-baseline.md)
audit holds.

## Authoring the widget-type bead

The seed fixture at
`gardenio/controller/cmd/beads-viewer/fixtures/fix-available-pending-branch-htmx.json`
carries the schema, sample_payload, and template_source. Ship it with:

```sh
kd create 'Free-htmx widget: fix-available-pending-branch-htmx' \
  --type=widget-type \
  -F gardenio/controller/cmd/beads-viewer/fixtures/fix-available-pending-branch-htmx.json
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
   `widget_type: fix-available-pending-branch-htmx` parented to a real
   rich-report bead — the producer must pre-shape the diff via
   `parseUnifiedDiff`-equivalent logic. Visually verify against the
   matching `fix-available-pending-branch` instance on the same report
   (chroma highlighting, summary markdown, merge buttons absent in
   htmx — expected gaps).
3. Soak for one cycle. Watch for:
   - dispatcher timeouts in beads-viewer logs
     (`widget-htmx: list widget-type beads`)
   - cache-staleness reports (operators editing the widget-type bead)
   - css regressions on rows lacking chroma highlighting
4. **Stage B (gated on ka-0000004464)**: once the prod helm rollback
   lands, render at least one `fix-available-pending-branch-htmx`
   widget-instance on a real rich-report bead in prod beads-viewer.
   Update ka-0000005157 and the gardener rich-report ka-0000003788 with
   the prod confirmation.
