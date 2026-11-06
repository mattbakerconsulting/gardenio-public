package bridge

// End-to-end conversational round-trip tests for the Jira bridge (epic
// ka-0000007401). These are the FIRST tests that drive the full conversational
// loop — @gardenio mention → agent spawn → `gb jira reply` → human follow-up →
// nudge/respawn — through the REAL HTTP handlers on a SINGLE mux backed by a
// SINGLE shared daemon, mirroring how cmd/jira-bridge/main.go wires /webhook and
// /api/jira/* onto one http.ServeMux.
//
// Why this is distinct from the existing coverage (all of which tests the legs
// in ISOLATION, hand-seeding each leg's starting state):
//
//   - jira_mentions_e2e_test.go drives ONLY leg (a): a mention webhook → spawn.
//   - jira_reply_test.go builds the reply agent bead BY HAND (replyAgentDaemon)
//     rather than one produced by a real spawn.
//   - the follow-up respawn tests (jira_webhook_test.go) start from a
//     hand-seeded CLOSED task bead, not one created by a prior mention + reply.
//
// The legs are never CONNECTED: no test proves that the jira_key /
// jira_comment_id / role that the SPAWN leg stamps onto the agent bead are the
// exact values the REPLY leg then reads to post the comment back, nor that the
// same bead is what the FOLLOW-UP leg finds to nudge (alive) or respawn (dead).
//
// roundTripDaemon is one in-memory fake implementing the union of every client
// interface the four legs touch (JiraMentionBeadClient, JiraWebhookBeadClient,
// JiraSquawkClient, JiraAgentResolverClient), so state flows across legs exactly
// as it does in production: CreateBead/SpawnAgent write real beads that
// FindAgentBead, ListTaskBeadsForJiraKey, GetBead, CloseBead and UpdateBeadFields
// then observe and mutate.
//
// Acceptance (task ka-0000007401): agents can @gardenio-mention in Jira, receive
// the mention via jira-bridge, and respond conversationally back to the Jira
// issue — with the conversation surviving both a live follow-up (nudge) and a
// follow-up after the agent stopped (session-continuity respawn).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// roundTripDaemon is the single shared fake backing every leg of the
// conversational round-trip. Task beads (created by the mention path) and agent
// beads (created by SpawnAgent, keyed by name) live in separate maps so
// ListTaskBeadsForJiraKey returns only task beads (matching the daemon's
// server-side issue-kind + jira_key predicate) while FindAgentBead resolves the
// spawned agent by name.
type roundTripDaemon struct {
	mu sync.Mutex

	taskBeads  map[string]*beadsapi.BeadDetail // by bead ID
	agentBeads map[string]*beadsapi.BeadDetail // by agent NAME

	triggerBeads []*beadsapi.BeadDetail // spawn-role trigger lookups
	mirrorBeads  []*beadsapi.BeadDetail // jira:<key> mirror lookups

	spawns []spawnCall  // every SpawnAgent call (mention + respawn)
	deps   []addDepCall // every AddDependency call
	closed []string     // bead IDs passed to CloseBead

	nextID int
}

func newRoundTripDaemon() *roundTripDaemon {
	return &roundTripDaemon{
		taskBeads:  make(map[string]*beadsapi.BeadDetail),
		agentBeads: make(map[string]*beadsapi.BeadDetail),
	}
}

// --- JiraMentionBeadClient ---

func (d *roundTripDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	id := fmt.Sprintf("task-bead-%d", d.nextID)
	fields := map[string]string{}
	if len(req.Fields) > 0 {
		// The mention path marshals its fields to JSON; parse them back so the
		// stored bead carries jira_key/jira_comment_id/operated_by exactly as the
		// daemon would persist them.
		_ = json.Unmarshal(req.Fields, &fields)
	}
	d.taskBeads[id] = &beadsapi.BeadDetail{
		ID:         id,
		Title:      req.Title,
		Type:       req.Type,
		Status:     "open",
		Labels:     req.Labels,
		Priority:   req.Priority,
		CreatedBy:  req.CreatedBy,
		Fields:     fields,
		OperatedBy: fields["operated_by"],
	}
	return id, nil
}

func (d *roundTripDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, prompt string, extra ...map[string]string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var extraFields map[string]string
	if len(extra) > 0 {
		extraFields = extra[0]
	}
	d.spawns = append(d.spawns, spawnCall{
		AgentName: agentName, Project: project, TaskID: taskID,
		Role: role, Prompt: prompt, Extra: extraFields,
	})

	// Materialize the agent bead the way the controller does: role + the stamped
	// extraFields (jira_key, jira_comment_id, operated_by, spawn_source) become
	// the agent bead's fields, which the reply/thread/resolver legs read back.
	fields := map[string]string{"agent": agentName, "role": role}
	maps.Copy(fields, extraFields)
	d.nextID++
	agentBeadID := fmt.Sprintf("agent-bead-%d", d.nextID)
	d.agentBeads[agentName] = &beadsapi.BeadDetail{
		ID:         agentBeadID,
		Title:      agentName,
		Type:       "agent",
		Status:     "in_progress",
		Fields:     fields,
		OperatedBy: fields["operated_by"],
	}

	// Assign the spawned agent to its task bead so the webhook follow-up loop
	// (which routes on bead.Assignee) finds it, exactly as production does.
	if tb, ok := d.taskBeads[taskID]; ok {
		tb.Assignee = agentName
	}
	return agentBeadID, nil
}

func (d *roundTripDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*beadsapi.BeadDetail, 0, len(d.taskBeads))
	for _, b := range d.taskBeads {
		out = append(out, b)
	}
	return out, nil
}

func (d *roundTripDaemon) ListTaskBeadsForJiraKey(_ context.Context, jiraKey string) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if jiraKey == "" {
		return nil, nil
	}
	var out []*beadsapi.BeadDetail
	for _, b := range d.taskBeads {
		if b.Fields["jira_key"] == jiraKey {
			out = append(out, b)
		}
	}
	return out, nil
}

func (d *roundTripDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Route jira:<key> mirror lookups to the mirror fixture; everything else
	// (spawn-role trigger lookups) to the trigger fixture — same split the
	// mention/resolver mocks use.
	for _, l := range q.Labels {
		if strings.HasPrefix(l, "jira:") {
			return &beadsapi.ListBeadsResult{Beads: d.mirrorBeads}, nil
		}
	}
	return &beadsapi.ListBeadsResult{Beads: d.triggerBeads}, nil
}

func (d *roundTripDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deps = append(d.deps, addDepCall{BeadID: beadID, DependsOnID: dependsOnID, DepType: depType, CreatedBy: createdBy})
	return nil
}

// --- JiraWebhookBeadClient ---

func (d *roundTripDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.taskBeads[beadID]; ok {
		maps.Copy(b.Fields, fields)
		return nil
	}
	for _, b := range d.agentBeads {
		if b.ID == beadID {
			maps.Copy(b.Fields, fields)
			return nil
		}
	}
	return fmt.Errorf("bead %s not found", beadID)
}

func (d *roundTripDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	apply := func(b *beadsapi.BeadDetail) {
		if req.Status != nil {
			b.Status = *req.Status
		}
		for k, v := range req.Fields {
			if v == nil {
				delete(b.Fields, k)
				continue
			}
			b.Fields[k] = fmt.Sprintf("%v", v)
		}
	}
	if b, ok := d.taskBeads[beadID]; ok {
		apply(b)
		return nil
	}
	for _, b := range d.agentBeads {
		if b.ID == beadID {
			apply(b)
			return nil
		}
	}
	return fmt.Errorf("bead %s not found", beadID)
}

// AddComment bridges a Jira comment onto a task bead. The round-trip assertions
// key on the agent conversation (spawn/reply/nudge/respawn), not the bead-side
// comment ledger, so this is accepted as a no-op — the webhook's own comment
// bridging is pinned by jira_webhook_test.go.
func (d *roundTripDaemon) AddComment(_ context.Context, _, _, _ string) error {
	return nil
}

// --- JiraSquawkClient / JiraAgentResolverClient shared ---

func (d *roundTripDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.agentBeads[agentName]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("agent %s not found", agentName)
}

func (d *roundTripDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*beadsapi.BeadDetail, 0, len(d.agentBeads))
	for _, b := range d.agentBeads {
		out = append(out, b)
	}
	return out, nil
}

func (d *roundTripDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.taskBeads[beadID]; ok {
		return b, nil
	}
	for _, b := range d.agentBeads {
		if b.ID == beadID {
			return b, nil
		}
	}
	return nil, nil
}

func (d *roundTripDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = append(d.closed, beadID)
	for _, b := range d.agentBeads {
		if b.ID == beadID {
			b.Status = "closed"
		}
	}
	return nil
}

// --- test-only accessors + mutators ---

func (d *roundTripDaemon) getAgentBead(name string) *beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.agentBeads[name]
}

func (d *roundTripDaemon) getTaskBead(id string) *beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.taskBeads[id]
}

func (d *roundTripDaemon) setAgentFields(name string, kv map[string]string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	b := d.agentBeads[name]
	maps.Copy(b.Fields, kv)
}

func (d *roundTripDaemon) snapshot() (spawns int, closed []string, deps []addDepCall) {
	d.mu.Lock()
	defer d.mu.Unlock()
	closedCopy := append([]string(nil), d.closed...)
	depsCopy := append([]addDepCall(nil), d.deps...)
	return len(d.spawns), closedCopy, depsCopy
}

// roundTripSpawnTrigger returns an enabled spawn_agent trigger whose role is
// "jira-mention" — a role present in DefaultFieldAllowlist, so the reply leg's
// AllowReplyComment verdict permits the internal note. (jiraTestSpawnTriggerBeads
// uses "gardenio-crew", which is NOT in the allowlist and would make the reply
// leg 403 — wrong for a round-trip that must reach a landed comment.)
func roundTripSpawnTrigger(project string) []*beadsapi.BeadDetail {
	return []*beadsapi.BeadDetail{
		{Fields: map[string]string{
			"action": "spawn_agent", "project": project,
			"role": "jira-mention", "enabled": "true",
		}},
	}
}

// roundTripCommentPayload builds a Jira comment webhook body with an explicit
// comment id (makeJiraWebhookPayload hardcodes "12345", but the round-trip needs
// distinct ids to prove the reply anchor and the GAP-2 live-agent refresh track
// the comment that actually fired).
func roundTripCommentPayload(event, jiraKey, commentID, body, author string) []byte {
	payload := map[string]any{
		"webhookEvent": event,
		"issue": map[string]any{
			"key":    jiraKey,
			"fields": map[string]any{"summary": "Round-trip conversational ticket"},
		},
		"comment": map[string]any{
			"id":   commentID,
			"body": body,
			"author": map[string]any{
				"displayName": author,
				"accountId":   "acct-" + author,
			},
		},
	}
	b, _ := json.Marshal(payload)
	return b
}

// roundTripHarness wires one mux with both the real webhook handler and the real
// reply API, over one shared daemon and one shared write policy — the same
// objects cmd/jira-bridge/main.go threads into every path.
type roundTripHarness struct {
	daemon         *roundTripDaemon
	jira           *mockJiraSquawkJira
	mux            *http.ServeMux
	resolverNudged []nudgeRecord
	rawNudged      []nudgeRecord
}

type nudgeRecord struct {
	agent string
	msg   string
}

func newRoundTripHarness(t *testing.T) *roundTripHarness {
	t.Helper()
	h := &roundTripHarness{
		daemon: newRoundTripDaemon(),
		jira:   &mockJiraSquawkJira{},
		mux:    http.NewServeMux(),
	}
	h.daemon.triggerBeads = roundTripSpawnTrigger("gardenio")
	logger := jiraSquawkTestLogger()

	// Shared write policy — enforce mode, outbox off — threaded into the reply
	// leg (the only leg whose gate is non-nil here; the mention/follow-up project
	// gates fall back to the package defaults, which admit PE exactly as prod).
	policy := enforcedWritePolicy(false)

	mention := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  h.daemon,
		Project: "gardenio",
		Logger:  logger,
	})

	// Resolver for the follow-up leg. Its Nudge override records live-agent
	// nudges so we assert delivery without a real coop PTY.
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: h.daemon,
		Logger: logger,
		Nudge: func(_ context.Context, agent, msg string) error {
			h.resolverNudged = append(h.resolverNudged, nudgeRecord{agent, msg})
			return nil
		},
	})

	h.mux.Handle("/webhook", JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         h.daemon,
		MentionHandler: mention,
		Resolver:       resolver,
		DevMode:        true, // skip HMAC for the wire test
		Logger:         logger,
		// Raw nudge fallback — must NOT fire while the resolver owns delivery.
		Nudge: func(_ context.Context, agent, msg string) error {
			h.rawNudged = append(h.rawNudged, nudgeRecord{agent, msg})
			return nil
		},
	}))

	replyAPI := NewJiraReplyAPI(JiraReplyAPIConfig{
		Daemon:      h.daemon,
		Jira:        h.jira,
		Logger:      logger,
		WritePolicy: policy,
	})
	replyAPI.RegisterRoutes(h.mux)

	return h
}

// postWebhook drives a Jira webhook event through the shared mux.
func (h *roundTripHarness) postWebhook(t *testing.T, body []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("webhook POST returned %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
}

// postReply drives `gb jira reply`'s HTTP contract through the shared mux and
// decodes the response.
func (h *roundTripHarness) postReply(t *testing.T, agent, text string) jiraReplyResponse {
	t.Helper()
	raw, _ := json.Marshal(jiraReplyRequest{Agent: agent, Text: text})
	req := httptest.NewRequest(http.MethodPost, "/api/jira/reply", bytes.NewReader(raw))
	w := httptest.NewRecorder()
	h.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("reply POST returned %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resp jiraReplyResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode reply response: %v", err)
	}
	return resp
}

// spawnedAgentName returns the name of the agent the mention leg spawned.
func (h *roundTripHarness) spawnedAgentName(t *testing.T) string {
	t.Helper()
	h.daemon.mu.Lock()
	defer h.daemon.mu.Unlock()
	if len(h.daemon.spawns) == 0 {
		t.Fatal("no agent was spawned by the mention leg")
	}
	return h.daemon.spawns[0].AgentName
}

// TestE2E_JiraConversationalRoundTrip_LiveAgentNudge drives the full loop when
// the agent is still ALIVE at follow-up time: mention → spawn → reply → live
// nudge (no respawn), and asserts the conversation stays on one agent with its
// reply anchor advancing to the comment that woke it (GAP 2).
func TestE2E_JiraConversationalRoundTrip_LiveAgentNudge(t *testing.T) {
	const (
		jiraKey       = "PE-8801"
		mentionCommID = "10001"
		followUpComm  = "10002"
		mirrorID      = "gd-jira-mirror-pe8801"
	)
	h := newRoundTripHarness(t)
	h.daemon.mirrorBeads = []*beadsapi.BeadDetail{
		{ID: mirrorID, Labels: []string{"source:jira", "jira:" + jiraKey}},
	}

	// --- Leg (a): @gardenio mention → agent spawn ---
	h.postWebhook(t, roundTripCommentPayload(
		"comment_created", jiraKey, mentionCommID,
		"@gardenio please look into the flaky login test", "Jane Reporter"))

	agent := h.spawnedAgentName(t)
	if !strings.HasPrefix(agent, "jira-pe-8801-") {
		t.Errorf("spawned agent name = %q, want jira-pe-8801-<suffix>", agent)
	}
	agentBead := h.daemon.getAgentBead(agent)
	if agentBead == nil {
		t.Fatal("spawn leg did not materialize an agent bead")
	}
	if got := agentBead.Fields["jira_key"]; got != jiraKey {
		t.Errorf("agent bead jira_key = %q, want %q", got, jiraKey)
	}
	if got := agentBead.Fields["jira_comment_id"]; got != mentionCommID {
		t.Errorf("agent bead jira_comment_id = %q, want %q (the originating mention comment)", got, mentionCommID)
	}
	if got := agentBead.Fields["role"]; got != "jira-mention" {
		t.Errorf("agent bead role = %q, want jira-mention", got)
	}
	// The mention leg also stamps the triggered-by lineage to the Jira mirror.
	spawns, _, deps := h.daemon.snapshot()
	if spawns != 1 {
		t.Fatalf("expected exactly 1 spawn from the mention leg, got %d", spawns)
	}
	if len(deps) != 1 || deps[0].DependsOnID != mirrorID || deps[0].DepType != "triggered-by" {
		t.Errorf("expected 1 triggered-by dep to the mirror, got %+v", deps)
	}

	// Agent boots and reports live (running + coop_url), as the controller
	// stamps once the pod is up.
	h.daemon.setAgentFields(agent, map[string]string{
		"agent_state": "running",
		"coop_url":    "http://coop.local/" + agent,
	})

	// --- Leg (b): agent responds conversationally via `gb jira reply` ---
	resp := h.postReply(t, agent, "On it — reproduced the flake, fix incoming in an MR.")
	if !resp.OK || resp.JiraKey != jiraKey {
		t.Fatalf("reply response = %+v, want OK on %s", resp, jiraKey)
	}
	comments := h.jira.getComments()
	if len(comments) != 1 {
		t.Fatalf("expected exactly 1 comment posted back to Jira, got %d", len(comments))
	}
	c := comments[0]
	if c.Key != jiraKey {
		t.Errorf("reply landed on %q, want %q (the agent's OWN bound ticket)", c.Key, jiraKey)
	}
	if c.Vis != CommentInternal {
		t.Errorf("reply visibility = %q, want internal (autonomous-agent ceiling)", c.Vis)
	}
	if !c.Markdown {
		t.Error("reply should post through the markdown-rendering ADF variant (H1)")
	}
	// The crux tying leg (a)→(b): the jira_comment_id the SPAWN leg stamped is
	// the anchor the REPLY leg threads to. No hand-seeding — it flowed through
	// the real spawn.
	if c.SourceCommentID != mentionCommID {
		t.Errorf("reply source anchor = %q, want %q (the mention comment stamped at spawn)", c.SourceCommentID, mentionCommID)
	}
	if !strings.Contains(c.Text, agent) || !strings.Contains(c.Text, "reproduced the flake") {
		t.Errorf("reply body missing agent/text: %q", c.Text)
	}

	// --- Leg (c): human follow-up while the agent is still ALIVE → nudge ---
	h.postWebhook(t, roundTripCommentPayload(
		"comment_created", jiraKey, followUpComm,
		"any update on the fix?", "Jane Reporter"))

	// Still exactly one agent — a live follow-up must NUDGE, never respawn.
	spawns, closed, _ := h.daemon.snapshot()
	if spawns != 1 {
		t.Errorf("live follow-up must not spawn a second agent; spawn count = %d", spawns)
	}
	if len(closed) != 0 {
		t.Errorf("live follow-up must not close any bead; closed = %v", closed)
	}
	if len(h.resolverNudged) != 1 {
		t.Fatalf("expected exactly 1 resolver nudge to the live agent, got %d", len(h.resolverNudged))
	}
	if h.resolverNudged[0].agent != agent {
		t.Errorf("nudge went to %q, want the live agent %q", h.resolverNudged[0].agent, agent)
	}
	if !strings.Contains(h.resolverNudged[0].msg, "any update on the fix?") {
		t.Errorf("nudge should carry the follow-up comment, got %q", h.resolverNudged[0].msg)
	}
	if len(h.rawNudged) != 0 {
		t.Errorf("legacy raw nudge must not fire when the resolver owns delivery; got %v", h.rawNudged)
	}
	// GAP 2 (ka-0000007091): the live agent's reply anchor advances to the
	// comment that woke it, so its NEXT `gb jira reply` threads to the follow-up.
	if got := h.daemon.getAgentBead(agent).Fields["jira_comment_id"]; got != followUpComm {
		t.Errorf("live agent jira_comment_id = %q, want %q (refreshed to the waking comment)", got, followUpComm)
	}
}

// TestE2E_JiraConversationalRoundTrip_StoppedAgentRespawn drives the full loop
// when the agent has STOPPED before the follow-up: mention → spawn → reply →
// (agent stops) → follow-up → same-name respawn carrying the new comment anchor.
// This is the session-continuity half of the conversational contract — a
// follow-up must revive a finished agent rather than drop the conversation.
func TestE2E_JiraConversationalRoundTrip_StoppedAgentRespawn(t *testing.T) {
	const (
		jiraKey       = "PE-8802"
		mentionCommID = "20001"
		followUpComm  = "20002"
	)
	h := newRoundTripHarness(t)

	// --- Leg (a): mention → spawn ---
	h.postWebhook(t, roundTripCommentPayload(
		"comment_created", jiraKey, mentionCommID,
		"@gardenio can you bump the dependency and open an MR?", "Sam Requester"))

	agent := h.spawnedAgentName(t)
	agentBead := h.daemon.getAgentBead(agent)
	if agentBead == nil {
		t.Fatal("spawn leg did not materialize an agent bead")
	}
	staleAgentBeadID := agentBead.ID

	// Agent boots, works, replies while alive.
	h.daemon.setAgentFields(agent, map[string]string{
		"agent_state": "running",
		"coop_url":    "http://coop.local/" + agent,
	})

	// --- Leg (b): reply lands on the bound ticket ---
	resp := h.postReply(t, agent, "Bumped to v2.4.0, MR !1234 is up for review.")
	if !resp.OK || resp.JiraKey != jiraKey {
		t.Fatalf("reply response = %+v, want OK on %s", resp, jiraKey)
	}
	if got := h.jira.getComments(); len(got) != 1 || got[0].SourceCommentID != mentionCommID {
		t.Fatalf("reply did not land anchored to the mention comment: %+v", got)
	}

	// Agent finishes and STOPS: terminal agent_state, its bead not yet reaped
	// (coop_url lingers) — the exact stale-terminal shape the resolver must
	// treat as dead.
	h.daemon.setAgentFields(agent, map[string]string{"agent_state": "done"})

	// --- Leg (c): follow-up after the agent stopped → same-name respawn ---
	h.postWebhook(t, roundTripCommentPayload(
		"comment_created", jiraKey, followUpComm,
		"thanks — could you also add a changelog entry?", "Sam Requester"))

	spawns, closed, _ := h.daemon.snapshot()
	if spawns != 2 {
		t.Fatalf("stopped-agent follow-up must respawn (expected 2 spawns total), got %d", spawns)
	}
	// The stale terminal bead is closed first so SpawnAgent's name-dedup does not
	// swallow the respawn.
	if len(closed) != 1 || closed[0] != staleAgentBeadID {
		t.Errorf("expected the stale agent bead %q to be closed before respawn, got %v", staleAgentBeadID, closed)
	}
	// The respawn reuses the SAME agent name (session continuity) and carries the
	// follow-up comment as its new reply anchor.
	h.daemon.mu.Lock()
	respawn := h.daemon.spawns[1]
	h.daemon.mu.Unlock()
	if respawn.AgentName != agent {
		t.Errorf("respawn name = %q, want the same agent %q (session continuity)", respawn.AgentName, agent)
	}
	if respawn.Project != "gardenio" {
		t.Errorf("respawn project = %q, want gardenio", respawn.Project)
	}
	if got := respawn.Extra["jira_comment_id"]; got != followUpComm {
		t.Errorf("respawn jira_comment_id = %q, want %q (the follow-up comment)", got, followUpComm)
	}
	if !strings.Contains(respawn.Prompt, "add a changelog entry") {
		t.Errorf("respawn prompt should carry the follow-up comment, got %q", respawn.Prompt)
	}
	// A respawn is not a nudge: neither delivery channel fires.
	if len(h.resolverNudged) != 0 {
		t.Errorf("a respawn must not also nudge; resolver nudges = %v", h.resolverNudged)
	}
	if len(h.rawNudged) != 0 {
		t.Errorf("legacy raw nudge must not fire; got %v", h.rawNudged)
	}
}

// TestE2E_JiraConversationalRoundTrip_FreshRespawn drives the same
// stopped-agent respawn as above, but the follow-up carries `--fresh`
// (ka-0000008188, Slack parity). It asserts the full wire path — real webhook
// handler → real resolver — realizes a CLEAN respawn: session_resume=false is
// stamped on the respawned agent, the worked task bead's resume_s3_key
// breadcrumb is cleared, and the `--fresh` control token never leaks into the
// prompt the respawned agent reads.
func TestE2E_JiraConversationalRoundTrip_FreshRespawn(t *testing.T) {
	const (
		jiraKey       = "PE-8803"
		mentionCommID = "30001"
		followUpComm  = "30002"
	)
	h := newRoundTripHarness(t)

	// --- Leg (a): mention → spawn ---
	h.postWebhook(t, roundTripCommentPayload(
		"comment_created", jiraKey, mentionCommID,
		"@gardenio please investigate the flaky test", "Sam Requester"))

	agent := h.spawnedAgentName(t)
	agentBead := h.daemon.getAgentBead(agent)
	if agentBead == nil {
		t.Fatal("spawn leg did not materialize an agent bead")
	}

	// The agent boots, works, and STOPS. Simulate a prior involuntary-death
	// breadcrumb by stamping resume_s3_key on the worked task bead — the exact
	// pointer resolveCanonicalS3Key would follow to resurrect the transcript.
	h.daemon.setAgentFields(agent, map[string]string{"agent_state": "done"})
	taskID := agentBead.Fields["mention_task"]
	if taskID == "" {
		// Fall back to the task the spawn was assigned to.
		h.daemon.mu.Lock()
		taskID = h.daemon.spawns[0].TaskID
		h.daemon.mu.Unlock()
	}
	if err := h.daemon.UpdateBeadFields(context.Background(), taskID,
		map[string]string{"resume_s3_key": "agent-bead-original-prefix"}); err != nil {
		t.Fatalf("seeding resume_s3_key breadcrumb: %v", err)
	}

	// --- Leg (b): --fresh follow-up after the agent stopped → clean respawn ---
	// A plain follow-up comment (no @gardenio re-mention) with --fresh: the
	// webhook follow-up loop owns the wake, so the flag routes straight to the
	// resolver respawn (a re-mention would instead be dedup-classified by the
	// mention handler; the follow-up-comment path is the primary --fresh entry).
	h.postWebhook(t, roundTripCommentPayload(
		"comment_created", jiraKey, followUpComm,
		"--fresh start over and re-analyze from scratch", "Sam Requester"))

	spawns, _, _ := h.daemon.snapshot()
	if spawns != 2 {
		t.Fatalf("--fresh follow-up on a stopped agent must respawn (expected 2 spawns), got %d", spawns)
	}
	h.daemon.mu.Lock()
	respawn := h.daemon.spawns[1]
	h.daemon.mu.Unlock()

	if respawn.AgentName != agent {
		t.Errorf("respawn name = %q, want same agent %q", respawn.AgentName, agent)
	}
	// Lever 1: session_resume=false stamped on the clean respawn.
	if got := respawn.Extra["session_resume"]; got != "false" {
		t.Errorf("--fresh respawn session_resume = %q, want \"false\"", got)
	}
	// The reply anchor still advances to the waking comment.
	if got := respawn.Extra["jira_comment_id"]; got != followUpComm {
		t.Errorf("respawn jira_comment_id = %q, want %q", got, followUpComm)
	}
	// The --fresh control token must not leak into the prompt.
	if strings.Contains(respawn.Prompt, "--fresh") {
		t.Errorf("--fresh must be stripped from the respawn prompt, got %q", respawn.Prompt)
	}
	if !strings.Contains(respawn.Prompt, "re-analyze from scratch") {
		t.Errorf("respawn prompt should carry the follow-up instruction, got %q", respawn.Prompt)
	}
	// Lever 2: the task bead's resume_s3_key breadcrumb is cleared, so
	// resolveCanonicalS3Key won't adopt the original transcript prefix at boot.
	if tb := h.daemon.getTaskBead(taskID); tb != nil {
		if got := tb.Fields["resume_s3_key"]; got != "" {
			t.Errorf("--fresh respawn must clear resume_s3_key on the task bead, got %q", got)
		}
	} else {
		t.Errorf("task bead %q not found after respawn", taskID)
	}
}
