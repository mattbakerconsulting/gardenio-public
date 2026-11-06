package conversation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// DM channels (design section 2.4). A DM is a channel bead with
// channel_kind=dm and a members list fixed at create: two or more principals
// (oidc:<email>, agent:<name>). It belongs to one land, so a person in two
// lands has separate DMs in each and nothing crosses the wall. A DM is made on
// first use, by gb say --to and by the DM-first decision and escalation
// delivery, and found again by its name, which is derived from its members.

// ChannelKindDM is a DM's channel_kind.
const ChannelKindDM = "dm"

// ChannelMembers is a channel bead's members field.
const ChannelMembers = "members"

// BeadTo is a message bead's DM target (design section 2.3): the principal a
// gb say --to was sent to.
const BeadTo = "to"

// ErrDMMembers is returned for a DM of fewer than two distinct principals.
var ErrDMMembers = errors.New("conversation: a DM needs two or more distinct members")

// DMMembers returns principals sorted, with blanks and duplicates dropped:
// a DM's members in the order its name is made from.
func DMMembers(principals ...string) []string {
	var out []string
	for _, p := range principals {
		if p = strings.TrimSpace(p); p != "" && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// DMName is the channel name of the DM between members: "dm-" and a hash of
// them, so the same members always find the same channel and the slug names
// nobody. members is DMMembers' order.
func DMName(members []string) string {
	sum := sha256.Sum256([]byte(strings.Join(members, "\n")))
	return "dm-" + hex.EncodeToString(sum[:8])
}

// MembersOf reads a channel bead's members (a string[] the daemon client hands
// back as its JSON text).
func MembersOf(fields map[string]string) []string {
	return stringList(fields[ChannelMembers])
}

// IsDM reports whether b is a live DM channel bead.
func IsDM(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "channel" && b.Status != "closed" && b.Fields[ChannelKind] == ChannelKindDM
}

// DMStore is a Store that can reopen a closed DM. *beadsapi.Client satisfies
// it.
type DMStore interface {
	Store
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
}

// EnsureDM returns the open DM channel of land between principals, creating
// it with title as its display name when there is none. kbeads keeps a
// channel name unique among a land's open channels, so two racing creates
// leave one channel: the loser's create is refused and the lookup that
// follows finds the winner's. A DM closed once it was done with (DMCloser)
// is reopened, so the same members keep one channel and its history.
func EnsureDM(ctx context.Context, st DMStore, land, title string, principals ...string) (ChannelRef, error) {
	if land == "" {
		return ChannelRef{}, ErrNoLand
	}
	members := DMMembers(principals...)
	if len(members) < 2 {
		return ChannelRef{}, ErrDMMembers
	}
	name := DMName(members)
	if ch, ok, err := findDM(ctx, st, land, name, "open"); err != nil || ok {
		return ch, err
	}
	if ch, ok, err := findDM(ctx, st, land, name, "closed"); err == nil && ok {
		open := "open"
		if err := st.UpdateBead(ctx, ch.ID, beadsapi.UpdateBeadRequest{Status: &open}); err == nil {
			return ch, nil
		}
	}
	raw, _ := json.Marshal(map[string]any{ // strings and a []string always marshal
		ChannelName:    name,
		ChannelKind:    ChannelKindDM,
		ChannelMembers: members,
	})
	if strings.TrimSpace(title) == "" {
		title = strings.Join(members, ", ")
	}
	id, err := st.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:  title,
		Type:   "channel",
		Kind:   "data",
		Labels: []string{"project:" + land},
		Fields: raw,
	})
	if err != nil {
		if ch, ok, ferr := findDM(ctx, st, land, name, "open"); ferr == nil && ok {
			return ch, nil
		}
		return ChannelRef{}, fmt.Errorf("conversation: create DM %s in %s: %w", name, land, err)
	}
	return ChannelRef{ID: id, Land: land}, nil
}

// findDM returns the DM channel named name in land whose status is status.
func findDM(ctx context.Context, st Store, land, name, status string) (ChannelRef, bool, error) {
	res, err := st.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"channel"},
		Statuses: []string{status},
		Labels:   []string{"project:" + land},
		Fields:   map[string]string{ChannelName: name, ChannelKind: ChannelKindDM},
		Limit:    1,
	})
	if err != nil {
		return ChannelRef{}, false, fmt.Errorf("conversation: find DM %s in %s: %w", name, land, err)
	}
	if len(res.Beads) == 0 || res.Beads[0] == nil {
		return ChannelRef{}, false, nil
	}
	return ChannelFromBead(res.Beads[0], land), true, nil
}
