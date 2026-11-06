package beadsapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWindowLive_CarriesOnlyTheWindowToken: the stream goes out with the
// window token as its only credential, whatever the client and ctx hold, and
// its body is the daemon's event stream.
func TestWindowLive_CarriesOnlyTheWindowToken(t *testing.T) {
	c, got := windowServer(t, http.StatusOK, "event: view\ndata: {}\n\n")
	c.SetRequireUserBearerForReads(true)
	ctx := WithActor(WithUserBearer(context.Background(), "human-oidc-token"), "someone@example.com")
	body, err := c.WindowLive(ctx, testWindowToken)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(body)
	if err := body.Close(); err != nil {
		t.Error(err)
	}
	if string(b) != "event: view\ndata: {}\n\n" {
		t.Errorf("body = %q", b)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/v1/windows/self/live" {
		t.Errorf("request = %s %s", got.Method, got.URL.Path)
	}
	if a := got.Header.Get("Authorization"); a != "Bearer "+testWindowToken {
		t.Errorf("Authorization = %q; want the window token", a)
	}
	for _, h := range []string{"X-Actor", "X-Window-Views", "X-Operator"} {
		if v := got.Header.Get(h); v != "" {
			t.Errorf("%s = %q went out with the window token", h, v)
		}
	}
}

func TestWindowLive_Refusals(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusNotFound, http.StatusTooManyRequests} {
		c, _ := windowServer(t, code, `{"error":"no"}`)
		_, err := c.WindowLive(context.Background(), testWindowToken)
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != code {
			t.Errorf("HTTP %d: err = %v, want an *APIError with that status", code, err)
		}
	}
	c, _ := windowServer(t, http.StatusOK, "")
	if _, err := c.WindowLive(context.Background(), ""); !errors.Is(err, ErrMachineAuthUnavailable) {
		t.Errorf("empty token: %v", err)
	}
}

// TestWindowLive_EndsWithCtx: cancelling the caller's ctx ends a stream the
// daemon holds open.
func TestWindowLive_EndsWithCtx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{HTTPAddr: srv.URL, Token: "static-daemon-token"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	body, err := c.WindowLive(ctx, testWindowToken)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	cancel()
	if _, err := io.ReadAll(body); err == nil {
		t.Error("the stream read to a clean EOF after ctx was cancelled; want an error")
	}
}
