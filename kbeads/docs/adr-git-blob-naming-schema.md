# ADR: `git-blob` Bead Type — Naming, Kind, Schema, Write Policy

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p0-gitblob-adr-zek
**Phase**: P0 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000006442
**Supersedes**: nothing (new type)
**Depends on**: plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (MR !1973)
**Resolves decisions**: **D-2** (write policy), **D-5** (kind)
**Defers**: D-1 (repo identity, → P1), D-3 (bead-id strategy, → P1), D-4 (storage tiering, → P2), D-6 (secrets, → P3), D-7 (typed-mention verbs, → P5), D-8 (embedding policy, → P6)

## TL;DR

This ADR fixes the **type-config registration shape** for the new `git-blob` bead type so the rest of the research epic (P1–P8) has a stable contract to design against. It chooses:

- **Type name**: `git-blob` (kebab-case, vs `git_blob` / `gitblob` / `blob` / `code-snapshot`).
- **Kind**: `data` (not `issue`) — `git-blob` is an immutable historical record, not work.
- **Required fields**: `repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`.
- **Optional fields**: `content`, `content_ref`, `language`, `mime_type`, `truncated`, `secrets_redacted`, `secrets_summary`, plus a provenance bundle (`commit_message`, `commit_author`, `commit_committer`, `committed_at`, `branch`, `parent_commit_sha`, `prior_blob_id`, `gitlab_project_id`, `gitlab_mr_iid`, `source`, `origin_bead`).
- **Write policy**: `project_scoped: true`, `escalatable: true`, no `min_role`. Authorization is gated by project label + a server-side secret scanner (designed in P3), not by a global admin role.

This ADR does **not** ship code. It is the spec the eventual P9 implementation will register at `kbeads/internal/server/config.go` next to `type:formula`, `type:molecule`, `type:audit`, etc. — using the existing `model.TypeConfig` machinery (`kbeads/internal/model/type_config.go:54-59`).

The two operator-facing forks resolved here (D-2 and D-5) are surfaced in a single `gb decision` so the operator either accepts the recommended defaults or sends back a specific override. All other decisions in the plan-of-record are **deferred** to their owning phase to keep this ADR small and reviewable.

## 1. Context

The plan-of-record (`kbeads/docs/research-git-blob-bead-type.md`) proposes a new bead type that snapshots `(repo, commit_sha, path) → file contents` so agent-produced code becomes a first-class node in the bead graph. The plan-of-record decomposes the design space into 8 decisions (D-1…D-8) and 9 phases (P0…P8 + a separate operator-authored P9 implementation epic).

P0 — this ADR — is the *naming and registration* phase. Without a fixed type name and a fixed kind, every later ADR has to hedge on whether it is talking about an `issue`-kind bead (which would clutter `kd ready`, status transitions, the work board) or a `data`-kind bead (which sits beside `formula`, `audit`, `escalation`). And without a fixed required-field set, every later ADR has to re-litigate which fields are part of the natural key.

**Why fix these now and not later.** Because the rest of the research epic — identity (P1), storage (P2), security (P3), CLI (P4), graph (P5), embedding (P6), lifecycle (P7), bridge (P8) — all branch on the answers. P1's uniqueness-index ADR is meaningless without knowing which fields are required. P3's secret-scanner ADR is meaningless without knowing whether secret-redaction is a write-time admin override or a project-scoped default. Settling P0 first costs one short ADR; deferring P0 costs cross-cutting churn in every later ADR.

**What the existing built-in type configs look like** (`kbeads/internal/server/config.go`):

- `type:formula` — `kind: "data"`, `write_policy: {project_scoped: true, escalatable: true}`, fields are mostly `json` blobs.
- `type:molecule` — `kind: "issue"`, no write_policy, three identifying fields.
- `type:audit` — `kind: "data"`, `write_policy: {min_role: "gardenio-admin"}`, required `actor`/`action`/`outcome`.
- `type:memory` — `kind: "data"`, no write_policy, large field set including provenance and lifecycle.
- `type:escalation` — `kind: "data"`, required operation fields.

`git-blob` belongs in this `data`-kind family. The default for new types is `kind: "issue"` (work-tracking), which is the wrong default here — see § 3.2.

## 2. Decision: type name → `git-blob`

### Options considered

| Name | Pros | Cons |
|---|---|---|
| `git-blob` ★ | Mirrors the git-internals term ("blob" = file contents at a commit). Kebab-case matches every other multi-word type prefix in `config.go` (`escalation`, `audit`, `memory`, `runbook` are single tokens; multi-token examples in proto fields use snake_case but type *names* are always single tokens or kebab). |
| `git_blob` | snake_case matches field names. | Inconsistent with bead-type convention — every existing built-in type registration uses a single kebab-able token (`formula`, `molecule`, `audit`, `gate`, `message`, `memory`, `runbook`, `mention`, `artifact`). Adding the first snake-cased type would invite drift. |
| `blob` | Shortest. | Ambiguous — "blob" is a generic term and a postgres datatype. Loses the `git-` prefix that signals "this came from a git repo, not from a binary upload". |
| `code-snapshot` | Self-documenting. | Misleading — the type is content-addressed by `(repo, commit, path)`, not by "this is code" (it can store YAML, markdown, anything). The git-internals term is more precise. |
| `gitblob` | One token. | Loses readability; no precedent in the type registry for unhyphenated multi-word names. |

### Decision

Use **`git-blob`**. Registered as `type:git-blob` in `kbeads/internal/server/config.go` (P9, not this ADR).

### Rationale

1. **Term of art.** "Blob" in git is exactly the thing this type stores: the byte contents of a file at a specific tree entry. Operators familiar with git internals already know what a blob is.
2. **Matches existing kebab convention** for the rare multi-word type slots that may appear later (`type:git-blob` reads cleanly next to `type:formula`).
3. **Future-proofing.** If we later add `git-tree` or `git-commit` beads (explicitly out of scope per plan-of-record § 1.5), the `git-` prefix is already idiomatic. We are not committing to those types — only ensuring the name does not box them out.

This naming is **not** an operator decision — it is a default that any later phase can revise via a follow-up ADR if a stronger argument appears, but the current research surfaces no such argument. It is recorded here so all later ADRs can refer to a single canonical name.

## 3. Decision: kind → `data` (resolves **D-5**)

### 3.1 Options considered

| Kind | Behavior | Fit for `git-blob` |
|---|---|---|
| `data` ★ | Sits beside `formula`, `molecule`, `audit`, `memory`. Excluded from default work-board / `kd ready` / `kd list --status=open` work-tracking flows by convention. No `in_progress` semantics. Status transitions still legal but rarely meaningful. |
| `issue` | The work-tracking default. Visible on the operator's work board. `kd ready` surfaces it. Status transitions matter (`open` → `in_progress` → `closed`). |
| `config` | Reserved for fleet-/system-level config beads. |

### 3.2 Decision

**`kind: "data"`**.

### 3.3 Rationale

A `git-blob` bead is an immutable historical snapshot. It is not work, it has no assignee, no status progression beyond "exists", and it should not pollute the operator's open-work view. The plan-of-record § 9.1 already calls this out:

> "`git-blob` beads are immutable historical records. Default: status `open` on creation. Stays `open` indefinitely. … Closing is allowed but discouraged. … No `in_progress` semantics (it's data, not work). Alternative: introduce `kind: data` for `git-blob` (instead of `issue`) so it doesn't pollute the work-tracking views. Recommended."

`kind: data` puts `git-blob` next to `formula`, `audit`, `memory`, `escalation`, and `gate` — all of which are similarly "this is a record of something, not a thing to work on". The same `Bead` struct, store, event-recording, dependency machinery applies — kind is a single discriminator that controls UI and `kd ready` ergonomics, not a code-path branch.

### 3.4 Consequences of `kind: data`

- `kd ready` and the default `kd list` views must filter out `git-blob` (already the convention for `data`-kind beads).
- The beads-viewer constellation should default-hide `git-blob` nodes (echoed by plan-of-record § 6.3); the rendering ADR is P5, not this one.
- Closing a `git-blob` is technically allowed but operationally rare — only when a repo is hard-deleted upstream (plan-of-record § 9.3).
- A `git-blob` cannot be the parent of work; if an operator wants to link work *to* a blob, the work bead is the parent (`issue` → `data`), not the other way around.
- Lifecycle GC (P7) inherits the existing 90-day orphan soft-delete pathway from migration 009, which already handles `data`-kind beads.

### 3.5 Counter-arguments considered

- **"`issue` would let operators see what code agents touched."** Filtering and reverse-lookup queries already do that without forcing the bead onto the work board. `kd refs <issue-id>` (and the beads-viewer constellation) can surface the linked `git-blob` beads on demand.
- **"Status transitions are meaningful — open=current, closed=superseded by a newer commit."** Plan-of-record § 9.1 explicitly rejects this: a newer snapshot is a *different* bead with a different `(repo, commit_sha, path)` triple. Marking the old one "closed" implies it is wrong, not just older. Use the typed-mention verb `superseded-by` (P5) instead.

## 4. Decision: required and optional fields

This section locks the field set so P1 (identity), P2 (storage), P3 (security) can design against a stable shape. Field *types* below mean `model.FieldType` values from `kbeads/internal/model/type_config.go:6-17`.

### 4.1 Required fields

| Field | Type | Why required | Notes |
|---|---|---|---|
| `repo` | `string` | Without it, `(commit, path)` collides across forks. | Normalization rules deferred to P1 / D-1. |
| `commit_sha` | `string` | Identity. | Full 40-char lowercase hex; reject short SHAs at the API. |
| `path` | `string` | Identity. | POSIX, no leading `/`, UTF-8. |
| `blob_sha` | `string` | Lets P2 dedup content storage by hash; lets P5 detect "same file, same bytes" across snapshots. | Computed server-side on ingest (per plan-of-record § 2.2), not supplied by client. |
| `file_size` | `integer` | Needed by storage-tier policy (P2) without reading content. | Bytes. |
| `encoding` | `enum` | Determines how to decode `content` / whether content was offloaded / whether content was omitted. | Values: `text`, `base64`, `truncated-text`, `truncated-base64`, `omitted`. |

The first three (`repo`, `commit_sha`, `path`) form the natural key per plan-of-record § 2.1. The remaining three (`blob_sha`, `file_size`, `encoding`) are required because every consumer needs them; making them optional would force every list query and every storage decision to special-case a "we don't know" branch.

`content` and `content_ref` are *not* required because P2 may decide to offload large blobs or omit content entirely (`encoding=omitted`). Required content would make that impossible.

### 4.2 Optional content fields

| Field | Type | Notes |
|---|---|---|
| `content` | `string` | Inline body, encoded per `encoding`. Subject to P2 size cap. Mutually exclusive with `content_ref`. |
| `content_ref` | `string` | When content is offloaded (e.g. S3 URL, content-addressed-store key). Mutually exclusive with `content`. |
| `language` | `string` | Auto-detected from path/extension. Optional override. |
| `mime_type` | `string` | Auto-detected. |
| `truncated` | `boolean` | True when content was capped. |
| `secrets_redacted` | `boolean` | True when scanner found and stripped patterns. |
| `secrets_summary` | `json` | Redaction record `[{rule, line, replacement}]`. Never the raw secret. |

### 4.3 Optional provenance fields

| Field | Type | Notes |
|---|---|---|
| `commit_message` | `string` | First line; full message in `notes`. |
| `commit_author` | `string` | `Name <email>`. |
| `commit_committer` | `string` |  |
| `committed_at` | `timestamp` |  |
| `branch` | `string` | Context only — *not* part of identity. |
| `parent_commit_sha` | `string` | First parent of the commit. |
| `prior_blob_id` | `string` | Previous `git-blob` bead for the same `(repo, path)`. Enables fast diff. |
| `gitlab_project_id` | `string` | When sourced from GitLab. |
| `gitlab_mr_iid` | `string` | When the snapshot was captured as part of an MR. |
| `source` | `enum` | `agent-cli`, `gitlab-bridge`, `manual`, `backfill`. |
| `origin_bead` | `string` | The bead this blob derives from (typically the originating issue/MR bead). Plan-of-record § 6.1 default. |

### 4.4 Why this list and not more / fewer

- **No `repo_id` here** — D-1 (P1) chooses whether to store one or both of `repo` and `repo_id`. If P1 picks "both", it is added then.
- **No `is_truncated_at` byte-offset field** — P2's storage tiering will decide whether truncation happens at byte or line boundaries. If a byte offset is needed, P2 adds it.
- **No `permissions` / `mode`** — git stores file mode (executable bit, symlinks). Out of scope; if a later use case needs it, add as an optional field.
- **No `tree_sha`** — by design (plan-of-record § 1.5 non-goal: full git DAG).

### 4.5 Field-validation notes (specs only — not implementation)

- `commit_sha` should be validated as `^[a-f0-9]{40}$` at the API. The existing `FieldDef` machinery (`type_config.go:21-29`) does not yet support regex validation; if P1 needs that, P1 can either add it or do server-side validation in the `git-blob` create handler. Either is fine; flag it now so P1 is not surprised.
- `encoding` is an `enum` field with the five values listed above.
- `source` is an `enum` field with the four values listed above.
- All optional fields are nullable; absence is fine.

## 5. Decision: write policy → `project_scoped: true, escalatable: true`, no `min_role` (resolves **D-2**)

### 5.1 Options considered

| Option | `min_role` | `project_scoped` | `escalatable` | Effect |
|---|---|---|---|---|
| **A** ★ | — | `true` | `true` | Any operator authorized for the project label can register. Server-side secret scan (P3) handles secret risk. |
| **B** | `gardenio-admin` | `true` | `true` | Admin-only registration. Limits secret-leak blast radius by gating who can write at all. |
| **C** | — | `false` | `true` | Any operator anywhere can register against any repo. Maximally permissive; relies entirely on scanner + rate limit. |

### 5.2 Decision

**Option A**: `project_scoped: true, escalatable: true`, no `min_role`.

This matches the formula/molecule write policy (`config.go:490-498`) and the gate write policy (`config.go:480`). It is the standard "operator can do work in their own project" shape.

### 5.3 Rationale

1. **Volume.** If only admins can register `git-blob` beads, the type is dead on arrival — the value comes from agents and operators registering blobs as a normal part of work. Making it admin-only means it never gets used.
2. **Risk model is content, not author.** The leak risk for `git-blob` is "an unredacted secret ends up in the bead store". That risk is best handled by the server-side scanner (P3) on the *content*, not by gating *who* can write. An admin pasting a secret is just as bad as an operator pasting a secret — gating by role does not reduce content risk.
3. **Project scoping is the right authorization unit.** A `git-blob` for `repo=gitlab.com/example-org/site-deployment` should only be writable by operators authorized for the project that owns that repo. Project-scoping enforces this. (The repo→project mapping is a separate concern; if the binding is loose at first, the project label is what matters.)
4. **Escalatable** is on so an authorized-but-not-for-this-project operator can request a one-off escalation through the existing pathway (`type_config.go:46`).
5. **Read access** is unchanged from the default (fleet-bounded, project-scoped if labels apply). Plan-of-record § 8.2 spells this out.

### 5.4 What this leaves to P3

- The scanner choice (gitleaks per default), redaction format, and admin-override flag (`--no-redact` requiring `gardenio-admin`) are P3's remit (D-6). This ADR only asserts that the *write* gate is project-scoped, not role-scoped. The *content* gate (scanner) is a separate, in-process check.
- Per-fleet rate limits (plan-of-record § 8.3) are P3 / P7's remit. They are compatible with project-scoped writes.

### 5.5 Counter-arguments considered

- **"Admin-only is safer."** Admin-only is also unusable. The audit-bead pattern (`min_role: gardenio-admin`) exists because audits should never be written by hand. `git-blob` is the opposite — it should be written constantly, by every agent that opens an MR. The two have different threat models.
- **"What about repos this fleet should not see?"** The `git-blob` type does not bypass fleet isolation; it inherits whatever fleet-scoping kbeads already enforces for bead reads. A fleet that cannot read a project's beads cannot read its `git-blob` beads either. Cross-fleet sharing is an explicit non-goal (plan-of-record § 1.5, § 8.2).

## 6. What this ADR does NOT decide

To keep this ADR small and reviewable, the following are explicitly **out of scope** and deferred to their owning phase:

| Decision | Owning phase | Question |
|---|---|---|
| D-1 | P1 | Canonical `repo` form (URL vs `provider/path` vs `provider:id`). |
| D-3 | P1 | Bead-ID strategy (random nanoid + uniqueness index, deterministic-from-triple, slug). |
| D-4 | P2 | Storage tier thresholds and external-store choice. |
| D-6 | P3 | Secret scanner, redaction format, admin override path. |
| D-7 | P5 | Typed-mention verb extensions (`produces`, `derives-from`). |
| D-8 | P6 | Embedding policy (default-on per repo vs opt-in). |

This ADR also does **not** decide:

- The implementation patch (where in `config.go` to insert the entry, migration ordering, etc.) — that is P9.
- The CLI surface (`kd git-blob add`) — that is P4.
- The bridge auto-registration story — that is P8.
- The diff endpoint — explicitly out of scope per plan-of-record § 13.

## 7. Consequences

### 7.1 For the rest of the research epic

- **P1** can write its identity ADR knowing the natural key is `(repo, commit_sha, path)` and these three are required `string` fields. P1 will revisit `repo` normalization (D-1) and the uniqueness-constraint shape (D-3).
- **P2** can write its storage ADR knowing `blob_sha`, `file_size`, `encoding`, `content`, `content_ref` are the fields it must place. The `encoding=omitted` value is reserved for the case "we accepted the registration but stored no bytes".
- **P3** can write its security ADR knowing the write gate is project-scoped (not admin) and that any role-gated behavior (e.g. `--no-redact`) is layered on top of the project-scoped default.
- **P4** can write its CLI/API spec knowing the required fields the CLI must elicit and what the server will compute (`blob_sha`, `file_size`, `encoding` defaulting to `text` then promoted to `truncated-*` / `base64` / `omitted` as needed).
- **P5** can write its graph ADR knowing the `origin_bead` field exists as the canonical issue→blob attribution.
- **P6** can write its embedding ADR knowing only `encoding ∈ {text, truncated-text}` blobs are eligible for pgvector.
- **P7** can write its lifecycle ADR knowing `kind: data` already participates in the existing 90-day orphan GC pathway.
- **P8** can write its bridge ADR knowing the bridge will use `source=gitlab-bridge` and that batch-registration uses the same project-scoped write path (no special bridge role).

### 7.2 For the eventual P9 implementation

The P9 implementation registers `type:git-blob` in `kbeads/internal/server/config.go` next to `type:formula` (~line 490). The literal JSON the implementation will register is roughly:

```jsonc
"type:git-blob": {
  "kind": "data",
  "write_policy": {"project_scoped": true, "escalatable": true},
  "fields": [
    // identity (required)
    {"name": "repo",        "type": "string"},
    {"name": "commit_sha",  "type": "string"},
    {"name": "path",        "type": "string"},
    {"name": "blob_sha",    "type": "string"},
    {"name": "file_size",   "type": "integer"},
    {"name": "encoding",    "type": "enum",
     "values": ["text", "base64", "truncated-text", "truncated-base64", "omitted"]},
    // content (optional)
    {"name": "content",          "type": "string"},
    {"name": "content_ref",      "type": "string"},
    {"name": "language",         "type": "string"},
    {"name": "mime_type",        "type": "string"},
    {"name": "truncated",        "type": "boolean"},
    {"name": "secrets_redacted", "type": "boolean"},
    {"name": "secrets_summary",  "type": "json"},
    // provenance (optional)
    {"name": "commit_message",     "type": "string"},
    {"name": "commit_author",      "type": "string"},
    {"name": "commit_committer",   "type": "string"},
    {"name": "committed_at",       "type": "timestamp"},
    {"name": "branch",             "type": "string"},
    {"name": "parent_commit_sha",  "type": "string"},
    {"name": "prior_blob_id",      "type": "string"},
    {"name": "gitlab_project_id",  "type": "string"},
    {"name": "gitlab_mr_iid",      "type": "string"},
    {"name": "source",             "type": "enum",
     "values": ["agent-cli", "gitlab-bridge", "manual", "backfill"]},
    {"name": "origin_bead",        "type": "string"}
  ]
}
```

P1 may add `repo_id`. P3 may revise `secrets_summary` shape. The above is a sketch, not the final implementation patch — the final patch is P9's deliverable.

The required-field semantics on `FieldDef` (`type_config.go:21-29`) currently use `Required bool`. Six of the fields above are required; if the JSON-form `"required": true` enforcement is light (the conversion path goes through `internal/server/config.go` and is consumed at create time), P1's identity ADR should validate that the required-field path actually rejects writes that omit any of the six. If it doesn't, P1 should call that out as a follow-up so it lands before P9 ships.

### 7.3 For operators

- A new bead type with a stable shape that any agent can register against, scoped to its project label.
- No new admin role to manage. Existing project-authorization continues to be the gate.
- `git-blob` beads are filtered out of work-tracking views by default; operators can still query them with `kd list --type=git-blob`.

### 7.4 Cost / risk

- Negligible cost from this ADR (it ships zero code).
- Risk that a later phase wants to revise the field set. Mitigation: this ADR explicitly defers the most contentious decisions (D-1, D-3, D-4, D-6, D-7, D-8). If P1/P2/P3 surface a reason to revise the *required* set, that comes back as an addendum to this ADR rather than a silent change in P1.
- Risk that `kind: data` turns out to be wrong (e.g. operators want them on the work board). Mitigation: `kind` can be revised by changing one JSON value in the type-config registration; no migration needed (`kind` is not stored on the row, it is derived from the type-config). The cost of changing minds later is low.

## 8. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The accompanying `gb decision` (D-2 + D-5 bundled) is resolved by the operator either accepting the recommended defaults or directing a specific override.
3. P1 (ka-0000003043's next-phase task — to be created after operator approval of the parent epic) can begin against the shape locked here.

This ADR is **not** the green light to register the type in `config.go`. That is P9, after the operator authorizes the implementation epic per plan-of-record § 11.

## 9. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (MR !1973, bead ka-0000003043).
- Type config machinery: `kbeads/internal/model/type_config.go`.
- Built-in type registrations (where P9 will land): `kbeads/internal/server/config.go` (see `type:formula`, `type:molecule`, `type:audit`, `type:memory`, `type:escalation`).
- Companion research: `kbeads/docs/research-typed-mention-syntax.md` (typed-mention epic ka-0000001333; relevant to P5/D-7).
- Reference-extraction precedent: `kbeads/docs/design-bead-references.md`.
- Embedding precedent: `kbeads/docs/design-embedding-search.md`.
