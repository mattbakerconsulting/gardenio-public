# Design: Semantic Search for Beads via Embeddings

**Status**: Sketch  
**Author**: monorepo-we-need-a-q04  
**Date**: 2026-04-15  
**Bead**: ka-0000006386  

## Problem

The current `kd search` uses `ILIKE '%query%'` on title and description. This is
fast and simple but fails when:

- The user's query uses different words than the bead text ("auth issue" won't
  match a bead titled "login session token expiry bug")
- The user wants to find _related_ beads conceptually ("what else is like this?")
- Advice beads should surface for tasks they're relevant to, even when no labels
  or keywords overlap

Embedding-based (semantic) search solves this by comparing _meaning_ rather than
lexical overlap.

## Constraints

- kbeads already runs Postgres — avoid adding a separate vector database
- Must work with the existing Store interface pattern
- Must compose with existing structured filters (status, type, labels, etc.)
- Embedding model must be pluggable (cloud API vs. local inference)
- Latency budget: search should complete in <500ms for typical fleet sizes (<50k beads)

## Design

### 1. Storage: pgvector

Use the [pgvector](https://github.com/pgvector/pgvector) extension for Postgres.
It adds a `vector` column type and efficient approximate nearest neighbor (ANN)
indexes. This keeps everything in one database — no new infrastructure.

**New table** (not a column on `beads` — keeps the hot path unaffected and lets us
rebuild embeddings independently):

```sql
-- migration 005_bead_embeddings.up.sql
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE bead_embeddings (
    bead_id    TEXT PRIMARY KEY REFERENCES beads(id) ON DELETE CASCADE,
    embedding  vector(1024),          -- dimension depends on model; 1024 for nomic-embed-text
    model      TEXT NOT NULL,          -- e.g. "nomic-embed-text-v1.5", "text-embedding-3-small"
    text_hash  TEXT NOT NULL,          -- SHA-256 of the source text; skip re-embed if unchanged
    fleet      TEXT NOT NULL DEFAULT 'default',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- IVFFlat index — good enough for <100k rows, easy to tune.
-- Switch to HNSW if we exceed ~500k beads per fleet.
CREATE INDEX idx_bead_embeddings_ivfflat
    ON bead_embeddings USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);

CREATE INDEX idx_bead_embeddings_fleet ON bead_embeddings(fleet);
```

**Down migration:**

```sql
-- migration 005_bead_embeddings.down.sql
DROP TABLE IF EXISTS bead_embeddings;
-- Note: don't DROP EXTENSION vector — other tables might use it.
```

**Why a separate table?**
- Embeddings are large (~4KB per row at 1024 dims); keeping them out of `beads`
  avoids bloating every `SELECT *` query that doesn't need them.
- Embeddings can be rebuilt from scratch without touching bead data.
- Different models produce different dimensions — the table is self-describing.

### 2. What Gets Embedded

Concatenate the bead's text fields into a single document:

```
Title: {title}
Type: {type}
Description: {description}
Notes: {notes}
Labels: {comma-separated labels}
```

Comments are excluded from the initial embedding because:
- They change frequently (would require constant re-embedding)
- They're often conversational noise ("ack", "LGTM")
- They can be added in a future phase as a separate embedding per comment

The concatenated text is hashed (SHA-256) and stored as `text_hash`. On update,
if the hash hasn't changed, we skip re-embedding.

### 3. Embedding Model: Pluggable `Embedder` Interface

```go
// internal/embedding/embedder.go

package embedding

import "context"

// Embedder generates vector embeddings from text.
type Embedder interface {
    // Embed returns a float32 vector for the given text.
    Embed(ctx context.Context, text string) ([]float32, error)

    // EmbedBatch returns vectors for multiple texts. Implementations should
    // batch API calls where the provider supports it.
    EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

    // ModelName returns the identifier stored in bead_embeddings.model.
    ModelName() string

    // Dimensions returns the vector dimensionality (e.g. 1024, 1536).
    Dimensions() int
}
```

**Shipped implementations:**

| Backend | Model | Dims | Dependency | Use case |
|---------|-------|------|------------|----------|
| `ollama` | nomic-embed-text-v1.5 | 1024 | Local Ollama server | Air-gapped, dev, self-hosted |
| `openai` | text-embedding-3-small | 1536 | OpenAI API key | Cloud, lowest latency per token |
| `voyage` | voyage-3-lite | 1024 | Voyage API key | Cloud, best quality/cost ratio |

The server picks the backend based on a new env var:

```
BEADS_EMBED_BACKEND=ollama          # or "openai", "voyage", "noop"
BEADS_EMBED_URL=http://localhost:11434  # Ollama base URL (default)
BEADS_EMBED_API_KEY=...             # for cloud providers
BEADS_EMBED_MODEL=nomic-embed-text  # override default model for the backend
```

A `noop` backend disables embedding entirely — existing deployments aren't
affected. The feature is opt-in.

```go
// internal/embedding/ollama.go

package embedding

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "net/http"
)

// OllamaEmbedder calls the Ollama /api/embed endpoint.
type OllamaEmbedder struct {
    baseURL string
    model   string
    dims    int
    client  *http.Client
}

func NewOllamaEmbedder(baseURL, model string) *OllamaEmbedder {
    return &OllamaEmbedder{
        baseURL: baseURL,
        model:   model,
        dims:    1024, // nomic-embed-text default
        client:  &http.Client{},
    }
}

func (o *OllamaEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
    body, _ := json.Marshal(map[string]any{
        "model": o.model,
        "input": text,
    })
    req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/embed", bytes.NewReader(body))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := o.client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("ollama embed: %w", err)
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        return nil, fmt.Errorf("ollama embed: status %d", resp.StatusCode)
    }

    var result struct {
        Embeddings [][]float32 `json:"embeddings"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return nil, fmt.Errorf("ollama embed decode: %w", err)
    }
    if len(result.Embeddings) == 0 {
        return nil, fmt.Errorf("ollama embed: empty response")
    }
    return result.Embeddings[0], nil
}

func (o *OllamaEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
    // Ollama /api/embed supports batch input natively.
    body, _ := json.Marshal(map[string]any{
        "model": o.model,
        "input": texts,
    })
    req, err := http.NewRequestWithContext(ctx, "POST", o.baseURL+"/api/embed", bytes.NewReader(body))
    if err != nil {
        return nil, err
    }
    req.Header.Set("Content-Type", "application/json")

    resp, err := o.client.Do(req)
    if err != nil {
        return nil, fmt.Errorf("ollama embed batch: %w", err)
    }
    defer resp.Body.Close()

    var result struct {
        Embeddings [][]float32 `json:"embeddings"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
        return nil, fmt.Errorf("ollama embed batch decode: %w", err)
    }
    return result.Embeddings, nil
}

func (o *OllamaEmbedder) ModelName() string { return o.model }
func (o *OllamaEmbedder) Dimensions() int   { return o.dims }
```

### 4. Indexing Pipeline

Embeddings are generated **asynchronously** via the existing event bus:

```text
bead.created / bead.updated
        │
        ▼
  Event Bus (NATS)
        │
        ▼
  EmbeddingWorker
        │
        ├─ compute text_hash
        ├─ skip if hash unchanged
        ├─ call Embedder.Embed()
        └─ UPSERT into bead_embeddings
```

```go
// internal/embedding/worker.go

package embedding

import (
    "context"
    "crypto/sha256"
    "fmt"
    "strings"

    "gardenio/kbeads/internal/model"
)

// BeadText builds the text document to embed for a bead.
func BeadText(b *model.Bead) string {
    var sb strings.Builder
    sb.WriteString("Title: ")
    sb.WriteString(b.Title)
    sb.WriteString("\nType: ")
    sb.WriteString(string(b.Type))
    if b.Description != "" {
        sb.WriteString("\nDescription: ")
        sb.WriteString(b.Description)
    }
    if b.Notes != "" {
        sb.WriteString("\nNotes: ")
        sb.WriteString(b.Notes)
    }
    if len(b.Labels) > 0 {
        sb.WriteString("\nLabels: ")
        sb.WriteString(strings.Join(b.Labels, ", "))
    }
    return sb.String()
}

// TextHash returns a hex-encoded SHA-256 of the text.
func TextHash(text string) string {
    h := sha256.Sum256([]byte(text))
    return fmt.Sprintf("%x", h)
}
```

**Batch backfill** for existing beads:

```
kd embed sync [--fleet default] [--batch-size 50] [--force]
```

This iterates all beads (paginated), computes embeddings in batches, and upserts.
`--force` re-embeds even when the hash matches (needed after model changes).

### 5. Store Interface Extension

```go
// Added to store.Store interface:

// Embedding operations
UpsertEmbedding(ctx context.Context, beadID string, embedding []float32, model string, textHash string) error
SemanticSearch(ctx context.Context, queryVec []float32, filter model.BeadFilter, limit int) ([]*model.SemanticResult, error)
DeleteEmbedding(ctx context.Context, beadID string) error
GetEmbeddingStats(ctx context.Context) (*model.EmbeddingStats, error)
```

```go
// internal/model/embedding.go

package model

// SemanticResult is a bead with a similarity score.
type SemanticResult struct {
    Bead       *Bead   `json:"bead"`
    Score      float64 `json:"score"`      // cosine similarity (0..1, higher = more similar)
    Distance   float64 `json:"distance"`   // cosine distance (1 - score)
}

// EmbeddingStats reports the state of the embedding index.
type EmbeddingStats struct {
    TotalBeads      int    `json:"total_beads"`
    EmbeddedBeads   int    `json:"embedded_beads"`
    Model           string `json:"model"`
    StaleCount      int    `json:"stale_count"` // beads whose text changed since last embed
}
```

### 6. Semantic Search Query

The key SQL query combines pgvector's nearest-neighbor search with existing
structured filters:

```sql
SELECT b.*, 1 - (e.embedding <=> $1::vector) AS score
FROM bead_embeddings e
JOIN beads b ON b.id = e.bead_id
WHERE b.fleet = $2
  AND b.status IN ('open', 'in_progress')   -- structured filters composed here
  AND EXISTS (SELECT 1 FROM labels WHERE bead_id = b.id AND label = 'project:monorepo')
ORDER BY e.embedding <=> $1::vector          -- cosine distance, uses IVFFlat index
LIMIT 20;
```

The `<=>` operator is pgvector's cosine distance. The IVFFlat index makes this
fast even at 50k+ rows. Structured filters are applied as WHERE clauses _before_
the ANN scan, which is exactly how pgvector's filtered search works.

### 7. API Surface

**HTTP:**

```
POST /v1/search/semantic
Content-Type: application/json

{
  "query": "authentication bugs in the login flow",
  "status": ["open", "in_progress"],
  "type": ["bug", "task"],
  "labels": ["project:monorepo"],
  "limit": 20,
  "min_score": 0.3
}

Response:
{
  "results": [
    {
      "bead": { "id": "ka-abc123", "title": "...", ... },
      "score": 0.87
    },
    ...
  ]
}
```

**gRPC:**

```protobuf
// Added to service.proto

message SemanticSearchRequest {
  string query = 1;
  repeated string status = 2;
  repeated string type = 3;
  repeated string kind = 4;
  repeated string labels = 5;
  string assignee = 6;
  string fleet = 7;
  int32 limit = 8;
  float min_score = 9;
}

message SemanticSearchResult {
  Bead bead = 1;
  float score = 2;
}

message SemanticSearchResponse {
  repeated SemanticSearchResult results = 1;
}

// In BeadsService:
rpc SemanticSearch(SemanticSearchRequest) returns (SemanticSearchResponse);
```

**CLI:**

```bash
# Semantic search (new flag on existing command)
kd search --semantic "auth bugs in the login flow"
kd search --semantic "what advice exists about database migrations" -t advice

# Embedding management
kd embed sync                     # backfill embeddings for all beads
kd embed sync --force             # re-embed everything (after model change)
kd embed stats                    # show embedding coverage

# "Find similar" — given a bead, find related ones
kd similar ka-abc123              # top 10 similar beads
kd similar ka-abc123 --limit 20   # control result count
```

### 8. Server Wiring

```go
// internal/server/http.go — new route
mux.HandleFunc("POST /v1/search/semantic", requireRole(RoleViewer, s.handleSemanticSearch))

// internal/server/http_search.go — handler sketch
func (s *BeadsServer) handleSemanticSearch(w http.ResponseWriter, r *http.Request) {
    var req struct {
        Query    string   `json:"query"`
        Status   []string `json:"status,omitempty"`
        Type     []string `json:"type,omitempty"`
        Kind     []string `json:"kind,omitempty"`
        Labels   []string `json:"labels,omitempty"`
        Assignee string   `json:"assignee,omitempty"`
        Fleet    string   `json:"fleet,omitempty"`
        Limit    int      `json:"limit,omitempty"`
        MinScore float64  `json:"min_score,omitempty"`
    }
    if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
        http.Error(w, "invalid request body", http.StatusBadRequest)
        return
    }

    if req.Query == "" {
        http.Error(w, "query is required", http.StatusBadRequest)
        return
    }
    if req.Limit <= 0 {
        req.Limit = 20
    }

    // 1. Embed the query text
    queryVec, err := s.embedder.Embed(r.Context(), req.Query)
    if err != nil {
        http.Error(w, "embedding failed", http.StatusInternalServerError)
        return
    }

    // 2. Build structured filter
    filter := model.BeadFilter{
        Status:   toStatuses(req.Status),
        Type:     toBeadTypes(req.Type),
        Kind:     toKinds(req.Kind),
        Labels:   req.Labels,
        Assignee: req.Assignee,
        Fleet:    fleetFromContext(r.Context(), req.Fleet),
    }

    // 3. Search
    results, err := s.store.SemanticSearch(r.Context(), queryVec, filter, req.Limit)
    if err != nil {
        http.Error(w, "search failed", http.StatusInternalServerError)
        return
    }

    // 4. Filter by minimum score
    if req.MinScore > 0 {
        filtered := results[:0]
        for _, r := range results {
            if r.Score >= req.MinScore {
                filtered = append(filtered, r)
            }
        }
        results = filtered
    }

    writeJSON(w, http.StatusOK, map[string]any{"results": results})
}
```

### 9. `kd similar` — Find Related Beads

A high-value shortcut: given an existing bead, find others like it.

```go
// cmd/kd/similar.go

var similarCmd = &cobra.Command{
    Use:   "similar <bead-id>",
    Short: "Find beads similar to a given bead",
    Args:  cobra.ExactArgs(1),
    RunE: func(cmd *cobra.Command, args []string) error {
        id := args[0]
        limit, _ := cmd.Flags().GetInt("limit")

        // Fetch the bead, build its text, embed it, search.
        // Server-side: GET /v1/beads/{id}/similar?limit=20
        results, err := beadsClient.FindSimilar(ctx, id, limit)
        // ...print results with scores...
    },
}
```

This is implemented server-side by:
1. Looking up the bead's existing embedding (or computing it on the fly)
2. Running the same `SemanticSearch` with that vector, excluding the source bead

### 10. Configuration

New env vars for the beads server:

| Variable | Default | Purpose |
|----------|---------|---------|
| `BEADS_EMBED_BACKEND` | `noop` | Embedding backend: `noop`, `ollama`, `openai`, `voyage` |
| `BEADS_EMBED_URL` | `http://localhost:11434` | Base URL for embedding API |
| `BEADS_EMBED_API_KEY` | _(empty)_ | API key for cloud backends |
| `BEADS_EMBED_MODEL` | _(backend default)_ | Override model name |
| `BEADS_EMBED_ASYNC` | `true` | Embed on write via event bus; `false` = sync on write |

### 11. Deployment

**Prerequisites:**
- Postgres 15+ with pgvector extension installed (`CREATE EXTENSION vector`)
- An embedding backend (Ollama sidecar, or cloud API key)

**Helm additions** (for Ollama sidecar):

```yaml
# gardenio/helm/gardenio/values.yaml
embedding:
  enabled: false
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

When `embedding.enabled=true`, the chart adds an Ollama sidecar to the kbeads
deployment and sets the env vars automatically.

For cloud backends, just set `BEADS_EMBED_BACKEND=openai` and
`BEADS_EMBED_API_KEY` via a secret.

### 12. Phasing

| Phase | Scope | Effort |
|-------|-------|--------|
| **P0** | Migration, `Embedder` interface, Ollama backend, `SemanticSearch` store method, HTTP endpoint, `kd search --semantic`, `kd embed sync` | ~3 days |
| **P1** | Async worker (NATS subscriber), `kd similar`, OpenAI backend, Helm sidecar | ~2 days |
| **P2** | Voyage backend, `kd embed stats`, automatic re-embed on model change, HNSW index option | ~1 day |
| **P3** | Hybrid ranking (RRF of keyword + semantic), comment embeddings, cross-fleet search | Future |
| **P4** | Intelligence: auto-dedup (flag >0.92 similar open beads on create), related-work suggestions in `gb ready`, formula recommendation ("beads like this used formula X") | Future |

### 13. Hybrid Scoring (Phase P3 Detail)

Phase P3 adds reciprocal rank fusion (RRF) to combine keyword and semantic results.
This mirrors what hippo already does in `hippo/search.go:HybridSearch`, but server-side.

**Approach**: Two-phase retrieval.

1. **Candidate retrieval**: Run both keyword (existing ILIKE or future tsvector) and
   vector search in parallel, each returning top-K candidates (K = limit * 3).
2. **RRF merge**: For each candidate appearing in either result set, compute:
   ```
   rrf_score = alpha / (k + rank_vector) + (1-alpha) / (k + rank_keyword)
   ```
   where `k=60` (standard RRF constant) and `alpha=0.6` (weight toward semantic).
3. **Metadata boost**: Apply strength/recency/decision multipliers (same formula as
   `hippo/search.go:196`) for memory-type beads.
4. **Return**: Top-N by composite score.

This avoids re-implementing BM25 in SQL — pgvector handles the vector leg, existing
ILIKE/tsvector handles the keyword leg, and Go does the fusion.

### 14. Hippo Alignment

Hippo defaults to `Xenova/all-MiniLM-L6-v2` (384 dims, see `hippo/config.go:66`).
The kbeads default is `nomic-embed-text` (1024 dims). These are different vector spaces.

**Recommendation**: Make the default model configurable but document that choosing the
same model as hippo enables cross-system similarity (e.g., finding advice beads relevant
to a hippo memory, or vice versa). If hippo's model is changed, kbeads should follow —
or provide a `kd embed sync --force` workflow to re-embed after model changes.

For Phase P4 intelligence, same-model alignment would enable:
- `gb ready` could rank available beads by similarity to the agent's recent hippo memories
- Advice beads could auto-surface during `hippo-context` injection based on vector similarity
  to the current task bead, not just tag/label matching

### 15. Index Choice: IVFFlat vs HNSW

The migration uses IVFFlat (`lists=100`). This is the right initial choice:
- IVFFlat is cheaper to build and update
- At <10k beads, scan cost is negligible

**When to upgrade to HNSW**: If bead count exceeds ~50k per fleet _and_ search
latency exceeds 100ms. HNSW gives better recall at higher row counts but costs
more memory and has slower inserts. The switch is a migration + re-index:

```sql
DROP INDEX idx_bead_embeddings_ivfflat;
CREATE INDEX idx_bead_embeddings_hnsw
    ON bead_embeddings USING hnsw (embedding vector_cosine_ops)
    WITH (m = 16, ef_construction = 128);
```

### 16. Alternatives Considered

**Postgres `tsvector` (full-text search)**  
Better than ILIKE but still lexical. Doesn't solve the synonym/concept problem.
Could be a quick win alongside (Phase P3 hybrid ranking).

**Separate vector DB (Qdrant, Weaviate, Pinecone)**  
More operational complexity. Not justified at our scale (<100k beads). pgvector
keeps the transactional guarantees and avoids sync issues.

**Embed at query time only (no stored embeddings)**  
Would require scanning all beads on every search — O(n) per query. Not viable
past a few thousand beads.

## File Map (New/Modified)

```text
kbeads/
├── internal/
│   ├── embedding/
│   │   ├── embedder.go          # Embedder interface
│   │   ├── ollama.go            # Ollama backend
│   │   ├── openai.go            # OpenAI backend (P1)
│   │   ├── voyage.go            # Voyage backend (P2)
│   │   ├── noop.go              # No-op (disabled)
│   │   ├── worker.go            # Async NATS subscriber (P1)
│   │   └── text.go              # BeadText() + TextHash()
│   ├── model/
│   │   └── embedding.go         # SemanticResult, EmbeddingStats
│   ├── store/
│   │   ├── store.go             # + 4 new methods on Store interface
│   │   └── postgres/
│   │       ├── migrations/
│   │       │   ├── 005_bead_embeddings.up.sql
│   │       │   └── 005_bead_embeddings.down.sql
│   │       └── queries_embedding.go  # SQL for upsert/search/stats
│   └── server/
│       ├── http.go              # + 1 new route
│       └── http_search.go       # handleSemanticSearch, handleFindSimilar
├── cmd/kd/
│   ├── search.go                # + --semantic flag
│   ├── similar.go               # kd similar command
│   └── embed.go                 # kd embed sync/stats commands
└── proto/beads/v1/
    ├── beads.proto              # + SemanticSearch messages
    └── service.proto            # + SemanticSearch RPC
```

## Open Questions

1. **Ollama sidecar or shared instance?** Running Ollama as a sidecar in the kbeads
   pod is simpler operationally but costs ~2-4 GiB memory per pod. A shared Ollama
   deployment (separate service) is more efficient at scale. Start with sidecar, extract
   if memory pressure becomes an issue.

2. **Embedding dimension lock-in.** Switching from nomic-embed-text (1024d) to a
   different model requires `kd embed sync --force` and an `ALTER TABLE ... ALTER COLUMN
   embedding SET DATA TYPE vector(N)`. The `model` column tracks provenance, but the
   migration tooling doesn't exist yet. Phase P2 should address this.

3. **Fleet isolation for vector search.** The WHERE clause on `fleet` provides logical
   isolation, but pgvector's IVFFlat does post-filtering — it scans the index first, then
   discards rows outside the fleet. For fleets with very different sizes, this could cause
   the smaller fleet to see degraded recall. HNSW with pre-filtering (Postgres 16+) or
   partitioned tables per fleet would solve this at scale.

4. **Embedding budget for `type=memory` beads.** Hippo memories stored as beads already
   have their own embedding lifecycle. Should the kbeads worker skip `type=memory` beads
   to avoid double-embedding? Or should it embed them independently (different model/text
   construction) for cross-type search? Start by skipping them (hippo owns its own
   embeddings), revisit when cross-system search (Phase P4) is designed.
