package server

import (
	"errors"
	"net/http"
	"time"

	"gardenio/kbeads/internal/embedding"
	"gardenio/kbeads/internal/metrics"
	"gardenio/kbeads/internal/model"
)

// SemanticSearchRequest is the body for POST /v1/search/semantic.
type SemanticSearchRequest struct {
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

// SemanticSearchResponse is the response body for POST /v1/search/semantic.
type SemanticSearchResponse struct {
	Model   string                  `json:"model,omitempty"`
	Results []*model.SemanticResult `json:"results"`
}

// handleSemanticSearch handles POST /v1/search/semantic. It embeds the
// caller-supplied query text, runs a nearest-neighbor search over the
// configured store, and returns the top-N scoring beads (optionally
// filtered by a minimum score).
//
// When the configured embedder is disabled (BEADS_EMBED_BACKEND=noop),
// the handler returns 503 Service Unavailable so clients can fall back
// to lexical search.
func (s *BeadsServer) handleSemanticSearch(w http.ResponseWriter, r *http.Request) {
	var req SemanticSearchRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Query == "" {
		writeError(w, http.StatusBadRequest, "query is required")
		return
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	if req.Limit > 100 {
		req.Limit = 100
	}

	// Reject unknown type= filter values server-side (ka-0000007554) — the SEARCH
	// twin of the LIST choke point (ka-0000008041). Validate BEFORE embedding so a
	// bad type fails loud (400) regardless of embedder availability, rather than
	// spending an embed call only to silently match nothing. filterTypes is
	// already alias-resolved by toBeadTypes.
	filterTypes := toBeadTypes(req.Type)
	if err := s.validateFilterTypes(r.Context(), filterTypes); err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
		} else {
			writeServerError(r.Context(), w, "handleSemanticSearch", err, "failed to validate type filter")
		}
		return
	}

	// A disabled/absent embedder is an EXPECTED degraded state (e.g.
	// BEADS_EMBED_BACKEND=noop in the e2e fixture env), and clients fall back
	// to lexical search on a 503. But 503 is still a 5xx, so the logging
	// middleware emits a "http 5xx root cause" WARN — attach a structured
	// cause so that line reads as the known "semantic search disabled" state
	// instead of the misleading "unknown (handler did not call
	// AttachServerError)" path (ka-0000002434).
	if s.embedder == nil {
		AttachServerError(r.Context(), "handleSemanticSearch", embedding.ErrDisabled)
		writeError(w, http.StatusServiceUnavailable, "semantic search disabled (no embedder configured)")
		return
	}

	// Time the whole search and each phase separately so the
	// gardenio_beads_semantic_search_duration_seconds histogram splits into
	// embed_query / pgvector_scan / total (kbeads-embedding board).
	searchStart := time.Now()
	queryVec, err := s.embedder.Embed(r.Context(), req.Query)
	metrics.SemanticSearchDuration.WithLabelValues(metrics.PhaseEmbedQuery).Observe(time.Since(searchStart).Seconds())
	if errors.Is(err, embedding.ErrDisabled) {
		AttachServerError(r.Context(), "handleSemanticSearch", err)
		writeError(w, http.StatusServiceUnavailable, "semantic search disabled (BEADS_EMBED_BACKEND=noop)")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleSemanticSearch", err, "failed to embed query")
		return
	}

	filter := model.BeadFilter{
		Status:   toStatuses(req.Status),
		Type:     filterTypes,
		Kind:     toKinds(req.Kind),
		Labels:   req.Labels,
		Assignee: req.Assignee,
		Fleet:    FleetFromContext(r.Context()),
	}
	// Read-side scope: mirrors handleListBeads so that neither an OIDC user
	// nor a project-bound fleet token can semantic-search its way into a
	// project it is not authorized to read (ka-0000008132), and so a caller
	// lacking a taint grant cannot search its way to a tainted bead's content
	// either (ADR docs/adr/taints.md §4.3/§5.1, gd-Jler8Q2wts) — this used to
	// duplicate just the two project gates inline; routing through
	// scopeListFilter picks up the taint gate for free and stays in sync with
	// ListBeads/handleListBeads going forward.
	filter = s.scopeListFilter(r.Context(), filter)

	scanStart := time.Now()
	results, err := s.store.SemanticSearch(r.Context(), queryVec, filter, req.Limit)
	metrics.SemanticSearchDuration.WithLabelValues(metrics.PhasePgvector).Observe(time.Since(scanStart).Seconds())
	if err != nil {
		writeServerError(r.Context(), w, "handleSemanticSearch", err, "semantic search failed")
		return
	}

	out := make([]*model.SemanticResult, 0, len(results))
	for _, res := range results {
		if res == nil || res.Bead == nil {
			continue
		}
		if req.MinScore > 0 && res.Score < req.MinScore {
			continue
		}
		out = append(out, res)
	}

	if err := s.propagateReadTaintBeads(r.Context(), semanticResultIDs(out)); err != nil {
		writeServerError(r.Context(), w, "handleSemanticSearch", err, "failed to propagate read taint")
		return
	}

	metrics.SemanticSearchDuration.WithLabelValues(metrics.PhaseTotal).Observe(time.Since(searchStart).Seconds())
	metrics.SemanticSearchResults.Observe(float64(len(out)))

	resp := &SemanticSearchResponse{Results: out}
	if s.embedder != nil {
		resp.Model = s.embedder.ModelName()
	}
	writeJSON(w, http.StatusOK, resp)
}

func toStatuses(ss []string) []model.Status {
	if len(ss) == 0 {
		return nil
	}
	out := make([]model.Status, 0, len(ss))
	for _, s := range ss {
		if s == "" {
			continue
		}
		out = append(out, model.Status(s))
	}
	return out
}

func toBeadTypes(ts []string) []model.BeadType {
	if len(ts) == 0 {
		return nil
	}
	out := make([]model.BeadType, 0, len(ts))
	for _, t := range ts {
		if t == "" {
			continue
		}
		bt := model.BeadType(t)
		// Resolve deprecated aliases (template→formula, bundle→molecule) so the
		// SEARCH path matches buildListFilter/handleListBeads — otherwise a
		// deprecated-but-valid alias would silently match nothing and, once
		// validated by validateFilterTypes, would even 400 a value the create
		// path accepts (ka-0000007554).
		if canonical, ok := model.TypeAliases[bt]; ok {
			bt = canonical
		}
		out = append(out, bt)
	}
	return out
}

func toKinds(ks []string) []model.Kind {
	if len(ks) == 0 {
		return nil
	}
	out := make([]model.Kind, 0, len(ks))
	for _, k := range ks {
		if k == "" {
			continue
		}
		out = append(out, model.Kind(k))
	}
	return out
}
