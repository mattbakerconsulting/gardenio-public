package main

// gb stop — successor=new mint (persistent-agents Phase 1, task ka-0000006931,
// design memo §2.5/§3.1).
//
// When a wrap-up declares `successor:"new"`, this agent's work continues on a
// FRESHLY-MINTED successor agent: a new name + new card + new conversation. The
// predecessor is then closed terminally with close_reason
// "superseded: continued as <successor-id>" — the load-bearing contract the
// bridge card-chaining side already parses (successorIDFromCloseReason,
// successorContinuationLine in internal/bridge/bot_agents_wrapup.go, MR !6499).
//
// The successor inherits role/mode/project/gardener ownership/terminal_goal from
// the predecessor, seeds its opening prompt from the predecessor's handoff_notes,
// and carries the two neutral succession back-pointers:
//   - prior_agent_id  = the predecessor bead id (the one lineage pointer).
//   - lineage_root_id = the predecessor's own root, else the predecessor id
//     itself (anchoring a brand-new chain).
//
// This is additive and fully opt-in: nothing runs unless an agent explicitly
// asks for successor=new, so the default terminal `gb stop` is unchanged.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// errLineageBudgetSpent is returned by mintSuccessorAgent when the lineage's
// continuation budget is exhausted. The caller (cmd/gb stop.go) treats it like
// any other mint failure — it falls back to a plain terminal close — but logs it
// distinctly so a spent budget reads as "chain capped, not continued", not a bug.
// This is the enforcement point for the per-lineage-root ceiling (ka-0000001361):
// a spent budget means the predecessor closes normally and NO successor is minted,
// which is what breaks the self-spawn loop.
var errLineageBudgetSpent = errors.New("lineage continuation budget spent")

// supersededCloseReasonPrefix mirrors the bridge-side constant
// (internal/bridge/bot_agents_wrapup.go). The predecessor's close_reason is set
// to this prefix + the successor bead id so the card-chaining renderer can parse
// the successor id back out and draw the "🔗 Continued as <successor>" link. The
// exact prefix is a cross-package contract — keep the two in sync.
const supersededCloseReasonPrefix = "superseded: continued as "

// mintSuccessorAgent creates the successor agent bead for a successor=new stop
// and returns its bead id. It is best-effort at the boundary: a nil daemon, an
// unfetchable predecessor, or a spawn error returns ("", err) and the caller
// falls back to a plain terminal close (no chaining) rather than dead-ending the
// stop. On success the caller closes the predecessor with the superseded
// close_reason carrying the returned id.
//
// predID is the predecessor (this) agent's bead id; w is its validated wrap-up
// (guaranteed non-nil with Successor=="new" by the caller); actor is the
// stamping identity.
func mintSuccessorAgent(ctx context.Context, predID string, w *WrapUp, actor string) (string, error) {
	if daemon == nil {
		return "", fmt.Errorf("no daemon client")
	}
	if predID == "" {
		return "", fmt.Errorf("empty predecessor id")
	}

	pred, err := daemon.GetBead(ctx, predID)
	if err != nil {
		return "", fmt.Errorf("fetching predecessor bead %s: %w", predID, err)
	}
	if pred == nil {
		return "", fmt.Errorf("predecessor bead %s not found", predID)
	}
	pf := pred.Fields
	if pf == nil {
		pf = map[string]string{}
	}

	// Inherited identity. Role/mode/project define what the successor IS; an
	// empty inherited value falls back to the SpawnAgent defaults (role→crew,
	// mode→crew), matching a first spawn.
	project := pf["project"]
	role := pf["role"]

	// Successor name: derive a fresh, human-readable name from the predecessor's
	// base name (strip the trailing random suffix) plus a new random suffix so
	// the card reads as a continuation of the same lineage, not an unrelated
	// agent. spawnGenerateSpawnName gives the project-prefixed fallback when the
	// predecessor name is unusable.
	agentName := successorAgentName(pf["agent"], project)

	// lineage_root_id: the predecessor's own root anchors the chain; if the
	// predecessor was itself a root (no recorded root), the predecessor id is the
	// root of this new chain.
	lineageRoot := pf["lineage_root_id"]
	if lineageRoot == "" {
		lineageRoot = predID
	}

	// Per-lineage continuation budget (ka-0000001361). Before minting a fresh
	// successor, count how many agent beads already carry this lineage_root_id —
	// the chain's total length — and refuse if it has reached the ceiling. This is
	// the ONLY bound on the successor=new self-spawn loop: unlike the wake-scanner's
	// per-bead respawn_count (which resets on every fresh successor bead), the
	// lineage count accumulates across the whole chain, so it is what actually caps
	// an agent that stops with successor=new on every run.
	//
	// Fail-OPEN on a count error (a transient daemon hiccup must not silently
	// dead-end a legitimate continuation); fail-CLOSED only on a definitively-spent
	// budget. The ceiling is a per-lineage override carried forward from the
	// predecessor, else DefaultMaxLineageContinuations.
	maxLineage := beadsapi.DefaultMaxLineageContinuations
	if v := strings.TrimSpace(pf[beadsapi.FieldMaxLineageContinuations]); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxLineage = n
		}
	}
	if count, err := daemon.LineageContinuationCount(ctx, lineageRoot); err != nil {
		// Fail-open: log and proceed. A count failure must not block continuation.
		fmt.Fprintf(os.Stderr, "Warning: lineage budget check failed (proceeding): %v\n", err)
	} else if count >= maxLineage {
		return "", fmt.Errorf("%w: lineage %s has %d/%d continuations",
			errLineageBudgetSpent, lineageRoot, count, maxLineage)
	}

	extra := map[string]string{
		"prior_agent_id":  predID,
		"lineage_root_id": lineageRoot,
		"spawn_source":    "successor",
		"spawned_by":      "successor:" + predID,
	}
	// Carry the per-lineage ceiling override forward so the whole chain shares one
	// budget rather than each successor re-defaulting.
	if v := strings.TrimSpace(pf[beadsapi.FieldMaxLineageContinuations]); v != "" {
		extra[beadsapi.FieldMaxLineageContinuations] = v
	}
	// Inherit mode explicitly when the predecessor set a non-default one (e.g.
	// a crew agent keeps its PVC-backed mode). SpawnAgent defaults mode=crew, so
	// only override when the predecessor recorded something and it differs.
	if mode := pf["mode"]; mode != "" {
		extra["mode"] = mode
	}
	// Carry gardener ownership + terminal goal forward so the successor is never
	// an orphan and keeps steering toward the same goal (design memo §2.5: the
	// successor inherits gardener_id/terminal_goal). Only copy non-empty values.
	for _, k := range []string{
		"gardener_id",
		"schedule_id",
		"schedule_terminal_goal",
		"terminal_goal",
		"operated_by",
		"slack_user_id",
		"user_bead_id",
	} {
		if v := pf[k]; v != "" {
			extra[k] = v
		}
	}
	// Post where the predecessor did (gd-jOHtHdkdIt): the full Slack routing
	// set — spawn channel with its inherited marker, schedule channel, thread
	// binding — plus the wish pointer, without which a thread agent's
	// successor upserts a second top-level card (gd-AflFRvDw7Z).
	beadsapi.CopySlackRoutingFields(extra, pf)
	beadsapi.SetWishPointer(extra, firstNonEmpty(pf[beadsapi.FieldThreadID], pf[beadsapi.FieldSlackThreadBeadID]))
	// Stamp the actor as operated_by when the predecessor carried none, so the
	// successor pod still boots attributed.
	if extra["operated_by"] == "" && isHumanShapedActor(actor) {
		extra["operated_by"] = actor
	}

	// Opening prompt: seed from the predecessor's handoff_notes so the successor
	// picks up where this run left off (design memo §2.5). Fall back to the
	// predecessor's own prompt, then to a minimal continuation instruction.
	prompt := successorPrompt(w, pf)

	// Task target: continue on the same task bead the predecessor worked, when
	// present, so the successor is dispatched against the same unit of work.
	taskID := pf["task_id"]

	successorID, err := daemon.SpawnAgent(ctx, agentName, project, taskID, role, prompt, extra)
	if err != nil {
		return "", fmt.Errorf("spawning successor agent %q: %w", agentName, err)
	}
	return successorID, nil
}

// successorAgentName derives a fresh successor name from the predecessor's agent
// name. It strips a trailing "-<suffix>" random token (if present) and appends a
// new random suffix, keeping the lineage visually recognizable. Falls back to a
// project-prefixed random name when the predecessor name is empty/unusable.
func successorAgentName(predName, project string) string {
	base := strings.TrimSpace(predName)
	if base == "" {
		return spawnGenerateSpawnName(project)
	}
	// Drop a trailing random suffix so successive continuations don't accrete
	// "-ab12-cd34-…". Only strip when there's a real base left to keep.
	if i := strings.LastIndex(base, "-"); i > 0 {
		base = base[:i]
	}
	return base + "-" + spawnRandomSuffix(3)
}

// successorPrompt builds the successor's opening prompt: predecessor
// handoff_notes first (the explicit "here's where I left off" channel), then the
// predecessor's own prompt, then a minimal continuation line. The result is
// wrapped so the successor understands it is continuing a lineage, not starting
// cold.
func successorPrompt(w *WrapUp, predFields map[string]string) string {
	var seed string
	switch {
	case w != nil && strings.TrimSpace(w.HandoffNotes) != "":
		seed = strings.TrimSpace(w.HandoffNotes)
	case predFields["prompt"] != "":
		seed = strings.TrimSpace(predFields["prompt"])
	}
	if seed == "" {
		return "Continue the work of your predecessor agent. See the linked lineage for prior context."
	}
	return "You are the successor continuing a predecessor agent's work.\n\n" +
		"Handoff from your predecessor:\n" + seed
}

// isHumanShapedActor reports whether an actor identity is a real human/OIDC
// principal worth stamping as operated_by (not an empty/unknown sentinel).
func isHumanShapedActor(actor string) bool {
	a := strings.TrimSpace(actor)
	if a == "" || a == "unknown" {
		return false
	}
	return strings.HasPrefix(a, "oidc:") || strings.HasPrefix(a, "slack:") ||
		strings.Contains(a, "@")
}

// closePredecessorAsSuperseded closes the predecessor bead terminally with the
// superseded close_reason carrying the successor id, so the bridge card-chaining
// renderer links the retired card forward. Returns any close error for the
// caller to warn on (non-fatal — the successor already exists).
func closePredecessorAsSuperseded(ctx context.Context, predID, successorID, actor string) error {
	fields := map[string]string{
		"agent_state":       "done",
		"gate_satisfied_by": "operator",
		"close_reason":      supersededCloseReasonPrefix + successorID,
	}
	if actor != "" {
		return daemon.CloseBeadAs(ctx, predID, actor, fields)
	}
	return daemon.CloseBead(ctx, predID, fields)
}
