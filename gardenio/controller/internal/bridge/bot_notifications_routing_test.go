package bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Crash, jack and death notices land next to the agent's card (gd-Fzwyi0B3NT):
// a full project/role/name assignee is reduced to the short name the bridge's
// caches are keyed by, a known card's channel beats the current route, and a
// ListProjectBeads blip keeps serving the last good project channel cache.

const (
	routingCardChannel = "C-CARD"
	routingCardTS      = "1700000000.000001"
)

// newCardThreadingBot returns a bot with agent-card threading on and a card
// for short name "my-agent" in the hot cache at (C-CARD, routingCardTS).
func newCardThreadingBot(t *testing.T, daemon BeadClient) (*Bot, func() []slackCall) {
	t.Helper()
	srv, calls, mu := capturingSlackServer(t)
	t.Cleanup(srv.Close)
	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"
	bot.threadingMode = "agent"
	bot.agentCards["my-agent"] = MessageRef{ChannelID: routingCardChannel, Timestamp: routingCardTS}
	return bot, func() []slackCall { return getCalls(calls, mu) }
}

// assertThreadedUnderCard checks that exactly one message posted, as a reply
// under the agent's card in the card's channel (no second card was posted).
func assertThreadedUnderCard(t *testing.T, got []slackCall) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call (the notice, no new card), got %d: %+v", len(got), got)
	}
	if got[0].Channel != routingCardChannel || got[0].ThreadTS != routingCardTS {
		t.Errorf("expected reply in %s under %s, got channel=%q thread_ts=%q",
			routingCardChannel, routingCardTS, got[0].Channel, got[0].ThreadTS)
	}
}

func TestNotifyAgentCrash_FullPathAssignee_ThreadsUnderCard(t *testing.T) {
	bot, calls := newCardThreadingBot(t, newMockDaemon())

	if err := bot.NotifyAgentCrash(context.Background(), BeadEvent{
		ID:       "bd-crash-full",
		Assignee: "gardenio/crew/my-agent",
		Fields:   map[string]string{"agent_state": "failed"},
	}); err != nil {
		t.Fatalf("NotifyAgentCrash: %v", err)
	}
	assertThreadedUnderCard(t, calls())
}

func TestNotifyJackNotifiers_FullPathAssignee_ThreadUnderCard(t *testing.T) {
	notifiers := map[string]func(*Bot, context.Context, BeadEvent) error{
		"on":      (*Bot).NotifyJackOn,
		"off":     (*Bot).NotifyJackOff,
		"expired": (*Bot).NotifyJackExpired,
	}
	for name, notify := range notifiers {
		t.Run(name, func(t *testing.T) {
			daemon := newMockDaemon()
			enableVibeForAgent(daemon, "my-agent") // jack notices post only in vibe mode
			bot, calls := newCardThreadingBot(t, daemon)

			if err := notify(bot, context.Background(), BeadEvent{
				ID:       "bd-jack-full",
				Assignee: "gardenio/crew/my-agent",
				Fields:   map[string]string{"target": "prod-db"},
			}); err != nil {
				t.Fatalf("NotifyJack %s: %v", name, err)
			}
			assertThreadedUnderCard(t, calls())
		})
	}
}

func TestNotifyAgentDeath_FullPathAssignee_ThreadsUnderCard(t *testing.T) {
	bot, calls := newCardThreadingBot(t, newMockDaemon())

	if err := bot.NotifyAgentDeath(context.Background(), BeadEvent{
		ID:       "gd-dead-full",
		Assignee: "gardenio/crew/my-agent",
		Fields:   map[string]string{"close_reason": "oom_involuntary"},
	}); err != nil {
		t.Fatalf("NotifyAgentDeath: %v", err)
	}
	assertThreadedUnderCard(t, calls())
}

// A card posted before routing changed lives in a channel other than the one
// resolveChannel now returns; the threaded reply must follow the card.
func TestResolveThreadedChannel_PrefersCardChannelOverResolveChannel(t *testing.T) {
	bot, calls := newCardThreadingBot(t, newMockDaemon())
	bot.agentSpawnChannel["my-agent"] = "C-NEW-ROUTE"
	if got := bot.resolveChannel("my-agent"); got != "C-NEW-ROUTE" {
		t.Fatalf("test setup: resolveChannel = %q, want C-NEW-ROUTE", got)
	}

	ch, ts := bot.resolveThreadedChannel(context.Background(), "my-agent")
	if ch != routingCardChannel || ts != routingCardTS {
		t.Errorf("resolveThreadedChannel = (%q, %q), want card location (%s, %s)",
			ch, ts, routingCardChannel, routingCardTS)
	}
	if got := calls(); len(got) != 0 {
		t.Errorf("expected no Slack calls (no card moved or re-posted), got %d: %+v", len(got), got)
	}
}

// listProjectsErrDaemon fails ListProjectBeads while err is set.
type listProjectsErrDaemon struct {
	*mockDaemon
	err error
}

func (d *listProjectsErrDaemon) ListProjectBeads(ctx context.Context) (map[string]beadsapi.ProjectInfo, error) {
	if d.err != nil {
		return nil, d.err
	}
	return d.mockDaemon.ListProjectBeads(ctx)
}

func TestProjectPrimaryChannel_ListErrorServesWarmCache(t *testing.T) {
	inner := newMockDaemon()
	inner.seedProjectWithChannel("gardenio", "C-GARDENIO")
	daemon := &listProjectsErrDaemon{mockDaemon: inner}
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.channel = "C-DEFAULT"
	bot.agentProject["my-agent"] = "gardenio"

	if got := bot.projectPrimaryChannel("gardenio"); got != "C-GARDENIO" {
		t.Fatalf("warm-up: projectPrimaryChannel = %q, want C-GARDENIO", got)
	}

	// Expire the cache, then make the refresh fail.
	bot.mu.Lock()
	bot.projectChannelCacheAt = time.Now().Add(-2 * projectChannelCacheTTL)
	bot.mu.Unlock()
	daemon.err = errors.New("daemon unavailable")

	if got := bot.projectPrimaryChannel("gardenio"); got != "C-GARDENIO" {
		t.Errorf("projectPrimaryChannel on refresh error = %q, want stale C-GARDENIO", got)
	}
	if got := bot.resolveChannel("my-agent"); got != "C-GARDENIO" {
		t.Errorf("resolveChannel on refresh error = %q, want stale C-GARDENIO (not default)", got)
	}

	// Once the daemon recovers the cache refreshes again.
	daemon.err = nil
	inner.seedProjectWithChannel("gardenio", "C-GARDENIO-2")
	if got := bot.projectPrimaryChannel("gardenio"); got != "C-GARDENIO-2" {
		t.Errorf("projectPrimaryChannel after recovery = %q, want C-GARDENIO-2", got)
	}
}

func TestProjectPrimaryChannel_ListErrorColdCacheReturnsEmpty(t *testing.T) {
	daemon := &listProjectsErrDaemon{mockDaemon: newMockDaemon(), err: errors.New("daemon unavailable")}
	srv := newFakeSlackServer(t)
	defer srv.Close()
	bot := newTestBot(daemon, srv)

	if got := bot.projectPrimaryChannel("gardenio"); got != "" {
		t.Errorf("projectPrimaryChannel with cold cache and error = %q, want empty", got)
	}
}
