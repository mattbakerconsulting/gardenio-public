package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// DecisionAPIClient is the subset of beadsapi.Client used by the decisions web API.
type DecisionAPIClient interface {
	ListDecisions(ctx context.Context, status string, limit int) ([]beadsapi.DecisionDetail, error)
	GetDecision(ctx context.Context, decisionID string) (*beadsapi.DecisionDetail, error)
	ResolveDecision(ctx context.Context, decisionID string, req beadsapi.ResolveDecisionRequest) error
	CancelDecision(ctx context.Context, decisionID, reason, canceledBy string) error
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// DecisionAPI serves the decisions web UI API endpoints.
type DecisionAPI struct {
	client DecisionAPIClient
	logger *slog.Logger
}

// NewDecisionAPI creates the decisions API handler.
func NewDecisionAPI(client DecisionAPIClient, logger *slog.Logger) *DecisionAPI {
	return &DecisionAPI{client: client, logger: logger}
}

// RegisterRoutes registers decision API routes on the given mux.
func (a *DecisionAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/decisions", a.handleList)
	mux.HandleFunc("/api/decisions/", a.handleByID)
}

// handleList handles GET /api/decisions.
func (a *DecisionAPI) handleList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := r.URL.Query().Get("status")
	decisions, err := a.client.ListDecisions(r.Context(), status, 50)
	if err != nil {
		a.logger.Error("failed to list decisions", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to list decisions")
		return
	}

	writeJSONResponse(w, map[string]any{"decisions": decisions})
}

// handleByID routes /api/decisions/{id} and /api/decisions/{id}/{action}.
func (a *DecisionAPI) handleByID(w http.ResponseWriter, r *http.Request) {
	// Parse path: /api/decisions/{id}[/{action}]
	path := strings.TrimPrefix(r.URL.Path, "/api/decisions/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing decision ID", http.StatusBadRequest)
		return
	}

	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		a.handleShow(w, r, id)
	case "resolve":
		a.handleResolve(w, r, id)
	case "dismiss":
		a.handleDismiss(w, r, id)
	case "promote":
		a.handlePromote(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// handleShow handles GET /api/decisions/{id}.
func (a *DecisionAPI) handleShow(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	detail, err := a.client.GetDecision(r.Context(), id)
	if err != nil {
		a.logger.Error("failed to get decision", "id", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to get decision")
		return
	}

	writeJSONResponse(w, detail)
}

// actAsCaller returns the context a decision write runs on and the caller's
// operator id. Every daemon call on that context goes out on the caller's own
// verified bearer, never the bridge's token, so the daemon authorizes the
// write against that person's roles and projects and records them as its
// actor. Who acted is never read from the request body (gd-Fhizmbk9Ee). ok is
// false, and a 401 has been written, when the request carries no verified
// identity and bearer.
func actAsCaller(w http.ResponseWriter, r *http.Request) (ctx context.Context, operator string, ok bool) {
	email := oidcauth.EmailFromContext(r.Context())
	bearer := oidcauth.AccessTokenFromRequest(r)
	if email == "" || bearer == "" {
		writeJSONError(w, http.StatusUnauthorized, "sign in to act on decisions")
		return nil, "", false
	}
	operator = "oidc:" + email
	ctx = beadsapi.WithOperator(r.Context(), operator)
	return beadsapi.WithUserBearerOnly(ctx, bearer), operator, true
}

// resolveRequest is the JSON body for POST /api/decisions/{id}/resolve.
type resolveRequest struct {
	Chosen    string `json:"chosen"`
	Rationale string `json:"rationale"`
}

// handleResolve handles POST /api/decisions/{id}/resolve.
func (a *DecisionAPI) handleResolve(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Chosen == "" {
		writeJSONError(w, http.StatusBadRequest, "chosen is required")
		return
	}

	ctx, operator, ok := actAsCaller(w, r)
	if !ok {
		return
	}

	err := a.client.ResolveDecision(ctx, id, beadsapi.ResolveDecisionRequest{
		SelectedOption: req.Chosen,
		ResponseText:   req.Rationale,
		RespondedBy:    operator,
	})
	if err != nil {
		a.logger.Error("failed to resolve decision", "id", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to resolve decision")
		return
	}

	a.logger.Info("decision resolved via web UI", "id", id, "chosen", req.Chosen, "by", operator)
	writeJSONResponse(w, map[string]string{"status": "resolved"})
}

// dismissRequest is the JSON body for POST /api/decisions/{id}/dismiss.
type dismissRequest struct {
	Reason string `json:"reason"`
}

// handleDismiss handles POST /api/decisions/{id}/dismiss.
func (a *DecisionAPI) handleDismiss(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req dismissRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, operator, ok := actAsCaller(w, r)
	if !ok {
		return
	}

	err := a.client.CancelDecision(ctx, id, req.Reason, operator)
	if err != nil {
		a.logger.Error("failed to dismiss decision", "id", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to dismiss decision")
		return
	}

	a.logger.Info("decision dismissed via web UI", "id", id, "reason", req.Reason, "by", operator)
	writeJSONResponse(w, map[string]string{"status": "dismissed"})
}

// promoteRequest is the JSON body for POST /api/decisions/{id}/promote.
type promoteRequest struct {
	OperationType   string `json:"operationType"`
	ExecutionTarget string `json:"executionTarget"`
}

// handlePromote handles POST /api/decisions/{id}/promote.
// It creates a new type:escalation bead from the decision, links them, and
// closes the decision as promoted.
func (a *DecisionAPI) handlePromote(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req promoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, operator, ok := actAsCaller(w, r)
	if !ok {
		return
	}

	escalationID, err := PromoteDecisionToEscalation(ctx, a.client, id, req.OperationType, req.ExecutionTarget, operator)
	if err != nil {
		a.logger.Error("failed to promote decision", "id", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, err.Error())
		return
	}

	a.logger.Info("decision promoted to escalation via web UI",
		"decision", id, "escalation", escalationID, "by", operator)
	writeJSONResponse(w, map[string]string{
		"status":       "promoted",
		"escalationId": escalationID,
	})
}

// PromotionClient is the interface needed to promote a decision to an escalation.
type PromotionClient interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// PromoteDecisionToEscalation creates a type:escalation bead from a decision,
// links them via a "promoted" dependency, and closes the decision. Returns the
// new escalation bead ID.
func PromoteDecisionToEscalation(ctx context.Context, client PromotionClient, decisionID, operationType, executionTarget, promotedBy string) (string, error) {
	decision, err := client.GetBead(ctx, decisionID)
	if err != nil {
		return "", fmt.Errorf("fetch decision: %w", err)
	}
	if decision.Status == "closed" {
		return "", fmt.Errorf("decision %s is already closed", decisionID)
	}

	// Build escalation fields from the decision.
	question := decision.Fields["prompt"]
	if question == "" {
		question = decision.Fields["question"]
	}

	opType := operationType
	if opType == "" {
		opType = "general"
	}

	// Build the operation_spec from the decision's options and context.
	opSpec := map[string]any{
		"original_prompt": question,
	}
	if opts := decision.Fields["options"]; opts != "" {
		opSpec["original_options"] = json.RawMessage(opts)
	}
	opSpecJSON, _ := json.Marshal(opSpec)

	escalationFields := map[string]string{
		"operation_type":   opType,
		"operation_spec":   string(opSpecJSON),
		"context":          decision.Fields["context"],
		"requesting_agent": decision.Fields["requesting_agent_bead_id"],
		"requesting_task":  decision.Fields["session"],
	}
	if executionTarget != "" {
		escalationFields["execution_target"] = executionTarget
	}

	// Filter out empty values.
	fieldsJSON := make(map[string]string)
	for k, v := range escalationFields {
		if v != "" {
			fieldsJSON[k] = v
		}
	}

	rawFields, _ := json.Marshal(fieldsJSON)

	// Propagate project labels from the decision.
	var labels []string
	for _, l := range decision.Labels {
		if len(l) > 0 {
			labels = append(labels, l)
		}
	}

	escalationID, err := client.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       fmt.Sprintf("Escalation: %s", truncate(question, 80)),
		Type:        "escalation",
		Description: fmt.Sprintf("Promoted from decision %s.\n\n%s", decisionID, question),
		Labels:      labels,
		Priority:    decision.Priority,
		Fields:      rawFields,
	})
	if err != nil {
		return "", fmt.Errorf("create escalation bead: %w", err)
	}

	// Link escalation → decision with "promoted" dependency type.
	if err := client.AddDependency(ctx, escalationID, decisionID, "promoted", promotedBy); err != nil {
		// Non-fatal: the escalation was created, just the link failed.
		// Log but don't fail the whole operation.
		_ = err
	}

	// Close the decision as promoted.
	closeFields := map[string]string{
		"chosen":    "promoted_to_escalation",
		"rationale": fmt.Sprintf("Promoted to escalation %s by %s", escalationID, promotedBy),
	}
	if err := client.CloseBead(ctx, decisionID, closeFields); err != nil {
		// Non-fatal: the escalation was created, just the close failed.
		_ = err
	}

	return escalationID, nil
}

// writeJSONResponse writes a JSON response with status 200.
func writeJSONResponse(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a JSON error response.
func writeJSONError(w http.ResponseWriter, code int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
