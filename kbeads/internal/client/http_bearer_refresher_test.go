package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// gd-NrzwHXUxV0: a 401 WITHOUT the daemon's expired-token challenge (what an
// edge proxy in front of the daemon returns) asks the bearer refresher for a
// new token and retries once.
func TestBearerRefresherOn401(t *testing.T) {
	var (
		calls       atomic.Int32
		authHeaders []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte("Unauthorized"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	var refreshes atomic.Int32
	c := NewHTTPClient(srv.URL, "access-1")
	c.SetBearerRefresher(func(context.Context) (string, error) {
		refreshes.Add(1)
		return "access-2", nil
	})
	got, err := c.Health(context.Background())
	if err != nil || got != "ok" {
		t.Fatalf("Health() = %q, %v; want ok", got, err)
	}
	if refreshes.Load() != 1 || calls.Load() != 2 {
		t.Errorf("refreshes = %d, calls = %d; want 1 and 2", refreshes.Load(), calls.Load())
	}
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer access-1" || authHeaders[1] != "Bearer access-2" {
		t.Errorf("auth headers = %q", authHeaders)
	}
}

// A refresher that fails, or hands back the same token, leaves the original
// 401 to surface and does not retry.
func TestBearerRefresherFailureSurfacesOriginal401(t *testing.T) {
	for name, fn := range map[string]func(context.Context) (string, error){
		"error":      func(context.Context) (string, error) { return "", errors.New("keycloak unreachable") },
		"same token": func(context.Context) (string, error) { return "access-1", nil },
		"empty":      func(context.Context) (string, error) { return "", nil },
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("Unauthorized"))
			}))
			defer srv.Close()
			c := NewHTTPClient(srv.URL, "access-1")
			c.SetBearerRefresher(fn)
			_, err := c.Health(context.Background())
			var apiErr *APIError
			if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusUnauthorized {
				t.Fatalf("err = %v; want the original 401", err)
			}
			if calls.Load() != 1 {
				t.Errorf("calls = %d, want 1 (no retry without a new token)", calls.Load())
			}
		})
	}
}

// A second 401 after the retry is not refreshed again: one retry per request.
func TestBearerRefresherRetriesOnce(t *testing.T) {
	var calls, refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, "access-1")
	c.SetBearerRefresher(func(context.Context) (string, error) {
		return "access-" + string(rune('1'+refreshes.Add(1))), nil
	})
	if _, err := c.Health(context.Background()); err == nil {
		t.Fatal("want 401 error")
	}
	if calls.Load() != 2 || refreshes.Load() != 1 {
		t.Errorf("calls = %d, refreshes = %d; want 2 and 1", calls.Load(), refreshes.Load())
	}
}

// The daemon's own expired-token challenge keeps using the refresh-token path;
// the bearer refresher is not consulted for it.
func TestBearerRefresherNotUsedForDaemonChallenge(t *testing.T) {
	var health atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/auth/refresh":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"access_token":"access-2","expires_in":3600,"token_type":"Bearer"}`))
		default:
			if health.Add(1) == 1 {
				w.Header().Set("WWW-Authenticate", expiredTokenChallenge)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}
	}))
	defer srv.Close()
	c := NewHTTPClient(srv.URL, "access-1")
	c.SetRefreshToken("refresh-1")
	bearerCalled := false
	c.SetBearerRefresher(func(context.Context) (string, error) { bearerCalled = true; return "x", nil })
	if got, err := c.Health(context.Background()); err != nil || got != "ok" {
		t.Fatalf("Health() = %q, %v", got, err)
	}
	if bearerCalled {
		t.Error("bearer refresher must not run for the daemon's expired-token challenge")
	}
}
