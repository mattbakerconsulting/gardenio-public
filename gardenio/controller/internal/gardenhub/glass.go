package gardenhub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// The Glass relay, read side (Lands workbench-glass spec §3.4-§3.5, step 6.3, gd-O31EYiDGzm; GL-1, GL-3, GL-4, GL-6,
// GL-14). GET /garden/glass/{agent} is the panel's first paint: who the agent is, its cached screen, and what the viewer
// may do. GET /garden/glass/{agent}/stream is its live terminal as SSE: the shared per-session upstream in internal/glass
// (one coopmux subscription however many watch), resumed with Last-Event-ID and resynced with a screen on a gap.
//
// Both resolve the agent the same way, in the spec's order: the agent bead on the viewer's own bearer (kbeads cuts what
// the viewer may not see), its project against the viewer's land scope (an agent outside it is a 404, identical to a
// missing one, so ids do not leak), then the tier (watch is operator-only in v1, owner Q8d: a PTY can show a secret),
// then the session coopmux knows it by (sessionOf: the pod name in its notes, else last_session_id). The browser never reaches coopmux: the stream is opened server-side with the service
// token and an edge-auth envelope naming the human, and coopmux re-checks the tier.

// GlassScreens reads a session's cached screen for the first paint; *coopmuxapi.Client is one.
type GlassScreens interface {
	GetScreen(ctx context.Context, sessionID string) (*coopmuxapi.Screen, error)
}

// Glass wires the Glass relay into the garden.
type Glass struct {
	// Relay holds the shared upstreams. Nil answers the stream 503.
	Relay *glass.Relay
	// Wall holds the Wall's one shared /ws/mux upstream (GET /garden/glass/wall, glass_wall.go). Nil answers its stream 503.
	Wall *glass.Wall
	// Screens reads the cached screen. Nil sends the panel no screen.
	Screens GlassScreens
	// InScope reports whether the viewer's land scope admits project (an unrestricted viewer admits every project). An
	// agent it does not admit answers 404. Nil admits nothing.
	InScope func(r *http.Request, project string) bool
	// Who is the human behind the request as the upstream envelope names them: email, auth mode and canonical gardenio
	// roles. The tier checks read its roles.
	Who func(r *http.Request) coopmuxapi.Identity
	// Nudge delivers a nudge (GL-7, glass_send.go) to the agent of that name and says how it went: beads-viewer wires the
	// bridge's NudgeAgent (G-D4). Nil answers nudges 503 and offers none.
	Nudge func(ctx context.Context, agent, message string) NudgeOutcome
	// Say writes a delivered nudge into the agent's Commons thread as the viewer's message, on the viewer's bearer, and
	// returns its id (commons.Handler.SayInThread with GlassOrigin). Nil writes none.
	Say func(r *http.Request, agentID, text string) (string, error)
	// Prompts reads the prompt the agent is parked at and answers it (GL-8). Nil offers no answers.
	Prompts GlassPrompts
	// Answered closes the agent's open permission request for tool once the Glass has answered its permission prompt, so
	// the Commons card (and its Slack prompt) says it is answered (spec 3.3: answering in either place resolves both):
	// behavior is allow, deny or "", said the option's label. It returns the request bead, "" when none was open
	// (commons.Handler.PermissionAnswered, through the bridge). Nil closes none.
	Answered func(r *http.Request, agentID, tool, behavior, said string) (string, error)
	// Logger takes the agent_terminal_write audit lines (GL-11). Nil is slog.Default().
	Logger *slog.Logger
	// MayAttach reports whether the viewer may take the keyboard of an agent in project (GL-9, glass_attach.go;
	// beads-viewer wires GlassAttachTier). Nil means no.
	MayAttach func(r *http.Request, project string) bool
	// Keyboard types into the PTY of the person holding the keyboard: input, keys, resize and compact. Nil offers no
	// attach.
	Keyboard GlassKeyboard
	// Pool reads coopmux's credential pool for GET /garden/glass/credentials (GL-15, glass_pool.go). Nil answers 503 and
	// offers no opener.
	Pool GlassPool

	now func() time.Time // test seam; time.Now when nil
}

// mayWatch reports whether the page should offer the Glass to this viewer: it is wired, and the viewer holds the watch
// tier (glassAgent checks it again on every route). The page then shows the glass kind and its Watch links.
func (h *Handler) mayWatch(r *http.Request) bool {
	if h.Glass == nil || h.Glass.Who == nil {
		return false
	}
	return glassWatchTier.Allows(h.Glass.Who(r).Roles)
}

const (
	glassTimeout       = 8 * time.Second
	glassScreenTimeout = 3 * time.Second
	glassTitleMax      = 400
	// glassStateTimeout bounds the first paint's read of coop's state: past it the doc keeps the bead's agent_state.
	glassStateTimeout = 300 * time.Millisecond
	// glassWatchTier is who may watch: operators, on every land (spec §3.4 #5; owner Q8d keeps the per-land opt-in to
	// members off). glassWatchPolicy is that setting's name as the panel is told it.
	glassWatchTier   = glass.TierOperator
	glassWatchPolicy = "operators"
)

// GlassScreen is the cached screen the panel paints before the stream opens.
type GlassScreen struct {
	Lines     []string `json:"lines"`
	Cols      uint16   `json:"cols"`
	Rows      uint16   `json:"rows"`
	AltScreen bool     `json:"alt_screen,omitempty"`
	Seq       uint64   `json:"seq"`
}

// GlassDoc is GET /garden/glass/{agent}: the Glass header (GL-3), the state, the cached screen and what the viewer may do.
type GlassDoc struct {
	ID        string `json:"id"` // the agent bead
	Agent     string `json:"agent"`
	Project   string `json:"project,omitempty"`
	Role      string `json:"role,omitempty"`
	Task      string `json:"task,omitempty"`
	TaskTitle string `json:"task_title,omitempty"`
	Pod       string `json:"pod,omitempty"`
	Account   string `json:"account,omitempty"`
	Cost      string `json:"cost,omitempty"` // "$1.23", "" when none is reported
	Context   int64  `json:"context_tokens,omitempty"`
	// State is coop's state for a live agent when coop answers in time (GL-4: a watch-only viewer's chip says Prompt
	// with no stream, gd-vWfjr8cTVq), else the agent bead's agent_state; the stream's state events supersede it.
	State string `json:"state,omitempty"`
	// Live: the agent has a session and coopmux has it now. Stream is set only then.
	Live   bool         `json:"live"`
	Stream string       `json:"stream,omitempty"`
	Screen *GlassScreen `json:"screen,omitempty"`
	// Watch is the land's glass_watch setting: who may watch.
	Watch    string `json:"glass_watch"`
	CanNudge bool   `json:"can_nudge"`
	// CanRespond: the viewer may answer the agent's prompts (GL-8); GET .../prompt renders the current one.
	CanRespond bool `json:"can_respond"`
	// CanAttach: the viewer may take the keyboard (GL-9, glass_attach.go); Control is who holds it now, and You is the
	// viewer as a claim names them, so the panel can tell its own claim from another's.
	CanAttach bool           `json:"can_attach"`
	Control   *glass.Control `json:"control,omitempty"`
	You       string         `json:"you,omitempty"`
}

// glassAgent resolves {agent} for a Glass route, answering the request itself when it may not go on.
func (h *Handler) glassAgent(w http.ResponseWriter, r *http.Request) (*beadsapi.BeadDetail, string, coopmuxapi.Identity, bool) {
	return h.glassAgentID(w, r, r.PathValue("agent"))
}

// glassAgentID is glassAgent for the agent bead id, wherever the route carries it.
func (h *Handler) glassAgentID(w http.ResponseWriter, r *http.Request, id string) (*beadsapi.BeadDetail, string, coopmuxapi.Identity, bool) {
	var who coopmuxapi.Identity
	if h.Glass == nil || h.Beads == nil {
		http.Error(w, "the glass is not enabled", http.StatusServiceUnavailable)
		return nil, "", who, false
	}
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 200 {
		http.Error(w, "bad agent id", http.StatusBadRequest)
		return nil, "", who, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), glassTimeout)
	defer cancel()
	b, err := h.Beads.GetBead(ctx, id)
	if err != nil {
		var ae *beadsapi.APIError
		if errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden) {
			http.Error(w, "no such agent", http.StatusNotFound)
		} else {
			http.Error(w, "the beads daemon did not answer", http.StatusBadGateway)
		}
		return nil, "", who, false
	}
	project := ""
	if b != nil {
		project = strings.TrimSpace(b.Fields["project"])
		if project == "" {
			project = projectOf(b)
		}
	}
	if b == nil || b.Type != "agent" || h.Glass.InScope == nil || !h.Glass.InScope(r, project) {
		http.Error(w, "no such agent", http.StatusNotFound) // out of scope is not there
		return nil, "", who, false
	}
	if h.Glass.Who != nil {
		who = h.Glass.Who(r)
	}
	if !glassWatchTier.Allows(who.Roles) {
		http.Error(w, "watching an agent needs "+glassWatchTier.String(), http.StatusForbidden)
		return nil, "", who, false
	}
	return b, project, who, true
}

// glassView serves GET /garden/glass/{agent}.
func (h *Handler) glassView(w http.ResponseWriter, r *http.Request) {
	b, project, who, ok := h.glassAgent(w, r)
	if !ok {
		return
	}
	doc := glassDoc(b, project)
	doc.Watch = glassWatchPolicy
	doc.CanNudge = h.Glass.Nudge != nil && glass.SessionFrameTier([]byte(`{"event":"nudge"}`)).Allows(who.Roles)
	doc.CanRespond = h.Glass.Prompts != nil && glass.SessionFrameTier([]byte(`{"event":"respond"}`)).Allows(who.Roles)
	doc.CanAttach = h.mayAttach(r, project, who)
	if h.readOnly(r) { // nudge, respond and attach are /garden writes, admin-only for a read-only viewer
		doc.CanNudge, doc.CanRespond, doc.CanAttach = false, false, false
	}
	doc.You = who.Operator
	if doc.Task != "" {
		ctx, cancel := context.WithTimeout(r.Context(), glassTimeout)
		if t, err := h.Beads.GetBead(ctx, doc.Task); err == nil && t != nil {
			doc.TaskTitle = clip(t.Title, glassTitleMax)
		}
		cancel()
	}
	if session := sessionOf(b); session != "" {
		doc.Live = true // unless coopmux says otherwise below
		if h.Glass.Screens != nil {
			ctx, cancel := context.WithTimeout(r.Context(), glassScreenTimeout)
			s, err := h.Glass.Screens.GetScreen(ctx, session)
			cancel()
			switch {
			case err == nil && s != nil:
				doc.Screen = &GlassScreen{Lines: s.Lines, Cols: s.Cols, Rows: s.Rows, AltScreen: s.AltScreen, Seq: s.Seq}
			case errors.Is(err, coopmuxapi.ErrSessionNotFound), errors.Is(err, coopmuxapi.ErrDisabled):
				doc.Live = false
			}
		}
		if doc.Screen != nil && doc.Screen.Lines == nil {
			doc.Screen.Lines = []string{}
		}
	}
	if doc.Live {
		doc.Stream = h.BasePath + "/garden/glass/" + b.ID + "/stream"
		if h.Glass.Prompts != nil { // coop's state outranks the bead's; on an error or a slow coop the bead's stands
			ctx, cancel := context.WithTimeout(r.Context(), glassStateTimeout)
			if st, err := h.Glass.Prompts.GetAgentState(ctx, sessionOf(b)); err == nil && st != nil && st.State != "" {
				doc.State = st.State
			}
			cancel()
		}
		if h.Glass.Relay != nil {
			if c, held := h.Glass.Relay.Holder(sessionOf(b)); held {
				doc.Control = &c
			}
		}
	}
	writeDoc(w, "glass", doc)
}

// glassDoc is the header the agent bead itself answers.
func glassDoc(b *beadsapi.BeadDetail, project string) *GlassDoc {
	notes := beadsapi.ParseNotes(b.Notes)
	d := &GlassDoc{
		ID: b.ID, Agent: firstNonEmpty(b.Fields["agent"], b.Title), Project: project, Role: b.Fields["role"],
		Task: strings.TrimSpace(b.Fields["task_id"]), Account: b.Fields["account"], State: b.Fields["agent_state"],
		Pod: firstNonEmpty(strings.TrimSpace(notes["pod_name"]), b.Fields["pod_name"]),
	}
	if v, err := strconv.ParseFloat(strings.TrimPrefix(strings.TrimSpace(b.Fields["live_cost_usd"]), "$"), 64); err == nil && v > 0 {
		d.Cost = fmt.Sprintf("$%.2f", v)
	}
	if v, err := strconv.ParseInt(strings.TrimSpace(b.Fields["context_tokens"]), 10, 64); err == nil && v > 0 {
		d.Context = v
	}
	return d
}

// sessionOf is the id coopmux knows the agent's session by: the pod name in its notes, else last_session_id
// (beadsapi.BeadDetail.MuxSessionID). A pod registers under its own name, not coop's session id (gd-oJOJvsOdw7).
func sessionOf(b *beadsapi.BeadDetail) string { return b.MuxSessionID() }

// glassStream serves GET /garden/glass/{agent}/stream: the agent's live session as SSE events named for internal/glass's
// event kinds (replay, pty, screen, state, usage, exit, ended), each byte-bearing one with its stream offset as the id.
func (h *Handler) glassStream(w http.ResponseWriter, r *http.Request) {
	b, _, who, ok := h.glassAgent(w, r)
	if !ok {
		return
	}
	session := sessionOf(b)
	if session == "" {
		http.Error(w, "no live session", http.StatusNotFound)
		return
	}
	// The per-session upgrade is itself operator-tier at coopmux (it can carry control frames); the relay checks it too.
	if t := glass.RouteTier(http.MethodGet, "/ws/"+session); !t.Allows(who.Roles) {
		http.Error(w, "watching an agent needs "+t.String(), http.StatusForbidden)
		return
	}
	if h.Glass.Relay == nil {
		http.Error(w, "the glass relay is not enabled", http.StatusServiceUnavailable)
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	last := int64(-1)
	if v, err := strconv.ParseInt(strings.TrimSpace(r.Header.Get("Last-Event-ID")), 10, 64); err == nil && v >= 0 {
		last = v
	}
	watcher := h.Glass.Relay.Watch(session, who, last)
	defer watcher.Close()

	hd := w.Header()
	hd.Set("Content-Type", "text/event-stream")
	hd.Set("Cache-Control", "no-cache")
	hd.Set("Connection", "keep-alive")
	hd.Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	fl.Flush()

	hb := time.NewTicker(heartbeatEvery)
	defer hb.Stop()
	done := r.Context().Done()
	for {
		select {
		case <-done:
			return
		case <-watcher.Ready():
			evs, finished := watcher.Take()
			for _, ev := range evs {
				if ev.ID >= 0 {
					fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", ev.Kind, ev.ID, ev.Data)
				} else {
					fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Kind, ev.Data)
				}
			}
			fl.Flush()
			if finished {
				return
			}
		case <-hb.C:
			fmt.Fprint(w, "event: hb\ndata: {}\n\n")
			fl.Flush()
		}
	}
}
