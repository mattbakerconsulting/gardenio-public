package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// EscalationDetail is the response from GET /v1/escalations/{id}.
type EscalationDetail struct {
	Escalation *BeadDetail `json:"escalation"`
	Issue      *BeadDetail `json:"issue,omitempty"`
}

// escalationDetailJSON is the raw JSON representation.
type escalationDetailJSON struct {
	Escalation *beadJSON `json:"escalation"`
	Issue      *beadJSON `json:"issue,omitempty"`
}

func (d *escalationDetailJSON) toDetail() *EscalationDetail {
	ed := &EscalationDetail{}
	if d.Escalation != nil && d.Escalation.ID != "" {
		ed.Escalation = d.Escalation.toDetail()
	}
	if d.Issue != nil {
		ed.Issue = d.Issue.toDetail()
	}
	// kbeads' escalation list/get responses put ONLY the extracted
	// escalation-specific fields (operation_type, approved_by, …) under
	// `escalation` — with no bead id/status/created_by — and the full bead
	// under `issue`. So the `d.Escalation.ID != ""` guard above is never
	// satisfied for real responses, leaving ed.Escalation nil. Backfill it
	// from the full issue bead so every consumer (ask-gate handle checks,
	// escalation executor, …) sees a populated bead instead of nil. The
	// issue bead carries the escalation fields in its Fields map, so prompt
	// builders that read operation_type/approved_by keep working.
	if ed.Escalation == nil && ed.Issue != nil {
		ed.Escalation = ed.Issue
	}
	return ed
}

// GetEscalation fetches an escalation by ID.
func (c *Client) GetEscalation(ctx context.Context, escalationID string) (*EscalationDetail, error) {
	var resp escalationDetailJSON
	path := "/v1/escalations/" + url.PathEscape(escalationID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting escalation %s: %w", escalationID, err)
	}
	return resp.toDetail(), nil
}

// LanePresence is held_by_presence on GET /v1/escalations/{id}: whether the
// presence-tracked coordinator lane that holds the escalation is online
// (gd-t41kYFbLUt). LastSeenAt is "" when the lane was never seen.
type LanePresence struct {
	Lane       string `json:"lane"`
	Client     string `json:"client"`
	Online     bool   `json:"online"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	Window     string `json:"window"`
}

// EscalationHolderPresence returns the presence of escalationID's holder, nil
// when its holder is not a presence-tracked lane.
func (c *Client) EscalationHolderPresence(ctx context.Context, escalationID string) (*LanePresence, error) {
	var resp struct {
		Escalation struct {
			Presence *LanePresence `json:"held_by_presence"`
		} `json:"escalation"`
	}
	path := "/v1/escalations/" + url.PathEscape(escalationID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting escalation %s presence: %w", escalationID, err)
	}
	return resp.Escalation.Presence, nil
}

// PermissionDiffRow is one rule of a privileged spawn's ServiceAccount, as
// approved and as live: a resource rule (APIGroup, Resource, optionally
// ResourceNames) or a non-resource URL. AddedVerbs are live verbs no approved
// rule covers.
type PermissionDiffRow struct {
	APIGroup       string   `json:"api_group,omitempty"`
	Resource       string   `json:"resource,omitempty"`
	ResourceNames  []string `json:"resource_names,omitempty"`
	NonResourceURL string   `json:"non_resource_url,omitempty"`
	ApprovedVerbs  []string `json:"approved_verbs,omitempty"`
	LiveVerbs      []string `json:"live_verbs,omitempty"`
	AddedVerbs     []string `json:"added_verbs,omitempty"`
}

// NamespacePermissionDiff is one namespace's comparison. State is
// "unchanged", "widened" or "unknown"; Reason says why it is unknown.
type NamespacePermissionDiff struct {
	Namespace string              `json:"namespace"`
	State     string              `json:"state"`
	Reason    string              `json:"reason,omitempty"`
	Rows      []PermissionDiffRow `json:"rows,omitempty"`
}

// SpawnPermissionDiff is the `diff` payload of
// GET /v1/escalations/{id}/permission-diff (gd-SyEYkqe601).
type SpawnPermissionDiff struct {
	SnapshotAt     string                    `json:"snapshot_at"`
	ServiceAccount string                    `json:"service_account"`
	Widened        bool                      `json:"widened"`
	Unknown        bool                      `json:"unknown"`
	Namespaces     []NamespacePermissionDiff `json:"namespaces"`
}

// GetEscalationPermissionDiff fetches the live-vs-approved ServiceAccount
// permission diff of a privileged spawn escalation. ok is false when the
// escalation stored no snapshot (the daemon's `available: false`).
func (c *Client) GetEscalationPermissionDiff(ctx context.Context, escalationID string) (diff *SpawnPermissionDiff, ok bool, err error) {
	var resp struct {
		Available bool                 `json:"available"`
		Diff      *SpawnPermissionDiff `json:"diff,omitempty"`
	}
	path := "/v1/escalations/" + url.PathEscape(escalationID) + "/permission-diff"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, false, fmt.Errorf("getting permission diff for escalation %s: %w", escalationID, err)
	}
	if !resp.Available || resp.Diff == nil {
		return nil, false, nil
	}
	return resp.Diff, true, nil
}

// ListEscalations lists escalations with optional status/limit filters.
func (c *Client) ListEscalations(ctx context.Context, status string, limit int) ([]EscalationDetail, error) {
	q := url.Values{}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", limit))
	}
	path := "/v1/escalations"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}

	var resp struct {
		Escalations []escalationDetailJSON `json:"escalations"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing escalations: %w", err)
	}
	result := make([]EscalationDetail, len(resp.Escalations))
	for i := range resp.Escalations {
		result[i] = *resp.Escalations[i].toDetail()
	}
	return result, nil
}

// ApproveEscalationRequest is the request body for POST /v1/escalations/{id}/approve.
type ApproveEscalationRequest struct {
	ApprovedBy     string `json:"approved_by"`
	ApprovalMethod string `json:"approval_method,omitempty"`
	// ExecutionTarget is the approver's executor override ("local" or
	// "service_account"), recorded by the daemon with the approval.
	ExecutionTarget string `json:"execution_target,omitempty"`
	// Comment is the approver's optional note ("approve with comment"): the
	// daemon records it as a comment on the escalation by the approver and as
	// approval_comment (gd-yuOG6cX6VI).
	Comment string `json:"comment,omitempty"`
}

// ApproveEscalation approves an escalation by ID.
//
// When ctx carries a per-request user bearer (WithUserBearer), it is forced
// onto the wire so the daemon authenticates the call AS THE CALLING HUMAN
// directly — this is the path beads-viewer's web UI takes (operatorMiddleware
// always attaches the caller's OIDC bearer when the edge forwards one), and
// it is the preferred path: escalation approval is a privileged,
// security-sensitive gate, and the daemon's escalationApprover
// (kbeads/internal/server/escalation_approval_authz.go) only trusts a direct
// OIDC/edge identity to mean "this specific human approved this" — a
// relay-based approval only ever meant "someone holding the shared static
// token claims a human did" (gd-J8Fet2YH6y).
//
// Callers with no forwardable user bearer (the `gb escalation approve` CLI,
// which authenticates headless agents/service identities with no human OIDC
// token, and the Slack bridge's escalation web API, which resolves the
// approver from the Slack-linked user bead rather than a browser session)
// fall back to the client's usual static-token + X-Operated-By relay — mirrors
// ListTaintScrubRequests.
func (c *Client) ApproveEscalation(ctx context.Context, escalationID string, req ApproveEscalationRequest) error {
	path := "/v1/escalations/" + url.PathEscape(escalationID) + "/approve"
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, req, nil)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, req, nil)
	}
	if err != nil {
		return fmt.Errorf("approving escalation %s: %w", escalationID, err)
	}
	return nil
}

// RejectEscalationRequest is the request body for POST /v1/escalations/{id}/reject.
type RejectEscalationRequest struct {
	RejectedBy string `json:"rejected_by"`
	Reason     string `json:"reason,omitempty"`
}

// RejectEscalation rejects an escalation by ID. See ApproveEscalation's doc
// comment for the user-bearer-first, relay-fallback authentication strategy.
func (c *Client) RejectEscalation(ctx context.Context, escalationID string, req RejectEscalationRequest) error {
	path := "/v1/escalations/" + url.PathEscape(escalationID) + "/reject"
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, req, nil)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, req, nil)
	}
	if err != nil {
		return fmt.Errorf("rejecting escalation %s: %w", escalationID, err)
	}
	return nil
}

// RouteEscalationRequest is the request body for POST /v1/escalations/{id}/audience.
// An empty Audience asks the daemon to stamp the audience it derives (the
// forced-owner rule, else a valid stored value, else the operation_type
// default), which is what backfill-audience sends.
type RouteEscalationRequest struct {
	Audience string `json:"audience,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// RouteEscalation re-routes an escalation (sets its audience) and returns the
// audience the daemon stored. Only a gardenio-operator or admin on their own
// sign-in may; the daemon refuses a generic field write of audience from
// anyone, and records the change as a comment. Authenticates like
// ApproveEscalation: the user's bearer when the context carries one.
func (c *Client) RouteEscalation(ctx context.Context, escalationID string, req RouteEscalationRequest) (string, error) {
	path := "/v1/escalations/" + url.PathEscape(escalationID) + "/audience"
	var out struct {
		Escalation struct {
			Audience string `json:"audience"`
		} `json:"escalation"`
	}
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, req, &out)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, req, &out)
	}
	if err != nil {
		return "", fmt.Errorf("re-routing escalation %s: %w", escalationID, err)
	}
	return out.Escalation.Audience, nil
}

// CaptureEscalationResultRequest is the request body for POST /v1/escalations/{id}/result.
type CaptureEscalationResultRequest struct {
	Executor        string `json:"executor,omitempty"`
	ExecutionTarget string `json:"execution_target,omitempty"`
	Result          any    `json:"result,omitempty"`
	ResultStatus    string `json:"result_status"`
	// Fields are extra top-level bead fields merged atomically with the result
	// record + close (the typed fields above win on collision). The auto-reaper
	// uses this to land its render/audit trail (approved_by, reaped*, the MR
	// witness) in the same operation as the close — see the daemon-side
	// captureResultRequest.Fields doc for why atomicity matters.
	Fields map[string]string `json:"fields,omitempty"`
	// KeepRequestingTask withdraws the escalation's close_requesting_task
	// opt-in in the same write as the close, so the daemon leaves the
	// requesting task open (gd-MgicCcY2rj).
	KeepRequestingTask bool `json:"keep_requesting_task,omitempty"`
}

// CaptureEscalationResult records the execution result on an escalation.
func (c *Client) CaptureEscalationResult(ctx context.Context, escalationID string, req CaptureEscalationResultRequest) error {
	path := "/v1/escalations/" + url.PathEscape(escalationID) + "/result"
	if err := c.doJSON(ctx, http.MethodPost, path, req, nil); err != nil {
		return fmt.Errorf("capturing escalation result %s: %w", escalationID, err)
	}
	return nil
}

// EscalationDeployment is the daemon's note on the deployment a helm_deploy
// escalation's close recorded: Release is set when it was recorded, Error when
// it was not.
type EscalationDeployment struct {
	Project  string `json:"project"`
	Tag      string `json:"tag"`
	Target   string `json:"target"`
	Revision string `json:"revision,omitempty"`
	Release  string `json:"release,omitempty"`
	Error    string `json:"error,omitempty"`
}

// CaptureEscalationResultDeployment is CaptureEscalationResult, returning the
// deployment the close recorded (nil when it was not a helm_deploy closed with
// a tag).
func (c *Client) CaptureEscalationResultDeployment(ctx context.Context, escalationID string, req CaptureEscalationResultRequest) (*EscalationDeployment, error) {
	path := "/v1/escalations/" + url.PathEscape(escalationID) + "/result"
	var resp struct {
		Deployment *EscalationDeployment `json:"deployment"`
	}
	if err := c.doJSON(ctx, http.MethodPost, path, req, &resp); err != nil {
		return nil, fmt.Errorf("capturing escalation result %s: %w", escalationID, err)
	}
	return resp.Deployment, nil
}

// ListEscalationsHeldBy lists escalations whose held_by is one of heldBy (an
// unstamped escalation counts as "operator"; "@me" means the caller's own
// lanes, resolved by the daemon). statuses defaults to open,in_progress on
// the daemon.
func (c *Client) ListEscalationsHeldBy(ctx context.Context, heldBy []string, statuses []string, limit int) ([]*BeadDetail, error) {
	if len(heldBy) == 0 {
		return nil, fmt.Errorf("listing escalations by held_by: no lane refs given")
	}
	q := url.Values{}
	q.Set("held_by", strings.Join(heldBy, ","))
	if len(statuses) > 0 {
		q.Set("status", strings.Join(statuses, ","))
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	var resp struct {
		Escalations []escalationDetailJSON `json:"escalations"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/v1/escalations?"+q.Encode(), nil, &resp); err != nil {
		return nil, fmt.Errorf("listing escalations held by %s: %w", strings.Join(heldBy, ","), err)
	}
	out := make([]*BeadDetail, 0, len(resp.Escalations))
	for i := range resp.Escalations {
		if d := resp.Escalations[i].toDetail(); d.Escalation != nil {
			out = append(out, d.Escalation)
		}
	}
	return out, nil
}

// CanApproveResult is the daemon's dry-run answer to "may the caller approve
// this escalation": the same checks the approve endpoint runs (lanes, the
// hold, operator_only, self-approval), with nothing written.
type CanApproveResult struct {
	Allowed bool   `json:"allowed"`
	Method  string `json:"method"`
	As      string `json:"as"`
	Reason  string `json:"reason"`
}

// CanApproveEscalation asks POST /v1/escalations/{id}/can-approve. Like
// ApproveEscalation it forces the per-request user bearer onto the wire when
// ctx carries one, so the daemon answers for the calling human; without one the
// static token answers, and that token is a member of no lane (allowed=false).
func (c *Client) CanApproveEscalation(ctx context.Context, id string) (CanApproveResult, error) {
	path := "/v1/escalations/" + url.PathEscape(id) + "/can-approve"
	var out CanApproveResult
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, struct{}{}, &out)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, struct{}{}, &out)
	}
	if err != nil {
		return CanApproveResult{}, fmt.Errorf("checking approval of escalation %s: %w", id, err)
	}
	return out, nil
}

// ClaimEscalation claims an escalation the caller's lane holds
// (POST /v1/escalations/{id}/claim). Status is unchanged.
func (c *Client) ClaimEscalation(ctx context.Context, id string) (*BeadDetail, error) {
	return c.postEscalationVerb(ctx, id, "claim", struct{}{})
}

// ClaimEscalationFor claims an escalation for agentID, an agent a trigger
// spawned (POST /v1/escalations/{id}/claim-for). Only the trigger engine's
// machine principal may call it (gd-1yeWlbJUJi).
func (c *Client) ClaimEscalationFor(ctx context.Context, id, agentID string) (*BeadDetail, error) {
	return c.postEscalationVerb(ctx, id, "claim-for", map[string]string{"agent": agentID})
}

// HandUpEscalationRequest is the body for POST /v1/escalations/{id}/hand-up.
// OperatorClass ("1"-"3") is the merge desk's OPERATOR-ONLY class on a PR
// merge, which puts it on the owner floor.
type HandUpEscalationRequest struct {
	Reason        string `json:"reason"`
	OperatorClass string `json:"operator_class,omitempty"`
}

// HandUpEscalation hands an escalation up one level (operator, or the land
// owner for a tenant land) with a reason.
func (c *Client) HandUpEscalation(ctx context.Context, id string, req HandUpEscalationRequest) (*BeadDetail, error) {
	return c.postEscalationVerb(ctx, id, "hand-up", req)
}

// RerouteEscalationsRequest is the body for POST /v1/escalations/reroute.
type RerouteEscalationsRequest struct {
	ApprovedUnexecuted bool `json:"approved_unexecuted"`
	DryRun             bool `json:"dry_run"`
}

// ReroutedEscalation is one escalation a reroute moved (or would move).
type ReroutedEscalation struct {
	ID        string `json:"id"`
	From      string `json:"from"`
	ExecuteBy string `json:"execute_by"`
	HeldBy    string `json:"held_by"`
}

// RerouteEscalationsResult is POST /v1/escalations/reroute's response.
type RerouteEscalationsResult struct {
	Considered int                  `json:"considered"`
	Rerouted   []ReroutedEscalation `json:"rerouted"`
	DryRun     bool                 `json:"dry_run"`
	Truncated  bool                 `json:"truncated"`
}

// RerouteEscalations re-targets the approved escalations an idle hand-up
// stranded on a coordinator executor (P7.1 backfill, admin only).
func (c *Client) RerouteEscalations(ctx context.Context, req RerouteEscalationsRequest) (RerouteEscalationsResult, error) {
	var out RerouteEscalationsResult
	if err := c.doJSON(ctx, http.MethodPost, "/v1/escalations/reroute", req, &out); err != nil {
		return RerouteEscalationsResult{}, fmt.Errorf("rerouting escalations: %w", err)
	}
	return out, nil
}

// postEscalationVerb forwards the per-request user bearer when present, like
// ApproveEscalation, so a human in the viewer acts as themselves.
func (c *Client) postEscalationVerb(ctx context.Context, id, verb string, body any) (*BeadDetail, error) {
	path := "/v1/escalations/" + url.PathEscape(id) + "/" + verb
	var b beadJSON
	var err error
	if bearer := UserBearerFromContext(ctx); bearer != "" {
		err = c.doJSONAsUser(ctx, http.MethodPost, path, bearer, body, &b)
	} else {
		err = c.doJSON(ctx, http.MethodPost, path, body, &b)
	}
	if err != nil {
		return nil, fmt.Errorf("%s escalation %s: %w", verb, id, err)
	}
	return b.toDetail(), nil
}
