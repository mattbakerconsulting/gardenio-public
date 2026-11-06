package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// gb thread is gb say's reading twin (design spec section 4.1): it reads a
// thread from the bridge's GET /api/conversation/thread, which serves a
// thread's message beads, or its Slack history while the thread is mirrored
// to Slack. gb slack thread is its permanent alias for the home thread.

var threadCmd = &cobra.Command{
	Use:   "thread [head-id]",
	Short: "Read a thread: your home thread, or the one with this head bead",
	Long: `Reads a thread as markdown suitable for context injection. With no
argument it reads your home thread (the Slack thread you were spawned from, or
the thread on your agent bead); with a thread head's bead id it reads that one.

A thread that lives in Slack is read from Slack, which holds its full history
while Slack is mirrored; a thread that lives only in the Commons is read from
its message beads. --source=beads reads the beads of a Slack thread instead.

Bot messages (squawks, replies, decisions) are included by default. Bridge
auto-posts (squawk scaffolding, bead-lifecycle mirrors, Block-Kit-only cards,
survey rate cards) are dropped by default — override with --exclude-subtypes="".

gb slack thread is a permanent alias of 'gb thread' with no argument.`,
	GroupID: "orchestration",
	Args:    cobra.MaximumNArgs(1),
	RunE:    runThread,
}

func init() {
	addThreadReadFlags(threadCmd)
	threadCmd.Flags().String("source", "auto", "where to read the thread from: auto, beads or slack")
}

// addThreadReadFlags registers the read flags gb thread and gb slack thread
// share.
func addThreadReadFlags(cmd *cobra.Command) {
	cmd.Flags().Int("limit", 50, "maximum number of messages to fetch")
	cmd.Flags().Int("max-chars", 4000, "truncate output after this many characters")
	cmd.Flags().Bool("include-bots", true, "include bot messages in output (squawks, replies, decisions)")
	cmd.Flags().String("exclude-subtypes", defaultExcludeSubtypes, excludeSubtypesFlagHelp)
}

func runThread(cmd *cobra.Command, args []string) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	bridgeURL := resolveBridgeURL()
	if bridgeURL == "" {
		return fmt.Errorf("SLACK_BRIDGE_URL is not set — cannot reach the bridge")
	}
	limit, _ := cmd.Flags().GetInt("limit")
	source := "auto"
	if f := cmd.Flags().Lookup("source"); f != nil {
		source = f.Value.String()
	}

	// The bridge reads a thread only for an agent of its land (or the agent's
	// own thread): it needs to know who is reading.
	q := url.Values{"agent": {bridgeAgent(ctx)}}
	var channel, ts string
	if len(args) == 1 {
		q.Set("thread", args[0])
	} else {
		if channel, ts = resolveThreadCoords(ctx); channel != "" && ts != "" {
			q.Set("channel", channel)
			q.Set("ts", ts)
		} else if head := homeThreadHead(ctx); head != "" {
			q.Set("thread", head)
		} else {
			return fmt.Errorf("no home thread: SLACK_THREAD_CHANNEL/SLACK_THREAD_TS are not set and the agent bead names no thread; pass a thread head id")
		}
	}
	if limit > 0 {
		q.Set("limit", fmt.Sprint(limit))
	}
	if source != "" && source != "auto" {
		q.Set("source", source)
	}

	msgs, err := conversationThread(ctx, bridgeURL, q)
	if errors.Is(err, errBridgeWithoutConversation) {
		if channel == "" || ts == "" || (source != "auto" && source != "slack") {
			return fmt.Errorf("the bridge at %s predates gb thread; it can only read a Slack thread (gb slack thread)", bridgeURL)
		}
		msgs, err = slackThreadMessages(ctx, bridgeURL, channel, ts, limit)
	}
	if err != nil {
		return err
	}

	if jsonOutput {
		printJSON(msgs)
		return nil
	}
	maxChars, _ := cmd.Flags().GetInt("max-chars")
	includeBots, _ := cmd.Flags().GetBool("include-bots")
	excludeSpec, _ := cmd.Flags().GetString("exclude-subtypes")
	printThreadMarkdown(msgs, maxChars, includeBots, parseExcludeSubtypes(excludeSpec))
	return nil
}

// conversationThread reads a thread from GET /api/conversation/thread.
func conversationThread(ctx context.Context, bridgeURL string, q url.Values) ([]slackThreadMessage, error) {
	u := strings.TrimRight(bridgeURL, "/") + "/api/conversation/thread?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	setBridgeAgentToken(ctx, req)
	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching thread: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg := readLimited(resp.Body, 4096)
		if conversationUnavailable(resp, msg) {
			return nil, errBridgeWithoutConversation
		}
		return nil, bridgeError(resp, msg)
	}
	var out struct {
		Messages []slackThreadMessage `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return out.Messages, nil
}

// slackThreadMessages reads a Slack thread from the legacy
// GET /api/slack/threads, for a bridge that predates /api/conversation.
func slackThreadMessages(ctx context.Context, bridgeURL, channel, ts string, limit int) ([]slackThreadMessage, error) {
	u := fmt.Sprintf("%s/api/slack/threads?channel=%s&ts=%s",
		strings.TrimRight(bridgeURL, "/"), url.QueryEscape(channel), url.QueryEscape(ts))
	if limit > 0 {
		u += fmt.Sprintf("&limit=%d", limit)
	}
	u += legacyThreadAgentParam(ctx)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	setBridgeAgentToken(ctx, req)
	resp, err := slackHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching thread: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bridge returned HTTP %d", resp.StatusCode)
	}
	var msgs []slackThreadMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return msgs, nil
}
