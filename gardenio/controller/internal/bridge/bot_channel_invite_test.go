package bridge

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// This file backfills the gardener-on-invite spawn path
// (handleMemberJoinedChannel, bot_channel_invite.go) — the flow where inviting
// the gardenio app to a Slack channel spawns exactly one gardener bound to that
// channel. Coverage gap ka-0000006640: the create/archive/invite HTTP endpoints
// already have unit tests (bot_channels_manage_test.go), but the
// member_joined_channel → gardener spawn path had none.
//
// This is a Go integration test, NOT a shell e2e, on purpose:
// member_joined_channel is a Socket Mode (websocket) inbound event with no HTTP
// injection surface on the bridge, so a shell module against a deployed
// namespace cannot drive it — every slack-* shell module SKIP-passes without a
// live test-Slack rig. Driving handleMemberJoinedChannel directly against the
// newTestBot/newMockDaemon/newFakeSlackServer harness (as bot_mentions_e2e_test.go
// does for app_mention) is the only way to exercise the real spawn logic.

// newInviteTestBot builds a Bot wired for the gardener-on-invite path: a mock
// daemon seeded with `project` mapped to `channel`, a fake Slack server, a real
// on-disk StateManager (the durable channel→gardener binding lives there), the
// given botUserID, and gardenerOnInvite toggled per the argument.
func newInviteTestBot(t *testing.T, daemon *mockDaemon, botUserID string, gardenerOnInvite bool) *Bot {
	t.Helper()
	slackSrv := newFakeSlackServer(t)
	t.Cleanup(slackSrv.Close)

	state, err := NewStateManager(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("NewStateManager: %v", err)
	}

	b := newTestBot(daemon, slackSrv)
	b.state = state
	b.botUserID = botUserID
	b.gardenerOnInvite = gardenerOnInvite
	return b
}

// TestHandleMemberJoinedChannel_SpawnsGardenerBoundToChannel drives the happy
// path: the bot is invited to a project-mapped channel, GARDENER_ON_INVITE is on,
// and exactly one gardener is spawned, bound to the channel (SetThreadAgent with
// an empty thread_ts), with the operator stamped from the inviter and the
// spawn keyed invite:<channel>.
func TestHandleMemberJoinedChannel_SpawnsGardenerBoundToChannel(t *testing.T) {
	const (
		channel = "C-INVITE-1"
		botID   = "U-BOT"
		inviter = "U-INVITER"
	)

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)

	b := newInviteTestBot(t, daemon, botID, true)

	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User:    botID,
		Channel: channel,
		Inviter: inviter,
	})

	agentBeads := filterAgentBeads(daemon.beads)
	if len(agentBeads) != 1 {
		t.Fatalf("expected exactly 1 gardener spawned, got %d", len(agentBeads))
	}
	agent := agentBeads[0]

	// The gardener must be bound to project gardenio as a gardener (mode is
	// overwritten from the SpawnAgent default of crew to gardener).
	if got := agent.Fields["project"]; got != "gardenio" {
		t.Errorf("project = %q, want gardenio", got)
	}
	if got := agent.Fields["role"]; got != "gardener" {
		t.Errorf("role = %q, want gardener", got)
	}
	if got := agent.Fields["mode"]; got != "gardener" {
		t.Errorf("mode = %q, want gardener (overwritten from crew)", got)
	}
	if got := agent.Fields["spawn_source"]; got != "channel-invite" {
		t.Errorf("spawn_source = %q, want channel-invite", got)
	}
	if got := agent.Fields["slack_thread_channel"]; got != channel {
		t.Errorf("slack_thread_channel = %q, want %q", got, channel)
	}

	// Operator stamped from the inviter. The fake Slack server resolves no
	// email, so operatorFromSlackUser returns the slack:<user-id> sentinel.
	if got := agent.Fields["slack_user_id"]; got != inviter {
		t.Errorf("slack_user_id = %q, want %q", got, inviter)
	}
	if got := agent.Fields["operated_by"]; got != "slack:"+inviter {
		t.Errorf("operated_by = %q, want slack:%s", got, inviter)
	}

	// Channel-level binding: any top-level channel message must reach the
	// gardener, so the state binds on (channel, "").
	agentName := agent.Fields["agent"]
	if agentName == "" {
		t.Fatal("spawned gardener has no agent field")
	}
	if bound := b.getThreadSpawnedAgent(channel, ""); bound != agentName {
		t.Errorf("channel binding = %q, want %q", bound, agentName)
	}

	// The race guard is keyed invite:<channel> (ka-0000006640 repro).
	b.mu.Lock()
	inFlight := b.spawnInFlight["invite:"+channel]
	b.mu.Unlock()
	if !inFlight {
		t.Errorf("spawnInFlight[invite:%s] = false, want true (durable guard held)", channel)
	}
}

// TestHandleMemberJoinedChannel_IdempotentOnRedelivery asserts that a
// redelivered member_joined_channel event for the same channel spawns at most
// one gardener — the exact "idempotent on redelivery" contract.
func TestHandleMemberJoinedChannel_IdempotentOnRedelivery(t *testing.T) {
	const (
		channel = "C-INVITE-2"
		botID   = "U-BOT"
		inviter = "U-INVITER"
	)

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)

	b := newInviteTestBot(t, daemon, botID, true)

	ev := &slackevents.MemberJoinedChannelEvent{User: botID, Channel: channel, Inviter: inviter}
	b.handleMemberJoinedChannel(context.Background(), ev)
	// Redelivery of the identical event — must not spawn a second gardener.
	b.handleMemberJoinedChannel(context.Background(), ev)

	if n := len(filterAgentBeads(daemon.beads)); n != 1 {
		t.Fatalf("expected exactly 1 gardener across redelivery, got %d", n)
	}
}

// TestHandleMemberJoinedChannel_ExistingGardenerBound_NoSpawn exercises the
// durable guard: when a gardener is already bound to the channel in state, a
// fresh member_joined_channel event (e.g. after a bridge restart cleared the
// in-flight map) does not spawn a duplicate.
func TestHandleMemberJoinedChannel_ExistingGardenerBound_NoSpawn(t *testing.T) {
	const (
		channel = "C-INVITE-3"
		botID   = "U-BOT"
	)

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)

	b := newInviteTestBot(t, daemon, botID, true)
	// Pre-bind a gardener to the channel, as a prior invite would have.
	if err := b.state.SetThreadAgent(channel, "", "existing-gardener"); err != nil {
		t.Fatalf("SetThreadAgent: %v", err)
	}

	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User:    botID,
		Channel: channel,
		Inviter: "U-INVITER",
	})

	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("expected no new gardener when one is already bound, got %d", n)
	}
	// The pre-existing binding is left untouched.
	if bound := b.getThreadSpawnedAgent(channel, ""); bound != "existing-gardener" {
		t.Errorf("channel binding = %q, want existing-gardener (unchanged)", bound)
	}
}

// TestHandleMemberJoinedChannel_NonBotMember_NoSpawn asserts a human (or any
// non-bot) joining the channel never triggers a gardener spawn.
func TestHandleMemberJoinedChannel_NonBotMember_NoSpawn(t *testing.T) {
	const channel = "C-INVITE-4"

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)

	b := newInviteTestBot(t, daemon, "U-BOT", true)

	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User:    "U-SOMEONE-ELSE", // not the bot
		Channel: channel,
		Inviter: "U-INVITER",
	})

	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("expected no gardener for a non-bot join, got %d", n)
	}
}

// TestHandleMemberJoinedChannel_Disabled_NoSpawn asserts that with
// GARDENER_ON_INVITE off, inviting the bot is a no-op.
func TestHandleMemberJoinedChannel_Disabled_NoSpawn(t *testing.T) {
	const (
		channel = "C-INVITE-5"
		botID   = "U-BOT"
	)

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)

	b := newInviteTestBot(t, daemon, botID, false) // gardenerOnInvite disabled

	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User:    botID,
		Channel: channel,
		Inviter: "U-INVITER",
	})

	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("expected no gardener when GARDENER_ON_INVITE is disabled, got %d", n)
	}
}

// TestHandleMemberJoinedChannel_UnresolvableProject_NoSpawn asserts that when
// the channel maps to no project (and DM resolution can't pick one either — two
// candidate projects, neither claiming the channel), the handler declines to
// spawn and clears its in-flight guard so a later, correctly-mapped invite can
// still succeed.
func TestHandleMemberJoinedChannel_UnresolvableProject_NoSpawn(t *testing.T) {
	const (
		channel = "C-UNMAPPED"
		botID   = "U-BOT"
	)

	daemon := newMockDaemon()
	// Two projects, neither mapped to the channel → projectFromChannel returns
	// "" and resolveDMProject cannot disambiguate between the two.
	daemon.seedProjectWithChannel("gardenio", "C-OTHER-A")
	daemon.seedProjectWithChannel("monorepo", "C-OTHER-B")

	b := newInviteTestBot(t, daemon, botID, true)

	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User:    botID,
		Channel: channel,
		Inviter: "U-INVITER",
	})

	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("expected no gardener for an unresolvable channel, got %d", n)
	}
	// The in-flight guard must be cleared so a re-invite (after the channel is
	// mapped) is not permanently wedged.
	b.mu.Lock()
	inFlight := b.spawnInFlight["invite:"+channel]
	b.mu.Unlock()
	if inFlight {
		t.Errorf("spawnInFlight[invite:%s] = true after unresolvable spawn, want cleared", channel)
	}
}

// gd-VuYwGGYLLT: a channel invite has no prior agent to continue, so once
// gardener is privileged the daemon answers the create with an escalation
// (the invite asks for one via X-Escalate). That is not a failure: nothing is
// spawned, the in-flight guard is cleared so a re-invite works, and the
// channel hears that approval is pending.
func TestHandleMemberJoinedChannel_PrivilegedGardener_EscalatesNoSpawn(t *testing.T) {
	const (
		channel = "C-INVITE-ESC"
		botID   = "U-BOT"
	)

	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)
	daemon.spawnAgentErr = &beadsapi.EscalationCreated{EscalationID: "gd-esc1", Message: "escalation created: pending operator approval"}

	b := newInviteTestBot(t, daemon, botID, true)
	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User: botID, Channel: channel, Inviter: "U-INVITER",
	})

	if !beadsapi.EscalateFromContext(daemon.lastSpawnAgentCtx) {
		t.Error("the gardener-on-invite spawn must ask for X-Escalate")
	}
	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("an escalated invite must not create an agent, got %d", n)
	}
	if bound := b.getThreadSpawnedAgent(channel, ""); bound != "" {
		t.Errorf("channel must stay unbound until the gardener exists, got %q", bound)
	}
	b.mu.Lock()
	inFlight := b.spawnInFlight["invite:"+channel]
	b.mu.Unlock()
	if inFlight {
		t.Error("spawnInFlight must be cleared after an escalation so a re-invite can spawn")
	}
}

// A plain spawn failure still clears the guard and spawns nothing.
func TestHandleMemberJoinedChannel_SpawnFailure_NoSpawn(t *testing.T) {
	const channel = "C-INVITE-FAIL"
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("gardenio", channel)
	daemon.spawnAgentErr = errors.New("403 spawn denied")
	b := newInviteTestBot(t, daemon, "U-BOT", true)
	b.handleMemberJoinedChannel(context.Background(), &slackevents.MemberJoinedChannelEvent{
		User: "U-BOT", Channel: channel, Inviter: "U-INVITER",
	})
	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Fatalf("got %d agents", n)
	}
}
