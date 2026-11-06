# Plan: agent artifact bundles + multi-document viewer in beads-viewer

**Status:** Research / scoping — pending operator approval to unfreeze epic.
**Tracking bead:** `ka-0000003253` (this scoping task)
**Epic:** `ka-0000008153` — Agent artifact bundles + multi-document viewer in beads-viewer (on ice).
**Sibling epics (informational, do not block):**
- `ka-0000003558` — Project view with epic sequencer.
- `ka-0000005836` — Sequencer ↔ report linking + decision-block visibility (depends on this epic for "what counts as a report").
**Inputs:**
- Epic description on `ka-0000008153` (Matt Baker, 2026-05-04 Slack thread).
- Beads-viewer code at `gardenio/controller/cmd/beads-viewer/`.
- Existing `gb decision report` flow at `gardenio/controller/cmd/gb/decision.go:743-872`.
- `richReportView` rendering at `gardenio/controller/cmd/beads-viewer/rich_report.go`.
- Advices `ka-0000000831` (file deps so reports stay attached) and `ka-0000008096` (beads is the doc store; link, don't duplicate).

> **Scope of this document:** research + design recommendations. Implementation
> is out of scope and explicitly deferred to the child tasks filed under the epic.

## 1. Problem statement

A reader (operator or downstream agent) trying to understand "what did this
agent actually produce for this epic" today has to stitch together:

- The bead description and comments.
- One or more `type=report` beads created by `gb decision report` (linked
  parent-child to the originating decision bead, content stored in
  `fields.content`).
- Markdown docs committed under `gardenio/docs/` or `gardenio/plans/`.
- Squawks (Slack messages, not first-class artifacts).
- Diff summaries posted to MRs.
- Plans-of-record on epic descriptions.

The artifact graph already exists in beads — `bead_references` typed mentions
and parent-child deps are the substrate — but there is no **named collection**
that says "these N artifacts together are the deliverable for bead X" and no
**reading surface** that renders them as a single multi-document experience.

The `type=rich-report` bead and its `richReportView` are the closest
precedent: a parent bead with parent-child `widget-instance` children, sorted
by `position`, rendered as one page. Bundles generalize that pattern away
from the Incident-Intel-specific widget registry to arbitrary documents.

## 2. Current state

### 2.1 Existing report shape (post `gb decision report`)

`gardenio/controller/cmd/gb/decision.go:816-848` creates a bead with:

| Field            | Value                                    |
|------------------|------------------------------------------|
| `type`           | `report`                                 |
| `kind`           | `data`                                   |
| `fields.decision_id`   | The originating decision bead's ID |
| `fields.report_type`   | `report` / `plan` / `checklist` / `diff-summary` / `epic` / `bug` / `tldr` (from `validArtifactTypes`) |
| `fields.content`       | Inline markdown payload (string)   |
| `fields.format`        | Default `markdown`                  |

Then `AddDependency(reportID, decisionID, "parent-child", actor)` links it,
the report bead is closed immediately, and the decision gate is satisfied.

**Implication:** every decision-report artifact already lives as a queryable
bead with a parent-child edge to its origin. Bundle work should build *on*
this, not replace it.

### 2.2 Existing viewer routing + type dispatch

`gardenio/controller/cmd/beads-viewer/server.go`:

- Routes registered in `registerRoutes` (line 540+). Top-level pages live at
  `/{path}`, individual beads at `/{id}` (line 669, `handleBeadShow`).
- Per-type view-model dispatch in `injectTypeView` (line 1525). Today
  `decision`, `schedule`, `formula`, `advice`, `rating`, and `rich-report`
  have specialised views; other types fall through to the generic field
  dump.
- Templates: per-type partials under `templates/partials/type_*.html`.
  Page-level templates registered in `pageNames` (line 415).

### 2.3 Existing rich-report aggregation

`rich_report.go::buildRichReportView`:

1. Reads bundle-level fields from the parent bead (`report_kind`, `headline`,
   `severity`, `source_ref`).
2. Walks `MentionedBy` references, filters to `type=widget-instance` *and*
   `relationship_type=parent-child`.
3. Per child: fetches the child bead, parses `widget_type` / `headline` /
   `position` / `data` fields, dispatches to a registered renderer, falls
   back to a `<pre>` JSON dump.
4. Sorts by `position`.
5. Computes a back-link to the first parent-child *forward* mention.

**This is the pattern to clone for bundles.** The only structurally
unfamiliar piece is the per-child renderer dispatch by document **format**
(md / diff / csv / image) instead of by widget slug.

### 2.4 Existing typed-mention graph

`kbeads/internal/references/typed.go` ships closed-core verbs (`parent`,
`child-of`, `blocks`, `blocked-by`, `related`, `duplicates`,
`duplicated-by`, `supersedes`, `superseded-by`) plus untyped fallback. Bundle
membership can ride on this substrate without a schema change.

## 3. Design space

Three axes need a decision: **backing model**, **authoring API**, and
**viewer surface**. Permalinks, formats, and shareability fall out from those.

### 3.1 Backing model

| Option | Sketch | Pros | Cons |
|---|---|---|---|
| **A. New `type=bundle` bead** | A first-class bead per bundle. Members linked via parent-child typed mention `parent(ka-bundle)`. Bundle bead carries title, description, maybe `fields.layout` (toc-order, format hints). | Mirrors `rich-report`. Queryable as a list (`kd list --type bundle`). Permalink falls out (`/{bundle-id}`). Cleanly addressable from sequencer (`ka-0000005836` predicate "has bundle"). Single source of truth for ordering/title/description. Composes with existing parent-child plumbing. | New type to plumb: `kd` filters, viewer dispatch, advice, prime hints. Migration story for existing reports (do we adopt them retroactively?). |
| **B. Labeled artifact set** | No bundle bead. Artifacts wear a label like `bundle:ka-XYZ` where `ka-XYZ` is the origin bead. The viewer page for `ka-XYZ` grows a "Bundle" tab that lists everything carrying that label. | Zero new types. Cheapest to ship a v0. | No first-class title/description/order for the bundle as a whole. Permalink piggybacks on origin bead → ambiguous when one bead has multiple bundles (e.g. plan + post-mortem). Order requires either label conventions (`bundle:X:order:7`) or another field. Sequencer "has bundle" predicate is "any artifact has label = bundle:<this-id>" — works but ugly. |
| **C. Content-addressable doc set** | Bundle is a manifest blob (CAS-addressed) containing references to docs (each doc is itself either a bead or a path). Stored on a bead via field. | Immutable snapshots, useful for releases. | Heavy machinery for a problem that is fundamentally graph-shaped. No existing CAS layer in beads. Hard to evolve a bundle (a bundle should be append-only by an agent during a task; a CAS manifest forces a new ID per change). |

**Recommendation: Option A — new `type=bundle` bead, members linked by
parent-child typed mention.**

Rationale: the rich-report precedent already proved this shape works in
beads-viewer; the typed-mention graph is the right primitive; the sibling
epic `ka-0000005836` needs a stable predicate for "this bead has a report"
and the cleanest version of that predicate is "this bead has a child of
`type=bundle`". Option B is cheaper for a v0 demo but creates ambiguity
that the sibling epic would have to clean up later. Option C is over-built.

Where Option A leaves room for a fast v0: the bundle bead's first
implementation can be **created lazily by `gb decision report`** when an
agent submits its first report against a bead that does not yet have a
bundle. Subsequent reports auto-attach. This is invisible to the agent and
is functionally close to Option B's UX without paying Option B's cost.

### 3.2 Authoring API

| Option | Sketch | Pros | Cons |
|---|---|---|---|
| **I. Implicit auto-collect** | `gb decision report` creates/updates a `type=bundle` bead on the **origin task bead** (the one the agent is working) and links the report as a parent-child child of *the bundle*, not of the decision. Squawks of kind "doc"/"report" optionally auto-collect. | Zero new agent affordance. Existing crew nudge already requires reports for decisions; bundles fall out naturally. Compatible with advice `ka-0000000831` (deps file themselves). | Surprising for the operator: artifacts move from "child of decision" to "grandchild via bundle". Need a flag to opt out for one-shot reports. Hard to attach pre-existing committed docs (those live in git, not beads). |
| **II. Explicit `gb bundle add`** | New CLI: `gb bundle create <bead-id> --title "..."`, `gb bundle add <bundle-id> <artifact-bead-id>`, `gb bundle add <bundle-id> --doc <path>` (creates a `type=doc` bead from a path). | Crisp authoring. Lets agents build a curated reading order. Lets one-off committed docs be promoted into the bundle. | Yet another verb to teach. Adds friction; agents will forget. Requires advice + nudge for adoption. |
| **III. Hybrid (auto + explicit)** | Auto-collect for decision reports (the most common case). `gb bundle add` for everything else (committed docs, manual pulls). `gb bundle order` to set TOC order. | Captures the common path without new affordance, but lets agents curate. Consistent with how `gb squawk` / `kd comment` coexist with each other. | Two paths to the same shape — reader has to understand both to debug. Slightly more code. |

**Recommendation: Option III — hybrid auto + explicit.** Implicit collection
captures 80% of the value with zero agent burden; the explicit `gb bundle add`
verb is essential for promoting committed docs (e.g. plans under
`gardenio/plans/`) and for setting a curated reading order. The advice corpus
already nudges agents to prefer beads as the doc store
(`ka-0000008096`) — `gb bundle add --doc <path>` becomes the natural
one-step migration when an agent realises a doc should be in a bundle.

### 3.3 Viewer surface

Three sub-decisions: route, layout, supported formats.

#### 3.3.1 Route

| Option | URL | Pros | Cons |
|---|---|---|---|
| **a. Dedicated `/bundle/<id>`** | `/bundle/ka-XYZ` | Clean, Slack-shareable. No collision with `/{id}` (bundle beads have ids that already match `kd-…`). | Adds a new route; users may also reach the bundle by hitting `/{bundle-id}` and expecting the doc view. |
| **b. Type-dispatched on `/{id}`** | `/ka-XYZ` where `bead.type == "bundle"` renders the multi-doc view. Mirrors how `/ka-XYZ` for `type=rich-report` already specialises. | Zero new routing. Permalinks are just bead permalinks. Plays with `injectTypeView` exactly like rich-report. | The detail panels (deps, comments, activity) on the bead-show layout don't make as much sense on a "reading" surface — multi-doc bundles want a docs-first chrome. |
| **c. Both** | Bead-show on `/{id}` shows a TOC + summary + "Open as document" link to `/bundle/{id}` for the immersive view. | Familiar bead-show plumbing on the chrome side; immersive surface available when wanted. | Two URLs for the same content; Slack permalinks need a convention. |

**Recommendation: Option c — both, with `/bundle/<id>` as the canonical
shareable URL.** The bead-show page on `/{id}` should grow a `type=bundle`
view-model summary (TOC + first-doc preview) so the bundle bead remains
inspectable as a bead (deps, owner, status). The immersive document
experience lives at `/bundle/<id>` and is the URL agents hand to operators in
Slack. To enforce "Slack permalink = `/bundle/`", `gb decision report` and
`gb bundle add` print the `/bundle/` URL on success; the bridge’s
auto-posted bead summaries can grow a "Read as document →" link.

#### 3.3.2 Layout

A single-column sequenced reading view, with a left-rail TOC for jumping.
Each doc renders inside a card with its title, format badge, and a "view
raw" link to the underlying bead. Inline navigation between docs uses
anchors (`#doc-<bead-id>`).

For the v0, do **not** ship side-by-side reading. The Slack ask is "browsable
side-by-side or in sequence"; sequence is the natural default and the
substrate for any later split-pane.

#### 3.3.3 Supported formats

A **document** in a bundle is a bead with `type=doc` (or a `type=report`
adopted from `gb decision report`) carrying:

| Field | Purpose |
|---|---|
| `fields.content` | Inline payload (string) for `md`, `diff`, `csv`, `tldr`, `code`. |
| `fields.content_url` | Optional pointer to S3 / git for large or binary payloads (e.g. `image`). |
| `fields.format` | One of `md`, `diff`, `csv`, `code`, `image`, `link` (catch-all). |
| `fields.headline` | Display title; falls back to bead title. |
| `fields.position` | Integer for TOC ordering (matches rich-report `position` semantics). |
| `fields.language` | For `code` / `diff` syntax highlighting (Prism-style). |

Initial render dispatch (mirrors `rich_report.go::lookupWidget`):

| Format | Renderer |
|---|---|
| `md` | Existing `s.md.Convert` (goldmark) → linkified. Same path as bead descriptions. |
| `diff` | New `diffRenderer` reusing `code_highlighter.go`'s Prism-style classes; `diff_parser.go` already exists. |
| `csv` | New `csvRenderer` → `<table>` with the first row treated as headers; size-cap to a few thousand rows, beyond which we render the raw bytes in a `<pre>`. |
| `code` | Existing `code_highlighter.go`, language-keyed. |
| `image` | `<img src="{content_url}">` only when `content_url` resolves to a beads-served path (no arbitrary external fetches; see §5). |
| `link` | A card with title + URL, no embed; allows the bundle to point at MR descriptions, Confluence, etc. without inlining. |

Formats outside this list degrade to the generic `<pre>` placeholder.
Adding a renderer is a one-file change (`bundle_renderers.go` analog of
`widgets_phase4.go`).

### 3.4 Permalinks and Slack shareability

- Bundle URL: `/bundle/<bundle-bead-id>` is canonical and stable for the
  lifetime of the bundle bead.
- Per-doc deep link: `/bundle/<id>#doc-<doc-bead-id>` — fragments scroll +
  highlight, mirroring the typed-mention navigation in
  `plans/typed-mentions-graph-edges-and-cli.md`.
- The bridge (`internal/bridge/bot_decisions.go`) already auto-posts decision
  reports to Slack with their content. When a report enters a bundle, the
  Slack message should grow a "Read in context →" link to `/bundle/<id>`
  (sibling-epic `ka-0000005836` ships the badge; this epic is responsible for
  the URL being meaningful).

### 3.5 Relationship to the dependency graph

Per advice `ka-0000000831`, every artifact MUST have a typed dep edge back
to its origin. The bundle backing model preserves this:

```text
origin task bead  ──parent-child──>  bundle bead
                                          │
                                          ├── parent-child ── doc bead 1
                                          ├── parent-child ── doc bead 2
                                          └── parent-child ── doc bead 3
```

The constellation already renders parent-child as a solid edge; bundles fall
out as a visible cluster around the origin bead.

For `ka-0000005836`, the **canonical "this bead has a report" predicate**
becomes:

```
exists child of `bead.id` where child.type = 'bundle'
```

This is one indexed `bead_references` lookup, fast enough for a sequencer
that lists ~hundreds of epics.

## 4. Open questions to surface to the operator

These are forks where the recommendation in this doc is informed but the
operator should sign off before implementation begins. If the answer to any
of these flips, the affected child tasks below need rewriting. Filing each as
its own `gb decision create` is overkill — instead the operator's review of
this doc is the single decision point.

1. **Bundle creation timing.** Lazy on first `gb decision report` (default
   recommended) vs. eager on `gb yield` start vs. only via explicit
   `gb bundle create`. Recommendation: lazy, hidden behind a feature flag
   for the rollout window.
2. **Adoption of pre-existing reports.** Do we backfill — sweep existing
   `type=report` beads into bundles by their `decision_id` ancestry? Or
   leave them orphaned and only collect from cutover forward?
   Recommendation: do not backfill in v0; ship a one-shot script after the
   shape stabilises if the operator wants historical bundles.
3. **Document size ceiling.** What's the largest single doc payload we
   inline? `gb decision report` has no explicit cap today (the tldr path
   has 500 char default; full reports are unbounded). Recommendation: cap
   inline `fields.content` at 256 KB; over-cap docs MUST use `content_url`.
4. **Image hosting.** `image` format requires a place to serve bytes. Reuse
   the coop S3 prefix (already wired into the agent image) or stand up a
   beads-served path? Recommendation: reuse S3 with the same per-bead
   prefix coop already uses for transcripts; viewer fronts a signed-URL
   redirect at `/bundle/<id>/asset/<doc-id>`.
5. **Multi-bundle per origin.** Can one bead have N bundles (e.g. "Plan",
   "Post-mortem", "Spec") or is a bead capped at one? Recommendation: allow
   N — the bundle bead carries its own title/description and a `kind` field
   (`plan`, `post-mortem`, `spec`, …); the sequencer "has bundle" predicate
   is satisfied by any of them.

## 5. Security / safety

- **No arbitrary external fetches.** `image` and `link` doc formats accept
  URLs but the renderer must whitelist hosts; the v0 whitelist is the S3
  bucket coop is already configured for plus the gardenio origin. External
  links render as a card with the URL text, not as an `<img>` or fetched
  preview.
- **Content sanitisation.** Markdown goes through goldmark, which is
  already configured for safe HTML in `templates.go`. Diff, csv, and code
  renderers must HTML-escape before adding syntax-highlight spans (the
  existing `code_highlighter.go` already does this).
- **Authorisation.** Bundles inherit the bead's auth. Today `beads-viewer`
  applies OIDC middleware globally; `/bundle/<id>` reuses the same
  middleware. No bundle-level ACL is in scope for v0.
- **CSP / unsafe-inline.** Per `gardenio/docs/csp-unsafe-inline-deferral.md`,
  inline styles are temporarily allowed; bundle renderers should not bake
  in inline `<script>` and should keep CSS in the existing stylesheet. Code
  highlighting reuses the existing class-based approach.

## 6. Acceptance criteria summary

A v0 bundle implementation is **done** when:

- [ ] A `type=bundle` bead can be created via `gb bundle create <origin-id>`
      and rendered at `/bundle/<id>`.
- [ ] `gb decision report` lazily creates / appends to a bundle on its origin
      task bead and prints the `/bundle/<id>` URL.
- [ ] `gb bundle add <bundle-id> --doc <path>` creates a `type=doc` bead from
      a markdown file under the repo and links it parent-child to the bundle.
- [ ] The viewer renders `md`, `diff`, `csv`, `code`, and `image` (S3-hosted)
      doc formats with TOC; unknown formats degrade to a placeholder.
- [ ] Per-doc deep links (`/bundle/<id>#doc-<doc-id>`) resolve and scroll.
- [ ] Bead-show on `/{bundle-id}` carries a `type=bundle` view-model with TOC
      preview and "Open as document" link.
- [ ] The `"this bead has a bundle"` predicate is exposed on the bead detail
      payload returned by `beadsapi.GetBead` (a single bool field) so the
      sequencer epic can consume it without a graph walk.
- [ ] Tests: a `bundle_test.go` covering bundle creation, add, render, and
      empty-state paths; renderer unit tests for each supported format;
      htmx test exercising TOC fragment navigation.

## 7. Implementable child tasks (proposed)

Each task below is single-purpose, ordered, and parented to `ka-0000008153`.
T2 depends on T1 (data model first). T3 depends on T1 (CLI needs the type
shape). T4 depends on T1+T3 (auto-collect needs the CLI plumbing). T5
depends on T2 (render needs the route). T6 depends on T5 (deep links need
the renderer). T7 is parallel and depends only on T1. T8 is the
documentation/advice gate and depends on T1–T6.

| # | Title | Acceptance criteria |
|---|---|---|
| **T1** | Define `type=bundle` and `type=doc` schemas + daemon-side helpers | New types accepted by the daemon; field-shape documented; `beadsapi.BeadDetail` exposes a `HasBundle` boolean for bead-show callers. Includes migration / fields validation. |
| **T2** | Add `/bundle/<id>` route + `bundleView` skeleton + `templates/bundle.html` | New page registered in `pageNames`; route resolves; empty-state renders. Mirrors `rich_report.go` shape but with format dispatch hook. Excludes per-format renderers. |
| **T3** | `gb bundle` CLI: `create`, `add`, `add --doc <path>`, `order`, `show` | New cobra subcommand under `gb`. Creates / mutates bundle and doc beads via daemon API. Prints `/bundle/<id>` URL. Tests under `cmd/gb/`. |
| **T4** | Auto-collect: `gb decision report` lazily creates / appends to a bundle on origin bead | Behind `GARDENIO_BUNDLE_AUTOCOLLECT=1` flag for the rollout window. Existing flow remains intact when flag unset. Test: a decision report against a bead with no bundle yields a new bundle; a second report against the same bead appends. |
| **T5** | Per-format renderers: `md`, `diff`, `csv`, `code`, `image`, `link` | One renderer per format wired into the bundle dispatch. Reuses existing goldmark, `code_highlighter.go`, `diff_parser.go`. Image renderer accepts only whitelisted hosts. Per-format tests. |
| **T6** | TOC + per-doc deep-link navigation in `/bundle/<id>` | Left-rail TOC renders ordered docs; clicking scrolls + highlights the target. Mirrors typed-mention navigation pattern. Browser-level htmx test. |
| **T7** | Bead-show `type=bundle` view-model: TOC summary + "Open as document" CTA | New `injectTypeView` case for `bundle`; `templates/partials/type_bundle.html`; passes the existing bead-show test fixture. |
| **T8** | Documentation + advice: update `gardenio/docs/`, file global advice for bundle authoring | New `gardenio/docs/agent-bundles.md` covering CLI + URL conventions. New advice bead (under-200 char hook) directing agents to `gb bundle add` after producing a long-form artifact. Closes the loop on advice `ka-0000000831` / `ka-0000008096`. |

A **stretch task T9** ("backfill historical reports into bundles") is
**explicitly deferred** pending operator response to Open Question §4.2.

## 8. Coordination with sibling epics

- **`ka-0000005836` (sequencer ↔ report linking).** This epic is the
  upstream provider of the "has bundle" predicate that `ka-0000005836` will
  consume. Concrete contract: `BeadDetail.HasBundle bool` and a SQL helper
  that does the indexed lookup. Surfaced in T1 acceptance.
- **`ka-0000003558` (project view with sequencer).** This epic does not
  block the project view, but `/bundle/<id>` URLs become the natural target
  for "click an epic with a green report indicator". No code coupling at
  the bundle-epic layer; alignment is via shared URL conventions.

This doc references both sibling epics by ID so each scoping crew picks up
the linkage automatically (see advice `ka-0000007977` on typed-mention
hygiene).

## 9. What this doc does NOT decide

- Visual design of `/bundle/<id>` beyond the layout sketch in §3.3.2 — UX
  iteration belongs in T2 / T6.
- Concrete renderer implementations — left to T5.
- Whether to retire the standalone `gb decision report` content blob in
  favour of always going through a bundle — out of scope, will revisit when
  v0 is stable.
- iOS parity (the iOS app has its own surfaces; bundle support there is a
  separate epic if the operator wants it).
