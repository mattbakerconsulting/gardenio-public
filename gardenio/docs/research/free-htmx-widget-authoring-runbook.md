# Free-htmx widget — gardener authoring loop & agent workflow runbook

> ## ⚠️ DEPRECATED (2026-08-02)
>
> **Widgets and reports are deprecated in favor of bundles.** Operator directive
> from Matt Baker (Slack thread ka-0000004862): low-quality widget-based reports
> were reaching operators. Do **not** follow this runbook to author new widgets.
>
> Publish user-facing deliverables as a `type=bundle` with `type=doc` children:
>
> ```sh
> gb bundle create <origin-bead-id> --title "..." --kind report \
>     --headline "one-line takeaway" --summary "the paragraph a VP reads"
> gb bundle doc <bundle-id> --title "Root cause" --format md --content @rca.md
> ```
>
> `gb widget emit` and `gb widget scaffold-htmx --commit` now refuse to run
> without `--i-know-widgets-are-deprecated`. See advice **ka-0000007697** and
> tracking bead **ka-0000007561**. The **dogfooding mandate** described below
> (emit a widget instance per cycle) is **withdrawn** — advice ka-0000004820 is
> closed. This document is retained for maintaining already-published widgets.

> Synthesis deliverable for **ka-0000003004** — Phase 1 closing artifact for
> the free-form htmx widget research epic.
>
> **Parent epic:** ka-0000006879
> **Gardener:** ka-0000005515 (disabled until operator review)
> **Gardener rich-report (dogfood surface):** ka-0000003788
>
> **Synthesizes:**
> - ka-0000000776 — wire shape & template contract
> - ka-0000000873 — sandbox / CSP / sanitizer policy
> - ka-0000007136 — backend contract (`/htmx/*` route surface)
> - ka-0000002102 — dogfooding + operator UX (CLI verbs, preview surface, lifecycle)
>
> Sibling still in flight: ka-0000007457 (port a Phase-4 widget end-to-end).

---

## TL;DR

The gardener authoring loop is **eight stages**, each with a single CLI
invocation and a single validation gate. The renderer lives in the bead
description and **does not need an MR**: only an operator label flip.

| # | Stage           | Primary CLI                                          | Where the artifact lives                                                  | Validation gate                                                |
| - | --------------- | ---------------------------------------------------- | ------------------------------------------------------------------------- | -------------------------------------------------------------- |
| 1 | Search          | `gb widget similar "<description>"`                  | (read-only)                                                               | Semantic-search recall ≥ N hits OR explicit "no match" log.    |
| 2 | Scaffold        | `gb widget scaffold-htmx <slug> --out <file>`        | Local file (e.g. `/tmp/<slug>.md`) — *not* a bead yet.                    | Scaffold parses (`gb widget lint <file>` clean).               |
| 3 | Edit            | (editor)                                             | Same local file.                                                          | `gb widget lint <file>` after each save.                       |
| 4 | Preview         | `gb widget preview <file> --payload @sample.json`    | stdout HTML or `--browser` URL.                                           | Exit 0 (compile + sanitize + schema all green).                |
| 5 | File the bead   | `gb widget scaffold-htmx <slug> --commit < <file>` OR `kd create --type=widget-type …` | New `widget-type` bead (`widget-stage:draft`, `renderer_kind=htmx`).     | Bead created; `gb widget validate <id> --strict` passes.        |
| 6 | Emit instance   | `gb widget emit <slug> --report <ka-rr> --data @…`   | New `widget-instance` bead parented to a rich-report.                     | kbeads pre-write hook validates payload against the type bead's `schema`. |
| 7 | Operator review | (operator browses `/widget-types/<slug>` Preview tab)| Decision bead bound to the type bead.                                     | Operator picks `[publish, request-changes, reject]`.            |
| 8 | Promote         | `gb widget publish <id> --note "<rationale>"`        | Label flip on the type bead (`widget-stage:draft` → `:stable`).        | `validate --strict` re-runs server-side; `RoleOperator` enforced.|

**Key invariants:**

- Every renderer is reviewed exactly once (at publish), never per-instance.
- `gb widget similar` is the de-duplication primitive; the operator publish
  gate is the curation primitive. Together they cap catalog sprawl.
- The gardener's mandate "every artifact ships as a widget instance" is
  enforced by **closing-comment lint** in the gardener dispatch prompt
  (§4.2) and by an audit step on the rich-report bead (§4.4).
- Failure of any stage drops the agent back into the previous one with a
  structured diagnostic — never further down the pipeline.

---

## 1. Why this runbook exists

C1–C5 each took a slice of the free-htmx contract and locked it down. The
operator does not need to read 2,944 lines of research to understand "what
does an agent actually do, end to end, when the gardener dispatches it." This
doc is that walk-through, with each step pinned to:

- The exact CLI invocation an agent runs.
- Which file or bead carries the artifact at that stage.
- The single validation gate that says "advance" vs "loop back".

The point is to make the loop trivially auditable. If at any step the
artifact doesn't match the gate, the gardener halts the crew and the
operator looks at one thing.

---

## 2. The eight-stage workflow in detail

### Stage 1 — Search existing widget-types

> *"Reuse beats reinvention."* Today's `gb widget` already says this in its
> `Long` text. The gardener enforces it as a hard precondition.

**CLI:**

```sh
gb widget similar "<description of intent, ≥ 6 words>" --json
```

**File / bead:** none — read-only against the daemon's semantic index.

**Validation gate:**
- If the top result has score ≥ 0.45 *and* `widget-stage:stable`, **the
  agent must use it** instead of scaffolding. The gardener rejects a
  scaffold that follows a strong-match search without a written rationale
  in the new type bead's description ("Forked because: …").
- If no match crosses 0.45, the agent logs `"no widget-type match for: …"`
  and proceeds to stage 2.

**Notes:**
- Per ka-0000002102 §3.2, `gb widget similar` defaults to
  `widget-stage:stable` only. Drafts do not surface here unless the
  agent passes `--include-draft` — drafts are not de-duplication targets.
- The gardener dispatch prompt (§4.2) carries this rule verbatim. An agent
  that skips it is a soft policy violation reported on the gardener
  rich-report (ka-0000003788).

### Stage 2 — Scaffold a draft

**CLI:**

```sh
gb widget scaffold-htmx <slug> \
  --summary "<one-liner>" \
  --with-fields "k1:type,k2:type" \
  [--with-live] [--with-action <name>] \
  --out /tmp/<slug>.md
```

**File / bead:** local file `/tmp/<slug>.md` containing four sections —
`Summary`, `Schema` (fenced ```json```), `Sample payload`, `Template`
(fenced ```html```). See ka-0000000776 §"Concrete example" and
ka-0000002102 Appendix A for canonical body.

**Validation gate:** `gb widget lint /tmp/<slug>.md` exits 0. Lint runs:
- `html/template.Parse(body)` (compile errors with line numbers).
- Banned-attribute scan: `hx-on*`, `hx-include`, `hx-headers`, `hx-ext`,
  `hx-vars`, `style=`, `srcdoc=`, `formaction=` (ka-0000000873 §2).
- URL prefix scan: every literal `hx-get`/`hx-post` value either starts
  with `/htmx/` or comes from a URL helper (`{{ panelURL … }}`,
  `{{ actionURL … }}`, `{{ apiURL … }}`, `{{ decisionOptionURL … }}` —
  ka-0000007136 §5.2 / `htmx_helpers.go`).
- `hx-target` scan: `this`, `closest …`, `find …`, or `#widget-…` /
  `#w-…`.

**Failure mode:** lint failure routes back to stage 2 (re-edit). Lint is
sub-second; the agent stays in the loop without round-tripping the daemon.

### Stage 3 — Edit

The agent edits `/tmp/<slug>.md` in place — adjusting headlines, layout,
htmx attributes, schema, sample payload. Reruns `gb widget lint` after
each save.

**File / bead:** unchanged from stage 2.

**Validation gate:** `gb widget lint` clean *and* the template body
references the URL helpers, not literal paths — the gardener's closing
audit (§4.4) flags literal `/htmx/...` strings as a "should be `panelURL`"
warning.

### Stage 4 — Preview

**CLI:**

```sh
gb widget preview /tmp/<slug>.md \
  --payload '{"k1":"v1","k2":42}' \
  > /tmp/<slug>.html
# or:
gb widget preview /tmp/<slug>.md --payload @/tmp/p.json --browser
```

**File / bead:** rendered HTML on stdout, written to disk for inspection.
Browser mode opens a one-shot URL (`/widget-types/<slug>/preview`) on
beads-viewer; agent pods that lack `$BROWSER` print the URL for the
operator (ka-0000002102 §8.5).

**Validation gate:** `gb widget preview` exit codes (ka-0000002102 §3.1):

| Exit | Meaning                                              | Loop back to |
| ---- | ---------------------------------------------------- | ------------ |
| 0    | Render OK; sanitizer made no rejections.             | advance to 5 |
| 1    | Template compile error.                              | stage 3      |
| 2    | Render-time error (nil-deref, range over `nil`, …).  | stage 3      |
| 3    | Sanitizer stripped one or more elements / attributes.| stage 3      |
| 4    | Payload failed schema validation.                    | stage 3      |

**Notes:**
- Preview is bit-identical with what production renders on a rich-report
  bead. The sanitizer policy (ka-0000000873 §2 `FreeHtmxPolicy`), the URL
  allowlist, and the strict CSP all apply at preview time.
- `--no-sanitize` is *strictly diagnostic* (prints a warning header). The
  gardener audit rejects any closing artifact that still uses
  `--no-sanitize` in its dispatch trace.

### Stage 5 — File the widget-type bead

Two equivalent paths; both produce the same bead:

**Path A — `gb widget scaffold-htmx --commit`:**

```sh
gb widget scaffold-htmx <slug> --commit < /tmp/<slug>.md
# → Created widget-type ka-XYZ (slug=<slug>, version=1, stage=draft)
```

**Path B — explicit `kd create`:**

```sh
kd create "Widget: <title>" \
  --type=widget-type --kind=data \
  --label=widget-stage:draft \
  -f slug=<slug> \
  -f widget_version=1 \
  -f renderer_kind=htmx \
  -f template_engine=go-html-template-v1 \
  -f schema=@/tmp/<slug>.schema.json \
  -f template_source=@/tmp/<slug>.tmpl \
  -d @/tmp/<slug>.md
```

Path A is the default; Path B is for agents that already split the artifact
across files or want to attach the type bead under a parent epic with extra
typed mentions.

**File / bead:** new `widget-type` bead (`ka-XYZ`).

**Validation gates (two layers):**

1. **Server-side write hook** (ka-0000000776 §"Validation: where it runs"
   layer 2). On `widget-type` create where `renderer_kind=htmx`, kbeads
   compiles the template, parses the schema, and rejects malformed
   payloads. A bead that lands has at minimum a parseable template +
   parseable schema.
2. **`gb widget validate <id> --strict`** (ka-0000002102 §3.1). Runs the
   full pre-publish checklist: schema parses, template parses, sample
   payload validates, sample render succeeds, sanitizer no-ops, URL
   helpers only, banned attrs absent, polling cadence allowed, class
   namespacing, stable target IDs, description has `## Summary`,
   `widget_version` set.

**Failure mode:** `validate --strict` failure → fix the type bead body
(`kd update ka-XYZ -d @/tmp/<slug>.md` or
`kd update ka-XYZ -f template_source=@…`) and re-validate. The bead can
stay in `draft` while the agent iterates; the operator review gate
(stage 7) is the durable checkpoint.

### Stage 6 — Emit the first instance

The gardener mandate ("every artifact ships as a widget instance" —
ka-0000006879 *Dogfooding mandate*) lands here for free-htmx widgets. The
first emission is on a *draft* rich-report so the operator can review the
widget without publishing an instance into a real incident report.

**Bootstrap rich-report:**

```sh
kd create "<slug> draft preview" \
  --type=rich-report --kind=issue \
  --label=widget-report:1 \
  -f headline="<slug> dogfood preview" \
  -f report_kind=research-rollup \
  -f severity=P3 \
  -d "Draft preview rich-report for widget <slug>. parent(ka-XYZ)."
# → Created ka-RP1
```

**Emit:**

```sh
gb widget emit <slug> \
  --report ka-RP1 \
  --version 1 \
  --data @/tmp/<slug>.payload.json \
  --draft-ok
# warning: emitting draft widget on non-draft rich-report ka-RP1
# Created widget-instance ka-INST1
```

**File / bead:** new `widget-instance` bead (`ka-INST1`) parented to
`ka-RP1`.

**Validation gate:** **kbeads pre-write hook** runs JSON Schema validation
of the `data` payload against the type bead's `schema` field
(ka-0000000776 §"Validation: where it runs" layer 2 — *authoritative*).
This is the same hook regardless of who calls the daemon — `gb widget
emit`, a curl script, a future Python tool. Layer 1 (CLI advisory) is
faster UX but no longer load-bearing for safety.

**Notes on dogfooding:**
- The gardener emits **two waves** of instances on the gardener rich-report
  ka-0000003788:
  - **Pre-port** — today's stable builtins (`markdown-narrative`,
    `activity-log`, `related-tickets`, headline) summarising the
    research artifact. This is what ka-0000000873, ka-0000007136, and
    ka-0000002102 already do (see those beads' closing comments).
  - **Post-port** — once ka-0000000776 lands and the wire shape is
    deployed, the gardener additionally emits **one free-htmx widget**
    per closing artifact: e.g. `research-summary`, `agent-loop-trace`,
    `next-actions` (ka-0000002102 §7.2). This is the "dogfood the new
    substrate as it ships" milestone.
- This synthesis bead's own deliverable: a `research-summary` instance on
  ka-0000003788. Until the free-htmx renderer is live, the dogfood
  fallback is `markdown-narrative` + `activity-log` + `related-tickets`.

### Stage 7 — Operator review

The agent posts a `gb squawk` linking the type bead and its draft preview
rich-report:

```sh
gb squawk "deploy-lag widget ready for review on ka-RP1; type bead ka-XYZ. \
gb widget validate ka-XYZ → PASS. Preview: \
https://beads-viewer/widget-types/deploy-lag"
```

**Operator workflow** (ka-0000002102 §4.2):
1. Open `/widget-types/<slug>` in beads-viewer.
2. **Preview tab** — payload textarea hx-posts on `keyup changed
   delay:500ms` to `/htmx/widget-types/<slug>/preview`; live re-render.
   Try a few payloads, including malformed inputs.
3. **Diagnostics tab** — last 50 sanitizer rejections + CSP violations
   for this slug. New widgets start empty.
4. **Validation panel** — runs `gb widget validate` server-side, surfaces
   findings inline. Each deep-links to the offending template line.
5. Pick `[publish]`, `[request-changes]`, or `[reject]`. The buttons file
   a decision bead pre-bound to the widget-type.

**File / bead:** decision bead, parented to the widget-type bead.

**Validation gate:** operator-driven; outcome flows to stage 8.

**Notes:** if the operator picks `request-changes`, the decision report
carries the change list and the gardener crew loops back to stage 3.

### Stage 8 — Publish (operator-only label flip)

**CLI (operator runs on their own machine):**

```sh
gb widget publish ka-XYZ --note "approved 2026-05-09; covers <gap>"
# Validating ka-XYZ ...
# PASS — flipping to widget-stage:stable
# Audit comment recorded: "published by U-OPERATOR; …"
```

**Behaviour** (ka-0000002102 §3.1 `gb widget publish`):
1. Loads the widget-type bead.
2. Refuses if caller lacks `RoleOperator` (existing oidcauth gate).
3. Re-runs `gb widget validate --strict`. `--force` overrides (logged).
4. Removes `widget-stage:draft`, adds `widget-stage:stable`.
5. Files an audit comment (`published by <operator>` + `validate`
   summary).
6. Optional `--announce`: file an `announcement` bead linking the new
   widget.

**File / bead:** label change + audit comment on `ka-XYZ`. Optional
announcement bead.

**Validation gate:** `gb widget validate --strict` is the final
checkpoint (server-side); `RoleOperator` is the authorization gate.

**Post-promotion behaviour:**
- The gardener crew listens for `widget-stage:stable` (kbeads watch on
  labels) and resumes its plan, emitting `<slug>` widgets on real
  incident rich-reports.
- `gb widget similar` immediately returns the new widget by default; the
  catalog is up-to-date for the next agent's stage 1.

---

## 3. End-to-end CLI tape (copy-pasteable)

A complete authoring run for a hypothetical `deploy-lag` widget:

```sh
# --- Stage 1: search
gb widget similar "deploy lag service" --json | jq '.results[:3]'

# --- Stage 2: scaffold
gb widget scaffold-htmx deploy-lag \
  --summary "Time-since-last-deploy + lag warnings" \
  --with-fields "service:string,lag_minutes:integer,last_deploy_at:string" \
  --with-live \
  --out /tmp/deploy-lag.md

# --- Stage 3: edit (then lint)
$EDITOR /tmp/deploy-lag.md
gb widget lint /tmp/deploy-lag.md

# --- Stage 4: preview
gb widget preview /tmp/deploy-lag.md \
  --payload '{"service":"controller","lag_minutes":42,"last_deploy_at":"2026-05-09T18:00:00Z"}' \
  > /tmp/deploy-lag.html

# --- Stage 5: file the type bead
gb widget scaffold-htmx deploy-lag --commit < /tmp/deploy-lag.md
# → ka-XYZ
gb widget validate ka-XYZ --strict

# --- Stage 6: emit on a draft rich-report
kd create "deploy-lag draft preview" --type=rich-report --kind=issue \
  --label=widget-report:1 \
  -f headline="deploy-lag dogfood preview" -f report_kind=research-rollup \
  -f severity=P3 -d "Draft. parent(ka-XYZ)."
# → ka-RP1
gb widget emit deploy-lag --report ka-RP1 --version 1 \
  --data '{"service":"controller","lag_minutes":42,"last_deploy_at":"2026-05-09T18:00:00Z"}' \
  --draft-ok

# --- Stage 7: notify operator
gb squawk "deploy-lag widget ready for review on ka-RP1; type bead ka-XYZ. \
gb widget validate ka-XYZ → PASS. \
Preview: https://beads-viewer/widget-types/deploy-lag"

# --- Stage 8: operator publishes (operator's machine)
gb widget publish ka-XYZ --note "approved; covers deploy-lag visibility gap"
```

**Total wall-clock:** minutes for the agent (1–7), minutes for the
operator (8). No MR, no engineer, no Go file — the operator review is
the gate.

---

## 4. Gardener enforcement — making the workflow the path of least resistance

The gardener (ka-0000005515; bound rich-report ka-0000003788) does three
things to make this loop the default:

### 4.1 Gardener advice (durable rules surfaced on every spawn)

A small set of advice beads scoped to `project:gardenio` and selected for
crew with `area:widgets`:

| Advice (one-liner)                                                                 | Rationale                                                                                          |
| ---------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| Always run `gb widget similar` before scaffolding a new widget-type.               | De-duplication primitive. Catalog sprawl is the #1 failure mode (ka-0000006879 open question).      |
| Edit widget templates as local files; only `--commit` after `gb widget lint` passes. | Saves wasted bead creates; fast feedback loop.                                                     |
| Use URL helpers (`panelURL` / `actionURL` / `apiURL` / `decisionOptionURL`); never literal `/htmx/...`. | Compile-time URL checking + future-proofs against route-shape evolution (ka-0000007136 §5.2). |
| Free-htmx widget instances ride the same `widget-instance` shape as builtin widgets — do not invent a new bead type. | ka-0000000776 §"Wire shape" — instance shape unchanged.                                            |
| Closing-comment template (see §4.3) is required for every artifact; the gardener audits compliance. | Dogfooding mandate enforcement.                                                                    |

These are filed as project-scoped advice (not inline) — they appear in
prime output without bypassing the byte budget.

### 4.2 Gardener dispatch prompt (the hot path)

When the gardener dispatches a crew member to drive a research / port /
implementation slice under ka-0000006879, the prompt includes the
following block verbatim:

```
You are the crew for <bead>. Your closing artifact MUST include a widget
instance on gardener rich-report ka-0000003788. Procedure:

  1. gb widget similar "<topic>" — pick a stable widget if score ≥ 0.45.
  2. If no match: scaffold + edit + lint + preview locally; only --commit
     once gb widget preview returns exit 0.
  3. Validate with gb widget validate --strict before posting the publish
     squawk.
  4. Emit one widget instance on ka-0000003788 with the closing artifact
     summary; if the free-htmx renderer is live, prefer a free-htmx widget
     (research-summary, agent-loop-trace, next-actions). If not, fall back
     to markdown-narrative + activity-log + related-tickets.
  5. Close the bead with the §4.3 closing-comment template, then `gb stop`.

Forbidden in this loop:
  - --no-sanitize on any preview that lands on a closing artifact.
  - Literal /htmx/... URLs in template_source (use the URL helpers).
  - hx-on*, hx-include, hx-headers, hx-ext, hx-vars, style=, srcdoc=,
    formaction=. (Sanitizer strips them; lint catches them earlier.)
```

The forbidden list mirrors ka-0000000873 §2 exactly. The gardener's
spawned crew sessions inherit this prompt; the gardener itself audits that
the closing comment matches the §4.3 template before letting the bead
close.

### 4.3 Closing-comment template

Every research / implementation bead under ka-0000006879 closes with a
comment of this shape (ka-0000000873, ka-0000007136, ka-0000002102 all
follow it today):

```
Research/implementation delivered:
  Doc: gardenio/docs/research/free-htmx-widget-<slice>.md
  MR:  https://gitlab.com/example-org/gardenio/-/merge_requests/NNNN
  Plan bead: kd-<plan-id> (parent: <this-bead>) — if applicable

Dogfood:
  Pre-port (today's widgets): kd-<rich-report> with widget instances kd-<a>, kd-<b>, …
  Post-port (free-htmx, once live): kd-<instance-id> on ka-0000003788
    Type bead: kd-<type-id> (slug=<slug>, version=1, stage=stable|draft)

Synthesis: ka-0000003004
Open forks (if any): listed below or filed as gb decision …
```

The gardener dispatch prompt requires this template; the gardener itself
re-runs `gb widget validate` on every referenced widget-type and posts a
short audit on ka-0000003788.

### 4.4 Gardener audit step

After each crew run, the gardener:
1. Walks `kd list -p ka-0000006879 --status=closed --since=<lastcycle>`.
2. For each closed bead, reads the closing comment.
3. Checks: doc path exists in repo, MR url resolvable, dogfood widgets
   exist as actual `widget-instance` beads parented to ka-0000003788.
4. If anything is missing, files a `gb decision create` against the
   gardener rich-report asking the operator: `[reopen-bead, accept-as-is,
   patch-comment]`.

The audit is one cycle behind so it doesn't block in-flight work.

---

## 5. Failure modes & how the design handles them

### 5.1 Catalog sprawl ("agent drafts hundreds of micro-widgets")

**Mechanisms in tension:**

| Mechanism                                                | Effect                                                                                                  |
| -------------------------------------------------------- | ------------------------------------------------------------------------------------------------------- |
| `gb widget similar` (semantic search, default-stable) | Push agents toward reuse before scaffolding.                                                            |
| Operator publish gate                                    | Only widgets that survive review enter the de-dup pool. Drafts are inert until promoted.                |
| `gb widget similar --include-draft`                      | Lets a curious agent see in-flight drafts but does not surface them by default.                         |
| Per-slug diagnostics ring buffer                         | Surfaces "this widget keeps tripping the sanitizer" — operator data for deprecation decisions.          |
| `widget-stage:deprecated` with `--replaced-by` pointer    | Retire near-duplicates without breaking historical reports.                                             |

**What "catalog sprawl" actually requires to happen:** an agent has to
(a) skip `gb widget similar`, (b) scaffold a new slug instead of bumping
an existing version, (c) get an operator to publish it. That's three
gates. The gardener dispatch prompt + closing audit close the (a) gap; the
operator review surface closes the (c) gap. (b) is fundamentally an
operator-judgement call we accept.

**Watch-metric (post-Phase-3):** count of `widget-stage:stable` slugs
over time, segmented by `renderer_kind`. Linear growth is fine; super-
linear is a sprawl signal.

### 5.2 Schema mismatch on emit

A gardener bypasses `gb widget emit` and POSTs to kbeads directly with the
wrong payload shape. **kbeads pre-write hook (layer 2)** rejects the
write, with a structured error citing the JSON-Schema path. The instance
bead never lands. The gardener logs the error, retries with a corrected
payload.

### 5.3 Template panic at render time

A widget template references `.Payload.user.id` but `.user` was nil. The
beads-viewer renderer's per-widget `recover()` (ka-0000002102 §5.4)
catches the panic, logs it to the per-slug diagnostics ring buffer,
renders the unified error chip (§5.5 of the same doc) inline. **The
rich-report does not 500.** The widget author (or the gardener audit) sees
the error in the Diagnostics tab.

### 5.4 Operator publishes a widget that breaks under real payloads

Two failsafes:
1. **Live diagnostics** — every render-time error on a stable widget
   feeds the per-slug ring buffer. Operator reviewing Diagnostics
   sees the pattern and can `gb widget deprecate` (with
   `--replaced-by`).
2. **Gardener auto-deprecation watchdog (post-v1)** — if a widget
   accumulates ≥ 10 distinct sanitizer rejections or render errors in 24h,
   file a `gb decision` against the operator with options
   `[deprecate, force-fix, accept-noise]`. Tracked as a post-v1
   follow-up, not blocking the runbook.

### 5.5 Gardener itself bypasses the workflow

The gardener dispatch prompt (§4.2) is text — a sufficiently broken crew
session could ignore it. Backstop: the gardener's audit step (§4.4) walks
closed beads under ka-0000006879 and files a decision when any closing
comment is malformed or any referenced widget-instance is missing. The
audit is intentionally one cycle behind — it can't block in-flight work,
only the next cycle's allocations.

---

## 6. What stays the same vs. what's net-new

### 6.1 Unchanged

- `widget-instance` wire shape (ka-0000000776 §"Wire shape").
- `widget_version` versioning (monotonic per slug, instances pin both).
- `gb widget emit / list / show / similar` core semantics.
- Today's builtin widgets keep working under `renderer_kind=builtin`.
- The gardener rich-report dogfood pattern — already used by
  ka-0000000873, ka-0000007136, ka-0000002102 for their closing artifacts
  on ka-0000006906.

### 6.2 Net-new

- New `widget-type` fields: `renderer_kind`, `template_source`,
  `template_engine`, `render_endpoint` (reserved). Existing schema /
  summary fields carry over.
- kbeads pre-write hook on `widget-instance` writes
  (`internal/server/<write_path>.go`).
- 6 new `gb widget` verbs: `scaffold-htmx`, `lint`, `preview`,
  `validate`, `publish`, `deprecate`. Extensions to `emit`, `similar`,
  `list`, `show` (ka-0000002102 §3.2).
- `/htmx/*` route surface in beads-viewer (ka-0000007136 §1.1).
- `/widget-types/` index + `/widget-types/{slug}` tabbed show
  (Overview / Preview / Diagnostics / History) + per-slug diagnostics
  ring buffer (ka-0000002102 §4).
- Per-render `recover()` in `rich_report.go`.
- 3-stage lifecycle for `renderer_kind=htmx`: `draft → stable →
  deprecated` (no `proposed`).

### 6.3 File map (synthesised across C1–C5)

```text
gardenio/controller/cmd/gb/
├── widget.go                   (EDIT)
├── widget_scaffold.go          (NEW) — gb widget scaffold-htmx
├── widget_lint.go              (NEW)
├── widget_preview.go           (NEW)
├── widget_validate.go          (NEW)
├── widget_publish.go           (NEW)
├── widget_deprecate.go         (NEW)
└── widget_test.go              (EDIT)

gardenio/controller/cmd/beads-viewer/
├── widget_types_index.go       (NEW) — /widget-types/
├── widget_type_show.go         (NEW) — /widget-types/{slug} tabs
├── htmx_panels.go              (NEW) — /htmx/bead/{id}/panel/{name}[/live]
├── htmx_decisions.go           (NEW) — /htmx/decision/{id}/option/{opt}
├── htmx_actions.go             (NEW) — /htmx/action/{name} dispatcher
├── htmx_api.go                 (NEW) — /htmx/api/list, /htmx/api/bead/{id}
├── htmx_mezmo.go               (NEW) — /htmx/api/mezmo
├── htmx_widget_preview.go      (NEW) — /htmx/widget-types/{slug}/preview
├── htmx_widget_diagnostics.go  (NEW) — /htmx/widget-types/{slug}/diagnostics
├── htmx_widget_csp.go          (NEW) — /htmx/widget-types/csp-report
├── htmx_helpers.go             (NEW) — panelURL/actionURL/apiURL/… funcs
├── rich_report.go              (EDIT) — stage-aware draft/deprecated chips + recover()
└── server.go                   (EDIT) — wire new mux subtree

gardenio/controller/internal/widgets/
├── render.go                   (NEW) — shared template + sanitize entrypoint
├── lifecycle.go                (NEW) — stage transitions, validate gate
└── policy.go                   (NEW) — FreeHtmxPolicy() (bluemonday + URL allowlist)

kbeads/internal/server/
└── widgets_validate.go         (NEW) — pre-write hook for widget-instance
```

Total (net-new): 18 files; 4 edits.

---

## 7. Open synthesis decisions (the real fork list)

C5 §8 / §12 enumerated seven small forks. Most resolve cleanly; this
section lists the synthesis-bead decisions the operator should ratify.

| #   | Fork                                                         | Recommendation                                       | Why this synthesis takes a position                                                                            |
| --- | ------------------------------------------------------------ | ---------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| S1  | 3-stage vs 4-stage lifecycle for free-htmx                   | **3** (`draft → stable → deprecated`)             | Without an MR gate, `proposed` adds operator UX cost for no semantic difference. C5 §2.1.                      |
| S2  | `gb widget publish` permission                                | **`RoleOperator` only** (no `RoleViewer` buffer)     | Buffers weaken the gate; operator review is the curation primitive (§5.1).                                     |
| S3  | Diagnostics retention shape                                   | **Per-slug ring buffer (50 entries)**                | Bead-per-error is noisy. Escalate to a bead only on repeated same-class errors.                                |
| S4  | Inline preview on type-bead bead-show                         | **Lazy** (`hx-trigger="intersect once"`)             | Eager would melt a list-of-types page.                                                                          |
| S5  | `gb widget preview --browser` on agent pods                   | **Print URL when `$BROWSER` unset; no surprise**     | Agent pods cannot open browsers; printing the URL routes the operator to it.                                   |
| S6  | Naming: `published` vs `stable` for the htmx promoted stage  | **`stable`** (RESOLVED — DEC-S6 ka-0000006220) | Vocabulary continuity with `builtin` lifecycle: one shared chip set, one verb-help string, one gardener-advice phrase across `renderer_kind=builtin` and `renderer_kind=htmx`. |
| S7  | `gb widget scaffold-htmx --from <slug>` — new slug or new version? | **New slug** (versions are evolved-in-place, forks are new slugs) | Versions reserved for in-place evolution; "I want one like this but different" is a fresh slug. C5 §12.        |

**DEC-S6 resolved (2026-05-09).** Operator @matthew.baker picked
`widget-stage:stable` over `widget-stage:published` via Slack. Rationale:
vocabulary continuity with the existing `builtin` lifecycle. Phase-3
implementation MUST use `widget-stage:stable` everywhere this runbook
references the promoted stage — `gb widget publish` verb output, the
operator-promotion page, gardener advice text, and the `gb widget similar`
default filter. The verb name (`gb widget publish`) is unchanged; only
the resulting label vocabulary moves.

The other six are filed as documentation positions in this runbook;
implementers (Phase 3 epic) can revisit any of them with a follow-up
decision if they hit friction.

---

## 8. Phase ladder hand-off

Per ka-0000006879:

- **Phase 1 — Research.** Closes when this synthesis lands and DEC-S6
  resolves. **DONE** — DEC-S6 (ka-0000006220) resolved 2026-05-09:
  `widget-stage:stable`.
- **Phase 2 — Design synthesis.** *This document.* DEC-S6 filed,
  resolved, and the runbook updated to match.
- **Phase 3 — Implementation.** Feature-branch + MR per slice; one crew
  per slice; cap 3 concurrent. Slice ordering recommendation:

  1. `internal/widgets/policy.go` + `render.go` (the trust-boundary core).
  2. kbeads pre-write hook (`widgets_validate.go`).
  3. `widget-type` field additions (`renderer_kind`, `template_source`,
     `template_engine`).
  4. `gb widget scaffold-htmx` + `lint` + `preview` (the inner
     authoring loop; agent unblocks here).
  5. `gb widget validate` + `publish` + `deprecate` (the gate).
  6. `/htmx/widget-types/*` preview / diagnostics routes.
  7. `/widget-types/{slug}` tabbed show + index.
  8. Rich-report stage-aware chips + per-render `recover()`.
  9. `/htmx/*` action / panel / api / mezmo routes
     (ka-0000007136 deliverable).
  10. Migration of one Phase-4 widget (ka-0000007457).

  Each slice ships its own free-htmx widget on ka-0000003788 once
  slice 4 is live (the dogfooding mandate flips from "today's widgets"
  to "post-port widgets").

- **Phase 4 — Ship.** Live in prod, ≥ 1 free-htmx widget rendering on a
  real rich-report bead, follow-up tracker for migration backlog.

---

## 9. TL;DR for the gardener dispatch prompt

If a crew session reads only one line of this doc, this is it:

> **`gb widget similar` → `scaffold-htmx --out` → `lint` → `preview` →
> `--commit` → `validate --strict` → `emit --draft-ok` on ka-0000003788 →
> `gb squawk` to operator → operator `gb widget publish`.**
> Use URL helpers, never literal `/htmx/...`. Close the bead with the §4.3
> template. Dogfood every artifact.

---

## Linkbacks

- Parent epic: ka-0000006879
- Gardener rich-report (dogfood surface): ka-0000003788
- Gardener schedule: ka-0000005515
- Wire shape: ka-0000000776 → `free-htmx-widget-wire-shape.md`
- Sandbox / CSP / sanitizer: ka-0000000873 → `free-htmx-widget-security.md`
- Backend contract: ka-0000007136 → `free-htmx-widget-backend-contract.md`
- Operator UX: ka-0000002102 → `free-htmx-widget-authoring-ux.md`
- Migration plan (in flight): ka-0000007457
- Prior synthesis: ka-0000001930 (predefined-widget substrate)
