package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// testJWT builds an unsigned compact JWT; bearerExpiry only reads the payload.
func testJWT(payload string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(payload)) + "." + enc([]byte("sig"))
}

func TestBearerExpiry(t *testing.T) {
	exp := time.Now().Add(time.Hour).Unix()
	got, ok := bearerExpiry(testJWT(fmt.Sprintf(`{"exp":%d}`, exp)))
	if !ok || got.Unix() != exp {
		t.Fatalf("bearerExpiry = %v, %v; want %d, true", got, ok, exp)
	}
	for name, tok := range map[string]string{
		"empty":      "",
		"opaque":     "ya29.opaque-token",
		"no exp":     testJWT(`{"sub":"u"}`),
		"zero exp":   testJWT(`{"exp":0}`),
		"bad base64": "a.!!!.c",
		"bad json":   testJWT(`not json`),
	} {
		if _, ok := bearerExpiry(tok); ok {
			t.Errorf("%s: bearerExpiry ok, want not ok", name)
		}
	}
}

func TestCtxUntilBearerExpiry(t *testing.T) {
	soon := time.Now().Add(150*time.Millisecond).Unix() + 1 // exp has 1s resolution
	ctx := beadsapi.WithUserBearer(context.Background(), testJWT(fmt.Sprintf(`{"exp":%d}`, soon)))
	derived, cancel := ctxUntilBearerExpiry(ctx)
	defer cancel()
	dl, ok := derived.Deadline()
	if !ok || dl.Unix() != soon {
		t.Fatalf("deadline = %v, %v; want exp %d", dl, ok, soon)
	}
	// The user bearer must survive derivation: re-renders still read as the user.
	if beadsapi.UserBearerFromContext(derived) == "" {
		t.Fatal("derived ctx lost the user bearer")
	}

	noExp, cancel2 := ctxUntilBearerExpiry(beadsapi.WithUserBearer(context.Background(), "opaque"))
	defer cancel2()
	if _, ok := noExp.Deadline(); ok {
		t.Fatal("opaque bearer must not impose a deadline")
	}
}

// TestSSEStreamsEndWhenTokenExpires drives each long-lived SSE handler against
// a daemon stream that never closes and never sends. With a bearer that
// expires, the handler must hang up by itself (so the browser reconnects with a
// fresh token); with a bearer that does not, it must keep streaming.
func TestSSEStreamsEndWhenTokenExpires(t *testing.T) {
	routes := []struct {
		name, path string
		seed       func(f *fakeDaemon)
	}{
		{"plan", "/plans/gd-plan/events", func(f *fakeDaemon) {
			f.beads["gd-plan"] = &beadsapi.BeadDetail{ID: "gd-plan", Type: "epic", Status: "open"}
		}},
		{"agent", "/agents/gd-ag/events", func(f *fakeDaemon) {
			f.beads["gd-ag"] = &beadsapi.BeadDetail{ID: "gd-ag", Type: "agent", Status: "in_progress"}
		}},
		{"molecule", "/molecule/gd-mol/events", func(f *fakeDaemon) {
			f.beads["gd-mol"] = &beadsapi.BeadDetail{ID: "gd-mol", Type: "molecule", Status: "open"}
		}},
		{"agents index", "/agents/events", func(*fakeDaemon) {}},
		{"feed", "/feed/events", func(*fakeDaemon) {}},
	}
	for _, rt := range routes {
		t.Run(rt.name, func(t *testing.T) {
			serve := func(t *testing.T, bearer string, reqCtx context.Context) (done <-chan struct{}) {
				t.Helper()
				f := newFake()
				f.eventStreamCh = make(chan beadsapi.SSEEvent) // open forever, silent
				rt.seed(f)
				mux := mkAgentServer(t, f, &fakeScreenClient{})
				req := httptest.NewRequest(http.MethodGet, rt.path, nil).
					WithContext(beadsapi.WithUserBearer(reqCtx, bearer))
				ch := make(chan struct{})
				go func() {
					defer close(ch)
					mux.ServeHTTP(httptest.NewRecorder(), req)
				}()
				return ch
			}

			// Expires within ~1-2s (exp is whole seconds).
			exp := time.Now().Add(time.Second).Unix()
			done := serve(t, testJWT(fmt.Sprintf(`{"exp":%d}`, exp)), context.Background())
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stream still open well after the token expired")
			}

			// No exp: stays open until the client goes away.
			reqCtx, cancel := context.WithCancel(context.Background())
			done = serve(t, "opaque-token", reqCtx)
			select {
			case <-done:
				t.Fatal("stream with a non-expiring bearer closed on its own")
			case <-time.After(1500 * time.Millisecond):
			}
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not end when the client went away")
			}
		})
	}
}
