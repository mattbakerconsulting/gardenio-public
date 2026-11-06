package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// e2eDaemon is a combined mock that satisfies GitLabBeadClient,
// AgentResolverClient, GitLabSquawkClient, and (with the M2b/§3.1 fields
// below) MentionBeadClient for E2E testing.
type e2eDaemon struct {
	mu           sync.Mutex
	beads        map[string]*beadsapi.BeadDetail // bead ID → bead
	agentBeads   map[string]*beadsapi.BeadDetail // agent name → agent bead
	comments     []commentRecord
	spawnedBeads []*beadsapi.BeadDetail

	// MentionBeadClient extensions for the auto-link e2e suite (epic
	// ka-0000000933 / plan gd-T5vMf08jDN §3.1). Default-zero values keep the
	// existing review-comment e2e tests working unchanged: nil maps short-
	// circuit to nil returns.
	mrBeadsByURL  map[string]*beadsapi.BeadDetail   // MR URL → MR bead
	tasksByMRBead map[string][]*beadsapi.BeadDetail // MR bead ID → producing task beads
	projects      map[string]beadsapi.ProjectInfo
	createdBeads  []beadsapi.CreateBeadRequest
	createBeadID  string // returned by CreateBead; defaults to "bd-e2e-created"
	depCalls      []depCall
	// spawnAgentErr, when non-nil, makes SpawnAgent return it WITHOUT recording
	// a spawn — modelling the webhook-time dispatch failure the mention-catchup
	// watchdog exists to recover from (ka-0000000279). Toggle it off before the
	// watchdog runs so the rescue spawn succeeds.
	spawnAgentErr error
}

func newE2EDaemon() *e2eDaemon {
	return &e2eDaemon{
		beads:      make(map[string]*beadsapi.BeadDetail),
		agentBeads: make(map[string]*beadsapi.BeadDetail),
	}
}

// --- GitLabBeadClient methods ---

func (d *e2eDaemon) ListTaskBeads(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range d.beads {
		if b.Type == "task" {
			result = append(result, b)
		}
	}
	return result, nil
}

func (d *e2eDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	return d.ListTaskBeads(context.Background())
}

// ListTaskBeadsForMR mirrors the daemon's server-side fields.mr_url equality
// predicate so the e2e path exercises the same narrowing as production.
func (d *e2eDaemon) ListTaskBeadsForMR(_ context.Context, mrURL string) ([]*beadsapi.BeadDetail, error) {
	if mrURL == "" {
		return nil, nil
	}
	all, err := d.ListTaskBeads(context.Background())
	if err != nil {
		return nil, err
	}
	var result []*beadsapi.BeadDetail
	for _, b := range all {
		if b.Fields["mr_url"] == mrURL {
			result = append(result, b)
		}
	}
	return result, nil
}

func (d *e2eDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Check task beads by ID.
	if bead, ok := d.beads[beadID]; ok {
		if bead.Fields == nil {
			bead.Fields = make(map[string]string)
		}
		for k, v := range fields {
			bead.Fields[k] = v
		}
		return nil
	}
	// Check agent beads by ID (agentBeads is keyed by name, so scan by ID).
	for _, bead := range d.agentBeads {
		if bead.ID == beadID {
			if bead.Fields == nil {
				bead.Fields = make(map[string]string)
			}
			for k, v := range fields {
				bead.Fields[k] = v
			}
			return nil
		}
	}
	return fmt.Errorf("bead %s not found", beadID)
}

func (d *e2eDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.beads[beadID]; !ok {
		return fmt.Errorf("bead %s not found", beadID)
	}
	d.comments = append(d.comments, commentRecord{BeadID: beadID, Author: author, Text: text})
	return nil
}

func (d *e2eDaemon) UpsertMergeRequestBead(_ context.Context, _ beadsapi.MergeRequestBeadFields, _ bool) (string, error) {
	return "", nil
}

func (d *e2eDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.depCalls = append(d.depCalls, depCall{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		DepType:     depType,
		CreatedBy:   createdBy,
	})
	return nil
}

// --- MentionBeadClient extensions (auto-link §3.1) ---

func (d *e2eDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.createdBeads = append(d.createdBeads, req)
	id := d.createBeadID
	if id == "" {
		id = fmt.Sprintf("bd-e2e-created-%d", len(d.createdBeads))
	}
	// Decode the typed fields blob (best-effort) so downstream readers — e.g.
	// the mention-catchup watchdog's spawn_source/mr_url/created_at reads — see
	// what the webhook stamped, not just the title/type.
	var fields map[string]string
	if len(req.Fields) > 0 {
		_ = json.Unmarshal(req.Fields, &fields)
	}
	// Mirror into d.beads so subsequent lookups succeed.
	d.beads[id] = &beadsapi.BeadDetail{
		ID:        id,
		Title:     req.Title,
		Type:      req.Type,
		Status:    "open",
		Assignee:  req.Assignee,
		Labels:    req.Labels,
		Fields:    fields,
		CreatedAt: time.Now(),
	}
	return id, nil
}

func (d *e2eDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.projects == nil {
		return map[string]beadsapi.ProjectInfo{}, nil
	}
	return d.projects, nil
}

func (d *e2eDaemon) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mrBeadsByURL == nil {
		return nil, nil
	}
	return d.mrBeadsByURL[mrURL], nil
}

func (d *e2eDaemon) ProducingTaskBeadsForMR(_ context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.tasksByMRBead == nil {
		return nil, nil
	}
	return d.tasksByMRBead[mrBeadID], nil
}

func (d *e2eDaemon) getDepCalls() []depCall {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]depCall, len(d.depCalls))
	copy(out, d.depCalls)
	return out
}

// MRBeadForTask is a no-op stub for the e2e harness — the e2e flow exercises
// the legacy task-bead mr_* field path. Tests that need MR-bead lookups use
// the focused mockResolverDaemon harness instead.
func (d *e2eDaemon) MRBeadForTask(_ context.Context, _ string) (*beadsapi.BeadDetail, error) {
	return nil, nil
}

// --- AgentResolverClient methods ---

func (d *e2eDaemon) FindAgentBead(_ context.Context, agentName string) (*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if b, ok := d.agentBeads[agentName]; ok {
		return b, nil
	}
	return nil, &beadsapi.APIError{StatusCode: 404, Message: "not found"}
}

func (d *e2eDaemon) ListActiveAgentBeadDetails(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var result []*beadsapi.BeadDetail
	for _, b := range d.agentBeads {
		result = append(result, b)
	}
	return result, nil
}

// ListBeadsFiltered serves the small subset of predicates the mention-catchup
// watchdog uses (type, status, field equality) across BOTH d.beads (task beads)
// and d.agentBeads (agent beads), so a single call can find the orphaned task
// and check for a live linked agent bead. Mirrors the daemon's AND semantics.
func (d *e2eDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all := make([]*beadsapi.BeadDetail, 0, len(d.beads)+len(d.agentBeads))
	for _, b := range d.beads {
		all = append(all, b)
	}
	for _, b := range d.agentBeads {
		all = append(all, b)
	}
	var out []*beadsapi.BeadDetail
	for _, b := range all {
		if len(q.Types) > 0 && !typeIn(b.Type, q.Types) {
			continue
		}
		if len(q.Statuses) > 0 && !statusIn(b.Status, q.Statuses) {
			continue
		}
		if !fieldsMatch(b.Fields, q.Fields) {
			continue
		}
		out = append(out, b)
	}
	return &beadsapi.ListBeadsResult{Beads: out, Total: len(out)}, nil
}

func (d *e2eDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, customPrompt string, extraFields ...map[string]string) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.spawnAgentErr != nil {
		return "", d.spawnAgentErr
	}
	id := "bd-spawned-" + agentName
	fields := map[string]string{
		"agent":   agentName,
		"project": project,
		"mode":    "crew",
		"role":    role,
		"prompt":  customPrompt,
	}
	if taskID != "" {
		fields["task_id"] = taskID
	}
	for _, extra := range extraFields {
		for k, v := range extra {
			fields[k] = v
		}
	}
	bead := &beadsapi.BeadDetail{
		ID:     id,
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: fields,
	}
	d.spawnedBeads = append(d.spawnedBeads, bead)
	// Also register as an agent bead so squawk forwarder can find it.
	d.agentBeads[agentName] = bead
	return id, nil
}

// --- Test helpers ---

func (d *e2eDaemon) getComments() []commentRecord {
	d.mu.Lock()
	defer d.mu.Unlock()
	result := make([]commentRecord, len(d.comments))
	copy(result, d.comments)
	return result
}

func (d *e2eDaemon) getBead(id string) *beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.beads[id]
}

func (d *e2eDaemon) getSpawned() []*beadsapi.BeadDetail {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*beadsapi.BeadDetail{}, d.spawnedBeads...)
}

// TestE2E_GitLabMention_MentionTaskHasDepToMRBead drives a `@gardenio`
// note webhook through the full GitLab webhook handler →
// MentionHandler.HandleMention → CreateBead + linkMentionTaskToMR path,
// and asserts the freshly-created mention task carries a `triggered-by`
// dep to the MR bead. Plan gd-T5vMf08jDN §3.1 (epic ka-0000000933 / M2b).
//
// Distinct from TestHandleMention_MentionTaskTriggeredByMRBead (M2b unit
// test): that test calls h.HandleMention directly with a synthesized
// noteContext. This e2e test POSTs a realistic GitLab note webhook
// payload through GitLabWebhookHandlerWithConfig so the wire-level
// routing (HMAC, JSON parse, noteContext construction) is also pinned.
func TestE2E_GitLabMention_MentionTaskHasDepToMRBead(t *testing.T) {
	const (
		mrURL    = "https://gitlab.com/group/project/-/merge_requests/777"
		mrBeadID = "gd-mr-bead-777"
		botUser  = "group_99_bot_xyz"
		secret   = "test-secret"
	)

	daemon := newE2EDaemon()
	daemon.createBeadID = "gd-mention-task-1"
	daemon.mrBeadsByURL = map[string]*beadsapi.BeadDetail{
		mrURL: {
			ID:    mrBeadID,
			Type:  "merge_request",
			Title: "MR for auto-link e2e",
		},
	}

	mentionHandler := NewMentionHandler(MentionHandlerConfig{
		Daemon:      daemon,
		Trigger:     "@gardenio",
		BotUsername: botUser,
		Project:     "gardenio",
		Users:       testUserLookup(), // grants the "mention" capability to "reviewer".
		Logger:      slog.Default(),
	})

	webhookHandler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:         daemon,
		WebhookSecret:  secret,
		BotUsername:    botUser,
		MentionHandler: mentionHandler,
		Logger:         slog.Default(),
	})

	noteEvent := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer"},
		"object_attributes": map[string]any{
			"note":          "@gardenio please rebase this branch",
			"noteable_type": "MergeRequest",
			"system":        false,
			"discussion_id": "disc-1",
		},
		"merge_request": map[string]any{
			"iid":   777,
			"url":   mrURL,
			"title": "MR for auto-link e2e",
		},
	}
	body, _ := json.Marshal(noteEvent)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", secret)
	rr := httptest.NewRecorder()
	webhookHandler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("webhook POST returned %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	// The mention handler must have created exactly one tracking task.
	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created tracking bead, got %d", len(daemon.createdBeads))
	}
	if daemon.createdBeads[0].Type != "task" {
		t.Errorf("created bead type = %q, want task", daemon.createdBeads[0].Type)
	}

	// Assert: tracking task → triggered-by → MR bead with CreatedBy=gitlab-bridge.
	var sawDep bool
	for _, d := range daemon.getDepCalls() {
		if d.DepType != "triggered-by" {
			continue
		}
		if d.BeadID == "gd-mention-task-1" && d.DependsOnID == mrBeadID {
			sawDep = true
			if d.CreatedBy != "gitlab-bridge" {
				t.Errorf("triggered-by dep CreatedBy = %q, want gitlab-bridge", d.CreatedBy)
			}
		}
	}
	if !sawDep {
		t.Errorf("missing triggered-by dep from mention task to MR bead %s (deps=%+v)",
			mrBeadID, daemon.getDepCalls())
	}
}

// TestE2E_GitLabReviewComment_AgentAlive_ResponsePostedAsMRNote validates:
// 1. GitLab note webhook → bead comment + mr_has_review_comments flag
// 2. SSE bead update → agent resolver nudges alive agent via coop
// 3. Agent squawk → squawk forwarder posts MR note
func TestE2E_GitLabReviewComment_AgentAlive_ResponsePostedAsMRNote(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/42"
	const agentName = "review-agent"

	// Track nudge messages received by the coop server.
	var nudgeMessages []string
	var nudgeMu sync.Mutex
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeMu.Lock()
			nudgeMessages = append(nudgeMessages, body["message"])
			nudgeMu.Unlock()
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	// Track MR notes posted to GitLab.
	var postedNotes []string
	var notesMu sync.Mutex
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/notes") {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			notesMu.Lock()
			postedNotes = append(postedNotes, body["body"])
			notesMu.Unlock()
			resp := GitLabNote{ID: 5001, Body: body["body"]}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		// Return MR details for GetMergeRequestByPath.
		if strings.Contains(r.URL.Path, "/merge_requests/") {
			resp := GitLabMR{IID: 42, ProjectID: 99, SourceBranch: "fix/auth", State: "opened"}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
	defer gitlabServer.Close()

	gitlabClient := newTestGitLabClient(gitlabServer.URL)
	logger := slog.Default()

	// Set up the combined mock daemon.
	daemon := newE2EDaemon()

	// Seed: task bead with mr_url assigned to an agent.
	daemon.beads["gd-task-1"] = &beadsapi.BeadDetail{
		ID:       "gd-task-1",
		Title:    "Fix auth token refresh",
		Type:     "task",
		Assignee: agentName,
		Fields:   map[string]string{"mr_url": mrURL},
	}

	// Seed: agent bead with coop_url (alive).
	daemon.agentBeads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-agent-1",
		Title: agentName,
		Type:  "agent",
		Notes: "coop_url: " + coopServer.URL,
		Fields: map[string]string{
			"agent":   agentName,
			"project": "gardenio",
		},
	}

	ctx := context.Background()

	// ──────────────────────────────────────────────────────────────────
	// STEP 1: GitLab note webhook → bead comment + review flag
	// ──────────────────────────────────────────────────────────────────
	// Use the realistic GitLab group-bot username format `group_<id>_bot_<hash>`
	// (gd-bi3lO4oBIh) — the bridge must accept whatever GITLAB_BOT_USERNAME
	// supplies, not a hardcoded "gardenio-bot".
	const gitlabGroupBot = "group_14176856_bot_abc123"

	webhookHandler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		GitLab:        gitlabClient,
		Daemon:        daemon,
		WebhookSecret: "test-secret",
		BotUsername:   gitlabGroupBot,
		Nudge: func(ctx context.Context, agent, msg string) error {
			// Webhook-level nudge (simple, not resolver).
			return nil
		},
		Logger: logger,
	})

	noteEvent := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer-alice"},
		"object_attributes": map[string]any{
			"note":          "@" + gitlabGroupBot + " Please add error handling on line 42",
			"noteable_type": "MergeRequest",
			"system":        false,
			"discussion_id": "disc-e2e-abc",
			"position": map[string]any{
				"new_path": "pkg/auth/handler.go",
				"new_line": 42,
			},
		},
		"merge_request": map[string]any{
			"iid":   42,
			"url":   mrURL,
			"title": "Fix auth token refresh",
		},
	}
	body, _ := json.Marshal(noteEvent)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "test-secret")
	w := httptest.NewRecorder()
	webhookHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("step 1: webhook returned %d, want 200", w.Code)
	}

	// Verify comment was added to the bead.
	comments := daemon.getComments()
	if len(comments) != 1 {
		t.Fatalf("step 1: expected 1 comment, got %d", len(comments))
	}
	if comments[0].BeadID != "gd-task-1" {
		t.Errorf("step 1: comment bead ID = %q, want gd-task-1", comments[0].BeadID)
	}
	if !strings.Contains(comments[0].Text, "reviewer-alice") {
		t.Errorf("step 1: comment should mention reviewer, got: %s", comments[0].Text)
	}
	if !strings.Contains(comments[0].Text, "pkg/auth/handler.go:42") {
		t.Errorf("step 1: comment should contain file:line, got: %s", comments[0].Text)
	}

	// Verify mr_has_review_comments flag was set.
	taskBead := daemon.getBead("gd-task-1")
	if taskBead.Fields["mr_has_review_comments"] != "true" {
		t.Errorf("step 1: mr_has_review_comments = %q, want true", taskBead.Fields["mr_has_review_comments"])
	}

	// ──────────────────────────────────────────────────────────────────
	// STEP 2: SSE bead update → agent resolver nudges alive agent
	// ──────────────────────────────────────────────────────────────────
	resolver := NewAgentResolver(AgentResolverConfig{
		Daemon: daemon,
		GitLab: gitlabClient,
		Nudge:  subscriberNudge(daemon.FindAgentBead),
		Logger: logger,
	})

	gitlabSync := NewGitLabSync(GitLabSyncConfig{
		Daemon:   daemon,
		GitLab:   gitlabClient,
		Logger:   logger,
		Resolver: resolver,
	})

	// Simulate the SSE bead.updated event that fires when mr_has_review_comments is set.
	sseData, _ := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":       "gd-task-1",
			"type":     "task",
			"title":    "Fix auth token refresh",
			"assignee": agentName,
			"fields": map[string]string{
				"mr_has_review_comments": "true",
				"mr_url":                 mrURL,
			},
		},
		"changes": map[string]any{
			"mr_has_review_comments": "true",
		},
	})
	gitlabSync.handleReviewNudge(ctx, sseData)

	// Verify the nudge was delivered to the coop server.
	nudgeMu.Lock()
	nudgeCount := len(nudgeMessages)
	nudgeMu.Unlock()
	if nudgeCount != 1 {
		t.Fatalf("step 2: expected 1 nudge, got %d", nudgeCount)
	}
	nudgeMu.Lock()
	nudgeMsg := nudgeMessages[0]
	nudgeMu.Unlock()
	if !strings.Contains(nudgeMsg, "review comments") {
		t.Errorf("step 2: nudge should mention review comments, got: %s", nudgeMsg)
	}
	if !strings.Contains(nudgeMsg, mrURL) {
		t.Errorf("step 2: nudge should contain MR URL, got: %s", nudgeMsg)
	}

	// Verify MR binding fields were set on agent bead.
	daemon.mu.Lock()
	agentBead := daemon.agentBeads[agentName]
	agentMRURL := agentBead.Fields["gitlab_mr_url"]
	daemon.mu.Unlock()
	if agentMRURL != mrURL {
		t.Errorf("step 2: agent gitlab_mr_url = %q, want %q", agentMRURL, mrURL)
	}

	// ──────────────────────────────────────────────────────────────────
	// STEP 3: Agent squawk → squawk forwarder posts MR note
	// ──────────────────────────────────────────────────────────────────
	squawkFwd := NewGitLabSquawkForwarder(GitLabSquawkForwarderConfig{
		Daemon: daemon,
		GitLab: gitlabClient,
		Logger: logger,
	})

	// Simulate a closed squawk bead from the agent. This is a reviewer-facing
	// reply, so it carries the opt-in "to-mr" label (ka-0000005748) — the only
	// kind of squawk the forwarder mirrors to the MR.
	squawkData := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-e2e",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:" + agentName},
		Fields: map[string]string{
			"source_agent": agentName,
			"text":         "Fixed the error handling, please re-review",
		},
	})
	squawkFwd.handleClosed(ctx, squawkData)

	// Verify the squawk was posted as a GitLab MR note.
	notesMu.Lock()
	noteCount := len(postedNotes)
	notesMu.Unlock()
	if noteCount != 1 {
		t.Fatalf("step 3: expected 1 MR note, got %d", noteCount)
	}
	notesMu.Lock()
	noteBody := postedNotes[0]
	notesMu.Unlock()
	if !strings.Contains(noteBody, agentName) {
		t.Errorf("step 3: MR note should contain agent name, got: %s", noteBody)
	}
	if !strings.Contains(noteBody, "Fixed the error handling") {
		t.Errorf("step 3: MR note should contain squawk text, got: %s", noteBody)
	}
}

// TestE2E_GitLabReviewComment_AgentDead_SpawnsNewAgent validates:
// 1. GitLab note webhook → bead update
// 2. SSE bead update → agent resolver spawns a new agent (original is dead)
// 3. The spawned agent has MR context metadata
func TestE2E_GitLabReviewComment_AgentDead_SpawnsNewAgent(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/10"
	const agentName = "dead-agent"

	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := GitLabMR{IID: 10, ProjectID: 55, SourceBranch: "fix/deadcode", State: "opened"}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer gitlabServer.Close()

	gitlabClient := newTestGitLabClient(gitlabServer.URL)
	logger := slog.Default()

	daemon := newE2EDaemon()

	// Seed: task bead assigned to agent (no agent bead → agent is dead).
	daemon.beads["gd-task-2"] = &beadsapi.BeadDetail{
		ID:       "gd-task-2",
		Title:    "Remove dead code",
		Type:     "task",
		Assignee: agentName,
		Labels:   []string{"project:gardenio"},
		Fields:   map[string]string{"mr_url": mrURL},
	}

	ctx := context.Background()

	// Step 1: Webhook sets review flag.
	webhookHandler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		Logger:        logger,
	})

	noteEvent := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "bob"},
		"object_attributes": map[string]any{
			"note":          "This is still referenced",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid":   10,
			"url":   mrURL,
			"title": "Remove dead code",
		},
	}
	body, _ := json.Marshal(noteEvent)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	webhookHandler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook returned %d", w.Code)
	}

	// Step 2: SSE triggers resolver → agent is dead → spawn.
	resolver := NewAgentResolver(AgentResolverConfig{
		Daemon: daemon,
		GitLab: gitlabClient,
		Nudge:  subscriberNudge(daemon.FindAgentBead),
		Logger: logger,
	})

	gitlabSync := NewGitLabSync(GitLabSyncConfig{
		Daemon:   daemon,
		GitLab:   gitlabClient,
		Logger:   logger,
		Resolver: resolver,
	})

	sseData, _ := json.Marshal(map[string]any{
		"bead": map[string]any{
			"id":       "gd-task-2",
			"type":     "task",
			"title":    "Remove dead code",
			"assignee": agentName,
			"labels":   []string{"project:gardenio"},
			"fields": map[string]string{
				"mr_has_review_comments": "true",
				"mr_url":                 mrURL,
			},
		},
		"changes": map[string]any{
			"mr_has_review_comments": "true",
		},
	})
	gitlabSync.handleReviewNudge(ctx, sseData)

	// Verify agent was spawned.
	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(spawned))
	}
	agent := spawned[0]
	if agent.Fields["agent"] != agentName {
		t.Errorf("spawned agent name = %q, want %q", agent.Fields["agent"], agentName)
	}
	if agent.Fields["task_id"] != "gd-task-2" {
		t.Errorf("task_id = %q, want gd-task-2", agent.Fields["task_id"])
	}
	if agent.Fields["gitlab_mr_url"] != mrURL {
		t.Errorf("gitlab_mr_url = %q, want %q", agent.Fields["gitlab_mr_url"], mrURL)
	}
	if agent.Fields["gitlab_mr_source_branch"] != "fix/deadcode" {
		t.Errorf("gitlab_mr_source_branch = %q, want fix/deadcode", agent.Fields["gitlab_mr_source_branch"])
	}
	if agent.Fields["spawn_source"] != "gitlab-mr-review" {
		t.Errorf("spawn_source = %q, want gitlab-mr-review", agent.Fields["spawn_source"])
	}
	if agent.Fields["project"] != "gardenio" {
		t.Errorf("project = %q, want gardenio", agent.Fields["project"])
	}
}

// TestE2E_SquawkForwarder_DiscussionReply validates that when an agent has
// gitlab_discussion_id set, the squawk is posted as a discussion reply.
func TestE2E_SquawkForwarder_DiscussionReply(t *testing.T) {
	const mrURL = "https://gitlab.com/org/repo/-/merge_requests/7"
	const agentName = "reply-agent"

	var postedPath string
	var postedBody string
	var mu sync.Mutex
	gitlabServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			mu.Lock()
			postedPath = r.URL.Path
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			postedBody = body["body"]
			mu.Unlock()
			resp := GitLabNote{ID: 3001, Body: body["body"]}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(resp)
			return
		}
		http.NotFound(w, r)
	}))
	defer gitlabServer.Close()

	daemon := newE2EDaemon()
	daemon.agentBeads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-agent-reply",
		Title: agentName,
		Type:  "agent",
		Fields: map[string]string{
			"agent":                agentName,
			"gitlab_mr_url":        mrURL,
			"gitlab_mr_iid":        "7",
			"gitlab_discussion_id": "disc-reply-123",
		},
	}

	squawkFwd := NewGitLabSquawkForwarder(GitLabSquawkForwarderConfig{
		Daemon: daemon,
		GitLab: newTestGitLabClient(gitlabServer.URL),
		Logger: slog.Default(),
	})

	squawkData := marshalSSEBeadPayload(BeadEvent{
		ID:     "bd-squawk-reply",
		Type:   "message",
		Labels: []string{"squawk", "to-mr", "from:" + agentName},
		Fields: map[string]string{
			"source_agent": agentName,
			"text":         "Done, error handling added",
		},
	})
	squawkFwd.handleClosed(context.Background(), squawkData)

	mu.Lock()
	path := postedPath
	noteBody := postedBody
	mu.Unlock()

	// Verify it was posted as a discussion reply (path contains /discussions/).
	if !strings.Contains(path, "/discussions/disc-reply-123/notes") {
		t.Errorf("expected discussion reply path, got: %s", path)
	}
	if !strings.Contains(noteBody, agentName) {
		t.Errorf("reply should contain agent name, got: %s", noteBody)
	}
	if !strings.Contains(noteBody, "error handling added") {
		t.Errorf("reply should contain squawk text, got: %s", noteBody)
	}
}

// TestE2E_BotComment_Ignored validates that comments from the bot user
// do not trigger the review comment flow.
func TestE2E_BotComment_Ignored(t *testing.T) {
	daemon := newE2EDaemon()
	daemon.beads["gd-task-3"] = &beadsapi.BeadDetail{
		ID:       "gd-task-3",
		Type:     "task",
		Assignee: "some-agent",
		Fields:   map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/5"},
	}

	// Realistic GitLab group-bot username format (gd-bi3lO4oBIh).
	const gitlabGroupBot = "group_14176856_bot_abc123"

	handler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:        daemon,
		WebhookSecret: "secret",
		BotUsername:   gitlabGroupBot,
		Logger:        slog.Default(),
	})

	event := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": gitlabGroupBot},
		"object_attributes": map[string]any{
			"note":          "I've addressed this in the latest commit",
			"noteable_type": "MergeRequest",
			"system":        false,
		},
		"merge_request": map[string]any{
			"iid": 5,
			"url": "https://gitlab.com/org/repo/-/merge_requests/5",
		},
	}
	body, _ := json.Marshal(event)

	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	// Bot comments should not create bead comments or set the review flag.
	if len(daemon.getComments()) != 0 {
		t.Errorf("expected 0 comments for bot user, got %d", len(daemon.getComments()))
	}
	if daemon.getBead("gd-task-3").Fields["mr_has_review_comments"] != "" {
		t.Error("mr_has_review_comments should be empty for bot note")
	}
}

// TestE2E_GitLabMention_OrphanedTaskRespawnedByCatchup is the end-to-end
// acceptance test for the mention-catchup watchdog (ka-0000000279, plan
// ka-0000004007 Track A). It reproduces the Patrick !1160 incident and proves
// the rescue:
//
//  1. A realistic @gardenio note webhook is POSTed through the full webhook
//     handler → MentionHandler.HandleMention → CreateBead (the task lands) →
//     SpawnAgent (which FAILS, exactly as it did for all 3 of Patrick's
//     comments) → HandleMention returns false, leaving the task ORPHANED: open,
//     no agent bead, no pod.
//  2. Time passes (the task ages past the grace window) and the spawn failure
//     clears (the transient daemon hiccup is over).
//  3. The MentionCatchupWatchdog sweeps, detects the orphan (spawn_source=
//     gitlab-mention, aged, no live linked agent), and respawns the crew.
//
// This pins the whole seam the incident exposed: webhook create succeeds, spawn
// drops, and nothing else recovers it — until the watchdog does.
func TestE2E_GitLabMention_OrphanedTaskRespawnedByCatchup(t *testing.T) {
	const (
		mrURL   = "https://gitlab.com/acme/platform/monorepo/-/merge_requests/1160"
		botUser = "group_14176856_bot_xyz"
		secret  = "test-secret"
	)

	daemon := newE2EDaemon()
	daemon.createBeadID = "gd-mention-orphan"
	// The webhook-time SpawnAgent FAILS — the incident's dispatch drop.
	daemon.spawnAgentErr = fmt.Errorf("simulated dispatch failure: daemon 503")

	mentionHandler := NewMentionHandler(MentionHandlerConfig{
		Daemon:      daemon,
		Trigger:     "@gardenio",
		BotUsername: botUser,
		Project:     "gardenio",
		Users:       testUserLookup(),
		Logger:      slog.Default(),
	})
	webhookHandler := GitLabWebhookHandlerWithConfig(GitLabWebhookConfig{
		Daemon:         daemon,
		WebhookSecret:  secret,
		BotUsername:    botUser,
		MentionHandler: mentionHandler,
		Logger:         slog.Default(),
	})

	noteEvent := map[string]any{
		"object_kind": "note",
		"user":        map[string]any{"username": "reviewer"},
		"object_attributes": map[string]any{
			"note":          "@gardenio please address the review comment on foo.go",
			"noteable_type": "MergeRequest",
			"system":        false,
			"discussion_id": "disc-1160",
		},
		"merge_request": map[string]any{
			"iid":   1160,
			"url":   mrURL,
			"title": "Patrick's MR",
		},
	}
	body, _ := json.Marshal(noteEvent)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	req.Header.Set("X-Gitlab-Token", secret)
	rr := httptest.NewRecorder()
	webhookHandler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("webhook POST returned %d, want 200 (body=%s)", rr.Code, rr.Body.String())
	}

	// Step 1 assertion: the task bead exists but NO agent was spawned — the
	// orphan the incident produced.
	orphan := daemon.getBead("gd-mention-orphan")
	if orphan == nil {
		t.Fatal("expected the mention task bead to be created despite the spawn failure")
	}
	if orphan.Fields["spawn_source"] != "gitlab-mention" {
		t.Fatalf("task spawn_source = %q, want gitlab-mention", orphan.Fields["spawn_source"])
	}
	if orphan.Fields["mention_instruction"] == "" {
		t.Fatal("task must carry mention_instruction so the watchdog can rebuild the prompt")
	}
	if len(daemon.getSpawned()) != 0 {
		t.Fatalf("expected 0 spawned agents after the webhook-time failure, got %d", len(daemon.getSpawned()))
	}

	// Step 2: age the task past the grace window and clear the transient failure.
	daemon.mu.Lock()
	orphan.CreatedAt = time.Now().Add(-20 * time.Minute)
	daemon.spawnAgentErr = nil
	daemon.mu.Unlock()

	// Step 3: the watchdog sweeps and rescues the orphan.
	watchdog := NewMentionCatchupWatchdog(MentionCatchupConfig{
		Daemon:  daemon,
		Logger:  slog.Default(),
		Grace:   10 * time.Minute,
		Project: "gardenio",
	})
	watchdog.sweepOnce(context.Background())

	spawned := daemon.getSpawned()
	if len(spawned) != 1 {
		t.Fatalf("expected the watchdog to respawn exactly 1 crew, got %d", len(spawned))
	}
	agent := spawned[0]
	if agent.Fields["task_id"] != "gd-mention-orphan" {
		t.Errorf("respawned agent task_id = %q, want gd-mention-orphan", agent.Fields["task_id"])
	}
	if agent.Fields["spawn_source"] != "gitlab-mention" {
		t.Errorf("respawned agent spawn_source = %q, want gitlab-mention", agent.Fields["spawn_source"])
	}
	if agent.Fields["role"] != "crew" {
		t.Errorf("respawned agent role = %q, want crew", agent.Fields["role"])
	}
	if !strings.Contains(agent.Fields["prompt"], "please address the review comment") {
		t.Errorf("respawned agent prompt missing the original instruction; got:\n%s", agent.Fields["prompt"])
	}
	if !strings.Contains(agent.Fields["prompt"], "!1160") {
		t.Errorf("respawned agent prompt missing MR ref !1160; got:\n%s", agent.Fields["prompt"])
	}
	// An audit comment records the rescue on the task bead.
	var sawAudit bool
	for _, c := range daemon.getComments() {
		if c.BeadID == "gd-mention-orphan" && c.Author == mentionCatchupActor {
			sawAudit = true
		}
	}
	if !sawAudit {
		t.Errorf("expected a %q audit comment on the rescued task; comments=%+v", mentionCatchupActor, daemon.getComments())
	}

	// Step 4: idempotency — a second sweep must NOT respawn again (the first
	// rescue created a live agent bead linked to the task).
	watchdog.sweepOnce(context.Background())
	if got := len(daemon.getSpawned()); got != 1 {
		t.Fatalf("expected the watchdog to be idempotent (still 1 spawn), got %d", got)
	}
}

// pouring-stall dependency-graph stubs (ka-0000000957) — this mock does not
// exercise the sweep.
func (d *e2eDaemon) GetReverseDependencies(_ context.Context, _ string) ([]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *e2eDaemon) BatchGetDependencies(_ context.Context, _ []string, _ string) (map[string][]beadsapi.Dependency, error) {
	return nil, nil
}
func (d *e2eDaemon) BatchGetBeads(_ context.Context, _ []string) (map[string]*beadsapi.BeadDetail, error) {
	return nil, nil
}
