# Free-htmx widget — Phase-4 port prototype: `activity-log` migration plan

> Research deliverable for **ka-0000007457**.
> Parent epic: **ka-0000006879** (free-form htmx widget research).
> Sibling research closed:
> - ka-0000000776 — wire shape + template contract
> - ka-0000000873 — sandbox + CSP + sanitizer policy
> - ka-0000007136 — backend contract for `/htmx/*`
> - ka-0000002102 — dogfooding + operator UX
>
> Author: seer `gardenio-research-migration-plan-m2z`, 2026-05-09.

## TL;DR

- Port **`activity-log`** (ka-0000000776 called this one out as the right pick;
  reaffirmed here) end-to-end to a free-htmx widget under a new slug
  `activity-log-htmx`. The two coexist as parallel registry entries; the Go
  renderer stays the operator-promoted "fast path" until the htmx port has
  one full cycle in production.
- The wire shape is a **shape-preserving rename**: the existing payload
  `{entries: [{when, actor, kind, text}]}` carries forward unchanged. No
  schema field renames, no nested-shape changes. This is the cheapest
  possible port and is the value of doing it first.
- The renderer becomes a Go `html/template` body shipped on a
  `widget-type` bead at `template_source`, structured as the same `<ol>`
  the existing `widget_activity_log` template emits. **No new client-side
  JS** — the existing page-level `ii-relative-time` enhancer already
  picks up the `<time>` element by class. Time formatting works for
  free.
- The free-htmx port surfaces three concrete gaps and one open follow-up:
  a server-side **template helper for `data-*` attributes** (sanitizer
  rejects `data-kind` today because the ka-0000000873 policy only
  allowlists `data-widget-key|value|bead-id`); a **JSON Schema** for the
  payload (Phase-4 widgets ship none); and a **render-time call budget**
  guard for the dispatcher to make sure a free-htmx render that loops
  over a giant `entries` array can't starve the rich-report.
- **Backend endpoint changes are zero for v1.** `activity-log` is a pure
  read of payload fields — no `/htmx/api/list`, no
  `/htmx/bead/{id}/panel/...`, no `/htmx/action/...`. The port works with
  only the dispatcher branch + sanitizer wiring already scoped by
  ka-0000007136 §1.2 and ka-0000000873 §3. Live polling is also out of
  scope for v1 (`live_interval_secs=0`); when we add it, it's the same
  `/htmx/bead/{id}/panel/{name}/live` route the backend doc already
  reserves.
- **`gb widget` extension surface for the port is the minimum viable
  subset of ka-0000002102:** `gb widget scaffold-htmx`, `gb widget
  validate`, `gb widget emit` (existing). `gb widget preview` is
  recommended-but-not-required. `gb widget publish` / `gb widget
  deprecate` are the v2 lifecycle commands; the port can ride on bare
  `kd update` label flips for Phase-1 because there is exactly one
  operator and one widget-type bead.
- **Deprecation strategy: parallel registry, port-then-soak, deprecate.**
  Ship `activity-log-htmx` alongside the existing `activity-log`. Swap
  the gardener's `gb widget emit` calls to `activity-log-htmx`. Run for
  ≥ one release cycle (4 days minimum, two scheduled-gardener ticks). If
  parity holds (defined below), **the Go `activity-log` renderer enters
  `widget-stage:deprecated` but is not removed**; existing instances
  keep rendering. Removal is a separate decision after free-htmx has
  ridden ≥ 4 widgets to maturity.
- **The catalog migration story for the other 14+ widgets is not "port
  them all in order."** Sequence by demonstrated need: port a widget
  when an agent asks to *change* it (rather than burning a port budget
  upfront). The single exception is the next port in this epic, which
  the synthesis bead (ka-0000003004) should pick deliberately to
  exercise the gaps this prototype surfaces — recommended:
  `reproduction-steps` (markdown handling) **or** `related-tickets`
  (reverse-dep walking via `/htmx/api/bead/{id}` from ka-0000007136).

The recommendation in one paragraph:

> The port is the cheapest test of the entire substrate. Activity-log is
> structural — payload-driven `<ol>` with no markdown, no fetches, no
> live polling — so a port that renders bit-identically against the
> sample payload pins the wire-shape, sanitizer, and dispatcher
> contracts at one stroke. Ship it as a parallel registry entry, exercise
> it via the gardener's existing dogfood beads (`gb widget emit
> activity-log-htmx --report ka-0000003788`), and let the gaps it
> surfaces drive Phase 2 synthesis (ka-0000003004) — not the other way
> round. We do not deprecate the Go renderer until Phase 4 has a second
> port (markdown or reverse-dep) and a real cross-page diagnostics tab.

This doc closes ka-0000007457 and feeds the synthesis bead
(ka-0000003004). It is not a code MR — that lands under Phase 3 on a
separate slice — but it does pin the contract enough that the
implementation MR is mechanical.

---

## 1. Target widget — why `activity-log`

### 1.1 The candidates

The Phase-4 catalog has 11 widgets; Phase 5–7 add another 5. The
realistic candidates for *first port* are the ones that:

1. Ship today (have a registered Go renderer + an HTML sub-template).
2. Have a stable, narrow payload shape (a port shouldn't double as a
   schema redesign).
3. Don't depend on Phase-8 live polling or Phase-9 mezmo splice (those
   are independent moving parts).
4. Render structurally — i.e., the rendered HTML is what an agent
   would actually want to author by hand.

That matrix narrows to four:

| Slug                | Payload shape                                            | Markdown? | Live? | Dep walk? | Notes                                                       |
| ------------------- | -------------------------------------------------------- | --------- | ----- | --------- | ----------------------------------------------------------- |
| `reporter-info`     | `{name, email, team, is_internal, bead_link}`            | no        | no    | no        | The wire-shape doc's worked example; smaller than activity-log. |
| `activity-log`      | `{entries: [{when, actor, kind, text}]}`                 | no (text) | no    | no        | The bead-description recommendation; iterates a list.        |
| `reproduction-steps`| `{steps: [{text, bead_ref}]}`                            | yes       | no    | yes (preview hovers) | Backend-contract doc's worked example.                       |
| `related-tickets`   | `{rows: [{title, status, ticket_id, …}]}`                | no        | no    | yes       | Iterates rows, links back to beads.                          |

The bead description picks `activity-log`. I considered overriding to
`reporter-info` (smaller, no list iteration) and to `reproduction-steps`
(exercises more of the contract), and re-arrived at **`activity-log`**:

- **`reporter-info` is too small.** Five scalar fields, no iteration.
  It exercises payload field access but not a Go template `{{ range }}`
  block. The first port should stress-test the most common authoring
  pattern, which is "iterate a payload list and emit a card per
  element." `reporter-info` doesn't.
- **`reproduction-steps` is too large for a *first* port.** It pulls in
  goldmark markdown rendering, which means the renderer's `template.HTML`
  output now depends on the markdown sanitizer pass *plus* the
  free-htmx sanitizer pass. Two sanitizers in series have known
  ordering footguns (markdown emits raw HTML; bluemonday strips it; some
  classes of bug only appear in this composition). Worth porting, but
  not first.
- **`related-tickets` is the *second* port candidate**, not the first.
  It exercises reverse-dep walking against `/htmx/api/bead/{id}`, which
  is the new backend contract surface — but that means the first port
  is bottlenecked on the backend MR landing. Activity-log doesn't need
  any new backend route.
- **`activity-log` exercises the right things at the right scale**:
  payload field access (4 scalar fields per entry), `{{ range }}`
  iteration, time formatting via the existing `<time
  class="ii-relative-time">` enhancer, `data-*` attribute use (which
  uncovers the data-attribute sanitizer gap — see §6), conditional
  rendering of the empty state, and the `ii-pill` chip pattern that
  every Phase-4 widget shares. Five overlapping muscles, one widget.

The recommendation lands here. The synthesis bead may pick a different
order; the contract this doc pins is independent of the choice.

### 1.2 What `activity-log` does today

Code: `gardenio/controller/cmd/beads-viewer/widgets_phase4.go:567-595`
(W19). Template:
`gardenio/controller/cmd/beads-viewer/templates/partials/widgets_phase4.html:171-188`.

The Go renderer:

1. Reads the JSON payload via `decodePayload`.
2. Pulls `entries` (a `[]map[string]any`).
3. For each entry, extracts `when` (RFC3339 string), `actor`, `kind`
   (lowercased), `text`. All four are scalar strings.
4. Calls `renderWidgetTemplate(ctx, "widget_activity_log", v)` against
   a struct `{Entries []activityLogEntry}`.

The HTML template:

```html
{{define "widget_activity_log"}}
<div class="ii-w-activity">
  {{if .Entries}}
    <ol class="ii-w-activity-list">
      {{range .Entries}}
        <li class="ii-w-activity-row" {{if .Kind}}data-kind="{{.Kind}}"{{end}}>
          {{if .WhenISO}}<time class="ii-relative-time" datetime="{{.WhenISO}}">{{.WhenISO}}</time>{{end}}
          {{if .Actor}}<span class="ii-w-activity-actor">{{.Actor}}</span>{{end}}
          {{if .Kind}}<span class="ii-pill" data-state="{{.Kind}}">{{.Kind}}</span>{{end}}
          {{if .Text}}<span class="ii-w-activity-text">{{.Text}}</span>{{end}}
        </li>
      {{end}}
    </ol>
  {{else}}
    <p class="empty">No activity recorded.</p>
  {{end}}
</div>
{{end}}
```

The page chrome above this fragment is the existing
`<article class="widget-card">` wrapper from `rich_report.go`; the
template fragment slots into `card.RenderedHTML`. The
`ii-relative-time` class is enhanced into "3 minutes ago" by a
page-level JS file (`/assets/static/relative-time.js`); widgets do not
own that enhancement. The `ii-pill` chip styling is global CSS.

That's the entire surface. Five muscles, ~17 lines of template, four
scalar fields per entry, and the JS enhancer is page-level so a port
that emits the same `<time>` element gets relative-time formatting for
free.

---

## 2. Wire shape mapping — current → free-htmx

### 2.1 The payload is preserved verbatim

The existing payload format:

```json
{
  "entries": [
    {
      "when": "2026-05-09T18:00:00Z",
      "actor": "operator@anthropic.com",
      "kind": "comment",
      "text": "Reviewed the migration plan; agree with the deprecation strategy."
    },
    {
      "when": "2026-05-09T18:05:14Z",
      "actor": "gardener-free-htmx-widget",
      "kind": "transition",
      "text": "Moved ka-0000007457 → in_progress."
    }
  ]
}
```

This is the payload an agent would emit today via `gb widget emit
activity-log --data @payload.json`. The free-htmx port carries this
shape **unchanged**. No field renames, no nested-shape changes, no
case changes. This is the single most important property of the port:

> A gardener that emits `gb widget emit activity-log-htmx
> --data @payload.json` against the same payload it used today gets a
> rendered widget that is bit-identical to the Go renderer's output.

If we changed the payload shape on the free-htmx port (e.g. flattened
to `[{...}, {...}]`, added an envelope `{schema_version: 1, entries:
[...]}`, or renamed fields), the port would conflate two changes and
the synthesis bead couldn't pick the right one. **Shape-preserve.**

### 2.2 The widget-type bead

Following ka-0000000776 §Wire shape, the free-htmx widget-type bead
carries:

```sh
kd create "Widget: activity-log-htmx v1" \
  --type=widget-type \
  --kind=data \
  --label=widget-stage:draft \
  --label=project:gardenio \
  -f slug=activity-log-htmx \
  -f widget_version=1 \
  -f renderer_kind=htmx \
  -f template_engine=go-html-template-v1 \
  -f live_interval_secs=0 \
  -f schema=@activity-log-htmx.schema.json \
  -f sample_payload=@activity-log-htmx.sample.json \
  -f template_source=@activity-log-htmx.tmpl \
  -d @activity-log-htmx.description.md
```

The four file references are payload, schema, sample, and template. All
land on the bead (per ka-0000000776 the template body is a typed field,
not the description; the description carries operator-readable docs).

The new fields vs the existing `widget-type` bead surface (only
`{slug, widget_version, summary, schema}` today):

| Field             | Type   | Required for htmx | Notes                                                                |
| ----------------- | ------ | ------------------ | -------------------------------------------------------------------- |
| `renderer_kind`   | enum   | yes               | `htmx` (vs the default `builtin`).                                   |
| `template_source` | string | yes               | Go `html/template` body. ~30 lines for activity-log-htmx.            |
| `template_engine` | enum   | recommended       | `go-html-template-v1`. Reserves room for v2 (sprig, partials).       |
| `live_interval_secs` | int | recommended       | 0 for activity-log v1; reserved for the Phase-2 live port.           |
| `sample_payload`  | json   | recommended       | Drives `gb widget preview` and the operator's Preview tab.           |

`schema` is required for `renderer_kind=htmx` per ka-0000000776 §Wire
shape; the existing builtin widgets often leave it empty, but a free
widget cannot — it has no Go reviewer between the agent's emit and the
rendered widget, so the JSON Schema is the contract.

### 2.3 The JSON Schema (gap surfaced by the port)

Phase-4 widgets ship without a `schema`. The port has to author one;
it's the first concrete schema-from-source-code translation in the
catalog and pins the tone for future ports.

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "required": ["entries"],
  "properties": {
    "entries": {
      "type": "array",
      "minItems": 0,
      "maxItems": 200,
      "items": {
        "type": "object",
        "required": ["when"],
        "properties": {
          "when":  {"type": "string", "format": "date-time"},
          "actor": {"type": "string", "maxLength": 200},
          "kind":  {"type": "string",
                    "enum": ["comment", "transition", "decision",
                             "deploy", "alert", "claim", "release", "rate"]},
          "text":  {"type": "string", "maxLength": 2000}
        },
        "additionalProperties": false
      }
    }
  },
  "additionalProperties": false
}
```

Three opinionated choices the schema makes that the Go renderer
implicitly tolerated:

1. **`maxItems: 200`** caps the entries list. The Go renderer iterates
   without a cap; a malicious / runaway payload could render 10⁵
   entries. Free-htmx renders are subject to the ka-0000007136 §4.3
   render-time budget (4 s context timeout, 256 KiB fragment cap), but
   declarative caps in the schema fail loudly at write time, which is
   the better signal. The cap is generous — real activity-logs in
   production rarely exceed ~50 entries — and can be raised on a
   per-instance basis only by bumping `widget_version`.
2. **`kind` is enumerated.** The Go renderer accepts arbitrary strings
   and lowercases them. The enum is the union of kinds observed in
   shipping rich-reports, which keeps the `data-kind` CSS hook honest:
   you can write `[data-kind="alert"]` rules confident no surprise
   value will leak through. New kinds bump `widget_version`.
3. **`additionalProperties: false`.** ka-0000000776 §Validation calls
   this advisory by default; this widget opts in to strict.
   Activity-log payloads are short and the contract is small enough
   that strictness is free; later widgets with larger payloads (e.g.
   `service-health` with optional Mezmo overrides) may want
   advisory.

The Go renderer continues to accept its broader payload (no schema, no
caps); deprecation is a separate question (§7).

### 2.4 The widget-instance bead

Unchanged from today. `widget_type=activity-log-htmx`,
`widget_version=1`, `position=N`, `headline=...` (optional),
`data=<JSON>`. The dispatcher in `rich_report.go::buildRichReport`
picks the renderer by `widget_type`; the instance bead does not carry
`renderer_kind` (the type bead does).

---

## 3. Renderer — HTML template + CSP-safe JS approach

### 3.1 The template body

```gotemplate
{{- /* activity-log-htmx v1
       payload: {entries: [{when, actor, kind, text}]}
       schema:  defined on the type bead
       parent:  rich-report bead (.Bead)                              */ -}}
<section class="w-activity" id="widget-{{ .Widget.ID }}-body">
  {{- if .Payload.entries }}
    <ol class="w-activity-list">
      {{- range $entry := .Payload.entries }}
        <li class="w-activity-row"
            {{ if $entry.kind }}{{ widgetData "kind" $entry.kind }}{{ end }}>
          {{- if $entry.when }}
            <time class="w-relative-time"
                  datetime="{{ $entry.when }}">{{ $entry.when }}</time>
          {{- end }}
          {{- if $entry.actor }}
            <span class="w-activity-actor">{{ $entry.actor }}</span>
          {{- end }}
          {{- if $entry.kind }}
            <span class="w-pill"
                  {{ widgetData "state" $entry.kind }}>{{ $entry.kind }}</span>
          {{- end }}
          {{- if $entry.text }}
            <span class="w-activity-text">{{ $entry.text }}</span>
          {{- end }}
        </li>
      {{- end }}
    </ol>
  {{- else }}
    <p class="w-empty">No activity recorded.</p>
  {{- end }}
</section>
```

Five intentional differences from the existing template:

1. **Class names are `w-`-prefixed**, not `ii-w-`. The ka-0000000873
   sandbox enforces `^w-[…]$` on `class=` values; `ii-w-activity`
   would be stripped. The catalog migration brings beads-viewer CSS
   along (see §3.4).
2. **`id="widget-{{.Widget.ID}}-body"`** matches the
   ka-0000007136 §3.2 stable-id convention so OOB swaps and live
   polling can target this fragment by id without colliding with
   other widgets.
3. **`data-kind` and `data-state` are emitted via `widgetData`
   helper.** The existing template writes `data-kind="{{.Kind}}"`
   directly. The ka-0000000873 policy only allowlists
   `data-widget-key|value|bead-id`; everything else is stripped.
   Rather than expand the allowlist for every widget that wants a
   custom data-attribute, `widgetData` (a server-side template
   helper, see §6) emits a sanitizer-friendly `data-w-kind` /
   `data-w-state` attribute and the catalog CSS is migrated to read
   the `data-w-*` namespace. **This is the largest concrete gap the
   port surfaces** — see §6.
4. **`<time class="w-relative-time">` (not `ii-relative-time`)**.
   The page-level JS enhancer that turns ISO timestamps into "3
   minutes ago" is matched by class. The catalog migration ships
   the enhancer keyed on `w-relative-time`; the existing
   `ii-relative-time` enhancer keeps working for the Go widgets
   until they're deprecated. **Two-class double registration** for
   the parallel-registry period; one class once the Go renderer is
   removed.
5. **Empty state renders a `<p class="w-empty">` instead of `<p
   class="empty">`.** Same CSS rename rationale; less interesting.

### 3.2 Template scope

The template runs against the ka-0000007136 §5.1 pre-injected scope:

| Variable          | What activity-log-htmx uses                               |
| ----------------- | --------------------------------------------------------- |
| `.Bead`           | not used (would be the rich-report bead)                  |
| `.Parent`         | not used                                                  |
| `.Siblings`       | not used                                                  |
| `.Widget`         | uses `.Widget.ID` for the stable target id                |
| `.Payload`        | the only data source — `.Payload.entries[*]`              |
| `.Viewer`         | not used                                                  |
| `.URLs`           | not used (no `hx-*` round-trips in this widget)           |
| `widgetData`      | helper — see §6 (gap)                                     |

The widget is intentionally **read-only**: no `hx-get`, no `hx-post`,
no `hx-trigger`. This keeps the port's attack surface flat and lets us
exercise the *baseline* render path before we add round-trips.

### 3.3 CSP-safe JS approach — there isn't any

`activity-log-htmx` ships **zero widget-authored JavaScript**. The
only "JS" involved is the page-level relative-time enhancer that
matches `class="w-relative-time"`. That enhancer is shipped as part of
beads-viewer's static asset pipeline (`/assets/static/relative-time.js`)
and loaded once per page; widgets do not author it, do not register
it, and cannot disable it. The enhancer is invoked by class name on
DOM-ready and on htmx `htmx:afterSettle` (so swap-ins also get
enhanced).

This satisfies the ka-0000000873 §3 strict CSP (`script-src 'self'`)
without `'unsafe-inline'`: the widget renders pure HTML, the script
that touches the DOM is same-origin, and there is no inline event
handler anywhere. The template **never** authors `onclick=`,
`hx-on:*`, or `<script>` — the sanitizer would strip them anyway, but
the conscious choice not to author them is what keeps the port a
"baseline" port.

When a future widget needs round-trips (e.g. when activity-log gains
"refresh now" or "load more" controls), it does so via `hx-*`
attributes pointing at `/htmx/bead/{id}/panel/activity[/live]` (a
route reserved by ka-0000007136 §1.1 but not yet implemented). That's
a Phase 2 port, not Phase 1.

### 3.4 CSS migration — class-name renames

The existing widget uses `ii-w-activity`, `ii-w-activity-list`,
`ii-w-activity-row`, `ii-w-activity-actor`, `ii-w-activity-text`,
`ii-pill`, `ii-relative-time`, `empty`. The port uses
`w-activity`, `w-activity-list`, `w-activity-row`,
`w-activity-actor`, `w-activity-text`, `w-pill`, `w-relative-time`,
`w-empty`.

The catalog migration ships **both classes on each rule** during the
parallel-registry period:

```css
.ii-w-activity, .w-activity { ... }
.ii-w-activity-list, .w-activity-list { ... }
/* etc. */
```

When the Go renderer is deprecated and removed (§7 step 5), the
`ii-w-*` aliases are dropped in the same MR. This is a search-and-
replace change in the existing
`gardenio/controller/cmd/beads-viewer/static/incident-intel.css`
(or wherever the widget styles live; `rg ii-w-activity static/` will
find them).

### 3.5 Visual fidelity test plan

The port is "done" when:

1. A side-by-side render of `activity-log` (Go) and
   `activity-log-htmx` (free-htmx) on the same payload produces:
   - Identical DOM tree at the `<ol>` level (modulo class name
     renames, modulo `id="widget-…-body"` on the new wrapper).
   - Identical `<time datetime>` values (so the enhancer produces
     identical "X minutes ago" strings).
   - Identical text content per entry.
2. A Playwright screenshot of each on the same headless Chromium
   produces images that pass an `--ssim 0.99` similarity check (one
   exception: the `data-w-kind` rename means CSS hooks differ; the
   migrated CSS asserts both selectors apply during the parallel
   period so visual output is still identical).
3. The ka-0000000873 sanitizer corpus tests all pass against the
   rendered output. Specifically: `data-w-kind` and `data-w-state`
   both pass the sanitizer (proving the helper works), and the
   sanitizer rejects a "raw" `data-kind` if a future template author
   bypasses the helper.

The screenshots and test fixtures live in
`gardenio/test/fixtures/free-htmx-activity-log/` (Phase-3
implementation MR; not part of this research).

---

## 4. Backend endpoint changes

### 4.1 v1: zero new routes

`activity-log-htmx` is a **pure read of payload fields**. It does not
fetch any kbeads data, does not poll, does not post actions. The
endpoints catalogued by ka-0000007136 are not exercised by the port's
template body.

This is a deliberate property of choosing this widget first: the port
isolates the *substrate change* (renderer dispatcher branch +
sanitizer + template helper) from the *backend contract change* (new
`/htmx/*` routes). When the implementation MR for this port lands,
the test surface is small and doesn't require ka-0000007136's punch
list to land first.

### 4.2 v2 and beyond: live refresh

When activity-log gains live refresh (Phase-3 follow-up), it picks up:

- `/htmx/bead/{id}/panel/activity/live` (ka-0000007136 §1.1) — the
  rich-report's bead id, panel name `activity`. The handler reads
  the current widget-instance's `data` field, applies the cross-
  filter if any, and re-renders the fragment. Same template, same
  scope, same sanitizer.
- `live_interval_secs=30` on the type bead (the ka-0000007136 §6.4
  default).
- `liveAttrs` template helper invocation:
  ```
  <section ... {{ liveAttrs .Widget }}>
  ```
- Cache headers: `Cache-Control: no-store` (ka-0000007136 §9).

That widget-version bumps the type bead to v2; v1 instances continue
rendering the static fragment. **Not in scope for this port.**

### 4.3 Endpoints exercised by *future* activity-log evolution

For completeness, the catalog of routes activity-log might grow into:

| Route                                          | Use case                                                                             | When                                         |
| ---------------------------------------------- | ------------------------------------------------------------------------------------ | -------------------------------------------- |
| `/htmx/bead/{id}/panel/activity/live`          | Live refresh of the activity stream.                                                 | v2 (post-port soak).                         |
| `/htmx/api/list?type=task&parent=…`            | If the activity-log starts pulling event data from beads-viewer rather than payload. | v3 (paradigm shift; not recommended).        |
| `/htmx/action/expand-activity-row`             | A "show details" button per row.                                                     | Stretch; only if a real consumer asks.       |
| `/htmx/bead/{id}/panel/activity/csv`           | Export to CSV download.                                                              | Out of scope; non-htmx endpoint.             |

Nothing in this catalog is required for v1.

### 4.4 What the dispatcher branch needs (v1 backend change)

The single backend change required for the port is the dispatcher
branch in `widgets_phase4.go::lookupWidget` (or, more cleanly, a new
`widgets_htmx.go` that wraps it). Pseudocode:

```go
func lookupWidget(slug string, daemon BeadFetcher) (widgetRenderer, bool) {
    if r, ok := widgetRegistry[slug]; ok {
        return r, true
    }
    // Free-htmx fallback: resolve the active widget-type bead by slug.
    typeBead, err := daemon.LookupActiveWidgetType(slug)
    if err != nil || typeBead == nil {
        return nil, false
    }
    if typeBead.Fields["renderer_kind"] != "htmx" {
        return nil, false
    }
    return compileHtmxRenderer(typeBead), true
}
```

`compileHtmxRenderer` reads `template_source`, parses it, runs it
against the template scope (ka-0000007136 §5.1), feeds the output
through the sanitizer (ka-0000000873 §2), and returns the
`widgetRenderer` func. Compiled templates cache on `(typeBead.ID,
typeBead.UpdatedAt)` so a type-bead edit invalidates immediately
(ka-0000000776 §Versioning).

This is not "the implementation MR" — it's the punch list of
substrate changes the port relies on. Implementation lives in Phase-3
slices owned by separate crews.

---

## 5. `gb widget` CLI changes (emit / register flow)

### 5.1 The minimum viable subset for the port

ka-0000002102 specs the full operator UX surface (`scaffold-htmx`,
`lint`, `preview`, `validate`, `publish`, `deprecate`). The port
**does not** need the full surface to land. It needs:

| Verb                       | Role for the port                                                              | Required?           |
| -------------------------- | ------------------------------------------------------------------------------ | ------------------- |
| `gb widget emit`           | Existing — emits a `widget-instance` bead. Slug + report + payload.            | yes (already ships) |
| `gb widget scaffold-htmx`  | Prints a starter `widget-type` bead body for `activity-log-htmx`.              | strongly recommended (drives the dogfood loop) |
| `gb widget validate`       | Checks the new type-bead against the schema and sanitizer before publish.      | strongly recommended |
| `gb widget preview`        | Renders the type-bead against `sample_payload` for fast author iteration.      | optional v1; required by Phase 2 |
| `gb widget lint`           | Static checks on the template body.                                            | optional            |
| `gb widget publish`        | Operator label-flip `draft → published`.                                       | not required for v1; bare `kd update` works for one widget |
| `gb widget deprecate`      | Operator label-flip `published → deprecated`.                                  | not required for v1 |

The port is single-widget, single-operator, single-gardener, and the
gardener (ka-0000003094) already runs scheduled. So:

- **Required**: `gb widget emit` (already ships) + a way to create
  the `widget-type` bead. Bare `kd create --type=widget-type
  -f renderer_kind=htmx -f template_source=@... ` works without any
  CLI extension. The agent can author the type bead with the
  existing `kd` surface.
- **Recommended**: `gb widget scaffold-htmx` so the agent doesn't
  hand-author the type-bead body. Same author workflow the operator
  UX research describes; just the *first verb* of that surface.
- **Recommended**: `gb widget validate` so the agent can sanity-check
  the type bead before declaring it ready. Implementation: schema
  parses + template parses + sample payload validates + sample
  render succeeds + sanitizer no-ops. Five checks; all read-only.

The remaining verbs (`preview`, `lint`, `publish`, `deprecate`) are
deferred to a later port — the second-port crew (probably
reproduction-steps or related-tickets) builds them out as their
authoring loop demands.

### 5.2 Existing `gb widget emit` works unchanged

The bead description for ka-0000007457 asks "gb CLI changes (gb widget
emit / register flow)". The answer for v1 is **no changes**: the existing
emit verb (`gardenio/controller/cmd/gb/widget.go::widgetEmitCmd`,
lines 56-225) writes a `widget-instance` bead with
`{widget_type, widget_version, position, headline, data}` fields and
a `parent(<report>)` typed mention in the description. That's the
exact wire shape ka-0000000776 §Wire shape calls for free-htmx
instances — they reuse the builtin shape exactly.

```sh
# This already works today (no CLI change):
gb widget emit activity-log-htmx \
  --report ka-0000003788 \
  --version 1 \
  --data '{"entries":[
    {"when":"2026-05-09T19:00:00Z","actor":"seer","kind":"comment",
     "text":"Migration plan landed."}
  ]}'
```

The dispatcher in `rich_report.go::buildRichReport` looks up the slug,
finds no Go-coded renderer in `widgetRegistry`, falls through to the
free-htmx fallback in `lookupWidget` (§4.4), compiles the type-bead
template, validates the payload, runs the template, sanitizes the
output, and emits the fragment.

**The whole point of free-htmx is that emit doesn't change.** A future
agent who wants to ship a widget never has to touch
`gb widget emit`'s code; they only author a new type bead. The port
proves this by *not* changing emit.

### 5.3 The future "register" flow

The bead description also says "register flow." The free-htmx
register flow is:

1. `gb widget scaffold-htmx <slug>` — print starter type-bead body.
2. Agent edits.
3. `kd create --type=widget-type ...` (or
   `gb widget scaffold-htmx <slug> --commit` per ka-0000002102).
4. `gb widget validate <id>` — go/no-go.
5. `gb widget publish <id>` — operator flip
   (`widget-stage:draft → widget-stage:published`).

For the port: steps 1-4 are recommended; step 5 is bare `kd update`
or done implicitly by the gardener marking the bead as published in
its dispatch prompt. The Phase 2 synthesis bead picks the first port
that actually exercises the publish gate; for now the gardener's
reviewer-of-record is enough.

### 5.4 What the port adds to `gb widget show`

When `renderer_kind=htmx`, `gb widget show` should print the
**template body** in a fenced code block. This is a ka-0000002102
recommendation (§3.2, "show" verb extensions); the port is a good
forcing function for it. Two extra lines of code in
`gardenio/controller/cmd/gb/widget.go::widgetShowCmd`:

```text
+ if rk := picked.Fields["renderer_kind"]; rk == "htmx" {
+     fmt.Println("Template:")
+     fmt.Println("```html")
+     fmt.Println(picked.Fields["template_source"])
+     fmt.Println("```")
+ }
```

Useful for the operator's manual review pass; cheap to implement.
Not strictly required for the port to ship.

---

## 6. Gaps the port surfaces

### 6.1 `data-*` attribute helper

**The biggest concrete gap.** ka-0000000873 §2 allowlists exactly
three `data-*` names: `data-widget-key`, `data-widget-value`,
`data-bead-id`. The activity-log template needs `data-kind` and
`data-state`; the related-tickets template will want `data-status`
and `data-priority`; the service-health template will want
`data-health`. Every Phase-4 widget uses at least one custom
`data-*` attribute as a CSS hook.

Three options for the synthesis bead to pick from:

#### Option A: Server-side `widgetData` helper that emits `data-w-*` (recommended)

Author writes:
```html
<li class="w-activity-row" {{ widgetData "kind" $entry.kind }}>
```
Helper emits:
```html
<li class="w-activity-row" data-w-kind="comment">
```
Sanitizer allowlist regex: `^data-w-[a-z][a-z0-9-]{0,40}$`. CSS rules
read `[data-w-kind="…"]`. The "w-" namespace mirrors the class
namespace decision in ka-0000000873 and is the cheapest single rule
the sanitizer needs.

This makes the port write `data-w-kind` and the catalog migration
adds `[data-w-kind]` selectors alongside `[data-kind]` until the Go
renderer is removed (§7).

**Cost**: one regex change in the sanitizer policy + one helper in
the template FuncMap + ~1 CSS file MR. ~50 LOC across three files.

#### Option B: Per-widget allowlist registration

Each `widget-type` bead declares its own `data-*` attributes via a
new `data_attrs` field; the sanitizer reads the field on dispatch
and applies an instance-scoped policy. More flexible, more complex,
new wire-shape field.

**Cost**: new wire-shape field (ka-0000000776 amendment) + dynamic
sanitizer policy compilation per type-bead + cache invalidation. ~150
LOC; harder to review.

Recommend rejecting Option B. Per-type allowlists make the sanitizer
policy a function of the widget catalog, which is the wrong direction
— the sanitizer is the trust boundary and should be uniform.

#### Option C: Re-use existing `data-widget-value`

Encode `kind=comment` as `data-widget-value="kind:comment"`; CSS
selectors use attribute-substring matching: `[data-widget-value*=":alert"]`.

**Cost**: zero sanitizer changes; ugly CSS hooks; doesn't compose
with multiple data attributes per element.

Recommend rejecting Option C as a hack.

**Recommendation: Option A.** The synthesis bead picks; for the port
to ship, *some* answer is required. Option A is cheapest and
forwards-compatible.

### 6.2 Schema authoring is non-trivial

The port has to author a JSON Schema. Phase-4 widgets ship none. The
schema in §2.3 took ~30 minutes to write from the renderer source;
across 14 widgets that's a real day. **Recommended Phase-2 follow-up**:
a `gb widget schema-from-renderer <slug>` verb that reads the Go
renderer (literally, by AST), inspects the
`widgetString/widgetObjectSlice/widgetStringSlice` calls, and emits a
draft JSON Schema. Not a port blocker; a catalog-migration
accelerator.

### 6.3 Render-time call budget

The ka-0000007136 §4.3 budget is a 4 s context timeout per request
+ 256 KiB fragment cap. For activity-log-htmx these are vastly
generous (1000 entries render in ~50 ms; 256 KiB caps at ~5000
entries). But the budget enforcement code is **not yet implemented**
in beads-viewer (verified by ripgrep: no `time.NewTimer` /
`context.WithTimeout` wraps the widget render path in
`rich_report.go`). The port does not need this implemented to ship,
but the Phase-3 implementation MR should land it before
`renderer_kind=htmx` is real, because it's the only thing keeping a
runaway template from starving the rich-report.

### 6.4 Live-poll cadence allowlist enforcement

The ka-0000007136 §6.4 cadence allowlist (`{0, 5, 30, 300}`s) is a
field on the type bead. **Currently not enforced** in
`widgets_live.go::liveWidgetPolicies` (those are static per-slug
policies hard-coded in Go). For activity-log-htmx v1
(`live_interval_secs=0`), this doesn't matter. For v2 it does.
Phase-3 MR.

### 6.5 Open follow-up: the dispatcher cache

`compileHtmxRenderer` (§4.4) caches compiled templates on
`(typeBeadID, updatedAt)`. The cache is a `map[string]*template.Template`
behind a `sync.RWMutex`. **Open**: invalidation on bead delete.
Today the rich-report dispatcher doesn't subscribe to bead-deletion
events; if a type bead is deleted, the cache holds the compiled
template indefinitely. Probably fine (cached compiles aren't a
correctness risk — the type-bead is gone so no instances will
dispatch to it), but worth a comment in the implementation MR.

---

## 7. Deprecation strategy — parallel registry, port-then-soak

### 7.1 The decision: parallel registry, NOT port-then-deprecate

Two strategies were on the table:

#### Strategy A — Parallel registry (recommended)

Register `activity-log` (Go) and `activity-log-htmx` (free-htmx) as
**different slugs**. The free-htmx port lives at a new slug; the Go
renderer continues to handle existing instances. Gardeners emit the
new slug going forward. After a soak period, the Go renderer is
deprecated (label flip; renderer kept registered) and eventually
removed (Go file deleted).

| Property                                    | Strategy A                                                                     |
| ------------------------------------------- | ------------------------------------------------------------------------------ |
| Backwards compatibility for old instances   | Perfect. Existing `widget_type=activity-log` instances render unchanged.       |
| Operator review surface                     | One new type-bead (`activity-log-htmx`) — tiny diff; easy to grasp.            |
| A/B fidelity testing                        | Trivial: emit the same payload at both slugs on a side-by-side rich-report.    |
| Rollback                                    | Stop emitting the new slug; the world reverts.                                 |
| Gardener dispatch change                     | One-line: `--type=activity-log` → `--type=activity-log-htmx`.                  |
| Eventual removal                            | Two-step: deprecate → soak → remove.                                           |
| Cost                                        | Two registry entries during the soak; minor.                                   |

#### Strategy B — Port-then-deprecate (rejected)

Keep the slug `activity-log`. Replace the Go renderer with a free-htmx
type bead at the same slug. Existing instances pick up the new
renderer the moment the type bead is published.

| Property                                    | Strategy B                                                                     |
| ------------------------------------------- | ------------------------------------------------------------------------------ |
| Backwards compatibility                     | Tested: the port has to be bit-identical or existing rich-reports change.      |
| Operator review surface                     | Risky: a single type-bead publish flips behaviour for every existing instance. |
| A/B fidelity testing                        | Hard: requires holding a Go branch and flipping at-runtime per request.        |
| Rollback                                    | Re-flip the type-bead — but hot rich-reports may have already cached.          |
| Gardener dispatch change                     | None.                                                                          |
| Eventual removal                            | One-step: just delete the Go file.                                             |
| Cost                                        | Single registry entry; **single point of failure**.                            |

The reason to **reject Strategy B** is the same reason we layer
validation across CLI + pre-write hook + render time
(ka-0000000776 §Validation): we don't want a single artifact (the
type-bead) to flip live production rendering for instances that were
authored under different assumptions. Strategy A keeps the old
instances on the old renderer forever; only new emissions go through
the new path.

It also gives a **measurable A/B test**: the gardener emits *both*
slugs on the same rich-report during the soak. The operator and the
diagnostics tab compare side-by-side. Strategy B has no such
verification window.

**Decision: Strategy A. Parallel registry, port-then-soak,
deprecate.**

### 7.2 The lifecycle steps

```
1. Land the type bead (activity-log-htmx v1)         — gardener authors, operator publishes.
2. Gardener dogfood: emit activity-log-htmx on        — both slugs visible on this rich-report.
   the gardener's rich-report alongside activity-log.
3. Soak (≥ 4 days, ≥ 2 gardener ticks).               — operator + diagnostics tab review.
4. Gardener swap: `--type=activity-log-htmx` for new   — old instances continue on activity-log.
   emissions.
5. Deprecate Go renderer:                             — no-op for existing instances.
   - widget-stage:deprecated label on a synthetic
     widget-type bead for activity-log (one-time
     metadata; the renderer itself is in Go code).
   - registerWidget("activity-log", ...) gets a
     `// deprecated: use activity-log-htmx` comment.
6. Soak (≥ 2 weeks).
7. Remove Go renderer:                                 — remove ~30 LOC.
   - delete renderActivityLog + activityLog struct.
   - delete widget_activity_log template.
   - delete `registerWidget("activity-log", …)`.
   - delete ii-w-activity CSS aliases.
   - existing widget_type=activity-log instances
     fall through to the free-htmx fallback if a
     compat shim type-bead is created (optional);
     otherwise they degrade to the placeholder
     card. Operator decides whether to author the
     shim before removal.
```

Steps 1-4 are this port. Step 5 is a follow-up MR. Steps 6-7 are
deferred until *after* the second port (probably reproduction-steps
or related-tickets) has soaked successfully — i.e., we don't deprecate
*any* Go renderer until at least two free-htmx widgets have ridden a
full release cycle.

### 7.3 The catalog migration sequence (other 14+ widgets)

The catalog at the time of writing:

```text
header-incident-summary  service-health           reporter-info
ai-recommended-actions   affected-files           reproduction-steps
suggested-test-cases     user-reported-issue-header   workaround
related-tickets          activity-log             rca-evidence-cards
source-code-analysis     fix-available-pending-branch   vm-sparklines
event-timeline           +mezmo (live)
```

The migration order is **demand-driven**, not category-driven:

1. **First port (this doc)**: `activity-log` — structural baseline.
2. **Second port (synthesis-driven)**: pick one of:
   - `reproduction-steps` — exercises markdown rendering composition
     with the sanitizer (the most likely place a sanitizer bug
     lives).
   - `related-tickets` — exercises `/htmx/api/bead/{id}` for
     reverse-dep walking; first widget to hit a backend route.
3. **Third+ ports**: only when an agent asks to *change* the widget.
   The port-on-demand triggers:
   - "Add field X to widget Y" — port Y to free-htmx (new field is
     a payload schema diff, not a Go file).
   - "This widget should poll" — port Y, then add live polling.
   - "Two widgets need the same partial" — port both, ship a
     `widget-partial` bead (ka-0000000776 §Component reuse).

The non-trigger: "we should port them all because consistency."
Reject. Burning 14 ports on widgets nobody wants to change is
free-htmx tech debt without a payoff. Each widget is ~30 LOC of Go
and ~20 LOC of HTML; "rewrite in free-htmx" is real work without a
correctness or feature payoff.

The exception is **mezmo widgets** (`vm-sparklines`, `event-timeline`,
the live overlay). These have the highest cost-of-change today (any
change is a Go MR plus an mezmo splice review) and the most ambitious
operator UX (live polling, cross-filter forwarding). They're the
right *third* port. But they're only the third port after we've
proved the substrate handles markdown (port #2) and reverse-dep walks
(port #2 or #2.5).

### 7.4 Rollback

Strategy A's rollback is "stop emitting the new slug." Concretely:

| Failure mode                                 | Rollback                                                                                |
| -------------------------------------------- | --------------------------------------------------------------------------------------- |
| The free-htmx port renders incorrectly.      | Gardener flips back to `--type=activity-log`. Existing free-htmx instances render Lightning the broken output until the type bead is patched. **Mitigation**: type-bead edit is hot-reload (ka-0000000776 §Versioning); fix lands without any new instance emission. |
| A sanitizer bug strips a load-bearing attr.  | Patch sanitizer in beads-viewer; redeploy.                                              |
| The dispatcher cache leaks compiled templates. | Restart beads-viewer; cache is in-memory. (Documented in §6.5.)                       |
| `compileHtmxRenderer` panics.                | Per-render `recover()` (ka-0000002102 §5.4) + render the inline error chip.             |
| The kbeads pre-write hook rejects valid payloads. | Ship a CLI override path (`gb widget emit --skip-validate`) for emergency authoring. **Recommended now**: don't add the override; rely on patching the schema. |

The deprecation steps (5-7) are also reversible: undeprecate by
removing the label; un-remove by restoring the deleted Go file from
git. Step 7 (Go-renderer removal) is the only step that adds friction
to rollback (recovering from a wholesale removal requires reverting a
commit), and it doesn't run for ≥ 2 weeks after step 5.

---

## 8. Open questions for synthesis (ka-0000003004)

This doc takes positions, but the synthesis bead is the operator's
final pick. The forks worth flagging:

1. **Class-name namespace: `w-` or `ii-w-`?** §3.4 picks `w-`
   matching ka-0000000873 §2. If the synthesis prefers continuity
   with existing Phase-4 styles (`ii-w-…` is everywhere), the
   sanitizer policy can allow either prefix at the cost of a
   slightly more permissive regex.
2. **Schema strictness: `additionalProperties: false` on
   activity-log-htmx, or default advisory?** §2.3 picks strict.
   Defensible alternative: advisory by default, opt-in to strict
   per-widget.
3. **`data-*` helper: Option A, B, or C?** §6.1 picks Option A
   (server-side helper emitting `data-w-*`). The synthesis bead
   should formally land this; until it does, the port is blocked
   on a sanitizer policy change.
4. **`widget_version` default behaviour for free-htmx widgets.** Today
   builtins default to v1 implicitly when the field is unset; for
   free-htmx the type bead should require an explicit
   `widget_version`. Defer to ka-0000000776 synthesis.
5. **Can the type bead's `template_source` field be empty?** §2.2
   says no for `renderer_kind=htmx`. Should the kbeads pre-write
   hook reject? Recommend yes; defer to ka-0000000776.
6. **Bare `kd update` vs `gb widget publish` for v1.** §5.1 says bare
   `kd update` works for one widget. Defensible alternative: build
   `gb widget publish` first, gate on it, set the precedent. I lean
   toward bare for the *first* port; the second port builds the
   verb.
7. **Soak length.** §7.2 picks ≥ 4 days for step 3 and ≥ 2 weeks
   for step 6. Defensible alternative: tie soak to release cycles
   (≥ 1 release for step 3; ≥ 2 releases for step 6). Recommend
   release-cycle-based; gardenio ships ~daily so 1-2 release cycles
   is days, not weeks.
8. **CSS migration coupling.** §3.4 ships the `w-` aliases in the
   *same MR* as the renderer dispatcher. Defensible alternative:
   ship the CSS first as a no-op preparatory MR, then the renderer.
   Recommend coupled — the CSS without the renderer is dead code,
   and the renderer without the CSS regresses visuals.

---

## 9. Implementation budget (rough)

The port itself, given the substrate (dispatcher branch, sanitizer,
helpers, validate verb) is in place. Pre-implementation guesses;
Phase-3 owners refine.

| Slice                                                       | Eng-days |
| ----------------------------------------------------------- | -------- |
| Author the `activity-log-htmx` widget-type bead             | 0.25     |
| Sanitizer policy: `data-w-*` allowlist + `widgetData` helper | 0.5      |
| `widget_activity_log` parity test + sample fixture          | 0.25     |
| CSS migration (`w-activity*` aliases)                       | 0.5      |
| Gardener dispatch swap (`--type=activity-log-htmx`)          | 0.1      |
| `gb widget show` template-body printing                     | 0.25     |
| Soak diagnostics surface (light: kbeads comment per render error) | 0.5      |
| Docs + changelog                                            | 0.25     |
| **Total** (post-substrate)                                  | **~2.6 eng-days** |

The substrate (dispatcher branch + sanitizer wiring + template
helpers + `gb widget validate`) is the heavy lift. That's the
~11.5 eng-days from ka-0000007136 §12 + the ~15.5 from
ka-0000002102 §11. The port itself is small; that's the point.

---

## 10. Linkbacks for synthesis

- **Wire shape (ka-0000000776)** — the type-bead field set
  (`renderer_kind`, `template_source`, `template_engine`,
  `live_interval_secs`, `sample_payload`) is consumed verbatim. The
  port does not propose any wire-shape additions.
- **Sandbox + CSP (ka-0000000873)** — the sanitizer policy is
  consumed verbatim except for §6.1 (data-attribute namespace
  amendment); the port treats this as the synthesis bead's open
  fork, not a sibling research re-open.
- **Backend contract (ka-0000007136)** — v1 of the port exercises
  zero `/htmx/*` routes. v2+ uses
  `/htmx/bead/{id}/panel/{name}/live` exactly as the doc reserves.
  Helpers `liveAttrs` / `panelURL` etc. are referenced but not
  redefined.
- **Operator UX (ka-0000002102)** — the port consumes the minimum
  viable subset (§5.1). The full lifecycle (`publish` /
  `deprecate` / Preview tab) is recommended to land alongside or
  before the *second* port, not the first.
- **Gardener authoring loop (ka-0000003004, synthesis)** — this doc
  is the synthesis bead's input on "first port end-to-end."

---

## 11. Closing dogfood checklist

Per ka-0000006879 dogfooding mandate and the closing-checklist in
ka-0000002102 §7.3, this doc's closing comment includes:

```
Research delivered:
  Doc: gardenio/docs/research/free-htmx-widget-port-prototype.md
  MR:  https://gitlab.com/.../merge_requests/NNNN  (this MR)
Dogfood:
  Pre-port rich-report: ka-0000003788
    - markdown-narrative widget summarising the port plan
    - activity-log widget summarising the migration steps
    - related-tickets widget linking sibling research beads
  Post-port instance: deferred to Phase 3 (wire shape ships first)
Synthesis: ka-0000003004
```

Specifically: this MR emits a `markdown-narrative` widget instance on
ka-0000003788 summarising the plan (the bead-description deliverable
for ka-0000007457). The `activity-log` and `related-tickets` widgets
on the same rich-report follow the same pattern the closed siblings
(ka-0000000873, ka-0000007136, ka-0000002102) used — see those beads'
rich-reports as templates.

End of doc.
