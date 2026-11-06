package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// closeAgentBeadOnExit closes the agent bead so the reconciler does not
// respawn the pod after Claude Code exits. Best-effort: failures are logged
// but do not prevent the stop from proceeding.
//
// When the agent never submitted a structured wrap-up (e.g. it exited without
// `gb stop`, or stopped without a --wrapup — ka-0000000101), this synthesizes a
// minimal fallback wrapup so the Slack agent card still renders a tl;dr headline
// instead of metadata-only. A real wrap-up already on the bead always takes
// precedence and is never overwritten.
func closeAgentBeadOnExit(reason string) {
	// Laptop guard (gd-3s1xZJyeNI): on a laptop the Stop hook fires at each
	// turn end while gb agent start and coop keep running, and that process
	// closes the bead when the session really ends. Closing here marked a live
	// agent done. Pods never set the pid, so they are unaffected.
	if pid, alive := localAgentStartAlive(); alive {
		fmt.Fprintf(os.Stderr, "[stop-gate] laptop session: gb agent start (pid %d) is still running and closes the agent bead at exit; not closing (%s)\n", pid, reason)
		return
	}

	agentBeadID, _ := resolveAgentID("")
	if agentBeadID == "" {
		agentBeadID = resolveAgentByActor(context.Background(), actor)
	}
	if agentBeadID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Frozen-awaiting-thaw guard (ka-0000002104): an OPEN bead at
	// agent_state=stopping is a SUSPENDED agent — suspendAgent (stop.go) parked
	// it as the thaw target and the controller is checkpointing/tearing down the
	// pod. The suspend path satisfies the decision gate but a Stop-hook firing
	// inside the cooldown window still lands here; closing the bead would
	// destroy the frozen signal and defeat the thaw (same clobber as the
	// init_agent_loop teardown path). Leave the bead as the suspend parked it.
	if bead, err := daemon.GetBead(ctx, agentBeadID); err == nil && bead != nil &&
		bead.Status != "closed" && bead.Fields["agent_state"] == "stopping" {
		fmt.Fprintf(os.Stderr, "[stop-gate] agent bead %s is frozen awaiting thaw (agent_state=stopping); not closing\n", agentBeadID)
		return
	}

	// P0 ka-0000003464 (items 2 & 3): a stop-gate close on the cooldown /
	// escape-hatch / rate-limited path fires when Claude Code exited WITHOUT a
	// clean `gb done` (`gb stop` blocks up-front on claimed in-progress work, so a
	// genuine finished stop never reaches here still holding a task). During an
	// account-exhaustion storm this closes a *working* agent mid-turn, orphaning
	// its claimed task in in_progress so the formula/queue stalls behind it.
	// Return that task to the ready queue before we tear the agent down.
	reopened, lookupFailed := reopenOrphanedClaimedTask(ctx, reason)

	// An involuntary mid-work death is NOT an idle-out. When the agent still had
	// real work in flight — a claimed task we had to reopen, or tool calls
	// already recorded on the bead — stamp the close reason `*_involuntary` so
	// neither the synthesized wrap-up nor the Slack card mislabels it "idled
	// out" (item 3a). A genuine zero-work idle keeps the original phrasing.
	//
	// lookupFailed forces didWork=true even with zero recorded tool calls: a
	// failed ListAssignedTask lookup means we never actually confirmed "no
	// claim" — treating it as a clean idle would repeat the gd-k3kzZeIlT0
	// regression this retry/signal was added to close.
	//
	// ka-0000008254 NOTE: the beadToolCallCount>0 fallback is deliberately KEPT
	// here (the voluntary stop-gate path), and dropped only in the
	// `context_cancelled` teardown path (init_agent_loop.go). The observed
	// gardener-recycle false-positives were 100% `context_cancelled` family. This
	// path fires on a stop-gate force-exit (cooldown/escape_hatch/rate_limited)
	// — Claude exited mid-turn WITHOUT `gb done` — which is a genuine lost-work
	// signal (e.g. crew rotation-churn) the metric wants to keep, and no gardener
	// reaches it by cleanly stopping.
	didWork := reopened != "" || lookupFailed
	if !didWork {
		if bead, err := daemon.GetBead(ctx, agentBeadID); err == nil && bead != nil {
			didWork = beadToolCallCount(bead) > 0
		}
	}
	closeReason := fmt.Sprintf("stop_gate_%s: claude exited without gb done", reason)
	if didWork {
		closeReason = fmt.Sprintf("stop_gate_%s_involuntary: claude exited mid-work without gb done", reason)
		if reopened != "" {
			closeReason += "; reopened " + reopened
		}
		if lookupFailed {
			closeReason += "; claim lookup failed, unverified"
		}
	}

	fields := map[string]string{
		"agent_state":  "done",
		"close_reason": closeReason,
	}

	if err := closeAgentBeadWithFallback(ctx, agentBeadID, reason, closeReason, fields); err != nil {
		fmt.Fprintf(os.Stderr, "[stop-gate] warning: failed to close agent bead %s: %v\n", agentBeadID, err)
	} else {
		fmt.Fprintf(os.Stderr, "[stop-gate] closed agent bead %s (%s)\n", agentBeadID, reason)
	}
}

// reopenOrphanedClaimedTask returns a claimed-but-unfinished issue task to the
// ready queue when the agent is being torn down by the stop gate without a
// clean `gb done`. It looks up the in_progress issue-kind bead this agent still
// holds (the same query `gb stop` and the claim-reminder use), reopens it
// (status=open, assignee cleared) and drops an audit comment, so `gb ready`
// re-surfaces the work for another agent instead of leaving it orphaned in
// in_progress forever (P0 ka-0000003464 — account-exhaustion storm). A claim
// with awaits_release=true is skipped: its release closes it (gd-aaGqLb2cUk).
//
// Returns the reopened task ID (or "") so the caller can fold it into the
// agent's close reason, plus lookupFailed=true when ListAssignedTask could not
// be verified at all (as opposed to a clean "holds no claim" empty result).
// Best-effort throughout: any lookup/update failure is logged and swallowed —
// the agent close must still proceed — but a lookup failure must never be
// silently folded into "no claim": gd-k3kzZeIlT0 fixed the ListAssignedTask
// error path to log a warning, but the warning goes to stderr and dies with
// the pod, and the caller still treated err!=nil the same as task==nil, so a
// transient blip during SIGTERM teardown (the pod's network path can already
// be degrading) still produced a `context_cancelled_unclaimed` close on a task
// that was genuinely still claimed — a regression of the exact gap that bug
// was filed to close. Retrying absorbs the common transient case; lookupFailed
// lets callers stamp a close reason that says "could not verify" rather than
// asserting "no claimed issue" when that was never actually confirmed.
func reopenOrphanedClaimedTask(ctx context.Context, reason string) (reopenedID string, lookupFailed bool) {
	if daemon == nil || actor == "" || actor == "unknown" {
		return "", false
	}
	task, err := lookupReopenableTask(ctx)
	if err != nil {
		logInit("warning: reopenOrphanedClaimedTask: ListAssignedTask(%s) failed after %d attempts: %v",
			actor, reopenLookupMaxAttempts, err)
		return "", true
	}
	if task == nil {
		return "", false
	}
	// ListAssignedTasks already scopes to status=in_progress + kind=issue for
	// this assignee; re-check defensively so we never reopen closed/done work.
	if task.Status != "in_progress" {
		return "", false
	}

	// Attempt cap (ka-0000002985 DEFECT 3). The reopen_attempts counter lives on
	// the ISSUE task bead and survives across respawns, so it counts total
	// involuntary-death reopens of THIS work item — not per-pod. Beyond the cap,
	// stop auto-dispatching and PARK the task for a human instead of returning it
	// to the queue to fail the same way again (the 2026-08 site-deployment loop:
	// 137 reopens of 3 items, no cap, no backoff).
	attempts := beadsapi.ParseReopenAttempts(task.Fields)
	next := attempts + 1
	if next > maxReopenAttempts() {
		parkCappedTask(ctx, task, attempts, reason)
		return "", false
	}

	openStatus := "open"
	clearedAssignee := ""
	reopenReq := beadsapi.UpdateBeadRequest{
		Status:   &openStatus,
		Assignee: &clearedAssignee,
		// Persist the incremented attempt count in the SAME reopen patch so the
		// next involuntary death of this task sees the running total.
		Fields: map[string]any{
			beadsapi.ReopenAttemptsField: strconv.Itoa(next),
			beadsapi.ReapedAssigneeField: task.Assignee,
		},
	}

	// Resume-chain handoff (ka-0000003160): record the S3 session key THIS pod
	// was using onto the task bead, IN THE SAME reopen PATCH (atomic — one
	// daemon write/event), so the next pod the gardener spawns for this
	// (same-task_id, new-bead-id) work can follow it back to the canonical
	// transcript prefix instead of cold-starting under its fresh bead ID.
	// currentS3SessionKeyForTask returns the INHERITED canonical key when this
	// pod was itself a resume, so a chain of N respawns all point at the ONE
	// original prefix (never a moving target). Gated on an S3 bucket being
	// configured; a no-op otherwise leaves the reopen behavior unchanged.
	var handoffKey string
	if os.Getenv("COOP_S3_BUCKET") != "" {
		if handoffKey = currentS3SessionKeyForTask(ctx); handoffKey != "" {
			reopenReq.Fields[resumeS3KeyField] = handoffKey
		}
	}

	if err := daemon.UpdateBead(ctx, task.ID, reopenReq); err != nil {
		fmt.Fprintf(os.Stderr, "[stop-gate] warning: failed to reopen orphaned task %s: %v\n", task.ID, err)
		return "", false
	}
	if handoffKey != "" {
		fmt.Fprintf(os.Stderr, "[stop-gate] stamped resume_s3_key=%s on %s for transcript continuity\n", handoffKey, task.ID)
	}

	comment := fmt.Sprintf(
		"Reopened by stop-gate: agent `%s` exited (%s) with this task still in_progress "+
			"and no `gb done` — an involuntary mid-work death (account exhaustion/rotation, "+
			"or pod teardown). Returned to the ready queue for another agent. (P0 ka-0000003464)",
		actor, reason)
	if err := daemon.AddComment(ctx, task.ID, "gardenio", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[stop-gate] note: reopened %s but audit comment failed: %v\n", task.ID, err)
	}
	fmt.Fprintf(os.Stderr, "[stop-gate] reopened orphaned in_progress task %s (agent %s exited: %s) (attempt %d/%d)\n",
		task.ID, actor, reason, next, maxReopenAttempts())
	return task.ID, false
}

// lookupReopenableTask returns the actor's first in_progress claim that is not
// awaiting its release (nil when there is none), retrying the lookup up to
// reopenLookupMaxAttempts times. A task awaiting its release is done, not
// orphaned: the deployment that carries it closes it from in_progress
// (gd-aaGqLb2cUk).
func lookupReopenableTask(ctx context.Context) (*beadsapi.BeadDetail, error) {
	var tasks []*beadsapi.BeadDetail
	var err error
retryLoop:
	for attempt := 1; attempt <= reopenLookupMaxAttempts; attempt++ {
		tasks, err = daemon.ListAssignedTasks(ctx, actor)
		if err == nil {
			break retryLoop
		}
		if attempt < reopenLookupMaxAttempts {
			select {
			case <-ctx.Done():
				break retryLoop // context expiring — stop retrying, report the last error below
			case <-time.After(reopenLookupRetryDelay):
			}
		}
	}
	if err != nil {
		return nil, err
	}
	for _, t := range tasks {
		if !beadsapi.AwaitsRelease(t.Fields) {
			return t, nil
		}
	}
	return nil, nil
}

// reopenLookupMaxAttempts and reopenLookupRetryDelay bound the ListAssignedTask
// retry in reopenOrphanedClaimedTask. Overridable in tests to keep the retry
// loop fast; production defaults absorb a short transient blip without
// meaningfully eating into the 5-10s context budget callers pass in.
var (
	reopenLookupMaxAttempts = 3
	reopenLookupRetryDelay  = 200 * time.Millisecond
)

// maxReopenAttempts is the reopen cap, defaulting to beadsapi.MaxReopenAttempts
// and overridable via GB_MAX_REOPEN_ATTEMPTS (a positive integer). A non-positive
// or malformed override falls back to the default rather than disabling the cap —
// the cap is a safety valve and must not be silently switched off by a bad value.
func maxReopenAttempts() int {
	if v := strings.TrimSpace(os.Getenv("GB_MAX_REOPEN_ATTEMPTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return beadsapi.MaxReopenAttempts
}

// parkCappedTask stops auto-dispatching an issue task that has hit the reopen cap
// and surfaces it for a human (ka-0000002985 DEFECT 3). It sets status=blocked
// (invisible to BOTH reopen scanners — this one and the stalereaper backstop scan
// status=in_progress; `gb ready` scans status=open — so the loop stops with no
// scheduler change), clears the assignee, stamps the cap fields, and leaves an
// audit comment. The comment auto-forwards to Slack (bridge comment forwarding),
// so a human is notified without the dying pod having to file an escalation.
// attempts is the count BEFORE this (capping) attempt. Best-effort throughout.
func parkCappedTask(ctx context.Context, task *beadsapi.BeadDetail, attempts int, reason string) {
	blocked := "blocked"
	clearedAssignee := ""
	req := beadsapi.UpdateBeadRequest{
		Status:   &blocked,
		Assignee: &clearedAssignee,
		Fields: map[string]any{
			beadsapi.ReopenAttemptsField:  strconv.Itoa(attempts + 1),
			beadsapi.ReopenCappedField:    "true",
			beadsapi.ReopenCapReasonField: reason,
			beadsapi.ReapedAssigneeField:  task.Assignee,
		},
	}
	if err := daemon.UpdateBead(ctx, task.ID, req); err != nil {
		fmt.Fprintf(os.Stderr, "[stop-gate] warning: failed to park capped task %s: %v\n", task.ID, err)
		return
	}
	comment := fmt.Sprintf(
		":no_entry: *Reopen cap reached* — task `%s` has been reopened %d times after involuntary agent deaths "+
			"(last: %s) and keeps failing the same way. Auto-dispatch STOPPED: parked to `status=blocked` so the "+
			"scheduler no longer respawns against it. A repeated same-way failure almost always means the WORK is "+
			"un-runnable as specified (a project-wide credential/auth failure, a missing project record, or a bad task "+
			"spec) — requeuing a fresh agent cannot fix that. A human should diagnose the root cause, then set "+
			"`status=open` to resume. (ka-0000002985 DEFECT 3)",
		task.ID, attempts+1, reason)
	if err := daemon.AddComment(ctx, task.ID, "gardenio", comment); err != nil {
		fmt.Fprintf(os.Stderr, "[stop-gate] note: parked %s but audit comment failed: %v\n", task.ID, err)
	}
	fmt.Fprintf(os.Stderr, "[stop-gate] PARKED task %s after %d reopen attempts (cap %d reached): %s\n",
		task.ID, attempts+1, maxReopenAttempts(), reason)
}

// closeAgentBeadWithFallback closes an agent bead, first synthesizing a minimal
// fallback wrap-up from (reason, closeReason) when the bead carries none — so
// the Slack agent card renders a tl;dr headline instead of metadata-only. This
// is the shared close path for every in-pod terminal close (stop-gate exit,
// restart-loop stop_requested, idle-TTL despawn — Fix A, ka-0000007639). A real
// wrap-up already on the bead ALWAYS wins and is never overwritten; only empty
// wrap-ups are filled. Synthesizing the fallback is best-effort: a fetch
// failure just skips it (the close itself still proceeds). fields supplies the
// close fields (agent_state, close_reason, …); the synthesized wrap-up is
// merged in under WrapUpFieldName only when missing.
func closeAgentBeadWithFallback(ctx context.Context, agentBeadID, reason, closeReason string, fields map[string]string) error {
	if fields == nil {
		fields = map[string]string{}
	}
	if bead, err := daemon.GetBead(ctx, agentBeadID); err == nil && bead != nil {
		if strings.TrimSpace(bead.Fields[WrapUpFieldName]) == "" {
			if wrapup := synthesizeFallbackWrapUp(reason, closeReason, bead); wrapup != "" {
				fields[WrapUpFieldName] = wrapup
			}
		}
	}
	return daemon.CloseBead(ctx, agentBeadID, fields)
}

// stopGateFallbackTLDR maps a stop-gate close reason to a one-line tl;dr for
// the synthesized fallback wrap-up. These cover the paths in the stop-gate
// command (cooldown / escape_hatch / rate_limited) plus the loop teardown
// reasons (context_cancelled, etc.) that also flow a reason through here.
//
// toolCalls is the agent bead's accumulated total_tool_calls at close time.
// When it is > 0 the agent clearly did real work before the turn ended without
// an explicit `gb stop`, so the "idled out" phrasing (which implies the agent
// did nothing) is misleading — emit an honest "closed without a wrap-up after
// N tool calls" headline instead (ka-0000000083). A genuine zero-work idle
// keeps the original idled-out string.
func stopGateFallbackTLDR(reason string, toolCalls int) string {
	if toolCalls > 0 {
		// The agent did real work; describe that honestly rather than
		// claiming it idled out. Keep the reason in parentheses for context.
		return fmt.Sprintf("Session closed (%s) without an explicit wrap-up after %d tool call%s",
			reason, toolCalls, plural(toolCalls))
	}
	switch reason {
	case "cooldown":
		return "Session idled out without an explicit wrap-up"
	case "escape_hatch":
		return "Session stopped after repeated stop-gate blocks without an explicit wrap-up"
	case "rate_limited":
		return "Session ended rate-limited without an explicit wrap-up"
	case "context_cancelled":
		return "Pod terminated before an explicit wrap-up was submitted"
	case "stop_requested":
		return "Session ended on a requested stop without an explicit wrap-up"
	case "idle_ttl_expired":
		return "Session idled out (warm window expired) without an explicit wrap-up"
	default:
		return fmt.Sprintf("Session closed (%s) without an explicit wrap-up", reason)
	}
}

// plural returns "s" when n != 1, for grammatical tldr phrasing.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// beadToolCallCount reads the accumulated total_tool_calls field off an agent
// bead, returning 0 when absent or unparseable. The field is written as a
// float-ish integer string by the session-usage increment path; parse leniently.
func beadToolCallCount(bead *beadsapi.BeadDetail) int {
	if bead == nil {
		return 0
	}
	raw := strings.TrimSpace(bead.Fields["total_tool_calls"])
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	// Tolerate a float-formatted value (e.g. "49.0" / "49").
	if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
		return int(f)
	}
	return 0
}

// beadUsageInt reads an accumulated usage counter field off an agent bead,
// returning 0 when absent or unparseable. Session-usage increments write these
// as float-ish integer strings ("1", "1.0"); parse leniently, matching
// beadToolCallCount's tolerance.
func beadUsageInt(bead *beadsapi.BeadDetail, field string) int {
	if bead == nil {
		return 0
	}
	raw := strings.TrimSpace(bead.Fields[field])
	if raw == "" {
		return 0
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(raw, 64); err == nil && f > 0 {
		return int(f)
	}
	return 0
}

// beadZeroInferenceTurn reports whether an agent bead's usage fields prove the
// run never completed a single inference turn — a hard platform failure (model
// outage or a pod-startup infra fault before first inference) rather than an
// agent that did work and forgot to wrap up. This is the write-side twin of the
// bridge render classifier (zeroInferenceTurn in bot_agents_wrapup.go): a
// request was attempted (total_request_count >= 1) yet no context reached the
// model and no tool ran (total_input_tokens == 0 && total_tool_calls == 0) —
// the exact state the bug reported (ka-0000000315). Requiring a counted request
// keeps a bead whose usage never flushed (all fields absent → 0) out of the
// classification, so the ordinary idle/teardown headline is preserved for it.
func beadZeroInferenceTurn(bead *beadsapi.BeadDetail) bool {
	if beadUsageInt(bead, "total_request_count") < 1 {
		return false
	}
	return beadUsageInt(bead, "total_input_tokens") == 0 &&
		beadUsageInt(bead, "total_tool_calls") == 0
}

// synthesizeFallbackWrapUp builds a minimal wrap-up JSON ({tldr, ...}) for an
// agent bead that is being closed without one. The tl;dr is derived from the
// close reason; the bead title is appended for context when present. Returns
// "" only if a tl;dr could not be derived (it always can, so this is defensive).
func synthesizeFallbackWrapUp(reason, closeReason string, bead *beadsapi.BeadDetail) string {
	toolCalls := beadToolCallCount(bead)
	tldr := stopGateFallbackTLDR(reason, toolCalls)
	// Zero-inference-turn platform failure (ka-0000000315): when the usage fields
	// prove the agent never got a model response (a request was attempted but no
	// context reached the model and no tool ran), the honest headline names the
	// platform, not the agent — the ordinary "…without an explicit wrap-up"
	// phrasing reads as agent laziness and points away from the real cause (the
	// 2026-07-25 opus-5 outage). Mark it platform_failure so the render side can
	// badge the card even if the usage fields are later reconciled away. This is
	// the mirror of the toolCalls>0 honest-work branch above (ka-0000000083).
	platformFailure := beadZeroInferenceTurn(bead)
	if platformFailure {
		tldr = "Platform failure — the agent never got an inference turn (no model response before teardown)"
	} else if toolCalls == 0 && strings.Contains(closeReason, "_involuntary") {
		// An involuntary mid-work death (a claimed task we had to reopen, account
		// churn, or pod teardown) must never read as an idle-out even when the
		// tool-call counter had not flushed yet (toolCalls==0). closeAgentBeadOnExit
		// stamps `_involuntary` on the close reason for exactly this case — honor it
		// here so the synthesized headline matches the close reason (ka-0000003464
		// item 3a). When toolCalls>0, stopGateFallbackTLDR already emits an honest
		// "after N tool calls" headline, so leave that path alone.
		tldr = fmt.Sprintf("Session ended involuntarily mid-work (%s) without an explicit wrap-up", reason)
	}
	if title := strings.TrimSpace(beadDisplayTitle(bead)); title != "" {
		tldr = fmt.Sprintf("%s — %s", tldr, title)
	}

	custom := map[string]string{
		"synthesized":  "true",
		"close_reason": closeReason,
	}
	if platformFailure {
		custom["platform_failure"] = "true"
	}
	wu := WrapUp{
		TLDR:            tldr,
		Accomplishments: tldr,
		Custom:          custom,
	}
	data, err := json.Marshal(wu)
	if err != nil {
		return ""
	}
	return string(data)
}

// beadDisplayTitle returns a short human label for the agent bead — the agent
// name when set (thread/crew identity), otherwise the bead title. Used to give
// the synthesized fallback tl;dr some specificity beyond the generic reason.
func beadDisplayTitle(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	if agent := strings.TrimSpace(bead.Fields["agent"]); agent != "" {
		return agent
	}
	return strings.TrimSpace(bead.Title)
}
