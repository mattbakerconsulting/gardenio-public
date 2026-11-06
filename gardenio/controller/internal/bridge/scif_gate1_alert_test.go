package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"

	slackapi "github.com/slack-go/slack"
)

// capturingGate1Slack records each PostMessageContext call (channel, text,
// thread_ts) so the Gate-1 tests can assert routing, pointer-only content, and
// threading. It returns a fixed non-empty ts so an opening can be threaded onto.
type capturingGate1Slack struct {
	mu       sync.Mutex
	calls    int
	channels []string
	texts    []string
	threads  []string
	ts       string
	err      error
}

func (c *capturingGate1Slack) PostMessageContext(_ context.Context, channelID string, opts ...slackapi.MsgOption) (string, string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.channels = append(c.channels, channelID)
	if _, values, err := slackapi.UnsafeApplyMsgOptions("token", channelID, "https://slack.test/", opts...); err == nil {
		c.texts = append(c.texts, values.Get("text"))
		c.threads = append(c.threads, values.Get("thread_ts"))
	} else {
		c.texts = append(c.texts, "")
		c.threads = append(c.threads, "")
	}
	if c.err != nil {
		return "", "", c.err
	}
	ts := c.ts
	if ts == "" {
		ts = "1700000000.000100"
	}
	return channelID, ts, nil
}

func gate1PendingFields() map[string]string {
	return map[string]string{
		"approval_status": "pending",
		"data_scope":      `{"tenant":"phlth-devel"}`,
		"requested_by":    "alice@acme.example",
		"prompt": "Count patients with condition X across the cohort, stratify by age and site, " +
			"then correlate against the primary outcome variable across every enrolled site and " +
			"produce a full longitudinal report ending with the SECRETTAILMARKER token.",
	}
}

func TestScifGateAlert_Gate1PostsOnPendingCreate(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)
	if a == nil {
		t.Fatal("NewScifGateAlert returned nil")
	}

	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-G1REQ1", scifGateAlertBeadType, nil, gate1PendingFields()))

	if api.calls != 1 {
		t.Fatalf("calls = %d, want 1", api.calls)
	}
	if api.channels[0] != "C-gate" {
		t.Errorf("channel = %q, want C-gate", api.channels[0])
	}
	txt := api.texts[0]
	for _, want := range []string{"Gate-1", "phlth-devel", "alice@acme.example", "gd-G1REQ1", "SCIF portal"} {
		if !strings.Contains(txt, want) {
			t.Errorf("text missing %q:\n%s", want, txt)
		}
	}
	// Pointers only: a long prompt is truncated, so its tail never reaches the
	// channel (guards against a full prompt / trailing identifier leaking).
	if strings.Contains(txt, "SECRETTAILMARKER") {
		t.Errorf("prompt tail leaked into alert (not truncated):\n%s", txt)
	}
	if !strings.Contains(txt, "…") {
		t.Errorf("expected truncation ellipsis in excerpt:\n%s", txt)
	}
}

func TestScifGateAlert_Gate1IgnoresNonPendingAndWrongType(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)

	// approved-at-create (not pending) → no opening post.
	f := gate1PendingFields()
	f["approval_status"] = "approved"
	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-G1REQ2", scifGateAlertBeadType, nil, f))

	// wrong type → no post.
	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-G1REQ3", "task", nil, gate1PendingFields()))

	if api.calls != 0 {
		t.Fatalf("calls = %d, want 0", api.calls)
	}
}

func TestScifGateAlert_Gate1DedupsCreate(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)
	data := marshalBeadEvent(t, "gd-G1REQ4", scifGateAlertBeadType, nil, gate1PendingFields())
	a.handleCreated(context.Background(), data)
	a.handleCreated(context.Background(), data) // SSE reconnect replay
	if api.calls != 1 {
		t.Fatalf("calls = %d, want 1 (dedup)", api.calls)
	}
}

func TestScifGateAlert_Gate1ThreadsApprovalOutcome(t *testing.T) {
	api := &capturingGate1Slack{ts: "1700000000.000999"}
	a := newTestGateAlert(api)

	// Opening remembers the ts.
	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-G1REQ5", scifGateAlertBeadType, nil, gate1PendingFields()))

	// Decision flip: approval_status present in the changes map.
	f := gate1PendingFields()
	f["approval_status"] = "approved"
	f["approved_by"] = "bob@acme.example"
	f["approved_at"] = "2026-09-01T12:00:00Z"
	a.handleUpdated(context.Background(), marshalUpdatedBeadEvent(t, "gd-G1REQ5", scifGateAlertBeadType, f,
		map[string]any{"approval_status": "approved"}))

	if api.calls != 2 {
		t.Fatalf("calls = %d, want 2", api.calls)
	}
	if got := api.threads[1]; got != "1700000000.000999" {
		t.Errorf("outcome thread_ts = %q, want opening ts", got)
	}
	txt := api.texts[1]
	for _, want := range []string{"Approved", "bob@acme.example", "2026-09-01T12:00:00Z"} {
		if !strings.Contains(txt, want) {
			t.Errorf("outcome text missing %q:\n%s", want, txt)
		}
	}
}

func TestScifGateAlert_Gate1DenialStandaloneWhenTSUnknown(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)

	// No prior opening (e.g. bridge restart) → outcome posts standalone.
	f := gate1PendingFields()
	f["approval_status"] = "denied"
	f["denied_by"] = "carol@acme.example"
	f["denied_at"] = "2026-09-01T13:00:00Z"
	f["denial_reason"] = "tenant not authorized"
	a.handleUpdated(context.Background(), marshalUpdatedBeadEvent(t, "gd-G1REQ6", scifGateAlertBeadType, f,
		map[string]any{"approval_status": "denied"}))

	if api.calls != 1 {
		t.Fatalf("calls = %d, want 1", api.calls)
	}
	if api.threads[0] != "" {
		t.Errorf("expected standalone (no thread_ts), got %q", api.threads[0])
	}
	txt := api.texts[0]
	for _, want := range []string{"Denied", "carol@acme.example", "tenant not authorized", "gd-G1REQ6"} {
		if !strings.Contains(txt, want) {
			t.Errorf("denial text missing %q:\n%s", want, txt)
		}
	}
}

func TestScifGateAlert_Gate1UnrelatedUpdateDoesNotFire(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)

	// An update that does NOT touch approval_status (approval_status absent from
	// changes) must not post an outcome, even though the field's current value is
	// a terminal status.
	f := gate1PendingFields()
	f["approval_status"] = "approved"
	a.handleUpdated(context.Background(), marshalUpdatedBeadEvent(t, "gd-G1REQ7", scifGateAlertBeadType, f,
		map[string]any{"launch_directive_written": "true"}))

	if api.calls != 0 {
		t.Fatalf("calls = %d, want 0 (approval_status not in changes)", api.calls)
	}
}

func TestScifGateAlert_Gate1DedupsOutcome(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)
	f := gate1PendingFields()
	f["approval_status"] = "approved"
	data := marshalUpdatedBeadEvent(t, "gd-G1REQ8", scifGateAlertBeadType, f, map[string]any{"approval_status": "approved"})
	a.handleUpdated(context.Background(), data)
	a.handleUpdated(context.Background(), data)
	if api.calls != 1 {
		t.Fatalf("calls = %d, want 1 (outcome dedup)", api.calls)
	}
}

func TestScifGateAlert_Gate1SlackFailureIsSwallowed(t *testing.T) {
	api := &capturingGate1Slack{err: context.DeadlineExceeded}
	a := newTestGateAlert(api)
	// Must not panic and must not record a ts to thread against.
	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-G1REQ9", scifGateAlertBeadType, nil, gate1PendingFields()))
	if got := a.recallTS("gd-G1REQ9"); got != "" {
		t.Errorf("failed post should not remember ts, got %q", got)
	}
}

// TestScifGateAlert_Gate1AndGate2Coexist proves the two gates on the one bead do
// not suppress each other: a Gate-1 opening (pending) then a Gate-2 opening
// (release_status→pending) both post.
func TestScifGateAlert_Gate1AndGate2Coexist(t *testing.T) {
	api := &capturingGate1Slack{}
	a := newTestGateAlert(api)

	a.handleCreated(context.Background(), marshalBeadEvent(t, "gd-BOTH", scifGateAlertBeadType, nil, gate1PendingFields()))
	if api.calls != 1 {
		t.Fatalf("after Gate-1 opening: calls = %d, want 1", api.calls)
	}

	// Later, the same bead's run finishes and awaits release (Gate-2).
	f := gate1PendingFields()
	f["approval_status"] = "approved"
	f["release_status"] = "pending"
	a.handleUpdated(context.Background(), marshalUpdatedBeadEvent(t, "gd-BOTH", scifGateAlertBeadType, f,
		map[string]any{"release_status": "pending"}))
	if api.calls != 2 {
		t.Fatalf("after Gate-2 opening: calls = %d, want 2", api.calls)
	}
	if !strings.Contains(api.texts[1], "awaiting release") {
		t.Errorf("second post is not the Gate-2 opening:\n%s", api.texts[1])
	}
}
