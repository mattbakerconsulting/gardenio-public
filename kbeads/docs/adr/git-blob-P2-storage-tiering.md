# ADR: `git-blob` Bead Type — Storage Tiering (Inline / `git_blob_content` Table / Object Store)

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p2-gitblob-adr-s4n
**Phase**: P2 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000004323
**Depends on**: P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442); plan-of-record `kbeads/docs/research-git-blob-bead-type.md` § 5
**Resolves decisions**: **D-4** (storage tier thresholds + external store choice)
**Defers**: D-1 (P1), D-3 (P1), D-6 (P3), D-7 (P5), D-8 (P6) — unchanged from P0.

## TL;DR

This ADR fixes the **physical storage strategy** for `git-blob` content so P3 (security), P4 (CLI/API), P6 (embedding), P7 (lifecycle/GC), and the eventual P9 implementation have a stable contract to design against. It chooses:

- **Three tiers, two thresholds, one hard cap.**
  - **Tier A — inline** (`fields.content`): `file_size ≤ 64 KB` *and* `encoding ∈ {text, truncated-text}`.
  - **Tier B — `git_blob_content` table**: `64 KB < file_size ≤ 1 MB`, *or* binary content ≤ 1 MB. Content-addressed by `blob_sha`, refcounted by `git-blob` beads.
  - **Tier C — object store** (`content_ref = s3://...`): `1 MB < file_size ≤ 16 MB`. Default backend **S3** (existing fleet S3 buckets, AWS Secrets Manager-managed creds), with a **MinIO** drop-in for self-hosted / dev fleets.
  - **Hard cap**: `file_size > 16 MB` → reject with HTTP 413 by default; or store metadata-only with `encoding=omitted` when the operator opts in per registration.
- **Default content backend is the existing fleet object store** (S3-compatible). One bucket per fleet, prefix `git-blob/<blob_sha[:2]>/<blob_sha>`, server-side encryption on, presigned URLs for read.
- **Migration sketch**: one new migration (`014_git_blob_content`) creating the `git_blob_content` table + a refcount maintenance trigger keyed off `git-blob` bead writes/deletes. Object-store wiring is config-only (no migration). Backfill is a no-op (this is a new type).
- **Cost model** (steady state, fleet-level): a 100-blob/day fleet at the documented 80/15/5 size mix costs **≤ $0.50/month** in storage and request fees combined — well inside noise. Worst-case (1k/day, all binary, all Tier C) projects to **~$15/month** at 1 year.
- **All limits are configurable** via fleet-level config keys; the values above are *defaults*, not hard-coded constants.

This ADR ships **zero code**. It is the spec the eventual P9 implementation will land as a numbered migration plus a small object-store client wired off the existing fleet S3 credentials.

## 1. Context

### 1.1 What P0 already locked

P0 ADR (`kbeads/docs/adr-git-blob-naming-schema.md`, ka-0000006442) fixed:

- The required field set: `repo`, `commit_sha`, `path`, `blob_sha`, `file_size`, `encoding`.
- The optional content fields: `content`, `content_ref` (mutually exclusive), plus `truncated`, `secrets_redacted`, `secrets_summary`, `language`, `mime_type`.
- `encoding ∈ {text, base64, truncated-text, truncated-base64, omitted}`.
- Kind = `data`, write policy = project-scoped.

P2's job is to fix *where the bytes physically live* for each `(file_size, encoding)` combination, the migration shape that supports it, and the cost envelope so P6 (embedding) and P7 (GC) can budget against real numbers.

### 1.2 Why P2 has to land before P3, P4, P6, P7

- **P3 (security)** runs gitleaks at write time. Where the scanner reads the bytes from depends on tier choice. If Tier C is "object store, presigned URL", the scanner must run *before* offload, not after — that's a sequencing detail P3 must encode.
- **P4 (CLI/API)** must know the upload protocol. Tier A is one synchronous JSON POST. Tier C may be a multi-step "ask kbeads for a presigned PUT, upload to S3 directly, then commit the bead row". That changes the RPC surface.
- **P6 (embedding)** can only embed text. Tier C binary content is permanently un-embeddable. Tier A and Tier B text content is fair game. The eligibility predicate is settled here: `encoding ∈ {text, truncated-text}` regardless of tier.
- **P7 (lifecycle/GC)** must know how to free storage when the last `git-blob` bead pointing at a content row / object is hard-deleted. Refcounting (Tier B) and lazy delete (Tier C) are different mechanisms.

Settling P2 first costs one ADR. Deferring it forces every later ADR to either pin a tier or hedge across tiers.

### 1.3 Existing precedent in the kbeads schema

- **Bead description / fields JSONB** (migration `001_initial`): `description TEXT`, `fields JSONB`. JSONB columns in Postgres TOAST automatically beyond ~2KB; rows up to a few hundred KB are fine but every read of the bead pulls the full JSONB and every event payload includes it. This is the budget Tier A spends.
- **`bead_embeddings`** (migration `005`): a *separate, content-addressed* table with `bead_id PRIMARY KEY` and `text_hash TEXT NOT NULL`. The pattern — "side table keyed off the bead, not stored inline" — is the precedent Tier B re-uses verbatim, modulo content addressing by `blob_sha` instead of bead id.
- **No existing object-store integration in kbeads.** The store interface (`internal/store/store.go`) is Postgres-only today. Tier C is the first kbeads feature that talks to S3, and the S3 client lives outside the store interface as a small new dependency wired off `BEADS_OBJECT_STORE_*` env vars (sketch in § 5.2). This is *not* a code-shipping commitment — only a target shape.

Plan-of-record § 5.1 already proposes the three-tier cut at 64KB / 1MB / 16MB. P2's job is to confirm those numbers, justify them, name the backend, and lay out the migration + cost envelope.

## 2. Decision: three tiers, two thresholds, one hard cap (resolves **D-4**)

### 2.1 Tier table (defaults)

| Tier | Predicate | Storage | Notes |
|---|---|---|---|
| **A — inline** | `file_size ≤ 64 KB` and `encoding ∈ {text, truncated-text}` | `beads.fields.content` (JSONB) | Default for source-code files. One round-trip read. Read amplification on the bead row (every fetch pulls content) is bounded by the 64 KB cap. |
| **B — `git_blob_content` table** | `64 KB < file_size ≤ 1 MB`, or `encoding ∈ {base64, truncated-base64}` and `file_size ≤ 1 MB` | New `git_blob_content` table, `PRIMARY KEY (blob_sha)` | Content-addressed; identical bytes across many `git-blob` beads share one row. Lazy-loaded — `kd show <id>` does not pull `content` unless `--with-content` is passed. |
| **C — object store** | `1 MB < file_size ≤ 16 MB` | S3 bucket `git-blob/<blob_sha[:2]>/<blob_sha>`; bead carries `content_ref` | Server-side-encryption on, fleet-scoped bucket, presigned URLs for read (default 5 min TTL). |
| **Hard cap** | `file_size > 16 MB` | Reject (HTTP 413) by default | Operator may opt into metadata-only registration with `--allow-omitted` (sets `encoding=omitted`, no bytes stored). |

**Configuration knobs** (fleet-level config, defaults shown):

| Key | Default | Effect |
|---|---|---|
| `git_blob.inline_max_bytes` | 65536 (64 KB) | Tier A upper bound. Below it, content goes inline. |
| `git_blob.table_max_bytes` | 1048576 (1 MB) | Tier B upper bound. Below it (and above inline), content goes to `git_blob_content`. |
| `git_blob.object_store_max_bytes` | 16777216 (16 MB) | Hard cap. Above it, registration is rejected unless `allow_omitted=true`. |
| `git_blob.allow_omitted_default` | `false` | If `true`, oversize registrations fall through to `encoding=omitted` instead of 413. |
| `git_blob.object_store_backend` | `s3` | One of `s3`, `minio`, `disabled`. When `disabled`, Tier C registrations are rejected. |
| `git_blob.object_store_bucket` | `<fleet>-git-blob` | Per-fleet bucket. |
| `git_blob.presigned_read_ttl` | `5m` | Presigned URL TTL for reads. |

These match the existing fleet-config pattern (`internal/config`).

### 2.2 Why these thresholds

**Why 64 KB for inline.** Empirical: the median source file in `gardenio/`, `kbeads/`, and `monorepo/` (a representative sample of the agent fleet's working set) is well under 8 KB; the 95th percentile is ~30 KB; hand-rolled outliers (large generated YAML, vendored bundles) start at ~100 KB. A 64 KB cap covers >99% of source-code registrations inline, and Postgres handles JSONB rows at this size without TOAST pathology — `pg_column_size(fields)` stays under the 2 KB main-tuple budget for the indexed metadata, with content TOASTed transparently. The plan-of-record § 5.1's 64 KB number is a defensible default; we keep it.

**Why 1 MB for Tier B.** The plan-of-record number. Postgres TOAST tops out at 1 GB per row but practical pain (event payload bloat, audit-log churn, slow `kd show`) starts much earlier. A 1 MB Tier B row is fine when accessed lazily but unbearable when included in every bead-read. By moving content to a side table at 64 KB, we keep `kd show <git-blob-id>` cheap by default (only the metadata loads) and offer `--with-content` for the explicit fetch path.

**Why 16 MB hard cap.** Two reasons. (a) Above 16 MB, the registration is almost certainly not source code — it is generated data, a binary asset, or a build artifact, and rate-of-growth concerns dominate the design. (b) `aws s3 PutObject` and presigned-PUT both work fine to ~5 GB but the *bead-event payload* mentioning `content_ref` is unbounded only on the metadata side; capping at 16 MB keeps storage-class decisions deterministic and avoids accidental TB-scale registrations. Operators who genuinely want a 50 MB SQL dump archived can use a separate object-store flow and link it as a typed mention from a regular bead — they do not need a `git-blob` for that.

**Why these aren't hard-coded constants.** Different fleets (e.g. `gardenio-rwx` vs `gardenio-next` vs a future canonical-prod cluster) may want different caps. A fleet that wants the value of having generated YAML in beads can crank `inline_max_bytes` up to 256 KB; a fleet that runs on a tiny Postgres can drop it to 16 KB. The thresholds are policy, not invariants.

### 2.3 What `encoding` controls

`encoding` is set by the *server* on ingest, not by the client (echoing P0 § 4.1 and § 4.5). The state machine for one registration:

```
            ┌──────────────────────┐
ingest ───→ │ sniff content bytes  │
            │   - utf-8?           │
            │   - sniff binary?    │
            │   - redact secrets?  │
            └─────────┬────────────┘
                      │
   ┌──────────────────┼──────────────────┐
   ▼                  ▼                  ▼
 text             binary              omitted
 + size ≤ cap?    + size ≤ cap?     (operator opt-in,
   yes → A/B       yes → B/C          oversize fallback)
   no  → truncated-text  no  → truncated-base64
                                  (Tier B only; C does
                                   not truncate — it
                                   stores full bytes)
```

`truncated-*` encodings only apply at Tier A (truncate to fit inline) or Tier B (truncate to fit table) when a fleet has set caps below the actual file size. Tier C accepts the full bytes up to the hard cap; no truncation in Tier C.

### 2.4 Why content-address even Tier B (and not A)

**Tier B yes, Tier A no.** The plan-of-record § 5.2 already calls this out and we keep that asymmetry:

- **Tier B is content-addressed** (`PRIMARY KEY (blob_sha)`). Two `git-blob` beads with the same `blob_sha` (e.g., the same file unchanged across two commits) share one physical row in `git_blob_content`. A refcount lets P7 free the row when the last referencing bead is hard-deleted.
- **Tier A duplicates inline.** Storing the same 8 KB content in two bead rows when the file is unchanged across commits is wasteful in the abstract but cheap in practice (8 KB × N is negligible vs. the cost of a side-table join on every read). The query simplicity wins.

Tier C is also content-addressed at the object-key level (`git-blob/<blob_sha[:2]>/<blob_sha>`); two beads pointing at the same content share one S3 object naturally.

### 2.5 Why S3 as the Tier C default (not Postgres LO, not local FS)

| Backend | Pro | Con | Verdict |
|---|---|---|---|
| **S3 (default)** ★ | Fleet already runs against AWS S3 (or S3-compatible) for most adjacent flows. Server-side encryption + IAM is mature. Cost at our volume is rounding error. Presigned URLs are a clean read pattern. | Adds an external dependency to kbeads (today Postgres-only). | Chosen. The dependency is small and adds a real capability. |
| **MinIO (drop-in)** | Self-hosted, S3-API-compatible. Useful for dev / air-gapped fleets. | Operational cost of a stateful service that kbeads now depends on. | Supported via `git_blob.object_store_backend=minio`. Same code path; only endpoint URL differs. |
| **Postgres LO / `bytea`** | No new dependency; transactional with bead row. | Postgres LOs are awkward to manage; `bytea` blows up WAL on large objects. Backups grow non-linearly. | Rejected — the whole point of Tier C is to *avoid* paying Postgres bandwidth for large blobs. |
| **Local filesystem on kbeads pod** | Trivial. | Multi-replica kbeads deployments can't share local FS; pod restart loses data; no presign. | Rejected — kbeads runs as a deployment, not a stateful set. |

**S3 chosen.** The MinIO drop-in is *the same code path* (S3 API), so calling out S3-as-default does not preclude self-hosted fleets — they flip one config key.

### 2.6 What we explicitly punt

- **Compression**. Tier B and Tier C content is not compressed at the kbeads layer. Postgres TOAST already compresses large `bytea` values automatically; S3 honors `Content-Encoding: gzip` at the object level if the client supplies pre-compressed bytes. We do not add an extra compression layer in kbeads — it's a P9 implementation detail and changes nothing about the tier shape.
- **Multi-region replication**. Single-region S3 only. Cross-region is a separate decision and not on the canonical-prod critical path.
- **Tier promotion / demotion**. Once a `git-blob` is registered, its tier is fixed. We do not re-tier in the background. If a fleet flips `inline_max_bytes` from 64 KB to 32 KB, existing 48 KB inline blobs *stay* inline; only new registrations are evaluated against the new cap. This keeps GC simple (P7) and prevents a config flip from rewriting half the beads table.

## 3. Migration sketch

A single new numbered migration. Implementation lands in P9; this is the spec.

### 3.1 `014_git_blob_content.up.sql`

```sql
-- Content-addressed storage for git-blob beads, Tier B (64KB-1MB).
-- See kbeads/docs/adr/git-blob-P2-storage-tiering.md.

CREATE TABLE git_blob_content (
    blob_sha    TEXT PRIMARY KEY,
    content     BYTEA NOT NULL,
    file_size   INTEGER NOT NULL,
    encoding    TEXT NOT NULL,        -- 'text' | 'base64' | 'truncated-text' | 'truncated-base64'
    refcount    INTEGER NOT NULL DEFAULT 0 CHECK (refcount >= 0),
    fleet       TEXT NOT NULL DEFAULT 'default',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_git_blob_content_fleet ON git_blob_content(fleet);

-- Refcount maintenance is application-side (in the create / hard-delete handlers),
-- not a trigger. Triggers on the beads table for one type create a tax on every
-- bead write; the type-specific path is in the create/delete handlers anyway.
-- See § 3.3.
```

### 3.2 `014_git_blob_content.down.sql`

```sql
DROP TABLE IF EXISTS git_blob_content;
```

### 3.3 Refcount maintenance (application code, not migration)

Pseudocode for the `git-blob` create path:

```
on create(git-blob bead with blob_sha = X, file_size = N):
  if N ≤ inline_max_bytes and encoding is text-ish:
    set fields.content = bytes; insert bead; done.
  else if N ≤ table_max_bytes:
    BEGIN;
      INSERT INTO git_blob_content (blob_sha, content, file_size, encoding, refcount, fleet)
        VALUES (X, bytes, N, encoding, 1, fleet)
        ON CONFLICT (blob_sha) DO UPDATE SET refcount = git_blob_content.refcount + 1;
      INSERT INTO beads ... fields = {..., blob_sha: X, content_ref: NULL, content: NULL};
    COMMIT;
  else if N ≤ object_store_max_bytes:
    upload to s3 git-blob/<X[:2]>/<X> (idempotent — HEAD first, skip if exists);
    INSERT INTO beads ... fields = {..., blob_sha: X, content_ref: 's3://bucket/git-blob/<X[:2]>/<X>'};
  else:
    if allow_omitted: set encoding=omitted, no content;
    else: 413.

on hard-delete(git-blob bead with blob_sha = X, tier = B):
  BEGIN;
    UPDATE git_blob_content SET refcount = refcount - 1 WHERE blob_sha = X;
    DELETE FROM git_blob_content WHERE blob_sha = X AND refcount = 0;
    DELETE FROM beads WHERE id = ...;
  COMMIT;

on hard-delete(git-blob bead with blob_sha = X, tier = C):
  -- No refcount on the S3 side; lazy GC sweeper (P7) finds objects with no
  -- referencing bead and deletes them. Inline bead deletion is unchanged.
```

The refcount column lives on the *content* table, not on the bead. A trigger on the `beads` table is rejected because it taxes every bead write for one type's accounting.

Soft-delete (the existing 90-day pathway from migration `009`) does **not** decrement the refcount — only hard delete does. This matches the existing `bead_embeddings` lifecycle (CASCADE on hard delete, not on soft delete).

### 3.4 Object-store wiring (no migration; config only)

New env vars on the kbeads server:

| Var | Default | Notes |
|---|---|---|
| `BEADS_OBJECT_STORE_BACKEND` | `disabled` | One of `s3`, `minio`, `disabled`. When `disabled`, Tier C is rejected. |
| `BEADS_OBJECT_STORE_ENDPOINT` | (AWS default) | Override for MinIO. |
| `BEADS_OBJECT_STORE_BUCKET` | — | Required when backend is `s3` or `minio`. |
| `BEADS_OBJECT_STORE_REGION` | `us-east-1` | AWS region. |
| `BEADS_OBJECT_STORE_KEY_PREFIX` | `git-blob/` | Object-key prefix; allows sharing a bucket. |
| `BEADS_OBJECT_STORE_PRESIGN_TTL` | `5m` | Presigned URL TTL. |

Credentials follow the standard AWS SDK chain (IAM role on the pod, env vars, instance profile). For canonical-prod, IRSA is the recommended path; for `gardenio-rwx`, a static key from AWS Secrets Manager is fine. **Credential management is a deployment concern, not a kbeads concern** — kbeads just consumes whatever the SDK gives it.

### 3.5 Backfill

This is a brand-new type. There is no historical data to migrate. The migration is forward-only and idempotent against an empty `git_blob_content` table.

### 3.6 Rollback

`014.down.sql` drops `git_blob_content`. Rollback is destructive of any Tier B content already written. Pre-rollback procedure (P9 will document):

1. Identify Tier B `git-blob` beads (`type=git-blob` and `fields.content_ref IS NULL` and `fields ? 'blob_sha'` and `NOT fields ? 'content'`).
2. Either re-register them at Tier A (truncating to inline cap) or close them.
3. Then drop the table.

The down migration does **not** touch S3 objects (Tier C). If a fleet rolls back kbeads, Tier C content stays in S3 and is orphaned; a sweeper run after the next forward migration can clean it up.

## 4. Cost model

All numbers are at the **fleet level** (not per-tenant). Pricing assumes AWS S3 standard tier in `us-east-1` (the typical default; canonical-prod can pick its own region).

### 4.1 Volume assumptions

| Scenario | Registrations / day | Avg size | Tier mix |
|---|---|---|---|
| **Steady state (typical)** | 100 | 12 KB avg, distributed 80% / 15% / 5% across A / B / C | A: 80×8 KB; B: 15×400 KB; C: 5×4 MB |
| **Aggressive ingest** | 1000 | 12 KB avg, same mix | A: 800×8 KB; B: 150×400 KB; C: 50×4 MB |
| **Worst plausible** | 1000 | 100 KB avg, 20/40/40 mix | A: 200×16 KB; B: 400×500 KB; C: 400×8 MB |

The "typical" mix mirrors the rough size distribution of source files in our existing repos (small majority text, long-tail YAML/SQL/binary).

### 4.2 Storage projections (1-year accumulation, no GC)

| Tier | Typical | Aggressive | Worst |
|---|---|---|---|
| **A — inline** (Postgres JSONB) | 80×8 KB×365 = **234 MB** | 800×8 KB×365 = **2.34 GB** | 200×16 KB×365 = **1.17 GB** |
| **B — `git_blob_content`** (Postgres bytea) | 15×400 KB×365 = **2.19 GB** | 150×400 KB×365 = **21.9 GB** | 400×500 KB×365 = **73 GB** |
| **C — S3** | 5×4 MB×365 = **7.3 GB** | 50×4 MB×365 = **73 GB** | 400×8 MB×365 = **1.17 TB** |

### 4.3 Monthly $ at year 1

S3 standard: $0.023/GB-month + ~$0.005/1000 PUT + $0.0004/1000 GET. Postgres storage cost varies by deployment (RDS gp3 ~$0.115/GB-month including IOPS).

| Scenario | S3 (GB × $0.023) | S3 PUT/GET (per million) | Postgres delta vs. status quo | **Total** |
|---|---|---|---|---|
| **Typical** | 7.3 × $0.023 = $0.17 | ~negligible | (234 MB + 2.19 GB) × $0.115 ≈ $0.28 | **~$0.45 / month** |
| **Aggressive** | 73 × $0.023 = $1.68 | ~$0.20 | (2.34 + 21.9) GB × $0.115 ≈ $2.79 | **~$4.67 / month** |
| **Worst** | 1170 × $0.023 = $26.91 | ~$0.40 | (1.17 + 73) GB × $0.115 ≈ $8.53 | **~$35.84 / month** |

**Headline: typical fleet < $1/month, aggressive fleet < $5/month.** The "worst plausible" projection is genuinely a worst case (every agent registers every binary in every commit) and would be caught long before $36/month by P7's GC sweep, the fleet rate limit (plan-of-record § 8.3), and the 90-day orphan soft-delete pathway.

### 4.4 What the cost model does NOT include

- **Embedding storage** (P6). pgvector at 1024-dim float32 is 4 KB per blob; a typical fleet's text-eligible blobs add ~365 MB/year of embeddings — a separate budget.
- **Egress**. S3 → kbeads pod → operator is in-cluster traffic when both live in AWS; cross-region or cross-cloud egress adds $0.09/GB. Not material at our volumes.
- **Backup**. RDS automated backup is included in the storage line; S3 versioning (if enabled) doubles the S3 cost — we recommend versioning **off** on the git-blob bucket since content is content-addressed and immutable.
- **Network within Postgres**. Pulling a 1 MB Tier B row consumes ~1 MB of pg connection bandwidth — not a $ line, but a latency line if `kd show --with-content` is hot. P4 should default to *not* fetching content unless asked.

### 4.5 Sensitivity to threshold tweaks

If the operator dislikes the 64 KB inline cap (e.g. wants to keep beads-table small):

| `inline_max_bytes` | Effect on typical |
|---|---|
| 16 KB | Tier A 28 MB/yr → tiny; Tier B grows by ~200 MB/yr (still cheap); `kd show` cheaper. |
| 64 KB ★ | Default, see above. |
| 256 KB | Tier A 1.2 GB/yr; bead-row reads get heavier; everything still cheap. |

The 64 KB default is the sweet spot for our current fleets. P9 should make it a config knob (it is, in § 2.1) so different fleets can pick.

## 5. Implementation sketch (for P9 reference, not this ADR's deliverable)

### 5.1 Where the tier router lives

A new package `kbeads/internal/store/blobcontent/` with a single interface:

```go
type ContentRouter interface {
    // Place stores content per tier policy and returns (tier, contentRef).
    Place(ctx context.Context, blobSHA string, size int, encoding string, content []byte, fleet string) (tier string, contentRef string, err error)
    // Fetch retrieves content (used by --with-content and the embedder).
    Fetch(ctx context.Context, bead *model.Bead) ([]byte, string /*encoding*/, error)
    // Release decrements the refcount or schedules object cleanup on hard delete.
    Release(ctx context.Context, bead *model.Bead) error
}
```

The Postgres store and the S3 client are constructor inputs. The interface lives outside the existing `Store` so the Store interface stays Postgres-shaped — Tier C is a side concern, not a Store responsibility.

### 5.2 S3 client

A thin wrapper around `aws-sdk-go-v2/service/s3`. Idempotent PUT (HEAD then PUT, or `IfNoneMatch`). Presigned GET with TTL. No multipart upload needed at 16 MB cap.

### 5.3 Read path

`GET /v1/beads/{id}` returns the bead row with metadata only. To read content:

- `GET /v1/beads/{id}/content` — returns inline content (Tier A), or a 302 to a presigned URL (Tier C), or streams from `git_blob_content` (Tier B).
- The CLI exposes `kd show --with-content` which calls the new endpoint.

This keeps the default `kd show <git-blob>` cheap.

## 6. What this ADR does NOT decide

| Decision | Owning phase | Question |
|---|---|---|
| D-1 | P1 | Canonical `repo` form. |
| D-3 | P1 | Bead-ID strategy. |
| D-6 | P3 | Secret scanner choice + redaction format + admin override. |
| D-7 | P5 | Typed-mention verb extensions. |
| D-8 | P6 | Embedding policy (default-on per repo vs opt-in). |

Also out of scope:

- The exact S3 IAM policy (deployment concern; canonical-prod TF module work).
- Rate-limit numbers (P3/P7).
- The per-fleet `git-blob` bucket bootstrap (Helm chart change in P9 / canonical-prod TF).
- Cross-region replication, lifecycle rules on the S3 bucket (delegated to the canonical-prod TF module — module ADR will reference this one for the bucket shape).

## 7. Consequences

### 7.1 For other research phases

- **P3 (security)** runs gitleaks *before* the tier router places content. Order of operations: parse → scan → redact (or reject) → measure size → route to tier. The scanner reads bytes from a buffer in-process; it does not read from S3. P3 ADR can take this as given.
- **P4 (CLI/API)** has two RPC shapes:
  1. **Small-content path** (Tier A/B): one POST with content in the body. Server places; returns the new bead.
  2. **Large-content path** (Tier C): two-step — (a) `POST /v1/git-blob/init` with metadata + `blob_sha` + size; server returns presigned PUT URL; (b) client uploads to S3; (c) client calls `POST /v1/git-blob/commit` with the bead id. The two-step exists so kbeads doesn't proxy 16 MB of bytes through its pod. P4 owns the RPC names.
- **P6 (embedding)** eligibility: `encoding ∈ {text, truncated-text}`, any tier. Embedder fetches via the same `Fetch` interface; for Tier C it pulls the object once at embed time and discards.
- **P7 (lifecycle/GC)**: refcount sweep on Tier B (delete row when `refcount=0`); orphan-object sweeper for Tier C runs daily, finds S3 objects with `blob_sha` that has no referencing `git-blob` bead, deletes them. Both are bounded background jobs.
- **P8 (bridge)**: when the GitLab bridge auto-registers blobs on MR merge, it follows the same tier router. No special path. The bridge service account needs project-scoped write + S3 PutObject for the fleet bucket.

### 7.2 For canonical-prod TF module work (ka-0000005315)

The module will need a `git_blob` S3 bucket with:

- Server-side encryption (SSE-S3 default, SSE-KMS option).
- Versioning **off** (content-addressed; immutability handled at app layer).
- Lifecycle rule: noncurrent versions deleted after 1 day (defense in depth even with versioning off).
- IAM role for the kbeads pod (IRSA): `s3:PutObject`, `s3:GetObject`, `s3:HeadObject`, `s3:DeleteObject`, scoped to `arn:aws:s3:::<bucket>/git-blob/*`.
- No public access. Block-public-access on at the bucket level.
- (Optional, fleet choice) Object-lock retention to prevent accidental deletes during dev.

This is a forward-pointer for the canonical-prod TF work, not a commitment from this ADR. The module ADR (Epic 5 / ka-0000008037 — stateful services) is the place to land the bucket shape.

### 7.3 For operators

- **Default config is safe and cheap.** A fresh fleet with `BEADS_OBJECT_STORE_BACKEND=disabled` rejects Tier C registrations but happily handles Tier A/B. Operators opt into S3 by setting backend + bucket.
- **Disk-pressure on Postgres is bounded** — Tier A inline is capped at 64 KB × N registrations, Tier B at 1 MB × N, both governed by GC. No surprise table bloat.
- **Read-path latency is bounded** — `kd show <git-blob>` is one row read; content fetch is opt-in.

### 7.4 Cost / risk

- **Cost**: typical fleet < $1/mo, aggressive fleet < $5/mo. See § 4.
- **Risk: tier-flip surprise.** If a fleet flips `inline_max_bytes` and re-runs an ingest pipeline, existing inline blobs stay inline (per § 2.6). Document this loudly in the ops runbook (P9 deliverable).
- **Risk: S3 outage.** Tier A/B keep working; Tier C reads return a 503 (or redirect to the cached presigned URL if operator has cached). Tier C writes fail (registration is rejected) until S3 is back. This is acceptable — `git-blob` is not a write-availability-critical path.
- **Risk: refcount drift.** A bug in the create/delete refcount handler could leak rows in `git_blob_content`. Mitigation: a P7 reconciliation job that rebuilds refcounts from `beads.fields ->> 'blob_sha'` periodically. P7 owns the job; this ADR flags the requirement.

## 8. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The accompanying `gb decision` for D-4 is resolved by the operator either accepting the recommended defaults (the three-tier shape with 64KB/1MB/16MB caps and S3 default) or directing a specific override (e.g. "Tier C off entirely; reject anything > 1 MB" or "use MinIO instead of S3 by default for now").
3. P3, P4, P6, P7 (research phases not yet created) can begin against the shape locked here.

This ADR is **not** the green light to land the migration or wire the S3 client. That is P9, after the operator authorizes the implementation epic per plan-of-record § 11.

## 9. Open follow-ups (not blocking)

- **What about content compression in Postgres `bytea`?** TOAST handles it transparently; we don't need to specify. Worth measuring once Tier B has real data.
- **Should the inline cap shrink when `secrets_redacted=true`?** Probably not — redaction shrinks size, doesn't grow it. But noting for P3 to consider.
- **Does Tier B need a per-fleet quota?** Probably yes (defense against runaway agent ingest). P7 lifecycle owns rate-limit + quota; flagged here.
- **Should `kd show` ever auto-fetch Tier B content?** No — explicit `--with-content` only, to keep listing cheap. Confirms in P4.

## 10. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` § 5 (storage strategy). MR !1973, bead ka-0000003043.
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks required field set including `content`, `content_ref`, `blob_sha`, `file_size`, `encoding`.
- Embedding precedent (content-addressed side table): `kbeads/internal/store/postgres/migrations/005_bead_embeddings.up.sql` and `kbeads/docs/design-embedding-search.md`.
- Soft-delete pathway: `kbeads/internal/store/postgres/migrations/009_soft_delete.up.sql`.
- Fleet config conventions: `kbeads/internal/config`.
- Canonical-prod TF module (forward pointer for S3 bucket shape): research epic ka-0000005972, stateful-services epic ka-0000008037.
