package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The verbosity seam (gb-ux epic ka-0000005059) suppresses or collapses
// low-signal lifecycle chatter for simple-mode (non-vibe) recipients. It must
// NEVER touch human-actionable controls: a decision's resolve/dismiss/escalate
// buttons and an escalation's "Review in Web UI" button have to survive in
// EVERY mode, because a simple-mode operator still needs to act on them. These
// notifications are intentionally not vibe-gated in production today; the tests
// below run the same button assertions under BOTH a vibe-mode and a non-vibe
// owning operator so that a future change which naively extends the seam to
// gate them — stripping the buttons for simple-mode recipients — trips the
// non-vibe arm. (gb-ux #8, gd-8ZjOYHZPdC; hard rule from the intake brief.)

// seedSimpleOperatorForAgent seeds an agent bead stamped with an operator whose
// user bead carries no vibe_mode preference, so vibeModeForAgent(agentName)
// resolves the SIMPLE default (false). The mirror of enableVibeForAgent
// (vibe_mode_test.go) for the non-vibe arm.
func seedSimpleOperatorForAgent(daemon *mockDaemon, agentName string) {
	email := agentName + "@simple.example"
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID: "agent-" + agentName, Type: "agent", Status: "open", Title: agentName,
		Fields: map[string]string{"agent": agentName, "operated_by": "oidc:" + email},
	}
	daemon.beads["user-simple-"+agentName] = &beadsapi.BeadDetail{
		ID: "user-simple-" + agentName, Type: "user", Kind: "config", Status: "open",
		Fields: map[string]string{"email": email, "preferences": `{}`},
	}
}

// TestNotifyDecision_ButtonsIntactRegardlessOfVibeMode asserts a decision
// notification always carries its per-option resolve buttons plus the Dismiss
// and Escalate action buttons, whether the owning operator is in vibe or simple
// mode. The buttons live in the Block Kit "blocks" payload as button
// action_ids (resolve_<bead>_<n>, dismiss_decision, promote_decision), which
// the capturing server records verbatim.
func TestNotifyDecision_ButtonsIntactRegardlessOfVibeMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		vibe bool
	}{
		{"vibe-operator", true},
		{"simple-operator", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			srv, calls, mu := capturingSlackServer(t)
			defer srv.Close()
			bot := newTestBot(daemon, srv)
			bot.channel = "C-test"

			const agent = "dec-btn-agent"
			if tc.vibe {
				enableVibeForAgent(daemon, agent)
			} else {
				seedSimpleOperatorForAgent(daemon, agent)
			}

			opts, _ := json.Marshal([]string{"ship-it", "hold"})
			if err := bot.NotifyDecision(context.Background(), BeadEvent{
				ID:       "bd-dec-btn",
				Type:     "decision",
				Title:    "Ship or hold?",
				Priority: 2,
				Assignee: agent,
				Fields:   map[string]string{"prompt": "Ship or hold?", "options": string(opts)},
			}); err != nil {
				t.Fatalf("NotifyDecision: %v", err)
			}

			got := getCalls(calls, mu)
			if len(got) != 1 {
				t.Fatalf("decision must always post exactly once, got %d calls", len(got))
			}
			blocks := got[0].Blocks
			if blocks == "" {
				t.Fatal("decision must carry Block Kit blocks in every mode (buttons live there)")
			}
			// One resolve button per option, plus the always-present controls.
			for _, wantBtn := range []string{
				"resolve_bd-dec-btn_1", // option 1 (ship-it)
				"resolve_bd-dec-btn_2", // option 2 (hold)
				"resolve_other_bd-dec-btn",
				"dismiss_decision",
				"promote_decision",
			} {
				if !containsButton(blocks, wantBtn) {
					t.Errorf("decision blocks missing button %q in %s mode\nblocks=%s", wantBtn, tc.name, blocks)
				}
			}
		})
	}
}

// TestNotifyNewEscalation_ReviewButtonIntactRegardlessOfVibeMode asserts a new
// escalation notification always carries its "Review in Web UI" action button
// (action_id escalation_open_ui), whether the requesting agent's operator is in
// vibe or simple mode. The button only renders when beadsWebURL is configured
// (Slack rejects relative URLs), so the test sets it.
func TestNotifyNewEscalation_ReviewButtonIntactRegardlessOfVibeMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		vibe bool
	}{
		{"vibe-operator", true},
		{"simple-operator", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			srv, calls, mu := capturingSlackServer(t)
			defer srv.Close()
			bot := newTestBot(daemon, srv)
			bot.channel = "C-test"
			bot.beadsWebURL = "https://gardenio.example/beads" // absolute → button renders

			const agent = "esc-btn-agent"
			if tc.vibe {
				enableVibeForAgent(daemon, agent)
			} else {
				seedSimpleOperatorForAgent(daemon, agent)
			}

			if err := bot.NotifyNewEscalation(context.Background(), BeadEvent{
				ID:       "bd-esc-btn",
				Type:     "escalation",
				Title:    "Rotate the prod secret",
				Priority: 1,
				Fields: map[string]string{
					"operation_type":   "secret-rotate",
					"requesting_agent": agent,
				},
			}); err != nil {
				t.Fatalf("NotifyNewEscalation: %v", err)
			}

			got := getCalls(calls, mu)
			if len(got) != 1 {
				t.Fatalf("escalation must always post exactly once, got %d calls", len(got))
			}
			if got[0].Blocks == "" {
				t.Fatal("escalation must carry Block Kit blocks in every mode (button lives there)")
			}
			if !containsButton(got[0].Blocks, "escalation_open_ui") {
				t.Errorf("escalation blocks missing the Review-in-Web-UI button in %s mode\nblocks=%s", tc.name, got[0].Blocks)
			}
		})
	}
}

// containsButton reports whether the raw Slack "blocks" JSON carries a button
// with the given action_id. The slack-go marshaller emits it as the compact
// pair `"action_id":"<id>"`, so matching on the quoted key+value avoids a
// spurious hit on an unrelated field that merely contains the same substring.
func containsButton(blocksJSON, actionID string) bool {
	return strings.Contains(blocksJSON, `"action_id":"`+actionID+`"`)
}
