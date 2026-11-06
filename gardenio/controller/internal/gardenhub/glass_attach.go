package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
)

// The Glass, attach (Lands workbench-glass spec GL-9, GL-10, GL-11; step 6.6, gd-mWooPMTNwA).
//
// Attach is hands on the keyboard, and an explicit act. POST /garden/glass/{agent}/control claims the agent's keyboard
// (action=claim), keeps the claim alive (renew) and lets go of it (release). The claim is the relay's (internal/glass
// control.go): every watcher of the session is told who holds it ("attached as <them>"), one who joins later hears it
// first thing, and a claim nobody renews lapses on its own. A keyboard someone else holds is taken only when the person
// says so (take=1); the one it was taken from is told.
//
// Only then do the keyboard frames on /send go on, and only from the holder: input (typed text), keys (named keys: Esc,
// Tab, Enter, arrows, Ctrl chords), mouse (a click or wheel turn, once the app has turned mouse tracking on), resize
// (the terminal fitted to the holder's screen) and compact (Claude Code's /compact, from the attach toolbar, GL-10).
// Each one renews the claim. A mouse frame is the SGR report xterm made (ESC [ < b ; x ; y M/m) and only that: it is
// read here into a button and a cell, and coop's input/mouse builds the bytes again as the app asked (gd-5LOtAx1sh1),
// so no raw bytes reach the PTY (raw is admin's) and nothing is sent while the app is not tracking the mouse. A batch of keys with Ctrl-C, Ctrl-D, Ctrl-\ or
// Ctrl-Z in it, and a compact, is sent only once confirmed (confirmed=1); the first send answers 409 with result
// "confirm" and sends nothing, and a cancel (cancel=1) is audited and sends nothing, as beads-viewer's agent page does.
// Typed text carries no control characters: those are keys, so a key coop's tier table holds for admin (raw bytes) cannot
// ride in as text.
//
// Who may attach is Glass.MayAttach: operators on the operator's own lands and admins only on a tenant land until Fences
// (spec §3.4 #5, GlassAttachTier). Every claim, release, take and keyboard write emits the agent_terminal_write audit line
// (GL-11). Writes reach coopmux on the viewer's service bearer, as nudge and respond do (ruling gd-9ZGh01W5ub).

// GlassKeyboard types into an agent's PTY; *coopmuxapi.Client is one.
type GlassKeyboard interface {
	SendInput(ctx context.Context, sessionID, text string) error
	SendKeys(ctx context.Context, sessionID string, keys []string) error
	SendMouse(ctx context.Context, sessionID string, ev coopmuxapi.MouseEvent) (int, error)
	Resize(ctx context.Context, sessionID string, cols, rows uint16) error
	// Nudge types a message and submits it: how /compact is sent (beads-viewer agent_compact.go).
	Nudge(ctx context.Context, sessionID, message string) error
}

const (
	glassInputMax = 8 * 1024 // typed text in one send, as beads-viewer's /agents/{id}/input caps it
	glassKeysMax  = 32       // named keys in one send
	glassCompact  = "/compact"
)

// GlassAttachTier is who may take the keyboard of an agent on project's land (spec §3.4 #5): an operator on one of the
// operator's own lands, only an admin on a tenant land until Fences (phase 10). A land the farm cannot place (no farm,
// no cost blob yet, a project it does not list) is held to admin.
func GlassAttachTier(f *Farm, project string) glass.Tier {
	if f != nil && project != "" {
		if known, listed := f.KnowsProject(project); listed && known && !f.IsTenantLand(project) {
			return glass.TierOperator
		}
	}
	return glass.TierAdmin
}

// mayAttach reports whether the viewer may take the keyboard of an agent on project: attach is wired (a relay to hold
// the claim, a keyboard to type with), they are a named person, and MayAttach admits them.
func (h *Handler) mayAttach(r *http.Request, project string, who coopmuxapi.Identity) bool {
	g := h.Glass
	return g.Relay != nil && g.Keyboard != nil && g.MayAttach != nil && who.Operator != "" && g.MayAttach(r, project)
}

// GlassControlled is the answer to a control action asked for JSON.
type GlassControlled struct {
	OK      bool   `json:"ok"`
	Action  string `json:"action"` // claim | renew | release
	Result  string `json:"result"` // claimed, taken, renewed, released, held, not_attached, not_live, failed
	Note    string `json:"note"`
	AuditID string `json:"audit_id"`
	// Control is the keyboard as it is after the action: who holds it, "" for nobody.
	Control glass.Control `json:"control"`
}

// glassControl serves POST /garden/glass/{agent}/control.
func (h *Handler) glassControl(w http.ResponseWriter, r *http.Request) {
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
	action := f.get("action")
	if action != "claim" && action != "renew" && action != "release" {
		http.Error(w, "unknown control action", http.StatusBadRequest)
		return
	}
	if !h.mayAttach(r, project, who) {
		http.Error(w, "taking the keyboard of this agent needs "+GlassAttachTier(h.Farm, project).String(), http.StatusForbidden)
		return
	}
	a := glassWrite{h: h, w: w, r: r, b: b, who: who, kind: action, auditID: coopmuxapi.NewAuditID()}
	relay, session := h.Glass.Relay, sessionOf(b)
	answer := func(status int, c GlassControlled) {
		c.Action, c.AuditID = action, a.auditID
		if c.Control.Holder == "" && session != "" {
			c.Control, _ = relay.Holder(session)
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(c)
	}
	if session == "" {
		a.log("not_live")
		answer(http.StatusConflict, GlassControlled{Result: "not_live", Note: "The agent has no live session, so there is no keyboard to take."})
		return
	}
	switch action {
	case "claim":
		c, err := relay.Claim(session, who.Operator, f.get("take") == "1")
		if errors.Is(err, glass.ErrHeld) {
			a.log("held", "holder", c.Holder)
			answer(http.StatusConflict, GlassControlled{Result: "held", Control: c,
				Note: c.Holder + " has the keyboard. Take it from them only if you mean to: they are told."})
			return
		}
		if err != nil {
			a.log("failed", "error", err.Error())
			answer(http.StatusBadRequest, GlassControlled{Result: "failed", Note: "The keyboard could not be taken."})
			return
		}
		a.log(c.Reason, "from", c.From)
		note := "Attached: what you type goes to the agent. Esc Esc lets go."
		if c.Reason == glass.ControlTaken {
			note = "Attached: you took the keyboard from " + c.From + ", who has been told. Esc Esc lets go."
		}
		answer(http.StatusOK, GlassControlled{OK: true, Result: c.Reason, Control: c, Note: note})
	case "renew":
		c, held := relay.Renew(session, who.Operator)
		if !held {
			answer(http.StatusConflict, GlassControlled{Result: "not_attached", Note: notAttachedNote(relay, session)})
			return
		}
		answer(http.StatusOK, GlassControlled{OK: true, Result: "renewed", Control: c})
	case "release":
		if !relay.Release(session, who.Operator) {
			answer(http.StatusOK, GlassControlled{OK: true, Result: "not_attached", Note: "You had let go already."})
			return
		}
		a.log(glass.ControlReleased)
		answer(http.StatusOK, GlassControlled{OK: true, Result: glass.ControlReleased, Note: "Let go: you are watching again."})
	}
}

// notAttachedNote says why a keyboard frame was refused: nobody holds it, or who does.
func notAttachedNote(relay *glass.Relay, session string) string {
	if c, ok := relay.Holder(session); ok {
		return "Not sent: " + c.Holder + " has the keyboard now."
	}
	return "Not sent: you are not attached (the keyboard was let go, or the claim lapsed). Attach again to type."
}

// keyboard answers the keyboard frames (input, keys, resize, compact): only from the person holding the keyboard.
func (a glassWrite) keyboard(project string, get func(string) string) {
	g := a.h.Glass
	if !a.h.mayAttach(a.r, project, a.who) {
		a.log("refused", "reason", "may_not_attach")
		http.Error(a.w, "typing into this agent needs "+GlassAttachTier(a.h.Farm, project).String(), http.StatusForbidden)
		return
	}
	session := sessionOf(a.b)
	if session == "" {
		a.log("not_live")
		a.answer(http.StatusConflict, GlassSent{Result: "not_live", Note: "Not sent: the agent has no live session."})
		return
	}
	if _, held := g.Relay.Renew(session, a.who.Operator); !held {
		a.log("refused", "reason", "needs_control_claim")
		a.answer(http.StatusConflict, GlassSent{Result: "not_attached", Note: notAttachedNote(g.Relay, session)})
		return
	}
	ctx, cancel := context.WithTimeout(a.r.Context(), glassSendTimeout)
	defer cancel()
	switch a.kind {
	case "input":
		text := get("text")
		switch {
		case text == "":
			a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: "Nothing to type."})
		case len(text) > glassInputMax:
			a.answer(http.StatusRequestEntityTooLarge, GlassSent{Result: "failed", Note: "That is too much to type at once: send it in pieces under 8 KB."})
		case strings.ContainsFunc(text, unicode.IsControl):
			a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: "Typed text carries no control characters: send Enter, Tab, Esc and Ctrl chords as keys."})
		default:
			a.wrote(g.Keyboard.SendInput(ctx, session, text), "len", len(text), "snippet", inputSnippet(text, 80))
		}
	case "keys":
		keys, err := parseKeys(get("keys"))
		if err != nil {
			a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: err.Error()})
			return
		}
		if glass.IsDestructiveKey(keys) && !a.confirmed(get, "keys", strings.Join(keys, " "), "keys", keys) {
			return
		}
		a.wrote(g.Keyboard.SendKeys(ctx, session, keys), "keys", keys, "confirmed", get("confirmed") == "1")
	case "mouse":
		ev, err := parseMouse(get("report"))
		if err != nil {
			a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: err.Error()})
			return
		}
		kv := []any{"button", ev.Button, "col", ev.Col, "row", ev.Row, "release", ev.Release}
		n, err := g.Keyboard.SendMouse(ctx, session, ev)
		if err == nil && n == 0 {
			a.log("not_tracking", kv...)
			a.answer(http.StatusOK, GlassSent{OK: true, Result: "not_tracking", Note: "Not sent: the app is not taking the mouse."})
			return
		}
		a.wrote(err, kv...)
	case "resize":
		cols, err1 := strconv.ParseUint(strings.TrimSpace(get("cols")), 10, 16)
		rows, err2 := strconv.ParseUint(strings.TrimSpace(get("rows")), 10, 16)
		if err1 != nil || err2 != nil || cols < 2 || cols > 1000 || rows < 1 || rows > 500 {
			a.answer(http.StatusBadRequest, GlassSent{Result: "failed", Note: "A terminal is 2 to 1000 columns by 1 to 500 rows."})
			return
		}
		a.wrote(g.Keyboard.Resize(ctx, session, uint16(cols), uint16(rows)), "cols", cols, "rows", rows)
	case "compact":
		if !a.confirmed(get, "compact", "/compact") {
			return
		}
		a.wrote(g.Keyboard.Nudge(ctx, session, glassCompact))
	}
}

// confirmed holds a send that asks first: unconfirmed, it is answered 409 "confirm" (what would be sent, in words); a
// cancel is audited and answered "canceled". Either way nothing is sent and false is returned.
func (a glassWrite) confirmed(get func(string) string, what, said string, kv ...any) bool {
	if get("confirmed") == "1" {
		return true
	}
	if get("cancel") == "1" {
		a.log("ux_canceled", kv...)
		a.answer(http.StatusOK, GlassSent{OK: true, Result: "canceled", Note: "Not sent."})
		return false
	}
	a.log("confirm_required", kv...)
	note := "Send " + said + " to " + a.agentName() + "? It can stop what the agent is doing."
	if what == "compact" {
		note = "Compact " + a.agentName() + "'s context? This sends /compact to Claude Code, which rewrites the session's transcript."
	}
	a.answer(http.StatusConflict, GlassSent{Result: "confirm", Note: note})
	return false
}

func (a glassWrite) agentName() string {
	return firstNonEmpty(strings.TrimSpace(a.b.Fields["agent"]), a.b.Title, a.b.ID)
}

// wrote answers a keyboard write coopmux took (err nil) or did not.
func (a glassWrite) wrote(err error, kv ...any) {
	if err != nil {
		a.upstreamFailed(err)
		return
	}
	a.log("ok", kv...)
	a.answer(http.StatusOK, GlassSent{OK: true, Result: "delivered", Note: "Sent."})
}

// parseKeys reads a comma-separated list of key names coop can type.
func parseKeys(raw string) ([]string, error) {
	var keys []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		if !glass.IsKeyName(k) {
			return nil, errors.New("coop cannot type the key " + strconv.Quote(clip(k, 32)))
		}
		keys = append(keys, k)
	}
	switch {
	case len(keys) == 0:
		return nil, errors.New("no keys to send")
	case len(keys) > glassKeysMax:
		return nil, errors.New("too many keys at once")
	}
	return keys, nil
}

// sgrMouse is an SGR mouse report as xterm writes it: ESC [ < button ; column ; row, M for a press, m for a release.
var sgrMouse = regexp.MustCompile(`^\x1b\[<(\d{1,3});(\d{1,4});(\d{1,4})([Mm])$`)

// sgrButtons are the buttons a report's low bits and wheel bit name; motion (bit 32) and the rest are not taken.
var sgrButtons = map[uint64]string{0: "left", 1: "middle", 2: "right", 64: "wheel_up", 65: "wheel_down"}

// parseMouse reads one SGR mouse report into the event coop's input/mouse takes.
func parseMouse(report string) (coopmuxapi.MouseEvent, error) {
	m := sgrMouse.FindStringSubmatch(report)
	if m == nil {
		return coopmuxapi.MouseEvent{}, errors.New("not a mouse report")
	}
	b, _ := strconv.ParseUint(m[1], 10, 8)
	col, _ := strconv.ParseUint(m[2], 10, 16)
	row, _ := strconv.ParseUint(m[3], 10, 16)
	button, ok := sgrButtons[b&^(4|8|16)]
	if !ok || col < 1 || row < 1 || col > 1000 || row > 500 {
		return coopmuxapi.MouseEvent{}, errors.New("coop sends clicks and wheel turns inside the terminal, not this")
	}
	return coopmuxapi.MouseEvent{Button: button, Col: uint16(col), Row: uint16(row), Release: m[4] == "m",
		Shift: b&4 != 0, Alt: b&8 != 0, Ctrl: b&16 != 0}, nil
}

// inputSnippet is typed text for the audit line, one line and clipped, as beads-viewer's agentInputSnippet logs it.
func inputSnippet(text string, n int) string {
	return clip(strings.NewReplacer("\n", " ", "\r", " ").Replace(text), n)
}
