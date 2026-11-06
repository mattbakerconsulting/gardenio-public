package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var squawkNoComment bool
var squawkToMR bool

var squawkCmd = &cobra.Command{
	Use:   "squawk <message>",
	Short: "Send an informational message to the operator (alias of gb say)",
	Long: `Posts a short informational message that the operator will see in Slack.
A permanent alias of a plain 'gb say "<message>"'; see gb say for replies in
other threads, posts in channels and files.

The message is stored as a bead (type=message) and relayed to the
appropriate Slack thread by the bridge. The agent does not need to know
about Slack — it just "squawks" something.

Good for: progress updates, completion notices, findings worth flagging.
Not for: questions (use gb decision), long reports (use gb decision report),
or agent-to-agent comms (use gb mail send).

By default a squawk goes to Slack only. If you are bound to a GitLab MR and want
a specific message to also appear as a comment on that MR (e.g. a review reply
or a final summary the reviewer needs), pass --to-mr. Routine progress squawks
should NOT use --to-mr — they would flood the human review thread.`,
	GroupID: "orchestration",
	Args:    cobra.ExactArgs(1),
	RunE:    runSquawk,
}

func init() {
	squawkCmd.Flags().BoolVar(&squawkNoComment, "no-comment", false,
		"Do not auto-comment the squawk on the actor's currently-claimed bead")
	squawkCmd.Flags().BoolVar(&squawkToMR, "to-mr", false,
		"Also mirror this squawk to the MR-bound agent's GitLab MR as a comment (opt-in; default Slack-only)")
}

func runSquawk(cmd *cobra.Command, args []string) error {
	// DELIBERATELY LITERAL — no @file/@-/@@ expansion here (ka-0000003818).
	// A squawk is a short, high-frequency Slack-facing status line, not a
	// file-sourced artifact. Agents legitimately squawk text that starts with
	// "@" (e.g. "@matt heads up …"), where the leading "@" is a Slack mention,
	// not a curl-style file pointer. Silently reading a file — or, worse,
	// failing loud when "@channel" isn't a path — would surprise the common
	// case far more than the rare "I meant a file" case it would help. The
	// audit's contract is "expand OR keep literal by explicit design, never the
	// accidental middle"; squawk is explicitly literal. Sibling: `gb dm send
	// --body` (Slack DM, same reasoning). See TestSquawk_LeadingAtStoredLiteral.
	text := args[0]
	if text == "" {
		return fmt.Errorf("message text cannot be empty")
	}

	// Truncate title to 500 chars (bead title limit).
	title := text
	if len(title) > 500 {
		title = title[:497] + "..."
	}

	fields := map[string]string{
		"source_agent": actor,
		"text":         text,
	}
	// When this squawk originates inside a Slack thread, bind it to that
	// thread's (wish) head via the origin-agnostic wish-pointer field
	// thread_id, so the beads-viewer /threads timeline renders it as a
	// conversation member. The viewer resolves membership as
	// UNION(fields.thread_id == headID, parent-child closure)
	// (cmd/beads-viewer/threads.go); the triggered-by lineage stamped below is
	// NOT a membership edge, so without thread_id the squawk is invisible in
	// the thread view (ka-0000003279).
	//
	// Gated on isThreadSpawnedAgent() (both SLACK_THREAD_* env vars set) so the
	// extra head lookup runs ONLY for thread-bound agents — a non-thread
	// gardener/crew squawk adds no daemon call. lookupSlackThreadBead still
	// returns "" if the head can't be resolved, so the bind is best-effort.
	//
	// The same gate picks the Slack channel the squawk is said in, for the
	// Commons channel_id stamp (channel_stamp.go). Any other agent's squawk is
	// posted in Slack as a reply to its agent card, so in the Commons it is a
	// reply in the card's thread (agentCardPlace).
	var at commonsPlace
	if isThreadSpawnedAgent() {
		if headID := lookupSlackThreadBead(cmd.Context()); headID != "" {
			fields["thread_id"] = headID
		}
		if id, land := lookupChannelBead(cmd.Context(), os.Getenv("SLACK_THREAD_CHANNEL")); id != "" {
			at = commonsPlace{channel: id, land: land}
		}
	} else {
		at = agentCardPlace(cmd.Context())
	}

	// Create the message bead.
	//
	// The "to-mr" label is an OPT-IN mirror flag (ka-0000005748): the GitLab
	// squawk forwarder only publishes a squawk to its MR-bound agent's MR when
	// this label is present. Without it the squawk is Slack-only, so routine
	// progress narration no longer floods the human review thread.
	labels := []string{"squawk", fmt.Sprintf("from:%s", actor)}
	if squawkToMR {
		labels = append(labels, "to-mr")
	}
	beadID, err := createMessageBeadAt(cmd.Context(), beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "message",
		Kind:      "data",
		Labels:    labels,
		CreatedBy: actor,
	}, fields, at)
	if err != nil {
		return fmt.Errorf("creating message bead: %w", err)
	}

	// Stamp `triggered-by` lineage so the squawk is reachable from the agent
	// bead (and the originating trigger, if any) in the beads graph. Lineage is
	// best-effort — failures are reported on stderr but do not fail the squawk.
	writeSquawkLineage(cmd, beadID)

	// Immediately close the bead (fire-and-forget).
	if err := daemon.CloseBead(cmd.Context(), beadID, nil); err != nil {
		return fmt.Errorf("closing message bead: %w", err)
	}

	// Best-effort: auto-comment the squawk on the actor's currently-claimed
	// issue bead so squawks are discoverable from the work bead they relate to.
	if !squawkNoComment {
		autoCommentClaimedBead(cmd.Context(), beadID, text)
	}

	if jsonOutput {
		printJSON(map[string]string{"id": beadID, "status": "sent"})
	} else {
		fmt.Println("OK")
	}
	return nil
}

// writeSquawkLineage stamps `triggered-by` deps from the squawk bead onto the
// agent bead (from BOAT_AGENT_BEAD_ID) and, when distinct, the originating
// trigger bead (from BOAT_TRIGGER_BEAD_ID). Mirrors the pattern used by the
// trigger engine in internal/bridge/triggers.go writeTriggerLineage.
//
// When no agent context exists (BOAT_AGENT_BEAD_ID + KD_AGENT_ID both unset,
// i.e. a human-invoked squawk from local terminal or `! gb squawk` in Slack),
// falls back to the slack-thread bead (via SLACK_THREAD_CHANNEL/SLACK_THREAD_TS)
// or the operator alias (user) bead (via SLACK_USER_ID / git user.email) to
// avoid producing orphan type=message beads.
func writeSquawkLineage(cmd *cobra.Command, beadID string) {
	const depType = "triggered-by"

	agentBeadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if agentBeadID == "" {
		agentBeadID = os.Getenv("KD_AGENT_ID")
	}
	triggerBeadID := os.Getenv("BOAT_TRIGGER_BEAD_ID")

	if agentBeadID != "" {
		if err := daemon.AddDependency(cmd.Context(), beadID, agentBeadID, depType, actor); err != nil {
			fmt.Fprintf(os.Stderr, "squawk: warn: failed to add triggered-by dep to agent bead %s: %v\n", agentBeadID, err)
		}
	}
	if triggerBeadID != "" && triggerBeadID != agentBeadID {
		if err := daemon.AddDependency(cmd.Context(), beadID, triggerBeadID, depType, actor); err != nil {
			fmt.Fprintf(os.Stderr, "squawk: warn: failed to add triggered-by dep to trigger bead %s: %v\n", triggerBeadID, err)
		}
	}

	if agentBeadID == "" && triggerBeadID == "" {
		if parentID := resolveHumanSquawkParent(cmd.Context()); parentID != "" {
			if err := daemon.AddDependency(cmd.Context(), beadID, parentID, depType, actor); err != nil {
				fmt.Fprintf(os.Stderr, "squawk: warn: failed to add triggered-by dep to %s: %v\n", parentID, err)
			}
		}
	}
}

// resolveHumanSquawkParent picks a parent bead for a squawk created with no
// agent context. It prefers the slack-thread bead (identified by
// SLACK_THREAD_CHANNEL/SLACK_THREAD_TS) and falls back to the operator alias
// (user) bead matched by SLACK_USER_ID or git user.email. Returns "" when
// nothing resolves; callers keep the legacy no-op behaviour.
func resolveHumanSquawkParent(ctx context.Context) string {
	if id := lookupSlackThreadBead(ctx); id != "" {
		return id
	}
	return lookupOperatorAliasBead(ctx)
}

// resolveThreadCoords returns the (channel, ts) of the Slack thread this agent
// belongs to, preferring the SLACK_THREAD_CHANNEL/SLACK_THREAD_TS env vars and
// falling back to the agent bead's slack_thread_channel/slack_thread_ts fields.
//
// The fallback is essential for POOL-ASSIGNED (prewarmed) agents: a prewarmed
// pod is created before it knows which thread it will serve, so its env vars
// are baked empty and never updated on the running pod. The controller does
// write the coords onto the agent bead at assign time, so the bead is the only
// place a prewarmed agent can recover them. This mirrors resolveSlackEnv() in
// slack.go, which `gb slack reply` already uses. See bug ka-0000006843.
func resolveThreadCoords(ctx context.Context) (channel, ts string) {
	channel = os.Getenv("SLACK_THREAD_CHANNEL")
	ts = os.Getenv("SLACK_THREAD_TS")
	if channel != "" && ts != "" {
		return channel, ts
	}
	beadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if beadID == "" {
		beadID = os.Getenv("KD_AGENT_ID")
	}
	if beadID == "" || daemon == nil {
		return channel, ts
	}
	bead, err := daemon.GetBead(ctx, beadID)
	if err != nil || bead == nil {
		return channel, ts
	}
	return threadCoordsFromBead(bead)
}

// threadCoordsFromBead returns the Slack thread (channel, ts), preferring the
// SLACK_THREAD_CHANNEL/SLACK_THREAD_TS env vars and falling back per-field to
// the supplied agent bead's slack_thread_channel/slack_thread_ts fields.
//
// It is the shared fallback core used by both resolveThreadCoords (which fetches
// the agent bead itself) and slackThreadCoordsFromBead (which already holds the
// bead). Keeping the env→bead precedence in one place guarantees every
// thread-coord reader recovers coords on a prewarmed (pool-assigned) pod, whose
// env is baked empty at create time and only the bead carries the coords the
// controller stamps at assign time. See bug ka-0000006843.
func threadCoordsFromBead(bead *beadsapi.BeadDetail) (channel, ts string) {
	channel = os.Getenv("SLACK_THREAD_CHANNEL")
	ts = os.Getenv("SLACK_THREAD_TS")
	if channel != "" && ts != "" {
		return channel, ts
	}
	if bead == nil || bead.Fields == nil {
		return channel, ts
	}
	if channel == "" {
		channel = bead.Fields["slack_thread_channel"]
	}
	if ts == "" {
		ts = bead.Fields["slack_thread_ts"]
	}
	return channel, ts
}

// lookupSlackThreadBead returns the gd- id of the bead representing the
// current Slack thread, or "" when the thread coordinates cannot be resolved
// (neither env vars nor the agent bead carry them) or the bead does not exist
// yet. The label format mirrors bridge.threadBeadLabel.
func lookupSlackThreadBead(ctx context.Context) string {
	channel, ts := resolveThreadCoords(ctx)
	if channel == "" || ts == "" {
		return ""
	}
	label := "slack-thread:" + channel + ":" + ts
	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{label},
		Limit:  1,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "squawk: warn: failed to look up slack-thread bead %s: %v\n", label, err)
		return ""
	}
	if result == nil || len(result.Beads) == 0 {
		return ""
	}
	return result.Beads[0].ID
}

// lookupOperatorAliasBead returns the gd- id of the type=user/kind=config bead
// that matches the current human operator, derived from SLACK_USER_ID (matched
// against slack_id) or git user.email (matched against email). Returns "" when
// no identifier resolves or no user bead matches.
func lookupOperatorAliasBead(ctx context.Context) string {
	var candidates []string
	if u := os.Getenv("SLACK_USER_ID"); u != "" {
		candidates = append(candidates, u)
	}
	if email := gitUserEmail(); email != "" {
		candidates = append(candidates, email)
	}
	if len(candidates) == 0 {
		return ""
	}

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "squawk: warn: failed to look up operator alias bead: %v\n", err)
		return ""
	}
	if result == nil {
		return ""
	}
	for _, b := range result.Beads {
		for _, c := range candidates {
			if b.Fields["slack_id"] == c || b.Fields["email"] == c {
				return b.ID
			}
		}
	}
	return ""
}

func gitUserEmail() string {
	out, err := exec.Command("git", "config", "user.email").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// autoCommentClaimedBead posts a best-effort comment on the actor's
// currently-claimed issue bead (status=in_progress, assignee=actor,
// kind=issue), embedding the squawk id, agent, and originating slack thread
// (from SLACK_THREAD_CHANNEL / SLACK_THREAD_TS) as a footer. Failures are
// logged to stderr and never fail the squawk.
func autoCommentClaimedBead(ctx context.Context, squawkBeadID, text string) {
	if actor == "" || actor == "unknown" {
		return
	}

	result, err := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Kinds:    []string{"issue"},
		Statuses: []string{"in_progress"},
		Assignee: actor,
		Limit:    2,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "squawk: warn: failed to list claimed beads: %v\n", err)
		return
	}
	if result == nil || len(result.Beads) == 0 {
		return
	}
	claimed := result.Beads[0]
	if claimed == nil || claimed.ID == "" {
		return
	}

	channel, ts := resolveThreadCoords(ctx)
	var footer string
	if channel != "" || ts != "" {
		footer = fmt.Sprintf("\n\n_squawk %s from %s [slack-thread channel=%s ts=%s]_", squawkBeadID, actor, channel, ts)
	} else {
		footer = fmt.Sprintf("\n\n_squawk %s from %s_", squawkBeadID, actor)
	}

	if err := daemon.AddComment(ctx, claimed.ID, actor, text+footer); err != nil {
		fmt.Fprintf(os.Stderr, "squawk: warn: failed to auto-comment on claimed bead %s: %v\n", claimed.ID, err)
	}
}
