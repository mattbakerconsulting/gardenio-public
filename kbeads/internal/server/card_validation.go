package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Commons cards (spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-commons-design.md §2.3, §2.5).
//
// A card is a closed type=message bead with kind=card and subject_id: the bead
// it renders (an agent, decision, escalation or permission request). There is
// one card per (subject_id, channel_id), and it is never re-posted: a change to
// the subject re-renders the existing card in place (D1). So a create that
// names a (subject, channel) that already has a card returns that card, the
// lookup-or-return shape type=merge-request and type=rating use, and an update
// that would give a second card the same pair is refused.
//
// A card with no channel_id (a land with no Commons yet, keyed by its Slack
// channel) is not covered by the uniqueness rule; the bridge's router keys
// those by idempotency key.

const (
	messageKindCard = "card"

	messageKindField    = "kind"
	messageSubjectField = "subject_id"
)

// messageCardFields is the subset of type:message fields the card rules read.
type messageCardFields struct {
	Kind         string `json:"kind"`
	SubjectID    string `json:"subject_id"`
	ChannelID    string `json:"channel_id"`
	SlackChannel string `json:"slack_channel"`
	SlackTS      string `json:"slack_ts"`
}

func decodeMessageCardFields(fields json.RawMessage) messageCardFields {
	var f messageCardFields
	if len(fields) > 0 {
		// Generic field validation already rejects malformed JSON.
		_ = json.Unmarshal(fields, &f)
	}
	f.Kind = strings.TrimSpace(f.Kind)
	f.SubjectID = strings.TrimSpace(f.SubjectID)
	f.ChannelID = strings.TrimSpace(f.ChannelID)
	return f
}

// validateMessageCardFields checks the per-bead card rules on a type=message
// bead's post-write fields: subject_id is set exactly when kind=card, and the
// Slack ref (slack_channel / slack_ts) is only on a conversation message, one
// that has a kind. The last rule keeps the ka-0000002479 seam: a channel-root
// notice names its channel in `channel`, and a bare {text, slack_channel} is
// still refused instead of being silently ignored by the Notice watcher.
func validateMessageCardFields(fields json.RawMessage) error {
	f := decodeMessageCardFields(fields)
	if f.Kind == messageKindCard && f.SubjectID == "" {
		return fmt.Errorf("a kind=card message needs subject_id: the bead the card renders")
	}
	if f.SubjectID != "" && f.Kind != messageKindCard {
		return fmt.Errorf("subject_id is only for kind=card messages (got kind=%q)", f.Kind)
	}
	if (f.SlackChannel != "" || f.SlackTS != "") && f.Kind == "" {
		return fmt.Errorf("slack_channel/slack_ts are the Slack ref of a conversation message and need a kind; a channel-root notice names its Slack channel in `channel`")
	}
	return nil
}

// cardKey returns the (subject_id, channel_id) of a card bead that the
// uniqueness rule covers, and ok=false for any other bead.
func cardKey(beadType model.BeadType, fields json.RawMessage) (subjectID, channelID string, ok bool) {
	if beadType != "message" {
		return "", "", false
	}
	f := decodeMessageCardFields(fields)
	if f.Kind != messageKindCard || f.SubjectID == "" || f.ChannelID == "" {
		return "", "", false
	}
	return f.SubjectID, f.ChannelID, true
}

// findCard returns the card bead other than selfID for (subjectID, channelID),
// or nil. It reads through the store, never the caller's scope, like the
// channel uniqueness lookups.
func findCard(ctx context.Context, st store.Store, subjectID, channelID, selfID string) (*model.Bead, error) {
	beads, _, err := st.ListBeads(ctx, model.BeadFilter{
		Type: []model.BeadType{"message"},
		Fields: map[string]string{
			messageKindField:    messageKindCard,
			messageSubjectField: subjectID,
			"channel_id":        channelID,
		},
		Fleet: model.FleetAll,
		Limit: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("card lookup failed: %w", err)
	}
	for _, b := range beads {
		if b.ID != selfID {
			return b, nil
		}
	}
	return nil, nil
}

// cardUpdateError refuses an update that would give a second card the same
// (subject_id, channel_id).
func cardUpdateError(ctx context.Context, st store.Store, bead *model.Bead) error {
	subjectID, channelID, ok := cardKey(bead.Type, bead.Fields)
	if !ok {
		return nil
	}
	clash, err := findCard(ctx, st, subjectID, channelID, bead.ID)
	if err != nil {
		return err
	}
	if clash != nil {
		return fmt.Errorf("channel %s already has a card for %s (%s); a card is never re-posted, update that one", channelID, subjectID, clash.ID)
	}
	return nil
}
