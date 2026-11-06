package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/mailproto"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"

	"github.com/slack-go/slack"
)

// warmPoolResumeEnabledEnv is the bridge kill switch for the warm-pool
// coop-resume graft (ka-0000000983). When true, respawnThreadAgent hands the
// prior agent's S3 session key to the pool so the warm pod resumes that
// transcript; otherwise a warm follow-up starts a fresh session.
const warmPoolResumeEnabledEnv = "WARM_POOL_RESUME_ENABLED"

// warmPoolResumeEnabled reports whether the warm-pool coop-resume graft is on.
// Read on each respawn so the Deployment env can flip it; unset or unparseable
// means off.
func warmPoolResumeEnabled() bool {
	v := strings.TrimSpace(os.Getenv(warmPoolResumeEnabledEnv))
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	return err == nil && enabled
}

// resolveWarmResumeS3Key returns the S3 session key whose transcript a warm
// follow-up assignment should resume, or "" to start a fresh warm session
// (always "" unless WARM_POOL_RESUME_ENABLED). Mirrors coop's
// currentS3SessionKeyForTask: the s3_session_key field, else the prior agent
// bead's own id (COOP_S3_SESSION_KEY=<agent-bead-id>). priorBead may be nil.
func (b *Bot) resolveWarmResumeS3Key(priorBead *beadsapi.BeadDetail, agentName string) string {
	if !warmPoolResumeEnabled() {
		return ""
	}
	if priorBead == nil {
		return ""
	}
	key := strings.TrimSpace(priorBead.Fields["s3_session_key"])
	if key == "" {
		key = strings.TrimSpace(priorBead.ID)
	}
	if key == "" {
		b.logger.Info("warm-pool resume: no prior S3 key resolvable — fresh warm session",
			"agent", agentName)
		return ""
	}
	b.logger.Info("warm-pool resume: grafting prior transcript onto warm pod",
		"agent", agentName, "resume_s3_key", key)
	return key
}

// killWaitPoll / killWaitMax bound how long killAgent waits for the agent to
// act on a control:kill mail before closing the bead anyway.
var (
	killWaitPoll = 2 * time.Second
	killWaitMax  = 30 * time.Second
)

// errAgentNotBead is a kill or restart held to one agent bead whose name now
// resolves to another (a same-named agent in another land, or a successor).
var errAgentNotBead = errors.New("agent name resolves to another bead")

// killAgentBead closes an agent bead and compacts its Slack card (/kill, Clear,
// the Commons). Unless force, it first sends control:kill and waits up to
// killWaitMax, then hard-closes regardless. A non-empty beadID holds the kill
// to that bead: if agentName resolves elsewhere, it returns errAgentNotBead.
func (b *Bot) killAgentBead(ctx context.Context, agentName, beadID string, force bool) error {
	agentName = extractAgentName(agentName)

	// WithoutCancel: Slack's slash-command ctx expires after ~3s; it keeps ctx's
	// values so the caller's actor still reaches the daemon (gd-jpzPamBtO3).
	killCtx, killCancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer killCancel()

	bead, err := b.daemon.FindAgentBead(killCtx, agentName)
	if err != nil {
		return fmt.Errorf("find agent bead: %w", err)
	}
	if beadID != "" && bead.ID != beadID {
		return fmt.Errorf("%w: %q is %s, not %s", errAgentNotBead, agentName, bead.ID, beadID)
	}

	// A paused laptop agent's machine is offline: no one would read the
	// control:kill mail, so close it at once instead of waiting killWaitMax.
	if !force && !deadAgentStates[bead.Fields["agent_state"]] && bead.Fields["agent_state"] != "paused" {
		b.sendControlKill(killCtx, agentName, bead.ID)
	}

	if err := b.daemon.CloseBead(killCtx, bead.ID, nil); err != nil {
		return fmt.Errorf("close agent bead: %w", err)
	}

	// Compress the agent card to a compact single-line format.
	b.mu.Lock()
	ref, hasCard := b.agentCards[agentName]
	if hasCard {
		delete(b.agentPending, agentName)
		delete(b.agentPodName, agentName)
		delete(b.agentImageTag, agentName)
		delete(b.agentRole, agentName)
		delete(b.agentProject, agentName)
	}
	b.mu.Unlock()

	if hasCard {
		// Full rollup, not just cost: the compact card shows turns/context on its
		// one usage line (ka-0000003171). fetchAgentRollup is status-agnostic, so
		// the just-closed bead still resolves.
		th := b.themeFor(killCtx, ref.ChannelID)
		blocks := buildCompactAgentCardBlocks(th, agentName, "done", b.fetchAgentRollup(killCtx, th, agentName).Usage, nil)
		_, _, _, err := b.api.UpdateMessageContext(killCtx, ref.ChannelID, ref.Timestamp,
			slack.MsgOptionText(fmt.Sprintf("Agent: %s (done)", extractAgentName(agentName)), false),
			slack.MsgOptionBlocks(blocks...),
		)
		if err != nil {
			b.logger.Error("kill agent: failed to compress card", "agent", agentName, "error", err)
		}
	}

	// Clean up any thread→agent mappings for this agent.
	if b.state != nil {
		_ = b.state.RemoveThreadAgentByAgent(agentName)
	}

	return nil
}

// tryMicrovmResume asks the controller to resume a suspended MicroVM thread
// agent's VM in place (POST /api/v1/microvm/resume; plan ka-0000004518). It
// returns true only when the controller confirms the resume; any other outcome
// is false and the caller cold-respawns. Resume is idempotent, so false is
// always safe.
func (b *Bot) tryMicrovmResume(ctx context.Context, agentName string) bool {
	if b.controllerURL == "" {
		return false
	}
	reqBody, err := json.Marshal(map[string]string{"agent": extractAgentName(agentName)})
	if err != nil {
		return false
	}
	resumeURL := strings.TrimRight(b.controllerURL, "/") + "/api/v1/microvm/resume"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, resumeURL, bytes.NewReader(reqBody))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	setControllerAPIAuth(req)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		b.logger.Debug("microvm resume request failed", "agent", agentName, "error", err)
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b.logger.Debug("microvm resume returned non-200", "agent", agentName, "status", resp.StatusCode)
		return false
	}
	var result struct {
		Resumed   bool   `json:"resumed"`
		MicrovmID string `json:"microvm_id"`
		Reason    string `json:"reason"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		b.logger.Warn("microvm resume: failed to decode response", "agent", agentName, "error", err)
		return false
	}
	if !result.Resumed {
		b.logger.Debug("microvm resume: controller reports not resumed",
			"agent", agentName, "reason", result.Reason)
		return false
	}
	return true
}

// respawnThreadAgent re-creates a dead thread agent's bead under the same name
// so coop --resumes its session and workspace when a thread reply arrives.
// triggerText goes into the description so the agent knows why it woke.
func (b *Bot) respawnThreadAgent(ctx context.Context, channel, threadTS, agentName, triggerText, userID string) {
	b.respawnThreadAgentAt(ctx, b.slackThreadAt(channel, threadTS), agentName, triggerText, userID)
}

// respawnThreadAgentAt is respawnThreadAgent for any thread: a Slack thread,
// or a Commons thread with no Slack copy, known by its head alone (slice 4d).
// Without a Slack copy nothing is posted, streamed or set on a status bar, the
// notes go to at.Say, the project falls back to the head's land, and the
// channel's role and RuntimeClass come from its channel bead.
func (b *Bot) respawnThreadAgentAt(ctx context.Context, at threadAt, agentName, triggerText, userID string) {
	channel, threadTS := at.Channel, at.TS
	th := b.themeFor(ctx, channel)
	agentName = extractAgentName(agentName)

	// A respawn mints a new pod, so a --model in the wake-up text is honored and
	// wins over priorModel; an unrecognized value is warned and dropped. Flags
	// are stripped from triggerText so they do not leak into the Description.
	respawnModelOverride := ""
	// A resumed session cannot change agent CLI, so --harness is honored only if
	// it matches the prior harness, and --model resolves against that harness
	// (ADR 0004 section 6).
	priorHarnessOverride := ""
	priorHarnessResolved := ""
	if pb := b.priorAgentBead(ctx, agentName); pb != nil {
		priorHarnessOverride = strings.TrimSpace(pb.Fields["harness"])
		priorHarnessResolved = strings.TrimSpace(pb.Fields["resolved_harness"])
	}
	if rawModel, rawHarness, stripped := parseModelHarnessFlags(triggerText); rawModel != "" || rawHarness != "" {
		triggerText = stripped
		priorHarness := priorHarnessOverride
		if priorHarness == "" {
			priorHarness = priorHarnessResolved
		}
		if h, err := beadsapi.ParseHarness(rawHarness); err == nil && h != "" && priorHarness != "" && h != priorHarness && rawModel != h {
			b.threadNote(at, fmt.Sprintf(
				th.Semantic(SemWarn)+" This thread's agent runs on `%s`; a resumed session cannot switch to `%s`. Start a new thread with `--harness %s` instead. Resuming on `%s`.",
				priorHarness, h, h, priorHarness))
			rawHarness = ""
		}
		if rawHarness == "" && priorHarness != "" {
			rawHarness = priorHarness
		}
		if isHarnessShorthand(rawModel) {
			rawModel = "" // harness shorthand, not a model; nothing to pin on resume
		}
		sel, unrecognized, errMsg := resolveHarnessModel(rawHarness, rawModel)
		switch {
		case errMsg != "":
			b.logger.Warn("respawn-thread-agent: rejected model override",
				"channel", channel, "thread_ts", threadTS, "raw", rawModel, "reason", errMsg)
			b.threadNote(at, th.Semantic(SemWarn)+" "+errMsg+" Resuming on the prior/default model instead.")
		case unrecognized:
			b.logger.Warn("respawn-thread-agent: unrecognized model override, using prior/default",
				"channel", channel, "thread_ts", threadTS, "raw", rawModel)
			b.threadNote(at, fmt.Sprintf(
				th.Semantic(SemWarn)+" Unrecognized model %q — resuming on the prior/default model instead. Valid options: %s.",
				rawModel, allowedModelHint()))
		default:
			respawnModelOverride = sel.Model
			if respawnModelOverride != "" {
				b.logger.Info("respawn-thread-agent: model override from wake-up text",
					"channel", channel, "thread_ts", threadTS, "raw", rawModel, "model", respawnModelOverride)
			}
		}
	}

	// --ultracode override. "" means absent (never "off"), so a prior "off"
	// carries forward on a no-flag wake-up.
	freshUltracode := ""
	if v, bad, stripped := parseUltracodeFlag(triggerText); v != "" || bad != "" {
		triggerText = stripped
		if v != "" {
			freshUltracode = v
			b.logger.Info("respawn-thread-agent: ultracode override from wake-up text",
				"channel", channel, "thread_ts", threadTS, "ultracode", freshUltracode)
		} else {
			b.logger.Warn("respawn-thread-agent: unrecognized ultracode value, using prior/default",
				"channel", channel, "thread_ts", threadTS, "raw", bad)
			b.threadNote(at, fmt.Sprintf(
				th.Semantic(SemWarn)+" Unrecognized `--ultracode=%s` — resuming with the prior/default ultracode setting. Use `--ultracode=on` or `--ultracode=off`.",
				bad))
		}
	}

	// --backend override; "" means absent, so the prior execution_target carries
	// forward (see effectiveBackend below).
	freshBackend := ""
	if t, bad, stripped := parseBackendFlag(triggerText); t != "" || bad != "" {
		triggerText = stripped
		if t != "" {
			freshBackend = t
			b.logger.Info("respawn-thread-agent: backend override from wake-up text",
				"channel", channel, "thread_ts", threadTS, "execution_target", freshBackend)
		} else {
			b.logger.Warn("respawn-thread-agent: unrecognized backend value, using prior/default",
				"channel", channel, "thread_ts", threadTS, "raw", bad)
			b.threadNote(at, fmt.Sprintf(
				th.Semantic(SemWarn)+" Unrecognized `--backend=%s` — resuming on the prior/default backend. Use `--backend=microvm` or `--backend=kubernetes`.",
				bad))
		}
	}

	// --fresh keeps the thread but drops the prior conversation. It later skips
	// the reviveBeadID lookup (new bead id, empty S3 prefix), the warm-pool
	// graft (ka-0000000983), the gVisor thaw and the MicroVM resume, and stamps
	// session_resume=false (BOAT_SESSION_RESUME=0).
	fresh, strippedFresh := parseFreshFlag(triggerText)
	if fresh {
		triggerText = strings.TrimSpace(strippedFresh)
		b.logger.Info("respawn-thread-agent: --fresh — starting a clean conversation (no resume)",
			"channel", channel, "thread_ts", threadTS, "agent", agentName)
		// Tell the operator, so a clean session is not mistaken for lost context.
		b.threadNote(at, th.Semantic(SemInfo)+" Starting a *fresh conversation* — I've dropped the prior context for this thread and will answer your next message with a clean slate. (The Slack thread history is still available to me.)")
	}

	// Cold-path latency anchor; the stream manager closes
	// gardenio_mention_cold_respawn_seconds on this agent's SessionStart hook.
	respawnStart := time.Now()

	// Check-and-set so concurrent thread replies cause only one respawn.
	spawnKey := at.key()
	b.mu.Lock()
	if b.spawnInFlight == nil {
		b.spawnInFlight = make(map[string]bool)
	}
	if b.spawnInFlight[spawnKey] {
		b.mu.Unlock()
		b.logger.Info("respawn-thread-agent: spawn already in flight, skipping",
			"channel", channel, "thread_ts", threadTS, "agent", agentName)
		return
	}
	b.spawnInFlight[spawnKey] = true
	b.mu.Unlock()
	// Clear the flag late so goroutines that passed FindAgentBead before the
	// bead was created still see the guard.
	defer func() {
		time.AfterFunc(10*time.Second, func() {
			b.mu.Lock()
			delete(b.spawnInFlight, spawnKey)
			b.mu.Unlock()
		})
	}()

	// Double-check: an earlier respawn may have finished before the guard. A
	// live bead means skip; a terminal-state bead (done/failed/stopping/
	// budget_exceeded) lingers open until the stale-reaper runs, so close it and
	// mint the resume bead under the same name (ka-0000002089).
	//
	// frozenAwaitingThaw: a runsc agent that ran `gb stop` is suspended in
	// agent_state=stopping with a name-keyed snapshot (HasSnapshot(agentName)).
	// Only a same-name mint fires the thaw, so the warm pool is skipped
	// (ka-0000003838).
	frozenAwaitingThaw := false
	// frozenRuntimeClass is the suspended agent's own runtime_class, the fallback
	// pin when runtimeClassFromChannel returns "": an unpinned re-mint fails the
	// controller's shouldThaw runsc check and cold-starts (ka-0000005508, !4532).
	frozenRuntimeClass := ""
	if existing, err := b.daemon.FindAgentBead(ctx, agentName); err == nil {
		if !isAgentTerminal(existing.Fields["agent_state"]) {
			b.logger.Info("respawn-thread-agent: agent already exists after concurrent respawn, skipping",
				"channel", channel, "thread_ts", threadTS, "agent", agentName)
			return
		}
		// A suspended MicroVM agent (stopping + runtime_id) is resumed in place:
		// its VM identity lives in runtime_id on this bead, so close+remint would
		// terminate the VM (plan ka-0000004518; cf. ka-0000003838). Skipped when
		// --backend leaves MicroVM or under --fresh; on a miss, fall through.
		operatorLeavingMicrovm := freshBackend != "" && freshBackend != podmanager.ExecutionTargetLambdaMicroVM
		if existing.Fields["execution_target"] == podmanager.ExecutionTargetLambdaMicroVM &&
			existing.Fields["agent_state"] == "stopping" &&
			strings.TrimSpace(existing.Fields["runtime_id"]) != "" &&
			!operatorLeavingMicrovm && !fresh {
			if b.tryMicrovmResume(ctx, agentName) {
				b.logger.Info("respawn-thread-agent: resumed suspended MicroVM agent in place (no close/remint)",
					"channel", channel, "thread_ts", threadTS, "agent", agentName,
					"bead", existing.ID, "runtime_id", existing.Fields["runtime_id"])
				b.recordMentionDelivery(mentionDeliveryWarm)
				// Re-bind the thread under the same name, mirroring the pool path.
				if b.state != nil {
					b.bindThreadAgent(at, agentName)
					if wasListenThread := b.state.IsListenThread(channel, threadTS); wasListenThread {
						_ = b.state.SetListenThread(channel, threadTS)
					}
				}
				b.setThreadStatusAt(ctx, at,
					fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink(existing.ID, existing.ID)))
				return
			}
			b.logger.Info("respawn-thread-agent: MicroVM resume not applicable/failed; falling through to normal respawn",
				"channel", channel, "thread_ts", threadTS, "agent", agentName, "bead", existing.ID)
		}
		// Only suspendAgent writes agent_state=stopping, so it means frozen.
		frozenAwaitingThaw = existing.Fields["agent_state"] == "stopping"
		if frozenAwaitingThaw {
			frozenRuntimeClass = strings.TrimSpace(existing.Fields["runtime_class"])
		}
		b.logger.Info("respawn-thread-agent: closing stale terminal-state bead before resume",
			"channel", channel, "thread_ts", threadTS, "agent", agentName,
			"bead", existing.ID, "agent_state", existing.Fields["agent_state"],
			"frozen_awaiting_thaw", frozenAwaitingThaw)
		if err := b.daemon.CloseBead(ctx, existing.ID, nil); err != nil {
			b.logger.Warn("respawn-thread-agent: failed to close stale terminal bead; proceeding anyway",
				"agent", agentName, "bead", existing.ID, "error", err)
		}
	}

	// Snapshot the listen flag before RemoveThreadAgentByAgent clears it; it is
	// restored after re-binding.
	wasListenThread := b.state != nil && b.state.IsListenThread(channel, threadTS)

	// Resume inherits the prior bead's project before channel inference, since
	// the first spawn may have used a DM fallback or --project.
	priorBead := b.priorAgentBead(ctx, agentName)
	priorFields := map[string]string(nil)
	if priorBead != nil {
		priorFields = priorBead.Fields
	}
	project := priorFields["project"]
	if project == "" && !at.slack() {
		project = at.Land
	}
	var miss channelProjectMiss
	if project == "" {
		project, miss = b.projectForChannel(ctx, channel)
	}
	if project == "" && b.router != nil {
		if mapped := b.router.GetAgentByChannel(channel); mapped != "" {
			project = projectFromAgentIdentity(mapped)
		}
	}
	// DM fallback, as in handleThreadSpawn.
	if project == "" && strings.HasPrefix(channel, "D") {
		project = b.resolveDMProject(ctx, channel, userID, "respawn-thread-agent")
	}

	if project == "" {
		b.logger.Warn("respawn-thread-agent: rejected — no project resolved",
			"channel", channel, "agent", agentName)
		if reason := miss.reason(); reason != "" {
			b.threadNote(at, th.Semantic(SemFail)+" Cannot resume agent — "+reason)
			return
		}
		msg := th.Semantic(SemFail) + " Cannot resume agent — no project is mapped to this channel."
		if strings.HasPrefix(channel, "D") {
			msg += "\n:bulb: To enable DM spawning, set `dm_default=true` on a project bead or configure `DEFAULT_DM_PROJECT` on the bridge."
		} else {
			msg += " Use `--project <name>` to specify one."
		}
		b.threadNote(at, msg)
		return
	}

	// Resume keeps the role the agent was spawned with (roleFromPriorAgentBead),
	// since a role can pin a different model; the channel_roles default applies
	// only with no prior role (ka-0000001567).
	channelRole := "thread"
	commonsRole, commonsRuntimeClass := "", ""
	if !at.slack() {
		channelRole = b.commonsThreadRole(ctx, project)
		commonsRole, commonsRuntimeClass = b.commonsChannelSettings(ctx, at.ChannelID)
	}
	if commonsRole != "" {
		channelRole = commonsRole
	} else if project != "" && at.slack() {
		if cr := b.roleFromChannel(ctx, channel, project); cr != "" {
			channelRole = cr
		}
	}
	threadRole := channelRole
	if priorRole := roleFromPriorAgentBead(priorBead); priorRole != "" {
		threadRole = priorRole
		if channelRole != priorRole {
			b.logger.Info("respawn-thread-agent: preserving prior agent role over channel default",
				"agent", agentName, "channel", channel, "thread_ts", threadTS,
				"prior_role", priorRole, "channel_role", channelRole)
		}
	}

	// Channel RuntimeClass (e.g. runsc): the pool assign requires a matching pod,
	// and the cold mint stamps it (epic ka-0000003080, ka-0000004563).
	channelRuntimeClass := commonsRuntimeClass
	if at.slack() {
		channelRuntimeClass = b.runtimeClassFromChannel(ctx, channel, project)
	}

	// Effective model/ultracode/backend: the wake-up flag wins, else the prior
	// bead's value carries forward, else "" (role default). Any non-empty
	// override skips the warm pool, since a pooled pod bakes these at create.
	effectiveModel := strings.TrimSpace(priorFields["model"])
	effectiveHarness := priorHarnessOverride // agent-level stamp only; role/project defaults still apply on resume
	if respawnModelOverride != "" {
		effectiveModel = respawnModelOverride
	}
	effectiveUltracode := strings.TrimSpace(priorFields["ultracode"])
	if freshUltracode != "" {
		effectiveUltracode = freshUltracode
	}
	// A MicroVM backend forces ultracode on unless set, as on spawn.
	effectiveBackend := carriedBackend(priorFields["execution_target"])
	if freshBackend != "" {
		effectiveBackend = freshBackend
	}
	if effectiveBackend == podmanager.ExecutionTargetLambdaMicroVM && freshUltracode == "" && effectiveUltracode == "" {
		effectiveUltracode = "on"
	}
	// A pooled pod has no PVC transcript, which is acceptable for a thread agent
	// because `gb prime` bakes in the Slack thread; a pool miss cold-mints with
	// BOAT_SESSION_RESUME.
	resumeDescription := fmt.Sprintf("<system-reminder>\nNEW MESSAGE — answer specifically. Prior conversation context is reference only; do not assume this question is already answered. Read the trigger text below and respond to it on its own terms.\n</system-reminder>\n\nSession-resumed thread agent (warm-pool).\n\nTriggered by thread reply:\n%s",
		truncateText(triggerText, 2000))
	assignOperator := ""
	if userID != "" {
		assignOperator = b.operatorFromSlackUser(ctx, userID)
	} else if at.Actor != "" {
		assignOperator = at.Actor
	}
	// Resolve the parent thread bead up front so both branches stamp
	// slack_thread_bead_id and the triggered-by edge the autocloser needs
	// (ka-0000001874 Part B). Idempotent; on failure the respawn proceeds.
	threadBeadID, threadBeadErr := at.Head, error(nil)
	if at.slack() {
		threadBeadID, _, threadBeadErr = b.getOrCreateThreadBead(ctx, project, threadBeadFields{
			Channel:      channel,
			ThreadTS:     threadTS,
			SpawnChannel: channel,
			OpenerUserID: userID,
			RootText:     triggerText,
		})
	}
	if threadBeadErr == nil {
		at.Head = threadBeadID
	}
	if threadBeadErr != nil {
		b.logger.Warn("respawn-thread-agent: resolve thread bead failed; skipping thread link",
			"channel", channel, "thread_ts", threadTS, "error", threadBeadErr)
		threadBeadID = ""
	}

	// The pool is skipped when frozen (name-keyed thaw), for MicroVM (a pool pod
	// cannot become a VM) or for any override. The pool assign requires
	// channelRuntimeClass (ka-0000004563); --fresh forces no warm graft.
	warmResumeS3Key := ""
	if !fresh {
		warmResumeS3Key = b.resolveWarmResumeS3Key(priorBead, agentName)
	}

	poolBeadID, poolAgent := "", ""
	if !frozenAwaitingThaw && effectiveModel == "" && effectiveUltracode == "" &&
		effectiveBackend != podmanager.ExecutionTargetLambdaMicroVM {
		poolBeadID, poolAgent = b.tryPoolAssign(ctx, channel, threadTS, threadBeadID, resumeDescription, project, threadRole, assignOperator, channelRuntimeClass, warmResumeS3Key)
	}
	if beadID, assignedAgent := poolBeadID, poolAgent; beadID != "" {
		b.logger.Info("respawn-thread-agent: assigned prewarmed agent (warm-pool resume)",
			"bead", beadID, "agent", assignedAgent,
			"channel", channel, "thread_ts", threadTS, "listen", wasListenThread)

		b.recordMentionDelivery(mentionDeliveryWarm)

		// Per-user fields, as on the cold path.
		userFields := map[string]string{}
		if userID != "" {
			userFields["slack_user_id"] = userID
			userFields["operated_by"] = assignOperator
			if u := b.UserDirectory().LookupBySlackID(ctx, userID); u != nil {
				userFields["user_bead_id"] = u.BeadID
			}
		}
		b.commonsSpawnFields(ctx, at, userID, "commons-thread-resume", userFields)
		if lastSessionID := priorFields["last_session_id"]; lastSessionID != "" {
			userFields["last_session_id"] = lastSessionID
		}
		// The thread-cards ownership gate needs the opener (ka-0000000954).
		if opener := priorFields["slack_opener_user_id"]; opener != "" {
			userFields["slack_opener_user_id"] = opener
		}
		// Parent thread link for the autocloser (ka-0000001874 B); writes
		// thread_id and slack_thread_bead_id (ka-0000007241).
		beadsapi.SetWishPointer(userFields, threadBeadID)
		if len(userFields) > 0 {
			_ = b.daemon.UpdateBeadFields(ctx, beadID, userFields)
		}
		if threadBeadID != "" {
			if err := b.daemon.AddDependency(ctx, beadID, threadBeadID, "triggered-by", "slack-bridge"); err != nil {
				b.logger.Warn("respawn-thread-agent: failed to add triggered-by dep (warm-pool)",
					"bead", beadID, "thread_bead", threadBeadID, "error", err)
			}
		}

		assignedShort := extractAgentName(assignedAgent)
		if b.state != nil {
			b.bindThreadAgent(at, assignedShort)
			if wasListenThread {
				_ = b.state.SetListenThread(channel, threadTS)
			}
		}
		// Fetch pod_name so state transitions and nudges work.
		b.fetchAndCachePodName(ctx, assignedShort)

		// Resurrect the agent card under the pooled agent name if one exists.
		if b.state != nil {
			if cardRef, ok := b.state.GetAgentCard(assignedShort); ok {
				b.mu.Lock()
				b.agentCards[assignedShort] = cardRef
				b.agentState[assignedShort] = "spawning"
				b.agentSeen[assignedShort] = time.Now()
				b.mu.Unlock()
				b.updateAgentCard(ctx, assignedShort)
			}
		}

		// Anchor TTFT on the follow-up's arrival, as the cold path does.
		if b.streamMgr != nil {
			b.streamMgr.MarkTriggerSent(assignedShort, respawnStart)
		}
		if !at.slack() {
			// A Commons thread has no Slack stream or status bar: its card is the
			// agent's status.
		} else if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
			streamTS := b.streamMgr.StartSession(ctx, assignedShort, channel, threadTS, userID)
			if streamTS != "" {
				b.mu.Lock()
				b.threadSpawnMsgs[assignedShort] = MessageRef{
					ChannelID: channel, Timestamp: streamTS, ThreadTS: threadTS,
				}
				b.mu.Unlock()
			}
			b.setThreadStatusAt(ctx, at,
				fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink(beadID, beadID)))
		} else {
			b.setThreadStatusAt(ctx, at,
				fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink(beadID, beadID)))
			b.mu.Lock()
			b.threadSpawnMsgs[assignedShort] = MessageRef{
				ChannelID: channel, ThreadTS: threadTS,
			}
			b.mu.Unlock()
		}

		// A reused pool bead fires no created event for NotifyAgentSpawn, so
		// post the card here (ka-0000000954).
		if at.slack() {
			b.postWarmThreadCard(ctx, assignedShort, project, channel, threadTS,
				priorFields["slack_opener_user_id"], userID)
		}
		return
	}

	// Cold fallback. coop's S3 resume prefix is the agent bead id
	// (COOP_S3_SESSION_KEY), so a prior bead is revived in place rather than
	// minting a new id (ka-0000006807, twin of ka-0000002360). Not for --fresh,
	// frozenAwaitingThaw (name-keyed thaw needs the remint) or MicroVM.
	reviveBeadID := ""
	if !fresh && !frozenAwaitingThaw && effectiveBackend != podmanager.ExecutionTargetLambdaMicroVM {
		if prior, findErr := b.daemon.FindAgentBeadAnyStatus(ctx, agentName); findErr == nil &&
			prior != nil && prior.ID != "" &&
			prior.Fields["execution_target"] != podmanager.ExecutionTargetLambdaMicroVM {
			reviveBeadID = prior.ID
		}
	}

	fields := map[string]string{
		"agent":                agentName,
		"mode":                 "crew",
		"role":                 threadRole,
		"project":              project,
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
		"slack_spawn_channel":  channel,
		"spawn_source":         "slack-thread-resume",
		// So a mention racing pod boot takes the boot-retry path (ka-0000000292).
		"agent_state": "spawning",
	}
	// Parent thread link for the autocloser (ka-0000001874 Part B,
	// ka-0000007241).
	beadsapi.SetWishPointer(fields, threadBeadID)
	// Pin the channel RuntimeClass (ka-0000003080). A frozen revive with none
	// falls back to frozenRuntimeClass, else runsc (ka-0000005508).
	runtimeClass := channelRuntimeClass
	if runtimeClass == "" && frozenAwaitingThaw {
		if frozenRuntimeClass != "" {
			runtimeClass = frozenRuntimeClass
		} else {
			runtimeClass = podmanager.RuntimeClassRunsc
		}
		b.logger.Info("respawn-thread-agent: channel resolved no runtime_class for a frozen-awaiting-thaw revive; pinning runsc so the controller thaws the snapshot",
			"channel", channel, "thread_ts", threadTS, "agent", agentName,
			"runtime_class", runtimeClass, "from_prior_bead", frozenRuntimeClass != "")
	}
	if runtimeClass != "" {
		fields["runtime_class"] = runtimeClass
	}
	b.commonsSpawnFields(ctx, at, userID, "commons-thread-resume", fields)
	if userID != "" {
		fields["slack_user_id"] = userID
		// R3 (ka-0000002887): every spawn stamps operated_by (BEADS_OPERATED_BY).
		fields["operated_by"] = b.operatorFromSlackUser(ctx, userID)
		if u := b.UserDirectory().LookupBySlackID(ctx, userID); u != nil {
			fields["user_bead_id"] = u.BeadID
		}
	}
	stampHumanOwner(fields)
	// gb init passes it as COOP_S3_RESUME_SESSION when the PVC was lost.
	if lastSessionID := priorFields["last_session_id"]; lastSessionID != "" {
		fields["last_session_id"] = lastSessionID
	}
	// Succession back-pointers for a cold mint (ka-0000006931, design memo
	// §3.1); the revive branch strips them to avoid a self-pointer.
	if priorBead != nil && priorBead.ID != "" {
		fields["prior_agent_id"] = priorBead.ID
		if root := priorFields["lineage_root_id"]; root != "" {
			fields["lineage_root_id"] = root
		} else {
			fields["lineage_root_id"] = priorBead.ID
		}
	}
	// Stamp the effective overrides so they survive restart/resume.
	if effectiveModel != "" {
		fields["model"] = effectiveModel
	}
	if effectiveHarness != "" {
		fields["harness"] = effectiveHarness
	}
	if effectiveUltracode != "" {
		fields["ultracode"] = effectiveUltracode
	}
	if effectiveBackend != "" {
		fields["execution_target"] = effectiveBackend
	}
	// BOAT_SESSION_RESUME=0: no PVC resume and shouldThaw vetoes the runsc thaw.
	if fresh {
		fields["session_resume"] = "false"
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		b.logger.Error("respawn-thread-agent: marshal fields", "error", err)
		return
	}

	labels := []string{"slack-thread"}
	if !at.slack() {
		labels = []string{commonsThreadLabel}
	}
	if project != "" {
		labels = append(labels, "project:"+project)
	}

	// The reminder counters a resumed agent's bias to treat the new message as
	// already answered; --fresh has no prior context, so it gets its own text.
	description := fmt.Sprintf("<system-reminder>\nNEW MESSAGE — answer specifically. Prior conversation context is reference only; do not assume this question is already answered. Read the trigger text below and respond to it on its own terms.\n</system-reminder>\n\nSession-resumed thread agent.\n\nTriggered by thread reply:\n%s",
		truncateText(triggerText, 2000))
	if fresh {
		description = fmt.Sprintf("<system-reminder>\nFRESH CONVERSATION — the operator asked to continue this Slack thread with a CLEAN context. There is intentionally no prior conversation transcript to resume; the Slack thread history is available via gb prime. Read the trigger text below and respond to it on its own terms.\n</system-reminder>\n\nThread agent (fresh conversation — prior context intentionally dropped).\n\nTriggered by thread reply:\n%s",
			truncateText(triggerText, 2000))
	}

	// The mint retries transient daemon errors (ka-0000004735). The
	// IdempotencyKey is per wake-up (respawnStart), so a retry after a lost
	// response cannot double-mint.
	respawnIdempotencyKey := fmt.Sprintf("thread-respawn:%s:%d", at.key(), respawnStart.UnixNano())
	var beadID string
	if reviveBeadID != "" {
		// Revive in place (ka-0000006807). The prior bead is closed, so
		// closed→in_progress is the transition mapBeadEvent re-spawns on, and
		// fields land in the same PATCH: one atomic write, one event
		// (ka-0000004492). A retried UpdateBead is idempotent.
		reviveFields := make(map[string]any, len(fields)+3)
		for k, v := range fields {
			reviveFields[k] = v
		}
		// Clear the prior life's terminal markers. stop_requested must be ""
		// or gb init's shouldExitBeforeStart exits the revived pod at boot.
		// close_reason/wrapup are nil-deleted, with a non-empty wrapup first
		// archived to prior_wrapup for the card (ka-0000003692); an empty one
		// leaves an earlier prior_wrapup intact.
		if priorWrapup := strings.TrimSpace(priorFields["wrapup"]); priorWrapup != "" {
			reviveFields["prior_wrapup"] = priorWrapup
		}
		reviveFields["stop_requested"] = ""
		reviveFields["close_reason"] = nil
		reviveFields["wrapup"] = nil
		// Same id: drop the succession pointers to avoid a self-pointer
		// (ka-0000006931).
		delete(reviveFields, "prior_agent_id")
		delete(reviveFields, "lineage_root_id")
		inProgress := "in_progress"
		err = b.retryTransientDaemon(ctx, "respawn_revive", func() error {
			return b.daemon.UpdateBead(ctx, reviveBeadID, beadsapi.UpdateBeadRequest{
				Status:      &inProgress,
				Description: &description,
				Fields:      reviveFields,
			})
		})
		if err == nil {
			beadID = reviveBeadID
			b.logger.Info("respawn-thread-agent: revived prior thread agent bead in place (stable S3 resume key)",
				"agent", agentName, "bead", beadID, "channel", channel, "thread_ts", threadTS)
		} else {
			// Fall through to a fresh mint: a cold start, never a dropped reply.
			b.logger.Warn("respawn-thread-agent: revive-in-place failed; falling back to fresh mint",
				"agent", agentName, "bead", reviveBeadID, "channel", channel, "thread_ts", threadTS, "error", err)
		}
	}
	if beadID == "" {
		err = b.retryTransientDaemon(ctx, "respawn_create", func() error {
			id, e := b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
				Title:          agentName,
				Type:           "agent",
				Description:    description,
				Fields:         json.RawMessage(fieldsJSON),
				Labels:         labels,
				IdempotencyKey: respawnIdempotencyKey,
			})
			if e != nil {
				return e
			}
			beadID = id
			return nil
		})
		if err != nil {
			b.logger.Error("respawn-thread-agent: failed to create bead after retries",
				"agent", agentName, "channel", channel, "thread_ts", threadTS, "error", err)
			b.threadNote(at, th.Semantic(SemFail)+" Failed to resume agent — please try mentioning @gardenio again.")
			return
		}
	}

	// Lineage edge for the autocloser (ka-0000001874 Part B).
	if threadBeadID != "" {
		if err := b.daemon.AddDependency(ctx, beadID, threadBeadID, "triggered-by", "slack-bridge"); err != nil {
			b.logger.Warn("respawn-thread-agent: failed to add triggered-by dep (cold-resume)",
				"bead", beadID, "thread_bead", threadBeadID, "error", err)
		}
	}

	// Cold delivery; the histograms close in HandleSessionStart (ka-0000001565).
	b.recordMentionDelivery(mentionDeliveryCold)
	if b.streamMgr != nil {
		// A cold respawn also observes gardenio_resume_latency_seconds{path=cold}
		// (ka-0000004119), except a frozen thaw: the controller records that
		// sample, so it gets the plain MarkColdRespawn.
		if frozenAwaitingThaw {
			b.streamMgr.MarkColdRespawn(agentName, respawnStart)
		} else {
			resumeRuntime := ""
			if channelRuntimeClass == podmanager.RuntimeClassRunsc {
				resumeRuntime = gardeniometrics.ResumeRuntimeRunsc
			}
			// channel+threadTS: where "Resumed in Xs" is posted (ka-0000005084).
			b.streamMgr.MarkColdResume(agentName, respawnStart, threadRole, resumeRuntime, channel, threadTS)
		}
		// Per-phase breakdown (ka-0000001457): time.Now() ends bridge dispatch;
		// pod boot runs until first proof-of-life.
		b.streamMgr.MarkRespawnPhase(agentName, respawnStart, time.Now(), threadRole)
		// TTFT anchor (ka-0000001004).
		b.streamMgr.MarkTriggerSent(agentName, respawnStart)
	}

	if b.state != nil {
		b.bindThreadAgent(at, agentName)
		if wasListenThread {
			_ = b.state.SetListenThread(channel, threadTS)
		}
	}

	// Restore a pruned card from persisted state so updates reuse it rather
	// than posting a second card.
	if b.state != nil {
		if cardRef, ok := b.state.GetAgentCard(agentName); ok {
			b.mu.Lock()
			b.agentCards[agentName] = cardRef
			b.agentState[agentName] = "spawning"
			b.agentSeen[agentName] = time.Now()
			b.mu.Unlock()
			b.updateAgentCard(ctx, agentName)
		}
	}

	b.logger.Info("respawned thread agent with session resume",
		"agent", agentName, "bead", beadID, "channel", channel, "thread_ts", threadTS,
		"listen", wasListenThread)

	// Stream plus status bar; a Commons thread has neither.
	if !at.slack() {
		return
	}
	if b.streamMgr != nil && !b.notifConfig.SuppressToolStream {
		streamTS := b.streamMgr.StartSession(ctx, agentName, channel, threadTS, userID)
		if streamTS != "" {
			b.mu.Lock()
			b.threadSpawnMsgs[agentName] = MessageRef{
				ChannelID: channel, Timestamp: streamTS, ThreadTS: threadTS,
			}
			b.mu.Unlock()
		}
		statusText := fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink(beadID, beadID))
		b.setThreadStatusAt(ctx, at, statusText)
	} else {
		statusText := fmt.Sprintf("Resuming... (tracking: %s)", b.beadLink(beadID, beadID))
		b.setThreadStatusAt(ctx, at, statusText)
		b.mu.Lock()
		b.threadSpawnMsgs[agentName] = MessageRef{
			ChannelID: channel, ThreadTS: threadTS,
		}
		b.mu.Unlock()
	}
}

// handleClearAgent handles the "Clear" button on a done/failed agent card.
// It deletes the Slack message and cleans up state; the only path that fully
// removes a card (kill and prune compress instead).
func (b *Bot) handleClearAgent(ctx context.Context, agentIdentity string, callback slack.InteractionCallback) {
	b.clearAgent(ctx, agentIdentity, callback.Channel.ID, callback.User.ID, b.slackSay(callback.Channel.ID, callback.User.ID))
}

// slackSay answers the Slack user who clicked with an ephemeral in channel:
// the say an agent control reports through when the click came from Slack.
func (b *Bot) slackSay(channelID, userID string) func(string) {
	return func(text string) {
		if b.api != nil {
			_, _ = b.api.PostEphemeral(channelID, userID, slack.MsgOptionText(text, false))
		}
	}
}

// clearAgent forgets agentIdentity's card: it deletes the Slack card and drops
// the agent from the card and thread state. channelID themes the answers; say
// reports a failure to whoever asked.
func (b *Bot) clearAgent(ctx context.Context, agentIdentity, channelID, userID string, say func(string)) {
	th := b.themeFor(ctx, channelID)
	agentIdentity = extractAgentName(agentIdentity)

	b.mu.Lock()
	ref, hasCard := b.agentCards[agentIdentity]
	if hasCard {
		delete(b.agentCards, agentIdentity)
		delete(b.agentPending, agentIdentity)
		delete(b.agentState, agentIdentity)
		delete(b.agentPodName, agentIdentity)
		delete(b.agentImageTag, agentIdentity)
		delete(b.agentRole, agentIdentity)
		delete(b.agentProject, agentIdentity)
	}
	b.mu.Unlock()

	if hasCard {
		if b.state != nil {
			_ = b.state.RemoveAgentCard(agentIdentity)
		}
		if _, _, err := b.api.DeleteMessageContext(ctx, ref.ChannelID, ref.Timestamp); err != nil {
			b.logger.Error("clear agent: failed to delete card",
				"agent", agentIdentity, "error", err)
			say(fmt.Sprintf(th.Semantic(SemFail)+" Failed to clear agent %q: %s", agentIdentity, err.Error()))
			return
		}
	}

	// Clean up any thread→agent mappings for this agent.
	if b.state != nil {
		_ = b.state.RemoveThreadAgentByAgent(agentIdentity)
	}

	b.logger.Info("cleared agent", "agent", agentIdentity, "user", userID)
}

// sendControlKill mails agentName a control:kill and waits until agent bead
// beadID is closed or reads a dead agent_state, or killWaitMax passes. It
// polls beadID, not the name, so a same-named bead cannot end the wait. The
// mail is closed on return so a later session under the name never drains it.
func (b *Bot) sendControlKill(ctx context.Context, agentName, beadID string) {
	mailID, err := b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:       "Stop",
		Type:        "mail",
		Kind:        "data",
		Description: "Kill requested from Slack. Finish the current tool call, then stop.",
		Assignee:    agentName,
		Labels:      []string{mailproto.FromLabel(NudgeSender), mailproto.ControlLabel(mailproto.ControlKill)},
		CreatedBy:   NudgeSender,
	})
	if err != nil {
		b.logger.Warn("control:kill mail failed, falling back to hard-close", "agent", agentName, "error", err)
		return
	}
	defer func() {
		if err := b.daemon.CloseBead(ctx, mailID, nil); err != nil {
			b.logger.Debug("closing control:kill mail", "agent", agentName, "mail", mailID, "error", err)
		}
	}()
	b.logger.Info("control:kill mail sent, waiting for the agent to stop", "agent", agentName, "mail", mailID)
	deadline := time.Now().Add(killWaitMax)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return
		case <-time.After(killWaitPoll):
		}
		cur, err := b.daemon.GetBead(ctx, beadID)
		if err != nil || cur == nil || cur.Status == "closed" || deadAgentStates[cur.Fields["agent_state"]] {
			return
		}
	}
	b.logger.Warn("agent did not stop after control:kill, falling back to hard-close", "agent", agentName)
}

// handleKillThreadAgent handles the "Kill Agent" button on thread spawn
// messages; the button value is the agent identity. Owner-only
// (bot_kill_owner.go).
func (b *Bot) handleKillThreadAgent(ctx context.Context, agentName string, callback slack.InteractionCallback) {
	bead := b.slackKillTarget(ctx, "kill button", agentName, callback.Channel.ID, callback.Container.ThreadTs, callback.User.ID)
	if bead == nil {
		return
	}
	b.killThreadAgent(ctx, agentName, bead.ID, callback.Channel.ID, callback.User.ID, b.slackSay(callback.Channel.ID, callback.User.ID))
}

// killThreadAgent kills agentName in the background, reporting through say:
// at once, and again from the goroutine when the kill ends. channelID themes
// the answers. A set beadID holds the kill to that agent bead (killAgentBead).
// The kill runs on ctx's actor, not the bridge, past the end of the request.
func (b *Bot) killThreadAgent(ctx context.Context, agentName, beadID, channelID, userID string, say func(string)) {
	th := b.themeFor(ctx, channelID)
	agentName = extractAgentName(agentName)

	// Acknowledge immediately — kill can take 30s+.
	say(fmt.Sprintf(th.Glyph(LifecycleWorking)+" Killing thread agent *%s*…", agentName))

	actorCtx := context.WithoutCancel(ctx)
	go func() {
		if err := b.killAgentBead(actorCtx, agentName, beadID, false); err != nil {
			b.logger.Error("kill-thread-agent button: failed", "agent", agentName, "error", err)
			say(fmt.Sprintf(th.Semantic(SemFail)+" Failed to kill thread agent %q: %s", agentName, err.Error()))
			return
		}
		b.logger.Info("killed thread agent via button", "agent", agentName, "user", userID)
		say(fmt.Sprintf(th.Semantic(SemStop)+" Thread agent *%s* terminated.", agentName))
	}()
}

// handleRestartThreadAgent handles the "Restart Agent" button: kill, then
// re-spawn under the same name for coop --resume. Owner-only
// (bot_kill_owner.go).
func (b *Bot) handleRestartThreadAgent(ctx context.Context, agentName string, callback slack.InteractionCallback) {
	// Outside a thread restartThreadAgent refuses before it touches the agent.
	beadID := ""
	if threadTS := callback.Container.ThreadTs; threadTS != "" {
		bead := b.slackKillTarget(ctx, "restart button", agentName, callback.Channel.ID, threadTS, callback.User.ID)
		if bead == nil {
			return
		}
		beadID = bead.ID
	}
	b.restartThreadAgent(ctx, agentName, beadID, callback.Channel.ID, callback.Container.ThreadTs, callback.User.ID,
		b.slackSay(callback.Channel.ID, callback.User.ID))
}

// restartThreadAgent kills agentName and spawns it again under the same name in
// the Slack thread channelID/threadTS, reporting through say. userID is the
// Slack user who asked ("" from the Commons). A set beadID holds the restart to
// that agent bead: a name that resolves to another is not killed or respawned.
func (b *Bot) restartThreadAgent(ctx context.Context, agentName, beadID, channelID, threadTS, userID string, say func(string)) {
	th := b.themeFor(ctx, channelID)
	agentName = extractAgentName(agentName)

	if threadTS == "" {
		say(th.Semantic(SemFail) + " Restart is only available in threads.")
		return
	}

	// Debounce (ka-0000000034): a double click must not run two kill+remint
	// cycles under one name; only the leading request proceeds.
	if !b.reopenDebouncer.Allow(agentName) {
		b.logger.Info("restart-thread-agent: coalesced rapid restart (debounced)",
			"agent", agentName, "channel", channelID, "thread_ts", threadTS, "user", userID)
		say(fmt.Sprintf(th.Glyph(LifecycleRestarted)+" Agent *%s* is already restarting — coalescing your request.", agentName))
		return
	}

	say(fmt.Sprintf(th.Glyph(LifecycleRestarted)+" Restarting thread agent *%s*…", agentName))

	bgCtx := context.WithoutCancel(ctx)
	go func() {

		bead, err := b.daemon.FindAgentBead(bgCtx, agentName)
		if err != nil {
			b.logger.Error("restart-thread-agent: agent bead not found", "agent", agentName, "error", err)
			say(fmt.Sprintf(th.Semantic(SemFail)+" Agent %q not found: %s", agentName, err))
			return
		}
		if beadID != "" && bead.ID != beadID {
			b.logger.Error("restart-thread-agent: name resolves to another bead", "agent", agentName, "bead", bead.ID, "want", beadID)
			say(fmt.Sprintf(th.Semantic(SemFail)+" Agent %q is now another agent; not restarted.", agentName))
			return
		}
		project := bead.Fields["project"]
		slackUserID := bead.Fields["slack_user_id"]
		// Snapshot the per-spawn overrides before the kill; the re-mint carries
		// them (ka-0000008253). A click has no flags, so prior-else-default.
		priorBackend := carriedBackend(bead.Fields["execution_target"])
		priorUltracode := strings.TrimSpace(bead.Fields["ultracode"])
		priorModel := strings.TrimSpace(bead.Fields["model"])
		priorHarness := strings.TrimSpace(bead.Fields["harness"])

		// Snapshot the listen-thread flag before killAgent clears it.
		wasListenThread := b.state != nil && b.state.IsListenThread(channelID, threadTS)

		if err := b.killAgentBead(bgCtx, agentName, bead.ID, false); err != nil {
			b.logger.Error("restart-thread-agent: kill failed", "agent", agentName, "error", err)
			say(fmt.Sprintf(th.Semantic(SemFail)+" Failed to kill agent %q: %s", agentName, err))
			return
		}

		// Re-spawn under the same name for session continuity, keeping the
		// pre-kill bead's own role over the channel default (ka-0000001567).
		channelRole := "thread"
		if project != "" {
			if cr := b.roleFromChannel(bgCtx, channelID, project); cr != "" {
				channelRole = cr
			}
		}
		threadRole := channelRole
		if priorRole := roleFromPriorAgentBead(bead); priorRole != "" {
			threadRole = priorRole
			if channelRole != priorRole {
				b.logger.Info("restart-thread-agent: preserving prior agent role over channel default",
					"agent", agentName, "channel", channelID, "thread_ts", threadTS,
					"prior_role", priorRole, "channel_role", channelRole)
			}
		}

		// Link the agent to its thread head for the autocloser, as the respawn
		// path does (ka-0000001530, ka-0000001874). Idempotent; on failure the
		// restart proceeds.
		threadBeadID, _, threadBeadErr := b.getOrCreateThreadBead(bgCtx, project, threadBeadFields{
			Channel:      channelID,
			ThreadTS:     threadTS,
			SpawnChannel: channelID,
			OpenerUserID: slackUserID,
		})
		if threadBeadErr != nil {
			b.logger.Warn("restart-thread-agent: resolve thread bead failed; skipping thread link",
				"channel", channelID, "thread_ts", threadTS, "error", threadBeadErr)
			threadBeadID = ""
		}

		fields := map[string]string{
			"agent":                agentName,
			"mode":                 "job",
			"role":                 threadRole,
			"project":              project,
			"slack_thread_channel": channelID,
			"slack_thread_ts":      threadTS,
			"slack_spawn_channel":  channelID,
			"spawn_source":         "slack-thread",
			// So a mention racing pod boot takes the boot-retry path (ka-0000000292).
			"agent_state": "spawning",
		}
		if slackUserID != "" {
			fields["slack_user_id"] = slackUserID
		}
		// Succession back-pointers to the pre-kill bead (ka-0000001928).
		if bead != nil && bead.ID != "" {
			fields["prior_agent_id"] = bead.ID
			if root := bead.Fields["lineage_root_id"]; root != "" {
				fields["lineage_root_id"] = root
			} else {
				fields["lineage_root_id"] = bead.ID
			}
		}
		// Carry the per-spawn overrides; empty means inherit the role/project
		// default.
		if priorBackend != "" {
			fields["execution_target"] = priorBackend
		}
		if priorUltracode != "" {
			fields["ultracode"] = priorUltracode
		}
		if priorModel != "" {
			fields["model"] = priorModel
		}
		if priorHarness != "" {
			fields["harness"] = priorHarness
		}
		beadsapi.SetWishPointer(fields, threadBeadID)
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			b.logger.Error("restart-thread-agent: marshal fields", "error", err)
			return
		}

		// Bare `slack-thread`, never slack-thread:{channel}:{ts}: that label
		// identifies the thread head (Limit:1 lookups), and an agent bead
		// carrying it would shadow the head.
		labels := []string{"slack-thread"}
		if project != "" {
			labels = append(labels, "project:"+project)
		}

		newBeadID, err := b.daemon.CreateBead(bgCtx, beadsapi.CreateBeadRequest{
			Title:       agentName,
			Type:        "agent",
			Description: "Restarted thread agent (session resume).",
			Fields:      json.RawMessage(fieldsJSON),
			Labels:      labels,
		})
		if err != nil {
			b.logger.Error("restart-thread-agent: failed to create new bead", "agent", agentName, "error", err)
			say(fmt.Sprintf(th.Semantic(SemFail)+" Killed agent but failed to re-spawn: %s", err))
			return
		}

		// Lineage edge to the thread bead (ka-0000001530).
		if threadBeadID != "" {
			if err := b.daemon.AddDependency(bgCtx, newBeadID, threadBeadID, "triggered-by", "slack-bridge"); err != nil {
				b.logger.Warn("restart-thread-agent: failed to add triggered-by dep",
					"agent_bead", newBeadID, "thread_bead", threadBeadID, "error", err)
			}
		}

		if b.state != nil {
			_ = b.state.SetThreadAgent(channelID, threadTS, agentName)
			if wasListenThread {
				_ = b.state.SetListenThread(channelID, threadTS)
			}
		}

		b.logger.Info("restarted thread agent", "agent", agentName, "new_bead", newBeadID,
			"user", userID, "channel", channelID, "thread_ts", threadTS,
			"listen", wasListenThread)

		if b.api != nil {
			msg := fmt.Sprintf(th.Glyph(LifecycleRestarted)+" Agent *%s* restarted with session resume. (tracking: %s)", agentName, b.beadLink(newBeadID, fmt.Sprintf("`%s`", newBeadID)))
			_, _, _ = b.api.PostMessage(channelID,
				slack.MsgOptionText(msg, false),
				slack.MsgOptionBlocks(
					slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", msg, false, false), nil, nil),
					slack.NewActionBlock("thread_agent_actions",
						slack.NewButtonBlockElement("restart_thread_agent", agentName,
							slack.NewTextBlockObject("plain_text", "Restart Agent", false, false)),
						slack.NewButtonBlockElement("kill_thread_agent", agentName,
							slack.NewTextBlockObject("plain_text", "Kill Agent", false, false)).
							WithStyle(slack.StyleDanger),
					),
				),
				slack.MsgOptionTS(threadTS),
			)
		}
	}()
}

// priorAgentBead returns the newest agent bead (any status) for agentName, or
// nil. Resume uses it to inherit the original spawn's state (project,
// last_session_id, role: label). The lookup is server-side (Limit 1, newest
// first) because a full scan hit the daemon's row cap (ka-0000006235 / M8);
// idx_beads_fields_agent backs it.
func (b *Bot) priorAgentBead(ctx context.Context, agentName string) *beadsapi.BeadDetail {
	if b.daemon == nil || agentName == "" {
		return nil
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress", "blocked", "deferred", "closed"},
		Fields:   map[string]string{"agent": agentName},
		Sort:     "-created_at",
		Limit:    1,
	})
	if err != nil || res == nil {
		return nil
	}
	for _, bead := range res.Beads {
		// Re-check: a lenient daemon or stub may ignore the field filter.
		if bead == nil || bead.Fields["agent"] != agentName {
			continue
		}
		return bead
	}
	return nil
}

// roleFromPriorAgentBead recovers the role an agent was spawned with, so a
// resume can keep it (ka-0000001567). It prefers the role: labels (stamped once
// at spawn, never rewritten; multiple are rejoined with commas), else
// fields.role. "" means the caller uses the channel_roles default.
func roleFromPriorAgentBead(bead *beadsapi.BeadDetail) string {
	if bead == nil {
		return ""
	}
	var roles []string
	for _, l := range bead.Labels {
		if r, ok := strings.CutPrefix(l, "role:"); ok && r != "" {
			roles = append(roles, r)
		}
	}
	if len(roles) > 0 {
		return strings.Join(roles, ",")
	}
	return strings.TrimSpace(bead.Fields["role"])
}

// carriedBackend is the prior agent's execution_target a bridge re-mint
// carries forward. A local (laptop) target is dropped: a re-mint is a fleet
// spawn, and a fresh bead stamped local would be one the controller never runs.
func carriedBackend(prior string) string {
	prior = strings.TrimSpace(prior)
	if podmanager.IsLaptopTarget(prior) {
		return ""
	}
	return prior
}
