package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockDaemon implements BeadClient for testing.
type mockDaemon struct {
	mu                      sync.Mutex
	beads                   map[string]*beadsapi.BeadDetail
	closed                  []closeCall
	comments                []commentCall
	beadComments            map[string][]beadsapi.Comment
	getCalls                int
	lastCreateBeadRawFields json.RawMessage
	lastCreateBeadReq       beadsapi.CreateBeadRequest
	mails                   []beadsapi.CreateBeadRequest // type=mail creates; see createMail
	closedMails             []string
	deps                    []depCall
	lastCloseBeadCtx        context.Context
	lastCreateBeadCtx       context.Context
	lastGetBeadCtx          context.Context
	lastAddCommentCtx       context.Context
	lastSpawnAgentCtx       context.Context
	// commentSeq drives deterministic, monotonically-increasing CreatedAt
	// timestamps stamped on comments recorded via AddComment, so dedupe logic
	// that orders comments by CreatedAt has a stable clock in tests.
	commentSeq int
	// enforceActiveStatusScoping makes FindAgentBead hide CLOSED beads, matching
	// the real daemon client's active-status query. Opt-in; see FindAgentBead.
	enforceActiveStatusScoping bool
	// createBeadHook, when non-nil, is consulted at the start of every CreateBead
	// call (under the mock lock). Returning a non-nil error makes CreateBead
	// return it WITHOUT recording a bead — so tests can drive the transient-retry
	// + thread-reply-DLQ paths (ka-0000000612). The int arg is the 1-based call
	// count so a hook can fail the first N attempts then succeed. The request is
	// passed so a hook can target only the message create (IdempotencyKey prefix
	// "thread-reply:") and leave the thread-head create alone.
	createBeadHook  func(n int, req beadsapi.CreateBeadRequest) error
	createBeadCalls int
	// updateBeadHook, when set, is invoked on each UpdateBead (call count, id,
	// req). A non-nil error is returned WITHOUT applying the update, letting a
	// test inject a transient failure on the revive-in-place path
	// (respawnThreadAgent, ka-0000006807) the same way createBeadHook does for
	// the mint path.
	updateBeadHook  func(n int, beadID string, req beadsapi.UpdateBeadRequest) error
	updateBeadCalls int
	// updateFieldsErr, when set, is invoked on each UpdateBeadFields. A
	// non-nil error is returned WITHOUT applying the patch: the daemon
	// refusing the bridge's stamp (gd-2IEp8qx7bq).
	updateFieldsErr func(beadID string, fields map[string]string) error
	// closeBeadErr, when non-nil, makes CloseBead return it WITHOUT recording
	// the close — modelling a daemon-side rejection (e.g. edge-auth denial) so
	// tests can assert the bridge surfaces the failure loudly (ka-0000005533).
	closeBeadErr error
	// getBeadErr, when set, is consulted on each GetBead; a non-nil error is
	// returned in place of the bead (a daemon blip on one read).
	getBeadErr func(beadID string) error
	// listBeadsErr, when non-nil, makes ListBeadsFiltered return it.
	listBeadsErr error
	// spawnAgentErr, when non-nil, makes SpawnAgent return it WITHOUT creating
	// a bead — the daemon refusing a spawn (gd-VuYwGGYLLT).
	spawnAgentErr error
	// closeBeadPanic, when true, makes CloseBead panic — modelling a latent
	// defect deep in a dispatched handler's call tree so tests can assert the
	// bridge's goroutine recover boundary keeps the process alive (gd-A5wuFQHqtn).
	closeBeadPanic bool
	// slackAliases seeds the operators slack-alias feed returned by
	// ListSlackOperatorAliases, so cross-operator mention resolution can be
	// driven against the real feed's shapes (ka-0000002131).
	slackAliases []beadsapi.SlackOperatorAlias
	// slackAliasErr, when non-nil, makes ListSlackOperatorAliases fail.
	slackAliasErr error
	// slackAliasCalls counts feed fetches, so the TTL cache is assertable.
	slackAliasCalls int
	// mentionedBy seeds GetMentionedBy: subject bead id → the beads whose text
	// references it. Drives the C4 auto-close reactor's candidate scan.
	mentionedBy map[string][]beadsapi.BeadReferenceSummary
	// mentionedByErr, when non-nil, makes GetMentionedBy fail — so a mention
	// lookup failure can be asserted to skip auto-close without breaking dismissal.
	mentionedByErr error
	// reverseDeps seeds GetReverseDependencies: bead id → rows reading "BeadID
	// depends on DependsOnID". Drives the C4 auto-close leaf guard.
	reverseDeps map[string][]beadsapi.Dependency
	// reverseDepsErr, when non-nil, makes GetReverseDependencies fail — so the
	// leaf guard's fail-safe (skip the close) can be asserted.
	reverseDepsErr error
	// findAgentAnyErr, when non-nil, makes FindAgentBeadAnyStatus fail — a
	// daemon read error under a Commons mention lookup.
	findAgentAnyErr error
	// listProjectsErr, when non-nil, makes ListProjectBeads fail — the daemon
	// unreachable while a mention resolves its channel's project.
	listProjectsErr error
	// revokedRunners records each RevokeRunner call's runner id.
	revokedRunners []string
}

type commentCall struct {
	BeadID string
	Author string
	Text   string
}

type closeCall struct {
	BeadID string
	Fields map[string]string
}

func newMockDaemon() *mockDaemon {
	return &mockDaemon{
		beads: make(map[string]*beadsapi.BeadDetail),
	}
}

// seedProject pre-populates the mock with a project bead so that project
// validation in handleSpawnCommand passes for tests that use a project name.
func (m *mockDaemon) seedProject(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "proj-" + name
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  name,
		Type:   "project",
		Status: "open",
	}
}

// seedProjectWithChannel pre-populates the mock with a project bead that has
// a slack_channel field for channel-to-project resolution tests.
func (m *mockDaemon) seedProjectWithChannel(name, channelID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "proj-" + name
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  name,
		Type:   "project",
		Fields: map[string]string{"slack_channel": channelID},
	}
}

// seedProjectWithChannelRole pre-populates the mock with a project bead that has
// a slack_channel and channel_roles field for channel-to-role resolution tests.
func (m *mockDaemon) seedProjectWithChannelRole(name, channelID, role string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "proj-" + name
	m.beads[id] = &beadsapi.BeadDetail{
		ID:    id,
		Title: name,
		Type:  "project",
		Fields: map[string]string{
			"slack_channel": channelID,
			"channel_roles": fmt.Sprintf(`{%q:%q}`, channelID, role),
		},
	}
}

// seedProjectWithChannelRuntimeClass pre-populates the mock with a project bead
// that has a slack_channel and channel_runtime_class field, for tests that a
// channel (e.g. gb-turbo) pins its thread agents onto a RuntimeClass (runsc).
func (m *mockDaemon) seedProjectWithChannelRuntimeClass(name, channelID, runtimeClass string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "proj-" + name
	m.beads[id] = &beadsapi.BeadDetail{
		ID:    id,
		Title: name,
		Type:  "project",
		Fields: map[string]string{
			"slack_channel":         channelID,
			"channel_runtime_class": fmt.Sprintf(`{%q:%q}`, channelID, runtimeClass),
		},
	}
}

// seedProjectWithDMDefault pre-populates the mock with a project bead that has
// the dm_default field set to "true".
func (m *mockDaemon) seedProjectWithDMDefault(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := "proj-" + name
	m.beads[id] = &beadsapi.BeadDetail{
		ID:     id,
		Title:  name,
		Type:   "project",
		Fields: map[string]string{"dm_default": "true"},
	}
}

func (m *mockDaemon) GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	m.lastGetBeadCtx = ctx
	if m.getBeadErr != nil {
		if err := m.getBeadErr(beadID); err != nil {
			return nil, err
		}
	}
	if b, ok := m.beads[beadID]; ok {
		return b, nil
	}
	return &beadsapi.BeadDetail{ID: beadID}, nil
}

// enforceActiveStatusScoping makes FindAgentBead behave like the REAL daemon
// client, which queries /v1/agents-by-name?status=open,in_progress,blocked,deferred
// and therefore cannot see a CLOSED agent bead. The mock is lenient by default
// because ~150 existing test sites seed `Status: "closed"` beads and expect
// FindAgentBead to resolve them; flipping the default would rewrite all of them.
// Tests that specifically exercise closed-bead behaviour opt in here — without
// it a status-scoping bug like ka-0000005302 is invisible to the whole suite.
func (m *mockDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	if b, ok := m.beads[agentName]; ok {
		if m.enforceActiveStatusScoping && b.Status == "closed" {
			return nil, fmt.Errorf("%w: %q", beadsapi.ErrAgentNotFound, agentName)
		}
		return b, nil
	}
	return nil, fmt.Errorf("%w: %q", beadsapi.ErrAgentNotFound, agentName)
}

// FindAgentBeadAnyStatus resolves the agent bead by name regardless of status,
// mirroring the daemon's FindAgentByName (fields.agent == name, most-recent
// wins). It first tries the direct key (seeders that key by agent NAME), then
// scans fields.agent so seeders that key by a distinct ID (e.g. the
// "<name>-prior" closed-bead convention seedPriorAgentBead uses) still resolve
// — the respawn revive path (ka-0000006807) depends on finding that CLOSED
// prior bead so it can reopen it in place instead of minting a fresh id.
func (m *mockDaemon) FindAgentBeadAnyStatus(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.getCalls++
	if m.findAgentAnyErr != nil {
		return nil, m.findAgentAnyErr
	}
	if b, ok := m.beads[agentName]; ok {
		return b, nil
	}
	var best *beadsapi.BeadDetail
	for _, b := range m.beads {
		if b == nil || b.Type != "agent" || b.Fields["agent"] != agentName {
			continue
		}
		if best == nil || b.CreatedAt.After(best.CreatedAt) {
			best = b
		}
	}
	if best != nil {
		return best, nil
	}
	return nil, fmt.Errorf("%w: %q", beadsapi.ErrAgentNotFound, agentName)
}

// CaptureEscalationResult satisfies the BeadClient interface for the dedicated
// escalation result verb (ka-0000006558). The decisions/escalations tests that
// use mockDaemon do not exercise result capture, so this is a no-op recorder.
func (m *mockDaemon) CaptureEscalationResult(_ context.Context, _ string, _ beadsapi.CaptureEscalationResultRequest) error {
	return nil
}

func (m *mockDaemon) CloseBead(ctx context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closeBeadPanic {
		panic("boom: simulated CloseBead defect in dispatched handler")
	}
	if m.closeBeadErr != nil {
		return m.closeBeadErr
	}
	if strings.HasPrefix(beadID, "mail-") {
		m.closedMails = append(m.closedMails, beadID)
		return nil
	}
	m.closed = append(m.closed, closeCall{BeadID: beadID, Fields: fields})
	m.lastCloseBeadCtx = ctx
	// Beads are keyed in m.beads by different conventions across seeders: some by
	// ID (e.g. "proj-<name>"), agent beads by agent NAME. CloseBead is called
	// with the bead ID, so fall back to an ID search when the direct key lookup
	// misses — otherwise closing an agent bead (keyed by name) would silently
	// no-op and leave its status/agent_state stale, diverging from production.
	bead, ok := m.beads[beadID]
	if !ok {
		for _, candidate := range m.beads {
			if candidate != nil && candidate.ID == beadID {
				bead, ok = candidate, true
				break
			}
		}
	}
	if ok {
		bead.Status = "closed"
		for k, v := range fields {
			if bead.Fields == nil {
				bead.Fields = make(map[string]string)
			}
			bead.Fields[k] = v
		}
	}
	return nil
}

func (m *mockDaemon) RevokeRunner(_ context.Context, runnerID string) (int, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.revokedRunners = append(m.revokedRunners, runnerID)
	return 1, 0, nil
}

// lastCreateBead returns the last request CreateBead recorded.
func (m *mockDaemon) lastCreateBead() beadsapi.CreateBeadRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastCreateBeadReq
}

// createMail records a type=mail create outside m.beads, so nudges do not
// shift bead ids or counts, and stands in for the recipient's mail
// subscriber: delivery:interrupt mail is POSTed to the assignee's coop_url as
// the coop nudge the bridge used to send itself, so tests that watch a coop
// server still see the message arrive.
func (m *mockDaemon) createMail(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	m.lastCreateBeadCtx = ctx
	m.lastCreateBeadReq = req
	m.mails = append(m.mails, req)
	id := fmt.Sprintf("mail-%d", len(m.mails))
	var coopURL string
	if agent := m.beads[req.Assignee]; agent != nil {
		coopURL = beadsapi.ParseNotes(agent.Notes)["coop_url"]
	} else {
		for _, b := range m.beads {
			if b != nil && b.Type == "agent" && b.Fields["agent"] == req.Assignee {
				coopURL = beadsapi.ParseNotes(b.Notes)["coop_url"]
				break
			}
		}
	}
	m.mu.Unlock()
	if coopURL != "" && slices.Contains(req.Labels, "control:permission") {
		// The subscriber hands a verdict to the loopback channel server
		// (cmd/gb/mail_permission.go); coop_url stands in for it here.
		var verdict map[string]string
		_ = json.Unmarshal(req.Fields, &verdict)
		body, _ := json.Marshal(verdict)
		if hr, err := http.NewRequestWithContext(ctx, http.MethodPost, coopURL+"/permission", bytes.NewReader(body)); err == nil {
			hr.Header.Set("Content-Type", "application/json")
			if resp, err := http.DefaultClient.Do(hr); err == nil {
				resp.Body.Close()
			}
		}
	}
	if coopURL != "" && slices.Contains(req.Labels, "delivery:interrupt") {
		body, _ := json.Marshal(map[string]string{"message": req.Description})
		if hr, err := http.NewRequestWithContext(ctx, http.MethodPost, coopURL+"/api/v1/agent/nudge", bytes.NewReader(body)); err == nil {
			hr.Header.Set("Content-Type", "application/json")
			if resp, err := http.DefaultClient.Do(hr); err == nil {
				resp.Body.Close()
			}
		}
	}
	return id, nil
}

// mailBeadsLocked returns the recorded mail as beads. Caller holds m.mu.
func (m *mockDaemon) mailBeadsLocked() []*beadsapi.BeadDetail {
	out := make([]*beadsapi.BeadDetail, 0, len(m.mails))
	for i, req := range m.mails {
		fields := map[string]string{}
		var raw map[string]any
		if json.Unmarshal(req.Fields, &raw) == nil {
			for k, v := range raw {
				fields[k] = fmt.Sprintf("%v", v)
			}
		}
		out = append(out, &beadsapi.BeadDetail{
			ID: fmt.Sprintf("mail-%d", i+1), Type: req.Type, Title: req.Title, Description: req.Description,
			Assignee: req.Assignee, Labels: req.Labels, Fields: fields,
		})
	}
	return out
}

func (m *mockDaemon) CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	if req.Type == "mail" {
		return m.createMail(ctx, req)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastCreateBeadCtx = ctx
	m.createBeadCalls++
	if m.createBeadHook != nil {
		if err := m.createBeadHook(m.createBeadCalls, req); err != nil {
			return "", fmt.Errorf("creating bead: %w", err)
		}
	}
	id := fmt.Sprintf("bd-chat-%d", len(m.beads)+1)
	m.lastCreateBeadRawFields = append(json.RawMessage(nil), req.Fields...)
	m.lastCreateBeadReq = req
	var fields map[string]string
	if len(req.Fields) > 0 {
		// Fields may contain mixed-type values (e.g. integer ratings).
		// Decode into map[string]any and stringify so the mock's
		// map[string]string Fields stays usable for assertions.
		var raw map[string]any
		if err := json.Unmarshal(req.Fields, &raw); err == nil {
			fields = make(map[string]string, len(raw))
			for k, v := range raw {
				fields[k] = fmt.Sprintf("%v", v)
			}
		}
	}

	if req.Type == "message" {
		if err := messageCardRule(fields); err != nil {
			return "", fmt.Errorf("creating bead: %w", err)
		}
	}

	// Mirror the daemon's idempotency-key lookup-or-return contract
	// (kbeads internal/server/beads_idempotency.go, ka-0000002856): when the
	// caller supplies an IdempotencyKey, persist it under
	// fields.idempotency_key and return any existing non-deleted bead in the
	// same project that already carries it instead of inserting a duplicate.
	// This is what makes getOrCreateThreadBead race-safe across binaries; the
	// mock reproduces it so the bridge-side race test exercises the real
	// guarantee rather than a fake. The lock above serializes the
	// lookup+insert, modelling the per-fleet partial unique index (migration
	// 029) that backstops the concurrent-insert race at the real daemon.
	if req.IdempotencyKey != "" {
		if fields == nil {
			fields = make(map[string]string, 1)
		}
		fields["idempotency_key"] = req.IdempotencyKey
		reqProject := projectFromLabels(req.Labels)
		for _, b := range m.beads {
			if b.Fields["idempotency_key"] == req.IdempotencyKey &&
				projectFromLabels(b.Labels) == reqProject {
				return b.ID, nil
			}
		}
	}

	m.beads[id] = &beadsapi.BeadDetail{
		ID:    id,
		Title: req.Title,
		Type:  req.Type,
		// Priority is carried through verbatim (including an explicit 0) so
		// tests can assert on it. CreateBeadRequest.Priority has no omitempty,
		// so an unset field really is transmitted as P0 — the trap behind
		// ka-0000003760, where thread heads were silently minted at top
		// priority and buried every real bead in `gb ready`.
		Priority:    req.Priority,
		Status:      "open",
		Assignee:    req.Assignee,
		Labels:      req.Labels,
		Fields:      fields,
		Description: req.Description,
	}
	return id, nil
}

func (m *mockDaemon) SpawnAgent(ctx context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSpawnAgentCtx = ctx
	if m.spawnAgentErr != nil {
		return "", m.spawnAgentErr
	}
	if role == "" {
		role = "crew"
	}
	id := fmt.Sprintf("bd-agent-%d", len(m.beads)+1)
	desc := ""
	if taskID != "" {
		desc = "Assigned to task: " + taskID
	}
	fields := map[string]string{"agent": agentName, "project": project, "mode": "crew", "role": role}
	if customPrompt != "" {
		fields["prompt"] = customPrompt
	}
	if taskID != "" {
		fields["task_id"] = taskID
	}
	for _, extra := range extraFields {
		for k, v := range extra {
			fields[k] = v
		}
	}
	m.beads[id] = &beadsapi.BeadDetail{
		ID:          id,
		Title:       agentName,
		Type:        "agent",
		Description: desc,
		Fields:      fields,
	}
	return id, nil
}

func (m *mockDaemon) ListDecisionBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		if b.Type == "decision" {
			result = append(result, b)
		}
	}
	return result, nil
}

func (m *mockDaemon) ListAssignedTask(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range m.beads {
		if b.Status == "in_progress" && b.Assignee == agentName && b.Kind == "issue" {
			return b, nil
		}
	}
	return nil, nil
}

func (m *mockDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listProjectsErr != nil {
		return nil, m.listProjectsErr
	}
	result := make(map[string]beadsapi.ProjectInfo)
	for _, b := range m.beads {
		if b.Type == "project" {
			info := beadsapi.ProjectInfo{Name: b.Title}
			if b.Fields != nil {
				if ch := b.Fields["slack_channel"]; ch != "" {
					info.SlackChannels = []string{ch}
				}
				if prefix := b.Fields["prefix"]; prefix != "" {
					info.Prefix = prefix
				}
				if raw := b.Fields["channel_roles"]; raw != "" {
					var roles map[string]string
					if json.Unmarshal([]byte(raw), &roles) == nil {
						info.ChannelRoles = beadsapi.ParseChannelRolesMap(roles)
					}
				}
				if raw := b.Fields["channel_modes"]; raw != "" {
					var modes map[string]string
					if json.Unmarshal([]byte(raw), &modes) == nil {
						info.ChannelModes = modes
					}
				}
				if raw := b.Fields["channel_runtime_class"]; raw != "" {
					var rcs map[string]string
					if json.Unmarshal([]byte(raw), &rcs) == nil {
						info.ChannelRuntimeClasses = rcs
					}
				}
				if b.Fields["dm_default"] == "true" {
					info.DMDefault = true
				}
				info.LandKind = b.Fields["land_kind"]
				info.LandDefaultChannel = b.Fields["land_default_channel"]
				// Default true unless explicitly "false".
				info.SchedulesEnabled = b.Fields["schedules_enabled"] != "false"
			}
			result[b.Title] = info
		}
	}
	return result, nil
}

func (m *mockDaemon) ListAgentBeads(_ context.Context) ([]beadsapi.AgentBead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []beadsapi.AgentBead
	for _, b := range m.beads {
		if b.Type == "agent" {
			// Mirror production: ListAgentBeads queries activeStatuses only, so
			// a closed agent bead drops out of the list (ka-0000004075). Empty
			// status is treated as active to preserve existing test fixtures.
			if b.Status == "closed" {
				continue
			}
			result = append(result, beadsapi.AgentBead{
				ID:         b.ID,
				Title:      b.Title,
				Project:    b.Fields["project"],
				Mode:       "crew",
				Role:       b.Fields["role"],
				AgentName:  b.Fields["agent"],
				AgentState: b.Fields["agent_state"],
				// Mirror production: ListAgentBeads carries the bead's updated_at
				// (client.go), which the sweep's spawn-stall-grace check reads to
				// demote a wedged non-terminal member (1b-9, ka-0000007017).
				UpdatedAt: b.UpdatedAt,
				Metadata:  b.Fields,
			})
		}
	}
	return result, nil
}

func (m *mockDaemon) ListAccountBeads(_ context.Context) (map[string]beadsapi.AccountInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make(map[string]beadsapi.AccountInfo)
	for _, b := range m.beads {
		if b.Type == "account" && b.Title != "" {
			result[b.Title] = beadsapi.AccountInfo{
				Name:   b.Title,
				Fields: b.Fields,
			}
		}
	}
	return result, nil
}

func (m *mockDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listBeadsErr != nil {
		return nil, m.listBeadsErr
	}
	var result []*beadsapi.BeadDetail
	for _, b := range m.beads {
		match := true
		if len(q.Types) > 0 {
			typeMatch := false
			for _, t := range q.Types {
				if b.Type == t {
					typeMatch = true
					break
				}
			}
			if !typeMatch {
				match = false
			}
		}
		if match && len(q.Statuses) > 0 {
			statusMatch := false
			for _, s := range q.Statuses {
				if b.Status == s {
					statusMatch = true
					break
				}
			}
			if !statusMatch {
				match = false
			}
		}
		if match && len(q.Labels) > 0 {
			// All requested labels must be present on the bead (AND semantics).
			for _, want := range q.Labels {
				found := false
				for _, have := range b.Labels {
					if have == want {
						found = true
						break
					}
				}
				if !found {
					match = false
					break
				}
			}
		}
		if match && len(q.Fields) > 0 {
			// All requested field predicates must match (AND semantics),
			// mirroring the daemon's fields->>'k' = v equality filter.
			for k, v := range q.Fields {
				if b.Fields[k] != v {
					match = false
					break
				}
			}
		}
		if match {
			result = append(result, b)
		}
	}
	return &beadsapi.ListBeadsResult{Beads: result, Total: len(result)}, nil
}

func (m *mockDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deps = append(m.deps, depCall{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		DepType:     depType,
		CreatedBy:   createdBy,
	})
	return nil
}

func (m *mockDaemon) getDeps() []depCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]depCall, len(m.deps))
	copy(out, m.deps)
	return out
}

// UpdateBead applies a status change and/or a partial fields patch in one call,
// mirroring the daemon's atomic reopen-in-place semantics used by
// respawnThreadAgent's revive path (ka-0000006807). Fields follow RFC-7386
// merge: a nil value DELETES the key, any other value sets it (stringified so
// the mock's map[string]string store stays usable for assertions). The bead is
// resolved by ID first, then by a scan on BeadDetail.ID (agent beads are keyed
// by name in the mock, so a reopen addressed by ID must fall back to the scan).
func (m *mockDaemon) UpdateBead(_ context.Context, beadID string, req beadsapi.UpdateBeadRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updateBeadCalls++
	if m.updateBeadHook != nil {
		if err := m.updateBeadHook(m.updateBeadCalls, beadID, req); err != nil {
			return err
		}
	}
	bead, ok := m.beads[beadID]
	if !ok {
		for _, candidate := range m.beads {
			if candidate != nil && candidate.ID == beadID {
				bead, ok = candidate, true
				break
			}
		}
	}
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	if req.Status != nil {
		bead.Status = *req.Status
	}
	if req.Description != nil {
		bead.Description = *req.Description
	}
	if len(req.Fields) > 0 {
		if bead.Fields == nil {
			bead.Fields = make(map[string]string)
		}
		for k, v := range req.Fields {
			if v == nil {
				delete(bead.Fields, k)
				continue
			}
			bead.Fields[k] = fmt.Sprintf("%v", v)
		}
	}
	return nil
}

func (m *mockDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.updateFieldsErr != nil {
		if err := m.updateFieldsErr(beadID, fields); err != nil {
			return err
		}
	}
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	if bead.Fields == nil {
		bead.Fields = make(map[string]string)
	}
	for k, v := range fields {
		bead.Fields[k] = v
	}
	return nil
}

// AddLabel appends a label to a bead, preserving existing labels and
// deduplicating. Mirrors the daemon's idempotent label-add semantics so the
// failed→needs-triage split's idempotency tests (1b-7, ka-0000003060) hold.
func (m *mockDaemon) AddLabel(_ context.Context, beadID, label string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	for _, l := range bead.Labels {
		if l == label {
			return nil
		}
	}
	bead.Labels = append(bead.Labels, label)
	return nil
}

// GetMentionedBy returns the seeded reverse-mention references for beadID, so
// the C4 auto-close reactor can be driven against a subject's remediation/tune-
// tasks. Seed via m.mentionedBy[subject]; mentionedByErr forces a lookup failure.
func (m *mockDaemon) GetMentionedBy(_ context.Context, beadID string, limit int) ([]beadsapi.BeadReferenceSummary, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mentionedByErr != nil {
		return nil, m.mentionedByErr
	}
	refs := m.mentionedBy[beadID]
	if limit > 0 && len(refs) > limit {
		refs = refs[:limit]
	}
	return append([]beadsapi.BeadReferenceSummary{}, refs...), nil
}

// GetReverseDependencies returns the seeded reverse dependencies for beadID, so
// the C4 auto-close leaf guard can be exercised. Seed via m.reverseDeps[id];
// reverseDepsErr forces a lookup failure.
func (m *mockDaemon) GetReverseDependencies(_ context.Context, beadID string) ([]beadsapi.Dependency, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.reverseDepsErr != nil {
		return nil, m.reverseDepsErr
	}
	return append([]beadsapi.Dependency{}, m.reverseDeps[beadID]...), nil
}

func (m *mockDaemon) UpdateBeadFieldsTyped(_ context.Context, beadID string, fields map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	bead, ok := m.beads[beadID]
	if !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	if bead.Fields == nil {
		bead.Fields = make(map[string]string)
	}
	for k, v := range fields {
		bead.Fields[k] = fmt.Sprintf("%v", v)
	}
	return nil
}

func (m *mockDaemon) AddComment(ctx context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastAddCommentCtx = ctx
	m.comments = append(m.comments, commentCall{BeadID: beadID, Author: author, Text: text})
	// Mirror the write into the comment history so GetComments (used by the
	// thread-task-autocloser's hint dedupe) sees comments added during a test.
	// Stamp a deterministic, strictly-increasing CreatedAt: each comment is
	// newer than the previous, mirroring the daemon's append-only ordering.
	if m.beadComments == nil {
		m.beadComments = make(map[string][]beadsapi.Comment)
	}
	m.commentSeq++
	m.beadComments[beadID] = append(m.beadComments[beadID], beadsapi.Comment{
		Author:    author,
		Text:      text,
		CreatedAt: time.Unix(0, 0).Add(time.Duration(m.commentSeq) * time.Second),
	})
	return nil
}

func (m *mockDaemon) GetComments(_ context.Context, beadID string) ([]beadsapi.Comment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.beadComments == nil {
		return nil, nil
	}
	out := append([]beadsapi.Comment(nil), m.beadComments[beadID]...)
	return out, nil
}

func (m *mockDaemon) ListSlackOperatorAliases(_ context.Context) (*beadsapi.ListSlackOperatorAliasesResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.slackAliasCalls++
	if m.slackAliasErr != nil {
		return nil, m.slackAliasErr
	}
	return &beadsapi.ListSlackOperatorAliasesResponse{Aliases: m.slackAliases}, nil
}

func (m *mockDaemon) ResyncOperatorIdentity(_ context.Context, _ int64) (*beadsapi.UpdateOperatorIdentityResponse, error) {
	return &beadsapi.UpdateOperatorIdentityResponse{Updated: true}, nil
}

func (m *mockDaemon) ResyncUserBeadAliases(_ context.Context, _ string) (*beadsapi.LinkOperatorAliasesResponse, error) {
	return &beadsapi.LinkOperatorAliasesResponse{}, nil
}

func (m *mockDaemon) getComments() []commentCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]commentCall{}, m.comments...)
}

func (m *mockDaemon) ResolveTicket(_ context.Context, ticketKey string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Check direct bead ID match.
	if b, ok := m.beads[ticketKey]; ok {
		return b, nil
	}
	// Check jira_key field match.
	for _, b := range m.beads {
		if b.Fields != nil && b.Fields["jira_key"] == ticketKey {
			return b, nil
		}
	}
	return nil, fmt.Errorf("ticket %q not found", ticketKey)
}

func (m *mockDaemon) getClosed() []closeCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]closeCall{}, m.closed...)
}

func (m *mockDaemon) getGetCalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.getCalls
}

// mockNotifier records calls to NotifyDecision, UpdateDecision, NotifyEscalation, DismissDecision, and PostReport.
type mockNotifier struct {
	mu        sync.Mutex
	created   []BeadEvent
	updated   []updateCall
	escalated []BeadEvent
	dismissed []string
	reports   []reportCall
}

type reportCall struct {
	DecisionID string
	ReportType string
	Content    string
}

type updateCall struct {
	BeadID    string
	Chosen    string
	Rationale string
}

func (m *mockNotifier) NotifyDecision(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, bead)
	return nil
}

func (m *mockNotifier) UpdateDecision(_ context.Context, beadID, chosen, rationale string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.updated = append(m.updated, updateCall{beadID, chosen, rationale})
	return nil
}

func (m *mockNotifier) NotifyEscalation(_ context.Context, bead BeadEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.escalated = append(m.escalated, bead)
	return nil
}

func (m *mockNotifier) DismissDecision(_ context.Context, beadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dismissed = append(m.dismissed, beadID)
	return nil
}

func (m *mockNotifier) PostReport(_ context.Context, _, decisionID, reportType, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports = append(m.reports, reportCall{decisionID, reportType, content})
	return nil
}

func (m *mockNotifier) getReports() []reportCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]reportCall{}, m.reports...)
}

func (m *mockNotifier) getCreated() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.created...)
}

func (m *mockNotifier) getUpdated() []updateCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]updateCall{}, m.updated...)
}

func (m *mockNotifier) getEscalated() []BeadEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]BeadEvent{}, m.escalated...)
}

func (m *mockNotifier) getDismissed() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string{}, m.dismissed...)
}

// marshalSSEBeadPayload wraps a BeadEvent in the kbeads SSE event format
// ({"bead": {...}}) for testing handleCreated/handleClosed which now accept
// raw SSE JSON data.
func marshalSSEBeadPayload(bead BeadEvent) []byte {
	data, _ := json.Marshal(map[string]any{"bead": bead})
	return data
}

// messageCardRule mirrors the daemon's per-bead card rules on a type=message
// create (kbeads validateMessageCardFields, card_validation.go), so a bridge
// write the daemon would refuse fails here too. Without it, death and crash
// notices carrying subject_id passed every test and were refused on live
// (gd-JGmXQuSndB).
func messageCardRule(f map[string]string) error {
	kind, subject := strings.TrimSpace(f["kind"]), strings.TrimSpace(f["subject_id"])
	if kind == "card" && subject == "" {
		return fmt.Errorf("a kind=card message needs subject_id: the bead the card renders")
	}
	if subject != "" && kind != "card" {
		return fmt.Errorf("subject_id is only for kind=card messages (got kind=%q)", kind)
	}
	if (f["slack_channel"] != "" || f["slack_ts"] != "") && kind == "" {
		return fmt.Errorf("slack_channel/slack_ts are the Slack ref of a conversation message and need a kind")
	}
	return nil
}
