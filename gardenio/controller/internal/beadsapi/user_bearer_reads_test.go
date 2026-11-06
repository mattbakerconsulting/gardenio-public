package beadsapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// readRecorder is an httptest daemon that records every request's
// Authorization header and answers a bead GET with a minimal body.
type readRecorder struct {
	srv   *httptest.Server
	auths []string
	hits  atomic.Int32
}

func newReadRecorder(t *testing.T) *readRecorder {
	t.Helper()
	rr := &readRecorder{}
	rr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rr.hits.Add(1)
		rr.auths = append(rr.auths, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"gd-x","title":"x","type":"task","status":"open"}`))
	}))
	t.Cleanup(rr.srv.Close)
	return rr
}

// TestReads_UseUserBearerWhenEnforced: with read enforcement on and a user
// bearer in ctx, the read authenticates as the user, not the static token.
func TestReads_UseUserBearerWhenEnforced(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForReads(true)

	ctx := WithUserBearer(context.Background(), "user-jwt")
	if _, err := c.GetBead(ctx, "gd-x"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if len(rr.auths) != 1 || rr.auths[0] != "Bearer user-jwt" {
		t.Fatalf("Authorization = %v, want [Bearer user-jwt]", rr.auths)
	}
}

// TestReads_FailClosedWithoutBearer: no bearer and no system-read marker is an
// error and nothing is sent.
func TestReads_FailClosedWithoutBearer(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForReads(true)

	_, err := c.GetBead(context.Background(), "gd-x")
	if !errors.Is(err, ErrUserBearerRequiredForRead) {
		t.Fatalf("err = %v, want ErrUserBearerRequiredForRead", err)
	}
	if n := rr.hits.Load(); n != 0 {
		t.Fatalf("daemon saw %d requests, want 0 (read must fail closed before sending)", n)
	}
}

// TestReads_FailClosedForReadOnlyPost: the read-only POST endpoints are reads
// for enforcement purposes and are gated like GETs.
func TestReads_FailClosedForReadOnlyPost(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForReads(true)

	err := c.doJSON(context.Background(), http.MethodPost, "/v1/search/semantic", map[string]string{"q": "x"}, nil)
	if !errors.Is(err, ErrUserBearerRequiredForRead) {
		t.Fatalf("err = %v, want ErrUserBearerRequiredForRead", err)
	}
	if n := rr.hits.Load(); n != 0 {
		t.Fatalf("daemon saw %d requests, want 0", n)
	}
}

// TestReads_SystemReadMarkerUsesStaticToken is the positive control: the
// explicit marker lets a human-less read through on the client's own token.
func TestReads_SystemReadMarkerUsesStaticToken(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForReads(true)

	if _, err := c.GetBead(WithSystemRead(context.Background()), "gd-x"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if len(rr.auths) != 1 || rr.auths[0] != "Bearer static-token" {
		t.Fatalf("Authorization = %v, want [Bearer static-token]", rr.auths)
	}
}

// TestReads_UserBearerBeatsSystemMarker: a ctx carrying both reads as the user.
func TestReads_UserBearerBeatsSystemMarker(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForReads(true)

	ctx := WithSystemRead(WithUserBearer(context.Background(), "user-jwt"))
	if _, err := c.GetBead(ctx, "gd-x"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if len(rr.auths) != 1 || rr.auths[0] != "Bearer user-jwt" {
		t.Fatalf("Authorization = %v, want [Bearer user-jwt]", rr.auths)
	}
}

// TestReads_MachinePrincipalPassesEnforcement: a reactor ctx with its own
// machine principal is not a bearer-less read; it keeps the machine identity.
func TestReads_MachinePrincipalPassesEnforcement(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForReads(true)

	ctx := WithMachinePrincipal(context.Background(), staticTokenSource("machine-jwt"))
	if _, err := c.GetBead(ctx, "gd-x"); err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if len(rr.auths) != 1 || rr.auths[0] != "Bearer machine-jwt" {
		t.Fatalf("Authorization = %v, want [Bearer machine-jwt]", rr.auths)
	}
}

// TestReads_EnforcementOffUnchanged: default behaviour is the static token,
// with or without a user bearer in ctx (the write flag alone does not move reads).
func TestReads_EnforcementOffUnchanged(t *testing.T) {
	rr := newReadRecorder(t)
	c := newTestClientWithToken(t, rr.srv, "static-token")
	c.SetRequireUserBearerForWrites(true)

	for _, ctx := range []context.Context{
		context.Background(),
		WithUserBearer(context.Background(), "user-jwt"),
	} {
		if _, err := c.GetBead(ctx, "gd-x"); err != nil {
			t.Fatalf("GetBead: %v", err)
		}
	}
	for i, a := range rr.auths {
		if a != "Bearer static-token" {
			t.Fatalf("read %d Authorization = %q, want static token", i, a)
		}
	}
}
