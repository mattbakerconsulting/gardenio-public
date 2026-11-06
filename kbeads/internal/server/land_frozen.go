package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// frozenLandDenial is the daemon's half of the Lands per-land kill switch
// (Track B spec §5.5, step 10.5): while a land's project bead has
// land_frozen=true, a scoped principal (a land member's OIDC bearer, or a
// land principal) writes nothing in it: no Commons message, no task, no
// spawn, and no comment, update, reaction, join or leave, field write, close,
// label, dependency, answer or approval either (gd-vV9NSBoND2). The Commons
// shows the members a banner instead of the composer; this is the fence
// behind it. The one write a freeze lets through, a person's post in the
// land's own #support, never reaches it (frozenSupportPost). Operators and
// the fleet are never scoped, so they still write (an operator says why in
// the land's #general). Reads are untouched.
//
// A per-agent token bound to a land (frozenLandAgent) is held to it too:
// the watcher's park takes a grace to reach a pod, and never reaches a laptop
// agent, so the daemon is the brake on what a frozen land's agents write
// (gd-qpFpWcWucD). Its own land is checked whatever the bead's labels say.
// It may still update and close a type=agent bead, which is what gb stop
// writes on its way out, though only toward stopping (frozenAgentPatchDenial);
// every other write, a comment or a decision included, is refused.
//
// authorizeWriteTitled runs it for every route that reaches write_policy;
// the routes that run scopedWriteDenial without it (tokenAuthorizedForBead,
// authorizeResolutionTarget, scopedApproverAllowed) and the targets that are
// not the bead written (a relabel's new labels, a dependency's other end)
// call it themselves. TestFrozenLandRefusesScopedWrites walks each route.
//
// A project lookup that fails for any reason but "no such project" refuses
// the write: the land might be frozen, and fail closed is the rule for the
// switch.
func frozenLandDenial(ctx context.Context, st store.Store, action string, beadType model.BeadType, labels []string) *AuthzDenial {
	switch agentLand, isAgent := frozenLandAgent(ctx); {
	case isScopedPrincipal(ctx):
	case isAgent:
		if beadType == typeAgent && (action == "update" || action == "close") {
			return nil
		}
		labels = append(slices.Clip(labels), "project:"+agentLand)
	default:
		return nil
	}
	checked := map[string]bool{}
	for _, l := range labels {
		land, ok := strings.CutPrefix(l, "project:")
		if !ok || land == "" || checked[land] {
			continue
		}
		checked[land] = true
		frozen, err := landFrozen(ctx, st, land)
		if err == nil && !frozen {
			continue
		}
		msg := "land " + land + " is frozen: nothing new can be written in it until the operator thaws it"
		if err != nil {
			msg = "land " + land + " could not be checked for a freeze, so the write is refused: try again"
		}
		return &AuthzDenial{Action: action, BeadType: string(beadType), RequiredScope: land, Message: msg}
	}
	return nil
}

// frozenLandAgent returns the land a per-agent token is bound to. ok is false
// for every other caller, and for an agent token bound to no project.
func frozenLandAgent(ctx context.Context) (land string, ok bool) {
	if _, agent := agentBeadIDFromContext(ctx); !agent {
		return "", false
	}
	land = TokenProjectFromContext(ctx)
	return land, land != ""
}

// frozenAgentLeaving are the agent states a frozen land's agent may still
// move an agent bead to: the ones gb stop and its close write.
var frozenAgentLeaving = map[string]bool{"stopping": true, "done": true, "failed": true}

// frozenAgentPatchDenial holds frozenLandDenial's gb stop exemption to
// leaving: in a frozen land, a patch by the land's agent token on an agent
// bead may move agent_state only to a frozenAgentLeaving state, and may not
// change agent or project. Those are what the purse watcher reads to find the
// agents it parks, so an agent that set itself paused, or blanked its name,
// would never be parked (gd-qpFpWcWucD). An echo of a stored value is not a
// change. A lookup that fails refuses, as frozenLandDenial does.
func frozenAgentPatchDenial(ctx context.Context, st store.Store, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	land, ok := frozenLandAgent(ctx)
	if !ok || bead == nil || bead.Type != typeAgent || len(patch) == 0 {
		return nil
	}
	pm, em := fieldsAsMap(patch), fieldsAsMap(bead.Fields)
	var changed []string
	for _, k := range frozenAgentWatchedFields {
		pv, set := pm[k]
		if !set || parkFieldText(pv) == parkFieldText(em[k]) || (k == "agent_state" && frozenAgentLeaving[parkFieldText(pv)]) {
			continue
		}
		changed = append(changed, k)
	}
	return frozenAgentDenial(ctx, st, land, changed)
}

// frozenAgentWatchedFields are the agent bead fields the purse watcher reads
// to find the agents it parks.
var frozenAgentWatchedFields = []string{"agent_state", "agent", "project"}

// frozenAgentFieldKeyDenial is frozenAgentPatchDenial for the per-field
// endpoints (edit, append, reorder, increment), which know the key and not the
// value it ends up with: in a frozen land, the land's agent token may not
// touch agent_state, agent or project on an agent bead at all. An append of
// paused to agent_state would leave it unparkable (gd-qpFpWcWucD). gb stop
// writes agent_state by PATCH, which frozenAgentPatchDenial lets through.
func frozenAgentFieldKeyDenial(ctx context.Context, st store.Store, bead *model.Bead, keys ...string) *AuthzDenial {
	land, ok := frozenLandAgent(ctx)
	if !ok || bead == nil || bead.Type != typeAgent {
		return nil
	}
	var changed []string
	for _, k := range keys {
		for _, w := range frozenAgentWatchedFields {
			if strings.EqualFold(k, w) {
				changed = append(changed, k)
			}
		}
	}
	return frozenAgentDenial(ctx, st, land, changed)
}

// authorizeAgentFieldEndpoint is the agent bead gate of the per-field
// endpoints: authorizeAgentBeadFieldWrite, then frozenAgentFieldKeyDenial. The
// close route runs only the first, since gb stop closes with agent_state done.
func (s *BeadsServer) authorizeAgentFieldEndpoint(ctx context.Context, bead *model.Bead, keys ...string) *AuthzDenial {
	if denial := s.authorizeAgentBeadFieldWrite(ctx, "update", bead, keys...); denial != nil {
		return denial
	}
	return frozenAgentFieldKeyDenial(ctx, s.store, bead, keys...)
}

// frozenAgentLabelDenial refuses the land's agent token a label change on an
// agent bead that leaves it without its project:<land> label while the land
// is frozen: the purse watcher lists a land's agents by that label, so an
// agent that dropped it would never be parked (gd-qpFpWcWucD). after is the
// bead's label set once the change lands.
func frozenAgentLabelDenial(ctx context.Context, st store.Store, bead *model.Bead, after []string) *AuthzDenial {
	land, ok := frozenLandAgent(ctx)
	if !ok || bead == nil || bead.Type != typeAgent {
		return nil
	}
	label := "project:" + land
	if !slices.Contains(bead.Labels, label) || slices.Contains(after, label) {
		return nil
	}
	return frozenAgentDenial(ctx, st, land, []string{"label " + label})
}

// labelsWithout is labels less every copy of label.
func labelsWithout(labels []string, label string) []string {
	return slices.DeleteFunc(slices.Clone(labels), func(l string) bool { return l == label })
}

// frozenAgentDenial refuses a change to what the purse watcher reads (changed,
// for the message) by an agent token bound to land, if the land is frozen or
// cannot be checked.
func frozenAgentDenial(ctx context.Context, st store.Store, land string, changed []string) *AuthzDenial {
	if len(changed) == 0 {
		return nil
	}
	frozen, err := landFrozen(ctx, st, land)
	switch {
	case err != nil:
		return &AuthzDenial{Action: "update", BeadType: string(typeAgent), RequiredScope: land,
			Message: "land " + land + " could not be checked for a freeze, so the write is refused: try again"}
	case !frozen:
		return nil
	}
	return &AuthzDenial{Action: "update", BeadType: string(typeAgent), RequiredScope: land,
		Message: "land " + land + " is frozen: its agents may only stop (agent_state stopping, done or failed), not change " + strings.Join(changed, ", ")}
}

// supportChannelName is the channel the Lands provisioner seeds in every
// tenant land for its people to reach the operator (provision.DefaultChannels,
// Track B spec §6.1).
const supportChannelName = "support"

// frozenSupportPost reports whether bead, about to be created, is the one
// write a freeze still takes from a land's people: a plain message in their
// frozen land's own #support. The Terms (§9.1) promise that a frozen land's
// members can still talk to us there, and the AUP (§6.2) has the operator ask
// the owner there (gd-mJw9pxK2wu). createBead then skips frozenLandDenial
// and, since nobody can join a channel during a freeze, #support's
// members-only gate (it is a user channel).
//
// Only a person (an OIDC caller) is let through: a frozen land's agents are
// to stop, so neither its land principal nor an agent token is. The message carries exactly one land, that
// land is frozen, and channel_id names a live channel called support in that
// same land. A card is not a post. Every other write, and any doubt (a lookup
// that fails), falls to frozenLandDenial as before.
func frozenSupportPost(ctx context.Context, st store.Store, bead *model.Bead) bool {
	if !isScopedPrincipal(ctx) || authMethodFromContext(ctx) != authMethodOIDC || bead.Type != typeMessage {
		return false
	}
	if decodeMessageCardFields(bead.Fields).Kind == messageKindCard {
		return false
	}
	lands := projectLabelValues(bead.Labels)
	chID := messageChannelID(bead.Fields)
	if len(lands) != 1 || chID == "" {
		return false
	}
	ch, err := st.GetBead(ctx, chID)
	if err != nil || ch == nil || ch.Type != typeChannel || ch.Status == model.StatusClosed {
		return false
	}
	if chLands := projectLabelValues(ch.Labels); len(chLands) != 1 || chLands[0] != lands[0] {
		return false
	}
	if readChannelFields(ch.Fields).Name != supportChannelName {
		return false
	}
	frozen, err := landFrozen(ctx, st, lands[0])
	return err == nil && frozen
}

// landFrozen reads land_frozen off the land's project bead. No project is
// not frozen. The field is a JSON boolean; a string "true" or "false" is
// honoured too. Fields that do not parse, or a land_frozen of any other shape
// (a number, an object, another string), are an error, which
// frozenLandDenial refuses on: the switch fails closed (gd-dMof1PW1oJ).
func landFrozen(ctx context.Context, st store.Store, land string) (bool, error) {
	p, err := st.FindProjectByName(ctx, land)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && p == nil:
		return false, nil
	case err != nil:
		return false, err
	}
	var f struct {
		Frozen any `json:"land_frozen"`
	}
	if len(p.Fields) > 0 {
		if err := json.Unmarshal(p.Fields, &f); err != nil {
			return false, fmt.Errorf("land %s: project fields: %w", land, err)
		}
	}
	switch v := f.Frozen.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	case string:
		switch strings.TrimSpace(v) {
		case "true":
			return true, nil
		case "false", "":
			return false, nil
		}
	}
	return false, fmt.Errorf("land %s: land_frozen is %T, not a boolean", land, f.Frozen)
}
