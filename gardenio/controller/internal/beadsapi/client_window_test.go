package beadsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

const testWindowToken = "gw_AAAAAAAAAAAAAAAAAAAAAA"

// windowServer records the one request WindowView sends and answers it.
func windowServer(t *testing.T, status int, body string) (*Client, *http.Request) {
	t.Helper()
	got := new(http.Request)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = *r.Clone(context.Background())
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	c, err := New(Config{HTTPAddr: srv.URL, Token: "static-daemon-token"})
	if err != nil {
		t.Fatal(err)
	}
	return c, got
}

// TestWindowView_CarriesOnlyTheWindowToken pins PW.3's credential: the window
// token goes out as the bearer with both user-bearer flags off (the chart
// default) and with them on, never the client's static token, and no user
// bearer or actor on the caller's ctx goes with it.
func TestWindowView_CarriesOnlyTheWindowToken(t *testing.T) {
	for _, flags := range []struct {
		name        string
		reads, writ bool
	}{{"flags off", false, false}, {"flags on", true, true}} {
		t.Run(flags.name, func(t *testing.T) {
			c, got := windowServer(t, http.StatusOK, `{"plan":{"title":"T"}}`)
			c.SetRequireUserBearerForReads(flags.reads)
			c.SetRequireUserBearerForWrites(flags.writ)
			ctx := WithActor(WithUserBearer(context.Background(), "human-oidc-token"), "someone@example.com")
			body, err := c.WindowView(ctx, testWindowToken, 3)
			if err != nil {
				t.Fatal(err)
			}
			if string(body) != `{"plan":{"title":"T"}}` {
				t.Errorf("body = %s", body)
			}
			if got.Method != http.MethodGet || got.URL.Path != "/v1/windows/self/view" {
				t.Errorf("request = %s %s", got.Method, got.URL.Path)
			}
			if a := got.Header.Get("Authorization"); a != "Bearer "+testWindowToken {
				t.Errorf("Authorization = %q; want the window token", a)
			}
			if a := got.Header.Get("X-Actor"); a != "" {
				t.Errorf("X-Actor = %q; the caller's actor went out with the window token", a)
			}
			if v := got.Header.Get("X-Window-Views"); v != "3" {
				t.Errorf("X-Window-Views = %q, want 3", v)
			}
		})
	}
}

func TestWindowView_ZeroViewsAndRefusals(t *testing.T) {
	c, got := windowServer(t, http.StatusOK, `{}`)
	if _, err := c.WindowView(context.Background(), testWindowToken, -2); err != nil {
		t.Fatal(err)
	}
	if v := got.Header.Get("X-Window-Views"); v != "0" {
		t.Errorf("X-Window-Views = %q, want 0 for a read no visitor opened", v)
	}

	c, _ = windowServer(t, http.StatusUnauthorized, `{"error":"invalid token"}`)
	_, err := c.WindowView(context.Background(), testWindowToken, 1)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("err = %v; want the daemon's 401 as an *APIError", err)
	}

	if _, err := c.WindowView(context.Background(), "", 1); err == nil {
		t.Error("an empty token was sent")
	}
}

// TestWindowView_OtherCallsCarryNoViews: the views header is WindowView's
// alone.
func TestWindowView_OtherCallsCarryNoViews(t *testing.T) {
	c, got := windowServer(t, http.StatusOK, `{"operator_id":"x"}`)
	if _, err := c.Whoami(context.Background()); err != nil {
		t.Fatal(err)
	}
	if v, ok := got.Header["X-Window-Views"]; ok {
		t.Errorf("Whoami sent X-Window-Views %v", v)
	}
}
