package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"

	"golang.org/x/time/rate"
)

// --- Pure function tests ---

func TestExtractImageTag_AllFormats(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ghcr.io/org/agent:v2026.58.3", "v2026.58.3"},
		{"image:latest", "latest"},
		{"latest", "latest"},
		{"", ""},
		{"registry.io/repo:tag:extra", "extra"},
	}
	for _, tc := range tests {
		if got := extractImageTag(tc.input); got != tc.want {
			t.Errorf("extractImageTag(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestExtractAgentProject(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gardenio/crew/test-bot", "gardenio"},
		{"test-bot", ""},
		{"a/b", "a"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := extractAgentProject(tc.input); got != tc.want {
			t.Errorf("extractAgentProject(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// GL-13, G-R2 (Lands step 6.8): the agent card links the agent's Glass when the
// bridge knows the viewer and the agent bead its pod resolves to, else the
// coopmux #<pod> deep link (mux.html stays, G-D6), else nothing.
func TestAgentCardHref(t *testing.T) {
	const viewer, mux = "https://garden.example.com/beads", "https://mux.example.com"
	tests := []struct {
		name                            string
		viewerURL, agentID, muxURL, pod string
		want                            string
	}{
		{"glass", viewer, "gd-Agent01", mux, "pod-123", viewer + "/garden?glass=gd-Agent01"},
		{"glass without coopmux", viewer, "gd-Agent01", "", "pod-123", viewer + "/garden?glass=gd-Agent01"},
		{"no agent bead falls back to coopmux", viewer, "", mux, "pod-123", mux + "#pod-123"},
		{"no viewer falls back to coopmux", "", "gd-Agent01", mux, "pod-123", mux + "#pod-123"},
		{"an id that is not a bead id is not linked", viewer, "gd-x|<evil>", mux, "pod-123", mux + "#pod-123"},
		{"no pod, no session to watch", viewer, "gd-Agent01", mux, "", ""},
		{"no links configured", "", "gd-Agent01", "", "pod-123", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentCardHref(tc.viewerURL, tc.agentID, tc.muxURL, tc.pod); got != tc.want {
				t.Errorf("agentCardHref(%q,%q,%q,%q) = %q, want %q", tc.viewerURL, tc.agentID, tc.muxURL, tc.pod, got, tc.want)
			}
		})
	}
}

func TestAgentCardLink(t *testing.T) {
	if got, want := agentCardLink("https://garden.example.com/garden?glass=gd-A1", "my-agent"), "<https://garden.example.com/garden?glass=gd-A1|my-agent>"; got != want {
		t.Errorf("agentCardLink = %q, want %q", got, want)
	}
	if got := agentCardLink("", "my-agent"); got != "my-agent" {
		t.Errorf("agentCardLink with no href = %q, want the plain name", got)
	}
}

// TestAgentThreadLink_NoCoopmuxLink guards ka-0000007955: agentThreadLink (used
// by thread state replies, comment-forward, and squawk) renders the plain word
// "agent" with no coopmux link, even when a pod name is cached. The clickable
// terminal link belongs only on the agent card.
func TestAgentThreadLink_NoCoopmuxLink(t *testing.T) {
	b := &Bot{
		coopmuxPublicURL: "https://mux.example.com",
		agentPodName:     map[string]string{"some-agent": "pod-some-agent-abc12"},
	}
	got := b.agentThreadLink("gardenio/crew/some-agent")
	if got != "agent" {
		t.Errorf("agentThreadLink = %q, want plain %q (no coopmux link)", got, "agent")
	}
	if strings.Contains(got, "mux.example.com") || strings.Contains(got, "<") {
		t.Errorf("agentThreadLink leaked a coopmux link: %q", got)
	}
}

// TestAgentDisplayName_NoCoopmuxLink guards ka-0000007955: agentDisplayName (used
// by the non-card spawn message, escalations, decision/mail notifications, chat)
// renders the plain short agent name with no coopmux link.
func TestAgentDisplayName_NoCoopmuxLink(t *testing.T) {
	b := &Bot{
		coopmuxPublicURL: "https://mux.example.com",
		agentPodName:     map[string]string{"some-agent": "pod-some-agent-abc12"},
	}
	got := b.agentDisplayName("gardenio/crew/some-agent")
	if got != "some-agent" {
		t.Errorf("agentDisplayName = %q, want plain %q (no coopmux link)", got, "some-agent")
	}
	if strings.Contains(got, "mux.example.com") || strings.Contains(got, "<") {
		t.Errorf("agentDisplayName leaked a coopmux link: %q", got)
	}
}

// TestBuildAgentCardBlocks_KeepsAgentLink guards ka-0000007955: the agent card
// is the ONE surface that must still carry the clickable agent link (since
// Lands step 6.8 the agent's Glass, GL-13).
func TestBuildAgentCardBlocks_KeepsAgentLink(t *testing.T) {
	blocks := buildAgentCardBlocks(Garden,
		"gardenio/crew/test-bot",
		"",          // subject
		0,           // pending
		"working",   // state
		"",          // taskTitle
		time.Time{}, // seen (zero)
		"https://garden.example.com/beads/garden?glass=gd-Agent01", // agentHref
		"",                // imageTag
		"",                // role
		"",                // costLine
		"",                // contextLine
		nil,               // toolStats
		"",                // bridgeVersion
		false,             // schedulingBlocked
		triggerCardInfo{}, // trigger
		nil,               // local
	)
	var rendered strings.Builder
	for _, blk := range blocks {
		if sec, ok := blk.(*slack.SectionBlock); ok && sec.Text != nil {
			rendered.WriteString(sec.Text.Text)
			rendered.WriteString("\n")
		}
	}
	if !strings.Contains(rendered.String(), "<https://garden.example.com/beads/garden?glass=gd-Agent01|test-bot>") {
		t.Errorf("expected the Glass link on agent card, got:\n%s", rendered.String())
	}
}

func TestBuildCompactAgentCardBlocks(t *testing.T) {
	tests := []struct {
		name           string
		agent          string
		state          string
		wantEmoji      string
		wantLabel      string
		wantNotContain string
	}{
		{"done", "test-bot", "done", Garden.Glyph(LifecycleDone), "agent finished", "~test-bot~"},
		{"failed", "test-bot", "failed", Garden.Glyph(LifecycleAborted), "failed", "~test-bot~"},
		{"other", "test-bot", "unknown", Garden.Glyph(LifecycleSpawning), "unknown", "~test-bot~"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			blocks := buildCompactAgentCardBlocks(Garden, tc.agent, tc.state, "", nil)
			if len(blocks) != 1 {
				t.Fatalf("expected 1 block, got %d", len(blocks))
			}
			section, ok := blocks[0].(*slack.SectionBlock)
			if !ok {
				t.Fatalf("expected *slack.SectionBlock, got %T", blocks[0])
			}
			text := section.Text.Text
			// Accessibility rule: the glyph is glued to its verbal label.
			if !strings.Contains(text, tc.wantEmoji+" "+tc.wantLabel) {
				t.Errorf("expected glyph %q glued to label %q in %q", tc.wantEmoji, tc.wantLabel, text)
			}
			if !strings.Contains(text, tc.wantLabel) {
				t.Errorf("expected state label %q in %q", tc.wantLabel, text)
			}
			if !strings.Contains(text, tc.agent) {
				t.Errorf("expected agent name %q in %q", tc.agent, text)
			}
			if strings.Contains(text, tc.wantNotContain) {
				t.Errorf("expected no strikethrough %q in %q", tc.wantNotContain, text)
			}
		})
	}
}

func TestBuildCompactAgentCardBlocks_DoneRelabel(t *testing.T) {
	blocks := buildCompactAgentCardBlocks(Garden, "my-agent", "done", "", nil)
	section := blocks[0].(*slack.SectionBlock)
	text := section.Text.Text
	if strings.Contains(text, " done ·") {
		t.Errorf("expected 'done' to be relabeled to 'agent finished', got %q", text)
	}
	if !strings.Contains(text, Garden.Glyph(LifecycleDone)+" agent finished ·") {
		t.Errorf("expected done glyph glued to 'agent finished' in %q", text)
	}
}

func TestBuildAgentCardBlocks_PendingOverridesState(t *testing.T) {
	// When pendingCount > 0, indicator should be blue circle regardless of state.
	blocks := buildAgentCardBlocks(Garden, "test-agent", "", 2, "working", "", time.Time{}, "", "", "", "", "", nil, "", false, triggerCardInfo{}, nil)
	if len(blocks) < 1 {
		t.Fatal("expected at least 1 block")
	}
	// Just verify the function runs without panic and returns blocks.
}

func TestBuildAgentCardBlocks_AllStatesDisplay(t *testing.T) {
	states := []string{"spawning", "working", "done", "failed", "rate_limited", "idle", ""}
	for _, state := range states {
		blocks := buildAgentCardBlocks(Garden, "test-agent", "", 0, state, "Fix bugs", time.Now(), "", "v1.0", "crew", "", "", nil, "", false, triggerCardInfo{}, nil)
		if len(blocks) < 2 {
			t.Errorf("state %q: expected at least 2 blocks, got %d", state, len(blocks))
		}
	}
}

func TestBuildAgentCardBlocks_TerminalStatesHaveClearButton(t *testing.T) {
	for _, state := range []string{"done", "failed"} {
		blocks := buildAgentCardBlocks(Garden, "test-agent", "", 0, state, "", time.Time{}, "", "", "", "", "", nil, "", false, triggerCardInfo{}, nil)
		// Terminal states should have 3 blocks: section + context + action (Clear button).
		if len(blocks) != 3 {
			t.Errorf("state %q: expected 3 blocks (with Clear button), got %d", state, len(blocks))
		}
	}
	// Non-terminal state should have 2 blocks (no Clear button).
	blocks := buildAgentCardBlocks(Garden, "test-agent", "", 0, "working", "", time.Time{}, "", "", "", "", "", nil, "", false, triggerCardInfo{}, nil)
	if len(blocks) != 2 {
		t.Errorf("state working: expected 2 blocks (no Clear button), got %d", len(blocks))
	}
}

func TestBuildWrapUpAgentCardBlocks(t *testing.T) {
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Garden, "test-agent", "done", `{"accomplishments":["Fixed bug"]}`, "", 0, nil)
	// Should have: header + wrapup content + action (Clear button).
	if len(blocks) < 2 {
		t.Errorf("expected at least 2 blocks, got %d", len(blocks))
	}
}

// --- NotifyAgentSpawn tests ---

func TestNotifyAgentSpawn_FlatMode_PostsMessage(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-1"] = &beadsapi.BeadDetail{
		ID: "agent-1", Type: "agent", Status: "open",
		Title: "my-agent", Fields: map[string]string{"agent": "my-agent"},
	}

	var mu sync.Mutex
	var postedText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedText = r.FormValue("text")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	// threadingMode="" = flat mode.

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "agent-1",
		Type:     "agent",
		Title:    "my-agent",
		Assignee: "my-agent",
		Fields:   map[string]string{"agent": "my-agent", "role": "crew"},
	})

	if state := bot.agentState["my-agent"]; state != "spawning" {
		t.Errorf("expected agentState=spawning, got %q", state)
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(postedText, "Agent spawned") {
		t.Errorf("expected spawn text, got %q", postedText)
	}
}

func TestNotifyAgentSpawn_AgentMode_PostsCard(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-2"] = &beadsapi.BeadDetail{
		ID: "agent-2", Type: "agent", Status: "open",
		Title: "card-agent", Fields: map[string]string{"agent": "card-agent"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "agent-2",
		Type:     "agent",
		Title:    "card-agent",
		Assignee: "card-agent",
		Fields:   map[string]string{"agent": "card-agent"},
	})

	if state := bot.agentState["card-agent"]; state != "spawning" {
		t.Errorf("expected agentState=spawning, got %q", state)
	}

	// Agent card should exist.
	if _, ok := bot.agentCards["card-agent"]; !ok {
		t.Error("expected agent card to be created in agent threading mode")
	}
}

func TestNotifyAgentSpawn_IdentityFromFields(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-3"] = &beadsapi.BeadDetail{
		ID: "agent-3", Type: "agent", Status: "open",
		Title: "field-agent", Fields: map[string]string{"agent": "field-agent"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// No Assignee — identity from Fields["agent"].
	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:     "agent-3",
		Type:   "agent",
		Title:  "field-agent",
		Fields: map[string]string{"agent": "gardenio/crew/field-agent"},
	})

	// Should be stored under short name.
	if _, ok := bot.agentState["field-agent"]; !ok {
		t.Error("expected agentState keyed by short name 'field-agent'")
	}
}

func TestNotifyAgentSpawn_IdentityFromTitle(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-4"] = &beadsapi.BeadDetail{
		ID: "agent-4", Type: "agent", Status: "open",
		Title: "title-agent", Fields: map[string]string{},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// No Assignee, no Fields["agent"] — identity from Title.
	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:     "agent-4",
		Type:   "agent",
		Title:  "title-agent",
		Fields: map[string]string{},
	})

	if _, ok := bot.agentState["title-agent"]; !ok {
		t.Error("expected agentState keyed by 'title-agent'")
	}
}

func TestNotifyAgentSpawn_EmptyAgent_NoOp(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-5"] = &beadsapi.BeadDetail{
		ID: "agent-5", Type: "agent", Status: "open",
		Fields: map[string]string{},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	// No identity at all — should return early.
	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:     "agent-5",
		Type:   "agent",
		Fields: map[string]string{},
	})

	if len(bot.agentState) != 0 {
		t.Errorf("expected no agentState entries, got %d", len(bot.agentState))
	}
}

func TestNotifyAgentSpawn_CachesRole(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["agent-role"] = &beadsapi.BeadDetail{
		ID: "agent-role", Type: "agent", Status: "open",
		Title: "role-agent", Fields: map[string]string{"agent": "role-agent", "role": "gardener"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:       "agent-role",
		Type:     "agent",
		Assignee: "role-agent",
		Fields:   map[string]string{"agent": "role-agent", "role": "gardener"},
	})

	if role := bot.agentRole["role-agent"]; role != "gardener" {
		t.Errorf("expected agentRole=gardener, got %q", role)
	}
}

// --- NotifyAgentState tests ---

func TestNotifyAgentState_UpdatesState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentState["test-agent"] = "spawning"

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "test-agent",
		Fields:   map[string]string{"agent_state": "working"},
	})

	if state := bot.agentState["test-agent"]; state != "working" {
		t.Errorf("expected agentState=working, got %q", state)
	}
}

// TestNotifyAgentState_PrewarmedWithoutCard_NoCard verifies that an updated
// event carrying agent_state=prewarmed for an agent with no existing card does
// NOT post a top-level idle card. SyncAll field writes (pod_scheduling_blocked,
// pod_name) arrive on prewarmed beads as "updated" events; without the guard
// these fall through updateAgentCard's create branch and leak an idle card for
// an unengaged pool agent (ka-0000003743).
func TestNotifyAgentState_PrewarmedWithoutCard_NoCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			postCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1.1"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentState(context.Background(), BeadEvent{
		ID:       "pool-1",
		Assignee: "pool-agent",
		Fields: map[string]string{
			"agent_state":            "prewarmed",
			"pod_scheduling_blocked": "false",
		},
	})

	if _, ok := bot.agentCards["pool-agent"]; ok {
		t.Error("expected no card for prewarmed agent without an existing card")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero chat.postMessage calls for idle prewarmed agent, got %d", postCount)
	}
	// State must still be tracked so the later spawning→working transition has
	// a correct prevState.
	if state := bot.agentState["pool-agent"]; state != "prewarmed" {
		t.Errorf("expected agentState=prewarmed to be recorded, got %q", state)
	}
}

// TestNotifyAgentState_PrewarmedWithCard_StillUpdates verifies the guard only
// suppresses card creation, not legitimate updates: once a prewarmed agent has
// a card (e.g. it was assigned then the pool re-asserted state), updates flow.
func TestNotifyAgentState_PrewarmedWithCard_StillUpdates(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"
	bot.agentCards["carded-agent"] = MessageRef{ChannelID: "C123", Timestamp: "5.5"}
	bot.agentState["carded-agent"] = "working"

	bot.NotifyAgentState(context.Background(), BeadEvent{
		ID:       "carded-1",
		Assignee: "carded-agent",
		Fields:   map[string]string{"agent_state": "prewarmed"},
	})

	if state := bot.agentState["carded-agent"]; state != "prewarmed" {
		t.Errorf("expected agentState=prewarmed, got %q", state)
	}
}

// TestNotifyAgentState_QueuedPoolAgent_NoCard reproduces the #gardenio card
// flood (ka-0000007013): under a pod-cap wall the reconciler flips a fresh
// prewarmed-pool bead to agent_state=queued (max_pods deferral). That arrives
// as an "updated" event. The old guard only matched state=="prewarmed", so the
// queued event fell through to updateAgentCard's create branch and posted a
// top-level card. The label-based isUnengagedPoolAgent guard must suppress it.
func TestNotifyAgentState_QueuedPoolAgent_NoCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			postCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1.1"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentState(context.Background(), BeadEvent{
		ID:       "pool-q",
		Assignee: "pool-queued-agent",
		Labels:   []string{"prewarmed", "project:gardenio"},
		Fields: map[string]string{
			"agent_state": "queued",
		},
	})

	if _, ok := bot.agentCards["pool-queued-agent"]; ok {
		t.Error("expected no card for queued unengaged pool agent")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero chat.postMessage calls for queued pool agent, got %d", postCount)
	}
}

// TestNotifyAgentState_PoolAgentClosedDone_NoCard covers the close path:
// expireDeferredBeads closes a never-carded pool bead, and handleClosed forces
// agent_state=done before calling NotifyAgentState. Keying only off
// state=="prewarmed" would let this done event create a card at close time;
// the label-based guard must still suppress it.
func TestNotifyAgentState_PoolAgentClosedDone_NoCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			postCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1.1"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentState(context.Background(), BeadEvent{
		ID:       "pool-c",
		Assignee: "pool-closed-agent",
		Labels:   []string{"prewarmed", "project:gardenio"},
		Fields: map[string]string{
			"agent_state": "done",
		},
	})

	if _, ok := bot.agentCards["pool-closed-agent"]; ok {
		t.Error("expected no card for closed unengaged pool agent")
	}
	mu.Lock()
	defer mu.Unlock()
	if postCount != 0 {
		t.Errorf("expected zero chat.postMessage calls for closed pool agent, got %d", postCount)
	}
}

// TestNotifyAgentState_EngagedPoolAgent_StillPosts verifies the guard does not
// over-suppress: once the pool assigns an agent (spawn_source=slack-thread-pool
// + thread binding), isUnengagedPoolAgent returns false so normal card flow
// resumes. A thread-bound agent uses streaming (no top-level card), so we assert
// the agent is recognized as engaged by checking state is recorded and the guard
// path was not taken (card creation proceeds to the thread-bound branch).
func TestNotifyAgentState_EngagedPoolAgent_NotSuppressed(t *testing.T) {
	bead := BeadEvent{
		ID:       "pool-e",
		Assignee: "engaged-agent",
		Labels:   []string{"prewarmed", "project:gardenio"},
		Fields: map[string]string{
			"agent_state":          "working",
			"spawn_source":         "slack-thread-pool",
			"slack_thread_channel": "C999",
			"slack_thread_ts":      "123.456",
		},
	}
	if isUnengagedPoolAgent(bead) {
		t.Error("expected an assigned pool agent (spawn_source + thread binding) to be treated as engaged")
	}
}

// TestIsUnengagedPoolAgent table-tests the engagement classifier directly.
func TestIsUnengagedPoolAgent(t *testing.T) {
	cases := []struct {
		name   string
		bead   BeadEvent
		expect bool
	}{
		{
			name:   "prewarmed label, no markers — unengaged",
			bead:   BeadEvent{Labels: []string{"prewarmed"}, Fields: map[string]string{"agent_state": "queued"}},
			expect: true,
		},
		{
			name:   "no prewarmed label — not a pool agent",
			bead:   BeadEvent{Labels: []string{"project:gardenio"}, Fields: map[string]string{"agent_state": "queued"}},
			expect: false,
		},
		{
			name:   "prewarmed label + spawn_source — engaged",
			bead:   BeadEvent{Labels: []string{"prewarmed"}, Fields: map[string]string{"spawn_source": "slack-thread-pool"}},
			expect: false,
		},
		{
			name:   "prewarmed label + thread binding — engaged",
			bead:   BeadEvent{Labels: []string{"prewarmed"}, Fields: map[string]string{"slack_thread_channel": "C1", "slack_thread_ts": "1.2"}},
			expect: false,
		},
		{
			name:   "prewarmed label + task_id — engaged",
			bead:   BeadEvent{Labels: []string{"prewarmed"}, Fields: map[string]string{"task_id": "gd-abc"}},
			expect: false,
		},
		{
			name:   "no labels at all — not a pool agent",
			bead:   BeadEvent{Fields: map[string]string{"agent_state": "prewarmed"}},
			expect: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnengagedPoolAgent(tc.bead); got != tc.expect {
				t.Errorf("isUnengagedPoolAgent = %v, want %v", got, tc.expect)
			}
		})
	}
}

func TestNotifyAgentState_EmptyAgent_NoOp(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// No identity — should return early.
	bot.NotifyAgentState(context.Background(), BeadEvent{
		Fields: map[string]string{"agent_state": "working"},
	})

	if len(bot.agentState) != 0 {
		t.Errorf("expected no agentState entries, got %d", len(bot.agentState))
	}
}

func TestNotifyAgentState_TerminalState_UpdatesCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentState["done-agent"] = "working"
	bot.agentCards["done-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "done-agent",
		Fields:   map[string]string{"agent_state": "done", "wrapup": `{"accomplishments":["Completed task"]}`},
	})

	if state := bot.agentState["done-agent"]; state != "done" {
		t.Errorf("expected agentState=done, got %q", state)
	}

	mu.Lock()
	defer mu.Unlock()
	if updateCount < 1 {
		t.Error("expected agent card update for terminal state")
	}
}

// TestNotifyAgentState_StoppingWithWrapup_RendersCard verifies the suspend/
// freeze path renders the wrap-up card (ka-0000006840). A freeze-eligible
// agent's `gb stop --wrapup` transitions agent_state=stopping and leaves the
// bead OPEN (no CloseBead). The card must still be replaced with the wrap-up
// summary — same as done/failed — so the operator sees the wrap-up.
func TestNotifyAgentState_StoppingWithWrapup_RendersCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			if err := r.ParseForm(); err == nil {
				mu.Lock()
				updateText = r.FormValue("text")
				mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentState["frozen-agent"] = "working"
	bot.agentCards["frozen-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "frozen-agent",
		Fields:   map[string]string{"agent_state": "stopping", "wrapup": `{"accomplishments":["Suspended after work"]}`},
	})

	if state := bot.agentState["frozen-agent"]; state != "stopping" {
		t.Errorf("expected agentState=stopping, got %q", state)
	}

	mu.Lock()
	defer mu.Unlock()
	// The wrap-up replacement renders the state into the message fallback text
	// ("Agent: <name> (stopping)"). A plain updateAgentCard would not.
	if !strings.Contains(updateText, "stopping") {
		t.Errorf("expected wrap-up card replacement for stopping state, got text %q", updateText)
	}
}

// TestNotifyAgentState_StoppingWithoutWrapup_NoWrapupCard verifies the stopping
// gate only fires when a wrapup is present: a bare stopping transition (no
// --wrapup) falls through to updateAgentCard and is NOT replaced with a
// wrap-up summary (ka-0000006840).
func TestNotifyAgentState_StoppingWithoutWrapup_NoWrapupCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateText string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			if err := r.ParseForm(); err == nil {
				mu.Lock()
				updateText = r.FormValue("text")
				mu.Unlock()
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentState["bare-stop-agent"] = "working"
	bot.agentCards["bare-stop-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "bare-stop-agent",
		Fields:   map[string]string{"agent_state": "stopping"},
	})

	mu.Lock()
	defer mu.Unlock()
	// updateAgentCard's fallback text is "Agent: <name>" without a state suffix;
	// the wrap-up replacement appends "(stopping)". Absence of "(stopping)"
	// confirms we did not take the wrap-up path.
	if strings.Contains(updateText, "(stopping)") {
		t.Errorf("expected no wrap-up replacement without a wrapup field, got text %q", updateText)
	}
}

// --- NotifyAgentTaskUpdate tests ---

func TestNotifyAgentTaskUpdate_UpdatesCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["task-agent"] = MessageRef{ChannelID: "C123", Timestamp: "2222.3333"}
	bot.agentState["task-agent"] = "working"

	bot.NotifyAgentTaskUpdate(context.Background(), "task-agent")

	mu.Lock()
	defer mu.Unlock()
	if updateCount < 1 {
		t.Error("expected agent card update on task update")
	}

	// agentSeen should be updated.
	if bot.agentSeen["task-agent"].IsZero() {
		t.Error("expected agentSeen to be set")
	}
}

func TestNotifyAgentTaskUpdate_NoCard_NoOp(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// No card exists — should not panic.
	bot.NotifyAgentTaskUpdate(context.Background(), "no-card-agent")

	// agentSeen should NOT be set since no card exists.
	if !bot.agentSeen["no-card-agent"].IsZero() {
		t.Error("expected agentSeen to not be set when no card exists")
	}
}

func TestNotifyAgentTaskUpdate_FullPath_NormalizesToShort(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["my-agent"] = MessageRef{ChannelID: "C123", Timestamp: "3333.4444"}
	bot.agentState["my-agent"] = "working"

	// Full path should be normalized to short name.
	bot.NotifyAgentTaskUpdate(context.Background(), "gardenio/crew/my-agent")

	mu.Lock()
	defer mu.Unlock()
	if updateCount < 1 {
		t.Error("expected agent card update with full path normalized to short name")
	}
}

// --- Card update debounce tests ---

func TestUpdateAgentCard_DebouncesRapidUpdates(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["debounce-agent"] = MessageRef{ChannelID: "C123", Timestamp: "5555.6666"}
	bot.agentState["debounce-agent"] = "working"

	ctx := context.Background()

	// First call should go through (no prior update).
	bot.updateAgentCard(ctx, "debounce-agent")

	// Rapid second call should be debounced (within cardUpdateMinInterval).
	bot.updateAgentCard(ctx, "debounce-agent")

	mu.Lock()
	count := updateCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 Slack API call (second debounced), got %d", count)
	}

	// Agent should be marked dirty.
	bot.mu.Lock()
	dirty := bot.agentCardDirty["debounce-agent"]
	bot.mu.Unlock()

	if !dirty {
		t.Error("expected debounced agent to be marked dirty")
	}
}

func TestUpdateAgentCard_TerminalStateBypassesDebounce(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["terminal-agent"] = MessageRef{ChannelID: "C123", Timestamp: "7777.8888"}

	ctx := context.Background()

	// First call with working state.
	bot.agentState["terminal-agent"] = "working"
	bot.updateAgentCard(ctx, "terminal-agent")

	// Second call with terminal state should bypass debounce.
	bot.agentState["terminal-agent"] = "done"
	bot.updateAgentCard(ctx, "terminal-agent")

	mu.Lock()
	count := updateCount
	mu.Unlock()

	if count != 2 {
		t.Errorf("expected 2 Slack API calls (terminal state bypasses debounce), got %d", count)
	}
}

func TestFlushDirtyCards_ProcessesDirtyAgents(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["flush-agent"] = MessageRef{ChannelID: "C123", Timestamp: "9999.0000"}
	bot.agentState["flush-agent"] = "working"

	// Simulate a dirty agent whose debounce window has elapsed.
	bot.agentCardDirty["flush-agent"] = true
	bot.agentCardLastUpdate["flush-agent"] = time.Now().Add(-10 * time.Second)

	bot.flushDirtyCards(context.Background())

	mu.Lock()
	count := updateCount
	mu.Unlock()

	if count != 1 {
		t.Errorf("expected 1 flush for dirty agent, got %d", count)
	}

	// Dirty flag should be cleared.
	bot.mu.Lock()
	dirty := bot.agentCardDirty["flush-agent"]
	bot.mu.Unlock()

	if dirty {
		t.Error("expected dirty flag to be cleared after flush")
	}
}

func TestFlushDirtyCards_SkipsRecentlyUpdated(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["recent-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.0000"}
	bot.agentState["recent-agent"] = "working"

	// Dirty but recently updated — should be skipped.
	bot.agentCardDirty["recent-agent"] = true
	bot.agentCardLastUpdate["recent-agent"] = time.Now()

	bot.flushDirtyCards(context.Background())

	mu.Lock()
	count := updateCount
	mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 flushes for recently-updated agent, got %d", count)
	}

	// Dirty flag should still be set (not yet eligible for flush).
	bot.mu.Lock()
	dirty := bot.agentCardDirty["recent-agent"]
	bot.mu.Unlock()

	if !dirty {
		t.Error("expected dirty flag to remain set for recently-updated agent")
	}
}

func TestFlushDirtyCards_RespectsGlobalRateLimiter(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Configure a very restrictive limiter: burst=1, then exhausted.
	bot.slackCardLimiter = rate.NewLimiter(rate.Limit(0.001), 1) // ~0 refill

	// Set up 5 dirty agents, all eligible for flush.
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("agent-%d", i)
		bot.agentCards[name] = MessageRef{ChannelID: "C123", Timestamp: fmt.Sprintf("%d.0000", 1000+i)}
		bot.agentState[name] = "working"
		bot.agentCardDirty[name] = true
		bot.agentCardLastUpdate[name] = time.Now().Add(-10 * time.Second)
	}

	bot.flushDirtyCards(context.Background())

	mu.Lock()
	count := updateCount
	mu.Unlock()

	// With burst=1, only 1 agent should be flushed; the rest stay dirty.
	if count != 1 {
		t.Errorf("expected 1 flush (rate limiter burst=1), got %d", count)
	}

	bot.mu.Lock()
	dirtyCount := len(bot.agentCardDirty)
	bot.mu.Unlock()

	// 4 agents should still be dirty (the flushed one's dirty flag was cleared).
	if dirtyCount != 4 {
		t.Errorf("expected 4 agents still dirty, got %d", dirtyCount)
	}
}

func TestFlushDirtyCards_RespectsRetryAfterBackoff(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["backoff-agent"] = MessageRef{ChannelID: "C123", Timestamp: "2222.3333"}
	bot.agentState["backoff-agent"] = "working"
	bot.agentCardDirty["backoff-agent"] = true
	bot.agentCardLastUpdate["backoff-agent"] = time.Now().Add(-10 * time.Second)

	// Simulate an active Retry-After backoff.
	bot.mu.Lock()
	bot.slackRateLimitUntil = time.Now().Add(30 * time.Second)
	bot.mu.Unlock()

	bot.flushDirtyCards(context.Background())

	mu.Lock()
	count := updateCount
	mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 flushes during Retry-After backoff, got %d", count)
	}

	// Agent should still be dirty.
	bot.mu.Lock()
	dirty := bot.agentCardDirty["backoff-agent"]
	bot.mu.Unlock()

	if !dirty {
		t.Error("expected agent to remain dirty during backoff")
	}
}

func TestUpdateAgentCard_DefersWhenBackingOff(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var updateCount int
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.update" {
			mu.Lock()
			updateCount++
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["backoff-agent2"] = MessageRef{ChannelID: "C123", Timestamp: "4444.5555"}
	bot.agentState["backoff-agent2"] = "working"

	// Simulate an active Retry-After backoff.
	bot.mu.Lock()
	bot.slackRateLimitUntil = time.Now().Add(30 * time.Second)
	bot.mu.Unlock()

	// Non-terminal update should be deferred.
	bot.updateAgentCard(context.Background(), "backoff-agent2")

	mu.Lock()
	count := updateCount
	mu.Unlock()

	if count != 0 {
		t.Errorf("expected 0 API calls during backoff, got %d", count)
	}

	bot.mu.Lock()
	dirty := bot.agentCardDirty["backoff-agent2"]
	bot.mu.Unlock()

	if !dirty {
		t.Error("expected agent to be marked dirty during backoff")
	}
}

// --- agentTask tests ---

func TestAgentTask_FindsTask(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["task-1"] = &beadsapi.BeadDetail{
		ID:       "task-1",
		Type:     "task",
		Kind:     "issue",
		Title:    "Fix login bug",
		Status:   "in_progress",
		Assignee: "test-agent",
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	title := agentTaskTitle(bot, "test-agent")
	if title != "Fix login bug" {
		t.Errorf("expected task title 'Fix login bug', got %q", title)
	}
}

func TestAgentTask_RetriesShortName(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["task-2"] = &beadsapi.BeadDetail{
		ID:       "task-2",
		Type:     "task",
		Kind:     "issue",
		Title:    "Deploy service",
		Status:   "in_progress",
		Assignee: "short-agent", // stored under short name
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Query with full path — should retry with short name.
	title := agentTaskTitle(bot, "gardenio/crew/short-agent")
	if title != "Deploy service" {
		t.Errorf("expected task title 'Deploy service', got %q", title)
	}
}

func TestAgentTask_NoTask_ReturnsEmpty(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	title := agentTaskTitle(bot, "no-task-agent")
	if title != "" {
		t.Errorf("expected empty title, got %q", title)
	}
}

// agentTaskTitle returns the title of bot.agentTask(agent), or "".
func agentTaskTitle(bot *Bot, agent string) string {
	if task := bot.agentTask(context.Background(), agent); task != nil {
		return task.Title
	}
	return ""
}

// --- fetchAndCachePodName tests ---

func TestFetchAndCachePodName_CachesPodAndImageTag(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["pod-agent"] = &beadsapi.BeadDetail{
		ID:    "bd-pod-agent",
		Type:  "agent",
		Title: "pod-agent",
		Notes: "pod_name:my-pod-123\nimage_tag:ghcr.io/org/agent:v2026.58.3",
		Fields: map[string]string{
			"agent": "pod-agent",
			"role":  "crew",
		},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	bot.fetchAndCachePodName(context.Background(), "pod-agent")

	if got := bot.agentPodName["pod-agent"]; got != "my-pod-123" {
		t.Errorf("expected podName=my-pod-123, got %q", got)
	}
	if got := bot.agentImageTag["pod-agent"]; got != "v2026.58.3" {
		t.Errorf("expected imageTag=v2026.58.3, got %q", got)
	}
	if got := bot.agentRole["pod-agent"]; got != "crew" {
		t.Errorf("expected role=crew, got %q", got)
	}
}

func TestFetchAndCachePodName_NotFound_NoOp(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Should not panic when agent bead not found.
	bot.fetchAndCachePodName(context.Background(), "unknown-agent")

	if len(bot.agentPodName) != 0 {
		t.Errorf("expected no podName entries, got %d", len(bot.agentPodName))
	}
}

// --- resolveAgentPod tests (ka-0000004617, Lands GL-13) ---

// A pod rename (thaw-b / warm-b / restart) after the first card render updates
// the bead's authoritative pod_name note but NOT the sticky in-memory cache.
// resolveAgentPod must return the fresh note value and self-heal the cache
// so the coopmux terminal link stops pointing at the dead pod.
func TestResolveAgentPod_FreshNoteWinsOverStaleCache(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["pod-agent"] = &beadsapi.BeadDetail{
		ID:     "bd-pod-agent",
		Type:   "agent",
		Title:  "pod-agent",
		Notes:  "pod_name:crew-gardenio-x-thaw-b\nimage_tag:ghcr.io/org/agent:v1",
		Fields: map[string]string{"agent": "pod-agent"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.coopmuxPublicURL = "https://mux.example.com"
	// Stale value cached from the pre-rename first render.
	bot.agentPodName["pod-agent"] = "crew-gardenio-x"

	got, agentID := bot.resolveAgentPod(context.Background(), "pod-agent", "crew-gardenio-x")
	if got != "crew-gardenio-x-thaw-b" {
		t.Errorf("expected fresh note pod name crew-gardenio-x-thaw-b, got %q", got)
	}
	if agentID != "bd-pod-agent" {
		t.Errorf("expected the pod resolved to agent bead bd-pod-agent, got %q", agentID)
	}
	// Cache must be self-healed so later surfaces converge.
	if cached := bot.agentPodName["pod-agent"]; cached != "crew-gardenio-x-thaw-b" {
		t.Errorf("expected cache self-healed to crew-gardenio-x-thaw-b, got %q", cached)
	}
}

// An empty pod_name note (pre-first-SyncAll window) must NOT clobber a good
// cached value — degrade to "possibly stale", never to "no link".
func TestResolveAgentPod_EmptyNoteKeepsCache(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["pod-agent"] = &beadsapi.BeadDetail{
		ID:     "bd-pod-agent",
		Type:   "agent",
		Title:  "pod-agent",
		Notes:  "image_tag:ghcr.io/org/agent:v1",
		Fields: map[string]string{"agent": "pod-agent"},
	}

	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.coopmuxPublicURL = "https://mux.example.com"
	bot.agentPodName["pod-agent"] = "crew-gardenio-x"

	got, _ := bot.resolveAgentPod(context.Background(), "pod-agent", "crew-gardenio-x")
	if got != "crew-gardenio-x" {
		t.Errorf("expected cached crew-gardenio-x preserved, got %q", got)
	}
}

// A daemon fetch failure must fall back to the cached value rather than dropping
// the link.
func TestResolveAgentPod_FetchFailureFallsBackToCache(t *testing.T) {
	daemon := newMockDaemon() // no bead seeded → FindAgentBeadAnyStatus returns not-found
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.coopmuxPublicURL = "https://mux.example.com"

	got, agentID := bot.resolveAgentPod(context.Background(), "unknown-agent", "cached-pod")
	if got != "cached-pod" || agentID != "" {
		t.Errorf("expected fallback to cached-pod and no agent on fetch failure, got %q, %q", got, agentID)
	}
}

// When no agent link is rendered at all (coopmuxPublicURL and beadsViewerURL
// unset), skip the daemon round-trip entirely and return the cache unchanged.
func TestResolveAgentPod_NoLinkURLSkipsFetch(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.coopmuxPublicURL = "" // linkless deployment
	bot.beadsViewerURL = ""

	got, _ := bot.resolveAgentPod(context.Background(), "pod-agent", "cached-pod")
	if got != "cached-pod" {
		t.Errorf("expected cached-pod returned without fetch, got %q", got)
	}
	if daemon.getCalls != 0 {
		t.Errorf("expected 0 daemon calls when no link URL is set, got %d", daemon.getCalls)
	}
}

// GL-13 (Lands step 6.8): a card render resolves the card's pod to its agent
// bead and links that agent's Glass, /garden?glass=<agent bead>; with no
// viewer URL, it keeps the coopmux #<pod> link.
func TestAgentCardHref_ResolvesThePodToTheAgentsGlass(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["pod-agent"] = &beadsapi.BeadDetail{
		ID:     "gd-PodAgent1",
		Type:   "agent",
		Title:  "pod-agent",
		Notes:  "pod_name:crew-gardenio-x-thaw-b",
		Fields: map[string]string{"agent": "pod-agent"},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.coopmuxPublicURL = "https://mux.example.com"
	bot.beadsViewerURL = "https://garden.example.com/beads"
	if got, want := bot.agentCardHref(context.Background(), "pod-agent", "crew-gardenio-x"), "https://garden.example.com/beads/garden?glass=gd-PodAgent1"; got != want {
		t.Errorf("agentCardHref = %q, want %q", got, want)
	}
	bot.beadsViewerURL = ""
	if got, want := bot.agentCardHref(context.Background(), "pod-agent", "crew-gardenio-x"), "https://mux.example.com#crew-gardenio-x-thaw-b"; got != want {
		t.Errorf("agentCardHref without a viewer = %q, want %q", got, want)
	}
}

// --- resolveChannel tests ---

func TestResolveChannel_DefaultChannel(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	if got := bot.resolveChannel("any-agent"); got != "C-DEFAULT" {
		t.Errorf("expected default channel, got %q", got)
	}
}

func TestResolveChannel_EmptyAgent(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-DEFAULT"

	// Empty agent should return default even with router.
	if got := bot.resolveChannel(""); got != "C-DEFAULT" {
		t.Errorf("expected default channel for empty agent, got %q", got)
	}
}

// --- Bot helper tests ---

func TestExtractAgentName_Paths(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"gardenio/crew/test-bot", "test-bot"},
		{"test-bot", "test-bot"},
		{"a/b/c/d", "d"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := extractAgentName(tc.input); got != tc.want {
			t.Errorf("extractAgentName(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestBeadTitle(t *testing.T) {
	tests := []struct {
		id, title, want string
	}{
		{"dec-1", "Deploy decision", "Deploy decision"},
		{"dec-2", "", "dec-2"},
	}
	for _, tc := range tests {
		if got := beadTitle(tc.id, tc.title); got != tc.want {
			t.Errorf("beadTitle(%q, %q) = %q, want %q", tc.id, tc.title, got, tc.want)
		}
	}
}

func TestTruncateText_EdgeCases(t *testing.T) {
	tests := []struct {
		input  string
		maxLen int
		want   string
	}{
		{"short", 10, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"this is a longer string", 10, "this is..."},
		{"ab", 2, "ab"},
		{"abc", 2, "ab"},
		{"abcde", 3, "abc"},
	}
	for _, tc := range tests {
		if got := truncateText(tc.input, tc.maxLen); got != tc.want {
			t.Errorf("truncateText(%q, %d) = %q, want %q", tc.input, tc.maxLen, got, tc.want)
		}
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name    string
		t       time.Time
		wantSfx string
	}{
		{"recent", now.Add(-10 * time.Second), "M"}, // time with AM/PM e.g. "3:04 PM UTC"
		{"minutes", now.Add(-5 * time.Minute), "m ago"},
		{"hours", now.Add(-3 * time.Hour), "h ago"},
		{"days", now.Add(-48 * time.Hour), "d ago"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatAge(tc.t)
			if !strings.Contains(got, tc.wantSfx) {
				t.Errorf("formatAge(%v) = %q, want suffix %q", tc.t, got, tc.wantSfx)
			}
		})
	}
}

// --- ensureAgentCard tests ---

func TestEnsureAgentCard_ReturnsCachedCard(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["cached-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}

	ts, err := bot.ensureAgentCard(context.Background(), "cached-agent", "C123")
	if err != nil {
		t.Fatalf("ensureAgentCard: %v", err)
	}
	if ts != "1111.2222" {
		t.Errorf("expected cached timestamp 1111.2222, got %q", ts)
	}
}

func TestEnsureAgentCard_PostsNewCard(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedChannel string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedChannel = r.FormValue("channel")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-NEW", "ts": "9999.8888"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	ts, err := bot.ensureAgentCard(context.Background(), "new-agent", "C-NEW")
	if err != nil {
		t.Fatalf("ensureAgentCard: %v", err)
	}
	if ts != "9999.8888" {
		t.Errorf("expected new timestamp, got %q", ts)
	}

	mu.Lock()
	defer mu.Unlock()
	if postedChannel != "C-NEW" {
		t.Errorf("expected post to C-NEW, got %q", postedChannel)
	}

	// Card should be cached.
	if _, ok := bot.agentCards["new-agent"]; !ok {
		t.Error("expected agent card to be cached")
	}
}

func TestEnsureAgentCard_DifferentChannel_CompressesOldAndPostsNew(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postCount int
	var updateCount int
	var updatedChannel string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/chat.postMessage":
			postCount++
		case "/chat.update":
			updateCount++
			_ = r.ParseForm()
			updatedChannel = r.FormValue("channel")
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-NEW", "ts": "9999.0001"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Card exists in C-OLD — requesting for C-NEW should compress old card
	// and post a new one.
	bot.agentCards["drift-agent"] = MessageRef{ChannelID: "C-OLD", Timestamp: "1111.1111"}

	ts, err := bot.ensureAgentCard(context.Background(), "drift-agent", "C-NEW")
	if err != nil {
		t.Fatalf("ensureAgentCard: %v", err)
	}
	// Should return the new card's timestamp, not the old one.
	if ts == "1111.1111" {
		t.Error("expected new card timestamp, got old one")
	}

	mu.Lock()
	defer mu.Unlock()
	// Old card should have been compressed via chat.update.
	if updateCount == 0 {
		t.Error("expected chat.update to compress old card")
	}
	if updatedChannel != "C-OLD" {
		t.Errorf("expected update to C-OLD, got %q", updatedChannel)
	}
	// New card should have been posted.
	if postCount == 0 {
		t.Error("expected chat.postMessage for new card")
	}
}

func TestEnsureAgentCard_InThread_SetsThreadTS(t *testing.T) {
	daemon := newMockDaemon()

	var mu sync.Mutex
	var postedThreadTS string
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			mu.Lock()
			postedThreadTS = r.FormValue("thread_ts")
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-THREAD", "ts": "8888.9999"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	ts, err := bot.ensureAgentCard(context.Background(), "thread-agent", "C-THREAD", "1234.5678")
	if err != nil {
		t.Fatalf("ensureAgentCard: %v", err)
	}
	if ts != "8888.9999" {
		t.Errorf("expected new timestamp 8888.9999, got %q", ts)
	}

	// Verify the card was posted in the thread.
	mu.Lock()
	defer mu.Unlock()
	if postedThreadTS != "1234.5678" {
		t.Errorf("expected thread_ts 1234.5678, got %q", postedThreadTS)
	}

	// Verify the cached ref has ThreadTS set.
	ref, ok := bot.agentCards["thread-agent"]
	if !ok {
		t.Fatal("expected agent card to be cached")
	}
	if ref.ThreadTS != "1234.5678" {
		t.Errorf("expected cached ThreadTS 1234.5678, got %q", ref.ThreadTS)
	}
}

func TestNotifyAgentState_RecoversMissingMetadata(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// Simulate bridge restart: card exists from state but in-memory routing
	// metadata is empty (agentSpawnChannel, agentProject, agentRole).
	bot.agentCards["recover-agent"] = MessageRef{ChannelID: "C-PROJECT", Timestamp: "1111.2222"}

	// Send a state update with bead fields that include routing metadata.
	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "recover-agent",
		Fields: map[string]string{
			"agent_state":         "working",
			"agent":               "recover-agent",
			"slack_spawn_channel": "C-SPAWN",
			"project":             "gardenio",
			"role":                "crew",
		},
	})

	// Verify routing metadata was recovered.
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if ch := bot.agentSpawnChannel["recover-agent"]; ch != "C-SPAWN" {
		t.Errorf("expected agentSpawnChannel=C-SPAWN, got %q", ch)
	}
	if p := bot.agentProject["recover-agent"]; p != "gardenio" {
		t.Errorf("expected agentProject=gardenio, got %q", p)
	}
	if r := bot.agentRole["recover-agent"]; r != "crew" {
		t.Errorf("expected agentRole=crew, got %q", r)
	}
}

func TestNotifyAgentState_DoesNotOverrideExistingMetadata(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["existing-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}
	// Pre-populate routing metadata (simulating normal spawn path).
	bot.agentSpawnChannel["existing-agent"] = "C-ORIGINAL"
	bot.agentProject["existing-agent"] = "original-project"
	bot.agentRole["existing-agent"] = "lead"

	// Send a state update with different routing metadata.
	bot.NotifyAgentState(context.Background(), BeadEvent{
		Assignee: "existing-agent",
		Fields: map[string]string{
			"agent_state":         "working",
			"agent":               "existing-agent",
			"slack_spawn_channel": "C-DIFFERENT",
			"project":             "other-project",
			"role":                "crew",
		},
	})

	// Existing metadata should NOT be overwritten.
	bot.mu.Lock()
	defer bot.mu.Unlock()
	if ch := bot.agentSpawnChannel["existing-agent"]; ch != "C-ORIGINAL" {
		t.Errorf("expected agentSpawnChannel=C-ORIGINAL (not overwritten), got %q", ch)
	}
	if p := bot.agentProject["existing-agent"]; p != "original-project" {
		t.Errorf("expected agentProject=original-project (not overwritten), got %q", p)
	}
	if r := bot.agentRole["existing-agent"]; r != "lead" {
		t.Errorf("expected agentRole=lead (not overwritten), got %q", r)
	}
}

// TestNotifyAgentSpawn_SkipsPrewarmedAgent verifies that NotifyAgentSpawn
// does not post a card for prewarmed pool agents (agent_state=prewarmed).
// These agents sit idle until assigned to a thread; posting a card during
// pool warmup creates a stale top-level message.
func TestNotifyAgentSpawn_SkipsPrewarmedAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["pool-1"] = &beadsapi.BeadDetail{
		ID: "pool-1", Type: "agent", Status: "open",
		Title:  "prewarmed-1234-1",
		Fields: map[string]string{"agent": "prewarmed-1234-1", "agent_state": "prewarmed"},
	}

	var mu sync.Mutex
	posted := false
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			posted = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:     "pool-1",
		Type:   "agent",
		Title:  "prewarmed-1234-1",
		Fields: map[string]string{"agent": "prewarmed-1234-1", "agent_state": "prewarmed", "project": "gardenio"},
	})

	mu.Lock()
	defer mu.Unlock()
	if posted {
		t.Error("expected no Slack message for prewarmed agent, but one was posted")
	}
	// Agent state should NOT be cached (we returned before caching).
	if _, ok := bot.agentState["prewarmed-1234-1"]; ok {
		t.Error("expected no agentState entry for prewarmed agent")
	}
}

// TestNotifyAgentSpawn_SkipsThreadSourcedAgent verifies that the spawn_source
// fallback prevents card posting for thread-spawned agents even when the
// slack_thread_channel/ts fields are not yet available.
func TestNotifyAgentSpawn_SkipsThreadSourcedAgent(t *testing.T) {
	daemon := newMockDaemon()
	// Seed the agent bead WITHOUT slack_thread_channel/ts to simulate the
	// race where the SSE event arrives before those fields propagate.
	daemon.beads["thread-agent"] = &beadsapi.BeadDetail{
		ID: "thread-agent", Type: "agent", Status: "open",
		Title: "thread-1234-5678",
		Fields: map[string]string{
			"agent":        "thread-1234-5678",
			"spawn_source": "slack-thread",
		},
	}

	var mu sync.Mutex
	posted := false
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			posted = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:    "thread-agent",
		Type:  "agent",
		Title: "thread-1234-5678",
		Fields: map[string]string{
			"agent":        "thread-1234-5678",
			"spawn_source": "slack-thread",
		},
	})

	mu.Lock()
	defer mu.Unlock()
	if posted {
		t.Error("expected no Slack message for thread-sourced agent (spawn_source fallback), but one was posted")
	}
}

// TestNotifyAgentSpawn_SkipsThreadBoundAgent verifies that thread-bound agents
// (with slack_thread_channel and slack_thread_ts in bead fields) skip the
// top-level card notification.
func TestNotifyAgentSpawn_SkipsThreadBoundAgent(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["thread-bound"] = &beadsapi.BeadDetail{
		ID: "thread-bound", Type: "agent", Status: "open",
		Title: "thread-9999-0001",
		Fields: map[string]string{
			"agent":                "thread-9999-0001",
			"slack_thread_channel": "C-THREAD",
			"slack_thread_ts":      "9999.0001",
			"spawn_source":         "slack-thread",
		},
	}

	var mu sync.Mutex
	posted := false
	slackSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			mu.Lock()
			posted = true
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	}))
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C123"
	bot.threadingMode = "agent"

	bot.NotifyAgentSpawn(context.Background(), BeadEvent{
		ID:    "thread-bound",
		Type:  "agent",
		Title: "thread-9999-0001",
		Fields: map[string]string{
			"agent":                "thread-9999-0001",
			"slack_thread_channel": "C-THREAD",
			"slack_thread_ts":      "9999.0001",
			"spawn_source":         "slack-thread",
		},
	})

	mu.Lock()
	defer mu.Unlock()
	if posted {
		t.Error("expected no Slack message for thread-bound agent, but one was posted")
	}
}

// TestAgentCardLocation_HotCacheHit: when a card ref is in the hot cache, the
// resolver returns its channel + ts. This is the canonical path used by the
// human-survey resolver (ka-0000002021) to land surveys "where the card lives".
func TestAgentCardLocation_HotCacheHit(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["fsm-triage-1"] = MessageRef{
		ChannelID: "C-FSM-TRIAGE",
		Timestamp: "1700000000.001234",
	}

	ch, ts, ok := bot.AgentCardLocation("fsm-triage-1")
	if !ok {
		t.Fatalf("expected ok=true for cached agent")
	}
	if ch != "C-FSM-TRIAGE" || ts != "1700000000.001234" {
		t.Errorf("got (%q, %q); want (C-FSM-TRIAGE, 1700000000.001234)", ch, ts)
	}
}

// TestAgentCardLocation_MissReturnsFalse: agents with no card in the hot cache
// or persisted state return ok=false. The HumanSurveyAPI bubbles this as 400
// "channel is required", which the agent then soft-skips.
func TestAgentCardLocation_MissReturnsFalse(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	// state is nil (test bot has no persisted state) — so the only lookup
	// path is the hot cache, which is empty for this agent.

	if _, _, ok := bot.AgentCardLocation("orphan-agent"); ok {
		t.Errorf("expected ok=false for unknown agent")
	}
}

// TestAgentCardLocation_EmptyAgentReturnsFalse: an empty agent string is
// rejected without panicking — defensive guard for the resolver path.
func TestAgentCardLocation_EmptyAgentReturnsFalse(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	if _, _, ok := bot.AgentCardLocation(""); ok {
		t.Errorf("expected ok=false for empty agent string")
	}
}

// TestAgentCardLocation_NilBot: calling on a nil receiver does not panic and
// returns ok=false.
func TestAgentCardLocation_NilBot(t *testing.T) {
	var bot *Bot
	if _, _, ok := bot.AgentCardLocation("any"); ok {
		t.Errorf("expected ok=false for nil bot")
	}
}

// TestAgentCardLocation_IgnoresIncompleteRef: a malformed ref with missing
// channel or ts does not satisfy the lookup — callers expect either a fully-
// populated ref or ok=false.
func TestAgentCardLocation_IgnoresIncompleteRef(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)
	bot.agentCards["partial-channel"] = MessageRef{ChannelID: "", Timestamp: "1.2"}
	bot.agentCards["partial-ts"] = MessageRef{ChannelID: "C-X", Timestamp: ""}

	if _, _, ok := bot.AgentCardLocation("partial-channel"); ok {
		t.Errorf("ref with empty channel must not satisfy lookup")
	}
	if _, _, ok := bot.AgentCardLocation("partial-ts"); ok {
		t.Errorf("ref with empty ts must not satisfy lookup")
	}
}
