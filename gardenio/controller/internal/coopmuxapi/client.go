// Package coopmuxapi is a thin HTTP client to the coopmux mux service. It is
// used by the beads-viewer agent-detail view for two purposes:
//
//   - Render a cached PTY screen snapshot for first-paint (GetScreen) before
//     the xterm.js bundle attaches the live WebSocket stream.
//   - Forward operator keystrokes to a session as part of the Phase 2
//     "Take control" write surface (SendInput). This is the single write
//     verb exposed; any further write methods (kill, start, etc.) would be
//     scope expansion and need to go through coopmux's own auth boundary.
//
// Subscribe and SubscribeMux (subscribe.go) are the Glass relay's streaming
// upstream: one WebSocket per session or for the whole mux, opened with the
// service token and an edge-auth v2 envelope naming the human watching.
package coopmuxapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"gardenio/controller/internal/httpjson"
)

// ErrDisabled is returned by Client methods when the client was constructed
// with an empty base URL (i.e. coopmux is not deployed alongside this
// beads-viewer instance). Callers should treat this as a benign degraded
// mode and render a placeholder rather than surfacing an error.
var ErrDisabled = errors.New("coopmuxapi: client disabled (empty base URL)")

// ErrSessionNotFound is returned when the mux has no session matching the
// requested ID. The agent-detail view treats this the same as a missing
// last_session_id field on the bead.
var ErrSessionNotFound = errors.New("coopmuxapi: session not found")

// Config configures a Client.
type Config struct {
	// BaseURL is the coopmux HTTP address (e.g., "http://gardenio-coopmux:9800").
	// Empty BaseURL constructs a disabled client whose methods always return
	// ErrDisabled — useful for environments where coopmux is not deployed.
	BaseURL string

	// Token is the static Bearer token for the coopmux API. Sent as
	// "Authorization: Bearer <token>" on every request when non-empty.
	Token string

	// HTTPClient is optional. Defaults to a 5s-timeout client.
	HTTPClient *http.Client

	// RelayHMACKey signs the edge-auth v2 envelope that Subscribe and
	// SubscribeMux send to name the human the stream is opened for. It is the
	// Glass relay key, which only coopmux verifies (DAEMON_HMAC_KEY_RELAY) and
	// only on these two watch upgrades; it is never the v2 key the beads
	// daemon trusts (ruling gd-9ZGh01W5ub). Empty makes both streaming methods
	// fail closed with ErrNoRelayKey; the HTTP methods above do not use it.
	RelayHMACKey []byte

	// Dialer is optional and only used by the streaming methods. Defaults to
	// a dialer with a 10s handshake timeout.
	Dialer *websocket.Dialer
}

// Client is an HTTP client to coopmux. Read methods (GetScreen) are the
// happy path; SendInput is the single write verb exposed for the Phase 2
// "Take control" operator surface.
type Client struct {
	baseURL  string
	token    string
	http     *http.Client
	relayKey []byte
	dialer   *websocket.Dialer
}

// New constructs a Client. An empty cfg.BaseURL constructs a disabled
// client whose methods return ErrDisabled.
func New(cfg Config) *Client {
	c := &Client{
		baseURL:  strings.TrimRight(cfg.BaseURL, "/"),
		token:    cfg.Token,
		http:     cfg.HTTPClient,
		relayKey: cfg.RelayHMACKey,
		dialer:   cfg.Dialer,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 5 * time.Second}
	}
	if c.dialer == nil {
		c.dialer = &websocket.Dialer{Proxy: http.ProxyFromEnvironment, HandshakeTimeout: 10 * time.Second}
	}
	return c
}

// Disabled reports whether this client is in degraded (no-op) mode.
func (c *Client) Disabled() bool { return c == nil || c.baseURL == "" }

// Screen mirrors the coopmux CachedScreen response shape from
// GET /api/v1/sessions/{id}/screen. Only the fields used by the
// beads-viewer fallback render are surfaced.
type Screen struct {
	Lines     []string `json:"lines"`
	ANSI      []string `json:"ansi"`
	Cols      uint16   `json:"cols"`
	Rows      uint16   `json:"rows"`
	AltScreen bool     `json:"alt_screen"`
	Seq       uint64   `json:"seq"`
	FetchedAt uint64   `json:"fetched_at"`
}

// Text returns the screen as newline-joined plain-text lines, suitable for
// dropping into a <pre> block. ANSI escape sequences from the `ansi` field
// are NOT used — a raw escape stream rendered as text would be unreadable.
func (s *Screen) Text() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.Lines, "\n")
}

// GetScreen fetches the cached screen snapshot for the given session id.
//
// Returns ErrDisabled if the client has no base URL configured, and
// ErrSessionNotFound on a 404 response. Other HTTP / transport errors are
// returned wrapped.
func (c *Client) GetScreen(ctx context.Context, sessionID string) (*Screen, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	if sessionID == "" {
		return nil, fmt.Errorf("coopmuxapi: empty session id")
	}
	var s Screen
	if err := c.doSession(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(sessionID)+"/screen", nil, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// AgentState mirrors the subset of the coopmux GET
// /api/v1/sessions/{id}/agent response the beads-viewer needs. coopmux proxies
// this from the in-pod coop's GET /api/v1/agent (coop/crates/mux/src/transport/
// http.rs session_agent → UpstreamClient.get_agent), whose LastMessage is the
// FULL, untruncated final assistant reply held in coop's driver state — unlike
// the 256KB-budgeted NATS assistant_text forward. It is the >256KB full-text
// FALLBACK for the plan-transcript AssistantTurnMinter (task 2a): the NATS
// event alone carries the complete reply for anything under the wire budget, so
// this second hop is exercised only for the rare oversized turn.
type AgentState struct {
	// State is the agent's coarse lifecycle state (e.g. "working", "idle",
	// "parked"); surfaced so a caller can tell a torn-down session from a live
	// one. Display-optional.
	State string `json:"state,omitempty"`
	// LastMessage is the full untruncated text of the agent's most recent
	// assistant reply. Empty when the agent has not yet produced one (or the
	// conversation was cleared).
	LastMessage string `json:"last_message,omitempty"`
	// Prompt is the prompt the agent is parked at while State is "prompt",
	// nil otherwise. The Glass's respond (Lands GL-8) labels its answers from
	// it and checks a chosen option against it before answering.
	Prompt *Prompt `json:"prompt,omitempty"`
}

// Prompt mirrors coop's PromptContext (coop/crates/cli/src/driver/mod.rs).
type Prompt struct {
	// Type is permission, plan, question or setup.
	Type string `json:"type"`
	// Subtype classifies within a type: trust or tool for a permission;
	// oauth_login, fable_consent, theme_picker, ... for a setup.
	Subtype string `json:"subtype,omitempty"`
	Tool    string `json:"tool,omitempty"`
	Input   string `json:"input,omitempty"`
	// Options are the numbered option labels coop read off the screen, in
	// order: option N (1-based) is Options[N-1].
	Options []string `json:"options,omitempty"`
	// OptionsFallback: coop found no options on the screen and Options are
	// its stand-ins, so option 1 accepts and any other declines.
	OptionsFallback bool `json:"options_fallback,omitempty"`
	// Questions are a question dialog's questions; QuestionCurrent is the
	// 0-based one being asked (== len(Questions) is the confirm step).
	Questions       []PromptQuestion `json:"questions,omitempty"`
	QuestionCurrent int              `json:"question_current,omitempty"`
	// Ready: coop has finished reading the prompt (a permission or plan
	// prompt's options arrive a moment after the prompt does).
	Ready bool `json:"ready,omitempty"`
}

// PromptQuestion is one question of a question dialog.
type PromptQuestion struct {
	Question string   `json:"question"`
	Options  []string `json:"options,omitempty"`
}

// GetAgentState fetches the agent-state snapshot (including the full untruncated
// LastMessage) for the given session id from coopmux.
//
// Returns ErrDisabled when the client has no base URL, ErrSessionNotFound on a
// 404 (the session was torn down — a parked/thawed/respawned assistant), and a
// wrapped error for other transport / non-2xx failures. Mirrors GetScreen's
// error contract so callers can branch identically.
func (c *Client) GetAgentState(ctx context.Context, sessionID string) (*AgentState, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	if sessionID == "" {
		return nil, fmt.Errorf("coopmuxapi: empty session id")
	}
	var a AgentState
	if err := c.doSession(ctx, http.MethodGet, "/api/v1/sessions/"+url.PathEscape(sessionID)+"/agent", nil, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// PoolAccount is one account of coopmux's credential pool as GET
// /api/v1/credentials/pool reports it (coop/crates/mux/src/transport/
// http_cred.rs credentials_pool): the broker's view of it, enriched with the
// controller's rate-limit probe. The probe fields are empty until the account
// is probed. No secret is in it: has_api_key only says whether one is set.
type PoolAccount struct {
	Name                  string `json:"name"`
	Provider              string `json:"provider"`
	Status                string `json:"status"` // healthy, missing, warming, throttled
	SessionCount          uint32 `json:"session_count"`
	CumulativeAssignments uint64 `json:"cumulative_assignments"`
	HasAPIKey             bool   `json:"has_api_key"`
	ExpiresAt             uint64 `json:"expires_at,omitempty"` // epoch seconds; 0 for a static key
	ThrottleStatus        string `json:"throttle_status,omitempty"`
	Utilization5h         string `json:"utilization_5h,omitempty"` // a fraction, "0.4200"
	Utilization7d         string `json:"utilization_7d,omitempty"`
	ThrottleResetAt       string `json:"throttle_reset_at,omitempty"` // RFC3339
	RepresentativeClaim   string `json:"representative_claim,omitempty"`
}

// CredentialPool is GET /api/v1/credentials/pool: every account, the totals
// and how the broker picks one for a new session.
type CredentialPool struct {
	Accounts         []PoolAccount `json:"accounts"`
	TotalSessions    uint32        `json:"total_sessions"`
	HealthyAccounts  int           `json:"healthy_accounts"`
	TotalAccounts    int           `json:"total_accounts"`
	Strategy         string        `json:"strategy"` // least_loaded, round_robin
	RotatorCursor    *uint64       `json:"rotator_cursor"`
	RotatorNextIndex *uint64       `json:"rotator_next_index"`
	// AutoAssignDisabled: COOPMUX_DISABLE_AUTO_ASSIGN is set, so the broker
	// never counts assignments and the session counts read 0 (ka-0000005557).
	AutoAssignDisabled bool `json:"auto_assign_disabled"`
}

// CredentialPool fetches the credential pool snapshot. coopmux holds every
// /api/v1/credentials route to the admin tier; the caller checks that tier
// before it asks, since this request carries only the service bearer.
//
// Returns ErrDisabled when the client has no base URL and a wrapped error for
// transport / non-2xx failures.
func (c *Client) CredentialPool(ctx context.Context) (*CredentialPool, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	var p CredentialPool
	if err := c.do(ctx, http.MethodGet, "/api/v1/credentials/pool", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// do sends one JSON request to the mux and decodes a 2xx body into out (nil
// discards it, a *[]byte takes it raw). It returns ErrDisabled when the
// client has no base URL and a wrapped error for transport / non-2xx failures;
// a status failure wraps the *httpjson.StatusError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	if c.Disabled() {
		return ErrDisabled
	}
	err := httpjson.Do(ctx, c.http, httpjson.Request{
		Method: method,
		URL:    c.baseURL + path,
		Header: httpjson.Bearer(c.token),
		Body:   body,
	}, out)
	var se *httpjson.StatusError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &se):
		return fmt.Errorf("coopmuxapi: unexpected %w", se)
	default:
		return fmt.Errorf("coopmuxapi: %w", err)
	}
}

// doSession is do for a /api/v1/sessions/{id} route, where a 404 means the
// session is gone: it returns ErrSessionNotFound.
func (c *Client) doSession(ctx context.Context, method, path string, body, out any) error {
	err := c.do(ctx, method, path, body, out)
	var se *httpjson.StatusError
	if errors.As(err, &se) && se.StatusCode == http.StatusNotFound {
		return ErrSessionNotFound
	}
	return err
}

// SendKeys POSTs a list of named keys (e.g. "Enter", "Esc", "Ctrl-C") to a
// session's PTY. Mirrors POST /api/v1/sessions/{id}/input/keys with body
// {"keys": [...]}. Empty key lists are a no-op (returns nil).
func (c *Client) SendKeys(ctx context.Context, sessionID string, keys []string) error {
	if c.Disabled() {
		return ErrDisabled
	}
	if sessionID == "" {
		return fmt.Errorf("coopmuxapi: empty session id")
	}
	if len(keys) == 0 {
		return nil
	}
	return c.doSession(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(sessionID)+"/input/keys",
		map[string]any{"keys": keys}, nil,
	)
}

// MouseEvent is one mouse press or release at a cell (1-based Col and Row),
// the body of POST /api/v1/sessions/{id}/input/mouse. Coop builds the report
// bytes itself, as the app asked for them, so no raw bytes are sent.
type MouseEvent struct {
	Button  string `json:"button"` // left | middle | right | wheel_up | wheel_down
	Col     uint16 `json:"col"`
	Row     uint16 `json:"row"`
	Release bool   `json:"release,omitempty"`
	Shift   bool   `json:"shift,omitempty"`
	Alt     bool   `json:"alt,omitempty"`
	Ctrl    bool   `json:"ctrl,omitempty"`
}

// SendMouse POSTs one mouse event to a session's PTY and returns the bytes
// coop wrote: 0 when the app has mouse tracking off, so nothing was sent.
func (c *Client) SendMouse(ctx context.Context, sessionID string, ev MouseEvent) (int, error) {
	if c.Disabled() {
		return 0, ErrDisabled
	}
	if sessionID == "" {
		return 0, fmt.Errorf("coopmuxapi: empty session id")
	}
	var out struct {
		BytesWritten int `json:"bytes_written"`
	}
	if err := c.doSession(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(sessionID)+"/input/mouse",
		ev, &out,
	); err != nil {
		return 0, err
	}
	return out.BytesWritten, nil
}

// Resize POSTs a terminal size to a session's PTY. Mirrors POST
// /api/v1/sessions/{id}/resize with body {"cols": .., "rows": ..}; every
// watcher's terminal follows, so only the person attached sends one (the
// Glass, Lands GL-9). A zero size is a no-op.
func (c *Client) Resize(ctx context.Context, sessionID string, cols, rows uint16) error {
	if c.Disabled() {
		return ErrDisabled
	}
	if sessionID == "" {
		return fmt.Errorf("coopmuxapi: empty session id")
	}
	if cols == 0 || rows == 0 {
		return nil
	}
	return c.doSession(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(sessionID)+"/resize",
		map[string]any{"cols": cols, "rows": rows}, nil,
	)
}

// Nudge POSTs a nudge message to a parked agent. Mirrors POST
// /api/v1/sessions/{id}/agent/nudge with body {"message": "..."}. Empty
// messages are a no-op.
func (c *Client) Nudge(ctx context.Context, sessionID, message string) error {
	if c.Disabled() {
		return ErrDisabled
	}
	if sessionID == "" {
		return fmt.Errorf("coopmuxapi: empty session id")
	}
	if message == "" {
		return nil
	}
	return c.doSession(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(sessionID)+"/agent/nudge",
		map[string]any{"message": message}, nil,
	)
}

// RespondRequest mirrors the coopmux RespondRequest body for
// POST /api/v1/sessions/{id}/agent/respond. Each field is optional — the
// caller decides which combination matches the agent's current ask_user
// prompt (yes/no, free-text, or numbered option).
type RespondRequest struct {
	Accept *bool   `json:"accept,omitempty"`
	Text   *string `json:"text,omitempty"`
	Option *int    `json:"option,omitempty"`
	// Answers answer a question dialog, one per question from the current one.
	Answers []QuestionAnswer `json:"answers,omitempty"`
}

// QuestionAnswer answers one question: an option (1-based) or free text.
type QuestionAnswer struct {
	Option *int    `json:"option,omitempty"`
	Text   *string `json:"text,omitempty"`
}

// RespondOutcome is coop's answer to a respond: Delivered is false when the
// agent was at no prompt by the time it arrived (Reason says so).
type RespondOutcome struct {
	Delivered  bool   `json:"delivered"`
	PromptType string `json:"prompt_type,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// Respond POSTs an answer to an agent's ask_user prompt. Mirrors
// POST /api/v1/sessions/{id}/agent/respond. Returns ErrDisabled /
// ErrSessionNotFound on the same conditions as the other write methods.
func (c *Client) Respond(ctx context.Context, sessionID string, req RespondRequest) error {
	_, err := c.RespondOutcome(ctx, sessionID, req)
	return err
}

// RespondOutcome is Respond, returning coop's outcome: whether the answer
// reached a prompt. A 2xx with a body that is not an outcome reads as
// delivered, as Respond always took it.
func (c *Client) RespondOutcome(ctx context.Context, sessionID string, req RespondRequest) (*RespondOutcome, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	if sessionID == "" {
		return nil, fmt.Errorf("coopmuxapi: empty session id")
	}
	out := &RespondOutcome{Delivered: true}
	var body []byte
	if err := c.doSession(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(sessionID)+"/agent/respond",
		req, &body,
	); err != nil {
		return nil, err
	}
	var o RespondOutcome
	if json.Unmarshal(body, &o) == nil && bytes.Contains(body, []byte(`"delivered"`)) {
		out = &o
	}
	return out, nil
}

// SendInput POSTs operator keystrokes to a session's PTY. Mirrors the
// coopmux endpoint POST /api/v1/sessions/{id}/input which accepts a JSON
// body of {"text": "..."}.
//
// Returns ErrDisabled when the client has no base URL, ErrSessionNotFound
// on a 404, and a wrapped error for other transport / non-2xx responses.
// An empty text is a no-op (returns nil) so callers can safely forward an
// empty form field without surfacing an error.
func (c *Client) SendInput(ctx context.Context, sessionID, text string) error {
	if c.Disabled() {
		return ErrDisabled
	}
	if sessionID == "" {
		return fmt.Errorf("coopmuxapi: empty session id")
	}
	if text == "" {
		return nil
	}
	return c.doSession(ctx, http.MethodPost,
		"/api/v1/sessions/"+url.PathEscape(sessionID)+"/input",
		map[string]string{"text": text}, nil,
	)
}
