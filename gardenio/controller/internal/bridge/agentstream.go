// Package bridge provides the agent streaming status manager.
//
// AgentStreamManager uses Slack's chat streaming APIs (startStream, appendStream,
// stopStream) to show live tool-use statistics and activity status for agents
// working in assistant threads. It aggregates hook events (PreToolUse, PostToolUse)
// and flushes batched updates every few seconds.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gardeniometrics "gardenio/controller/internal/metrics"

	"github.com/slack-go/slack"
)

// slackTSToTime parses a Slack message timestamp ("1719331200.123456", seconds
// since the unix epoch with microsecond precision) into a time.Time. A Slack
// `ts` is the wall-clock instant the message was sent, which is exactly the
// reference point for time-to-first-token. Returns ok=false for empty or
// unparseable input so callers can skip the TTFT annotation rather than show a
// bogus value.
func slackTSToTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	f, err := strconv.ParseFloat(ts, 64)
	if err != nil || f <= 0 {
		return time.Time{}, false
	}
	sec := int64(f)
	nsec := int64((f - float64(sec)) * 1e9)
	return time.Unix(sec, nsec), true
}

// formatTTFT renders a time-to-first-token duration for the first agent
// message. Sub-10s values get one decimal (e.g. "2.3s"); larger cold-start
// values round to whole seconds (e.g. "47s").
func formatTTFT(d time.Duration) string {
	secs := d.Seconds()
	if secs < 0 {
		secs = 0
	}
	if secs < 10 {
		return fmt.Sprintf("%.1fs", secs)
	}
	return fmt.Sprintf("%ds", int(secs+0.5))
}

// streamFlushInterval is how often dirty sessions are flushed to Slack.
const streamFlushInterval = 3 * time.Second

// streamSessionMaxAge is the maximum age of a streaming session before it is
// garbage-collected (failsafe for agents that crash without closing).
const streamSessionMaxAge = 24 * time.Hour

// StreamClient is the subset of the slack.Client API used by AgentStreamManager.
// Extracted for testing.
type StreamClient interface {
	StartStreamContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
	AppendStreamContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error)
	StopStreamContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error)

	// StartStreamWithModeContext starts a stream with the task_display_mode parameter.
	// Use taskDisplayMode "plan" for multi-task agents, "timeline" for sequential display.
	StartStreamWithModeContext(ctx context.Context, channelID, taskDisplayMode string, options ...slack.MsgOption) (string, string, error)

	// AppendStreamChunksContext appends structured chunks (task_update, plan_update)
	// to a streaming message. chunksJSON is the JSON-encoded chunks array.
	AppendStreamChunksContext(ctx context.Context, channelID, timestamp, chunksJSON string) error

	// DeleteMessageContext deletes a Slack message. Used to remove empty streaming
	// messages that had no content appended during the session.
	DeleteMessageContext(ctx context.Context, channelID, timestamp string) (string, string, error)
}

// StreamChunk represents a structured chunk for the Slack streaming API.
type StreamChunk struct {
	Type    string              `json:"type"`
	ID      string              `json:"id,omitempty"`
	Title   string              `json:"title,omitempty"`
	Status  string              `json:"status,omitempty"`
	Details string              `json:"details,omitempty"`
	Output  string              `json:"output,omitempty"`
	Text    string              `json:"text,omitempty"`
	Sources []StreamChunkSource `json:"sources,omitempty"`
}

// StreamChunkSource is a URL source reference within a task_update chunk.
type StreamChunkSource struct {
	Type string `json:"type"`
	Text string `json:"text"`
	URL  string `json:"url"`
}

// SlackStreamAdapter wraps *slack.Client to implement StreamClient with extended
// support for the chunks and task_display_mode parameters (not yet in slack-go).
type SlackStreamAdapter struct {
	client *slack.Client
	teamID string // Slack workspace team ID (required for stream APIs)
}

// NewSlackStreamAdapter creates a new adapter wrapping a Slack client.
func NewSlackStreamAdapter(client *slack.Client) *SlackStreamAdapter {
	return &SlackStreamAdapter{client: client}
}

// SetTeamID sets the Slack workspace team ID on the adapter.
func (a *SlackStreamAdapter) SetTeamID(teamID string) {
	a.teamID = teamID
}

func (a *SlackStreamAdapter) StartStreamContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error) {
	opts := a.withTeamID(options)
	return a.client.StartStreamContext(ctx, channelID, opts...)
}

func (a *SlackStreamAdapter) AppendStreamContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error) {
	opts := a.withTeamID(options)
	return a.client.AppendStreamContext(ctx, channelID, timestamp, opts...)
}

func (a *SlackStreamAdapter) StopStreamContext(ctx context.Context, channelID, timestamp string, options ...slack.MsgOption) (string, string, error) {
	opts := a.withTeamID(options)
	return a.client.StopStreamContext(ctx, channelID, timestamp, opts...)
}

func (a *SlackStreamAdapter) DeleteMessageContext(ctx context.Context, channelID, timestamp string) (string, string, error) {
	return a.client.DeleteMessageContext(ctx, channelID, timestamp)
}

// withTeamID prepends MsgOptionRecipientTeamID if a team ID is configured.
func (a *SlackStreamAdapter) withTeamID(options []slack.MsgOption) []slack.MsgOption {
	if a.teamID == "" {
		return options
	}
	opts := make([]slack.MsgOption, 0, len(options)+1)
	opts = append(opts, slack.MsgOptionRecipientTeamID(a.teamID))
	opts = append(opts, options...)
	return opts
}

// StartStreamWithModeContext starts a stream with task_display_mode set via
// UnsafeMsgOptionEndpoint (runs before MsgOptionStartStream to inject values).
func (a *SlackStreamAdapter) StartStreamWithModeContext(ctx context.Context, channelID, taskDisplayMode string, options ...slack.MsgOption) (string, string, error) {
	allOpts := make([]slack.MsgOption, 0, len(options)+3)
	// UnsafeMsgOptionEndpoint runs first: injects task_display_mode into form values.
	// MsgOptionStartStream runs second: sets the correct API endpoint.
	allOpts = append(allOpts, slack.UnsafeMsgOptionEndpoint("_placeholder", func(v url.Values) {
		if taskDisplayMode != "" {
			v.Set("task_display_mode", taskDisplayMode)
		}
	}))
	allOpts = append(allOpts, slack.MsgOptionStartStream())
	if a.teamID != "" {
		allOpts = append(allOpts, slack.MsgOptionRecipientTeamID(a.teamID))
	}
	allOpts = append(allOpts, options...)
	ch, ts, _, err := a.client.SendMessageContext(ctx, channelID, allOpts...)
	return ch, ts, err
}

// AppendStreamChunksContext appends structured chunks to a streaming message
// using UnsafeMsgOptionEndpoint to inject the chunks form parameter.
func (a *SlackStreamAdapter) AppendStreamChunksContext(ctx context.Context, channelID, timestamp, chunksJSON string) error {
	opts := []slack.MsgOption{
		// UnsafeMsgOptionEndpoint runs first: injects chunks into form values.
		slack.UnsafeMsgOptionEndpoint("_placeholder", func(v url.Values) {
			v.Set("chunks", chunksJSON)
		}),
		// MsgOptionAppendStream runs second: sets correct endpoint + ts.
		slack.MsgOptionAppendStream(timestamp),
	}
	if a.teamID != "" {
		opts = append(opts, slack.MsgOptionRecipientTeamID(a.teamID))
	}
	_, _, _, err := a.client.SendMessageContext(ctx, channelID, opts...)
	return err
}

// ThreadStatusSetter updates the assistant thread status text in Slack.
// Extracted as an interface so AgentStreamManager doesn't depend on the full Bot.
type ThreadStatusSetter interface {
	SetThreadStatus(ctx context.Context, channelID, threadTS, status string)
}

// AgentCardRefresher refreshes the Slack agent card with updated tool stats.
// Extracted as an interface so AgentStreamManager doesn't depend on the full Bot.
type AgentCardRefresher interface {
	RefreshAgentCard(ctx context.Context, agentName string)
}

// ThreadMessagePoster posts a persistent plain message into a Slack thread.
// Distinct from ThreadStatusSetter (which updates the ephemeral assistant status
// bar): this is used for the durable "Resumed in Xs" confirmation. Extracted as
// an interface so AgentStreamManager doesn't depend on the full Bot.
type ThreadMessagePoster interface {
	PostThreadMessage(ctx context.Context, channelID, threadTS, text string)
}

// TTFTRecorder persists the boot→first-token time-to-first-token (in
// milliseconds) onto an agent's bead, so it is queryable via the API
// (kd show <agent> --json .fields.ttft_ms) and rendered on the agent card.
// Extracted as an interface so AgentStreamManager doesn't depend on the full Bot
// and stays trivially fakeable in tests. ka-0000003911.
type TTFTRecorder interface {
	RecordTTFT(ctx context.Context, agentName string, ttftMs int64)
}

// AgentStreamManager tracks active streaming sessions per agent. Each agent
// with an active assistant thread gets a streaming message showing real-time
// tool use stats.
type AgentStreamManager struct {
	api           StreamClient
	logger        *slog.Logger
	threadStatus  ThreadStatusSetter    // optional — updates assistant thread status text
	threadPoster  ThreadMessagePoster   // optional — posts durable thread messages (resume confirmation)
	cardRefresh   AgentCardRefresher    // optional — refreshes agent card with tool stats
	modelFallback *ModelFallbackFlagger // optional — flags served-vs-expected model divergence on usage events
	refusalLog    *RefusalLogger        // optional — persists + flags model refusals (stop_reason=refusal) on refusal events
	teamID        string                // Slack workspace team ID (required for stream APIs)
	mu            sync.Mutex
	sessions      map[string]*agentStreamSession // agent name → session

	// coldRespawnStart records, per agent name, the wall-clock instant a COLD
	// thread-agent respawn began (top of respawnThreadAgent). HandleSessionStart
	// closes the gardenio_mention_cold_respawn_seconds histogram against it when
	// the respawned agent first proves life, then deletes the entry. Keyed
	// separately from sessions because the respawn stamps the time before any
	// stream session exists. ka-0000001565.
	coldRespawnStart map[string]time.Time

	// pendingLive records agents whose SessionStart hook fired BEFORE a
	// streaming/card session was registered. SessionStart is the earliest
	// proof-of-life, but the NATS hook can beat session registration on a
	// prewarmed-pool assignment, a bridge restart, or any spawn where the
	// hook outruns NotifyAgentSpawn. Without this buffer the live signal is
	// dropped and the card sits at "starting" until the first tool event or
	// the controller's 60s SyncAll poll. StartSession/StartCardSession drain
	// this set so the session is born live and effectiveAgentState promotes
	// it immediately. Timestamped so gcStaleSessions can evict entries whose
	// session never materialized (agent died during boot). ka-0000002206.
	pendingLive map[string]time.Time

	// triggerSent records, per agent name, the send-time of the Slack message
	// that triggered the agent, stamped at mention-dispatch time (which can run
	// before the stream session is registered — prewarmed-pool assignment, cold
	// respawn, or any spawn where dispatch outruns StartSession). StartSession
	// drains this into the session's triggerSentAt so the first assistant_text
	// event can report time-to-first-token. Entries are evicted by
	// gcStaleSessions if a session never materializes.
	triggerSent map[string]time.Time

	// coldResumeMeta records, per agent name, the resume labels captured when a
	// COLD thread-agent respawn began, so HandleSessionStart can also observe the
	// shared gardenio_resume_latency_seconds{path=cold} histogram against the same
	// t0/proof-of-life the cold-respawn histogram uses (task ka-0000004119 / C7).
	// Set alongside coldRespawnStart by MarkColdRespawn; cleared in the same
	// SessionStart critical section. Populated only when the shared resume metric
	// is wired (SetResumeMetrics) — otherwise MarkColdRespawn leaves it empty and
	// the tee is a no-op.
	coldResumeMeta map[string]resumeLabels

	// resumeMetrics is the shared resume-latency histogram
	// (gardenio_resume_latency_seconds). Nil unless SetResumeMetrics wires it. When
	// present, a cold thread-agent RESUME (respawnThreadAgent) observes one sample
	// with path=cold/regime=R0 at the same first-proof-of-life the cold-respawn
	// histogram closes on. This is the common non-runsc resume path the
	// controller's thaw-only call site never covers (the reason the metric emitted
	// zero prod samples — task ka-0000004119). Frozen-awaiting-thaw resumes are
	// EXCLUDED here (the controller emits their thaw-warm/thaw-fresh sample) so the
	// two emitters stay disjoint and never double-count.
	resumeMetrics resumeObserver

	// respawnPhase records, per agent name, the two boundary instants a cold
	// thread-agent respawn crosses on the bridge side — t0 (follow-up arrival,
	// top of respawnThreadAgent) and dispatchDone (the moment respawnThreadAgent
	// finished minting the resume bead and armed the resume tracker) — plus the
	// agent's role. HandleSessionStart reads it at first proof-of-life to observe
	// the per-phase breakdown (gardenio_slackbridge_respawn_phase_seconds:
	// dispatch = dispatchDone-t0, boot = SessionStart-dispatchDone, total =
	// SessionStart-t0), then deletes the entry. Set alongside coldRespawnStart by
	// MarkColdResume / MarkColdRespawn and evicted in lockstep by gcStaleSessions.
	// task ka-0000001457.
	respawnPhase map[string]respawnPhaseStamp

	// ttftRecorder persists the boot→first-token TTFT (ms) onto the agent bead.
	// Nil unless SetTTFTRecorder wires it — then the first assistant_text event
	// after a known SessionStart (liveAt) records ttft_ms once per session. A nil
	// recorder leaves the store a no-op (the card still shows nothing), exactly
	// as before this wiring existed. ka-0000003911.
	ttftRecorder TTFTRecorder
}

// respawnPhaseStamp carries the bridge-observed boundary instants + role of a
// cold thread-agent respawn so the delayed proof-of-life (SessionStart) can
// attribute the latency to a bridge-side (dispatch) vs pod-side (boot) phase.
type respawnPhaseStamp struct {
	t0           time.Time // follow-up arrival (top of respawnThreadAgent)
	dispatchDone time.Time // resume bead minted + tracker armed (end of synchronous dispatch)
	role         string    // agent role for the phase-series label (empty → "unknown")
}

// resumeLabels carries the resume-latency label set captured at cold-respawn
// time so the delayed proof-of-life observation can attribute the sample. armed
// distinguishes a MarkColdResume stamp (tee one path=cold sample) from a plain
// MarkColdRespawn (no tee — another emitter covers this wake).
type resumeLabels struct {
	role    string
	runtime string
	armed   bool
	// channel/threadTS anchor the "Resumed in Xs" thread post that
	// HandleSessionStart emits when a cold thread-agent resume completes. Empty
	// when the resume did not originate from a Slack thread (then no post is
	// made). See ka-0000002949 / ka-0000005084.
	channel  string
	threadTS string
}

// resumeObserver is the minimal surface AgentStreamManager needs from the shared
// metrics.ResumeMetrics — one Observe call. An interface keeps the bridge package
// decoupled from the concrete metrics type (and trivially fakeable in tests).
type resumeObserver interface {
	RecordResumeLatency(seconds float64, regime, role, runtime, path string) bool
}

// agentStreamSession tracks a single agent's streaming state.
type agentStreamSession struct {
	channelID      string
	threadTS       string         // assistant thread ts (or card ts for card-only sessions)
	streamTS       string         // streaming message ts (from startStream; empty for card-only)
	userID         string         // Slack user ID of the thread owner (for recipient_user_id)
	cardOnly       bool           // true = card-only session (no Slack streaming API calls)
	toolCounts     map[string]int // tool_name → count
	totalTools     int            // total tool invocations
	chunksAppended int            // number of content chunks appended (excludes stop summary)
	lastAppend     time.Time      // last appendStream call
	startedAt      time.Time      // when the session started
	activity       string         // current activity description (e.g. "reading files")
	lastTool       string         // last tool name used (e.g. "Read", "Bash")
	lastToolLabel  string         // short label from tool input (e.g. "git status", "settings.json")
	live           bool           // SessionStart hook has fired — Claude is alive in the container
	dirty          bool           // needs flush
	stopped        bool           // stream has been finalized

	// triggerSentAt is the wall-clock instant the user's Slack message that
	// triggered this session was sent (parsed from the Slack message ts). Zero
	// when unknown. Used to compute time-to-first-token on the first
	// assistant_text event. ttftShown latches true once the TTFT annotation has
	// been emitted so it is only ever prepended to the *first* agent message.
	triggerSentAt time.Time
	ttftShown     bool

	// liveAt is the wall-clock instant this agent first proved life IN the
	// container — the SessionStart hook (Claude booted and running). Distinct
	// from startedAt (when the bridge registered the stream session) and from
	// triggerSentAt (when the user's Slack message was sent). It anchors the
	// boot→first-token TTFT: the delta liveAt→first assistant_text is the
	// agent's own first-inference latency and, because it starts at
	// proof-of-life, EXCLUDES spawn queue / image-pull / network delay
	// (ka-0000003911). Zero until SessionStart is observed. ttftMsShown latches
	// once the boot→first-token measurement has been consumed so it is recorded
	// at most once per session.
	liveAt      time.Time
	ttftMsShown bool
}

// NewAgentStreamManager creates a new manager for streaming agent status.
func NewAgentStreamManager(api StreamClient, logger *slog.Logger) *AgentStreamManager {
	return &AgentStreamManager{
		api:              api,
		logger:           logger,
		sessions:         make(map[string]*agentStreamSession),
		coldRespawnStart: make(map[string]time.Time),
		coldResumeMeta:   make(map[string]resumeLabels),
		pendingLive:      make(map[string]time.Time),
		triggerSent:      make(map[string]time.Time),
		respawnPhase:     make(map[string]respawnPhaseStamp),
	}
}

// SetResumeMetrics wires the shared resume-latency histogram so a cold
// thread-agent resume (respawnThreadAgent) tees a gardenio_resume_latency_seconds
// {path=cold} sample at first proof-of-life, alongside the cold-respawn
// histogram. Optional: nil (unset) leaves the tee a no-op, exactly as before this
// wiring existed. Called once at bridge startup (task ka-0000004119 / C7).
func (m *AgentStreamManager) SetResumeMetrics(rm resumeObserver) {
	m.resumeMetrics = rm
}

// SetThreadStatusSetter configures the optional assistant thread status updater.
// When set, the flush loop updates the thread status with the agent's current
// activity (e.g., "reading files", "running commands").
func (m *AgentStreamManager) SetThreadStatusSetter(setter ThreadStatusSetter) {
	m.threadStatus = setter
}

// SetAgentCardRefresher configures the optional agent card refresher.
// When set, the flush loop also refreshes agent cards with live tool stats,
// keeping the card in sync with the thread status bar.
func (m *AgentStreamManager) SetAgentCardRefresher(refresher AgentCardRefresher) {
	m.cardRefresh = refresher
}

// SetThreadMessagePoster configures the optional durable thread poster. When
// set, HandleSessionStart posts a "Resumed in Xs" confirmation to the thread
// that a cold thread-agent resume originated from (ka-0000005084).
func (m *AgentStreamManager) SetThreadMessagePoster(poster ThreadMessagePoster) {
	m.threadPoster = poster
}

// SetTTFTRecorder configures the optional boot→first-token recorder. When set,
// the first assistant_text event of a session whose SessionStart was observed
// stores the in-container boot→first-token latency as ttft_ms on the agent bead
// (ka-0000003911). Nil (unset) leaves the store a no-op.
func (m *AgentStreamManager) SetTTFTRecorder(r TTFTRecorder) {
	m.ttftRecorder = r
}

// SetModelFallbackFlagger configures the optional model-fallback flagger. When
// set, the usage handler compares each turn's SERVED model against the pod's
// EXPECTED (baked) model and, on a real divergence, posts a Slack flag and writes
// a durable record (ka-0000003127). Nil (unset) leaves the check a no-op.
func (m *AgentStreamManager) SetModelFallbackFlagger(f *ModelFallbackFlagger) {
	m.modelFallback = f
}

// SetRefusalLogger configures the optional refusal logger. When set, the refusal
// handler persists each coop-classified model refusal (stop_reason=refusal) as a
// durable type=rating record on the surveys/ratings surface and posts a Slack flag
// (ka-0000006122). Nil (unset) leaves the refusal handler a no-op.
func (m *AgentStreamManager) SetRefusalLogger(f *RefusalLogger) {
	m.refusalLog = f
}

// SetTeamID sets the Slack workspace team ID required by the streaming APIs.
// Called after Bot.Run completes AuthTest and obtains the team ID.
// Propagates to the underlying StreamClient if it supports SetTeamID.
func (m *AgentStreamManager) SetTeamID(teamID string) {
	m.teamID = teamID
	if setter, ok := m.api.(interface{ SetTeamID(string) }); ok {
		setter.SetTeamID(teamID)
	}
	m.logger.Info("agentstream: team ID configured", "team_id", teamID)
}

// MarkColdRespawn records the instant a COLD thread-agent respawn began for
// agentName, so HandleSessionStart can close the cold-respawn latency histogram
// when that agent first proves life. Called from respawnThreadAgent. Overwrites
// any prior pending stamp for the same agent (a fresh follow-up resets the
// clock). This variant does NOT arm the shared resume-latency tee — use it for
// respawns that another emitter already covers (a frozen-awaiting-thaw resume,
// whose thaw the controller records as path=thaw-warm/thaw-fresh). It clears any
// stale resume-meta so a prior armed follow-up can never leak a cold sample onto
// this respawn. ka-0000001565.
func (m *AgentStreamManager) MarkColdRespawn(agentName string, at time.Time) {
	agentName = extractAgentName(agentName)
	m.mu.Lock()
	m.coldRespawnStart[agentName] = at
	delete(m.coldResumeMeta, agentName)
	m.mu.Unlock()
}

// MarkColdResume is MarkColdRespawn that ALSO arms the shared resume-latency
// tee. A cold thread-agent respawn IS a genuine session resume
// (respawnThreadAgent re-mints the same-name bead with --resume /
// BOAT_SESSION_RESUME so coop restores the prior transcript), so when the shared
// resume histogram is wired (SetResumeMetrics) HandleSessionStart also observes
// one gardenio_resume_latency_seconds{path=cold} sample against this same t0/
// proof-of-life. role/runtime are the labels that sample carries; both may be
// empty (role collapses to "unknown"; runtime defaults to runc — the class the
// non-runsc cold-resume path actually uses). This is the dominant resume path
// the controller's thaw-only call site never covered — the reason the metric
// emitted zero prod samples (task ka-0000004119 / C7).
//
// Presence of a coldResumeMeta entry is the tee gate: only MarkColdResume sets
// it, so a frozen-awaiting-thaw respawn (which the controller's thaw emitter
// already covers) must use MarkColdRespawn instead and stays disjoint — the two
// emitters never double-count the same wake.
// channel/threadTS, when non-empty, anchor a user-visible "Resumed in Xs" post
// to that Slack thread when the resume completes (first proof-of-life) — the
// visible confirmation Matt asked for now that a pod-gone follow-up resumes
// silently instead of posting the old "nudge failed" warning (ka-0000005084).
func (m *AgentStreamManager) MarkColdResume(agentName string, at time.Time, role, runtime, channel, threadTS string) {
	agentName = extractAgentName(agentName)
	m.mu.Lock()
	m.coldRespawnStart[agentName] = at
	m.coldResumeMeta[agentName] = resumeLabels{
		role: role, runtime: runtime, armed: true,
		channel: channel, threadTS: threadTS,
	}
	m.mu.Unlock()
}

// MarkRespawnPhase arms the per-phase cold-respawn breakdown for agentName.
// respawnThreadAgent calls it alongside MarkColdResume/MarkColdRespawn once it
// has finished its synchronous dispatch work (bead mint + tracker arm): t0 is
// the follow-up-arrival instant (the same respawnStart the cold-respawn
// histogram uses), `at` (== now) is the dispatch-done boundary, and role labels
// the emitted series. HandleSessionStart reads the stamp at first proof-of-life
// to observe gardenio_slackbridge_respawn_phase_seconds{phase=dispatch/boot/total}
// and then clears it. Overwrites any prior pending stamp for the same agent (a
// fresh follow-up resets the clock), mirroring MarkColdRespawn. task
// ka-0000001457.
func (m *AgentStreamManager) MarkRespawnPhase(agentName string, t0, at time.Time, role string) {
	agentName = extractAgentName(agentName)
	m.mu.Lock()
	m.respawnPhase[agentName] = respawnPhaseStamp{t0: t0, dispatchDone: at, role: role}
	m.mu.Unlock()
}

// MarkTriggerSent records the send-time of the Slack message that triggered
// agentName, so the first assistant_text event can report time-to-first-token.
// Called from the mention-dispatch paths (spawn / warm nudge / cold respawn).
// If a session already exists, the time is applied directly; otherwise it is
// buffered in triggerSent for StartSession to drain. A zero `at` is ignored.
func (m *AgentStreamManager) MarkTriggerSent(agentName string, at time.Time) {
	if at.IsZero() {
		return
	}
	agentName = extractAgentName(agentName)
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[agentName]; ok && !s.stopped {
		// A live session already owns the TTFT clock. Adopt the time only if it
		// hasn't emitted TTFT yet, keeping the earliest known send-time so a
		// burst of follow-ups doesn't push the anchor past the message that
		// actually woke the agent. Once TTFT is shown, do nothing — and crucially
		// do NOT fall through to the buffer, or a later cold respawn's
		// StartSession would drain this stale time and mislabel its first message.
		if !s.ttftShown && (s.triggerSentAt.IsZero() || at.Before(s.triggerSentAt)) {
			s.triggerSentAt = at
		}
		return
	}
	if prev, ok := m.triggerSent[agentName]; !ok || at.Before(prev) {
		m.triggerSent[agentName] = at
	}
}

// StartSession begins a streaming message in the agent's assistant thread.
// Called when an agent spawns in an assistant thread context.
// userID is the Slack user ID of the thread owner — required by the Slack
// streaming API as recipient_user_id (pass "" if unknown; the call may fail).
// Returns the streaming message timestamp, or "" on failure or duplicate.
func (m *AgentStreamManager) StartSession(ctx context.Context, agentName, channelID, threadTS, userID string) string {
	return m.StartSessionWithText(ctx, agentName, channelID, threadTS, userID, "")
}

// StartSessionWithText begins a streaming message with custom initial text.
// If initialText is empty, a default message is used. Returns the streaming
// message timestamp, or "" on failure or duplicate.
func (m *AgentStreamManager) StartSessionWithText(ctx context.Context, agentName, channelID, threadTS, userID, initialText string) string {
	m.mu.Lock()
	// Don't start duplicate sessions — check for active or pending sessions.
	if s, ok := m.sessions[agentName]; ok && !s.stopped {
		m.mu.Unlock()
		return ""
	}
	// Reserve the slot with a pending sentinel so concurrent callers see it
	// as occupied while we make the Slack API call without holding the lock.
	// Apply any buffered live signal: if SessionStart fired before this
	// session existed, the agent is already proven alive. ka-0000002206. The
	// buffered value is the SessionStart instant, so it also seeds liveAt — the
	// boot→first-token TTFT anchor for a session born already-live (prewarmed
	// assign / cold respawn / hook outrunning StartSession). ka-0000003911.
	liveSince, live := m.pendingLive[agentName]
	delete(m.pendingLive, agentName)
	// Drain any trigger send-time buffered before the session existed (cold
	// respawn / prewarmed assign / dispatch racing StartSession) so the first
	// assistant_text event can report time-to-first-token. ka-0000001004.
	triggerSentAt := m.triggerSent[agentName]
	delete(m.triggerSent, agentName)
	m.sessions[agentName] = &agentStreamSession{
		channelID:     channelID,
		threadTS:      threadTS,
		userID:        userID,
		toolCounts:    make(map[string]int),
		startedAt:     time.Now(),
		live:          live,
		liveAt:        liveSince,
		triggerSentAt: triggerSentAt,
	}
	m.mu.Unlock()

	// Build start options.
	opts := []slack.MsgOption{
		slack.MsgOptionTS(threadTS),
	}
	if userID != "" {
		opts = append(opts, slack.MsgOptionRecipientUserID(userID))
	}

	// Start the Slack stream in "timeline" mode with structured chunk support.
	// All updates — tool use stats and task status changes — are sent as
	// structured task_update chunks via AppendStreamChunksContext, using
	// ID-based deduplication so each flush replaces the previous content.
	// This makes the purple shimmer show the same status text as the gray
	// assistant thread footer.
	_, streamTS, err := m.api.StartStreamWithModeContext(ctx, channelID, "timeline", opts...)
	if err != nil {
		m.logger.Warn("agentstream: failed to start stream",
			"agent", agentName, "channel", channelID, "error", err)
		// Remove the pending sentinel on failure so the slot can be retried.
		m.mu.Lock()
		delete(m.sessions, agentName)
		m.mu.Unlock()
		return ""
	}

	// Fill in the stream timestamp now that the API call succeeded.
	m.mu.Lock()
	live = false
	if s, ok := m.sessions[agentName]; ok {
		s.streamTS = streamTS
		live = s.live
	}
	m.mu.Unlock()

	m.logger.Info("agentstream: started stream",
		"agent", agentName, "channel", channelID, "stream_ts", streamTS)

	// If SessionStart proved life before this session existed (buffered via
	// pendingLive), re-render the card now so it shows "working" instead of the
	// just-posted "starting". ka-0000002206.
	if live && m.cardRefresh != nil {
		m.cardRefresh.RefreshAgentCard(ctx, agentName)
	}
	return streamTS
}

// StartCardSession begins tracking tool events for an agent's card thread
// without calling the Slack streaming APIs (startStream/appendStream/stopStream).
// This enables live tool stats on agent cards for non-thread-spawned agents.
// Returns true if a new session was created, false if one already exists.
func (m *AgentStreamManager) StartCardSession(agentName, channelID, cardTS string) bool {
	m.mu.Lock()

	if s, ok := m.sessions[agentName]; ok && !s.stopped {
		m.mu.Unlock()
		return false // session already exists
	}

	// Apply any buffered live signal: if SessionStart fired before this
	// card session existed, the agent is already proven alive. ka-0000002206.
	// The buffered value is the SessionStart instant, so it also seeds liveAt —
	// the boot→first-token TTFT anchor. ka-0000003911.
	liveSince, live := m.pendingLive[agentName]
	delete(m.pendingLive, agentName)
	m.sessions[agentName] = &agentStreamSession{
		channelID:  channelID,
		threadTS:   cardTS,
		cardOnly:   true, // no Slack streaming API calls
		toolCounts: make(map[string]int),
		startedAt:  time.Now(),
		live:       live,
		liveAt:     liveSince,
	}
	m.mu.Unlock()

	m.logger.Info("agentstream: started card session",
		"agent", agentName, "channel", channelID, "card_ts", cardTS)

	// The card was just rendered as "spawning"; if SessionStart already proved
	// life, re-render now so it shows "working" instead of waiting for the first
	// tool event or the 60s SyncAll poll. ka-0000002206.
	if live && m.cardRefresh != nil {
		m.cardRefresh.RefreshAgentCard(context.Background(), agentName)
	}
	return true
}

// HandleToolEvent processes a tool use hook event and updates the agent's
// streaming session. Marks the session dirty for the next flush cycle.
// toolLabel is a short description extracted from tool_input (e.g. the Bash
// description or command, the Read file path). Pass "" when unavailable.
func (m *AgentStreamManager) HandleToolEvent(agentName, toolName, eventType, toolLabel string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[agentName]
	if !ok || s.stopped {
		return
	}

	if eventType == "PreToolUse" {
		s.toolCounts[toolName]++
		s.totalTools++
		s.activity = toolActivity(toolName)
		s.lastTool = toolName
		s.lastToolLabel = toolLabel
		s.dirty = true
	} else if eventType == "PostToolUse" {
		// Update activity to reflect completion.
		s.activity = ""
		s.dirty = true
	}
}

// StopSession finalizes the streaming message for an agent.
// Called when the agent closes or crashes.
func (m *AgentStreamManager) StopSession(ctx context.Context, agentName, outcome string) {
	m.mu.Lock()
	s, ok := m.sessions[agentName]
	if !ok || s.stopped {
		m.mu.Unlock()
		return
	}
	s.stopped = true
	// Copy data under lock.
	channelID := s.channelID
	threadTS := s.threadTS
	streamTS := s.streamTS
	cardOnly := s.cardOnly
	totalTools := s.totalTools
	chunksAppended := s.chunksAppended
	toolCounts := make(map[string]int, len(s.toolCounts))
	for k, v := range s.toolCounts {
		toolCounts[k] = v
	}
	duration := time.Since(s.startedAt)
	delete(m.sessions, agentName)
	m.mu.Unlock()

	// Card-only sessions: just refresh the card one last time and clean up.
	// No Slack streaming API calls needed.
	if cardOnly {
		if m.cardRefresh != nil {
			m.cardRefresh.RefreshAgentCard(ctx, agentName)
		}
		m.logger.Info("agentstream: stopped card session",
			"agent", agentName, "outcome", outcome, "tools", totalTools,
			"duration", duration.Round(time.Second))
		return
	}

	// Clear the assistant thread status now that the agent is done.
	if m.threadStatus != nil {
		m.threadStatus.SetThreadStatus(ctx, channelID, threadTS, "")
	}

	// If the stream had no tool calls and no content chunks were appended,
	// delete the empty streaming message instead of leaving a "0 tools" stub.
	if streamTS != "" && totalTools == 0 && chunksAppended == 0 {
		_, _, err := m.api.DeleteMessageContext(ctx, channelID, streamTS)
		if err != nil {
			m.logger.Warn("agentstream: failed to delete empty stream",
				"agent", agentName, "channel", channelID, "stream_ts", streamTS, "error", err)
		} else {
			m.logger.Info("agentstream: deleted empty stream",
				"agent", agentName, "channel", channelID, "stream_ts", streamTS,
				"duration", duration.Round(time.Second))
		}
		return
	}

	// Build final summary and send as a structured chunk before stopping.
	summary := buildStopSummary(m.themeFor(ctx, channelID), agentName, outcome, totalTools, toolCounts, duration)

	// Send the final summary as a task_update chunk with "complete" status.
	// The "agent-status" ID replaces the in-progress status chunk from flush().
	m.appendChunk(ctx, channelID, streamTS, StreamChunk{
		Type:    "task_update",
		ID:      "agent-status",
		Title:   fmt.Sprintf("Agent %s — %s", agentName, outcome),
		Status:  "complete",
		Details: summary,
	})

	// StopStream finalizes the streaming message (skip if stream was never started).
	if streamTS == "" {
		m.logger.Info("agentstream: stopped session (no stream)",
			"agent", agentName, "outcome", outcome, "tools", totalTools,
			"duration", duration.Round(time.Second))
		return
	}
	_, _, err := m.api.StopStreamContext(ctx, channelID, streamTS)
	if err != nil {
		m.logger.Warn("agentstream: failed to stop stream",
			"agent", agentName, "error", err)
		return
	}

	m.logger.Info("agentstream: stopped stream",
		"agent", agentName, "outcome", outcome, "tools", totalTools,
		"duration", duration.Round(time.Second))
}

// Run starts the background flush loop. Blocks until ctx is cancelled.
func (m *AgentStreamManager) Run(ctx context.Context) {
	ticker := time.NewTicker(streamFlushInterval)
	defer ticker.Stop()

	// GC ticker for stale sessions.
	gcTicker := time.NewTicker(5 * time.Minute)
	defer gcTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.flush(ctx)
		case <-gcTicker.C:
			m.gcStaleSessions()
		}
	}
}

// flush updates streaming sessions with structured task_update chunks.
// Each flush sends an "agent-status" chunk via AppendStreamChunksContext,
// using ID-based replacement so the shimmer content shows the latest status
// instead of Slack's generic placeholder.
func (m *AgentStreamManager) flush(ctx context.Context) {
	m.mu.Lock()
	type flushItem struct {
		agent         string
		channelID     string
		threadTS      string
		streamTS      string
		cardOnly      bool
		activity      string
		lastTool      string
		lastToolLabel string
		totalTools    int
		toolCounts    map[string]int
		startedAt     time.Time
	}
	var items []flushItem

	for agent, s := range m.sessions {
		if !s.dirty || s.stopped {
			continue
		}
		s.dirty = false
		s.lastAppend = time.Now()
		s.chunksAppended++

		// Copy tool counts for status display.
		tc := make(map[string]int, len(s.toolCounts))
		for k, v := range s.toolCounts {
			tc[k] = v
		}
		items = append(items, flushItem{
			agent:         agent,
			channelID:     s.channelID,
			threadTS:      s.threadTS,
			streamTS:      s.streamTS,
			cardOnly:      s.cardOnly,
			activity:      s.activity,
			lastTool:      s.lastTool,
			lastToolLabel: s.lastToolLabel,
			totalTools:    s.totalTools,
			toolCounts:    tc,
			startedAt:     s.startedAt,
		})
	}
	m.mu.Unlock()

	for _, item := range items {
		status := buildThreadStatusText(m.themeFor(ctx, item.channelID), item.activity, item.lastTool, item.lastToolLabel, item.totalTools, item.toolCounts, item.startedAt)

		// Update assistant thread footer (gray status bar).
		if m.threadStatus != nil && !item.cardOnly {
			if status != "" {
				m.threadStatus.SetThreadStatus(ctx, item.channelID, item.threadTS, status)
			}
		}

		// Send structured chunk to the stream (purple shimmer) so it shows
		// the same text as the assistant thread footer.
		if !item.cardOnly && item.streamTS != "" && status != "" {
			m.appendChunk(ctx, item.channelID, item.streamTS, StreamChunk{
				Type:   "task_update",
				ID:     "agent-status",
				Title:  status,
				Status: "in_progress",
			})
		}

		// Also refresh the agent card so it shows the same tool stats as the thread status bar.
		if m.cardRefresh != nil {
			m.cardRefresh.RefreshAgentCard(ctx, item.agent)
		}
	}
}

// buildThreadStatusText builds a concise status string for the thread status bar.
// Shows the current activity, last tool used (with label context), total tool count,
// and elapsed time.
// Examples:
//
//	"running commands... (44 tools) [9m57s] · last: Bash(show working tree status)"
//	"Working... (12 tools) [2m30s] · last: Read(settings.json)"
func buildThreadStatusText(th Theme, activity, lastTool, lastToolLabel string, totalTools int, toolCounts map[string]int, startedAt time.Time) string {
	if totalTools == 0 && activity == "" {
		return ""
	}

	// The working glyph leads the text and stays glued to its verbal label (the
	// activity, or "Working") in the same run — accessibility rule.
	var b strings.Builder
	b.WriteString(th.Glyph(LifecycleWorking))
	b.WriteByte(' ')
	if activity != "" {
		fmt.Fprintf(&b, "%s...", activity)
	} else {
		b.WriteString("Working...")
	}

	if totalTools > 0 {
		fmt.Fprintf(&b, " (%d tools)", totalTools)
	}

	if !startedAt.IsZero() {
		elapsed := time.Since(startedAt).Truncate(time.Second)
		fmt.Fprintf(&b, " [%s]", formatDuration(elapsed))
	}

	if lastTool != "" {
		if lastToolLabel != "" {
			fmt.Fprintf(&b, " · last: %s(%s)", lastTool, lastToolLabel)
		} else {
			fmt.Fprintf(&b, " · last: %s", lastTool)
		}
	}

	return b.String()
}

// gcStaleSessions removes sessions older than streamSessionMaxAge.
func (m *AgentStreamManager) gcStaleSessions() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for agent, s := range m.sessions {
		if time.Since(s.startedAt) > streamSessionMaxAge {
			m.logger.Warn("agentstream: gc stale session", "agent", agent,
				"age", time.Since(s.startedAt))
			delete(m.sessions, agent)
		}
	}

	// Evict buffered live signals whose session never materialized (agent
	// died during boot, or the spawn was aborted). These are tiny but must
	// not accumulate unbounded. ka-0000002206.
	for agent, at := range m.pendingLive {
		if time.Since(at) > streamSessionMaxAge {
			delete(m.pendingLive, agent)
		}
	}

	// Evict buffered trigger send-times whose session never materialized, same
	// rationale as pendingLive above. ka-0000001004.
	for agent, at := range m.triggerSent {
		if time.Since(at) > streamSessionMaxAge {
			delete(m.triggerSent, agent)
		}
	}

	// Evict cold-respawn stamps whose SessionStart never fired (the respawned
	// agent died during boot, so the follow-up->first-token close never ran).
	// Keep coldResumeMeta in lockstep so the resume-latency tee's label buffer
	// tracks its clock exactly. ka-0000004119.
	for agent, at := range m.coldRespawnStart {
		if time.Since(at) > streamSessionMaxAge {
			delete(m.coldRespawnStart, agent)
			delete(m.coldResumeMeta, agent)
		}
	}

	// Evict respawn-phase stamps whose SessionStart never fired (the respawned
	// agent died during boot, so the phase-breakdown close never ran), mirroring
	// the cold-respawn eviction above. task ka-0000001457.
	for agent, st := range m.respawnPhase {
		if time.Since(st.t0) > streamSessionMaxAge {
			delete(m.respawnPhase, agent)
		}
	}
}

// HandleBeadClaimed sends a task_update chunk with status "in_progress" to
// the agent's streaming session. Called when a bead is claimed by an agent.
// Also updates the assistant thread status to show the claimed task.
func (m *AgentStreamManager) HandleBeadClaimed(ctx context.Context, agentName, beadID, title string) {
	m.sendTaskUpdate(ctx, agentName, beadID, title, "in_progress", "")

	if m.threadStatus != nil && title != "" {
		m.mu.Lock()
		s, ok := m.sessions[agentName]
		var channelID, threadTS string
		if ok && !s.stopped {
			channelID = s.channelID
			threadTS = s.threadTS
		}
		m.mu.Unlock()
		if channelID != "" {
			m.threadStatus.SetThreadStatus(ctx, channelID, threadTS,
				"Working on: "+truncateText(title, 80))
		}
	}
}

// HandleBeadClosed sends a task_update chunk with status "complete" to
// the agent's streaming session. Called when a bead is closed.
func (m *AgentStreamManager) HandleBeadClosed(ctx context.Context, agentName, beadID, title string) {
	m.sendTaskUpdate(ctx, agentName, beadID, title, "complete", "")
}

// sendTaskUpdate sends a structured task_update chunk to the agent's stream
// using AppendStreamChunksContext. The bead ID is used as the chunk ID so
// updates for the same bead replace in-place instead of appending.
func (m *AgentStreamManager) sendTaskUpdate(ctx context.Context, agentName, beadID, title, status, output string) {
	m.mu.Lock()
	s, ok := m.sessions[agentName]
	if !ok || s.stopped {
		m.mu.Unlock()
		return
	}
	channelID := s.channelID
	streamTS := s.streamTS
	s.chunksAppended++
	m.mu.Unlock()

	chunk := StreamChunk{
		Type:   "task_update",
		ID:     beadID,
		Title:  truncateText(title, 200),
		Status: status,
		Output: output,
	}
	m.appendChunk(ctx, channelID, streamTS, chunk)

	m.logger.Info("agentstream: sent task_update",
		"agent", agentName, "bead", beadID, "status", status)
}

// HandleSubagentStart sends a task_update chunk with status "in_progress" for
// a subagent delegation. Called when a SubagentStart hook event is received.
func (m *AgentStreamManager) HandleSubagentStart(ctx context.Context, agentName, subagentID, subagentType string) {
	title := "Subagent"
	if subagentType != "" {
		title = fmt.Sprintf("Subagent (%s)", subagentType)
	}
	chunkID := "subagent:" + subagentID
	if subagentID == "" {
		chunkID = "subagent:" + agentName
	}
	m.sendTaskUpdate(ctx, agentName, chunkID, title, "in_progress", "")
}

// HandleSubagentStop sends a task_update chunk with status "complete" for
// a subagent delegation. Called when a SubagentStop hook event is received.
// lastMessage is the subagent's final assistant message; it is forwarded as
// the chunk Output so the completed delegation shows its result instead of a
// bare status flip — mirroring HandleStopEvent for the host agent. Empty when
// the hook carries no final message.
func (m *AgentStreamManager) HandleSubagentStop(ctx context.Context, agentName, subagentID, subagentType, lastMessage string) {
	title := "Subagent"
	if subagentType != "" {
		title = fmt.Sprintf("Subagent (%s)", subagentType)
	}
	chunkID := "subagent:" + subagentID
	if subagentID == "" {
		chunkID = "subagent:" + agentName
	}
	m.sendTaskUpdate(ctx, agentName, chunkID, title, "complete", truncateText(lastMessage, 300))
}

// HandleDecisionCreated sends a task_update chunk with status "in_progress"
// when a decision checkpoint is created by the agent. Also updates the
// assistant thread status to indicate the agent is waiting for input.
func (m *AgentStreamManager) HandleDecisionCreated(ctx context.Context, agentName, decisionID, prompt string) {
	title := "Decision checkpoint"
	if prompt != "" {
		title = truncateText(prompt, 100)
	}
	m.sendTaskUpdate(ctx, agentName, decisionID, title, "in_progress", "Waiting for human input")

	if m.threadStatus != nil {
		m.mu.Lock()
		s, ok := m.sessions[agentName]
		var channelID, threadTS string
		if ok && !s.stopped {
			channelID = s.channelID
			threadTS = s.threadTS
		}
		m.mu.Unlock()
		if channelID != "" {
			m.threadStatus.SetThreadStatus(ctx, channelID, threadTS, "Waiting for human input...")
		}
	}
}

// HandleDecisionResolved sends a task_update chunk with status "complete"
// when a decision is resolved.
func (m *AgentStreamManager) HandleDecisionResolved(ctx context.Context, agentName, decisionID, chosen string) {
	title := "Decision checkpoint"
	output := ""
	if chosen != "" {
		output = "Chosen: " + chosen
	}
	m.sendTaskUpdate(ctx, agentName, decisionID, title, "complete", output)
}

// HandleGitEvent sends a task_update chunk for a detected git operation
// (commit, push) extracted from Bash tool output.
func (m *AgentStreamManager) HandleGitEvent(ctx context.Context, agentName, summary string) {
	m.sendTaskUpdate(ctx, agentName, "git:"+agentName, "Git", "complete", summary)
}

// HandleToolError sends a task_update chunk when a tool invocation fails
// (PostToolUseFailure or StopFailure). Shows the error in the stream.
func (m *AgentStreamManager) HandleToolError(ctx context.Context, agentName, toolName, errMsg string) {
	title := fmt.Sprintf("%s failed", toolName)
	m.sendTaskUpdate(ctx, agentName, "error:"+agentName, title, "complete", truncateText(errMsg, 200))
}

// HandleNotification sends a task_update chunk for a Claude Code notification event.
func (m *AgentStreamManager) HandleNotification(ctx context.Context, agentName, notificationType string) {
	title := "Notification"
	if notificationType != "" {
		title = fmt.Sprintf("Notification: %s", notificationType)
	}
	m.sendTaskUpdate(ctx, agentName, "notification:"+agentName, title, "complete", "")
}

// HandleStopEvent processes a Stop hook event. Updates the stream with the
// last assistant message summary if available.
func (m *AgentStreamManager) HandleStopEvent(ctx context.Context, agentName, lastMessage string) {
	if lastMessage == "" {
		return
	}
	m.sendTaskUpdate(ctx, agentName, "stop:"+agentName, "Turn complete", "complete", truncateText(lastMessage, 300))
}

// HandleTaskCreated sends a task_update chunk when an agent creates a new task
// (via the TaskCreate tool). Shows the task in the stream as in_progress.
func (m *AgentStreamManager) HandleTaskCreated(ctx context.Context, agentName, taskID, taskSubject string) {
	title := "Task created"
	if taskSubject != "" {
		title = truncateText(taskSubject, 100)
	}
	id := "task:" + taskID
	if taskID == "" {
		id = "task:" + agentName
	}
	m.sendTaskUpdate(ctx, agentName, id, title, "in_progress", "")
}

// HandleTaskCompleted sends a task_update chunk when an agent completes a task.
func (m *AgentStreamManager) HandleTaskCompleted(ctx context.Context, agentName, taskID, taskSubject string) {
	title := "Task completed"
	if taskSubject != "" {
		title = truncateText(taskSubject, 100)
	}
	id := "task:" + taskID
	if taskID == "" {
		id = "task:" + agentName
	}
	m.sendTaskUpdate(ctx, agentName, id, title, "complete", "")
}

// HandleCwdChanged sends a task_update chunk when the agent's working directory changes.
func (m *AgentStreamManager) HandleCwdChanged(ctx context.Context, agentName, oldCwd, newCwd string) {
	summary := newCwd
	if oldCwd != "" && newCwd != "" {
		summary = oldCwd + " → " + newCwd
	}
	m.sendTaskUpdate(ctx, agentName, "cwd:"+agentName, "Changed directory", "complete", truncateText(summary, 200))
}

// HandleFileChanged sends a task_update chunk when a watched file is modified.
func (m *AgentStreamManager) HandleFileChanged(ctx context.Context, agentName, filePath, changeType string) {
	title := "File changed"
	if changeType != "" {
		title = "File " + changeType
	}
	m.sendTaskUpdate(ctx, agentName, "file:"+agentName, title, "complete", truncateText(filePath, 200))
}

// HandlePermissionRequest sends a task_update chunk when a permission dialog is shown.
func (m *AgentStreamManager) HandlePermissionRequest(ctx context.Context, agentName, toolName, requestID string) {
	title := "Permission requested"
	if toolName != "" {
		title = "Permission: " + toolName
	}
	id := "perm:" + requestID
	if requestID == "" {
		id = "perm:" + agentName
	}
	m.sendTaskUpdate(ctx, agentName, id, title, "in_progress", "")
}

// HandleSessionStart sends a task_update chunk when a new session begins.
func (m *AgentStreamManager) HandleSessionStart(ctx context.Context, agentName, source string) {
	title := "Session started"
	if source != "" {
		title = "Session: " + source
	}
	m.sendTaskUpdate(ctx, agentName, "session:"+agentName, title, "complete", "")

	// SessionStart is the earliest proof-of-life: Claude has booted and is
	// running in the container, even though it has not yet invoked any tool.
	// Mark the session live and refresh the card so the render-layer self-heal
	// (effectiveAgentState) promotes a stale "spawning"/"assigning" state to
	// "working" immediately — instead of waiting up to 60s for the controller's
	// next SyncAll poll to observe the pod as Running. This closes the
	// pre-first-tool window that the tool-event self-heal (ka-0000005799) and
	// the SyncAll-side fixes (ka-0000000427) could not cover. ka-0000003479.
	m.mu.Lock()
	s, ok := m.sessions[agentName]
	if ok && !s.stopped {
		s.live = true
		// Anchor boot→first-token TTFT at the first proof-of-life. SessionStart
		// fires once per boot, but keep the earliest instant defensively so a
		// re-delivered hook can never push the anchor later and shrink the
		// measured latency. ka-0000003911.
		if s.liveAt.IsZero() {
			s.liveAt = time.Now()
		}
	} else {
		// SessionStart beat session registration (prewarmed-pool assignment,
		// bridge restart, or the hook outrunning NotifyAgentSpawn). Buffer the
		// live signal so StartSession/StartCardSession can apply it when the
		// session is created — otherwise the proof-of-life is lost and the card
		// stays "starting" until the first tool event or the 60s SyncAll poll.
		// ka-0000002206.
		m.pendingLive[agentName] = time.Now()
	}
	// COLD-respawn latency close: if this agent's wake-up went through a cold
	// respawn (respawnThreadAgent stamped MarkColdRespawn), SessionStart is the
	// first proof-of-life — observe follow-up -> first-token and clear the
	// pending stamp so a later warm session never reuses it. ka-0000001565.
	coldStart, cold := m.coldRespawnStart[agentName]
	coldMeta := m.coldResumeMeta[agentName]
	if cold {
		delete(m.coldRespawnStart, agentName)
		delete(m.coldResumeMeta, agentName)
	}
	// Per-phase cold-respawn breakdown (task ka-0000001457): the stamp carries
	// the two bridge-observed boundaries (t0 = follow-up arrival, dispatchDone =
	// bead minted + tracker armed). SessionStart is first proof-of-life, so it
	// closes both intervals here. Read + clear under the same lock as the
	// cold-respawn close so a boot-time race cannot double-observe.
	phaseStamp, hasPhase := m.respawnPhase[agentName]
	if hasPhase {
		delete(m.respawnPhase, agentName)
	}
	resumeMetrics := m.resumeMetrics
	m.mu.Unlock()
	if hasPhase {
		// SessionStart IS the proof-of-life instant. Anchor all three intervals to
		// ONE `now` so dispatch + boot == total exactly (the built-in cross-check).
		now := time.Now()
		role := phaseStamp.role
		recordRespawnPhase(role, respawnPhaseDispatch, phaseStamp.dispatchDone.Sub(phaseStamp.t0).Seconds())
		recordRespawnPhase(role, respawnPhaseBoot, now.Sub(phaseStamp.dispatchDone).Seconds())
		recordRespawnPhase(role, respawnPhaseTotal, now.Sub(phaseStamp.t0).Seconds())
	}
	if cold {
		elapsed := time.Since(coldStart).Seconds()
		mentionColdRespawnSeconds.Observe(elapsed)
		// Post a user-visible "Resumed in Xs" confirmation to the originating
		// Slack thread. A pod-gone follow-up now RESUMES the agent silently
		// (instead of the old misleading "nudge failed … next cycle" reply), so
		// this is the honest replacement: it tells the operator the agent is back
		// and how long the resume took (ka-0000005084, ka-0000002949). Only for a
		// thread-anchored cold RESUME (coldMeta.armed + channel/threadTS set) —
		// prewarmed/frozen-thaw wakes and non-thread respawns leave it empty.
		if coldMeta.armed && coldMeta.channel != "" && coldMeta.threadTS != "" && m.threadPoster != nil {
			th := m.themeFor(ctx, coldMeta.channel)
			msg := fmt.Sprintf("%s Resumed in %s.", th.Semantic(SemResumed), formatResumeElapsed(elapsed))
			m.threadPoster.PostThreadMessage(ctx, coldMeta.channel, coldMeta.threadTS, msg)
		}
		// Tee the same t0->proof-of-life measurement into the shared
		// resume-latency histogram as a path=cold / regime=R0 resume — but only
		// when MarkColdResume armed it (coldMeta.armed). This is the common
		// non-runsc resume the controller's thaw-only call site never covers —
		// the reason gardenio_resume_latency_seconds emitted zero prod samples
		// (task ka-0000004119 / C7). A frozen-awaiting-thaw respawn is NOT armed
		// (the controller records its thaw sample instead), so the two emitters
		// stay disjoint. runtime defaults to runc (the class the non-runsc
		// cold-resume path actually runs under). A nil resumeMetrics (metric not
		// wired) no-ops.
		if resumeMetrics != nil && coldMeta.armed {
			runtime := coldMeta.runtime
			if runtime == "" {
				runtime = gardeniometrics.ResumeRuntimeRunc
			}
			resumeMetrics.RecordResumeLatency(elapsed, gardeniometrics.ResumeRegimeR0,
				coldMeta.role, runtime, gardeniometrics.ResumePathCold)
		}
	}
	// Refresh the card regardless of whether a session existed yet. When the
	// session is still pending, the card-state path reads the agent bead's
	// agent_state directly; the buffered pendingLive flag is applied the moment
	// the session is registered, so a refresh triggered by that registration
	// (or the next tool event) renders "working". Refreshing here also covers
	// the case where the session is created microseconds later. ka-0000002206.
	if m.cardRefresh != nil {
		m.cardRefresh.RefreshAgentCard(ctx, agentName)
	}
}

// formatResumeElapsed renders a resume duration for the "Resumed in Xs" thread
// post. Resumes are typically a few seconds, so it shows one decimal below a
// minute ("3.4s") and falls back to the coarser formatDuration above a minute
// (a resume that slow is an outlier worth reading as "1m 5s").
func formatResumeElapsed(seconds float64) string {
	if seconds < 60 {
		return fmt.Sprintf("%.1fs", seconds)
	}
	return formatDuration(time.Duration(seconds * float64(time.Second)))
}

// HandleSessionEnd sends a task_update chunk when a session ends.
func (m *AgentStreamManager) HandleSessionEnd(ctx context.Context, agentName, reason string) {
	title := "Session ended"
	if reason != "" {
		title = "Session ended: " + reason
	}
	m.sendTaskUpdate(ctx, agentName, "session:"+agentName, title, "complete", "")
}

// HandleCompactEvent sends a task_update chunk for PreCompact or PostCompact events.
func (m *AgentStreamManager) HandleCompactEvent(ctx context.Context, agentName, event, trigger string) {
	title := "Context compaction"
	if event == "PreCompact" {
		title = "Compacting context"
	} else if event == "PostCompact" {
		title = "Context compacted"
	}
	summary := ""
	if trigger != "" {
		summary = "trigger: " + trigger
	}
	m.sendTaskUpdate(ctx, agentName, "compact:"+agentName, title, "complete", summary)
}

// HandleTeammateIdle sends a task_update chunk when a teammate goes idle.
func (m *AgentStreamManager) HandleTeammateIdle(ctx context.Context, agentName, teammateID, teammateType string) {
	title := "Teammate idle"
	if teammateType != "" {
		title = "Teammate idle: " + teammateType
	}
	id := "teammate:" + teammateID
	if teammateID == "" {
		id = "teammate:" + agentName
	}
	m.sendTaskUpdate(ctx, agentName, id, title, "complete", "")
}

// HandleConfigChange sends a task_update chunk when configuration changes.
func (m *AgentStreamManager) HandleConfigChange(ctx context.Context, agentName, source, filePath string) {
	title := "Config changed"
	if source != "" {
		title = "Config: " + source
	}
	m.sendTaskUpdate(ctx, agentName, "config:"+agentName, title, "complete", truncateText(filePath, 200))
}

// HandleInstructionsLoaded sends a task_update chunk when instructions are loaded.
func (m *AgentStreamManager) HandleInstructionsLoaded(ctx context.Context, agentName, filePath, loadReason string) {
	title := "Instructions loaded"
	if loadReason != "" {
		title = "Instructions: " + loadReason
	}
	m.sendTaskUpdate(ctx, agentName, "instructions:"+agentName, title, "complete", truncateText(filePath, 200))
}

// HandleElicitation sends a task_update chunk when an MCP elicitation is shown.
func (m *AgentStreamManager) HandleElicitation(ctx context.Context, agentName, serverName, message string) {
	title := "Elicitation"
	if serverName != "" {
		title = "Elicitation: " + serverName
	}
	m.sendTaskUpdate(ctx, agentName, "elicitation:"+agentName, title, "in_progress", truncateText(message, 200))
}

// HandleElicitationResult sends a task_update chunk when an elicitation is resolved.
func (m *AgentStreamManager) HandleElicitationResult(ctx context.Context, agentName, serverName, action string) {
	title := "Elicitation result"
	if action != "" {
		title = "Elicitation: " + action
	}
	m.sendTaskUpdate(ctx, agentName, "elicitation:"+agentName, title, "complete", serverName)
}

// HandleWorktreeCreate sends a task_update chunk when a worktree is created.
func (m *AgentStreamManager) HandleWorktreeCreate(ctx context.Context, agentName, name string) {
	title := "Worktree created"
	m.sendTaskUpdate(ctx, agentName, "worktree:"+agentName, title, "complete", truncateText(name, 200))
}

// HandleWorktreeRemove sends a task_update chunk when a worktree is removed.
func (m *AgentStreamManager) HandleWorktreeRemove(ctx context.Context, agentName, worktreePath string) {
	title := "Worktree removed"
	m.sendTaskUpdate(ctx, agentName, "worktree:"+agentName, title, "complete", truncateText(worktreePath, 200))
}

// HandleSetup sends a task_update chunk when setup completes.
func (m *AgentStreamManager) HandleSetup(ctx context.Context, agentName, trigger string) {
	title := "Setup"
	if trigger != "" {
		title = "Setup: " + trigger
	}
	m.sendTaskUpdate(ctx, agentName, "setup:"+agentName, title, "complete", "")
}

// HandleMessageEvent processes a forwarded session log event from coop's NATS
// message forwarder. Dispatches assistant text, thinking, usage, and state
// change events to the Slack streaming API for live observability.
func (m *AgentStreamManager) HandleMessageEvent(agentName string, evt *messageEvent) {
	m.mu.Lock()
	_, hasSession := m.sessions[agentName]
	m.mu.Unlock()

	if !hasSession {
		slog.Debug("agentstream: message event dropped (no streaming session)", "agent", agentName, "event_type", evt.EventType)
		return
	}

	ctx := context.Background()

	switch evt.EventType {
	case "assistant_text":
		preview := truncateText(evt.Text, 200)
		if preview == "" {
			return
		}
		// On the FIRST assistant message of the session, prepend the
		// time-to-first-token: how long the user waited from sending their
		// Slack message to the agent's first emitted token. This is the
		// headline latency the warm-idle work optimises; surfacing it inline
		// makes it visible without a dashboard. ka-0000001004.
		title := "Generating response"
		if ttft := m.consumeTTFT(agentName); ttft != "" {
			title = "ttft: " + ttft
		}
		// Boot→first-token TTFT: the in-container latency from proof-of-life
		// (SessionStart) to this first emitted token, EXCLUDING spawn queue /
		// image-pull / network delay. Persisted as ttft_ms on the agent bead so
		// it is queryable via the API and rendered on the agent card. Distinct
		// from the consumeTTFT annotation above (Slack-send→first-token, message
		// title). The store is a bead write, so it runs off the NATS event path.
		// ka-0000003911.
		if m.ttftRecorder != nil {
			if ttftMs, ok := m.consumeBootTTFT(agentName); ok {
				go m.ttftRecorder.RecordTTFT(context.Background(), agentName, ttftMs)
			}
		}
		m.sendTaskUpdate(ctx, agentName, "text:"+agentName, title, "complete", preview)
	case "thinking":
		preview := truncateText(evt.Text, 200)
		title := "Thinking"
		if preview != "" {
			title = "Thinking..."
		}
		m.sendTaskUpdate(ctx, agentName, "thinking:"+agentName, title, "complete", preview)
	case "usage":
		summary := formatUsageSummary(evt.InputTokens, evt.OutputTokens, evt.CacheRead, evt.DurationMs)
		m.sendTaskUpdate(ctx, agentName, "usage:"+agentName, "Token usage", "complete", summary)
		// Flag a silent model switch: the usage event carries the model actually
		// SERVED this turn plus the model the pod EXPECTED (baked). On a real
		// divergence the flagger posts a Slack flag and writes a durable record.
		// A nil flagger (unwired) makes this a no-op. ka-0000003127.
		if m.modelFallback != nil {
			channelID, threadTS := m.sessionThread(agentName)
			m.modelFallback.Flag(ctx, evt, agentName, channelID, threadTS)
		}
	case "refusal":
		// coop classified this turn as an Anthropic stop_reason=refusal. Persist a
		// durable record on the surveys/ratings surface (source=refusal) and post a
		// Slack flag. A nil logger (unwired) makes this a no-op. ka-0000006122.
		if m.refusalLog != nil {
			channelID, threadTS := m.sessionThread(agentName)
			m.refusalLog.Log(ctx, evt, agentName, channelID, threadTS)
		}
	}
}

// sessionThread returns the Slack channel + thread anchor for an agent's active
// streaming session, so an out-of-band flag (e.g. a model-fallback warning) can be
// posted into the agent's thread. threadTS is the assistant thread ts (or the card
// ts for card-only sessions). Returns ("","") when the agent has no session.
func (m *AgentStreamManager) sessionThread(agentName string) (channelID, threadTS string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[agentName]
	if !ok {
		return "", ""
	}
	return s.channelID, s.threadTS
}

// consumeTTFT returns the formatted time-to-first-token for agentName's first
// assistant message and latches ttftShown so subsequent messages don't repeat
// it. Returns "" when the trigger send-time is unknown, when TTFT was already
// shown, or when the computed latency is non-positive (clock skew between the
// Slack-assigned ts and the bridge clock). The latch is set on the first call
// for a session regardless of whether a value is produced, so a missing
// trigger time never causes a later message to be mislabeled as the first.
func (m *AgentStreamManager) consumeTTFT(agentName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[agentName]
	if !ok || s.stopped || s.ttftShown {
		return ""
	}
	s.ttftShown = true
	if s.triggerSentAt.IsZero() {
		return ""
	}
	d := time.Since(s.triggerSentAt)
	if d <= 0 {
		return ""
	}
	return formatTTFT(d)
}

// consumeBootTTFT returns the boot→first-token latency in milliseconds for
// agentName's first assistant message and latches ttftMsShown so it is recorded
// at most once per session. The anchor is liveAt (the SessionStart proof-of-life
// instant), so the measurement is the agent's own first-inference latency and
// EXCLUDES the spawn queue / image-pull / network delay that precede boot — the
// metric this task asks for, distinct from the Slack-send→first-token annotation
// consumeTTFT computes (ka-0000003911). Returns (0, false) when the session is
// unknown/stopped, when SessionStart was never observed (liveAt zero), when the
// measurement was already taken, or when the delta is non-positive (clock skew).
// The latch is set on the first call regardless of whether a value is produced,
// so a missing liveAt never lets a later message be mis-recorded as the first.
func (m *AgentStreamManager) consumeBootTTFT(agentName string) (int64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[agentName]
	if !ok || s.stopped || s.ttftMsShown {
		return 0, false
	}
	s.ttftMsShown = true
	if s.liveAt.IsZero() {
		return 0, false
	}
	d := time.Since(s.liveAt)
	if d <= 0 {
		return 0, false
	}
	return d.Milliseconds(), true
}

// formatUsageSummary builds a compact token usage string for display.
func formatUsageSummary(input, output, cacheRead uint64, durationMs uint64) string {
	parts := []string{fmt.Sprintf("%dK in / %dK out", input/1000, output/1000)}
	if cacheRead > 0 {
		parts = append(parts, fmt.Sprintf("%dK cached", cacheRead/1000))
	}
	if durationMs > 0 {
		secs := float64(durationMs) / 1000.0
		parts = append(parts, fmt.Sprintf("%.1fs", secs))
	}
	return strings.Join(parts, " · ")
}

// appendChunk marshals a StreamChunk and sends it via AppendStreamChunksContext.
// Skips the API call if streamTS is empty (card-only session or failed stream init).
// If Slack returns "message_not_in_streaming_state", the session is marked stopped
// to prevent further append attempts on a finalized stream.
func (m *AgentStreamManager) appendChunk(ctx context.Context, channelID, streamTS string, chunk StreamChunk) {
	if streamTS == "" {
		return
	}
	chunks := []StreamChunk{chunk}
	data, err := json.Marshal(chunks)
	if err != nil {
		m.logger.Warn("agentstream: failed to marshal chunk", "error", err)
		return
	}
	if err := m.api.AppendStreamChunksContext(ctx, channelID, streamTS, string(data)); err != nil {
		if isStreamFinalized(err) {
			m.markStreamFinalized(channelID, streamTS)
			m.logger.Info("agentstream: stream finalized externally, stopped session",
				"channel", channelID, "stream_ts", streamTS)
			return
		}
		m.logger.Warn("agentstream: failed to append chunk",
			"channel", channelID, "stream_ts", streamTS, "chunk_id", chunk.ID, "error", err)
	}
}

// isStreamFinalized returns true if the error indicates the Slack message has
// already exited streaming state (e.g. stopped by another caller or timed out).
func isStreamFinalized(err error) bool {
	return err != nil && strings.Contains(err.Error(), "message_not_in_streaming_state")
}

// markStreamFinalized finds the session matching the given streamTS and marks
// it as stopped so no further append attempts are made.
func (m *AgentStreamManager) markStreamFinalized(channelID, streamTS string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.channelID == channelID && s.streamTS == streamTS && !s.stopped {
			s.stopped = true
			return
		}
	}
}

// HasSession returns true if the agent has an active (non-stopped) streaming session.
func (m *AgentStreamManager) HasSession(agentName string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[agentName]
	return ok && !s.stopped
}

// GetStreamTS returns the streaming message timestamp for an agent's active
// session. Returns "" if no active session exists. Used by callers that need
// to track the stream message (e.g., for threadSpawnMsgs cleanup).
func (m *AgentStreamManager) GetStreamTS(agentName string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[agentName]
	if ok && !s.stopped {
		return s.streamTS
	}
	return ""
}

// HasSessionForThread returns true if any active session is streaming in the
// given channel and thread. Used by the assistant handler to avoid clearing the
// thread status when a streaming session is actively updating it.
func (m *AgentStreamManager) HasSessionForThread(channelID, threadTS string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if !s.stopped && s.channelID == channelID && s.threadTS == threadTS {
			return true
		}
	}
	return false
}

// RestoreThreadStatus re-applies the thread status for an active session in the
// given channel and thread. Called after a PostMessage to an AI container thread,
// because Slack clears the assistant status whenever a bot message is posted.
func (m *AgentStreamManager) RestoreThreadStatus(ctx context.Context, channelID, threadTS string) {
	if m.threadStatus == nil {
		return
	}
	m.mu.Lock()
	var activity, lastTool, lastToolLabel string
	var totalTools int
	var toolCounts map[string]int
	var startedAt time.Time
	found := false
	for _, s := range m.sessions {
		if !s.stopped && s.channelID == channelID && s.threadTS == threadTS {
			activity = s.activity
			lastTool = s.lastTool
			lastToolLabel = s.lastToolLabel
			totalTools = s.totalTools
			toolCounts = make(map[string]int, len(s.toolCounts))
			for k, v := range s.toolCounts {
				toolCounts[k] = v
			}
			startedAt = s.startedAt
			found = true
			break
		}
	}
	m.mu.Unlock()

	if !found {
		return
	}

	th := m.themeFor(ctx, channelID)
	status := buildThreadStatusText(th, activity, lastTool, lastToolLabel, totalTools, toolCounts, startedAt)
	if status == "" {
		// Session exists but no tools yet — show a generic starting status.
		status = th.Glyph(LifecycleSpawning) + " Starting up..."
	}
	m.threadStatus.SetThreadStatus(ctx, channelID, threadTS, status)
}

// AgentToolStats holds a snapshot of an agent's tool use statistics from
// the active streaming session. Used by the agent card to display consistent
// status information matching the thread status bar.
type AgentToolStats struct {
	TotalTools    int
	LastTool      string
	LastToolLabel string // short label from tool input (e.g. "git status", "settings.json")
	Activity      string
	StartedAt     time.Time
	// Live reports that the agent's SessionStart hook has fired — Claude is
	// booted and alive in the container, even if it has not yet invoked a tool.
	// This is the earliest proof-of-life signal and lets the card self-heal a
	// stale "spawning" state during the pre-first-tool window. ka-0000003479.
	Live bool
}

// GetSessionStats returns the current tool use statistics for an agent's active
// streaming session. Returns nil if no active session exists. Thread-safe.
func (m *AgentStreamManager) GetSessionStats(agentName string) *AgentToolStats {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[agentName]
	if !ok || s.stopped {
		return nil
	}
	return &AgentToolStats{
		TotalTools:    s.totalTools,
		LastTool:      s.lastTool,
		LastToolLabel: s.lastToolLabel,
		Activity:      s.activity,
		StartedAt:     s.startedAt,
		Live:          s.live,
	}
}

// SessionCount returns the number of active streaming sessions.
func (m *AgentStreamManager) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, s := range m.sessions {
		if !s.stopped {
			count++
		}
	}
	return count
}

// --- Formatting ---

// toolActivity returns a human-readable activity string for a tool name.
func toolActivity(toolName string) string {
	switch toolName {
	case "Read":
		return "reading files"
	case "Grep":
		return "searching code"
	case "Glob":
		return "finding files"
	case "Edit":
		return "editing code"
	case "Write":
		return "writing files"
	case "Bash":
		return "running commands"
	case "WebFetch":
		return "fetching web content"
	case "WebSearch":
		return "searching the web"
	case "Agent":
		return "delegating to subagent"
	default:
		return "working"
	}
}

// buildStopSummary builds the final summary for a stopStream call, with the
// outcome, stats and per-tool glyphs resolved through th. Every glyph sits
// next to its verbal label (outcome word, "tool calls", tool name).
func buildStopSummary(th Theme, agentName, outcome string, totalTools int, toolCounts map[string]int, duration time.Duration) string {
	var b strings.Builder

	outcomeEmoji := th.Semantic(SemOK)
	switch outcome {
	case "failed":
		outcomeEmoji = th.Semantic(SemFail)
	case "stopped":
		outcomeEmoji = th.Semantic(SemStop)
	}

	fmt.Fprintf(&b, "\n%s Agent `%s` — %s", outcomeEmoji, agentName, outcome)
	fmt.Fprintf(&b, "\n%s %d tool calls · %s", th.Semantic(SemReport), totalTools, formatDuration(duration))

	if len(toolCounts) > 0 {
		type toolEntry struct {
			name  string
			count int
		}
		entries := make([]toolEntry, 0, len(toolCounts))
		for name, count := range toolCounts {
			entries = append(entries, toolEntry{name, count})
		}
		sort.Slice(entries, func(i, j int) bool {
			return entries[i].count > entries[j].count
		})

		b.WriteString("\n")
		parts := make([]string, 0, len(entries))
		for _, e := range entries {
			parts = append(parts, fmt.Sprintf("%s %s (%d)", th.Tool(e.name), e.name, e.count))
		}
		b.WriteString(strings.Join(parts, " · "))
	}

	return b.String()
}

// formatDuration is defined in bot_agents.go — reused here.
