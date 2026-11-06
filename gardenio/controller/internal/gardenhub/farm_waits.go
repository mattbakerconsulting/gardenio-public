package gardenhub

// farm_waits.go — what an open decision or escalation waits on: the agent
// references a wait can name, the live-agent roster that says whether each is
// still around, and the FarmWait projection (moved out of farm.go).

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// beadRefRe matches a bead id. requesting_agent on an escalation (and
// requesting_agent_bead_id on a decision) names the agent either by name or by
// its agent bead id; an id is resolved to a name through BatchGetBeads.
var beadRefRe = regexp.MustCompile(`^(gd|ka)-[A-Za-z0-9]{6,}$`)

// syntheticExecRe matches the name the escalation executor is spawned under
// ("escalation-<lowercased id>"). It is an agent, never a person to show.
var syntheticExecRe = regexp.MustCompile(`(?i)^escalation-(gd|ka)-[a-z0-9]{6,}$`)

// deadAgentStates are the agent_state values of an agent that is finished or
// going away even while its bead is still open (bridge deadAgentStates).
var deadAgentStates = map[string]bool{"done": true, "failed": true, "stopping": true, "budget_exceeded": true}

// agentRef is what one agent reference resolved to when it was read.
type agentRef struct {
	name  string
	agent bool // an agent bead backs it
	done  bool // that bead was closed or in a dead agent_state when read
}

// agentRoster is the live agents: open or in_progress agent beads not in a
// dead agent_state, by bead id and by name.
type agentRoster struct {
	at       time.Time // last good read
	complete bool      // the read was not truncated
	ids      map[string]bool
	names    map[string]bool
}

// Liveness of one party a wait could name.
const (
	partyPerson = iota // not a known agent (a person, an unresolved id) or not checkable: name it
	partyLive          // a live agent
	partyGone          // a finished agent, and the roster is fresh: skip it
	partyDone          // a finished agent, but the roster is not fresh: name it, as finished
)

// agentRefs are the fields, in order of preference, that may name who a
// non-human wait is on: for an escalation the executor first, then the
// assignee, then the agent that asked; for a decision the agent that asked.
func agentRefs(b *beadsapi.BeadDetail) []string {
	f := b.Fields
	if b.Type == "decision" {
		return []string{f["requesting_agent_bead_id"], f["requested_by"], b.CreatedBy}
	}
	return []string{f["executor"], b.Assignee, f["requesting_agent"], b.CreatedBy}
}

// resolveAgents reads what the farm needs to say who asks wait on: the agent
// beads named by id that are not cached yet and the agent names not yet looked
// up (the roster comes from the fast tier's agent read). A failed read leaves
// those refs unresolved (shown as they are) for this refresh; it never fails
// the refresh.
func (f *Farm) resolveAgents(ctx context.Context, asks []*beadsapi.BeadDetail) {
	if len(asks) == 0 {
		return
	}
	if len(f.agentByName) > maxAgentNames {
		f.agentByName = map[string]agentRef{}
	}
	var missing, names []string
	seen := map[string]bool{}
	for _, b := range asks {
		for _, ref := range agentRefs(b) {
			ref = strings.TrimSpace(ref)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			if beadRefRe.MatchString(ref) {
				if _, ok := f.agentNames[ref]; !ok {
					missing = append(missing, ref)
				}
				continue
			}
			// A live name is an agent already (kept, so it is known once it
			// leaves the roster); an email or OIDC subject is a person.
			if _, ok := f.agentByName[ref]; ok || strings.Contains(ref, "@") {
				continue
			}
			if f.roster.names[ref] {
				f.agentByName[ref] = agentRef{name: ref, agent: true}
			} else {
				names = append(names, ref)
			}
		}
	}
	if len(missing) > 0 {
		f.readAgentIDs(ctx, missing)
	}
	if len(names) > 0 {
		f.lookupAgentNames(ctx, names)
	}
}

func (f *Farm) readAgentIDs(ctx context.Context, missing []string) {
	got, err := f.src.BatchGetBeads(ctx, missing)
	if err != nil {
		f.logger.Warn("garden farm: resolving agent names failed", "error", err)
		return
	}
	if len(f.agentNames)+len(missing) > maxAgentNames {
		f.agentNames = map[string]agentRef{}
	}
	for _, id := range missing {
		ref := agentRef{name: id} // not an agent bead, or gone: keep the id and do not ask again
		if b := got[id]; b != nil && b.Type == "agent" {
			ref = agentFromBead(b)
			if ref.name == "" {
				ref.name = id
			}
		}
		f.agentNames[id] = ref
	}
}

// lookupAgentNames asks, once per name, whether an agent bead carries it.
func (f *Farm) lookupAgentNames(ctx context.Context, names []string) {
	if len(names) > maxNameLookups {
		names = names[:maxNameLookups] // the rest next refresh
	}
	for _, n := range names {
		res, err := f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types: []string{"agent"}, Fields: map[string]string{"agent": n}, Sort: "-updated_at", Limit: 5,
		})
		if err != nil {
			f.logger.Warn("garden farm: looking up agent name failed", "name", n, "error", err)
			return
		}
		ref := agentRef{name: n}
		for _, b := range res.Beads {
			if b == nil || b.Type != "agent" {
				continue
			}
			a := agentFromBead(b)
			if !ref.agent {
				ref.agent, ref.done = true, a.done
			} else if !a.done {
				ref.done = false
			}
		}
		f.agentByName[n] = ref
	}
}

func agentFromBead(b *beadsapi.BeadDetail) agentRef {
	ref := agentRef{agent: true, done: b.Status == "closed" || deadAgentStates[b.Fields["agent_state"]]}
	if n := strings.TrimSpace(b.Fields["agent"]); n != "" {
		ref.name = n
	} else {
		ref.name = strings.TrimSpace(b.Title)
	}
	return ref
}

// setRoster rebuilds the live agents from the fast tier's agent read. A failed
// read never reaches it, so the last good roster stays until it goes stale; a
// truncated one is kept but marked incomplete, so it calls nobody gone.
func (f *Farm) setRoster(res *beadsapi.ListBeadsResult) {
	now := f.now()
	ids, names := map[string]bool{}, map[string]bool{}
	for _, b := range res.Beads {
		if b == nil || b.Type != "agent" || deadAgentStates[b.Fields["agent_state"]] {
			continue
		}
		ids[b.ID] = true
		if a := agentFromBead(b); a.name != "" {
			names[a.name] = true
		}
	}
	f.roster = agentRoster{at: now, complete: !res.MayHaveMore(), ids: ids, names: names}
}

// party turns an agent reference into a name and says whether it is live.
// Only a fresh, complete roster can say an agent is gone; without one an agent
// is named as before, marked finished only if its own bead said so when read.
func (f *Farm) party(ref string) (string, int) {
	fresh := f.roster.complete && !f.roster.at.IsZero() && f.now().Sub(f.roster.at) < farmRosterStale
	var info agentRef
	var ok, live bool
	if beadRefRe.MatchString(ref) {
		info, ok = f.agentNames[ref]
		live = f.roster.ids[ref]
	} else {
		info, ok = f.agentByName[ref]
		live = f.roster.names[ref]
		if live {
			info, ok = agentRef{name: ref, agent: true}, true
		}
	}
	name := ref
	if ok && info.name != "" {
		name = info.name
	}
	switch {
	case !ok || !info.agent:
		return name, partyPerson
	case fresh && live:
		return name, partyLive
	case fresh:
		return name, partyGone
	case info.done:
		return name, partyDone
	}
	return name, partyPerson
}

func fieldUnix(v string) int64 {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(v))
	if err != nil {
		return 0
	}
	return t.Unix()
}

// waitOf says what an open decision or escalation waits on. The states follow
// the kbeads lifecycle: an answer (chosen / response_text) or an approval
// (approved_at, which also moves the escalation to in_progress) ends the human
// part of the wait. A rejection or a failed run writes result_status=failure
// and normally closes the bead, so an open one is rare, but it is still named.
//
// A non-human wait names the first live party of its refs (gd-g7jfH27efD): a
// finished agent is skipped, and when every named party is finished the wait
// falls to the operator, which makes it a human wait again. An approved
// escalation run locally or by hand waits on the operator from the start.
func waitOf(b *beadsapi.BeadDetail, party func(string) (string, int)) FarmWait {
	f := b.Fields
	w := FarmWait{ID: b.ID, Title: b.Title, Kind: b.Type, Since: b.CreatedAt.Unix()}
	since := func(key string) {
		if t := fieldUnix(f[key]); t > 0 {
			w.Since = t
		}
	}
	operator := func() {
		w.WaitsOn, w.Human, w.Due = WaitsOnOperator, true, fieldUnix(f["due_at"])
	}
	waitsOn := func(refs ...string) {
		named := false
		for _, ref := range refs {
			if ref = strings.TrimSpace(ref); ref == "" {
				continue
			}
			named = true
			name, state := party(ref)
			if state == partyGone {
				continue
			}
			if syntheticExecRe.MatchString(name) {
				name = WaitsOnExecutor
				if state == partyDone {
					name = WaitsOnExecutorDone
				}
			}
			w.WaitsOn = name
			return
		}
		if named {
			operator()
		}
	}
	if b.Type == "decision" {
		chosen := strings.TrimSpace(f["chosen"])
		if (chosen != "" && chosen != "_expired") || strings.TrimSpace(f["response_text"]) != "" {
			w.State = WaitAnswered
			waitsOn(agentRefs(b)...)
			since("responded_at")
			return w
		}
		w.State, w.Human = WaitUnanswered, true
		return w
	}
	w.Project, w.Op = projectOf(b), strings.TrimSpace(f["operation_type"])
	w.Family = EscalationFamily(w.Op)
	w.Held = AgentLaneHolder(f)
	switch w.State = escalationState(b); w.State {
	case WaitFailed, WaitRejected:
		waitsOn(f["requesting_agent"], b.CreatedBy)
	case WaitApproved:
		if f["execution_target"] == "local" || f["operation_type"] == "manual_operator_action" {
			operator()
		} else {
			waitsOn(agentRefs(b)...)
		}
		since("approved_at")
	default:
		w.Human = true
	}
	// An escalation the desk or the operators have is not the owner's to act on
	// (gd-bmOiteQi36): it is no "needs you" and says who has it instead.
	if w.Human {
		switch beadsapi.EscalationAudience(f) {
		case beadsapi.AudienceDesk:
			w.Human, w.WaitsOn, w.Due = false, WaitsOnDesk, 0
		case beadsapi.AudienceOperator:
			w.Human, w.WaitsOn, w.Due = false, WaitsOnOperators, 0
		}
	}
	// One a lane other than the owner's holds is that lane's (escalation
	// routing D9/D11), as /up's needs-you pick reads held_by (gd-vFUcVoRCU4):
	// a coordinator's forced-owner credential item is the coordinator's wait.
	// A land owner's stays a human wait; unstamped follows the audience above.
	if w.Human {
		if held := beadsapi.HeldByFromFields(f); held != "" && held != beadsapi.LaneOperator {
			if _, land := beadsapi.LandOwnerLane(held); !land {
				w.Human, w.WaitsOn, w.Due = false, beadsapi.LaneLabel(held), 0
			}
		}
	}
	return w
}

// escalationState is an escalation's Wait* state: failed or rejected once it
// ran and failed, approved once approved (or claimed), else pending.
func escalationState(b *beadsapi.BeadDetail) string {
	f := b.Fields
	switch {
	case f["result_status"] == "failure":
		// The reject handler records result {"error":"rejected", ...}.
		if strings.Contains(f["result"], "rejected") {
			return WaitRejected
		}
		return WaitFailed
	case strings.TrimSpace(f["approved_at"]) != "" || b.Status == "in_progress":
		return WaitApproved
	}
	return WaitPending
}

// finishWaits counts the human waits and orders the list (human first, then
// the longest wait first), capped at maxWaiting.
func finishWaits(ws []FarmWait) (int, []FarmWait) {
	human := 0
	for _, w := range ws {
		if w.Human {
			human++
		}
	}
	sort.Slice(ws, func(i, j int) bool { return waitLess(ws[i], ws[j]) })
	if len(ws) > maxWaiting {
		ws = ws[:maxWaiting]
	}
	return human, ws
}

// waitLess is the waits' order: human first, then the longest wait first.
func waitLess(a, b FarmWait) bool {
	if a.Human != b.Human {
		return a.Human
	}
	if a.Since != b.Since {
		return a.Since < b.Since
	}
	return a.ID < b.ID
}
