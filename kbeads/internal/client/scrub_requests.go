package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"gardenio/kbeads/internal/model"
)

// ApproveScrubRequestRequest is the body for POST /v1/scrub-requests/{id}/approve.
//
// Deliberately carries no ApprovedBy field, unlike ApproveEscalationRequest:
// the server derives the approver from the authenticated OIDC identity and
// only logs a body-supplied value (taints ADR §6.3 correction 1) — giving the
// CLI a field for it would invite callers to believe they can set it.
type ApproveScrubRequestRequest struct {
	ApprovalReason string `json:"approval_reason,omitempty"`
}

// approveScrubRequestResponse is the shape of a successful POST
// /v1/scrub-requests/{id}/approve response body (handleApproveScrubRequest,
// http_scrub_requests.go). Artifact is nil whenever this call's execution
// didn't actually run ScrubTaint (e.g. an idempotent retry of an
// already-executed request) — see BuildScrubAuditArtifact's doc comment.
type approveScrubRequestResponse struct {
	Artifact *model.ScrubAuditArtifact `json:"artifact"`
}

// ApproveScrubRequest approves a scrub-request (POST
// /v1/scrub-requests/{id}/approve). Operator-only on the server
// (RoleOperator), and OIDC-only within that via scrubApprover (NOT
// escalationApprover): an agent token, and a static/fleet token even when it
// relays a human identity via X-Operated-By, both get 403.
//
// Returns the docs/adr/taints.md §6.7 audit artifact the response carried,
// nil when the response carried none (see approveScrubRequestResponse).
func (c *HTTPClient) ApproveScrubRequest(ctx context.Context, id string, req ApproveScrubRequestRequest) (*model.ScrubAuditArtifact, error) {
	path := "/v1/scrub-requests/" + url.PathEscape(id) + "/approve"
	var resp approveScrubRequestResponse
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, fmt.Errorf("approving scrub-request %s: %w", id, err)
	}
	return resp.Artifact, nil
}

// RejectScrubRequestRequest is the body for POST /v1/scrub-requests/{id}/reject.
//
// Reason is required by the server (empty is rejected as a 400) and, like
// ApproveScrubRequestRequest, carries no client-suppliable identity field —
// the rejector is server-derived the same way the approver is.
type RejectScrubRequestRequest struct {
	Reason string `json:"reason"`
}

// RejectScrubRequest rejects a scrub-request (POST
// /v1/scrub-requests/{id}/reject). Same identity restrictions as
// ApproveScrubRequest.
func (c *HTTPClient) RejectScrubRequest(ctx context.Context, id string, req RejectScrubRequestRequest) error {
	path := "/v1/scrub-requests/" + url.PathEscape(id) + "/reject"
	if err := c.doJSON(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("rejecting scrub-request %s: %w", id, err)
	}
	return nil
}
