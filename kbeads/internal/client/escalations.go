package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// EscalationDetail is the decoded response from GET /v1/escalations/{id} and a
// single element of GET /v1/escalations. The daemon wraps every escalation as
// two views: `escalation` holds only the extracted escalation-specific fields
// (operation_type, approved_by, …) while `issue` is the full underlying bead
// (id, status, created_by, priority, …). Consumers that need id/status/created_at
// read Issue; the escalation-specific projection is exposed as Fields.
//
// This mirrors gardenio/controller/internal/beadsapi.EscalationDetail — kd needs
// the same read surface so a ka-only agent can inspect escalations without
// switching to gb (ka-0000007156).
type EscalationDetail struct {
	// Escalation is the raw escalation-fields projection the daemon returns
	// under the `escalation` key (operation_type, sensitivity, approved_by, …).
	// It carries no bead identity, so callers that need id/status/etc use Issue.
	Escalation map[string]any `json:"escalation"`
	// Issue is the full underlying bead — the authoritative source of id,
	// status, priority, created_by, created_at, and the complete Fields map.
	Issue *model.Bead `json:"issue,omitempty"`
}

// GetEscalation fetches a single escalation by ID (GET /v1/escalations/{id}).
// Returns an *APIError with StatusCode 404 when no such escalation exists.
func (c *HTTPClient) GetEscalation(ctx context.Context, id string) (*EscalationDetail, error) {
	var resp EscalationDetail
	path := "/v1/escalations/" + url.PathEscape(id)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting escalation %s: %w", id, err)
	}
	return &resp, nil
}

// ListEscalationsOptions narrows GET /v1/escalations. Every field is optional;
// the zero value lists the daemon default (open + in_progress, 50 rows).
type ListEscalationsOptions struct {
	// Status is a comma-separated status list. Empty defers to the daemon
	// default (open + in_progress).
	Status string
	// Limit <= 0 defers to the daemon default (50).
	Limit int
	// OperationTypes keeps escalations whose operation_type is any of these.
	OperationTypes []string
	// Labels keeps escalations carrying ALL of these labels.
	Labels []string
	// ExcludeLabels drops escalations carrying ANY of these labels.
	ExcludeLabels []string
}

// ListEscalations lists escalations (GET /v1/escalations), filtered by opts.
func (c *HTTPClient) ListEscalations(ctx context.Context, opts ListEscalationsOptions) ([]EscalationDetail, error) {
	q := url.Values{}
	if opts.Status != "" {
		q.Set("status", opts.Status)
	}
	if opts.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", opts.Limit))
	}
	if len(opts.OperationTypes) > 0 {
		q.Set("operation_type", strings.Join(opts.OperationTypes, ","))
	}
	if len(opts.Labels) > 0 {
		q.Set("label", strings.Join(opts.Labels, ","))
	}
	if len(opts.ExcludeLabels) > 0 {
		q.Set("exclude_label", strings.Join(opts.ExcludeLabels, ","))
	}
	path := "/v1/escalations"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}

	var resp struct {
		Escalations []EscalationDetail `json:"escalations"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing escalations: %w", err)
	}
	return resp.Escalations, nil
}

// ApproveEscalationRequest is the body for POST /v1/escalations/{id}/approve.
type ApproveEscalationRequest struct {
	ApprovedBy     string `json:"approved_by"`
	ApprovalMethod string `json:"approval_method,omitempty"`
	// GrantUntil is the approver-chosen expires_at for a `grant`-type
	// escalation (ADR docs/adr/taints.md §9.4/§10.4). Nil defers to the
	// server's default (7 days from approval time). Ignored for every other
	// operation_type.
	GrantUntil *time.Time `json:"grant_until,omitempty"`
	// ExecutionTarget is the approver's executor override ("local" or
	// "service_account"), recorded by the daemon with the approval.
	ExecutionTarget string `json:"execution_target,omitempty"`
	// Comment is the approver's optional note, recorded by the daemon as a
	// comment on the escalation and as approval_comment (gd-yuOG6cX6VI).
	Comment string `json:"comment,omitempty"`
}

// ApproveEscalation approves an escalation, triggering the daemon's auto-replay
// of the denied operation (POST /v1/escalations/{id}/approve). Operator-only on
// the server (RoleOperator): an agent token gets 403 by design — escalations are
// resolved by operators, and a crew agent that needs a work-blocking answer
// should file a `gb decision` instead (ka-0000007156).
func (c *HTTPClient) ApproveEscalation(ctx context.Context, id string, req ApproveEscalationRequest) error {
	path := "/v1/escalations/" + url.PathEscape(id) + "/approve"
	if err := c.doJSON(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("approving escalation %s: %w", id, err)
	}
	return nil
}

// RejectEscalationRequest is the body for POST /v1/escalations/{id}/reject.
type RejectEscalationRequest struct {
	RejectedBy string `json:"rejected_by"`
	Reason     string `json:"reason,omitempty"`
}

// RejectEscalation rejects an escalation, closing it as result_status=failure
// with no side-effect replay (POST /v1/escalations/{id}/reject). Operator-only
// on the server (RoleOperator).
func (c *HTTPClient) RejectEscalation(ctx context.Context, id string, req RejectEscalationRequest) error {
	path := "/v1/escalations/" + url.PathEscape(id) + "/reject"
	if err := c.doJSON(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("rejecting escalation %s: %w", id, err)
	}
	return nil
}

// ClaimEscalation claims an escalation the caller's held_by lane holds
// (POST /v1/escalations/{id}/claim). Status is unchanged.
func (c *HTTPClient) ClaimEscalation(ctx context.Context, id string) (*model.Bead, error) {
	var b model.Bead
	path := "/v1/escalations/" + url.PathEscape(id) + "/claim"
	if err := c.doJSON(ctx, http.MethodPost, path, struct{}{}, &b); err != nil {
		return nil, fmt.Errorf("claiming escalation %s: %w", id, err)
	}
	return &b, nil
}

// HandUpEscalationRequest is the body for POST /v1/escalations/{id}/hand-up.
// OperatorClass ("1"-"3") is the merge desk's OPERATOR-ONLY class on a PR
// merge, which puts it on the owner floor.
type HandUpEscalationRequest struct {
	Reason        string `json:"reason"`
	OperatorClass string `json:"operator_class,omitempty"`
}

// HandUpEscalation hands an escalation up one level with a reason.
func (c *HTTPClient) HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*model.Bead, error) {
	var b model.Bead
	path := "/v1/escalations/" + url.PathEscape(id) + "/hand-up"
	if err := c.doJSON(ctx, http.MethodPost, path, req, &b); err != nil {
		return nil, fmt.Errorf("handing up escalation %s: %w", id, err)
	}
	return &b, nil
}

// CaptureEscalationResultRequest is the body for POST /v1/escalations/{id}/result.
type CaptureEscalationResultRequest struct {
	Executor        string `json:"executor,omitempty"`
	ExecutionTarget string `json:"execution_target,omitempty"`
	Result          any    `json:"result,omitempty"`
	ResultStatus    string `json:"result_status"`
}

// CaptureEscalationResult records an externally-run execution result and closes
// the escalation (POST /v1/escalations/{id}/result). Used by `kd escalation
// resolve` to record a result the operator obtained out of band. Operator-only
// on the server (RoleOperator).
func (c *HTTPClient) CaptureEscalationResult(ctx context.Context, id string, req CaptureEscalationResultRequest) error {
	path := "/v1/escalations/" + url.PathEscape(id) + "/result"
	if err := c.doJSON(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("capturing escalation result %s: %w", id, err)
	}
	return nil
}
