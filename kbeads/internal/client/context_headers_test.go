package client

import (
	"context"
	"testing"
)

// --- Context-helper round-trips (set/get) ---

func TestWithNoAutoParent_RoundTrip(t *testing.T) {
	base := context.Background()
	if NoAutoParentFromContext(base) {
		t.Error("NoAutoParentFromContext(empty) = true, want false")
	}
	ctx := WithNoAutoParent(base)
	if !NoAutoParentFromContext(ctx) {
		t.Error("NoAutoParentFromContext(marked) = false, want true")
	}
}

func TestWithTriggerBead_RoundTrip(t *testing.T) {
	base := context.Background()
	if got := TriggerBeadFromContext(base); got != "" {
		t.Errorf("TriggerBeadFromContext(empty) = %q, want \"\"", got)
	}
	ctx := WithTriggerBead(base, "gd-ABC123")
	if got := TriggerBeadFromContext(ctx); got != "gd-ABC123" {
		t.Errorf("TriggerBeadFromContext = %q, want 'gd-ABC123'", got)
	}
}

func TestWithTriggerBead_EmptyIsNoop(t *testing.T) {
	base := context.Background()
	// An empty id must return the context unchanged (no value stored).
	ctx := WithTriggerBead(base, "")
	if got := TriggerBeadFromContext(ctx); got != "" {
		t.Errorf("TriggerBeadFromContext(empty id) = %q, want \"\"", got)
	}
}

// --- Header application through sendOnce ---

// TestSendOnce_ContextHeaders verifies the optional context-driven headers are
// emitted on the wire only when their context marker is present.
func TestSendOnce_ContextHeaders(t *testing.T) {
	tests := []struct {
		name       string
		ctx        func(context.Context) context.Context
		wantHeader string
		wantValue  string
	}{
		{
			name:       "no-auto-parent",
			ctx:        WithNoAutoParent,
			wantHeader: "X-No-Auto-Parent",
			wantValue:  "1",
		},
		{
			name:       "trigger-bead",
			ctx:        func(c context.Context) context.Context { return WithTriggerBead(c, "gd-TRIG") },
			wantHeader: "X-Trigger-Bead",
			wantValue:  "gd-TRIG",
		},
		{
			name:       "escalate",
			ctx:        WithEscalate,
			wantHeader: "X-Escalate",
			wantValue:  "true",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &testHandler{responseBody: `{"status":"ok"}`}
			c, srv := newTestClient(h)
			defer srv.Close()

			ctx := tt.ctx(context.Background())
			if _, err := c.Health(ctx); err != nil {
				t.Fatalf("Health() error = %v", err)
			}
			if got := h.headers.Get(tt.wantHeader); got != tt.wantValue {
				t.Errorf("header %s = %q, want %q", tt.wantHeader, got, tt.wantValue)
			}
		})
	}
}

// TestSendOnce_NoOptionalHeadersByDefault confirms a plain context emits none
// of the optional markers.
func TestSendOnce_NoOptionalHeadersByDefault(t *testing.T) {
	h := &testHandler{responseBody: `{"status":"ok"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	for _, hdr := range []string{"X-No-Auto-Parent", "X-Trigger-Bead", "X-System-Operator", "X-Escalate"} {
		if got := h.headers.Get(hdr); got != "" {
			t.Errorf("unexpected header %s = %q, want absent", hdr, got)
		}
	}
}

// TestHTTPClient_SetFleetActor verifies SetFleet/SetActor put the configured
// values on the wire as X-Fleet / X-Actor headers.
func TestHTTPClient_SetFleetActor(t *testing.T) {
	h := &testHandler{responseBody: `{"status":"ok"}`}
	c, srv := newTestClient(h)
	defer srv.Close()

	c.SetFleet("fleet-7")
	c.SetActor("agent-bot")

	if _, err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if got := h.headers.Get("X-Fleet"); got != "fleet-7" {
		t.Errorf("X-Fleet = %q, want 'fleet-7'", got)
	}
	if got := h.headers.Get("X-Actor"); got != "agent-bot" {
		t.Errorf("X-Actor = %q, want 'agent-bot'", got)
	}
}
