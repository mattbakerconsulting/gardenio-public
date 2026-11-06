# ADR: `git-blob` Bead Type — Embedding & Search Policy

**Status**: Proposed (research only — implementation gated on operator approval of parent epic ka-0000003043)
**Date**: 2026-05-03
**Author**: gardenio-p6-gitblob-adr-v6s
**Phase**: P6 of research epic ka-0000003043 (git-blob bead type)
**Bead**: ka-0000005801
**Supersedes**: nothing
**Depends on**:
- Plan-of-record `kbeads/docs/research-git-blob-bead-type.md` (MR !1973), §10 D-8.
- P0 ADR `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — locks type name `git-blob`, kind `data`, the six required fields, write policy.
- P2 ADR `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323) — locks tier shape (A inline ≤64KB / B `git_blob_content` ≤1MB / C S3 ≤16MB) and the `Fetch` interface every reader (including the embedder) goes through.
- Companion design `kbeads/docs/design-embedding-search.md` — the bead-embedding pipeline that already ships for `description`/`notes`/`labels`; this ADR layers the `git-blob` policy on top.
**Resolves decisions**: **D-8** (embedding policy: index `git-blob.content` in pgvector by default, or opt-in per repo).
**Defers**: D-7 (typed-mention verbs, → P5). All other D-* are already settled in P0/P1/P2/P3.

## TL;DR

The bead-embedding pipeline (`design-embedding-search.md`) already wires pgvector + a pluggable `Embedder` against bead `description`/`notes`/`labels`. P6 closes the policy questions for `git-blob` content:

- **Default policy: opt-in per repo.** A `git-blob` is **not** embedded by default. Embedding turns on for a repo only when a fleet-scoped config bead lists it. This bounds embedding cost (and pgvector index size) to repos the operator deliberately wants searchable; it survives a worst-case "agent registers every binary in every commit" event without blowing up the embedding budget.
- **Eligibility predicate (whether or not the repo is opted in)**: a `git-blob` is embeddable **iff** `encoding ∈ {text, truncated-text}` **and** `secrets_redacted = false` **and** `file_size ≤ embed_max_bytes` (default 256 KB; soft cap, configurable per fleet) **and** the repo is opted-in. Binary content (Tier B/C base64), redacted content (P3), oversize content, and content from a non-opted-in repo are skipped — they go in `bead_embeddings_skipped` with a reason, not in `bead_embeddings`.
- **What gets embedded**: a `BlobText()` builder analogous to `BeadText()` — a small header (`Repo: ...\nPath: ...\nLanguage: ...\nCommit: <short-sha>\n`) followed by the content body, truncated to `embed_max_bytes` characters. Provenance fields (`commit_message`, `commit_author`, etc.) are *not* embedded — they're noise for code-similarity search and would dilute the vector. Title/labels stay out for the same reason.
- **Storage**: re-use the existing `bead_embeddings` table from migration 005. **One embedding row per bead**, keyed off `bead_id`. No new table, no new dimension. The `text_hash` column already handles "skip re-embed if unchanged" — for `git-blob` it will essentially never change after first embed (immutable identity by P0+P1).
- **Search ergonomics**: extend `model.BeadFilter` with a `repo` field (multi-valued) and a `path_prefix` field. The existing `SemanticSearch` SQL composes both as additional `WHERE` clauses; `kd search --semantic --repo=<r> --path-prefix=<p>` becomes the natural CLI surface. `kd similar <git-blob-id>` works unchanged. Type-mixing (search across `git-blob` *and* `issue` beads) is allowed by default — the existing `--type` filter scopes when wanted.
- **List-filter ergonomics** (orthogonal to semantic search): a `kd list --type=git-blob` invocation needs first-class field filters on `repo`, `commit_sha`, `path`, `path_prefix`, `language`, `file_size`, `committed_at`, `since`. P6 specifies the filter contract; P4 owns the CLI flag-naming.
- **Migration & cost**: zero new migrations. The configurable opt-in lives as fleet-scoped config beads (existing pattern). Worst-case storage at the documented opt-in posture is **<5 GB/year** of pgvector embeddings for a typical fleet — well inside the existing bead-embedding budget.

This ADR ships **zero code**. It is the spec the eventual P9 implementation will land as: (a) one new package-level helper `BlobText()` next to `embedding.BeadText()`, (b) one extension to `model.BeadFilter`, (c) two SQL WHERE-clause additions to `SemanticSearch`, (d) a `git-blob.embed_repos` config-bead schema, (e) a small refactor of the embed worker to consult eligibility before writing.

## 1. Context

### 1.1 What ships today and where the gap is

`design-embedding-search.md` lays out the pipeline kbeads already runs:

- pgvector table `bead_embeddings (bead_id PK, embedding vector(1024), model, text_hash, fleet, created_at, updated_at)` — migration 005.
- `Embedder` interface with `noop`, `ollama` backends shipped; cloud backends pluggable.
- `EmbeddingWorker` listens on the event bus, computes `BeadText()` + hash, upserts the embedding.
- `SemanticSearch(ctx, queryVec, filter, limit) ([]*SemanticResult, error)` on the Store interface; HTTP `POST /v1/search/semantic`; CLI `kd search --semantic`, `kd similar`, `kd embed sync`, `kd embed stats`.

The pipeline is type-agnostic — every bead kind (`issue`, `data`, `config`) goes through `BeadText()`, which embeds `Title / Type / Description / Notes / Labels`. For a typical issue-kind bead, those fields are the bead. For a `git-blob`, those fields are nearly empty (the title is autogenerated, there is no description/notes, only labels carry signal); the *interesting* text is in `fields.content`.

So today, if `git-blob` registration started without P6 changes, every blob would land in `bead_embeddings` with a tiny vector built from a near-empty `BeadText()` output. That is *worse than nothing*: the vector is misleading (it doesn't represent the file content) and it crowds the IVFFlat index with low-information rows. The pipeline has to be made content-aware *or* the type has to be opted out of embedding entirely.

P6's job is to settle which.

### 1.2 What P0/P1/P2 already lock

- **P0** (ka-0000006442): `kind: data`, required fields include `encoding`, `file_size`, `blob_sha`. Optional `content` and `content_ref`.
- **P1** (ka-0000002044): identity is `(repo, commit_sha, path)`. Bead ID is a normal nanoid; idempotent upsert. `repo` is normalized `<host>/<owner>/<…>/<name>`, deduped case-insensitively. `repo_id` is an opaque `<provider>:<numeric-id>` parallel field.
- **P2** (ka-0000004323): three storage tiers. Critical for P6 — `Fetch(ctx, bead) ([]byte, encoding, error)` is the **single read interface** the embedder uses. P6 does **not** care which tier the bytes come from; it only cares that `Fetch` returns them and that the embedder counts the latency/cost against its own budget.
- **P3** (ka-0000007332): every `git-blob` write runs gitleaks; `secrets_redacted: true` means the stored content has placeholders where secrets were. P6 leans on this signal to skip embedding redacted blobs (see § 3.3).

### 1.3 Why P6 has to land before P7 / P8 / P9

- **P7 (lifecycle/GC)** has to know whether deleting a `git-blob` bead also deletes its `bead_embeddings` row. The existing `ON DELETE CASCADE` on `bead_embeddings.bead_id` already handles hard delete. P6 confirms no new lifecycle surface is needed.
- **P8 (bridge auto-registration)** writes blobs at MR-merge time. If embedding is on by default, every merged MR populates pgvector — at scale, that's the largest write source. The opt-in default means the bridge can register safely without needing a per-fleet embed-budget conversation first.
- **P9 (implementation)** ships the `BlobText()` helper, the eligibility check, and the filter additions. P9 cannot start without the eligibility predicate fixed.

### 1.4 Why "default-on" would be wrong

The plan-of-record §10 D-8 explicitly proposes opt-in per repo as the default. The reasoning — "cost-bounded" — is short there; expanded here:

- **Embedding cost scales with content count and content length.** At 1024-dim float32, each embedding is 4 KB on disk; the IVFFlat index roughly doubles that. A naive embed-everything posture against the worst-plausible scenario in P2 §4.1 (1000 registrations/day, all eligible) projects to ~4 MB/day × 365 = 1.4 GB/year of vectors per fleet, plus index overhead. That's tractable, but the *embedding API spend* (Ollama is cheap, OpenAI/Voyage are not) and the *worker throughput* (~50 ms per embed at Ollama defaults) are not. A rogue agent cycle that registers 10k blobs takes the worker offline for ~8 minutes and costs ~$1 of cloud-embed credit. Not a disaster — but a recurring foot-gun.
- **Embedding quality degrades with low-signal content.** Generated YAML, lockfiles, vendored dependencies are big, low-information, and they pollute neighbor-search results: a query for "auth refactor" returns a `package-lock.json` because a unique substring shared a token. Opt-in per repo lets the operator pick the repos where embedding pays off (the agent-edited app repos) and skip the noise.
- **Embedding is reversible.** `kd embed sync --type=git-blob --repo=<r>` backfills any time. Starting opt-in and adding repos as they prove valuable is one direction of pressure; starting opt-in-everywhere and pruning low-signal repos is the other. The former composes with rate-limit and budget conversations; the latter requires deletion of existing rows and cache invalidation.

The opposite-direction case — "embed every blob, let the operator opt out" — is rejected on cost-of-mistake grounds. Mistakes that grow storage are recoverable; mistakes that fan out to a cloud API bill are visible only after the bill arrives.

## 2. Decision: opt-in-per-repo by default (resolves **D-8**)

### 2.1 Shape of the opt-in

Embedding for `git-blob` content turns on **per-repo per-fleet**. Encoded as a fleet-scoped config bead:

```jsonc
// type:config, slug: git-blob.embed_repos
{
  "embed_repos": [
    {"repo": "gitlab.com/example-org/gardenio", "since": "2026-05-03"},
    {"repo": "gitlab.com/example-org/monorepo"}
    // implicit: every other repo is OFF
  ],
  "embed_max_bytes": 262144,         // 256 KB; soft cap, see § 3.4
  "embed_skip_languages": ["lock", "min", "map"]  // optional list of languages auto-skipped even when repo is on
}
```

The bead lives at the same path as other fleet-config beads (existing `type:config` registration pattern in `internal/server/config.go`). One bead per fleet; default is empty (i.e. nothing opted in).

The set is checked **on every embed**, not cached client-side, because the worker is async and the operator may flip a repo on without restarting kbeads.

### 2.2 What "opted in" means

A `git-blob` bead is embedded when **all** of the following hold:

1. The bead's normalized `repo` (P1 §2.4) matches an entry in `embed_repos`. Case-insensitive match (mirrors the natural-key index).
2. `encoding ∈ {text, truncated-text}`. Binary blobs are skipped — embedding base64 is meaningless.
3. `secrets_redacted = false`. Redacted content has placeholders where the interesting tokens were; embedding it produces a vector that's correct for the redacted text, not for the actual file. Skip and let the operator know via `bead_embeddings_skipped`.
4. `file_size ≤ embed_max_bytes` (default 256 KB). Above the cap, the file is too long for a meaningful single-vector representation anyway (the model context windows we use saturate around 8K tokens ≈ 32 KB; 256 KB is a generous over-cap that defaults to "embed at will" for source files and "skip" for generated artifacts). See § 3.4.
5. The bead's `language` (auto-detected on ingest per P0) is **not** in `embed_skip_languages`. Default skip list is empty; an operator can add `lock`, `min`, `map` (i.e. lockfiles, minified JS, sourcemaps) without touching code.

If any predicate fails, the embed worker writes a row to `bead_embeddings_skipped` (see § 4.2) with the failing reason and **does not** write to `bead_embeddings`.

### 2.3 What changes for `issue`-kind and other existing beads

**Nothing.** The opt-in is *only* for `type=git-blob`. Every other type continues to be embedded by `BeadText()` exactly as today. This ADR does not touch the existing pipeline for issue/task/epic/memory/etc. beads.

The worker check is roughly:

```
on bead-changed event:
  if bead.type == 'git-blob':
    if not eligible(bead, embed_repos_config):
      record_skip(bead, reason)
      return
    text = BlobText(bead)        # see § 3.1
  else:
    text = BeadText(bead)        # unchanged
  hash = TextHash(text)
  if existing.text_hash == hash and existing.model == embedder.model:
    return  # no-op
  vec = embedder.Embed(text)
  upsert(bead.id, vec, embedder.model, hash)
```

### 2.4 Options considered

| Option | Default | Pros | Cons |
|---|---|---|---|
| **A** ★ Opt-in per repo via fleet-config bead | Off | Cost-bounded; easy to add repos as they prove valuable; survives worst-case ingest; skips low-signal repos by default. | Operator has to do something to get any blob search at all. Mitigated by adding common repos in the same MR that turns the type on for a fleet. |
| B | Opt-in **per blob** (CLI flag at registration) | Off | Most precise. | Friction at every registration; agent code has to remember the flag; dedup behavior across "first registration without flag, second with flag" needs another rule. |
| C | Opt-in **per fleet** (one switch) | Off | Simplest config. | Coarse — opting in fleet-wide gives you `package-lock.json` embeddings whether you wanted them or not. |
| D | Opt-out per repo (default-on) | On | Search "just works" without configuration. | The cost-recovery argument from § 1.4. |
| E | Smart-default by language (e.g. always embed `.go`/`.py`/`.md`, never embed `.lock`/`.svg`) | Smart-default-on | No config for the common case. | Hard-coding language policy in the engine layer is exactly the kind of "module embeds caller-specific policy" anti-pattern the canonical-prod work flagged; better to keep the policy in config (the `embed_skip_languages` field in Option A absorbs the use case). |

**Choice: A.** It matches the plan-of-record default, gives operators one config bead to flip on or off per repo, and absorbs the language-skip use case as an additional config field rather than an engine-level hard-code.

### 2.5 Counter-arguments considered

- **"Off-by-default means agents won't know if their MRs are searchable."** True; mitigated by `kd embed stats --type=git-blob` (already specified in `design-embedding-search.md` §7) reporting "N blobs eligible, M embedded, K skipped (reason breakdown)". An operator flipping a repo on can see the backfill complete. The `kd similar` and `kd search --semantic --type=git-blob` UX both surface "no results — no blobs embedded for `repo=...`; flip on with `kd config edit git-blob.embed_repos`" as a hint when the type-filtered semantic search returns zero rows but `kd list --type=git-blob -f repo=...` returns rows.
- **"Just always embed and rely on the IVFFlat index to be cheap."** The index *is* cheap to query, but cheap at 50k rows is not free at 5M rows; pre-filtering on `repo` (an indexed field) is what keeps us in the cheap regime. Eligibility predicates that run at index-write time (instead of at query time) are the right place to spend.
- **"Why not use a per-blob `embed=true` field?"** Considered (Option B). The friction at registration time is too high — every CLI invocation, every bridge code path, every test fixture has to remember it. Per-repo opt-in is one decision per repo per fleet; a repo with 10k blobs registers them all under the same policy.

## 3. Decision: what gets embedded for an eligible `git-blob`

### 3.1 `BlobText()` — the document the embedder sees

A new helper next to `embedding.BeadText()`:

```go
// BlobText builds the text document to embed for a git-blob bead.
// Only called when eligibility predicates pass (see ADR P6 § 2.2).
func BlobText(b *model.Bead, content []byte) string {
    var sb strings.Builder
    sb.WriteString("Repo: ")
    sb.WriteString(stringField(b, "repo"))
    sb.WriteString("\nPath: ")
    sb.WriteString(stringField(b, "path"))
    if lang := stringField(b, "language"); lang != "" {
        sb.WriteString("\nLanguage: ")
        sb.WriteString(lang)
    }
    if sha := stringField(b, "commit_sha"); len(sha) >= 7 {
        sb.WriteString("\nCommit: ")
        sb.WriteString(sha[:7])
    }
    sb.WriteString("\n\n")
    // Content body, truncated to embed_max_bytes characters (post-redaction
    // bytes if P3 redacted; eligibility already excluded redacted blobs).
    sb.Write(content)
    return sb.String()
}
```

### 3.2 What is *not* in the embedded text

Explicitly excluded from `BlobText`:

| Field | Why excluded |
|---|---|
| `commit_message` | Useful prose, but it pollutes code-similarity search ("fix typo" matches everything). If an operator wants to find blobs by commit message they should query the issue bead that produced them. |
| `commit_author`, `commit_committer` | Identity — would bias the vector toward author signal, not content. |
| `committed_at`, `branch` | Recency / context, not content. Use list filters (`--since`, etc.) for that. |
| `gitlab_project_id`, `gitlab_mr_iid` | Provider IDs — not content. |
| `secrets_summary` | The whole point of redaction is to remove these tokens; explicitly *re-introducing* them in the embedded text would defeat the redaction.  |
| Bead title | Auto-generated from `path`/`repo`; redundant with the header. |
| Bead description, notes, labels | None of these carry blob-content signal; including them would re-introduce the dilution problem from § 1.1. |

If a future query pattern wants to combine "code similarity *and* commit-message search", that's a hybrid-rank concern (see `design-embedding-search.md` §13) and should be handled by RRF over two separate vectors, not by stuffing both into one vector.

### 3.3 `secrets_redacted=true` skips embedding entirely

Three reasons:

1. **Redaction changes the vector.** A file with `AKIA...` redacted to `<REDACTED>` produces a vector for the redacted text, not the source. Search hits on it are misleading.
2. **Re-embedding after a re-redact is awkward.** P3's design has redaction as a write-time, one-shot operation. If the policy ever changes, the eligibility predicate flips skip → embed naturally.
3. **It defends against a leaky scanner.** If gitleaks misses a secret pattern, embedding the content sends the secret to whatever embedding API is wired (OpenAI, Voyage, etc.). Skipping redacted-positive blobs *and* the not-yet-scanned ones (i.e. embed only when `secrets_redacted=false` *and* `secrets_summary IS NULL`) keeps that blast radius zero.

### 3.4 `embed_max_bytes` cap and truncation

256 KB is the default; configurable per fleet. Above the cap the blob is skipped (not truncated-and-embedded). Reasons:

- Truncating to 256 KB and embedding the prefix is misleading — it produces a vector that represents the *header* of a 5 MB SQL dump, not the dump.
- The model context windows we use (Ollama nomic-embed-text: 8K tokens ≈ 32 KB; OpenAI text-embedding-3-small: 8K tokens) saturate well below 256 KB anyway. The cap is a defensive over-cap, not a target.
- For the rare case where an operator wants to search a large file, they can chunk it manually and register multiple `git-blob` beads (or, in a future ADR, a per-blob "chunk into N vectors" pipeline). Out of scope here.

If a fleet wants to tighten the cap to 32 KB to better match the model context window, they flip `embed_max_bytes`. If a fleet wants to crank it to 1 MB (and accept the truncation effect), same. The default of 256 KB is the "embed everything that fits in the model context comfortably" knob.

### 3.5 Re-embed semantics

The existing `text_hash` skip in the worker carries over: same hash, same model → no re-embed. For `git-blob`:

- The triple `(repo, commit_sha, path)` is immutable (P1). The blob's `content` does not change.
- `BlobText()` output therefore changes *only* when the embedder model changes, the eligibility config changes (e.g. operator added a `language` to skip list — the bead becomes ineligible, see § 4.3), or the `embed_max_bytes` cap was tightened past the bead's `file_size`.
- A model change triggers `kd embed sync --force --type=git-blob` (existing CLI), same as for any other type.

## 4. Decision: storage, skip-tracking, and search composition

### 4.1 Re-use `bead_embeddings`

Use the existing migration-005 table. One row per `git-blob` bead that passes eligibility; `bead_id` is the PK, `embedding` is the vector, `model` and `text_hash` carry provenance. The `fleet` column already partitions by fleet for the search WHERE clause.

**No new column** is needed on `bead_embeddings` for the source-text type (i.e. "this row was built from `BlobText()` not `BeadText()`"). The bead's `type` is already on the joined `beads` row, so `WHERE b.type = 'git-blob'` discriminates trivially. Re-embedding when `BeadText`/`BlobText` swap their definitions is handled by the model-version pump, not by a discriminator column.

### 4.2 `bead_embeddings_skipped` — track what didn't embed and why

A small companion table so `kd embed stats --type=git-blob` can report skip reasons without a full table-scan or guesswork:

```sql
-- migration NNN_bead_embeddings_skipped.up.sql  (P9, NOT this ADR)
CREATE TABLE bead_embeddings_skipped (
    bead_id    TEXT PRIMARY KEY REFERENCES beads(id) ON DELETE CASCADE,
    reason     TEXT NOT NULL,                  -- 'repo_not_opted_in' | 'binary' | 'redacted' | 'too_large' | 'language_skipped' | 'no_content'
    detail     JSONB,                          -- e.g. {"file_size": 4194304, "limit": 262144}
    fleet      TEXT NOT NULL DEFAULT 'default',
    skipped_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_bead_embeddings_skipped_reason ON bead_embeddings_skipped(reason);
CREATE INDEX idx_bead_embeddings_skipped_fleet  ON bead_embeddings_skipped(fleet);
```

The skipped table is **not** required for correctness — the embedder is free to be silent. But:

- It lets `kd embed stats` answer "why isn't blob X searchable?" without re-running the eligibility check.
- It supports a "preview" mode for opt-in changes: an operator considering adding `repo=foo` can run `kd embed preview --repo=foo` and see how many blobs would become eligible (currently in `bead_embeddings_skipped` with `reason='repo_not_opted_in'`).
- A row is upserted on every (re-)evaluation; on transition skip→embed, the row is deleted and a `bead_embeddings` row is inserted.

`ON DELETE CASCADE` mirrors `bead_embeddings`'s own behavior — when a `git-blob` is hard-deleted (P7), the skipped-tracking row goes with it.

This is the **only new table** in P6, and it is small (one row per skipped blob, no vector). At the worst-plausible-volume from P2 §4.1 (1000 blobs/day, 80% skipped because no repo opted in), it accumulates ~290 MB/year before any GC — still trivial.

### 4.3 Eligibility transitions

The repos config bead is the natural unit of change. When an operator edits it:

- **Repo added**: a follow-up `kd embed sync --type=git-blob --repo=<added>` (or an automatic background sweep, fleet-config flag) walks every `git-blob` with that `repo`, checks eligibility, and either embeds (and removes the skipped row) or updates the skipped row's reason.
- **Repo removed**: walk every `git-blob` with that `repo`, delete its `bead_embeddings` row, insert a `bead_embeddings_skipped` row with `reason='repo_not_opted_in'`. Do **not** delete the `git-blob` itself.
- **`embed_max_bytes` tightened**: walk affected blobs (those whose `file_size` is between the new cap and the old), delete their embeddings, insert skipped rows with `reason='too_large'`.
- **`embed_skip_languages` change**: same pattern, scoped by `language` field.

P9 implements the sweep; P6 specifies the contract. The sweep is bounded background work — at typical fleet size it completes in seconds.

### 4.4 Search composition: `BeadFilter` extension

`model.BeadFilter` today carries `Status`, `Type`, `Kind`, `Labels`, `Assignee`, `Fleet`. P6 adds two fields whose meaning is well-defined for `git-blob` and harmless for other types (they reduce to no-ops if the type doesn't expose `repo` / `path`):

```go
type BeadFilter struct {
    // ... existing fields ...

    // Repo filters by the normalized `repo` field on git-blob beads.
    // Multi-valued; matches case-insensitively.
    // No-op for types without a `repo` field.
    Repo []string

    // PathPrefix filters by the `path` field on git-blob beads, matching
    // any path that has the given prefix (POSIX, case-sensitive).
    // No-op for types without a `path` field.
    PathPrefix string
}
```

The `SemanticSearch` SQL (`design-embedding-search.md` §6) gains two WHERE clauses:

```sql
-- when filter.Repo is non-empty
AND LOWER(b.fields->>'repo') = ANY($N::text[])
-- when filter.PathPrefix is set
AND (b.fields->>'path') LIKE $M || '%'
```

Both use the natural-key index from P1 §4.1 implicitly (the `LOWER(repo)` expression is already indexed). The path prefix uses the existing JSONB GIN index pattern (no new index for the prefix-LIKE, since path-prefix queries are typically further constrained by `repo` and the post-pgvector candidate set is already ≤ `limit * 3`).

### 4.5 Search composition: filter ergonomics

CLI flags (P4 owns the final names; P6 fixes the contract):

```bash
# Semantic search scoped to a repo
kd search --semantic "auth refactor" --type=git-blob --repo=gitlab.com/example-org/gardenio

# Path-prefix narrowing
kd search --semantic "request handling" --type=git-blob \
    --repo=gitlab.com/example-org/gardenio \
    --path-prefix=internal/server/

# Cross-type: find both the issue and the blobs that look semantically close
kd search --semantic "session handling" --type=git-blob,task

# Find similar (existing UX) — works for git-blob via existing kd similar
kd similar ka-blob123          # finds any bead, including other blobs
kd similar ka-blob123 --type=git-blob   # restrict to other blob beads
```

For *non-semantic* list filters (separate command, separate code path):

```bash
# Structured list — no embedding required, uses JSONB indexed lookups
kd list --type=git-blob -f repo=gitlab.com/example-org/gardenio -f commit_sha=deadbeef...
kd list --type=git-blob -f path-prefix=internal/server/ --since=7d
kd list --type=git-blob -f language=go --status=open
```

The two `-f`-style filters that need first-class support (i.e. an indexed lookup, not a JSONB scan):

- `repo=` (case-insensitive equals — uses the existing `LOWER(repo)` natural-key index).
- `commit_sha=` (uses the natural-key index).
- `path-prefix=` (LIKE, uses btree on `path` if added; otherwise post-filter — performance acceptable up to 100k rows per fleet).
- `language=`, `since=` — already supported by existing list filter machinery.

`file_size` range filters (`-f file-size-gt=100000`) are nice-to-have; defer to P4.

### 4.6 `kd similar` for `git-blob`

The existing `kd similar <id>` command works for `git-blob` beads with no change to behavior. Two notes:

- When `<id>` is a `git-blob`, `similar` will only find other beads whose embedding exists — i.e. opted-in `git-blob`s and ordinary issue/task beads. Skipped blobs do not appear as candidates (correct).
- `kd similar <git-blob-id> --same-repo` is a useful future flag (find similar blobs *only* in the same repo). Defer to P4 as a flag-naming concern, but the underlying composition is just `--repo=<that-repo's-value>` automatically applied.

### 4.7 Listing-vs-search separation

Important: `kd list --type=git-blob -f path=...` and `kd search --semantic --type=git-blob "..."` answer different questions and use different indexes. The first uses the JSONB / partial-unique index from P1 (it returns the *exact* triple match or an empty result); the second uses pgvector and is approximate. Operators should reach for the first when they know the triple, and the second when they want similarity.

We should not blur the two: `kd search --semantic "exact/path/to/file.go"` is a misuse — embedding-based search is bad at exact-path lookup. The CLI help for `kd search --semantic` should mention this. Documentation deliverable, not code.

## 5. Migration sketch (for P9 reference, not this ADR's deliverable)

P6 lands as P9 implementation work in three files plus one optional migration:

1. **`kbeads/internal/embedding/blobtext.go`** — new file, `BlobText()` per § 3.1, plus `EligibleForEmbedding(bead, content, config) (bool, reason string)`.
2. **`kbeads/internal/embedding/worker.go`** — extend the existing worker to branch on `bead.type == 'git-blob'`, consult eligibility, fetch content via P2's `Fetch` interface only when eligible.
3. **`kbeads/internal/store/store.go`** + queries — extend `BeadFilter` per § 4.4, add the WHERE-clause additions to `SemanticSearch` queries.
4. **`kbeads/internal/store/postgres/migrations/NNN_bead_embeddings_skipped.up.sql`** — the optional skipped-tracking table per § 4.2. P9 may defer to a later sub-task if `kd embed stats --type=git-blob` is not initial-MVP.
5. **`kbeads/internal/server/config.go`** — register a fleet-config schema for `git-blob.embed_repos` so `kd config edit git-blob.embed_repos` validates.

The Helm chart picks up the new config bead by including it in the bootstrap manifests for fleets that want defaults. No new env var on the kbeads server (the existing `BEADS_EMBED_*` chain covers backend/model selection); no new pod-level dependency.

### 5.1 Backfill story

For an existing fleet that adopts the type retroactively (e.g. canonical-prod imports historical blob beads from another fleet — not a P0–P8 deliverable, just hypothetical):

```bash
# After flipping repos on in the config bead:
kd embed sync --type=git-blob --repo=gitlab.com/example-org/gardenio
# Streams progress; each blob is fetched via P2 Fetch, eligibility-checked,
# embedded if eligible, skipped-tracked if not.
```

`--force` re-embeds even when text_hash matches (needed after model swap). `--dry-run` is a useful flag (P4 to confirm) — list what would be embedded vs skipped, without writing.

### 5.2 What no-ops at the DB layer

- `bead_embeddings` schema unchanged — same `vector(1024)` (or whatever the active embedder uses), same indexes.
- IVFFlat tuning unchanged. The `lists=100` default in migration 005 is fine through the projected blob volume; HNSW upgrade decision (`design-embedding-search.md` §15) is a separate trigger and not P6's concern.
- No new pgvector extension features required.

## 6. Cost model

Reusing the volume assumptions from P2 §4.1, layered with the opt-in posture.

### 6.1 Eligible-blob count

Assume the operator opts in **2 repos** (typical: `gardenio` + `monorepo`) and 60% of registrations target those repos. Of those, ~80% are text/non-redacted/under-cap.

| Scenario | Registrations/day | Eligible/day (after repo opt-in × text-eligible filter) | Eligible/year |
|---|---|---|---|
| Typical | 100 | 100 × 0.6 × 0.8 ≈ 48 | ~17 500 |
| Aggressive | 1000 | 480 | ~175 000 |
| Worst plausible | 1000 | 480 | ~175 000 |

(The "worst plausible" stays at the aggressive number — the cap is the eligibility filter, not the registration rate, because non-eligible blobs do not embed.)

### 6.2 pgvector storage

At 1024-dim float32 = 4 KB per row, plus ~4 KB IVFFlat overhead per row = ~8 KB amortized.

| Scenario | Embeddings/year | Storage/year |
|---|---|---|
| Typical | 17 500 | ~140 MB |
| Aggressive | 175 000 | ~1.4 GB |

Both are inside the existing `bead_embeddings` budget (the table already grows by issue/task/memory beads). No new storage tier needed.

### 6.3 Embedding-API cost

Ollama (local nomic-embed-text): zero $-cost; only worker CPU/memory and ~50 ms/embed throughput. At 480 eligible/day in the aggressive case, ~24 seconds of worker time per day. Trivial.

OpenAI text-embedding-3-small: $0.020 / 1M tokens. A 32 KB text body ≈ 8K tokens. Aggressive case: 480 × 8K = 3.84M tokens/day = ~$0.077/day = **~$2.30/month**. Tractable; smaller than the S3 cost from P2 §4.3.

Voyage voyage-3-lite: ~$0.020 / 1M tokens. Same arithmetic.

If a fleet opts in a third or fourth repo, multiply linearly. The per-fleet `kd embed stats --type=git-blob` output should report token-spend estimates so the operator notices before the bill does.

### 6.4 Index-write throughput

IVFFlat is cheap on insert. At 480/day the worker is not stressed. At a hypothetical 100 000/day (well past worst-plausible), the worker becomes the bottleneck, not the index — at which point the `BEADS_EMBED_ASYNC=true` event-bus path is essential and HNSW becomes worth considering. Out of scope for P6.

## 7. Consequences

### 7.1 For other research phases

- **P5 (graph integration)**: typed-mention edges (`produces`, `derives-from`) are unaffected. They are graph edges, not vectors.
- **P7 (lifecycle/GC)**: the `ON DELETE CASCADE` on `bead_embeddings.bead_id` (and on the new `bead_embeddings_skipped`) means hard-delete of a `git-blob` cleans up both rows automatically. Soft-delete (90-day path) does **not** delete the embedding — the bead is still findable in semantic search, which is correct (recently soft-deleted work should remain searchable). When the soft-deleted row is hard-deleted, both vector and skip-track go.
- **P8 (bridge)**: when the bridge auto-registers blobs on MR merge, opt-in semantics apply naturally — only blobs in opted-in repos pay the embed cost. The bridge does not need a per-bead opt-in flag.
- **P9 (implementation)**: see § 5 file map. P9 also needs to write the `kd embed stats --type=git-blob` output to surface eligibility breakdown, plus the `--type=git-blob --repo=` filter on `kd embed sync` and `kd search --semantic`.

### 7.2 For canonical-prod TF module work (ka-0000005315)

Nothing module-level. The pgvector extension and `bead_embeddings` table already exist; this ADR only changes worker-side behavior and adds one optional table. The canonical-prod Helm values for kbeads pick up the same defaults.

### 7.3 For operators

- **Default posture is safe and cheap.** A fresh fleet has zero blob embeddings. Embedding turns on by editing the `git-blob.embed_repos` config bead.
- **Eligibility is observable.** `kd embed stats --type=git-blob` and `kd embed preview --repo=<r>` answer "what's searchable?" and "what would become searchable if I opted this repo in?" without surprise.
- **Search "just works" once a repo is on.** `kd search --semantic --type=git-blob --repo=<r> "query"` returns ranked results within the existing latency budget (<500 ms for typical fleet sizes per `design-embedding-search.md`).
- **Cost ceilings are local.** A rogue agent that registers 10 000 blobs into a non-opted-in repo costs zero embedding $ (no embeds happen). Into an opted-in repo, it costs at most ~$0.20 of OpenAI embed credit (10 000 × 8K tokens × $0.020/M) before P7's rate limit and orphan-GC kick in. Either way, recoverable.

### 7.4 Cost / risk

- **Cost**: reusing the existing `bead_embeddings` table; ~140 MB/year of vectors per fleet at typical opt-in. ~$2.30/month max API cost in the aggressive case (Ollama is free). See § 6.
- **Risk: redacted-leak via embedding API.** Eligibility predicate skips `secrets_redacted=true` (and skips when `secrets_summary` exists) so we never send post-redaction content to the API. The remaining residual is "scanner missed a secret" — same residual as P3 (the scanner is the same gate). No new exposure.
- **Risk: operator opts in too many repos at once.** The backfill sweep is bounded — at typical fleet size, opting a 5k-blob repo on completes in under 5 minutes. An operator who opts in 10 large repos at once might trip the embed-API rate limit; mitigation is `kd embed sync --type=git-blob --repo=<r>` per repo. Operationally annoying, not dangerous.
- **Risk: stale skip rows.** If an operator removes a repo from the opt-in list, the sweep deletes the embeddings and writes skipped rows. If the sweep is interrupted halfway, the skip-track table and the embeddings table can disagree. Mitigation: a P9 reconciliation job (analogous to the P2 §3.3 refcount reconciliation flagged for P7) — both sides cheap to recompute from the source of truth (the `git-blob` bead + the config bead).
- **Risk: opt-in config bead becomes a footgun.** A misconfigured `git-blob.embed_repos` (e.g. typo'd repo name) silently embeds nothing for that "repo". Mitigation: server-side validation that each `repo` in the config bead matches at least one existing `git-blob` bead's `repo` field — warn (not reject — repos can be opted in *before* any blob is registered). `kd embed stats --type=git-blob` shows a `repo_not_opted_in` count per `repo` value across all blobs, so an operator notices quickly.

## 8. What this ADR does NOT decide

| Decision | Owning phase | Question |
|---|---|---|
| D-7 | P5 | Typed-mention verb extensions (`produces`, `derives-from`). |
| n/a | P4 | Final CLI flag names, exit codes, dry-run UX, RPC shape for the search-with-repo-filter and embed-sync commands. |
| n/a | P7 | Reconciliation job for stale embeddings/skip-tracks. Rate-limit numbers. Quota per fleet. |
| n/a | P8 | Bridge auto-registration's interaction with embed worker pacing (bridge can ingest in bursts). |
| n/a | future | Per-blob "chunk into N vectors" pipeline for large files (out of scope; § 3.4 declines it). |
| n/a | future | Cross-type hybrid ranking (RRF over bead-text and blob-text vectors) — flagged in `design-embedding-search.md` §13 as P3 of the embedding roadmap; orthogonal to this ADR. |
| n/a | future | Embedding `commit_message` separately to enable "find blobs by commit-message similarity". Hybrid concern; not P6. |

## 9. Acceptance criteria

This ADR is accepted when:

1. The ADR doc lands on `main` via MR.
2. The `gb decision` for D-8 is resolved by the operator either accepting the recommended opt-in-per-repo default or directing a specific override (e.g. "default-on with per-repo opt-out", or "do not embed `git-blob` at all in this fleet — pgvector budget is reserved for issue beads").
3. P7 (lifecycle/GC), P8 (bridge), and P9 (implementation) can begin against the eligibility predicate and filter contract locked here.

This ADR is **not** the green light to land the `BlobText()` helper, the eligibility config bead schema, or the `bead_embeddings_skipped` migration. That is P9, after the operator authorizes the implementation epic per plan-of-record §11.

## 10. Open follow-ups (not blocking)

- **`kd embed preview --repo=<r>` flag.** Useful operator UX; P4 may want to spec it. Implementation is trivial given the eligibility predicate is already factored.
- **Per-fleet token-spend ceiling.** A `BEADS_EMBED_BUDGET_TOKENS_PER_DAY` env var that hard-stops the worker when exceeded. P7 territory.
- **HNSW upgrade trigger.** When eligible-blob count (across all types) exceeds ~50k per fleet *and* search latency exceeds 100 ms, swap IVFFlat for HNSW. Operational, not P6.
- **`embed_priority_paths`.** Some fleets may want to *always* embed `README.md` / `CLAUDE.md` regardless of repo opt-in. Flagged as a future config-bead extension, not P6.
- **Cross-fleet embed sharing.** If two fleets opt in the same repo, they re-embed independently. Sharing embeddings across fleets is a federation question (orthogonal); not P6.

## 11. References

- Plan-of-record: `kbeads/docs/research-git-blob-bead-type.md` (MR !1973, bead ka-0000003043). §10 D-8 (this decision).
- P0 ADR: `kbeads/docs/adr-git-blob-naming-schema.md` (ka-0000006442) — required field set including `encoding`, `secrets_redacted`, `language`, `file_size`.
- P1 ADR: `kbeads/docs/adr/git-blob-P1-identity-dedup.md` (ka-0000002044) — normalized `repo` form and natural-key index used by the `repo`-filter index pattern.
- P2 ADR: `kbeads/docs/adr/git-blob-P2-storage-tiering.md` (ka-0000004323) — the `Fetch` interface the embedder reads through.
- P3 ADR: `kbeads/docs/adr/git-blob-P3-security-redaction.md` (ka-0000007332) — `secrets_redacted` semantics that gate eligibility.
- Companion design: `kbeads/docs/design-embedding-search.md` — the existing embedding pipeline this ADR layers on top of.
- Migration precedent (content-addressed side table, CASCADE on hard delete): `kbeads/internal/store/postgres/migrations/005_bead_embeddings.up.sql`.
- Type config / fleet config bead pattern: `kbeads/internal/server/config.go`, `kbeads/internal/model/type_config.go`.
- Embedding worker: described in `design-embedding-search.md` §4 (`BeadText()`, `TextHash()`, the NATS-fed worker loop).
