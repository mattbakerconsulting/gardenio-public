package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Commons channel_id dual-write for the message beads gb writes (spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-commons-design.md §2.3,
// §7.1 step 4). A squawk or recorded Slack reply said in a Slack channel that a
// type=channel bead is bound to (fields.slack_channel_id) carries that bead's
// id as fields.channel_id, which is what places it in the Commons. A squawk of
// an agent with no Slack thread is a reply in its agent card's thread there
// too (agentCardPlace), so it also carries the card bead as fields.thread_id.
//
// The stamp has to land at create time: the slack-bridge is an ordinary fleet
// token and cannot update a message an agent created (spec §2.3). kbeads requires a
// message that names a channel to carry exactly one project:<land> label equal
// to the channel's, so the channel's land label is added with the stamp, and a
// message already labelled with another land is not stamped.

// lookupChannelBead returns the id and land of the channel bead bound to a
// Slack channel, or ("", "") when none is bound or the lookup fails.
func lookupChannelBead(ctx context.Context, slackChannel string) (id, land string) {
	if daemon == nil || slackChannel == "" {
		return "", ""
	}
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"channel"},
		Fields: map[string]string{"slack_channel_id": slackChannel},
		Limit:  1,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to look up channel bead for %s: %v\n", slackChannel, err)
		return "", ""
	}
	if result == nil || len(result.Beads) == 0 {
		return "", ""
	}
	ch := result.Beads[0]
	if lands := labelLands(ch.Labels); len(lands) == 1 {
		return ch.ID, lands[0]
	}
	return "", ""
}

// labelLands returns every distinct non-empty project:<land> value on labels.
func labelLands(labels []string) []string {
	var lands []string
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" && !slices.Contains(lands, v) {
			lands = append(lands, v)
		}
	}
	return lands
}

// commonsPlace is where a message bead belongs in the Commons: a channel bead,
// that channel's land, and the thread head when the message is a reply there.
// The zero value is no place.
type commonsPlace struct {
	channel, land, thread string
}

// createMessageBead creates the message bead req describes with fields as its
// fields, stamped with channel_id when slackChannel is bound to a channel bead
// (createMessageBeadAt).
func createMessageBead(ctx context.Context, req beadsapi.CreateBeadRequest, fields map[string]string, slackChannel string) (string, error) {
	var at commonsPlace
	if id, land := lookupChannelBead(ctx, slackChannel); id != "" {
		at = commonsPlace{channel: id, land: land}
	}
	return createMessageBeadAt(ctx, req, fields, at)
}

// createMessageBeadAt creates the message bead req describes with fields as its
// fields, stamped with at: channel_id, and thread_id when at names a thread,
// unless the message is already labelled with another land. If the daemon
// refuses the stamped create with a 4xx (the binding changed under us), the
// message is created unstamped instead: the stamp must never cost the message.
func createMessageBeadAt(ctx context.Context, req beadsapi.CreateBeadRequest, fields map[string]string, at commonsPlace) (string, error) {
	plain, err := json.Marshal(fields)
	if err != nil {
		return "", fmt.Errorf("marshalling fields: %w", err)
	}
	req.Fields = plain

	stamped := req
	if id := at.channel; id != "" && at.land != "" {
		switch lands := labelLands(req.Labels); {
		case len(lands) == 0:
			stamped.Labels = append(slices.Clone(req.Labels), "project:"+at.land)
		case len(lands) == 1 && lands[0] == at.land:
		default:
			id = ""
		}
		if id != "" {
			withChannel := make(map[string]string, len(fields)+2)
			for k, v := range fields {
				withChannel[k] = v
			}
			withChannel["channel_id"] = id
			if at.thread != "" {
				withChannel["thread_id"] = at.thread
			}
			if j, err := json.Marshal(withChannel); err == nil {
				stamped.Fields = j
			}
		}
	}

	beadID, err := daemon.CreateBead(ctx, stamped)
	var apiErr *beadsapi.APIError
	if err != nil && string(stamped.Fields) != string(plain) &&
		errors.As(err, &apiErr) && apiErr.StatusCode >= http.StatusBadRequest && apiErr.StatusCode < http.StatusInternalServerError {
		fmt.Fprintf(os.Stderr, "warn: channel-stamped message refused (%v); writing it without channel_id\n", err)
		return daemon.CreateBead(ctx, req)
	}
	return beadID, err
}

// agentCardPlace returns where a squawk of an agent that is not bound to a
// Slack thread belongs in the Commons: a reply in its agent card's thread,
// which is where the bridge posts it in Slack (routeAgentThreadMessage). The
// card is the agent's newest top-level card bead (kind=card, subject_id = the
// agent bead) in a Commons channel. Zero when there is no agent bead, when the
// agent bead names a home thread of its own (thread_id, spec 4.1, or a Slack
// thread binding, which is where the bridge posts instead), or when no card is
// in the Commons yet: an early squawk can beat its card, and goes unstamped.
func agentCardPlace(ctx context.Context) commonsPlace {
	agentID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if agentID == "" {
		agentID = os.Getenv("KD_AGENT_ID")
	}
	if daemon == nil || agentID == "" {
		return commonsPlace{}
	}
	agent, err := daemon.GetBead(ctx, agentID)
	if err != nil || agent == nil {
		return commonsPlace{}
	}
	for _, k := range []string{"thread_id", "slack_thread_bead_id", "slack_thread_ts"} {
		if strings.TrimSpace(agent.Fields[k]) != "" {
			return commonsPlace{}
		}
	}
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Fields: map[string]string{"kind": "card", "subject_id": agent.ID},
		Sort:   "-created_at",
		Limit:  10,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: failed to look up the agent card of %s: %v\n", agent.ID, err)
		return commonsPlace{}
	}
	if res == nil {
		return commonsPlace{}
	}
	for _, card := range res.Beads {
		if card == nil || strings.TrimSpace(card.Fields["thread_id"]) != "" {
			continue // a card posted as a reply is no thread root
		}
		chID := strings.TrimSpace(card.Fields["channel_id"])
		if lands := labelLands(card.Labels); chID != "" && len(lands) == 1 {
			return commonsPlace{channel: chID, land: lands[0], thread: card.ID}
		}
	}
	return commonsPlace{}
}
