package main

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/spf13/cobra"
)

// boundChannel is a type=channel bead in land gardenio bound to Slack C123.
func boundChannel() map[string]map[string]any {
	return map[string]map[string]any{
		"C123": {
			"id":     "gd-CH1",
			"type":   "channel",
			"status": "open",
			"labels": []string{"project:gardenio"},
			"fields": map[string]string{"name": "general", "slack_channel_id": "C123"},
		},
	}
}

func runThreadSquawk(t *testing.T, mock *lineageMockServer) {
	t.Helper()
	clearLineageEnv(t)
	t.Setenv("SLACK_THREAD_CHANNEL", "C123")
	t.Setenv("SLACK_THREAD_TS", "1779836613.718519")
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	oldActor := actor
	actor = "me"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"progress update"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

// A squawk said in a Slack thread whose channel is bound to a channel bead
// carries channel_id and exactly that channel's land label, which kbeads
// requires of a message naming a channel (Commons I step 4).
func TestRunSquawk_StampsChannelID(t *testing.T) {
	mock := &lineageMockServer{channelBeads: boundChannel()}
	runThreadSquawk(t, mock)

	fields := mock.lastCreateFields()
	if fields["channel_id"] != "gd-CH1" {
		t.Errorf("squawk channel_id = %q, want gd-CH1 (fields=%v)", fields["channel_id"], fields)
	}
	labels := mock.lastCreateLabels()
	if !slices.Equal(labelLands(labels), []string{"gardenio"}) || !slices.Contains(labels, "squawk") {
		t.Errorf("squawk labels = %v, want squawk + exactly project:gardenio", labels)
	}
}

// An unbound Slack channel leaves the squawk exactly as before.
func TestRunSquawk_UnboundChannel_NoStamp(t *testing.T) {
	mock := &lineageMockServer{}
	runThreadSquawk(t, mock)

	if _, ok := mock.lastCreateFields()["channel_id"]; ok {
		t.Errorf("unbound channel must not stamp channel_id")
	}
	if lands := labelLands(mock.lastCreateLabels()); len(lands) != 0 {
		t.Errorf("unbound channel must not add a land label, got %v", lands)
	}
}

// A daemon refusal of the stamped squawk falls back to the unstamped one: the
// stamp must never cost the squawk.
func TestRunSquawk_RefusedStampFallsBack(t *testing.T) {
	mock := &lineageMockServer{channelBeads: boundChannel(), refuseStamped: true}
	runThreadSquawk(t, mock)

	mock.mu.Lock()
	creates := len(mock.createBodies)
	mock.mu.Unlock()
	if creates != 2 {
		t.Fatalf("creates = %d, want 2 (stamped refused, then unstamped)", creates)
	}
	if _, ok := mock.lastCreateFields()["channel_id"]; ok {
		t.Errorf("fallback create must not carry channel_id")
	}
	if lands := labelLands(mock.lastCreateLabels()); len(lands) != 0 {
		t.Errorf("fallback create must not carry the land label, got %v", lands)
	}
}

// agentCardMock is a daemon holding agent bead gd-AG1 with fields, and its
// agent card gd-CARD1 in #general (gd-CH1) of land gardenio.
func agentCardMock(agentFields map[string]string) *lineageMockServer {
	return &lineageMockServer{
		agentBeads: map[string]map[string]any{
			"gd-AG1": {"id": "gd-AG1", "type": "agent", "status": "open", "fields": agentFields},
		},
		cardBeads: []map[string]any{{
			"id": "gd-CARD1", "type": "message", "status": "closed",
			"labels": []string{"project:gardenio"},
			"fields": map[string]string{"kind": "card", "subject_id": "gd-AG1", "channel_id": "gd-CH1", "slack_channel": "C123"},
		}},
	}
}

// runAgentSquawk squawks as agent gd-AG1, which has no Slack thread.
func runAgentSquawk(t *testing.T, mock *lineageMockServer) {
	t.Helper()
	clearLineageEnv(t)
	t.Setenv("BOAT_AGENT_BEAD_ID", "gd-AG1")
	srv := httptest.NewServer(mock.handler(t))
	t.Cleanup(srv.Close)
	swapDaemon(t, srv.URL)
	oldActor := actor
	actor = "crew-1"
	t.Cleanup(func() { actor = oldActor })

	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	if err := runSquawk(cmd, []string{"progress update"}); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
}

// A squawk of an agent with no Slack thread is posted in Slack as a reply to
// its agent card, so in the Commons it is a reply in the card's thread: it
// carries the card as thread_id, the card's channel_id and the card's land
// (gd-fIM0zNikOd).
func TestRunSquawk_NonThreadAgent_StampsCardThread(t *testing.T) {
	mock := agentCardMock(map[string]string{"agent": "crew-1"})
	runAgentSquawk(t, mock)

	fields := mock.lastCreateFields()
	if fields["thread_id"] != "gd-CARD1" || fields["channel_id"] != "gd-CH1" {
		t.Errorf("squawk thread_id, channel_id = %q, %q, want gd-CARD1, gd-CH1 (fields=%v)", fields["thread_id"], fields["channel_id"], fields)
	}
	labels := mock.lastCreateLabels()
	if !slices.Equal(labelLands(labels), []string{"gardenio"}) || !slices.Contains(labels, "squawk") {
		t.Errorf("squawk labels = %v, want squawk + exactly project:gardenio", labels)
	}
	if len(mock.cardQueries) != 1 || !slices.Contains(mock.cardQueries[0], "kind=card") || !slices.Contains(mock.cardQueries[0], "subject_id=gd-AG1") {
		t.Errorf("card lookups = %v, want one by kind=card and subject_id=gd-AG1", mock.cardQueries)
	}
}

// The agent's home thread wins over its card (spec 4.1): an agent bead with a
// thread_id or a Slack thread binding is left to that thread, and its card is
// not looked up.
func TestRunSquawk_NonThreadAgent_OwnHomeThreadSkipsCard(t *testing.T) {
	for _, f := range []map[string]string{
		{"thread_id": "gd-WISH1"},
		{"slack_thread_bead_id": "gd-WISH1"},
		{"slack_thread_channel": "C9", "slack_thread_ts": "1.2"},
	} {
		mock := agentCardMock(f)
		runAgentSquawk(t, mock)
		fields := mock.lastCreateFields()
		if fields["thread_id"] != "" || fields["channel_id"] != "" || len(mock.cardQueries) != 0 {
			t.Errorf("agent fields %v: squawk fields %v, card lookups %v; want no card stamp", f, fields, mock.cardQueries)
		}
	}
}

// No card in the Commons yet (an early squawk, or a card in an unbound Slack
// channel, which has no channel_id) leaves the squawk unstamped; so does a
// card that is itself a reply.
func TestRunSquawk_NonThreadAgent_NoCommonsCard_NoStamp(t *testing.T) {
	for name, cards := range map[string][]map[string]any{
		"no card":       nil,
		"unbound card":  {{"id": "gd-CARD1", "type": "message", "labels": []string{"project:gardenio"}, "fields": map[string]string{"kind": "card", "subject_id": "gd-AG1"}}},
		"card is reply": {{"id": "gd-CARD1", "type": "message", "labels": []string{"project:gardenio"}, "fields": map[string]string{"kind": "card", "subject_id": "gd-AG1", "channel_id": "gd-CH1", "thread_id": "gd-T"}}},
	} {
		mock := agentCardMock(map[string]string{})
		mock.cardBeads = cards
		runAgentSquawk(t, mock)
		fields := mock.lastCreateFields()
		if fields["thread_id"] != "" || fields["channel_id"] != "" {
			t.Errorf("%s: squawk fields %v, want no stamp", name, fields)
		}
		if lands := labelLands(mock.lastCreateLabels()); len(lands) != 0 {
			t.Errorf("%s: squawk lands %v, want none", name, lands)
		}
	}
}

// A refused card-thread stamp falls back to the plain squawk, thread_id and
// all: the stamp must never cost the squawk.
func TestRunSquawk_NonThreadAgent_RefusedStampFallsBack(t *testing.T) {
	mock := agentCardMock(map[string]string{})
	mock.refuseStamped = true
	runAgentSquawk(t, mock)

	mock.mu.Lock()
	creates := len(mock.createBodies)
	mock.mu.Unlock()
	if creates != 2 {
		t.Fatalf("creates = %d, want 2 (stamped refused, then unstamped)", creates)
	}
	if fields := mock.lastCreateFields(); fields["thread_id"] != "" || fields["channel_id"] != "" {
		t.Errorf("fallback create carries a stamp: %v", fields)
	}
}

func TestLabelLands(t *testing.T) {
	got := labelLands([]string{"squawk", "project:a", "project:", "project:a", "project:b"})
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("labelLands = %v, want [a b]", got)
	}
}
