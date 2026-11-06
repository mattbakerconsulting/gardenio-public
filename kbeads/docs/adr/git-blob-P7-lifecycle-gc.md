# ADR: `git-blob` Bead Type — Lifecycle and GC

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p7-gitblob-adr-bhy
**Phase**: P7 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000005504
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (§ 9 lifecycle, § 5 storage references)
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks `kind: data`, the field set, and the `project_scoped: true, escalatable: true` write policy
- P1 ADR `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044) — locks `(repo, commit_sha, path)` natural-key, partial unique index, identity-field immutability rule
- P2 ADR `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323) — locks the three-tier storage model and the `git_blob_content` table with an application-maintained `refcount` column
**Resolves**: no plan-of-record decision IDs (lifecycle is internal mechanics, not surfaced as a D-x). One new internal decision is recorded here: **L-1** — orphan-detection scope (typed mentions + deps + `origin_bead`, see § 4.2).
**Defers**: D-7 (P5), D-8 (P6), bridge-rollout backfill (P8)

## TL;DR

This ADR fixes the **lifecycle and reclamation contract** for `git-blob` beads so the eventual P9 implementation has a single specification to land. Decisions:

- **Status transitions**: `git-blob` beads are created `open` and stay `open`. Server enforces `kind=data` types with `lifecycle=immutable` reject any write that transitions status to `in_progress`. `closed` is allowed but discouraged — operationally it is the "hide from default views" knob, not a "this is done" signal. `deferred` is not a legal terminal state for `git-blob`; an attempted set-status to `deferred` is rejected with 409.
- **Identity-field immutability**: `repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`, `content`, and `content_ref` are write-once. Any update RPC that mutates one of those fields is rejected at the type-config layer (P1 § 3.5 already establishes the rule for identity fields; P7 extends it to the content-shape fields as well).
- **Orphan GC**: an orphan is a `git-blob` bead with no incoming graph edges (no row in `deps` where `depends_on_id = blob.id`, no row in `bead_references` where `target_bead_id = blob.id`, and no other `git-blob` bead's `prior_blob_id` pointing at it, and no `issue`-kind bead's `origin_bead` field referencing it). Eligible after **90 days** of orphanhood (`created_at < NOW() - 90d` AND last-orphan-check confirms zero edges). The sweeper soft-deletes; the existing 30-day post-soft-delete purge then hard-deletes (cumulative ≥120d retention). The 90d window and the orphan predicate are **server-side defaults**, configurable per fleet.
- **Hard-delete cascade**: when a `git-blob` bead is hard-deleted (purge sweep at the 30d post-soft-delete mark), the create-handler's mirror logic decrements `git_blob_content.refcount` for Tier B and removes the row at `refcount = 0`. Tier A is in-row and goes away with the bead. Tier C objects in S3 are **not** deleted synchronously — a separate object-store sweeper reconciles unreferenced keys on a schedule (default daily). This matches P2 § 3.3.
- **Repo deletion**: when a repo is removed from the fleet's allowed-projects list, its blobs are **read-preserved, write-frozen, list-hidden** by default. New writes targeting that repo are rejected with 403 (`project_scoped` policy revocation). Reads continue. A new optional default list filter excludes blobs from removed repos. Bulk hard-delete of a removed repo's blobs is operator-initiated and audit-logged; it is not automatic.
- **Refcount migration**: the migration spec from P2 § 3.1 (`014_git_blob_content`) is extended here with a **forward-only repair migration** (`015_git_blob_content_refcount_repair`) that recomputes `refcount` from the live bead set on first apply. This handles the case where Tier B rows exist in a stage environment ahead of the lifecycle code landing (defense in depth — there is no production data today, so the practical surface is zero, but the migration is cheap to ship).

This ADR ships **no code**. It is the contract the eventual P9 implementation will register at the create / update / delete handlers and at the new GC sweeper background job. There is no `gb decision` to surface — every choice here either follows directly from a prior ADR (P0/P1/P2) or sits inside the existing soft-delete + purge pathway, with the single new internal decision (L-1, orphan-edge scope) recorded inline.

## 1. Context

### 1.1 What the plan-of-record asks

`kbeads/docs/research-git-blob-bead-type.md` § 9 frames the lifecycle in three short paragraphs:

1. **Status transitions** — created `open`, stays `open`. Closing allowed but discouraged. No `in_progress` semantics (it's data, not work). Recommended kind is `data` (P0 already locked this).
2. **GC** — an orphan is a blob with no incoming references; after 90 days an orphan is eligible for soft-delete. Refcounting in `git_blob_content` cleans up content rows when the last referencing blob bead is hard-deleted.
3. **Repo deletion** — existing blobs stay readable, are excluded from default list filters, and are not registerable.

P7's job is to turn those three paragraphs into a contract concrete enough that the P9 implementer cannot reasonably misinterpret them, and the GC sweeper they write does not silently delete the wrong rows.

### 1.2 What P0 / P1 / P2 already locked

- **P0** locked `kind: data` and the field schema (including `secrets_redacted`, `secrets_summary`, `prior_blob_id`, `origin_bead`). It also called out (§ 3.4 fifth bullet) that "lifecycle GC (P7) inherits the existing 90-day orphan soft-delete pathway from migration 009". P7 honors that — no new orphan-tracking column on `beads`; the predicate is computed.
- **P1** locked the partial unique index on `(repo, commit_sha, path)` and the rule that identity fields are write-once. Lifecycle code that needs to "find this blob" uses the natural key, not a separate orphan-tracking table.
- **P2** locked `git_blob_content(blob_sha PRIMARY KEY, refcount INTEGER NOT NULL CHECK (refcount >= 0))` and the application-maintained refcount in the create / hard-delete handlers (§ 3.3). P7 fills in the *when* of the hard-delete — i.e. how a bead becomes hard-delete-eligible in the first place.

P7 does not relitigate any of those. It binds them together into a single lifecycle picture.

### 1.3 What the existing soft-delete pathway looks like

Migration `009_soft_delete` (`kbeads/internal/store/postgres/migrations/009_soft_delete.up.sql`) already gives every bead a `deleted_at TIMESTAMPTZ NULL` column with a partial index on `WHERE deleted_at IS NOT NULL`. Today's flow:

1. `kd delete <id>` → `queryDeleteBead` (`kbeads/internal/store/postgres/queries.go:554`) sets `deleted_at = NOW()`. The bead disappears from default listings (admin can re-include with `--include-deleted`).
2. `kd restore <id>` clears `deleted_at`. Allowed within the retention window (admin role).
3. A **future purge sweep** (acknowledged as TODO in `queries.go:551`) hard-deletes rows where `deleted_at < NOW() - retention`. Default retention per migration comment is 30 days. This sweeper does not exist in tree yet — it is a P9 deliverable.

P7's contract reuses both of those primitives unchanged. The orphan sweeper soft-deletes (sets `deleted_at`); the purge sweeper hard-deletes; and only at hard-delete does the `git_blob_content.refcount` drop from P2 fire. There is no parallel pathway and no new state column.

### 1.4 Why P7 has to land before P8 (bridge / rollout)

P8 is bridge auto-registration on merge — i.e. the firehose-rate writer. If P8 lands without P7's GC, every merge keeps adding rows forever and the `git_blob_content` table monotonically grows. The cost model in P2 § 4 assumes GC exists; the embedding policy in P6 (already drafted at ka-0000005801) assumes embedding rows go away when their parent blob is hard-deleted. P7 is the load-bearing piece that lets all of that be true.

## 2. Decision: status transitions (immutability, no `in_progress`)

### 2.1 The rule

A `git-blob` bead has exactly two legal status states:

| Status | Meaning | How it's reached |
|---|---|---|
| `open` | The blob exists and is the canonical record of `(repo, commit_sha, path) → contents`. | Default on create. |
| `closed` | The blob is hidden from default views. The row still exists; reads still work for explicit lookups (`kd show <id>`, natural-key resolves). | Operator-initiated `kd close <id>` only. |

`in_progress` and `deferred` are **not legal** for `git-blob`. Any RPC that attempts those transitions returns 409 with `error: "git-blob beads do not support status \"<x>\""`.

`closed` does NOT trigger hard-delete or refcount decrement. It is purely a visibility flip. To remove a blob entirely, the operator soft-deletes (`kd delete`) and waits for the purge sweep, or the orphan GC does it for them.

### 2.2 Why `closed` is allowed at all

The plan-of-record § 9.1 says "closing is allowed but discouraged: only when the underlying repo or MR is hard-deleted upstream and the operator wants the blob hidden from default views." Two concrete cases support keeping the lever:

1. **Upstream hard-delete of a single MR/file**. The repo is still allowed but a specific file (e.g. an accidental commit of a credentials file that gitleaks missed) needs to disappear from default views *before* the orphan sweeper would naturally retire it. Closing it is the fastest immediate action.
2. **Repo decommission, slow-rolled** (§ 5 below). Operator wants to hide the blobs without deleting yet, so search results stop surfacing them while the cleanup MR is reviewed.

Both are operator decisions, not automatic transitions.

### 2.3 Why `in_progress` is rejected (not just discouraged)

`kind: data` already conventionally excludes `in_progress`. But the type-config registry today does not enforce kind-level transition restrictions — `formula`, `audit`, and `memory` rely on convention plus the fact that nobody runs `kd update` on them. With `git-blob` accepting writes from agents at high rate (P8 bridge integration), convention is not enough. P7 specifies that the type registration must carry an explicit `lifecycle` flag, and the create/update handlers must consult it.

The minimal shape for that flag (P9 will implement it; the spec is here):

```go
// kbeads/internal/model/type_config.go (sketch — added in P9)
type LifecyclePolicy struct {
    AllowedStatuses []string `json:"allowed_statuses"`            // e.g. ["open", "closed"]
    Immutable       bool     `json:"immutable"`                    // identity+content fields write-once
    NoInProgress    bool     `json:"no_in_progress"`               // redundant with AllowedStatuses, kept for ergonomics
}

// In TypeConfig:
//   Lifecycle *LifecyclePolicy `json:"lifecycle,omitempty"`
```

The `git-blob` registration sets:

```jsonc
"lifecycle": {
  "allowed_statuses": ["open", "closed"],
  "immutable": true,
  "no_in_progress": true
}
```

Existing types (`formula`, `audit`, etc.) leave `lifecycle` nil → existing behavior unchanged. This is additive.

### 2.4 Identity-field immutability (extending P1)

P1 § 3.5 establishes that identity fields (`repo`, `commit_sha`, `path`) cannot be changed post-create — first-writer-wins for content, last-writer-wins for provenance. P7 extends the immutability surface to the *content-shape* fields:

| Field | Immutable post-create? | Why |
|---|---|---|
| `repo`, `commit_sha`, `path` | yes (P1) | Natural key. |
| `blob_sha` | yes (P7) | Content fingerprint; mutating it would silently divorce the bead from its `git_blob_content` row. |
| `file_size` | yes (P7) | Same reason. |
| `encoding` | yes (P7) | Determines storage tier and read path. Mutating it after store routing has happened is ill-defined. |
| `content` (Tier A inline) | yes (P7) | If A→B/C migration is ever needed, P9 does it through a re-register, not an in-place mutation. |
| `content_ref` (Tier C pointer) | yes (P7) | Object-store key is content-addressed via `blob_sha`; mutating divorces the bead from the object. |
| `secrets_redacted`, `secrets_summary` | **no** (mutable) | P3 § 6 retro-redact explicitly needs to update these when the rule set changes. |
| Provenance fields (`commit_message`, `commit_author`, `committed_at`, `branch`, `parent_commit_sha`, `prior_blob_id`, `gitlab_*`, `source`, `origin_bead`) | no (last-writer-wins per P1 § 3.5) | Different agents may know different parts of the provenance bundle; we want the union. |

The handler-level enforcement: on `UpdateBead`, the converter compares incoming fields against the current bead row. Any change to an immutable field returns 409 with the field name in the error. P3's retro-redact tool uses a privileged path that touches only `content`/`secrets_redacted`/`secrets_summary` (P3 § 6.2) and is exempt from the content-shape immutability — its scope is precisely those mutable fields, so no conflict.

### 2.5 Counter-arguments considered

- *"Make `closed` automatic when the natural-key resolves to a deleted upstream object."* No. The bead system has no live link to upstream; we'd need a webhook or a periodic poll, both out of scope (plan-of-record § 13). Closing remains operator-driven.
- *"Allow `in_progress` for blobs that are mid-secret-scan."* No. P3 specifies the scan happens **synchronously inside the create handler** before persistence; there is no `pending-scan` state. If we ever need an async path, a separate `pending` kind makes more sense than abusing `git-blob`'s status.

## 3. Decision: orphan GC at 90 days

### 3.1 The orphan predicate (decision **L-1**)

A `git-blob` bead `B` is **orphan** at time `T` iff every one of the following holds:

| # | Condition | Source table |
|---|---|---|
| O-1 | No `deps` row with `depends_on_id = B.id`. | `deps` |
| O-2 | No `bead_references` row with `target_bead_id = B.id`. | `bead_references` (covers untyped mentions and typed-mention edges per CLAUDE.md "Typed mention syntax") |
| O-3 | No other `git-blob` bead `B'` with `B'.fields ->> 'prior_blob_id' = B.id`. | `beads.fields` JSONB lookup, `type = 'git-blob'` only |
| O-4 | No bead (any kind) with `fields ->> 'origin_bead' = B.id`. | `beads.fields` JSONB lookup |
| O-5 | `B.created_at < T - 90 days`. | `beads.created_at` |
| O-6 | `B.deleted_at IS NULL`. | `beads.deleted_at` (already-soft-deleted blobs are out of scope; they're on the purge pathway already). |

If all six hold, `B` is **eligible for orphan soft-delete**.

This is the decision L-1: the orphan predicate is the **union of all known graph-edge sources** that can point at a `git-blob`. It is intentionally not just "no `bead_references` row" — that would miss `origin_bead` produces-edges from work beads (a write-time pointer that does not always go through text extraction), and it would miss `prior_blob_id` chain-of-revision edges that P0 § 4.3 added precisely so a successor blob keeps the predecessor reachable.

L-1 alternatives considered:

| Alternative | Why rejected |
|---|---|
| Just `bead_references`. | Misses `origin_bead` (set by agents who wrote the blob, not by text mention) and `prior_blob_id` (chain head retains tail). |
| Just `bead_references` + `deps`. | Same — misses `origin_bead` and `prior_blob_id`. |
| Add a materialized `blob_in_use(blob_id, last_seen)` table updated on every write. | Premature optimization. The four checks above are partial-indexable; running them once a day for the slice of blobs older than 90 days is cheap relative to the volumes in P2 § 4. Revisit only if the sweeper's runtime exceeds budget. |
| Treat any `git-blob` whose repo is in the allowed list as in-use. | Would never GC. Defeats the purpose. |

### 3.2 Why 90 days

Plan-of-record § 9.2 sets 90 days. Two practical reasons reinforce it:

1. **Alignment with the existing purge pathway.** The 30-day soft-delete window from migration `009` runs *after* the orphan sweeper sets `deleted_at`. Total retention from create to hard-delete for an orphaned blob is **120 days minimum** (90 untouched + 30 in soft-delete). That is comfortably longer than any realistic agent-iteration window (a single epic almost never spans 90 days; if it does, it is generating new mentions and the blob is not orphan).
2. **Volume of work-tracking history.** Beads themselves age out at similar timescales. 30d is too aggressive (a closed epic frequently re-opens within a month for a follow-up); 365d is too lax (cost model in P2 § 4 assumes meaningful turnover). 90d sits at the natural release-cycle boundary.

The window is per-fleet configurable (`BEADS_GIT_BLOB_ORPHAN_DAYS`, default 90). P7 specifies the knob; P9 wires it.

### 3.3 The sweeper job (spec, not implementation)

A new background sweeper, scheduled daily (with jitter), runs the predicate and soft-deletes eligible blobs. Pseudocode:

```
job orphan_git_blob_sweeper:
  cutoff := NOW() - configured_orphan_days  -- default 90d

  -- Candidate set: open or closed (NOT already soft-deleted), older than cutoff,
  -- type = git-blob, kind = data. Bounded LIMIT per run; resumable across runs.
  for each candidate B in:
      SELECT id FROM beads
       WHERE type = 'git-blob'
         AND deleted_at IS NULL
         AND created_at < $1::timestamptz
       ORDER BY created_at ASC
       LIMIT $2  -- e.g. 1000 per run

  do:
    -- All four edge checks in one round-trip
    SELECT
      EXISTS(SELECT 1 FROM deps WHERE depends_on_id = B.id) AS d,
      EXISTS(SELECT 1 FROM bead_references WHERE target_bead_id = B.id) AS r,
      EXISTS(SELECT 1 FROM beads
              WHERE type = 'git-blob' AND fields ->> 'prior_blob_id' = B.id
                AND deleted_at IS NULL) AS p,
      EXISTS(SELECT 1 FROM beads WHERE fields ->> 'origin_bead' = B.id
                AND deleted_at IS NULL) AS o;

    if NOT (d OR r OR p OR o):
      -- Soft-delete via existing path; reuses the deleted_at machinery.
      UPDATE beads SET deleted_at = NOW(), updated_at = NOW() WHERE id = B.id AND deleted_at IS NULL;
      RecordEvent(actor='system:orphan-gc', action='delete', target=B.id, reason='orphan-90d')
      Publish(event)
```

Notes:
- The job runs as `system:orphan-gc` actor so the events row distinguishes it from operator-initiated deletes. CLAUDE.md "Landing the plane" item #5 mandates RecordEvent + Publish.
- The two `fields ->> '<key>'` predicates are JSONB lookups. P9 should add (in the same migration that ships the sweeper) two partial expression indexes:
  - `CREATE INDEX idx_beads_fields_prior_blob_id ON beads ((fields ->> 'prior_blob_id')) WHERE type = 'git-blob' AND fields ? 'prior_blob_id';`
  - `CREATE INDEX idx_beads_fields_origin_bead ON beads ((fields ->> 'origin_bead')) WHERE fields ? 'origin_bead';`
  These index existence is part of the P7 contract because the sweeper is a hot loop — without them the JSONB scan is full-table.
- LIMIT-batching keeps each job invocation bounded; the next run picks up where the last left off via the `created_at` ordering.
- The sweeper is **idempotent**: running it twice in a row on the same data is a no-op on the second pass (every eligible row is now `deleted_at != NULL`).

### 3.4 Restoration window

Within the 30-day soft-delete window, an operator can `kd restore <id>` and the blob is back to `open` (or its prior status). The restore path does not need any blob-specific extension — the existing `kbeads/cmd/kd/restore.go` already works for any soft-deleted bead.

If the orphan condition still holds 24h after restore, the sweeper will re-soft-delete on the next run. To prevent thrash, P9 will add a `restored_at` column or an event-history check so a recently-restored blob gets a fresh 90d grace. Spec: **a blob whose most recent `event = restore` happened within `configured_orphan_days` is NOT eligible for orphan GC**. This is a two-line predicate addition in the sweeper, not a schema change.

### 3.5 What the sweeper does NOT do

- It does not hard-delete. Hard-delete is the existing 30-day purge sweep on `deleted_at`.
- It does not consult upstream (GitLab API, git refs). The plan-of-record says no live mirror; the sweeper honors that.
- It does not delete `git_blob_content` rows or S3 objects. Those happen at hard-delete (§ 4 below).
- It does not look at typed mentions inside Slack messages or external systems — only at edges materialized inside kbeads (`deps`, `bead_references`, `fields ->> 'prior_blob_id'`, `fields ->> 'origin_bead'`). External pointers are the operator's problem.

## 4. Decision: hard-delete cascade and `git_blob_content` refcount

### 4.1 The cascade

When a `git-blob` bead is **hard-deleted** (the 30-day purge sweep fires, or an admin runs the eventual `kd admin purge <id>` command), the create-handler's mirror in the delete handler runs:

```
on hard-delete(git-blob bead B):
  fields := B.fields  -- snapshot before row goes away

  BEGIN;

    -- Tier A: inline content. Nothing to do — the bytes live in the bead row
    -- and disappear with it via DELETE.

    -- Tier B: shared content table.
    if fields.encoding != 'omitted'
       AND NOT (fields ? 'content')         -- not Tier A
       AND NOT (fields ? 'content_ref')     -- not Tier C
       AND fields ? 'blob_sha':
        UPDATE git_blob_content
           SET refcount = refcount - 1, updated_at = NOW()
         WHERE blob_sha = fields.blob_sha;
        DELETE FROM git_blob_content
         WHERE blob_sha = fields.blob_sha AND refcount = 0;

    -- Tier C: object store. Do NOT delete here. Mark for later sweep
    -- (see § 4.3). Reasoning: synchronous S3 deletes inside a DB tx
    -- are wrong (no rollback), and a network blip would partially
    -- complete the delete.

    -- bead row + cascaded FKs (deps, comments, bead_references, bead_embeddings)
    -- via existing ON DELETE CASCADE constraints.
    DELETE FROM beads WHERE id = B.id;

  COMMIT;
```

This matches P2 § 3.3 exactly; P7 binds the *triggering condition* (purge after soft-delete window) to the existing handler logic.

### 4.2 Why refcount lives on the content table, not on the bead

Already justified in P2. P7 does not relitigate. One additional consequence locked here: **soft-delete does NOT decrement refcount**. The content row stays alive across the soft-delete window so a `kd restore` is cheap (the bytes are still there). This matches P2 § 3.3 final paragraph and the existing `bead_embeddings` lifecycle precedent.

### 4.3 Tier C object-store sweeper

Tier C objects in S3 are not refcounted (P2 § 3.3 final pseudocode). A separate sweeper, scheduled daily, runs:

```
job s3_unreferenced_object_sweeper:
  -- For each object in s3://<bucket>/<key_prefix>/...
  for each object key K with blob_sha extracted from K:
    if NOT EXISTS (
      SELECT 1 FROM beads
       WHERE type = 'git-blob'
         AND fields ->> 'blob_sha' = blob_sha_of(K)
    ):
      -- No live or soft-deleted bead references this object.
      DeleteObject(K)
      RecordEvent(actor='system:s3-gc', action='delete', target=K, reason='unreferenced')
```

Notes:
- The check is against **live or soft-deleted** beads (i.e. all rows, including `deleted_at IS NOT NULL`) so a soft-deleted bead's S3 object is not deleted out from under a possible restore. Hard-deleted rows are gone, so their S3 objects become unreferenced and the sweep takes them.
- Listing the bucket can be expensive at scale. P9 will batch with `MaxKeys=1000` and resume via continuation token across job runs.
- Two-phase: (1) list candidate keys, (2) check DB. Never the reverse — listing the DB then deleting from S3 races against new writes.
- Optional optimization (P9, not part of P7's contract): mirror Tier C deletions into a `pending_s3_deletes` table at hard-delete time so the sweeper does targeted deletes instead of a full bucket scan. P7 picks the bucket-scan default because it works without any new schema; the mirror table is an optimization the implementer may add if listing cost dominates.

### 4.4 Per-fleet cleanup

The `git_blob_content.fleet` column (P2 § 3.1) lets a multi-fleet deployment scope refcounting. P7's contract: refcount maintenance is per-`(blob_sha, fleet)` if and only if the content row is fleet-tagged. P2 ships `fleet TEXT NOT NULL DEFAULT 'default'` on the content table; P7 specifies that the `INSERT … ON CONFLICT (blob_sha)` upsert in P2 § 3.3 must be rewritten as `ON CONFLICT (blob_sha, fleet)` once the schema acquires that composite key.

Practical consequence: P9 should change the migration so the primary key is `(blob_sha, fleet)`, not `blob_sha` alone. This is a **revision to P2** that P7 surfaces — see § 7 (Open follow-ups) for the formal flag.

## 5. Decision: repo-deletion semantics

### 5.1 The decision matrix

When repo `R` is removed from the fleet's allowed-projects list (the project-scoped write policy revokes write access), three categories of behavior:

| Operation | Behavior |
|---|---|
| `kd git-blob add` (or any new write) targeting `R` | **403** with `error: "repo R is not in the fleet's allowed-projects list"`. Same error path as any project-scope violation. |
| `kd show <blob-id>` for a blob whose `repo = R` | **200**. Reads are preserved — the blob is a historical record. |
| `kd refs <blob-id>` etc. | **200**. Graph edges are preserved. |
| `kd list --type=git-blob` (default) | **Excludes** blobs whose `repo` is in the fleet's removed-projects list. |
| `kd list --type=git-blob --include-removed-repos` | **Includes** them. New flag on the existing `kd list` command. |
| Orphan GC sweeper | **Runs unchanged.** A blob from a removed repo can still be orphaned and aged out the normal way; removed-repo state does not extend retention. |
| `kd close <blob-id>` for a blob whose `repo = R` | **200.** Operator can manually close a blob from a removed repo to remove it from the `--include-removed-repos` view too. |
| Bulk hard-delete of all blobs from `R` | **Operator-initiated only.** New admin command `kd admin purge-repo <R>` (spec only — implementation is P9). Audit-bead per blob is excessive; one summary audit-bead listing the count is sufficient. |

### 5.2 Why not auto-delete on repo removal

Three reasons to **not** auto-delete:

1. **Reversibility.** Repo removal is sometimes a misclick or a temporary policy change. If the data is gone, restoring access does not bring back the snapshots.
2. **Audit / forensics.** A blob that was registered while the repo was allowed is still a valid record of what existed at that commit. Removing it because the policy changed today destroys yesterday's evidence.
3. **Cost discipline.** If a fleet does want bulk cleanup, the explicit `kd admin purge-repo` makes the intent legible in the event log. Implicit auto-delete is a footgun.

The default — read-preserved, write-frozen, list-hidden — is the conservative position the plan-of-record § 9.3 already proposes. P7 just nails down the exact UX.

### 5.3 How "removed-projects" is checked

The fleet's allowed-projects is already implied by the `project_scoped` write policy. P7 does not add a new "removed" set — the absence of a repo from the allowed-projects list is the signal. Practically, the write-handler check is:

```
on create(git-blob bead with repo = R):
  if R not in fleet.allowed_projects:
    return 403  -- regardless of whether R was ever allowed
```

The list-default exclusion is symmetric:

```
on list(filter type = git-blob):
  if NOT include_removed_repos:
    add WHERE fields ->> 'repo' = ANY (fleet.allowed_projects)
```

This means a fleet that has never allowed `R` and a fleet that allowed-then-removed `R` look identical at the policy layer — both reject writes and exclude from default lists. The only operational distinction is whether any blobs *exist* from `R`. That distinction is queryable but does not change behavior.

### 5.4 What this leaves to P8 (bridge / rollout)

P8 will spec the auto-registration trigger (gitlab-bridge on merge). P7 establishes that **the bridge MUST consult the same allowed-projects check** before issuing a `git-blob` write. P8 inherits the 403 behavior; it does not get a special bypass.

### 5.5 Counter-arguments considered

- *"Removed repos should hide blobs immediately, not just from default lists."* No — that breaks the historical-record contract. Hide-from-default-list is the right balance: nobody trips over them in normal use, but explicit lookups still work.
- *"Add a `repo_removed_at` field on the blob."* Unnecessary. The fleet config is the source of truth; mirroring it onto every blob duplicates state and creates a sync problem.

## 6. Decision: refcount-repair migration

### 6.1 The shape

P2 ships `014_git_blob_content` with the table and the application-side refcount. P7 ships an additional migration:

```sql
-- 015_git_blob_content_refcount_repair.up.sql
-- Forward-only migration. Recomputes refcount from the live bead set as a
-- defense-in-depth fixpoint. Idempotent: running on a correctly-counted
-- table is a no-op.

BEGIN;

WITH live_refs AS (
    SELECT (fields ->> 'blob_sha') AS blob_sha,
           COALESCE(fields ->> 'fleet', 'default') AS fleet,
           COUNT(*) AS n
      FROM beads
     WHERE type = 'git-blob'
       AND deleted_at IS NULL
       AND fields ? 'blob_sha'
       AND NOT (fields ? 'content')        -- exclude Tier A inline
       AND NOT (fields ? 'content_ref')    -- exclude Tier C
     GROUP BY 1, 2
)
UPDATE git_blob_content c
   SET refcount = COALESCE(lr.n, 0),
       updated_at = NOW()
  FROM (
      SELECT blob_sha, fleet, n FROM live_refs
      UNION
      SELECT c.blob_sha, c.fleet, 0
        FROM git_blob_content c
        LEFT JOIN live_refs lr USING (blob_sha, fleet)
       WHERE lr.blob_sha IS NULL
  ) lr
 WHERE c.blob_sha = lr.blob_sha
   AND c.fleet    = lr.fleet;

-- Then prune any rows whose recomputed refcount is 0. Aligns with the
-- application invariant `refcount > 0` for live rows.
DELETE FROM git_blob_content WHERE refcount = 0;

COMMIT;
```

```sql
-- 015_git_blob_content_refcount_repair.down.sql
-- No rollback. Refcount repair is forward-only: rolling back doesn't
-- "un-recompute" the prior values, and migration 014's down already
-- drops the table.
SELECT 1;  -- no-op
```

### 6.2 Why this migration exists at all

Three reasons:

1. **Defense in depth.** P2's refcount is application-maintained. Any partial deploy where the Tier B path was wrong (e.g. an early P9 bug, a partially-applied hot-fix) leaves drift. Recompute on schema upgrade is cheap and self-correcting.
2. **Test environment hygiene.** `gardenio-rwx` and `gardenio-next` may end up with synthetic `git_blob_content` rows from agent experimentation. The repair migration normalizes them on the next deploy without operator intervention.
3. **Composite-key change.** If P7's recommendation to make the PK `(blob_sha, fleet)` (§ 4.4) is adopted, the repair migration is the natural place to enforce the new key — `015` runs after the PK change in `014` revision and re-computes against the composite.

### 6.3 Why forward-only

Migration `down` paths for refcount repair are nonsensical — there is no prior numeric state to restore to. `015.down.sql` is a no-op. If someone needs to roll back, they roll back to `014`'s baseline and accept the recomputed values; they do not "uncompute".

### 6.4 What this migration does NOT do

- It does not change schema. No new columns, no new indexes (those are P7 § 3.3 partial expression indexes, which are part of the orphan-sweeper deployment, not this migration).
- It does not touch S3. Tier C reconciliation is the runtime sweeper's job (§ 4.3).
- It does not delete beads. Only `git_blob_content` rows.

## 7. Open follow-ups (not blocking P7)

These are issues P7 surfaces and recommends but does not lock — P9 / P2 revision can address them independently.

| # | Follow-up | Recommendation |
|---|---|---|
| F-1 | `git_blob_content` PK should be `(blob_sha, fleet)`, not `blob_sha` alone (see § 4.4). | File as a revision request against the P2 ADR. The change is a one-line PK swap in `014.up.sql` plus the upsert change in `git_blob_content` insert. |
| F-2 | A `restored_at` event-history check in the sweeper to prevent thrash (§ 3.4). | P9 implementer's choice — column or event-derived predicate. The contract here is that recently-restored blobs get a fresh window; the implementation is open. |
| F-3 | An optimization: `pending_s3_deletes` table to avoid bucket-scan in § 4.3. | P9 may add if profiling shows full-bucket-listing dominates sweeper cost. Not a P7 deliverable. |
| F-4 | Cross-fleet shared blobs: today the design isolates by `fleet`. If two fleets register the same `(repo, commit_sha, path)` they pay storage twice. Cross-fleet dedup is out of scope (plan-of-record § 12). | Out of scope. Recorded for future reference. |
| F-5 | Compression of long-soft-deleted Tier B content (e.g. zstd in-place after 30d). | Out of scope. Plan-of-record § 13 does not contemplate it; revisit only if storage costs in P2 § 4 are exceeded. |

## 8. What this ADR does NOT decide

- **Scanner false-positive retro-redact** lifecycle. P3 owns it. P7 only specifies that retro-redact mutates `secrets_redacted` / `secrets_summary` / `content` and is exempt from § 2.4's content-shape immutability; the *when* and *who* of retro-redact runs is P3.
- **Embedding row lifecycle**. Bead embeddings (`bead_embeddings` table from migration `005`) cascade on hard delete via the existing FK; no new behavior needed. P6 (ka-0000005801) owns the indexing-policy decision; P7 inherits the cascade unchanged.
- **Bridge / auto-registration backfill**. P8.
- **Volume-blow-up early-warning metrics** (Prometheus alerts on `git_blob_content` row count growth, S3 bucket size, sweeper backlog). Spec-able here, but the metrics layer is an observability concern that lands with the kbeads serve runtime. P9 implementer should expose at minimum `git_blob_orphan_sweeper_eligible_total`, `git_blob_orphan_sweeper_soft_deleted_total`, and `git_blob_content_refcount_zero_purged_total` counters.
- **`gb decision` for an operator fork**. P7 does not raise one. Every choice here either flows from a prior ADR or is an internal mechanic. The implementation epic (P9) authorization is the operator gate; P7 itself does not need a per-decision approval.

## 9. Consequences

### 9.1 For the rest of the research epic

- **P8 (bridge & rollout)** — inherits the repo-deletion 403 behavior; does not get a bypass. Backfill story for pre-existing repo histories must consider the orphan sweeper window (a backfilled blob with no current mention will start its 90d clock at backfill time, not at the historical commit time, since `created_at` is the bead's create time).
- **P9 implementation epic** — has a complete contract for the create-handler immutability check, the type-config `lifecycle` extension, the orphan sweeper, the S3 unreferenced-object sweeper, the partial-expression indexes, and the refcount-repair migration. Together with P0–P6's contracts, P9 has every "what" it needs.

### 9.2 For the eventual P9 implementation

A non-exhaustive checklist:

1. Add `LifecyclePolicy` to `model.TypeConfig` and the type-config converter in `internal/server/convert.go`.
2. Register `git-blob` with `lifecycle: { allowed_statuses: ["open", "closed"], immutable: true, no_in_progress: true }` in `internal/server/config.go`.
3. Extend `UpdateBead` handler to consult `Lifecycle.Immutable` and reject changes to identity + content-shape fields. Reuse the field-list from P1 § 3.5 and P7 § 2.4. The P3 retro-redact privileged path uses a separate handler (or a write-policy bypass) scoped to `secrets_redacted` / `secrets_summary` / `content`.
4. Add the orphan-sweeper job. Schedule, batching, and event-recording per § 3.3.
5. Add the S3 unreferenced-object sweeper. Schedule and batching per § 4.3.
6. Add the two partial expression indexes on `beads.fields`.
7. Ship migration `015_git_blob_content_refcount_repair`.
8. Wire the configurable knob `BEADS_GIT_BLOB_ORPHAN_DAYS` (default 90) and document it in `kbeads/CLAUDE.md` § Configuration.
9. Add the three Prometheus counters (§ 8 last bullet).
10. Add `kd admin purge-repo <R>` command (§ 5.1 last row) — admin-only, audit-bead emitted, idempotent.

### 9.3 For operators

- An operator who registers a `git-blob` and never references it from any other bead will see it disappear from default views after **120 days** (90 orphan + 30 soft-delete) unless they explicitly reference it or close it manually.
- An operator who removes a repo from the allowed list does not lose history; it just falls out of default lists.
- An operator who needs an "I deleted everything for repo R" cleanup runs `kd admin purge-repo R` and gets one summary audit bead. P9 will document the command.
- The sweeper runs daily and is logged; operators can confirm via `kd events --actor system:orphan-gc`.

### 9.4 Cost / risk

- **Sweeper full-table scan risk.** Mitigated by the two new partial expression indexes (§ 3.3). Without them, the sweeper degrades to O(N) JSONB lookups per candidate. The index spec is part of the P7 contract.
- **S3 sweeper listing cost.** Mitigated by daily cadence and continuation-token batching. F-3 (mirror table) is the escape valve if cost grows.
- **Refcount drift.** Mitigated by the periodic repair-migration (§ 6) and by the hard-delete being the only refcount-decrement path (so concurrency bugs cannot under-count).
- **Restore thrash.** Mitigated by F-2 (recently-restored exclusion).
- **Sweeper deletes a still-needed blob due to a missing edge type.** This is the L-1 risk surface. The four-edge predicate is the union of *all* materialized edge sources today. If the typed-mention research (ka-0000001333) introduces a new edge table outside `bead_references`, P7 must be revised to add it to the predicate; otherwise blobs reachable only via the new edge become silent orphan targets.

## 10. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. P0, P1, P2 ADRs are merged (already true at branch creation: 0876eefd / 79f48801 / f4ebd6c0).
3. P9 (implementation) can begin against the contract locked here, with no further ADR work needed for lifecycle / GC / repo-deletion / refcount.
4. F-1 (composite PK on `git_blob_content`) is filed as a revision request against the P2 ADR or formally deferred.

This ADR is **not** the green light to add the `LifecyclePolicy`, write the orphan sweeper, ship `015_*`, or wire the new env knob. Those are P9 deliverables, after the operator authorizes the implementation epic per plan-of-record § 11.

## 11. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (§ 9 lifecycle, § 5 storage, § 12 risks).
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442).
- P1 ADR: `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044).
- P2 ADR: `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323).
- P3 ADR: `kbeads/docs/adr/git-blob-P3-security-redaction.md` (ka-0000007332) — retro-redact path is an explicit immutability exemption (§ 2.4).
- Existing soft-delete: `kbeads/internal/store/postgres/migrations/009_soft_delete.up.sql`; soft-delete handler `kbeads/internal/store/postgres/queries.go:549`.
- Type-config machinery: `kbeads/internal/model/type_config.go`, `kbeads/internal/server/config.go`.
- Bead-references CASCADE behavior: `kbeads/internal/store/postgres/migrations/010_bead_references.up.sql` (`source_bead_id ... ON DELETE CASCADE`).
- CLAUDE.md "Landing the plane" event-recording mandate: `kbeads/CLAUDE.md` § Landing the plane #5.
- Companion phases (in flight at time of writing):
  - P4 (ka-0000004396) — ingestion CLI / API
  - P5 (ka-0000002819) — graph integration
  - P6 (ka-0000005801) — embedding & search policy
  - P8 (not yet scheduled) — bridge / rollout
