package eventbus

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATS creates an embedded NATS server with JetStream for testing.
func startEmbeddedNATS(t *testing.T) (*server.Server, nats.JetStreamContext) {
	t.Helper()
	opts := &server.Options{
		Host:      "127.0.0.1",
		Port:      -1, // random port
		JetStream: true,
		StoreDir:  t.TempDir(),
	}
	ns, err := server.NewServer(opts)
	if err != nil {
		t.Fatalf("failed to create NATS server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server not ready")
	}
	t.Cleanup(func() { ns.Shutdown() })

	nc, err := nats.Connect(ns.ClientURL())
	if err != nil {
		t.Fatalf("failed to connect to NATS: %v", err)
	}
	t.Cleanup(func() { nc.Close() })

	js, err := nc.JetStream()
	if err != nil {
		t.Fatalf("failed to create JetStream context: %v", err)
	}

	return ns, js
}

func TestEnsureStreams(t *testing.T) {
	_, js := startEmbeddedNATS(t)

	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams failed: %v", err)
	}

	// Verify all streams were created. Every stream except MESSAGE_EVENTS keeps
	// the historical count/byte caps; MESSAGE_EVENTS gets the per-subject
	// retention floor (MaxMsgs=-1, per-subject cap, byte backstop, MaxAge).
	for _, name := range StreamNames {
		jsName := StreamNameForJetStream(name)
		info, err := js.StreamInfo(jsName)
		if err != nil {
			t.Errorf("stream %s (%s) not found: %v", name, jsName, err)
			continue
		}
		if jsName == StreamMessageEvents {
			c := info.Config
			if c.MaxMsgs != -1 {
				t.Errorf("MESSAGE_EVENTS MaxMsgs = %d, want -1 (unlimited)", c.MaxMsgs)
			}
			if c.MaxMsgsPerSubject != messageStreamMaxMsgsPerSubject {
				t.Errorf("MESSAGE_EVENTS MaxMsgsPerSubject = %d, want %d", c.MaxMsgsPerSubject, messageStreamMaxMsgsPerSubject)
			}
			if c.MaxBytes != messageStreamMaxBytes {
				t.Errorf("MESSAGE_EVENTS MaxBytes = %d, want %d", c.MaxBytes, messageStreamMaxBytes)
			}
			if c.MaxAge != messageStreamMaxAge {
				t.Errorf("MESSAGE_EVENTS MaxAge = %v, want %v", c.MaxAge, messageStreamMaxAge)
			}
			continue
		}
		if info.Config.MaxMsgs != defaultStreamMaxMsgs {
			t.Errorf("stream %s MaxMsgs = %d, want %d", name, info.Config.MaxMsgs, defaultStreamMaxMsgs)
		}
	}

	// Calling EnsureStreams again should be idempotent.
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams (second call) failed: %v", err)
	}
}

// TestEnsureStreamsMigratesMessageEventsRetention is the migration guard: it
// pre-seeds MESSAGE_EVENTS with the OLD create-only config (MaxMsgs=10000 /
// MaxBytes=100MB, no per-subject floor, no MaxAge), publishes an assistant_text
// message, then runs EnsureStreams and asserts the new per-subject retention
// config landed on the EXISTING stream AND the already-stored message survived.
// This is the exact prod scenario: EnsureStreams used to be create-only, so a
// live stream would keep its old config forever without the UpdateStream path.
func TestEnsureStreamsMigratesMessageEventsRetention(t *testing.T) {
	ns, js := startEmbeddedNATS(t)
	_ = ns

	// Pre-seed the OLD config, exactly as create-only EnsureStreams once wrote it.
	if _, err := js.AddStream(&nats.StreamConfig{
		Name:     StreamMessageEvents,
		Subjects: []string{SubjectMessagePrefix + ">"},
		Storage:  nats.FileStorage,
		MaxMsgs:  10000,
		MaxBytes: 100 << 20,
	}); err != nil {
		t.Fatalf("pre-seed old MESSAGE_EVENTS: %v", err)
	}

	// Publish an assistant_text message onto the old stream.
	subject := SubjectForMessageEvent("assistant_text", "bright-hog")
	if _, err := js.Publish(subject, []byte(`{"event_type":"assistant_text","text":"hello"}`)); err != nil {
		t.Fatalf("publish assistant_text: %v", err)
	}

	before, err := js.StreamInfo(StreamMessageEvents)
	if err != nil {
		t.Fatalf("StreamInfo before: %v", err)
	}
	if before.State.Msgs != 1 {
		t.Fatalf("expected 1 stored msg before migration, got %d", before.State.Msgs)
	}

	// Run the reconciler — should UpdateStream the existing stream in place.
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams (migration): %v", err)
	}

	after, err := js.StreamInfo(StreamMessageEvents)
	if err != nil {
		t.Fatalf("StreamInfo after: %v", err)
	}
	c := after.Config
	if c.MaxMsgs != -1 {
		t.Errorf("after migration MaxMsgs = %d, want -1", c.MaxMsgs)
	}
	if c.MaxMsgsPerSubject != messageStreamMaxMsgsPerSubject {
		t.Errorf("after migration MaxMsgsPerSubject = %d, want %d", c.MaxMsgsPerSubject, messageStreamMaxMsgsPerSubject)
	}
	if c.MaxBytes != messageStreamMaxBytes {
		t.Errorf("after migration MaxBytes = %d, want %d", c.MaxBytes, messageStreamMaxBytes)
	}
	if c.MaxAge != messageStreamMaxAge {
		t.Errorf("after migration MaxAge = %v, want %v", c.MaxAge, messageStreamMaxAge)
	}
	// The already-stored message MUST survive the retention update (non-destructive).
	if after.State.Msgs != 1 {
		t.Errorf("stored msg did not survive migration: State.Msgs = %d, want 1", after.State.Msgs)
	}
}

// TestEnsureStreamsNoSpuriousUpdates guards the drift gate: after the first
// reconcile brings every stream to its desired config, a second reconcile must
// be a pure no-op — otherwise the nine unchanged streams would be needlessly
// UpdateStream'd on every daemon boot. It asserts this two ways: (1) the config
// reported back is byte-for-byte stable across two reconciles (retentionDrifted
// reports false), and (2) the drift gate itself returns false when a
// freshly-created stream's read-back config is compared against its own desired
// spec — the actual per-boot check, which catches config()/server read-back
// mismatches such as the MaxMsgsPerSubject 0-vs-(-1) normalization footgun.
func TestEnsureStreamsNoSpuriousUpdates(t *testing.T) {
	_, js := startEmbeddedNATS(t)

	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams (first): %v", err)
	}

	// Snapshot every stream's reconciled config.
	first := make(map[string]nats.StreamConfig)
	for _, name := range StreamNames {
		jsName := StreamNameForJetStream(name)
		info, err := js.StreamInfo(jsName)
		if err != nil {
			t.Fatalf("StreamInfo %s: %v", jsName, err)
		}
		first[jsName] = info.Config
	}

	// A second reconcile should be a pure no-op — retentionDrifted must report
	// false for every stream, so no UpdateStream fires and the config is stable.
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams (second): %v", err)
	}
	for _, name := range StreamNames {
		jsName := StreamNameForJetStream(name)
		info, err := js.StreamInfo(jsName)
		if err != nil {
			t.Fatalf("StreamInfo %s (second): %v", jsName, err)
		}
		want := first[jsName]
		if retentionDrifted(info.Config, &want) {
			t.Errorf("stream %s reported drift after a clean reconcile — spurious UpdateStream would fire every boot", jsName)
		}
	}

	// Directly assert the drift gate does not fire against the desired spec on a
	// freshly-reconciled stream (the actual per-boot check).
	for _, s := range streamSpecs {
		info, err := js.StreamInfo(s.name)
		if err != nil {
			t.Fatalf("StreamInfo %s: %v", s.name, err)
		}
		if retentionDrifted(info.Config, s.config()) {
			t.Errorf("stream %s drifts vs its own spec after create — config()/read-back mismatch (e.g. MaxMsgsPerSubject 0-vs-1)", s.name)
		}
	}
}

func TestBusSetJetStream(t *testing.T) {
	_, js := startEmbeddedNATS(t)

	bus := New()
	if bus.JetStreamEnabled() {
		t.Error("new bus should not have JetStream")
	}

	bus.SetJetStream(js)
	if !bus.JetStreamEnabled() {
		t.Error("bus should have JetStream after SetJetStream")
	}
	if bus.JetStream() == nil {
		t.Error("JetStream() should not be nil")
	}

	bus.SetJetStream(nil)
	if bus.JetStreamEnabled() {
		t.Error("bus should not have JetStream after SetJetStream(nil)")
	}
}

func TestBusDispatchPublishesToJetStream(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	bus := New()
	bus.SetJetStream(js)

	event := &Event{
		Type:      EventSessionStart,
		SessionID: "test-session",
		Actor:     "bright-hog",
	}

	result, err := bus.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if result == nil {
		t.Fatal("Dispatch returned nil result")
	}

	// Give JetStream a moment to persist.
	time.Sleep(50 * time.Millisecond)

	// Verify the event was published to the hooks stream.
	info, err := js.StreamInfo(StreamHookEvents)
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.Msgs == 0 {
		t.Error("expected at least 1 message in HOOK_EVENTS stream")
	}
}

func TestPublishRaw(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	bus := New()
	bus.SetJetStream(js)

	payload := MutationEventPayload{
		Type:    "MutationCreate",
		IssueID: "gd-abc123",
		Title:   "Test issue",
	}
	data, _ := json.Marshal(payload)

	bus.PublishRaw(SubjectMutationPrefix+"MutationCreate", data)

	time.Sleep(50 * time.Millisecond)

	info, err := js.StreamInfo(StreamMutationEvents)
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.Msgs == 0 {
		t.Error("expected at least 1 message in MUTATION_EVENTS stream")
	}
}

func TestPublishRawNoJetStream(t *testing.T) {
	bus := New()
	// Should not panic when JetStream is not configured.
	bus.PublishRaw("mutations.MutationCreate", []byte(`{"test":true}`))
}

// TestEnsureStreamsCreatesPlanEventsWithDedupe verifies EnsureStreams stands up
// the PLAN_EVENTS stream with the durable retention + the Duplicates dedupe
// window the plan step-added signal relies on (producer half of P3,
// ka-0000004264).
func TestEnsureStreamsCreatesPlanEventsWithDedupe(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	info, err := js.StreamInfo(StreamPlanEvents)
	if err != nil {
		t.Fatalf("PLAN_EVENTS not created: %v", err)
	}
	if info.Config.Duplicates != planStreamDuplicates {
		t.Errorf("PLAN_EVENTS Duplicates = %v, want %v", info.Config.Duplicates, planStreamDuplicates)
	}
	if info.Config.MaxAge != planStreamMaxAge {
		t.Errorf("PLAN_EVENTS MaxAge = %v, want %v", info.Config.MaxAge, planStreamMaxAge)
	}
	if info.Config.Storage != nats.FileStorage {
		t.Errorf("PLAN_EVENTS Storage = %v, want FileStorage (durable)", info.Config.Storage)
	}
	// The stream subscribes the plan.> subject so plan.step.added lands on it.
	if len(info.Config.Subjects) != 1 || info.Config.Subjects[0] != SubjectPlanPrefix+">" {
		t.Errorf("PLAN_EVENTS Subjects = %v, want [%s>]", info.Config.Subjects, SubjectPlanPrefix)
	}
}

// TestPublishDedupedCollapsesDuplicate is the core spike the task calls out
// (risk #5, at-least-once + dedupe over JetStream): two publishes of the SAME
// Nats-Msg-Id inside the Duplicates window land exactly ONE message on the
// stream, so a re-emit of the same plan-step edge never double-fires the
// consumer's nudge.
func TestPublishDedupedCollapsesDuplicate(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	bus := New()
	bus.SetJetStream(js)

	subject := SubjectPlanPrefix + "step.added"
	msgID := "plan-step-added:gd-plan01:gd-step01"
	data := []byte(`{"plan_id":"gd-plan01","step_id":"gd-step01"}`)

	// Publish the same logical event three times with the same id — a
	// double-write + a reconnect replay + an ON CONFLICT re-insert.
	bus.PublishDeduped(subject, msgID, data)
	bus.PublishDeduped(subject, msgID, data)
	bus.PublishDeduped(subject, msgID, data)

	time.Sleep(100 * time.Millisecond)

	info, err := js.StreamInfo(StreamPlanEvents)
	if err != nil {
		t.Fatalf("StreamInfo: %v", err)
	}
	if info.State.Msgs != 1 {
		t.Errorf("PLAN_EVENTS has %d messages after 3 same-id publishes, want 1 (dedupe collapsed duplicates)", info.State.Msgs)
	}

	// A DIFFERENT step id under the same plan is a distinct add — it must NOT be
	// deduped away.
	bus.PublishDeduped(subject, "plan-step-added:gd-plan01:gd-step02", data)
	time.Sleep(100 * time.Millisecond)
	info, err = js.StreamInfo(StreamPlanEvents)
	if err != nil {
		t.Fatalf("StreamInfo (second): %v", err)
	}
	if info.State.Msgs != 2 {
		t.Errorf("PLAN_EVENTS has %d messages after a distinct-id publish, want 2", info.State.Msgs)
	}
}

// TestPublishDedupedDurableReplay verifies the at-least-once half: a durable
// pull consumer created AFTER the publish still receives the message (it was
// persisted to the file-backed stream, not lost to a fire-and-forget core-NATS
// publish). This is what lets a controller reactor that was down/mid-restart at
// publish time still pick the step up on reconnect.
func TestPublishDedupedDurableReplay(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}
	bus := New()
	bus.SetJetStream(js)

	subject := SubjectPlanPrefix + "step.added"
	bus.PublishDeduped(subject, "plan-step-added:gd-planZ:gd-stepZ", []byte(`{"plan_id":"gd-planZ"}`))
	time.Sleep(50 * time.Millisecond)

	// Subscribe AFTER the publish; DeliverAll replays the persisted message.
	sub, err := js.SubscribeSync(subject, nats.DeliverAll())
	if err != nil {
		t.Fatalf("SubscribeSync: %v", err)
	}
	defer sub.Unsubscribe() //nolint:errcheck // test cleanup

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("expected a replayed message from the durable stream, got: %v", err)
	}
	if !strings.Contains(string(msg.Data), "gd-planZ") {
		t.Errorf("replayed message body = %q, want it to contain the plan id", string(msg.Data))
	}
}

func TestPublishDedupedNoJetStream(t *testing.T) {
	bus := New()
	// Should not panic when JetStream is not configured.
	bus.PublishDeduped("plan.step.added", "id-1", []byte(`{"test":true}`))
}

func TestDispatchWithDecisionEventScoping(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	bus := New()
	bus.SetJetStream(js)

	// Subscribe to the scoped decision subject.
	sub, err := js.SubscribeSync("decisions.bright-hog.>")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	raw := json.RawMessage(`{"requested_by":"bright-hog","decision_id":"d-123"}`)
	event := &Event{
		Type: EventDecisionCreated,
		Raw:  raw,
	}

	_, err = bus.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("NextMsg: %v (event may not have been published to scoped subject)", err)
	}
	if msg.Subject != "decisions.bright-hog.DecisionCreated" {
		t.Errorf("subject = %q, want %q", msg.Subject, "decisions.bright-hog.DecisionCreated")
	}
}

func TestDispatchWithHookEventActorScoping(t *testing.T) {
	_, js := startEmbeddedNATS(t)
	if err := EnsureStreams(js); err != nil {
		t.Fatalf("EnsureStreams: %v", err)
	}

	bus := New()
	bus.SetJetStream(js)

	// Subscribe to agent-scoped hook events.
	sub, err := js.SubscribeSync("hooks.sharp-seal.>")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	event := &Event{
		Type:  EventStop,
		Actor: "sharp-seal",
	}

	_, err = bus.Dispatch(context.Background(), event)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	msg, err := sub.NextMsg(2 * time.Second)
	if err != nil {
		t.Fatalf("NextMsg: %v", err)
	}
	if msg.Subject != "hooks.sharp-seal.Stop" {
		t.Errorf("subject = %q, want %q", msg.Subject, "hooks.sharp-seal.Stop")
	}
}
