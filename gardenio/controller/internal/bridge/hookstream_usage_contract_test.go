package bridge

// hookstream_usage_contract_test.go — end-to-end contract test for the coop
// usage NATS event (task gd-NU1IbgLk5v, half c).
//
// WHY THIS FILE EXISTS (the gap it closes):
//   coop's nats_forward.rs publishes a per-turn usage event on
//   coop.events.messages.<actor>.usage carrying the served `model` and — by the
//   strictly-cost-free contract (ka-0000002983 / epic ka-0000000758) — NO
//   response_cost field. The controller consumes it in HookStream via
//   handleMessageForward -> messageEvent decode -> AgentStreamManager.
//
//   The existing bridge tests (hookstream_e2e_test.go TestE2E_MessageForward_Usage,
//   model_fallback_test.go) drive that path by MARSHALLING the consumer's own
//   messageEvent struct back to JSON. That can never catch a coop/controller
//   contract DRIFT, because it round-trips the controller's field names, not
//   coop's. And nothing asserts the cost-free half: that a payload with NO
//   response_cost decodes cleanly (coop stopped emitting it) AND that even a
//   stray response_cost on the wire is NOT consumed as spend (the controller
//   carries no cost field to read it into — spend is reconciled controller-side
//   from LiteLLM at close, not from the usage event).
//
//   These tests publish the EXACT raw JSON bytes coop emits (hand-written to the
//   coop wire shape, not re-marshaled from messageEvent) through a REAL embedded
//   NATS server and the REAL HookStream subscription on coop.events.messages.>,
//   so a drift in coop's field names, the subject, or the cost-free contract
//   surfaces here.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startEmbeddedNATS boots an in-process NATS server on an ephemeral port and
// returns its client URL. Mirrors the embedded-server pattern already used by
// thinking_hub_test.go / assistantturnminter_replay_test.go.
func startEmbeddedNATS(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{Host: "127.0.0.1", Port: -1}
	ns, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("new embedded nats server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats server not ready within 5s")
	}
	t.Cleanup(ns.Shutdown)
	return ns.ClientURL()
}

// usageContractFixture wires a REAL HookStream (subscribed over a REAL embedded
// NATS connection to coop.events.messages.>) to a mock stream client + a started
// session, so a published coop usage event flows the whole production path:
// NATS -> handleMessageForward -> messageEvent decode -> HandleMessageEvent ->
// stream chunk. A publisher connection on the same server emits coop's raw bytes.
type usageContractFixture struct {
	mock *mockStreamClient
	hs   *HookStream
	pub  *nats.Conn
}

func newUsageContractFixture(t *testing.T, agent string) *usageContractFixture {
	t.Helper()
	url := startEmbeddedNATS(t)

	mock := &mockStreamClient{}
	mgr := NewAgentStreamManager(mock, testLogger())
	// A live streaming session for `agent` so usage dispatch produces a chunk.
	mgr.StartSession(context.Background(), agent, "C-USAGE", "thread-usage", "U-USAGE")

	hs := NewHookStream(HookStreamConfig{
		NatsURL:       url,
		StreamManager: mgr,
		Logger:        testLogger(),
	})
	if err := hs.Start(); err != nil {
		t.Fatalf("hookstream start: %v", err)
	}
	t.Cleanup(hs.Close)

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("publisher connect: %v", err)
	}
	t.Cleanup(pub.Close)

	return &usageContractFixture{mock: mock, hs: hs, pub: pub}
}

// publishUsage emits raw JSON bytes on coop's usage subject for `actor` and waits
// (bounded) for the event to flow the WHOLE path: NATS -> handleMessageForward ->
// decode -> HandleMessageEvent -> a rendered usage chunk. It synchronizes on the
// appendChunks call COUNT increasing (the observable the tests assert on) rather
// than on LastEventAt, which the handler bumps BEFORE it dispatches — so a chunk
// may not yet be appended when LastEventAt advances (a real race, gd-NU1IbgLk5v).
func (f *usageContractFixture) publishUsage(t *testing.T, actor string, rawJSON string) {
	t.Helper()
	before := f.appendChunkCount()
	subject := "coop.events.messages." + actor + ".usage"
	if err := f.pub.Publish(subject, []byte(rawJSON)); err != nil {
		t.Fatalf("publish usage: %v", err)
	}
	if err := f.pub.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if f.appendChunkCount() > before {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("usage event did not render a chunk via the HookStream subscription within 3s")
}

// appendChunkCount is the number of appendChunks calls recorded so far.
func (f *usageContractFixture) appendChunkCount() int {
	n := 0
	for _, c := range f.mock.getCalls() {
		if c.method == "appendChunks" {
			n++
		}
	}
	return n
}

// lastUsageChunk returns the chunksJSON of the most recent appendChunks call, or
// "" if none. Usage renders through sendTaskUpdate -> AppendStreamChunksContext.
func (f *usageContractFixture) lastUsageChunk() string {
	calls := f.mock.getCalls()
	for i := len(calls) - 1; i >= 0; i-- {
		if calls[i].method == "appendChunks" {
			return calls[i].chunksJSON
		}
	}
	return ""
}

// coopUsagePayload builds the EXACT JSON coop's nats_forward.rs emits for a usage
// event: token counts + duration_ms, actor/session/event_type envelope, session
// metadata (agent type + k8s pod_name), and `model` ONLY when non-empty (coop
// omits it otherwise). It carries NO response_cost — coop is strictly cost-free.
// `extra` injects additional top-level keys (used to prove a stray response_cost
// is ignored).
//
// The controller resolves the agent name from k8s.pod_name first, then the top-
// level `agent` (which in coop's real payload is the agent TYPE, e.g. "claude",
// NOT the per-agent identity) — see handleMessageForward. So the per-agent
// identity `actor` is mirrored into k8s.pod_name here, exactly as the pod's coop
// metadata carries it, so the event routes to the started session.
func coopUsagePayload(actor, sessionID, model string, extra map[string]any) string {
	obj := map[string]any{
		"event_type":    "usage",
		"session_id":    sessionID,
		"actor":         actor,
		"agent":         "claude", // coop's `agent` is the agent TYPE, not the identity
		"k8s":           map[string]any{"pod_name": actor},
		"input_tokens":  1000,
		"output_tokens": 500,
		"cache_read":    200,
		"cache_write":   100,
		"duration_ms":   1234,
		"ts":            1785700000000,
	}
	if model != "" {
		obj["model"] = model
	}
	for k, v := range extra {
		obj[k] = v
	}
	b, _ := json.Marshal(obj)
	return string(b)
}

// TestUsageContract_ModelPresent proves a coop usage event carrying the served
// model flows the whole real path (NATS -> decode -> dispatch -> chunk) AND that
// the served model is decoded off coop's exact wire bytes into
// messageEvent.Model — the field the divergence flagger reads. The rendered
// usage chunk (formatUsageSummary) does NOT include the model, so the chunk
// alone proves only dispatch; the direct decode below is what proves the model
// crossed the coop→controller contract intact.
func TestUsageContract_ModelPresent(t *testing.T) {
	const actor = "gardenio-usagecrew-m1"
	const servedModel = "claude-opus-4-8"
	f := newUsageContractFixture(t, actor)

	payload := coopUsagePayload(actor, "sess-usage-m1", servedModel, nil)
	f.publishUsage(t, actor, payload)

	chunk := f.lastUsageChunk()
	if chunk == "" {
		t.Fatal("no usage chunk rendered — coop usage payload did not decode + dispatch")
	}
	if !strings.Contains(chunk, "Token usage") {
		t.Errorf("expected 'Token usage' title in chunk, got %s", chunk)
	}
	// The token counts from coop's payload made it through the decode (1000 in).
	if !strings.Contains(chunk, "1K in") && !strings.Contains(chunk, "1000") {
		t.Errorf("expected input tokens from coop payload in chunk, got %s", chunk)
	}

	// The served model is carried end-to-end: decode coop's EXACT bytes into the
	// consumer struct and assert Model landed (the chunk render drops it, so this
	// is the load-bearing model-contract assertion).
	var evt messageEvent
	if err := json.Unmarshal([]byte(payload), &evt); err != nil {
		t.Fatalf("decode coop usage bytes: %v", err)
	}
	if evt.Model != servedModel {
		t.Errorf("decoded messageEvent.Model = %q, want %q (served model must survive the coop→controller decode)", evt.Model, servedModel)
	}
	if evt.EventType != "usage" {
		t.Errorf("decoded messageEvent.EventType = %q, want usage", evt.EventType)
	}
}

// TestUsageContract_CostFree proves the cost-free contract at the consumer:
//  1. A payload with NO response_cost decodes and dispatches cleanly (coop
//     stopped emitting the field — the consumer must not require it).
//  2. Even a STRAY response_cost on the wire is NOT consumed as spend: the
//     controller's messageEvent has no field to read it into, and the usage
//     render is identical with or without it. Spend is reconciled controller-side
//     from LiteLLM at close, never from the usage event.
func TestUsageContract_CostFree(t *testing.T) {
	const actor = "gardenio-usagecrew-c1"
	f := newUsageContractFixture(t, actor)

	// (1) cost-free payload → renders.
	f.publishUsage(t, actor, coopUsagePayload(actor, "sess-cf-1", "claude-opus-4-8", nil))
	costFreeChunk := f.lastUsageChunk()
	if costFreeChunk == "" || !strings.Contains(costFreeChunk, "Token usage") {
		t.Fatalf("cost-free usage payload did not render a usage chunk, got %q", costFreeChunk)
	}
	// The rendered chunk must carry no dollar figure — usage is token/duration
	// only; a cost would mean the consumer grew a spend concept from the event.
	if strings.Contains(costFreeChunk, "$") {
		t.Errorf("usage chunk unexpectedly contains a cost figure: %s", costFreeChunk)
	}

	// (2) same payload PLUS a stray response_cost → decodes identically, and the
	// injected cost never surfaces (the consumer has no field to consume it).
	f.publishUsage(t, actor, coopUsagePayload(actor, "sess-cf-2", "claude-opus-4-8",
		map[string]any{"response_cost": 0.4242}))
	withCostChunk := f.lastUsageChunk()
	if withCostChunk == "" || !strings.Contains(withCostChunk, "Token usage") {
		t.Fatalf("usage payload with stray response_cost did not render, got %q", withCostChunk)
	}
	if strings.Contains(withCostChunk, "0.4242") || strings.Contains(withCostChunk, "$") {
		t.Errorf("stray response_cost leaked into the usage render: %s", withCostChunk)
	}

	// Decode the raw bytes straight into the consumer struct and confirm there is
	// no cost field that captured the injected value — a belt-and-suspenders check
	// that the wire→struct mapping carries model but no cost.
	var evt messageEvent
	if err := json.Unmarshal([]byte(coopUsagePayload(actor, "sess-cf-3", "claude-opus-4-8",
		map[string]any{"response_cost": 0.99})), &evt); err != nil {
		t.Fatalf("decode coop usage bytes: %v", err)
	}
	if evt.Model != "claude-opus-4-8" {
		t.Errorf("decoded messageEvent.Model = %q, want claude-opus-4-8", evt.Model)
	}
	if evt.EventType != "usage" {
		t.Errorf("decoded messageEvent.EventType = %q, want usage", evt.EventType)
	}
	assertNoCostField(t, evt)
}

// TestUsageContract_ModelAbsent proves a coop usage event that reports no model
// (coop omits the field, e.g. a turn with no served model) decodes with an empty
// Model and still renders the usage chunk — model presence is optional, not
// required, exactly as coop's `if !model.is_empty()` publish guard implies.
func TestUsageContract_ModelAbsent(t *testing.T) {
	const actor = "gardenio-usagecrew-a1"
	f := newUsageContractFixture(t, actor)

	f.publishUsage(t, actor, coopUsagePayload(actor, "sess-usage-a1", "", nil))

	chunk := f.lastUsageChunk()
	if chunk == "" || !strings.Contains(chunk, "Token usage") {
		t.Fatalf("model-absent usage payload did not render a usage chunk, got %q", chunk)
	}

	// Direct decode: Model is empty, everything else present.
	var evt messageEvent
	if err := json.Unmarshal([]byte(coopUsagePayload(actor, "sess-usage-a1", "", nil)), &evt); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if evt.Model != "" {
		t.Errorf("decoded messageEvent.Model = %q, want empty (coop omitted model)", evt.Model)
	}
	if evt.InputTokens != 1000 || evt.OutputTokens != 500 {
		t.Errorf("token counts wrong after decode: in=%d out=%d", evt.InputTokens, evt.OutputTokens)
	}
}

// assertNoCostField re-marshals the decoded messageEvent and asserts the round
// trip carries no cost/spend field — the structural proof that the consumer has
// no place to store a per-turn cost, so coop's cost-free contract is enforced at
// the type level, not just by coop's emitter.
func assertNoCostField(t *testing.T, evt messageEvent) {
	t.Helper()
	b, err := json.Marshal(evt)
	if err != nil {
		t.Fatalf("re-marshal messageEvent: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode re-marshaled messageEvent: %v", err)
	}
	for _, forbidden := range []string{"response_cost", "cost", "cost_usd", "spend", "spend_usd"} {
		if _, present := m[forbidden]; present {
			t.Errorf("messageEvent carries a forbidden cost field %q — the cost-free contract requires none: %s",
				forbidden, string(b))
		}
	}
}
