package bridge

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func newTestState(t *testing.T) *StateManager {
	t.Helper()
	sm, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}
	return sm
}

// A Slack thread aliased to its head reaches the one binding by either key,
// and an entry written before the head was known moves under the head.
func TestThreadHeads_AliasMovesTupleBindingUnderHead(t *testing.T) {
	sm := newTestState(t)
	if err := sm.SetThreadAgent("C1", "1.2", "thread-1-2"); err != nil {
		t.Fatal(err)
	}
	if err := sm.BindThreadHead("C1", "1.2", "gd-head"); err != nil {
		t.Fatal(err)
	}
	if got, ok := sm.GetThreadAgentByHead("gd-head"); !ok || got != "thread-1-2" {
		t.Fatalf("by head = %q %v, want thread-1-2", got, ok)
	}
	if got, ok := sm.GetThreadAgent("C1", "1.2"); !ok || got != "thread-1-2" {
		t.Fatalf("by tuple = %q %v, want thread-1-2", got, ok)
	}
	if _, ok := sm.data.ThreadAgents["C1:1.2"]; ok {
		t.Fatal("the tuple entry should have moved under the head")
	}
	if c, ts, ok := sm.SlackThreadOfHead("gd-head"); !ok || c != "C1" || ts != "1.2" {
		t.Fatalf("SlackThreadOfHead = %q %q %v", c, ts, ok)
	}

	// A write through the tuple lands on the head's entry.
	if err := sm.SetThreadAgent("C1", "1.2", "thread-1-2b"); err != nil {
		t.Fatal(err)
	}
	if got, _ := sm.GetThreadAgentByHead("gd-head"); got != "thread-1-2b" {
		t.Fatalf("by head after tuple write = %q", got)
	}
	if len(sm.data.ThreadAgents) != 1 {
		t.Fatalf("want one binding, got %v", sm.data.ThreadAgents)
	}
}

// A Commons thread with no Slack copy is bound by its head alone, and the
// state file keeps it across a reload.
func TestThreadHeads_HeadOnlyBindingPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	sm, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SetThreadAgentByHead("gd-commons", "thread-cabc"); err != nil {
		t.Fatal(err)
	}
	if err := sm.SetThreadAgentByHead("C1:1.2", "x"); err == nil {
		t.Fatal("a tuple is no head id")
	}
	sm2, err := NewStateManager(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := sm2.GetThreadAgentByHead("gd-commons"); !ok || got != "thread-cabc" {
		t.Fatalf("reloaded = %q %v", got, ok)
	}
	b := sm2.AllThreadBindings()
	if len(b) != 1 || b[0].Head != "gd-commons" || b[0].Channel != "" {
		t.Fatalf("bindings = %+v", b)
	}
	if got := sm2.AllThreadAgents(); got["gd-commons"] != "thread-cabc" {
		t.Fatalf("AllThreadAgents = %v", got)
	}
}

// The 24h sweep and RemoveThreadAgentByAgent drop a head-keyed binding with
// the listen flag of its Slack alias, and the sweep prunes a dangling alias.
func TestThreadHeads_SweepAndRemoveByAgent(t *testing.T) {
	sm := newTestState(t)
	_ = sm.BindThreadHead("C1", "1.2", "gd-a")
	_ = sm.SetThreadAgent("C1", "1.2", "agent-a")
	_ = sm.SetListenThread("C1", "1.2")
	_ = sm.BindThreadHead("C2", "3.4", "gd-b")
	_ = sm.SetThreadAgent("C2", "3.4", "agent-b")

	if err := sm.RemoveThreadAgentByAgent("agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, ok := sm.GetThreadAgentByHead("gd-a"); ok {
		t.Fatal("agent-a should be unbound")
	}
	if sm.IsListenThread("C1", "1.2") {
		t.Fatal("agent-a's listen flag should be gone")
	}

	// Age agent-b's binding past the TTL.
	sm.mu.Lock()
	e := sm.data.ThreadAgents["gd-b"]
	e.LastActive = time.Now().Add(-2 * ThreadAgentTTL)
	sm.data.ThreadAgents["gd-b"] = e
	sm.mu.Unlock()
	if n, err := sm.CleanExpiredThreadAgents(ThreadAgentTTL); err != nil || n != 1 {
		t.Fatalf("CleanExpiredThreadAgents = %d %v, want 1", n, err)
	}
	if _, ok := sm.ThreadHead("C1", "1.2"); ok {
		t.Fatal("a dangling alias should be pruned")
	}
	if _, ok := sm.ThreadHead("C2", "3.4"); ok {
		t.Fatal("an expired binding's alias should be pruned")
	}
	if got := sm.GetThreadAgentsByChannel("C2"); len(got) != 0 {
		t.Fatalf("GetThreadAgentsByChannel = %v", got)
	}
}

// GetThreadAgentsByChannel finds a head-keyed binding through its alias.
func TestThreadHeads_ByChannelThroughAlias(t *testing.T) {
	sm := newTestState(t)
	_ = sm.BindThreadHead("C9", "5.6", "gd-c")
	_ = sm.SetThreadAgent("C9", "5.6", "agent-c")
	_ = sm.SetThreadAgent("C9", "7.8", "agent-legacy")
	got := sm.GetThreadAgentsByChannel("C9")
	if len(got) != 2 {
		t.Fatalf("GetThreadAgentsByChannel = %v, want both", got)
	}
}

// Validation probes a Commons thread's head, not Slack: a closed head's
// binding goes, an open one's stays, and a Slack-bound one is left to Slack.
func TestValidateThreadBindings_HeadOnly(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-open"] = &beadsapi.BeadDetail{ID: "gd-open", Type: "task", Status: "open", Labels: []string{"thread"}}
	daemon.beads["gd-shut"] = &beadsapi.BeadDetail{ID: "gd-shut", Type: "task", Status: "closed", Labels: []string{"thread"}}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	b := newTestBot(daemon, slackSrv)
	b.state = newTestState(t)
	b.api = nil // no Slack: a Slack-bound binding is kept unprobed
	_ = b.state.SetThreadAgentByHead("gd-open", "thread-copen")
	_ = b.state.SetThreadAgentByHead("gd-shut", "thread-cshut")
	_ = b.state.SetThreadAgent("C1", "1.2", "thread-1-2")

	b.validateThreadBindings(context.Background())

	if _, ok := b.state.GetThreadAgentByHead("gd-open"); !ok {
		t.Error("an open head's binding should stay")
	}
	if _, ok := b.state.GetThreadAgentByHead("gd-shut"); ok {
		t.Error("a closed head's binding should go")
	}
	if _, ok := b.state.GetThreadAgent("C1", "1.2"); !ok {
		t.Error("a Slack binding should stay when Slack is not reachable")
	}
}
