# Plan: surface typed mentions in beads-viewer graph + kd CLI + advice

**Status:** Plan-of-record — pending operator approval
**Tracking bead:** ka-0000002612 (umbrella)
**Epics:**
- E1 — beads-viewer constellation render: [`ka-0000004349`](https://beads/ka-0000004349)
- E2 — clickable edges + inline highlight: [`ka-0000000608`](https://beads/ka-0000000608) (blocked-by E1)
- E3 — kd CLI typed-mention parity: [`ka-0000001544`](https://beads/ka-0000001544)
- E4 — global advice: [`ka-0000007842`](https://beads/ka-0000007842)
**Builds on:**
- Closed implementation epic [`ka-0000003840`](https://beads/ka-0000003840) — typed-mention syntax (`parent(ka-X)` and `parent: ka-X` forms) with `bead_references.relationship_type` column, parser at `kbeads/internal/references/typed.go`, write-path in `kbeads/internal/store/postgres/queries_references.go`, HTTP endpoints `/v1/beads/{id}/mentions` + `/mentioned-by` with `?type=<verb>` filter.
- Research epic [`ka-0000001333`](https://beads/ka-0000001333) — typed mentions design.

## Slack ask (Matt Baker, 2026-05-03)

> "now that we have typed mentions we should enable showing graph edges for mentions in the beads viewer constellation, and even make it so when you click on the graph edges it brings you to the bead mention in the page and highlights it. We should also add advice globally on how to use typed mentions aggressively, and ensure that the usual kd commands all support them first class for searching and listing and such."

## Goal

Make typed mentions a fully first-class citizen of the beads experience:
1. **Visible** as graph edges in the beads-viewer constellation, distinguishable from explicit deps and from each other by verb.
2. **Navigable** — clicking an edge takes you to the source bead's page and highlights the exact text where the mention appears.
3. **Queryable** from the CLI — `kd list --mentions`, `kd show` mention summaries, gRPC parity.
4. **Promoted** — a global advice bead teaches every agent to use typed mentions aggressively when authoring bead text.

## Current state (research summary)

**Server side** (kbeads) is **complete** for read access:
- Schema: `bead_references(source_bead_id, target_bead_id, source_field, relationship_type)` with unique index over the four-tuple. Migration 013.
- Parser: `internal/references/typed.go::ExtractTyped` — closed-core verbs `parent`, `child-of`, `blocks`, `blocked-by`, `related`, `duplicates`, `duplicated-by`, `supersedes`, `superseded-by`. Unknown verbs degrade to untyped (NULL relationship_type) with `RawVerb` captured.
- Extraction triggers: bead CREATE/UPDATE on `title`/`description`/`notes`; comments on insert (gated by `BEADS_REFERENCES_EXTRACT_COMMENTS`).
- HTTP: `GET /v1/beads/{id}/mentions?type=<verb>` and `/mentioned-by?type=<verb>`. `?type=` empty → untyped only; `?type=blocks` → exact match.
- Graph emission: `/v1/graph` edges carry `Type=DepType=<verb>` for typed, `"mentions"` for untyped, with `Metadata["source"]="text"`.

**Beads-viewer** today (`gardenio/controller/cmd/beads-viewer/`):
- Server-rendered Go + htmx + vanilla JS; SVG constellation built server-side in `constellation.go::buildConstellation`.
- Already fetches mentions: `daemon.GetMentions(...)` + `daemon.GetMentionedBy(...)` in `handleBeadShow`.
- Already merges *typed* mentions into the deps view via `mergeTypedMentionsIntoDeps` (line 473-509) so they render identically to explicit deps. **Untyped mentions are dropped.**
- Edges: `<line>` SVG elements, no click handlers, no edge identity (no source-bead linkage on the visual edge).
- Inline mention rendering: `linkify.go::LinkifyHTML` wraps bare `kd-…` in `<a href="/ka-XXX">ka-XXX</a>` — **no `id`/`data-` attribute**, so we have no scroll/highlight target.

**kd CLI** today (`kbeads/cmd/kd/`):
- `kd refs mentioned-by/mentions <id> --type <verb>` — already shipped, full surface.
- `kd search [query] --mentions <id>` — already shipped.
- `kd list --mentions <id>` — **flag missing**, but `ListBeadsRequest.Mentions` field exists in client and HTTP wire (`internal/client/http.go:179-180`). Just needs flag binding.
- `kd show <id>` — prints deps + comments but **no mention summaries**.
- gRPC proto: `ListMentionedByRequest`/`ListMentionsRequest` lack `relationship_type` filter; `ListBeadsRequest` proto lacks `mentions` field. HTTP-only today.

**Advice corpus** today: no advice bead teaches mention syntax. Agents copy patterns from CLAUDE.md, plans, and other beads ad hoc.

## Design

Four epics, each scoped to one gardener. Run E1+E2 sequentially (E2 depends on E1's edge-data plumbing); E3 and E4 are independent.

### E1 — beads-viewer constellation: render typed-mention edges

**Gardener role:** crew (delivery)
**Repo:** gardenio monorepo
**Branches off:** main

**Scope:**
- Stop dropping untyped mentions in `mergeTypedMentionsIntoDeps`. Both typed and untyped mention edges should render in the constellation when the focal bead is the source or target.
- Keep typed mentions visually parity-with-deps (per closed decision `ka-0000000923`: solid, weighted), but add an edge **role/origin** distinction:
  - Explicit `deps` row → `role="dep"` (existing styling)
  - Typed mention (relationship_type set) → `role="dep-text"` — same color as the matching dep type, but with a subtle dash pattern (`stroke-dasharray="6 2"`) to mark "derived from text" provenance. Per `ka-0000000923` parity, *if* the operator wants strict identical rendering the dash can be elided behind a feature flag.
  - Untyped mention (relationship_type IS NULL) → `role="mention"` — new neutral color (gray `#7a7a7a`), opacity 0.55, dashed `"3 4"`.
- Update legend in `constellation.html` to add the two new roles and group them.
- Add a UI toggle (cookie-persisted) to hide untyped mentions for users who find them noisy.
- Surface the edge's verb on hover via SVG `<title>` (e.g. "blocks (text)").

**Deliverables:**
- `constellation.go` updated: edge struct gains `Origin` (`"dep"|"text"`) and `RelationshipType` (verb or empty); `buildConstellation` emits all three categories; `mergeTypedMentionsIntoDeps` is split into `mergeTypedMentionsAsDepEdges` and `appendUntypedMentionEdges`.
- `templates/partials/constellation.html`: new legend entries; `<line>`s carry `data-source-bead`, `data-target-bead`, `data-origin`, `data-relationship-type`, `<title>` tooltip.
- New CSS class on legend toggles; `localStorage.setItem("constellation:hide-untyped-mentions","1")` toggle in `templates/static/`.
- Tests:
  - `constellation_test.go`: new test cases for typed-text, untyped, and mixed sets.
  - Snapshot a known fixture's constellation HTML (golden file) so styling regressions are caught.
- Doc update: `docs/beads-viewer-constellation.md` (new) — short reference of edge roles and styling.

**Out of scope:**
- Edge click handling — owned by E2 (ka-0000000608).
- 3D constellation in beads3d — deprecated repo per CLAUDE.md.
- Backfill / re-extraction — already shipped (`kd admin reextract-references`).

**Acceptance:**
- Open `/ka-0000005983` (a real bead with mentions) and see distinct edge styles for explicit deps vs. typed mentions vs. untyped mentions. Hover shows the verb. Toggle hides untyped.
- `make build && make test` passes; visual regression caught by golden test.

### E2 — beads-viewer: clickable mention edges scroll/highlight inline mention

**Gardener role:** crew (delivery)
**Repo:** gardenio monorepo
**Branches off:** main
**Depends on:** E1 (needs `data-source-bead`/`data-target-bead`/`data-relationship-type` attributes on edges).

**Scope:**

**Inline anchors** in `linkify.go::LinkifyHTML`:
- For each bare `ka-XXX` matched, wrap as `<a href="/ka-XXX" id="mention-ka-XXX-N" data-bead-id="ka-XXX" class="mention">ka-XXX</a>` where `N` is a per-page running counter (so multiple mentions of the same target each get a unique anchor).
- For typed mentions specifically (matched by the typed regex from `internal/references/typed.go`), also include `data-relationship-type="<verb>"` on the anchor.
- Server-side this means the linkifier needs to either re-run `ExtractTyped` over the description, or the page handler needs to pass the parsed mention list down so anchors and edges agree on indices. Recommend the latter — fetch once via `daemon.GetMentions(id, all)` and let the renderer correlate by `(target_id, source_field, position)`.

**Edge click handler** (in `templates/static/constellation.js` — new file or extend existing inline script):
- On `<line>` click with `data-origin="text"`: read `data-source-bead`, navigate to `/<source-bead>?highlight=<target-bead>&rel=<verb>`.
- On `<line>` click with `data-origin="dep"`: keep existing behavior (no nav today; or open the dep edit dialog if desired — out of scope).

**Highlight behavior** (in `templates/bead_show.html` or a new `templates/static/highlight.js`):
- On page load, parse `?highlight=<id>&rel=<verb>` from URL.
- Find the first matching `<a class="mention" data-bead-id="<id>"[ data-relationship-type="<verb>"]>` in the description block. If `rel` is provided, prefer the typed match; otherwise fall back to first untyped or any match.
- Add CSS class `mention-highlight` for ~3 seconds (yellow flash → fade to subtle outline) then `scrollIntoView({block:"center"})`.
- If no match (mention may live in `notes` or a comment), scroll to and highlight the corresponding section heading and emit a console warn (for debugging).

**Deliverables:**
- `linkify.go` and `linkify_test.go` updated with anchor injection.
- New static file `templates/static/highlight.js` (~50 lines) and CSS additions for `.mention-highlight`.
- Edge click handler in `templates/static/constellation.js`.
- New test: serve a fixture bead, simulate click via Playwright (or Go-side: assert anchor is rendered + highlight URL form is correct).
- Doc update: append "Edge click → mention highlight" section to E1's `docs/beads-viewer-constellation.md`.

**Out of scope:**
- Highlighting mentions inside *comments*: comments load via htmx fragments and need a different anchor strategy. File as a follow-up bead.
- Highlighting mentions inside `notes`: same — follow-up.

**Acceptance:**
- Click a mention edge in the constellation, land on the source bead's page, see the relevant `ka-XXX` highlighted in the description for ~3 seconds and scrolled into view. Works for both typed and untyped mentions.
- Hashed URL is shareable and re-runs the highlight on reload.

### E3 — kd CLI: first-class typed-mention search/list/show + proto alignment

**Gardener role:** crew (delivery)
**Repo:** gardenio monorepo (kbeads/)
**Branches off:** main

**Scope:**

**Phase 1 — CLI flag closures (low risk, HTTP reuse):**
- Add `--mentions <id>` flag to `kd list` (already wired client→server via `ListBeadsRequest.Mentions`; just needs flag binding in `cmd/kd/list.go`).
- Add `--mentions-type <verb>` companion to `kd list` — server addition: extend `GET /v1/beads?mentions=<id>&mentions_type=<verb>` and the request struct + handler.
- Extend `kd show <id>` output to print mention summaries (grouped by verb, both directions). Format:
  ```
  Mentions:
    blocks: 2 (ka-AAA, ka-BBB)
    parent: 1 (ka-CCC)
    untyped: 4 (ka-DDD, ka-EEE, ...)
  Mentioned by:
    blocked-by: 1 (ka-XXX)
    untyped: 2 (ka-YYY, ka-ZZZ)
  ```
- Add `kd mention <id>` as a top-level alias for `kd refs mentions <id>` (matches Slack-ask phrasing "kd … mentions"; keeps `kd refs` for the precise crowd).

**Phase 2 — proto/gRPC parity:**
- Add `optional string mentions = N;` and `optional string mentions_type = N+1;` to `ListBeadsRequest` in `proto/beads/v1/beads.proto`.
- Add `optional string relationship_type = N;` to `ListMentionedByRequest` and `ListMentionsRequest`.
- Regenerate proto (`make proto`), wire `internal/client/grpc.go` to mirror the HTTP client.
- Update gRPC service handlers in `internal/server/grpc_*.go`.

**Phase 3 — output sugar:**
- Linkify mention IDs in `kd show` output when stdout is a TTY (use existing color/link facility if any; otherwise print plain). Out of scope: full markdown rendering.
- Add `--mentions-only` to `kd show` for "show me only the mentions section" (handy for piping).

**Deliverables:**
- `cmd/kd/list.go`, `cmd/kd/show.go`, `cmd/kd/output.go` updated; new `cmd/kd/mention.go` for the alias.
- `proto/beads/v1/beads.proto` and `service.proto` updated; generated code committed.
- `internal/client/http.go`, `internal/client/grpc.go`, `internal/server/http_beads.go`, `internal/server/grpc_*.go` updated to surface the filters.
- Tests:
  - `cmd/kd/list_test.go`: flag parsing + roundtrip with mock client.
  - `internal/server/http_beads_mentions_test.go`: query filter end-to-end.
  - `cmd/kd/show_test.go`: golden-file test of show output with mention summary.
- Doc update: refresh `kbeads/docs/cli.md` (or create `kbeads/docs/cli-mentions.md`) with the new surface.

**Acceptance:**
- `kd list --mentions ka-AAA --mentions-type=blocks` returns only beads whose text contains `blocks(ka-AAA)` or `blocks: ka-AAA`.
- `kd show ka-AAA` prints both mention sections grouped by verb.
- gRPC clients can issue the same filters as HTTP — `make test` covers both transports.
- `kd mention ka-AAA` works as an alias.

### E4 — Global advice: use typed mentions aggressively

**Gardener role:** gardener (this gardener or a small dedicated one)
**Repo:** beads only — no code change.

**Scope:**
- Author one global advice bead, scope label `global` (no `inline:true` — this is guidance, not policy).
- Title: **"Use typed mentions aggressively when authoring bead text"**
- Body (~250–350 words) covering:
  1. **What typed mentions are** — both syntactic forms (`parent(ka-X)` and `parent: ka-X`), the closed core verbs, what unknown verbs do.
  2. **When to use which** — colon form for top-of-description structure declarations (Parent/Blocks/Related), call form for inline prose ("This supersedes parent(ka-X)…").
  3. **Why use them aggressively** — they create navigable graph edges visible in beads-viewer, queryable via `kd list --mentions`/`kd refs`, and surfaced in `kd show`. Sparse use leaves context buried in prose.
  4. **What to mention** — parent/epic at top of description, blockers as encountered, related work when adjacent, supersedes when retiring old beads, MR links via `!NNNN`, plan-of-record paths.
  5. **Anti-patterns** — duplicate text mentions of the same target with different verbs (only the latest write wins per `(source, target, field, verb)` row); free-form prose like "see X, Y, Z" without IDs.
- One-line description (the "hook" surfaced in every prime): "Use typed mention syntax (`parent(ka-X)`, `blocks: ka-X`) aggressively in descriptions/comments — every typed mention becomes a graph edge in beads-viewer and a queryable filter in `kd list --mentions`."
- Cross-link to E1, E2, E3 once they ship — initially anchor against current state.

**Deliverables:**
- One advice bead created via `gb advice create` (or equivalent), labeled `global`, with the body above.
- Two paired beads filed against this gardener (per `ka-0000001818` "action items go in new task or bug beads"):
  - Bug: any bead that demonstrably violates the new advice (sparse mentions in a rich epic) — let agents discover and fix organically; this is just the seed.
  - Follow-up: revisit the advice once E2 ships to add "click any mention edge in beads-viewer to land on the inline anchor" line.

**Acceptance:**
- New advice bead appears in `gb prime` global section.
- Bead description ≤350 words, hook ≤200 chars, no `inline:true`.
- A second agent reading the advice can author a typed-mention-rich epic without further guidance.

## Sequencing

| Order | Epic | Why this order |
|---|---|---|
| 1 | E4 (advice) | Independent and short; sets corpus norms while engineering work is in flight. |
| 2 | E1 (constellation render) | Visible win, prerequisite for E2. |
| 3 | E2 (clickable + highlight) | Builds on E1's edge data attributes. |
| 4 | E3 (kd CLI) | Independent; can run in parallel with E1/E2 if a second crew agent is available. |

## Open questions for operator

1. **Strict parity vs. provenance dash on typed mentions** — closed decision `ka-0000000923` says "render identically to deps." The spec above proposes a *subtle* dash on text-derived edges so users can see provenance when wanted (with a toggle). Confirm: keep the dash, or strict parity per the closed decision?
2. **`kd mention` as a top-level alias** — yes/no? `kd refs mentions` works today; the alias matches the Slack ask phrasing but adds surface area.
3. **Comment/notes mention highlighting (E2 scope)** — defer to follow-up beads as proposed, or include in E2?
4. **Advice scope (E4)** — global is the obvious pick. Should we *also* land a gardener-role variant emphasizing "gardeners: when filing crew tasks, always parent: ka-X the source epic"?

## Files & paths (reference index)

| Concern | Path |
|---|---|
| Typed-mention parser | `kbeads/internal/references/typed.go` |
| Reference write path | `kbeads/internal/store/postgres/queries_references.go` |
| HTTP mention endpoints | `kbeads/internal/server/http.go` (lines ~101-102) |
| Graph emission | `kbeads/internal/server/http_graph.go` |
| Constellation builder | `gardenio/controller/cmd/beads-viewer/constellation.go` |
| Constellation template | `gardenio/controller/cmd/beads-viewer/templates/partials/constellation.html` |
| Linkifier | `gardenio/controller/cmd/beads-viewer/linkify.go` |
| Bead detail handler | `gardenio/controller/cmd/beads-viewer/server.go::handleBeadShow` |
| kd CLI commands | `kbeads/cmd/kd/{list,show,refs,search}.go` |
| ListBeads HTTP wire | `kbeads/internal/client/http.go` (lines ~179-181) |
| Proto definitions | `kbeads/proto/beads/v1/{beads,service}.proto` |
