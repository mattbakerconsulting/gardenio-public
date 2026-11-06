package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// pipelineSource is the author recorded on thread lines this handler emits.
const pipelineSource = "github-bridge"

// Pipeline status vocabulary stamped on mr_pipeline_status. It is the GitLab
// pipeline vocabulary (success/failed/canceled/skipped/manual), not GitHub's,
// because beads-viewer, the DM text and the trigger sources all key on it.
const (
	pipelineSuccess  = "success"
	pipelineFailed   = "failed"
	pipelineCanceled = "canceled"
	pipelineSkipped  = "skipped"
	pipelineManual   = "manual"
)

// pipelineStatusFor maps a GitHub check_suite/workflow_run conclusion to the
// gitlab-bridge pipeline status. ok is false for a conclusion that carries no
// verdict (null/empty on a still-running run, or one GitHub adds later): the
// handler ignores those rather than stamping a made-up status.
func pipelineStatusFor(conclusion string) (status string, ok bool) {
	switch strings.ToLower(conclusion) {
	case "success", "neutral":
		return pipelineSuccess, true
	case "failure", "timed_out", "startup_failure":
		return pipelineFailed, true
	case "cancelled", "stale":
		return pipelineCanceled, true
	case "skipped":
		return pipelineSkipped, true
	case "action_required":
		return pipelineManual, true
	}
	return "", false
}

// pipelineBeads is the slice of *beadsapi.Client the handler uses (a seam for
// tests).
type pipelineBeads interface {
	MRBeadByURL(ctx context.Context, mrURL string) (*beadsapi.BeadDetail, error)
	UpdateBeadFields(ctx context.Context, beadID string, fields map[string]string) error
	ProducingTaskBeadsForMR(ctx context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

// pipelineHandler turns completed check_suite / workflow_run deliveries into
// mr_pipeline_status + mr_pipeline_url on the PR's merge-request bead, and a
// pass/fail line in the owning agent's Slack thread.
//
// Publishing is the bead write itself: kbeads emits beads.bead.updated for the
// MR bead, which is the shared event path agents (local ones included) and
// slack-bridge already subscribe to. The thread line is a squawk-shaped message
// bead, the same carrier `gb squawk` uses, so slack-bridge posts it into the
// agent's thread on its own credentials.
type pipelineHandler struct {
	beads  pipelineBeads
	logger *slog.Logger
}

func newPipelineHandler(beads pipelineBeads, logger *slog.Logger) *pipelineHandler {
	return &pipelineHandler{beads: beads, logger: logger}
}

// handles reports whether ev is one this handler consumes.
func (h *pipelineHandler) handles(ev Event) bool {
	return ev.Action == "completed" && (ev.Type == "check_suite" || ev.Type == "workflow_run")
}

type pipelinePR struct {
	Number int `json:"number"`
}

// run is the decoded subset shared by check_suite and workflow_run. The two
// events nest it under a different key.
type pipelineRun struct {
	ID           int64        `json:"id"`
	Name         string       `json:"name"` // workflow_run only
	HeadSHA      string       `json:"head_sha"`
	Conclusion   string       `json:"conclusion"`
	HTMLURL      string       `json:"html_url"` // workflow_run only
	PullRequests []pipelinePR `json:"pull_requests"`
	App          struct {
		Name string `json:"name"`
	} `json:"app"` // check_suite only
}

// Handle implements EventSink for the events it claims; others are a no-op so
// it composes behind a router.
func (h *pipelineHandler) Handle(ctx context.Context, ev Event) error {
	if !h.handles(ev) {
		return nil
	}
	var p struct {
		CheckSuite  pipelineRun `json:"check_suite"`
		WorkflowRun pipelineRun `json:"workflow_run"`
	}
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		// Signed by GitHub but undecodable: retrying cannot help, so do not 5xx.
		h.logger.Warn("pipeline: undecodable payload", "delivery", ev.DeliveryID, "event", ev.Type, "error", err)
		return nil
	}
	run := p.CheckSuite
	if ev.Type == "workflow_run" {
		run = p.WorkflowRun
	}
	label := run.Name // workflow name, or the check app for a check_suite
	if label == "" {
		label = run.App.Name
	}
	status, ok := pipelineStatusFor(run.Conclusion)
	if !ok {
		h.logger.Info("pipeline: completed event without a usable conclusion, ignored",
			"delivery", ev.DeliveryID, "event", ev.Type, "conclusion", run.Conclusion)
		return nil
	}
	if ev.Repo == "" || len(run.PullRequests) == 0 {
		// Fork PRs and branch pushes arrive with an empty pull_requests list.
		h.logger.Debug("pipeline: event has no associated pull request",
			"delivery", ev.DeliveryID, "event", ev.Type, "repo", ev.Repo)
		return nil
	}
	pipelineURL := run.HTMLURL
	if pipelineURL == "" && run.HeadSHA != "" {
		pipelineURL = fmt.Sprintf("https://github.com/%s/commit/%s/checks", ev.Repo, run.HeadSHA)
	}

	var firstErr error
	for _, pr := range run.PullRequests {
		if pr.Number <= 0 {
			continue
		}
		mrURL := fmt.Sprintf("https://github.com/%s/pull/%d", ev.Repo, pr.Number)
		if err := h.apply(ctx, mrURL, status, pipelineURL, label, run.HeadSHA, pr.Number); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	// A returned error makes the receiver 5xx and forget the delivery id so
	// GitHub's redelivery is processed; apply is idempotent (unchanged state is
	// not rewritten and not re-announced).
	return firstErr
}

func (h *pipelineHandler) apply(ctx context.Context, mrURL, status, pipelineURL, label, sha string, number int) error {
	mr, err := h.beads.MRBeadByURL(ctx, mrURL)
	if err != nil {
		return fmt.Errorf("look up MR bead %s: %w", mrURL, err)
	}
	if mr == nil {
		h.logger.Info("pipeline: no MR bead for pull request, ignored", "mr_url", mrURL)
		return nil
	}
	prevStatus := mr.Fields["mr_pipeline_status"]
	if prevStatus == status && mr.Fields["mr_pipeline_url"] == pipelineURL {
		return nil
	}
	fields := map[string]string{"mr_pipeline_status": status}
	if pipelineURL != "" {
		fields["mr_pipeline_url"] = pipelineURL
	}
	if err := h.beads.UpdateBeadFields(ctx, mr.ID, fields); err != nil {
		return fmt.Errorf("update MR bead %s: %w", mr.ID, err)
	}
	h.logger.Info("pipeline: MR bead updated", "mr_bead", mr.ID, "mr_url", mrURL, "status", status, "previous", prevStatus)

	// One line per verdict change; a URL-only change (check_suite and
	// workflow_run both reporting the same result) stays silent.
	if prevStatus == status {
		return nil
	}
	h.announce(ctx, mr, status, pipelineURL, label, sha, number)
	return nil
}

// pipelineLine renders the thread line. Slack mrkdwn: <url|text> links.
func pipelineLine(status, pipelineURL, label, sha string, number int) string {
	var icon, verdict string
	switch status {
	case pipelineSuccess:
		icon, verdict = "✅", "passed"
	case pipelineFailed:
		icon, verdict = "❌", "failed"
	case pipelineCanceled:
		icon, verdict = "🚫", "was cancelled"
	case pipelineSkipped:
		icon, verdict = "⏭️", "was skipped"
	default:
		icon, verdict = "⏸️", "needs approval"
	}
	subject := "CI"
	if label != "" {
		subject = label
	}
	if pipelineURL != "" {
		subject = fmt.Sprintf("<%s|%s>", pipelineURL, subject)
	}
	line := fmt.Sprintf("%s %s %s on PR #%d", icon, subject, verdict, number)
	if len(sha) >= 7 {
		line += " (`" + sha[:7] + "`)"
	}
	return line
}

// announce posts the thread line. Best effort: the bead fields are already
// written, and a failed announcement must not turn into a redelivery that
// would repeat the write.
func (h *pipelineHandler) announce(ctx context.Context, mr *beadsapi.BeadDetail, status, pipelineURL, label, sha string, number int) {
	agent := h.owningAgent(ctx, mr)
	if agent == "" {
		h.logger.Info("pipeline: no owning agent for MR bead, thread line skipped", "mr_bead", mr.ID)
		return
	}
	text := pipelineLine(status, pipelineURL, label, sha, number)
	fieldsJSON, err := json.Marshal(map[string]string{
		"source_agent": agent,
		"text":         text,
		"via":          pipelineSource,
	})
	if err != nil {
		return
	}
	id, err := h.beads.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:     text,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"squawk", "from:" + agent},
		Fields:    json.RawMessage(fieldsJSON),
		CreatedBy: pipelineSource,
	})
	if err != nil {
		h.logger.Warn("pipeline: thread line not created", "mr_bead", mr.ID, "error", err)
		return
	}
	if err := h.beads.CloseBead(ctx, id, nil); err != nil {
		h.logger.Warn("pipeline: thread line not closed", "bead", id, "error", err)
	}
}

// owningAgent resolves the agent that produced the MR: the assignee of the
// task bead linked by a `produces` edge.
func (h *pipelineHandler) owningAgent(ctx context.Context, mr *beadsapi.BeadDetail) string {
	tasks, err := h.beads.ProducingTaskBeadsForMR(ctx, mr.ID)
	if err != nil {
		h.logger.Warn("pipeline: producing task lookup failed", "mr_bead", mr.ID, "error", err)
		return ""
	}
	for _, t := range tasks {
		if t.Assignee != "" {
			return t.Assignee
		}
	}
	return ""
}

// fanoutSink hands each event to every sink in order and stops at the first
// error (the receiver then 5xxs and GitHub redelivers). Sinks ignore events
// they do not own, so adding a slice of the bridge is appending one here.
type fanoutSink []EventSink

func (f fanoutSink) Handle(ctx context.Context, ev Event) error {
	for _, s := range f {
		if err := s.Handle(ctx, ev); err != nil {
			return err
		}
	}
	return nil
}
