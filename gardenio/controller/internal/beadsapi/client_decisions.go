package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
)

// ResolveDecisionRequest is the request body for POST /v1/decisions/{id}/resolve.
type ResolveDecisionRequest struct {
	SelectedOption string `json:"selected_option"`
	ResponseText   string `json:"response_text,omitempty"`
	RespondedBy    string `json:"responded_by"`
}

// ResolveDecision resolves a decision by ID.
func (c *Client) ResolveDecision(ctx context.Context, decisionID string, req ResolveDecisionRequest) error {
	path := "/v1/decisions/" + url.PathEscape(decisionID) + "/resolve"
	if err := c.doJSON(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("resolving decision %s: %w", decisionID, err)
	}
	return nil
}

// CancelDecision cancels a decision by ID.
func (c *Client) CancelDecision(ctx context.Context, decisionID, reason, canceledBy string) error {
	body := map[string]string{
		"reason":      reason,
		"canceled_by": canceledBy,
	}
	path := "/v1/decisions/" + url.PathEscape(decisionID) + "/cancel"
	if err := c.doJSON(ctx, http.MethodPost, path, body, nil); err != nil {
		return fmt.Errorf("canceling decision %s: %w", decisionID, err)
	}
	return nil
}

// DecisionDetail is the response from GET /v1/decisions/{id}.
type DecisionDetail struct {
	Decision *BeadDetail `json:"decision"`
	Issue    *BeadDetail `json:"issue,omitempty"`
}

// decisionDetailJSON is the raw JSON representation used for unmarshaling.
// Fields may contain arrays/objects (e.g., options) that need ParseFieldsJSON.
type decisionDetailJSON struct {
	Decision *beadJSON `json:"decision"`
	Issue    *beadJSON `json:"issue,omitempty"`
}

func (d *decisionDetailJSON) toDetail() *DecisionDetail {
	dd := &DecisionDetail{}
	if d.Decision != nil && d.Decision.ID != "" {
		dd.Decision = d.Decision.toDetail()
	}
	if d.Issue != nil {
		dd.Issue = d.Issue.toDetail()
	}
	return dd
}

// GetDecision fetches a decision by ID with its associated issue.
func (c *Client) GetDecision(ctx context.Context, decisionID string) (*DecisionDetail, error) {
	var resp decisionDetailJSON
	path := "/v1/decisions/" + url.PathEscape(decisionID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting decision %s: %w", decisionID, err)
	}
	return resp.toDetail(), nil
}

// ListDecisions lists decisions with optional status/limit filters.
func (c *Client) ListDecisions(ctx context.Context, status string, limit int) ([]DecisionDetail, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := "/v1/decisions"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp struct {
		Decisions []decisionDetailJSON `json:"decisions"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing decisions: %w", err)
	}
	result := make([]DecisionDetail, len(resp.Decisions))
	for i := range resp.Decisions {
		dd := resp.Decisions[i].toDetail()
		result[i] = *dd
	}
	return result, nil
}
