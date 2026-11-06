package main

// crew.go — `gb crew` lists the crew agents the caller is responsible for.
//
// SCOPING (bug gd-bcdJr7cT0L). This command used to filter agent beads on
// `spawned_by == <current actor>` and nothing else. That is correct for a human
// operator and WRONG for a gardener, because a gardener is not one process: every
// cycle / re-spawn mints a NEW agent identity (gardenio-gardener-for-<epic>-<rand>),
// while its predecessor's crews keep running under the PREDECESSOR's name. So a
// fresh gardener instance always sees an empty crew list.
//
// Observed 2026-09-21: a new taints gardener ran `gb crew`, saw nothing while two
// crews were demonstrably alive (agent beads working, last_progress minutes old,
// pods Running 28m/24m, zero restarts), declared their in_progress tasks
// orphaned, and dispatched two duplicates — taking the fleet to 8 crew pods
// against ~2-concurrent guidance. The event-driven gardener design makes
// re-spawns MORE frequent, so the duplicate dispatch repeats every cycle.
//
// The fix: a gardener's crew is defined by the WORK, not by the spawn lineage.
// Every active agent bead whose fields.task_id sits inside the gardener's epic
// subtree is its crew, regardless of who spawned it. `--tree <bead>` names that
// root explicitly, and the gardener path AUTO-DERIVES it from the caller's own
// agent bead — so the answer never depends on anyone remembering the flag.
//
// OWNERSHIP. Listing is only half of it: the gardener also has to decide whether
// an in_progress task is free. `crewOwnership` answers that from the agent
// bead's own heartbeat — a non-terminal agent whose last_progress_at is within
// crewOwnershipWindow OWNS its task and must never be reclaimed. A just-spawned
// agent that has not landed a turn yet falls back to created_at, so the booting
// window is covered too (fail-safe toward never-reclaim).

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// crewOwnershipWindow is how fresh an agent's heartbeat must be for its task to
// count as definitively OWNED — i.e. a task the gardener must NOT reclaim or
// re-dispatch. 15 minutes is deliberately tighter than
// beadsapi.ProgressStaleThreshold (90m, the single-claim guard's liveness
// bound): this is the "provably alive right now" marker the gardener acts on,
// not the outer bound past which a claim may be released. Both crews in the
// incident had last_progress 2m and 20m old at takeover.
const crewOwnershipWindow = 15 * time.Minute

// crewTreeScanLimit bounds the subtree walk behind the tree scope. An epic with
// more descendants than this is far beyond any real gardener's span; the cap is
// a safety valve, not a functional limit.
const crewTreeScanLimit = 2000

// crewTreeExcludeTypes drops narrative/record descendants from the subtree walk
// so the scan budget is spent on work items. No agent bead ever carries a
// task_id pointing at one of these, so excluding them cannot hide a crew.
var crewTreeExcludeTypes = []string{"agent", "bundle", "report", "rich-report", "session", "mail"}

// terminalCrewStates are the agent_state values that mean the agent is finished
// (or dead) and is therefore no longer holding its task. Mirrors
// beadsapi.terminalAgentStates, which is unexported.
var terminalCrewStates = map[string]bool{"done": true, "failed": true}

var crewCmd = &cobra.Command{
	Use:     "crew",
	Short:   "Show the crew agents you are responsible for",
	GroupID: "orchestration",
	Long: `Show the crew agents the caller is responsible for: their status, the
task they are on, how long they have been running, and whether they currently
OWN that task.

Two scopes are unioned:

  tree     every active agent bead whose task_id is inside your epic's
           subtree, REGARDLESS of who spawned it. Use --tree <bead> to name
           the root; a gardener derives it automatically from its own agent
           bead, because a gardener re-spawn has a new identity and would
           otherwise be blind to its predecessor's live crews (see bug gd-bcdJr7cT0L).
  lineage  agents whose spawned_by matches the current --actor (defaults to
           $KD_ACTOR or git user.name).

An agent marked OWNED is non-terminal and has a heartbeat within 15 minutes.
Its task is NOT orphaned and must not be reclaimed or re-dispatched.

--project filters the lineage scope only. A task inside your tree is yours
whatever project label its crew carries, so tree members are never dropped.`,
	RunE: runCrew,
}

func init() {
	crewCmd.Flags().Bool("all", false, "show all agents (not just yours)")
	crewCmd.Flags().String("project", defaultProject(), "filter by project (default: $KD_PROJECT or $BOAT_PROJECT)")
	crewCmd.Flags().String("tree", "", "scope to every agent working inside this bead's subtree (default: auto-derived for a gardener)")
}

// crewEntry pairs an agent bead with its assigned task (if any) and the
// ownership verdict for that task.
type crewEntry struct {
	Agent    beadsapi.AgentBead
	TaskID   string
	TaskName string
	// InTree is true when the agent's task sits inside the resolved tree root's
	// subtree — i.e. it is this gardener's crew by WORK rather than by lineage.
	InTree bool
	// MineBySpawn is true when the agent's spawned_by matches the current actor.
	MineBySpawn bool
	// Owned is the never-reclaim verdict for TaskID (see crewOwnership).
	Owned bool
	// Heartbeat is the freshest liveness signal found on the agent bead
	// (last_progress_at, else created_at). Zero when neither parsed.
	Heartbeat time.Time
}

// crewJSONEntry is the machine-readable shape of a crew listing, so a gardener
// can gate dispatch on `owned` instead of on prose.
type crewJSONEntry struct {
	AgentBead   string `json:"agent_bead"`
	Agent       string `json:"agent"`
	Role        string `json:"role,omitempty"`
	Project     string `json:"project,omitempty"`
	State       string `json:"state"`
	TaskID      string `json:"task_id,omitempty"`
	TaskTitle   string `json:"task_title,omitempty"`
	SpawnedBy   string `json:"spawned_by,omitempty"`
	InTree      bool   `json:"in_tree"`
	MineBySpawn bool   `json:"mine_by_spawn"`
	Owned       bool   `json:"owned"`
	Heartbeat   string `json:"heartbeat,omitempty"`
}

func runCrew(cmd *cobra.Command, args []string) error {
	showAll, _ := cmd.Flags().GetBool("all")
	project, _ := cmd.Flags().GetString("project")
	treeFlag, _ := cmd.Flags().GetString("tree")
	ctx := cmd.Context()
	out := cmd.OutOrStdout()

	agents, err := daemon.ListAgentBeads(ctx)
	if err != nil {
		return fmt.Errorf("listing agents: %w", err)
	}

	// Resolve the tree scope first. An explicit --tree always wins; otherwise a
	// gardener derives its epic from its own agent bead. Auto-derivation is
	// best-effort — a caller with no agent identity (an operator laptop) simply
	// gets the lineage scope, exactly as before.
	var (
		treeRoot string
		tree     map[string]bool
		selfBead string
	)
	if !showAll {
		treeRoot, selfBead = resolveCrewTreeRoot(ctx, treeFlag)
		if treeRoot != "" {
			tree, err = crewTreeBeadIDs(ctx, treeRoot)
			if err != nil {
				return fmt.Errorf("walking the subtree under %s: %w", treeRoot, err)
			}
		}
	}

	now := time.Now()
	var entries []crewEntry
	for _, a := range agents {
		if selfBead != "" && a.ID == selfBead {
			continue // never report yourself as your own crew
		}
		taskID := a.Metadata["task_id"]
		inTree := taskID != "" && tree[taskID]
		mine := isMySpawn(a.Metadata["spawned_by"], actor)
		if !inTree {
			// Unchanged behaviour for every non-tree candidate.
			if project != "" && a.Project != project {
				continue
			}
			if !showAll && !mine {
				continue
			}
		}
		hb, owned := crewOwnership(a.AgentState, a.Metadata["last_progress_at"], a.Metadata["created_at"], now)
		entries = append(entries, crewEntry{
			Agent:       a,
			TaskID:      taskID,
			InTree:      inTree,
			MineBySpawn: mine,
			Owned:       owned,
			Heartbeat:   hb,
		})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Agent.AgentName != entries[j].Agent.AgentName {
			return entries[i].Agent.AgentName < entries[j].Agent.AgentName
		}
		return entries[i].Agent.ID < entries[j].Agent.ID
	})

	// Batch-fetch task titles for agents that have tasks.
	taskTitles := make(map[string]string)
	for _, e := range entries {
		if e.TaskID != "" {
			taskTitles[e.TaskID] = ""
		}
	}
	for tid := range taskTitles {
		if detail, err := daemon.GetBead(ctx, tid); err == nil && detail != nil {
			title := detail.Title
			if len(title) > 50 {
				title = title[:49] + "…"
			}
			taskTitles[tid] = title
		}
	}
	for i := range entries {
		entries[i].TaskName = taskTitles[entries[i].TaskID]
	}

	if jsonOutput {
		printJSON(crewJSON(entries))
		return nil
	}

	if len(entries) == 0 {
		switch {
		case showAll:
			fmt.Fprintln(out, "No active agents.")
		case treeRoot != "":
			fmt.Fprintf(out, "No active crew under %s (and none spawned by %s).\n", treeRoot, actor)
		default:
			fmt.Fprintf(out, "No active crew spawned by %s.\n", actor)
			fmt.Fprintln(out, "  note: no tree scope resolved — pass --tree <epic> to list every agent")
			fmt.Fprintln(out, "        working inside your epic, regardless of who spawned it.")
		}
		return nil
	}

	switch {
	case showAll:
		fmt.Fprintf(out, "Active agents (%d):\n\n", len(entries))
	case treeRoot != "":
		fmt.Fprintf(out, "Your crew (%d agents; tree %s + spawned by %s):\n\n", len(entries), treeRoot, actor)
	default:
		fmt.Fprintf(out, "Your crew (%d agents):\n\n", len(entries))
	}

	owned := 0
	for _, e := range entries {
		state := e.Agent.AgentState
		if state == "" {
			state = "unknown"
		}

		// Duration since agent started (approximate from bead metadata).
		var durationStr string
		if created := e.Agent.Metadata["created_at"]; created != "" {
			if t, err := time.Parse(time.RFC3339, created); err == nil {
				durationStr = shortDuration(time.Since(t))
			}
		}

		taskInfo := ""
		if e.TaskID != "" {
			if e.TaskName != "" {
				taskInfo = fmt.Sprintf(" → %s (%s)", e.TaskName, e.TaskID)
			} else {
				taskInfo = fmt.Sprintf(" → %s", e.TaskID)
			}
		}

		marker := ""
		if e.Owned {
			owned++
			marker = "  [OWNED]"
		}
		if e.InTree && !e.MineBySpawn {
			marker += fmt.Sprintf("  [tree, by %s]", formatSpawner(e.Agent.Metadata["spawned_by"]))
		} else if showAll {
			marker += fmt.Sprintf("  [by %s]", formatSpawner(e.Agent.Metadata["spawned_by"]))
		}

		if durationStr != "" {
			fmt.Fprintf(out, "  %-10s %-28s %s%s%s\n",
				state, e.Agent.AgentName, durationStr, taskInfo, marker)
		} else {
			fmt.Fprintf(out, "  %-10s %-28s%s%s\n",
				state, e.Agent.AgentName, taskInfo, marker)
		}
	}

	if owned > 0 {
		fmt.Fprintf(out, "\n%d task(s) marked [OWNED] — a live agent is on them. Do NOT reclaim or re-dispatch.\n", owned)
	}
	fmt.Fprintln(out)
	return nil
}

// crewJSON projects entries into the machine-readable listing.
func crewJSON(entries []crewEntry) []crewJSONEntry {
	outEntries := make([]crewJSONEntry, 0, len(entries))
	for _, e := range entries {
		je := crewJSONEntry{
			AgentBead:   e.Agent.ID,
			Agent:       e.Agent.AgentName,
			Role:        e.Agent.Role,
			Project:     e.Agent.Project,
			State:       defaultIfEmpty(e.Agent.AgentState, "unknown"),
			TaskID:      e.TaskID,
			TaskTitle:   e.TaskName,
			SpawnedBy:   e.Agent.Metadata["spawned_by"],
			InTree:      e.InTree,
			MineBySpawn: e.MineBySpawn,
			Owned:       e.Owned,
		}
		if !e.Heartbeat.IsZero() {
			je.Heartbeat = e.Heartbeat.UTC().Format(time.RFC3339)
		}
		outEntries = append(outEntries, je)
	}
	return outEntries
}

// crewOwnership computes the heartbeat and the never-reclaim verdict for one
// agent bead.
//
// The agent OWNS its task when it is non-terminal AND its freshest heartbeat is
// within crewOwnershipWindow. last_progress_at is the primary signal (it is
// stamped only when a genuine turn lands, so it freezes the instant real work
// stops); created_at is the fallback for an agent that has not landed its first
// turn yet, so a booting crew is never mistaken for an orphan.
//
// A heartbeat in the future is treated as fresh: clock skew must not evict a
// live agent.
func crewOwnership(state, lastProgressAt, createdAt string, now time.Time) (time.Time, bool) {
	hb := parseCrewTime(lastProgressAt)
	if hb.IsZero() {
		hb = parseCrewTime(createdAt)
	}
	if terminalCrewStates[strings.ToLower(strings.TrimSpace(state))] {
		return hb, false
	}
	if hb.IsZero() {
		return hb, false
	}
	return hb, now.Sub(hb) <= crewOwnershipWindow
}

// parseCrewTime parses an RFC3339 bead timestamp, returning the zero time for
// anything unparseable.
func parseCrewTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// resolveCrewTreeRoot returns the bead whose subtree defines "my crew", plus
// the caller's own agent bead id (so it can be excluded from its own listing).
//
// An explicit --tree always wins. Otherwise the root is auto-derived from the
// caller's agent bead: a gardener (role contains "gardener", covering both
// "gardener" and the plan-gardener role) is pinned to its epic via fields.task_id,
// which is exactly the subtree it dispatches into. A non-gardener agent gets a
// tree scope only when its own task_id is a phase container (epic/molecule) —
// a leaf-task crew has no fan-out to be blind to.
//
// Every failure path returns "" rather than an error: `gb crew` must keep
// working from an operator laptop with no agent identity at all.
func resolveCrewTreeRoot(ctx context.Context, explicit string) (root, selfBead string) {
	if strings.TrimSpace(explicit) != "" {
		explicit = strings.TrimSpace(explicit)
	}
	agentID, err := resolveAgentIDWithFallback(ctx, "")
	if err != nil || agentID == "" {
		return explicit, ""
	}
	selfBead = agentID
	if explicit != "" {
		return explicit, selfBead
	}
	self, err := daemon.GetBead(ctx, agentID)
	if err != nil || self == nil {
		return "", selfBead
	}
	taskID := strings.TrimSpace(self.Fields["task_id"])
	if taskID == "" {
		return "", selfBead
	}
	if strings.Contains(strings.ToLower(self.Fields["role"]), "gardener") {
		return taskID, selfBead
	}
	task, err := daemon.GetBead(ctx, taskID)
	if err != nil || task == nil || !beadsapi.IsPhaseContainer(task) {
		return "", selfBead
	}
	return taskID, selfBead
}

// crewTreeBeadIDs returns the id set of root's transitive parent-child subtree,
// root included. A crew assigned directly to the epic is this gardener's crew
// too, so the root is a member.
//
// Deliberately NOT status-filtered: an agent still wrapping up on a task that
// was just closed is live and must still be listed. Cross-project by
// construction — the beadsapi client injects no implicit project label — so a
// child carrying a different project label is not silently dropped.
func crewTreeBeadIDs(ctx context.Context, root string) (map[string]bool, error) {
	res, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Parent:       root,
		ExcludeTypes: crewTreeExcludeTypes,
		Limit:        crewTreeScanLimit,
	})
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{root: true}
	for _, b := range res.Beads {
		if b == nil || b.ID == "" {
			continue
		}
		ids[b.ID] = true
	}
	if res.Total > len(res.Beads) {
		fmt.Fprintf(os.Stderr,
			"warning: subtree under %s truncated at %d of %d beads; `gb crew` may under-report\n",
			root, len(res.Beads), res.Total)
	}
	return ids, nil
}

// isMySpawn checks if the spawned_by value matches the current actor.
// Handles both direct name matches and schedule-prefixed spawners.
func isMySpawn(spawner, myName string) bool {
	if spawner == "" || myName == "" {
		return false
	}
	return strings.EqualFold(spawner, myName)
}

// shortDuration formats a duration compactly for display.
func shortDuration(d time.Duration) string {
	d = d.Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if m == 0 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dh%dm", h, m)
}
