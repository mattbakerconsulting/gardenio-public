package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// SemanticSearchRequest is the body for POST /v1/search/semantic.
type SemanticSearchRequest struct {
	Query            string   `json:"query"`
	Status           []string `json:"status,omitempty"`
	Type             []string `json:"type,omitempty"`
	Kind             []string `json:"kind,omitempty"`
	Labels           []string `json:"labels,omitempty"`
	Assignee         string   `json:"assignee,omitempty"`
	Fleet            string   `json:"fleet,omitempty"`
	OperatedBy       string   `json:"operated_by,omitempty"`
	OperatedByIsNull bool     `json:"operated_by_is_null,omitempty"`
	Limit            int      `json:"limit,omitempty"`
	MinScore         float64  `json:"min_score,omitempty"`
}

// SemanticSearchResult is one neighbor bead with its similarity score.
type SemanticSearchResult struct {
	Bead     *BeadDetail `json:"-"`
	Score    float64     `json:"score"`
	Distance float64     `json:"distance"`
}

// SemanticSearchResponse is the response body for POST /v1/search/semantic.
type SemanticSearchResponse struct {
	Model   string                  `json:"model,omitempty"`
	Results []*SemanticSearchResult `json:"results"`
}

// semanticSearchWire is the JSON decode shape (Bead is beadJSON on the wire).
type semanticSearchWire struct {
	Model   string `json:"model,omitempty"`
	Results []struct {
		Bead     *beadJSON `json:"bead"`
		Score    float64   `json:"score"`
		Distance float64   `json:"distance"`
	} `json:"results"`
}

// FindSimilarResponse is the response body for GET /v1/beads/{id}/similar.
type FindSimilarResponse struct {
	SourceID string                  `json:"source_id"`
	Model    string                  `json:"model,omitempty"`
	Results  []*SemanticSearchResult `json:"-"`
}

// findSimilarWire is the JSON decode shape (Bead is beadJSON on the wire).
type findSimilarWire struct {
	SourceID string `json:"source_id"`
	Model    string `json:"model,omitempty"`
	Results  []struct {
		Bead     *beadJSON `json:"bead"`
		Score    float64   `json:"score"`
		Distance float64   `json:"distance"`
	} `json:"results"`
}

// FindSimilar calls GET /v1/beads/{id}/similar?limit=N. Returns the source
// bead's nearest neighbors by embedding similarity, excluding the source bead
// itself. Callers should treat any error as "hide the panel" and not surface
// it to the user (common errors: 404 when the bead is gone, 409 when the bead
// has no embedding yet, 503 when the embedder is disabled).
func (c *Client) FindSimilar(ctx context.Context, id string, limit int) (*FindSimilarResponse, error) {
	if limit <= 0 {
		limit = 10
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(limit))
	path := "/v1/beads/" + url.PathEscape(id) + "/similar?" + q.Encode()

	var wire findSimilarWire
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &wire); err != nil {
		return nil, fmt.Errorf("find similar: %w", err)
	}
	out := &FindSimilarResponse{
		SourceID: wire.SourceID,
		Model:    wire.Model,
		Results:  make([]*SemanticSearchResult, 0, len(wire.Results)),
	}
	for _, r := range wire.Results {
		if r.Bead == nil {
			continue
		}
		out.Results = append(out.Results, &SemanticSearchResult{
			Bead:     r.Bead.toDetail(),
			Score:    r.Score,
			Distance: r.Distance,
		})
	}
	return out, nil
}

// SemanticSearch calls POST /v1/search/semantic. When the daemon's embedder is
// disabled (BEADS_EMBED_BACKEND=noop) the daemon returns 503; callers should
// detect this via *APIError with StatusCode == http.StatusServiceUnavailable
// and fall back to lexical search (ListBeadsFiltered with Search set).
func (c *Client) SemanticSearch(ctx context.Context, req SemanticSearchRequest) (*SemanticSearchResponse, error) {
	var wire semanticSearchWire
	if err := c.doJSON(ctx, http.MethodPost, "/v1/search/semantic", req, &wire); err != nil {
		return nil, fmt.Errorf("semantic search: %w", err)
	}
	out := &SemanticSearchResponse{
		Model:   wire.Model,
		Results: make([]*SemanticSearchResult, 0, len(wire.Results)),
	}
	for _, r := range wire.Results {
		if r.Bead == nil {
			continue
		}
		out.Results = append(out.Results, &SemanticSearchResult{
			Bead:     r.Bead.toDetail(),
			Score:    r.Score,
			Distance: r.Distance,
		})
	}
	return out, nil
}
