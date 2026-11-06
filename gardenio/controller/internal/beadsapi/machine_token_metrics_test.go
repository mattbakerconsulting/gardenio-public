package beadsapi

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// A Token call that fails closed with auth_unavailable is counted per client:
// the missing-Secret case (the mount is optional) and the expired-token case.
// A refresh failure that serves a cached, unexpired token is a success and is
// not counted.
func TestMachineTokenSource_CountsAuthUnavailable(t *testing.T) {
	const client = "metrics-test-client"
	c := machineTokenUnavailableTotal.WithLabelValues(client)
	// The fixture endpoint only accepts its own client id.
	const fixtureClient = "gardenio-controller"
	before := testutil.ToFloat64(c)

	missing := filepath.Join(t.TempDir(), "no-such-secret")
	ts := NewMachineTokenSource(context.Background(), slog.Default(), "http://127.0.0.1:1/token", client, missing)
	for i := 0; i < 2; i++ {
		if _, err := ts.Token(context.Background()); !IsAuthUnavailable(err) {
			t.Fatalf("missing secret: err=%v, want auth_unavailable", err)
		}
	}
	if got := testutil.ToFloat64(c) - before; got != 2 {
		t.Fatalf("auth_unavailable counter delta=%v, want 2", got)
	}

	ep, srv, _ := newFixtureEndpoint(t, 1800)
	file := writeSecretFile(t, t.TempDir(), fixtureSecret)
	ok := NewMachineTokenSource(context.Background(), slog.Default(), srv.URL, fixtureClient, file)
	c = machineTokenUnavailableTotal.WithLabelValues(fixtureClient)
	src := ok.(*machineTokenSource)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	clock := base
	src.now = func() time.Time { return clock }
	if _, err := ok.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	before = testutil.ToFloat64(c)
	ep.fail.Store(true)
	clock = base.Add(1000 * time.Second) // refresh fails, cached token served
	if _, err := ok.Token(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(c) - before; got != 0 {
		t.Fatalf("served-from-cache must not count, delta=%v", got)
	}
	clock = base.Add(1801 * time.Second) // expired: fails closed
	if _, err := ok.Token(context.Background()); !IsAuthUnavailable(err) {
		t.Fatalf("expired: err=%v", err)
	}
	if got := testutil.ToFloat64(c) - before; got != 1 {
		t.Fatalf("expired token must count once, delta=%v", got)
	}
}
