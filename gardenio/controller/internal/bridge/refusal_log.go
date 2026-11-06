package bridge

// refusal_log.go — persist + flag model refusals (ka-0000006122, epic
// ka-0000000949). Matt: "we need to keep a log of all refusals; put it into the
// surveys section, since that's underexploited today." coop classifies a turn as
// a refusal when Anthropic's stop_reason == "refusal" and publishes a
// `coop.events.messages.<actor>.refusal` NATS event (ka-0000004364) carrying the
// served model, the session's requested model, and a truncated refusal snippet.
// The controller subscribes to `coop.events.messages.>` and dispatches events in
// AgentStreamManager.HandleMessageEvent; this logger is the `refusal` case.
//
// The durable record lands on the SAME shared surveys/ratings surface the
// model-fallback flagger writes to (type=rating, score=0 informational row),
// distinguished by `source=refusal`. The viewer's ratings surface already renders
// any type=rating row and supports a `?source=refusal` filter with no viewer
// change (cmd/beads-viewer/ratings.go) — so persisting here populates the
// underexploited surveys section directly. A Slack flag is also posted to the
// agent thread + an ops channel so a refusal is visible in real time.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"

	slackapi "github.com/slack-go/slack"
)

// refusalSource is the `source` field value stamped on the durable type=rating
// record so the surveys/ratings surface distinguishes a refusal from a human
// rating or a model-fallback row (which uses source=model-fallback). The viewer's
// `?source=refusal` filter scopes the refusal log.
const refusalSource = "refusal"

// refusalKindLabel is the `kind:` label on the durable record, mirroring the
// model-fallback path's `kind:model-fallback`. Lets the viewer / triggers scope
// refusal rows by label without parsing fields.
const refusalKindLabel = "kind:refusal"

// refusalWriter is the created_by / rater identity for records this logger writes.
const refusalWriter = "slack-bridge"

// RefusalLogDaemon is the minimal beads-daemon surface the logger needs to write a
// durable record and link it to the agent bead. Satisfied by *beadsapi.Client; an
// interface keeps the logger testable. Mirrors ModelFallbackDaemon.
type RefusalLogDaemon interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
}

// RefusalLogConfig configures the logger.
type RefusalLogConfig struct {
	// API posts the Slack flag. Nil disables Slack posting (the durable record is
	// still written when Daemon is set).
	API NoticeSlackAPI
	// Daemon writes the durable type=rating record. Nil disables the record (the
	// Slack flag is still posted when API is set).
	Daemon RefusalLogDaemon
	// OpsChannel is an ops/notifications channel that receives every refusal flag
	// in addition to the agent thread. Empty posts only to the agent thread.
	OpsChannel string
	// BeadsViewerURL, when set, is prepended to bead-id deep links in the flag.
	BeadsViewerURL string
	// ThemeFor resolves each target channel's card theme. Nil renders Garden.
	ThemeFor ThemeResolver
	Logger   *slog.Logger
}

// RefusalLogger consumes a coop `refusal` event and fans it out to a durable
// surveys-surface record + a Slack flag. It de-duplicates per (agent, session,
// snippet) so a refusal that surfaces across repeated turns of one session logs
// once, not per turn.
type RefusalLogger struct {
	api            NoticeSlackAPI
	daemon         RefusalLogDaemon
	opsChannel     string
	beadsViewerURL string
	themeFor       ThemeResolver
	logger         *slog.Logger

	mu   sync.Mutex
	seen map[string]bool
}

// NewRefusalLogger builds a logger. Returns nil when neither a Slack API nor a
// daemon is configured (nothing to do). A nil logger's Log is a safe no-op.
func NewRefusalLogger(cfg RefusalLogConfig) *RefusalLogger {
	if cfg.API == nil && cfg.Daemon == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &RefusalLogger{
		api:            cfg.API,
		daemon:         cfg.Daemon,
		opsChannel:     cfg.OpsChannel,
		beadsViewerURL: strings.TrimRight(cfg.BeadsViewerURL, "/"),
		themeFor:       cfg.ThemeFor,
		logger:         logger,
		seen:           make(map[string]bool),
	}
}

// Log records a refusal event: on a first-seen refusal for the (agent, session,
// snippet) it writes a durable record and posts a Slack flag. channelID/threadTS
// anchor the agent-thread flag (from the agent's streaming session); either may be
// empty (then only the ops channel, if set, receives the flag). A no-op on a nil
// receiver or a nil/non-refusal event.
func (f *RefusalLogger) Log(ctx context.Context, evt *messageEvent, agentName, channelID, threadTS string) {
	if f == nil || evt == nil {
		return
	}
	// Defensive: only act on a genuine refusal classification. coop only emits the
	// refusal event on stop_reason=="refusal", but guard anyway so a mis-routed
	// event never manufactures a record.
	if !strings.EqualFold(strings.TrimSpace(evt.StopReason), refusalSource) {
		return
	}
	if f.alreadyLogged(agentName, evt.SessionID, evt.Text) {
		return
	}

	served := strings.TrimSpace(evt.Model)
	requested := strings.TrimSpace(evt.RequestedModel)
	fallback := served != "" && requested != "" && modelsDiverge(requested, served)

	// Downstream-observability time series (ka-0000002213): one increment per
	// de-duped refusal, so gardenio_model_refusal_total reconstructs the durable
	// refusal-bead count while making the RATE alertable in Grafana — the query
	// the point-in-time bead + Slack flag cannot answer.
	recordModelRefusal(served, fallback)

	f.logger.Warn("refusal-log: model refused a turn",
		"agent", agentName,
		"served_model", served,
		"requested_model", requested,
		"served_by_fallback", fallback,
		"session_id", evt.SessionID)

	recordID := f.writeRecord(ctx, evt, agentName, served, requested, fallback)
	f.postFlag(ctx, evt, agentName, channelID, threadTS, recordID, served, requested, fallback)
}

// alreadyLogged latches a refusal key so it only fires once per (agent, session,
// snippet). Including the snippet lets a genuinely different refusal later in the
// same session still log, while a refusal re-surfaced across repeated turns of the
// same conversation logs once.
func (f *RefusalLogger) alreadyLogged(agentName, sessionID, snippet string) bool {
	key := agentName + "|" + sessionID + "|" + snippet
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen[key] {
		return true
	}
	f.seen[key] = true
	return false
}

// writeRecord persists the refusal as a durable type=rating bead on the shared
// surveys/ratings surface (source=refusal, score=0 informational). Returns the new
// bead id, or "" on failure / when no daemon is configured. Best-effort: a write
// failure is logged and the Slack flag still fires.
func (f *RefusalLogger) writeRecord(ctx context.Context, evt *messageEvent, agentName, served, requested string, fallback bool) string {
	if f.daemon == nil {
		return ""
	}
	ratedBeadID := evt.AgentBeadID
	if ratedBeadID == "" {
		ratedBeadID = agentName
	}

	labels := []string{refusalKindLabel}
	if ratedBeadID != "" {
		labels = append(labels, "agent:"+ratedBeadID)
	}
	if evt.Project != "" {
		labels = append(labels, "project:"+evt.Project)
	}

	// score=0 so the surveys/ratings surface renders this as a non-star
	// informational row (ShowStars = score > 0), matching the feedback-only /
	// model-fallback paths.
	fields := map[string]any{
		"rated_bead_id": ratedBeadID,
		"rater":         refusalWriter,
		"source":        refusalSource,
		"score":         0,
		"stop_reason":   "refusal",
	}
	if served != "" {
		fields["served_model"] = served
	}
	if requested != "" {
		fields["requested_model"] = requested
	}
	if fallback {
		fields["served_by_fallback"] = true
	}
	if evt.SessionID != "" {
		fields["session_id"] = evt.SessionID
	}
	// The refusal snippet coop forwards (already truncated to text_budget). Kept
	// in a field for structured querying; also folded into the description below.
	snippet := strings.TrimSpace(evt.Text)
	if snippet != "" {
		fields["feedback"] = snippet
	}
	if evt.FullLength > 0 {
		fields["full_length"] = evt.FullLength
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		f.logger.Warn("refusal-log: failed to marshal record fields",
			"agent", agentName, "error", err)
		return ""
	}

	var desc strings.Builder
	fmt.Fprintf(&desc, "Model refusal recorded for agent `%s` (Anthropic stop_reason=refusal).\n\n", agentName)
	if served != "" {
		fmt.Fprintf(&desc, "Served model: `%s`\n", served)
	}
	if requested != "" {
		fmt.Fprintf(&desc, "Requested model: `%s`\n", requested)
	}
	if fallback {
		desc.WriteString("This refusal was served by a SILENT FALLBACK model (served != requested) — see the Opus 5 help article (support.claude.com/en/articles/16049681).\n")
	}
	if snippet != "" {
		fmt.Fprintf(&desc, "\nRefusal message:\n> %s\n", snippet)
	}
	desc.WriteString("\nSee epic ka-0000000949. Browse all refusals in the surveys/ratings surface (filter source=refusal).")

	req := beadsapi.CreateBeadRequest{
		Title:       "refusal: " + agentName,
		Type:        "rating",
		Description: desc.String(),
		Labels:      labels,
		CreatedBy:   refusalWriter,
		Fields:      fieldsJSON,
	}
	id, err := f.daemon.CreateBead(ctx, req)
	if err != nil {
		f.logger.Warn("refusal-log: failed to write durable record",
			"agent", agentName, "error", err)
		return ""
	}
	// Link the record to the agent bead (rates edge) so it hangs off the agent in
	// the graph, mirroring the human-rating / model-fallback path. Best-effort — a
	// missing agent bead must not lose the already-written record.
	if evt.AgentBeadID != "" {
		if err := f.daemon.AddDependency(ctx, id, evt.AgentBeadID, "rates", refusalWriter); err != nil {
			f.logger.Debug("refusal-log: failed to link record to agent bead",
				"record", id, "agent_bead", evt.AgentBeadID, "error", err)
		}
	}
	f.logger.Info("refusal-log: wrote durable record",
		"record", id, "agent", agentName, "served", served, "requested", requested)
	return id
}

// postFlag posts the Slack flag to the agent thread (channelID/threadTS) and, when
// configured and distinct, to the ops channel. Best-effort per target.
func (f *RefusalLogger) postFlag(ctx context.Context, evt *messageEvent, agentName, channelID, threadTS, recordID, served, requested string, fallback bool) {
	if f.api == nil {
		return
	}
	posted := false
	if channelID != "" {
		opts := []slackapi.MsgOption{
			slackapi.MsgOptionText(f.format(themeOf(ctx, f.themeFor, channelID), evt, agentName, recordID, served, requested, fallback), false),
			slackapi.MsgOptionDisableLinkUnfurl(),
		}
		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}
		if _, _, err := f.api.PostMessageContext(ctx, channelID, opts...); err != nil {
			f.logger.Warn("refusal-log: failed to post agent-thread flag",
				"agent", agentName, "channel", channelID, "error", err)
		} else {
			posted = true
		}
	}

	// Ops channel: skip when unset or when it is the same channel we just posted to
	// (avoid a duplicate in an agent whose card lives in the ops channel).
	if f.opsChannel != "" && f.opsChannel != channelID {
		if _, _, err := f.api.PostMessageContext(ctx, f.opsChannel,
			slackapi.MsgOptionText(f.format(themeOf(ctx, f.themeFor, f.opsChannel), evt, agentName, recordID, served, requested, fallback), false),
			slackapi.MsgOptionDisableLinkUnfurl(),
		); err != nil {
			f.logger.Warn("refusal-log: failed to post ops-channel flag",
				"agent", agentName, "channel", f.opsChannel, "error", err)
		} else {
			posted = true
		}
	}
	if !posted {
		f.logger.Debug("refusal-log: no Slack target for flag", "agent", agentName)
	}
}

// format renders the Slack flag text.
func (f *RefusalLogger) format(th Theme, evt *messageEvent, agentName, recordID, served, requested string, fallback bool) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — `%s` was refused a turn (stop_reason=refusal)\n", semLabel(th, SemDenied, "*Model refusal*"), agentName)
	if served != "" {
		fmt.Fprintf(&sb, "• Served: `%s`\n", served)
	}
	if requested != "" && requested != served {
		fmt.Fprintf(&sb, "• Requested: `%s`\n", requested)
	}
	if fallback {
		sb.WriteString("• " + semLabel(th, SemWarn, "served by a *silent fallback* (served != requested)") + "\n")
	}
	if snippet := strings.TrimSpace(evt.Text); snippet != "" {
		fmt.Fprintf(&sb, "• Message: %s\n", truncateText(snippet, 240))
	}
	if recordID != "" {
		fmt.Fprintf(&sb, "• Logged: %s", f.beadLink(recordID))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// beadLink renders a bead id as a viewer deep link when a viewer URL is
// configured, else the bare id.
func (f *RefusalLogger) beadLink(id string) string {
	if f.beadsViewerURL == "" || id == "" {
		return id
	}
	return fmt.Sprintf("<%s/%s|%s>", f.beadsViewerURL, id, id)
}
