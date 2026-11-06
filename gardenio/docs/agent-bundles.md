# Agent artifact bundles

A **bundle** aggregates the long-form deliverables an agent produces for a
task — plans, post-mortems, specs, reports, decision artifacts, committed
docs — into a single navigable reading surface at `/bundle/<bundle-id>` in
beads-viewer. Bundles are the answer to "what did this agent actually
produce for this epic?".

- **Plan-of-record:** [`plans/agent-artifact-bundles-multi-doc-viewer.md`](../plans/agent-artifact-bundles-multi-doc-viewer.md)
- **Tracking epic:** `ka-0000008153`
- **Closes the loop on advices:** `ka-0000000831` (file deps so reports stay
  attached) and `ka-0000008096` (beads is the doc store; link, don't
  duplicate).

## When to use a bundle

Use a bundle when you produce a **long-form artifact** (or several) that an
operator or another agent will want to read end-to-end:

- A plan or design doc.
- A post-mortem or incident write-up.
- A spec, ADR, or research note.
- A multi-doc deliverable (e.g. plan + checklist + diff summary).
- Any decision-report content longer than a few paragraphs.

Don't use a bundle for:

- A short progress update — use `gb squawk`.
- A one-line follow-up on a bead — use `kd comment add <id>`.
- A pure status check — use the bead's existing description / activity.

| Surface          | When to use                                                      |
|------------------|------------------------------------------------------------------|
| `gb squawk`      | Progress updates, milestones, findings the operator wants to see |
| `kd comment`     | Short notes tied to a specific bead                              |
| Bundle           | Long-form artifacts (plan, post-mortem, spec, multi-doc)         |

## Backing model

A bundle is a `type=bundle` bead parented to the originating task via a
`parent-child` typed mention. Each document inside the bundle is a
`type=doc` bead linked under the bundle the same way:

```text
origin task bead  ──parent-child──>  bundle bead (type=bundle)
                                          │
                                          ├── parent-child ── doc bead 1 (type=doc, format=md)
                                          ├── parent-child ── doc bead 2 (type=doc, format=diff)
                                          └── parent-child ── report bead (type=report)
```

Both `type=doc` and `type=report` beads can sit under a bundle. `gb decision
report` produces `type=report` beads; `gb bundle add --doc` produces
`type=doc` beads. The viewer treats them identically.

### Bundle fields (`type=bundle`)

| Field             | Purpose                                                             |
|-------------------|---------------------------------------------------------------------|
| `fields.kind`     | `plan`, `post-mortem`, `spec`, `report`, or `other`                 |
| `fields.headline` | Optional one-line headline for the bundle                           |
| `fields.summary`  | Optional summary paragraph                                          |

### Doc fields (`type=doc`)

| Field                | Purpose                                                            |
|----------------------|--------------------------------------------------------------------|
| `fields.format`      | One of `md`, `diff`, `csv`, `code`, `image`, `link`                |
| `fields.content`     | Inline payload (string). Soft cap: **256 KiB**.                    |
| `fields.content_url` | Pointer to S3 / external host. Required for payloads over 256 KiB. |
| `fields.headline`    | Display title shown in the TOC; falls back to bead title           |
| `fields.position`    | Integer for TOC ordering (lower renders first)                     |
| `fields.language`    | Language hint for `code` / `diff` syntax highlighting              |

## Authoring API

Two paths to populate a bundle. Both produce the same shape — pick whichever
fits the workflow.

### Path A: implicit auto-collect (`gb decision report`)

When the env var `GARDENIO_BUNDLE_AUTOCOLLECT=1` is set, `gb decision report`
will:

1. Find the originating task bead for the decision (the bead the agent is
   working).
2. Look for an existing `type=bundle` parent-child child of that bead.
3. If none exists, create one (`title="<origin title> — Bundle",
   kind=report"`) and link it under the origin.
4. Link the new `type=report` artifact under that bundle.
5. Print the `/bundle/<id>` URL alongside the report URL.

This is the lowest-friction path: every agent that submits a decision report
on a task ends up with a bundle assembled for free. The flag is **off by
default** during the rollout window.

### Path B: explicit `gb bundle` CLI

Use the explicit CLI when:

- The artifact is a committed file in the repo (e.g. a plan under
  `gardenio/plans/`) that you want promoted into a bundle.
- You want curated TOC ordering (`gb bundle order`).
- You want a bundle with a custom title / kind, or multiple bundles per
  origin (e.g. one `plan` and one `post-mortem`).

```bash
# Create a bundle parented to an origin task.
gb bundle create <origin-bead-id> --title "Auth refactor deliverables" --kind plan

# Add a committed doc by path (creates a type=doc bead from the file).
gb bundle add <bundle-id> --doc gardenio/plans/auth-refactor.md

# Add a doc by format + URL (for images / large payloads / external links).
gb bundle add <bundle-id> --content-url https://… --format image --headline "Diagram"

# Link an existing bead under the bundle (e.g. a type=report bead).
gb bundle add <bundle-id> <doc-bead-id>

# Rewrite an existing doc's body IN PLACE (converge — no new doc bead).
# Use this, NOT `add --doc`, to refresh a doc that is already in the bundle:
# `add` always creates a new bead, so re-adding an updated file forks a
# duplicate into the TOC. Alias: `gb bundle edit`.
gb bundle update <doc-id> --doc gardenio/plans/auth-refactor.md
gb bundle update <doc-id> --doc plan.md --headline "Status: phase 3"

# Set TOC position (lower numbers render first).
gb bundle order <bundle-id> <doc-id> 10

# Print the TOC and viewer URL.
gb bundle show <bundle-id>

# Read a doc body from the terminal (a doc id is globally unique — no bundle id
# needed). This is the first-class alternative to the raw jq fallback
# `kd show <doc-id> --json | jq -r .fields.content`.
gb bundle doc <doc-id>
gb bundle doc <doc-id> --json    # {id,title,format,headline,content,content_url}
```

Flag reference (subset; run `gb bundle <subcommand> --help` for the full
list):

| Flag             | `create` | `add` | `update` | Purpose                                   |
|------------------|----------|-------|----------|-------------------------------------------|
| `--title`        | required | optional | optional | Bundle title / override doc title    |
| `--kind`         | optional | —     | —        | `plan`, `post-mortem`, `spec`, `report`, `other` |
| `--headline`     | optional | optional | optional | One-line headline (TOC label for docs) |
| `--summary`      | optional | —     | —        | Paragraph summary on the bundle           |
| `--doc <path>`   | —        | optional | optional | Read file → doc body (`add` creates, `update` replaces) |
| `--content-url`  | —        | optional | optional | URL to external bytes (>256 KiB or binary) |
| `--format`       | —        | optional | optional | `md`, `diff`, `csv`, `code`, `image`, `link` |
| `--language`     | —        | optional | optional | Hint for `code` / `diff` highlighting  |
| `--position`     | —        | optional | —        | Integer TOC position (use `gb bundle order` to change it) |

`--format` is inferred from the file extension when `--doc` is used (`.md`,
`.diff`/`.patch`, `.csv`, image extensions, common code extensions) — except on
`update`, which keeps the doc's **current** format unless `--format` is passed,
so a differently-suffixed file cannot silently reclassify an existing doc.

`update` is the converge verb, and it is idempotent: re-pushing a file whose
contents already match reports `already matches — nothing written` and issues no
PATCH, so a per-cycle gardener loop does not spam the activity feed. Switching a
doc between inline and out-of-band clears the field it leaves behind, so a doc
never carries both a stale body and a new URL.

### Attaching images (`gb image upload`)

Local screenshots and diagrams live on the agent's filesystem, not in the
repo, so `gb bundle add --doc <path>` cannot reach them directly. Use `gb
image upload` to push the file to the same S3 bucket the viewer's image
whitelist covers, then either embed the printed URL in a markdown body or
link it as a `format=image` doc on the bundle.

```bash
# 1. Bare upload — prints the public URL on stdout. Use the URL inline in
#    a bundle's markdown doc, a decision report, a kd comment, etc.
gb image upload screenshot.png
# → https://<bucket>.s3.<region>.amazonaws.com/.../images/2026-05-15/<sha8>-screenshot.png

# 2. One-shot: upload + create a type=doc bead under a bundle.
gb image upload screenshot.png --bundle ka-Bundle789 --headline "Login flow"
# → URL printed on stdout; "Doc ka-DocXYZ added to bundle ka-Bundle789" on stderr.

# 3. Scope under a specific bead (key path uses the bead id instead of today's date).
gb image upload diagram.svg --bead ka-Origin123
```

Notes:

- Idempotent: the key embeds the first 8 hex chars of the file's SHA-256,
  so re-uploading identical bytes overwrites the same object.
- Default cap is 10 MiB. Raise with `--max-bytes` only when you mean it —
  large media belongs in a bundle doc, not inline.
- Content-Type is sniffed from the extension; override with
  `--content-type` for unusual binaries.
- Inline `gb squawk` messages should not embed large images. Upload, then
  reference the URL.

### Worked example: report → committed plan, same bundle

The common case: an agent submits a decision report (auto-collected into a
bundle) and then promotes a committed plan file into the same bundle.

```bash
# 1. The agent has been working on origin task ka-Origin123 and resolves a
#    decision via gb decision report. With GARDENIO_BUNDLE_AUTOCOLLECT=1, the
#    report bead is auto-linked under a freshly-created bundle.
gb decision report <decision-id> --content "$(cat report.md)"
# → Report ka-Report456 created.
#   /bundle/ka-Bundle789

# 2. The agent committed its plan under gardenio/plans/auth-refactor.md.
#    Promote it into the same bundle so a reader sees report + plan together.
gb bundle add ka-Bundle789 --doc gardenio/plans/auth-refactor.md \
    --headline "Auth refactor — plan-of-record" --position 10
# → Doc ka-Doc012 added to bundle ka-Bundle789 (format=md)
#   /bundle/ka-Bundle789#doc-ka-Doc012

# 3. (Optional) curate TOC order so the plan appears before the report.
gb bundle order ka-Bundle789 ka-Report456 20
gb bundle show   ka-Bundle789
```

## Viewer

### Routes

| URL                                | What it renders                                                          |
|------------------------------------|--------------------------------------------------------------------------|
| `/bundle/<bundle-id>`              | Canonical immersive multi-doc reading surface                            |
| `/bundle/<bundle-id>#doc-<doc-id>` | Per-doc deep link (scrolls + highlights)                                 |
| `/<bundle-id>`                     | Bead-show page with `type=bundle` view-model: TOC summary + Open-as-document CTA |
| `/<doc-id>`                        | Generic bead-show on the underlying `type=doc` / `type=report` bead      |

The viewer lives in the htmx server at
[`gardenio/controller/cmd/beads-viewer/`](../controller/cmd/beads-viewer/),
**not** the deprecated `beads3d/` 3D-graph project. The route handler is
`handleBundleShow` (`server.go`); the per-page view-model is `bundleView`
(`bundle.go`); the `type=bundle` bead-show partial is
`templates/partials/type_bundle.html`.

### Stable permalink shape

`/bundle/<bundle-id>` is the **canonical, Slack-shareable URL** for the
bundle. It is stable for the lifetime of the bundle bead.

When emitting permalinks (`gb bundle …` already does this) the URL is
absolute when `BEADS_VIEWER_URL` is set in the agent's environment, and
relative (`/bundle/<id>`) otherwise — so logs and tests don't depend on
host config.

### Format renderers

Format dispatch is a per-format renderer registered via
`registerBundleRenderer(format, renderer)`. Unknown formats degrade to a
placeholder card. Adding a new renderer is a one-file change.

| Format  | Renderer                                                            |
|---------|---------------------------------------------------------------------|
| `md`    | goldmark (same path as bead descriptions); linkified                |
| `diff`  | `code_highlighter.go` Prism-style classes; `diff_parser.go`         |
| `csv`   | `<table>` with header row; size-capped, raw `<pre>` beyond cap      |
| `code`  | `code_highlighter.go`, language-keyed                               |
| `image` | `<img>` with whitelisted hosts only (S3 + gardenio origin)           |
| `link`  | Card with title + URL; no embed (good for MR descriptions, Confluence) |

## Relationship to the dependency-hygiene advices

Bundles satisfy the two advices that govern artifact discoverability:

- **`ka-0000000831`** — every report a session produces must be linked back
  into the dependency tree of its originating task. Bundles enforce this
  structurally: doc beads parent-child to the bundle; the bundle parent-child
  to the origin task. Reports filed via `gb decision report` (auto-collect)
  or via `gb bundle add` are deps-linked by construction. Nothing the agent
  produces ends up orphaned.
- **`ka-0000008096`** — beads is the primary source of record for reports
  and documentation; new long-form artifacts should land as beads, not as
  uncommitted markdown scattered around the repo. Bundles are the
  ergonomic answer: instead of writing `docs/reports/<thing>.md` and hoping
  someone finds it, run `gb bundle add --doc <path>` and the doc is
  promoted into a `type=doc` bead linked under a bundle on the origin
  task — discoverable from the bead graph and renderable in beads-viewer.

The sequencer epic (`ka-0000005836`) consumes the **"this bead has a
bundle"** predicate so a list of epics can show a green badge for "has
deliverable". The predicate is defined as: a `parent-child` child of the
bead with `type='bundle'` exists. T1 of the bundles epic exposes this on
`beadsapi.BeadDetail.HasBundle`.

## Out of scope

- **Backfill of historical reports.** Existing `type=report` beads created
  before bundles shipped are not retroactively assembled into bundles.
  Deferred — possibly a one-shot script later if the operator wants
  historical bundles.
- **Auto-propagating edits from a committed source file.** Once a doc is
  added to a bundle, later edits to the source file in the repo do not
  propagate on their own — nothing watches the file. Re-run `gb bundle
  update <doc-id> --doc <path>` to push the new contents onto the existing
  doc bead (that verb closed ka-0000003568; the earlier advice here to
  re-run `add --doc` was what forked duplicate docs into the TOC).
- **iOS parity.** The iOS app has separate surfaces; bundle support there
  is a separate epic.

## See also

- [`plans/agent-artifact-bundles-multi-doc-viewer.md`](../plans/agent-artifact-bundles-multi-doc-viewer.md) —
  research + design plan-of-record, including the design-space exploration,
  open questions, and per-task acceptance criteria.
- [`docs/beads-viewer-dev.md`](beads-viewer-dev.md) — local dev loop for the
  htmx beads-viewer.
- Advice `ka-0000000831`, `ka-0000008096` — dependency hygiene + beads as
  the doc store.
