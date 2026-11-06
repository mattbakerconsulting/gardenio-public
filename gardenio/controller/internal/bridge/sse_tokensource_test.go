package bridge

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSSETokenSource struct {
	tok string
	err error
}

func (f fakeSSETokenSource) Token(context.Context) (string, error) { return f.tok, f.err }

func newSSETestStream(t *testing.T, url string, cfg SSEStreamConfig) *SSEStream {
	t.Helper()
	cfg.BeadsHTTPAddr = url
	cfg.Logger = slog.New(slog.NewTextHandler(&strings.Builder{}, nil))
	return NewSSEStream(cfg)
}

// The machine TokenSource wins over the static Token and supplies the bearer
// for the connection (gd-5QYwXtV7IA: the stream must not need the shared token).
func TestSSEStream_TokenSourceWinsOverStaticToken(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	s := newSSETestStream(t, srv.URL, SSEStreamConfig{Token: "static-secret", TokenSource: fakeSSETokenSource{tok: "machine-jwt"}})
	_ = s.stream(context.Background())
	if got != "Bearer machine-jwt" {
		t.Fatalf("Authorization = %q, want the machine token", got)
	}
}

// A failing TokenSource fails the connect and never substitutes the static token.
func TestSSEStream_TokenSourceErrorDoesNotFallBackToStatic(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	s := newSSETestStream(t, srv.URL, SSEStreamConfig{Token: "static-secret", TokenSource: fakeSSETokenSource{err: errors.New("auth_unavailable")}})
	err := s.stream(context.Background())
	if err == nil || !strings.Contains(err.Error(), "auth_unavailable") {
		t.Fatalf("stream err = %v, want the token error", err)
	}
	if called {
		t.Fatal("request was sent despite the token fetch failing")
	}
}
