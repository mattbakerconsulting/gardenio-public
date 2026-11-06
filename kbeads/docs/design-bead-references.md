# Design: Bead-ID References Parsed from Text

**Status**: Sketch
**Author**: thread-1777342314-897689
**Date**: 2026-04-28
**Bead (epic)**: TBD (see `kd list -l project:gardenio -t epic` after creation)
**Origin**: Slack thread — Matt Baker, 2026-04-28
**See also**: [`research-typed-mention-syntax.md`](research-typed-mention-syntax.md) — the follow-up design that extends this untyped extractor with verb-tagged mentions (`parent(ka-X)`, `blocks: ka-Y`) carried in `bead_references.relationship_type`.

## Problem

The constellation visualization (`beads3d/`) draws edges only from explicit
relationships stored in the `deps` table — `blocks`, `parent-child`, plus a few
client-side synthesized edges (`assigned_to`, `rig_conflict`). It ignores a rich
source of latent structure: **bead IDs that authors paste into descriptions,
notes, and comments** ("see ka-0000004973", "supersedes ka-0000000015").

Two user-visible gaps:

1. **Constellation is sparser than reality.** A bead can name five other beads
   in its description without ever showing up as an edge.
2. **No reverse lookup.** "Find every bead that mentions ka-0000004973" is
   currently a `kd list` plus an `ILIKE '%ka-Ab…%'` scan in the user's head.

## Goals

- Parse `ka-0000001713` references out of bead text on write, persist the edges,
  and surface them everywhere the explicit `deps` graph is surfaced.
- Expose a queryable reverse-lookup ("what mentions this bead?") in the HTTP/gRPC
  API and in `kd`.
- Make the new edge type a first-class citizen in `/v1/graph` and `beads3d` —
  visually distinct, filterable, opt-out-able.
- Keep the hot write path cheap: extraction must not block create/update calls.

## Non-goals

- Markdown link parsing, URL parsing, slug parsing — only the canonical
  `kd-[A-Za-z0-9]{10}` ID form. (Slug refs can be a follow-up; they require
  resolving slugs at extraction time, which is more invasive.)
- Cross-project / multi-fleet references. Single-fleet only.
- Backfilling reference edges into the `deps` table. References are a separate
  edge type with their own table and lifecycle.

## Current state (research findings)

Cited at `kbeads/docs/design-embedding-search.md`-style depth so future readers
can verify; full reports in the planning bead.

| Item | Where |
|---|---|
| Constellation render | `beads3d/src/main.js:2062-2206` (`fetchViaGraph`) |
| Graph API handler | `kbeads/internal/server/http_graph.go:17` |
| Dep types in use | `kbeads/internal/model/dependency.go` (`blocks`, `parent-child`, `related` defined; first two rendered) |
| Beads table text columns | `migrations/001_initial.up.sql:1-21` — `title`, `description`, `notes`, `fields` (jsonb) |
| Comments table | `migrations/001_initial.up.sql:47-55` — `text` column, **excluded** from semantic search by design |
| ID format | `kbeads/internal/idgen/idgen.go:11-17` — prefix `kd-`, 10 chars, alphabet `[A-Za-z0-9]` |
| Update paths converge at | `store.UpdateBead` (called from `cmd/kd/update.go`, `internal/server/http_beads.go:277`, `internal/server/beads.go:565`) |
| Existing reference parsing | **None** — greenfield |

## Design

### 1. Schema: `bead_references` table

A separate table — **not** a new `deps.type` — because:

- References can be many-to-many across the same pair with different source
  fields (description vs. comment vs. notes); `deps` PK is `(bead_id,
  depends_on_id, type)` and would collapse them.
- References are derived state. We want to rebuild from text without touching
  user-entered `deps`.
- A separate table lets us drop & rebuild the index during backfill without
  risking dependency loss.

```sql
-- migrations/0NN_bead_references.up.sql
CREATE TABLE bead_references (
    source_bead_id  TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
    target_bead_id  TEXT NOT NULL,                       -- NOT FK: dangling refs are valid signal
    source_field    TEXT NOT NULL,                       -- 'description' | 'notes' | 'title' | 'fields' | 'comment:<id>'
    extracted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_bead_id, target_bead_id, source_field)
);

CREATE INDEX idx_bead_references_target ON bead_references (target_bead_id);
CREATE INDEX idx_bead_references_source ON bead_references (source_bead_id);
```

Notes:

- `target_bead_id` is intentionally **not** a FK. Authors mistype IDs and paste
  IDs from other fleets — those are still useful signal ("dangling reference"
  is a real bead-quality flag we may want to surface later). A FK would force
  us to drop them, losing information.
- `source_field` carries provenance so the UI can tell the user *why* an edge
  exists ("mentioned in your description", "mentioned in a comment by alice").
- Comments are scoped per-comment via `comment:<id>` so per-comment deletes
  cleanly delete the right rows. (Simple `comment` would over-delete on
  recompute.)
- The `fields` bucket (added for ka-0000001507) extracts **untyped** references
  from the raw JSON of `beads.fields`. Many bead types keep their canonical,
  operator-facing body in a fields key rather than `description` — most notably
  `gb decision create`, which writes the decision body to `fields.context` and
  leaves `description` empty. Without this bucket those ids produce no edge, so
  `kd list --mentions <id>` (the §5.0 pre-mutation decision gate) failed OPEN
  for the majority of open decisions. The bucket is untyped-only: extraction
  runs `references.Extract` over the raw JSON bytes (typed parsing / dep
  promotion out of arbitrary JSON blob content — option labels, prompts — would
  poison the deps graph), which also captures ids nested under any key (context,
  options, prompt, …) at any depth.

### 2. Extraction

A `references` package in `kbeads/internal/references/`:

```go
package references

// Pattern matches the canonical kd ID form. Anchored on word boundaries so it
// doesn't fire inside other tokens (e.g. "akd-XYZ…" or hash suffixes).
var Pattern = regexp.MustCompile(`\bkd-[A-Za-z0-9]{10}\b`)

// Extract returns the unique IDs found in s, preserving first-seen order.
func Extract(s string) []string { ... }
```

**Tests cover**: empty string, no matches, exact match, embedded in markdown
link, multiple matches, duplicates collapsed, false positives rejected
(`ka-tooshort`, `ka-12345678901` (too long), `ka-ABC!XYZ012` (bad chars)),
case-sensitivity preserved.

### 3. Write-path integration

Extraction runs **after** the bead is committed and **before** the function
returns, but in a transaction-coupled write to `bead_references`. Two options
considered:

- **Option A (chosen): synchronous, same-tx upsert.** In `store.UpdateBead`
  and `store.CreateBead` (and the comment create/update paths), after the
  primary write succeeds, compute `Extract(title) ∪ Extract(description) ∪
  Extract(notes)`, `DELETE FROM bead_references WHERE source_bead_id=$1 AND
  source_field IN (...)`, then bulk-INSERT the new set. Same transaction, so
  failure rolls back atomically.
- **Option B (rejected for now): async via the event bus.** Subscribe to
  `bead.updated` events, recompute references off the hot path. Cheaper writes,
  but adds a "references catch up" race with the graph endpoint, and requires
  reasoning about ordering during reorg/burn. Punt to a later optimization if
  measurement shows the synchronous path matters.

Cost of Option A at expected sizes: a description with 5 references costs one
DELETE + one 5-row INSERT, both indexed by `source_bead_id`. Negligible.

### 4. Backfill

Add `kd admin reextract-references` (or similar — name TBD with reviewer) that
streams every bead, runs `Extract` on its text fields, and replaces its rows in
`bead_references`. Idempotent. Run once after migration; safe to re-run.

For comments: same job iterates `comments` and writes `source_field='comment:<id>'`.

As of T4 of the typed-mention epic (`ka-0000003840`) the same pass also populates
`bead_references.relationship_type` for every recognized canonical typed mention
(`parent(ka-X)`, `blocks: ka-Y`, etc.). Operator runbook —
when to run, expected duration, verification — lives in
[`operator-reextract-references.md`](operator-reextract-references.md).

### 5. Read APIs

Two new reads, plus an extension to the existing graph endpoint.

**a. `GET /v1/beads/{id}/mentioned-by`** — reverse lookup. Returns minimal
bead summaries (id, title, type, status, source_field) for every bead whose
text references `{id}`. Paginated. The natural CLI surface is
`kd refs mentioned-by <id>` (and `kd refs mentions <id>` for the forward
direction — which can be served from the same table without re-parsing).

**b. `/v1/graph` extension.** Add a query flag `include_text_references=true`
(default: `true` — the user explicitly asked for it). Server joins
`bead_references` for the in-window beads and emits edges with a new
`dep_type: "mentions"`. Source field is included as edge metadata so beads3d
can render it on hover.

**c. Search composition.** Extend `kd search` (and its HTTP counterpart) with
a `--mentions ka-XXX` filter that joins through `bead_references`. Composes
with existing `--status`, `--label`, etc.

### 6. beads3d rendering

In `beads3d/src/main.js` `fetchViaGraph()`:

- Recognize `dep_type === 'mentions'` alongside the existing ranks (lowest
  priority — below `relates-to`, above synthesized edges).
- New visual style: dashed line, lower opacity, color borrowed from the
  "weaker relationship" palette already used for `related`.
- Filter dashboard (`filter-dashboard.js`): toggle "show text mentions" — on
  by default but user can hide.
- Detail panel (`detail-panel.js`): in the relationships list, show
  "Mentioned in description by foo, in a comment by bar" with provenance.

### 7. Observability

- Counter: `bead_references_extracted_total{source_field}` on every write.
- Histogram: `bead_references_extract_duration_seconds`.
- Gauge: `bead_references_total` (table cardinality, refreshed by stats job).

## Migration & rollout

1. Ship the table + extraction code dark (no read API, no graph integration).
   Backfill in production. Verify cardinality and write latency look sane.
2. Ship the read APIs. Operator can use them via `kd refs ...` before the UI
   exists.
3. Ship beads3d rendering behind an off-by-default filter toggle. Flip on.
4. Keep the off-toggle around — some users find "mentions" noisy.

### Comment-text extraction toggle

Comment-body extraction is gated behind a config flag — `BEADS_REFERENCES_EXTRACT_COMMENTS`
(default `0`/off) — pending decision ka-0000000868. Until that decision lands,
deployments must opt in explicitly:

1. Set `BEADS_REFERENCES_EXTRACT_COMMENTS=1` on the beads server.
2. Run the backfill: `kd admin reextract-references` (idempotent; the backfill
   honors the same flag, so it iterates the comments table only when on).
3. Once the decision flips comment-text on by default, the flag is removed in a
   follow-up release; until then, both halves of the contract are pinned by
   tests in `internal/store/postgres/queries_references_comment_flag_test.go`.

Flag-off behavior: `AddComment` and the backfill skip the `bead_references`
write entirely for comment text. Bead title/description/notes extraction is
unaffected.

## Open questions / decisions needed

- **Should comment text be parsed?** It's the highest-volume source and the
  most conversational — likely the most noise. Recommend yes (it's the
  obvious thing the user expects), but make the comment-text edges a separate
  toggle in beads3d so users can suppress them independently.
- **Slug references.** Some teams paste slugs (`fix-login-bug`) instead of
  IDs. Out of scope here; a follow-up can resolve slugs at extraction time
  using the existing slug → id map, but this requires care around slug
  rewrites and beads that haven't been written yet.
- **Mutation history & event payloads.** These can also contain ID strings
  but are audit, not authored content. Skip.

## Implementation epic

Subtasks (one bead each):

1. Schema migration `bead_references` + indices.
2. `internal/references` package with `Pattern` + `Extract` + tests.
3. Wire extraction into `store.CreateBead`, `store.UpdateBead`, comment
   create/update; metrics.
4. `kd admin reextract-references` backfill command.
5. HTTP `GET /v1/beads/{id}/mentioned-by` + gRPC equivalent + `kd refs`
   subcommand.
6. Extend `/v1/graph` with `include_text_references` and `dep_type: "mentions"`.
7. `kd search --mentions <id>` filter.
8. beads3d: render `mentions` edges, dashed style, filter toggle, detail panel
   provenance.
9. Decision bead — confirm comment-text inclusion default and the off-by-default
   filter posture before shipping step 8.
