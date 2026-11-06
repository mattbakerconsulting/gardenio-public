package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
	"gardenio/controller/internal/respond"
)

// The Glass, nudge and respond (Lands workbench-glass spec GL-7, GL-8, GL-11; step 6.5, gd-m1aiBXtDak).
//
// POST /garden/glass/{agent}/send takes one control frame. Two kinds are taken here:
//   - nudge: a sentence for the agent's next turn. It goes through the bridge's one nudge path (Glass.Nudge, wired to
//     bridge.NudgeAgent: the MCP channel first, coop's PTY nudge after, G-D4), and once delivered it is written into the
//     agent's Commons thread as the person's message (Glass.Say, Commons D6), so the thread shows what the agent was told.
//     A nudge that does not land says why honestly (busy at a prompt, stopped, gone) and writes nothing.
//   - respond: an answer to the prompt the agent is parked at (Permission, Plan, Question, Setup), chosen from the
//     respond component (internal/respond) that GET /garden/glass/{agent}/prompt renders from coop's prompt. The answer
//     is checked against the prompt as coop has it now before it is sent, so an answer for a prompt that moved on is a
//     409, never the wrong option.
//
// input, keys, mouse, resize and compact are the keyboard's: they need the person to hold the agent's keyboard (GL-9,
// glass_attach.go). Every write, whatever its outcome, emits the agent_terminal_write audit line beads-viewer's agent page
// writes (GL-11), with surface=glass, the person's oidc: email and roles, and an audit id the answer also carries.
//
// The frame goes on as the person asked only after the Glass's own checks: the agent bead on the viewer's bearer and in
// their land scope (else 404), the watch tier, then the frame's tier from the ported coopmux table (glass.SessionFrameTier:
// nudge and respond are operator). Writes reach coopmux on the viewer's service bearer, as /agents/{id}/respond's do:
// the Glass relay key verifies only the watch upgrades (ruling gd-9ZGh01W5ub), so coopmux names the relay for a write
// and the audit line here names the person.

// NudgeOutcome is how a nudge went.
type NudgeOutcome string

const (
	// NudgeDelivered: the agent has the words for its next turn.
	NudgeDelivered NudgeOutcome = "delivered"
	// NudgeBusy: the pod answers but would not take a nudge now (parked at a prompt, or wedged).
	NudgeBusy NudgeOutcome = "busy"
	// NudgeStopped: the agent is done, failed, out of budget, or stopping.
	NudgeStopped NudgeOutcome = "stopped"
	// NudgeGone: the agent's pod is not there to take it (it never registered, or no longer answers).
	NudgeGone NudgeOutcome = "gone"
	// NudgeFailed: the nudge could not be sent, for another reason.
	NudgeFailed NudgeOutcome = "failed"
)

// GlassPrompts reads and answers an agent's prompt; *coopmuxapi.Client is one.
type GlassPrompts interface {
	GetAgentState(ctx context.Context, sessionID string) (*coopmuxapi.AgentState, error)
	RespondOutcome(ctx context.Context, sessionID string, req coopmuxapi.RespondRequest) (*coopmuxapi.RespondOutcome, error)
	SendKeys(ctx context.Context, sessionID string, keys []string) error
}

const (
	glassSendMax     = 32 << 10 // a send's body
	glassNudgeMax    = 4 * 1024 // a nudge's words, as /agents/{id}/nudge caps them
	glassSendTimeout = 45 * time.Second
	// GlassOrigin is the origin of the message a Glass nudge writes into the agent's thread: not "commons", so the
	// bridge does not deliver it again.
	GlassOrigin = "glass"
)

// GlassSent is the answer to a send asked for JSON.
type GlassSent struct {
	OK   bool   `json:"ok"`
	Kind string `json:"kind"` // nudge | respond | input | keys | mouse | resize | compact
	// Result is delivered, busy, stopped, gone or failed; changed, no_prompt or not_live for a respond; not_attached,
	// confirm (sent only once confirmed) or canceled for the keyboard's frames; not_tracking for a mouse frame the app
	// would not take.
	Result string `json:"result"`
	Note   string `json:"note"` // the outcome in words, for the person
	// Message is the message bead the nudge wrote into the agent's thread, "" when none was written.
	Message string `json:"message,omitempty"`
	AuditID string `json:"audit_id"`
}

func (h *Handler) glassLog() *slog.Logger {
	if h.Glass != nil && h.Glass.Logger != nil {
		return h.Glass.Logger
	}
	return slog.Default()
}

// sendFrame is a send's body: JSON {"type": ..., ...} or the same names as a form.
type sendFrame struct {
	Type    string
	Message string
	get     func(string) string
}

func readSendFrame(r *http.Request) (sendFrame, error) {
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		var m map[string]any
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			return sendFrame{}, errors.New("the frame is not JSON")
		}
		get := func(k string) string {
			switch v := m[k].(type) {
			case string:
				return v
			case float64:
				return strconv.FormatFloat(v, 'f', -1, 64)
			}
			return ""
		}
		return sendFrame{Type: get("type"), Message: get("message"), get: get}, nil
	}
	if err := r.ParseForm(); err != nil {
		return sendFrame{}, errors.New("the frame is not a form")
	}
	return sendFrame{Type: r.PostForm.Get("type"), Message: r.PostForm.Get("message"), get: r.PostForm.Get}, nil
}

// frameEvents are a send's types as coopmux names their frames, for the ported tier table.
var frameEvents = map[string]string{
	"nudge": "nudge", "respond": "respond", "input": "input:send", "keys": "keys:send", "mouse": "mouse:send", "resize": "resize",
	"compact": "nudge",
}

// glassSend serves POST /garden/glass/{agent}/send.
func (h *Handler) glassSend(w http.ResponseWriter, r *http.Request) { h.glassSendAs(w, r, "") }

// GlassNudge serves a nudge for the agent bead in r's "agent" path value, from a form or JSON "message": the Glass's own
// nudge (bridge NudgeAgent, the message in the agent's thread, the audit line), for beads-viewer's POST
// /agents/{id}/nudge, which is re-pointed here (step 6.5).
func (h *Handler) GlassNudge(w http.ResponseWriter, r *http.Request) { h.glassSendAs(w, r, "nudge") }

// glassSendAs serves a send; kind, when set, is the only frame type taken (the body's type is then not read).
func (h *Handler) glassSendAs(w http.ResponseWriter, r *http.Request, kind string) {
	if crossOrigin(r) {
		http.Error(w, "cross-origin", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, glassSendMax)
	b, project, who, ok := h.glassAgent(w, r)
	if !ok {
		return
	}
	f, err := readSendFrame(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if kind != "" {
		f.Type = kind
	}
	event, known := frameEvents[f.Type]
	if !known {
		http.Error(w, "unknown frame type", http.StatusBadRequest)
		return
	}
	frame, _ := json.Marshal(map[string]string{"event": event})
	if t := glass.SessionFrameTier(frame); !t.Allows(who.Roles) {
		http.Error(w, f.Type+" needs "+t.String(), http.StatusForbidden)
		return
	}
	a := glassWrite{h: h, w: w, r: r, b: b, who: who, kind: f.Type, auditID: coopmuxapi.NewAuditID()}
	switch f.Type {
	case "nudge":
		a.nudge(f.Message)
	case "respond":
		a.respond(f.get)
	default:
		a.keyboard(project, f.get)
	}
}

// glassWrite is one send being answered.
type glassWrite struct {
	h       *Handler
	w       http.ResponseWriter
	r       *http.Request
	b       *beadsapi.BeadDetail
	who     coopmuxapi.Identity
	kind    string
	auditID string
}

// log writes the GL-11 audit line, the shape beads-viewer's agent page writes (cmd/beads-viewer agent_phase3.go).
func (a glassWrite) log(result string, kv ...any) {
	args := append([]any{
		"event", "agent_terminal_write",
		"subverb", a.kind,
		"surface", "glass",
		"agent", a.b.ID,
		"session", sessionOf(a.b),
		"operator", a.who.Operator,
		"roles", a.who.Roles,
		"audit_id", a.auditID,
		"result", result,
	}, kv...)
	a.h.glassLog().Info("agent_terminal_write", args...)
}

// answer replies with the outcome: JSON when asked for it, else a status line.
func (a glassWrite) answer(status int, s GlassSent) {
	s.Kind, s.AuditID = a.kind, a.auditID
	if !strings.Contains(a.r.Header.Get("Accept"), "application/json") {
		if s.OK {
			a.w.WriteHeader(http.StatusNoContent)
		} else {
			http.Error(a.w, s.Note, status)
		}
		return
	}
	a.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	a.w.Header().Set("Cache-Control", "no-store")
	a.w.WriteHeader(status)
	_ = json.NewEncoder(a.w).Encode(s)
}

// nudgeNotes say how a nudge went, for the person.
var nudgeNotes = map[NudgeOutcome]string{
	NudgeDelivered: "Sent. It reaches the agent in its next turn, and it is in the agent's thread.",
	NudgeBusy:      "Not sent: the agent is busy and will not take a nudge now (it may be waiting at a prompt). Answer the prompt, or try again in a moment.",
	NudgeStopped:   "Not sent: the agent has stopped, so there is no next turn to put it in. Reply in its thread to wake it.",
	NudgeGone:      "Not sent: the agent's pod is not there to take it. Reply in its thread, which can bring it back.",
	NudgeFailed:    "Not sent: the nudge could not be delivered. Try again in a moment.",
}

func (a glassWrite) nudge(message string) {
	message = strings.TrimSpace(message)
	switch {
	case message == "":
		a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: "Write something to send."})
		return
	case len(message) > glassNudgeMax:
		a.answer(http.StatusRequestEntityTooLarge, GlassSent{Result: "failed", Note: "That is too long for a nudge: keep it under 4 KB."})
		return
	}
	g := a.h.Glass
	if g.Nudge == nil {
		a.answer(http.StatusServiceUnavailable, GlassSent{Result: "failed", Note: "Nudges are not enabled here."})
		return
	}
	agent := firstNonEmpty(strings.TrimSpace(a.b.Fields["agent"]), a.b.Title)
	ctx, cancel := context.WithTimeout(a.r.Context(), glassSendTimeout)
	out := g.Nudge(ctx, agent, message)
	cancel()
	note, known := nudgeNotes[out]
	if !known {
		out, note = NudgeFailed, nudgeNotes[NudgeFailed]
	}
	if out != NudgeDelivered {
		a.log(string(out), "len", len(message))
		status := http.StatusConflict
		if out == NudgeFailed {
			status = http.StatusBadGateway
		}
		a.answer(status, GlassSent{Result: string(out), Note: note})
		return
	}
	s := GlassSent{OK: true, Result: string(out), Note: note}
	if g.Say != nil {
		id, err := g.Say(a.r, a.b.ID, message)
		if err != nil {
			s.Note = "Sent. It reaches the agent in its next turn, but it could not be written into the agent's thread."
			a.h.glassLog().Warn("glass: the nudge was not written into the agent's thread", "agent", a.b.ID, "audit_id", a.auditID, "error", err)
		}
		s.Message = id
	}
	a.log(string(out), "len", len(message), "message", s.Message)
	a.answer(http.StatusOK, s)
}

func (a glassWrite) respond(get func(string) string) {
	ans, err := respond.ParseAnswer(get)
	if err != nil {
		a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: err.Error()})
		return
	}
	g := a.h.Glass
	session := sessionOf(a.b)
	if g.Prompts == nil || session == "" {
		a.log("not_live")
		a.answer(http.StatusConflict, GlassSent{Result: "not_live", Note: "Not answered: the agent has no live session."})
		return
	}
	ctx, cancel := context.WithTimeout(a.r.Context(), glassSendTimeout)
	defer cancel()
	st, err := g.Prompts.GetAgentState(ctx, session)
	if err != nil {
		a.upstreamFailed(err)
		return
	}
	var p *coopmuxapi.Prompt
	if st != nil && st.State == "prompt" {
		p = st.Prompt
	}
	reply, err := respond.Check(p, ans)
	if err != nil {
		result, note := "changed", "Not answered: the prompt changed before the answer arrived. Here it is as it is now."
		if errors.Is(err, respond.ErrNoPrompt) {
			result, note = "no_prompt", "Not answered: the agent is no longer at a prompt."
		}
		a.log(result, "prompt", ans.Prompt, "option", ans.Option, "detail", err.Error())
		a.answer(http.StatusConflict, GlassSent{Result: result, Note: note})
		return
	}
	if reply.Keys != nil {
		if err := g.Prompts.SendKeys(ctx, session, reply.Keys); err != nil {
			a.upstreamFailed(err)
			return
		}
	} else {
		o, err := g.Prompts.RespondOutcome(ctx, session, *reply.Request)
		if err != nil {
			a.upstreamFailed(err)
			return
		}
		if o != nil && !o.Delivered {
			a.log("no_prompt", "prompt", ans.Prompt, "option", ans.Option, "detail", o.Reason)
			a.answer(http.StatusConflict, GlassSent{Result: "no_prompt", Note: "Not answered: the agent was no longer at a prompt."})
			return
		}
	}
	s := GlassSent{OK: true, Result: "delivered", Note: "Answered: " + reply.Said + "."}
	var settled string
	if p.Type == respond.TypePermission && p.Subtype != "trust" && g.Answered != nil {
		id, err := g.Answered(a.r, a.b.ID, strings.TrimSpace(p.Tool), permissionBehavior(reply.Said), reply.Said)
		switch {
		case err != nil:
			s.Note += " Its permission card in the Commons still shows it open."
			a.h.glassLog().Warn("glass: the permission card was not closed", "agent", a.b.ID, "audit_id", a.auditID, "error", err)
		case id != "":
			settled = id
			s.Note += " Its permission card in the Commons says so too."
		}
	}
	a.log("ok", "prompt", ans.Prompt, "option", ans.Option, "has_text", strings.TrimSpace(ans.Text) != "", "said", clip(reply.Said, 120),
		"permission", settled)
	a.answer(http.StatusOK, s)
}

// permissionBehavior is a permission prompt's answer as the permission card records it: Claude Code's options read
// "Yes", "Yes, and don't ask again for ..." and "No, and tell Claude what to do differently", so allow, deny, or ""
// for an option that says neither.
func permissionBehavior(label string) string {
	w := strings.ToLower(strings.TrimSpace(label))
	switch {
	case w == "yes" || strings.HasPrefix(w, "yes,") || strings.HasPrefix(w, "yes "):
		return "allow"
	case w == "no" || strings.HasPrefix(w, "no,") || strings.HasPrefix(w, "no "):
		return "deny"
	}
	return ""
}

func (a glassWrite) upstreamFailed(err error) {
	not := "Not sent: "
	if a.kind == "respond" {
		not = "Not answered: "
	}
	result, status, note := "failed", http.StatusBadGateway, not+"coopmux did not take it. Try again in a moment."
	switch {
	case errors.Is(err, coopmuxapi.ErrSessionNotFound):
		result, status, note = "not_live", http.StatusConflict, not+"coopmux no longer has the agent's session."
	case errors.Is(err, coopmuxapi.ErrDisabled):
		status, note = http.StatusServiceUnavailable, not+"the garden cannot reach coopmux here."
	}
	a.log(result, "error", err.Error())
	a.answer(status, GlassSent{Result: result, Note: note})
}

// glassPromptView is the prompt fragment: what coop's prompt asks, and the respond component to answer it.
type glassPromptView struct {
	Type, Heading, Tool, Input, Question string
	Form                                 respond.Form
	Reading                              bool
}

var glassPromptTmpl = template.Must(template.Must(template.New("glassprompt").Parse(
	`{{define "prompt"}}<section class="gl-prompt" data-prompt="{{.Type}}" aria-label="{{.Heading}}">
<p class="gl-prompt-head"><span class="gl-prompt-kind">{{.Heading}}</span>{{with .Tool}} · <code>{{.}}</code>{{end}}</p>
{{- with .Input}}
<pre class="gl-prompt-input"><code>{{.}}</code></pre>
{{- end}}
{{- with .Question}}
<p class="gl-prompt-q">{{.}}</p>
{{- end}}
{{- if .Reading}}
<p class="bv-note">Coop is still reading this prompt's options; they show in a moment.</p>
{{- else}}
{{template "respond" .Form}}
{{- end}}
</section>{{end}}`)).Parse(respond.Template))

// glassPromptInputMax caps the tool input the prompt shows (the Commons card cuts it at 500 too).
const glassPromptInputMax = 500

// glassPrompt serves GET /garden/glass/{agent}/prompt: the agent's prompt as coop has it now, as an HTML fragment with
// the respond component, posting to /send. 204 when the agent is at no prompt (or the viewer may not respond).
func (h *Handler) glassPrompt(w http.ResponseWriter, r *http.Request) {
	b, _, who, ok := h.glassAgent(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	session := sessionOf(b)
	if !glass.SessionFrameTier([]byte(`{"event":"respond"}`)).Allows(who.Roles) || session == "" || h.Glass.Prompts == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), glassScreenTimeout)
	st, err := h.Glass.Prompts.GetAgentState(ctx, session)
	cancel()
	if err != nil {
		if errors.Is(err, coopmuxapi.ErrSessionNotFound) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "coopmux did not answer", http.StatusBadGateway)
		return
	}
	if st == nil || st.State != "prompt" || st.Prompt == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	p := st.Prompt
	form, ready := respond.FromPrompt(p, h.BasePath+"/garden/glass/"+b.ID+"/send", "glass", respond.Field{Name: "type", Value: "respond"})
	v := glassPromptView{Type: p.Type, Heading: respond.Heading(p), Tool: p.Tool, Question: respond.Question(p), Form: form, Reading: !ready}
	if p.Subtype != "oauth_login" { // its input is the sign-in link, offered as one
		v.Input = clip(p.Input, glassPromptInputMax)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := glassPromptTmpl.ExecuteTemplate(w, "prompt", v); err != nil {
		h.glassLog().Warn("glass: prompt fragment", "agent", b.ID, "error", err)
	}
}
