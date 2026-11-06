package coopmuxapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewTrimsTrailingSlashAndDefaultsClient(t *testing.T) {
	t.Parallel()
	c := New(Config{BaseURL: "http://mux:9800/"})
	if c.baseURL != "http://mux:9800" {
		t.Errorf("baseURL = %q, want trailing slash trimmed", c.baseURL)
	}
	if c.http == nil {
		t.Fatal("http client should default to non-nil")
	}
	if c.http.Timeout == 0 {
		t.Error("default http client should carry a non-zero timeout")
	}
}

func TestNewKeepsSuppliedHTTPClient(t *testing.T) {
	t.Parallel()
	custom := &http.Client{}
	c := New(Config{BaseURL: "http://mux", HTTPClient: custom})
	if c.http != custom {
		t.Error("supplied HTTPClient should be retained, not replaced")
	}
}

func TestDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		c    *Client
		want bool
	}{
		{"nil client", nil, true},
		{"empty base url", New(Config{}), true},
		{"configured", New(Config{BaseURL: "http://mux"}), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.c.Disabled(); got != tt.want {
				t.Errorf("Disabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScreenText(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		s    *Screen
		want string
	}{
		{"nil screen", nil, ""},
		{"empty lines", &Screen{Lines: nil}, ""},
		{"single line", &Screen{Lines: []string{"hello"}}, "hello"},
		{"multi line", &Screen{Lines: []string{"a", "b", "c"}}, "a\nb\nc"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.Text(); got != tt.want {
				t.Errorf("Text() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetScreenDisabled(t *testing.T) {
	t.Parallel()
	c := New(Config{}) // no base URL → disabled
	_, err := c.GetScreen(context.Background(), "sess-1")
	if !errors.Is(err, ErrDisabled) {
		t.Errorf("err = %v, want ErrDisabled", err)
	}
}

func TestGetScreenEmptySessionID(t *testing.T) {
	t.Parallel()
	c := New(Config{BaseURL: "http://mux"})
	_, err := c.GetScreen(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty session id") {
		t.Errorf("err = %v, want empty session id error", err)
	}
}

func TestGetScreenHappyPath(t *testing.T) {
	t.Parallel()
	want := Screen{
		Lines:     []string{"line1", "line2"},
		Cols:      80,
		Rows:      24,
		AltScreen: true,
		Seq:       42,
	}
	var gotPath, gotEscaped, gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotEscaped = r.URL.EscapedPath()
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		_ = json.NewEncoder(w).Encode(want)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Token: "tok", HTTPClient: srv.Client()})
	got, err := c.GetScreen(context.Background(), "sess id/with space")
	if err != nil {
		t.Fatalf("GetScreen: %v", err)
	}
	if got.Cols != want.Cols || got.Rows != want.Rows || got.Seq != want.Seq || !got.AltScreen {
		t.Errorf("decoded screen = %+v, want %+v", got, want)
	}
	if got.Text() != "line1\nline2" {
		t.Errorf("Text() = %q", got.Text())
	}
	// PathEscape should have encoded the space in the session id.
	if !strings.Contains(gotPath, "/api/v1/sessions/") || !strings.Contains(gotPath, "/screen") {
		t.Errorf("path = %q, want sessions/.../screen", gotPath)
	}
	// The space in the session id must be percent-escaped on the wire
	// (PathEscape turns " " into "%20"); the decoded path will still show it.
	if strings.Contains(gotEscaped, " ") {
		t.Errorf("escaped path %q should not contain a raw space (session id must be escaped)", gotEscaped)
	}
	if !strings.Contains(gotEscaped, "%20") {
		t.Errorf("escaped path %q should contain %%20 for the session-id space", gotEscaped)
	}
	if gotAuth != "Bearer tok" {
		t.Errorf("Authorization = %q, want Bearer tok", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("Accept = %q, want application/json", gotAccept)
	}
}

func TestGetScreenNoTokenOmitsAuthHeader(t *testing.T) {
	t.Parallel()
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(Screen{})
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()}) // no token
	if _, err := c.GetScreen(context.Background(), "s"); err != nil {
		t.Fatalf("GetScreen: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty when no token configured", gotAuth)
	}
}

func TestGetScreenNotFound(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := c.GetScreen(context.Background(), "missing")
	if !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("err = %v, want ErrSessionNotFound", err)
	}
}

func TestGetScreenServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "boom")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := c.GetScreen(context.Background(), "s")
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if !strings.Contains(err.Error(), "500") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want status code + body", err)
	}
}

func TestGetScreenBadJSON(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "not json")
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	_, err := c.GetScreen(context.Background(), "s")
	if err == nil || !strings.Contains(err.Error(), "decode") {
		t.Errorf("err = %v, want decode error", err)
	}
}

// writeMethod abstracts the POST-style write verbs so the shared
// disabled / empty-id / no-op / status-mapping behaviour can be table-tested.
type writeMethod struct {
	name string
	// call invokes the method with a non-empty payload (should hit the server).
	call func(c *Client, ctx context.Context, sessionID string) error
	// noop invokes the method with an empty payload (should NOT hit the server).
	noop func(c *Client, ctx context.Context, sessionID string) error
	// wantPath is the expected request path suffix when call() hits the server.
	wantPath string
}

func writeMethods() []writeMethod {
	return []writeMethod{
		{
			name:     "SendKeys",
			call:     func(c *Client, ctx context.Context, s string) error { return c.SendKeys(ctx, s, []string{"Enter"}) },
			noop:     func(c *Client, ctx context.Context, s string) error { return c.SendKeys(ctx, s, nil) },
			wantPath: "/input/keys",
		},
		{
			name:     "Nudge",
			call:     func(c *Client, ctx context.Context, s string) error { return c.Nudge(ctx, s, "hi") },
			noop:     func(c *Client, ctx context.Context, s string) error { return c.Nudge(ctx, s, "") },
			wantPath: "/agent/nudge",
		},
		{
			name:     "SendInput",
			call:     func(c *Client, ctx context.Context, s string) error { return c.SendInput(ctx, s, "x") },
			noop:     func(c *Client, ctx context.Context, s string) error { return c.SendInput(ctx, s, "") },
			wantPath: "/input",
		},
		{
			name:     "Resize",
			call:     func(c *Client, ctx context.Context, s string) error { return c.Resize(ctx, s, 120, 40) },
			noop:     func(c *Client, ctx context.Context, s string) error { return c.Resize(ctx, s, 0, 40) },
			wantPath: "/resize",
		},
	}
}

func TestSendMouse(t *testing.T) {
	t.Parallel()
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotPath, gotBody = r.URL.Path, string(b)
		_, _ = w.Write([]byte(`{"bytes_written":9}`))
	}))
	defer srv.Close()
	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	n, err := c.SendMouse(context.Background(), "s1", MouseEvent{Button: "left", Col: 10, Row: 5, Release: true})
	if err != nil || n != 9 {
		t.Fatalf("SendMouse = %d, %v; want 9, nil", n, err)
	}
	if gotPath != "/api/v1/sessions/s1/input/mouse" || gotBody != `{"button":"left","col":10,"row":5,"release":true}` {
		t.Errorf("posted %s %s", gotPath, gotBody)
	}
	if _, err := New(Config{}).SendMouse(context.Background(), "s1", MouseEvent{}); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled err = %v, want ErrDisabled", err)
	}
}

func TestWriteMethodsDisabled(t *testing.T) {
	t.Parallel()
	c := New(Config{}) // disabled
	for _, m := range writeMethods() {
		t.Run(m.name, func(t *testing.T) {
			if err := m.call(c, context.Background(), "s"); !errors.Is(err, ErrDisabled) {
				t.Errorf("%s err = %v, want ErrDisabled", m.name, err)
			}
		})
	}
	// Respond has no empty-payload no-op; check it too.
	if err := c.Respond(context.Background(), "s", RespondRequest{}); !errors.Is(err, ErrDisabled) {
		t.Errorf("Respond err = %v, want ErrDisabled", err)
	}
}

func TestWriteMethodsEmptySessionID(t *testing.T) {
	t.Parallel()
	c := New(Config{BaseURL: "http://mux"})
	for _, m := range writeMethods() {
		t.Run(m.name, func(t *testing.T) {
			err := m.call(c, context.Background(), "")
			if err == nil || !strings.Contains(err.Error(), "empty session id") {
				t.Errorf("%s err = %v, want empty session id error", m.name, err)
			}
		})
	}
	if err := c.Respond(context.Background(), "", RespondRequest{}); err == nil ||
		!strings.Contains(err.Error(), "empty session id") {
		t.Errorf("Respond err = %v, want empty session id error", err)
	}
}

func TestWriteMethodsNoOpDoesNotHitServer(t *testing.T) {
	t.Parallel()
	for _, m := range writeMethods() {
		t.Run(m.name, func(t *testing.T) {
			hit := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hit = true
			}))
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
			if err := m.noop(c, context.Background(), "s"); err != nil {
				t.Errorf("%s no-op err = %v, want nil", m.name, err)
			}
			if hit {
				t.Errorf("%s empty payload should not issue an HTTP request", m.name)
			}
		})
	}
}

func TestWriteMethodsHappyPath(t *testing.T) {
	t.Parallel()
	for _, m := range writeMethods() {
		t.Run(m.name, func(t *testing.T) {
			var gotPath, gotMethod, gotCT string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				gotCT = r.Header.Get("Content-Type")
				w.WriteHeader(http.StatusOK)
			}))
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, Token: "t", HTTPClient: srv.Client()})
			if err := m.call(c, context.Background(), "sess"); err != nil {
				t.Fatalf("%s err = %v", m.name, err)
			}
			if gotMethod != http.MethodPost {
				t.Errorf("%s method = %s, want POST", m.name, gotMethod)
			}
			if !strings.HasSuffix(gotPath, m.wantPath) {
				t.Errorf("%s path = %q, want suffix %q", m.name, gotPath, m.wantPath)
			}
			if gotCT != "application/json" {
				t.Errorf("%s content-type = %q, want application/json", m.name, gotCT)
			}
		})
	}
}

func TestWriteMethodsNotFound(t *testing.T) {
	t.Parallel()
	for _, m := range writeMethods() {
		t.Run(m.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNotFound)
			}))
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
			if err := m.call(c, context.Background(), "s"); !errors.Is(err, ErrSessionNotFound) {
				t.Errorf("%s err = %v, want ErrSessionNotFound", m.name, err)
			}
		})
	}
}

func TestWriteMethodsServerError(t *testing.T) {
	t.Parallel()
	for _, m := range writeMethods() {
		t.Run(m.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusBadGateway)
				_, _ = io.WriteString(w, "upstream down")
			}))
			defer srv.Close()
			c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
			err := m.call(c, context.Background(), "s")
			if err == nil || !strings.Contains(err.Error(), "502") {
				t.Errorf("%s err = %v, want 502 status error", m.name, err)
			}
		})
	}
}

func TestRespondSendsBodyFields(t *testing.T) {
	t.Parallel()
	accept := true
	opt := 2
	txt := "answer"
	var got RespondRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/agent/respond") {
			t.Errorf("path = %q, want /agent/respond suffix", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	err := c.Respond(context.Background(), "s", RespondRequest{Accept: &accept, Option: &opt, Text: &txt})
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got.Accept == nil || *got.Accept != true {
		t.Errorf("Accept = %v, want true", got.Accept)
	}
	if got.Option == nil || *got.Option != 2 {
		t.Errorf("Option = %v, want 2", got.Option)
	}
	if got.Text == nil || *got.Text != "answer" {
		t.Errorf("Text = %v, want answer", got.Text)
	}
}

func TestRespondOmitsNilFields(t *testing.T) {
	t.Parallel()
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &raw)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, HTTPClient: srv.Client()})
	accept := false
	if err := c.Respond(context.Background(), "s", RespondRequest{Accept: &accept}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if _, ok := raw["text"]; ok {
		t.Error("nil Text should be omitted from JSON body")
	}
	if _, ok := raw["option"]; ok {
		t.Error("nil Option should be omitted from JSON body")
	}
	if _, ok := raw["accept"]; !ok {
		t.Error("non-nil Accept should be present in JSON body")
	}
}

func TestCredentialPool(t *testing.T) {
	t.Parallel()
	// The shape coopmux's credentials_pool answers (http_cred.rs), one account probed and one not.
	const body = `{"accounts":[
		{"name":"max-a","provider":"claude","status":"healthy","session_count":3,"cumulative_assignments":41,"has_api_key":true,
		 "throttle_status":"allowed","utilization_5h":"0.4200","utilization_7d":"0.1000","throttle_reset_at":"2026-10-26T05:00:00Z","representative_claim":"five_hour"},
		{"name":"max-b","provider":"claude","status":"throttled","session_count":0,"cumulative_assignments":7,"has_api_key":true,"expires_at":1792980000}],
		"total_sessions":3,"healthy_accounts":1,"total_accounts":2,"strategy":"least_loaded","rotator_cursor":null,"rotator_next_index":null,
		"auto_assign_disabled":true}`
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.Method+" "+r.URL.Path
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	p, err := New(Config{BaseURL: srv.URL, Token: "svc"}).CredentialPool(context.Background())
	if err != nil {
		t.Fatalf("CredentialPool: %v", err)
	}
	if gotPath != "GET /api/v1/credentials/pool" || gotAuth != "Bearer svc" {
		t.Errorf("request = %q with %q, want GET /api/v1/credentials/pool with the service bearer", gotPath, gotAuth)
	}
	if len(p.Accounts) != 2 || p.Strategy != "least_loaded" || p.TotalSessions != 3 || p.HealthyAccounts != 1 || !p.AutoAssignDisabled || p.RotatorCursor != nil {
		t.Fatalf("pool = %+v", p)
	}
	a, b := p.Accounts[0], p.Accounts[1]
	if a.Name != "max-a" || a.SessionCount != 3 || a.CumulativeAssignments != 41 || a.Utilization5h != "0.4200" || a.ThrottleResetAt != "2026-10-26T05:00:00Z" {
		t.Errorf("first account = %+v", a)
	}
	if b.Status != "throttled" || b.ExpiresAt != 1792980000 || b.Utilization5h != "" {
		t.Errorf("second account = %+v", b)
	}
}

func TestCredentialPoolErrors(t *testing.T) {
	t.Parallel()
	if _, err := New(Config{}).CredentialPool(context.Background()); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled client: err = %v, want ErrDisabled", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "admin role required", http.StatusForbidden)
	}))
	defer srv.Close()
	_, err := New(Config{BaseURL: srv.URL}).CredentialPool(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("refused: err = %v, want the status named", err)
	}
}
