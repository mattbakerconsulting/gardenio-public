package coopmuxapi

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testEdgeKey = "test-relay-key"

// fakeMux is a coopmux stand-in for /ws/{session} and /ws/mux. It verifies the
// edge-auth v2 envelope the way coop/crates/mux/src/transport/edge_auth.rs
// does, and refuses a frame above the signed role with the same "error"
// frame transport/ws.rs sends.
type fakeMux struct {
	t        *testing.T
	sessions map[string]bool
	dials    atomic.Int32

	mu       sync.Mutex
	headers  http.Header
	path     string
	rawQuery string
	received []map[string]any
	gotFrame chan struct{}
	// onConnect runs after the upgrade; nil sends one "pty" frame.
	onConnect func(conn *websocket.Conn)
}

func newFakeMux(t *testing.T, onConnect func(*websocket.Conn), sessions ...string) (*fakeMux, *httptest.Server) {
	t.Helper()
	f := &fakeMux{t: t, sessions: map[string]bool{}, gotFrame: make(chan struct{}, 16), onConnect: onConnect}
	for _, s := range sessions {
		f.sessions[s] = true
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeMux) verify(r *http.Request) (roles []string, ok bool) {
	h := r.Header
	if h.Get(headerVersion) != "2" || h.Get("X-Edge-Auth-Sig") != "" {
		return nil, false
	}
	want := signEdgeAuthV2([]byte(testEdgeKey), h.Get(headerOperator), h.Get(headerAuthMode),
		h.Get(headerRoles), h.Get(headerAuditID), r.Method, r.URL.EscapedPath(), h.Get(headerTimestamp))
	if !hmac.Equal([]byte(want), []byte(h.Get(headerSignatureV2))) {
		return nil, false
	}
	return strings.Split(h.Get(headerRoles), ","), true
}

func (f *fakeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.dials.Add(1)
	f.mu.Lock()
	f.headers = r.Header.Clone()
	f.path = r.URL.EscapedPath()
	f.rawQuery = r.URL.RawQuery
	f.mu.Unlock()

	roles, ok := f.verify(r)
	if !ok {
		http.Error(w, "edge-auth verification failed", http.StatusUnauthorized)
		return
	}
	if p := r.URL.Path; p != "/ws/mux" && !f.sessions[strings.TrimPrefix(p, "/ws/")] {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		f.t.Errorf("upgrade: %v", err)
		return
	}
	defer conn.Close()
	if f.onConnect != nil {
		f.onConnect(conn)
	} else {
		_ = conn.WriteJSON(map[string]any{"event": "pty", "data": "aGk=", "offset": 0})
	}
	for {
		var frame map[string]any
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		f.mu.Lock()
		f.received = append(f.received, frame)
		f.mu.Unlock()
		f.gotFrame <- struct{}{}
		if frame["event"] == "input:send" && !hasRole(roles, "gardenio-operator") {
			_ = conn.WriteJSON(map[string]any{"event": "error", "code": "forbidden",
				"message": "insufficient permissions: requires gardenio-operator"})
		}
	}
}

func hasRole(roles []string, want string) bool {
	for _, r := range roles {
		if r == want || r == "gardenio-admin" {
			return true
		}
	}
	return false
}

func (f *fakeMux) frames() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]any(nil), f.received...)
}

func (f *fakeMux) waitFrame(t *testing.T) {
	t.Helper()
	select {
	case <-f.gotFrame:
	case <-time.After(5 * time.Second):
		t.Fatal("fake coopmux never received the frame")
	}
}

func operatorCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return WithIdentity(ctx, Identity{
		Operator: "alice@acme.example",
		AuthMode: "cookie",
		Roles:    []string{"gardenio-viewer", "gardenio-operator", "gardenio-viewer"},
	})
}

func testClient(srv *httptest.Server) *Client {
	return New(Config{BaseURL: srv.URL, Token: "svc-token", RelayHMACKey: []byte(testEdgeKey)})
}

func recv(t *testing.T, s *Stream) Frame {
	t.Helper()
	fr, err := s.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	return fr
}

// The v2 signing string must match coopmux byte for byte: this is the vector
// v2_cross_language_known_vector asserts in coop/crates/mux/src/transport/edge_auth_tests.rs.
func TestSignEdgeAuthV2MatchesCoopmuxVector(t *testing.T) {
	t.Parallel()
	got := signEdgeAuthV2([]byte("shared-secret"), "operator@acme.example", "entra",
		"gardenio-operator,gardenio-viewer", "audit-123", "post", "/v1/beads", "1700000000")
	if want := "KNAEp9aRG8u2NQXkBgNsyD/R3X7l6RdnkI+CFL7mkBY="; got != want {
		t.Errorf("signEdgeAuthV2 = %q, want %q", got, want)
	}
}

func TestSubscribeSignsEnvelopeNamingTheHuman(t *testing.T) {
	t.Parallel()
	f, srv := newFakeMux(t, nil, "sess-1")
	s, err := testClient(srv).Subscribe(operatorCtx(t), "sess-1", []string{"pty", "state"})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()

	fr := recv(t, s)
	if fr.Event != "pty" {
		t.Errorf("first frame event = %q, want pty", fr.Event)
	}
	var body struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(fr.Data, &body); err != nil || body.Data != "aGk=" {
		t.Errorf("frame data = %s (%v), want the whole pty frame", fr.Data, err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.path != "/ws/sess-1" {
		t.Errorf("path = %q, want /ws/sess-1", f.path)
	}
	if f.rawQuery != "subscribe=pty%2Cstate" {
		t.Errorf("query = %q, want subscribe=pty,state", f.rawQuery)
	}
	for k, want := range map[string]string{
		"Authorization":   "Bearer svc-token",
		headerOperator:    "alice@acme.example",
		headerAuthMode:    "cookie",
		headerRoles:       "gardenio-operator,gardenio-viewer",
		headerAuditID:     s.AuditID(),
		headerVersion:     "2",
		"X-Edge-Auth-Sig": "",
	} {
		if got := f.headers.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if s.AuditID() == "" {
		t.Error("AuditID should be set")
	}
	if !strings.Contains(f.headers.Get("Sec-WebSocket-Version"), "13") {
		t.Error("request should be a WebSocket upgrade")
	}
}

func TestSubscribeNoTopicsLeavesCoopmuxDefault(t *testing.T) {
	t.Parallel()
	f, srv := newFakeMux(t, nil, "sess-1")
	s, err := testClient(srv).Subscribe(operatorCtx(t), "sess-1", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()
	recv(t, s)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rawQuery != "" {
		t.Errorf("query = %q, want none so coopmux uses pty,screen,state", f.rawQuery)
	}
}

// Every refusal below happens before a dial: the stream fails closed rather
// than reaching coopmux on the bare service token.
func TestSubscribeFailsClosedWithoutDialing(t *testing.T) {
	t.Parallel()
	f, srv := newFakeMux(t, nil, "sess-1")
	good := operatorCtx(t)
	withID := func(id Identity) context.Context { return WithIdentity(good, id) }
	tests := []struct {
		name    string
		c       *Client
		ctx     context.Context
		session string
		topics  []string
		want    error
	}{
		{"disabled", New(Config{}), good, "sess-1", nil, ErrDisabled},
		{"no edge key", New(Config{BaseURL: srv.URL, Token: "svc-token"}), good, "sess-1", nil, ErrNoRelayKey},
		{"no identity", testClient(srv), context.Background(), "sess-1", nil, ErrNoIdentity},
		{"empty session", testClient(srv), good, "", nil, nil},
		{"bad topic", testClient(srv), good, "sess-1", []string{"pty,state"}, nil},
		{"empty topic", testClient(srv), good, "sess-1", []string{""}, nil},
		{"no operator", testClient(srv), withID(Identity{AuthMode: "cookie", Roles: []string{"gardenio-viewer"}}), "sess-1", nil, nil},
		{"bad auth mode", testClient(srv), withID(Identity{Operator: "a@b", AuthMode: "slack", Roles: []string{"gardenio-viewer"}}), "sess-1", nil, nil},
		{"no roles", testClient(srv), withID(Identity{Operator: "a@b", AuthMode: "cookie"}), "sess-1", nil, nil},
		{"non-canonical role", testClient(srv), withID(Identity{Operator: "a@b", AuthMode: "cookie", Roles: []string{"admin"}}), "sess-1", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := tt.c.Subscribe(tt.ctx, tt.session, tt.topics)
			if err == nil {
				s.Close()
				t.Fatal("Subscribe should fail")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
	if n := f.dials.Load(); n != 0 {
		t.Errorf("fake coopmux saw %d dials, want 0", n)
	}
}

func TestSubscribeMapsUpgradeRefusals(t *testing.T) {
	t.Parallel()
	_, srv := newFakeMux(t, nil, "sess-1")

	if _, err := testClient(srv).Subscribe(operatorCtx(t), "other-land-sess", nil); !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("unknown session: err = %v, want ErrSessionNotFound", err)
	}
	wrongKey := New(Config{BaseURL: srv.URL, Token: "svc-token", RelayHMACKey: []byte("wrong")})
	if _, err := wrongKey.Subscribe(operatorCtx(t), "sess-1", nil); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("wrong key: err = %v, want ErrUnauthorized", err)
	}
}

func TestStreamSendAndPerFrameRefusal(t *testing.T) {
	t.Parallel()
	f, srv := newFakeMux(t, nil, "sess-1")
	viewer := WithIdentity(operatorCtx(t), Identity{Operator: "bob@acme.example", AuthMode: "cookie", Roles: []string{"gardenio-viewer"}})
	s, err := testClient(srv).Subscribe(viewer, "sess-1", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()
	recv(t, s)

	if err := s.Send(map[string]any{"event": "input:send", "text": "ls", "enter": true}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	f.waitFrame(t)
	if got := f.frames(); len(got) != 1 || got[0]["text"] != "ls" {
		t.Errorf("fake received %v, want the input frame", got)
	}
	if fr := recv(t, s); fr.Event != "error" || !strings.Contains(string(fr.Data), "forbidden") {
		t.Errorf("frame = %s, want coopmux's forbidden error frame", fr.Data)
	}
}

func TestStreamCancelUnblocksRecv(t *testing.T) {
	t.Parallel()
	_, srv := newFakeMux(t, func(*websocket.Conn) {}, "sess-1") // send nothing
	ctx, cancel := context.WithCancel(operatorCtx(t))
	s, err := testClient(srv).Subscribe(ctx, "sess-1", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	done := make(chan error, 1)
	go func() { _, err := s.Recv(); done <- err }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Recv after cancel should error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Recv still blocked after ctx cancel")
	}
	if err := s.Close(); err != nil && !strings.Contains(err.Error(), "closed") {
		t.Errorf("second Close: %v", err)
	}
}

func TestStreamNormalCloseIsEOF(t *testing.T) {
	t.Parallel()
	_, srv := newFakeMux(t, func(conn *websocket.Conn) {
		_ = conn.WriteMessage(websocket.BinaryMessage, []byte{0x01}) // skipped
		_ = conn.WriteJSON(map[string]any{"event": "exit", "code": 0})
		_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	}, "sess-1")
	s, err := testClient(srv).Subscribe(operatorCtx(t), "sess-1", nil)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer s.Close()
	if fr := recv(t, s); fr.Event != "exit" {
		t.Errorf("event = %q, want exit (binary frame skipped)", fr.Event)
	}
	if _, err := s.Recv(); !errors.Is(err, io.EOF) {
		t.Errorf("Recv after normal close = %v, want io.EOF", err)
	}
}

func TestSubscribeMux(t *testing.T) {
	t.Parallel()
	f, srv := newFakeMux(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"event": "sessions", "sessions": []any{map[string]any{"id": "sess-1"}}})
	})
	m, err := testClient(srv).SubscribeMux(operatorCtx(t))
	if err != nil {
		t.Fatalf("SubscribeMux: %v", err)
	}
	defer m.Close()
	if fr := recv(t, m.Stream); fr.Event != "sessions" {
		t.Errorf("first frame = %q, want sessions", fr.Event)
	}
	f.mu.Lock()
	if f.path != "/ws/mux" || f.rawQuery != "" {
		t.Errorf("dialled %q?%q, want /ws/mux", f.path, f.rawQuery)
	}
	f.mu.Unlock()

	if err := m.SubscribeSessions(); err != nil {
		t.Fatalf("empty SubscribeSessions: %v", err)
	}
	if err := m.SubscribeSessions("sess-1", "sess-2"); err != nil {
		t.Fatalf("SubscribeSessions: %v", err)
	}
	f.waitFrame(t)
	if err := m.UnsubscribeSessions("sess-2"); err != nil {
		t.Fatalf("UnsubscribeSessions: %v", err)
	}
	f.waitFrame(t)
	got := f.frames()
	if len(got) != 2 {
		t.Fatalf("fake received %d frames, want 2 (empty subscribe is a no-op): %v", len(got), got)
	}
	if got[0]["event"] != "subscribe" || len(got[0]["sessions"].([]any)) != 2 {
		t.Errorf("frame 0 = %v, want subscribe of two sessions", got[0])
	}
	if got[1]["event"] != "unsubscribe" {
		t.Errorf("frame 1 = %v, want unsubscribe", got[1])
	}
}

func TestSubscribeMuxFailsClosed(t *testing.T) {
	t.Parallel()
	_, srv := newFakeMux(t, nil)
	if _, err := New(Config{BaseURL: srv.URL}).SubscribeMux(operatorCtx(t)); !errors.Is(err, ErrNoRelayKey) {
		t.Errorf("err = %v, want ErrNoRelayKey", err)
	}
	if _, err := New(Config{}).SubscribeMux(operatorCtx(t)); !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}
