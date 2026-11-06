package assistantturnminter

// assistantturnminter_test.go — the minter's per-event mint path (role filter →
// plan resolution + caching → full-text vs >256KB hydration → dedup-safe mint).
// Rehomed from cmd/beads-viewer's assistant_turn_minter_test.go (task
// ka-0000006891) with self-contained fakes so the package tests without the
// viewer's fakeDaemon. The render-side dedup + transcript-query tests stay in
// the viewer (they exercise viewer render code), covered there.
//
// These tests drive handleMessage directly with a synthesized coop envelope so
// they stay pure (no NATS broker), mirroring how the viewer's ThinkingHub logic
// is unit-tested. The NATS/JetStream replay plumbing is exercised end-to-end in
// assistantturnminter_replay_test.go against the embedded JetStream server.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/plantranscript"
)

// testNow is a fixed clock so the resolver cache TTL is deterministic.
var testNow = time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC)

// --- fakes ---

// fakeDaemon is a minimal in-memory BeadAPI: GetBead reads a seeded map,
// CreateBead records every request and models kbeads migration-029
// resolve-or-create dedup (a repeat IdempotencyKey returns the existing id and
// mints nothing new). mintMu guards the create bookkeeping so the concurrent
// live+replay test is race-clean.
type fakeDaemon struct {
	beads map[string]*beadsapi.BeadDetail

	getBeadMu    sync.Mutex
	getBeadCalls int
	getBeadErr   error

	mintMu          sync.Mutex
	createCalls     int
	createReqs      []beadsapi.CreateBeadRequest
	createLastOpBy  string
	createByIdemKey map[string]string
	createOnSuccess func(*fakeDaemon, string, beadsapi.CreateBeadRequest)
}

func newFake() *fakeDaemon {
	return &fakeDaemon{beads: map[string]*beadsapi.BeadDetail{}}
}

func (f *fakeDaemon) GetBead(_ context.Context, id string) (*beadsapi.BeadDetail, error) {
	f.getBeadMu.Lock()
	f.getBeadCalls++
	err := f.getBeadErr
	f.getBeadMu.Unlock()
	if err != nil {
		return nil, err
	}
	b, ok := f.beads[id]
	if !ok {
		return nil, &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "not found"}
	}
	return b, nil
}

func (f *fakeDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	f.mintMu.Lock()
	defer f.mintMu.Unlock()
	f.createCalls++
	f.createReqs = append(f.createReqs, req)
	f.createLastOpBy = beadsapi.OperatorFromContext(ctx)
	// Migration-029 resolve-or-create: a repeat IdempotencyKey returns the
	// existing id and mints nothing new.
	if req.IdempotencyKey != "" {
		if existing, ok := f.createByIdemKey[req.IdempotencyKey]; ok {
			return existing, nil
		}
	}
	id := "gd-new"
	if req.IdempotencyKey != "" {
		if f.createByIdemKey == nil {
			f.createByIdemKey = map[string]string{}
		}
		f.createByIdemKey[req.IdempotencyKey] = id
	}
	if f.createOnSuccess != nil {
		f.createOnSuccess(f, id, req)
	}
	return id, nil
}

func (f *fakeDaemon) createCallsOfType(beadType string) int {
	f.mintMu.Lock()
	defer f.mintMu.Unlock()
	n := 0
	for _, req := range f.createReqs {
		if req.Type == beadType {
			n++
		}
	}
	return n
}

// fakeScreenClient is a minimal ScreenClient: GetAgentState returns a canned
// state; disabled models coopmux being off.
type fakeScreenClient struct {
	disabled        bool
	agentState      *coopmuxapi.AgentState
	agentStateErr   error
	agentStateCalls int
	mu              sync.Mutex
}

func (c *fakeScreenClient) Disabled() bool { return c.disabled }

func (c *fakeScreenClient) GetAgentState(_ context.Context, _ string) (*coopmuxapi.AgentState, error) {
	c.mu.Lock()
	c.agentStateCalls++
	c.mu.Unlock()
	if c.agentStateErr != nil {
		return nil, c.agentStateErr
	}
	return c.agentState, nil
}

// --- seed helpers ---

func seedPlanEpic(f *fakeDaemon, id string) *beadsapi.BeadDetail {
	epic := &beadsapi.BeadDetail{
		ID:     id,
		Title:  "Plan: " + id,
		Type:   "epic",
		Kind:   "issue",
		Status: "in_progress",
		Labels: []string{"project:gardenio", "plan"},
	}
	f.beads[id] = epic
	return epic
}

func seedAssistantBead(f *fakeDaemon, id, name, epicID, state string, updatedAt time.Time) *beadsapi.BeadDetail {
	b := &beadsapi.BeadDetail{
		ID:     id,
		Title:  name,
		Type:   "agent",
		Kind:   "config",
		Labels: []string{plantranscript.PlanGardenerRoleLabel},
		Fields: map[string]string{
			plantranscript.PlanIDField: epicID,
			"agent_state":              state,
		},
		UpdatedAt: updatedAt,
	}
	f.beads[id] = b
	return b
}

func decodeFakeFields(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode create fields: %v", err)
	}
	return out
}

// mkMinter builds a Minter wired to the fake daemon + coopmux with a
// controllable clock, WITHOUT connecting to NATS (natsURL is set so the
// constructor returns non-nil, but Start is never called).
func mkMinter(f *fakeDaemon, cm ScreenClient, now time.Time) *Minter {
	m := New("nats://test", "", "", f, cm, nil)
	m.now = func() time.Time { return now }
	return m
}

// assistantEvent builds a coop assistant_text NATS message for the given agent.
func assistantEvent(actor, role, agentBeadID, sessionID string, ts int64, text string, fullLength int) *nats.Msg {
	payload := map[string]any{
		"event_type":    "assistant_text",
		"actor":         actor,
		"role":          role,
		"agent_bead_id": agentBeadID,
		"session_id":    sessionID,
		"ts":            ts,
		"text":          text,
	}
	if fullLength > 0 {
		payload["full_length"] = fullLength
	}
	data, _ := json.Marshal(payload)
	return &nats.Msg{Subject: "coop.events.messages." + actor + ".assistant_text", Data: data}
}

// toolUseEvent builds a coop tool_use NATS message for the given agent, mirroring
// coop's forwarder payload (event_type=tool_use, tool_name + optional
// tool_args_summary). An empty argsSummary is omitted, matching coop's wire shape.
func toolUseEvent(actor, role, agentBeadID, sessionID string, ts int64, toolName, argsSummary string) *nats.Msg {
	payload := map[string]any{
		"event_type":    "tool_use",
		"actor":         actor,
		"role":          role,
		"agent_bead_id": agentBeadID,
		"session_id":    sessionID,
		"ts":            ts,
		"tool_name":     toolName,
	}
	if argsSummary != "" {
		payload["tool_args_summary"] = argsSummary
	}
	data, _ := json.Marshal(payload)
	return &nats.Msg{Subject: "coop.events.messages." + actor + ".tool_use", Data: data}
}

// --- tests ---

// TestMintsFullTextTurn is the happy path: a plan-gardener's assistant_text event
// mints ONE type=message turn bead bound to the plan by fields.plan_id, carrying
// the full text, turn_role=assistant, a stable turn_id, and the
// process-independent IdempotencyKey.
func TestMintsFullTextTurn(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	reply := "Drafted 4 steps and 2 clarifying questions. See the plan panel."
	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, reply, 0))

	if got := f.createCallsOfType("message"); got != 1 {
		t.Fatalf("expected 1 transcript turn minted, got %d", got)
	}
	req := f.createReqs[len(f.createReqs)-1]
	if req.Type != "message" || req.Kind != "data" {
		t.Errorf("turn bead Type/Kind = %q/%q, want message/data", req.Type, req.Kind)
	}
	fields := decodeFakeFields(t, req.Fields)
	if fields[plantranscript.PlanIDField] != "gd-plan1" {
		t.Errorf("turn plan_id = %q, want gd-plan1", fields[plantranscript.PlanIDField])
	}
	if fields[plantranscript.TurnRoleField] != plantranscript.TurnRoleAssistant {
		t.Errorf("turn_role = %q, want assistant", fields[plantranscript.TurnRoleField])
	}
	if fields["text"] != reply {
		t.Errorf("turn text = %q, want the full reply", fields["text"])
	}
	if !strings.HasPrefix(fields[plantranscript.TurnIDField], "sess-9:1700000000000:") {
		t.Errorf("turn_id = %q, want sess-9:1700000000000:<hash> prefix", fields[plantranscript.TurnIDField])
	}
	if fields["text_truncated"] != "" {
		t.Errorf("full-text turn must NOT be marked truncated; got %q", fields["text_truncated"])
	}
	wantKey := "plan-turn:gd-plan1:" + fields[plantranscript.TurnIDField]
	if req.IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", req.IdempotencyKey, wantKey)
	}
	if req.CreatedBy != "gardenio-plan-asst-1" {
		t.Errorf("CreatedBy = %q, want the agent actor", req.CreatedBy)
	}
}

// TestAttributesToPlanOperator proves the assistant turn is stamped with the
// plan epic's operator on the mint ctx (X-Operated-By), so it attributes to the
// same human as the operator turns and a background write with no OIDC identity
// does not fail-closed.
func TestAttributesToPlanOperator(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.OperatedBy = "oidc:matthew.baker@acme.example"
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "reply", 0))

	if f.createLastOpBy != "oidc:matthew.baker@acme.example" {
		t.Errorf("mint operator = %q, want the plan epic's operated_by", f.createLastOpBy)
	}
}

// TestSkipsNonPlanAgent proves the cheap role filter rejects a non-plan agent
// BEFORE any daemon lookup (the fleet-wide firehose guard).
func TestSkipsNonPlanAgent(t *testing.T) {
	f := newFake()
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(assistantEvent("gardenio-crew-1", "crew,thread", "gd-crew1", "sess-1", 1, "did stuff", 0))

	if f.createCalls != 0 {
		t.Errorf("non-plan agent must not mint; got %d creates", f.createCalls)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("cheap role filter must reject BEFORE any daemon GetBead; got %d GetBead calls", f.getBeadCalls)
	}
}

// TestRoleIsPlanAssistant guards the filter: it matches the BARE plan-gardener
// role name, including inside a comma-separated list, and does NOT match the
// role:-prefixed label form nor the removed legacy plan-assistant alias.
func TestRoleIsPlanAssistant(t *testing.T) {
	cases := []struct {
		role string
		want bool
	}{
		{"plan-gardener", true},
		{"plan-assistant", false},
		{"crew,plan-gardener", true},
		{" plan-gardener ", true},
		{"crew,thread", false},
		{"", false},
		{"role:plan-gardener", false},
		{"planner", false},
	}
	for _, c := range cases {
		if got := roleIsPlanAssistant(c.role); got != c.want {
			t.Errorf("roleIsPlanAssistant(%q) = %v, want %v", c.role, got, c.want)
		}
	}
}

// TestCachesPlanResolution proves a chatty assistant's repeated turns resolve
// the plan binding via the daemon only ONCE (positive cache).
func TestCachesPlanResolution(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	for i := range 3 {
		m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", int64(1700000000000+i), "turn", 0))
	}
	if f.getBeadCalls != 2 {
		t.Errorf("expected the plan binding to be resolved once (cached; 2 GetBead: agent+epic); got %d", f.getBeadCalls)
	}
	if got := f.createCallsOfType("message"); got != 3 {
		t.Errorf("expected 3 distinct turns minted, got %d", got)
	}
}

// TestNegativeCache proves a NON-plan agent that survives the role prefilter
// (plan-role label but no plan_id) is resolved once and cached negative.
func TestNegativeCache(t *testing.T) {
	f := newFake()
	f.beads["gd-x"] = &beadsapi.BeadDetail{
		ID: "gd-x", Title: "gardenio-x", Type: "agent", Kind: "config",
		Labels: []string{plantranscript.PlanGardenerRoleLabel},
		Fields: map[string]string{"agent_state": "working"},
	}
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(assistantEvent("gardenio-x", plantranscript.PlanGardenerRole, "gd-x", "sess-1", 1, "a", 0))
	m.handleMessage(assistantEvent("gardenio-x", plantranscript.PlanGardenerRole, "gd-x", "sess-1", 2, "b", 0))

	if f.getBeadCalls != 1 {
		t.Errorf("negative result must be cached; got %d GetBead calls", f.getBeadCalls)
	}
	if f.createCalls != 0 {
		t.Errorf("an agent with no plan_id must not mint; got %d creates", f.createCalls)
	}
}

// TestDedupOnReDelivery proves that re-delivering the SAME event (live +
// JetStream replay) mints against the SAME IdempotencyKey.
func TestDedupOnReDelivery(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	evt := assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "the reply", 0)
	m.handleMessage(evt)
	m.handleMessage(evt)

	keys := distinctMessageKeys(f)
	if len(keys) != 1 {
		t.Errorf("redelivery must reuse ONE IdempotencyKey; got %d distinct: %v", len(keys), keys)
	}
}

// TestHydratesOversizedReply proves a >256KB reply is hydrated to full text via
// coopmux GetAgentState and is NOT marked truncated.
func TestHydratesOversizedReply(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	full := "FULL untruncated body that is longer than the clipped wire copy"
	cm := &fakeScreenClient{agentState: &coopmuxapi.AgentState{LastMessage: full}}
	m := mkMinter(f, cm, testNow)

	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "FULL untruncated body...", 999999))

	if cm.agentStateCalls != 1 {
		t.Errorf("oversized reply must hydrate via GetAgentState; got %d calls", cm.agentStateCalls)
	}
	req := f.createReqs[len(f.createReqs)-1]
	fields := decodeFakeFields(t, req.Fields)
	if fields["text"] != full {
		t.Errorf("hydrated turn text = %q, want the full body", fields["text"])
	}
	if fields["text_truncated"] != "" {
		t.Errorf("successfully hydrated turn must NOT be marked truncated; got %q", fields["text_truncated"])
	}
}

// TestTruncationVisibleWhenHydrationFails proves that when the reply is oversized
// AND hydration is unavailable, the clipped text is persisted but STAMPED
// text_truncated=true + full_length.
func TestTruncationVisibleWhenHydrationFails(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	cm := &fakeScreenClient{disabled: true}
	m := mkMinter(f, cm, testNow)

	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "clipped body", 999999))

	req := f.createReqs[len(f.createReqs)-1]
	fields := decodeFakeFields(t, req.Fields)
	if fields["text"] != "clipped body" {
		t.Errorf("turn text = %q, want the clipped wire copy", fields["text"])
	}
	if fields["text_truncated"] != "true" {
		t.Errorf("truncated turn must be marked text_truncated=true; got %q", fields["text_truncated"])
	}
	if fields["full_length"] != "999999" {
		t.Errorf("truncated turn must record full_length; got %q", fields["full_length"])
	}
}

// TestSkipsEmptyReply proves a whitespace-only reply mints nothing.
func TestSkipsEmptyReply(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "   ", 0))
	if f.createCalls != 0 {
		t.Errorf("empty reply must not mint; got %d creates", f.createCalls)
	}
}

// TestHydrationWrongTurnGuarded proves the identity guard: if coopmux's CURRENT
// last_message is a DIFFERENT turn, the minter does NOT splice that wrong body
// in; it persists the clipped text marked truncated instead.
func TestHydrationWrongTurnGuarded(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	cm := &fakeScreenClient{agentState: &coopmuxapi.AgentState{LastMessage: "a completely different newer reply"}}
	m := mkMinter(f, cm, testNow)

	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "this turn's clipped prefix...", 999999))

	req := f.createReqs[len(f.createReqs)-1]
	fields := decodeFakeFields(t, req.Fields)
	if fields["text"] != "this turn's clipped prefix..." {
		t.Errorf("mismatched hydration must be rejected; kept text = %q, want the clipped wire copy", fields["text"])
	}
	if fields["text_truncated"] != "true" {
		t.Errorf("a rejected hydration must mark the turn truncated; got %q", fields["text_truncated"])
	}
}

// TestWireTextTruncated covers the exact-equality detection.
func TestWireTextTruncated(t *testing.T) {
	cases := []struct {
		name       string
		wireText   string
		fullLength int
		want       bool
	}{
		{"not truncated, equal length", "hello", 5, false},
		{"clearly truncated (wire much shorter than true)", "hello...", 500, true},
		{"boundary: true length within ellipsis width", "hello...", 7, true},
		{"no full_length reported", "hello...", 0, false},
		{"real text ending in ellipsis, not truncated", "wait...", 7, false},
	}
	for _, c := range cases {
		if got := wireTextTruncated(c.wireText, c.fullLength); got != c.want {
			t.Errorf("%s: wireTextTruncated(%q,%d) = %v, want %v", c.name, c.wireText, c.fullLength, got, c.want)
		}
	}
}

// TestTransientErrorNotCachedNegative proves a transient daemon error on the
// agent-bead GetBead is NOT cached as a negative binding.
func TestTransientErrorNotCachedNegative(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	f.getBeadErr = &beadsapi.APIError{StatusCode: 500, Message: "kbeads blip"}
	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "first", 0))
	if f.createCallsOfType("message") != 0 {
		t.Fatalf("a daemon error must skip the mint; got %d", f.createCallsOfType("message"))
	}

	f.getBeadErr = nil
	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 2, "second", 0))
	if f.createCallsOfType("message") != 1 {
		t.Errorf("after recovery the turn must mint (error not cached negative); got %d", f.createCallsOfType("message"))
	}
}

// TestSameMillisecondRepliesDistinct proves two DISTINCT replies sharing an
// epoch-ms ts get DISTINCT turn_ids (via the content-hash suffix).
func TestSameMillisecondRepliesDistinct(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "first block", 0))
	m.handleMessage(assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "second block", 0))

	keys := distinctMessageKeys(f)
	if len(keys) != 2 {
		t.Errorf("two distinct same-ms replies must get 2 distinct idempotency keys; got %d: %v", len(keys), keys)
	}
}

// TestAssistantActorFromSubject proves the suffix-agnostic actor parse works for
// .assistant_text subjects.
func TestAssistantActorFromSubject(t *testing.T) {
	cases := []struct {
		subject string
		want    string
	}{
		{"coop.events.messages.gardenio-plan-asst-1.assistant_text", "gardenio-plan-asst-1"},
		{"coop.events.messages.agent-x.thinking", "agent-x"},
		{"coop.events.messages.a.usage", "a"},
		{"not.a.coop.subject", ""},
		{"coop.events.messages.", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := assistantActorFromSubject(c.subject); got != c.want {
			t.Errorf("assistantActorFromSubject(%q) = %q, want %q", c.subject, got, c.want)
		}
	}
}

// --- tool-turn mint (data slice ka-0000004690) ---

// TestMintsToolTurn is the tool-call happy path: a plan-gardener's tool_use event
// mints ONE type=message turn bead bound to the plan, carrying turn_role=tool,
// the tool name + args summary in the structured fields, a stable turn_id, and
// the process-independent IdempotencyKey.
func TestMintsToolTurn(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "Bash", "gh workflow run e2e.yml"))

	if got := f.createCallsOfType("message"); got != 1 {
		t.Fatalf("expected 1 tool turn minted, got %d", got)
	}
	req := f.createReqs[len(f.createReqs)-1]
	if req.Type != "message" || req.Kind != "data" {
		t.Errorf("tool turn Type/Kind = %q/%q, want message/data", req.Type, req.Kind)
	}
	fields := decodeFakeFields(t, req.Fields)
	if fields[plantranscript.PlanIDField] != "gd-plan1" {
		t.Errorf("tool turn plan_id = %q, want gd-plan1", fields[plantranscript.PlanIDField])
	}
	if fields[plantranscript.TurnRoleField] != plantranscript.TurnRoleTool {
		t.Errorf("turn_role = %q, want tool", fields[plantranscript.TurnRoleField])
	}
	if fields[plantranscript.ToolNameField] != "Bash" {
		t.Errorf("tool_name = %q, want Bash", fields[plantranscript.ToolNameField])
	}
	if fields[plantranscript.ToolArgsSummaryField] != "gh workflow run e2e.yml" {
		t.Errorf("tool_args_summary = %q, want the args summary", fields[plantranscript.ToolArgsSummaryField])
	}
	// A phase-2-deferred result summary must be absent (the field is omitted).
	if _, ok := fields[plantranscript.ToolResultSummaryField]; ok {
		t.Errorf("tool_result_summary must be absent (phase 2); got %q", fields[plantranscript.ToolResultSummaryField])
	}
	// fields.text is the human-readable fallback line "<tool>: <args>".
	if fields["text"] != "Bash: gh workflow run e2e.yml" {
		t.Errorf("fallback text = %q, want %q", fields["text"], "Bash: gh workflow run e2e.yml")
	}
	if !strings.HasPrefix(fields[plantranscript.TurnIDField], "sess-9:1700000000000:") {
		t.Errorf("turn_id = %q, want sess-9:1700000000000:<hash> prefix", fields[plantranscript.TurnIDField])
	}
	wantKey := "plan-turn:gd-plan1:" + fields[plantranscript.TurnIDField]
	if req.IdempotencyKey != wantKey {
		t.Errorf("IdempotencyKey = %q, want %q", req.IdempotencyKey, wantKey)
	}
	if req.CreatedBy != "gardenio-plan-asst-1" {
		t.Errorf("CreatedBy = %q, want the agent actor", req.CreatedBy)
	}
}

// TestToolTurnNoArgsSummary proves a tool with no summarizable args still mints a
// tool turn (name-only), with the fallback text being just the tool name.
func TestToolTurnNoArgsSummary(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "Bash", ""))

	if got := f.createCallsOfType("message"); got != 1 {
		t.Fatalf("expected 1 name-only tool turn minted, got %d", got)
	}
	fields := decodeFakeFields(t, f.createReqs[len(f.createReqs)-1].Fields)
	if fields[plantranscript.ToolNameField] != "Bash" {
		t.Errorf("tool_name = %q, want Bash", fields[plantranscript.ToolNameField])
	}
	if _, ok := fields[plantranscript.ToolArgsSummaryField]; ok {
		t.Errorf("tool_args_summary must be omitted when empty; got %q", fields[plantranscript.ToolArgsSummaryField])
	}
	if fields["text"] != "Bash" {
		t.Errorf("fallback text = %q, want just the tool name", fields["text"])
	}
}

// TestToolTurnSkipsEmptyToolName proves a tool_use event with no tool name mints
// nothing (an unusable, nameless tool chip).
func TestToolTurnSkipsEmptyToolName(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "", "some args"))

	if f.createCalls != 0 {
		t.Errorf("a nameless tool_use must not mint; got %d creates", f.createCalls)
	}
}

// TestToolTurnSkipsNonPlanAgent proves the cheap role filter rejects a non-plan
// agent's tool call BEFORE any daemon lookup (same firehose guard as prose).
func TestToolTurnSkipsNonPlanAgent(t *testing.T) {
	f := newFake()
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(toolUseEvent("gardenio-crew-1", "crew,thread", "gd-crew1", "sess-1", 1, "Bash", "ls"))

	if f.createCalls != 0 {
		t.Errorf("non-plan agent tool call must not mint; got %d creates", f.createCalls)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("cheap role filter must reject BEFORE any daemon GetBead; got %d GetBead calls", f.getBeadCalls)
	}
}

// TestToolTurnAttributesToPlanOperator proves a tool turn is stamped with the
// plan epic's operator on the mint ctx, exactly like an assistant turn.
func TestToolTurnAttributesToPlanOperator(t *testing.T) {
	f := newFake()
	epic := seedPlanEpic(f, "gd-plan1")
	epic.OperatedBy = "oidc:matthew.baker@acme.example"
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "Bash", "ls"))

	if f.createLastOpBy != "oidc:matthew.baker@acme.example" {
		t.Errorf("tool turn mint operator = %q, want the plan epic's operated_by", f.createLastOpBy)
	}
}

// TestToolTurnDedupOnReDelivery proves re-delivering the SAME tool_use event
// (live + JetStream replay) converges on ONE bead via the shared IdempotencyKey.
func TestToolTurnDedupOnReDelivery(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	ev := toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "Read", "foo.go")
	m.handleMessage(ev)
	m.handleMessage(ev) // re-delivery

	if got := len(distinctMessageKeys(f)); got != 1 {
		t.Errorf("re-delivered tool call must share ONE idempotency key; got %d distinct", got)
	}
}

// TestParallelToolCallsDistinctTurns proves two DISTINCT tool calls a plan agent
// published in the same epoch-ms (parallel tool_use blocks, same ts) mint TWO
// distinct turns — the tool+args hash disambiguates them.
func TestParallelToolCallsDistinctTurns(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	m.handleMessage(toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "Grep", "TODO"))
	m.handleMessage(toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1700000000000, "Read", "x.go"))

	if got := len(distinctMessageKeys(f)); got != 2 {
		t.Errorf("two distinct same-ms tool calls must get 2 distinct idempotency keys; got %d", got)
	}
}

// TestDropsNonTurnEventKinds proves the shared subscription drops event kinds
// the minter does not mint (thinking/usage/refusal) BEFORE any daemon lookup,
// even for a plan assistant — only assistant_text and tool_use are minted.
func TestDropsNonTurnEventKinds(t *testing.T) {
	f := newFake()
	seedPlanEpic(f, "gd-plan1")
	seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
	m := mkMinter(f, &fakeScreenClient{}, testNow)

	for _, kind := range []string{"thinking", "usage", "refusal"} {
		payload := map[string]any{
			"event_type":    kind,
			"actor":         "gardenio-plan-asst-1",
			"role":          plantranscript.PlanGardenerRole,
			"agent_bead_id": "gd-asst1",
			"session_id":    "sess-9",
			"ts":            int64(1),
			"text":          "irrelevant",
		}
		data, _ := json.Marshal(payload)
		m.handleMessage(&nats.Msg{Subject: "coop.events.messages.gardenio-plan-asst-1." + kind, Data: data})
	}

	if f.createCalls != 0 {
		t.Errorf("non-turn event kinds must not mint; got %d creates", f.createCalls)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("event-kind routing must drop before any daemon GetBead; got %d GetBead calls", f.getBeadCalls)
	}
}

// TestEventTypeFromSubject proves the subject-suffix fallback for the event_type
// envelope field (used only when a legacy producer omits event_type).
func TestEventTypeFromSubject(t *testing.T) {
	cases := []struct {
		subject string
		want    string
	}{
		{"coop.events.messages.gardenio-plan-asst-1.assistant_text", "assistant_text"},
		{"coop.events.messages.agent-x.tool_use", "tool_use"},
		{"coop.events.messages.a.usage", "usage"},
		{"not.a.coop.subject", ""},
		{"coop.events.messages.", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := eventTypeFromSubject(c.subject); got != c.want {
			t.Errorf("eventTypeFromSubject(%q) = %q, want %q", c.subject, got, c.want)
		}
	}
}

// distinctMessageKeys returns the set of distinct IdempotencyKeys across all
// recorded type=message create requests.
func distinctMessageKeys(f *fakeDaemon) map[string]struct{} {
	f.mintMu.Lock()
	defer f.mintMu.Unlock()
	keys := map[string]struct{}{}
	for _, req := range f.createReqs {
		if req.Type == "message" {
			keys[req.IdempotencyKey] = struct{}{}
		}
	}
	return keys
}

// TestStoreBindingSweepsExpired proves the cache is bounded to the live working
// set and does not grow without bound (gd-sjI7A55zUt). Each distinct agent bead
// that emits a turn then despawns is never re-read; without a write-side sweep
// its entry orphans in m.cache forever, a slow OOM in the always-on controller.
// Fault injection: removing the sweepExpiredLocked call in storeBinding makes
// this assertion fail (map size grows to 1000 instead of tracking the window).
func TestStoreBindingSweepsExpired(t *testing.T) {
	f := newFake()
	base := testNow
	clock := base
	m := New("nats://test", "", "", f, &fakeScreenClient{}, nil)
	m.now = func() time.Time { return clock }

	// Store 1000 distinct short-lived bindings, advancing the clock past the TTL
	// every few inserts so each earlier cohort is expired by the time later ones
	// land — exactly the despawn-and-never-return pattern of ephemeral agents.
	const total = 1000
	for i := range total {
		clock = base.Add(time.Duration(i) * time.Minute)
		m.storeBinding(fmt.Sprintf("gd-asst-%d", i), planBinding{isPlanAssistant: true, planID: "gd-plan", at: clock})
	}

	m.mu.Lock()
	size := len(m.cache)
	m.mu.Unlock()

	// The live window is planAssistantResolveTTL (5m) wide; at 1 insert/min the
	// bounded map holds only the last ~5 unexpired entries, never the full 1000.
	// Assert a generous bound well below the cumulative count to make the leak
	// (unbounded growth) fail loudly while tolerating sweep-timing slack.
	if size > 50 {
		t.Errorf("cache not bounded to live working set: size=%d after %d distinct stores (leak: no write-side eviction)", size, total)
	}
	if size == 0 {
		t.Fatalf("sweep evicted the just-stored live entry; size=%d", size)
	}
}

// projectLabels returns every project:<land> label on a create request.
func projectLabels(req beadsapi.CreateBeadRequest) []string {
	var out []string
	for _, l := range req.Labels {
		if strings.HasPrefix(l, "project:") {
			out = append(out, l)
		}
	}
	return out
}

// withWireProject stamps the coop envelope's project (the agent's BOAT_PROJECT)
// onto a synthesized event.
func withWireProject(t *testing.T, msg *nats.Msg, project string) *nats.Msg {
	t.Helper()
	var payload map[string]any
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	payload["project"] = project
	data, _ := json.Marshal(payload)
	msg.Data = data
	return msg
}

// TestTurnsCarryProjectLabel proves every minted turn — prose and tool — carries
// exactly one project:<land> label, so it lands in its plan's land instead of
// being projectless. The plan epic's label wins over the agent bead's and the
// wire envelope's; each fallback covers the case the one before it misses, and
// with nothing knowable the turn stays unlabelled rather than guessed.
func TestTurnsCarryProjectLabel(t *testing.T) {
	cases := []struct {
		name        string
		epicLabels  []string
		agentLabels []string
		wireProject string
		want        []string
	}{
		{"plan epic wins", []string{"project:gardenio", "plan"}, []string{"project:other"}, "wire", []string{"project:gardenio"}},
		{"agent bead fallback", []string{"plan"}, []string{"project:other"}, "wire", []string{"project:other"}},
		{"wire fallback", []string{"plan"}, nil, "wire", []string{"project:wire"}},
		{"nothing knowable", []string{"plan"}, nil, "", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFake()
			seedPlanEpic(f, "gd-plan1").Labels = c.epicLabels
			agent := seedAssistantBead(f, "gd-asst1", "gardenio-plan-asst-1", "gd-plan1", "working", testNow)
			agent.Labels = append(agent.Labels, c.agentLabels...)
			m := mkMinter(f, &fakeScreenClient{}, testNow)

			m.handleMessage(withWireProject(t, assistantEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 1, "reply", 0), c.wireProject))
			m.handleMessage(withWireProject(t, toolUseEvent("gardenio-plan-asst-1", plantranscript.PlanGardenerRole, "gd-asst1", "sess-9", 2, "Bash", "ls"), c.wireProject))

			if len(f.createReqs) != 2 {
				t.Fatalf("expected 2 turns minted, got %d", len(f.createReqs))
			}
			for _, req := range f.createReqs {
				if got := projectLabels(req); fmt.Sprint(got) != fmt.Sprint(c.want) {
					t.Errorf("turn %q project labels = %v, want %v (labels %v)", req.Title, got, c.want, req.Labels)
				}
				if fields := decodeFakeFields(t, req.Fields); fields["channel_id"] != "" {
					t.Errorf("turn carries fields.channel_id %q; the land label would then be held to the channel's land", fields["channel_id"])
				}
			}
		})
	}
}
