package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

func TestThreadAgentsPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Create state, add thread agent, close.
	state1, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := state1.SetThreadAgent("C-test", "1111.2222", "gardenio/crew/hq"); err != nil {
		t.Fatal(err)
	}

	// Reload from disk.
	state2, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}

	agent, ok := state2.GetThreadAgent("C-test", "1111.2222")
	if !ok || agent != "gardenio/crew/hq" {
		t.Errorf("after reload: got (%q, %v), want (%q, true)", agent, ok, "gardenio/crew/hq")
	}

	// Remove and verify.
	if err := state2.RemoveThreadAgent("C-test", "1111.2222"); err != nil {
		t.Fatal(err)
	}
	if _, ok := state2.GetThreadAgent("C-test", "1111.2222"); ok {
		t.Error("expected thread agent to be removed")
	}
}

func TestRemoveThreadAgentByAgent(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	_ = state.SetThreadAgent("C-a", "1.1", "gardenio/crew/hq")
	_ = state.SetThreadAgent("C-b", "2.2", "gardenio/crew/hq")
	_ = state.SetThreadAgent("C-c", "3.3", "gardenio/crew/k8s")

	if err := state.RemoveThreadAgentByAgent("gardenio/crew/hq"); err != nil {
		t.Fatal(err)
	}

	// hq entries should be gone.
	if _, ok := state.GetThreadAgent("C-a", "1.1"); ok {
		t.Error("expected C-a:1.1 to be removed")
	}
	if _, ok := state.GetThreadAgent("C-b", "2.2"); ok {
		t.Error("expected C-b:2.2 to be removed")
	}
	// k8s entry should remain.
	if agent, ok := state.GetThreadAgent("C-c", "3.3"); !ok || agent != "gardenio/crew/k8s" {
		t.Errorf("expected C-c:3.3 to remain, got (%q, %v)", agent, ok)
	}
}

func TestRemoveThreadAgentByAgent_CleansListenFlags(t *testing.T) {
	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	// Set up threads with listen mode.
	_ = state.SetThreadAgent("C-a", "1.1", "agent-a")
	_ = state.SetListenThread("C-a", "1.1")
	_ = state.SetThreadAgent("C-b", "2.2", "agent-b")
	_ = state.SetListenThread("C-b", "2.2")

	// Remove agent-a — should also clean up its listen flag.
	if err := state.RemoveThreadAgentByAgent("agent-a"); err != nil {
		t.Fatal(err)
	}

	// agent-a's listen flag should be gone.
	if state.IsListenThread("C-a", "1.1") {
		t.Error("expected listen flag for C-a:1.1 to be removed")
	}
	// agent-b's listen flag should remain.
	if !state.IsListenThread("C-b", "2.2") {
		t.Error("expected listen flag for C-b:2.2 to remain")
	}
}

func TestHandleThreadSpawn_CreatesBeadAndState(t *testing.T) {
	daemon := newMockDaemon()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		botUserID:  "U-BOT",
		agentCards: map[string]MessageRef{},
	}

	channel := "C-thread-test"
	threadTS := "1111.2222"

	// Verify no agent bound to this thread initially.
	if agent := b.getAgentByThread(channel, threadTS); agent != "" {
		t.Fatalf("expected no agent for thread, got %q", agent)
	}

	agentName := "thread-" + sanitizeTS(threadTS)
	if agentName != "thread-1111-2222" {
		t.Fatalf("expected agent name thread-1111-2222, got %q", agentName)
	}

	ctx := context.Background()
	beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "agent",
		Description: "Thread-spawned agent for test",
		Labels:      []string{"slack-thread"},
	})
	if err != nil {
		t.Fatalf("CreateBead failed: %v", err)
	}

	// Record thread→agent mapping.
	if err := state.SetThreadAgent(channel, threadTS, agentName); err != nil {
		t.Fatalf("SetThreadAgent failed: %v", err)
	}

	// Verify bead was created.
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil {
		t.Fatalf("GetBead failed: %v", err)
	}
	if bead.Type != "agent" {
		t.Errorf("bead type = %q, want agent", bead.Type)
	}
	if bead.Title != agentName {
		t.Errorf("bead title = %q, want %q", bead.Title, agentName)
	}
	if !hasLabel(bead.Labels, "slack-thread") {
		t.Errorf("bead labels = %v, want slack-thread", bead.Labels)
	}

	// Verify thread→agent state.
	agent, ok := state.GetThreadAgent(channel, threadTS)
	if !ok || agent != agentName {
		t.Errorf("thread agent = (%q, %v), want (%q, true)", agent, ok, agentName)
	}

	// Verify getAgentByThread now resolves.
	if got := b.getAgentByThread(channel, threadTS); got != agentName {
		t.Errorf("getAgentByThread = %q, want %q", got, agentName)
	}
}

func TestResolveAgentThread_ThreadBound(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-thread-agent",
		Title: "thread-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":                "thread-agent",
			"slack_thread_channel": "C-thread",
			"slack_thread_ts":      "1234.5678",
			"spawn_source":         "slack-thread",
		},
	}

	b := &Bot{
		daemon: daemon,
		logger: slog.Default(),
	}

	channel, ts := b.resolveAgentThread(context.Background(), "thread-agent")
	if channel != "C-thread" {
		t.Errorf("channel = %q, want C-thread", channel)
	}
	if ts != "1234.5678" {
		t.Errorf("ts = %q, want 1234.5678", ts)
	}
}

func TestResolveAgentThread_RegularAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["regular-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-regular-agent",
		Title: "regular-agent",
		Type:  "agent",
		Fields: map[string]string{
			"agent":   "regular-agent",
			"project": "gardenio",
		},
	}

	b := &Bot{
		daemon: daemon,
		logger: slog.Default(),
	}

	channel, ts := b.resolveAgentThread(context.Background(), "regular-agent")
	if channel != "" {
		t.Errorf("expected empty channel for regular agent, got %q", channel)
	}
	if ts != "" {
		t.Errorf("expected empty ts for regular agent, got %q", ts)
	}
}

func TestResolveAgentThread_NotFound(t *testing.T) {
	daemon := newMockDaemon()

	b := &Bot{
		daemon: daemon,
		logger: slog.Default(),
	}

	channel, ts := b.resolveAgentThread(context.Background(), "nonexistent")
	if channel != "" || ts != "" {
		t.Errorf("expected empty for nonexistent agent, got channel=%q ts=%q", channel, ts)
	}
}

func TestRespawnThreadAgent_CreatesSameNameBead(t *testing.T) {
	daemon := newMockDaemon()
	// Add a project bead so the channel maps to a project.
	daemon.beads["proj-test"] = &beadsapi.BeadDetail{
		ID: "proj-test", Title: "testproject", Type: "project",
		Fields: map[string]string{"slack_channel": "C-thread-test"},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	channel := "C-thread-test"
	threadTS := "1111.2222"
	agentName := "thread-1111-2222"

	// Pre-populate thread→agent mapping.
	_ = state.SetThreadAgent(channel, threadTS, agentName)

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	b.respawnThreadAgent(context.Background(), channel, threadTS, agentName, "wake up please", "U-SPAWNER")

	// Thread→agent mapping should still exist.
	agent, ok := state.GetThreadAgent(channel, threadTS)
	if !ok {
		t.Fatal("expected thread agent mapping to be preserved after respawn")
	}
	if agent != agentName {
		t.Errorf("expected agent name %q preserved, got %q", agentName, agent)
	}

	// Should have created an agent bead with the SAME name.
	var found *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == agentName {
			found = bead
			break
		}
	}
	if found == nil {
		t.Fatal("expected agent bead to be created with same name")
	}

	// Verify fields.
	if found.Fields["agent"] != agentName {
		t.Errorf("agent field = %q, want %q", found.Fields["agent"], agentName)
	}
	if found.Fields["slack_thread_channel"] != channel {
		t.Errorf("slack_thread_channel = %q, want %q", found.Fields["slack_thread_channel"], channel)
	}
	if found.Fields["slack_thread_ts"] != threadTS {
		t.Errorf("slack_thread_ts = %q, want %q", found.Fields["slack_thread_ts"], threadTS)
	}
	if found.Fields["spawn_source"] != "slack-thread-resume" {
		t.Errorf("spawn_source = %q, want slack-thread-resume", found.Fields["spawn_source"])
	}
	// Regression: respawned thread agents must use crew mode so they get a PVC
	// and BOAT_SESSION_RESUME=1, enabling coop --resume. Job mode uses EmptyDir
	// which loses conversation history when the pod dies (PR #487).
	if found.Fields["mode"] != "crew" {
		t.Errorf("mode = %q, want crew (required for session resume)", found.Fields["mode"])
	}
	if !hasLabel(found.Labels, "slack-thread") {
		t.Errorf("expected slack-thread label, got %v", found.Labels)
	}
}

func TestRespawnThreadAgent_RejectsNoProject(t *testing.T) {
	daemon := newMockDaemon()
	// No project bead → no channel mapping → should reject.

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-unmapped", "1111.2222", "thread-1111-2222")

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	b.respawnThreadAgent(context.Background(), "C-unmapped", "1111.2222", "thread-1111-2222", "wake up", "")

	// Should NOT have created any agent bead.
	for _, bead := range daemon.beads {
		if bead.Type == "agent" {
			t.Errorf("expected no agent bead, got %q", bead.Title)
		}
	}
}

// TestRespawnThreadAgent_InheritsProjectFromClosedBead verifies that resume
// uses the project from a previous (now-closed) agent bead with the same
// agent name even when the channel has no slack_channel mapping. This is
// the bug from gd-qFgPU9zGhT: a thread spawned via DM/AssistantThread/--project
// fallback would lose its project on the second mention because
// projectFromChannel returned "" and no other path consulted the prior bead.
func TestRespawnThreadAgent_InheritsProjectFromClosedBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio") // project exists, but channel has no mapping

	// Seed a closed agent bead from the original (first) spawn that picked
	// project=gardenio via some other resolution path (--project flag, DM
	// fallback, AssistantThread context, etc.).
	priorID := "bd-prior-agent"
	daemon.beads[priorID] = &beadsapi.BeadDetail{
		ID:        priorID,
		Title:     "thread-2222-3333",
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now().Add(-1 * time.Hour),
		Fields: map[string]string{
			"agent":   "thread-2222-3333",
			"project": "gardenio",
			"mode":    "crew",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	// Channel "C-unmapped" has no project bead with slack_channel — the
	// channel-based resolution that was previously the only source for
	// respawn would yield "" and produce the user-visible error.
	b.respawnThreadAgent(context.Background(), "C-unmapped", "2222.3333", "thread-2222-3333", "second mention", "U-USER")

	var found *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == "thread-2222-3333" && bead.Status != "closed" {
			found = bead
			break
		}
	}
	if found == nil {
		t.Fatal("expected respawn to create a new agent bead inheriting project from prior closed bead")
	}
	if found.Fields["project"] != "gardenio" {
		t.Errorf("project = %q, want gardenio (inherited from prior closed bead)", found.Fields["project"])
	}
}

func TestRespawnThreadAgent_InfersProjectFromChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-gardenio")

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	b.respawnThreadAgent(context.Background(), "C-gardenio", "2222.3333", "my-agent", "do the thing", "U-ORIGINAL")

	// Find the created agent bead.
	var found *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == "my-agent" {
			found = bead
			break
		}
	}
	if found == nil {
		t.Fatal("expected agent bead to be created")
	}

	if found.Fields["project"] != "gardenio" {
		t.Errorf("project = %q, want gardenio", found.Fields["project"])
	}
	if found.Fields["slack_user_id"] != "U-ORIGINAL" {
		t.Errorf("slack_user_id = %q, want U-ORIGINAL", found.Fields["slack_user_id"])
	}
	if found.Fields["mode"] != "crew" {
		t.Errorf("mode = %q, want crew (required for session resume)", found.Fields["mode"])
	}
	if !hasLabel(found.Labels, "project:gardenio") {
		t.Errorf("expected project:gardenio label, got %v", found.Labels)
	}
}

// TestRespawnThreadAgent_PreservesUserID verifies that respawnThreadAgent
// (invoked by the mention path — see bot_mentions.go) carries the requesting
// user's ID forward as slack_user_id on the new agent bead.
//
// Previously this coverage lived on handleThreadForward, but auto-respawn
// was removed from the forward path in gd-lUNTlpAVm5. The mention path now
// owns resume behavior, so we exercise respawnThreadAgent directly.
func TestRespawnThreadAgent_PreservesUserID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-uid-test")

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-uid-test"
	threadTS := "9999.1111"
	agentName := "thread-9999-1111"

	_ = state.SetThreadAgent(channel, threadTS, agentName)
	_ = state.SetListenThread(channel, threadTS)

	bot.respawnThreadAgent(context.Background(), channel, threadTS, agentName,
		"@bot follow up message", "U-POSTER")

	daemon.mu.Lock()
	var found *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Title == agentName {
			found = b
			break
		}
	}
	daemon.mu.Unlock()

	if found == nil {
		t.Fatal("expected agent bead to be created by respawn")
	}
	if found.Fields["slack_user_id"] != "U-POSTER" {
		t.Errorf("slack_user_id = %q, want U-POSTER", found.Fields["slack_user_id"])
	}
	if found.Fields["mode"] != "crew" {
		t.Errorf("mode = %q, want crew (required for session resume)", found.Fields["mode"])
	}
	// R3 regression (gd-V1AHgYdDgQ): respawn must stamp operated_by so the
	// resumed agent inherits BEADS_OPERATED_BY. Previously only slack_user_id
	// was set, leaving the resumed bead with an empty operated_by.
	if got := found.Fields["operated_by"]; got == "" {
		t.Errorf("operated_by is empty on respawn — every spawn must stamp operated_by (R3 ka-0000002887)")
	}
}

// TestRespawnThreadAgent_StampsOperatedByFromUserDirectory verifies the R3
// fix (gd-V1AHgYdDgQ): when the requesting Slack user resolves to a fleet
// human via UserDirectory, the resumed agent bead's operated_by must be
// `oidc:<email>` — not the `slack:<UID>` sentinel. Mirrors the initial-spawn
// behavior in handleThreadSpawn.
func TestRespawnThreadAgent_StampsOperatedByFromUserDirectory(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("testproj", "C-r3-test")
	// Fleet user bead so UserDirectory.LookupBySlackID resolves U-MATT.
	daemon.mu.Lock()
	daemon.beads["user-matt"] = &beadsapi.BeadDetail{
		ID:     "user-matt",
		Title:  "Matt Baker",
		Type:   "user",
		Kind:   "config",
		Status: "open",
		Fields: map[string]string{
			"email":    "matt@example.com",
			"slack_id": "U-MATT",
		},
	}
	daemon.mu.Unlock()

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	if err := bot.UserDirectory().Refresh(context.Background()); err != nil {
		t.Fatalf("UserDirectory.Refresh: %v", err)
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-r3-test"
	threadTS := "8888.2222"
	agentName := "thread-8888-2222"
	_ = state.SetThreadAgent(channel, threadTS, agentName)

	bot.respawnThreadAgent(context.Background(), channel, threadTS, agentName,
		"@bot wake up", "U-MATT")

	daemon.mu.Lock()
	var found *beadsapi.BeadDetail
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Title == agentName {
			found = b
			break
		}
	}
	daemon.mu.Unlock()

	if found == nil {
		t.Fatal("expected agent bead to be created by respawn")
	}
	if got, want := found.Fields["operated_by"], "oidc:matt@example.com"; got != want {
		t.Errorf("operated_by = %q, want %q (UserDirectory must resolve Slack id to fleet human)", got, want)
	}
	if got, want := found.Fields["user_bead_id"], "user-matt"; got != want {
		t.Errorf("user_bead_id = %q, want %q", got, want)
	}
	if got, want := found.Fields["owner"], "oidc:matt@example.com"; got != want {
		t.Errorf("owner = %q, want %q (the person who asked owns the thread agent)", got, want)
	}
}

// TestRespawnThreadAgent_CarriesLastSessionID verifies that respawnThreadAgent
// copies the last_session_id field from the most recent prior agent bead to
// the new bead. Without this, a respawned pod with an empty PVC has no way to
// locate the previous coop session in S3 (COOP_S3_RESUME_SESSION), defeating
// the whole point of S3 session archival. See gd-qQTNQNVHxK and gd-SoGqHnF4PJ.
func TestRespawnThreadAgent_CarriesLastSessionID(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-gardenio")

	agentName := "thread-3333-4444"
	priorID := "bd-prior-sess"
	daemon.beads[priorID] = &beadsapi.BeadDetail{
		ID:        priorID,
		Title:     agentName,
		Type:      "agent",
		Status:    "closed",
		CreatedAt: time.Now().Add(-30 * time.Minute),
		Fields: map[string]string{
			"agent":           agentName,
			"project":         "gardenio",
			"mode":            "crew",
			"last_session_id": "sess-TEST",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	b.respawnThreadAgent(context.Background(), "C-gardenio", "3333.4444", agentName, "wake up", "U-USER")

	var found *beadsapi.BeadDetail
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == agentName && bead.Status != "closed" {
			found = bead
			break
		}
	}
	if found == nil {
		t.Fatal("expected respawn to create a new agent bead")
	}
	if got := found.Fields["last_session_id"]; got != "sess-TEST" {
		t.Errorf("last_session_id = %q, want sess-TEST (must carry forward from prior bead for S3 restore)", got)
	}
}

func TestNotifyAgentState_DonePreservesThreadMapping(t *testing.T) {
	daemon := newMockDaemon()
	agentName := "thread-1111-2222"

	// Agent bead exists but WITHOUT thread fields — so resolveAgentThread
	// returns empty and we skip the postThreadStateReply path (which needs
	// a real Slack API). The mapping preservation is tested via the fact
	// that NotifyAgentState no longer calls RemoveThreadAgentByAgent.
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-agent-1",
		Title: agentName,
		Type:  "agent",
		Fields: map[string]string{
			"agent":       agentName,
			"agent_state": "done",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-test", "1111.2222", agentName)

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		agentState:      map[string]string{agentName: "working"},
		agentSeen:       map[string]time.Time{},
		agentPodName:    map[string]string{},
		agentImageTag:   map[string]string{},
		agentRole:       map[string]string{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	// Simulate agent transitioning to "done".
	b.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: agentName,
		Fields: map[string]string{
			"agent_state": "done",
			"agent":       agentName,
		},
	})

	// The thread→agent mapping should be PRESERVED (not cleared).
	agent, ok := state.GetThreadAgent("C-test", "1111.2222")
	if !ok {
		t.Fatal("expected thread→agent mapping to be preserved after agent done")
	}
	if agent != agentName {
		t.Errorf("mapping agent = %q, want %q", agent, agentName)
	}
}

func TestNotifyAgentState_FailedPreservesThreadMapping(t *testing.T) {
	daemon := newMockDaemon()
	agentName := "thread-2222-3333"

	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-agent-2",
		Title: agentName,
		Type:  "agent",
		Fields: map[string]string{
			"agent":       agentName,
			"agent_state": "failed",
		},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-test", "2222.3333", agentName)

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		agentState:      map[string]string{agentName: "working"},
		agentSeen:       map[string]time.Time{},
		agentPodName:    map[string]string{},
		agentImageTag:   map[string]string{},
		agentRole:       map[string]string{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	b.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: agentName,
		Fields: map[string]string{
			"agent_state": "failed",
			"agent":       agentName,
		},
	})

	// Mapping should still be there after failed state too.
	if _, ok := state.GetThreadAgent("C-test", "2222.3333"); !ok {
		t.Fatal("expected thread→agent mapping to be preserved after agent failed")
	}
}

func TestHandleAppMention_InactiveThreadAgent_Respawns(t *testing.T) {
	daemon := newMockDaemon()
	// Agent NOT in daemon → FindAgentBead will fail.
	// Add a project bead so the channel maps to a project.
	daemon.beads["proj-test"] = &beadsapi.BeadDetail{
		ID: "proj-test", Title: "testproject", Type: "project",
		Fields: map[string]string{"slack_channel": "C-test"},
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-test", "1111.2222", "thread-1111-2222")

	b := &Bot{
		daemon:          daemon,
		state:           state,
		logger:          slog.Default(),
		botUserID:       "U-BOT",
		agentCards:      map[string]MessageRef{},
		threadSpawnMsgs: make(map[string]MessageRef),
	}

	// Simulate an @mention in the thread with a dead agent.
	ev := &slackevents.AppMentionEvent{
		User:            "U-HUMAN",
		Text:            "<@U-BOT> check on this",
		Channel:         "C-test",
		TimeStamp:       "1111.3333",
		ThreadTimeStamp: "1111.2222",
	}

	b.handleAppMention(context.Background(), ev)

	// Should have respawned the SAME agent (not created a new thread-XXXX).
	var foundRespawn bool
	for _, bead := range daemon.beads {
		if bead.Type == "agent" && bead.Title == "thread-1111-2222" {
			foundRespawn = true
			if bead.Fields["spawn_source"] != "slack-thread-resume" {
				t.Errorf("expected spawn_source=slack-thread-resume, got %q", bead.Fields["spawn_source"])
			}
		}
		// Should NOT have spawned a different agent name.
		if bead.Type == "agent" && bead.Title != "thread-1111-2222" {
			t.Errorf("should not spawn a new agent, found %q", bead.Title)
		}
	}
	if !foundRespawn {
		t.Error("expected respawn of same agent thread-1111-2222")
	}

	// Mapping should be preserved.
	if _, ok := state.GetThreadAgent("C-test", "1111.2222"); !ok {
		t.Error("expected thread→agent mapping to be preserved after respawn")
	}
}

func TestHandleThreadSpawn_ConcurrentMentionsSpawnOnce(t *testing.T) {
	daemon := newMockDaemon()
	// Add a project bead so the channel maps to a project.
	daemon.beads["proj-race"] = &beadsapi.BeadDetail{
		ID: "proj-race", Title: "raceproject", Type: "project",
		Fields: map[string]string{"slack_channel": "C-race-test"},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-race-test"
	threadTS := "9999.1111"

	// Fire 5 concurrent mentions to the same thread.
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
				User:            "U-USER",
				Text:            "<@U-BOT> help me",
				TimeStamp:       "2222.3333",
				ThreadTimeStamp: threadTS,
				Channel:         channel,
			}, "help me")
		}()
	}
	wg.Wait()

	// Count how many agent beads were created for this thread.
	daemon.mu.Lock()
	var agentCount int
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			agentCount++
		}
	}
	daemon.mu.Unlock()

	if agentCount != 1 {
		t.Errorf("expected exactly 1 agent bead from concurrent spawns, got %d", agentCount)
	}
}

// TestHandleThreadSpawn_WritesTriggeredByDepToThreadBead exercises plan 3.1
// (epic ka-0000000933 / M2d): cold-start thread spawn must record a
// triggered-by dep from the spawned agent bead to the canonical Slack-thread
// bead. The pool-assign path is symmetric; both branches add the same dep.
func TestHandleThreadSpawn_WritesTriggeredByDepToThreadBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["proj-trig"] = &beadsapi.BeadDetail{
		ID: "proj-trig", Title: "trigproject", Type: "project",
		Fields: map[string]string{"slack_channel": "C-trig-test"},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	bot.state = state
	bot.botUserID = "U-BOT"
	bot.lastThreadNudge = make(map[string]time.Time)

	channel := "C-trig-test"
	threadTS := "7777.8888"

	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U-USER",
		Text:            "<@U-BOT> help me investigate",
		TimeStamp:       "8888.9999",
		ThreadTimeStamp: threadTS,
		Channel:         channel,
	}, "help me investigate")

	// Find the agent bead (cold-start path) and the thread bead.
	var agentBeadID, threadBeadID string
	threadLabel := threadBeadLabel(channel, threadTS)
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if b.Type == "agent" {
			agentBeadID = b.ID
		}
		if hasLabel(b.Labels, threadLabel) {
			threadBeadID = b.ID
		}
	}
	daemon.mu.Unlock()

	if agentBeadID == "" {
		t.Fatal("expected an agent bead from cold-start spawn")
	}
	if threadBeadID == "" {
		t.Fatal("expected the canonical thread bead to be created")
	}

	var matched bool
	for _, d := range daemon.getDeps() {
		if d.BeadID == agentBeadID && d.DependsOnID == threadBeadID && d.DepType == "triggered-by" {
			matched = true
			if d.CreatedBy != "slack-bridge" {
				t.Errorf("dep CreatedBy = %q, want slack-bridge", d.CreatedBy)
			}
			break
		}
	}
	if !matched {
		t.Errorf("expected triggered-by dep from %s to %s, got %+v", agentBeadID, threadBeadID, daemon.getDeps())
	}
}

func TestNotifyAgentState_WorkingUpdatesThreadSpawnMsg(t *testing.T) {
	daemon := newMockDaemon()
	agentName := "thread-coopmux-test"

	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:    "bd-coopmux-1",
		Title: agentName,
		Type:  "agent",
		Notes: "pod_name: gardenio-thread-coopmux-test-abc12",
		Fields: map[string]string{
			"agent":                agentName,
			"agent_state":         "working",
			"slack_thread_channel": "C-thread",
			"slack_thread_ts":     "5555.6666",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.coopmuxPublicURL = "https://mux.example.com"
	bot.agentState[agentName] = "spawning" // previous state
	bot.threadSpawnMsgs[agentName] = MessageRef{
		ChannelID: "C-thread",
		Timestamp: "7777.8888",
		ThreadTS:  "5555.6666",
	}

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	_ = state.SetThreadAgent("C-thread", "5555.6666", agentName)
	bot.state = state

	// Transition from spawning → working should update the spawn message.
	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: agentName,
		Fields: map[string]string{
			"agent_state": "working",
			"agent":       agentName,
		},
	})

	// Pod name should now be cached (fetched via fetchAndCachePodName).
	bot.mu.Lock()
	cachedPod := bot.agentPodName[agentName]
	bot.mu.Unlock()
	if cachedPod != "gardenio-thread-coopmux-test-abc12" {
		t.Errorf("expected pod_name cached, got %q", cachedPod)
	}
}

func TestReconcileThreadAgents(t *testing.T) {
	daemon := newMockDaemon()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	b := &Bot{
		daemon:     daemon,
		state:      state,
		logger:     slog.Default(),
		agentCards: make(map[string]MessageRef),
	}

	t.Run("restores thread mappings from agent beads", func(t *testing.T) {
		// Simulate an agent bead with thread fields (spawned by a previous bridge).
		daemon.mu.Lock()
		daemon.beads["bd-thread-agent"] = &beadsapi.BeadDetail{
			ID:    "bd-thread-agent",
			Title: "my-thread-agent",
			Type:  "agent",
			Fields: map[string]string{
				"agent":                "my-thread-agent",
				"role":                 "thread",
				"project":              "gardenio",
				"slack_thread_channel": "C-test",
				"slack_thread_ts":      "1111.2222",
			},
		}
		daemon.mu.Unlock()

		b.ReconcileThreadAgents(context.Background())

		agent, ok := state.GetThreadAgent("C-test", "1111.2222")
		if !ok {
			t.Fatal("expected thread→agent mapping to be restored")
		}
		if agent != "my-thread-agent" {
			t.Errorf("got %q, want %q", agent, "my-thread-agent")
		}
	})

	t.Run("does not overwrite existing mappings", func(t *testing.T) {
		// Pre-set a mapping for the same thread.
		_ = state.SetThreadAgent("C-test", "1111.2222", "existing-agent")

		b.ReconcileThreadAgents(context.Background())

		agent, _ := state.GetThreadAgent("C-test", "1111.2222")
		if agent != "existing-agent" {
			t.Errorf("existing mapping was overwritten: got %q, want %q", agent, "existing-agent")
		}
	})

	t.Run("skips agents without thread fields", func(t *testing.T) {
		// Clear all previous state and beads.
		state.ClearAllThreadAgents()
		daemon.mu.Lock()
		daemon.beads = map[string]*beadsapi.BeadDetail{
			"bd-card-agent": {
				ID:    "bd-card-agent",
				Title: "card-only-agent",
				Type:  "agent",
				Fields: map[string]string{
					"agent":   "card-only-agent",
					"role":    "crew",
					"project": "gardenio",
				},
			},
		}
		daemon.mu.Unlock()

		b.ReconcileThreadAgents(context.Background())

		if len(state.AllThreadAgents()) != 0 {
			t.Errorf("expected no thread agents, got %d", len(state.AllThreadAgents()))
		}
	})
}

func TestHandleThreadSpawn_DMChannel_CreatesAgentWithDPrefix(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state

	// Thread spawn in a DM channel (D-prefix).
	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U123",
		Text:            "<@U-BOT> project:gardenio help me with this",
		TimeStamp:       "7777.8888",
		ThreadTimeStamp: "7777.0000",
		Channel:         "D9876543210",
	}, "project:gardenio help me with this")

	// Verify an agent bead was created with the DM channel.
	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM thread spawn, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["slack_thread_channel"] != "D9876543210" {
			t.Errorf("expected slack_thread_channel=D9876543210, got %q", b.Fields["slack_thread_channel"])
		}
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio, got %q", b.Fields["project"])
		}
	}

	// Verify thread→agent mapping.
	agent, ok := state.GetThreadAgent("D9876543210", "7777.0000")
	if !ok {
		t.Fatal("expected thread→agent mapping to be set")
	}
	if agent != "thread-7777-0000" {
		t.Errorf("expected agent name thread-7777-0000, got %q", agent)
	}
}

func TestHandleThreadSpawn_DMChannel_AutoSelectsSoleProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state

	// DM thread spawn WITHOUT explicit project — should auto-select "gardenio".
	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U123",
		Text:            "<@U-BOT> help me with this",
		TimeStamp:       "8888.9999",
		ThreadTimeStamp: "8888.0000",
		Channel:         "D9876543210",
	}, "help me with this")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected 1 agent bead from DM auto-select, got %d", len(agentBeads))
	}
	for _, b := range agentBeads {
		if b.Fields["project"] != "gardenio" {
			t.Errorf("expected project=gardenio (auto-selected), got %q", b.Fields["project"])
		}
	}
}

func TestHandleThreadSpawn_DMChannel_MultipleProjects_Rejected(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")
	daemon.seedProject("monorepo")

	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state

	// DM thread spawn with multiple projects and no --project → should be rejected.
	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U123",
		Text:            "<@U-BOT> help me",
		TimeStamp:       "9999.1111",
		ThreadTimeStamp: "9999.0000",
		Channel:         "D9876543210",
	}, "help me")

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 0 {
		t.Errorf("expected 0 agent beads for DM with multiple projects, got %d", len(agentBeads))
	}
}

func TestHandleThreadSpawn_DMChannel_AutoSelectShowsProjectInMessage(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProject("gardenio")

	// Capture posted messages to verify content.
	var postedMessages []string
	var mu sync.Mutex
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/conversations.replies" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"ok":       true,
				"messages": []map[string]any{},
			})
			return
		}
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedMessages = append(postedMessages, r.FormValue("text"))
			mu.Unlock()
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	bot := newTestBot(daemon, slackSrv)
	bot.state = state

	// DM thread spawn WITHOUT explicit project — should auto-select and show project in message.
	bot.handleThreadSpawn(context.Background(), &slackevents.AppMentionEvent{
		User:            "U123",
		Text:            "<@U-BOT> help me",
		TimeStamp:       "5555.9999",
		ThreadTimeStamp: "5555.0000",
		Channel:         "D9876543210",
	}, "help me")

	// Verify the agent bead was created with the correct project (auto-selected).
	// Since spawn confirmations now use thread status instead of chat messages,
	// we verify the project was resolved by checking the created bead labels.
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	found := false
	for _, bead := range daemon.beads {
		if bead.Type == "agent" {
			for _, label := range bead.Labels {
				if label == "project:gardenio" {
					found = true
					break
				}
			}
		}
	}
	if !found {
		t.Error("expected agent bead to have project:gardenio label from auto-selection")
	}
}

// TestRespawnThreadAgent_ResurrectsCardFromState verifies that when
// respawnThreadAgent is called for an agent whose card was pruned from
// the hot cache but preserved in state, the card is restored to the hot
// cache and updated back to "spawning" state.
func TestRespawnThreadAgent_ResurrectsCardFromState(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-card-test")

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}

	channel := "C-card-test"
	threadTS := "5555.6666"
	agentName := "card-agent"

	// Simulate: card exists in persisted state (survived CompactStaleEntries)
	// but was pruned from the hot cache by pruneStaleAgentCards.
	cardRef := MessageRef{ChannelID: channel, Timestamp: threadTS}
	_ = state.SetAgentCard(agentName, cardRef)

	var updatedMessages []string
	var mu sync.Mutex
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			_ = r.ParseForm()
			mu.Lock()
			updatedMessages = append(updatedMessages, r.FormValue("ts"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.state = state

	// Hot cache does NOT have the card (simulating post-prune state).
	if _, ok := bot.agentCards[agentName]; ok {
		t.Fatal("precondition: card should not be in hot cache")
	}

	bot.respawnThreadAgent(context.Background(), channel, threadTS, agentName, "wake up", "U-USER")

	// Card should now be back in hot cache.
	ref, ok := bot.agentCards[agentName]
	if !ok {
		t.Fatal("expected card to be resurrected into hot cache")
	}
	if ref.ChannelID != channel || ref.Timestamp != threadTS {
		t.Errorf("card ref mismatch: got ch=%s ts=%s, want ch=%s ts=%s",
			ref.ChannelID, ref.Timestamp, channel, threadTS)
	}

	// Agent state should be set to "spawning".
	if bot.agentState[agentName] != "spawning" {
		t.Errorf("expected agentState[%s]=%q, got %q", agentName, "spawning", bot.agentState[agentName])
	}

	// Slack chat.update should have been called to update the card.
	mu.Lock()
	defer mu.Unlock()
	cardUpdated := false
	for _, ts := range updatedMessages {
		if ts == threadTS {
			cardUpdated = true
			break
		}
	}
	if !cardUpdated {
		t.Error("expected Slack card message to be updated via chat.update")
	}
}
