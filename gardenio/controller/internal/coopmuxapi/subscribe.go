package coopmuxapi

// Streaming upstream for the Glass relay (spec 2026-10-19-lands-workbench-glass-design.md
// §3.4-§3.5, §6 step 2). The relay in gardenhub opens one of these per watched
// session (Subscribe) and one for the Wall (SubscribeMux), then fans the frames
// out to browsers over SSE. Scope and tier checks happen in the relay before a
// stream is opened or a frame is sent; coopmux re-checks the tier per frame
// against the roles signed into the envelope, as defence in depth.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ErrNoRelayKey is returned by Subscribe and SubscribeMux when the client
// has no RelayHMACKey. The streams fail closed rather than open on the
// bare service token: a bearer-only stream is unscoped and untiered at
// coopmux, and its audit log would name the relay instead of the human.
var ErrNoRelayKey = errors.New("coopmuxapi: no Glass relay key configured")

// ErrNoIdentity is returned by Subscribe and SubscribeMux when ctx carries no
// Identity (see WithIdentity).
var ErrNoIdentity = errors.New("coopmuxapi: no identity on context")

// ErrUnauthorized is returned when coopmux refuses the WebSocket upgrade with
// 401 or 403 (envelope rejected, or the signed roles are below the route's tier).
var ErrUnauthorized = errors.New("coopmuxapi: unauthorized")

// Edge-auth v2 header names, as coopmux's transport/edge_auth.rs reads them.
const (
	headerOperator    = "X-Operator"
	headerAuditID     = "X-Auth-Audit-Id"
	headerTimestamp   = "X-Edge-Auth-Ts"
	headerVersion     = "X-Edge-Auth-Version"
	headerAuthMode    = "X-Edge-Auth-Mode"
	headerRoles       = "X-Edge-Auth-Roles"
	headerSignatureV2 = "X-Edge-Auth-Sig-V2"
)

// maxFrameBytes caps one upstream frame. The largest is the replay coop sends
// on connect: its 1 MiB default PTY ring (COOP_RING_SIZE), base64-encoded in JSON.
const maxFrameBytes = 8 << 20

// Identity is the human a stream is opened for. It is signed into the
// edge-auth v2 envelope so coopmux's audit and per-frame tier checks apply to
// the person, not to the relay's service identity.
type Identity struct {
	// Operator is the human's email, the same value the edge-auth proxy puts
	// in X-Operator.
	Operator string
	// AuthMode is the trust path that vouched for the human: one of
	// "cookie", "entra", "ava", "pat" (coopmux rejects any other). A browser on
	// /garden is "cookie".
	AuthMode string
	// Roles are canonical gardenio-viewer/-operator/-admin roles from the
	// identity provider, never from user input. They are signed sorted and
	// deduplicated, as coopmux requires.
	Roles []string
}

type identityKeyType struct{}

// WithIdentity returns ctx carrying the Identity that the next Subscribe or
// SubscribeMux call signs into its envelope.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKeyType{}, id)
}

// IdentityFromContext returns the Identity attached via WithIdentity and
// whether one was set.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(identityKeyType{}).(Identity)
	return id, ok
}

// canonicalRoles validates roles and returns them sorted, deduplicated and
// comma-joined, the only form coopmux's parse_canonical_roles accepts.
func canonicalRoles(roles []string) (string, error) {
	if len(roles) == 0 {
		return "", errors.New("coopmuxapi: identity has no roles")
	}
	out := make([]string, 0, len(roles))
	for _, r := range roles {
		switch r {
		case "gardenio-viewer", "gardenio-operator", "gardenio-admin":
		default:
			return "", fmt.Errorf("coopmuxapi: non-canonical role %q", r)
		}
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}
	slices.Sort(out)
	return strings.Join(out, ","), nil
}

func validAuthMode(mode string) bool {
	switch mode {
	case "cookie", "entra", "ava", "pat":
		return true
	}
	return false
}

// signEdgeAuthV2 mirrors canonical_msg_v2 in coop/crates/mux/src/transport/
// edge_auth.rs (and beadsapi's signEdgeAuthV2 for the daemon).
func signEdgeAuthV2(key []byte, operator, authMode, roles, auditID, method, path, ts string) string {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(strings.Join([]string{
		"v2", operator, authMode, roles, auditID, strings.ToUpper(method), path, ts,
	}, "\n")))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// NewAuditID mints an audit id of the X-Auth-Audit-Id shape, for a write the
// Glass logs itself (Lands GL-11) where no envelope carries one upstream.
func NewAuditID() string { return newAuditID() }

func newAuditID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// envelope builds the upgrade headers for a GET of path: the service bearer
// plus a v2 envelope for id. Only the v2 signature is sent: coopmux gives a
// verified v1 envelope every tier, which would skip its per-frame check.
func (c *Client) envelope(id Identity, path string) (http.Header, string, error) {
	if len(c.relayKey) == 0 {
		return nil, "", ErrNoRelayKey
	}
	if id.Operator == "" {
		return nil, "", errors.New("coopmuxapi: identity has no operator")
	}
	if !validAuthMode(id.AuthMode) {
		return nil, "", fmt.Errorf("coopmuxapi: invalid auth mode %q", id.AuthMode)
	}
	roles, err := canonicalRoles(id.Roles)
	if err != nil {
		return nil, "", err
	}
	auditID := newAuditID()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	h := http.Header{}
	if c.token != "" {
		h.Set("Authorization", "Bearer "+c.token)
	}
	h.Set(headerOperator, id.Operator)
	h.Set(headerAuditID, auditID)
	h.Set(headerTimestamp, ts)
	h.Set(headerVersion, "2")
	h.Set(headerAuthMode, id.AuthMode)
	h.Set(headerRoles, roles)
	h.Set(headerSignatureV2, signEdgeAuthV2(c.relayKey, id.Operator, id.AuthMode, roles, auditID, http.MethodGet, path, ts))
	return h, auditID, nil
}

// Frame is one JSON text message from coopmux.
type Frame struct {
	// Event is the frame's "event" tag: "pty", "replay", "screen",
	// "transition", "exit", ... on a session stream; "sessions",
	// "screen_batch", "transition", "session:online", "ping", ... on the mux
	// stream. Empty for a frame without one (a reply carrying request_id).
	Event string
	// Data is the whole frame as received, for the relay to forward or decode.
	Data json.RawMessage
}

// Stream is one open WebSocket to coopmux. Recv and Send may be called from
// different goroutines; Recv from one goroutine only.
type Stream struct {
	conn    *websocket.Conn
	auditID string

	writeMu   sync.Mutex
	closeOnce sync.Once
	done      chan struct{}
}

func newStream(ctx context.Context, conn *websocket.Conn, auditID string) *Stream {
	conn.SetReadLimit(maxFrameBytes)
	s := &Stream{conn: conn, auditID: auditID, done: make(chan struct{})}
	// The stream lives as long as ctx: cancelling it unblocks Recv.
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.done:
		}
	}()
	return s
}

// AuditID is the X-Auth-Audit-Id signed into this stream's envelope. coopmux
// logs it on every frame it forwards or denies; the relay's own audit lines
// (GL-11) carry it so the two can be joined.
func (s *Stream) AuditID() string { return s.auditID }

// Recv blocks for the next frame. It returns io.EOF when coopmux closes the
// stream normally, and an error once the stream is closed or its context is
// cancelled.
func (s *Stream) Recv() (Frame, error) {
	for {
		typ, data, err := s.conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return Frame{}, io.EOF
			}
			return Frame{}, fmt.Errorf("coopmuxapi: read: %w", err)
		}
		if typ != websocket.TextMessage {
			continue
		}
		var tag struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(data, &tag); err != nil {
			return Frame{}, fmt.Errorf("coopmuxapi: decode frame: %w", err)
		}
		return Frame{Event: tag.Event, Data: data}, nil
	}
}

// Send writes one JSON control frame upstream. coopmux checks the frame's tier
// against the envelope's roles and answers a refused frame with an "error"
// frame (code "forbidden") on the stream rather than closing it.
func (s *Stream) Send(frame any) error {
	data, err := json.Marshal(frame)
	if err != nil {
		return fmt.Errorf("coopmuxapi: marshal frame: %w", err)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		return fmt.Errorf("coopmuxapi: write: %w", err)
	}
	return nil
}

// Close closes the stream. It is safe to call more than once.
func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.done)
		// WriteControl may run alongside a blocked Send, so Close never
		// waits behind a slow write.
		_ = s.conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second))
		err = s.conn.Close()
	})
	return err
}

// dial opens a WebSocket to path (escaped, no query) with the identity on ctx.
func (c *Client) dial(ctx context.Context, path string, query url.Values) (*Stream, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	id, ok := IdentityFromContext(ctx)
	if !ok {
		return nil, ErrNoIdentity
	}
	hdr, auditID, err := c.envelope(id, path)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(c.baseURL)
	if err != nil {
		return nil, fmt.Errorf("coopmuxapi: parse base url: %w", err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	}
	// coopmux signs and routes on the path it receives, so the path is set
	// already escaped and verbatim.
	u.RawPath = path
	u.Path, _ = url.PathUnescape(path)
	u.RawQuery = query.Encode()

	conn, resp, err := c.dialer.DialContext(ctx, u.String(), hdr)
	if err != nil {
		if resp != nil {
			defer resp.Body.Close()
			switch resp.StatusCode {
			case http.StatusNotFound:
				return nil, ErrSessionNotFound
			case http.StatusUnauthorized, http.StatusForbidden:
				return nil, fmt.Errorf("%w: status %d", ErrUnauthorized, resp.StatusCode)
			}
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			return nil, fmt.Errorf("coopmuxapi: upgrade status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		return nil, fmt.Errorf("coopmuxapi: dial: %w", err)
	}
	return newStream(ctx, conn, auditID), nil
}

// Subscribe opens coopmux's per-session stream GET /ws/{session} for the
// Identity on ctx (WithIdentity). topics become the ?subscribe= flags
// ("pty", "screen", "state"; coopmux passes through events it has no flag
// for); nil leaves coopmux's default of pty,screen,state. ctx bounds the
// stream's whole life, not just the dial.
//
// Returns ErrDisabled with no base URL, ErrNoIdentity / ErrNoRelayKey when
// the envelope cannot be signed, ErrSessionNotFound on 404 (an unknown
// session, or one outside a land-scoped caller's scope), and ErrUnauthorized
// on 401/403.
func (c *Client) Subscribe(ctx context.Context, sessionID string, topics []string) (*Stream, error) {
	if c.Disabled() {
		return nil, ErrDisabled
	}
	if sessionID == "" {
		return nil, fmt.Errorf("coopmuxapi: empty session id")
	}
	q := url.Values{}
	if len(topics) > 0 {
		for _, t := range topics {
			if t == "" || strings.ContainsAny(t, ", ") {
				return nil, fmt.Errorf("coopmuxapi: invalid topic %q", t)
			}
		}
		q.Set("subscribe", strings.Join(topics, ","))
	}
	return c.dial(ctx, "/ws/"+url.PathEscape(sessionID), q)
}

// MuxStream is the /ws/mux aggregate feed. On connect coopmux sends a
// "sessions" frame listing every session; per-session "transition" events and
// periodic "screen_batch" thumbnails flow only for sessions named in
// SubscribeSessions.
type MuxStream struct {
	*Stream
}

// SubscribeSessions asks coopmux to start sending events and thumbnails for
// the given sessions. A no-op for an empty list.
func (m *MuxStream) SubscribeSessions(sessions ...string) error {
	if len(sessions) == 0 {
		return nil
	}
	return m.Send(map[string]any{"event": "subscribe", "sessions": sessions})
}

// UnsubscribeSessions stops events and thumbnails for the given sessions. A
// no-op for an empty list.
func (m *MuxStream) UnsubscribeSessions(sessions ...string) error {
	if len(sessions) == 0 {
		return nil
	}
	return m.Send(map[string]any{"event": "unsubscribe", "sessions": sessions})
}

// SubscribeMux opens coopmux's aggregate feed GET /ws/mux for the Identity on
// ctx. The relay filters its "sessions" and "screen_batch" frames per
// principal before fanning them out (spec §3.4 #2). Error contract as
// Subscribe, without ErrSessionNotFound.
func (c *Client) SubscribeMux(ctx context.Context) (*MuxStream, error) {
	s, err := c.dial(ctx, "/ws/mux", nil)
	if err != nil {
		return nil, err
	}
	return &MuxStream{Stream: s}, nil
}
