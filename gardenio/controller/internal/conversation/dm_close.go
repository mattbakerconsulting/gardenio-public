package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Closing a DM that is done with (gd-ZUywEoAvCi). A land has a DM for each
// agent that has written to a person, about 40 a day per person, and the
// Commons lists and reads every open channel. A DM closes when all three hold:
//
//   - every agent in it is gone: its agent bead is closed;
//   - its newest top-level entry is not a card whose subject (a decision,
//     permission or escalation) is still open;
//   - every person in it has read it: nothing someone else said is after
//     their read position (read_cursors on their user bead, the one the
//     Commons badge counts from).
//
// A closed DM keeps its history and its link. The same members writing again
// reopen it (EnsureDM); a successor agent has a name of its own, so its DM is
// a new one.

// DMSweepStore is the slice of the daemon client CloseIdleDMs reads and
// closes through. *beadsapi.Client satisfies it.
type DMSweepStore interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
	CloseBead(ctx context.Context, beadID string, fields map[string]string) error
}

const (
	// dmSweepMax caps the open DMs one sweep reads, across every land.
	dmSweepMax = 2000
	// dmLastWindow is how many of a DM's newest conversation beads (replies
	// included) are read to find its newest entries and the read position.
	dmLastWindow = 25
	// dmAgentMatches caps the agent beads read for one agent name.
	dmAgentMatches = 10
)

// DMCloser closes the DMs that are done with. It remembers the agents it has
// seen gone, which stay gone: an agent woken in place that writes again
// reopens its DM. Not safe for concurrent Sweeps.
type DMCloser struct {
	Store DMSweepStore
	gone  map[string]bool
}

// Sweep closes every open DM that is done with and returns how many it
// closed. A DM whose reads fail stays open; only a failed list of the DMs
// themselves is an error.
func (c *DMCloser) Sweep(ctx context.Context) (int, error) {
	res, err := c.Store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"channel"},
		Statuses: []string{"open"},
		Fields:   map[string]string{ChannelKind: ChannelKindDM},
		Limit:    dmSweepMax,
	})
	if err != nil {
		return 0, fmt.Errorf("conversation: list DMs: %w", err)
	}
	if c.gone == nil {
		c.gone = map[string]bool{}
	}
	s := dmSweep{DMCloser: c, cursors: map[string]map[string]string{}}
	closed := 0
	for _, b := range res.Beads {
		if ctx.Err() != nil {
			return closed, ctx.Err()
		}
		if !IsDM(b) || !s.done(ctx, b) {
			continue
		}
		if err := c.Store.CloseBead(ctx, b.ID, nil); err == nil {
			closed++
		}
	}
	return closed, nil
}

// dmSweep is one Sweep's reads: each person's read positions, read once.
type dmSweep struct {
	*DMCloser
	cursors map[string]map[string]string // by person principal; nil when unknown
}

// done reports whether the DM b may close.
func (s *dmSweep) done(ctx context.Context, b *beadsapi.BeadDetail) bool {
	members := MembersOf(b.Fields)
	var people []string
	for _, m := range members {
		if agent, ok := strings.CutPrefix(m, "agent:"); ok {
			if !s.agentGone(ctx, agent) {
				return false
			}
		} else if strings.HasPrefix(m, "oidc:") {
			people = append(people, m)
		}
	}
	if len(members) == len(people) {
		return false // no agent in it: a person's DM is theirs to keep
	}
	for _, p := range people {
		if s.cursorsOf(ctx, p)[b.ID] == "" {
			return false // never read in the Commons: everything in it is unread
		}
	}
	entries, ok := s.entries(ctx, b)
	if !ok {
		return false
	}
	if len(entries) > 0 && s.openCard(ctx, entries[0]) {
		return false
	}
	for _, p := range people {
		if !readTo(entries, p, s.cursorsOf(ctx, p)[b.ID]) {
			return false
		}
	}
	return true
}

// agentGone reports whether the agent's bead is closed. ref is its name, or
// its bead id (the DMs of 2026-10-21 named agents by bead id). An agent with
// no bead is not known to be gone.
func (s *dmSweep) agentGone(ctx context.Context, ref string) bool {
	if s.gone[ref] {
		return true
	}
	res, err := s.Store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"agent"},
		Fields: map[string]string{"agent": ref},
		Limit:  dmAgentMatches,
	})
	if err != nil {
		return false
	}
	beads := res.Beads
	if len(beads) == 0 {
		if b, err := s.Store.GetBead(ctx, ref); err == nil && b != nil && b.Type == "agent" {
			beads = append(beads, b)
		}
	}
	gone := len(beads) > 0 && len(beads) < dmAgentMatches
	for _, b := range beads {
		gone = gone && b != nil && b.Status == "closed"
	}
	if gone {
		s.gone[ref] = true
	}
	return gone
}

// cursorsOf returns the person's read positions by channel id, nil when they
// have no user bead or it cannot be read.
func (s *dmSweep) cursorsOf(ctx context.Context, person string) map[string]string {
	if cur, ok := s.cursors[person]; ok {
		return cur
	}
	var cur map[string]string
	res, err := s.Store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"user"},
		Fields: map[string]string{"email": strings.TrimPrefix(person, "oidc:")},
		Limit:  2,
	})
	if err == nil && len(res.Beads) == 1 && res.Beads[0] != nil {
		_ = json.Unmarshal([]byte(res.Beads[0].Fields["read_cursors"]), &cur)
	}
	s.cursors[person] = cur
	return cur
}

// dmEntry is one top-level entry of a DM.
type dmEntry struct {
	ID, Author, Kind, Subject string
}

// entries returns the DM's newest top-level entries, newest first, leaving
// out tombstones. ok is false when the read fails.
func (s *dmSweep) entries(ctx context.Context, b *beadsapi.BeadDetail) ([]dmEntry, bool) {
	res, err := s.Store.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"project:" + singleProjectLabel(b.Labels)},
		Fields: map[string]string{BeadChannelID: b.ID},
		Sort:   "-created_at",
		Limit:  dmLastWindow,
	})
	if err != nil {
		return nil, false
	}
	var top []*beadsapi.BeadDetail
	for _, m := range res.Beads {
		if m != nil && (m.Type == "message" || slices.Contains(m.Labels, "thread")) &&
			m.Fields[BeadThreadID] == "" && m.Fields["deleted"] != "true" {
			top = append(top, m)
		}
	}
	slices.SortStableFunc(top, func(x, y *beadsapi.BeadDetail) int { return y.CreatedAt.Compare(x.CreatedAt) })
	out := make([]dmEntry, len(top))
	for i, m := range top {
		out[i] = dmEntry{ID: m.ID, Author: m.Fields[BeadAuthor], Kind: m.Fields[BeadKind], Subject: m.Fields[BeadSubjectID]}
	}
	return out, true
}

// openCard reports whether e is a card whose subject is not closed. A subject
// that cannot be read counts as open.
func (s *dmSweep) openCard(ctx context.Context, e dmEntry) bool {
	if e.Kind != string(KindCard) || e.Subject == "" {
		return false
	}
	b, err := s.Store.GetBead(ctx, e.Subject)
	return err != nil || b == nil || b.Status != "closed"
}

// readTo reports whether person has read entries (newest first) to their
// read position cursor: everything before it is the person's own. A position
// older than the entries read is not known to cover them.
func readTo(entries []dmEntry, person, cursor string) bool {
	for _, e := range entries {
		if e.ID == cursor {
			return true
		}
		if !strings.EqualFold(e.Author, person) {
			return false
		}
	}
	return false
}
