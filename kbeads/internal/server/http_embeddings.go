package server

import (
	"fmt"
	"net/http"

	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/model"
)

// UpsertEmbeddingRequest is the JSON body for POST /v1/embeddings/upsert.
type UpsertEmbeddingRequest struct {
	BeadID    string    `json:"bead_id"`
	Embedding []float32 `json:"embedding"`
	Model     string    `json:"model"`
	TextHash  string    `json:"text_hash"`
}

// EmbeddingHashesRequest is the JSON body for POST /v1/embeddings/hashes.
type EmbeddingHashesRequest struct {
	BeadIDs []string `json:"bead_ids"`
}

// EmbeddingHashesResponse is the response for POST /v1/embeddings/hashes.
type EmbeddingHashesResponse struct {
	Hashes map[string]string `json:"hashes"`
}

// handleUpsertEmbedding handles POST /v1/embeddings/upsert. Inserts or
// updates the embedding row for a bead. Callers supply the vector, model
// identifier, and text hash computed from the bead's canonical text.
// Admin-only; used by the `kd embed sync` backfill flow.
func (s *BeadsServer) handleUpsertEmbedding(w http.ResponseWriter, r *http.Request) {
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "embedding upsert requires an OIDC admin")
		return
	}
	var req UpsertEmbeddingRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.BeadID == "" {
		writeError(w, http.StatusBadRequest, "bead_id is required")
		return
	}
	if len(req.Embedding) == 0 {
		writeError(w, http.StatusBadRequest, "embedding is required")
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.TextHash == "" {
		writeError(w, http.StatusBadRequest, "text_hash is required")
		return
	}
	if err := s.store.UpsertEmbedding(r.Context(), req.BeadID, req.Embedding, req.Model, req.TextHash); err != nil {
		writeServerError(r.Context(), w, "handleUpsertEmbedding", err, "failed to upsert embedding")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleEmbeddingHashes handles POST /v1/embeddings/hashes. Returns the
// stored text_hash for each requested bead that has an embedding row.
// Beads without an embedding are omitted. Admin-only; lets the sync
// client decide whether a bead needs to be re-embedded before calling
// the remote embedder.
func (s *BeadsServer) handleEmbeddingHashes(w http.ResponseWriter, r *http.Request) {
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "embedding hashes requires an OIDC admin")
		return
	}
	var req EmbeddingHashesRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	hashes, err := s.store.GetEmbeddingHashes(r.Context(), req.BeadIDs)
	if err != nil {
		writeServerError(r.Context(), w, "handleEmbeddingHashes", err, "failed to get embedding hashes")
		return
	}
	if hashes == nil {
		hashes = map[string]string{}
	}
	writeJSON(w, http.StatusOK, &EmbeddingHashesResponse{Hashes: hashes})
}

// MigrateEmbeddingsRequest is the JSON body for POST /v1/embeddings/migrate.
type MigrateEmbeddingsRequest struct {
	Model string `json:"model"`
	Dims  int    `json:"dims"`
	Actor string `json:"actor,omitempty"`
	Note  string `json:"note,omitempty"`
}

// handleMigrateEmbeddings handles POST /v1/embeddings/migrate. Destructive:
// drops the ANN index, truncates bead_embeddings, and alters the embedding
// column to vector(N). Callers are expected to follow up with the admin
// upsert endpoint (usually via `kd embed sync --force`) and then call the
// companion reindex endpoint to re-build the ANN index. Admin-only.
func (s *BeadsServer) handleMigrateEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "embedding migrate requires an OIDC admin")
		return
	}
	var req MigrateEmbeddingsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required")
		return
	}
	if req.Dims <= 0 {
		writeError(w, http.StatusBadRequest, "dims must be > 0")
		return
	}
	// Validate that the server's configured embedder matches the requested
	// target. Without this check, a caller could migrate the schema to a
	// model the server can't actually produce vectors for, leaving the
	// ANN index permanently empty.
	if s.embedder == nil {
		writeError(w, http.StatusServiceUnavailable, "embedding disabled (no embedder configured)")
		return
	}
	if _, disabled := s.embedder.(*embedding.NoopEmbedder); disabled {
		writeError(w, http.StatusServiceUnavailable, "embedding disabled (server BEADS_EMBED_BACKEND=noop)")
		return
	}
	if got := s.embedder.ModelName(); got != req.Model {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("server embedder reports model %q, migrate target is %q; reconfigure BEADS_EMBED_MODEL on the daemon first", got, req.Model))
		return
	}
	if got := s.embedder.Dimensions(); got != req.Dims {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("server embedder reports %d dims, migrate target is %d dims", got, req.Dims))
		return
	}
	if err := s.store.MigrateEmbeddingsSchema(r.Context(), req.Model, req.Dims, req.Actor, req.Note); err != nil {
		writeServerError(r.Context(), w, "handleMigrateEmbeddings", err, "failed to migrate embeddings schema")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// SyncBatchEmbeddingsRequest is the JSON body for POST /v1/embeddings/sync-batch.
// Callers paginate a backfill across many requests by advancing Offset by the
// PageCount returned in each response. Force skips the text_hash check.
type SyncBatchEmbeddingsRequest struct {
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Fleet  string `json:"fleet,omitempty"`
	Force  bool   `json:"force,omitempty"`
	// OnlyMissing pre-filters the page at the SQL layer to beads that have
	// no embedding row OR whose embedding is older than the bead, so the
	// CLI no longer walks every bead just to skip already-embedded ones.
	// Mutually exclusive with Force (which embeds everything regardless).
	OnlyMissing bool `json:"only_missing,omitempty"`
	// Types restricts embedding to beads whose type is in this list.
	// Empty = no allowlist (all types allowed). Operator-driven cost
	// control: telemetry-shaped types (event, session, agent stubs)
	// embed to ~zero semantic value and dominate request volume in
	// fleets that produce them.
	Types []string `json:"types,omitempty"`
	// ExcludeTypes skips beads whose type is in this list. Applied
	// after Types so it can subtract from an allowlist.
	ExcludeTypes []string `json:"exclude_types,omitempty"`
}

// SyncBatchEmbeddingsResponse describes the work the server performed on a
// single batch. PageCount is the number of beads the server considered this
// call (embedded + skipped + filtered_type); callers should advance Offset
// by PageCount and stop when PageCount is zero or less than Limit. Total
// is the total bead count reported by ListBeads (0 when unknown).
type SyncBatchEmbeddingsResponse struct {
	Total     int `json:"total"`
	PageCount int `json:"page_count"`
	Embedded  int `json:"embedded"`
	Skipped   int `json:"skipped"`
	// FilteredType is the count of beads on the page that were skipped
	// because of Types/ExcludeTypes (distinct from Skipped, which counts
	// text_hash-unchanged skips).
	FilteredType int    `json:"filtered_type,omitempty"`
	Model        string `json:"model,omitempty"`
}

// handleSyncBatchEmbeddings handles POST /v1/embeddings/sync-batch. Lists one
// page of beads, computes the canonical text + hash for each, skips beads
// whose hash has not changed (unless force=true), embeds the remainder via
// the server's configured embedder, and upserts the vectors. This endpoint
// lets the CLI drive a resumable backfill without ever calling the embedding
// backend from outside the cluster — important when the embedding API is
// behind an IP whitelist. Admin-only.
func (s *BeadsServer) handleSyncBatchEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "embedding sync-batch requires an OIDC admin")
		return
	}
	var req SyncBatchEmbeddingsRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Limit <= 0 {
		writeError(w, http.StatusBadRequest, "limit must be > 0")
		return
	}
	if req.Limit > 500 {
		writeError(w, http.StatusBadRequest, "limit must be <= 500")
		return
	}
	if req.Offset < 0 {
		writeError(w, http.StatusBadRequest, "offset must be >= 0")
		return
	}
	if s.embedder == nil {
		writeError(w, http.StatusServiceUnavailable, "embedding disabled (no embedder configured)")
		return
	}
	if _, disabled := s.embedder.(*embedding.NoopEmbedder); disabled {
		writeError(w, http.StatusServiceUnavailable, "embedding disabled (server BEADS_EMBED_BACKEND=noop)")
		return
	}

	ctx := r.Context()
	if req.OnlyMissing && req.Force {
		writeError(w, http.StatusBadRequest, "only_missing and force are mutually exclusive")
		return
	}
	var (
		beads []*model.Bead
		total int
		err   error
	)
	if req.OnlyMissing {
		beads, total, err = s.store.ListBeadsNeedingEmbedding(ctx, req.Fleet, req.Limit, req.Offset)
	} else {
		filter := model.BeadFilter{
			Limit:  req.Limit,
			Offset: req.Offset,
			Fleet:  req.Fleet,
		}
		beads, total, err = s.store.ListBeads(ctx, filter)
	}
	if err != nil {
		writeServerError(ctx, w, "handleSyncBatchEmbeddings", err, "failed to list beads")
		return
	}

	allowedTypes := toTypeSet(req.Types)
	excludedTypes := toTypeSet(req.ExcludeTypes)

	pending := make([]*model.Bead, 0, len(beads))
	pendingText := make([]string, 0, len(beads))
	pendingHash := make([]string, 0, len(beads))

	// Pre-filter by type before requesting hashes — saves a round-trip
	// when most of the page is filtered out (e.g. a pod that emits a lot
	// of `event` beads with `--exclude-types event`).
	filteredType := 0
	candidates := make([]*model.Bead, 0, len(beads))
	for _, b := range beads {
		if b == nil {
			continue
		}
		if !typeAllowedSrv(string(b.Type), allowedTypes, excludedTypes) {
			filteredType++
			continue
		}
		candidates = append(candidates, b)
	}

	var existing map[string]string
	if !req.Force && len(candidates) > 0 {
		ids := make([]string, 0, len(candidates))
		for _, b := range candidates {
			ids = append(ids, b.ID)
		}
		existing, err = s.store.GetEmbeddingHashes(ctx, ids)
		if err != nil {
			writeServerError(ctx, w, "handleSyncBatchEmbeddings", err, "failed to get existing hashes")
			return
		}
	}

	skipped := 0
	// Track skipped bead IDs so we can refresh their embedding timestamps
	// when only_missing is set: without this the SQL pre-filter
	// (b.updated_at > e.updated_at) keeps re-listing the same beads forever
	// and the CLI loop with offset=0 never progresses (ka-0000000728).
	var skippedIDs []string
	for _, b := range candidates {
		text := embedding.BeadText(b)
		hash := embedding.TextHash(text)
		if !req.Force {
			if existingHash, ok := existing[b.ID]; ok && existingHash == hash {
				skipped++
				if req.OnlyMissing {
					skippedIDs = append(skippedIDs, b.ID)
				}
				continue
			}
		}
		pending = append(pending, b)
		pendingText = append(pendingText, text)
		pendingHash = append(pendingHash, hash)
	}

	// Mark skipped beads as fresh so they drop out of the
	// ListBeadsNeedingEmbedding result on the next page. Done before the
	// embedder calls so that even if the embedder errors, progress is
	// preserved for the rows we already determined are up-to-date.
	if len(skippedIDs) > 0 {
		if _, err := s.store.RefreshEmbeddingTimestamps(ctx, skippedIDs); err != nil {
			writeServerError(ctx, w, "handleSyncBatchEmbeddings", err, "failed to refresh skipped embedding timestamps")
			return
		}
	}

	embedded := 0
	modelName := s.embedder.ModelName()
	if len(pending) > 0 {
		vectors, err := s.embedder.EmbedBatch(ctx, pendingText)
		if err != nil {
			writeServerError(ctx, w, "handleSyncBatchEmbeddings", err, "failed to embed batch")
			return
		}
		if len(vectors) != len(pending) {
			writeServerError(ctx, w, "handleSyncBatchEmbeddings",
				fmt.Errorf("embed batch returned %d vectors for %d texts", len(vectors), len(pending)),
				"embedder returned mismatched vector count")
			return
		}
		for i, b := range pending {
			if err := s.store.UpsertEmbedding(ctx, b.ID, vectors[i], modelName, pendingHash[i]); err != nil {
				writeServerError(ctx, w, "handleSyncBatchEmbeddings", err, "failed to upsert embedding")
				return
			}
			embedded++
		}
	}

	writeJSON(w, http.StatusOK, &SyncBatchEmbeddingsResponse{
		Total:        total,
		PageCount:    len(beads),
		Embedded:     embedded,
		Skipped:      skipped,
		FilteredType: filteredType,
		Model:        modelName,
	})
}

// toTypeSet converts a list of type names to a set; nil/empty -> nil.
func toTypeSet(types []string) map[string]struct{} {
	if len(types) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(types))
	for _, t := range types {
		if t == "" {
			continue
		}
		out[t] = struct{}{}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// typeAllowedSrv mirrors the CLI helper: empty allowed = "all types",
// excluded always wins over allowed.
func typeAllowedSrv(beadType string, allowed, excluded map[string]struct{}) bool {
	if _, blocked := excluded[beadType]; blocked {
		return false
	}
	if len(allowed) == 0 {
		return true
	}
	_, ok := allowed[beadType]
	return ok
}

// handleReindexEmbeddings handles POST /v1/embeddings/reindex. Re-creates the
// ANN index on bead_embeddings, intended to be called after a backfill has
// repopulated the table. Admin-only.
func (s *BeadsServer) handleReindexEmbeddings(w http.ResponseWriter, r *http.Request) {
	if !s.isAdminRequest(r) {
		writeError(w, http.StatusForbidden, "embedding reindex requires an OIDC admin")
		return
	}
	if err := s.store.RecreateEmbeddingIndex(r.Context()); err != nil {
		writeServerError(r.Context(), w, "handleReindexEmbeddings", err, "failed to recreate embedding index")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
