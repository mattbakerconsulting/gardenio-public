package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// mockMentionDaemon implements MentionBeadClient for testing.
type mockMentionDaemon struct {
	mu       sync.Mutex
	beads    []*beadsapi.BeadDetail
	projects map[string]beadsapi.ProjectInfo
	created  []beadsapi.CreateBeadRequest
	spawned  []spawnCall
	createID string
	spawnID  string
	// mrBeadsByURL + tasksByMRBead back the MR-bead-first lookup path
	// exercised by the migrated readers (epic ka-0000001613). Leaving them
	// empty exercises the legacy task-scan fallback.
	mrBeadsByURL  map[string]*beadsapi.BeadDetail
	tasksByMRBead map[string][]*beadsapi.BeadDetail
	// depCalls captures every AddDependency invocation so M2b tests
	// (epic ka-0000000933) can assert triggered-by edges. addDepErr, when
	// non-nil, makes the next AddDependency call return that error. depCall
	// is shared with bot_formula_test.go and uses DepType for the type field.
	depCalls  []depCall
	addDepErr error
	// comments captures every AddComment call so Track A2 tests can assert the
	// operator-visible dispatch-failure note; commentErr, when non-nil, makes
	// the next AddComment call return it.
	comments   []commentCall
	commentErr error
	// fieldUpdates captures every UpdateBeadFields call so Track A3 tests can
	// assert the nudge-delivery stamp; fieldUpdateErr, when non-nil, makes the
	// next UpdateBeadFields call return it.
	fieldUpdates   []fieldUpdateCall
	fieldUpdateErr error
	// spawnErr, when non-nil, makes the next SpawnAgent call fail without
	// recording a spawn — modelling the Track A2 webhook-time dispatch failure.
	spawnErr error
}

type fieldUpdateCall struct {
	BeadID string
	Fields map[string]string
}

type spawnCall struct {
	AgentName string
	Project   string
	TaskID    string
	Role      string
	Prompt    string
	Extra     map[string]string
}

func (m *mockMentionDaemon) CreateBead(_ context.Context, req beadsapi.CreateBeadRequest) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.created = append(m.created, req)
	return m.createID, nil
}

func (m *mockMentionDaemon) SpawnAgent(_ context.Context, agentName, project, taskID, role, prompt string, extra ...map[string]string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// spawnErr models the webhook-time dispatch failure (Track A2) — SpawnAgent
	// fails WITHOUT recording a spawn, the exact hole that dropped Patrick's
	// !1160 comments. It fires once, then clears so a later rescue spawn works.
	if m.spawnErr != nil {
		err := m.spawnErr
		m.spawnErr = nil
		return "", err
	}
	var extraFields map[string]string
	if len(extra) > 0 {
		extraFields = extra[0]
	}
	m.spawned = append(m.spawned, spawnCall{
		AgentName: agentName,
		Project:   project,
		TaskID:    taskID,
		Role:      role,
		Prompt:    prompt,
		Extra:     extraFields,
	})
	return m.spawnID, nil
}

func (m *mockMentionDaemon) ListTaskBeadsAll(_ context.Context) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.beads, nil
}

func (m *mockMentionDaemon) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.projects == nil {
		return map[string]beadsapi.ProjectInfo{}, nil
	}
	return m.projects, nil
}

func (m *mockMentionDaemon) MRBeadByURL(_ context.Context, mrURL string) (*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.mrBeadsByURL == nil {
		return nil, nil
	}
	return m.mrBeadsByURL[mrURL], nil
}

func (m *mockMentionDaemon) ProducingTaskBeadsForMR(_ context.Context, mrBeadID string) ([]*beadsapi.BeadDetail, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.tasksByMRBead == nil {
		return nil, nil
	}
	return m.tasksByMRBead[mrBeadID], nil
}

func (m *mockMentionDaemon) AddComment(_ context.Context, beadID, author, text string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.commentErr != nil {
		err := m.commentErr
		m.commentErr = nil
		return err
	}
	m.comments = append(m.comments, commentCall{BeadID: beadID, Author: author, Text: text})
	return nil
}

func (m *mockMentionDaemon) UpdateBeadFields(_ context.Context, beadID string, fields map[string]string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fieldUpdateErr != nil {
		err := m.fieldUpdateErr
		m.fieldUpdateErr = nil
		return err
	}
	m.fieldUpdates = append(m.fieldUpdates, fieldUpdateCall{BeadID: beadID, Fields: fields})
	return nil
}

func (m *mockMentionDaemon) AddDependency(_ context.Context, beadID, dependsOnID, depType, createdBy string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.addDepErr != nil {
		err := m.addDepErr
		m.addDepErr = nil
		return err
	}
	m.depCalls = append(m.depCalls, depCall{
		BeadID:      beadID,
		DependsOnID: dependsOnID,
		DepType:     depType,
		CreatedBy:   createdBy,
	})
	return nil
}

func TestExtractMentionInstruction(t *testing.T) {
	tests := []struct {
		name    string
		note    string
		trigger string
		want    string
	}{
		{
			name:    "simple mention",
			note:    "@gardenio fix the nil pointer in auth.go",
			trigger: "@gardenio",
			want:    "fix the nil pointer in auth.go",
		},
		{
			name:    "case insensitive",
			note:    "@Gardenio fix the nil pointer",
			trigger: "@gardenio",
			want:    "fix the nil pointer",
		},
		{
			name:    "mention in middle of text",
			note:    "Hey team, @gardenio please refactor this function",
			trigger: "@gardenio",
			want:    "please refactor this function",
		},
		{
			name:    "no mention",
			note:    "This is a normal review comment",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "mention without instruction",
			note:    "@gardenio",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "mention with only whitespace after",
			note:    "@gardenio   ",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "custom trigger",
			note:    "@mybot do the thing",
			trigger: "@mybot",
			want:    "do the thing",
		},
		{
			// Regression: after rotating the GitLab bot from "gardenio" (token
			// user, no real account) to "gardeniobot" (real user), users can
			// only @-mention the real account. The primary trigger "@gardenio"
			// must NOT greedily match inside "@gardeniobot" and must instead
			// fall through so the BotUsername trigger can handle it.
			name:    "trigger requires username boundary (gardenio vs gardeniobot)",
			note:    "@gardeniobot please review",
			trigger: "@gardenio",
			want:    "",
		},
		{
			// Regression: gd-bi3lO4oBIh — the previous strings.Index match
			// would yield "-bot please review" as the instruction; the
			// boundary fix forces a fall-through so the BotUsername trigger
			// (e.g. "@group_<id>_bot_<hash>") handles "@gardenio-bot" instead.
			name:    "trigger does not match inside @gardenio-bot",
			note:    "@gardenio-bot please review",
			trigger: "@gardenio",
			want:    "",
		},
		{
			name:    "trigger followed by punctuation is allowed",
			note:    "@gardenio, please review",
			trigger: "@gardenio",
			want:    ", please review",
		},
		{
			name:    "trigger followed by newline is allowed",
			note:    "@gardenio\nplease review",
			trigger: "@gardenio",
			want:    "please review",
		},
		{
			name:    "later matching trigger still detected past non-boundary prefix",
			note:    "no @gardeniobot but @gardenio here",
			trigger: "@gardenio",
			want:    "here",
		},
		{
			// Regression: ka-0000002216 — a rune that EXPANDS under Unicode
			// lowercasing (U+023A Ⱥ is 2 bytes, lowercase ⱥ is 3) before the
			// trigger made strings.ToLower offsets exceed len(note), so
			// note[end:] panicked with "slice bounds out of range". asciiToLower
			// is byte-length-preserving, so offsets stay valid. Attacker-
			// controlled comment text reaches this before the isAuthorized gate.
			name:    "non-ASCII expanding rune before trigger does not panic",
			note:    "Ⱥ@gardenio please review",
			trigger: "@gardenio",
			want:    "please review",
		},
		{
			// The trailing expanding rune ends up in the instruction; the point
			// is that slicing the original note by folded offsets is safe.
			name:    "non-ASCII expanding rune after trigger preserved",
			note:    "@gardenio Ⱥ review",
			trigger: "@gardenio",
			want:    "Ⱥ review",
		},
		{
			name:    "non-ASCII trailing rune with no instruction",
			note:    "@gardenioⱥ",
			trigger: "@gardenio",
			want:    "ⱥ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractMentionInstruction(tt.note, tt.trigger)
			if got != tt.want {
				t.Errorf("extractMentionInstruction(%q, %q) = %q, want %q", tt.note, tt.trigger, got, tt.want)
			}
		})
	}
}

func TestMentionAgentName(t *testing.T) {
	name := mentionAgentName(414)
	if len(name) == 0 {
		t.Fatal("expected non-empty name")
	}
	if name[:3] != "mr-" {
		t.Errorf("expected prefix 'mr-', got %q", name)
	}
	// Should contain the MR IID.
	if len(name) < 7 {
		t.Errorf("name too short: %q", name)
	}
	// Two calls should produce different names (random suffix).
	name2 := mentionAgentName(414)
	if name == name2 {
		t.Errorf("expected different names, got %q and %q", name, name2)
	}
}

// fakeUserLookup is a minimal GitLabUserLookup for tests. Maps exact
// GitLab usernames to a list of capabilities. Optionally, slackIDs and
// emails map a username to the Slack id / email carried on the resolved
// UserIdentity so operator-attribution tests (ka-0000004627) can exercise
// the slack:U… / oidc:<email> resolution ladder.
type fakeUserLookup struct {
	users    map[string][]string
	slackIDs map[string]string
	emails   map[string]string
}

func (f *fakeUserLookup) LookupByGitLabUsername(_ context.Context, username string) *UserIdentity {
	caps, ok := f.users[username]
	if !ok {
		return nil
	}
	return &UserIdentity{
		BeadID:         "gd-user-" + username,
		GitLabUsername: username,
		SlackID:        f.slackIDs[username],
		Email:          f.emails[username],
		Capabilities:   caps,
	}
}

// LookupByGitLabUserID finds nobody: the fake's users pin no gitlab_user_id,
// so they resolve by username alone (see lookupGitLabPerson).
func (f *fakeUserLookup) LookupByGitLabUserID(context.Context, string) *UserIdentity {
	return nil
}

// testUserLookup is the default lookup used by HandleMention tests that
// exercise the authorized path. Its members carry the "mention" capability
// so the gate admits them. Dedicated rejection tests construct their own.
func testUserLookup() *fakeUserLookup {
	return &fakeUserLookup{
		users: map[string][]string{
			"reviewer":     {"mention"},
			"matthewbaker": {"mention"},
		},
	}
}

func TestHandleMention_DetectsAndSpawns(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-test-task",
		spawnID:  "gd-test-agent",
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio fix the nil pointer in auth.go",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414", Title: "Fix auth"},
		DiscussionID: "disc-123",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// Check task bead was created.
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created, got %d", len(daemon.created))
	}
	req := daemon.created[0]
	if req.Type != "task" {
		t.Errorf("bead type = %q, want task", req.Type)
	}
	if req.CreatedBy != "gitlab-bridge" {
		t.Errorf("created_by = %q, want gitlab-bridge", req.CreatedBy)
	}
	// Check fields.
	var fields map[string]string
	if err := json.Unmarshal(req.Fields, &fields); err != nil {
		t.Fatalf("failed to unmarshal fields: %v", err)
	}
	if fields["mr_url"] != nc.MR.URL {
		t.Errorf("mr_url = %q, want %q", fields["mr_url"], nc.MR.URL)
	}
	if fields["spawn_source"] != "gitlab-mention" {
		t.Errorf("spawn_source = %q, want gitlab-mention", fields["spawn_source"])
	}
	if fields["mention_author"] != "reviewer" {
		t.Errorf("mention_author = %q, want reviewer", fields["mention_author"])
	}
	// Regression: gd-bi3lO4oBIh — fields stamped by spawnMentionAgent must
	// be registered on type:task in init.go, otherwise kbeads rejects the
	// CreateBead call with HTTP 400 and the mention silently fails. The
	// matching schema guard is TestTaskSchema_HasGitLabMentionFields.
	if fields["gitlab_discussion_id"] != "disc-123" {
		t.Errorf("gitlab_discussion_id = %q, want disc-123", fields["gitlab_discussion_id"])
	}

	// Check agent was spawned.
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	sc := daemon.spawned[0]
	if sc.Project != "gardenio" {
		t.Errorf("project = %q, want gardenio", sc.Project)
	}
	if sc.TaskID != "gd-test-task" {
		t.Errorf("task_id = %q, want gd-test-task", sc.TaskID)
	}
	if sc.Role != "crew" {
		t.Errorf("role = %q, want crew", sc.Role)
	}
}

// TestHandleMention_StampsOperatedBy pins the fallback GitLab → operated_by
// translation for an author whose user bead carries neither a Slack id nor an
// email: the operated_by field falls back to gitlab:<username> on both the
// task bead and the agent bead so the controller still injects a non-empty
// BEADS_OPERATED_BY (gd-IE5eI0uuVs). The resolved-human path is covered by
// TestHandleMention_AttributesResolvedHumanOperator (ka-0000004627).
func TestHandleMention_StampsOperatedBy(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-test-task",
		spawnID:  "gd-test-agent",
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio help with this",
		Author:       "matthewbaker",
		MR:           &webhookMR{IID: 999, URL: "https://gitlab.com/group/project/-/merge_requests/999", Title: "Test"},
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(daemon.created))
	}
	var taskFields map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &taskFields); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := taskFields["operated_by"], "gitlab:matthewbaker"; got != want {
		t.Errorf("task bead operated_by = %q, want %q", got, want)
	}
	if got, want := taskFields["gitlab_username"], "matthewbaker"; got != want {
		t.Errorf("task bead gitlab_username = %q, want %q", got, want)
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	extra := daemon.spawned[0].Extra
	if got, want := extra["operated_by"], "gitlab:matthewbaker"; got != want {
		t.Errorf("agent extra operated_by = %q, want %q", got, want)
	}
	if got, want := extra["gitlab_username"], "matthewbaker"; got != want {
		t.Errorf("agent extra gitlab_username = %q, want %q", got, want)
	}
}

// TestHandleMention_AttributesResolvedHumanOperator pins the ka-0000004627
// fix: when the mention author resolves to a known user bead, operated_by on
// both the task bead and the agent bead must be the human's email-bearing
// operator (slack:U… preferred, then oidc:<email>) — NOT gitlab:* and NOT a
// fleet sentinel. The audit operated_by column then resolves to the human
// because HandleMention stamps the same value on ctx via WithOperator. The
// raw gitlab username stays in gitlab_username for provenance, and the
// resolved slack_user_id / user_bead_id are mirrored for downstream lookups.
func TestHandleMention_AttributesResolvedHumanOperator(t *testing.T) {
	users := &fakeUserLookup{
		users:    map[string][]string{"matthewbaker": {"mention"}},
		slackIDs: map[string]string{"matthewbaker": "U0OPERATOR1"},
	}
	daemon := &mockMentionDaemon{createID: "gd-task", spawnID: "gd-agent"}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   users,
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio help with this",
		Author:       "matthewbaker",
		MR:           &webhookMR{IID: 700, URL: "https://gitlab.com/group/project/-/merge_requests/700", Title: "Test"},
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 task bead, got %d", len(daemon.created))
	}
	var tf map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &tf); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := tf["operated_by"], "slack:U0OPERATOR1"; got != want {
		t.Errorf("task operated_by = %q, want %q (human, not gitlab:*/fleet:*)", got, want)
	}
	if got, want := tf["slack_user_id"], "U0OPERATOR1"; got != want {
		t.Errorf("task slack_user_id = %q, want %q", got, want)
	}
	if got, want := tf["user_bead_id"], "gd-user-matthewbaker"; got != want {
		t.Errorf("task user_bead_id = %q, want %q", got, want)
	}
	if got, want := tf["gitlab_username"], "matthewbaker"; got != want {
		t.Errorf("task gitlab_username = %q, want %q (provenance preserved)", got, want)
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	extra := daemon.spawned[0].Extra
	if got, want := extra["operated_by"], "slack:U0OPERATOR1"; got != want {
		t.Errorf("agent operated_by = %q, want %q (human, not gitlab:*/fleet:*)", got, want)
	}
	if got, want := extra["slack_user_id"], "U0OPERATOR1"; got != want {
		t.Errorf("agent slack_user_id = %q, want %q", got, want)
	}
	if got, want := extra["user_bead_id"], "gd-user-matthewbaker"; got != want {
		t.Errorf("agent user_bead_id = %q, want %q", got, want)
	}
	if got, want := extra["gitlab_username"], "matthewbaker"; got != want {
		t.Errorf("agent gitlab_username = %q, want %q (provenance preserved)", got, want)
	}
}

// TestHandleMention_AttributesResolvedHumanOperatorViaEmail covers the second
// rung of the operator ladder: a resolved user with no Slack id but a valid
// email yields oidc:<email> (ka-0000004627).
func TestHandleMention_AttributesResolvedHumanOperatorViaEmail(t *testing.T) {
	users := &fakeUserLookup{
		users:  map[string][]string{"reviewer": {"mention"}},
		emails: map[string]string{"reviewer": "reviewer@acme.example"},
	}
	daemon := &mockMentionDaemon{createID: "gd-task", spawnID: "gd-agent"}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   users,
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio please review",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 701, URL: "https://gitlab.com/group/project/-/merge_requests/701"},
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}

	var tf map[string]string
	if err := json.Unmarshal(daemon.created[0].Fields, &tf); err != nil {
		t.Fatalf("unmarshal task fields: %v", err)
	}
	if got, want := tf["operated_by"], "oidc:reviewer@acme.example"; got != want {
		t.Errorf("task operated_by = %q, want %q", got, want)
	}
	// No Slack id resolved → slack_user_id must not be stamped.
	if got := tf["slack_user_id"]; got != "" {
		t.Errorf("task slack_user_id = %q, want empty (no Slack id resolved)", got)
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if got, want := daemon.spawned[0].Extra["operated_by"], "oidc:reviewer@acme.example"; got != want {
		t.Errorf("agent operated_by = %q, want %q", got, want)
	}
}

func TestHandleMention_BotUsernameTriggersSpawn(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-bot-task",
		spawnID:  "gd-bot-agent",
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:      daemon,
		Trigger:     "@gardenio",
		BotUsername: "group_14176856_bot_abc123",
		Project:     "gardenio",
		Users:       testUserLookup(),
		Logger:      slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@group_14176856_bot_abc123 please review this change",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 422, URL: "https://gitlab.com/group/project/-/merge_requests/422", Title: "Fix auth"},
		DiscussionID: "disc-bot-123",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected bot username mention to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if daemon.spawned[0].Project != "gardenio" {
		t.Errorf("project = %q, want gardenio", daemon.spawned[0].Project)
	}
}

func TestHandleMention_BotUsernameOverlappingFriendlyTrigger(t *testing.T) {
	// Regression: primary trigger "@gardenio" previously matched inside the
	// bot username "@gardeniobot", producing a mangled instruction ("pi ...").
	// With the boundary fix the primary trigger falls through and the
	// BotUsername trigger handles "@gardeniobot" cleanly.
	daemon := &mockMentionDaemon{
		createID: "gd-gpi-task",
		spawnID:  "gd-gpi-agent",
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:      daemon,
		Trigger:     "@gardenio",
		BotUsername: "gardeniobot",
		Project:     "gardenio",
		Users:       testUserLookup(),
		Logger:      slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardeniobot please review this change",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 500, URL: "https://gitlab.com/group/project/-/merge_requests/500", Title: "Feature"},
		DiscussionID: "disc-gpi",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected @gardeniobot mention to be handled")
	}
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	// The prompt must contain the full instruction, not "pi please review...".
	if !strings.Contains(daemon.spawned[0].Prompt, "please review this change") {
		t.Errorf("prompt missing full instruction, got:\n%s", daemon.spawned[0].Prompt)
	}
	if strings.Contains(daemon.spawned[0].Prompt, "Instruction: pi ") {
		t.Errorf("prompt has mangled instruction prefixed with 'pi ', got:\n%s", daemon.spawned[0].Prompt)
	}
}

func TestHandleMention_NoMentionReturnsFalse(t *testing.T) {
	daemon := &mockMentionDaemon{}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "This is a normal review comment",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414"},
	}

	handled := h.HandleMention(context.Background(), nc)
	if handled {
		t.Fatal("expected mention NOT to be handled for normal comment")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no beads created, got %d", len(daemon.created))
	}
}

func TestHandleMention_DedupSkipsDuplicate(t *testing.T) {
	// Existing in-progress task linked to the MR via the canonical MR bead +
	// produces dep edge (epic ka-0000001613). The task-bead `mr_url` field
	// is no longer scanned for dedup — readers walk via MRBeadByURL +
	// ProducingTaskBeadsForMR.
	existing := &beadsapi.BeadDetail{
		ID:     "gd-existing",
		Status: "in_progress",
		Fields: map[string]string{
			"gitlab_discussion_id": "disc-123",
			"spawn_source":         "gitlab-mention",
		},
	}
	daemon := &mockMentionDaemon{
		createID: "gd-test-task",
		spawnID:  "gd-test-agent",
		beads:    []*beadsapi.BeadDetail{existing},
		mrBeadsByURL: map[string]*beadsapi.BeadDetail{
			"https://gitlab.com/group/project/-/merge_requests/414": {
				ID:   "gd-mr-414",
				Type: "merge-request",
			},
		},
		tasksByMRBead: map[string][]*beadsapi.BeadDetail{
			"gd-mr-414": {existing},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio do the same thing again",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414"},
		DiscussionID: "disc-123",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled (dedup)")
	}
	// Should NOT create a new bead or spawn an agent.
	if len(daemon.created) != 0 {
		t.Errorf("expected no beads created (dedup), got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agents spawned (dedup), got %d", len(daemon.spawned))
	}
}

func TestHandleMention_ClosedBeadDoesNotDedup(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-test-task-2",
		spawnID:  "gd-test-agent-2",
		beads: []*beadsapi.BeadDetail{
			{
				ID:     "gd-old-closed",
				Status: "closed",
				Fields: map[string]string{
					"mr_url":               "https://gitlab.com/group/project/-/merge_requests/414",
					"gitlab_discussion_id": "disc-123",
					"spawn_source":         "gitlab-mention",
				},
			},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio try again please",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414"},
		DiscussionID: "disc-123",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}
	// Should spawn since old bead is closed.
	if len(daemon.created) != 1 {
		t.Errorf("expected 1 bead created, got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 1 {
		t.Errorf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
}

// TestHandleMention_StaleOrphanedTaskDoesNotDedup is the ka-0000001106
// regression: an OPEN gitlab-mention task with a matching discussion_id but
// assignee="" (its spawned agent crashed without closing the task) must NOT
// permanently mute the discussion. A re-mention arriving after the dedup
// window MUST still spawn (or nudge) rather than being silently dropped.
//
// Before the fix, hasPendingMention returned true for ANY open mention task
// on the discussion, so the orphaned task suppressed every subsequent
// @gardenio on that thread forever (site-deployment!1131, discussion
// 6dee4ed8…). This asserts the fresh-main fix: the stale open task is treated
// as NOT pending, so the mention falls through to spawnMentionAgent.
func TestHandleMention_StaleOrphanedTaskDoesNotDedup(t *testing.T) {
	mrURL := "https://gitlab.com/group/project/-/merge_requests/1131"
	// Orphaned: OPEN, assignee cleared (dead agent), created well outside the
	// dedup window so it must not suppress a fresh re-mention.
	orphan := &beadsapi.BeadDetail{
		ID:        "gd-orphan-open",
		Status:    "open",
		Assignee:  "", // dead agent — task never closed
		CreatedAt: time.Now().Add(-2 * time.Hour),
		Fields: map[string]string{
			"gitlab_discussion_id": "disc-6dee4ed8",
			"spawn_source":         "gitlab-mention",
		},
	}
	daemon := &mockMentionDaemon{
		createID: "gd-remention-task",
		spawnID:  "gd-remention-agent",
		beads:    []*beadsapi.BeadDetail{orphan},
		mrBeadsByURL: map[string]*beadsapi.BeadDetail{
			mrURL: {ID: "gd-mr-1131", Type: "merge-request"},
		},
		tasksByMRBead: map[string][]*beadsapi.BeadDetail{
			"gd-mr-1131": {orphan},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio you did not respond to my previous comment",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1131, URL: mrURL},
		DiscussionID: "disc-6dee4ed8",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}
	// MUST spawn — the orphaned open task must not mute the discussion.
	if len(daemon.created) != 1 {
		t.Errorf("expected 1 mention task created (orphan must not dedup), got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 1 {
		t.Errorf("expected 1 agent spawned (orphan must not dedup), got %d", len(daemon.spawned))
	}
}

// TestHandleMention_FreshOpenTaskDedups pins the burst-collapse behavior the
// dedup gate is FOR: a duplicate webhook delivery of the same comment, arriving
// while the just-spawned agent is still booting (open task created seconds
// ago), must still be suppressed so we don't spawn a second agent for one
// comment. This is the complement of the orphan regression above — the fix
// narrows suppression to a time window, it does not remove it.
func TestHandleMention_FreshOpenTaskDedups(t *testing.T) {
	mrURL := "https://gitlab.com/group/project/-/merge_requests/1131"
	fresh := &beadsapi.BeadDetail{
		ID:        "gd-fresh-open",
		Status:    "in_progress",
		Assignee:  "mr-1131-live",
		CreatedAt: time.Now().Add(-5 * time.Second), // just spawned, within window
		Fields: map[string]string{
			"gitlab_discussion_id": "disc-6dee4ed8",
			"spawn_source":         "gitlab-mention",
		},
	}
	daemon := &mockMentionDaemon{
		createID: "gd-dup-task",
		spawnID:  "gd-dup-agent",
		beads:    []*beadsapi.BeadDetail{fresh},
		mrBeadsByURL: map[string]*beadsapi.BeadDetail{
			mrURL: {ID: "gd-mr-1131", Type: "merge-request"},
		},
		tasksByMRBead: map[string][]*beadsapi.BeadDetail{
			"gd-mr-1131": {fresh},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio fix the flaky test",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1131, URL: mrURL},
		DiscussionID: "disc-6dee4ed8",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled (dedup)")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no beads created (fresh-task dedup), got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agents spawned (fresh-task dedup), got %d", len(daemon.spawned))
	}
}

// TestHandleMention_MixedFreshAndStaleDedups guards order-independence: a
// discussion carrying BOTH a stale orphan and a fresh (still-booting) task must
// dedup against the fresh one regardless of which the reverse-dep walk visits
// first. The stale orphan appears before the fresh task in the slice, so a
// naive per-bead early-return would wrongly spawn a duplicate.
func TestHandleMention_MixedFreshAndStaleDedups(t *testing.T) {
	mrURL := "https://gitlab.com/group/project/-/merge_requests/1131"
	staleFirst := &beadsapi.BeadDetail{
		ID:        "gd-stale-first",
		Status:    "open",
		Assignee:  "",
		CreatedAt: time.Now().Add(-3 * time.Hour),
		Fields: map[string]string{
			"gitlab_discussion_id": "disc-mixed",
			"spawn_source":         "gitlab-mention",
		},
	}
	freshSecond := &beadsapi.BeadDetail{
		ID:        "gd-fresh-second",
		Status:    "in_progress",
		Assignee:  "mr-1131-live",
		CreatedAt: time.Now().Add(-10 * time.Second),
		Fields: map[string]string{
			"gitlab_discussion_id": "disc-mixed",
			"spawn_source":         "gitlab-mention",
		},
	}
	daemon := &mockMentionDaemon{
		createID: "gd-mixed-task",
		spawnID:  "gd-mixed-agent",
		beads:    []*beadsapi.BeadDetail{staleFirst, freshSecond},
		mrBeadsByURL: map[string]*beadsapi.BeadDetail{
			mrURL: {ID: "gd-mr-1131", Type: "merge-request"},
		},
		tasksByMRBead: map[string][]*beadsapi.BeadDetail{
			"gd-mr-1131": {staleFirst, freshSecond}, // stale visited first
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio and one more thing",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1131, URL: mrURL},
		DiscussionID: "disc-mixed",
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no beads created (fresh task present ⇒ dedup), got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agents spawned (fresh task present ⇒ dedup), got %d", len(daemon.spawned))
	}
}

func TestHandleMention_ReusesExistingAgent(t *testing.T) {
	// Original task is closed but its assignee should still be reused.
	// Lookup walks task→MR bead via produces (epic ka-0000001613) — wire
	// both the MR bead and its reverse-dep so findExistingMRAgent finds it.
	original := &beadsapi.BeadDetail{
		ID:       "gd-original-task",
		Status:   "closed",
		Assignee: "fix-auth-abc",
		Fields:   map[string]string{},
	}
	daemon := &mockMentionDaemon{
		createID: "gd-new-task",
		spawnID:  "gd-new-agent",
		beads:    []*beadsapi.BeadDetail{original},
		mrBeadsByURL: map[string]*beadsapi.BeadDetail{
			"https://gitlab.com/group/project/-/merge_requests/414": {
				ID:   "gd-mr-414",
				Type: "merge-request",
			},
		},
		tasksByMRBead: map[string][]*beadsapi.BeadDetail{
			"gd-mr-414": {original},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio address the review feedback",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414", Title: "Fix auth"},
		DiscussionID: "disc-456",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// Should still create a new task bead for the mention.
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created, got %d", len(daemon.created))
	}

	// Should spawn using the EXISTING agent name, not a random mr-414-xxx.
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if daemon.spawned[0].AgentName != "fix-auth-abc" {
		t.Errorf("agent name = %q, want %q (reused from existing bead)", daemon.spawned[0].AgentName, "fix-auth-abc")
	}
}

func TestHandleMention_FallsBackToNewAgentWhenNoExisting(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-task-fresh",
		spawnID:  "gd-agent-fresh",
		beads:    []*beadsapi.BeadDetail{}, // no existing beads
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio do something new",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 500, URL: "https://gitlab.com/group/project/-/merge_requests/500", Title: "New MR"},
		DiscussionID: "disc-789",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	// Should generate a new mr-500-xxx name since no existing agent.
	if !strings.HasPrefix(daemon.spawned[0].AgentName, "mr-500-") {
		t.Errorf("expected agent name prefix 'mr-500-', got %q", daemon.spawned[0].AgentName)
	}
}

func TestHandleMention_ResolvesProjectFromMRURL(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-monorepo-task",
		spawnID:  "gd-monorepo-agent",
		projects: map[string]beadsapi.ProjectInfo{
			"gardenio": {
				Name:   "gardenio",
				GitURL: "https://github.com/joyousaversion/gardenio.git",
			},
			"monorepo": {
				Name:   "monorepo",
				GitURL: "https://gitlab.com/Acme/Platform/monorepo",
			},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio", // default project
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio fix the null check in NoteToFile",
		Author:       "matthewbaker",
		MR:           &webhookMR{IID: 422, URL: "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/422", Title: "Fix NTF role"},
		DiscussionID: "disc-monorepo",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// Task bead should be labeled with the monorepo project, not gardenio.
	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created, got %d", len(daemon.created))
	}
	req := daemon.created[0]
	foundMonorepoLabel := false
	for _, l := range req.Labels {
		if l == "project:monorepo" {
			foundMonorepoLabel = true
		}
		if l == "project:gardenio" {
			t.Error("task bead should NOT have project:gardenio label when MR URL matches monorepo")
		}
	}
	if !foundMonorepoLabel {
		t.Errorf("task bead should have project:monorepo label, got labels: %v", req.Labels)
	}

	// Agent should be spawned with the monorepo project.
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if daemon.spawned[0].Project != "monorepo" {
		t.Errorf("project = %q, want monorepo", daemon.spawned[0].Project)
	}
}

// TestHandleMention_ResolvesProjectFromMRURLViaRepos verifies that MRs in
// repos listed under a project bead's repos[] field also route to that
// project. The monorepo project bead lists site-deployment, gardenio, and
// k6-tests as additional repos; an MR in any of those should be handled by
// the monorepo project, not fall back to the default.
func TestHandleMention_ResolvesProjectFromMRURLViaRepos(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-monorepo-task",
		spawnID:  "gd-monorepo-agent",
		projects: map[string]beadsapi.ProjectInfo{
			"monorepo": {
				Name:   "monorepo",
				GitURL: "https://gitlab.com/Acme/Platform/monorepo",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/Acme/Platform/monorepo.git", Name: "monorepo"},
					{URL: "https://gitlab.com/Acme/Platform/site-deployment.git", Name: "site-deployment"},
					{URL: "https://gitlab.com/Acme/Platform/gardenio.git", Name: "gardenio"},
					{URL: "https://gitlab.com/terryvance1/k6-tests.git", Name: "k6-tests"},
				},
			},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio", // default fallback should NOT win
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio rebase and merge",
		Author:       "matthewbaker",
		MR:           &webhookMR{IID: 898, URL: "https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/898", Title: "Site-deployment MR"},
		DiscussionID: "disc-sd",
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}

	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if daemon.spawned[0].Project != "monorepo" {
		t.Errorf("project = %q, want monorepo (matched via repos[])", daemon.spawned[0].Project)
	}

	if len(daemon.created) != 1 {
		t.Fatalf("expected 1 bead created, got %d", len(daemon.created))
	}
	for _, l := range daemon.created[0].Labels {
		if l == "project:gardenio" {
			t.Error("task bead should NOT have project:gardenio label when MR is in a monorepo repos[] entry")
		}
	}
}

// TestProjectFromMRURL_DeterministicUnderGitURLCollision verifies that when
// two project beads share the same git_url, resolution always yields the SAME
// winner regardless of the (nondeterministic) map iteration order. The winner
// is the project with the lexicographically smallest BeadID. This is the
// durable fix for the MR-mention split-routing bug (ka-0000002164): before it,
// each pass returned the first map match and identical MRs routed to whichever
// project the map happened to yield first.
func TestProjectFromMRURL_DeterministicUnderGitURLCollision(t *testing.T) {
	// Two projects claim the exact same git_url. pickGitURLWinner must pick
	// the lowest BeadID no matter what order the slice is built in.
	high := beadsapi.ProjectInfo{Name: "zeta", BeadID: "gd-zzz999", GitURL: "https://gitlab.com/org/shared"}
	low := beadsapi.ProjectInfo{Name: "alpha", BeadID: "gd-aaa111", GitURL: "https://gitlab.com/org/shared"}

	// Both orderings must yield the same (lowest-BeadID) winner.
	if got := pickGitURLWinner([]beadsapi.ProjectInfo{high, low}); got.BeadID != low.BeadID {
		t.Errorf("winner (high,low) = %q, want %q", got.BeadID, low.BeadID)
	}
	if got := pickGitURLWinner([]beadsapi.ProjectInfo{low, high}); got.BeadID != low.BeadID {
		t.Errorf("winner (low,high) = %q, want %q", got.BeadID, low.BeadID)
	}

	// End-to-end: run resolution many times; a nondeterministic map iteration
	// would eventually flip the winner if the code returned the first match.
	daemon := &mockMentionDaemon{
		createID: "gd-t",
		spawnID:  "gd-a",
		projects: map[string]beadsapi.ProjectInfo{
			"zeta":  high,
			"alpha": low,
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})
	for i := range 50 {
		got := h.projectFromMRURL(context.Background(), "https://gitlab.com/org/shared/-/merge_requests/1")
		if got != "alpha" {
			t.Fatalf("iteration %d: project = %q, want alpha (lowest BeadID winner)", i, got)
		}
	}
}

// TestProjectFromMRURL_ReposPrefersPrimaryOverReference verifies that when two
// projects list the same repo in repos[], the project whose entry has
// Role=="primary" (it owns the repo) wins over one with Role=="reference".
func TestProjectFromMRURL_ReposPrefersPrimaryOverReference(t *testing.T) {
	owner := reposMatch{
		Info: beadsapi.ProjectInfo{Name: "owner", BeadID: "gd-zzz"}, // high BeadID, but primary
		Role: "primary",
	}
	ref := reposMatch{
		Info: beadsapi.ProjectInfo{Name: "ref", BeadID: "gd-aaa"}, // low BeadID, but reference
		Role: "reference",
	}
	if got := pickReposWinner([]reposMatch{ref, owner}); got.Name != "owner" {
		t.Errorf("winner = %q, want owner (primary role beats reference despite higher BeadID)", got.Name)
	}
	if got := pickReposWinner([]reposMatch{owner, ref}); got.Name != "owner" {
		t.Errorf("winner (reordered) = %q, want owner", got.Name)
	}
}

// TestProjectFromMRURL_ReposPrefersOwnerOverSatellite verifies that among
// repos[] matches with equal role, a project that owns some repo (non-empty
// git_url — e.g. monorepo) outranks a pure-reference satellite (empty git_url
// — e.g. devsecops) that only lists the repo as a reference. This is the exact
// shape of the split-routing bug: both projects listed site-deployment as a
// reference, and it must resolve to the owner deterministically.
func TestProjectFromMRURL_ReposPrefersOwnerOverSatellite(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-t",
		spawnID:  "gd-a",
		projects: map[string]beadsapi.ProjectInfo{
			// Satellite: empty git_url, low BeadID, holds only a reference.
			"devsecops": {
				Name:   "devsecops",
				BeadID: "gd-aaa",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/org/site-deployment.git", Role: "reference"},
				},
			},
			// Owner: non-empty git_url, higher BeadID, also references the repo.
			"monorepo": {
				Name:   "monorepo",
				BeadID: "gd-zzz",
				GitURL: "https://gitlab.com/org/monorepo",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/org/site-deployment.git", Role: "reference"},
				},
			},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})
	for i := range 20 {
		got := h.projectFromMRURL(context.Background(), "https://gitlab.com/org/site-deployment/-/merge_requests/9")
		if got != "monorepo" {
			t.Fatalf("iteration %d: project = %q, want monorepo (owner beats satellite)", i, got)
		}
	}
}

// TestProjectFromMRURL_ReposStableTieBreakByBeadID verifies that when two
// repos[] matches are otherwise indistinguishable (same role, both owners or
// both satellites), the tie-break is the lexicographically smallest BeadID.
func TestProjectFromMRURL_ReposStableTieBreakByBeadID(t *testing.T) {
	a := reposMatch{Info: beadsapi.ProjectInfo{Name: "a", BeadID: "gd-111", GitURL: "https://gitlab.com/org/a"}, Role: "reference"}
	b := reposMatch{Info: beadsapi.ProjectInfo{Name: "b", BeadID: "gd-222", GitURL: "https://gitlab.com/org/b"}, Role: "reference"}
	if got := pickReposWinner([]reposMatch{b, a}); got.BeadID != "gd-111" {
		t.Errorf("winner (b,a) = %q, want gd-111 (lowest BeadID)", got.BeadID)
	}
	if got := pickReposWinner([]reposMatch{a, b}); got.BeadID != "gd-111" {
		t.Errorf("winner (a,b) = %q, want gd-111 (lowest BeadID)", got.BeadID)
	}
}

// TestProjectFromMRURL_GitURLBeatsReposReference verifies the durable-fix
// invariant that the first pass (git_url ownership) still beats the second
// pass (repos[] reference): a project whose top-level git_url matches wins over
// a different project that only lists the repo as a repos[] reference.
func TestProjectFromMRURL_GitURLBeatsReposReference(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-t",
		spawnID:  "gd-a",
		projects: map[string]beadsapi.ProjectInfo{
			"owner": {
				Name:   "owner",
				BeadID: "gd-zzz", // higher BeadID — must still win via git_url pass
				GitURL: "https://gitlab.com/org/shared",
			},
			"referencer": {
				Name:   "referencer",
				BeadID: "gd-aaa",
				Repos: []beadsapi.RepoEntry{
					{URL: "https://gitlab.com/org/shared", Role: "reference"},
				},
			},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})
	got := h.projectFromMRURL(context.Background(), "https://gitlab.com/org/shared/-/merge_requests/3")
	if got != "owner" {
		t.Errorf("project = %q, want owner (git_url pass beats repos[] reference pass)", got)
	}
}

func TestHandleMention_FallsBackToDefaultProjectWhenNoMatch(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-unknown-task",
		spawnID:  "gd-unknown-agent",
		projects: map[string]beadsapi.ProjectInfo{
			"gardenio": {
				Name:   "gardenio",
				GitURL: "https://github.com/joyousaversion/gardenio.git",
			},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio fix something",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 100, URL: "https://gitlab.com/some/unknown/repo/-/merge_requests/100", Title: "Unknown repo"},
		DiscussionID: "disc-unknown",
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// Should fall back to default project "gardenio".
	if len(daemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(daemon.spawned))
	}
	if daemon.spawned[0].Project != "gardenio" {
		t.Errorf("project = %q, want gardenio (default fallback)", daemon.spawned[0].Project)
	}
}

func TestMatchGitURL(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want bool
	}{
		{"exact match", "https://gitlab.com/org/repo", "https://gitlab.com/org/repo", true},
		{"trailing slash", "https://gitlab.com/org/repo/", "https://gitlab.com/org/repo", true},
		{".git suffix", "https://gitlab.com/org/repo.git", "https://gitlab.com/org/repo", true},
		{"case insensitive", "https://GitLab.com/Org/Repo", "https://gitlab.com/org/repo", true},
		{"different paths", "https://gitlab.com/org/repo-a", "https://gitlab.com/org/repo-b", false},
		{"different hosts", "https://github.com/org/repo", "https://gitlab.com/org/repo", false},
		{"nested path", "https://gitlab.com/Acme/Platform/monorepo", "https://gitlab.com/Acme/Platform/monorepo", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchGitURL(tt.a, tt.b); got != tt.want {
				t.Errorf("matchGitURL(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestTruncateStr(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"this is a longer string", 10, "this is..."},
		{"hello", 2, "he"},
	}
	for _, tt := range tests {
		got := truncateStr(tt.input, tt.maxLen)
		if got != tt.want {
			t.Errorf("truncateStr(%q, %d) = %q, want %q", tt.input, tt.maxLen, got, tt.want)
		}
	}
}

func TestHandleMention_NudgesExistingAgent(t *testing.T) {
	// Set up a coop server that accepts nudges.
	var nudgeReceived string
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/nudge" {
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			nudgeReceived = body["message"]
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"delivered": true})
			return
		}
		http.NotFound(w, r)
	}))
	defer coopServer.Close()

	mrURL := "https://gitlab.com/test/repo/-/merge_requests/42"

	// Set up the resolver daemon with an active agent bound to this MR.
	resolverDaemon := newMockResolverDaemon()
	resolverDaemon.agentBeads["mr-42-abc"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-mr42",
		Title: "mr-42-abc",
		Type:  "agent",
		Fields: map[string]string{
			"agent":         "mr-42-abc",
			"gitlab_mr_url": mrURL,
			"coop_url":      coopServer.URL,
		},
	}

	resolver := NewAgentResolver(AgentResolverConfig{
		Daemon: resolverDaemon,
		Nudge:  subscriberNudge(resolverDaemon.FindAgentBead),
		Logger: slog.Default(),
	})

	mentionDaemon := &mockMentionDaemon{
		createID: "gd-nudge-task",
		spawnID:  "gd-nudge-agent",
	}
	handler := NewMentionHandler(MentionHandlerConfig{
		Daemon:   mentionDaemon,
		Resolver: resolver,
		Nudge:    resolver.deliverNudge,
		Trigger:  "@gardenio",
		Project:  "test-project",
		Users:    testUserLookup(),
		Logger:   slog.Default(),
	})

	nc := noteContext{
		Note:   "@gardenio fix the flaky test",
		Author: "reviewer",
		MR:     &webhookMR{IID: 42, URL: mrURL},
	}

	handled := handler.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// The mention should have nudged the existing agent, not spawned a new one.
	if nudgeReceived == "" {
		t.Fatal("expected nudge to be sent to existing agent")
	}
	if len(mentionDaemon.spawned) != 0 {
		t.Errorf("expected no new agents spawned, got %d", len(mentionDaemon.spawned))
	}
}

func TestHandleMention_SpawnsWhenNoActiveAgent(t *testing.T) {
	mrURL := "https://gitlab.com/test/repo/-/merge_requests/43"

	// Resolver with no active agents.
	resolverDaemon := newMockResolverDaemon()
	resolver := NewAgentResolver(AgentResolverConfig{
		Daemon: resolverDaemon,
		Nudge:  subscriberNudge(resolverDaemon.FindAgentBead),
		Logger: slog.Default(),
	})

	mentionDaemon := &mockMentionDaemon{
		createID: "gd-spawn-task",
		spawnID:  "gd-spawn-agent",
	}
	handler := NewMentionHandler(MentionHandlerConfig{
		Daemon:   mentionDaemon,
		Resolver: resolver,
		Nudge:    resolver.deliverNudge,
		Trigger:  "@gardenio",
		Project:  "test-project",
		Users:    testUserLookup(),
		Logger:   slog.Default(),
	})

	nc := noteContext{
		Note:   "@gardenio add error handling",
		Author: "reviewer",
		MR:     &webhookMR{IID: 43, URL: mrURL},
	}

	handled := handler.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// No active agent → should spawn a new one.
	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned, got %d", len(mentionDaemon.spawned))
	}
}

func TestHandleMention_FallsBackToSpawnOnNudgeFailure(t *testing.T) {
	// Coop server that rejects nudges.
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer coopServer.Close()

	mrURL := "https://gitlab.com/test/repo/-/merge_requests/44"

	resolverDaemon := newMockResolverDaemon()
	resolverDaemon.agentBeads["mr-44-abc"] = &beadsapi.BeadDetail{
		ID:    "bd-agent-mr44",
		Title: "mr-44-abc",
		Type:  "agent",
		Fields: map[string]string{
			"agent":         "mr-44-abc",
			"gitlab_mr_url": mrURL,
			"coop_url":      coopServer.URL,
		},
	}

	resolver := NewAgentResolver(AgentResolverConfig{
		Daemon: resolverDaemon,
		Nudge:  subscriberNudge(resolverDaemon.FindAgentBead),
		Logger: slog.Default(),
	})

	mentionDaemon := &mockMentionDaemon{
		createID: "gd-fallback-task",
		spawnID:  "gd-fallback-agent",
	}
	handler := NewMentionHandler(MentionHandlerConfig{
		Daemon:   mentionDaemon,
		Resolver: resolver,
		Nudge:    resolver.deliverNudge,
		Trigger:  "@gardenio",
		Project:  "test-project",
		Users:    testUserLookup(),
		Logger:   slog.Default(),
	})

	nc := noteContext{
		Note:   "@gardenio fix the bug",
		Author: "reviewer",
		MR:     &webhookMR{IID: 44, URL: mrURL},
	}

	handled := handler.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected mention to be handled")
	}

	// Nudge failed → should fall back to spawning a new agent.
	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned after nudge failure, got %d", len(mentionDaemon.spawned))
	}
}

func TestBuildMentionPrompt(t *testing.T) {
	nc := noteContext{
		Author: "reviewer",
		MR:     &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414"},
		Position: &notePosition{
			NewPath: "auth.go",
			NewLine: 42,
		},
	}

	prompt := buildMentionPrompt(nc, "fix the nil pointer", "gd-task-1", "feature-branch")

	// Check key pieces are in the prompt.
	checks := []string{
		"MR !414",
		"@reviewer",
		"auth.go:42",
		"fix the nil pointer",
		"feature-branch",
		"gd-task-1",
	}
	for _, c := range checks {
		if !strings.Contains(prompt, c) {
			t.Errorf("prompt missing %q:\n%s", c, prompt)
		}
	}
}

// TestHandleMention_FailsClosedWithoutUserDirectory verifies that when no
// UserDirectory is wired on the handler, every mention is rejected. Guards
// against a misconfigured bridge silently accepting every comment.
func TestHandleMention_FailsClosedWithoutUserDirectory(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-should-not-be-created",
		spawnID:  "gd-should-not-be-spawned",
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		// Users intentionally unset.
		Logger: slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio do the thing",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 414, URL: "https://gitlab.com/group/project/-/merge_requests/414"},
		DiscussionID: "disc-1",
	}

	handled := h.HandleMention(context.Background(), nc)
	// Returning true prevents fall-through to other handlers — the
	// mention was recognized and intentionally declined.
	if !handled {
		t.Fatal("expected handled=true so the webhook does not fall through to other processing")
	}
	if len(daemon.created) != 0 {
		t.Errorf("expected no task bead created, got %d", len(daemon.created))
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agent spawned, got %d", len(daemon.spawned))
	}
}

// TestHandleMention_RejectsUnknownAuthor verifies that a GitLab commenter
// without a matching user bead (type=user, kind=config with
// gitlab_username) is rejected. An outside collaborator commenting on a
// watched MR must NOT spawn an agent.
func TestHandleMention_RejectsUnknownAuthor(t *testing.T) {
	daemon := &mockMentionDaemon{createID: "gd-x", spawnID: "gd-y"}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(), // contains reviewer, matthewbaker
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio do the thing",
		Author:       "mallory", // no user bead
		MR:           &webhookMR{IID: 99, URL: "https://gitlab.com/group/project/-/merge_requests/99"},
	}

	handled := h.HandleMention(context.Background(), nc)
	if !handled {
		t.Fatal("expected handled=true (mention recognized, rejected)")
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agent spawned for unknown author, got %d", len(daemon.spawned))
	}
}

// TestHandleMention_RejectsUserLackingMentionCapability verifies that a
// known user whose bead does not carry the "mention" capability is
// rejected. A user can exist in the directory for other reasons (DM
// delivery, activity attribution) without being authorized to spawn agents.
func TestHandleMention_RejectsUserLackingMentionCapability(t *testing.T) {
	daemon := &mockMentionDaemon{createID: "gd-x", spawnID: "gd-y"}
	users := &fakeUserLookup{
		users: map[string][]string{
			"reviewer": {"approve:escalation"}, // no "mention"
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   users,
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio do the thing",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1, URL: "https://gitlab.com/group/project/-/merge_requests/1"},
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected handled=true (mention recognized, rejected)")
	}
	if len(daemon.spawned) != 0 {
		t.Errorf("expected no agent spawned for user without mention capability, got %d", len(daemon.spawned))
	}
}

// TestHandleMention_MentionTaskTriggeredByMRBead pins the M2b auto-link
// (epic ka-0000000933): when the MR bead already exists at mention time, the
// freshly-created mention task carries a `triggered-by` dep edge → MR bead so
// reverse-graph queries can join the work bead back to its upstream MR. See
// unit plan ka-0000004136 test 5.1.
func TestHandleMention_MentionTaskTriggeredByMRBead(t *testing.T) {
	const mrURL = "https://gitlab.com/group/project/-/merge_requests/777"
	const mrBeadID = "gd-mr-bead"
	daemon := &mockMentionDaemon{
		createID: "gd-mention-task",
		spawnID:  "gd-mention-agent",
		mrBeadsByURL: map[string]*beadsapi.BeadDetail{
			mrURL: {ID: mrBeadID},
		},
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio please fix",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 777, URL: mrURL, Title: "Test MR"},
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}

	var triggered *depCall
	for i := range daemon.depCalls {
		c := daemon.depCalls[i]
		if c.DepType == "triggered-by" && c.BeadID == "gd-mention-task" {
			triggered = &c
			break
		}
	}
	if triggered == nil {
		t.Fatalf("expected a triggered-by dep on the mention task bead, got %+v", daemon.depCalls)
	}
	if triggered.DependsOnID != mrBeadID {
		t.Errorf("triggered-by depends_on = %q, want %q", triggered.DependsOnID, mrBeadID)
	}
	if triggered.CreatedBy != "gitlab-bridge" {
		t.Errorf("triggered-by created_by = %q, want gitlab-bridge", triggered.CreatedBy)
	}
}

// TestHandleMention_FreshMRDeferred guards the fresh-MR race: when the MR
// bead has not yet been upserted at mention time, the mention task is still
// created but no triggered-by dep is written. The MR-sync webhook path
// backfills the edge on first MR upsert. M2b on epic ka-0000000933.
func TestHandleMention_FreshMRDeferred(t *testing.T) {
	daemon := &mockMentionDaemon{
		createID: "gd-mention-task",
		spawnID:  "gd-mention-agent",
		// mrBeadsByURL is empty — MRBeadByURL returns nil.
	}
	h := NewMentionHandler(MentionHandlerConfig{
		Daemon:  daemon,
		Trigger: "@gardenio",
		Project: "gardenio",
		Users:   testUserLookup(),
		Logger:  slog.Default(),
	})

	nc := noteContext{
		NoteableType: "MergeRequest",
		Note:         "@gardenio help",
		Author:       "reviewer",
		MR:           &webhookMR{IID: 1, URL: "https://gitlab.com/group/project/-/merge_requests/1"},
	}

	if !h.HandleMention(context.Background(), nc) {
		t.Fatal("expected mention to be handled")
	}
	if len(daemon.created) != 1 {
		t.Fatalf("expected mention task to be created even on fresh-MR race, got %d", len(daemon.created))
	}
	for _, c := range daemon.depCalls {
		if c.DepType == "triggered-by" {
			t.Errorf("expected no triggered-by dep on fresh-MR race, got %+v", c)
		}
	}
}
