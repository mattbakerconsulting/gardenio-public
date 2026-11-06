package bridge

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// EscalationAPIClient is the subset of beadsapi.Client used by the escalation web API.
type EscalationAPIClient interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}

// UserResolver looks up a user bead from an email address.
type UserResolver interface {
	LookupByEmail(ctx context.Context, email string) *UserIdentity
}

// SlackRefLookup returns the Slack MessageRef stored for an escalation bead,
// if one exists. Used by E2E tests to retrieve the exact (channel, ts,
// thread_ts) the bridge posted to, without having to scan channel history.
type SlackRefLookup func(beadID string) (MessageRef, bool)

// EscalationAPI serves the escalation approval web UI API endpoints.
type EscalationAPI struct {
	client   EscalationAPIClient
	users    UserResolver
	slackRef SlackRefLookup
	logger   *slog.Logger
}

// NewEscalationAPI creates the escalation API handler.
func NewEscalationAPI(client EscalationAPIClient, users UserResolver, logger *slog.Logger) *EscalationAPI {
	return &EscalationAPI{client: client, users: users, logger: logger}
}

// SetUserDirectory sets the user directory for authorization checks.
// Called after bot init when the UserDirectory becomes available.
func (a *EscalationAPI) SetUserDirectory(users *UserDirectory) {
	a.users = users
}

// SetSlackRefLookup wires a lookup function that returns the Slack message
// reference stored by the bridge for an escalation bead. Wired by main.go
// to the StateManager's GetDecisionMessage so external callers (tests, tools)
// can locate the posted message deterministically.
func (a *EscalationAPI) SetSlackRefLookup(f SlackRefLookup) {
	a.slackRef = f
}

// RegisterRoutes registers escalation API routes on the given mux.
func (a *EscalationAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/escalations", a.handleList)
	mux.HandleFunc("/api/escalations/", a.handleByID)
	mux.HandleFunc("/api/escalations/me", a.handleMe)
}

// handleMe returns the authenticated user's identity and capabilities.
func (a *EscalationAPI) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Only the identity the OIDC middleware verified: the raw
	// X-Auth-Request-Email header is the caller's to set (gd-Fhizmbk9Ee).
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		writeJSONError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	user := a.users.LookupByEmail(r.Context(), email)
	if user == nil {
		writeJSONError(w, http.StatusForbidden,
			"user not found in gardenio directory; contact an admin to create your user bead")
		return
	}

	writeJSONResponse(w, map[string]any{
		"bead_id":      user.BeadID,
		"email":        user.Email,
		"display_name": user.DisplayName,
		"capabilities": user.Capabilities,
	})
}

// handleList handles GET /api/escalations?status=open.
func (a *EscalationAPI) handleList(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/escalations" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	status := r.URL.Query().Get("status")
	statuses := []string{"open", "in_progress"}
	if status != "" {
		statuses = strings.Split(status, ",")
	}

	result, err := a.client.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Types:    []string{"escalation"},
		Statuses: statuses,
		Sort:     "priority",
		Limit:    50,
	})
	if err != nil {
		a.logger.Error("failed to list escalations", "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to list escalations")
		return
	}

	writeJSONResponse(w, map[string]any{"escalations": result.Beads})
}

// handleByID routes /api/escalations/{id} and /api/escalations/{id}/{action}.
func (a *EscalationAPI) handleByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/escalations/")
	if path == "me" {
		a.handleMe(w, r)
		return
	}

	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "missing escalation ID", http.StatusBadRequest)
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
	case "approve", "reject":
		// P7 Task 4: the bridge web API used to approve by PATCHing
		// approved_by, which the bridge then executed. Approval happens in
		// Slack (as the mapped person) or in the beads viewer.
		writeJSONError(w, http.StatusGone, "approve and reject moved: use the Slack card or the beads viewer")
	case "slack-ref":
		a.handleSlackRef(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// handleSlackRef handles GET /api/escalations/{id}/slack-ref.
// Returns the Slack MessageRef the bridge stored when it posted the
// escalation notification: {channel, ts, thread_ts}. Used by the E2E test
// to fetch the exact posted message via conversations.replies without
// scanning channel history (channels can be noisy enough that a 1000-message
// window does not reach long-lived agent card parents).
func (a *EscalationAPI) handleSlackRef(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if a.slackRef == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "slack ref lookup not configured")
		return
	}
	ref, ok := a.slackRef(id)
	if !ok {
		writeJSONError(w, http.StatusNotFound, "no slack message recorded for this escalation")
		return
	}
	writeJSONResponse(w, map[string]string{
		"channel":   ref.ChannelID,
		"ts":        ref.Timestamp,
		"thread_ts": ref.ThreadTS,
	})
}

// handleShow handles GET /api/escalations/{id}.
func (a *EscalationAPI) handleShow(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bead, err := a.client.GetBead(r.Context(), id)
	if err != nil {
		a.logger.Error("failed to get escalation", "id", id, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to get escalation")
		return
	}

	if bead.Type != "escalation" {
		writeJSONError(w, http.StatusNotFound, "not an escalation bead")
		return
	}

	writeJSONResponse(w, bead)
}
