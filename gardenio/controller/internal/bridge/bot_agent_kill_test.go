package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// --- handleClearAgent tests ---

func TestHandleClearAgent_DeletesCardAndCleansState(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Set up state manager.
	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	// Pre-populate agent maps.
	bot.mu.Lock()
	bot.agentCards["my-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}
	bot.agentPending["my-agent"] = 3
	bot.agentState["my-agent"] = "working"
	bot.agentPodName["my-agent"] = "pod-abc"
	bot.agentImageTag["my-agent"] = "v1.0.0"
	bot.agentRole["my-agent"] = "crew"
	bot.mu.Unlock()
	_ = sm.SetAgentCard("my-agent", MessageRef{ChannelID: "C123", Timestamp: "1111.2222"})
	_ = sm.SetThreadAgent("C123", "3333.4444", "my-agent")

	callback := slack.InteractionCallback{
		Channel: slack.Channel{},
		User:    slack.User{ID: "U456"},
	}
	callback.Channel.GroupConversation.Conversation.ID = "C123"

	bot.handleClearAgent(context.Background(), "my-agent", callback)

	// Verify all in-memory maps were cleaned.
	bot.mu.Lock()
	_, hasCard := bot.agentCards["my-agent"]
	_, hasPending := bot.agentPending["my-agent"]
	_, hasState := bot.agentState["my-agent"]
	_, hasPod := bot.agentPodName["my-agent"]
	_, hasTag := bot.agentImageTag["my-agent"]
	_, hasRole := bot.agentRole["my-agent"]
	bot.mu.Unlock()

	if hasCard {
		t.Error("expected agentCards to be cleared")
	}
	if hasPending {
		t.Error("expected agentPending to be cleared")
	}
	if hasState {
		t.Error("expected agentState to be cleared")
	}
	if hasPod {
		t.Error("expected agentPodName to be cleared")
	}
	if hasTag {
		t.Error("expected agentImageTag to be cleared")
	}
	if hasRole {
		t.Error("expected agentRole to be cleared")
	}
}

func TestHandleClearAgent_NoCard_NoOp(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	callback := slack.InteractionCallback{
		User: slack.User{ID: "U456"},
	}

	// Should not panic when no card exists.
	bot.handleClearAgent(context.Background(), "nonexistent-agent", callback)
}

func TestHandleClearAgent_ExtractsShortName(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	// Store card under short name.
	bot.mu.Lock()
	bot.agentCards["my-agent"] = MessageRef{ChannelID: "C123", Timestamp: "1111.2222"}
	bot.mu.Unlock()

	callback := slack.InteractionCallback{
		User: slack.User{ID: "U456"},
	}

	// Pass full identity; should extract short name and find the card.
	bot.handleClearAgent(context.Background(), "gardenio/crew/my-agent", callback)

	bot.mu.Lock()
	_, hasCard := bot.agentCards["my-agent"]
	bot.mu.Unlock()

	if hasCard {
		t.Error("expected card to be cleared using short name extracted from full identity")
	}
}

func TestHandleClearAgent_RemovesThreadAgentMapping(t *testing.T) {
	daemon := newMockDaemon()
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	tmpDir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(tmpDir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = sm

	// No card, but a thread mapping exists.
	_ = sm.SetThreadAgent("C123", "3333.4444", "my-agent")

	callback := slack.InteractionCallback{
		User: slack.User{ID: "U456"},
	}

	bot.handleClearAgent(context.Background(), "my-agent", callback)

	// Thread mapping should be cleaned up even when no card exists.
	_, ok := sm.GetThreadAgent("C123", "3333.4444")
	if ok {
		t.Error("expected thread agent mapping to be removed")
	}
}

// --- handleRestartThreadAgent thread-link tests (ka-0000001530) ---

// TestHandleRestartThreadAgent_LinksThreadBead is the regression guard for the
// restart-button linking gap (ka-0000001530). Before the fix,
// handleRestartThreadAgent built its agent-bead fields with NEITHER thread_id
// NOR slack_thread_bead_id and never called getOrCreateThreadBead — so the
// restart-spawned agent was invisible to the event-driven autocloser (which
// keys off slack_thread_bead_id) and to any thread_id rollup.
//
// This test FAILS on current main and PASSES after the fix. It asserts that a
// restart:
//
//  1. invokes getOrCreateThreadBead → a canonical thread-HEAD bead exists,
//     carrying the canonical slack-thread:{channel}:{ts} label,
//  2. stamps BOTH fields.thread_id AND fields.slack_thread_bead_id on the
//     restart-spawned agent bead, both equal to the thread-head id,
//  3. keeps the BARE `slack-thread` label on the AGENT bead (the canonical
//     label is the thread-head's exclusive identity — see
//     findThreadHeadByThreadLabel / getOrCreateThreadBead, which resolve it
//     Limit:1 newest-first and would otherwise be shadowed by the newer agent),
//  4. records a triggered-by dependency from the agent bead to the thread head.
func TestHandleRestartThreadAgent_LinksThreadBead(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-restart")
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = state

	channel := "C-restart"
	threadTS := "1700000000.123456"
	agentName := "thread-1700000000-123456"

	// Seed the existing (pre-restart) agent bead the restart button looks up.
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:        "bd-existing",
		Title:     agentName,
		Type:      "agent",
		Status:    "open",
		CreatedAt: time.Now().Add(-10 * time.Minute),
		Labels:    []string{"slack-thread", "project:gardenio"},
		// The restart button is the owner's (bot_kill_owner.go).
		OperatedBy: "oidc:opener@example.com",
		Fields: map[string]string{
			"agent":                agentName,
			"project":              "gardenio",
			"role":                 "thread",
			"slack_thread_channel": channel,
			"slack_thread_ts":      threadTS,
			"slack_user_id":        "U-OPENER",
		},
	}
	daemon.mu.Unlock()

	callback := slack.InteractionCallback{User: slack.User{ID: "U-CLICKER"}}
	callback.Channel.GroupConversation.Conversation.ID = channel
	callback.Container.ThreadTs = threadTS

	bot.handleRestartThreadAgent(asOpener(), agentName, callback)

	// The handler does its work in a goroutine; poll for the restart-spawned
	// agent bead (a new id, distinct from the seeded bd-existing).
	var restartBead *beadsapi.BeadDetail
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		daemon.mu.Lock()
		for _, b := range daemon.beads {
			if b.Type == "agent" && b.Title == agentName && b.ID != "bd-existing" {
				restartBead = b
				break
			}
		}
		daemon.mu.Unlock()
		if restartBead != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if restartBead == nil {
		t.Fatal("expected handleRestartThreadAgent to create a new agent bead")
	}

	// (1) getOrCreateThreadBead must have been invoked: a thread-HEAD bead
	// carrying the canonical slack-thread:{channel}:{ts} label must now exist.
	canonical := threadBeadLabel(channel, threadTS)
	var head *beadsapi.BeadDetail
	daemon.mu.Lock()
	for _, b := range daemon.beads {
		if slices.Contains(b.Labels, canonical) {
			head = b
			break
		}
	}
	daemon.mu.Unlock()
	if head == nil {
		t.Fatal("expected getOrCreateThreadBead to create a thread-head bead carrying the canonical slack-thread:{channel}:{ts} label")
	}
	if head.Type != "task" {
		t.Errorf("thread-head bead type = %q, want task (canonical label must live on the thread head, not the agent)", head.Type)
	}

	// (2) Both wish-pointer fields stamped on the agent bead, equal to the head.
	if got := restartBead.Fields["thread_id"]; got != head.ID {
		t.Errorf("restart agent fields.thread_id = %q, want %q (thread-head id)", got, head.ID)
	}
	if got := restartBead.Fields["slack_thread_bead_id"]; got != head.ID {
		t.Errorf("restart agent fields.slack_thread_bead_id = %q, want %q (thread-head id)", got, head.ID)
	}

	// (3) The AGENT bead keeps the BARE `slack-thread` label and must NOT carry
	// the canonical label (which would shadow the head in Limit:1 label lookups).
	hasBare := false
	for _, l := range restartBead.Labels {
		if l == "slack-thread" {
			hasBare = true
		}
		if l == canonical {
			t.Errorf("restart AGENT bead must NOT carry the canonical label %q — it is the thread-head's exclusive identity and would shadow the head in newest-first Limit:1 lookups", canonical)
		}
		if strings.HasPrefix(l, "slack-thread:") {
			t.Errorf("restart AGENT bead carries a canonical-form label %q; agent beads use the bare 'slack-thread'", l)
		}
	}
	if !hasBare {
		t.Errorf("restart agent labels = %v, want bare 'slack-thread' present", restartBead.Labels)
	}

	// (4) triggered-by dependency from the restart-spawned agent to the head.
	foundDep := false
	for _, d := range daemon.getDeps() {
		if d.BeadID == restartBead.ID && d.DependsOnID == head.ID && d.DepType == "triggered-by" {
			foundDep = true
			break
		}
	}
	if !foundDep {
		t.Errorf("expected triggered-by dep from restart agent %s to thread head %s; deps=%+v",
			restartBead.ID, head.ID, daemon.getDeps())
	}
}

// --- M8: priorAgentBead must push the lookup server-side ---
//
// Regression guard for ka-0000006235 / M8. The lookup resolves the newest agent
// bead for ONE agent name out of a population that INCLUDES closed beads — the
// unbounded axis. The old shape fetched every agent bead (no Limit, all
// statuses) and reduced client-side to the newest match. Once closed agent
// beads cross the daemon's default 1000-row cap, the fetch silently truncated
// (HTTP 200, no error): with the default created_at DESC ordering an OLDER
// agent name would fall past the cap and the function returned nil —
// indistinguishable from "no prior bead", cold-starting resume under a fresh
// id. The fix pushes Fields{agent}+Sort(-created_at)+Limit 1 to the daemon so
// the whole population no longer gates the lookup.

// recordingKillDaemon embeds the shared mockDaemon (promoting every BeadClient
// method) and overrides ONLY ListBeadsFiltered to honor Sort / Limit / Offset /
// Fields and record each query — the shared mock ignores them, which would hide
// the very truncation this guards against. Its corpus is an ordered slice so a
// created_at sort is observable.
type recordingKillDaemon struct {
	*mockDaemon
	corpus []*beadsapi.BeadDetail // pre-sorted newest-first by CreatedAt
	calls  []beadsapi.ListBeadsQuery
}

func (d *recordingKillDaemon) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	d.calls = append(d.calls, q)

	// Apply the field predicate (AND) the way the daemon would.
	matched := make([]*beadsapi.BeadDetail, 0, len(d.corpus))
	for _, b := range d.corpus {
		ok := true
		for k, v := range q.Fields {
			if b.Fields[k] != v {
				ok = false
				break
			}
		}
		if ok {
			matched = append(matched, b)
		}
	}

	// Order: corpus is stored newest-first; "-created_at" keeps that, plain
	// "created_at" reverses to oldest-first. The default (no sort) is
	// created_at DESC, matching the daemon.
	ordered := append([]*beadsapi.BeadDetail(nil), matched...)
	if q.Sort == "created_at" {
		for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
			ordered[i], ordered[j] = ordered[j], ordered[i]
		}
	}

	total := len(ordered)
	start := min(q.Offset, total)
	end := total
	// Model the daemon's silent default cap when the caller omits Limit — the
	// exact behavior M8 must defeat. With Limit<=0 the daemon applies
	// defaultListBeadsLimit (1000) and answers HTTP 200 with Total unchanged.
	limit := q.Limit
	if limit <= 0 {
		limit = 1000
	}
	if start+limit < end {
		end = start + limit
	}
	return &beadsapi.ListBeadsResult{Beads: ordered[start:end], Total: total}, nil
}

// TestPriorAgentBead_ServerSideLookup pins that the lookup is a
// targeted server-side query — Fields{agent}, Sort "-created_at", Limit 1 — and
// NOT a full scan, so an older agent name is still resolved even when 1000+
// newer (closed) agent beads would otherwise bury it past the daemon cap.
func TestPriorAgentBead_ServerSideLookup(t *testing.T) {
	base := time.Now()
	target := &beadsapi.BeadDetail{
		ID: "bd-old-target", Type: "agent", Status: "closed",
		CreatedAt: base.Add(-2000 * time.Hour), // oldest — would fall past a cap
		Fields: map[string]string{
			"agent":           "thread-old-agent",
			"project":         "gardenio",
			"last_session_id": "sess-XYZ",
		},
	}
	// Newest-first corpus: 1500 unrelated newer agent beads, then the target
	// last (oldest). A client-side scan capped at 1000 rows would never see it.
	corpus := make([]*beadsapi.BeadDetail, 0, 1501)
	for i := 0; i < 1500; i++ {
		corpus = append(corpus, &beadsapi.BeadDetail{
			ID: fmt.Sprintf("bd-noise-%04d", i), Type: "agent", Status: "closed",
			CreatedAt: base.Add(-time.Duration(i) * time.Minute),
			Fields:    map[string]string{"agent": fmt.Sprintf("other-%04d", i)},
		})
	}
	corpus = append(corpus, target)

	daemon := &recordingKillDaemon{mockDaemon: newMockDaemon(), corpus: corpus}
	bot := &Bot{daemon: daemon, logger: slog.Default()}

	got := priorAgentFields(bot, "thread-old-agent")

	// --- correctness: the older, buried bead is still resolved ---
	if got == nil {
		t.Fatal("priorAgentBead returned nil for an existing (older) agent bead — a full-scan lookup would silently miss it past the 1000-row cap (M8)")
	}
	if got["last_session_id"] != "sess-XYZ" {
		t.Errorf("resolved fields last_session_id=%q, want \"sess-XYZ\"", got["last_session_id"])
	}

	// --- request shape: server-side filter, not a full scan ---
	if len(daemon.calls) != 1 {
		t.Fatalf("made %d list calls, want exactly 1 targeted query", len(daemon.calls))
	}
	c := daemon.calls[0]
	if c.Fields["agent"] != "thread-old-agent" {
		t.Errorf("query Fields[agent]=%q, want \"thread-old-agent\" — the lookup must filter server-side, not scan (M8)", c.Fields["agent"])
	}
	if c.Sort != "-created_at" {
		t.Errorf("query Sort=%q, want \"-created_at\" (newest spawn first)", c.Sort)
	}
	if c.Limit != 1 {
		t.Errorf("query Limit=%d, want 1 — the caller reduces to a single map, so the daemon should return exactly one row", c.Limit)
	}
}

// TestPriorAgentBead_NoMatchReturnsNil pins that a name with no bead
// resolves to nil (the "no prior bead" signal), distinct from a truncation.
func TestPriorAgentBead_NoMatchReturnsNil(t *testing.T) {
	daemon := &recordingKillDaemon{mockDaemon: newMockDaemon(), corpus: []*beadsapi.BeadDetail{{
		ID: "bd-x", Type: "agent", Status: "open",
		Fields: map[string]string{"agent": "someone-else"},
	}}}
	bot := &Bot{daemon: daemon, logger: slog.Default()}
	if got := priorAgentFields(bot, "nobody"); got != nil {
		t.Errorf("expected nil for an unknown agent name, got %v", got)
	}
}

// TestPriorAgentBead_PicksNewestByCreatedAt pins the tie-break: when
// several beads share the agent name, the daemon's -created_at Limit 1 returns
// the newest, so callers inherit the most recent spawn's state.
func TestPriorAgentBead_PicksNewestByCreatedAt(t *testing.T) {
	base := time.Now()
	// corpus stored newest-first, both same agent name.
	daemon := &recordingKillDaemon{mockDaemon: newMockDaemon(), corpus: []*beadsapi.BeadDetail{
		{ID: "bd-new", Type: "agent", Status: "closed", CreatedAt: base,
			Fields: map[string]string{"agent": "dup", "last_session_id": "newest"}},
		{ID: "bd-old", Type: "agent", Status: "closed", CreatedAt: base.Add(-time.Hour),
			Fields: map[string]string{"agent": "dup", "last_session_id": "oldest"}},
	}}
	bot := &Bot{daemon: daemon, logger: slog.Default()}
	got := priorAgentFields(bot, "dup")
	if got == nil || got["last_session_id"] != "newest" {
		t.Errorf("expected newest bead (last_session_id=newest), got %v", got)
	}
}

// priorAgentFields returns the fields of bot.priorAgentBead(name), or nil.
func priorAgentFields(bot *Bot, name string) map[string]string {
	if bead := bot.priorAgentBead(context.Background(), name); bead != nil {
		return bead.Fields
	}
	return nil
}
