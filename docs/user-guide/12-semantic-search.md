# Semantic Search

Semantic search lets you find beads by *meaning* instead of lexical overlap.
A query like `"auth issue"` matches a bead titled `"login session token
expiry bug"` because their embeddings point at similar regions of vector
space — no shared keywords required.

Semantic search composes with the usual structured filters (status, type,
labels, assignee, fleet), so you can scope a conceptual query to, say, open
bugs on a specific project.

> **Design reference**: `kbeads/docs/design-embedding-search.md`. That doc
> covers storage, model choice, indexing, and phasing. This page is the
> operator- and agent-facing user guide.

## Feature Overview

| Capability | CLI | HTTP |
|---|---|---|
| Semantic search | `kd search --semantic "<query>"` | `POST /v1/search/semantic` |
| Similar beads | `kd similar <bead-id>` | `POST /v1/search/semantic` (vector of source bead) |
| Backfill embeddings | `kd embed sync` | — |
| Embedding coverage | `kd embed stats` | — |

Embeddings are stored in a separate `bead_embeddings` table keyed by
`bead_id`, with a SHA-256 hash of the source text so we can skip re-embedding
unchanged beads. The feature is opt-in — with `BEADS_EMBED_BACKEND=noop`
(the default) nothing runs.

## `kd search --semantic`

Semantic mode is a flag on the existing `kd search` command. Query text is
embedded client-side against the server's configured model, then compared
against stored bead embeddings using cosine similarity.

```bash
# Find beads conceptually related to "auth bugs in the login flow"
kd search --semantic "auth bugs in the login flow"

# Combine with structured filters
kd search --semantic "database migration advice" --type=advice --status=open
kd search --semantic "flaky e2e tests" -l project:gardenio --status=open

# Control result count and minimum score
kd search --semantic "deploy rollout problem" --limit 10 --min-score 0.4

# JSON output (includes score per result)
kd search --semantic "session storage" --json
```

### Score Interpretation

Each result carries a **cosine-similarity score** in `[0.0, 1.0]` (higher =
more similar). Rough rubric:

| Score | What it usually means |
|---|---|
| **≥ 0.70** | Strong match — same concept, probably the bead you wanted |
| **0.50 – 0.70** | Related — same neighborhood, worth skimming |
| **0.30 – 0.50** | Loose association — useful for discovery, noisy for precision |
| **< 0.30** | Probably unrelated — pass `--min-score 0.3` to trim |

These thresholds depend on the embedding model (nomic-embed-text vs
text-embedding-3-small vs voyage-3-lite produce slightly different score
distributions). Calibrate against a few known pairs on your fleet before
baking `--min-score` into scripts.

### Filtering vs Ranking

Structured filters (`--status`, `--type`, `-l`, `--assignee`) are applied
before the nearest-neighbor scan, so filtered results aren't just
truncated — the ANN index visits rows that match your filters first. This
also means a narrow filter can surface lower-scored matches that a broad
query would have buried.

## `kd similar`

Given a bead, find others like it — useful for dedup checks, finding prior
art, or exploring a concept cluster.

```bash
kd similar ka-ABC123              # Top 10 by default
kd similar ka-ABC123 --limit 20
kd similar ka-ABC123 --json
```

Behavior:

- The source bead's stored embedding is reused if present; otherwise the
  server computes it on the fly from the current text.
- The source bead is excluded from results.
- Results honor fleet scoping — you won't see beads from other fleets.

**Typical uses:**

- Before filing a bug: `kd similar <draft-id>` to catch duplicates.
- Debugging a regression: find older beads with similar symptoms.
- Auditing advice: `kd similar <advice-bead>` to find related guidance.

## `kd embed sync` — Backfill Embeddings

Embeddings are generated asynchronously when beads are created or updated.
Existing beads (written before embedding was enabled, or after switching
models) need a one-time backfill.

```bash
# Embed all beads that are missing an embedding, or whose text_hash
# drifted since the last embed.
kd embed sync

# Override batch size (default 50)
kd embed sync --batch-size 100

# Re-embed everything, ignoring text_hash. Required after changing
# BEADS_EMBED_MODEL (different model = different vector space).
kd embed sync --force

# Scope to a specific fleet
kd embed sync --fleet gardenio
```

**When to run:**

| Situation | Command |
|---|---|
| First time enabling embeddings | `kd embed sync` |
| After bulk import of beads | `kd embed sync` |
| After switching `BEADS_EMBED_MODEL` | `kd embed sync --force` |
| After switching `BEADS_EMBED_BACKEND` (different dimensionality) | Run migration + `kd embed sync --force` |
| Routine maintenance | Not needed — async worker handles live writes |

**Progress output:**

```
Embedding sync starting (fleet=default, backend=ollama, model=nomic-embed-text)
  Total beads:      4821
  Already embedded: 4210
  Stale (re-embed): 37
  Missing:          574
[batch 1/13] 50 beads embedded in 1.2s
[batch 2/13] 50 beads embedded in 1.1s
...
Done. Embedded 611 beads in 14.8s. Skipped 4210.
```

### `kd embed stats`

Non-destructive coverage check:

```bash
kd embed stats
```

```text
Embedding coverage
  Model:         nomic-embed-text
  Total beads:   4821
  Embedded:      4210  (87.3%)
  Stale:         37    (text changed since last embed)
  Missing:       574
```

Run this before `kd embed sync` to preview the work — and afterward to
confirm `Missing = 0`.

## Beads-Viewer Search Page

The beads-viewer web UI has a `/search` page that talks to
`POST /v1/search/semantic`. It exposes the same knobs as the CLI:

- **Query box** — free-text; submitting runs a semantic search.
- **Filter pills** — status, type, kind, labels, assignee, fleet.
- **Score threshold slider** — maps to `min_score`.
- **Result list** — each row shows bead ID, title, type, status, score, and
  a snippet of the description. Clicking a row opens the bead detail page.

On the bead detail page, a **Similar beads** panel shows the top-N results
from `POST /v1/search/semantic` using the current bead's embedding — the
same data as `kd similar` in list form, next to the bead you're reading.

> **Screenshot**: `docs/user-guide/screenshots/semantic-search.png` will be
> added once the viewer route ships in production. Until then, verify the
> page live against gardenio-rwx.

## Deployment

Semantic search is **opt-in**. Existing deployments keep working at
`BEADS_EMBED_BACKEND=noop` (the default); nothing is written to the
`bead_embeddings` table and the CLI flag returns an error telling operators
to enable a backend.

### Prerequisites

1. **Postgres with pgvector** — the 005 migration runs
   `CREATE EXTENSION IF NOT EXISTS vector`. The Postgres image must ship
   pgvector; `ankane/pgvector` and `pgvector/pgvector` work out of the box.
   The bundled gardenio Helm chart uses a pgvector-enabled image by default.
2. **An embedding backend** — either the Helm-bundled Ollama sidecar, or
   credentials for a cloud provider (OpenAI or Voyage).

### Environment Variables

Set on the **kbeads daemon** pod, not the CLI:

| Variable | Default | Purpose |
|---|---|---|
| `BEADS_EMBED_BACKEND` | `noop` | `noop`, `ollama`, `openai`, or `voyage`. `noop` disables embedding. |
| `BEADS_EMBED_URL` | `http://localhost:11434` | Base URL of the embedding HTTP API (Ollama) |
| `BEADS_EMBED_API_KEY` | *(empty)* | API key for `openai` / `voyage` backends |
| `BEADS_EMBED_MODEL` | *(backend default)* | Override the model name |
| `BEADS_EMBED_ASYNC` | `true` | Embed off the write path via NATS. `false` = sync on write (slower, but no async worker required). |

**Model defaults per backend:**

| Backend | Default model | Dimensions |
|---|---|---|
| `ollama` | `nomic-embed-text` | 1024 |
| `openai` | `text-embedding-3-small` | 1536 |
| `voyage` | `voyage-3-lite` | 1024 |

Changing the model changes the vector dimensionality for OpenAI vs the
other two — this requires an `ALTER TABLE` on `bead_embeddings.embedding`
and a full re-embed. Stick to one model per deployment unless you plan a
migration window.

### Helm (Ollama Sidecar)

`gardenio/helm/gardenio/values.yaml`:

```yaml
embedding:
  enabled: true          # default false — must be opted in
  backend: ollama
  ollama:
    image: ollama/ollama:latest
    model: nomic-embed-text
    resources:
      requests:
        cpu: 500m
        memory: 2Gi
      limits:
        cpu: "2"
        memory: 4Gi
```

When `embedding.enabled=true`:

- An Ollama sidecar is added to the kbeads deployment.
- The `BEADS_EMBED_*` env vars are set on the kbeads container.
- The pgvector migration is applied on next startup.

For cloud backends, disable the sidecar and set the backend + API key via a
secret:

```yaml
embedding:
  enabled: true
  backend: openai        # or voyage
  apiKeySecret:
    name: beads-embed
    key: api-key
```

### First-Time Rollout

```bash
# 1. Deploy with embedding.enabled=true (chart applies migration 005)
helm upgrade gardenio ... --set embedding.enabled=true

# 2. Wait for the kbeads pod to become Ready (Ollama pulls the model on first start)
kubectl rollout status deploy/gardenio-beads -n <ns>

# 3. Confirm the backend is live
kubectl exec deploy/gardenio-beads -n <ns> -- kd embed stats

# 4. Backfill existing beads
kubectl exec deploy/gardenio-beads -n <ns> -- kd embed sync

# 5. Smoke-test
kubectl exec deploy/gardenio-beads -n <ns> -- kd search --semantic "flaky tests" --limit 3
```

## Troubleshooting

### `kd search --semantic` returns `embedding disabled`

The server is running with `BEADS_EMBED_BACKEND=noop` (or unset). Set a real
backend on the kbeads pod and restart. The CLI flag is a no-op against a
`noop` server by design — it fails loud rather than silently falling back
to ILIKE, because a silent fallback would hide stale results with the same
flag.

### Empty results despite obvious matches

Check embedding coverage:

```bash
kd embed stats
```

If `Missing > 0`, the query bead or target beads were created before the
backfill — run `kd embed sync`.

If `Stale > 0`, the source text drifted since the last embed. `kd embed
sync` re-embeds stale rows automatically (hash check); `--force` is only
needed after a **model** change.

### Scores look wrong after a model change

Different models produce different vector spaces. A database that has
embeddings from two different models (because the env var was flipped
mid-deploy) will return arithmetic nonsense. Always follow a model change
with:

```bash
kd embed sync --force
```

`bead_embeddings.model` tracks the model used for each row — run
`SELECT model, COUNT(*) FROM bead_embeddings GROUP BY model` against
Postgres to spot mixed state.

### Dimension mismatch on `kd search --semantic`

If the query embedder returns a different vector length than the stored
embeddings (e.g., switched from `nomic-embed-text` at 1024d to
`text-embedding-3-small` at 1536d), pgvector rejects the query. Fix:

```sql
ALTER TABLE bead_embeddings
  ALTER COLUMN embedding SET DATA TYPE vector(<new-dim>);
```

Then `kd embed sync --force`. In practice this is rare — it only comes up
when changing backends between OpenAI (1536d) and the other providers
(1024d).

### Ollama sidecar OOMs or is slow

`nomic-embed-text` wants ~2 GiB resident; the default 2Gi request /
4Gi limit is a floor, not a ceiling. Bump `embedding.ollama.resources.limits.memory`
if you see OOMKills. For fleets > 10k beads, a shared Ollama deployment
(one replica, many kbeads pods pointing at it via `BEADS_EMBED_URL`) is
more memory-efficient than per-pod sidecars.

### The async worker is falling behind

Check kbeads logs for `embedding worker` entries. If the backlog grows,
either throttle write volume, scale the embedding backend, or flip
`BEADS_EMBED_ASYNC=false` to embed inline (which backpressures writes
instead of queueing). Async is the default because it keeps the bead write
path fast; inline is a safety valve.

### Cross-fleet leakage concerns

The SQL query filters on `fleet` in the WHERE clause, so results never
cross fleet boundaries. For the IVFFlat index this is post-filtering — the
index scans globally then discards non-matching rows. If one fleet is much
smaller than another, that smaller fleet can see reduced recall. At fleet
sizes > ~50k this warrants HNSW + pre-filtering (Postgres 16+); below that
threshold, the behavior is fine.

## See Also

- **Design doc**: `kbeads/docs/design-embedding-search.md` — storage,
  model choice, phasing, alternatives considered.
- **`kd` CLI reference**: [04-kd-cli-reference.md](04-kd-cli-reference.md) —
  general search, filters, listing.
- **Configuration**: [08-configuration.md](08-configuration.md) — Helm
  values, secrets, and deployment flow.
