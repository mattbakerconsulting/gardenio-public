package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack/slackevents"
)

// TestHandleAppMention_ThreadAgentStopRequested_Respawns covers the lame-duck
// shutdown-race (ka-0000002972) — the mirror image of the boot-race and of
// TestHandleAppMention_ThreadAgentNudgeConnFail_Respawns.
//
// A respond-then-stop thread agent has called `gb stop`, which stamps
// stop_requested="true" as its FIRST step, but the bead's agent_state has NOT
// yet flipped working→done (statusreporter lag). Crucially its coop_url is
// still LIVE — the pod has not finished tearing down — so a nudge WOULD be
// accepted and reported delivered. But the agent's turn is over: it will never
// process the nudge, and a respond-then-stop thread agent has no "next cycle".
//
// Before the fix NudgeAgent saw agent_state=working + a live coop_url, POSTed
// the nudge, got delivered:true, and returned success — so the bridge believed
// delivery worked and never respawned, silently dropping the message. This is
// exactly the real incident: Matt's "yep fix it" landed in that ~8s window and
// was lost. The fix classifies stop_requested="true" as ErrAgentNotNudgeable,
// which routes through respawnThreadForGoneNudge and
// mints a cold session-resume agent bead that re-injects the message.
func TestHandleAppMention_ThreadAgentStopRequested_Respawns(t *testing.T) {
	// A LIVE coop endpoint that would happily accept and "deliver" the nudge.
	// The whole point of this test vs the conn-fail sibling: the pod is still
	// reachable, so the ONLY thing that must trigger the respawn is the
	// stop_requested signal — not an unreachable coop_url.
	var nudged atomic.Int32
	coopSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nudged.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"delivered": true})
	}))
	defer coopSrv.Close()

	daemon := newMockDaemon()
	daemon.seedProjectWithChannelRole("monorepo", "C-LAMEDUCK", "thread")

	const agentName = "thread-9999-4444"
	// Active (open) agent bead reporting agent_state=working WITH a live
	// coop_url — resolves as live, so the mention path proceeds to nudge. But
	// stop_requested="true" means `gb stop` already ran: the turn is over.
	daemon.mu.Lock()
	daemon.beads[agentName] = &beadsapi.BeadDetail{
		ID:     "bd-agent-lameduck",
		Title:  agentName,
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":          agentName,
			"project":        "monorepo",
			"agent_state":    "working", // lag: not yet flipped to done
			"stop_requested": "true",    // gb stop already ran
		},
		Notes: "backend: coop\ncoop_url: " + coopSrv.URL,
	}
	daemon.mu.Unlock()

	dir := t.TempDir()
	sm, err := NewStateManager(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := sm.SetThreadAgent("C-LAMEDUCK", "1111.0000", agentName); err != nil {
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
		Text:            "<@U-BOT> yep fix it",
		TimeStamp:       "2222.3333",
		ThreadTimeStamp: "1111.0000",
		Channel:         "C-LAMEDUCK",
	})

	// The nudge must NOT have been delivered to the dying pod — the stop
	// signal short-circuits before the coop POST.
	if n := nudged.Load(); n != 0 {
		t.Errorf("expected 0 nudge deliveries to a stop-requested pod, got %d", n)
	}

	// The lame-duck agent must be respawned with a session-resume mint rather
	// than have its message dropped. A pool miss produces exactly one
	// cold-resume agent bead under the SAME name (session continuity).
	var resumeBeads []*beadsapi.BeadDetail
	for _, bead := range filterAgentBeads(daemon.beads) {
		if bead.Fields["spawn_source"] == "slack-thread-resume" {
			resumeBeads = append(resumeBeads, bead)
		}
	}
	if len(resumeBeads) != 1 {
		t.Fatalf("expected 1 cold-resume agent bead after a stop-requested nudge, got %d", len(resumeBeads))
	}
	if resumeBeads[0].Fields["agent"] != agentName {
		t.Errorf("resume bead agent = %q, want %q (same name for session continuity)",
			resumeBeads[0].Fields["agent"], agentName)
	}
}
