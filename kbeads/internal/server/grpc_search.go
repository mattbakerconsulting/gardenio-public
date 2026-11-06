package server

import (
	"context"
	"errors"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// SemanticSearch embeds the caller-supplied query text, runs a
// nearest-neighbor search over the configured store, and returns the top-N
// scoring beads (optionally filtered by a minimum score). Mirrors the
// semantics of POST /v1/search/semantic (see handleSemanticSearch).
//
// When the configured embedder is disabled or unset, the RPC returns
// codes.Unavailable so clients can fall back to lexical search.
func (s *BeadsServer) SemanticSearch(ctx context.Context, req *beadsv1.SemanticSearchRequest) (*beadsv1.SemanticSearchResponse, error) {
	if req.GetQuery() == "" {
		return nil, status.Error(codes.InvalidArgument, "query is required")
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	// Reject unknown type filter values server-side (ka-0000007554) — the SEARCH
	// twin of the LIST choke point (ka-0000008041). Validate BEFORE embedding so a
	// bad type fails loud (InvalidArgument) regardless of embedder availability,
	// rather than spending an embed call only to silently match nothing.
	// filterTypes is already alias-resolved by toBeadTypes.
	filterTypes := toBeadTypes(req.GetType())
	if err := s.validateFilterTypes(ctx, filterTypes); err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			return nil, status.Error(codes.InvalidArgument, ie.Error())
		}
		return nil, err
	}

	if s.embedder == nil {
		return nil, status.Error(codes.Unavailable, "semantic search disabled (no embedder configured)")
	}

	// Phase-split timing mirrors handleSemanticSearch so the gRPC and HTTP
	// search paths feed the same embed_query / pgvector_scan / total series.
	searchStart := time.Now()
	queryVec, err := s.embedder.Embed(ctx, req.GetQuery())
	metrics.SemanticSearchDuration.WithLabelValues(metrics.PhaseEmbedQuery).Observe(time.Since(searchStart).Seconds())
	if errors.Is(err, embedding.ErrDisabled) {
		return nil, status.Error(codes.Unavailable, "semantic search disabled (BEADS_EMBED_BACKEND=noop)")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to embed query")
	}

	filter := model.BeadFilter{
		Status:   toStatuses(req.GetStatus()),
		Type:     filterTypes,
		Kind:     toKinds(req.GetKind()),
		Labels:   req.GetLabels(),
		Assignee: req.GetAssignee(),
		Fleet:    FleetFromContext(ctx),
	}
	// Read-side scope: mirrors ListBeads so that neither an OIDC user nor a
	// project-bound fleet token can semantic-search its way into a project it
	// is not authorized to read (ka-0000008132), and so a caller lacking a
	// taint grant cannot search its way to a tainted bead's content either
	// (ADR docs/adr/taints.md §4.3/§5.1, gd-Jler8Q2wts) — this used to
	// duplicate just the two project gates inline; routing through
	// scopeListFilter picks up the taint gate for free and stays in sync with
	// ListBeads/handleListBeads going forward.
	filter = s.scopeListFilter(ctx, filter)

	scanStart := time.Now()
	results, err := s.store.SemanticSearch(ctx, queryVec, filter, limit)
	metrics.SemanticSearchDuration.WithLabelValues(metrics.PhasePgvector).Observe(time.Since(scanStart).Seconds())
	if err != nil {
		return nil, status.Error(codes.Internal, "semantic search failed")
	}

	minScore := float64(req.GetMinScore())
	out := make([]*beadsv1.SemanticSearchResult, 0, len(results))
	readIDs := make([]string, 0, len(results))
	for _, res := range results {
		if res == nil || res.Bead == nil {
			continue
		}
		if minScore > 0 && res.Score < minScore {
			continue
		}
		out = append(out, &beadsv1.SemanticSearchResult{
			Bead:  beadToProto(res.Bead),
			Score: float32(res.Score),
		})
		readIDs = append(readIDs, res.Bead.ID)
	}
	if err := s.propagateReadTaintBeads(ctx, readIDs); err != nil {
		return nil, status.Error(codes.Internal, "failed to propagate read taint")
	}

	metrics.SemanticSearchDuration.WithLabelValues(metrics.PhaseTotal).Observe(time.Since(searchStart).Seconds())
	metrics.SemanticSearchResults.Observe(float64(len(out)))

	resp := &beadsv1.SemanticSearchResponse{Results: out}
	if s.embedder != nil {
		resp.Model = s.embedder.ModelName()
	}
	return resp, nil
}
