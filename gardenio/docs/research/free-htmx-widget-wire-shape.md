> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx widget — wire shape & template contract

**Status:** research plan (Phase 1, free-htmx widget epic)
**Bead:** ka-0000000776
**Parent epic:** ka-0000006879
**Gardener:** ka-0000003094
**Gardener rich-report:** ka-0000006906
**Sibling research:** ka-0000000873 (sandbox/CSP), ka-0000007136 (backend contract), ka-0000007457 (migration), ka-0000002102 (operator UX), ka-0000003004 (authoring loop)
**Prior art:** ka-0000001930 (widget system synthesis), ka-0000006543 (rendering substrate)

## Goal

Define what a `type=widget-type` bead with `renderer_kind=htmx` carries on the
wire, and what the template-author contract is, so a free-htmx widget can be
authored, validated, rendered, and versioned without an MR for each new
widget.

The companion sandbox/CSP/sanitizer policy is **out of scope here** and lives
in ka-0000000873. This document assumes the sanitizer + CSP plumbing exists
and only references it where the contract has to hand off to it.

## Recommendation in one paragraph

A free-htmx widget-type bead carries its renderer as a single Go
`html/template` body in a typed `template_source` field, the input contract
as a JSON Schema in the existing `schema` field, and a renderer-kind
discriminator (`renderer_kind=htmx` vs the existing `builtin`). Validation
runs in two places by design: client-side in `gb widget emit` for fast
authoring feedback (already implemented for `builtin`, no change needed) and
server-side in the kbeads pre-write hook so a malformed payload cannot land
even if the agent skips the CLI. Versioning stays exactly as it is today —
monotonic `widget_version` per `slug`, instances pin both — and template
hot-reload is simply re-editing the type bead for non-contract changes; a
schema or contract change is a new widget-type bead at the next version. No
per-instance template overrides; instances carry only payload.

The free-htmx widget is **not** a new bead type. It reuses `widget-type` and
`widget-instance`. The free-form-ness comes from `renderer_kind=htmx`: the
beads-viewer dispatcher first tries the in-process Go-coded `widgetRegistry`
(today's `builtin` renderers), then — when the type bead declares
`renderer_kind=htmx` — falls back to compiling and executing the
`template_source` against the sanitizer + CSP policy from ka-0000000873.

## What changes vs. today's substrate

The widget substrate that already shipped (ka-0000001930 → Phase 4–8) has:

- `type=widget-type` bead with fields `{slug, widget_version, summary,
  schema}` (kbeads `internal/server/config.go` lines 317–325).
- `type=widget-instance` bead with fields `{widget_type, widget_version,
  position, headline, data}` (lines 331–340).
- Hard-coded Go `widgetRegistry` keyed on slug → `widgetRenderer` func in
  `gardenio/controller/cmd/beads-viewer/widgets_phase4.go`. New widgets ship
  as a Go file plus an `init()` registration; new widget = MR.
- Live-polling allowlist for four widgets in `widgets_live.go` —
  htmx `hx-get` against `/widgets/{slug}/{bead}` returns the rendered
  fragment; URL path is the canonical authority.
- `gb widget emit` (`gardenio/controller/cmd/gb/widget.go`) does client-side
  JSON Schema validation of `required` + scalar types + permissive
  `additionalProperties` warning.

Three additive changes turn this into the free-htmx contract:

1. New typed fields on `type=widget-type`: `renderer_kind` and
   `template_source` (and reserved `template_engine`, `render_endpoint`).
   Existing widgets carry `renderer_kind=builtin` (defaulted on read for
   backward compat); free-htmx widgets carry `renderer_kind=htmx` and a
   `template_source`.
2. New dispatcher branch in `widgets_phase4.go::lookupWidget` (or a thin
   wrapper) — when no Go-coded renderer is registered for the slug, look up
   the active widget-type bead by slug+version, read its `renderer_kind`,
   and if `htmx`, compile its `template_source` (cached) and run it through
   the sanitizer.
3. Server-side payload validation in the kbeads pre-write hook for
   `widget-instance` writes.

## Wire shape

### Widget-type bead (`type=widget-type`)

| Field             | Type          | Required | Purpose                                                                |
| ----------------- | ------------- | -------- | ---------------------------------------------------------------------- |
| `slug`            | string        | yes      | Stable handle (URL-safe). Registry key. Existing.                      |
| `widget_version`  | string (int)  | yes      | Monotonic per `slug`. Existing.                                        |
| `summary`         | string        | no       | One-liner for `gb widget list`. Existing.                              |
| `schema`          | json          | yes (new for htmx) | JSON Schema describing the instance payload. Existing field; existing widgets often left empty — required for `renderer_kind=htmx`. |
| `renderer_kind`   | enum          | yes (new) | `builtin` \| `htmx`. Defaults to `builtin` on read for back-compat.   |
| `template_source` | string (long) | conditional (new) | Required when `renderer_kind=htmx`. Empty / absent for `builtin`.  |
| `template_engine` | enum          | no (new) | Reserved. Currently always `go-html-template-v1`. Carried so v2 (e.g. with sprig, with partials) can ship without a wire break. Defaults to `go-html-template-v1`. |
| `render_endpoint` | string        | no (new) | Optional override for the live-poll route. Defaults to `/widgets/{slug}/{bead-id}` (the existing convention from `widgets_live.go`). Out-of-process renderers are out of scope; this is a future hook. |

The template body lives **in a typed field, not the description**. Reasons:

- The bead description is markdown-rendered on the bead page; embedding a
  raw `<div hx-get=…>` template in a fenced ```html block forces every
  reader through that fence and makes diffs noisy when the prose around the
  fence shifts. A typed field is opaque to the description renderer.
- `kd update -f template_source=@file.html` is a clean MR-diffable surface;
  the bead page can render the template in a labelled "Template source" pane
  with chroma highlighting. Reviewers see exactly the bytes the renderer
  will compile.
- The description stays free for prose: intent, examples, sample payloads,
  CSP exceptions requested, etc. The doc and the code don't fight.
- A bundle attachment is overkill for a single file and worse for diffs
  (bundle revisions are opaque to MR review).

The description still carries the JSON Schema in a fenced ```json block as
human-readable docs, but the authoritative copy is the typed `schema` field
(same as today). Drift between the two is a lint warning the gardener
catches, not a hard error — humans read the description, the renderer reads
the field.

### Widget-instance bead (`type=widget-instance`)

**No change.** The instance fields stay exactly as kbeads already carries
them: `{widget_type, widget_version, position, headline, data}`. Free-htmx
instances differ from `builtin` instances only in which renderer the
beads-viewer dispatcher picks; the wire shape is identical, which is exactly
what we want.

Specifically:

- `widget_type` = the slug, e.g. `reporter-info-htmx`.
- `widget_version` = pinned version. The dispatcher uses
  `(widget_type, widget_version)` to resolve the type bead.
- `position` = sort order on the rich-report.
- `headline` = optional override above the rendered card.
- `data` = the JSON payload, validated against the type bead's `schema`.

The parent-child mention `parent(ka-rich-report-id)` in the description
remains the placement primitive; `rich_report.go::buildRichReport` already
walks parent-child reverse references and dispatches by `widget_type`.

### Concrete example — porting `reporter-info` to free-htmx

Today's stable `reporter-info` widget is a Go renderer registered in
`widgets_phase4.go`. The free-htmx port lives entirely on a new widget-type
bead.

**Type bead (sketch):**

```sh
kd create "Widget: reporter-info-htmx v1" \
  --type=widget-type \
  --kind=data \
  --label=widget-stage:draft \
  -f slug=reporter-info-htmx \
  -f widget_version=1 \
  -f renderer_kind=htmx \
  -f template_engine=go-html-template-v1 \
  -f schema='{"type":"object","required":["name"],"properties":{
        "name":{"type":"string","maxLength":120},
        "email":{"type":"string","format":"email"},
        "team":{"type":"string"},
        "is_internal":{"type":"boolean"},
        "bead_link":{"type":"string","pattern":"^kd-[A-Za-z0-9]+$"}
     }}' \
  -f template_source=@reporter-info-htmx.tmpl \
  -d "Free-htmx port of the Phase-4 reporter-info widget."
```

**Template source (`reporter-info-htmx.tmpl`):**

```gotemplate
{{- /* reporter-info-htmx v1 — payload schema declared on the type bead */ -}}
<section class="w-reporter-info" data-w-slug="reporter-info-htmx" data-w-version="1">
  <header class="w-reporter-info__header">
    <h3 class="w-reporter-info__name">{{ .name }}</h3>
    {{ if .is_internal }}<span class="w-reporter-info__chip w-reporter-info__chip--internal">internal</span>{{ end }}
  </header>
  <dl class="w-reporter-info__details">
    {{ if .email }}
    <dt>Email</dt><dd><a href="mailto:{{ .email }}">{{ .email }}</a></dd>
    {{ end }}
    {{ if .team }}
    <dt>Team</dt>
    <dd hx-get="/htmx/lookup/team/{{ .team }}" hx-trigger="load" hx-swap="innerHTML">
      {{ .team }}
    </dd>
    {{ end }}
    {{ if .bead_link }}
    <dt>Bead</dt><dd><a href="/bead/{{ .bead_link }}">{{ .bead_link }}</a></dd>
    {{ end }}
  </dl>
</section>
```

**Instance emission** is unchanged from today:

```sh
gb widget emit reporter-info-htmx \
  --report ka-RichReportFsm123 \
  --version 1 \
  --data '{"name":"Paulo","email":"paulo@example.com","team":"infra","is_internal":true,"bead_link":"ka-FSM123"}'
```

The dispatcher in `rich_report.go::buildRichReport` looks up `widget_type =
reporter-info-htmx`, finds no Go-coded renderer in `widgetRegistry`, then
resolves the type bead, sees `renderer_kind=htmx`, compiles
`template_source`, validates the payload against `schema`, runs the template,
sanitizes the output, and emits the fragment.

## Template language choice

### Recommendation: Go `html/template` (`template_engine = "go-html-template-v1"`)

Go `html/template` wins on every dimension that matters for v1.

| Dimension                   | Go html/template                                  | Mustache (e.g. mustache.go)          | Raw HTML (no engine)                |
| --------------------------- | ------------------------------------------------- | ------------------------------------ | ----------------------------------- |
| Contextual auto-escape      | yes — different escape rules in `<script>`, `<a href>`, `<style>` etc. | logic-less; HTML-escapes everything  | none — author must escape          |
| htmx attribute round-trip   | yes (`hx-get="…"`, `hx-vals='{"x":{{.x}}}'` work natively) | works but interpolating into `hx-vals` JSON requires care | works                              |
| Author familiarity          | already used by every other beads-viewer template (`templates/partials/widgets_phase4.html`, `bead.html`, etc.); same file extensions, same `{{ … }}` syntax | foreign to this codebase             | foreign to this codebase            |
| Extra deps                  | none — already imported in 30+ beads-viewer files | new dep + sandbox surface            | none                                |
| Sandbox cost                | already understood; bluemonday + CSP plumbing in ka-0000000873 applies the same way regardless | bluemonday still required; mustache adds another parse path | bluemonday still required; trivially XSS-prone |
| Failure mode on author bug  | fails compile-time, error rendered as placeholder | mostly silent (missing key → empty)  | silent on missing data              |
| Partials                    | `{{ template "name" . }}` works; partial sourcing is the unsolved bit (see Open question below) | `{{> partial }}` works               | none                                |

Mustache's logic-less philosophy is appealing for "agents can't shoot
themselves in the foot," but the agent failure mode is rarely "wrote
complex logic in a template" — it's "forgot to escape an attribute." Go
`html/template` actively prevents that; mustache prevents it only as a side
effect of being too primitive to express the bug.

Raw HTML is the worst of all worlds: no escape, no logic, no partials, no
familiarity. Reject.

We pin the engine name as `go-html-template-v1` from day one so a future
upgrade — adding sprig, allowing partial registration, changing the
auto-escape policy — can ship as `go-html-template-v2` without breaking the
v1 widgets.

### Helper set (allowlisted FuncMap)

The compiled template runs against a **strictly allowlisted** FuncMap. v1
includes only:

- Existing helpers already used in beads-viewer templates that are
  side-effect-free and don't touch the daemon (`liveAttrs` wrappers excluded
  — htmx attrs are written by hand in templates).
- A small set of safe formatters: `lower`, `upper`, `truncate`, `formatTime`
  (RFC3339 → human), `pluralize`, `relTime`, `formatBytes`.
- `kdLink` — given a `kd-…` id from the payload, returns the canonical
  bead-page link (matches the existing description renderer's behavior).

We deliberately do **not** ship sprig in v1. Sprig is a 200+ function
catalog, much of it irrelevant or actively dangerous (`env`, `expandenv`,
`getHostByName`); whitelisting a subset is more work than starting from
zero. v2 can revisit.

The renderer rejects templates that reference unknown functions at compile
time — a typo in `{{ .name | uppe }}` fails the type-bead's compile pass
loudly, never at render time.

## Validation: where it runs

Three layers, deepest is authoritative:

| Layer                              | Where                                       | What it catches                                       | Authoritative? |
| ---------------------------------- | ------------------------------------------- | ----------------------------------------------------- | -------------- |
| 1. CLI client (`gb widget emit`)   | `gardenio/controller/cmd/gb/widget.go::validateAgainstSchema` | Missing required fields, scalar type mismatches, unknown properties (warning) | no — already exists, already advisory |
| 2. kbeads pre-write hook (NEW)     | kbeads `internal/server` write path for `type=widget-instance` | Full JSON-Schema validation against the type bead's `schema` field | **yes** — payload cannot land if it fails |
| 3. Render time                     | `rich_report.go::buildRichReport` + free-htmx dispatcher | Template execution errors, sanitizer rejections (CSP / hx-* policy) | yes for *rendering*; the bead persists, the render degrades to a placeholder |

The motivation for adding layer 2 (a pre-write hook) is exactly what makes
free-htmx different from `builtin`: there is no Go reviewer between an
agent's `gb widget emit` and the rendered widget. With `builtin`, the
renderer source is reviewed in an MR — a malformed payload at most causes a
runtime placeholder. With `htmx`, the schema **is** the contract, and the
schema's only enforcer was the CLI. A gardener that bypasses the CLI and
calls the daemon directly today can land a payload that the renderer can't
make sense of; we want that to fail at write time, not at the rich-report
view.

The pre-write hook is small:

1. On any `widget-instance` create / update, look up the active
   `(widget_type, widget_version)` widget-type bead.
2. If `schema` is empty, accept (legacy compat — most existing widgets
   don't ship schemas).
3. If `schema` is set, run the payload through a real JSON Schema validator
   (we standardize on `github.com/santhosh-tekuri/jsonschema/v5` — already
   well-known in Go ecosystem, draft 2020-12 support).
4. Reject the write with a structured error citing the schema path on
   failure. `gb widget emit` already prints these errors in human-readable
   form.

Out-of-band agents (a future Python tool, a curl-and-jq script, a different
host calling kbeads directly) all benefit. Layer 1 stays — it's faster and
better UX — but we no longer rely on it for safety.

We do **not** validate at render time again. Once the daemon enforces
schema-on-write, the renderer can trust the payload's shape; what it
*cannot* trust (and what render-time validation handles via the sanitizer
in ka-0000000873) is HTML output safety.

## Versioning + backwards-compat

We adopt the existing widget versioning, no new mechanism.

### Existing rules

- `widget_version` is monotonic per `slug`; instances pin both
  `widget_type` and `widget_version`.
- The renderer registry keys on `(slug, widget_version)`. Multiple
  versions ship concurrently.
- Successor type beads carry `blocks: ka-old-version-id` typed mention so
  the dependency graph shows v2 supersedes v1.
- `widget-stage:deprecated` keeps a renderer alive for historical instances
  but hides the type from agent-side discovery (`gb widget list` filters
  it out).

### What's new for free-htmx

**Template hot-reload via type-bead edit.** The beads-viewer caches compiled
templates keyed on `(widget_type_bead_id, updated_at)`; on cache miss, it
recompiles. An operator who wants to fix a typo in a v1 template:

- For a true bugfix that doesn't change the schema or the visible
  contract: edit `template_source` in place. The cache invalidates on next
  render and old instances pick up the fix immediately. This is the
  "hot-reload" path.
- For a behavior change (new field, removed field, layout change visible to
  consumers): file a v2 type bead; instances continue to pin v1 until the
  gardener emits new instances at v2. Old rich-reports keep rendering v1.

We deliberately do **not** treat every template edit as a breaking
change. The widget-version bump is for *contract* changes (schema,
documented behavior, layout consumers depend on); template polishing is
not. This mirrors how we treat Go renderer code: we ship typo fixes
without bumping `widget_version`.

### Schema evolution

JSON Schema field changes follow the standard rules:

- Adding an optional field (not in `required`, not enforced via
  `additionalProperties: false`) is non-breaking. No version bump.
- Adding a required field, removing a field consumers send, or
  tightening a type is breaking. New `widget_version`.

The v1 pre-write hook treats `additionalProperties` as advisory by default
(matches the CLI's permissive mode); a widget-type bead may opt into strict
mode by setting `"additionalProperties": false` in its schema.

### No per-instance template overrides

A `widget-instance` does **not** get to carry its own `template_source` to
override the type bead. Two reasons:

1. The instance becomes uninspectable as a class — review tooling can't say
   "what does an `rca-panel-htmx` instance look like" without crawling
   every instance.
2. It blows up the operator-promotion lifecycle. Today an operator can
   promote a draft widget by reviewing one type bead. Per-instance
   overrides mean reviewing N instances.

If a gardener wants two visually-distinct flavors of a widget, the right
move is two type beads (`rca-panel-compact`, `rca-panel-detailed`) with
different slugs.

## Component reuse / partials

**v1: no cross-widget partials.** A free-htmx template is a single
self-contained body. `{{ template "w/foo/header" . }}` referencing another
widget-type bead is rejected at compile time.

Reasons:

- The dependency graph between widgets becomes a render-time concern (what
  if the partial is at a different version than the caller?), and we want
  v1's only graph to be `(slug, version)` → renderer.
- Partials sourced from other type beads break the schema contract: the
  caller can't validate that the partial's expectations match its payload.
- Single-file widgets are easier to review during operator promotion.

A widget *can* define internal partials with `{{ define "section" }}…{{ end
}}` and then `{{ template "section" . }}` within the same body — that's
purely intra-template structure, no cross-widget edges.

We revisit cross-widget partials when there's a concrete demand: probably
shipped as a new bead type `widget-partial` (`type=widget-partial`,
versioned, allowlisted-from-templates) so partials get the same lifecycle
discipline as widgets. Tracked as a v2 follow-up.

## Open questions / followups for synthesis (ka-0000003094 Phase 2)

These are deliberately deferred to the synthesis decision the gardener will
file:

- **Partial registration** — when (not if) we add cross-widget partials, is
  the partial body owned by a `widget-partial` bead, or do we let widgets
  declare named partials and import them? Probably `widget-partial`.
- **Per-instance variants without new types** — should an instance be able
  to pass a `variant: "compact" | "detailed"` field that the template
  reads? Cheap; just a payload field. Probably yes; document the
  convention in the catalog rather than baking it into the wire.
- **Template build-time vs. render-time compile** — v1 compiles on first
  render and caches. Should we compile every type bead at controller
  startup so a malformed template is caught even with zero instances?
  Probably yes (cheap, a smoke-test signal); deferred to implementation.
- **Helper-set evolution** — when we want to add a helper, is it
  `template_engine: go-html-template-v2` (forces type-bead bumps) or do we
  sneak it in under v1 and trust nobody named a payload field
  `formatBytes`? Lean toward v2 bumps for contract clarity; revisit if v1
  feels too rigid in practice.
- **Render-endpoint override (`render_endpoint` field)** — reserved on the
  type bead for future use (out-of-process renderers, lambda-shaped
  renderers, separate template service). v1 ignores it; the dispatcher
  always renders in-process.

## Linkbacks for synthesis

- **Sandbox + CSP policy** — ka-0000000873 owns the sanitizer (`bluemonday`
  policy), the `hx-*` allowlist, host allowlist for `hx-get/hx-post`
  targets, CSP origin allowlist, class-name namespacing, `<script>` ban.
  This research assumes that policy exists and exposes a single
  `func SanitizeWidgetHTML(html string) (string, error)` entry point.
- **Backend contract for htmx** — ka-0000007136 owns the documented
  `/htmx/...` route catalog (auth, rate limits, fragment vs JSON+OOB).
  This research assumes `/widgets/{slug}/{bead}` (existing) plus the
  policy bead's allowlist scopes which other routes a widget may target.
- **Migration plan** — ka-0000007457 picks one Phase-4 widget to port
  end-to-end; the worked example here (`reporter-info-htmx`) is a sketch,
  not a commitment.
- **Operator UX** — ka-0000002102 owns `gb widget scaffold-htmx`,
  `gb widget preview`, draft-render UX, lifecycle stages.
- **Authoring loop** — ka-0000003004 owns the end-to-end agent workflow
  (search → scaffold → edit → validate → emit → review → promote).

## TL;DR

- Free-htmx widget = `type=widget-type` bead with new fields
  `renderer_kind=htmx`, `template_source` (Go html/template body),
  `template_engine=go-html-template-v1`. Existing `slug`, `widget_version`,
  `summary`, `schema` carry over unchanged.
- Free-htmx instance = unchanged `type=widget-instance` shape. Dispatcher
  picks the renderer based on the type bead's `renderer_kind`.
- Template lives in a typed field, not the description, not a bundle.
- Template language: Go `html/template` (`go-html-template-v1`) with a
  small allowlisted FuncMap. No sprig in v1.
- Validation is layered: CLI advisory, kbeads pre-write hook authoritative,
  renderer trusts shape and only catches HTML-safety issues.
- Versioning: monotonic `widget_version` per `slug` (existing). Template
  edits that don't change schema or contract don't bump version; renderer
  cache keys on `(type_bead_id, updated_at)`.
- No cross-widget partials and no per-instance template overrides in v1.
- The trust boundary, sanitizer, and `hx-*` policy live in ka-0000000873;
  this research hands off cleanly.
