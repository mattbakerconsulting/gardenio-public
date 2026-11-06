package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/slack-go/slack"
)

// defaultChannelCacheTTL is how long a fetched conversations.list result is
// served from cache before a refresh. conversations.list is a Slack Tier-2
// method (aggressive rate limiting); without caching, every name-based lookup
// (gb slack channel <name>, gb slack search <name> ...) burns the rate-limit
// window and once burned every name-based call 500s. (ka-0000001584)
const defaultChannelCacheTTL = 5 * time.Minute

// SlackChannelAPI serves the Slack channel read API endpoints.
// These endpoints allow agents to list channels and read channel history
// via the gb CLI, without needing direct Slack API access.
type SlackChannelAPI struct {
	api    *slack.Client
	logger *slog.Logger

	// Channel-list TTL cache. Guards against burning the conversations.list
	// Tier-2 rate limit on every name-based lookup. See listChannelsCached.
	cacheMu        sync.Mutex
	cachedChannels []ChannelInfo
	cacheExpiry    time.Time
	cacheStamped   bool          // whether cachedChannels holds a real (possibly stale) result
	cacheTTL       time.Duration // configurable for tests
	now            func() time.Time
}

// NewSlackChannelAPI creates a new Slack channel API handler.
func NewSlackChannelAPI(api *slack.Client, logger *slog.Logger) *SlackChannelAPI {
	return &SlackChannelAPI{
		api:      api,
		logger:   logger,
		cacheTTL: defaultChannelCacheTTL,
		now:      time.Now,
	}
}

// RegisterRoutes registers channel API routes on the given mux.
func (a *SlackChannelAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/channels", a.handleListChannels)
	mux.HandleFunc("/api/slack/channels/history", a.handleGetChannelHistory)
	mux.HandleFunc("/api/slack/channels/search", a.handleSearchChannel)
	mux.HandleFunc("/api/slack/channels/thread", a.handleGetChannelThread)
	mux.HandleFunc("/api/slack/messages/delete", a.handleDeleteMessage)
}

// ChannelInfo represents a Slack channel the bot is a member of.
type ChannelInfo struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Topic   string `json:"topic,omitempty"`
	Purpose string `json:"purpose,omitempty"`
	Private bool   `json:"private"`
}

// ChannelMessage represents a single message in a channel.
//
// Subtype is set for bot auto-posts that match a known noise bucket
// (squawk / bead-mirror / interactive / survey). Empty for human
// messages and conversational bot replies. See bot_message_subtype.go.
type ChannelMessage struct {
	Author    string          `json:"author"`
	Text      string          `json:"text"`
	Timestamp string          `json:"timestamp"`
	IsBot     bool            `json:"is_bot"`
	Subtype   string          `json:"subtype,omitempty"`
	ThreadTS  string          `json:"thread_ts,omitempty"`
	Files     []SlackFileInfo `json:"files,omitempty"`
}

// handleListChannels handles GET /api/slack/channels.
// Returns channels the bot is a member of, served from a short-lived TTL cache
// to avoid burning the conversations.list Tier-2 rate limit (ka-0000001584).
func (a *SlackChannelAPI) handleListChannels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	channels, err := a.listChannelsCached()
	if err != nil {
		var rl *slack.RateLimitedError
		if errors.As(err, &rl) {
			// Surface Slack's Retry-After distinctly instead of collapsing to a
			// generic 500, so callers see "rate limited, retry" rather than
			// concluding the channel does not exist. Honor Retry-After.
			secs := max(int(rl.RetryAfter.Round(time.Second)/time.Second), 1)
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			a.logger.Warn("slack rate limited listing channels", "retry_after_s", secs)
			http.Error(w,
				fmt.Sprintf(`{"error":"slack rate limited, retry in %ds","retry_after_s":%d}`, secs, secs),
				http.StatusTooManyRequests)
			return
		}
		a.logger.Error("failed to list channels", "error", err)
		http.Error(w, `{"error":"failed to list channels"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(channels)
}

// listChannelsCached returns the bot's member channels, serving a cached copy
// while it is fresh (cacheTTL) and otherwise refreshing from conversations.list.
//
// On a refresh error it falls back to the last successfully-fetched result (even
// if stale) rather than failing — a transient Slack 429 should not blank out
// name resolution when we already have a good answer. The error is only returned
// to the caller when there is no prior result to serve.
func (a *SlackChannelAPI) listChannelsCached() ([]ChannelInfo, error) {
	a.cacheMu.Lock()
	defer a.cacheMu.Unlock()

	if a.cacheStamped && a.now().Before(a.cacheExpiry) {
		return a.cachedChannels, nil
	}

	channels, err := a.fetchAllChannels()
	if err != nil {
		if a.cacheStamped {
			// Serve the last good (stale) result and log; don't fail resolution.
			a.logger.Warn("serving stale channel cache after refresh error", "error", err)
			return a.cachedChannels, nil
		}
		return nil, err
	}

	a.cachedChannels = channels
	a.cacheExpiry = a.now().Add(a.cacheTTL)
	a.cacheStamped = true
	return channels, nil
}

// fetchAllChannels pages conversations.list and returns member channels.
func (a *SlackChannelAPI) fetchAllChannels() ([]ChannelInfo, error) {
	var allChannels []ChannelInfo
	cursor := ""

	for {
		params := &slack.GetConversationsParameters{
			Limit:           200,
			Cursor:          cursor,
			ExcludeArchived: true,
			Types:           []string{"public_channel", "private_channel"},
		}
		channels, nextCursor, err := a.api.GetConversations(params)
		if err != nil {
			return nil, err
		}

		for _, ch := range channels {
			// Only include channels the bot is a member of.
			if !ch.IsMember {
				continue
			}
			allChannels = append(allChannels, ChannelInfo{
				ID:      ch.ID,
				Name:    ch.Name,
				Topic:   ch.Topic.Value,
				Purpose: ch.Purpose.Value,
				Private: ch.IsPrivate,
			})
		}

		if nextCursor == "" {
			break
		}
		cursor = nextCursor
	}

	return allChannels, nil
}

// extractMessageText returns a readable text representation of a Slack message,
// combining the plain text body with content from attachments and Block Kit blocks.
// Many bot messages (alerts, webhooks) put their content exclusively in attachments
// or blocks, leaving the Text field empty.
func extractMessageText(msg slack.Msg) string {
	var parts []string

	hasText := strings.TrimSpace(msg.Text) != ""
	if hasText {
		parts = append(parts, strings.TrimSpace(msg.Text))
	}

	// Extract text from attachments (legacy rich formatting).
	for _, att := range msg.Attachments {
		var attParts []string
		if att.Pretext != "" {
			attParts = append(attParts, att.Pretext)
		}
		if att.Title != "" {
			attParts = append(attParts, att.Title)
		}
		if att.Text != "" {
			attParts = append(attParts, att.Text)
		}
		for _, f := range att.Fields {
			if f.Title != "" && f.Value != "" {
				attParts = append(attParts, f.Title+": "+f.Value)
			} else if f.Value != "" {
				attParts = append(attParts, f.Value)
			}
		}
		if len(attParts) > 0 {
			parts = append(parts, strings.Join(attParts, "\n"))
		}
	}

	// Block Kit blocks are skipped when msg.Text is non-empty: Slack mirrors
	// the message body into both Text and a RichTextBlock for any post with
	// formatting, so extracting both double-renders the message (and the
	// per-element walk produces one-token-per-line garbage). Block extraction
	// is preserved for bot/webhook posts that have no Text.
	if hasText {
		return strings.TrimSpace(strings.Join(parts, "\n"))
	}

	// Extract text from Block Kit blocks.
	for _, block := range msg.Blocks.BlockSet {
		switch b := block.(type) {
		case *slack.SectionBlock:
			if b.Text != nil && b.Text.Text != "" {
				parts = append(parts, b.Text.Text)
			}
			for _, field := range b.Fields {
				if field != nil && field.Text != "" {
					parts = append(parts, field.Text)
				}
			}
		case *slack.HeaderBlock:
			if b.Text != nil && b.Text.Text != "" {
				parts = append(parts, b.Text.Text)
			}
		case *slack.ContextBlock:
			for _, elem := range b.ContextElements.Elements {
				if textObj, ok := elem.(*slack.TextBlockObject); ok && textObj.Text != "" {
					parts = append(parts, textObj.Text)
				}
			}
		case *slack.RichTextBlock:
			for _, elem := range b.Elements {
				if section, ok := elem.(*slack.RichTextSection); ok {
					for _, se := range section.Elements {
						if te, ok := se.(*slack.RichTextSectionTextElement); ok && te.Text != "" {
							parts = append(parts, te.Text)
						}
					}
				}
			}
		}
	}

	return strings.TrimSpace(strings.Join(parts, "\n"))
}

// handleGetChannelHistory handles GET /api/slack/channels/history?channel={id}&limit={n}&oldest={ts}&latest={ts}.
func (a *SlackChannelAPI) handleGetChannelHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	channel := r.URL.Query().Get("channel")
	if channel == "" {
		http.Error(w, `{"error":"channel query parameter is required"}`, http.StatusBadRequest)
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 200 {
				n = 200
			}
			limit = n
		}
	}

	params := &slack.GetConversationHistoryParameters{
		ChannelID: channel,
		Limit:     limit,
	}

	if oldest := r.URL.Query().Get("oldest"); oldest != "" {
		params.Oldest = oldest
	}
	if latest := r.URL.Query().Get("latest"); latest != "" {
		params.Latest = latest
	}

	resp, err := a.api.GetConversationHistory(params)
	if err != nil {
		a.logger.Error("failed to get channel history", "channel", channel, "error", err)
		http.Error(w, `{"error":"failed to fetch channel history"}`, http.StatusInternalServerError)
		return
	}

	// Convert to response format. Resolve user IDs to display names
	// via Slack API (cached per request).
	resolver := newSlackUserResolver(a.api, a.logger)
	result := make([]ChannelMessage, 0, len(resp.Messages))
	for _, msg := range resp.Messages {
		isBot := msg.BotID != "" || msg.SubType == "bot_message"

		author := resolver.resolve(msg.User)
		if author == "" {
			author = msg.Username
		}
		if author == "" && isBot {
			author = "bot"
		}

		text := extractMessageText(msg.Msg)
		cm := ChannelMessage{
			Author:    author,
			Text:      text,
			Timestamp: msg.Timestamp,
			IsBot:     isBot,
			ThreadTS:  msg.ThreadTimestamp,
		}
		if isBot {
			cm.Subtype = classifyBotMessage(text, msg.Msg)
		}

		for _, f := range msg.Files {
			cm.Files = append(cm.Files, SlackFileInfo{
				ID:       f.ID,
				Name:     f.Name,
				Mimetype: f.Mimetype,
				Size:     f.Size,
				ProxyURL: "/api/slack/files/" + f.ID,
			})
		}

		result = append(result, cm)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// SearchResult represents a message matching a search query with surrounding context.
type SearchResult struct {
	Message ChannelMessage   `json:"message"`
	Before  []ChannelMessage `json:"before,omitempty"`
	After   []ChannelMessage `json:"after,omitempty"`
}

// handleSearchChannel handles GET /api/slack/channels/search?channel={id}&q={query}&limit={n}&context={n}.
// Fetches recent history and filters server-side (bot tokens cannot use search.messages).
func (a *SlackChannelAPI) handleSearchChannel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	channel := r.URL.Query().Get("channel")
	query := r.URL.Query().Get("q")
	if channel == "" || query == "" {
		http.Error(w, `{"error":"channel and q query parameters are required"}`, http.StatusBadRequest)
		return
	}

	// How many messages to scan (default 200, max 1000).
	scanLimit := 200
	if v := r.URL.Query().Get("scan"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 1000 {
				n = 1000
			}
			scanLimit = n
		}
	}

	// How many context lines around each match.
	ctxLines := 2
	if v := r.URL.Query().Get("context"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			if n > 10 {
				n = 10
			}
			ctxLines = n
		}
	}

	// Max number of matches to return.
	maxResults := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			maxResults = n
		}
	}

	queryLower := strings.ToLower(query)

	// Resolve user IDs to display names via Slack API (cached per request).
	resolver := newSlackUserResolver(a.api, a.logger)

	// Fetch messages in batches to scan.
	var allMsgs []ChannelMessage
	cursor := ""
	for len(allMsgs) < scanLimit {
		batchSize := 200
		if remaining := scanLimit - len(allMsgs); remaining < batchSize {
			batchSize = remaining
		}

		params := &slack.GetConversationHistoryParameters{
			ChannelID: channel,
			Limit:     batchSize,
			Cursor:    cursor,
		}
		resp, err := a.api.GetConversationHistory(params)
		if err != nil {
			a.logger.Error("failed to get channel history for search", "channel", channel, "error", err)
			http.Error(w, `{"error":"failed to fetch channel history"}`, http.StatusInternalServerError)
			return
		}

		for _, msg := range resp.Messages {
			isBot := msg.BotID != "" || msg.SubType == "bot_message"
			author := resolver.resolve(msg.User)
			if author == "" {
				author = msg.Username
			}
			if author == "" && isBot {
				author = "bot"
			}
			text := extractMessageText(msg.Msg)
			cm := ChannelMessage{
				Author:    author,
				Text:      text,
				Timestamp: msg.Timestamp,
				IsBot:     isBot,
				ThreadTS:  msg.ThreadTimestamp,
			}
			if isBot {
				cm.Subtype = classifyBotMessage(text, msg.Msg)
			}
			allMsgs = append(allMsgs, cm)
		}

		if !resp.HasMore || resp.ResponseMetaData.NextCursor == "" {
			break
		}
		cursor = resp.ResponseMetaData.NextCursor
	}

	// Messages come newest-first from Slack; reverse for chronological context.
	for i, j := 0, len(allMsgs)-1; i < j; i, j = i+1, j-1 {
		allMsgs[i], allMsgs[j] = allMsgs[j], allMsgs[i]
	}

	// Find matches and extract context.
	var results []SearchResult
	for i, msg := range allMsgs {
		if !strings.Contains(strings.ToLower(msg.Text), queryLower) &&
			!strings.Contains(strings.ToLower(msg.Author), queryLower) {
			continue
		}

		sr := SearchResult{Message: msg}

		// Before context.
		start := i - ctxLines
		if start < 0 {
			start = 0
		}
		if start < i {
			sr.Before = allMsgs[start:i]
		}

		// After context.
		end := i + 1 + ctxLines
		if end > len(allMsgs) {
			end = len(allMsgs)
		}
		if i+1 < end {
			sr.After = allMsgs[i+1 : end]
		}

		results = append(results, sr)
		if len(results) >= maxResults {
			break
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(results)
}

// handleGetChannelThread handles GET /api/slack/channels/thread?channel={id}&ts={ts}&limit={n}.
// Reads a thread within any channel (not just the agent's spawning thread).
func (a *SlackChannelAPI) handleGetChannelThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	channel := r.URL.Query().Get("channel")
	ts := r.URL.Query().Get("ts")
	if channel == "" || ts == "" {
		http.Error(w, `{"error":"channel and ts query parameters are required"}`, http.StatusBadRequest)
		return
	}

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 500 {
				n = 500
			}
			limit = n
		}
	}

	msgs, _, _, err := a.api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: ts,
		Limit:     limit,
	})
	if err != nil {
		a.logger.Error("failed to get thread replies", "channel", channel, "ts", ts, "error", err)
		http.Error(w, `{"error":"failed to fetch thread messages"}`, http.StatusInternalServerError)
		return
	}

	resolver := newSlackUserResolver(a.api, a.logger)
	result := make([]ChannelMessage, 0, len(msgs))
	for _, msg := range msgs {
		isBot := msg.BotID != "" || msg.SubType == "bot_message"
		author := resolver.resolve(msg.User)
		if author == "" {
			author = msg.Username
		}
		if author == "" && isBot {
			author = "bot"
		}
		text := extractMessageText(msg.Msg)
		cm := ChannelMessage{
			Author:    author,
			Text:      text,
			Timestamp: msg.Timestamp,
			IsBot:     isBot,
		}
		if isBot {
			cm.Subtype = classifyBotMessage(text, msg.Msg)
		}
		for _, f := range msg.Files {
			cm.Files = append(cm.Files, SlackFileInfo{
				ID:       f.ID,
				Name:     f.Name,
				Mimetype: f.Mimetype,
				Size:     f.Size,
				ProxyURL: "/api/slack/files/" + f.ID,
			})
		}
		result = append(result, cm)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// deleteMessageRequest is the JSON body for POST /api/slack/messages/delete.
type deleteMessageRequest struct {
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
}

// deleteMessageResponse is the JSON response from POST /api/slack/messages/delete.
type deleteMessageResponse struct {
	OK        bool   `json:"ok"`
	Channel   string `json:"channel"`
	Timestamp string `json:"timestamp"`
	Error     string `json:"error,omitempty"`
}

// handleDeleteMessage handles POST /api/slack/messages/delete.
// Deletes a message posted by the bot. The Slack API only allows bots to
// delete their own messages (chat:write scope covers this).
func (a *SlackChannelAPI) handleDeleteMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req deleteMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid JSON body"}`, http.StatusBadRequest)
		return
	}

	if req.Channel == "" || req.Timestamp == "" {
		http.Error(w, `{"error":"channel and timestamp are required"}`, http.StatusBadRequest)
		return
	}

	_, _, err := a.api.DeleteMessage(req.Channel, req.Timestamp)
	if err != nil {
		a.logger.Error("failed to delete message", "channel", req.Channel, "ts", req.Timestamp, "error", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(deleteMessageResponse{
			OK:    false,
			Error: err.Error(),
		})
		return
	}

	a.logger.Info("deleted message", "channel", req.Channel, "ts", req.Timestamp)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(deleteMessageResponse{
		OK:        true,
		Channel:   req.Channel,
		Timestamp: req.Timestamp,
	})
}
