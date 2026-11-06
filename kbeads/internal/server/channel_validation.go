package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// Commons conversation beads (spec gardenio/docs/superpowers/specs/
// 2026-10-19-lands-commons-design.md §2.2, §6.2 rule 1).
//
// A land is a project, and land isolation is kbeads' project scope. The read
// cut for scoped principals is LabelsOrNoProject (read_authz_scope.go): a bead
// with NO project label is visible to every scoped principal, so a label-less
// channel or message would leak into every land. A bead with TWO project
// labels is visible in both lands. So every channel bead, and every message
// bead that names a channel (fields.channel_id), carries EXACTLY ONE
// project:<land> label, and a message's land equals its channel's. Two lands
// never share a Slack channel either: slack_channel_id is unique across every
// channel bead.
//
// Enforced at each write that can change those facts: createBead, updateBead
// (fields, labels or status), and the add/remove label routes on both
// transports. The checks read through the store, never the caller's scope, so a
// scoped caller cannot dodge a uniqueness check by not being able to see the
// clashing bead. The errors, though, follow the caller's scope (visible): a bead
// the caller cannot read is never named, nor is its land, and a channel_id the
// caller cannot read fails exactly like one that does not exist, so the error
// is no existence oracle across the wall. Admins and same-land callers keep the
// detailed text.

const typeChannel model.BeadType = "channel"

// channelNamePattern is the channel slug: Slack's own channel-name rules
// (lowercase letters, digits, '-' and '_', at most 80 characters), so a
// mirrored channel can keep its Slack name.
var channelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,79}$`)

// channelFields is the subset of type:channel fields the cross-field rules
// read. Per-field types and enum values are checked by the type config first.
type channelFields struct {
	Name           string   `json:"name"`
	ChannelKind    string   `json:"channel_kind"`
	SlackChannelID string   `json:"slack_channel_id"`
	Mirror         string   `json:"mirror"`
	Members        []string `json:"members"`
}

// channelKindMachines is a land's machine channel (steward amendment
// 2026-10-23 §2, single voice): agent, controller and bridge traffic kept out
// of #general. It is never mirrored to Slack, so it binds no Slack channel.
const channelKindMachines = "machines"

// projectLabelValues returns every non-empty project:<land> value on labels, in
// order, without duplicates.
func projectLabelValues(labels []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok && v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// singleLand returns the bead's one land, or an error naming the rule when it
// carries zero or several project labels.
func singleLand(what string, labels []string) (string, error) {
	lands := projectLabelValues(labels)
	switch len(lands) {
	case 1:
		return lands[0], nil
	case 0:
		return "", fmt.Errorf("%s needs exactly one project:<land> label, got none (a conversation bead with no project label is visible in every land)", what)
	default:
		return "", fmt.Errorf("%s needs exactly one project:<land> label, got %d (%s)", what, len(lands), strings.Join(lands, ", "))
	}
}

// messageChannelID returns fields.channel_id of a message bead ("" when unset).
func messageChannelID(fields json.RawMessage) string {
	if len(fields) == 0 {
		return ""
	}
	var f struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal(fields, &f); err != nil {
		return ""
	}
	return strings.TrimSpace(f.ChannelID)
}

// isLandBound reports whether the land rules apply to a bead of this type and
// fields: every channel, every message that names a channel, and every
// notification (notification.go).
func isLandBound(beadType model.BeadType, fields json.RawMessage) bool {
	switch beadType {
	case typeChannel:
		return true
	case "message":
		return messageChannelID(fields) != ""
	case typeNotification:
		return true
	}
	return false
}

// beadVisible reports whether the caller may read the bead with this id. The
// land rules use it only to decide how much an error may say.
type beadVisible func(id string) bool

// landErrorVisibility is the beadVisible the write paths pass: the same read
// gates a by-id fetch enforces (beadReadableByRequester). A lookup error counts
// as not visible, so it can only make an error say less.
func (s *BeadsServer) landErrorVisibility(ctx context.Context) beadVisible {
	return func(id string) bool {
		ok, err := s.beadReadableByRequester(ctx, id)
		return err == nil && ok
	}
}

// validateConversationLand runs the land rules for one bead in its post-write
// state. It is a no-op for beads the rules do not cover. bead.ID is the bead's
// own id (excluded from the uniqueness lookups) and may be empty on create.
func validateConversationLand(ctx context.Context, st store.Store, bead *model.Bead, visible beadVisible) error {
	switch bead.Type {
	case typeChannel:
		return validateChannel(ctx, st, bead, visible)
	case "message":
		if chID := messageChannelID(bead.Fields); chID != "" {
			return validateChannelMessage(ctx, st, bead.Labels, chID, visible)
		}
	case typeNotification:
		_, err := singleLand("a notification", bead.Labels)
		return err
	}
	return nil
}

func validateChannel(ctx context.Context, st store.Store, bead *model.Bead, visible beadVisible) error {
	land, err := singleLand("channel", bead.Labels)
	if err != nil {
		return err
	}
	var f channelFields
	if len(bead.Fields) > 0 {
		if err := json.Unmarshal(bead.Fields, &f); err != nil {
			// Generic field validation already rejects malformed JSON.
			return nil
		}
	}
	if !channelNamePattern.MatchString(f.Name) {
		return fmt.Errorf("channel name %q must be a slug: lowercase letters, digits, '-' or '_', starting with a letter or digit, at most 80 characters", f.Name)
	}
	if f.ChannelKind == "dm" && len(f.Members) < 2 {
		return fmt.Errorf("a dm channel needs members: at least two principals (oidc:<email> or agent:<name>)")
	}
	if len(f.Members) > 0 && !channelMembersOnly(f.ChannelKind) {
		return fmt.Errorf("members is only for channel_kind=user or dm (got channel_kind=%q); the land's default channels are open to everyone in it", f.ChannelKind)
	}
	if err := validateChannelMembers(f.ChannelKind, f.Members); err != nil {
		return err
	}
	if f.ChannelKind == channelKindMachines && (f.SlackChannelID != "" || (f.Mirror != "" && f.Mirror != "off")) {
		return fmt.Errorf("a machines channel is never mirrored to Slack: leave slack_channel_id unset and mirror unset or off")
	}

	// Name: unique among the land's live channels. Archiving frees the name,
	// and re-opening an archived channel checks it again.
	if bead.Status != model.StatusClosed {
		clash, err := findChannel(ctx, st, map[string]string{"name": f.Name}, []string{"project:" + land}, true, bead.ID)
		if err != nil {
			return err
		}
		if clash != "" && visible(clash) {
			return fmt.Errorf("land %q already has an open channel named %q (%s)", land, f.Name, clash)
		}
		if clash != "" {
			return fmt.Errorf("land %q already has an open channel named %q", land, f.Name)
		}
	}
	// slack_channel_id: unique across every channel bead in every land, open or
	// archived, so one Slack channel can never mirror into two lands (§6.2 rule
	// 2). Clear it on the old channel bead to re-bind.
	if f.SlackChannelID != "" {
		clash, err := findChannel(ctx, st, map[string]string{"slack_channel_id": f.SlackChannelID}, nil, false, bead.ID)
		if err != nil {
			return err
		}
		if clash != "" && visible(clash) {
			return fmt.Errorf("slack_channel_id %q is already bound to channel %s", f.SlackChannelID, clash)
		}
		if clash != "" {
			// The clash may be in another land: name neither it nor its land.
			return fmt.Errorf("slack_channel_id %q is already bound to another channel", f.SlackChannelID)
		}
	}
	return nil
}

// liveChannelStatuses are every status but closed: a channel is archived only
// by closing it.
var liveChannelStatuses = []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusDeferred, model.StatusBlocked, model.StatusPartial}

// validMemberPrincipal reports whether m is oidc:<email> or agent:<name>.
func validMemberPrincipal(m string) bool {
	for _, prefix := range []string{"oidc:", "agent:"} {
		if rest, ok := strings.CutPrefix(m, prefix); ok {
			return rest != "" && !strings.ContainsAny(rest, " \t\n,")
		}
	}
	return false
}

// findChannel returns the id of a channel bead other than selfID matching the
// field filter and labels, or "" when there is none.
func findChannel(ctx context.Context, st store.Store, fields map[string]string, labels []string, openOnly bool, selfID string) (string, error) {
	filter := model.BeadFilter{
		Type:   []model.BeadType{typeChannel},
		Fields: fields,
		Labels: labels,
		Fleet:  model.FleetAll,
		Limit:  2,
	}
	if openOnly {
		filter.Status = liveChannelStatuses
	}
	beads, _, err := st.ListBeads(ctx, filter)
	if err != nil {
		return "", fmt.Errorf("channel lookup failed: %w", err)
	}
	for _, b := range beads {
		if b.ID != selfID {
			return b.ID, nil
		}
	}
	return "", nil
}

func validateChannelMessage(ctx context.Context, st store.Store, labels []string, channelID string, visible beadVisible) error {
	land, err := singleLand("a message with channel_id", labels)
	if err != nil {
		return err
	}
	// A missing bead and one the caller cannot read get the same text, with no
	// id in it, so a writer cannot probe ids for which exist or which land owns
	// them.
	ch, err := st.GetBead(ctx, channelID)
	if err != nil || ch == nil || !visible(channelID) {
		return errors.New("channel_id is not a channel in this land")
	}
	if ch.Type != typeChannel {
		return fmt.Errorf("channel_id %q is not a channel bead", channelID)
	}
	if chLands := projectLabelValues(ch.Labels); len(chLands) != 1 || chLands[0] != land {
		return fmt.Errorf("message is in land %q but channel %s is in land %q", land, channelID, strings.Join(chLands, ", "))
	}
	return nil
}

// conversationLabelChangeError applies the land rules to a single label add or
// remove on an existing bead. Only a project: label can break them, so any
// other label (and any bead the rules do not cover) passes untouched.
func conversationLabelChangeError(ctx context.Context, st store.Store, bead *model.Bead, label string, adding bool) error {
	if bead == nil || !strings.HasPrefix(label, "project:") || !isLandBound(bead.Type, bead.Fields) {
		return nil
	}
	next := make([]string, 0, len(bead.Labels)+1)
	for _, l := range bead.Labels {
		if l != label {
			next = append(next, l)
		}
	}
	if adding {
		next = append(next, label)
	}
	what := "channel"
	switch bead.Type {
	case typeNotification:
		what = "a notification"
	case typeChannel:
	default:
		what = "a message with channel_id"
	}
	if _, err := singleLand(what, next); err != nil {
		return err
	}
	// Moving a bead to another land is a remove plus an add, and the first step
	// already fails the rule above, so the land itself never changes here.
	return nil
}

// channelLandChangeError refuses an update that moves a channel to another
// land. Its messages stay in the old land, so the move would split the
// channel across the wall.
func channelLandChangeError(prior, next []string) error {
	was, now := projectLabelValues(prior), projectLabelValues(next)
	if len(was) == 1 && len(now) == 1 && was[0] != now[0] {
		return fmt.Errorf("a channel cannot move between lands (%s to %s); create a channel in %s instead", was[0], now[0], now[0])
	}
	return nil
}
