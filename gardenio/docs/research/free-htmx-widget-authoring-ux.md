> ## ⚠️ DEPRECATED (2026-08-02)
>
> Widgets and reports are deprecated in favor of **bundles** (operator directive,
> Slack thread ka-0000004862). Do not author new widget-type or widget-instance
> beads from this doc. Publish deliverables with `gb bundle create --kind report`
> + `gb bundle doc`. See advice **ka-0000007697**, tracking **ka-0000007561**.
> Retained for maintaining already-published widgets.

# Free-htmx widget — agent authoring UX, operator preview surface, lifecycle

> Research deliverable for **ka-0000002102**
> Parent epic: **ka-0000006879** (free-form htmx widget research epic)
> Siblings:
> - ka-0000000776 — wire shape + template contract (in progress)
> - ka-0000000873 — sandbox + CSP + sanitizer policy (closed; doc on `feat/ka-0000000873-free-htmx-security-plan`)
> - ka-0000007136 — backend contract (closed; doc on `feat/ka-0000007136-free-htmx-backend-contract`)
> - ka-0000007457 — migration plan (in flight)
> - ka-0000003004 — synthesis (Phase 2)
>
> Author: seer `gardenio-research-dogfooding-operator-onw`, 2026-05-09

## TL;DR

The dogfooding-and-operator-UX surface is three things glued together:

1. **`gb widget` extensions** that turn the agent authoring loop into a
   tight `search → scaffold → edit → validate → preview → emit → publish`
   loop **with no MR step on the renderer itself** (the whole point of
   free-htmx). New verbs: `scaffold-htmx`, `preview`, `validate`, `lint`,
   `publish`, `deprecate`. Existing verbs (`emit`, `list`, `show`,
   `similar`) get small flags; their semantics carry over.
2. **A preview surface in beads-viewer** that lets the operator open any
   `widget-type` bead and see the template render against its
   `sample_payload` (or operator-supplied payload) inline, with a clear
   error panel when the template fails to compile or sanitize. Same
   sanitizer + CSP as production renders — preview is bit-identical with
   how the widget will look on a rich-report bead.
3. **A 3-stage lifecycle** for `renderer_kind=htmx` types:
   `draft → published → deprecated`. We **drop `proposed`**: without an MR
   gate, "proposed" doesn't mean anything different from `draft` for
   free-htmx widgets. The "I want this published" signal is a `gb decision
   create` against the type bead, not a separate label state. Builtins
   keep their existing 4-stage life unchanged.

The recommendation in one paragraph:

> Free-htmx flips the widget authoring loop from "agent files draft →
> operator approves → engineer lands MR with renderer" to "agent files
> widget-type bead with template inline → CLI compiles + sanitizes +
> previews locally → operator opens the bead in beads-viewer, hits the
> Preview tab, picks Publish/Reject from a decision card." The renderer
> never leaves the bead. **`gb widget` is the agent IDE; beads-viewer is
> the operator's review surface; the lifecycle is a single label flip.**
> Everything else in this doc is mechanism for that loop.

This doc closes ka-0000002102 and feeds the synthesis decision (ka-0000003004).

---

## 1. Goals and non-goals

**In scope:**
- The agent-facing CLI surface (`gb widget *`) needed to author a
  free-htmx widget without wedging the agent on syntax/sanitizer
  surprises late in the loop.
- The operator-facing preview, diagnostics, and promotion UX in
  beads-viewer.
- Lifecycle stages for `renderer_kind=htmx` types: how many, what each
  means, what flips between them, what gates each transition.
- Diagnostic UX when a template fails (compile error, sanitizer
  rejection, schema mismatch, render-time panic).
- The agent-loop end-to-end (worked example).
- Dogfooding mandate from the parent epic: every closing artifact under
  this epic ships as a widget instance on a rich-report bead.

**Out of scope (handled by siblings or follow-ups):**
- Template language itself (Go `html/template` + helper funcs is assumed
  per ka-0000000776 direction). This doc references `panelURL`,
  `actionURL`, `liveAttrs`, etc. but does not redesign them.
- Sandbox/CSP/sanitizer policy details — ka-0000000873.
- Backend `/htmx/*` route surface — ka-0000007136.
- Per-bead migration of any specific Phase-4 widget — ka-0000007457.
- Gardener dispatch / scheduling — ka-0000003004 synthesis.

---

## 2. Lifecycle for `renderer_kind=htmx` types

### 2.1 The recommended 3-stage life

| Stage         | Label                       | Who flips                       | Where it shows up                                                                |
| ------------- | --------------------------- | ------------------------------- | -------------------------------------------------------------------------------- |
| `draft`       | `widget-stage:draft`        | Agent (default on create)       | Renders **only on its own type-bead preview page** and on rich-report beads where it was emitted (with a "DRAFT" chip). Hidden from `gb widget similar` defaults. |
| `published`   | `widget-stage:published`    | Operator (label flip via MR-less path: `gb widget publish` requires `RoleOperator`) | Renders everywhere. Listed by `gb widget similar` / `gb widget list` defaults. |
| `deprecated`  | `widget-stage:deprecated`   | Operator                        | **Existing** instances keep rendering with a "DEPRECATED" chip and an optional `--replaced-by` pointer; **new** `gb widget emit` calls fail unless `--allow-deprecated` is passed. |

**Why drop `proposed`?**

The original 4-stage life (`draft → proposed → stable → deprecated`,
ka-0000001930) was shaped by the builtin renderer reality: `stable`
required a Go MR, so `proposed` existed as a queue between "agent wants
this" and "engineer sat down to write it." Free-htmx erases that queue:
the renderer ships **inside the type bead**, so promotion is a single
operator decision, not a queue handoff. Adding a stage that has no MR
gate and no different rendering behaviour costs operator UX (one more
label to think about, one more filter combo) for no design benefit.

The "I want this widget elevated" signal is now a `gb decision create`
attached to the type bead, with options `[publish, request-changes,
reject]`. The decision bead carries the conversation; the label flip is
the durable outcome.

**Builtins keep the 4-stage life** for backwards compatibility (Phase-4
through Phase-7 types are all `widget-stage:stable` builtins). The
renderer dispatcher reads `renderer_kind` first, then the stage:

- `renderer_kind=builtin`: stages are `{draft, proposed, stable, deprecated}`,
  same as today (ka-0000001930).
- `renderer_kind=htmx`: stages are `{draft, published, deprecated}`.

A widget-type bead can change its renderer kind only via decision review
(see §2.3 — "promotion direction").

### 2.2 What rendering does at each stage

Renderer dispatch table (all in beads-viewer; see ka-0000007136 §1.2):

| Where                                                    | `renderer_kind=builtin`             | `renderer_kind=htmx, draft`             | `renderer_kind=htmx, published`         | `renderer_kind=htmx, deprecated`         |
| -------------------------------------------------------- | ----------------------------------- | --------------------------------------- | --------------------------------------- | ---------------------------------------- |
| Rich-report bead-show with this widget-instance attached | Builtin renderer (existing path)    | Render + "DRAFT" chip                   | Render unchanged                        | Render + "DEPRECATED" chip                |
| `widget-type` bead-show (preview tab)                    | Static schema view                  | Render against `sample_payload`         | Render against `sample_payload`         | Render against `sample_payload`           |
| `gb widget similar` default results                      | Included if `widget-stage:stable`   | **Excluded** (need `--include-draft`)   | Included                                | **Excluded** (need `--include-deprecated`) |
| `gb widget emit` against this slug                       | Allowed if `stable`                 | Allowed (with stderr warning)           | Allowed                                 | **Rejected** unless `--allow-deprecated`  |

The "DRAFT" chip is a small inline pill on the widget header that links
to the type bead and reads "Draft preview — not yet published."
Operators viewing a rich-report still see the widget render so they can
evaluate it; consumers of the rich-report (e.g. an unrelated operator
looking at a triage report) see the chip and know to discount the
widget.

### 2.3 Promotion direction (free-htmx → builtin)

Per the parent epic ("predefined widgets become hardened versions of
free widgets that earned their keep"), the *intended* promotion
direction is the **inverse** of today: free-htmx instances that prove
out get converted to `renderer_kind=builtin` if performance or sharing
demands it. Concretely:

- Default: free-htmx is the "real" path. Most widgets stay there forever.
- Convert to builtin only when **measured**: live-poll cadence + traffic
  demonstrably exceeds the per-route rate-limit headroom (ka-0000007136
  §4.2), OR the widget is needed across so many rich-reports that
  per-widget MR review is cheaper than schema/payload churn.
- The conversion is an MR that lands a Go renderer, then bumps the
  type bead's `renderer_kind` from `htmx` to `builtin`. The template
  body stays in the bead description as the source-of-truth doc; the
  Go renderer is asserted to render bit-identical fragments.
- Reverse direction (builtin → htmx) is not recommended; if a builtin's
  shape is wrong, file a new htmx draft with a fresh slug.

This direction is normative for `gb widget` design: the CLI does **not**
have a `convert-to-builtin` verb. Conversion is a code MR, not a
template operation.

---

## 3. CLI command surface — `gb widget` extensions

### 3.1 New verbs

#### `gb widget scaffold-htmx <slug>`

**Purpose.** Print a starter `widget-type` bead body to stdout (or `--out`
file). Output is a full bead description ready to be fed to `kd create
--type=widget-type` (or, with `--commit`, to `daemon.CreateBead` directly).

**Output sections:**
1. `## Summary` (placeholder).
2. `## Schema` — fenced ```json``` block with a JSON Schema scaffolded
   from `--with-fields`.
3. `## Template` — fenced ```html``` block with a starter Go
   `html/template` body using the URL helpers (ka-0000007136 §5.2)
   and `w-` namespaced classes (ka-0000000873 §2).
4. `## Sample payload` — fenced ```json``` block matching `Schema`.

**Flags:**
- `--summary "<short text>"`
- `--with-fields key:type,key2:type` — scaffolds the JSON Schema
  `properties` + `required` lists. Types: `string|integer|number|boolean|array|object`.
- `--with-live` — emits `<div … {{ liveAttrs .Widget }}>` body so the
  widget polls.
- `--with-action <name>` — emits a button wired to
  `{{ actionURL "<name>" .Bead }}`.
- `--from <existing-slug>` — clone an existing widget-type bead's
  description as the starting point. Pulls schema + template body
  inline; sets a `Forked-From: kd-…` line in the new description.
- `--out <path>` — write to file instead of stdout.
- `--commit` — instead of printing, create the widget-type bead via
  `daemon.CreateBead` and print the new bead id. Sets
  `widget-stage:draft`, `renderer_kind=htmx`, `widget_version=1`,
  `slug=<slug>`. Author defaults to `--actor`.

**Why a print-by-default verb?** Agents read the output, edit it, then
either pipe to `kd create` themselves or re-run with `--commit`. Print-
first matches how agents author other content (e.g. plans, decisions);
it gives the agent full edit control before anything hits the daemon.
`--commit` is for "I trust the scaffold, just file it."

#### `gb widget preview <slug-or-id> [--payload @file.json]`

**Purpose.** Render a widget-type's template against a payload, locally,
with the same sanitizer + sample CSP as production. Two output modes:

- **stdout mode (default).** Prints sanitized HTML to stdout. Agents pipe
  this to `tidy`, `pup`, `prettier`, or `less`. Designed to be
  scriptable.
- **browser mode (`--browser`).** Opens a one-shot beads-viewer URL
  (`/widget-types/{slug}/preview?payload=…&token=…`) in `$BROWSER`, so the
  operator sees the rendered widget against the live CSP + asset
  pipeline. The `token` is a 5-minute HMAC bound to the operator's email
  + the type bead id; reuses the same approach as
  `oidcauth.SignedActionToken`. (Reuses the OIDC session if the agent
  pod has a forwarded one; otherwise prints the URL for the operator to
  open manually.)

**Flags:**
- `--payload @file.json | -` — explicit payload. Defaults to the type
  bead's `sample_payload` field.
- `--bead <id>` — fake the rich-report scope to the named bead so the
  template's `.Bead` / `.Parent` / `.Siblings` resolve. If absent, the
  preview uses **stub fixtures** (a fake `ka-PREVIEW0000` rich-report
  with two child siblings) so templates don't crash on missing scope.
- `--operator` — set `.Viewer.IsOperator=true` (default `false`).
- `--browser` — open in browser; mutually exclusive with `--out`.
- `--out <path>` — write rendered HTML to file. Useful with
  `--no-sanitize` for diff-driven debugging.
- `--no-sanitize` — skip the bluemonday pass. **Strictly diagnostic** —
  prints a warning header. Never used on a rich-report bead.
- `--with-csp` — wraps the output in a minimal HTML page that includes
  the production CSP via `<meta http-equiv="Content-Security-Policy">`,
  so `--browser` mode renders under realistic CSP. Default on for
  `--browser`, off for stdout.

**Exit codes:**
- 0 — render succeeded; sanitizer made no rejections.
- 1 — template compile error.
- 2 — render-time error (e.g. nil-deref in `range`).
- 3 — sanitizer stripped one or more elements/attributes.
- 4 — payload failed schema validation.

The non-zero exit codes are critical for CI/automation: a gardener that
runs `gb widget preview` as a smoke test wants to fail loudly on any
sanitizer rejection, not "just" on compile errors.

#### `gb widget validate <slug-or-id>`

**Purpose.** A bundle of static checks: "is this widget-type bead
well-formed and ready for a publish decision?" Designed to be the gate
the operator's `gb widget publish` runs internally, **and** the gate a
gardener runs in the agent loop before opening the publish decision.

**Checks (each emits a structured finding):**

| Check                            | Severity | Detail                                                                             |
| -------------------------------- | -------- | ---------------------------------------------------------------------------------- |
| Schema parses                    | fatal    | `schema` field is valid JSON Schema (Draft 7 subset; same constraints as `sample_payload` validation today). |
| Template parses                  | fatal    | Go `html/template.New(...).Parse(body)` succeeds.                                  |
| Sample payload validates         | fatal    | `sample_payload` validates against `schema`.                                       |
| Sample render succeeds           | fatal    | Template executes against `sample_payload` + stub scope without runtime error.     |
| Sanitizer no-ops                 | error    | bluemonday + URL allowlist on the rendered output strips zero attributes / elements. |
| URL helpers only                 | error    | All `hx-*` URLs in the rendered output match an allowed pattern (start with `/htmx/...`). |
| Banned attributes absent         | error    | No `hx-on*`, `hx-include`, `hx-headers`, `hx-ext`, `hx-vars`, `style=`, `srcdoc=`, `formaction=` in rendered output. |
| Polling cadence allowed          | error    | `live_interval_secs ∈ {0, 5, 30, 300}` (ka-0000007136 §6.4).                       |
| Class namespacing                | warn     | All `class="…"` values are `w-`-prefixed (ka-0000000873 §2).                       |
| Stable target IDs                | warn     | Every `id=` attribute matches `widget-{instance_id}-…` or `w-…` (ka-0000007136 §3.2). |
| Description has Summary section  | warn     | `## Summary` markdown section present.                                             |
| `widget_version` set             | warn     | `widget_version` field is set (recommended).                                       |

**Output:**
- Default: a tabular list (PASS / WARN / FAIL) with file:line references
  to the offending part of the template body.
- `--json` — machine-readable findings array; same shape as `gb advice
  show` JSON output. Each finding has `check`, `severity`, `message`,
  `template_line` (when relevant), `evidence` (the rendered HTML
  snippet that triggered the finding).
- `--strict` — promotes warnings to errors; exits non-zero on any.

**Why a separate `validate` verb (not just folded into `preview`)?**
Validate is the *ready-for-publish* gate; preview is the *what does it
look like* gate. They share a lot of code internally but they answer
different questions, and the operator wants both. `gb widget publish`
calls `validate --strict` internally and refuses the flip if anything
fails.

#### `gb widget lint <slug-or-id>`

**Purpose.** Best-effort static checks on the **template body** (before
render). Cheaper than `validate` (no payload required) and more
informative for in-loop authoring.

Lint runs:
- Go `html/template.Parse` (compile errors with line numbers).
- Lex pass over the template body looking for banned attributes
  (`hx-on*`, `hx-include`, etc.) and banned URL prefixes
  (literal `https://` outside the image allowlist; `javascript:`,
  `data:`, etc.).
- Check that every `hx-get` / `hx-post` value is either a `{{ panelURL
  ... }}`-style helper invocation OR a literal beginning with `/htmx/`.
- Check that every `hx-target` is `this`, `closest …`, `find …`, or an
  id beginning with `#widget-` / `#w-`.

Lint is permissive on dynamic content (interpolation may produce a path
that lint can't statically resolve); it punts those to `validate` /
`preview`. The agent runs lint repeatedly while editing; runs validate
once before opening the publish decision.

#### `gb widget publish <slug-or-id>`

**Purpose.** Operator-only label flip from `widget-stage:draft` to
`widget-stage:published` (or from `proposed` → `stable` for builtins,
preserving the existing path).

**Behaviour:**
1. Loads the widget-type bead.
2. Refuses if caller lacks `RoleOperator` (uses the existing
   oidcauth-via-CLI gate; same gate `gb advice` write paths use).
3. Runs `gb widget validate --strict` internally; refuses if any check
   fails. `--force` overrides (logged + recorded as audit field on the
   bead).
4. Updates labels: removes `widget-stage:draft`, adds
   `widget-stage:published` (or `widget-stage:stable` for builtins).
5. Files an audit comment on the bead: `published by <operator>` plus
   the `validate` summary.
6. Optionally creates a follow-up bead announcing the new published
   widget (default off; `--announce` to opt in).

**Flags:**
- `--force` — bypass `validate --strict`. Records the override.
- `--note <string>` — operator's rationale, attached as the audit
  comment.
- `--announce` — file an `announcement` bead linking the new published
  widget.

#### `gb widget deprecate <slug-or-id>`

**Purpose.** Operator-only flip to `widget-stage:deprecated`. Existing
instances keep rendering (with a "DEPRECATED" chip) so historical
reports don't break.

**Flags:**
- `--replaced-by <slug>` — sets a `replaced_by` field on the type bead;
  rendering surfaces it as "Use `<replacement-slug>` instead." in the
  chip.
- `--note <string>` — required; deprecation rationale.

`gb widget emit` against a deprecated slug fails by default; the
operator can override per-emit with `--allow-deprecated` (logged).

### 3.2 Existing verb extensions

#### `gb widget emit`

- Add `--dry-run`: render the widget-instance against the rich-report's
  context, print the HTML, do not write the bead. Reuses
  `gb widget preview` internals.
- Reject when the type is `widget-stage:deprecated` unless
  `--allow-deprecated` is passed (with stderr warning logged).
- Add `--draft-ok` — explicit ack that the type is `widget-stage:draft`;
  prints a stderr warning when a draft is emitted onto a non-draft
  rich-report. Default behaviour is to allow drafts (agents need the
  edit loop) but with the warning.

#### `gb widget similar`

- Default-filter to `widget-stage:published`. Existing behaviour was
  "any open widget-type bead"; this changes the default.
- New flag `--include-draft` to widen.
- New flag `--include-deprecated` (implies `--include-draft`).
- Sort order: published first, then drafts, then deprecated. Within a
  group, semantic-search score (existing).

#### `gb widget list`

- Already has `--stage`. Add `--all-stages` as a clearer alias for "no
  stage filter".
- Add `--with-preview` — print a preview URL alongside each bead.
- Output column for `kind`: surface `renderer_kind=htmx|builtin` so the
  operator can audit how many free-htmx widgets are deployed at a
  glance.

#### `gb widget show`

- When `renderer_kind=htmx`, print the **template body** in a fenced
  code block (alongside the existing schema + sample). Truncate at 200
  lines with a "…" tail; full body via `--full`.
- Add a new `Stages` line that summarises the lifecycle metadata
  (current stage, who flipped it, when, audit comment if any).

### 3.3 Help groupings

```text
gb widget
├── Authoring
│   ├── scaffold-htmx     # create a starter type-bead body
│   ├── lint              # static checks on the template
│   ├── preview           # render to stdout / browser
│   └── validate          # full pre-publish gate
├── Lifecycle
│   ├── publish           # operator: draft → published
│   └── deprecate         # operator: published → deprecated
└── Discovery
    ├── emit              # create a widget-instance on a rich-report
    ├── list              # list widget-types
    ├── show              # show one widget-type
    └── similar           # search for widget-types
```

This grouping carries through to the cobra help output (we already
use `GroupID` for `orchestration`; add `authoring`, `lifecycle`,
`discovery`).

### 3.4 Concrete man-page deltas

For copy-paste reference; this is the canonical change list a future
implementer should drop into `gardenio/controller/cmd/gb/widget.go`:

```text
 var widgetCmd = &cobra.Command{
   Use:   "widget",
-  Short: "Manage Incident Intelligence widget types and instances",
+  Short: "Manage widget types and instances (Incident Intelligence + free-htmx)",
   Long: `Create, register, preview, validate, and discover widgets.

 Widgets back the rich-report rendering surface. Two bead types:

   widget-type     — a registered renderer; carries slug + optional
                     widget_version, summary, JSON Schema, sample payload,
                     renderer_kind ('builtin' or 'htmx'), and (for htmx)
                     a Go html/template body in the description.
   widget-instance — a concrete widget on a rich-report page; references
                     a widget-type slug and carries the rendered data
                     payload.

 Authoring:
   scaffold-htmx   Print a starter htmx widget-type bead body
   lint            Static checks on the template body
   preview         Render a widget-type to stdout or in browser
   validate        Full pre-publish gate (schema + render + sanitize)

 Lifecycle:
   publish         Flip widget-stage:draft → widget-stage:published
   deprecate       Flip widget-stage:published → widget-stage:deprecated

 Discovery:
   emit            Create a widget-instance on a rich-report bead
   list            List registered widget-types
   show            Show a widget-type's body, schema, sample, and history
   similar         Find existing widget-types similar to a description

 Use 'gb widget similar' before drafting a new type — reuse beats reinvention.`,
```

The full new-verb signatures (mirrors §3.1):

```
gb widget scaffold-htmx <slug>
    [--summary <s>] [--with-fields k:t,...]
    [--with-live] [--with-action <name>]
    [--from <existing-slug>]
    [--out <path>] [--commit]

gb widget lint <slug-or-id>
    [--full] [--json]

gb widget preview <slug-or-id>
    [--payload @file.json | -]
    [--bead <ka-id>] [--operator]
    [--browser | --out <path>]
    [--with-csp] [--no-sanitize]

gb widget validate <slug-or-id>
    [--strict] [--json]

gb widget publish <slug-or-id>
    [--force] [--note <text>] [--announce]

gb widget deprecate <slug-or-id>
    --replaced-by <slug>
    --note <text>
```

---

## 4. Operator preview surface in beads-viewer

### 4.1 New routes (the punch list)

In dependency order:

1. **`/widget-types/`** *(net-new)* — gallery / index view of all
   widget-type beads. Filter chips: `kind=builtin|htmx`, stage chips
   (`draft`, `published`, `deprecated`, `all`), free-text search on
   slug + summary. Each card links to `/widget-types/{slug}`. Owner
   file: `widget_types_index.go`.
2. **`/widget-types/{slug}`** *(net-new)* — bead-show enhancement, but
   shaped as a typed-bead-show page (similar to how `/decisions/{id}`
   specializes the bead-show). Tabs: **Overview** (summary + schema +
   sample), **Preview** (live render), **Diagnostics** (last 50 errors),
   **History** (versions). For `renderer_kind=htmx`, the Preview tab is
   live; for `builtin`, it falls back to the existing widget-instance
   gallery. Owner file: `widget_type_show.go`.
3. **`/htmx/widget-types/{slug}/preview`** *(net-new)* — the htmx
   fragment endpoint that renders the type's template against a payload
   and returns sanitized HTML. Path lives under `/htmx/*` so the
   sandbox/CSP boundary applies. Drives the Preview tab + the inline
   preview chip on rich-report bead-show pages for draft widgets.
   Backed by the same renderer used for production widget instances —
   bit-identical output. Owner file: `htmx_widget_preview.go` (sibling
   to the routes from ka-0000007136).
4. **`/htmx/widget-types/{slug}/diagnostics`** *(net-new)* — htmx
   fragment endpoint returning the last 50 sanitizer rejections,
   compile errors, and CSP-report-uri violations for this type. Drives
   the Diagnostics tab. Server-side ring buffer keyed by slug; entries
   carry timestamp, viewer email (if logged in), payload digest, error
   message, evidence snippet. **No PII beyond the operator email.**
   Owner file: same.
5. **`/widget-types/{slug}/history`** *(net-new)* — version timeline of
   the type bead (existing in `gb widget show`; surface in viewer too).
6. **`/htmx/widget-types/csp-report`** *(net-new)* — receiver for CSP
   `report-uri` from ka-0000000873 §3. Writes to the per-slug
   diagnostics ring buffer and (rate-limited) files a bug bead.
7. **Rich-report rendering: draft chip** *(edit existing)* — when a
   widget-instance points at a `widget-stage:draft` type, render the
   widget plus a "DRAFT" chip linking to `/widget-types/{slug}`.
   Existing rich-report renderer in `rich_report.go` gets a stage
   lookup. Owner file: `rich_report.go` edit.
8. **Rich-report rendering: deprecated chip** *(edit existing)* —
   "DEPRECATED" chip plus optional "Use `<replacement-slug>` instead"
   pointer. Same owner file.
9. **Decision template: "Publish widget"** *(net-new)* — a beads-viewer
   page link that, from the widget-type bead, opens a pre-filled
   `/decisions/new` form with options `[publish, request-changes,
   reject]` and the widget id pre-bound. Drives the operator-facing
   promotion flow for free-htmx. Owner file: `widget_type_show.go` (a
   button that POSTs to the existing decision-create endpoint).

The v1 scope is items 1-9; nothing else is needed for the operator UX
to feel complete.

### 4.2 The Preview tab in detail

Layout for `/widget-types/{slug}` Preview tab:

```
┌─ Overview · Preview · Diagnostics · History ─────────────────────┐
│ Payload                                                          │
│ ┌──────────────────────────────────────────────────────────────┐ │
│ │ {"severity":"P2","headline":"db lag spike",…}              ▼│ │
│ └──────────────────────────────────────────────────────────────┘ │
│ [Use sample payload] [Reset] [Operator view ☐]                   │
│                                                                  │
│ Render                                                           │
│ ┌──────────────────────────────────────────────────────────────┐ │
│ │ ⚠ DRAFT                                                      │ │
│ │ ┌──────────────────────────────────────────────────────────┐ │ │
│ │ │  …rendered widget HTML…                                  │ │ │
│ │ └──────────────────────────────────────────────────────────┘ │ │
│ └──────────────────────────────────────────────────────────────┘ │
│                                                                  │
│ Validation                                                       │
│ ✓ Template compiles                                              │
│ ✓ Sample renders                                                 │
│ ⚠ Sanitizer stripped 'hx-on:click' (line 14)                     │
│ ✗ Schema mismatch: missing required field 'severity'             │
│                                                                  │
│ [Publish] [Request changes] [Reject]                             │
└──────────────────────────────────────────────────────────────────┘
```

Behaviour:
- The payload textarea hx-posts to
  `/htmx/widget-types/{slug}/preview` on `keyup changed delay:500ms`,
  re-rendering live. Default content is the type bead's
  `sample_payload`.
- The "Operator view" checkbox toggles `.Viewer.IsOperator` in the
  template scope and re-renders.
- The Render panel shows the sanitized output — bit-identical with
  what would land on a rich-report bead.
- The Validation panel runs `gb widget validate` server-side and
  surfaces findings inline. Each finding deep-links to the offending
  template line.
- The Publish / Request-changes / Reject buttons file decision beads
  pre-bound to this widget-type. Publish requires `RoleOperator`
  (existing oidcauth gate).

The whole tab is **just htmx fragments + the existing bead-show shell**.
No new SPA, no Vite bundle. Same trust boundary as the rest of
beads-viewer.

### 4.3 Diagnostics tab

Operators land on Diagnostics when a widget is misbehaving in the wild.
The tab shows:

- **Recent errors** (last 50 from the per-slug ring buffer): timestamp,
  rich-report bead id where it triggered, error class (compile / render
  / sanitizer / CSP-violation), short message, "show evidence" link
  expanding the offending HTML snippet.
- **CSP violations** filtered to this slug's URLs (ka-0000000873 §3
  `report-uri`).
- **Sanitizer rejection histogram** — top 10 attributes the sanitizer
  has stripped from this widget's renders, in descending count.
  Surfaces "this widget keeps trying to use `hx-on*`; the author needs
  guidance."
- **Live polling cadence** — cumulative requests/min for this widget's
  `/htmx/widget/{slug}/...` route. Crossed against the type's declared
  `live_interval_secs`. Anomalies highlighted.

Diagnostics is operator-only (RoleOperator required) — sanitizer
evidence can leak partial payload contents, which is sensitive for
incident-intel data.

### 4.4 Inline preview on the type bead's bead-show

The type bead's default bead-show page (the non-`/widget-types/{slug}`
view, which agents and other consumers will hit by default) gets a
small "Preview" panel between Description and Comments:

```
Description
…

▶ Preview (sample payload)
  …rendered widget…
  [Open full preview →] /widget-types/{slug}

Comments
…
```

The "▶" lazy-loads via `hx-get` on `intersect once` (htmx visibility
trigger; allowed by sandbox ka-0000000873 §2). This means most bead-show
visits don't pay the render cost; only operators who scroll into the
preview panel do.

### 4.5 The "type bead" page is more important than the "widget gallery"

Today's beads-viewer surfaces widgets only via instances on rich-report
beads. The operator's mental model has to flip: free-htmx widgets are
**registered renderers** with a public-ish reputation, not invisible
rendering plumbing. The `/widget-types/{slug}` page is the durable home
for that reputation; rich-report instances are short-lived consumers.

This is why the type-bead page deserves a tab structure (Overview /
Preview / Diagnostics / History) rather than a single dense panel.

---

## 5. Diagnostic UX — surfacing errors

This is the make-or-break of operator UX. Three error classes:

### 5.1 Template compile error

**When.** Agent saved a template with bad Go template syntax
(`{{ if .X }` instead of `{{ if .X }}`).

**Where surfaced:**
- `gb widget lint`: prints the compile error with `template:line:col`.
- `gb widget preview`: exits 1, prints the error.
- `gb widget validate`: fatal finding.
- Beads-viewer Preview tab: red banner with the line, snippet of the
  template body around the line, and the parse error.
- Rich-report bead-show: the widget area renders a small inline error
  card "Widget template did not compile (kd-…); see Diagnostics →"
  linking to the type bead's Diagnostics tab. **The rich-report does
  not 500.**

### 5.2 Sanitizer rejection

**When.** The rendered HTML contains an attribute or element the
sanitizer strips (e.g. agent wrote `hx-on:click`).

**Where surfaced:**
- `gb widget validate` / `lint`: finding with severity `error`,
  pointing at the offending template line (lint can spot the literal
  `hx-on` even before render).
- Beads-viewer Preview tab: the Render panel shows the *sanitized*
  output (not the raw output), with the Validation panel listing each
  rejection. This is the right default — the operator sees what users
  will see, plus a list of why it differs.
- Rich-report bead-show: the widget renders sanitized; no chrome
  difference at all. The author who shipped a draft with rejections
  sees the diff in their own author loop, not in production. (We do
  not "fail loud" in production for sanitizer rejections because the
  sanitized output is still safe; loud failure would just train the
  author to use `--no-sanitize`.)

### 5.3 Schema / payload mismatch

**When.** A widget-instance's `data` field doesn't match the type's
JSON Schema (e.g. missing required field).

**Where surfaced:**
- `gb widget emit`: best-effort schema check today (existing); add
  fail-fast on missing-required.
- `gb widget preview`: exit 4, print findings.
- Rich-report bead-show: the widget renders an inline "Payload didn't
  match schema" card with the missing fields listed; rest of the page
  unaffected.
- Beads-viewer widget-instance bead-show: the bead's Description gets
  a "schema-mismatch" warning chip. Author sees it on their own bead.

### 5.4 Render-time panic / nil-deref

**When.** Template references `.Payload.user.id` but `.user` was nil.

**Where surfaced:**
- `gb widget preview`: exit 2, print the panic stack with the template
  context.
- Rich-report bead-show: same inline error card as compile-error case.
  We **catch + log** in a per-render `recover()`; one widget panic
  cannot crash the rich-report page.
- Diagnostics tab: panic surfaces here as an error class with stack
  trace.

### 5.5 Unified error chip — single visual primitive

All four error classes render to the same inline chip primitive on
rich-report bead-show:

```
┌─ Widget rendering issue ─────────────────────────────────────┐
│ Type:   <slug> (kd-…)                                        │
│ Issue:  <one-line summary>                                   │
│ When:   2026-05-09T19:23:01Z                                 │
│ [Open Diagnostics] [Open type bead] [Dismiss]                │
└──────────────────────────────────────────────────────────────┘
```

Operators learn one chip pattern; the chip's "Open Diagnostics" link is
the canonical drill-down.

---

## 6. Authoring loop (worked example, end-to-end)

A gardener crew member wants to add a widget that shows "deploy lag" on
incident rich-reports.

### Step 1 — Search for existing types

```text
$ gb widget similar "deploy lag"
Semantic matches (model=…):
  ID            SCORE  SLUG                  VERSION  TITLE
  ka-7K…        0.21   service-health        2        Service health
  ka-8X…        0.18   activity-log          1        Activity log
  …
```

Nothing close. Move to scaffold.

### Step 2 — Scaffold a draft

```
$ gb widget scaffold-htmx deploy-lag \
    --summary "Time-since-last-deploy + lag warnings" \
    --with-fields service:string,lag_minutes:integer,last_deploy_at:string \
    --with-live \
    --out /tmp/deploy-lag.md
Wrote 142 lines to /tmp/deploy-lag.md
```

Agent edits `/tmp/deploy-lag.md`: tweaks the headline, adjusts the
template body to render the `lag_minutes` as a colour-coded chip, etc.

### Step 3 — Lint while editing

```
$ gb widget lint /tmp/deploy-lag.md
✓ Template parses
✗ Template uses banned attribute hx-on:click (line 23)
✗ Template URL '/api/deploys' must start with '/htmx/' (line 31)
```

Agent fixes, repeats. Lint is sub-second; the agent stays in the loop.

### Step 4 — Preview

```
$ gb widget preview /tmp/deploy-lag.md \
    --payload '{"service":"controller","lag_minutes":42,"last_deploy_at":"2026-05-09T18:00:00Z"}' \
    > /tmp/deploy-lag.html
$ open /tmp/deploy-lag.html
# (or: gb widget preview /tmp/deploy-lag.md --payload @/tmp/p.json --browser)
```

Looks right. Agent now files the type bead.

### Step 5 — File the type bead

```
$ gb widget scaffold-htmx deploy-lag --commit < /tmp/deploy-lag.md
Created widget-type ka-XYZ (slug=deploy-lag, version=1, stage=draft)
```

(Or the agent uses `kd create --type=widget-type` directly; both paths
produce the same bead.)

### Step 6 — Validate the bead

```
$ gb widget validate ka-XYZ --strict
✓ Schema parses
✓ Template parses
✓ Sample payload validates
✓ Sample render succeeds
✓ Sanitizer no-ops
✓ URL helpers only
✓ Banned attributes absent
⚠ Polling cadence allowed
✓ Class namespacing
✓ Stable target IDs
✓ Description has Summary section
✓ widget_version set

PASS — ka-XYZ is ready for publish review
```

### Step 7 — Dogfood: emit on a draft rich-report

```
$ kd create --type=rich-report --title="deploy-lag draft preview" \
    --label='widget-report:1' --description=$(cat <<EOF
Draft demo of the new deploy-lag widget. parent(ka-XYZ).
EOF)
Created ka-RP1
$ gb widget emit deploy-lag --report ka-RP1 \
    --data '{"service":"controller","lag_minutes":42,"last_deploy_at":"2026-05-09T18:00:00Z"}' \
    --draft-ok
warning: emitting draft widget on non-draft rich-report ka-RP1
Created widget-instance ka-INST1 (widget_type=deploy-lag) on rich-report ka-RP1
```

Agent links the rich-report from a `gb squawk` to the operator: "deploy-
lag widget ready for review on ka-RP1; type bead ka-XYZ."

### Step 8 — Operator reviews + publishes

Operator opens `https://beads-viewer/widget-types/deploy-lag`, hits
Preview tab, tries a few payloads (including malformed inputs to
exercise the error chip), looks at Diagnostics (empty — newly created),
then hits Publish.

```
# Equivalent CLI (on the operator's machine):
$ gb widget publish ka-XYZ --note "approved 2026-05-09; covers deploy-lag visibility gap"
Validating ka-XYZ ...
PASS — flipping to widget-stage:published
Audit comment recorded: "published by U-OPERATOR; …"
```

### Step 9 — Agent gets unblocked

The gardener crew listens for `widget-stage:published` (kbeads watch on
labels) and resumes its plan, emitting `deploy-lag` widgets on real
incident rich-reports.

**Total wall-clock for the loop**: minutes for the agent (steps 1-7),
minutes for the operator (step 8). No MR, no engineer, no Go file. The
operator review is the gate.

---

## 7. Dogfooding mandate

Per the parent epic, every closing artifact under ka-0000006879 ships
as a widget instance on a rich-report bead. The gardener (ka-0000003004)
is the enforcer; this section spells out the agent-side mechanics.

### 7.1 Pre-port (today's widgets)

Every research bead's closing comment includes:

1. The research-doc path under `gardenio/docs/research/free-htmx-*.md`.
2. The MR url that landed it.
3. A rich-report bead id whose contents include:
   - `markdown-narrative` widget with the doc TL;DR.
   - `activity-log` widget with bullets summarising the research
     children + decisions.
   - `related-tickets` widget linking sibling research beads.
   - A `headline` widget (Phase-4 builtin) with the research title.

The ka-0000000873 closing comment already follows this pattern (rich-
report ka-0000006906 + widget instances ka-0000006515,
ka-0000000917); use it as the template.

### 7.2 Post-port (free-htmx widgets)

Once the wire shape (ka-0000000776) and one Phase-4 migration
(ka-0000007457) land, the gardener switches to **emitting one free-htmx
widget per closing artifact**, in addition to the pre-port widgets.

Recommended free-htmx dogfood widgets:
- `research-summary` — fields: `tldr`, `siblings[]`, `decisions[]`,
  `mr_url`. Renders a card with TL;DR + a list of links.
- `agent-loop-trace` — fields: `steps[]` with `step.label`,
  `step.duration`, `step.outcome`. Renders a horizontal timeline.
  (Useful for showing the gardener's agent loop on its own bead.)
- `next-actions` — fields: `actions[]` with `description` + `bead_id`.
  Renders a checklist with `hx-post` to mark done. (Demonstrates the
  action surface from ka-0000007136.)

Each is a small piece of authoring effort and produces a visible
operator artifact.

### 7.3 Closing-checklist for each ka-0000006879 child

When a research bead under ka-0000006879 closes, the closing comment
**must** include:

```
Research delivered:
  Doc: gardenio/docs/research/free-htmx-widget-<slice>.md
  MR:  https://gitlab.com/.../merge_requests/NNNN
Dogfood:
  Pre-port rich-report:  kd-… (with markdown-narrative + activity-log + related-tickets)
  Post-port instance:    kd-… (free-htmx widget on the same rich-report; once wire shape ships)
Synthesis: ka-0000003004
```

The gardener's dispatch prompt enforces this checklist; the synthesis
bead audits compliance.

---

## 8. Decision points for synthesis (ka-0000003004)

This doc takes positions, but the synthesis bead is the operator's
final pick. The forks worth flagging:

1. **3-stage vs. 4-stage lifecycle for free-htmx.** This doc recommends
   3 (`draft → published → deprecated`); a defensible 4-stage
   alternative is to keep `proposed` as "agent-volunteered for
   publish". I prefer dropping it and using a `gb decision create`
   instead, but the operator may want the visible-in-list semantics.
2. **`gb widget publish` permission gate.** Operator-only via
   `RoleOperator` is the recommendation. Defensible alternative:
   `RoleViewer` with a 24h "publish-pending" buffer that any operator
   can override. Recommend operator-only — buffers add cognitive load
   and weaken the gate.
3. **Diagnostics retention.** Per-slug ring buffer of 50 entries is
   cheap; an alternative is a kbeads "diagnostic" bead per error
   (full audit, but noisy). Recommend ring buffer; escalate to a bead
   only on repeated errors of the same class.
4. **Inline preview on type-bead bead-show: lazy or eager?** Recommend
   lazy (`hx-trigger="intersect once"`). Eager would melt the viewer
   on a list-of-types page.
5. **`gb widget preview --browser` on agent pods.** Agent pods don't
   have `$BROWSER`; the verb prints the URL instead. Operator runs
   the command on their own machine. Recommend: print URL when
   `$BROWSER` unset, no surprise.

These are all small forks; I'd file them under the synthesis decision,
not as separate decisions.

---

## 9. Net-new beads-viewer pages — punch list

In dependency order so an implementation MR can land them in slices
(echoes §4.1 for reference):

| #  | Route                                                | Net-new? | Notes                                      |
| -- | ---------------------------------------------------- | -------- | ------------------------------------------ |
| 1  | `/widget-types/`                                     | yes      | Index / gallery; filter chips.             |
| 2  | `/widget-types/{slug}`                               | yes      | Tabbed type-bead show page.                |
| 3  | `/htmx/widget-types/{slug}/preview`                  | yes      | htmx render-fragment endpoint.             |
| 4  | `/htmx/widget-types/{slug}/diagnostics`              | yes      | htmx error-list fragment.                  |
| 5  | `/widget-types/{slug}/history`                       | yes      | Version timeline.                          |
| 6  | `/htmx/widget-types/csp-report`                      | yes      | CSP report-uri receiver.                   |
| 7  | Rich-report draft chip                               | edit     | `rich_report.go` stage-aware rendering.    |
| 8  | Rich-report deprecated chip                          | edit     | `rich_report.go` stage-aware rendering.    |
| 9  | Decision-create deep-link from type-bead show        | edit     | Pre-fills `[publish, request-changes, reject]`. |
| 10 | Type-bead bead-show inline preview panel             | edit     | Lazy `hx-get` to route #3.                 |
| 11 | Sidebar filter for `kind=htmx vs builtin`            | edit     | Existing widget-types filter list.         |

Post-v1:
- Per-widget rate-limit dashboard (ka-0000007136 §4.2 surfaces).
- "Widgets in the wild" search (find rich-reports using a given slug).
- Widget similarity browser (group by template-body distance).

---

## 10. File map (where the implementation lands)

```text
gardenio/controller/cmd/gb/
├── widget.go                  (EDIT) — root command + groupings
├── widget_scaffold.go         (NEW)  — gb widget scaffold-htmx
├── widget_lint.go             (NEW)  — gb widget lint
├── widget_preview.go          (NEW)  — gb widget preview
├── widget_validate.go         (NEW)  — gb widget validate
├── widget_publish.go          (NEW)  — gb widget publish
├── widget_deprecate.go        (NEW)  — gb widget deprecate
└── widget_test.go             (EDIT) — coverage for all new verbs

gardenio/controller/cmd/beads-viewer/
├── widget_types_index.go      (NEW)  — /widget-types/
├── widget_type_show.go        (NEW)  — /widget-types/{slug} tabs
├── htmx_widget_preview.go     (NEW)  — /htmx/widget-types/{slug}/preview
├── htmx_widget_diagnostics.go (NEW)  — /htmx/widget-types/{slug}/diagnostics
├── htmx_widget_csp.go         (NEW)  — /htmx/widget-types/csp-report
├── rich_report.go             (EDIT) — stage-aware draft/deprecated chips
└── server.go                  (EDIT) — wire the new mux subtree

gardenio/controller/internal/widgets/
├── render.go                  (NEW)  — shared template + sanitize entrypoint
└── lifecycle.go               (NEW)  — stage transitions, validate gate
```

Total: 8 new files, 4 edits.

---

## 11. Implementation budget (rough)

Pre-implementation guesses; the implementation epic owners refine.

| Slice                                                    | Eng-days |
| -------------------------------------------------------- | -------- |
| `gb widget scaffold-htmx`                                | 1        |
| `gb widget lint`                                         | 1        |
| `gb widget preview`                                      | 2        |
| `gb widget validate`                                     | 1.5      |
| `gb widget publish` + `deprecate`                        | 1        |
| `internal/widgets/render.go` shared core                 | 2        |
| `/widget-types/` index + `/widget-types/{slug}` tabs     | 2        |
| `/htmx/widget-types/{slug}/preview` + diagnostics + CSP  | 2.5      |
| Rich-report stage-aware chips                            | 1        |
| Tests + docs                                             | 1.5      |
| **Total**                                                | **~15.5 eng-days** |

Roughly the same shape as ka-0000007136's 11.5 eng-days; together they
form the bulk of the free-htmx implementation effort.

---

## 12. Open questions for synthesis

1. **3- vs 4-stage life.** §8.1. Recommend 3.
2. **`publish` permission gate.** §8.2. Recommend operator-only.
3. **Diagnostics retention shape.** §8.3. Recommend ring buffer.
4. **Inline preview eager vs lazy.** §8.4. Recommend lazy.
5. **`--browser` behaviour on agent pods.** §8.5. Recommend "print
   URL when `$BROWSER` unset."
6. **Naming: `published` vs `stable`.** I picked `published` to reflect
   "no MR happens here." If the synthesis prefers continuity with the
   builtin lifecycle, use `stable`; the rendering and gate semantics
   are identical.
7. **Forking workflow.** Should `gb widget scaffold-htmx --from <slug>`
   create a *new slug* (current proposal) or a new version of the same
   slug? Recommend new slug — versions are reserved for evolved-in-
   place widgets, not "I want one like this but different."

---

## Appendix A — example draft widget-type bead body

This is the canonical wire shape a `scaffold-htmx --commit` produces;
it's also what `gb widget show` prints back. The wire-shape sibling
(ka-0000000776) owns the final schema; this is the operator-UX-aligned
view.

```text
Title:        Deploy lag widget
Type:         widget-type
Kind:         data
Labels:       widget-stage:draft, project:gardenio
Fields:
  slug:                deploy-lag
  widget_version:      1
  renderer_kind:       htmx
  live_interval_secs:  30

Description:

## Summary

Renders time-since-last-deploy + lag warnings for a service.

## Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "type": "object",
  "required": ["service", "lag_minutes", "last_deploy_at"],
  "properties": {
    "service":         {"type": "string", "example": "controller"},
    "lag_minutes":     {"type": "integer", "minimum": 0, "example": 42},
    "last_deploy_at":  {"type": "string", "format": "date-time"}
  },
  "additionalProperties": false
}
```

## Sample payload

```json
{
  "service": "controller",
  "lag_minutes": 42,
  "last_deploy_at": "2026-05-09T18:00:00Z"
}
```

## Template

```html
{{ define "deploy-lag" }}
<section class="w-deploy-lag" id="widget-{{.Widget.ID}}-body"
         {{ liveAttrs .Widget }}>
  <header class="w-deploy-lag-head">
    <span class="w-deploy-lag-service">{{ .Payload.service }}</span>
    <span class="w-deploy-lag-since">last deployed
      {{ .Payload.last_deploy_at }}</span>
  </header>
  <div class="w-deploy-lag-body">
    {{ if gt .Payload.lag_minutes 60 }}
      <span class="w-deploy-lag-pill w-deploy-lag-pill-bad">
        {{ .Payload.lag_minutes }}m lag</span>
    {{ else if gt .Payload.lag_minutes 15 }}
      <span class="w-deploy-lag-pill w-deploy-lag-pill-warn">
        {{ .Payload.lag_minutes }}m lag</span>
    {{ else }}
      <span class="w-deploy-lag-pill w-deploy-lag-pill-ok">
        {{ .Payload.lag_minutes }}m lag</span>
    {{ end }}
  </div>
  {{ if .Viewer.IsOperator }}
    <button hx-post="{{ actionURL "rate" .Widget }}?score=helpful"
            hx-target="#widget-{{ .Widget.ID }}-body"
            hx-swap="innerHTML">Helpful</button>
  {{ end }}
</section>
{{ end }}
```
```

---

## Appendix B — `gb widget` help, end-to-end

Approximate help output after the changes in this doc land.

```
$ gb widget --help

Manage widget types and instances (Incident Intelligence + free-htmx).

Authoring:
  scaffold-htmx   Print a starter htmx widget-type bead body
  lint            Static checks on the template body
  preview         Render a widget-type to stdout or in browser
  validate        Full pre-publish gate (schema + render + sanitize)

Lifecycle:
  publish         Flip widget-stage:draft → widget-stage:published
  deprecate       Flip widget-stage:published → widget-stage:deprecated

Discovery:
  emit            Create a widget-instance on a rich-report bead
  list            List registered widget-types
  show            Show a widget-type's body, schema, sample, and history
  similar         Find existing widget-types similar to a description

Use 'gb widget similar' before drafting a new type — reuse beats reinvention.
```

End of doc.
