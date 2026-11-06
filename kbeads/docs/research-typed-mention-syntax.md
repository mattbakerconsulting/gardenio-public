# Research: Typed Mention Syntax for Bead Text References

**Status**: Research / plan-of-record (not implementation)
**Author**: thread-1777755904-293929
**Date**: 2026-05-02
**Epic**: ka-0000001333
**Origin**: Slack thread — Matt Baker, 2026-05-02 ("we need a format in text to also define the relationship, e.g. `parent(ka-123)`")
**Companion**: `kbeads/docs/design-bead-references.md` (the existing untyped-mentions design that this proposal extends)
**See also**: [`design-bead-references.md`](design-bead-references.md) — the original untyped-mentions design (extractor, `bead_references` table, `/v1/graph` `mentions` edges, reverse lookup). This document layers verb tagging on top of that pipeline.

## TL;DR

The current text-reference extractor (`internal/references`) recognizes bare
`ka-0000001713` IDs and emits a flat "mentions" edge between the source and
target bead. Matt has asked for a way for authors to also declare *what kind*
of relationship the mention represents, directly in prose:

> "any bead we mention from another bead is linked to that bead — but it's
> too simple; we need a format in text to also define the relationship, e.g.
> `parent(ka-123)`."

This document is a research plan, not an implementation proposal. It surveys
the design space, names the forks that need an operator decision before any
code lands, and recommends a default for each so the operator's choice is a
diff against a stated baseline rather than a from-scratch picklist.

The four decisions surfaced as `gb decision` checkpoints are listed in
[§ 9 Decision checkpoints](#9-decision-checkpoints). Implementation work is
explicitly out of scope until those decisions resolve and the operator opens
an implementation epic.

## 1. Problem statement

### 1.1 What we have today

Already shipped (per `design-bead-references.md`):

- `internal/references` extracts every `\bkd-[A-Za-z0-9]{10}\b` from
  `title`, `description`, `notes` (and optionally `comments` behind a flag).
- `bead_references` table stores `(source_bead_id, target_bead_id,
  source_field, extracted_at)`. PK `(source, target, field)`.
- `/v1/graph` emits these as `dep_type: "mentions"` edges; beads-viewer's
  constellation renders them dashed.
- Reverse lookup at `GET /v1/beads/{id}/mentioned-by`.

The shape of the data is: "bead A's text contained the literal token ka-B,
in field F." The relationship is implicitly *all are equally weighted
"mentions"* — we do not record what the author *meant* by the mention.

### 1.2 Why that's not enough

Authors *do* mean different things when they paste an ID:

| Intent in prose | What the user expects | What we record today |
|---|---|---|
| "Parent: ka-X" | parent-child edge | flat mention |
| "Blocks ka-Y" | blocks edge | flat mention |
| "Supersedes ka-Z" | supersedes edge | flat mention |
| "See also ka-W" | weak / informational | flat mention |
| "Closes ka-Q" | parent-child or blocks (ambiguous) | flat mention |

Today the only way to record those typed edges is via `kd dep add A B
--type=parent-child` — a separate command that the author has to remember to
run after writing the bead. The text and the dep graph drift. Worse, in an
agent-driven workflow, agents constantly write descriptions that *say* "this
is a child of ka-X" without ever calling `kd dep add`, so the rendered
constellation under-represents the structure the author already intended.

Matt's framing is that *the prose already carries the signal*; we should
formalize it.

### 1.3 Goals

1. Let authors declare a typed relationship in bead text using a syntax that
   reads naturally in prose and Markdown.
2. Persist typed mentions with provenance (which field, which mention).
3. Make typed mentions queryable and renderable — both as enriched edges in
   `/v1/graph` and as filters in `kd search` / beads-viewer.
4. Decide cleanly how typed mentions interact with the explicit `deps` table
   (do they materialize dep rows? coexist as a parallel layer? both?).
5. Stay within the existing extraction pipeline's cost envelope — same-tx
   bulk INSERT, no async catchup.

### 1.4 Non-goals

- Free-text natural-language inference ("blocks" inside a sentence without
  the prescribed syntax). The syntax must be **syntactically explicit** so
  extraction stays a regex/parser job, not an NLP job.
- Multi-fleet / cross-project references.
- Replacing `kd dep add` — typed mentions are an *additional* path, not a
  replacement.
- Markdown-link forms beyond the canonical typed syntax (slug links, URL
  links). Out of scope; can be a follow-up.

## 2. Design space — syntax

Five candidate syntaxes, scored on: human-readability, parser simplicity,
collision safety with bare IDs already in the wild, Markdown-friendliness,
and how naturally they extend to additional relationship types.

### 2.1 Function-call form: `parent(ka-123)` (operator's example)

```text
This task fixes parent(ka-0000007196) and blocks(ka-someTarget0).
```

- **Pros:** matches the operator's stated example, reads as a function call,
  clearly delimited, easy regex (`\b<verb>\(kd-[A-Za-z0-9]{10}\)`), works
  inline.
- **Cons:** parens collide with normal English ("fixed it (ka-X) - thanks!"
  — a bare paren around an ID would also match if we were sloppy with the
  pattern; we won't be, but pre-existing prose may match unintentionally).
  Doesn't compose with multiple targets (`parent(ka-A, ka-B)` needs either
  a separator or repeated keywords).
- **Markdown:** safe. Parens are not a Markdown construct.

### 2.2 Markdown-link form: `[parent](ka-123)`

```text
This task supersedes [the old auth flow](ka-0000007196).
```

- **Pros:** native Markdown, label is human-readable independent of the
  relationship verb, renders nicely in viewers that resolve `ka-XXX` as a
  link target.
- **Cons:** the label and the verb are conflated — "the old auth flow" is
  not a verb. We'd need a convention: the *first word* of the label is the
  verb, the rest is freeform? Or the *whole* label is the verb? Both are
  brittle. Beads-viewer rendering depends on whether `ka-XXX` is a real URL
  here or just a sentinel; today it isn't.

### 2.3 Colon form: `parent: ka-123`

```text
- parent: ka-0000007196
- blocks: ka-someTarget0
- supersedes: ka-thirdBead00
```

- **Pros:** YAML-frontmatter-ish, list-friendly, and matches how humans
  already write structured fields in bead notes. Easy parser. Composes well
  in line-oriented prose.
- **Cons:** weaker inline ergonomics ("we will eventually parent: ka-X this
  task" reads awkwardly). Dictionary words like "parent" appearing colon-
  prefixed elsewhere (e.g. `parent: foo` in unrelated YAML) could false-
  positive — we'd need to require the line to also contain `ka-XXX`, which
  is fine.

### 2.4 Slash form: `parent/ka-123`

```text
parent/ka-0000007196 blocks/ka-someTarget0
```

- **Pros:** compact, single-token, very simple regex.
- **Cons:** least readable; collides with file paths if anyone pastes one.
  Easy to typo. Punted as worse than 2.1/2.3 on every axis except parser
  simplicity.

### 2.5 Frontmatter form: YAML/TOML block at top of description

```yaml
---
parent: ka-0000007196
blocks: [ka-someTarget0, ka-anotherOne1]
supersedes: ka-thirdBead00
---
```

- **Pros:** unambiguous, structured, easy to parse, supports lists trivially,
  the relationship vocabulary is machine-checkable.
- **Cons:** all-or-nothing — only the frontmatter block is structured; inline
  mentions stay untyped. Authors who don't write frontmatter keep the
  current behavior. Looks like "structured metadata on a bead" rather than
  "a way to write rich prose," which is what Matt described.

### 2.6 Recommendation (baseline for decision)

Prefer **2.1 function-call form (`parent(ka-X)`) as the primary syntax**,
plus **2.3 colon-bullet form on its own line** as an equivalent, list-
friendly alias. Both are simple regexes; both are free of Markdown
collisions; both compose with bare `ka-X` mentions (which stay flat).
2.5 frontmatter is recommended *only as a follow-up* — high effort, narrow
audience.

→ **Decision 1** (§ 9.1) puts this to the operator.

## 3. Design space — relationship vocabulary

What set of relationship verbs is allowed inside the `verb(ka-X)` form?

### 3.1 Closed vocabulary, fixed at the schema level

Use the existing `model.DependencyType` constants verbatim (`blocks`,
`parent-child`, `related`, `duplicates`, `supersedes`, `rates`) plus their
*inverses* needed for natural prose (e.g. `child-of`, `blocked-by`,
`duplicated-by`). Anything else fails extraction (silently or loudly).

- **Pros:** machine-checkable, finite test surface, every typed mention is
  guaranteed to render; aligns 1:1 with `deps.type`.
- **Cons:** authors will reach for verbs the schema doesn't have ("fixes",
  "closes", "depends-on", "tracks", "follows", "implements", "tests"). We
  either reject those (annoying) or alias them ("fixes" → blocks?) which
  embeds editorial judgement in the parser.

### 3.2 Closed core + alias map

Same as 3.1 but with a small alias table — `closes` → `parent-child`,
`fixes` → `blocks`, `tracks` → `related`, etc. — maintained in
`internal/references` and reviewed when added.

- **Pros:** ergonomic for writers, still bounded.
- **Cons:** alias choices are editorial. Wrong aliases create
  miscategorized edges that the author won't notice. Needs a UX for
  "verb you used was aliased to X" so the author can correct it.

### 3.3 Open vocabulary, free-text relationship label

Persist whatever verb the author wrote, schema-side just stores the string.

- **Pros:** never rejects an author's intent. Survives future verb additions
  without code changes.
- **Cons:** breaks the typed-edge promise — `dep_type` becomes "any string a
  user typed once," and rendering gets vague. Two beads can use "fixes"
  vs. "fix" vs. "fixed" and the graph splits them. Mitigatable with case
  folding + a lemmatizer, but that path is an NLP project.

### 3.4 Closed core + capture-as-mention fallback

Closed core (3.1 list); unknown verbs degrade to a normal flat mention with
the verb stored as an annotation but **not** treated as a typed edge.
`parent(ka-X)` → typed parent edge. `closes(ka-X)` → flat mention with
`raw_verb='closes'` recorded but no typed edge written.

- **Pros:** never silently miscategorizes. Author sees no "rejection." Future
  versions can promote frequently-seen verbs into the closed core.
- **Cons:** user expectation gap — "I wrote `closes(ka-X)`, why is it dashed
  not solid?" Needs UI affordance to explain.

### 3.5 Recommendation (baseline for decision)

**3.4 closed core + capture-as-mention fallback**, with the closed core =
the existing `model.DependencyType` constants plus a small set of natural
inverses (`child-of`, `blocked-by`). Aliases (3.2) deferred — let usage
data tell us which aliases to add rather than guessing. This keeps the
schema honest and the author always sees *some* edge from their intent.

→ **Decision 2** (§ 9.2) puts this to the operator.

## 4. Design space — storage model

Where do typed mentions live?

### 4.1 Materialize into `deps` directly

When we extract `parent(ka-X)` from bead Y's description, INSERT a row into
`deps` with `(bead_id=Y, depends_on_id=X, type=parent-child)`, plus
metadata recording the source field.

- **Pros:** single source of truth for the dep graph. Every consumer
  (constellation, search, formula expansion, ready-queue) immediately
  sees the typed edge with no code change.
- **Cons:** mixes user-asserted edges with text-derived edges in one table.
  Conflicts get nasty: if I add `parent(ka-X)` and *also* `kd dep add Y X
  --type=blocks`, who wins? Edits to text could silently delete a dep the
  author created via `kd dep add`. Backfill rebuilds risk dropping
  hand-managed edges.

### 4.2 Keep typed mentions in `bead_references` (extended schema)

Add a `relationship_type TEXT NULL` column to `bead_references`. Untyped
mentions keep `relationship_type = NULL` (current behavior). Typed mentions
populate it. `/v1/graph` emits `dep_type: "<relationship_type>"` for non-
NULL rows and `dep_type: "mentions"` for NULL rows. `deps` is unchanged.

- **Pros:** clean separation: `deps` = what humans/agents explicitly assert
  via API, `bead_references` = what text says. No conflict semantics
  needed; renderers can show both, prefer one over the other, or merge
  them. Backfill / rebuild stays safe.
- **Cons:** consumers that only read `deps` (formula expansion, ready queue
  resolver, possibly some Helm charts of orchestration logic) won't see
  typed mentions. We'd have to enumerate those and either teach them about
  `bead_references` or accept the gap.

### 4.3 Hybrid: materialize *some* relationship types into `deps`, keep others in `bead_references`

E.g. parent-child and blocks materialize (because formula/ready-queue logic
depends on them); related/duplicates/supersedes stay in `bead_references`.

- **Pros:** matches each relationship's actual consumer set.
- **Cons:** gnarly rules; hard to explain; the policy table itself becomes
  a maintenance hotspot.

### 4.4 Recommendation (baseline for decision)

**4.2 bead_references with `relationship_type`**. The cleaner separation is
worth the consumer-side awareness work; it preserves the property the
existing design relies on — text references are a *derived* layer that can
be rebuilt without touching user-asserted state. Material gaps for
formula/ready-queue consumers should be enumerated as follow-up tasks
under the implementation epic, not lumped into the parser.

→ **Decision 3** (§ 9.3) puts this to the operator.

## 5. Design space — direction & inverses

`parent(ka-X)` on bead Y unambiguously means *"Y's parent is X"*. But
`blocks(ka-X)` on bead Y could plausibly mean either:

- (a) *Y blocks X* — Y must finish before X can start. Parser writes edge
  `Y --blocks--> X`.
- (b) *X blocks Y* — i.e. "this task is blocked by X." Parser writes
  `X --blocks--> Y`.

Authors use both phrasings. Today's `deps` table convention is *bead_id
depends_on depends_on_id*, so a `blocks` row reads "bead_id is blocked by
depends_on_id." That makes (b) the schema-direction match for the verb
"blocked by" but puts "blocks" naturally in (a).

We need a rule. Options:

- **Direction-by-verb-stem.** `blocks` always means (a) — Y blocks X. To get
  (b) you write `blocked-by(ka-X)`. Symmetric: `parent` means Y's parent is
  X; to assert Y is the parent of X, write `child-of(ka-X)` from X's side
  (or just write `parent(ka-Y)` from X's bead).
- **Direction-explicit-suffix.** Always require an inverse form to be
  unambiguous: `blocks-out(ka-X)` vs `blocks-in(ka-X)`. Robust; ugly.
- **Convention by Markdown context.** Heuristic: if the verb is in a list
  bullet on the bead's own page, it's *about this bead*. Brittle.

**Recommendation:** direction-by-verb-stem (option 1), plus an explicit
inverse vocabulary for the verbs whose natural prose form is the inverse
direction (`blocked-by`, `child-of`, `duplicated-by`). Document the
canonical direction for each verb in `references.RelationshipVocabulary`
and the spec doc.

This is wrapped into Decision 2 (§ 9.2) — the operator effectively picks
the verb set; documenting direction per verb is then mechanical.

## 6. Design space — lifecycle on edit & delete

The existing untyped extractor already replaces `bead_references` rows on
every text write (DELETE same-source-and-field, then bulk INSERT). Typed
mentions inherit that lifecycle for free — the typed row is just a
relationship-typed sibling of an untyped row.

The interesting cases:

- **Author removes `parent(ka-X)` from description.** Next write deletes
  the typed row. Renderer drops the parent edge. ✅ Matches author intent.
- **Author edits `parent(ka-X)` → `child-of(ka-X)`.** Replace cycle
  rewrites the typed row to the new relationship type. ✅
- **Author writes `parent(ka-X)` then later writes `blocks(ka-X)` on the
  same bead.** Two distinct rows in `bead_references` — same source, same
  target, same field, *different* `relationship_type`. The PK becomes
  `(source, target, field, relationship_type)`. Both edges render. ✅
- **Comment edited.** `comment:<id>` source-field semantics already
  per-comment-scoped; typed mentions in comments behave identically. ✅
- **Bead soft-deleted.** Existing CASCADE deletes the references rows. ✅

Conclusion: lifecycle is solved-by-construction in option 4.2; nothing new
to design here. If we picked 4.1 (materialize into `deps`), the lifecycle
question would be much harder — see § 4.1 cons.

## 7. Design space — parser & extraction

Sketch. This is implementation-shaped but useful for sizing the work.

```go
// internal/references/typed.go
type TypedReference struct {
    TargetID     string
    Verb         string  // canonical, lowercased; "" for bare-ID mentions
    Raw          string  // exactly what the author wrote (for provenance)
    Position     int     // byte offset for UI hover
}

var verbCallPattern = regexp.MustCompile(
    `\b([a-z][a-z0-9-]{1,30})\(\s*(kd-[A-Za-z0-9]{10})\s*\)`,
)

var verbColonPattern = regexp.MustCompile(
    `(?m)^\s*[-*]?\s*([a-z][a-z0-9-]{1,30})\s*:\s*(kd-[A-Za-z0-9]{10})\b`,
)
```

Closed-core check:

```go
var canonicalVerbs = map[string]model.DependencyType{
    "parent":       model.DepParentChild,
    "child-of":     model.DepParentChild, // inverse direction; emit reversed edge
    "blocks":       model.DepBlocks,
    "blocked-by":   model.DepBlocks,      // inverse direction
    "related":      model.DepRelated,
    "duplicates":   model.DepDuplicates,
    "duplicated-by": model.DepDuplicates,
    "supersedes":   model.DepSupersedes,
    "superseded-by": model.DepSupersedes,
}
```

Unknown verb → degrade to flat mention, persist verb as
`raw_verb` for later promotion. Bare `ka-X` (no verb) → behavior unchanged
from today.

Test surface: 30–40 small unit tests on `extract`, fixture-driven. The
parser is a straight regex; complexity is in the policy table, not the
matching.

## 8. Design space — UX surfaces

Once the data exists, expose it:

- **`kd refs from <id>`** already exists; extend to include
  `relationship_type` in the JSON output.
- **`kd refs mentioned-by <id> --type=parent-child`** filter.
- **`/v1/graph?include_text_references=true`** already exists; emit
  typed-mentions with `dep_type=<relationship_type>` and a new
  `source: "text"` field on the edge so consumers can distinguish from
  `deps`-derived edges. **Decision 4** (§ 9.4) governs whether typed
  mentions render the *same* visual style as their `deps` counterparts
  (solid, weighted) or stay dashed-but-color-coded (visually distinct
  from "real" deps).
- **beads-viewer surfaces.** Two render points:
  1. Constellation graph — typed-mention edges follow Decision 4 (§ 9.4).
     Edge metadata still carries `source: "text"` so a future toggle can
     split text-vs-deps provenance.
  2. Bead show page (`Mentions` / `Mentioned by` sections) — display the
     `relationship_type` next to each row so a reader sees
     `parent → ka-X (mentioned in description)` instead of an untyped link.

## 9. Decision checkpoints

Four operator decisions are surfaced as `gb decision create` checkpoints
linked to this epic. Each is single-blocker for an implementation epic.

### 9.1 Decision 1 — primary syntax

What syntax form do we ship?
Options: `parent(ka-X)` (function-call), `parent: ka-X` line form, both as
aliases, or Markdown-link `[parent](ka-X)`.
Recommendation: **both function-call and colon-line as aliases**.

### 9.2 Decision 2 — relationship vocabulary

What verb set is recognized?
Options: closed (existing `DependencyType` + inverses), closed + alias map,
open free-text, closed-core with unknown→flat-mention fallback.
Recommendation: **closed core + capture-as-mention fallback** (§ 3.4).

### 9.3 Decision 3 — storage model

Where do typed mentions live?
Options: materialize into `deps`, extend `bead_references` with
`relationship_type`, or hybrid.
Recommendation: **`bead_references.relationship_type`** (§ 4.2).

### 9.4 Decision 4 — render parity with `deps` edges

In `/v1/graph` and beads-viewer, do typed mentions render *visually equivalent*
to the same-typed `deps` row, or visually distinct (still dashed, color-
coded)?
Options: equivalent (looks the same), distinct (looks like enriched
mentions), hybrid (equivalent for parent/blocks; distinct for the rest).
Recommendation: **distinct** — keep authored prose visually
distinguishable from operator-asserted dep graph until we have data on
how often the two disagree.

## 10. Implementation epic (deferred)

After all four decisions resolve, the operator can open an implementation
epic with these subtasks (sketched here so the work is sized):

1. Schema migration: `bead_references.relationship_type` column + index;
   PK extended to `(source, target, field, relationship_type)`.
2. `internal/references/typed.go` parser + verb table + tests.
3. Wire typed extraction into `replaceBeadReferences` /
   `replaceCommentReferences`.
4. Backfill: `kd admin reextract-references` already exists — extend to
   also populate the new column. Idempotent.
5. `/v1/graph` emit: typed `dep_type`, edge `source: "text"`.
6. `kd refs ... --type=<verb>` filter + JSON field.
7. beads-viewer render: per Decision 4 outcome — constellation edge styling
   AND `Mentions` / `Mentioned by` sections on the bead show page surface
   the `relationship_type`. (beads3d is out of scope for this epic.)
8. Docs: `kbeads/docs/design-bead-references.md` cross-link to this file;
   `kbeads/CLAUDE.md` add a "Typed mention syntax" section once shipped.

Estimated size (post-decisions): ~1 week of focused work for one engineer,
~half of which is tests and the backfill.

## 11. Open questions worth flagging now

Not blocking the decisions but worth tracking:

- **Conflict signaling.** If a bead has both `parent(ka-X)` in text and a
  `kd dep add B X --type=blocks` in `deps`, do we surface the disagreement
  anywhere? Probably yes — a `kd refs conflicts` command — but it's a
  follow-up.
- **Slug references with verbs.** `parent(my-fancy-slug)` — out of scope
  for the same reason untyped slug references are. Same follow-up bead.
- **Validation in agent prompts.** Should agents be steered to *write*
  typed mentions (with a CLAUDE.md addition) once shipped, or should we
  let the syntax spread organically? Recommend the latter at first, then
  CLAUDE.md once we see real usage.

## 12. References

- `kbeads/docs/design-bead-references.md` — the existing untyped-mentions
  design that this proposal extends.
- `kbeads/internal/references/references.go` — current extractor.
- `kbeads/internal/store/postgres/queries_references.go` — write path.
- `kbeads/internal/model/dependency.go` — current `DependencyType` set.
- Slack thread origin — Matt Baker, 2026-05-02 (linked from epic
  ka-0000001333).

## 13. Replan note (2026-05-02)

Operator directive on impl epic `ka-0000003840`: drop beads3d from the T7
render scope; ship the typed-mention render in **beads-viewer** only. T7
now targets two surfaces in `gardenio/controller/cmd/beads-viewer/`:

1. `constellation.go` — typed-mention edges follow Decision 4 (equivalent).
2. `bead_preview.go` / `bead_show_test.go` (`Mentions` / `Mentioned by`
   sections) — show the typed verb next to each row, with provenance
   (`mentioned in <field>`) preserved.

beads3d is unchanged by this epic; if/when it picks up typed-mention
styling later it can read the same `/v1/graph` `dep_type` + `source`
fields shipped by T5.
