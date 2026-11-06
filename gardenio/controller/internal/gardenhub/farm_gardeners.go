package gardenhub

// farm_gardeners.go — gardeners[] on the farm document (gd-DKMSc1Y91I). The
// design is gardenio/docs/homestead-agents-as-people.md sections 2 and 3: one
// entry per live agent bead, placed on the plot of its task by the same
// ancestor walk that places the task, read on the fast tier from the one agent
// list the live-agent roster is also built from.

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/badge"
	"gardenio/controller/internal/beadsapi"
)

const (
	maxGardeners      = 300 // past it the newest by updated_at are kept (FarmMeta.GardenersTruncated)
	maxExits          = 20  // exits one refresh reads; the rest leave without one
	maxTitleReads     = 20  // beads one refresh reads for their titles (readRefTitles); the rest wait for the next
	maxGardenerTitle  = 120 // runes of FarmGardener.TaskTitle
	maxExitTLDR       = 140 // runes of FarmExit.TLDR
	gardenerSinceStep = 60  // FarmGardener.Since is floored to this many seconds
)

// Agent states, copied from beads-viewer (cmd/beads-viewer/agents.go
// agentStateAlive / agentStateStuck / agentStatePrewarmed / agentStateQueued).
// TestAgentBucketMatchesGardenhub there pins the copies together.
var (
	gardenerAliveStates = map[string]bool{"working": true, "spawning": true, "yielding": true}
	gardenerStuckStates = map[string]bool{"failed": true, "rate_limited": true, "budget_exceeded": true}
)

// leavingAgentStates are the agent_state values of an agent on its way out.
// The bead closes within seconds, so it is not a gardener: it leaves the set
// once, and the exit says why.
var leavingAgentStates = map[string]bool{"done": true, "stopping": true, "stop_requested": true, "expired": true}

// AgentBucket is beads-viewer's agentStateBucket: dormant, alive, stuck, or
// idle for everything else (including "", assigning and queued).
func AgentBucket(state string) string {
	switch {
	case state == beadsapi.AgentStateDormant:
		return GardenerDormant
	case gardenerAliveStates[state]:
		return GardenerAlive
	case gardenerStuckStates[state]:
		return GardenerStuck
	default:
		return GardenerIdle
	}
}

// IsPoolShell is beads-viewer's isUnengagedPrewarmedAgent: a prewarmed pool
// member nobody has engaged yet. It is prewarmed, or stranded in queued/idle
// with the pool's "prewarmed-" name and neither a thread nor a task.
func IsPoolShell(b *beadsapi.BeadDetail) bool {
	if b == nil {
		return false
	}
	state := b.Fields["agent_state"]
	if state == "prewarmed" {
		return true
	}
	if state != "queued" && state != "idle" {
		return false
	}
	if b.Fields["slack_thread_ts"] != "" || b.Fields["task_id"] != "" {
		return false
	}
	name := b.Fields["agent"]
	if name == "" {
		name = b.Title
	}
	return strings.HasPrefix(name, "prewarmed-")
}

// gardenerState is what the agent read last saw. Only refresh touches it.
type gardenerState struct {
	at       time.Time               // last good read
	complete bool                    // the last read was good and nothing was cut
	capped   bool                    // the last good read had more than maxGardeners
	byID     map[string]FarmGardener // the set from the last good read
}

// readAgents is the fast tier's one agent read: the open and in_progress agent
// beads, the same query the hub makes for the Porch.
func (f *Farm) readAgents(ctx context.Context) (*beadsapi.ListBeadsResult, error) {
	return f.src.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Statuses: []string{"open", "in_progress"}, Limit: listLimit,
		NoTotal: true, // completeness is MayHaveMore (gd-XOi5cqZP3I)
	})
}

// isGardener says whether a listed agent bead is drawn as a gardener.
func isGardener(b *beadsapi.BeadDetail) bool {
	return b != nil && b.Type == "agent" && (b.Status == "open" || b.Status == "in_progress") &&
		!IsPoolShell(b) && !leavingAgentStates[b.Fields["agent_state"]]
}

// IsThreadAgent says whether a role names a thread agent: one the bridge sends
// to answer a Commons or Slack thread (role "thread"), or a DM ("dm"). It
// stops after each reply and comes back on the next message.
func IsThreadAgent(role string) bool {
	for _, r := range strings.Split(role, ",") {
		if r = strings.ToLower(strings.TrimSpace(r)); r == "thread" || r == "dm" {
			return true
		}
	}
	return false
}

// threadHead is the thread head an agent bead names: fields.thread_id, else
// the older slack_thread_bead_id.
func threadHead(b *beadsapi.BeadDetail) string {
	return strings.TrimSpace(firstNonEmpty(b.Fields["thread_id"], b.Fields["slack_thread_bead_id"]))
}

// threadWords is a thread head's title as the garden says it after "In the
// Commons: ": without the "Commons thread: " or "Slack thread: " the bridge
// leads a head's title with.
func threadWords(title string) string {
	t := strings.TrimSpace(title)
	for _, p := range []string{"Commons thread:", "Slack thread:"} {
		if len(t) > len(p) && strings.EqualFold(t[:len(p)], p) {
			return strings.TrimSpace(t[len(p):])
		}
	}
	return t
}

// gardenerOf projects an agent bead. plotOf resolves a task to its plot (nil
// at home); titleOf names a task the farm read ("" when it did not); threadOf
// names a thread head (the rows, else readRefTitles); park and now feed its
// reason (farm_reason.go). The bead that places it is its task, else the bead
// it was sent to (tend.go), else the escalation it was spawned to run
// (escalation_id, the bridge's executor): an executor has no task of its own,
// and without this it stood at the barn while it worked (gd-sXAaYVo2UB).
func gardenerOf(b *beadsapi.BeadDetail, plotOf func(string) *FarmPlot, titleOf, threadOf func(string) string, park parkIndex, now time.Time) FarmGardener {
	state := strings.TrimSpace(b.Fields["agent_state"])
	task := strings.TrimSpace(b.Fields["task_id"])
	project := projectOf(b)
	if project == "" {
		project = strings.TrimSpace(b.Fields["project"])
	}
	since := fieldUnix(b.Fields["last_progress_at"])
	if since <= 0 {
		since = b.UpdatedAt.Unix()
	}
	since -= since % gardenerSinceStep
	g := FarmGardener{
		ID: b.ID, Agent: agentFromBead(b).name, Role: strings.TrimSpace(b.Fields["role"]), Project: project,
		Operator: lower(b.OperatedBy), State: state, Bucket: AgentBucket(state), Task: task,
		Since: since, Href: beadHref("", b.ID),
	}
	if task != "" {
		g.Plot = plotOf(task)
		if t := titleOf(task); t != "" {
			g.TaskTitle = clip(t, maxGardenerTitle)
		}
	}
	if tends := TendedBead(b); tends != "" {
		// Sent to a bead from the garden (tend.go): it stands on that bead's plot when it has no task of its own.
		g.Tends = tends
		if t := titleOf(tends); t != "" {
			g.TendsTitle = clip(t, maxGardenerTitle)
		}
		if task == "" {
			g.Plot = plotOf(tends)
		}
	}
	if esc := strings.TrimSpace(b.Fields["escalation_id"]); esc != "" {
		// Spawned to run an escalation (bridge escalations.go spawnExecutionAgent): it stands on the escalation's plot
		// when nothing else places it. An open escalation is a work row (farmWorkTypes), parented to the task that
		// filed it, so plotOf resolves it as it does that task.
		g.Escalation = esc
		if t := titleOf(esc); t != "" {
			g.EscalationTitle = clip(t, maxGardenerTitle)
		}
		if task == "" && g.Tends == "" {
			g.Plot = plotOf(esc)
		}
	}
	if head := threadHead(b); head != "" {
		g.Chat = "/garden/c/bead/" + url.PathEscape(b.ID)
		if t := threadWords(threadOf(head)); t != "" {
			g.ThreadTitle = clip(t, maxGardenerTitle)
		}
	}
	g.Reason = reasonOf(b, g, park, now)
	return g
}

// placedBy is the bead that places a gardener on the farm (gardenerOf): its
// task, else the bead it was sent to, else the escalation it runs; "" at the
// barn with nothing in hand.
func placedBy(g FarmGardener) string {
	if g.Task != "" {
		return g.Task
	}
	if g.Tends != "" {
		return g.Tends
	}
	return g.Escalation
}

// buildGardeners is the set from one complete agent read, and whether it was
// cut to maxGardeners (the newest by updated_at are kept). armsOf, when
// given, is the line arms of the bead that places each gardener
// (gardenerArms). runSpendOf is a gardener trigger's spend rollup
// (farm_gardener_cost.go), nil when not known.
func buildGardeners(agents []*beadsapi.BeadDetail, plotOf func(string) *FarmPlot, titleOf, threadOf func(string) string, armsOf func(string) *beadArms, runSpendOf func(string) *float64, park parkIndex, now time.Time) (map[string]FarmGardener, bool) {
	var keep []*beadsapi.BeadDetail
	seen := map[string]bool{}
	for _, b := range agents {
		if isGardener(b) && !seen[b.ID] {
			seen[b.ID] = true
			keep = append(keep, b)
		}
	}
	capped := len(keep) > maxGardeners
	if capped {
		sort.Slice(keep, func(i, j int) bool {
			if !keep[i].UpdatedAt.Equal(keep[j].UpdatedAt) {
				return keep[i].UpdatedAt.After(keep[j].UpdatedAt)
			}
			return keep[i].ID < keep[j].ID
		})
		keep = keep[:maxGardeners]
	}
	out := make(map[string]FarmGardener, len(keep))
	for _, b := range keep {
		g := gardenerOf(b, plotOf, titleOf, threadOf, park, now)
		if armsOf != nil {
			g.TaskArms = armsOf(placedBy(g))
		}
		g.Cost = gardenerCost(b.Fields, g.Bucket, runSpendOf(gardenerRun(b)))
		out[b.ID] = g
	}
	return out, capped
}

// refBead is what the farm keeps of a bead a gardener names that is not among
// the rows read (readRefTitles): its title, and its place in the line
// (gardenerArms) when the read found it; line.ID is "" when it did not.
type refBead struct {
	title string
	line  badge.LineNode
}

// farmLineNode is a row as the line walk reads it (badge.Node): its arms when
// its type may bear them, and its parent_id as its one parent.
// The farm reads no parent-child dependency rows, so a bead with a second
// parent walks only its parent_id, the edge that places it on a plot too
// (farm.go resolve).
func farmLineNode(b *beadsapi.BeadDetail) badge.LineNode {
	n := badge.Node(b, nil)
	if p := strings.TrimSpace(b.ParentID); p != "" {
		n.Parents = []string{p}
	}
	return n
}

// gardenerArms resolves the line arms of a bead a gardener stands for
// (heraldry step 5.9, gd-QboM9sPYCg): its nearest armed ancestor's by
// badge.ResolveArms, the rule the ledger's rows (line_arms.go) and gb badge
// draw by, walked over the open work rows the farm already read (byID) and
// its reference reads (refs: a closed task), so it costs no daemon read. A
// line that leaves those rows (a closed epic) bears none here. Memoised for
// one refresh: gardeners share tasks.
func gardenerArms(byID map[string]*beadsapi.BeadDetail, refs map[string]refBead) func(string) *beadArms {
	lookup := func(_ context.Context, id string) (badge.LineNode, error) {
		if b := byID[id]; b != nil {
			return farmLineNode(b), nil
		}
		if r, ok := refs[id]; ok && r.line.ID != "" {
			return r.line, nil
		}
		return badge.LineNode{ID: id}, nil // out of sight: an end of the line
	}
	memo := map[string]*beadArms{}
	return func(id string) *beadArms {
		if id == "" {
			return nil
		}
		if a, ok := memo[id]; ok {
			return a
		}
		var out *beadArms
		if a, err := badge.ResolveArms(context.Background(), id, lookup); err == nil && !a.None() {
			out = armsOf(a.Heraldry)
		}
		memo[id] = out
		return out
	}
}

// readRefTitles fills f.refTitles for the beads gardeners name that are not
// among the rows read (byID): a gardener's task, the bead it was sent to and
// the escalation it runs (closed, or past the row cap: the Porch said
// "Assigned: gd-A1y" for a closed task, gd-YvdhZD2UtL), and a thread agent's
// thread head. One BatchGetBeads of at most maxTitleReads beads not yet
// cached; one the read does not return is cached as "" so it is not asked
// again. Beads no live gardener names any more are dropped. A failed read
// caches nothing: those gardeners carry no title until the next refresh.
func (f *Farm) readRefTitles(ctx context.Context, agents []*beadsapi.BeadDetail, byID map[string]*beadsapi.BeadDetail) {
	want := map[string]bool{}
	var ask []string
	for _, b := range agents {
		if !isGardener(b) {
			continue
		}
		refs := []string{strings.TrimSpace(b.Fields["task_id"]), TendedBead(b), strings.TrimSpace(b.Fields["escalation_id"])}
		if IsThreadAgent(b.Fields["role"]) {
			refs = append(refs, threadHead(b))
		}
		for _, id := range refs {
			if id == "" || byID[id] != nil || want[id] {
				continue
			}
			want[id] = true
			if _, ok := f.refTitles[id]; !ok && len(ask) < maxTitleReads {
				ask = append(ask, id)
			}
		}
	}
	for id := range f.refTitles {
		if !want[id] {
			delete(f.refTitles, id)
		}
	}
	if len(ask) == 0 {
		return
	}
	sort.Strings(ask)
	got, err := f.src.BatchGetBeads(ctx, ask)
	if err != nil {
		f.logger.Warn("garden farm: reading gardener titles failed", "error", err)
		return
	}
	for _, id := range ask {
		r := refBead{}
		if h := got[id]; h != nil {
			r = refBead{title: h.Title, line: farmLineNode(h)}
		}
		f.refTitles[id] = r
	}
}

// gardenerEntries marshals a set. The key is 0 for all, so the snapshot array
// is sorted by id.
func gardenerEntries(gs map[string]FarmGardener) map[string]entry {
	out := make(map[string]entry, len(gs))
	for id, g := range gs {
		raw, _ := json.Marshal(g)
		out[id] = entry{raw: raw}
	}
	return out
}

// readExits says why each gardener in rm left the set, from one BatchGetBeads
// of the agent beads and of any task the open rows do not hold. A stop with
// its task still an open work step (not closed, not a plan or a phase
// container: isContainer) is parked. A failed read sends no exits: the set is right without
// them, and a client only misses an animation.
func (f *Farm) readExits(ctx context.Context, rm []string, prev map[string]FarmGardener, byID map[string]*beadsapi.BeadDetail, isContainer func(string) bool) []FarmExit {
	if len(rm) > maxExits {
		rm = rm[:maxExits]
	}
	ids := append([]string{}, rm...)
	asked := map[string]bool{}
	for _, id := range rm {
		if t := prev[id].Task; t != "" && byID[t] == nil && !asked[t] {
			asked[t] = true
			ids = append(ids, t)
		}
	}
	got, err := f.src.BatchGetBeads(ctx, ids)
	if err != nil {
		f.logger.Warn("garden farm: reading gardener exits failed", "error", err)
		return nil
	}
	openStep := func(task string) bool {
		if task == "" || isContainer(task) {
			return false
		}
		if byID[task] != nil {
			return true
		}
		b := got[task]
		return b != nil && b.Status != "closed" && !(b.Type == "epic" && hasLabel(b, "plan"))
	}
	at := f.now().Unix()
	out := make([]FarmExit, 0, len(rm))
	for _, id := range rm {
		p := prev[id]
		x := FarmExit{ID: id, Agent: p.Agent, Kind: ExitInterrupted, Plot: p.Plot, At: at, Tends: p.Tends, Escalation: p.Escalation}
		if b := got[id]; b != nil && b.Fields["stop_requested"] == "true" {
			task := p.Task
			if t := strings.TrimSpace(b.Fields["task_id"]); t != "" {
				task = t
			}
			x.Kind = ExitFinished
			if openStep(task) {
				x.Kind = ExitParked
			}
			x.TLDR = wrapupTLDR(b.Fields[beadsapi.WrapUpFieldName])
		}
		out = append(out, x)
	}
	return out
}

// wrapupTLDR is the tldr of a wrap-up JSON, cut at maxExitTLDR runes.
func wrapupTLDR(raw string) string {
	var w struct {
		TLDR string `json:"tldr"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &w) != nil {
		return ""
	}
	return clip(w.TLDR, maxExitTLDR)
}
