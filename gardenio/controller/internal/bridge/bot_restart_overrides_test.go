package bridge

import (
	"path/filepath"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack"
)

// The "Restart Agent" button is the THIRD spawn path (alongside handleThreadSpawn
// and respawnThreadAgent) and originally carried NONE of the per-spawn overrides
// (ka-0000008253). It re-mints a fresh agent bead from a hand-built fields map,
// so any field not explicitly carried is silently lost and the new agent falls
// back to the role/project default.
//
// That made the shipped --backend=microvm flag (gd-efZC6yErfl) leaky in a way
// that is hard to notice: a thread spawned on the MicroVM backend with ultracode
// on would come back as a plain K8s pod with ultracode OFF after one restart,
// with no warning anywhere.

// restartWithPriorFields seeds a pre-restart agent bead carrying the given
// override fields, clicks Restart, and returns the freshly minted agent bead.
func restartWithPriorFields(t *testing.T, priorFields map[string]string) *beadsapi.BeadDetail {
	t.Helper()

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", "C-restart-ovr")
	// t.Cleanup, not defer: handleRestartThreadAgent posts from a goroutine that
	// can still be running when this helper returns, so the server must outlive
	// the helper and be torn down at test end.
	slackSrv := newFakeSlackServer(t)
	t.Cleanup(slackSrv.Close)

	bot := newTestBot(daemon, slackSrv)

	dir := t.TempDir()
	state, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("failed to create state manager: %v", err)
	}
	bot.state = state

	channel := "C-restart-ovr"
	threadTS := "1700000000.777777"
	agentName := "thread-1700000000-777777"

	fields := map[string]string{
		"agent":                agentName,
		"project":              "gardenio",
		"role":                 "thread",
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
		"slack_user_id":        "U-OPENER",
	}
	for k, v := range priorFields {
		fields[k] = v
	}

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
		Fields:     fields,
	}
	daemon.mu.Unlock()

	callback := slack.InteractionCallback{User: slack.User{ID: "U-CLICKER"}}
	callback.Channel.GroupConversation.Conversation.ID = channel
	callback.Container.ThreadTs = threadTS

	bot.handleRestartThreadAgent(asOpener(), agentName, callback)

	// The handler works in a goroutine; poll for the re-minted bead (a new id,
	// distinct from the seeded bd-existing).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		daemon.mu.Lock()
		for _, b := range daemon.beads {
			if b.Type == "agent" && b.Title == agentName && b.ID != "bd-existing" {
				daemon.mu.Unlock()
				return b
			}
		}
		daemon.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected handleRestartThreadAgent to create a new agent bead")
	return nil
}

// TestHandleRestartThreadAgent_CarriesMicrovmBackendAndUltracode is the key
// regression for the --backend=microvm flag: restarting a MicroVM thread must
// keep it on the MicroVM backend AND keep ultracode on. Without the carry, the
// re-mint drops execution_target (resolveExecutionTarget then falls back to the
// role/project default, moving the agent onto a K8s pod) and drops ultracode
// (applyUltracode finds no agent-layer value, so BOAT_ULTRACODE falls back to
// the project/role ultracode config — unset here — and workflows+subagents
// silently switch off).
func TestHandleRestartThreadAgent_CarriesMicrovmBackendAndUltracode(t *testing.T) {
	bead := restartWithPriorFields(t, map[string]string{
		"execution_target": podmanager.ExecutionTargetLambdaMicroVM,
		"ultracode":        "on",
	})

	if got := bead.Fields["execution_target"]; got != podmanager.ExecutionTargetLambdaMicroVM {
		t.Errorf("restart agent fields.execution_target = %q, want %q (a --backend=microvm thread must stay on the MicroVM backend across a restart)",
			got, podmanager.ExecutionTargetLambdaMicroVM)
	}
	if got := bead.Fields["ultracode"]; got != "on" {
		t.Errorf("restart agent fields.ultracode = %q, want \"on\" (restart must not silently disable workflows+subagents)", got)
	}
}

// TestHandleRestartThreadAgent_CarriesModelAndExplicitUltracodeOff covers the
// other two carried values, including the tri-state trap: a prior "off" must
// survive too. If restart treated missing-as-default it would silently re-enable
// ultracode on a thread the operator explicitly turned it off for.
func TestHandleRestartThreadAgent_CarriesModelAndExplicitUltracodeOff(t *testing.T) {
	bead := restartWithPriorFields(t, map[string]string{
		"model":     "fable",
		"ultracode": "off",
	})

	if got := bead.Fields["model"]; got != "fable" {
		t.Errorf("restart agent fields.model = %q, want \"fable\" (a --model thread must not revert to the default model on restart)", got)
	}
	if got := bead.Fields["ultracode"]; got != "off" {
		t.Errorf("restart agent fields.ultracode = %q, want \"off\" (an explicit off must survive a restart)", got)
	}
}

// TestHandleRestartThreadAgent_OmitsAbsentOverrides locks in the
// empty-means-inherit contract shared with the spawn/respawn paths: a thread
// that never set an override must NOT have the key stamped, so the
// role/project default still applies (e.g. the gardenio-microvms ROLE bead's
// execution_target). Stamping "" would pin the agent to an empty target.
func TestHandleRestartThreadAgent_OmitsAbsentOverrides(t *testing.T) {
	bead := restartWithPriorFields(t, nil)

	for _, key := range []string{"execution_target", "ultracode", "model"} {
		if v, ok := bead.Fields[key]; ok {
			t.Errorf("restart agent fields[%q] = %q, want the key ABSENT so the role/project default still applies", key, v)
		}
	}
}
