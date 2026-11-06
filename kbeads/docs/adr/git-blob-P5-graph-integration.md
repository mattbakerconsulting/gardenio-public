# ADR: `git-blob` Bead Type — Graph Integration (`origin_bead`, Typed-Mention Verbs, Viewer Rules, Default Filters)

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p5-gitblob-adr-jh4
**Phase**: P5 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000002819
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (§ 6 — graph integration; § 9.1 — lifecycle; § 10 D-7).
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks type name `git-blob`, kind `data`, and the optional `origin_bead` field on the type sketch (§ 7.2).
- P1 ADR `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044) — locks the natural-key triple `(repo, commit_sha, path)`, the partial unique index, and the upsert-with-stable-ID semantics that make pre-existing typed mentions resolve cleanly (§ 5.1: "a typed mention `produces(ka-blobX)` written *before* the blob is registered will resolve when the blob registers, because the ID is stable from the moment of first registration").
- Companion (closed) typed-mention research: `kbeads/docs/research-typed-mention-syntax.md` (epic ka-0000001333) — defines the closed-core verb vocabulary, the `bead_references.relationship_type` storage shape, and Decision 4 (visually distinct rendering for typed mentions vs `deps`).
- Implementation precedent: `kbeads/internal/references/typed.go` (closed-core verb table), migration `013_bead_references_relationship_type` (storage), `gardenio/controller/cmd/beads-viewer/constellation.go` and `bead_preview.go` (viewer surfaces).

**Resolves decisions**: **D-7** (new typed-mention verbs for `git-blob`).
**Defers**: D-4 (settled in P2), D-6 (settled in P3), D-8 (P6, embedding policy), and the lifecycle/GC/orphan question (P7).

## TL;DR

Building on P0's locked schema and P1's stable-ID upsert, P5 closes the open question about how `git-blob` beads weave into the rest of the bead graph and what an operator sees when one shows up in a list or the constellation. Three decisions, plus two clarifications:

- **`origin_bead` field (clarification of P0 §7.2 sketch).** A single optional `string` field on `git-blob` beads carrying *one* `kd-` ID — the "canonical attribution" bead this snapshot was produced for (typically the issue/MR bead the agent was working). Set on first registration, then immutable; subsequent re-registrations of the same triple may *populate* `origin_bead` if it was empty but may not *change* a non-empty value (consistent with P1 § 3.5 "first-writer-wins on identity, last-writer-wins on provenance" — `origin_bead` sits on the identity side of that line). Multi-context attribution (this blob also relates to a different epic) goes through the typed-mention or explicit-dep path, not through `origin_bead`. The field is **not** part of the natural key and **not** indexed for uniqueness; it is rendered as a back-edge (§ 4) and used as the default group key for the constellation (§ 5.2).
- **Typed-mention verb extensions (resolves **D-7**).** Add **two new forward/inverse verb pairs** to `internal/references/typed.go`'s `canonicalVerbs` map: `produces`/`produced-by` and `derives-from`/`derived-from`. Both new pairs reuse the existing `model.DependencyType` constant `parent-child` — `produces` (forward, issue → blob: "this issue produces this blob") aliases to the same `parent-child` semantics that `parent`/`child-of` already use; `derives-from` (forward, blob → prior-blob: "this blob derives from that earlier blob") also aliases to `parent-child` because the prior blob is, semantically, the *parent* in the lineage chain. We do **not** add new `DependencyType` constants. We do **not** add `supersedes`-mapped variants (a blob does not supersede its predecessor in the bead-work sense; the predecessor is still a valid historical record). The two new pairs are **alias-only extensions** to the existing closed core — no schema change, no migration. The change is one diff in `canonicalVerbs` and one extension to `IsInverseVerb`; this ADR specifies the diff and the rationale, but ships no code.
- **beads-viewer rendering rules.** `git-blob` beads render with a distinct compact glyph (path-tail + 7-char SHA), are off by default in the constellation (filtered out of `/v1/graph` by default), and appear inline on the *origin* bead's show page in a new "Produced blobs" section. Typed-mention edges *to* a `git-blob` use the existing "visually distinct from `deps`" Decision 4 styling (dashed, color-coded by verb). The `origin_bead` back-edge renders as a solid edge styled the same as a `parent-child` `deps` row, because the field semantically *is* a parent-child assertion (issue is parent, blob is child) made at registration time.
- **Default list filters.** `kd list` and `/v1/beads` default behaviour excludes `kind: data` beads, which already covers `git-blob` (P0 lock). P5 adds two ergonomic surfaces on top of the existing kind filter: (a) `kd list --type=git-blob` includes `git-blob` rows even when the `kind: data` exclusion is otherwise active (specifying a type implies including its kind), and (b) a small set of git-blob-aware filter shortcuts (`-f repo=`, `-f path=`, `-f commit_sha=`, `-f origin_bead=`) that the existing JSONB-field filter machinery already supports; the ADR fixes the documented shortcut names so callers don't drift. The constellation-default `git-blob`-hidden behaviour is implemented as a `/v1/graph` query parameter (`include_data_kind=false` default) rather than a viewer-only filter, so any consumer (beads-viewer, beads3d, future skills) inherits the same default.
- **Pre-resolved-ID typed mentions are explicitly supported.** A typed mention written in prose *before* the target `git-blob` exists (e.g. an agent writes `produces(ka-blobX)` in the parent issue, then a moment later runs `kd git-blob add` which the operator approves) resolves once the blob registers. The reference-extraction pipeline already replays on text writes; the new bead's ID being stable per P1 § 3.4 means the edge materializes naturally. P5 does not add re-extraction — the existing pipeline is sufficient. We document this explicitly so future-us doesn't accidentally add a "blob registration triggers re-scan of all references" mechanism that isn't needed.

This ADR ships **zero code**. It is the spec the eventual P9 implementation will register at the closed-core verb table, the `/v1/graph` query handler, the beads-viewer constellation/show-page surfaces, and the `kd list` filter machinery. The operator-facing fork resolved here (D-7) is surfaced as a single `gb decision` so the operator either accepts the recommended verb additions or sends back a specific override.

## 1. Context

### 1.1 What earlier phases already locked

| Phase | Locked | Relevance to P5 |
|---|---|---|
| **P0** (ka-0000006442, `adr-git-blob-naming-schema.md`) | Type name `git-blob`, kind `data`, required field set, optional `origin_bead string` field listed in the § 7.2 P9 sketch, write policy `project_scoped: true, escalatable: true`. | The `origin_bead` field's *existence* is locked. P5 fixes its **shape, lifecycle, and rendering**. The `kind: data` lock is the load-bearing premise for the default-list-filter decision (§ 6). |
| **P1** (ka-0000002044, `git-blob-P1-identity-dedup.md`) | Random nanoid IDs, partial-unique-index dedup over `(LOWER(repo), commit_sha, path)`, "first-writer-wins on identity / last-writer-wins on provenance", upsert-with-stable-ID semantics. | The stable-ID property means a typed mention `produces(ka-blobX)` written before the blob registers will resolve once it does — without re-extraction. § 5.1 of the P1 ADR explicitly flagged this for P5; this ADR codifies the consequence (§ 7.3). |
| **P2** (ka-0000004323, `git-blob-P2-storage-tiering.md`) | Three-tier storage; `git_blob_content` table; per-bead `content_ref` for Tier C. | No direct interaction with graph integration. Mentioned only because the constellation render must not pull `content` to display a blob node — the listing endpoints and `/v1/graph` already project a small column set; this ADR codifies the projection (§ 5.3). |
| **P3** (ka-0000007332, `git-blob-P3-security-redaction.md`) | Server-side gitleaks scan + redaction; `secrets_redacted`, `secrets_summary` lifecycle. | The constellation badge for a `secrets_redacted=true` blob is part of the rendering rules in § 5.4. |

P4 (CLI/API spec) is in flight under a sibling task; this ADR cites it where the `kd refs` and `kd list` flags interact (§ 6.2) but does not depend on the P4 artifact landing first.

### 1.2 What the typed-mention research already shipped

`kbeads/docs/research-typed-mention-syntax.md` (epic ka-0000001333) is **closed and implemented**. The closed-core verb vocabulary lives in `internal/references/typed.go`:

```go
var canonicalVerbs = map[string]model.DependencyType{
    "parent":        model.DepParentChild,
    "child-of":      model.DepParentChild,
    "blocks":        model.DepBlocks,
    "blocked-by":    model.DepBlocks,
    "related":       model.DepRelated,
    "duplicates":    model.DepDuplicates,
    "duplicated-by": model.DepDuplicates,
    "supersedes":    model.DepSupersedes,
    "superseded-by": model.DepSupersedes,
}
```

Storage is `bead_references.relationship_type TEXT NULL` (migration `013_bead_references_relationship_type`); the unique index is over `(source_bead_id, target_bead_id, source_field, COALESCE(relationship_type, ''))`, so a single `(source, target, field)` can carry both an untyped flat mention *and* a typed edge with a different `relationship_type`.

Unknown verbs degrade to a flat mention with the verb captured in `RawVerb` for later promotion. That fallback is the load-bearing mechanism that lets P5 add `produces` / `derives-from` to the closed core *without* breaking any prose written before the change: prose that today says `produces(ka-blobX)` is already extracted as a flat mention; once the closed-core map gains the verb, the *next* text write on that bead promotes the existing flat mention to a typed edge via the standard delete-then-insert lifecycle (§ 6 of the typed-mention research). No backfill is required; an admin re-extraction (`kd admin reextract-references`) is sufficient to retro-promote, and is already an existing tool.

The Decision-4 outcome from that research — **typed mentions render visually distinct from same-typed `deps` rows** — is the inherited render policy that this ADR builds on (§ 5.1).

### 1.3 What the plan-of-record asks of P5

`kbeads/docs/research-git-blob-bead-type.md` § 6 frames graph integration as four sub-questions:

1. **§ 6.1** — How issues/MRs link to blobs. The PoR proposes "all three, but layered": `origin_bead` for canonical 1:1 attribution, typed-mentions for additional context, explicit deps for operator-asserted relationships.
2. **§ 6.2** — Whether to extend the typed-mention verb set with `produces`/`produced-by` and `derives-from`/`derived-from`. The PoR explicitly defers this to the typed-mention epic *or* P5 — and since the typed-mention epic closed without these verbs, P5 owns the call.
3. **§ 6.3** — What renders in beads-viewer: compact glyph, dashed back-edges, off-by-default in the constellation.
4. **§ 9.1** — `git-blob`s are immutable historical records; closing is allowed but discouraged. This affects whether closed blobs render in default views.

D-7 in PoR § 10 is named "Add new typed-mention verbs (`produces`, `derives-from`)?" with the default deferral to the typed-mention research. P5 takes it head-on.

### 1.4 What this ADR does *not* try to do

- **No diff endpoint.** PoR § 13 explicitly puts diff out of scope for the research epic; if a "diff between two blob beads" surface lands, that is a P-future ADR.
- **No bridge auto-registration.** The gitlab-bridge populating `produces(ka-blobX)` automatically on MR merge is P8.
- **No formula/ready-queue interaction.** `git-blob` beads are `kind: data` and never enter the work queue; a typed mention `produces(ka-blobX)` from a `parent-child`-mapped verb does *not* materialize a `deps` row (Decision 3 of the typed-mention research keeps typed mentions in `bead_references` only). § 7.4 calls this out so a future P-future ADR doesn't accidentally promote `produces` into `deps` and end up with `git-blob`s blocking ready-queue resolution.
- **No re-extraction trigger on blob registration.** The existing extraction pipeline runs on text writes, not on target-bead creation. With stable IDs (P1 § 3.4) re-extraction is unnecessary; § 7.3 documents why.

## 2. Decision: `origin_bead` field shape and lifecycle

### 2.1 Shape

`origin_bead` is an **optional, single-valued, string** field on `type:git-blob`. Format: a `ka-0000001713` bead ID (the canonical 12-character form including the `kd-` prefix, validated server-side against the same regex used by the references extractor: `^kd-[A-Za-z0-9]{10}$`).

| Property | Value |
|---|---|
| Field type | `string` |
| Required | no |
| Default | empty string |
| Cardinality | exactly one ID, or empty |
| Format validation | `^kd-[A-Za-z0-9]{10}$` when non-empty; rejected otherwise |
| Indexed | no (queryable via the standard JSONB-field filter machinery; see § 6.2) |
| Mutability | first-write-wins on transition empty → set; immutable thereafter |

### 2.2 Why single-valued

The PoR § 6.1 frames the field as "canonical attribution (1:1, set on registration)". Two arguments push us to keep it single-valued rather than promoting it to an array:

1. **There is exactly one bead the agent was working on when it produced the snapshot.** That bead is the canonical owner. Multi-context attribution ("this blob also relates to the epic, and to the security-review bug") is a graph question, not a field question — solve it with a typed-mention or `kd dep add`, not by widening the field.
2. **Single-valued fields stay queryable with the existing filter machinery.** `kd list -f origin_bead=ka-X` is a JSONB equality test; `-f origin_bead in [ka-X, ka-Y]` over an array would need either a containment operator on a JSONB array or a side-table. We pay a real complexity cost to gain a feature (multi-attribution) the typed-mention path already covers cleanly.

### 2.3 Lifecycle

`origin_bead` follows P1 § 3.5's "identity vs provenance" split, and we deliberately put it on the **identity side**:

- On the *first* successful registration of a triple, `origin_bead` may be empty (no attribution provided) or populated.
- On a subsequent upsert that hits the existing bead via the partial unique index:
  - If the existing `origin_bead` is empty *and* the new request supplies a non-empty value → set it. (Cheap "fill-in" semantics; useful when an agent first registers without `--origin-bead` and later adds the link.)
  - If the existing `origin_bead` is non-empty *and* the new request supplies a different value → reject the *write* with a 409 Conflict and a message that points at the existing value. (Canonical attribution is decided once; later "I think it should point at this *other* issue" is a graph operation, not a field-edit.)
  - If the existing `origin_bead` is non-empty *and* the new request supplies the same value or empty → no-op.

This is more conservative than provenance fields like `gitlab_mr_iid` (where last-writer-wins; P1 § 3.5). The conservatism is intentional: once humans see a blob attributed to issue X in lists and constellations, silently flipping that to issue Y on a later upsert would be confusing. The 409 forces an explicit administrator-driven correction (e.g. via a hypothetical `kd admin git-blob set-origin` tool — out of scope for this ADR; flagged in § 8).

### 2.4 What `origin_bead` is and is not

| Is | Is not |
|---|---|
| The single canonical owner bead this blob was produced for. | A general "linked beads" list. |
| Set on registration; settable later only when previously empty. | Editable freely after first set. |
| Rendered as a solid back-edge in the constellation. | A typed mention. (Typed mentions live in prose; this is a structured field.) |
| The default `kd list --group-by` key for `git-blob` beads in the constellation (§ 5.2). | Required. Many one-off blobs have no origin bead. |

### 2.5 Validation: existence vs format

- **Format validation** (regex match) happens server-side at write. Rejection on malformed IDs.
- **Existence validation** (does `ka-0000001713` actually point at a real, non-deleted bead?) does **not** happen at write. Reasons:
  - Cross-fleet references are out of scope (PoR § 1.4) but in-fleet references to a soft-deleted bead would otherwise be a write-time race with another tool's delete.
  - The references extractor already happily records `kd-` IDs that don't resolve; the existence check is a *render-time* concern, not a *write-time* concern. We follow the same convention.
  - Existence-check at write would force a DB lookup on every `git-blob` write that supplies `origin_bead`, doubling the write cost for the common case (it does resolve) to catch the rare case (it doesn't).
- The constellation render can show a "broken back-edge" affordance for `origin_bead` IDs that don't resolve (§ 5.5); the data is preserved, the UI flags the dangler.

### 2.6 Counter-arguments considered

- **"Make `origin_bead` an array."** Rejected per § 2.2.
- **"Add a typed-mention verb instead of a field."** Rejected because: (a) the typed-mention path already exists for *additional* context (§ 3); the PoR explicitly wants both layers, and (b) a structured field is cheaper to filter and group on than a typed-mention reverse lookup. The two layers are complementary, not alternatives.
- **"Make `origin_bead` mutable, last-writer-wins."** Rejected per § 2.3 — operator confusion outweighs the convenience of "I changed my mind about which issue owns this."
- **"Validate existence at write."** Rejected per § 2.5 — write cost without a corresponding correctness benefit.

## 3. Decision: typed-mention verb extensions (resolves **D-7**)

### 3.1 The four verbs to add

P5 adds **two forward/inverse pairs** to `canonicalVerbs`:

| Surface verb | Direction | `model.DependencyType` | Semantics |
|---|---|---|---|
| `produces` | forward (Y → X means *Y produces X*) | `parent-child` | Author bead is the parent (the work that produced the artifact); target bead is the child (the artifact). Typical: written on an issue/MR that points at one of its `git-blob` children. |
| `produced-by` | inverse (Y → X means *Y is produced by X*) | `parent-child` | Author bead is the child (the artifact); target bead is the parent (the work). Typical: written on a `git-blob` that points back at its origin issue (rarely needed because `origin_bead` covers the canonical case, but legitimate for cross-issue attribution). |
| `derives-from` | forward (Y → X means *Y derives from X*) | `parent-child` | Author bead derives from target bead. Typical: a newer `git-blob` for the same `(repo, path)` at a later commit pointing at its predecessor. The predecessor is the *parent* in the lineage chain. |
| `derived-from` | inverse (Y → X means *Y is derived from X*) | `parent-child` | Synonym of `derives-from` for prose ergonomics; the parser canonicalizes both forms to the same direction. |

Note that `derives-from` and `derived-from` have the same forward direction in our model — they're not a true inverse pair like `parent`/`child-of`. We add `derived-from` only because authors will write it; the parser maps both spellings to the same edge. This is a small ergonomic concession; it does **not** generalize (§ 3.5).

### 3.2 Why all four map to `parent-child` (and not new constants)

The closed core today (§ 1.2) maps verbs to `model.DependencyType` constants. Three options for handling the new verbs:

| Option | What it does | Pros | Cons |
|---|---|---|---|
| **A** ★ | Alias `produces`, `produced-by`, `derives-from`, `derived-from` to the existing `parent-child` constant. | Zero schema change. Zero migration. The existing `bead_references.relationship_type` column already stores the constant name; adding new surface forms is one map entry each. Renderers that already understand `parent-child` need no change. The semantic claim ("issue is parent, blob is child" / "newer blob's parent in lineage is the older blob") is a faithful read of `parent-child`. | Two semantically distinct relations (issue→artifact vs blob lineage) collapse into one edge type. Filter ergonomics: `kd refs --type=parent-child` returns both, with no built-in way to slice "just the artifact-attribution edges" without also looking at the source/target types. |
| B | Add new `model.DependencyType` constants `produces` and `derives-from`. Map verbs 1:1. | Filter ergonomics improve — `kd refs --type=produces` is exact. Render styles can diverge per relation. | New constants → schema-level allow-list change → migrations to wherever `DependencyType` is referenced (`deps.type` enum check, validation in `internal/model`). Ripples beyond `bead_references` into the `deps` table even though we are *not* materializing typed mentions into `deps` (Decision 3 of typed-mention research). Operationally non-trivial. |
| C | Hybrid: alias `produces` to `parent-child`, but add a new constant for `derives-from`. | Incrementally expand the schema only where the semantic divergence matters. | Editorial judgement embedded in the type system. Two of the four verbs are "real" types; the other two are aliases. Two-tier mental model is a tax for every future maintainer. |

### 3.3 Decision

**Option A**: alias all four new verbs to `parent-child`. We accept the filter-ergonomics cost in exchange for zero schema change.

### 3.4 Rationale

1. **The PoR § 6.2 explicitly defers the verb question to whichever phase owns it.** When the typed-mention research closed, it shipped exactly the closed core that `model.DependencyType` already had constants for. Adding a new constant after-the-fact is a schema-level migration to a system that already shipped — the cost-benefit ratio is bad for a feature whose primary value is *expressiveness in prose*, not *expressiveness in the dep-type enum*.
2. **The semantics genuinely *are* parent-child.** An issue is the parent of the blobs it produces. A new file version is the descendant of the prior version. We are not contorting the model — we are extending the surface vocabulary that maps to it. The closed core already does this for `parent`/`child-of` (two surface forms, same constant); adding `produces`/`produced-by` is the same pattern.
3. **Filter ergonomics are recoverable without new constants.** § 6.2 introduces the convention `kd refs --verb=produces` (a separate flag from `--type=parent-child`) that filters by *surface verb*. The data already supports this — the parser stores the surface verb on the `TypedReference` and the write path can persist it (proposal: a new `bead_references.surface_verb TEXT NULL` column, or — simpler — keep the surface verb in the existing `relationship_type` field and have the parser map both ways at read-time). The exact storage representation is a P9 decision; the *user-visible filter* is the contract this ADR pins.
4. **Backwards compatibility is automatic.** Prose written today as `produces(ka-X)` is already extracted as a flat mention with `RawVerb="produces"` (per the typed-mention research § 3.4 fallback). Once `canonicalVerbs` gains the verb, the *next* text write on that source bead promotes the flat mention to a `parent-child` typed edge via the existing delete-then-insert lifecycle. An admin re-extraction (`kd admin reextract-references`) accelerates the transition fleet-wide.
5. **`derives-from` and `derived-from` collapse to one direction.** "Y derives from X" reads naturally; "Y is derived from X" reads naturally; they mean the same thing. We accept both spellings and map them to the same edge so an author writing the second form gets the edge they expect rather than a flat mention. This is a small concession; we are *not* generalizing it (§ 3.5).

### 3.5 What we are not doing (and why)

- **We are not generalizing the "two spellings, one direction" trick.** `derives-from` ≡ `derived-from` is a one-off because the verb-stem and its passive form have identical direction in our model. We do not, for example, add `produced-by` ≡ `produces` (those are genuine inverses with reversed direction). Future verb-pair additions follow the existing pattern: forward + inverse, both directions, OR (rarely) a documented spelling alias when the prose grammar makes both natural.
- **We are not adding `supersedes`-mapped variants for `git-blob`.** Tempting analogy: "the new blob supersedes the old blob." We reject it because `supersedes` in the bead model means the predecessor is *no longer authoritative* — a closed bead, conceptually replaced. A `git-blob` is an immutable historical snapshot; its predecessor is not "replaced," it's "the prior version." `parent-child` lineage captures the relationship without the false "the old one is now obsolete" connotation. Operators who want a list of "blobs for this path over time" sort by `committed_at`, not by `supersedes` chains.
- **We are not adding `tests`, `fixes`, or `closes`.** Those verbs were considered and rejected by the typed-mention research (§ 3.5) because they encode editorial judgement (`fixes` → blocks? parent-child?). Same reasoning applies here. Authors who want them get the `RawVerb` fallback path and a flat mention; promoting them is a separate ADR if usage data warrants.
- **We are not adding `embeds` / `references` / `imports`.** Code-relationship verbs are out of scope. Code-aware indexing is explicitly a non-goal of the whole research epic (PoR § 13).

### 3.6 The exact diff to `internal/references/typed.go`

This is the prose form of the change P9 will land. **No code ships from this ADR.**

```go
// (delta to canonicalVerbs, internal/references/typed.go)
var canonicalVerbs = map[string]model.DependencyType{
    "parent":        model.DepParentChild,
    "child-of":      model.DepParentChild,
    "blocks":        model.DepBlocks,
    "blocked-by":    model.DepBlocks,
    "related":       model.DepRelated,
    "duplicates":    model.DepDuplicates,
    "duplicated-by": model.DepDuplicates,
    "supersedes":    model.DepSupersedes,
    "superseded-by": model.DepSupersedes,
    // P5 additions:
    "produces":      model.DepParentChild,
    "produced-by":   model.DepParentChild,
    "derives-from":  model.DepParentChild,
    "derived-from":  model.DepParentChild,
}
```

`IsInverseVerb` extends to recognize the two new inverse forms whose *edge direction* is reversed from the bead's authoring perspective:

```go
func IsInverseVerb(v string) bool {
    switch strings.ToLower(v) {
    case "child-of", "blocked-by", "duplicated-by", "superseded-by",
        "produced-by": // P5 addition
        return true
    }
    return false
}
```

`derives-from` and `derived-from` are **not** in `IsInverseVerb` — they are forward-direction synonyms (§ 3.4 rationale). The write path emits `Y --parent-child--> X` for both spellings (where Y is the bead being authored and X is the cited target).

### 3.7 Re-extraction story

When the closed-core map gains the new verbs, existing prose written with the verbs is already in `bead_references` as flat mentions (with `relationship_type=NULL`). To promote them to typed edges:

- **Per-bead automatic**: any text write on a source bead containing the verb triggers the standard delete-then-insert cycle and writes the typed row.
- **Fleet-wide retroactive**: run `kd admin reextract-references` (already an existing command per `kbeads/CLAUDE.md`'s `BEADS_REFERENCES_EXTRACT_COMMENTS` doc) which walks every bead, re-extracts, and replaces `bead_references` rows.

The retro-promote is operator-discretion; nothing breaks if it never runs. P9 should call this out in its rollout notes.

## 4. Decision: graph-edge layering (the "all three, layered" PoR § 6.1 rule)

The PoR proposes three ways to link an issue to its blobs (typed mention, explicit dep, `origin_bead` field). P5 confirms all three exist concurrently and fixes the precedence and rendering rules so they don't collide.

### 4.1 The three layers

| Layer | Where it lives | Authoring path | Renders as |
|---|---|---|---|
| **`origin_bead` field** | `beads.fields->>'origin_bead'` on the `git-blob` bead. | Set at registration: `kd git-blob add --origin-bead ka-issue ...`. | Solid back-edge `blob → origin` in the constellation; "Produced blobs" section on the origin bead's show page (§ 5.3). |
| **Typed mention** | `bead_references.relationship_type` rows. | Author writes `produces(ka-blobX)` (or `produced-by(...)`, `derives-from(...)`) in any field of any bead. Extractor populates the row on text-write. | Dashed, color-coded by verb (Decision 4 of typed-mention research) in the constellation; "Mentions" / "Mentioned by" sections on bead show pages with the verb labelled (§ 5.4). |
| **Explicit dep** | `deps` rows. | Operator runs `kd dep add A B --type=parent-child` (or any other dep type). | Solid edge in the constellation, dep-styled per the existing renderer. |

### 4.2 Precedence rules

The three layers describe (potentially) overlapping graph state. We do **not** dedupe them at the data layer — each layer has its own provenance and lifecycle. The renderer dedupes at *display* time:

1. If `origin_bead` is set on a `git-blob` *and* a typed mention `produced-by(ka-origin)` exists from the same blob to the same origin, the constellation renders **one** solid edge (the `origin_bead` form), not both. The typed-mention edge is not deleted; it simply doesn't draw a second line over the first.
2. If an explicit `deps` row `(blob, origin, parent-child)` exists *and* `origin_bead` points at the same origin, the constellation renders **one** solid edge, attributed to the field (because `origin_bead` is more specific than a generic `parent-child` dep).
3. If a typed mention and an explicit dep both exist for the same `(source, target, relationship_type)`, both render — the dep gets the heavyweight style (Decision 4 of typed-mention research), the typed mention gets the visually-distinct style. This is the existing typed-mention/`deps` overlap behaviour; nothing new here.

These rules apply only to *rendering*. The underlying rows are unchanged. § 5.5 spells out how this manifests in the viewer.

### 4.3 What this means for authors

- **Most agents** registering a blob set `--origin-bead` and write nothing else. Done.
- **Cross-context attribution** (a blob also relates to a different epic) → a typed mention from the epic to the blob: `produces(ka-blobX)`.
- **Lineage** (a blob succeeds a prior blob for the same path) → `derives-from(ka-priorBlob)` written on the new blob's notes (the agent that registers the blob can include this in `kd git-blob add --notes`).
- **Operator-imposed graph structure** (manual override of the auto-assigned attribution) → `kd dep add` as today; the `origin_bead` field stays set, the dep adds a new edge.

### 4.4 Counter-argument considered

**"Just use `origin_bead`; skip the typed-mention layer entirely for `git-blob`s."** Rejected because the PoR § 6.1 explicitly wants the typed-mention layer for *additional* context beyond the canonical owner, and because cross-blob lineage (`derives-from`) is a graph relationship that has no analog in `origin_bead`. Removing the typed-mention layer would either lose lineage edges entirely or force agents to use `kd dep add`, which doesn't compose with the prose-driven workflow the typed-mention research was built around.

## 5. Decision: beads-viewer rendering rules

These rules apply to two viewer surfaces in `gardenio/controller/cmd/beads-viewer/`:

- The **constellation** (`constellation.go` + the SVG/canvas render).
- The **bead show page** (`bead_preview.go`, `bead_show_test.go`).

### 5.1 Inherited from typed-mention Decision 4

Typed-mention edges (including the new `produces`/`derives-from` family, all currently aliased to `parent-child`) render **visually distinct** from same-typed `deps` rows: dashed lines, color-coded by canonical verb (the `relationship_type` value), with the `source: "text"` marker on the edge metadata. P5 inherits this verbatim — no new rendering rule for typed-mention `git-blob` edges. The renderer's existing color palette is reused; we recommend (but do not pin) a single muted color family for the four new `parent-child`-mapped surface verbs (`produces`, `produced-by`, `derives-from`, `derived-from`) so they are visually distinguishable from the existing `parent`/`child-of` edges *if* the renderer keys color on surface verb, and identical if it keys color on `relationship_type`. Either choice is consistent with this ADR; the call belongs to the viewer impl PR.

### 5.2 `git-blob` node glyph

A `git-blob` node in the constellation renders as:

- **Glyph**: a small file-icon-shaped node (distinct from the circle/square used by `issue`/`epic` kinds), with the path *tail* (the segment after the last `/`) and the first 7 chars of `commit_sha` rendered inline: e.g. `config.go @ deadbee`.
- **Tooltip / hover**: full path, full SHA, normalized `repo`, `file_size`, `language`, `secrets_redacted` indicator.
- **Click**: opens the bead show page (existing routing), which presents the blob detail panel (§ 5.3).
- **Default group**: when `origin_bead` is set, the blob is rendered "near" its origin bead in the layout (the origin bead is the layout-graph parent for layout purposes). When `origin_bead` is empty, the blob floats unanchored.

We deliberately do **not** show the file content in the constellation. The blob node is a navigation handle; the detail lives on the show page or on a hover panel that `kbeads` paginates lazily.

### 5.3 `git-blob` show page

The blob's show page (already routed because every bead gets one) gains:

- **Metadata block**: normalized `repo`, full `commit_sha` (with copy-to-clipboard), full `path`, `blob_sha`, `file_size`, `encoding`, `language`, `mime_type`, `truncated`, `secrets_redacted`, `secrets_summary` (rendered as a list of redactions if non-empty, with rule + line + replacement marker but never the original secret).
- **Content block** (when readable): the file body, syntax-highlighted by `language`. Tier-A blobs render inline; Tier-B and Tier-C blobs render after a "fetch content" click (so the show page doesn't pull megabytes by default). Binary blobs (`encoding ∈ {base64, truncated-base64}`) render the metadata only with a "binary content — download" affordance. `encoding=omitted` blobs render no content section.
- **Provenance block**: `origin_bead` (rendered as a `kd-` link with the target's title and kind), `branch`, `commit_message` (first line; full message available via expand), `commit_author`, `gitlab_project_id`, `gitlab_mr_iid`, `source`.
- **Lineage block**: `prior_blob_id` if set; "next versions" section listing any `git-blob` beads where this blob is the `prior_blob_id` or where a typed mention `derives-from(this-blob)` exists.

The blob show page is read-only. There is no inline edit. (Identity is immutable per P1; provenance edits go through the API, not the viewer.)

### 5.4 Origin bead's show page

When a non-`git-blob` bead has `git-blob`s pointing back at it via `origin_bead`, the show page gets a new section:

- **"Produced blobs"**: a compact list of `(repo, path-tail, short-SHA, file_size, secrets_redacted-badge)` rows, sortable by `committed_at` (default desc). Click → blob show page.
- **Visual separation from "Mentions" / "Mentioned by"**: those two sections continue to list typed and untyped mentions including any `produces`/`produced-by`/`derives-from` typed edges. The "Produced blobs" section is *only* for `origin_bead` back-edges, so the operator can tell at a glance "blobs the agent registered against this issue" vs "blobs that mention this issue from elsewhere".

### 5.5 Constellation default: `git-blob`s hidden

The constellation by default **does not** render `git-blob` beads. Reasons (PoR § 6.3 already flags this):

- Volume: a single MR may produce 5–50 blobs; rendering them all by default makes the global constellation unreadable.
- Relevance: most operators viewing the constellation want the issue/epic graph, not the artifacts.
- Performance: hiding by default at the API layer (`/v1/graph?include_data_kind=false`) lets the server skip the join and the response payload stays small.

The hide-by-default is *not* a viewer-only concern. It is implemented at `/v1/graph` as a query parameter:

```
GET /v1/graph?include_data_kind=false       # default
GET /v1/graph?include_data_kind=true        # opt in to data-kind beads (git-blob, future data-kind types)
GET /v1/graph?include_types=git-blob        # opt in to a specific data-kind type
```

Both the existing beads-viewer constellation and any future consumer (beads3d, ad-hoc skills, gardenio/controller surfaces) inherit the same default. The viewer adds a UI toggle for "show data-kind beads" that flips the query parameter.

When `include_data_kind=true` is set, a `secrets_redacted=true` blob node renders with a small inline badge so the operator immediately sees the redaction status without opening the show page.

### 5.6 Broken `origin_bead` reference

If `origin_bead` points at a `kd-` ID that does not resolve (deleted, never existed, cross-fleet), the show page renders the field with a "broken-link" affordance: the literal ID is shown, marked dimmed, with a tooltip "this bead no longer exists or is not visible from your fleet." The constellation simply omits the back-edge (no visible dangler). The data row is preserved.

### 5.7 What this ADR does *not* fix in the viewer

- The exact icon SVG, colors, font sizes, etc. — those are viewer-impl-PR decisions.
- Whether the "Produced blobs" section paginates or shows all rows. Recommendation: paginate at 50 with "show more"; pin the recommendation in P9, not here.
- The diff UI (out of scope per PoR § 13).
- The "fetch content" UX for Tier C (presigned-URL flow vs server-mediated proxy). P4 owns the API; the viewer follows.

## 6. Decision: default list filters

### 6.1 Inherited default: `kind: data` excluded

The existing `kd list` and `/v1/beads` endpoints already apply a default filter: `kind: data` beads are excluded unless explicitly requested. P0 locked `git-blob`'s kind to `data`, so this default *already covers* the noise concern: a casual `kd list` does not surface blobs.

P5 does **not** introduce a `git-blob`-specific default filter on top of the kind filter. The kind-level filter is the right level of abstraction; new data-kind types added later (e.g. embedded-payload beads, formula data beads) inherit the same exclusion automatically.

### 6.2 Explicit-include rules

When the caller specifies a type, the kind filter is implicitly relaxed for that type. This is already true of the `kd list` filter machinery; P5 documents the contract:

| Command | Behaviour |
|---|---|
| `kd list` | `kind: data` excluded by default → no `git-blob`s. |
| `kd list --kind=data` | All data-kind beads included. |
| `kd list --type=git-blob` | `git-blob` beads included even though they are `kind: data`. (`--type` overrides the default kind filter for that type.) |
| `kd list --type=git-blob -f repo=gitlab.com/example-org/gardenio` | Filter further by JSONB field. Existing filter machinery; P5 just documents the field name set. |

The documented filter shortcuts P9 should ensure work cleanly:

| Shortcut | Meaning |
|---|---|
| `-f repo=<repo>` | Equality on normalized `repo` (case-insensitive, matching the partial unique index's `LOWER(...)` semantics). |
| `-f repo_id=<provider:id>` | Equality on `repo_id`. |
| `-f commit_sha=<sha>` | Equality on full 40-char `commit_sha`. |
| `-f path=<path>` | Equality on `path`. |
| `-f path-prefix=<prefix>` | Prefix match on `path` (e.g. `internal/server/`). New shortcut; sketch only. |
| `-f origin_bead=<ka-id>` | Equality on `origin_bead`. |
| `-f language=<lang>` | Equality on `language`. |
| `-f secrets_redacted=true` | Equality on `secrets_redacted`. Useful for "find every redacted blob". |

The exact shortcut spelling (`-f path-prefix=` vs `-f path:prefix=` vs `--path-prefix=`) is a P4/P9 concern; what P5 fixes is the *set* of filter dimensions that should be supported and that they must operate on the JSONB fields directly without requiring a side index.

`kd refs` filters extend in lock-step:

| `kd refs` invocation | Behaviour |
|---|---|
| `kd refs <ka-issue> --type=parent-child` | All `parent-child` typed-mention edges from the issue (existing). After P5: includes `produces`/`derives-from`-mapped edges. |
| `kd refs <ka-issue> --verb=produces` | Filter by *surface verb* (P5 addition, recommended in § 3.4). Returns only edges the parser canonicalized as `produces`. |
| `kd refs mentioned-by <ka-blob>` | Reverse lookup; existing. Lists every bead that typed-or-untyped-mentions the blob. |
| `kd refs mentioned-by <ka-blob> --verb=produced-by` | Reverse, surface-verb filter. (Author beads that wrote `produces(...)` end up reachable as `mentioned-by --verb=produces` from the blob's perspective; the inverse spelling `produced-by` is also valid for prose written from the blob's side.) |

The `--verb=` flag is the bridge between the surface vocabulary and the canonical `relationship_type`. Without it, callers can only filter by the underlying constant (`--type=parent-child`), which under-specifies. P9's exact storage for "surface verb" can be:

- **Option α**: a new `bead_references.surface_verb TEXT NULL` column. Cleanest; one more column.
- **Option β**: lift surface verb into `relationship_type` itself for the new `git-blob`-related verbs (i.e. store `relationship_type = 'produces'` instead of `'parent-child'` for those edges). Cheaper; muddies the constant invariant established by the typed-mention research. Not recommended.

P5 recommends **option α** but does not pin it; P9 may revisit if column count is a concern.

### 6.3 Constellation default mirrors the list default

`/v1/graph` defaults to `include_data_kind=false` (§ 5.5). The opt-in `include_types=git-blob` parameter lets a viewer surface the issue + its produced blobs without flipping the global default.

### 6.4 Counter-arguments considered

- **"Add a `--no-blobs` flag for `kd list`."** Unnecessary: the kind-level default already excludes them.
- **"Make `kd list --type=git-blob` require an extra `--include-data` flag."** Rejected: violates the principle that specifying a type relaxes the kind default for that type. Would force every blob-aware caller to learn a separate flag.
- **"Hide `git-blob`s from the `kd list` even with `--type=git-blob` unless `--explicit-blobs` is also set."** Rejected; the user explicitly asked for them.

## 7. Edge cases and interactions

### 7.1 Blob registered with `origin_bead` pointing at itself

Reject at write. Self-attribution is meaningless and would render as a self-loop in the constellation. Validation: `origin_bead != <new bead's own ID>`. The new bead's ID is generated server-side before the JSON is persisted, so this check happens *after* ID generation, *before* the upsert hits the unique index.

### 7.2 Two blobs claim the same `origin_bead`

Allowed. An issue legitimately produces many blobs. The "Produced blobs" section on the origin bead's show page lists them all.

### 7.3 Typed mention to a not-yet-registered blob

Allowed. The references extractor records the row with `target_bead_id = ka-blobX` regardless of whether `ka-blobX` resolves. When the blob registers later (with the same `ka-blobX` ID — guaranteed by P1's stable-ID upsert if the registration succeeds first; *not* guaranteed if a different agent registers a different blob with that ID — but ID collisions across registrations require a nanoid collision, which the existing `idgen` handles by retry), the row is already correct. The renderer resolves the target lazily.

We do **not** add a "blob registration triggers re-extract of all references" mechanism. The existing pipeline already handles this case correctly — text writes are extracted at write-time, and the resulting rows persist through target-bead lifecycle changes.

### 7.4 `parent-child`-mapped typed mention does *not* materialize a `deps` row

Decision 3 of the typed-mention research: typed mentions live only in `bead_references`, never in `deps`. P5 inherits this verbatim. Consequences for `git-blob`:

- `produces(ka-blobX)` written on an issue does **not** make the blob block ready-queue resolution of the issue. (`git-blob`s are `kind: data` and never enter the work queue anyway, but this is the structural reason — the typed mention itself is a derived layer, not a work-graph assertion.)
- `kd ready` and formula-expansion logic that reads `deps` see no change.
- An operator who *wants* the blob to affect the work graph runs `kd dep add` explicitly (rare, expected only for narrow cases).

### 7.5 Blob soft-deleted while `origin_bead` still references it

The origin bead's "Produced blobs" section excludes soft-deleted blobs by default (existing soft-delete filter applies). A `?include_deleted=true` query parameter on the show page surfaces them with a "deleted" affordance. The `origin_bead` field on the deleted blob is preserved through soft-delete (no field mutation on delete).

### 7.6 Origin bead soft-deleted while blob still exists

The blob's show page renders the `origin_bead` link as broken (§ 5.6). The blob remains queryable. No GC cascade — `git-blob` lifecycle is P7's call; for now the blob simply orphans cleanly.

### 7.7 Cross-fleet `origin_bead`

`ka-XXX` IDs are unique within a fleet, not globally. A blob registered with an `origin_bead` that was minted in another fleet renders as a broken link in the local fleet. We do not attempt cross-fleet resolution (PoR § 1.4 puts cross-fleet out of scope). Operators who deliberately replicate beads across fleets must accept that `origin_bead` references may dangle on the consuming side.

### 7.8 Renamed origin bead title

`origin_bead` stores the ID, not the title. Title changes are picked up at render time via the standard bead resolution. No drift.

### 7.9 Self-loop typed mentions

A bead that writes `produces(<its-own-id>)` in its description would record a self-edge. The existing typed-mention extractor allows this (it allows any valid `kd-` ID); the renderer treats self-edges per the existing constellation rules (typically a small ring loop, or omitted for clarity). P5 does not add a new self-loop check — the existing behaviour applies.

## 8. Consequences

### 8.1 For P6 (embedding)

- `git-blob.content` is the embedding source for text blobs (`encoding ∈ {text, truncated-text}`). P6 decides the per-repo opt-in policy (D-8). P5 does not affect the embedding shape.
- Typed-mention edges *to* a `git-blob` from a non-blob bead may inform retrieval (e.g. "find the blob produced by this issue"), but that is a P-future search-skill concern; P5 does not pin retrieval semantics.

### 8.2 For P7 (lifecycle / GC)

- The "orphan blob" definition the PoR § 9.2 hints at — a blob with no incoming references — must include the `origin_bead` back-edge as an *incoming reference* for the purpose of orphan detection. A blob with `origin_bead = ka-X` and no other links is *not* orphan-eligible; it has a structured back-edge. P7 codifies this; P5 only flags it.
- Soft-deleted-and-re-registered blobs (P1 § 5.1) get a fresh ID and a fresh `origin_bead` (the operator must re-supply it on the new registration). The old soft-deleted bead's `origin_bead` is unchanged; no copy-forward.

### 8.3 For P8 (bridge auto-registration)

- The gitlab-bridge, when it auto-registers blobs on MR merge (P8's call to make), should set `origin_bead = <issue-bead-of-MR>` and `source = gitlab-bridge`. The bridge does **not** write typed mentions on the issue or the blobs — agent-authored prose is the channel for typed mentions; the bridge just sets fields.
- If the bridge registers a blob that the agent had already pre-mentioned in prose (`produces(ka-blobX)`), the typed-mention edge resolves naturally — no bridge-side coordination needed (§ 7.3).

### 8.4 For P9 (implementation)

P9 lands four sets of changes; none of them ship a migration:

1. **`internal/references/typed.go`** — extend `canonicalVerbs` with the four new entries (§ 3.6) and `IsInverseVerb` with `produced-by`.
2. **`internal/server/config.go`** — register `origin_bead` as an optional `string` field on `type:git-blob` (P0 already lists it; P5 confirms the format-validation regex and the first-write-wins lifecycle from § 2.3).
3. **Server-side handlers** — implement the lifecycle rules in § 2.3 (409 on identity-conflict, fill-in on empty), the `origin_bead != self` validation in § 7.1, and the `/v1/graph` query parameter `include_data_kind` defaulting to `false`.
4. **Viewer** — extend `constellation.go` with the data-kind toggle and `git-blob` glyph; extend `bead_preview.go` with the "Produced blobs" section, the metadata/content/provenance/lineage blocks on the blob show page, the `secrets_redacted` badge, and the broken-link affordance for unresolved `origin_bead`.
5. **CLI** — `kd refs --verb=` flag (existing `kd refs --type=` continues to filter by `relationship_type`; the new `--verb=` filters by surface verb). Document the JSONB-field filter shortcuts in `kd list --type=git-blob` help text.

P9 also runs `kd admin reextract-references` once after the closed-core-map change ships, to retro-promote prose-written `produces`/`derives-from` mentions into typed edges. This is a one-time operational task, not a migration.

### 8.5 For operators

- A new `git-blob` bead does not pollute the default `kd list` (kind filter does the right thing).
- The constellation stays readable by default; opt-in to see blobs.
- Authors writing `produces(ka-blobX)` or `derives-from(ka-priorBlob)` in prose get typed edges as soon as the closed-core map ships, automatically — no behavior change required for prose written before the change.
- `kd refs <issue> --verb=produces` becomes the canonical way to ask "what artifacts did this issue produce?" — and is composable with the `mentioned-by` direction.

### 8.6 For the eventual D-7 `gb decision`

The decision the operator resolves is whether to accept the four-verb addition (§ 3.1) and the alias-to-`parent-child` strategy (§ 3.3). Three failure-mode options the operator might pick instead:

| Operator choice | Consequence |
|---|---|
| Accept defaults. | P9 lands the diff in § 3.6 verbatim. |
| Accept the verbs but reject the `parent-child` aliasing (i.e. force option B from § 3.2). | P9 lands new `model.DependencyType` constants (`produces`, `derives-from`); migrations and validation expand accordingly. ~1 extra day of P9 work. |
| Reject the new verbs entirely. | `produces`/`derives-from` stay as flat untyped mentions (current behaviour). Authors lose the typed-edge promotion. The `origin_bead` field still lands; the `produced-by` reverse-lookup gets less expressive. |
| Add a different verb set (e.g. `attaches`, `outputs`). | Same shape of change, different keys in `canonicalVerbs`. P5 is agnostic on the exact words; § 3.1 is a recommendation, not a hard requirement. |

## 9. What this ADR does *not* decide

To keep this ADR focused on D-7 and the graph-integration spec, the following are explicitly **out of scope** and deferred:

| Item | Owning phase | Question |
|---|---|---|
| D-4 (storage tiering) | P2 | Settled — see P2 ADR. |
| D-6 (security/redaction) | P3 | Settled — see P3 ADR. |
| D-8 (embedding policy) | P6 | Whether `git-blob.content` is embedded by default. |
| Orphan GC, lifecycle transitions | P7 | When a blob is eligible for soft-delete; refcounting in `git_blob_content`. |
| CLI/API exact RPC shapes | P4 | The wire-format for `--verb=`, the `--from-file` ergonomics, the upload chunking. |
| Bridge auto-registration | P8 | Whether/when the gitlab-bridge writes blobs on MR merge. |
| Diff endpoint / diff UI | (P-future) | Out of scope per PoR § 13. |
| Cross-fleet federation | (P-future) | Out of scope per PoR § 1.4. |
| Code-aware indexing (AST, callgraph) | (P-future) | Out of scope per PoR § 13. |

Also out of scope:

- The `kd admin git-blob set-origin` tool for operator-driven `origin_bead` corrections (§ 2.3) — operational follow-up, not a research deliverable.
- Surface-verb storage column shape (option α vs β in § 6.2) — P9 can revisit; P5 recommends but does not pin.
- Render colors/icons/typography (§ 5.7) — viewer-impl-PR concerns.
- Promoting `tests`/`fixes`/`closes` to closed core (§ 3.5) — separate ADR if usage data warrants.

## 10. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The `gb decision` for D-7 is resolved by the operator (default: accept the four-verb additions and the `parent-child` aliasing) — to be raised after the ADR MR opens, per the "decisions surface as `gb decision` checkpoints once research starts" pattern from PoR § 10.
3. P6 (embedding) and P7 (lifecycle/GC) can begin (or continue) against the graph-shape locked here.

This ADR is **not** the green light to ship the closed-core verb diff, the `origin_bead` server-side validation, the `/v1/graph` `include_data_kind` parameter, or any viewer changes. That is P9, after the operator authorizes the implementation epic per PoR § 11.

## 11. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (MR !1973, bead ka-0000003043). § 6 (graph integration), § 9.1 (lifecycle), § 10 D-7.
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442). § 4 (required fields), § 7.2 (P9 sketch with `origin_bead`).
- P1 ADR: `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044). § 3 (bead-id strategy & upsert), § 5.1 (consequences for P5).
- P2 ADR: `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323). Tier definitions; relevant for the show-page lazy-fetch (§ 5.3).
- P3 ADR: `kbeads/docs/adr/git-blob-P3-security-redaction.md` (ka-0000007332). `secrets_redacted` lifecycle; relevant for the constellation badge (§ 5.5).
- Companion (closed) typed-mention research: `kbeads/docs/research-typed-mention-syntax.md` (epic ka-0000001333). Closed-core verbs; Decision 4 (typed-mention-vs-deps render distinctness).
- Implementation precedent:
  - `kbeads/internal/references/typed.go` — `canonicalVerbs`, `IsInverseVerb`, `verbCallPattern`, `verbColonPattern`, `ExtractTyped`.
  - `kbeads/internal/store/postgres/migrations/013_bead_references_relationship_type.up.sql` — typed-mention storage.
  - `kbeads/internal/store/postgres/migrations/009_soft_delete.up.sql` — soft-delete predicate (relevant for § 7.5–7.6).
  - `kbeads/CLAUDE.md` — typed-mention syntax section, `BEADS_REFERENCES_EXTRACT_COMMENTS` opt-in for comment-source extraction.
- Reference-extraction precedent: `kbeads/docs/design-bead-references.md`.
- Viewer surfaces: `gardenio/controller/cmd/beads-viewer/constellation.go`, `gardenio/controller/cmd/beads-viewer/bead_preview.go`, `gardenio/controller/cmd/beads-viewer/bead_show_test.go`.
