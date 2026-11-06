package commons

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// A permission answered on the Glass (Lands workbench-glass spec 3.3,
// gd-YqMRIwn3Xi). The Commons permission card and the Glass's prompt are one
// request seen twice: Claude Code shows its permission dialog in the terminal
// (the prompt the Glass answers through coop) and relays it to the agent's
// channel, whose callback the bridge posts as the card. Allow or Deny on the
// card is the channel's verdict, which Claude Code applies to the dialog, so
// the Glass's prompt goes with it. An answer on the Glass reaches the dialog
// only, so PermissionAnswered then has the bridge close the request with that
// answer and who gave it, and edit its Slack prompt to say so. The bridge
// sends the agent nothing more: it has its answer.

// cardActionAnswered is the bridge's card action for a permission answered
// elsewhere. No Commons button posts it (CardVerb has no entry for it); only
// PermissionAnswered sends it.
const cardActionAnswered = "answered"

// permissionsMax caps the open permission requests of one agent read to find
// the one the Glass answered.
const permissionsMax = 20

// ErrMayNotAnswer is PermissionAnswered's answer when the viewer's role in the
// request's land does not answer permissions.
var ErrMayNotAnswer = errors.New("commons: the viewer may not answer permissions in this land")

// PermissionAnswered closes agentID's open permission request for tool, the
// one the Glass answered on the viewer's behalf, through the bridge: behavior
// is allow, deny or "" (an answer that is neither), said the option's label.
// It returns the request bead, "" when there is none to close (no card for the
// prompt, a tool no open request names, or no way to the bridge). tool ""
// matches only when the agent has exactly one open request.
func (h *Handler) PermissionAnswered(r *http.Request, agentID, tool, behavior, said string) (string, error) {
	agentID = strings.TrimSpace(agentID)
	if h.Beads == nil || h.Inbound == nil || agentID == "" {
		return "", nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), cardTimeout)
	defer cancel()
	req, land, err := h.openPermission(ctx, h.scope(r), agentID, strings.TrimSpace(tool))
	if err != nil || req == nil {
		return "", err
	}
	card, err := cardOf(ctx, h.Beads, req.ID, land)
	if err != nil || card == "" {
		return "", err
	}
	if h.frozenFor(r, land) || h.Gate.MayAct == nil || !h.Gate.MayAct(r, land, CardVerb(cardKindAction, "permission", "allow")) {
		return "", ErrMayNotAnswer
	}
	if _, err := h.Inbound.Card(ctx, CardRequest{Kind: cardKindAction, Card: card, Action: cardActionAnswered,
		Values: map[string]string{"behavior": behavior, "said": clipBytes(strings.TrimSpace(said), 200)}}); err != nil {
		return "", err
	}
	return req.ID, nil
}

// openPermission is the newest open permission request of agent bead agentID
// for tool, in the agent's land, and that land; nil when there is none. A
// request is the agent's by its name in the land (the bridge records the name
// always, the agent's bead when it knew it), never one that records another
// agent's bead.
func (h *Handler) openPermission(ctx context.Context, sc Scope, agentID, tool string) (*beadsapi.BeadDetail, string, error) {
	a, err := h.Beads.GetBead(ctx, agentID)
	if err != nil {
		return nil, "", readErr("agent "+agentID, err)
	}
	land := ""
	if a != nil && a.Type == "agent" {
		land = landOf(a.Labels)
	}
	name := ""
	if a != nil {
		name = firstNonEmpty(strings.TrimSpace(a.Fields["agent"]), strings.TrimSpace(a.Title))
	}
	if !sc.Allows(land) || name == "" {
		return nil, "", nil
	}
	res, err := h.Beads.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"permission"},
		Statuses: []string{"open"},
		Labels:   []string{"project:" + land},
		Fields:   map[string]string{"agent": name},
		Limit:    permissionsMax,
	})
	if err != nil {
		return nil, "", readErr("permissions of "+agentID, err)
	}
	var open []*beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil || b.Type != "permission" || b.Status == "closed" || landOf(b.Labels) != land {
			continue
		}
		if id := strings.TrimSpace(b.Fields["agent_bead_id"]); id != "" && id != agentID {
			continue
		}
		open = append(open, b)
	}
	var got *beadsapi.BeadDetail
	switch {
	case tool == "" && len(open) == 1:
		got = open[0]
	case tool != "":
		for _, b := range open {
			if strings.TrimSpace(b.Fields["tool_name"]) == tool && (got == nil || b.CreatedAt.After(got.CreatedAt)) {
				got = b
			}
		}
	}
	if got == nil {
		return nil, "", nil
	}
	return got, land, nil
}

// cardOf is the card bead of subject in land, "" when it has none.
func cardOf(ctx context.Context, rd Reader, subject, land string) (string, error) {
	res, err := rd.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{typeMessage},
		Labels: []string{"project:" + land},
		Fields: map[string]string{fieldKind: kindCard, fieldSubjectID: subject},
		Limit:  5,
	})
	if err != nil {
		return "", readErr("cards of "+subject, err)
	}
	for _, b := range res.Beads {
		if b != nil && landOf(b.Labels) == land {
			return b.ID, nil
		}
	}
	return "", nil
}
