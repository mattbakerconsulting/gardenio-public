package bridge

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// These tests close the ka-0000002089 gap: a user @-mentions a thread agent
// RIGHT AFTER it shuts down. The agent bead still exists (the controller has
// not yet reaped it) but its agent_state is terminal (done/failed/stopping/
// budget_exceeded). Previously handleAppMention only respawned when
// FindAgentBead ERRORED (bead fully gone) — a terminal-state bead succeeded,
// isAgentStartingUp returned false, and the code fell through to the nudge
// path. NudgeAgent then returned ErrAgentNotNudgeable and the user saw the
// misleading ":warning: ... nudge failed ... will pick it up on its next
// cycle" reply, even though a done/failed agent has no next cycle and the task
// silently orphaned. The fix respawns (session resume) instead.

// TestHandleAppMention_ThreadAgentTerminal_Respawns verifies that mentioning a
// thread-bound agent whose bead is in a terminal state triggers a session-
// resume respawn (cold-resume bead on a pool miss) rather than a doomed nudge.
func TestHandleAppMention_ThreadAgentTerminal_Respawns(t *testing.T) {
	for _, state := range []string{"done", "failed", "stopping", "budget_exceeded"} {
		t.Run(state, func(t *testing.T) {
			daemon := newMockDaemon()
			daemon.seedProjectWithChannelRole("monorepo", "C-TERM", "thread")

			const agentName = "thread-7777-2222"
			// Active (open) agent bead in a terminal state — FindAgentBead
			// resolves it, exercising the terminal-state branch (not the
			// gone-bead branch which already respawned).
			daemon.mu.Lock()
			daemon.beads[agentName] = &beadsapi.BeadDetail{
				ID:     "bd-agent-term",
				Title:  agentName,
				Type:   "agent",
				Status: "open",
				Fields: map[string]string{
					"agent":       agentName,
					"project":     "monorepo",
					"agent_state": state,
				},
				Notes: "backend: coop\ncoop_url: http://127.0.0.1:9999",
			}
			daemon.mu.Unlock()

			dir := t.TempDir()
			sm, err := NewStateManager(filepath.Join(dir, "state.json"))
			if err != nil {
				t.Fatal(err)
			}
			// Bind the thread to the agent so the reverse-lookup resolves it.
			if err := sm.SetThreadAgent("C-TERM", "1111.0000", agentName); err != nil {
				t.Fatal(err)
			}

			slackSrv := newFakeSlackServer(t)
			defer slackSrv.Close()

			// Pool assign returns 404 (miss) so respawn degrades to the cold
			// session-resume mint, leaving an observable agent bead.
			var gotRole string
			var mu sync.Mutex
			var called bool
			ctrlSrv := fakeControllerForAssign(t, false, &gotRole, &mu, &called)
			defer ctrlSrv.Close()

			b := newTestBot(daemon, slackSrv)
			b.state = sm
			b.botUserID = "U-BOT"
			b.controllerURL = ctrlSrv.URL

			b.handleAppMention(context.Background(), &slackevents.AppMentionEvent{
				User:            "U-HUMAN",
				Text:            "<@U-BOT> are you still there?",
				TimeStamp:       "2222.3333",
				ThreadTimeStamp: "1111.0000",
				Channel:         "C-TERM",
			})

			mu.Lock()
			wasCalled := called
			mu.Unlock()
			// agent_state=stopping is the frozen-awaiting-thaw state (a runsc agent
			// suspended by `gb stop`). The controller's thaw is name-keyed, so the
			// respawn MUST skip the pool (a pool pod has a different name) and fall
			// through to the same-name cold-resume mint that triggers the thaw
			// (ka-0000003838). The other terminal states (done/failed/
			// budget_exceeded) are not frozen and still use the warm pool fast path.
			if state == "stopping" {
				if wasCalled {
					t.Fatal("expected respawn of a frozen (stopping) agent to SKIP the pool for a same-name thaw")
				}
			} else if !wasCalled {
				t.Fatal("expected respawn to attempt pool assign for a terminal-state thread agent")
			}

			// Cold resume on the pool miss must mint exactly one fresh agent
			// bead with the resume spawn_source — proof we respawned rather
			// than posting the misleading nudge-failed warning.
			var resumeBeads []*beadsapi.BeadDetail
			for _, bead := range filterAgentBeads(daemon.beads) {
				if bead.Fields["spawn_source"] == "slack-thread-resume" {
					resumeBeads = append(resumeBeads, bead)
				}
			}
			if len(resumeBeads) != 1 {
				t.Fatalf("expected 1 cold-resume agent bead for terminal-state mention, got %d", len(resumeBeads))
			}
			if resumeBeads[0].Fields["agent"] != agentName {
				t.Errorf("resume bead agent = %q, want %q (same name for session continuity)",
					resumeBeads[0].Fields["agent"], agentName)
			}
		})
	}
}
