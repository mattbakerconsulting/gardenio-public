package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"sync"

	"gardenio/controller/internal/beadsapi"
)

// mockDaemon is an in-process stand-in for the beads daemon that RECORDS the
// writes the jira-bridge conversational paths make (CreateBead, SpawnAgent,
// AddComment, UpdateBeadFields, CloseBead) instead of persisting them to a real
// daemon. It satisfies every bridge interface the probe wires:
//
//   - JiraMentionBeadClient (mention path)
//   - JiraWebhookBeadClient (webhook comment bridging)
//   - JiraSquawkClient      (squawk forwarder + reply API)
//   - JiraAgentResolverClient (follow-up nudge/respawn)
//   - jiraUserBackfiller / jiraTriggerLister (via UpdateBeadFields / ListBeadsFiltered)
//
// Because SpawnAgent is a RECORDED CALL and never writes a real type=agent bead,
// no controller/reconciler anywhere observes it, so the probe can exercise the
// full spawn/respawn logic with ZERO real pods.
type mockDaemon struct {
	mu sync.Mutex

	agents   map[string]*beadsapi.BeadDetail // agent name -> agent bead
	tasks    []*beadsapi.BeadDetail          // task beads for ListTaskBeadsAll
	beads    map[string]*beadsapi.BeadDetail // id -> bead, for GetBead
	filtered []*beadsapi.BeadDetail          // beads returned by ListBeadsFiltered (trigger beads, mirrors)

	creates  []createRecord
	spawns   []spawnRecord
	comments []commentRecord
	closed   map[string]bool

	nextID int
}

// createRecord captures one CreateBead call.
type createRecord struct {
	id     string
	title  string
	typ    string
	labels []string
	fields map[string]string
}

// spawnRecord captures one SpawnAgent call (extraFields merged).
type spawnRecord struct {
	agentName string
	project   string
	taskID    string
	role      string
	// prompt is the 6th SpawnAgent arg (customPrompt) — the interpolated agent
	// prompt buildJiraMentionPrompt produced. Recorded so the mention leg can
	// assert the prompt advertises `gb jira show <key>` + `gb jira thread`; the
	// deployed daemon lands this verbatim as the agent bead's fields.prompt
	// (beadsapi/client.go SpawnAgent), a real observable surface.
	prompt string
	fields map[string]string
}

// commentRecord captures one daemon-side AddComment call (bead comment, not a
// Jira write — the Jira writes are recorded by the stub Jira).
type commentRecord struct {
	beadID string
	author string
	text   string
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{
		agents: make(map[string]*beadsapi.BeadDetail),
		beads:  make(map[string]*beadsapi.BeadDetail),
		closed: make(map[string]bool),
	}
}

// seedSpawnTrigger adds an enabled spawn_agent trigger bead so resolveJiraSpawnRole
// resolves a role for the project (there is no hardcoded fallback — an unseeded
// project aborts the spawn).
func (m *mockDaemon) seedSpawnTrigger(project, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.filtered = append(m.filtered, &beadsapi.BeadDetail{
		ID:     "bd-trigger-" + project,
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:jira"},
		Fields: map[string]string{
			"enabled": "true",
			"action":  "spawn_agent",
			"project": project,
			"role":    role,
		},
	})
}

// ── recorded-call accessors ──────────────────────────────────────────────────

func (m *mockDaemon) createCalls() []createRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]createRecord, len(m.creates))
	copy(out, m.creates)
	return out
}

func (m *mockDaemon) spawnCalls() []spawnRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]spawnRecord, len(m.spawns))
	copy(out, m.spawns)
	return out
}

func (m *mockDaemon) wasClosed(beadID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed[beadID]
}

// commentsFor returns the number of daemon-side AddComment calls recorded for a
// bead — how many comments the webhook bridged onto it. Used by the
// crosslink-nowake leg to prove suppression is wake-only (the crosslink IS
// still bridged) rather than a drop.
func (m *mockDaemon) commentsFor(beadID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.comments {
		if c.beadID == beadID {
			n++
		}
	}
	return n
}

// ── beadsapi interface implementations ───────────────────────────────────────

func (m *mockDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	id := fmt.Sprintf("bd-mock-%d", m.nextID)
	fields := decodeFields(req.Fields)
	m.creates = append(m.creates, createRecord{
		id: id, title: req.Title, typ: req.Type, labels: req.Labels, fields: fields,
	})
	bead := &beadsapi.BeadDetail{
		ID:     id,
		Title:  req.Title,
		Type:   req.Type,
		Status: "open",
		Labels: req.Labels,
		Fields: fields,
	}
	m.beads[id] = bead
	if req.Type == "task" {
		m.tasks = append(m.tasks, bead)
	}
	return id, nil
}

func (m *mockDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	merged := make(map[string]string)
	for _, ef := range extraFields {
		maps.Copy(merged, ef)
	}
	m.spawns = append(m.spawns, spawnRecord{
		agentName: agentName, project: project, taskID: taskID, role: role, prompt: customPrompt, fields: merged,
	})
	m.nextID++
	return fmt.Sprintf("bd-agent-%d", m.nextID), nil
}

func (m *mockDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*beadsapi.BeadDetail, len(m.tasks))
	copy(out, m.tasks)
	return out, nil
}

// ListTaskBeadsForJiraKey mirrors the daemon's server-side fields.jira_key
// equality predicate so the probe exercises the same narrowing as production.
func (m *mockDaemon) ListTaskBeadsForJiraKey(_ context.Context, jiraKey string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if jiraKey == "" {
		return nil, nil
	}
	var out []*beadsapi.BeadDetail
	for _, b := range m.tasks {
		if b.Fields["jira_key"] == jiraKey {
			out = append(out, b)
		}
	}
	return out, nil
}

func (m *mockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*beadsapi.BeadDetail
	for _, b := range m.filtered {
		if matchesQuery(b, q) {
			out = append(out, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (m *mockDaemon) AddDependency(_ context.Context, _, _, _, _ string) error { return nil }

func (m *mockDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[beadID]; ok {
		if b.Fields == nil {
			b.Fields = make(map[string]string)
		}
		maps.Copy(b.Fields, fields)
	}
	return nil
}

func (m *mockDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[beadID]; ok {
		if req.Status != nil {
			b.Status = *req.Status
		}
		if req.Fields != nil {
			if b.Fields == nil {
				b.Fields = make(map[string]string)
			}
			for k, v := range req.Fields {
				if v == nil {
					delete(b.Fields, k)
					continue
				}
				b.Fields[k] = fmt.Sprintf("%v", v)
			}
		}
	}
	return nil
}

func (m *mockDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.comments = append(m.comments, commentRecord{beadID: beadID, author: author, text: text})
	return nil
}

func (m *mockDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.agents[agentName]; ok {
		return b, nil
	}
	return nil, fmt.Errorf("agent %q not found", agentName)
}

func (m *mockDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*beadsapi.BeadDetail, 0, len(m.agents))
	for _, b := range m.agents {
		out = append(out, b)
	}
	return out, nil
}

func (m *mockDaemon) GetBead(_ context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beads[beadID]; ok {
		return b, nil
	}
	for _, t := range m.tasks {
		if t.ID == beadID {
			return t, nil
		}
	}
	return nil, fmt.Errorf("bead %q not found", beadID)
}

func (m *mockDaemon) CloseBead(_ context.Context, beadID string, _ map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed[beadID] = true
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func decodeFields(raw json.RawMessage) map[string]string {
	fields := make(map[string]string)
	if len(raw) == 0 {
		return fields
	}
	_ = json.Unmarshal(raw, &fields)
	return fields
}

// matchesQuery applies the subset of ListBeadsQuery filters the bridge paths use
// (types, statuses, labels-AND). Enough for resolveJiraSpawnRole (trigger beads)
// and writeMirrorTriggeredBy (mirror lookup by label).
func matchesQuery(b *beadsapi.BeadDetail, q beadsapi.ListBeadsQuery) bool {
	if len(q.Types) > 0 && !contains(q.Types, b.Type) {
		return false
	}
	if len(q.Statuses) > 0 && !contains(q.Statuses, b.Status) {
		return false
	}
	for _, want := range q.Labels {
		if !contains(b.Labels, want) {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	return slices.Contains(haystack, needle)
}
