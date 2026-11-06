package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/slack-go/slack"
)

// maxFileProxySize is the maximum file size (50MB) the proxy will stream.
const maxFileProxySize = 50 * 1024 * 1024

// SlackFileInfo represents file metadata exposed to agents via the thread API.
type SlackFileInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Mimetype string `json:"mimetype"`
	Size     int    `json:"size"`
	ProxyURL string `json:"proxy_url"`
}

// SlackThreadAPI serves the Slack thread read/reply API endpoints.
// These endpoints allow agents to read thread messages and post replies
// via the gb CLI, without needing direct Slack API access.
type SlackThreadAPI struct {
	api    *slack.Client
	logger *slog.Logger
	dedup  *threadReplyDeduper
	// readWall checks a GET /api/slack/threads read (agent is "" when the
	// caller names none) against the land wall before Slack is read
	// (gd-3uBuj2h7Ar). NewConversationAgentAPI sets it; while it is nil (a
	// bridge with no bot, which cannot place a thread in a land) every read
	// is refused.
	readWall func(ctx context.Context, agent, channel, ts string) error
}

// NewSlackThreadAPI creates a new Slack thread API handler.
func NewSlackThreadAPI(api *slack.Client, logger *slog.Logger) *SlackThreadAPI {
	return &SlackThreadAPI{
		api:    api,
		logger: logger,
		dedup:  newThreadReplyDeduper(threadReplyDedupWindow),
	}
}

// RegisterRoutes registers thread API routes on the given mux.
func (a *SlackThreadAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/slack/threads", a.handleGetThreadMessages)
	mux.HandleFunc("/api/slack/threads/reply", a.handlePostReply)
	mux.HandleFunc("/api/slack/threads/upload", a.handlePostUpload)
	mux.HandleFunc("/api/slack/files/", a.handleFileProxy)
}

// ThreadMessage represents a single message in a Slack thread.
//
// Subtype is set for bot auto-posts that match a known noise bucket
// (squawk / bead-mirror / interactive / survey) so callers can filter
// without re-implementing classification. Empty for human messages and
// conversational bot replies. See bot_message_subtype.go.
type ThreadMessage struct {
	Author    string          `json:"author"`
	Text      string          `json:"text"`
	Timestamp string          `json:"timestamp"`
	IsBot     bool            `json:"is_bot"`
	Subtype   string          `json:"subtype,omitempty"`
	Files     []SlackFileInfo `json:"files,omitempty"`
}

// handleGetThreadMessages handles GET
// /api/slack/threads?channel={channel}&ts={ts}&limit={n}[&agent={name}], the
// reading agent for the land wall (readWall).
func (a *SlackThreadAPI) handleGetThreadMessages(w http.ResponseWriter, r *http.Request) {
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

	if a.readWall == nil {
		writeInboundError(w, http.StatusServiceUnavailable, "bridge has no Slack bot")
		return
	}
	if err := a.readWall(r.Context(), extractAgentName(r.URL.Query().Get("agent")), channel, ts); err != nil {
		writeSayError(w, err)
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	result, err := a.fetchThread(r.Context(), channel, ts, limit)
	if err != nil {
		http.Error(w, `{"error":"failed to fetch thread messages"}`, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// fetchThread reads up to limit messages of the Slack thread (channel, ts),
// root first, with authors resolved to display names (cached per call) and
// bot auto-posts classified. GET /api/slack/threads and
// GET /api/conversation/thread both read Slack history through it.
func (a *SlackThreadAPI) fetchThread(ctx context.Context, channel, ts string, limit int) ([]ThreadMessage, error) {
	msgs, _, _, err := a.api.GetConversationRepliesContext(ctx, &slack.GetConversationRepliesParameters{
		ChannelID: channel,
		Timestamp: ts,
		Limit:     limit,
	})
	if err != nil {
		a.logger.Error("failed to get thread replies", "channel", channel, "ts", ts, "error", err)
		return nil, err
	}

	// Convert to our response format, including bot messages.
	// Resolve user IDs to display names via Slack API (cached per request).
	resolver := newSlackUserResolver(a.api, a.logger)

	result := make([]ThreadMessage, 0, len(msgs))
	for _, msg := range msgs {
		isBot := msg.BotID != "" || msg.SubType == "bot_message"
		author := resolver.resolve(msg.User)
		if author == "" {
			author = msg.Username
		}
		if isBot && author == "" {
			// Use bot profile name for bot messages without a user/username.
			if msg.BotProfile != nil {
				author = msg.BotProfile.Name
			}
			if author == "" {
				author = "bot"
			}
		}
		text := extractMessageText(msg.Msg)
		tm := ThreadMessage{
			Author:    author,
			Text:      text,
			Timestamp: msg.Timestamp,
			IsBot:     isBot,
		}
		if isBot {
			tm.Subtype = classifyBotMessage(text, msg.Msg)
		}
		for _, f := range msg.Files {
			tm.Files = append(tm.Files, SlackFileInfo{
				ID:       f.ID,
				Name:     f.Name,
				Mimetype: f.Mimetype,
				Size:     f.Size,
				ProxyURL: "/api/slack/files/" + f.ID,
			})
		}
		result = append(result, tm)
	}
	return result, nil
}

// handleFileProxy handles GET /api/slack/files/{file_id}.
// It fetches file metadata from Slack and streams the file content to the caller.
func (a *SlackThreadAPI) handleFileProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	// Extract file ID from path: /api/slack/files/{file_id}
	fileID := strings.TrimPrefix(r.URL.Path, "/api/slack/files/")
	if fileID == "" {
		http.Error(w, `{"error":"file_id is required"}`, http.StatusBadRequest)
		return
	}

	// Get file metadata from Slack.
	file, _, _, err := a.api.GetFileInfo(fileID, 0, 0)
	if err != nil {
		a.logger.Error("failed to get file info", "file_id", fileID, "error", err)
		http.Error(w, `{"error":"file not found"}`, http.StatusNotFound)
		return
	}

	if file.Size > maxFileProxySize {
		http.Error(w, `{"error":"file too large"}`, http.StatusRequestEntityTooLarge)
		return
	}

	downloadURL := file.URLPrivateDownload
	if downloadURL == "" {
		downloadURL = file.URLPrivate
	}
	if downloadURL == "" {
		http.Error(w, `{"error":"file has no download URL"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", file.Mimetype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, file.Name))
	if file.Size > 0 {
		w.Header().Set("Content-Length", strconv.Itoa(file.Size))
	}

	// GetFileContext handles Bearer auth internally.
	if err := a.api.GetFileContext(r.Context(), downloadURL, w); err != nil {
		a.logger.Error("failed to download file", "file_id", fileID, "error", err)
		// Headers already sent; nothing we can do for the client.
		return
	}
}

// replyRequest is the request body for POST /api/slack/threads/reply.
type replyRequest struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Text     string `json:"text"`
}

// replyResponse is the response body for POST /api/slack/threads/reply.
//
// Deduped is true when the reply was collapsed onto an identical reply posted
// (or in flight) within threadReplyDedupWindow — nothing new was posted to
// Slack. Timestamp then carries the earlier post's ts when known, or "" if
// the winning post had not yet completed when this request was collapsed.
type replyResponse struct {
	OK        bool   `json:"ok"`
	Timestamp string `json:"timestamp"`
	Deduped   bool   `json:"deduped,omitempty"`
}

// handlePostReply handles POST /api/slack/threads/reply.
//
// Identical (channel, thread_ts, text) replies inside threadReplyDedupWindow
// are collapsed to a single Slack post (ka-0000002080): during pod churn, N
// concurrent teardown incarnations of the same thread agent each post the same
// "interrupted" breadcrumb within a second — the controller-side bead-field
// marker is a cross-process read-modify-write that cannot close that race, so
// the bridge (the single process every reply funnels through) is where the
// dedup actually holds. Collapsed requests still return 200/ok so best-effort
// callers treat the reply as delivered — it was, just once.
func (a *SlackThreadAPI) handlePostReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	var req replyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	if req.Channel == "" || req.ThreadTS == "" || req.Text == "" {
		http.Error(w, `{"error":"channel, thread_ts, and text are required"}`, http.StatusBadRequest)
		return
	}

	key, dup, prevTS := a.dedup.begin(req.Channel, req.ThreadTS, req.Text)
	if dup {
		a.logger.Info("collapsed duplicate thread reply",
			"channel", req.Channel, "thread_ts", req.ThreadTS, "prev_ts", prevTS)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(replyResponse{OK: true, Timestamp: prevTS, Deduped: true})
		return
	}

	_, ts, err := a.api.PostMessage(req.Channel,
		slack.MsgOptionText(req.Text, false),
		slack.MsgOptionTS(req.ThreadTS),
	)
	if err != nil {
		// Release the reservation so a legitimate retry of this reply is not
		// swallowed by a slot that never produced a Slack message.
		a.dedup.abort(key)
		a.logger.Error("failed to post thread reply",
			"channel", req.Channel, "thread_ts", req.ThreadTS, "error", err)
		http.Error(w, `{"error":"failed to post reply"}`, http.StatusInternalServerError)
		return
	}
	a.dedup.commit(key, ts)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(replyResponse{OK: true, Timestamp: ts})
}

// maxUploadSize bounds the multipart body the upload endpoint will accept
// (50MB), matching the file-proxy ceiling. Slack itself caps uploads at 1GB,
// but agent screenshots/diagrams are far smaller and a tight bound keeps the
// bridge's memory footprint predictable.
const maxUploadSize = 50 * 1024 * 1024

// uploadResponse is the response body for POST /api/slack/threads/upload.
//
// completeUploadExternal returns only the file id and title (no permalink),
// so those are all we surface. The agent does not need the permalink — the
// file is already shared into the thread.
type uploadResponse struct {
	OK     bool   `json:"ok"`
	FileID string `json:"file_id"`
	Title  string `json:"title,omitempty"`
}

// handlePostUpload handles POST /api/slack/threads/upload.
//
// It accepts a multipart/form-data body with:
//   - file            (required) — the binary file part
//   - channel         (required) — Slack channel ID
//   - thread_ts       (required) — parent message ts; the upload is shared
//     into that thread so it renders inline rather than as a bare link
//   - initial_comment (optional) — message text posted alongside the file
//   - title           (optional) — file title shown in Slack (defaults to filename)
//   - alt_txt         (optional) — accessibility alt text for images
//
// The upload goes through slack-go's UploadFileContext, which performs the
// modern files.upload v2 flow (getUploadURLExternal → POST bytes →
// completeUploadExternal). This requires the bot to hold the `files:write`
// OAuth scope; without it Slack returns missing_scope and this handler
// surfaces a 502 with the underlying error.
func (a *SlackThreadAPI) handlePostUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		a.logger.Error("failed to parse upload multipart form", "error", err)
		http.Error(w, `{"error":"invalid multipart form (or file too large)"}`, http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	channel := r.FormValue("channel")
	threadTS := r.FormValue("thread_ts")
	if channel == "" || threadTS == "" {
		http.Error(w, `{"error":"channel and thread_ts are required"}`, http.StatusBadRequest)
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		http.Error(w, `{"error":"file form part is required"}`, http.StatusBadRequest)
		return
	}
	defer file.Close()

	filename := header.Filename
	if filename == "" {
		filename = "upload"
	}
	if header.Size <= 0 {
		http.Error(w, `{"error":"file is empty"}`, http.StatusBadRequest)
		return
	}

	title := r.FormValue("title")
	if title == "" {
		title = filename
	}

	// UploadFileContext runs the files.upload v2 flow internally
	// (getUploadURLExternal → POST bytes → completeUploadExternal) and shares
	// the file into the thread via ThreadTimestamp.
	summary, err := a.api.UploadFileContext(r.Context(), slack.UploadFileParameters{
		Reader:          file,
		Filename:        filename,
		FileSize:        int(header.Size),
		Title:           title,
		Channel:         channel,
		ThreadTimestamp: threadTS,
		InitialComment:  r.FormValue("initial_comment"),
		AltTxt:          r.FormValue("alt_txt"),
	})
	if err != nil {
		a.logger.Error("failed to upload file to thread",
			"channel", channel, "thread_ts", threadTS, "filename", filename, "error", err)
		http.Error(w, fmt.Sprintf(`{"error":"failed to upload file: %s"}`, jsonEscape(err.Error())), http.StatusBadGateway)
		return
	}

	resp := uploadResponse{OK: true, Title: summary.Title}
	if summary.ID != "" {
		resp.FileID = summary.ID
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// jsonEscape escapes a string for safe inclusion inside a JSON string literal
// in an error response. It slices off exactly the surrounding quotes that
// json.Marshal adds — strings.Trim would also strip a trailing escaped quote
// (`\"`), corrupting values that end in a double-quote.
func jsonEscape(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return ""
	}
	if len(b) >= 2 {
		return string(b[1 : len(b)-1])
	}
	return string(b)
}
