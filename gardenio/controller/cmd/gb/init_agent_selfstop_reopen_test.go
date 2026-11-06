package main

import (
	"testing"
)

// TestTeardown_SelfStoppedThenReopenedIsLeftAlone pins gd-elNIGGSqwj: an
// incarnation that already closed its own bead (gb stop) must not, at the
// later teardown, close the bead again after the bridge reopened it for the
// next turn. Doing so posted a false "interrupted" breadcrumb and cancelled the
// pending wake-up.
func TestTeardown_SelfStoppedThenReopenedIsLeftAlone(t *testing.T) {
	mock := &toctouMockDaemon{
		agentBeadID: "gd-thread-selfstop",
		fields:      map[string]any{"agent_state": "spawning"},
	}
	setupToctouEnv(t, mock)

	selfStoppedBeforeExit.Store(true)
	t.Cleanup(func() { selfStoppedBeforeExit.Store(false) })

	closeAgentBeadBestEffort("context_cancelled")

	if mock.replyPosts != 0 {
		t.Errorf("no interrupted breadcrumb expected, got %d post(s)", mock.replyPosts)
	}
	if len(mock.order) != 0 {
		t.Errorf("teardown must not write to the reopened bead; observed: %v", mock.order)
	}
}

// TestTeardown_NotSelfStoppedStillPostsBreadcrumb guards the regression side: a
// genuine mid-turn kill (agent never closed its bead) keeps today's behaviour.
func TestTeardown_NotSelfStoppedStillPostsBreadcrumb(t *testing.T) {
	mock := &toctouMockDaemon{
		agentBeadID: "gd-thread-midturn",
		fields:      map[string]any{},
	}
	setupToctouEnv(t, mock)
	selfStoppedBeforeExit.Store(false)

	closeAgentBeadBestEffort("context_cancelled")

	if mock.replyPosts != 1 {
		t.Errorf("mid-turn kill must still post the breadcrumb, got %d", mock.replyPosts)
	}
}
