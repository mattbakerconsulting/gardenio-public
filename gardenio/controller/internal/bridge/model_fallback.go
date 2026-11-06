package bridge

// model_fallback.go — flag silent model switches (ka-0000003127, epic
// ka-0000000949). Per the Opus 5 help article (support.claude.com/en/articles/
// 16049681) Claude can silently switch models mid-conversation — a refusal- or
// capacity-triggered fallback to a DIFFERENT served model than the one requested.
// coop publishes a per-turn `usage` NATS event carrying the SERVED model id
// (message.model) plus the EXPECTED model the controller baked into the pod
// (ANTHROPIC_MODEL/CLAUDE_MODEL, surfaced by coop as `expected_model`). This
// detector compares the two — after normalizing alias-equivalent forms so a mere
// `-oauth` group suffix or a dated snapshot never reads as a divergence — and on a
// real divergence (a) posts a Slack flag to the agent thread + an ops channel and
// (b) writes a durable `type=rating` record into the shared surveys/ratings
// surface (coordinated with the refusal-log task ka-0000006122 so both land in ONE
// surface, distinguished by the `source` field).

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"

	slackapi "github.com/slack-go/slack"
)

// modelFallbackSource is the `source` field value stamped on the durable
// type=rating record so the surveys/ratings surface can distinguish a model
// fallback from a human rating or a refusal-log row (ka-0000006122 uses its own
// source value on the same type=rating surface).
const modelFallbackSource = "model-fallback"

// modelFallbackKindLabel is the `kind:` label on the durable record, mirroring the
// `kind:slack-feedback` discriminator the always-on feedback beads carry. Lets the
// viewer / triggers scope model-fallback rows without parsing fields.
const modelFallbackKindLabel = "kind:model-fallback"

// modelFallbackWriter is the created_by / rater identity for records this
// detector writes.
const modelFallbackWriter = "slack-bridge"

// datedSnapshotSuffix matches a trailing Anthropic dated-snapshot suffix on a
// model id: "-YYYYMMDD" (e.g. claude-opus-4-8-20260315) or "-YYYY-MM-DD". Anchored
// to the end so a family version segment ("-4-8") is never mistaken for a date
// (those are 1–2 digit groups, not an 8-digit run).
var datedSnapshotSuffix = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)

// bedrockVersionSuffix matches a trailing Bedrock inference-profile version tag
// like "-v1:0" so a Bedrock-served id normalizes to the same identity as the
// requested routing-group name.
var bedrockVersionSuffix = regexp.MustCompile(`-v\d+:\d+$`)

// modelProviderPrefixes are leading provider/route prefixes stripped during
// normalization so the OAuth path and the Bedrock path compare on the bare model
// identity. Longest-first so "us.anthropic." is stripped before "anthropic.".
var modelProviderPrefixes = []string{
	"us.anthropic.",
	"eu.anthropic.",
	"apac.anthropic.",
	"anthropic/",
	"anthropic.",
	"bedrock/",
	"bedrock-",
}

// normalizeModelID reduces a model id to a canonical identity for equivalence
// comparison. It lowercases/trims, strips a known provider/route prefix, then
// repeatedly strips alias-equivalent suffixes (`-oauth`, a dated snapshot, a
// Bedrock version tag) until stable — so any order of those suffixes collapses.
// Examples:
//
//	claude-opus-5-oauth              -> claude-opus-5
//	claude-opus-4-8-20260315         -> claude-opus-4-8
//	us.anthropic.claude-opus-5       -> claude-opus-5
//	bedrock-claude-opus-5            -> claude-opus-5
//
// Returns "" for empty/whitespace input (the caller then applies no divergence
// check — fail-open).
func normalizeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return ""
	}
	for _, p := range modelProviderPrefixes {
		if rest, ok := strings.CutPrefix(m, p); ok {
			m = rest
			break
		}
	}
	// Strip trailing alias-equivalent suffixes until nothing more matches, so
	// "-oauth" + a dated snapshot in either order both collapse.
	for {
		before := m
		m = strings.TrimSuffix(m, "-oauth")
		m = datedSnapshotSuffix.ReplaceAllString(m, "")
		m = bedrockVersionSuffix.ReplaceAllString(m, "")
		if m == before {
			break
		}
	}
	return m
}

// modelGroup maps a model id to its Anthropic model GROUP (fable/opus/sonnet/
// haiku) — the coarse tier the fallback article's "silent switch" most visibly
// crosses. Token-based (substring match) so a new dated snapshot or alias flows
// through without a code change, mirroring the controller spread selector's
// modelToGroup. Returns "" when the group cannot be determined (e.g. a metered
// Bedrock/glm model with no Anthropic subscription group). Kept local to the
// bridge package — cmd/controller's modelToGroup lives in package main and is not
// importable here; the duplication is deliberate and small.
func modelGroup(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "":
		return ""
	case strings.Contains(m, "fable"):
		return "fable"
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "haiku"):
		return "haiku"
	default:
		return ""
	}
}

// modelsDiverge reports whether a SERVED model is a real divergence from the
// EXPECTED model. It is deliberately fail-OPEN: it returns false whenever it lacks
// a confident signal — either side empty/unnormalizable — so a missing
// expected_model (older coop, no baked model) never manufactures a phantom flag.
// A divergence is reported only when both sides normalize to a non-empty identity
// and those identities differ. This catches BOTH a cross-tier switch (opus ->
// sonnet) and a within-tier switch (opus-5 -> opus-4-8, the exact configmap
// fallback target), while treating alias-equivalent forms (opus-5 vs
// opus-5-oauth, dated snapshots) as a match.
func modelsDiverge(expected, served string) bool {
	ne := normalizeModelID(expected)
	ns := normalizeModelID(served)
	if ne == "" || ns == "" {
		return false
	}
	return ne != ns
}

// ModelFallbackDaemon is the minimal beads-daemon surface the flagger needs to
// write a durable record and link it to the agent bead. Satisfied by
// *beadsapi.Client; an interface keeps the flagger testable.
type ModelFallbackDaemon interface {
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
}

// ModelFallbackConfig configures the flagger.
type ModelFallbackConfig struct {
	// API posts the Slack flag. Nil disables Slack posting (the durable record
	// is still written when Daemon is set).
	API NoticeSlackAPI
	// Daemon writes the durable type=rating record. Nil disables the record
	// (the Slack flag is still posted when API is set).
	Daemon ModelFallbackDaemon
	// OpsChannel is an ops/notifications channel that receives every fallback
	// flag in addition to the agent thread. Empty posts only to the agent thread.
	OpsChannel string
	// BeadsViewerURL, when set, is prepended to bead-id deep links in the flag.
	BeadsViewerURL string
	// ThemeFor resolves each target channel's card theme. Nil renders Garden.
	ThemeFor ThemeResolver
	Logger   *slog.Logger
}

// ModelFallbackFlagger detects a served-vs-expected model divergence on a usage
// event and fans it out to Slack + a durable record. It de-duplicates by
// (session, expected->served) so the many usage turns of one session flag once,
// not per turn.
type ModelFallbackFlagger struct {
	api            NoticeSlackAPI
	daemon         ModelFallbackDaemon
	opsChannel     string
	beadsViewerURL string
	themeFor       ThemeResolver
	logger         *slog.Logger

	mu   sync.Mutex
	seen map[string]bool
}

// NewModelFallbackFlagger builds a flagger. Returns nil when neither a Slack API
// nor a daemon is configured (nothing to do). A nil flagger's Flag is a safe
// no-op.
func NewModelFallbackFlagger(cfg ModelFallbackConfig) *ModelFallbackFlagger {
	if cfg.API == nil && cfg.Daemon == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &ModelFallbackFlagger{
		api:            cfg.API,
		daemon:         cfg.Daemon,
		opsChannel:     cfg.OpsChannel,
		beadsViewerURL: strings.TrimRight(cfg.BeadsViewerURL, "/"),
		themeFor:       cfg.ThemeFor,
		logger:         logger,
		seen:           make(map[string]bool),
	}
}

// Flag inspects a usage event for a model divergence and, on a first-seen
// divergence for the session, posts a Slack flag (agent thread + ops channel) and
// writes a durable record. channelID/threadTS anchor the agent-thread flag (from
// the agent's streaming session); either may be empty (then only the ops channel,
// if set, receives the flag). A no-op on a nil receiver, a non-divergent turn, or
// a repeat of an already-flagged divergence.
func (f *ModelFallbackFlagger) Flag(ctx context.Context, evt *messageEvent, agentName, channelID, threadTS string) {
	if f == nil || evt == nil {
		return
	}
	if !modelsDiverge(evt.ExpectedModel, evt.Model) {
		return
	}
	if f.alreadyFlagged(agentName, evt.SessionID, evt.ExpectedModel, evt.Model) {
		return
	}

	// Downstream-observability time series (ka-0000002213): one increment per
	// de-duped divergence, so gardenio_model_fallback_total reconstructs the
	// durable model-fallback-bead count while making the silent-switch RATE
	// alertable in Grafana.
	recordModelFallback(evt.ExpectedModel, evt.Model)

	f.logger.Warn("model-fallback: served model diverges from expected",
		"agent", agentName,
		"expected", evt.ExpectedModel,
		"served", evt.Model,
		"expected_group", modelGroup(evt.ExpectedModel),
		"served_group", modelGroup(evt.Model),
		"session_id", evt.SessionID)

	recordID := f.writeRecord(ctx, evt, agentName)
	f.postFlag(ctx, evt, agentName, channelID, threadTS, recordID)
}

// alreadyFlagged latches a divergence key so it only fires once per session. The
// key includes the raw expected/served ids so a genuinely NEW divergence later in
// the same session (a second, different switch) still flags.
func (f *ModelFallbackFlagger) alreadyFlagged(agentName, sessionID, expected, served string) bool {
	key := agentName + "|" + sessionID + "|" + expected + "|" + served
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seen[key] {
		return true
	}
	f.seen[key] = true
	return false
}

// writeRecord persists the divergence as a durable type=rating bead on the shared
// surveys/ratings surface (source=model-fallback). Returns the new bead id, or ""
// on failure / when no daemon is configured. Best-effort: a write failure is
// logged and the Slack flag still fires.
func (f *ModelFallbackFlagger) writeRecord(ctx context.Context, evt *messageEvent, agentName string) string {
	if f.daemon == nil {
		return ""
	}
	ratedBeadID := evt.AgentBeadID
	if ratedBeadID == "" {
		ratedBeadID = agentName
	}
	answeredAt := time.Now().UTC().Format(time.RFC3339)

	labels := []string{modelFallbackKindLabel}
	if ratedBeadID != "" {
		labels = append(labels, "agent:"+ratedBeadID)
	}
	if evt.Project != "" {
		labels = append(labels, "project:"+evt.Project)
	}

	// score=0 so the surveys/ratings surface renders this as a non-star
	// informational row (ShowStars = score > 0), matching the feedback-only path.
	fields := map[string]any{
		"rated_bead_id":  ratedBeadID,
		"rater":          modelFallbackWriter,
		"source":         modelFallbackSource,
		"score":          0,
		"answered_at":    answeredAt,
		"expected_model": evt.ExpectedModel,
		"served_model":   evt.Model,
		"expected_group": modelGroup(evt.ExpectedModel),
		"served_group":   modelGroup(evt.Model),
	}
	if evt.SessionID != "" {
		fields["session_id"] = evt.SessionID
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		f.logger.Warn("model-fallback: failed to marshal record fields",
			"agent", agentName, "error", err)
		return ""
	}

	desc := fmt.Sprintf(
		"Silent model fallback detected for agent `%s`.\n\nExpected model: `%s` (group `%s`)\nServed model: `%s` (group `%s`)\n\nClaude served a different model than the one baked into this pod. Per the Opus 5 help article this can happen as a refusal- or capacity-triggered fallback. See epic ka-0000000949.",
		agentName, evt.ExpectedModel, modelGroup(evt.ExpectedModel), evt.Model, modelGroup(evt.Model),
	)

	req := beadsapi.CreateBeadRequest{
		Title:       "model-fallback: " + agentName,
		Type:        "rating",
		Description: desc,
		Labels:      labels,
		CreatedBy:   modelFallbackWriter,
		Fields:      fieldsJSON,
	}
	id, err := f.daemon.CreateBead(ctx, req)
	if err != nil {
		f.logger.Warn("model-fallback: failed to write durable record",
			"agent", agentName, "error", err)
		return ""
	}
	// Link the record to the agent bead (rates edge) so it hangs off the agent in
	// the graph, mirroring the human-rating / feedback path. Best-effort — a
	// missing agent bead must not lose the already-written record.
	if evt.AgentBeadID != "" {
		if err := f.daemon.AddDependency(ctx, id, evt.AgentBeadID, "rates", modelFallbackWriter); err != nil {
			f.logger.Debug("model-fallback: failed to link record to agent bead",
				"record", id, "agent_bead", evt.AgentBeadID, "error", err)
		}
	}
	f.logger.Info("model-fallback: wrote durable record",
		"record", id, "agent", agentName, "expected", evt.ExpectedModel, "served", evt.Model)
	return id
}

// postFlag posts the Slack flag to the agent thread (channelID/threadTS) and, when
// configured and distinct, to the ops channel. Best-effort per target.
func (f *ModelFallbackFlagger) postFlag(ctx context.Context, evt *messageEvent, agentName, channelID, threadTS, recordID string) {
	if f.api == nil {
		return
	}
	posted := false
	if channelID != "" {
		opts := []slackapi.MsgOption{
			slackapi.MsgOptionText(f.format(themeOf(ctx, f.themeFor, channelID), evt, agentName, recordID), false),
			slackapi.MsgOptionDisableLinkUnfurl(),
		}
		if threadTS != "" {
			opts = append(opts, slackapi.MsgOptionTS(threadTS))
		}
		if _, _, err := f.api.PostMessageContext(ctx, channelID, opts...); err != nil {
			f.logger.Warn("model-fallback: failed to post agent-thread flag",
				"agent", agentName, "channel", channelID, "error", err)
		} else {
			posted = true
		}
	}

	// Ops channel: skip when unset or when it is the same channel we just posted
	// to (avoid a duplicate in an agent whose card lives in the ops channel).
	if f.opsChannel != "" && f.opsChannel != channelID {
		if _, _, err := f.api.PostMessageContext(ctx, f.opsChannel,
			slackapi.MsgOptionText(f.format(themeOf(ctx, f.themeFor, f.opsChannel), evt, agentName, recordID), false),
			slackapi.MsgOptionDisableLinkUnfurl(),
		); err != nil {
			f.logger.Warn("model-fallback: failed to post ops-channel flag",
				"agent", agentName, "channel", f.opsChannel, "error", err)
		} else {
			posted = true
		}
	}
	if !posted {
		f.logger.Debug("model-fallback: no Slack target for flag", "agent", agentName)
	}
}

// format renders the Slack flag text.
func (f *ModelFallbackFlagger) format(th Theme, evt *messageEvent, agentName, recordID string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s — `%s` was served a different model than requested\n", semLabel(th, SemWarn, "*Model fallback*"), agentName)
	fmt.Fprintf(&sb, "• Expected: `%s`\n", evt.ExpectedModel)
	fmt.Fprintf(&sb, "• Served: `%s`\n", evt.Model)
	if eg, sg := modelGroup(evt.ExpectedModel), modelGroup(evt.Model); eg != "" || sg != "" {
		fmt.Fprintf(&sb, "• Group: `%s` → `%s`\n", eg, sg)
	}
	if recordID != "" {
		fmt.Fprintf(&sb, "• Record: %s", f.beadLink(recordID))
	}
	return strings.TrimRight(sb.String(), "\n")
}

// beadLink renders a bead id as a viewer deep link when a viewer URL is
// configured, else the bare id.
func (f *ModelFallbackFlagger) beadLink(id string) string {
	if f.beadsViewerURL == "" || id == "" {
		return id
	}
	return fmt.Sprintf("<%s/%s|%s>", f.beadsViewerURL, id, id)
}
