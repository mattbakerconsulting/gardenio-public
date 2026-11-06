// Package coopapi provides an HTTP client for the coop REST API.
//
// Coop is the terminal session manager that wraps Claude Code.
// It exposes a REST API on localhost:8080 for agent state, screen
// content, input injection, nudging, and shutdown.
package coopapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"gardenio/controller/internal/httpjson"
)

// Client talks to the coop HTTP API.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// New creates a coop API client. baseURL is typically "http://localhost:8080".
func New(baseURL string) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// AgentState is the response from GET /api/v1/agent.
type AgentState struct {
	State         string       `json:"state"`
	ErrorCategory string       `json:"error_category,omitempty"`
	LastMessage   string       `json:"last_message,omitempty"`
	Prompt        *AgentPrompt `json:"prompt,omitempty"`
	// ParkedReason is set when State == "parked". For a terminal
	// hard-auth-403 park it is prefixed with "terminal_hard_auth_403"
	// (see coop's TERMINAL_HARD_AUTH_403_PARK_PREFIX) — the controller treats
	// that as an unrecoverable failure and closes the agent bead (ka-0000006562).
	ParkedReason string `json:"parked_reason,omitempty"`
	// RequestCount is the session-cumulative count of LLM requests coop's usage
	// tracker has observed (SessionUsage.request_count on the wire). It advances
	// ONLY when a turn produces a usage-bearing assistant message — a turn that
	// 429s surfaces an isApiErrorMessage that carries no usage and does NOT
	// advance it. That makes it the one "did a genuine turn actually succeed?"
	// signal that is immune to the transient "working" flip a rate-limit recovery
	// re-inject produces (coop clears driver.error on that flip, so state alone
	// and the existing backoff streak cannot tell a real turn from a doomed one).
	// The rate-limit livelock bound in monitorExit keys on it (ka-0000001209).
	RequestCount uint64 `json:"request_count,omitempty"`
}

// AgentPrompt describes a pending prompt from the agent.
//
// Prompt types:
//   - "permission" — tool permission dialog (options: Yes, Don't ask again, No)
//   - "plan"       — plan approval dialog (options: Yes+bypass, Yes+manual, Tell Claude what to change)
//   - "question"   — multi-question dialog (AskUserQuestion)
//   - "setup"      — first-run setup (theme, trust, login)
type AgentPrompt struct {
	Type            string            `json:"type,omitempty"`
	Subtype         string            `json:"subtype,omitempty"`
	Tool            string            `json:"tool,omitempty"`
	Input           string            `json:"input,omitempty"`
	Options         []string          `json:"options,omitempty"`
	OptionsFallback bool              `json:"options_fallback,omitempty"`
	Questions       []QuestionContext `json:"questions,omitempty"`
	QuestionCurrent int               `json:"question_current,omitempty"`
	Ready           bool              `json:"ready,omitempty"`
}

// QuestionContext describes a single question within a multi-question dialog.
type QuestionContext struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// IsPlan returns true if this is a plan approval prompt.
func (p *AgentPrompt) IsPlan() bool { return p.Type == "plan" }

// IsPermission returns true if this is a permission prompt.
func (p *AgentPrompt) IsPermission() bool { return p.Type == "permission" }

// IsQuestion returns true if this is a question prompt.
func (p *AgentPrompt) IsQuestion() bool { return p.Type == "question" }

// NudgeResponse is the response from POST /api/v1/agent/nudge.
type NudgeResponse struct {
	Delivered bool   `json:"delivered"`
	Reason    string `json:"reason,omitempty"`
}

// RespondRequest is the request body for POST /api/v1/agent/respond.
//
// For plan prompts, use Accept or Option to select a choice:
//   - Accept=true  → option 2 (approve, auto-accept edits)
//   - Accept=false → option 4 (freeform feedback, requires Text)
//   - Option=1     → approve (bypass permissions)
//   - Option=2     → approve (auto-accept edits)
//   - Option=3     → approve (manual approve each edit)
//   - Option=4     → provide feedback (freeform text)
type RespondRequest struct {
	Accept  *bool            `json:"accept,omitempty"`
	Option  *int             `json:"option,omitempty"`
	Text    string           `json:"text,omitempty"`
	Answers []QuestionAnswer `json:"answers,omitempty"`
}

// QuestionAnswer is an answer to a single question in a multi-question dialog.
type QuestionAnswer struct {
	Option *int   `json:"option,omitempty"`
	Text   string `json:"text,omitempty"`
}

// RespondResponse is the response from POST /api/v1/agent/respond.
type RespondResponse struct {
	Delivered  bool   `json:"delivered"`
	PromptType string `json:"prompt_type,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// GetAgentState returns the current agent state.
func (c *Client) GetAgentState(ctx context.Context) (*AgentState, error) {
	var state AgentState
	if err := c.getJSON(ctx, "/api/v1/agent", &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// GetScreenText returns the current screen content as plain text.
func (c *Client) GetScreenText(ctx context.Context) (string, error) {
	return c.getText(ctx, "/api/v1/screen/text")
}

// GetScreen returns the current screen content (may include formatting).
func (c *Client) GetScreen(ctx context.Context) (string, error) {
	return c.getText(ctx, "/api/v1/screen")
}

// SendKeys sends keyboard input to the agent.
func (c *Client) SendKeys(ctx context.Context, keys []string) error {
	body := map[string][]string{"keys": keys}
	return c.postJSON(ctx, "/api/v1/input/keys", body, nil)
}

// SendInput writes text directly to the agent's PTY via POST /api/v1/input.
// When enter is true a trailing carriage return is appended so the text is
// SUBMITTED (text alone sits unsubmitted in the input buffer).
//
// Unlike Nudge, this bypasses coop's nudge state-gate: coop.handle_nudge
// refuses to deliver when the agent is in the "error" state (it returns
// delivered=false for any non-idle/working/prompt state). A raw PTY write has
// no such gate, so it is the ONLY way to drive an agent that is wedged on an
// error screen — typing a prompt + Enter forces Claude to start a fresh turn,
// which supersedes the stuck "error" state with "working" (ka-0000006423).
func (c *Client) SendInput(ctx context.Context, text string, enter bool) error {
	body := map[string]any{"text": text, "enter": enter}
	return c.postJSON(ctx, "/api/v1/input", body, nil)
}

// Respond selects an option on a prompt dialog.
func (c *Client) Respond(ctx context.Context, option int) error {
	body := map[string]int{"option": option}
	return c.postJSON(ctx, "/api/v1/agent/respond", body, nil)
}

// RespondFull sends a structured response to the active prompt.
// Use this for plan approval, permission responses, or question answers.
func (c *Client) RespondFull(ctx context.Context, req RespondRequest) (*RespondResponse, error) {
	var resp RespondResponse
	if err := c.postJSON(ctx, "/api/v1/agent/respond", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// RespondPlan responds to a plan mode approval prompt.
//
// Options:
//
//	1 = approve (bypass all permissions)
//	2 = approve (auto-accept edits)
//	3 = approve (manually approve each edit)
//	4 = provide feedback (feedback text sent via the text field)
func (c *Client) RespondPlan(ctx context.Context, option int, feedback string) (*RespondResponse, error) {
	req := RespondRequest{Option: &option}
	if feedback != "" {
		req.Text = feedback
	}
	return c.RespondFull(ctx, req)
}

// Nudge injects a message into the agent's input.
func (c *Client) Nudge(ctx context.Context, message string) (*NudgeResponse, error) {
	body := map[string]string{"message": message}
	var resp NudgeResponse
	if err := c.postJSON(ctx, "/api/v1/agent/nudge", body, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Shutdown requests a graceful coop shutdown.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.postJSON(ctx, "/api/v1/shutdown", nil, nil)
}

// SessionStatus is the response from GET /api/v1/status.
type SessionStatus struct {
	SessionID    string `json:"session_id"`
	State        string `json:"state"`
	PID          *int   `json:"pid"`
	UptimeSecs   int64  `json:"uptime_secs"`
	ExitCode     *int   `json:"exit_code"`
	ScreenSeq    uint64 `json:"screen_seq"`
	BytesRead    uint64 `json:"bytes_read"`
	BytesWritten uint64 `json:"bytes_written"`
	WSClients    int    `json:"ws_clients"`
}

// GetSessionStatus returns the session status including session ID.
func (c *Client) GetSessionStatus(ctx context.Context) (*SessionStatus, error) {
	var status SessionStatus
	if err := c.getJSON(ctx, "/api/v1/status", &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// SessionUsage is the response from GET /api/v1/session/usage: coop's cumulative
// usage counters for the live session. coop fills them for every driver (Claude
// from the session log, Codex from turn.completed / thread/tokenUsage/updated),
// so it is the harness-neutral source for a session's usage. Mirrors coop
// transport/http/usage.rs UsageResponse.
type SessionUsage struct {
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	RequestCount     int64   `json:"request_count"`
	TotalAPIMs       int64   `json:"total_api_ms"`
	UptimeSecs       int64   `json:"uptime_secs"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	ContextTokens    int64   `json:"context_tokens"`
}

// GetSessionUsage returns coop's cumulative usage for the live session.
func (c *Client) GetSessionUsage(ctx context.Context) (*SessionUsage, error) {
	var u SessionUsage
	if err := c.getJSON(ctx, "/api/v1/session/usage", &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// TranscriptInfo describes a single transcript snapshot.
type TranscriptInfo struct {
	Number    uint32 `json:"number"`
	Timestamp string `json:"timestamp"`
	LineCount uint64 `json:"line_count"`
	ByteSize  uint64 `json:"byte_size"`
}

// TranscriptList is the response from GET /api/v1/transcripts.
type TranscriptList struct {
	Transcripts []TranscriptInfo `json:"transcripts"`
}

// ListTranscripts returns available transcript snapshots.
func (c *Client) ListTranscripts(ctx context.Context) (*TranscriptList, error) {
	var list TranscriptList
	if err := c.getJSON(ctx, "/api/v1/transcripts", &list); err != nil {
		return nil, err
	}
	return &list, nil
}

// GetTranscript downloads a single transcript as JSONL content.
func (c *Client) GetTranscript(ctx context.Context, number uint32) ([]byte, error) {
	path := fmt.Sprintf("/api/v1/transcripts/%d", number)
	return c.getBytes(ctx, path)
}

// EventsCatchup is the response from GET /api/v1/events/catchup.
type EventsCatchup struct {
	StateEvents []json.RawMessage `json:"state_events"`
	HookEvents  []json.RawMessage `json:"hook_events"`
}

// GetEventsCatchup returns all state and hook events from the session.
func (c *Client) GetEventsCatchup(ctx context.Context) (*EventsCatchup, error) {
	var events EventsCatchup
	if err := c.getJSON(ctx, "/api/v1/events/catchup", &events); err != nil {
		return nil, err
	}
	return &events, nil
}

// GetEventsCatchupSince returns state events with seq > sinceStateSeq and
// hook events with hook_seq > sinceHookSeq. Used by the local-mode nudge
// runner to gate follow-up nudges on observing an actual claudeless turn
// (working→idle state transition or a Stop hook event) since the previous
// nudge, rather than the weaker GetAgentState() == "idle" poll which can
// match a stale idle that pre-dates the nudge (ka-0000002034).
func (c *Client) GetEventsCatchupSince(ctx context.Context, sinceStateSeq, sinceHookSeq uint64) (*EventsCatchup, error) {
	path := fmt.Sprintf("/api/v1/events/catchup?since_seq=%d&since_hook_seq=%d", sinceStateSeq, sinceHookSeq)
	var events EventsCatchup
	if err := c.getJSON(ctx, path, &events); err != nil {
		return nil, err
	}
	return &events, nil
}

// GetRecording downloads the full recording JSONL file.
func (c *Client) GetRecording(ctx context.Context) ([]byte, error) {
	return c.getBytes(ctx, "/api/v1/recording/download")
}

// getBytes performs a GET request and returns the raw body bytes.
func (c *Client) getBytes(ctx context.Context, path string) ([]byte, error) {
	var data []byte
	if err := c.do(ctx, http.MethodGet, path, rawAccept, nil, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// getJSON performs a GET request and decodes the JSON response.
func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	return c.do(ctx, http.MethodGet, path, nil, nil, out)
}

// getText performs a GET request and returns the body as a string.
func (c *Client) getText(ctx context.Context, path string) (string, error) {
	data, err := c.getBytes(ctx, path)
	return string(data), err
}

// postJSON performs a POST request with a JSON body and optionally decodes the response.
func (c *Client) postJSON(ctx context.Context, path string, body any, out any) error {
	return c.do(ctx, http.MethodPost, path, nil, body, out)
}

// rawAccept is the header for the reads that take the body as-is (screen
// text, transcripts, the recording) rather than as JSON.
var rawAccept = http.Header{"Accept": {"*/*"}}

// do sends one request through httpjson.Do and keeps coopapi's error text:
// a non-2xx response is "unexpected status <code> from <path>".
func (c *Client) do(ctx context.Context, method, path string, header http.Header, body, out any) error {
	err := httpjson.Do(ctx, c.httpClient, httpjson.Request{
		Method: method,
		URL:    c.baseURL + path,
		Header: header,
		Body:   body,
	}, out)
	var se *httpjson.StatusError
	if errors.As(err, &se) {
		return fmt.Errorf("unexpected status %d from %s", se.StatusCode, path)
	}
	return err
}
