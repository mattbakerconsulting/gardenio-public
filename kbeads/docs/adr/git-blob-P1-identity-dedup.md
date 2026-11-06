# ADR: `git-blob` Bead Type — Identity, Dedup, Repo Normalization

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p1-gitblob-adr-zur
**Phase**: P1 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000002044
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (MR !1973), §3, §4, §10.
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (MR !1976) — locks type name `git-blob`, `kind: data`, the six required fields (`repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`), and the project-scoped write policy.
**Resolves decisions**: **D-1** (repo identity scheme), **D-3** (bead-id strategy).
**Defers**: D-4 (storage tiering, → P2), D-6 (secrets, → P3), D-7 (typed-mention verbs, → P5), D-8 (embedding policy, → P6).

## TL;DR

Building on P0's locked schema, P1 closes the two open questions that govern *how* one `git-blob` bead is identified and *how* duplicate registrations of the same triple collapse to one row:

- **Repo identity (D-1)**: store **two** fields. `repo` is a normalized `<host>/<path>` string (e.g. `gitlab.com/example-org/gardenio`) used for human-friendly listing and identity; `repo_id` is an opaque `<provider>:<numeric-id>` string (e.g. `gitlab:12345678`) populated *when known* and used as a rename-stable secondary key. The natural-key triple uses `repo` (not `repo_id`) so a registration without a provider numeric ID is still uniquely identified. `repo` normalization rules and edge cases are spelled out below.
- **Bead-ID strategy (D-3)**: keep the existing **random nanoid** ID generator (`internal/idgen`) — `git-blob` beads receive a normal `ka-0000001713` ID like every other bead. Idempotence is enforced by a **partial unique index** on `beads(LOWER(fields->>'repo'), fields->>'commit_sha', fields->>'path') WHERE type='git-blob' AND deleted_at IS NULL`. Re-registering the same triple is a server-side **upsert** that returns the existing bead's ID; we do **not** introduce a deterministic-from-hash ID and we do **not** introduce a public slug.
- **DB constraint sketch**: one new partial unique index, plus a CHECK that rejects empty identity fields and a check that exactly one of (`content`, `content_ref`) is populated when `encoding ∉ {omitted}`. The migration is a sketch — it is written for P9, not for this ADR.

The ADR is docs-only. No code, no migration, no schema mutation. P9 is the only phase that ships SQL.

## 1. Context

### 1.1 What P0 left open

P0 (ka-0000006442, MR !1976) locked: type name (`git-blob`), kind (`data`), required fields (`repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`), optional content and provenance fields, and write policy (`project_scoped: true`, `escalatable: true`, no `min_role`). It explicitly deferred:

- **D-1** to P1 — what *exact* string the `repo` field carries and whether a parallel rename-stable identifier is also stored.
- **D-3** to P1 — how the `kd-` ID is chosen and how dedup is enforced when the same `(repo, commit_sha, path)` is registered twice.

P1 must close both before P2 (storage tiering), P4 (CLI/API), P5 (graph integration), and P9 (implementation) can stop hedging on the natural-key shape.

### 1.2 What is non-negotiable from P0

P1 must not change:

1. The natural key is `(repo, commit_sha, path)` (plan-of-record §2.1, P0 §4.1).
2. `repo`, `commit_sha`, and `path` are required `string` fields.
3. `git-blob` is `kind: data` with no `min_role` write gate.

P1 may add **one** optional field (`repo_id`) and may revise the *validation rules* on existing fields. It may not relax the natural key.

### 1.3 What the existing infrastructure looks like

- Bead IDs are nanoid-generated, prefixed `kd-` (`kbeads/internal/idgen`). All built-in types use this generator; no type currently uses a deterministic-from-content scheme.
- Field storage is JSONB on `beads.fields` (`migrations/001_initial.up.sql`). Required-field enforcement happens in the create handler against the type-config; there is no DB-level CHECK for required fields today.
- Existing precedents for a unique index expressing a natural key:
  - `bead_references_unique` (`migrations/013_*`) — `UNIQUE(source_bead_id, target_bead_id, source_field, COALESCE(relationship_type, ''))`. Demonstrates `COALESCE` for nullable parts and replacing a PK with a multi-column unique index.
  - `idx_bead_references_target_type` (same migration) — `WHERE relationship_type IS NOT NULL`. Demonstrates partial indexes scoped by predicate.
- Soft delete: migration `009_soft_delete` adds a `deleted_at` column. Any uniqueness index for an immutable record type must scope on `deleted_at IS NULL` so a deleted-and-re-registered triple is permitted.

These three precedents fully cover the shape this ADR proposes. No new DB capability is needed.

## 2. Decision: repo identity (resolves **D-1**)

### 2.1 Shape

Store **two** fields on every `git-blob` bead:

| Field | Type | Required | Form | Purpose |
|---|---|---|---|---|
| `repo` | `string` | yes (P0 lock) | `<host>/<owner>/<sub>/<name>`, lowercase, no scheme, no trailing `.git`. e.g. `gitlab.com/example-org/gardenio`. | Human-readable canonical key. Part of the natural-key triple. |
| `repo_id` | `string` | no (added by this ADR) | `<provider>:<numeric-id>`, e.g. `gitlab:12345678`, `github:123456789`. Empty when not known. | Rename-stable secondary key. Lets us follow a repo across renames without a migration. |

The natural-key triple stays `(repo, commit_sha, path)`. `repo_id` is **not** part of the unique index — it is a parallel attribute that an admin migration tool can use to rewrite `repo` if a provider rename happens.

### 2.2 Options considered

| Option | `repo` form | `repo_id` | Pros | Cons |
|---|---|---|---|---|
| **A** ★ | `<host>/<owner>/<…>/<name>` (provider+path, normalized) | `<provider>:<numeric-id>`, optional | Human-readable identity; survives URL-form drift; rename-stability available when known. | Two fields. Requires a one-screen normalization spec. |
| B | Full HTTPS clone URL (`https://gitlab.com/example-org/.../gardenio.git`) | — | "What you paste is what you store." | URL form drifts: `http`/`https`, trailing `.git`, vanity hosts, mirrors, SSH-only repos. Every consumer must re-normalize on read. |
| C | `<provider>:<numeric-id>` only (e.g. `gitlab:12345678`) | n/a (already in `repo`) | Bulletproof against renames. One field. | Opaque to humans. Every list and grep needs a join to a `repos` config bead. Doesn't work for repos without a numeric ID (raw git over SSH, mirrored repos pre-import). |
| D | Repo nickname registered as a separate config bead (`gardenio`) | — | Pretty in lists. | Adds a config-bead lookup to every read; nickname collisions are a per-fleet hazard; doesn't solve the dedup question (two fleets can name the same repo differently). |

### 2.3 Decision

**Option A**: store both `repo` (provider+path, normalized) and `repo_id` (provider:numeric-id, when known). The natural-key triple uses `repo`.

This matches the plan-of-record's §3 recommendation verbatim ("store *both* a normalized `repo` … and a `repo_id` … so we get both stable identity and human readability"). The plan's reasoning holds; this ADR adds the normalization spec the plan-of-record left for P1.

### 2.4 `repo` normalization rules (the spec)

On every write path (CLI `kd git-blob add`, HTTP/gRPC `CreateBead`, bridge), the server applies these transformations *before* identity comparison and storage:

1. **Strip scheme**: drop a leading `http://`, `https://`, `git://`, `ssh://`, or `git@` prefix.
2. **SSH form rewrite**: `git@<host>:<path>` → `<host>/<path>`. Only the very first `:` is the host/path delimiter.
3. **Strip trailing `.git`**: `gardenio.git` → `gardenio`.
4. **Strip trailing `/`**: at most one.
5. **Lowercase the host**. Path segments stay case-preserved (GitHub & GitLab are case-insensitive in routing but case-preserving in the canonical reference; see §2.5).
6. **Reject** if the result does not match `^[a-z0-9.-]+/[A-Za-z0-9._/-]+$`. The host segment must be all-lowercase ASCII (ASCII-only because IDN repos are out of scope; reject with a 400 referencing this ADR).
7. **Reject** path traversal: any `..` segment, leading `/`, or empty segment after the host.
8. **Reject** vanity-host aliases that we know of (`www.gitlab.com`, `www.github.com`) — strip `www.` before the lowercase step.
9. **Length cap**: 255 bytes total. (Postgres GIN+JSONB has no hard cap; this is an operator-sanity cap.)

Examples:

| Input | Normalized `repo` |
|---|---|
| `https://gitlab.com/example-org/gardenio.git` | `gitlab.com/example-org/gardenio` |
| `git@gitlab.com:example-org/gardenio.git` | `gitlab.com/example-org/gardenio` |
| `https://www.github.com/anthropics/claude-code/` | `github.com/anthropics/claude-code` |
| `ssh://git@gitlab.com/example-org/gardenio` | `gitlab.com/example-org/gardenio` |
| `https://gitlab.com/Example-Org/gardenio` | `gitlab.com/Example-Org/gardenio` (host lowercased; path case preserved — see §2.5) |

### 2.5 Path-segment case: preserve, don't lowercase

GitLab and GitHub route case-insensitively but display the canonical case the owner registered. A `repo` written as `gitlab.com/Example-Org/...` and another as `gitlab.com/example-org/...` resolve to the same repo upstream.

We have two choices:

- **Lowercase aggressively** — collapse on write. Pro: cleaner dedup. Con: loses the canonical display form; future-us can't reconstruct `acme` from `acme` if a rendering wants the proper case.
- **Preserve case, dedup case-insensitively** — store what the user submitted (after the §2.4 transforms), but build the unique index on `LOWER(repo)`. Pro: keeps display form. Con: one more layer in the index expression.

**Choice: preserve case, dedup case-insensitively.** The unique index in §4.1 uses `LOWER(fields->>'repo')`. Display uses the stored form. This is symmetric with how Postgres `citext` works without forcing a column-type change.

A second `git-blob` registration for the *same* triple but with a different case-form returns the *first* bead (upsert hits the index). It does not "fix" the stored form. An admin tool may rewrite stored `repo` strings later (§2.7).

### 2.6 `repo_id` rules

- Format: `<provider>:<numeric-id>`. Provider tokens: `gitlab`, `github`, `gitea`, `bitbucket`. Tokens beyond this list are accepted (forward-compatible) but only the four tokens above are treated as known providers by any UI.
- Empty when not supplied. The CLI may resolve it on the user's behalf (e.g. by hitting GitLab's `/projects/:path` API and reading `id`), but the field is **not** required.
- `repo_id` is **not** part of the natural-key triple. Rationale: repos without numeric IDs (e.g. raw git over SSH on a custom host) can still be uniquely identified by `(repo, commit_sha, path)`. Forcing `repo_id` would either reject those or invent one.
- Invariant: within a single fleet, all `git-blob` beads carrying a given `repo_id` should share the same `repo` *up to a rename event*. After a rename, both old and new `repo` strings legitimately point at the same `repo_id`. The admin tool in §2.7 handles cleanup.

### 2.7 Repo renames (operational note, not a P1 deliverable)

When a provider renames a repo (`acme/platform` → `acme/foundations`), all existing `git-blob` beads keep their old `repo` string. New registrations land under the new `repo` string. The `repo_id` ties them together.

P1 does **not** specify a rename migration. Plan-of-record §12 flags this as an admin-only follow-up tool: "store `repo_id`; provide an admin-only migration tool". P9 (implementation) can ship a `kd admin git-blob rename-repo --from=<old> --to=<new>` that walks beads sharing a `repo_id` and rewrites `repo`. That tool is outside the scope of this ADR.

### 2.8 What this ADR does *not* do for repo identity

- Does not introduce a `repos` config-bead registry. (Option D was rejected.)
- Does not require `repo_id` lookup at registration time. (CLI may, server does not.)
- Does not normalize path-segment case. (Preserved per §2.5.)
- Does not handle vanity hosts beyond `www.<known>` stripping. A custom git host (`source.example.com`) is treated like any other host — operator's responsibility to keep the spelling consistent.
- Does not support per-host normalization plugins. The §2.4 rules are universal.

### 2.9 Counter-arguments considered

- **"Just store the URL — let the client deal with it."** Rejected because every list query, every dedup check, every embedding lookup, and every cross-bead comparison would re-implement the same normalization. One write-path normalization is cheaper and removes a class of "same repo, two beads" bugs.
- **"Just use `<provider>:<numeric-id>` and skip the human form."** Rejected because raw git over SSH on a custom host has no numeric ID; the type would either reject those repos (wrong: plan-of-record §1.4 wants any repo) or invent one. Also: `kd list --type=git-blob` should be readable without joining.
- **"Lowercase path segments — case-insensitivity is the truth."** Rejected because we want the *displayed* form to match what the operator sees on GitLab/GitHub; deduping on `LOWER(...)` gets us correctness without losing the display form.

## 3. Decision: bead-ID strategy (resolves **D-3**)

### 3.1 Options considered

| Option | ID source | Dedup mechanism | Pros | Cons |
|---|---|---|---|---|
| **A** ★ | Existing nanoid generator (`internal/idgen`). | DB partial unique index over `(LOWER(repo), commit_sha, path) WHERE type='git-blob' AND deleted_at IS NULL`. Server upserts on conflict. | Uses existing ID infrastructure; no new generator; ID format is the same as every other bead; survives changes to identity field shape (the index is rebuildable). | Operators can't tell from the ID alone what triple it represents — they must `kd show` or filter by triple. |
| B | Deterministic from triple: `kd-` + base32(sha256(`repo|commit_sha|path`))[:10]. | The ID itself is the dedup; no DB-level uniqueness needed (collision = same triple by construction). | Content-addressed; same triple → same ID across fleets without coordination; idempotent client-side. | Breaks the "all bead IDs are random nanoid" invariant — third-party tooling that assumes random IDs (notably `idgen`'s collision-detection logic) gets a special case. 50-bit hash (10 base32 chars) is collision-prone at high registration volume — the day we hit 33M registrations, birthday probability ≈ 0.5. Migration to a different hash scheme is brutal because IDs are referenced from prose. |
| C | Random nanoid + a public slug like `git-blob/gitlab.com/example-org/.../config.go@deadbeef`. | Slug-uniqueness via `bead_slugs` index; partial-indexed by type. | Human-readable in prose; works with the existing slug machinery. | Slug strings get long fast (full path + 40-char SHA). Collisions with other slug uses possible. The "looks human-readable" appeal evaporates once you write `kd show 'git-blob/gitlab.com/example-org/gardenio/internal/server/config.go@deadbeef0123456789abcdef0123456789abcdef'`. |

### 3.2 Decision

**Option A**: random nanoid for the ID; idempotence via a partial unique index on the natural-key triple.

### 3.3 Rationale

1. **Reuse the existing generator.** `internal/idgen` is already battle-tested. There is no value in special-casing one type out of fifteen. A future operator looking at `ka-0000002859` should not need to know the bead's *type* to know whether the ID is random or deterministic — that's a property of the bead system, not of one type.
2. **The triple is queryable without the ID.** `kd list --type=git-blob -f repo=... -f commit_sha=... -f path=...` answers the "what's the bead for this triple?" question without anyone caring what the ID is. Plan-of-record §1.2 lists this as a primary use case.
3. **Random IDs survive identity-shape changes.** If P9 implementation discovers a flaw in the §2.4 normalization rules and we need to rebuild the index, the IDs do not change — only the index does. With Option B, *changing* the hash means *changing every ID*, which breaks every prose mention of those IDs. That is a bad migration story for a system whose whole point is to embed `kd-` IDs in human-readable text.
4. **Collision resistance.** At 50 bits (Option B), birthday-probability hits 50% around 38M registrations. That is a number we plausibly reach in 2-3 years if every agent MR auto-registers a few blobs. Going to 16 chars (80 bits) breaks the existing nanoid format the rest of the system uses.
5. **Slugs are the wrong UX.** Slugs are great when the slug is short and memorable. A path-and-SHA slug is neither. Operators searching for "the blob for this file at this commit" will type `kd list --type=git-blob -f path=... -f commit_sha=...` long before they'll type the slug, and it's faster than the slug anyway because of tab-completion on field values vs. on long paths.

### 3.4 Idempotence semantics (the contract)

When a client registers a `git-blob`:

1. Server normalizes `repo` per §2.4.
2. Server checks the partial unique index for an existing bead with `(LOWER(repo)=$1, commit_sha=$2, path=$3, type='git-blob', deleted_at IS NULL)`.
3. If found:
   - Return the existing bead's ID.
   - Tag the response with `created: false` (or, in CLI, `dedup: existing`).
   - Do **not** mutate the existing bead's content fields. (See §3.5 for the "first writer wins" rule.)
4. If not found:
   - Create the bead with a fresh nanoid.
   - Insert into the unique index transactionally.
   - On a race-induced unique-violation, re-read and return the racing winner. (Standard upsert-on-conflict pattern.)
   - Tag the response with `created: true` (CLI: `dedup: created`).

### 3.5 First-writer-wins for content; last-writer-wins for provenance — and a hard ban on changing identity fields

A re-registration of the same triple with *different* content is a meaningful event: it means the client computed a different blob SHA from the same `(repo, commit, path)`. That should never happen unless the client lied or the upstream provider returned a different file body. We treat it as an error:

- If `blob_sha` mismatches the stored one, **reject the write** with a 409 Conflict. The bead's content is canonical; a true conflict is a bug, not a no-op.
- If `blob_sha` matches but other content fields (`encoding`, `language`, `mime_type`) differ, accept the new write but do not mutate. Log a warning. Treat the difference as a client-detection drift.

Provenance fields (`commit_message`, `commit_author`, `branch`, `gitlab_mr_iid`, `origin_bead`, `source`) are different. A bead may be registered by an `agent-cli` first, then a `gitlab-bridge` second. The bridge legitimately knows more (`gitlab_mr_iid`, `committed_at`). Allow last-writer-wins on provenance fields **only**, never on identity (`repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`).

P4 (CLI/API spec) will lock the precise list of "content-mutable" vs "provenance-mutable" fields. P1 only asserts the principle: identity is immutable; provenance is upsert-friendly.

### 3.6 What this ADR does *not* do for IDs

- Does not introduce slug routing for `git-blob` beads. (Existing slug machinery still works; nobody is required to set a slug.)
- Does not change the nanoid alphabet or length.
- Does not back-compute IDs for retroactively-registered blobs from provider history. Each registration gets a fresh ID.

## 4. DB constraint sketch

This section is the prose form of the migration P9 will write. **No migration ships from this ADR.**

### 4.1 The partial unique index

```sql
-- migrations/NNN_git_blob_uniqueness.up.sql  (P9, NOT this ADR)
CREATE UNIQUE INDEX IF NOT EXISTS idx_git_blob_natural_key
    ON beads (
        LOWER(fields->>'repo'),
        fields->>'commit_sha',
        fields->>'path'
    )
    WHERE type = 'git-blob'
      AND deleted_at IS NULL;
```

Why this shape:

- **Partial** by `type='git-blob'` — does not affect any other bead type.
- **Partial** by `deleted_at IS NULL` — soft-deleted beads do not block re-registration, so an operator who closes-and-deletes a blob (rare; plan-of-record §9.1 calls closing discouraged) can re-register without a "duplicate" error. Migration `009_soft_delete` is the source of `deleted_at`.
- **`LOWER` on `repo`** — case-insensitive dedup as decided in §2.5.
- **No `LOWER` on `commit_sha` or `path`** — `commit_sha` is normalized to lowercase hex by the create handler (§4.3); `path` is case-sensitive (POSIX semantics, plan-of-record §2.1).
- **Functional/expression index** is supported by Postgres ≥ 9.0 and is already used by precedent (`bead_references_unique` uses `COALESCE`).

### 4.2 Rejected alternatives for the index

- **Including `repo_id`**: rejected because `repo_id` is optional. Uniqueness has to be provable with what is required.
- **Materializing identity into dedicated columns** (e.g. add `git_blob_repo`, `git_blob_commit_sha`, `git_blob_path` to `beads`): rejected because that pollutes the shared `beads` table with type-specific columns. The JSONB+expression-index pattern is the established way.
- **A separate `git_blob_index` table**: viable, but adds a foreign-key relationship and a transactional invariant. Strictly more code than the expression index, with no benefit at expected volume.

### 4.3 CHECK constraints (server-enforced or DB-enforced)

The plan-of-record §2.1 already says `commit_sha` should validate as 40-char lowercase hex and `path` as POSIX-no-leading-slash. P0 §4.5 noted the existing `FieldDef` machinery does not yet support regex validation. P1's stance: enforce these in the server-side create handler for `type:git-blob` (see `internal/server/config.go` analog patterns), not via DB CHECK. Reasons:

- The `FieldDef` `Required bool` mechanism is the right level of abstraction; promoting validation to a DB CHECK would diverge from how every other type validates.
- Server-side gives better error messages.
- Server-side composes with the upsert path (we want to fail with a clear message *before* hitting the unique index, not get a generic constraint violation).

P9 may revisit this if ergonomic problems surface, but P1's recommendation is server-side validation:

| Field | Validation |
|---|---|
| `repo` | After §2.4 normalization, matches `^[a-z0-9.-]+(/[A-Za-z0-9._-]+)+$`, length ≤ 255. |
| `commit_sha` | `^[a-f0-9]{40}$`. Reject short SHAs. |
| `path` | UTF-8, no leading `/`, no `..` segments, length ≤ 4096, not empty. |
| `blob_sha` | `^[a-f0-9]{40}$`. Computed server-side; client-supplied value is rejected. |
| `file_size` | `>= 0`. |
| `encoding` | One of `text`, `base64`, `truncated-text`, `truncated-base64`, `omitted`. |
| `repo_id` | `^[a-z]+:[0-9]+$` when present; empty otherwise. |

### 4.4 Mutual-exclusion check (cross-field)

P0 §4.2 requires that `content` and `content_ref` are mutually exclusive. P1 makes this concrete:

- If `encoding = omitted`: both `content` and `content_ref` must be empty.
- If `encoding ∈ {text, truncated-text, base64, truncated-base64}`: exactly one of `content` or `content_ref` is populated.

Enforce server-side at create time. Not a DB check.

### 4.5 Backfill for an existing fleet

P1 is research; there is no existing data. But P9 will need to handle the "what if there are pre-existing beads with `type='git-blob'`?" case for the upgrade migration. If P9 inherits the type after this research lands, there cannot be pre-existing rows (the type does not exist in production). The migration is a clean add.

## 5. Consequences

### 5.1 For the rest of the research epic

- **P2** (storage tiering) writes against a stable identity: the same `(repo, commit_sha, path)` always maps to the same bead, and `blob_sha` is computed server-side and stable, so content-addressed dedup in `git_blob_content` (plan-of-record §5.1) is straightforward.
- **P3** (security/redaction) inherits the upsert semantics: a re-registration with `secrets_redacted=true` does *not* retroactively re-scan a previously-stored bead — that's a separate admin operation. P3 should call this out as a gap and decide whether a bulk re-scan is in scope.
- **P4** (CLI/API spec) needs to specify:
  - The `kd git-blob add` exit code / output for `created` vs `existing`.
  - The HTTP/gRPC response shape for the same.
  - Whether `--from-file` is allowed when the server can fetch from the provider (P4's call).
  - The 409 Conflict semantics from §3.5 (different `blob_sha` for the same triple).
- **P5** (graph integration) inherits the deterministic upsert: a typed mention `produces(ka-blobX)` written *before* the blob is registered will resolve when the blob registers, because the ID is stable from the moment of first registration.
- **P6** (embedding) can index by bead ID; the index is stable because the ID is stable.
- **P7** (lifecycle/GC) inherits the `deleted_at IS NULL` predicate on the unique index — a soft-deleted blob can be re-registered, restoring it under a new bead ID. P7 should decide whether re-registration of a soft-deleted blob restores the old bead or creates a new one. P1's recommendation: create a new bead; the old soft-deleted one is gone for orphan-GC reasons.
- **P8** (bridge) writes through the same upsert path; no special bridge identity. The bridge sets `source=gitlab-bridge` on the provenance side.
- **P9** (implementation) writes the migration in §4.1, the validation in §4.3, the upsert handler in `internal/server` (analog to `CreateBead` paths), and the `kd git-blob add` command.

### 5.2 For the eventual P9 implementation

P9 will register `git-blob` in `internal/server/config.go` with two field additions over the P0 sketch:

```jsonc
// (delta from P0 §7.2 — additions only)
"type:git-blob": {
  "kind": "data",
  "write_policy": {"project_scoped": true, "escalatable": true},
  "fields": [
    // ... P0's fields, plus:
    {"name": "repo_id", "type": "string"}
    // (already present in P0: repo, commit_sha, path, blob_sha, file_size, encoding,
    //  content, content_ref, language, mime_type, truncated, secrets_redacted,
    //  secrets_summary, commit_message, commit_author, commit_committer,
    //  committed_at, branch, parent_commit_sha, prior_blob_id, gitlab_project_id,
    //  gitlab_mr_iid, source, origin_bead)
  ]
}
```

P9 also writes:

- The migration in §4.1 (one new partial unique index — no table changes).
- Server-side normalization for `repo` (§2.4).
- Server-side validation per §4.3.
- The upsert handler that catches the unique-violation and returns the existing bead (§3.4).
- The 409-on-`blob_sha`-mismatch handler (§3.5).

The migration's `down.sql` is a single `DROP INDEX IF EXISTS idx_git_blob_natural_key`. The `up.sql` is the `CREATE UNIQUE INDEX` in §4.1. Both are idempotent.

### 5.3 For operators

- A user pasting a GitLab URL (`https://gitlab.com/example-org/gardenio.git`) into `kd git-blob add --repo=...` ends up with the same bead as a user pasting `git@gitlab.com:example-org/gardenio`. Predictable.
- Looking at a `git-blob` bead, operators see a normal `ka-0000001713` ID — same shape as every other bead. They can `kd show` to see the triple.
- Re-running `kd git-blob add` for the same `(repo, commit, path)` is safe — it's an upsert, not a create.

### 5.4 Cost / risk

- One new partial unique index on `beads`. Postgres maintains this on insert/update for `git-blob` rows only. Negligible storage cost at expected volume.
- Risk that the §2.4 normalization rules turn out wrong for some host. Mitigation: rules are server-side, in one place, revisable; an admin tool can rewrite stored `repo` strings if needed (§2.7).
- Risk that the 409-on-`blob_sha`-mismatch rule (§3.5) is too strict and surfaces real-world drift (e.g. a provider returning a different LF/CRLF normalization). Mitigation: P4 will add a `--force` flag for the CLI to deliberately overwrite, gated on `gardenio-admin`. P1 only sets the default behavior.
- Risk that operators want a deterministic ID for cross-fleet workflows. Mitigation: `repo_id` + the natural-key triple is enough to derive cross-fleet identity at the *bead* level without baking it into the *ID* string. If a future fleet-federation use case actually needs deterministic IDs, that's a P-future ADR — and even then it can live as a *secondary* identifier (e.g. a `cross_fleet_id` field) without changing the primary ID generator.

## 6. What this ADR does NOT decide

To keep this ADR focused on D-1 and D-3, the following are explicitly **out of scope** and deferred:

| Decision | Owning phase | Question |
|---|---|---|
| D-4 | P2 | Storage tier thresholds and external-store choice. |
| D-6 | P3 | Secret scanner, redaction format, admin override path. |
| D-7 | P5 | Typed-mention verb extensions. |
| D-8 | P6 | Embedding policy. |

Also out of scope:

- The CLI surface for `kd git-blob add` (P4).
- The gRPC/HTTP RPC shape (P4).
- The bridge auto-registration story (P8).
- The diff endpoint (explicitly out of scope per plan-of-record §13).
- The admin rename-repo tool (operational, not a research deliverable; flagged in §2.7).
- A `repos` config-bead registry (rejected in §2.2 Option D).
- Slug routing (rejected in §3.1 Option C).
- Fleet-federation cross-fleet identity (flagged as P-future in §5.4).

## 7. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The `gb decision` bundle for D-1 + D-3 is resolved by the operator (default: accept the recommended forms) — to be raised after the ADR MR opens, per the "decisions surface as `gb decision` checkpoints once research starts" pattern from plan-of-record §10.
3. P2, P3, P4, P5, P6, P7, P8 can begin (or continue) against the identity shape locked here.

This ADR is **not** the green light to ship the partial unique index, the `repo_id` field, or the upsert handler. That is P9, after the operator authorizes the implementation epic per plan-of-record §11.

## 8. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (MR !1973, bead ka-0000003043). §3 (repo identity), §4 (bead identity & idempotence), §10 (decisions).
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (MR !1976, bead ka-0000006442). §4 (required fields), §5 (write policy), §7.2 (P9 sketch).
- ID generator: `kbeads/internal/idgen` (nanoid, `kd-` prefix).
- Type-config machinery: `kbeads/internal/model/type_config.go`. Built-in registrations: `kbeads/internal/server/config.go`.
- Migration precedents:
  - `internal/store/postgres/migrations/013_bead_references_relationship_type.up.sql` — partial-unique-index pattern with `COALESCE`.
  - `internal/store/postgres/migrations/009_soft_delete.up.sql` — `deleted_at` predicate source.
  - `internal/store/postgres/migrations/005_bead_embeddings.up.sql` — content-addressed companion table precedent (relevant for P2, not this ADR).
- Companion research: `kbeads/docs/research-typed-mention-syntax.md` (typed-mention epic ka-0000001333; relevant for P5).
- Reference-extraction precedent: `kbeads/docs/design-bead-references.md`.
