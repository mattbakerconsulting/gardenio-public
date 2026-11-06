package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// ConversationAgentAPI serves the agent verbs of the conversation core (design
// spec 2026-10-19-lands-commons-design.md, section 4, slice 5):
//
//   - POST /api/conversation/say: gb say --thread / --channel / --file, and
//     the gb slack reply and gb slack upload aliases;
//   - GET /api/conversation/thread: gb thread and the gb slack thread alias.
//
// An agent names a thread or a channel, never a surface. The Router writes
// the message bead (with channel_id, thread_id and the land's project label)
// and projects it to every surface bound to the channel. The /api/slack/threads
// routes stay for gb binaries that predate these, and share the reply deduper,
// so the two paths collapse the same reply onto one post. Their thread read is
// land-walled here (legacyReadWall).
//
// The routes are agent-facing: bridgeRBAC admits an agent's per-agent daemon
// token, the shared agent bearer (as for /api/slack/*) or an OIDC admin, and
// the agent a say or read is made as comes from the caller
// (api_conversation_caller.go): a token's agent is the token's, never the
// request's.
type ConversationAgentAPI struct {
	bot     *Bot
	threads *SlackThreadAPI
	logger  *slog.Logger
	// refuseSharedBearer refuses the shared bridge bearer on these routes
	// (SetSharedBearerAgents(false)).
	refuseSharedBearer bool
}

// NewConversationAgentAPI returns the agent API over bot. threads is the
// legacy thread API registered beside it: its deduper and Slack history
// reader are shared, and its thread read is walled by legacyReadWall.
func NewConversationAgentAPI(bot *Bot, threads *SlackThreadAPI, logger *slog.Logger) *ConversationAgentAPI {
	if logger == nil {
		logger = slog.Default()
	}
	a := &ConversationAgentAPI{bot: bot, threads: threads, logger: logger}
	if threads != nil {
		threads.readWall = a.legacyReadWall
	}
	return a
}

// RegisterRoutes registers the say and thread routes on mux.
func (a *ConversationAgentAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/conversation/say", a.handleSay)
	mux.HandleFunc("GET /api/conversation/thread", a.handleThread)
}

// maxSayBody bounds a JSON say body. Text-only: files come as multipart,
// bounded by maxUploadSize.
const maxSayBody = 1 << 20

// sayRequest is the body of POST /api/conversation/say: a JSON object, or a
// multipart form with the same names plus one or more "file" parts (and
// "title" / "alt_txt" for the first file).
//
// The target is one of:
//   - Thread: a thread head bead id (a reply in that thread);
//   - Channel + ThreadTS: a Slack thread by its coordinates, which is how gb
//     names the agent's home thread (resolved to its head by the
//     slack-thread:<channel>:<ts> label);
//   - Channel alone: a new top-level post in a channel of the agent's land,
//     named by channel bead id, name (#general) or bound Slack channel id;
//   - To: a DM to a person or an agent (api_conversation_dm.go), with Ask
//     for a question whose reply is captured and nudges the agent.
type sayRequest struct {
	// Agent is the speaking agent. Under a per-agent token it is the token's
	// agent, and may only repeat it.
	Agent     string `json:"agent"`
	AgentBead string `json:"agent_bead,omitempty"` // its agent bead, for triggered-by lineage
	// TriggerBead is the trigger that spawned the agent, for triggered-by
	// lineage too (gb's BOAT_TRIGGER_BEAD_ID).
	TriggerBead string `json:"trigger_bead,omitempty"`
	// gb also sends the agent's land ("land"). The bridge ignores it: the
	// land wall reads the land from its own or the daemon's record of the
	// agent, never from the request.
	Thread   string `json:"thread,omitempty"`
	Channel  string `json:"channel,omitempty"`
	ThreadTS string `json:"thread_ts,omitempty"`
	Text     string `json:"text"`
	Title    string `json:"title,omitempty"`   // multipart only: the first file's title
	AltText  string `json:"alt_txt,omitempty"` // multipart only: the first file's alt text
	// To is a DM's target (gb say --to): agent:<name>, an email, a Slack
	// user id, or a GitLab or GitHub username. Ask makes it a question, and
	// Task is the task it is asked about (shown in the Slack IM).
	To   string `json:"to,omitempty"`
	Ask  bool   `json:"ask,omitempty"`
	Task string `json:"task,omitempty"`
	// Wake, with Thread, also gives the words to the thread's agent
	// (wakeThreadAgent): gb say --thread <head> --wake.
	Wake bool `json:"wake,omitempty"`
}

// sayResponse is the outcome of a say. Timestamp is the Slack copy's ts ("" for
// a file share, whose message the upload API does not return, and for a
// channel with no Slack copy). Deduped is true when the say collapsed onto an
// identical reply posted inside threadReplyDedupWindow; nothing new was
// written or posted, and Timestamp is the earlier post's ts when known.
type sayResponse struct {
	OK        bool   `json:"ok"`
	ID        string `json:"id,omitempty"`
	Thread    string `json:"thread,omitempty"`
	Channel   string `json:"channel,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Deduped   bool   `json:"deduped,omitempty"`
	// DM is the DM channel bead a say --to was written in.
	DM string `json:"dm,omitempty"`
	// Woke is what a wake did with the thread's agent ("Told x.", "Waking x
	// with this thread's conversation."); "" when the say asked for none.
	Woke string `json:"woke,omitempty"`
}

// sayTarget is where a say goes: a channel, and the thread in it for a reply.
type sayTarget struct {
	ch conversation.ChannelRef
	// thread is nil for a top-level post. Its Refs hold the Slack root when
	// the thread has a Slack copy.
	thread *conversation.Thread
	// named is true when the agent named the target by bead (a head id or a
	// channel); land isolation applies to it (section 6.2 rule 4).
	named bool
}

// slackRoot returns the thread's Slack root.
func (t sayTarget) slackRoot() (conversation.Ref, bool) {
	if t.thread == nil {
		return conversation.Ref{}, false
	}
	return t.thread.On(conversation.SurfaceSlack, t.ch)
}

// sayError is a refusal with its HTTP status.
type sayError struct {
	status int
	msg    string
}

func (e *sayError) Error() string { return e.msg }

func sayErrorf(status int, format string, args ...any) error {
	return &sayError{status: status, msg: fmt.Sprintf(format, args...)}
}

// writeSayError answers err as JSON; refusals keep their status, anything
// else is a 502 (the daemon or Slack failed).
func writeSayError(w http.ResponseWriter, err error) {
	var se *sayError
	if errors.As(err, &se) {
		writeInboundError(w, se.status, se.msg)
		return
	}
	writeInboundError(w, http.StatusBadGateway, err.Error())
}

func (a *ConversationAgentAPI) handleSay(w http.ResponseWriter, r *http.Request) {
	req, files, err := a.readSay(w, r)
	if err != nil {
		writeSayError(w, err)
		return
	}
	// The text goes out as written (no trim), exactly as the reply and upload
	// routes sent it.
	if strings.TrimSpace(req.Text) == "" && len(files) == 0 {
		writeInboundError(w, http.StatusBadRequest, "text or a file is required")
		return
	}
	if a.bot == nil {
		writeInboundError(w, http.StatusServiceUnavailable, "bridge has no Slack bot")
		return
	}

	// The post finishes even if gb hangs up: a reply cut off between the bead
	// and the Slack post would be retried as a duplicate.
	ctx := context.WithoutCancel(r.Context())
	sp, shared, err := a.callerSpeaker(ctx, req.Agent)
	if err != nil {
		writeSayError(w, err)
		return
	}
	if req.To != "" || req.Ask {
		if req.Wake {
			writeInboundError(w, http.StatusBadRequest, "wake needs thread, not to: it wakes a thread's agent")
			return
		}
		if req.To == "" {
			writeInboundError(w, http.StatusBadRequest, "ask needs to: the person or agent asked")
			return
		}
		a.sayTo(ctx, w, req, files, sp)
		return
	}
	agent := sp.agent
	t, err := a.resolveTarget(ctx, req.Thread, req.Channel, req.ThreadTS, &sp)
	if err == nil {
		err = a.fenceTarget(ctx, &sp, shared, t, req.Thread, req.Channel, req.ThreadTS)
	}
	if err == nil && req.Wake {
		err = refuseWake(req, t)
	}
	if err != nil {
		writeSayError(w, err)
		return
	}
	if a.bot.daemon == nil {
		// Nowhere to write the bead: post to Slack directly (routeSay).
		t.ch.Land = ""
	}

	resp := sayResponse{OK: true, Channel: t.ch.Slack}
	if t.thread != nil {
		resp.Thread = t.thread.Head
	}
	root, inSlackThread := t.slackRoot()

	// The same (channel, thread, text) reply inside the window posts once,
	// whichever API it came through (ka-0000002080).
	var dedupKey string
	if inSlackThread && len(files) == 0 && a.threads != nil {
		key, dup, prevTS := a.threads.dedup.begin(root.Channel, root.TS, req.Text)
		if dup {
			a.logger.Info("collapsed duplicate say",
				"channel", root.Channel, "thread_ts", root.TS, "prev_ts", prevTS, "agent", agent)
			resp.Timestamp, resp.Deduped = prevTS, true
			writeSayJSON(w, resp)
			return
		}
		dedupKey = key
	}

	m := conversation.Message{
		Text:   req.Text,
		Author: agentPrincipal(agent),
		Files:  files,
		Origin: "cli",
	}
	d, err := routeSay(ctx, a.bot.Conversation(), a.logger, t, m)
	if err == nil {
		if ferr := d.Failed[conversation.SurfaceSlack]; ferr != nil {
			err = ferr
		}
	}
	if err != nil {
		if dedupKey != "" {
			a.threads.dedup.abort(dedupKey)
		}
		a.logger.Error("conversation say failed",
			"agent", agent, "thread", resp.Thread, "channel", t.ch.Slack, "bead", d.BeadID, "error", err)
		if errors.Is(err, conversation.ErrEmptyMessage) || errors.Is(err, conversation.ErrUnsupported) {
			writeInboundError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeInboundError(w, http.StatusBadGateway, "failed to post: "+err.Error())
		return
	}
	resp.ID = d.BeadID
	if ref, ok := d.Ref(conversation.SurfaceSlack); ok {
		resp.Channel, resp.Timestamp = ref.Channel, ref.TS
	}
	if dedupKey != "" {
		a.threads.dedup.commit(dedupKey, resp.Timestamp)
	}
	a.stampLineage(ctx, d.BeadID, agent, req.AgentBead, req.TriggerBead)
	if req.Wake {
		// The words are posted: a failed wake is reported, not a failed say.
		woke, err := a.wakeThreadAgent(ctx, t, agent, req.Text, d.BeadID)
		if err != nil {
			a.logger.Warn("conversation say: wake failed", "agent", agent, "thread", resp.Thread, "error", err)
			woke = "The wake failed: " + err.Error()
		}
		resp.Woke = woke
	}
	writeSayJSON(w, resp)
}

func writeSayJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// readSay decodes a JSON or multipart say body.
func (a *ConversationAgentAPI) readSay(w http.ResponseWriter, r *http.Request) (sayRequest, []conversation.File, error) {
	var req sayRequest
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSayBody)).Decode(&req); err != nil {
			return req, nil, sayErrorf(http.StatusBadRequest, "invalid JSON body")
		}
		return req, nil, nil
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxUploadSize)
	if err := r.ParseMultipartForm(maxUploadSize); err != nil {
		return req, nil, sayErrorf(http.StatusBadRequest, "invalid multipart form (or file too large)")
	}
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	req = sayRequest{
		Agent:       r.FormValue("agent"),
		AgentBead:   r.FormValue("agent_bead"),
		TriggerBead: r.FormValue("trigger_bead"),
		Thread:      r.FormValue("thread"),
		Channel:     r.FormValue("channel"),
		ThreadTS:    r.FormValue("thread_ts"),
		Text:        r.FormValue("text"),
		Title:       r.FormValue("title"),
		AltText:     r.FormValue("alt_txt"),
		To:          r.FormValue("to"),
		Ask:         r.FormValue("ask") == "true",
		Task:        r.FormValue("task"),
		Wake:        r.FormValue("wake") == "true",
	}
	var files []conversation.File
	for i, fh := range r.MultipartForm.File["file"] {
		if fh.Size <= 0 {
			return req, nil, sayErrorf(http.StatusBadRequest, "file %q is empty", fh.Filename)
		}
		f, err := fh.Open()
		if err != nil {
			return req, nil, sayErrorf(http.StatusBadRequest, "reading file %q", fh.Filename)
		}
		data, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return req, nil, sayErrorf(http.StatusBadRequest, "reading file %q", fh.Filename)
		}
		file := conversation.File{
			Name:     fh.Filename,
			MimeType: fh.Header.Get("Content-Type"),
			Data:     data,
		}
		if file.Name == "" {
			file.Name = "upload"
		}
		if i == 0 {
			file.Title, file.AltText = req.Title, req.AltText
		}
		files = append(files, file)
	}
	return req, files, nil
}

// routeSay writes m as a message bead in t's channel and projects it, as a
// reply when t is a thread. Degrade, never drop: with no land (no daemon, or
// a Slack channel no land claims) or a failed bead write, a target with a
// Slack copy still gets the post, straight through the Slack surface, and the
// delivery has no bead.
func routeSay(ctx context.Context, r *conversation.Router, logger *slog.Logger, t sayTarget, m conversation.Message) (conversation.Delivery, error) {
	if t.ch.Land != "" {
		var d conversation.Delivery
		var err error
		if t.thread != nil {
			d, err = r.Reply(ctx, t.ch, *t.thread, m, false)
		} else {
			d, err = r.Post(ctx, t.ch, m)
		}
		if err == nil || errors.Is(err, conversation.ErrEmptyMessage) {
			return d, err
		}
		logger.Warn("conversation say: message bead write failed, posting to Slack directly",
			"land", t.ch.Land, "channel", t.ch.Slack, "error", err)
	}
	s, ok := r.Surface(conversation.SurfaceSlack)
	if !ok {
		return conversation.Delivery{}, fmt.Errorf("conversation: no slack surface: %w", conversation.ErrUnsupported)
	}
	var ref conversation.Ref
	var err error
	if root, ok := t.slackRoot(); ok {
		ref, err = s.Reply(ctx, root, m, false)
	} else if t.thread == nil && t.ch.Slack != "" {
		ref, err = s.Post(ctx, t.ch, m)
	} else {
		return conversation.Delivery{}, fmt.Errorf("conversation say: nowhere to post without a land: %w", conversation.ErrNoChannel)
	}
	if err != nil {
		return conversation.Delivery{}, err
	}
	return conversation.Delivery{Refs: []conversation.Ref{ref}}, nil
}

// stampLineage links the say's bead to the speaking agent's bead and the
// trigger that spawned it with triggered-by deps, as gb does for the message
// beads it writes itself (writeReplyLineage). Best-effort.
func (a *ConversationAgentAPI) stampLineage(ctx context.Context, beadID, agent string, parents ...string) {
	if beadID == "" || a.bot.daemon == nil {
		return
	}
	seen := map[string]bool{}
	for _, parent := range parents {
		if parent == "" || seen[parent] {
			continue
		}
		seen[parent] = true
		if err := a.bot.daemon.AddDependency(ctx, beadID, parent, "triggered-by", agent); err != nil {
			a.logger.Warn("conversation say: triggered-by lineage failed",
				"bead", beadID, "parent", parent, "error", err)
		}
	}
}

// speaker is the agent a say speaks as (or a read reads as), for the land
// wall: its land and its Slack thread binding as the bridge or the daemon
// records them. land is "" when neither knows it.
type speaker struct {
	agent              string
	land               string
	threadCh, threadTS string // the agent bead's binding, when it was read
	// beadRead is true once the agent bead has been read (or, for a token's
	// agent, looked up by its id), so threadCh is its binding.
	beadRead bool
	// read is true for a thread read, which the wall checks by Slack
	// coordinates too (see resolveTarget).
	read bool
	// fromToken is true for the agent a per-agent token names.
	fromToken bool
	// coordsWalled is true for a token's agent of a tenant land: its says
	// by Slack coordinates are walled as reads are (see resolveTarget).
	coordsWalled bool
}

// walledByCoords reports whether the wall checks a target the speaker names by
// Slack coordinates, not only one it names by bead.
func (sp *speaker) walledByCoords() bool {
	return sp.read || sp.coordsWalled
}

// verb is what the speaker does, for a refusal.
func (sp *speaker) verb() string {
	if sp.read {
		return "read"
	}
	return "speak"
}

// speakerOf looks up the speaking agent: the project the bridge has cached
// for it, else its agent bead in the daemon. The land gb reports is never
// used: an agent the bridge cannot place could name any land it likes.
func (a *ConversationAgentAPI) speakerOf(ctx context.Context, agent string) speaker {
	sp := speaker{agent: agent, land: a.bot.landOf(nil, agent)}
	if sp.land != "" || a.bot.daemon == nil {
		return sp
	}
	b, err := a.bot.daemon.FindAgentBead(ctx, agent)
	if err != nil || b == nil {
		if err != nil && !errors.Is(err, beadsapi.ErrAgentNotFound) {
			a.logger.Warn("conversation land wall: reading agent bead failed", "agent", agent, "error", err)
		}
		return sp
	}
	sp.land = projectFromLabels(b.Labels)
	if sp.land == "" {
		sp.land = strings.TrimSpace(b.Fields["project"])
	}
	sp.threadCh, sp.threadTS = b.Fields["slack_thread_channel"], b.Fields["slack_thread_ts"]
	sp.beadRead = true
	return sp
}

// boundTo reports whether the speaker is bound to the Slack thread: the
// bridge's thread-agent binding names it, or its agent bead does.
func (a *ConversationAgentAPI) boundTo(sp *speaker, channel, threadTS string) bool {
	if a.bot.state != nil {
		if bound, ok := a.bot.state.GetThreadAgent(channel, threadTS); ok && extractAgentName(bound) == sp.agent {
			return true
		}
	}
	return sp.threadCh == channel && sp.threadTS == threadTS
}

// readsOwnThread reports whether a reader is bound to the Slack thread. A
// reader the bridge placed from its cache has not had its agent bead read:
// the bead's binding is read here, as speakerOf would have.
func (a *ConversationAgentAPI) readsOwnThread(ctx context.Context, sp *speaker, channel, threadTS string) bool {
	if a.boundTo(sp, channel, threadTS) {
		return true
	}
	if sp.beadRead || a.bot.daemon == nil {
		return false
	}
	b, err := a.bot.daemon.FindAgentBead(ctx, sp.agent)
	if err != nil || b == nil {
		return false
	}
	sp.threadCh, sp.threadTS = b.Fields["slack_thread_channel"], b.Fields["slack_thread_ts"]
	sp.beadRead = true
	return sp.threadCh == channel && sp.threadTS == threadTS
}

// slackChannelIDRe matches a Slack conversation id (C…, G…, D…).
var slackChannelIDRe = regexp.MustCompile(`^[CGD][A-Z0-9]{6,}$`)

// resolveTarget resolves a say or read target (see sayRequest) for sp, the
// speaking or reading agent.
//
// A target named by bead must be in the speaker's land (section 6.2 rule 4).
// A say by Slack coordinates is exempt, because they are how gb names the
// agent's own thread and an agent is bound to threads in other lands' channels
// today (a mention with --project). A read by Slack coordinates is not: it
// reaches a thread outside the reader's land (another land's, or one in a
// Slack channel no land claims) only when it is the reader's own, and so does a
// say by a tenant land's token agent (coordsWalled). The wall fails closed: a speaker whose land is unknown names no target by bead, and
// reaches by Slack coordinates only the thread it is bound to.
func (a *ConversationAgentAPI) resolveTarget(ctx context.Context, headID, channel, threadTS string, sp *speaker) (sayTarget, error) {
	headID, channel, threadTS = strings.TrimSpace(headID), strings.TrimSpace(channel), strings.TrimSpace(threadTS)
	var agentLand string
	if sp != nil {
		agentLand = sp.land
		if agentLand == "" && (headID != "" || threadTS == "" && channel != "") {
			name := "channel " + channel
			if headID != "" {
				name = "thread " + headID
			}
			return sayTarget{}, sayErrorf(http.StatusForbidden,
				"the bridge has no record of agent %s's land; it may not %s %s", sp.agent, sp.verb(), name)
		}
	}
	var t sayTarget
	var err error
	switch {
	case headID != "":
		t, err = a.threadTarget(ctx, headID, true)
		t.named = true
	case channel != "" && threadTS != "":
		if sp != nil && agentLand == "" && !a.boundTo(sp, channel, threadTS) {
			return sayTarget{}, sayErrorf(http.StatusForbidden,
				"the bridge has no record of agent %s's land; it may %s only in its own thread", sp.agent, sp.verb())
		}
		// A say is placed in the agent's land when no land claims the
		// channel; a read (or a walled say) is placed by its target alone.
		fallback := agentLand
		if sp != nil && sp.walledByCoords() {
			fallback = ""
		}
		t = a.slackThreadTarget(ctx, channel, threadTS, fallback)
	case channel != "":
		t, err = a.channelTarget(ctx, channel, agentLand)
		t.named = true
	default:
		return sayTarget{}, sayErrorf(http.StatusBadRequest, "thread, or channel (with thread_ts for a Slack thread), is required")
	}
	if err != nil {
		return sayTarget{}, err
	}
	if sp != nil && t.ch.Land != agentLand &&
		(t.named && t.ch.Land != "" || !t.named && sp.walledByCoords() && !a.readsOwnThread(ctx, sp, channel, threadTS)) {
		return sayTarget{}, sayErrorf(http.StatusForbidden,
			"%s is in land %q; an agent of %q may %s only in its own land", targetName(t, headID, channel, threadTS), t.ch.Land, agentLand, sp.verb())
	}
	return t, nil
}

func targetName(t sayTarget, headID, channel, threadTS string) string {
	switch {
	case t.thread != nil && headID != "":
		return "thread " + headID
	case t.thread != nil:
		return "Slack thread " + channel + "/" + threadTS
	}
	return "channel " + channel
}

// threadTarget resolves a thread by its head bead: its channel from the
// head's channel_id (else the channel bead bound to its Slack channel), its
// Slack root from slack_thread_channel/slack_thread_ts, and its land from the
// channel, else the head's own project label.
//
// A message id names its thread too: follow is true on the first call, and a
// bead with a thread_id resolves to that head (once; a head has none).
func (a *ConversationAgentAPI) threadTarget(ctx context.Context, headID string, follow bool) (sayTarget, error) {
	if a.bot.daemon == nil {
		return sayTarget{}, sayErrorf(http.StatusServiceUnavailable, "bridge has no daemon")
	}
	head, err := a.bot.daemon.GetBead(ctx, headID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return sayTarget{}, sayErrorf(http.StatusNotFound, "thread %s not found", headID)
		}
		return sayTarget{}, fmt.Errorf("reading thread %s: %w", headID, err)
	}
	if head == nil {
		return sayTarget{}, sayErrorf(http.StatusNotFound, "thread %s not found", headID)
	}
	if parent := head.Fields[conversation.BeadThreadID]; follow && parent != "" && parent != head.ID {
		return a.threadTarget(ctx, parent, false)
	}
	th := &conversation.Thread{Head: head.ID}
	var ch conversation.ChannelRef
	root, hasRoot := slackThreadRoot(head.Fields)
	if hasRoot {
		th.Refs = []conversation.Ref{root}
		ch.Slack = root.Channel
	}
	if id := head.Fields[conversation.BeadChannelID]; id != "" {
		if cb, err := a.channelBead(ctx, id); err == nil {
			// The thread's Slack copy, not the channel's binding, decides
			// whether a reply reaches Slack: a thread started in the Commons
			// has no Slack root to reply under until two-way mirroring
			// (Commons II).
			ch = cb
			ch.Slack = root.Channel
		}
	} else if hasRoot {
		if ref, ok := a.bot.resolveChannelBead(ctx, root.Channel); ok {
			ch.ID, ch.Land, ch.Mirror = ref.ID, ref.Land, ref.Mirror
		}
	}
	if ch.Land == "" {
		ch.Land = projectFromLabels(head.Labels)
	}
	if ch.ID == "" && ch.Slack == "" {
		return sayTarget{}, sayErrorf(http.StatusUnprocessableEntity, "thread %s is in no channel", headID)
	}
	return sayTarget{ch: ch, thread: th}, nil
}

// slackThreadTarget resolves a Slack thread by its coordinates: the channel
// bead bound to the Slack channel, the head minted for the thread (if any),
// and the land from the channel, else the head, else the agent.
func (a *ConversationAgentAPI) slackThreadTarget(ctx context.Context, channel, threadTS, agentLand string) sayTarget {
	root := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: threadTS}
	t := sayTarget{
		ch:     conversation.ChannelRef{Slack: channel},
		thread: &conversation.Thread{Refs: []conversation.Ref{root}},
	}
	if ref, ok := a.bot.resolveChannelBead(ctx, channel); ok {
		t.ch.ID, t.ch.Land, t.ch.Mirror = ref.ID, ref.Land, ref.Mirror
	}
	if head := a.bot.slackThreadHead(ctx, channel, threadTS); head != nil {
		t.thread.Head = head.ID
		if t.ch.Land == "" {
			t.ch.Land = projectFromLabels(head.Labels)
		}
	}
	if t.ch.Land == "" {
		t.ch.Land = agentLand
	}
	return t
}

// channelTarget resolves a channel for a top-level post: a Slack channel id
// bound to a channel bead, a channel bead id, or a channel name in the agent's
// land.
func (a *ConversationAgentAPI) channelTarget(ctx context.Context, channel, agentLand string) (sayTarget, error) {
	if a.bot.daemon == nil {
		return sayTarget{}, sayErrorf(http.StatusServiceUnavailable, "bridge has no daemon")
	}
	var id string
	switch name := strings.TrimPrefix(channel, "#"); {
	case slackChannelIDRe.MatchString(channel):
		ref, ok := a.bot.resolveChannelBead(ctx, channel)
		if !ok {
			return sayTarget{}, sayErrorf(http.StatusNotFound, "no channel is bound to Slack channel %s", channel)
		}
		id = ref.ID
	case agentLand != "":
		res, err := a.bot.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{"channel"},
			Labels: []string{"project:" + agentLand},
			Fields: map[string]string{"name": name},
			Limit:  1,
		})
		if err != nil {
			return sayTarget{}, fmt.Errorf("finding channel %s: %w", channel, err)
		}
		if res != nil && len(res.Beads) > 0 {
			id = res.Beads[0].ID
		}
	}
	if id == "" {
		id = channel
	}
	ch, err := a.channelBead(ctx, id)
	if err != nil {
		return sayTarget{}, err
	}
	return sayTarget{ch: ch}, nil
}

// channelBead reads a channel bead into a ChannelRef. An archived (closed)
// channel takes no posts.
func (a *ConversationAgentAPI) channelBead(ctx context.Context, id string) (conversation.ChannelRef, error) {
	b, err := a.bot.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return conversation.ChannelRef{}, sayErrorf(http.StatusNotFound, "channel %s not found", id)
		}
		return conversation.ChannelRef{}, fmt.Errorf("reading channel %s: %w", id, err)
	}
	if b == nil || b.Type != "channel" {
		return conversation.ChannelRef{}, sayErrorf(http.StatusNotFound, "channel %s not found", id)
	}
	if b.Status == "closed" {
		return conversation.ChannelRef{}, sayErrorf(http.StatusUnprocessableEntity, "channel %s is archived", id)
	}
	lands := labelLands(b.Labels)
	if len(lands) != 1 {
		return conversation.ChannelRef{}, sayErrorf(http.StatusUnprocessableEntity, "channel %s has no single land", id)
	}
	return conversation.ChannelRef{
		ID:     b.ID,
		Land:   lands[0],
		Slack:  b.Fields["slack_channel_id"],
		Mirror: b.Fields["mirror"],
	}, nil
}

// slackThreadHead returns the head bead minted for a Slack thread, or nil.
func (b *Bot) slackThreadHead(ctx context.Context, channel, threadTS string) *beadsapi.BeadDetail {
	if b.daemon == nil {
		return nil
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{threadBeadLabel(channel, threadTS)},
		Limit:  1,
	})
	if err != nil || res == nil || len(res.Beads) == 0 {
		return nil
	}
	return res.Beads[0]
}

// Thread read sources.
const (
	threadSourceAuto  = "auto"
	threadSourceBeads = "beads"
	threadSourceSlack = "slack"
)

// threadResponse is the body of GET /api/conversation/thread: the thread's
// messages, root first, and where they were read from.
type threadResponse struct {
	Thread   string          `json:"thread,omitempty"`
	Source   string          `json:"source"`
	Messages []ThreadMessage `json:"messages"`
}

// handleThread handles GET /api/conversation/thread?agent=<name>&thread=<head>
// (or channel=<C>&ts=<ts>) [&limit=<n>] [&source=auto|beads|slack]. agent is
// the reading agent, for the land wall (resolveTarget).
//
// A thread with a Slack copy reads Slack by default: it is the complete
// history while Slack is mirrored, since a message said there before the land
// was seeded has no bead. A thread with no Slack copy reads its message beads
// (fields.thread_id == head), with the head as the root. source=beads reads the
// beads of a Slack thread too.
func (a *ConversationAgentAPI) handleThread(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := 50
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		limit = n
	}
	source := q.Get("source")
	if source == "" {
		source = threadSourceAuto
	}
	if source != threadSourceAuto && source != threadSourceBeads && source != threadSourceSlack {
		writeInboundError(w, http.StatusBadRequest, "source must be auto, beads or slack")
		return
	}
	if a.bot == nil {
		writeInboundError(w, http.StatusServiceUnavailable, "bridge has no Slack bot")
		return
	}
	ctx := r.Context()
	t, err := a.readWall(ctx, q.Get("agent"), q.Get("thread"), q.Get("channel"), q.Get("ts"))
	if err != nil {
		writeSayError(w, err)
		return
	}
	if t.thread == nil {
		writeInboundError(w, http.StatusBadRequest, "thread, or channel and ts, is required")
		return
	}
	root, inSlack := t.slackRoot()
	if source == threadSourceAuto {
		source = threadSourceBeads
		if inSlack {
			source = threadSourceSlack
		}
	}

	resp := threadResponse{Thread: t.thread.Head, Source: source}
	switch source {
	case threadSourceSlack:
		if !inSlack || a.threads == nil {
			writeInboundError(w, http.StatusUnprocessableEntity, "thread has no Slack copy")
			return
		}
		resp.Messages, err = a.threads.fetchThread(ctx, root.Channel, root.TS, limit)
		if err != nil {
			writeInboundError(w, http.StatusBadGateway, "failed to fetch thread messages")
			return
		}
	default:
		if t.thread.Head == "" {
			writeInboundError(w, http.StatusUnprocessableEntity, "thread has no head bead")
			return
		}
		resp.Messages, err = a.threadBeads(ctx, t.thread.Head, limit)
		if err != nil {
			writeInboundError(w, http.StatusBadGateway, "failed to read thread messages")
			return
		}
	}
	if resp.Messages == nil {
		resp.Messages = []ThreadMessage{}
	}
	writeSayJSON(w, resp)
}

// readWall resolves a read target for the reading agent: the caller's token
// agent, else named (the request's ?agent=). Reads are land-checked as says
// are, and by Slack coordinates too (resolveTarget), before either Slack or
// the beads are read: an agent reads only its own land's threads, and its own
// thread wherever it is. GET /api/conversation/thread and the legacy GET
// /api/slack/threads both read through it.
func (a *ConversationAgentAPI) readWall(ctx context.Context, named, headID, channel, threadTS string) (sayTarget, error) {
	rd, shared, err := a.callerSpeaker(ctx, named)
	if err != nil {
		return sayTarget{}, err
	}
	rd.read = true
	t, err := a.resolveTarget(ctx, headID, channel, threadTS, &rd)
	if err != nil {
		return sayTarget{}, err
	}
	if err := a.fenceTarget(ctx, &rd, shared, t, headID, channel, threadTS); err != nil {
		return sayTarget{}, err
	}
	return t, nil
}

// legacyReadWall walls GET /api/slack/threads?channel=&ts=[&agent=]
// (gd-3uBuj2h7Ar), the alias gb binaries that predate GET
// /api/conversation/thread still read through. With a reader (a per-agent
// token's agent, or ?agent=) it is readWall. A gb that predates ?agent= names
// no reader, so the thread is placed by its target alone and read only when it
// is in an operator land: a thread in a tenant land, in a Slack channel no land
// claims, or in a land the bridge cannot look up is refused. The shared bearer
// reads nothing once it is retired (SetSharedBearerAgents(false)).
func (a *ConversationAgentAPI) legacyReadWall(ctx context.Context, agent, channel, threadTS string) error {
	if a.bot == nil {
		return sayErrorf(http.StatusServiceUnavailable, "bridge has no Slack bot")
	}
	if _, ok := AgentCallerFromContext(ctx); ok || agent != "" {
		_, err := a.readWall(ctx, agent, "", channel, threadTS)
		return err
	}
	if a.refuseSharedBearer && !operatorCaller(ctx) {
		return errSharedBearerRetired
	}
	t, err := a.resolveTarget(ctx, "", channel, threadTS, nil)
	if err != nil {
		return err
	}
	name := targetName(t, "", channel, threadTS)
	if t.ch.Land == "" {
		return sayErrorf(http.StatusForbidden,
			"no land claims %s; a read that names no agent reaches only an operator land's thread", name)
	}
	if a.bot.daemon == nil {
		return sayErrorf(http.StatusServiceUnavailable, "bridge has no daemon")
	}
	projects, err := a.bot.daemon.ListProjectBeads(ctx)
	if err != nil {
		return fmt.Errorf("reading land %s: %w", t.ch.Land, err)
	}
	p, ok := projects[t.ch.Land]
	switch {
	case !ok:
		return sayErrorf(http.StatusForbidden,
			"%s is in land %q, which has no project; a read that names no agent reaches only an operator land's thread", name, t.ch.Land)
	case !p.IsOperatorLand():
		return sayErrorf(http.StatusForbidden,
			"%s is in %s land %q; a read that names no agent reaches only an operator land's thread", name, p.LandKind, t.ch.Land)
	}
	return nil
}

// threadBeads reads a thread from beads: the head, then up to limit-1 of its
// message beads, oldest first, as Slack returns a thread.
func (a *ConversationAgentAPI) threadBeads(ctx context.Context, headID string, limit int) ([]ThreadMessage, error) {
	head, err := a.bot.daemon.GetBead(ctx, headID)
	if err != nil {
		return nil, err
	}
	out := []ThreadMessage{headMessage(head)}
	if limit <= 1 {
		return out, nil
	}
	res, err := a.bot.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"message"},
		Fields: map[string]string{conversation.BeadThreadID: headID},
		Sort:   "created_at",
		Limit:  limit - 1,
	})
	if err != nil {
		return nil, err
	}
	if res != nil {
		for _, b := range res.Beads {
			out = append(out, beadMessage(b))
		}
	}
	return out, nil
}

// headMessage is a thread head as its root message: the words that opened it.
func headMessage(head *beadsapi.BeadDetail) ThreadMessage {
	text := head.Fields["slack_root_text"]
	if text == "" {
		text = head.Fields[conversation.BeadText]
	}
	if text == "" {
		text = head.Description
	}
	if text == "" {
		text = head.Title
	}
	author := beadAuthor(head)
	return ThreadMessage{
		Author:    principalName(author),
		Text:      text,
		Timestamp: beadTimestamp(head),
		IsBot:     !isPersonPrincipal(author),
	}
}

// beadMessage is a message bead as a thread message. A squawk keeps its
// Slack subtype, so gb's default noise filter drops it the same way.
func beadMessage(b *beadsapi.BeadDetail) ThreadMessage {
	text := b.Fields[conversation.BeadText]
	if text == "" {
		text = b.Title
	}
	author := beadAuthor(b)
	tm := ThreadMessage{
		Author:    principalName(author),
		Text:      text,
		Timestamp: beadTimestamp(b),
		IsBot:     !isPersonPrincipal(author),
	}
	if hasLabel(b.Labels, "squawk") || hasLabel(b.Labels, "say") {
		tm.Subtype = "squawk"
	}
	return tm
}

// beadAuthor is who said a conversation bead: its author field, else the
// agent that sent it, else its creator.
func beadAuthor(b *beadsapi.BeadDetail) string {
	for _, k := range []string{conversation.BeadAuthor, "source_agent", "operated_by"} {
		if v := b.Fields[k]; v != "" {
			return v
		}
	}
	return b.CreatedBy
}

// principalName strips a principal's kind (agent:, oidc:) for display.
func principalName(p string) string {
	for _, kind := range []string{"agent:", "oidc:"} {
		if name, ok := strings.CutPrefix(p, kind); ok && name != "" {
			return name
		}
	}
	return p
}

// isPersonPrincipal reports whether p is a person (an OIDC email) rather than
// an agent or the bridge.
func isPersonPrincipal(p string) bool {
	return strings.HasPrefix(p, "oidc:") || strings.Contains(p, "@")
}

// beadTimestamp is a bead's place in a thread: its Slack ts when it was
// projected there, else its creation time in the same seconds.micros form.
func beadTimestamp(b *beadsapi.BeadDetail) string {
	if ts := b.Fields["slack_ts"]; ts != "" {
		return ts
	}
	if b.CreatedAt.IsZero() {
		return ""
	}
	return fmt.Sprintf("%d.%06d", b.CreatedAt.Unix(), b.CreatedAt.Nanosecond()/int(time.Microsecond))
}
