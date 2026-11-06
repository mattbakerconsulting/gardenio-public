package main

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
)

// startEmbeddedNATSForFailover spins up an in-process NATS server on a random
// port and returns its ClientURL. Shut down via t.Cleanup.
func startEmbeddedNATSForFailover(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		ns.Shutdown()
		t.Fatal("embedded NATS not ready within 5s")
	}
	t.Cleanup(func() { ns.Shutdown() })
	return ns.ClientURL()
}

// noFallbackCollector subscribes to gardenio.rotation.no_fallback and records
// every event it sees, so a test can assert presence/absence.
type noFallbackCollector struct {
	mu     sync.Mutex
	events []rotationEvent
	conn   *nats.Conn
}

func newNoFallbackCollector(t *testing.T, natsURL string) *noFallbackCollector {
	t.Helper()
	conn, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatalf("collector nats.Connect: %v", err)
	}
	c := &noFallbackCollector{conn: conn}
	if _, err := conn.Subscribe("gardenio.rotation.no_fallback", func(msg *nats.Msg) {
		var evt rotationEvent
		if err := json.Unmarshal(msg.Data, &evt); err != nil {
			return
		}
		c.mu.Lock()
		c.events = append(c.events, evt)
		c.mu.Unlock()
	}); err != nil {
		t.Fatalf("collector subscribe: %v", err)
	}
	// Ensure the subscription is registered server-side before tests publish.
	if err := conn.Flush(); err != nil {
		t.Fatalf("collector flush: %v", err)
	}
	t.Cleanup(func() { conn.Close() })
	return c
}

func (c *noFallbackCollector) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.events)
}

// settle gives async NATS delivery a brief window to land before asserting.
// The notifier publishes synchronously from Handle, but delivery to the
// collector is async, so we flush both ends and yield.
func (c *noFallbackCollector) settle(t *testing.T) {
	t.Helper()
	if err := c.conn.Flush(); err != nil {
		t.Fatalf("settle flush: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
}

// TestAuthFailover_TerminalHealthy_NoNoFallbackEvent is the core regression for
// ka-0000005652: a project sitting on the TERMINAL account of its chain, where
// that account is healthy/active, must NOT emit a no_fallback event. Reaching
// the end of the chain with a healthy current account means "we are good".
func TestAuthFailover_TerminalHealthy_NoNoFallbackEvent(t *testing.T) {
	natsURL := startEmbeddedNATSForFailover(t)
	collector := newNoFallbackCollector(t, natsURL)

	// Single-account chain: terminal account is active, no fallback configured.
	// Mirrors litellm-bedrock being the chain terminal for project=monorepo.
	store := newMockStore(
		account("gd-term", "litellm-bedrock", "monorepo", map[string]any{
			"provider": "litellm",
			"status":   "active",
			// no fallback field — this IS the terminal of the chain
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.rotation = newRotationNotifier(natsURL, "", "", testFailoverLogger(), nil)
	defer f.rotation.Close()

	if err := f.Handle(context.Background(), "monorepo", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	collector.settle(t)
	if got := collector.count(); got != 0 {
		t.Errorf("terminal-healthy account must not emit no_fallback, got %d event(s)", got)
	}

	// And the account must NOT be marked exhausted — no rotation happened.
	if status := store.get("gd-term").Fields["status"]; status != "active" {
		t.Errorf("terminal account status = %v, want active (no rotation)", status)
	}
}

// TestAuthFailover_CurrentUnhealthyNoSuccessor_EmitsNoFallback verifies the
// alert still fires when the current account itself can no longer serve traffic
// AND there is no healthy successor in the chain — the genuine no_fallback case.
func TestAuthFailover_CurrentUnhealthyNoSuccessor_EmitsNoFallback(t *testing.T) {
	natsURL := startEmbeddedNATSForFailover(t)
	collector := newNoFallbackCollector(t, natsURL)

	// Current account is exhausted + rejected (cannot serve), and its only
	// successor is also exhausted + rejected (no healthy candidate exists).
	store := newMockStore(
		account("gd-cur", "primary", "myproj", map[string]any{
			"fallback":        "backup",
			"status":          "exhausted",
			"throttle_status": "rejected",
		}),
		account("gd-bak", "backup", "myproj", map[string]any{
			"status":          "exhausted",
			"throttle_status": "rejected",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 0, 0)
	f.rotation = newRotationNotifier(natsURL, "", "", testFailoverLogger(), nil)
	defer f.rotation.Close()

	// Seed runtime state so Current = the unhealthy primary.
	f.runtime.Set("myproj", failoverState{
		Primary: "primary",
		Current: "primary",
	})

	if err := f.Handle(context.Background(), "myproj", "gd-agent1"); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	collector.settle(t)
	if got := collector.count(); got != 1 {
		t.Errorf("current-unhealthy-no-successor must emit exactly one no_fallback, got %d", got)
	}
}

// TestCurrentAccountUsable exercises the helper that gates the no_fallback
// suppression directly.
func TestCurrentAccountUsable(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   bool
	}{
		{"active", map[string]string{"status": "active"}, true},
		{"active allowed", map[string]string{"status": "active", "throttle_status": "allowed"}, true},
		{"active rejected", map[string]string{"status": "active", "throttle_status": "rejected"}, true},
		{"exhausted allowed", map[string]string{"status": "exhausted", "throttle_status": "allowed"}, true},
		{"exhausted allowed_warning", map[string]string{"status": "exhausted", "throttle_status": "allowed_warning"}, true},
		{"exhausted rejected", map[string]string{"status": "exhausted", "throttle_status": "rejected"}, false},
		{"exhausted empty throttle", map[string]string{"status": "exhausted"}, false},
		{"disabled", map[string]string{"status": "disabled", "throttle_status": "allowed"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := currentAccountUsable(&beadsapi.BeadDetail{Fields: c.fields})
			if got != c.want {
				t.Errorf("currentAccountUsable(%v) = %v, want %v", c.fields, got, c.want)
			}
		})
	}
	if currentAccountUsable(nil) {
		t.Error("currentAccountUsable(nil) should be false")
	}
}
