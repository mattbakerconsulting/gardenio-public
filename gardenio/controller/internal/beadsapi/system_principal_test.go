package beadsapi

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

// With a system principal installed, a WithSystemRead read and the SSE stream
// go out as that principal, a user-less read without the marker stays on the
// static token, and a token-fetch failure fails the call closed.
func TestSystemPrincipal_SystemReadsAndEventStream(t *testing.T) {
	c, d := newRecordingClient(t, false)
	c.SetRequireUserBearerForReads(true)
	c.SetSystemPrincipal(fixedTokenSource{tok: "system-jwt"})

	_, _ = c.GetBead(WithSystemRead(context.Background()), "gd-x")
	if got := lastAuth(t, d); got != "Bearer system-jwt" {
		t.Fatalf("system read auth = %q, want the system principal", got)
	}

	// A user bearer on a read under read-bearer enforcement still wins.
	uctx := WithUserBearer(WithSystemRead(context.Background()), "user-jwt")
	_, _ = c.GetBead(uctx, "gd-x")
	if got := lastAuth(t, d); got != "Bearer user-jwt" {
		t.Fatalf("user-bearer read auth = %q, want the user's bearer", got)
	}

	// The event stream is system traffic whatever ctx it is opened on.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, _ = c.EventStream(ctx, "beads.bead.>")
	if got := lastAuth(t, d); got != "Bearer system-jwt" {
		t.Fatalf("event stream auth = %q, want the system principal", got)
	}
}

func TestSystemPrincipal_FetchFailureFailsClosed(t *testing.T) {
	d := &recordingDaemon{}
	srv := httptest.NewServer(d.handler())
	t.Cleanup(srv.Close)
	c, err := New(Config{HTTPAddr: srv.URL, Token: "static-read-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.SetSystemPrincipal(fixedTokenSource{err: ErrMachineAuthUnavailable})

	if _, err := c.GetBead(WithSystemRead(context.Background()), "gd-x"); !errors.Is(err, ErrMachineAuthUnavailable) {
		t.Fatalf("system read err = %v, want ErrMachineAuthUnavailable", err)
	}
	if _, err := c.EventStream(context.Background(), "beads.bead.>"); !errors.Is(err, ErrMachineAuthUnavailable) {
		t.Fatalf("event stream err = %v, want ErrMachineAuthUnavailable", err)
	}
	if n := len(d.requests()); n != 0 {
		t.Fatalf("%d requests reached the daemon on a failed credential; want 0 (no static-token fallback)", n)
	}
}
