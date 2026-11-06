package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

var slackCmd = &cobra.Command{
	Use:     "slack",
	Short:   "Slack commands for reading channels and threads",
	Long:    `Read Slack channels and threads, and post replies. Requires SLACK_BRIDGE_URL. Thread commands also require SLACK_THREAD_CHANNEL and SLACK_THREAD_TS.`,
	GroupID: "orchestration",
}

// ── slack thread ────────────────────────────────────────────────────

var slackThreadCmd = &cobra.Command{
	Use:   "thread",
	Short: "Fetch the thread this agent was spawned from (alias of gb thread)",
	Long: `A permanent alias of 'gb thread' with no argument: reads this agent's
home thread (SLACK_THREAD_CHANNEL / SLACK_THREAD_TS, else the agent bead's
slack_thread_channel / slack_thread_ts, else its thread_id) and outputs it as
markdown suitable for context injection.

Bot messages (squawks, replies, decisions) are included by default so the
agent can see the full conversation. Use --include-bots=false to hide them.

Bridge auto-posts (squawk scaffolding, bead-lifecycle mirrors, Block-Kit-only
interactive cards, post-stop survey rate cards) are dropped by default to
preserve the human-relevant signal — see ka-0000002440. Conversational bot
replies (e.g. gb slack reply posts) are kept. Override with --exclude-subtypes="".`,
	Args: cobra.NoArgs,
	RunE: runSlackThread,
}

// runSlackThread is gb thread for the home thread.
func runSlackThread(cmd *cobra.Command, _ []string) error {
	return runThread(cmd, nil)
}

// ── slack reply ─────────────────────────────────────────────────────

var slackReplyCmd = &cobra.Command{
	Use:   "reply <message>",
	Short: "Post a reply to the thread this agent was spawned from (alias of gb say)",
	Long: `A permanent alias of 'gb say' into this agent's home thread: posts the
message, as written, as a reply in the originating thread. Reads
SLACK_THREAD_CHANNEL and SLACK_THREAD_TS from the environment (else the agent
bead's slack_thread_channel / slack_thread_ts).

The bridge records the reply as a type=message bead in the thread (with
triggered-by lineage to the agent bead). Against a bridge that predates
gb say, the reply goes through the old /api/slack/threads/reply route and gb
records a best-effort bead (label slack-reply) itself.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackReply,
}

func runSlackReply(cmd *cobra.Command, args []string) error {
	bridgeURL, channel, threadTS, err := resolveSlackEnv()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}

	req := newSayParams(args[0])
	req.Channel, req.ThreadTS = channel, threadTS
	said, err := conversationSay(ctx, bridgeURL, req, nil)
	if errors.Is(err, errBridgeWithoutConversation) {
		return runSlackReplyLegacy(cmd, bridgeURL, channel, threadTS, args[0])
	}
	if err != nil {
		return err
	}
	if jsonOutput {
		printJSON(slackReplyResult{OK: said.OK, Timestamp: said.Timestamp})
	} else {
		fmt.Println("Reply posted.")
	}
	return nil
}

// runSlackReplyLegacy posts the reply through /api/slack/threads/reply, for a
// bridge with no /api/conversation/say, and records the reply's bead itself.
func runSlackReplyLegacy(cmd *cobra.Command, bridgeURL, channel, threadTS, text string) error {
	payload := map[string]string{
		"channel":   channel,
		"thread_ts": threadTS,
		"text":      text,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request: %w", err)
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/threads/reply"
	resp, err := slackHTTPClient().Post(u, "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("posting reply: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var result slackReplyResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	// Best-effort: record the reply as a type=message bead so it is reachable
	// from the agent / slack-thread graph in beads-viewer. Mirrors the
	// gb squawk message-bead shape (see ka-0000000087). Bead failures never
	// fail the reply itself — the Slack post has already succeeded.
	recordSlackReplyBead(cmd.Context(), text, channel, threadTS)

	if jsonOutput {
		printJSON(result)
	} else {
		fmt.Println("Reply posted.")
	}
	return nil
}

// recordSlackReplyBead creates a type=message bead recording an outbound
// `gb slack reply` post and stamps the same triggered-by lineage that
// gb squawk uses (agent bead, trigger bead, or human fallback parent).
//
// The bead carries label `slack-reply` (distinct from squawk's `squawk`
// label) plus `from:<actor>`. Failures are reported on stderr but never
// fail the caller — the Slack message has already been posted.
//
// Skips silently when the beads daemon is unreachable (local/dev runs of
// gb slack reply without a daemon configured).
func recordSlackReplyBead(ctx context.Context, text, channel, threadTS string) {
	if daemon == nil {
		return
	}

	title := text
	if len(title) > 500 {
		title = title[:497] + "..."
	}

	fields := map[string]string{
		"source_agent":         actor,
		"text":                 text,
		"slack_thread_channel": channel,
		"slack_thread_ts":      threadTS,
	}
	// Bind the reply to its thread (wish) head via the origin-agnostic
	// wish-pointer field thread_id, so the beads-viewer /threads timeline
	// renders it as a conversation member. The viewer resolves membership as
	// UNION(fields.thread_id == headID, parent-child closure)
	// (cmd/beads-viewer/threads.go); a triggered-by lineage dep is NOT a
	// membership edge, so without thread_id the reply is invisible in the
	// thread view (ka-0000003279). Resolve the head via the canonical
	// slack-thread:{channel}:{ts} label — best-effort, never fail the reply.
	if headID := lookupSlackThreadBead(ctx); headID != "" {
		fields["thread_id"] = headID
	}

	// createMessageBead also stamps the Commons channel_id when the channel is
	// bound to a channel bead (channel_stamp.go).
	beadID, err := createMessageBead(ctx, beadsapi.CreateBeadRequest{
		Title:     title,
		Type:      "message",
		Kind:      "data",
		Labels:    []string{"slack-reply", fmt.Sprintf("from:%s", actor)},
		CreatedBy: actor,
	}, fields, channel)
	if err != nil {
		fmt.Fprintf(os.Stderr, "slack reply: warn: failed to create message bead: %v\n", err)
		return
	}

	writeSlackReplyLineage(ctx, beadID)

	if err := daemon.CloseBead(ctx, beadID, nil); err != nil {
		fmt.Fprintf(os.Stderr, "slack reply: warn: failed to close message bead %s: %v\n", beadID, err)
	}
}

// writeSlackReplyLineage mirrors writeSquawkLineage in squawk.go — stamps
// `triggered-by` deps onto the agent bead (BOAT_AGENT_BEAD_ID / KD_AGENT_ID)
// and trigger bead (BOAT_TRIGGER_BEAD_ID). Falls back to the slack-thread
// or operator alias bead when no agent context is present.
func writeSlackReplyLineage(ctx context.Context, beadID string) {
	writeReplyLineage(ctx, beadID, "slack reply")
}

// writeReplyLineage is the shared lineage stamper behind slack and jira reply
// message beads. warnPrefix labels stderr warnings with the calling command.
func writeReplyLineage(ctx context.Context, beadID, warnPrefix string) {
	const depType = "triggered-by"

	agentBeadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if agentBeadID == "" {
		agentBeadID = os.Getenv("KD_AGENT_ID")
	}
	triggerBeadID := os.Getenv("BOAT_TRIGGER_BEAD_ID")

	if agentBeadID != "" {
		if err := daemon.AddDependency(ctx, beadID, agentBeadID, depType, actor); err != nil {
			fmt.Fprintf(os.Stderr, "%s: warn: failed to add triggered-by dep to agent bead %s: %v\n", warnPrefix, agentBeadID, err)
		}
	}
	if triggerBeadID != "" && triggerBeadID != agentBeadID {
		if err := daemon.AddDependency(ctx, beadID, triggerBeadID, depType, actor); err != nil {
			fmt.Fprintf(os.Stderr, "%s: warn: failed to add triggered-by dep to trigger bead %s: %v\n", warnPrefix, triggerBeadID, err)
		}
	}

	if agentBeadID == "" && triggerBeadID == "" {
		if parentID := resolveHumanSquawkParent(ctx); parentID != "" {
			if err := daemon.AddDependency(ctx, beadID, parentID, depType, actor); err != nil {
				fmt.Fprintf(os.Stderr, "%s: warn: failed to add triggered-by dep to %s: %v\n", warnPrefix, parentID, err)
			}
		}
	}
}

// ── types ───────────────────────────────────────────────────────────

type slackThreadMessage struct {
	Author    string          `json:"author"`
	Text      string          `json:"text"`
	Timestamp string          `json:"timestamp"`
	IsBot     bool            `json:"is_bot"`
	Subtype   string          `json:"subtype,omitempty"`
	Files     []slackFileInfo `json:"files,omitempty"`
}

// slackFileInfo mirrors bridge.SlackFileInfo (id, name, mimetype, size, proxy_url).
type slackFileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int    `json:"size"`
	ProxyURL string `json:"proxy_url"`
}

type slackReplyResult struct {
	OK        bool   `json:"ok"`
	Timestamp string `json:"timestamp"`
}

// defaultExcludeSubtypes is the set of bot-message subtypes dropped by default
// from the human-readable thread/channel/search output. These are bridge auto-
// posts (squawks, bead-lifecycle mirrors, Block-Kit-only interactive cards,
// post-stop survey rate cards) that bury the human signal at roughly 9:1 — see
// ka-0000002440. Conversational bot replies (e.g. `gb slack reply` posts) are
// untagged and pass through.
const defaultExcludeSubtypes = "squawk,bead-mirror,interactive,survey"

// parseExcludeSubtypes turns a comma-separated subtype list into a lookup set.
// "" disables filtering; "all" / "*" / "noise" expand to defaultExcludeSubtypes.
func parseExcludeSubtypes(spec string) map[string]bool {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	if spec == "all" || spec == "*" || spec == "noise" {
		spec = defaultExcludeSubtypes
	}
	set := make(map[string]bool)
	for _, p := range strings.Split(spec, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			set[p] = true
		}
	}
	return set
}

// ── helpers ─────────────────────────────────────────────────────────

// resolveSlackEnv reads and validates the required Slack env vars.
// Falls back to fetching thread coordinates from the agent bead when
// SLACK_THREAD_CHANNEL/SLACK_THREAD_TS are not set (non-thread-bound agents).
func resolveSlackEnv() (bridgeURL, channel, threadTS string, err error) {
	bridgeURL = os.Getenv("SLACK_BRIDGE_URL")
	if bridgeURL == "" {
		return "", "", "", fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}
	channel = os.Getenv("SLACK_THREAD_CHANNEL")
	threadTS = os.Getenv("SLACK_THREAD_TS")
	if channel != "" && threadTS != "" {
		return bridgeURL, channel, threadTS, nil
	}

	// Fallback: look up thread coordinates from the agent bead.
	// The bridge persists slack_thread_channel/slack_thread_ts on the bead
	// after posting the agent card, so even non-thread-bound agents can
	// discover their Slack thread.
	beadID := os.Getenv("BOAT_AGENT_BEAD_ID")
	if beadID == "" || daemon == nil {
		return "", "", "", fmt.Errorf("no thread context: SLACK_THREAD_CHANNEL and SLACK_THREAD_TS are not set and BOAT_AGENT_BEAD_ID is unavailable")
	}

	bead, err := daemon.GetBead(context.Background(), beadID)
	if err != nil {
		return "", "", "", fmt.Errorf("no thread context: failed to look up agent bead %s: %w", beadID, err)
	}

	channel = bead.Fields["slack_thread_channel"]
	threadTS = bead.Fields["slack_thread_ts"]
	if channel == "" || threadTS == "" {
		return "", "", "", fmt.Errorf("no thread context available — agent bead %s has no slack_thread_channel/slack_thread_ts (thread may not be created yet)", beadID)
	}

	// Cache in env for subsequent calls within this process.
	os.Setenv("SLACK_THREAD_CHANNEL", channel)
	os.Setenv("SLACK_THREAD_TS", threadTS)

	return bridgeURL, channel, threadTS, nil
}

// isThreadSpawnedAgent returns true when both SLACK_THREAD_CHANNEL and
// SLACK_THREAD_TS are set, indicating this agent was spawned from a Slack thread.
func isThreadSpawnedAgent() bool {
	return os.Getenv("SLACK_THREAD_CHANNEL") != "" && os.Getenv("SLACK_THREAD_TS") != ""
}

// fetchThreadMessages fetches thread messages from the bridge HTTP API.
// Returns nil on any error (non-fatal for callers like prime).
func fetchThreadMessages(bridgeURL, channel, threadTS string, limit int) []slackThreadMessage {
	u := fmt.Sprintf("%s/api/slack/threads?channel=%s&ts=%s",
		strings.TrimRight(bridgeURL, "/"),
		url.QueryEscape(channel),
		url.QueryEscape(threadTS))
	if limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}
	ctx := context.Background()
	u += legacyThreadAgentParam(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil
	}
	setBridgeAgentToken(ctx, req)
	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var msgs []slackThreadMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil
	}
	return msgs
}

// legacyThreadAgentParam names the reading agent on a GET /api/slack/threads
// read ("&agent=<agent>", "" when gb has none), so the bridge walls the read
// by the agent's land and thread rather than by the thread's land alone. The
// agent is its token's when gb has one (bridgeAgent).
func legacyThreadAgentParam(ctx context.Context) string {
	agent := bridgeAgent(ctx)
	if agent == "" {
		return ""
	}
	return "&agent=" + url.QueryEscape(agent)
}

// formatThreadMarkdown formats thread messages as markdown, returning the string.
// maxChars controls truncation (default 4000 if <= 0).
// When includeBots is false, bot messages are omitted entirely.
//
// Attachments (files) on a message are surfaced inline as a one-line `[attachment: ...]`
// hint per file, and a single trailing tip points the agent at `gb slack files
// fetch-all` so screenshots/PDFs can be read with the Read tool.
//
// Truncation strategy when the rendered thread exceeds maxChars: always preserve
// the most-recent message (it's what the agent must respond to), include the
// originating message as head when budget allows, and elide the middle with
// `_(N messages elided)_`. Walking oldest→newest and stopping at the cap (the
// previous behavior) silently dropped the latest replies — see ka-0000004284.
func formatThreadMarkdown(msgs []slackThreadMessage, maxChars int, includeBots bool, excludeSubtypes map[string]bool) string {
	if maxChars <= 0 {
		maxChars = 4000
	}

	if len(msgs) == 0 {
		return "(No thread messages)\n"
	}

	visible := make([]slackThreadMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.IsBot && !includeBots {
			continue
		}
		if m.IsBot && m.Subtype != "" && excludeSubtypes[m.Subtype] {
			continue
		}
		visible = append(visible, m)
	}
	if len(visible) == 0 {
		return "(No thread messages)\n"
	}

	rendered := make([]string, len(visible))
	hasFiles := false
	for i, m := range visible {
		var sb strings.Builder
		prefix := ""
		if m.IsBot {
			prefix = "[bot] "
		}
		sb.WriteString(fmt.Sprintf("%s**%s**: %s\n", prefix, m.Author, m.Text))
		for _, f := range m.Files {
			hasFiles = true
			sb.WriteString(fmt.Sprintf("  [attachment: %s (%s, %s) — file_id=%s]\n", f.Name, f.Mimetype, formatSize(int64(f.Size)), f.ID))
		}
		rendered[i] = sb.String()
	}

	tipLine := ""
	if hasFiles {
		tipLine = "\n_Tip: run `gb slack files fetch-all` to download attachments into ./slack-files/, then use the Read tool on the saved paths to view images/PDFs._\n"
	}

	budget := maxChars - len(tipLine)
	if budget < 0 {
		budget = 0
	}

	totalLen := 0
	for _, r := range rendered {
		totalLen += len(r)
	}

	var buf strings.Builder
	if totalLen <= budget {
		for _, r := range rendered {
			buf.WriteString(r)
		}
	} else {
		const elisionFmt = "\n_(%d messages elided)_\n"
		const elisionReserve = 32 // upper bound on a rendered elision marker

		n := len(rendered)
		lastIdx := n - 1
		if len(rendered[lastIdx]) >= budget {
			// The single most-recent message is already over budget. Keep it
			// intact rather than emitting a useless truncation marker — the
			// agent needs the latest message to respond coherently.
			if n > 1 {
				buf.WriteString(fmt.Sprintf(elisionFmt, n-1))
			}
			buf.WriteString(rendered[lastIdx])
		} else {
			used := len(rendered[lastIdx])
			tailMin := lastIdx
			includeHead := false
			if n >= 2 && len(rendered[0])+used+elisionReserve <= budget {
				includeHead = true
				used += len(rendered[0])
			}
			firstNonHead := 0
			if includeHead {
				firstNonHead = 1
			}
			for i := lastIdx - 1; i >= firstNonHead; i-- {
				size := len(rendered[i])
				reserve := 0
				if includeHead || i > 0 {
					reserve = elisionReserve
				}
				if used+size+reserve > budget {
					break
				}
				used += size
				tailMin = i
			}
			cursor := 0
			if includeHead {
				buf.WriteString(rendered[0])
				cursor = 1
			}
			if tailMin > cursor {
				buf.WriteString(fmt.Sprintf(elisionFmt, tailMin-cursor))
			}
			for i := tailMin; i <= lastIdx; i++ {
				buf.WriteString(rendered[i])
			}
		}
	}

	if hasFiles {
		buf.WriteString(tipLine)
	}
	return buf.String()
}

// slackHTTPClient returns an HTTP client with a reasonable timeout. When
// SLACK_BRIDGE_TOKEN is set, every request is automatically wrapped with
// `Authorization: Bearer <token>` so the bridge's agent-endpoint auth
// (see bridgeRBAC) accepts the call. Empty token → no auth header, which
// preserves local-dev behaviour.
func slackHTTPClient() *http.Client {
	c := &http.Client{Timeout: 15 * time.Second}
	if tok := os.Getenv("SLACK_BRIDGE_TOKEN"); tok != "" {
		c.Transport = &bearerTokenTransport{token: tok, base: http.DefaultTransport}
	}
	return c
}

type bearerTokenTransport struct {
	token string
	base  http.RoundTripper
}

func (t *bearerTokenTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+t.token)
	}
	return t.base.RoundTrip(req)
}

// printThreadMarkdown formats thread messages as markdown for context injection.
func printThreadMarkdown(msgs []slackThreadMessage, maxChars int, includeBots bool, excludeSubtypes map[string]bool) {
	fmt.Print(formatThreadMarkdown(msgs, maxChars, includeBots, excludeSubtypes))
}

// ── slack channels ─────────────────────────────────────────────────

// bridgeChannelListError turns a non-200 response from the channel-list
// endpoint into a caller-friendly error. A 429 (Slack rate limit propagated by
// the bridge) becomes a distinct "rate limited, retry" message with the
// Retry-After hint, instead of a bare "HTTP 500" that reads like the channel
// does not exist. (ka-0000001584)
func bridgeChannelListError(resp *http.Response) error {
	if resp.StatusCode == http.StatusTooManyRequests {
		retry := resp.Header.Get("Retry-After")
		if retry == "" {
			if body, err := io.ReadAll(io.LimitReader(resp.Body, 4096)); err == nil {
				var e struct {
					RetryAfter int `json:"retry_after_s"`
				}
				if json.Unmarshal(body, &e) == nil && e.RetryAfter > 0 {
					retry = strconv.Itoa(e.RetryAfter)
				}
			}
		}
		if retry != "" {
			return fmt.Errorf("slack rate limited while listing channels — retry in %ss (or address channels by ID)", retry)
		}
		return fmt.Errorf("slack rate limited while listing channels — retry shortly (or address channels by ID)")
	}
	return fmt.Errorf("bridge returned HTTP %d while listing channels", resp.StatusCode)
}

var slackChannelsCmd = &cobra.Command{
	Use:   "channels",
	Short: "List Slack channels the bot is a member of",
	Long:  `Queries the bridge for all channels the bot has been invited to. Only requires SLACK_BRIDGE_URL.`,
	RunE:  runSlackChannels,
}

func runSlackChannels(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/channels"
	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching channels: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return bridgeChannelListError(resp)
	}

	var channels []slackChannelInfo
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if jsonOutput {
		printJSON(channels)
		return nil
	}

	if len(channels) == 0 {
		fmt.Println("Bot is not a member of any channels.")
		return nil
	}

	for _, ch := range channels {
		visibility := "public"
		if ch.Private {
			visibility = "private"
		}
		line := fmt.Sprintf("%-14s %-25s (%s)", ch.ID, "#"+ch.Name, visibility)
		if ch.Topic != "" {
			line += "  " + ch.Topic
		}
		fmt.Println(line)
	}
	return nil
}

// ── slack channel (read history) ───────────────────────────────────

var slackChannelCmd = &cobra.Command{
	Use:   "channel <channel-id-or-name>",
	Short: "Read recent messages from a Slack channel",
	Long: `Fetches recent messages from a channel the bot is a member of.
Accepts either a channel ID (C0123ABC) or a channel name (#general or general).
Only requires SLACK_BRIDGE_URL.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackChannel,
}

func runSlackChannel(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelArg := strings.TrimPrefix(args[0], "#")
	limit, _ := cmd.Flags().GetInt("limit")
	includeBots, _ := cmd.Flags().GetBool("include-bots")

	// If it doesn't look like a channel ID, resolve the name.
	channelID := channelArg
	if !strings.HasPrefix(channelArg, "C") && !strings.HasPrefix(channelArg, "G") {
		resolved, err := resolveChannelName(bridgeURL, channelArg)
		if err != nil {
			return err
		}
		channelID = resolved
	}

	u := fmt.Sprintf("%s/api/slack/channels/history?channel=%s",
		strings.TrimRight(bridgeURL, "/"),
		url.QueryEscape(channelID))
	if limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}

	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching channel history: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var msgs []slackChannelMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if jsonOutput {
		printJSON(msgs)
		return nil
	}

	maxChars, _ := cmd.Flags().GetInt("max-chars")
	excludeSpec, _ := cmd.Flags().GetString("exclude-subtypes")
	printChannelMarkdown(msgs, maxChars, includeBots, parseExcludeSubtypes(excludeSpec))
	return nil
}

// resolveChannelName resolves a channel name to its ID via the channels list endpoint.
func resolveChannelName(bridgeURL, name string) (string, error) {
	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/channels"
	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return "", fmt.Errorf("fetching channels for name resolution: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", bridgeChannelListError(resp)
	}

	var channels []slackChannelInfo
	if err := json.NewDecoder(resp.Body).Decode(&channels); err != nil {
		return "", fmt.Errorf("decoding channels: %w", err)
	}

	for _, ch := range channels {
		if strings.EqualFold(ch.Name, name) {
			return ch.ID, nil
		}
	}
	return "", fmt.Errorf("channel %q not found — is the bot a member?", name)
}

// ── slack search ───────────────────────────────────────────────────

var slackSearchCmd = &cobra.Command{
	Use:   "search <channel> <query>",
	Short: "Search for messages in a Slack channel",
	Long: `Searches recent channel history for messages matching the query.
Returns matching messages with surrounding context lines (like grep -C).
Accepts channel ID or name. Only requires SLACK_BRIDGE_URL.

Note: Uses channel history scanning (not Slack search API) since bot tokens
cannot call search.messages. Scans up to --scan messages (default 200).`,
	Args: cobra.ExactArgs(2),
	RunE: runSlackSearch,
}

func runSlackSearch(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelArg := strings.TrimPrefix(args[0], "#")
	query := args[1]

	channelID := channelArg
	if !strings.HasPrefix(channelArg, "C") && !strings.HasPrefix(channelArg, "G") {
		resolved, err := resolveChannelName(bridgeURL, channelArg)
		if err != nil {
			return err
		}
		channelID = resolved
	}

	limit, _ := cmd.Flags().GetInt("limit")
	ctxLines, _ := cmd.Flags().GetInt("context")
	scan, _ := cmd.Flags().GetInt("scan")

	u := fmt.Sprintf("%s/api/slack/channels/search?channel=%s&q=%s",
		strings.TrimRight(bridgeURL, "/"),
		url.QueryEscape(channelID),
		url.QueryEscape(query))
	if limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}
	if ctxLines >= 0 {
		u += fmt.Sprintf("&context=%d", ctxLines)
	}
	if scan > 0 {
		u += fmt.Sprintf("&scan=%d", scan)
	}

	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("searching channel: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var results []slackSearchResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if jsonOutput {
		printJSON(results)
		return nil
	}

	if len(results) == 0 {
		fmt.Printf("No matches for %q\n", query)
		return nil
	}

	maxChars, _ := cmd.Flags().GetInt("max-chars")
	printSearchMarkdown(results, maxChars)
	return nil
}

// ── slack read-thread ──────────────────────────────────────────────

var slackReadThreadCmd = &cobra.Command{
	Use:   "read-thread <channel> <thread-ts>",
	Short: "Read a thread from any channel",
	Long: `Fetches thread replies from any channel the bot is a member of.
Unlike 'gb slack thread' which only reads the agent's spawning thread,
this command can read any thread given its channel and timestamp.
Only requires SLACK_BRIDGE_URL.`,
	Args: cobra.ExactArgs(2),
	RunE: runSlackReadThread,
}

func runSlackReadThread(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelArg := strings.TrimPrefix(args[0], "#")
	threadTS := args[1]

	channelID := channelArg
	if !strings.HasPrefix(channelArg, "C") && !strings.HasPrefix(channelArg, "G") {
		resolved, err := resolveChannelName(bridgeURL, channelArg)
		if err != nil {
			return err
		}
		channelID = resolved
	}

	limit, _ := cmd.Flags().GetInt("limit")

	u := fmt.Sprintf("%s/api/slack/channels/thread?channel=%s&ts=%s",
		strings.TrimRight(bridgeURL, "/"),
		url.QueryEscape(channelID),
		url.QueryEscape(threadTS))
	if limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}

	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching thread: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var msgs []slackChannelMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	if jsonOutput {
		printJSON(msgs)
		return nil
	}

	includeBots, _ := cmd.Flags().GetBool("include-bots")
	maxChars, _ := cmd.Flags().GetInt("max-chars")
	excludeSpec, _ := cmd.Flags().GetString("exclude-subtypes")
	printChannelMarkdown(msgs, maxChars, includeBots, parseExcludeSubtypes(excludeSpec))
	return nil
}

// ── slack delete ───────────────────────────────────────────────────

var slackDeleteCmd = &cobra.Command{
	Use:   "delete <channel> <timestamp>",
	Short: "Delete a bot message from a channel",
	Long: `Deletes a message posted by the bot in the specified channel.
The bot can only delete its own messages (Slack API restriction).
Channel can be an ID (C...) or a channel name.`,
	Args: cobra.ExactArgs(2),
	RunE: runSlackDelete,
}

func runSlackDelete(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelArg := strings.TrimPrefix(args[0], "#")
	timestamp := args[1]

	channelID := channelArg
	if !strings.HasPrefix(channelArg, "C") && !strings.HasPrefix(channelArg, "G") {
		resolved, err := resolveChannelName(bridgeURL, channelArg)
		if err != nil {
			return err
		}
		channelID = resolved
	}

	result, err := deleteSlackMessage(bridgeURL, channelID, timestamp)
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(result)
		return nil
	}

	if result.OK {
		fmt.Printf("Deleted message %s in channel %s\n", timestamp, channelID)
	} else {
		fmt.Printf("Failed to delete message: %s\n", result.Error)
	}
	return nil
}

// ── slack delete-bot-messages ──────────────────────────────────────

var slackDeleteBotMsgsCmd = &cobra.Command{
	Use:   "delete-bot-messages <channel>",
	Short: "Delete all bot messages from a channel",
	Long: `Scans a channel for messages posted by the bot and deletes them.
Only deletes messages the bot itself posted (Slack API restriction).
Use --dry-run to preview which messages would be deleted.
Channel can be an ID (C...) or a channel name.`,
	Args: cobra.ExactArgs(1),
	RunE: runSlackDeleteBotMsgs,
}

func runSlackDeleteBotMsgs(cmd *cobra.Command, args []string) error {
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the Slack bridge")
	}

	channelArg := strings.TrimPrefix(args[0], "#")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	scanLimit, _ := cmd.Flags().GetInt("scan")

	channelID := channelArg
	if !strings.HasPrefix(channelArg, "C") && !strings.HasPrefix(channelArg, "G") {
		resolved, err := resolveChannelName(bridgeURL, channelArg)
		if err != nil {
			return err
		}
		channelID = resolved
	}

	// Fetch channel history to find bot messages.
	u := fmt.Sprintf("%s/api/slack/channels/history?channel=%s&limit=%d",
		strings.TrimRight(bridgeURL, "/"),
		url.QueryEscape(channelID),
		scanLimit)

	resp, err := slackHTTPClient().Get(u)
	if err != nil {
		return fmt.Errorf("fetching channel history: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}

	var msgs []slackChannelMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}

	// Filter to bot messages only.
	var botMsgs []slackChannelMessage
	for _, m := range msgs {
		if m.IsBot {
			botMsgs = append(botMsgs, m)
		}
	}

	if len(botMsgs) == 0 {
		fmt.Println("No bot messages found in channel.")
		return nil
	}

	fmt.Printf("Found %d bot message(s) in channel %s:\n", len(botMsgs), channelID)
	for _, m := range botMsgs {
		preview := m.Text
		if len(preview) > 80 {
			preview = preview[:80] + "..."
		}
		fmt.Printf("  [%s] %s\n", m.Timestamp, preview)
	}

	if dryRun {
		fmt.Println("\n(dry run — no messages deleted)")
		return nil
	}

	fmt.Println()
	deleted := 0
	failed := 0
	for _, m := range botMsgs {
		result, err := deleteSlackMessage(bridgeURL, channelID, m.Timestamp)
		if err != nil || !result.OK {
			errMsg := ""
			if err != nil {
				errMsg = err.Error()
			} else {
				errMsg = result.Error
			}
			fmt.Printf("  FAIL %s: %s\n", m.Timestamp, errMsg)
			failed++
			continue
		}
		fmt.Printf("  OK   %s\n", m.Timestamp)
		deleted++
		// Brief pause to avoid Slack rate limits.
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Printf("\nDeleted %d/%d message(s)", deleted, len(botMsgs))
	if failed > 0 {
		fmt.Printf(" (%d failed)", failed)
	}
	fmt.Println()
	return nil
}

// deleteSlackMessage calls the bridge API to delete a single message.
func deleteSlackMessage(bridgeURL, channel, timestamp string) (*slackDeleteResponse, error) {
	u := strings.TrimRight(bridgeURL, "/") + "/api/slack/messages/delete"

	body, _ := json.Marshal(map[string]string{
		"channel":   channel,
		"timestamp": timestamp,
	})

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling delete API: %w", err)
	}
	defer resp.Body.Close()

	var result slackDeleteResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}

	if !result.OK {
		return &result, fmt.Errorf("delete failed: %s", result.Error)
	}
	return &result, nil
}

type slackDeleteResponse struct {
	OK        bool   `json:"ok"`
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
	Error     string `json:"error,omitempty"`
}

// ── channel types ──────────────────────────────────────────────────

type slackChannelInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Private bool   `json:"private"`
}

type slackChannelMessage struct {
	Author    string `json:"author"`
	Text      string `json:"text"`
	Timestamp string `json:"timestamp"`
	IsBot     bool   `json:"is_bot"`
	Subtype   string `json:"subtype,omitempty"`
	ThreadTS  string `json:"thread_ts,omitempty"`
}

type slackSearchResult struct {
	Message slackChannelMessage   `json:"message"`
	Before  []slackChannelMessage `json:"before,omitempty"`
	After   []slackChannelMessage `json:"after,omitempty"`
}

// ── channel helpers ────────────────────────────────────────────────

// resolveBridgeURL returns the Slack bridge URL from env, or empty string.
func resolveBridgeURL() string {
	return os.Getenv("SLACK_BRIDGE_URL")
}

// printChannelMarkdown formats channel messages as markdown output.
func printChannelMarkdown(msgs []slackChannelMessage, maxChars int, includeBots bool, excludeSubtypes map[string]bool) {
	if maxChars <= 0 {
		maxChars = 8000
	}

	if len(msgs) == 0 {
		fmt.Println("(No messages)")
		return
	}

	var buf strings.Builder
	for _, m := range msgs {
		if m.IsBot && !includeBots {
			continue
		}
		if m.IsBot && m.Subtype != "" && excludeSubtypes[m.Subtype] {
			continue
		}
		prefix := ""
		if m.IsBot {
			prefix = "[bot] "
		}
		line := fmt.Sprintf("%s**%s**: %s\n", prefix, m.Author, m.Text)
		if buf.Len()+len(line) > maxChars {
			buf.WriteString("\n_(channel history truncated)_\n")
			break
		}
		buf.WriteString(line)
	}
	fmt.Print(buf.String())
}

// printSearchMarkdown formats search results with context as markdown.
func printSearchMarkdown(results []slackSearchResult, maxChars int) {
	if maxChars <= 0 {
		maxChars = 12000
	}

	var buf strings.Builder
	for i, r := range results {
		if i > 0 {
			buf.WriteString("\n---\n\n")
		}
		for _, m := range r.Before {
			line := fmt.Sprintf("  %s: %s\n", m.Author, m.Text)
			buf.WriteString(line)
		}
		// Highlight the matching message.
		buf.WriteString(fmt.Sprintf("→ **%s**: %s\n", r.Message.Author, r.Message.Text))
		if r.Message.ThreadTS != "" {
			buf.WriteString(fmt.Sprintf("  _(thread: %s)_\n", r.Message.ThreadTS))
		}
		for _, m := range r.After {
			line := fmt.Sprintf("  %s: %s\n", m.Author, m.Text)
			buf.WriteString(line)
		}

		if buf.Len() > maxChars {
			buf.WriteString("\n_(search results truncated)_\n")
			break
		}
	}
	fmt.Printf("%d match(es):\n\n%s", len(results), buf.String())
}

// excludeSubtypesFlagHelp is the --exclude-subtypes help of every read command.
const excludeSubtypesFlagHelp = `comma-separated bot-message subtypes to drop from output (squawk,bead-mirror,interactive,survey). ` +
	`"all"/"noise"/"*" expands to the full default set. "" disables filtering. ` +
	`Conversational bot replies (untagged) are always kept when --include-bots=true.`

func init() {
	const excludeFlagHelp = excludeSubtypesFlagHelp

	addThreadReadFlags(slackThreadCmd)

	slackChannelCmd.Flags().Int("limit", 50, "maximum number of messages to fetch")
	slackChannelCmd.Flags().Int("max-chars", 8000, "truncate output after this many characters")
	slackChannelCmd.Flags().Bool("include-bots", false, "include bot messages in output")
	slackChannelCmd.Flags().String("exclude-subtypes", defaultExcludeSubtypes, excludeFlagHelp)

	slackSearchCmd.Flags().Int("limit", 20, "maximum number of matches to return")
	slackSearchCmd.Flags().IntP("context", "C", 2, "number of context messages around each match")
	slackSearchCmd.Flags().Int("scan", 200, "number of recent messages to scan (max 1000)")
	slackSearchCmd.Flags().Int("max-chars", 12000, "truncate output after this many characters")

	slackReadThreadCmd.Flags().Int("limit", 100, "maximum number of thread messages")
	slackReadThreadCmd.Flags().Int("max-chars", 8000, "truncate output after this many characters")
	slackReadThreadCmd.Flags().Bool("include-bots", false, "include bot messages in output")
	slackReadThreadCmd.Flags().String("exclude-subtypes", defaultExcludeSubtypes, excludeFlagHelp)

	slackDeleteBotMsgsCmd.Flags().Bool("dry-run", false, "preview messages that would be deleted without actually deleting")
	slackDeleteBotMsgsCmd.Flags().Int("scan", 200, "number of recent messages to scan (max 200)")

	slackCmd.AddCommand(slackThreadCmd)
	slackCmd.AddCommand(slackReplyCmd)
	slackCmd.AddCommand(slackChannelsCmd)
	slackCmd.AddCommand(slackUserCmd)
	slackCmd.AddCommand(slackChannelCmd)
	slackCmd.AddCommand(slackSearchCmd)
	slackCmd.AddCommand(slackReadThreadCmd)
	slackCmd.AddCommand(slackDeleteCmd)
	slackCmd.AddCommand(slackDeleteBotMsgsCmd)
}
