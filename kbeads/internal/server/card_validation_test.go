package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestCardCreate_OnePerSubjectAndChannel pins Lands spec §2.5: a card is unique
// per (subject_id, channel_id) and never re-posted, so a second create for the
// same pair returns the first card instead of inserting another.
func TestCardCreate_OnePerSubjectAndChannel(t *testing.T) {
	_, ms, h := newTestServer()
	land := []string{"project:a"}
	crew := createdID(t, createChannel(t, h, land, map[string]any{"name": "crew", "channel_kind": "crew"}))
	decisions := createdID(t, createChannel(t, h, land, map[string]any{"name": "decisions", "channel_kind": "decisions"}))

	card := map[string]any{"kind": "card", "subject_id": "gd-agent1", "channel_id": crew, "author": "agent:crew-1"}
	first := createdID(t, createMessage(t, h, land, card))
	again := createdID(t, createMessage(t, h, land, card))
	if again != first {
		t.Fatalf("second create for (gd-agent1, %s) = %s, want the existing card %s", crew, again, first)
	}

	// The same subject in another channel (a decision's DM-first copy) is a
	// second card; another subject in the same channel is too.
	if dm := createdID(t, createMessage(t, h, land, map[string]any{"kind": "card", "subject_id": "gd-agent1", "channel_id": decisions})); dm == first {
		t.Error("a card in another channel must be its own bead")
	}
	if other := createdID(t, createMessage(t, h, land, map[string]any{"kind": "card", "subject_id": "gd-agent2", "channel_id": crew})); other == first {
		t.Error("a card for another subject must be its own bead")
	}

	cards := 0
	for _, b := range ms.beads {
		if b.Type == "message" {
			cards++
		}
	}
	if cards != 3 {
		t.Errorf("message beads = %d, want 3 (one per (subject, channel))", cards)
	}
}

func TestCardCreate_FieldRules(t *testing.T) {
	_, _, h := newTestServer()
	land := []string{"project:a"}
	crew := createdID(t, createChannel(t, h, land, map[string]any{"name": "crew", "channel_kind": "crew"}))
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"card without subject", map[string]any{"kind": "card", "channel_id": crew}, "needs subject_id"},
		{"subject on text", map[string]any{"kind": "text", "subject_id": "gd-agent1", "channel_id": crew}, "only for kind=card"},
		{"subject without kind", map[string]any{"subject_id": "gd-agent1"}, "only for kind=card"},
		{"bad kind", map[string]any{"kind": "poster", "channel_id": crew}, "kind"},
		{"bad origin", map[string]any{"kind": "text", "origin": "email", "channel_id": crew}, "origin"},
		{"slack ref without kind", map[string]any{"text": "hi", "slack_channel": "C0CREW"}, "a channel-root notice names its Slack channel in `channel`"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			labels := land
			if _, ok := tc.fields["channel_id"]; !ok {
				labels = nil
			}
			requireRejected(t, createMessage(t, h, labels, tc.fields), tc.want)
		})
	}

	// A card with no channel bead yet (a land with no Commons) is keyed by its
	// Slack channel and needs no channel_id; the router's idempotency key
	// dedups it.
	createdID(t, createMessage(t, h, land, map[string]any{
		"kind": "card", "subject_id": "gd-agent1", "slack_channel": "C0CREW", "slack_ts": "1.2", "origin": "cli",
	}))
}

// TestCardUpdate_RefStampAndUniqueness: the router stamps the Slack ref back on
// the card (an update of the card itself, never a clash), and an update that
// would give a second card the same (subject, channel) is refused.
func TestCardUpdate_RefStampAndUniqueness(t *testing.T) {
	_, ms, h := newTestServer()
	land := []string{"project:a"}
	crew := createdID(t, createChannel(t, h, land, map[string]any{"name": "crew", "channel_kind": "crew"}))
	first := createdID(t, createMessage(t, h, land, map[string]any{"kind": "card", "subject_id": "gd-agent1", "channel_id": crew}))
	second := createdID(t, createMessage(t, h, land, map[string]any{"kind": "card", "subject_id": "gd-agent2", "channel_id": crew}))

	requireStatus(t, doJSON(t, h, "PATCH", "/v1/beads/"+first, map[string]any{
		"fields": map[string]any{"slack_channel": "C0CREW", "slack_ts": "1792393553.000129"},
	}), http.StatusOK)
	var f map[string]any
	if err := json.Unmarshal(ms.beads[first].Fields, &f); err != nil {
		t.Fatal(err)
	}
	if f["slack_ts"] != "1792393553.000129" || f["subject_id"] != "gd-agent1" {
		t.Errorf("card fields after ref stamp = %v", f)
	}

	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+second, map[string]any{
		"fields": map[string]any{"subject_id": "gd-agent1"},
	}), "already has a card for gd-agent1 ("+first+")")
	requireRejected(t, doJSON(t, h, "PATCH", "/v1/beads/"+second, map[string]any{
		"fields": map[string]any{"kind": "text"},
	}), "only for kind=card")
}

func TestBuiltinMessageDeclaresConversationFields(t *testing.T) {
	c, ok := builtinConfigs["type:message"]
	if !ok {
		t.Fatal(`builtinConfigs missing "type:message"`)
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:message: %v", err)
	}
	have := map[string]bool{}
	for _, f := range tc.Fields {
		have[f.Name] = true
	}
	// What the bridge's conversation router writes (Lands spec §2.3).
	for _, want := range []string{"channel_id", "thread_id", "kind", "subject_id", "author", "origin", "slack_channel", "slack_ts", "detail", "footnote", "files"} {
		if !have[want] {
			t.Errorf("type:message missing field %q (the conversation router writes it)", want)
		}
	}
}
