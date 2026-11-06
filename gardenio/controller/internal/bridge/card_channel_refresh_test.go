package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// Cold-cache card routing (gd-35LkF76wAr): a bridge that restarted, or missed
// an agent's spawn event, has no project, role or spawn channel cached for
// the agent. Decision and escalation cards must refresh the agent from its
// bead and land in its role or project channel, never the default channel.

const (
	coldDefaultCh = "C-DEFAULT"
	coldProjectCh = "C-GARDENIO"
	coldCrewCh    = "C-CREW"
)

// newColdRoutingFixture seeds a gardenio project whose channel_roles map the
// crew role to coldCrewCh, and returns a bot with empty routing caches and a
// Slack fake that records the channel of every chat.postMessage.
func newColdRoutingFixture(t *testing.T) (*Bot, *mockDaemon, func() []string) {
	t.Helper()
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", coldProjectCh, map[string]string{
		coldCrewCh: "crew",
	})

	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ch := ""
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			ch = r.FormValue("channel")
			mu.Lock()
			posted = append(posted, ch)
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": ch, "ts": "1700.0001"})
	}))
	t.Cleanup(srv.Close)

	bot := newTestBot(daemon, srv)
	bot.channel = coldDefaultCh
	return bot, daemon, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), posted...)
	}
}

func seedColdAgent(daemon *mockDaemon, key, name string, fields map[string]string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	f := map[string]string{"agent": name}
	for k, v := range fields {
		f[k] = v
	}
	daemon.beads[key] = &beadsapi.BeadDetail{
		ID:     "gd-" + name,
		Title:  name,
		Type:   "agent",
		Status: "open",
		Fields: f,
	}
}

func assertAllPostedTo(t *testing.T, posted []string, want string) {
	t.Helper()
	if len(posted) == 0 {
		t.Fatal("expected at least one chat.postMessage, got none")
	}
	for _, ch := range posted {
		if ch != want {
			t.Errorf("posted to %q, want every post in %q (all posts: %v)", ch, want, posted)
		}
	}
}

func TestNotifyDecision_ColdCache_PostsToRoleChannel(t *testing.T) {
	for _, mode := range []string{"", "agent"} {
		t.Run("threading="+mode, func(t *testing.T) {
			bot, daemon, posted := newColdRoutingFixture(t)
			bot.threadingMode = mode
			seedColdAgent(daemon, "crew-a", "crew-a", map[string]string{"project": "gardenio", "role": "crew"})

			if err := bot.NotifyDecision(context.Background(), BeadEvent{
				ID:       "dec-cold",
				Type:     "decision",
				Assignee: "crew-a",
				Fields:   map[string]string{"prompt": "Ship it?", "options": `["yes","no"]`},
			}); err != nil {
				t.Fatalf("NotifyDecision: %v", err)
			}
			// In agent threading mode this also covers the agent card the
			// decision threads under: it must not be created in the default
			// channel either.
			assertAllPostedTo(t, posted(), coldCrewCh)
		})
	}
}

// The requesting agent named by requesting_agent_bead_id, not the assignee,
// owns the card the decision threads under, so its channel is used.
func TestNotifyDecision_ColdCache_RequestingAgentChannel(t *testing.T) {
	bot, daemon, posted := newColdRoutingFixture(t)
	bot.threadingMode = "agent"
	seedColdAgent(daemon, "gd-crew-b", "crew-b", map[string]string{"project": "gardenio", "role": "crew"})

	if err := bot.NotifyDecision(context.Background(), BeadEvent{
		ID:       "dec-cold-req",
		Type:     "decision",
		Assignee: "some-user",
		Fields: map[string]string{
			"prompt":                   "Ship it?",
			"options":                  `["yes","no"]`,
			"requesting_agent_bead_id": "gd-crew-b",
		},
	}); err != nil {
		t.Fatalf("NotifyDecision: %v", err)
	}
	assertAllPostedTo(t, posted(), coldCrewCh)
}

func TestNotifyEscalation_ColdCache_PostsToProjectChannel(t *testing.T) {
	bot, daemon, posted := newColdRoutingFixture(t)
	// No channel_roles entry for this role: the project's primary channel.
	seedColdAgent(daemon, "builder-a", "builder-a", map[string]string{"project": "gardenio", "role": "builder"})

	if err := bot.NotifyEscalation(context.Background(), BeadEvent{
		ID:       "dec-esc-cold",
		Type:     "decision",
		Title:    "Urgent deploy",
		Assignee: "builder-a",
		Fields:   map[string]string{"prompt": "Deploy to prod?"},
	}); err != nil {
		t.Fatalf("NotifyEscalation: %v", err)
	}
	assertAllPostedTo(t, posted(), coldProjectCh)
}

func TestNotifyNewEscalation_ColdCache_PostsToRoleChannel(t *testing.T) {
	for _, mode := range []string{"", "agent"} {
		t.Run("threading="+mode, func(t *testing.T) {
			bot, daemon, posted := newColdRoutingFixture(t)
			bot.threadingMode = mode
			seedColdAgent(daemon, "crew-a", "crew-a", map[string]string{"project": "gardenio", "role": "crew"})

			if err := bot.NotifyNewEscalation(context.Background(), BeadEvent{
				ID:       "gd-esc-cold",
				Type:     "escalation",
				Title:    "Grant access",
				Priority: 1,
				Fields: map[string]string{
					"operation_type":   "iam_grant",
					"requesting_agent": "crew-a",
				},
			}); err != nil {
				t.Fatalf("NotifyNewEscalation: %v", err)
			}
			assertAllPostedTo(t, posted(), coldCrewCh)
		})
	}
}

// An escalation result can arrive after the requesting agent finished: its
// closed bead still routes the card.
func TestNotifyEscalationResult_ColdCache_ClosedAgentBead(t *testing.T) {
	bot, daemon, posted := newColdRoutingFixture(t)
	daemon.enforceActiveStatusScoping = true
	seedColdAgent(daemon, "crew-a", "crew-a", map[string]string{"project": "gardenio", "role": "crew"})
	daemon.beads["crew-a"].Status = "closed"

	if err := bot.NotifyEscalationResult(context.Background(), BeadEvent{
		ID:    "gd-esc-res-cold",
		Type:  "escalation",
		Title: "Run migration",
		Fields: map[string]string{
			"result_status":    "success",
			"requesting_agent": "crew-a",
		},
	}); err != nil {
		t.Fatalf("NotifyEscalationResult: %v", err)
	}
	assertAllPostedTo(t, posted(), coldCrewCh)
}

func TestNotifyEscalationPingback_ColdCache_PostsToRoleChannel(t *testing.T) {
	bot, daemon, posted := newColdRoutingFixture(t)
	seedColdAgent(daemon, "crew-a", "crew-a", map[string]string{"project": "gardenio", "role": "crew"})

	if err := bot.NotifyEscalationPingback(context.Background(), BeadEvent{
		ID:    "gd-esc-ping-cold",
		Type:  "escalation",
		Title: "Grant access",
		Fields: map[string]string{
			"pingback_count":   "1",
			"requesting_agent": "crew-a",
		},
	}); err != nil {
		t.Fatalf("NotifyEscalationPingback: %v", err)
	}
	assertAllPostedTo(t, posted(), coldCrewCh)
}

// Only an agent whose bead has no project goes to the default channel.
func TestResolveCardChannel_NoProject_DefaultChannel(t *testing.T) {
	bot, daemon, _ := newColdRoutingFixture(t)
	seedColdAgent(daemon, "loner", "loner", map[string]string{"role": "crew"})

	if got := bot.resolveCardChannel(context.Background(), "loner", "decision"); got != coldDefaultCh {
		t.Errorf("resolveCardChannel = %q, want default %q", got, coldDefaultCh)
	}
	// Unknown agent: no bead at all, also the default channel.
	if got := bot.resolveCardChannel(context.Background(), "ghost", "decision"); got != coldDefaultCh {
		t.Errorf("resolveCardChannel(ghost) = %q, want default %q", got, coldDefaultCh)
	}
}

// A warm cache is not refreshed: no daemon agent lookup per card.
func TestResolveCardChannel_WarmCache_NoRefresh(t *testing.T) {
	bot, daemon, _ := newColdRoutingFixture(t)
	bot.agentProject["crew-a"] = "gardenio"
	bot.agentRole["crew-a"] = "crew"

	if got := bot.resolveCardChannel(context.Background(), "crew-a", "decision"); got != coldCrewCh {
		t.Errorf("resolveCardChannel = %q, want %q", got, coldCrewCh)
	}
	daemon.mu.Lock()
	calls := daemon.getCalls
	daemon.mu.Unlock()
	if calls != 0 {
		t.Errorf("warm cache made %d agent bead lookups, want 0", calls)
	}
}

// A schedule's channel override, read from the bead on refresh, wins as it
// does on the spawn path.
func TestResolveCardChannel_ColdCache_ScheduleChannel(t *testing.T) {
	bot, daemon, _ := newColdRoutingFixture(t)
	seedColdAgent(daemon, "nightly", "nightly", map[string]string{
		"project":                "gardenio",
		"role":                   "crew",
		"slack_spawn_channel":    "C-SPAWN",
		"schedule_slack_channel": "C-SCHEDULE",
	})

	if got := bot.resolveCardChannel(context.Background(), "nightly", "escalation"); got != "C-SCHEDULE" {
		t.Errorf("resolveCardChannel = %q, want C-SCHEDULE", got)
	}
}
