package main

// human_survey.go — Exit survey for humans reviewing an agent's work.
//
// On gb stop (after wrap-up persists), if configured, the agent posts a
// block-kit rating prompt ("Was this agent helpful? −1 / 0 / +1") to the
// agent's Slack thread via the bridge. The operator clicks a rating and
// the bridge creates a human_survey bead keyed on
// (agent_bead_id, rater_slack_user).
//
// Lands dark behind human-survey-config.enabled defaulting to false. See
// HumanSurveyConfig for tunables. Eligibility filters (session too short,
// role not in allowlist, already prompted) are applied locally; the bridge
// returns the message ts which the agent persists as human_survey_prompt_ts
// on the agent bead for dedup on re-runs.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// HumanSurveyConfigCategory is the config bead title used to resolve the
// human exit-survey config. Mirrors SurveyConfigCategory.
const HumanSurveyConfigCategory = "human-survey-config"

// humanSurveyPromptTSField is the agent-bead field name used for dedup.
const humanSurveyPromptTSField = "human_survey_prompt_ts"

// HumanSurveyConfig tunes the human-exit-survey flow. Loaded from config
// beads via ResolveConfigBeads (global < project < role < agent) and merged
// with DefaultHumanSurveyConfig.
type HumanSurveyConfig struct {
	// Enabled gates the whole flow. Defaults to false so this lands dark —
	// later PRs flip it on per-role.
	Enabled bool `json:"enabled"`

	// RatingScaleMax is the max integer rating (Min..Max). Defaults to +1.
	// The canonical scale is −1/0/+1: −1 ("worse than nothing") so
	// operators can flag net-harmful sessions, 0 neutral, +1 helpful.
	RatingScaleMax int `json:"rating_scale_max"`

	// MinRating is the minimum integer rating. Defaults to -1.
	MinRating int `json:"min_rating"`

	// SkipIfSessionSecondsLt skips the prompt for sessions shorter than
	// this many seconds. Defaults to 60 (1 minute) — too short to have
	// produced reviewable work.
	SkipIfSessionSecondsLt int `json:"skip_if_session_seconds_lt"`

	// AllowedRoles restricts the prompt to agents whose subscriptions include
	// one of these roles. Empty means "any role". Defaults to
	// ["gardener","crew"]. Matched against `role:<name>` subscriptions.
	//
	// (Field was previously named SkipRoles, which was misleading — it is an
	// allowlist, not a denylist.)
	AllowedRoles []string `json:"allowed_roles"`

	// Public controls whether the rating prompt is posted to the whole
	// thread (true) or ephemerally to a single operator (false). Defaults
	// to true.
	Public bool `json:"public"`

	// AskFeedback controls whether the block-kit prompt includes a free-text
	// feedback field. Defaults to true.
	AskFeedback bool `json:"ask_feedback"`

	// ExpireHours is how long after the prompt is posted clicks are still
	// accepted. Bridge uses this to reject late clicks. Defaults to 168
	// (7 days).
	ExpireHours int `json:"expire_hours"`
}

// DefaultHumanSurveyConfig is the baseline used when no config beads match.
// Lands dark: Enabled=false.
func DefaultHumanSurveyConfig() HumanSurveyConfig {
	return HumanSurveyConfig{
		Enabled:                false,
		RatingScaleMax:         1,
		MinRating:              -1,
		SkipIfSessionSecondsLt: 60,
		AllowedRoles:           []string{"gardener", "crew"},
		Public:                 true,
		AskFeedback:            true,
		ExpireHours:            168,
	}
}

// LoadHumanSurveyConfig resolves HumanSurveyConfig from config beads
// matching the given subscriptions. Falls back to DefaultHumanSurveyConfig
// if none match. Callers are expected to pass enriched subscriptions (see
// advice.EnrichAgentSubscriptions) so role/project labels from the agent
// bead are picked up — non-enriched subs only include what can be parsed
// from the agent ID, which for flat IDs like "thread-1776-849" is just
// global/agent/project:<id>, missing role:<real-role>.
func LoadHumanSurveyConfig(ctx context.Context, lister configBeadLister, subs []string) HumanSurveyConfig {
	def := DefaultHumanSurveyConfig()

	merged, count, _ := ResolveConfigBeads(ctx, lister, HumanSurveyConfigCategory, subs)
	if count == 0 || merged == nil {
		return def
	}

	data, err := json.Marshal(merged)
	if err != nil {
		return def
	}

	cfg := def
	if err := json.Unmarshal(data, &cfg); err != nil {
		return def
	}
	if cfg.RatingScaleMax == 0 {
		cfg.RatingScaleMax = def.RatingScaleMax
	}
	if cfg.ExpireHours == 0 {
		cfg.ExpireHours = def.ExpireHours
	}
	return cfg
}

// roleAllowed returns true when the allowlist is empty or at least one
// role: label in the given subscriptions is in the allowlist. Callers
// should pass enriched subscriptions (see advice.EnrichAgentSubscriptions)
// so the agent bead's role field is visible — parsing the agent ID alone
// does not surface role for flat IDs like "thread-1776-849".
func roleAllowed(subs []string, allowlist []string) bool {
	if len(allowlist) == 0 {
		return true
	}
	allow := make(map[string]bool, len(allowlist))
	for _, r := range allowlist {
		allow[strings.TrimSpace(r)] = true
	}
	for _, s := range subs {
		if role, ok := strings.CutPrefix(s, "role:"); ok && allow[role] {
			return true
		}
	}
	return false
}

// humanSurveyPromptRequest is the payload POSTed to the bridge's
// /api/slack/human-survey/prompt endpoint.
type humanSurveyPromptRequest struct {
	AgentBeadID    string `json:"agent_bead_id"`
	Channel        string `json:"channel"`
	ThreadTS       string `json:"thread_ts"`
	RatingScaleMax int    `json:"rating_scale_max"`
	MinRating      int    `json:"min_rating"`
	AskFeedback    bool   `json:"ask_feedback"`
}

// humanSurveyPromptResponse is returned by the bridge with the posted
// message's timestamp.
type humanSurveyPromptResponse struct {
	OK        bool   `json:"ok"`
	Timestamp string `json:"timestamp"`
}

// postHumanSurveyPrompt is best-effort: on any ineligibility or error it
// returns nil without disrupting the stop flow. Errors are returned only
// for diagnostic purposes and callers are expected to log-and-continue.
//
// Preconditions (all must hold):
//   - human-survey-config.enabled=true (else skipped silently)
//   - agent session duration >= SkipIfSessionSecondsLt
//   - agent role in AllowedRoles (when non-empty)
//   - agent bead has no human_survey_prompt_ts already set
//   - SLACK_BRIDGE_URL and thread coords resolvable
//
// On success, the bridge-returned ts is stored as human_survey_prompt_ts on
// the agent bead so subsequent gb stop invocations skip re-posting.
func postHumanSurveyPrompt(ctx context.Context, agentID string) error {
	if daemon == nil || agentID == "" {
		return nil
	}

	// Enrich so role/project labels from the agent bead are visible. Without
	// this, thread-spawned agents with flat IDs (e.g. thread-1776-849) never
	// match role-scoped or project-scoped human-survey-config beads, and the
	// prompt is skipped silently.
	subs := advice.BuildAgentSubscriptions(agentID, nil)
	subs = advice.EnrichAgentSubscriptions(ctx, daemon, agentID, subs)

	cfg := LoadHumanSurveyConfig(ctx, daemon, subs)
	if !cfg.Enabled {
		return nil
	}

	if cfg.SkipIfSessionSecondsLt > 0 {
		if d := agentSessionDuration(ctx, agentID); d < time.Duration(cfg.SkipIfSessionSecondsLt)*time.Second {
			return nil
		}
	}

	if !roleAllowed(subs, cfg.AllowedRoles) {
		return nil
	}

	bead, err := daemon.GetBead(ctx, agentID)
	if err != nil || bead == nil {
		return fmt.Errorf("fetching agent bead: %w", err)
	}
	if bead.Fields != nil && bead.Fields[humanSurveyPromptTSField] != "" {
		// Already prompted this session — skip.
		return nil
	}

	bridgeURL := os.Getenv("SLACK_BRIDGE_URL")
	if bridgeURL == "" {
		return nil
	}
	channel, threadTS := slackThreadCoordsFromBead(bead)
	// channel may be empty for JIRA-spawned (and other non-thread) agents
	// — see ka-0000005949, ka-0000002021. Pass it through as-is; the bridge
	// resolves the agent card location and posts the survey threaded under
	// the card so it lands in the same channel the operator already
	// associates with the agent (e.g. fsm-triage agents → fsm-triage
	// channel via channel_roles routing). If the bridge can't find a card
	// either, it returns 400 and we treat it as a soft-skip.
	ts, err := httpPostHumanSurveyPrompt(ctx, bridgeURL, humanSurveyPromptRequest{
		AgentBeadID:    agentID,
		Channel:        channel,
		ThreadTS:       threadTS,
		RatingScaleMax: cfg.RatingScaleMax,
		MinRating:      cfg.MinRating,
		AskFeedback:    cfg.AskFeedback,
	})
	if err != nil {
		if isSurveyChannelUnresolved(err) {
			// No thread coords AND no card — nothing to do. Don't pollute
			// stop output with a "skipped" notice for an expected case.
			return nil
		}
		return fmt.Errorf("posting human-survey prompt: %w", err)
	}

	if err := daemon.UpdateBeadFields(ctx, agentID, map[string]string{
		humanSurveyPromptTSField: ts,
	}); err != nil {
		return fmt.Errorf("persisting %s: %w", humanSurveyPromptTSField, err)
	}
	return nil
}

// slackThreadCoordsFromBead returns (channel, threadTS) sourced from env
// (thread-spawned agents) or falling back per-field to the bead's stored
// fields. It delegates to the shared threadCoordsFromBead core so the env→bead
// precedence matches gb squawk / gb prime exactly (bug ka-0000006843).
func slackThreadCoordsFromBead(bead *beadsapi.BeadDetail) (string, string) {
	return threadCoordsFromBead(bead)
}

// surveyChannelUnresolvedErr is returned by httpPostHumanSurveyPrompt when
// the bridge rejects the request because no channel could be resolved
// (the agent supplied none and the bridge has no card for the agent).
// Distinguished from other errors so the caller can soft-skip silently.
var surveyChannelUnresolvedErr = fmt.Errorf("survey channel unresolved")

func isSurveyChannelUnresolved(err error) bool {
	return err != nil && (err == surveyChannelUnresolvedErr ||
		strings.Contains(err.Error(), "channel is required"))
}

// httpPostHumanSurveyPrompt is a var so tests can stub it. Default impl
// POSTs to the bridge's /api/slack/human-survey/prompt endpoint.
var httpPostHumanSurveyPrompt = func(ctx context.Context, bridgeURL string, req humanSurveyPromptRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/human-survey/prompt"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := slackHTTPClient().Do(hreq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Embed the bridge's response body so the caller can classify the
		// error. In particular, the bridge returns 400 with
		// {"error":"channel is required (and no agent card found to
		// resolve)"} when it cannot resolve a channel/card for the agent —
		// an expected soft-skip case that isSurveyChannelUnresolved matches
		// on the "channel is required" substring. Without the body the bare
		// "bridge returned HTTP 400" string carries none of it, so the
		// soft-skip in postHumanSurveyPrompt is unreachable and every
		// channel-unresolved stop prints a spurious "skipped" notice
		// (ka-0000006174). Other 400s (e.g. "invalid request body") do not
		// carry the substring and so still surface as real errors.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if msg := strings.TrimSpace(string(body)); msg != "" {
			return "", fmt.Errorf("bridge returned HTTP %d: %s", resp.StatusCode, msg)
		}
		return "", fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}
	var out humanSurveyPromptResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if !out.OK || out.Timestamp == "" {
		return "", fmt.Errorf("bridge returned empty timestamp")
	}
	return out.Timestamp, nil
}
