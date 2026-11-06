package bridge

// bot_agent_feedback.go — Always-on agent-feedback write path.
//
// This endpoint emits a stable, queryable feedback bead (`type=rating`,
// `feedback` field) DECOUPLED from the dark human-survey UX. The legacy
// "Add feedback" Slack modal (bot_human_survey_manual.go) writes the same
// shape, but only fires when human-survey-config.enabled flips it on — so
// the feedback bead is rarely written today. The feedback→bug trigger
// (ka-0000007151) needs a reliable source, so this path writes the bead
// regardless of the survey config and without any Slack dependency.
//
// Reuses type=rating (not a new type) so the trigger condition stays
// simple: `bead_type == rating AND feedback present`. A dedicated
// `kind:slack-feedback` label is added so the trigger (and any reader)
// can distinguish always-on feedback beads from modal star-ratings.
//
// The daemon enforces the (rater, rated_bead_id) idempotent upsert and
// auto-wires the `rating --rates--> agent` dep edge (preCreateRating in
// kbeads), so this handler is a thin CreateBead — repeated feedback from
// the same rater on the same agent updates the bead in place.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// agentFeedbackLabel marks always-on feedback beads so the feedback→bug
// trigger can scope its match away from modal star-rating beads.
const agentFeedbackLabel = "kind:slack-feedback"

// agentFeedbackSource is the `source` field stamped on always-on feedback
// beads. Reuses the slack-feedback-only sentinel so the server's score=0
// guard (preCreateRating) accepts the feedback-only row.
const agentFeedbackSource = "slack-feedback-only"

// agentFeedbackAnonRater is the fallback `rater` when the caller supplies
// no identity. The schema requires a non-empty rater; this keeps the bead
// writable for anonymous submissions. Anonymous feedbacks for the same
// agent bead collapse to one row via the (rater, rated_bead_id) upsert.
const agentFeedbackAnonRater = "fleet:slack-bridge"

// AgentFeedbackAPIClient is the subset of BeadClient this API needs. It
// deliberately excludes any Slack client so the write path works in
// Socket Mode, webhook mode, and no-Slack mode alike.
type AgentFeedbackAPIClient interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
}

// AgentFeedbackAPI serves POST /api/slack/agent-feedback — the always-on
// feedback write path.
type AgentFeedbackAPI struct {
	daemon AgentFeedbackAPIClient
	logger *slog.Logger
}

// NewAgentFeedbackAPI creates a new AgentFeedbackAPI handler.
func NewAgentFeedbackAPI(daemon AgentFeedbackAPIClient, logger *slog.Logger) *AgentFeedbackAPI {
	return &AgentFeedbackAPI{daemon: daemon, logger: logger}
}

// RegisterRoutes registers the agent-feedback route on the given mux. The
// path lives under /api/slack/ so bridgeRouteRole classifies it as an
// agent-facing endpoint (shared bearer token or OIDC admin fallback).
func (a *AgentFeedbackAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/agent-feedback", a.handlePostFeedback)
}

// agentFeedbackRequest is the JSON body for POST /api/slack/agent-feedback.
type agentFeedbackRequest struct {
	// AgentBeadID is the source agent bead the feedback is about. Required.
	AgentBeadID string `json:"agent_bead_id"`
	// Feedback is the freeform feedback text. Required.
	Feedback string `json:"feedback"`
	// Rater is an explicit caller identity (e.g. oidc:user@example.com).
	// Optional — when empty the handler derives it from SlackUserID (a
	// `slack:<id>` sentinel) or falls back to the anonymous rater.
	Rater string `json:"rater"`
	// Provenance (all optional).
	SlackUserID   string `json:"slack_user_id"`
	SlackChannel  string `json:"slack_channel"`
	SlackThreadTS string `json:"slack_thread_ts"`
}

// agentFeedbackResponse is the bridge's reply to the caller.
type agentFeedbackResponse struct {
	OK     bool   `json:"ok"`
	BeadID string `json:"bead_id,omitempty"`
}

// handlePostFeedback writes (or upserts) a feedback bead for the given
// agent bead. Always on — never gated by human-survey-config.enabled.
func (a *AgentFeedbackAPI) handlePostFeedback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}
	var req agentFeedbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	req.AgentBeadID = strings.TrimSpace(req.AgentBeadID)
	rawFeedback := strings.TrimSpace(req.Feedback)
	if req.AgentBeadID == "" {
		http.Error(w, `{"error":"agent_bead_id is required"}`, http.StatusBadRequest)
		return
	}
	if rawFeedback == "" {
		http.Error(w, `{"error":"feedback is required"}`, http.StatusBadRequest)
		return
	}

	agentBead, err := a.daemon.GetBead(r.Context(), req.AgentBeadID)
	if err != nil || agentBead == nil {
		a.logger.Warn("agent-feedback: source bead fetch failed",
			"agent_bead_id", req.AgentBeadID, "error", err)
		http.Error(w, `{"error":"agent_bead_id not found"}`, http.StatusNotFound)
		return
	}

	beadID, err := a.writeFeedbackBead(r.Context(), req, agentBead, rawFeedback)
	if err != nil {
		a.logger.Error("agent-feedback: write failed",
			"agent_bead_id", req.AgentBeadID, "error", err)
		http.Error(w, `{"error":"failed to write feedback bead"}`, http.StatusInternalServerError)
		return
	}

	a.logger.Info("agent-feedback: recorded",
		"agent_bead_id", req.AgentBeadID, "bead", beadID, "rater", resolveFeedbackRater(req))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(agentFeedbackResponse{OK: true, BeadID: beadID})
}

// writeFeedbackBead creates the feedback rating bead. The daemon enforces
// the (rater, rated_bead_id) idempotent upsert and auto-wires the `rates`
// dep edge, so this is a single CreateBead.
func (a *AgentFeedbackAPI) writeFeedbackBead(
	ctx context.Context,
	req agentFeedbackRequest,
	agentBead *beadsapi.BeadDetail,
	rawFeedback string,
) (string, error) {
	// XSS: escape HTML entities so Slack special syntax (<!channel>,
	// <@U123>, <#C456|name>, <http://…>) in user-submitted feedback is
	// never interpreted when the feedback is rendered back into Slack.
	escaped := slackEscapeMrkdwn(rawFeedback)
	rater := resolveFeedbackRater(req)

	fields := map[string]any{
		"rated_bead_id":   req.AgentBeadID,
		"rater":           rater,
		"slack_user_id":   req.SlackUserID,
		"slack_channel":   req.SlackChannel,
		"slack_thread_ts": req.SlackThreadTS,
		"source":          agentFeedbackSource,
		"score":           0,
		"feedback":        escaped,
		"feedback_raw":    rawFeedback,
		"answered_at":     time.Now().UTC().Format(time.RFC3339),
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}

	labels := []string{
		humanSurveyAgentLabelPrefix + req.AgentBeadID,
		agentFeedbackLabel,
	}
	if project := agentBead.Fields["project"]; project != "" {
		labels = append(labels, "project:"+project)
	}

	return a.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     "Feedback: " + req.AgentBeadID,
		Type:      ratingBeadType,
		Labels:    labels,
		CreatedBy: "slack-bridge",
		Fields:    fieldsJSON,
	})
}

// resolveFeedbackRater derives the `rater` identity. Precedence:
//  1. explicit req.Rater (e.g. oidc:user@example.com from an authenticated
//     viewer surface),
//  2. `slack:<id>` sentinel when a Slack user id is supplied,
//  3. the anonymous fallback so the bead is always writable.
func resolveFeedbackRater(req agentFeedbackRequest) string {
	if r := strings.TrimSpace(req.Rater); r != "" {
		return r
	}
	if u := strings.TrimSpace(req.SlackUserID); u != "" {
		return slackRaterSentinelPrefix + u
	}
	return agentFeedbackAnonRater
}
