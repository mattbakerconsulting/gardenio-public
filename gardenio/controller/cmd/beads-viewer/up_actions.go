package main

import "html/template"

// up_actions.go — the type-conditional action footer + global keymap model
// for the GV phase-2 /up surface (task ka-0000004641, PRD §4).
//
// The scaffold (up.go) rendered a single placeholder "suggested action" pair.
// This file replaces that with the full PRD §4.3 per-type footer: a primary
// action (Enter) plus up to four secondaries, each mnemonic-keyed, plus the
// PRD §4.2 global keymap. The Go side is the source of truth for BOTH — the
// template renders the footer from it, and up.js reads a JSON island of the
// same model so the keyboard dispatch and the visible buttons can never drift.
//
// What is REAL vs deferred (PRD §10 / Q8 hybrid recommendation C):
//   - Actions that hit an endpoint that exists today (close, comment, reassign,
//     claim/release, decision resolve/dismiss, inbox-pin toggle, reopen) are
//     LIVE: up.js POSTs them and applies the §4.4 result UX.
//   - Actions the PRD marks NEW or sibling-owned (snooze, oracle stack ops,
//     escalate-create, molecule/formula apply, agent nudge/send-key, schedule
//     run-now) are rendered but flagged Deferred: the key is still bound and the
//     button still shows, but dispatch surfaces a "lands with the chatbox/WS
//     phase" toast instead of silently no-op'ing or lying about success. This
//     keeps the footer honest AND lets the keymap compose the moment those
//     endpoints land.

// upActionKind classifies how up.js dispatches an action. The template uses it
// to pick the button affordance; up.js uses it to route the keypress.
type upActionKind string

const (
	// upActionPost issues a form POST to Endpoint and applies the §4.4 result
	// UX (auto-advance when AutoAdvance, else flash-and-stay).
	upActionPost upActionKind = "post"
	// upActionDrawerComment opens the inline comment drawer (§4.4 confirm).
	upActionDrawerComment upActionKind = "drawer-comment"
	// upActionDrawerEscalate opens the single-line escalate-reason drawer.
	upActionDrawerEscalate upActionKind = "drawer-escalate"
	// upActionOverlayReassign opens the assignee-picker overlay.
	upActionOverlayReassign upActionKind = "overlay-reassign"
	// upActionSkip pushes the current card down the stack (§5.2 "not now").
	upActionSkip upActionKind = "skip"
	// upActionBack walks browser card history (§4.2 Esc / ←).
	upActionBack upActionKind = "back"
	// upActionOpen opens the full bead view (new tab for `g`, same tab for the
	// footer "Open" affordance).
	upActionOpen upActionKind = "open"
	// upActionChatbox proxies an intent to the oracle chatbox (§5.2 snooze,
	// §8.2). Deferred until the chatbox lands — dispatch shows the deferred
	// toast today.
	upActionChatbox upActionKind = "chatbox"
	// upActionHelp toggles the keymap help overlay (§4.2 `?`).
	upActionHelp upActionKind = "help"
	// upActionPalette opens the existing command palette (§4.2 cmd-K).
	upActionPalette upActionKind = "palette"
	// upActionFocusChat focuses the chatbox input (§4.2 `/`). Deferred until
	// the chatbox lands.
	upActionFocusChat upActionKind = "focus-chat"
)

// upAction is one footer/keymap entry. It is emitted to the template (for the
// visible footer) and, for the JSON island, to up.js (for keyboard dispatch).
// The JSON tags are the contract up.js reads — keep them stable.
type upAction struct {
	Key   string `json:"key"`   // the keyboard key (e.g. "Enter", "c", "→")
	Label string `json:"label"` // footer button text (e.g. "Comment")
	// Mnemonic is the 0-based index of the underlined character in Label, or
	// -1 when the key isn't a letter in the label (Enter, →). The template
	// underlines Label[Mnemonic] so power users learn the letter.
	Mnemonic int          `json:"mnemonic"`
	Kind     upActionKind `json:"kind"`
	// Method + Endpoint are set for upActionPost. Endpoint is a path template
	// with a literal {id} that up.js substitutes with the card's bead id, so
	// the model never bakes in a specific bead. Empty for non-POST kinds.
	Method   string `json:"method,omitempty"`
	Endpoint string `json:"endpoint,omitempty"`
	// AutoAdvance is true when a successful dispatch takes the bead off the
	// operator's stack (§4.4 row 1 → auto-advance). False keeps the card and
	// flashes ✓ (§4.4 row 2).
	AutoAdvance bool `json:"autoAdvance,omitempty"`
	// Deferred marks an action whose backend is sibling-owned or not-yet-built
	// (PRD §10). The button renders muted; dispatch shows the "lands with the
	// chatbox/WS phase" toast instead of hitting a missing endpoint.
	Deferred bool `json:"deferred,omitempty"`
	// Primary marks the Enter-bound action (rendered emphasized). Exactly one
	// action per footer is Primary, unless the type is read-only.
	Primary bool `json:"primary,omitempty"`
	// Help is the one-liner shown in the `?` overlay next to the key.
	Help string `json:"help,omitempty"`
}

// upFooter is the fully-resolved footer + keymap for one card. Primary is the
// Enter action (nil for read-only types); Secondaries are the ≤4 type
// secondaries shown in the footer in order; Global is the always-available
// keymap (§4.2) shared by every card. ReadOnly types bind Enter to skip.
type upFooter struct {
	Primary     *upAction
	Secondaries []upAction
	Global      []upAction
	ReadOnly    bool
}

// Endpoint path templates. {id} is substituted client-side by up.js. Only the
// endpoints the footer model actually wires live here — reopen/status/labels
// and other overflow actions stay behind cmd-K (PRD §3.2/§4.3), so they are
// deliberately absent.
const (
	upEpClose      = "/b/{id}/close"
	upEpComments   = "/b/{id}/comments"
	upEpAssignee   = "/b/{id}/assignee"
	upEpDecResolve = "/b/{id}/decision/resolve"
	upEpDecDismiss = "/b/{id}/decision/dismiss"
	upEpInboxPin   = "/b/{id}/inbox-pin/toggle"
)

// upMnemonicHTML renders a footer label with the rune at idx underlined
// (PRD §4.3 "mnemonic underlined"). idx < 0 (or out of range) renders the
// label plain. Each segment is HTML-escaped, so an attacker-controlled label
// (there are none today — all labels are constants) still can't inject markup.
func upMnemonicHTML(label string, idx int) template.HTML {
	rs := []rune(label)
	if idx < 0 || idx >= len(rs) {
		return template.HTML(template.HTMLEscapeString(label))
	}
	before := template.HTMLEscapeString(string(rs[:idx]))
	mnem := template.HTMLEscapeString(string(rs[idx]))
	after := template.HTMLEscapeString(string(rs[idx+1:]))
	return template.HTML(before + "<u>" + mnem + "</u>" + after)
}

// upGlobalKeymap is the PRD §4.2 always-available keymap, shared by every card.
// Built once (it is stateless) and returned by value so a caller can append
// per-type keys without mutating the shared slice.
func upGlobalKeymap() []upAction {
	return []upAction{
		{Key: "→", Label: "Skip", Mnemonic: -1, Kind: upActionSkip, Help: "Skip to next, push this card down (also space / j)."},
		{Key: "Esc", Label: "Back", Mnemonic: -1, Kind: upActionBack, Help: "Back to the previous card (also ← / k)."},
		{Key: "c", Label: "Comment", Mnemonic: 0, Kind: upActionDrawerComment, Endpoint: upEpComments, Method: "POST", Help: "Comment (⌘+Enter to submit)."},
		{Key: "r", Label: "Reassign", Mnemonic: 0, Kind: upActionOverlayReassign, Endpoint: upEpAssignee, Method: "POST", Help: "Reassign — assignee picker overlay."},
		{Key: "s", Label: "Snooze", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Snooze (default 1h) — proxied to the oracle chatbox."},
		// Escalate is NOT marked Deferred: the drawer OPENS for real (composes
		// now); only its SUBMIT is deferred (the escalation-create endpoint is
		// sibling-owned — submitEscalate surfaces the phase toast). Marking the
		// action deferred here would make dispatch() short-circuit to a toast
		// before the drawer ever opened.
		{Key: "e", Label: "Escalate", Mnemonic: 0, Kind: upActionDrawerEscalate, Help: "Escalate — single-line reason (submit lands with the escalate endpoint)."},
		{Key: "f", Label: "Pin", Mnemonic: -1, Kind: upActionPost, Endpoint: upEpInboxPin, Method: "POST", Help: "Toggle inbox-pin (★)."},
		{Key: "g", Label: "Open", Mnemonic: -1, Kind: upActionOpen, Help: "Open the full bead view in a new tab."},
		{Key: "?", Label: "Help", Mnemonic: -1, Kind: upActionHelp, Help: "Toggle this keymap overlay."},
		{Key: "/", Label: "Chat", Mnemonic: -1, Kind: upActionFocusChat, Deferred: true, Help: "Focus the oracle chatbox (lands with the chatbox phase)."},
		{Key: "⌘K", Label: "Palette", Mnemonic: -1, Kind: upActionPalette, Help: "Open the command palette."},
	}
}

// disarmUpDecisionEnter takes Enter off a decision's resolve when the card
// shows no context (gd-PNtilz1bgs): one keystroke must never answer a
// question whose background the operator has not seen. Enter opens the full
// bead instead; the option chips stay, each an explicit click.
func disarmUpDecisionEnter(f upFooter) upFooter {
	p := upAction{Key: "Enter", Label: "Open to read first", Mnemonic: -1, Kind: upActionOpen, Primary: true, Help: "This decision carries no context: open the bead and read it before answering."}
	f.Primary = &p
	return f
}

// buildUpFooter resolves the type-conditional footer (PRD §4.3). It returns the
// Enter primary, the ordered secondaries, the shared global keymap, and the
// read-only flag. The per-type secondary keys reuse the disjoint mnemonic set
// from the global map (c/r/s/e) so a power user never relearns a letter across
// types (§4.1 rule 4) — the footer just shows which of them apply first.
//
// Live vs deferred is per-type: e.g. task Close is live (endpoint exists), but
// bug "Open MR" / epic "Pick child" / agent "Nudge" / molecule "Apply" are
// deferred to their sibling endpoints and render muted with the phase toast.
func buildUpFooter(t string, hasDecisionOptions bool) upFooter {
	global := upGlobalKeymap()

	// Shared secondary builders reusing the global keys so the mnemonic set
	// stays disjoint across types (§4.1). Each returns a fresh value.
	comment := upAction{Key: "c", Label: "Comment", Mnemonic: 0, Kind: upActionDrawerComment, Endpoint: upEpComments, Method: "POST", Help: "Comment on this bead."}
	reassign := upAction{Key: "r", Label: "Reassign", Mnemonic: 0, Kind: upActionOverlayReassign, Endpoint: upEpAssignee, Method: "POST", Help: "Reassign to another operator."}
	snooze := upAction{Key: "s", Label: "Snooze", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Snooze (default 1h)."}
	// Escalate opens the drawer for real (not Deferred); only submit is deferred
	// (submitEscalate toasts the sibling-owned endpoint). See upGlobalKeymap.
	escalate := upAction{Key: "e", Label: "Escalate", Mnemonic: 0, Kind: upActionDrawerEscalate, Help: "Escalate with a one-line reason."}

	switch t {
	case "task":
		p := upAction{Key: "Enter", Label: "Close", Mnemonic: -1, Kind: upActionPost, Endpoint: upEpClose, Method: "POST", AutoAdvance: true, Primary: true, Help: "Close this task and advance."}
		return upFooter{Primary: &p, Secondaries: []upAction{comment, reassign, snooze, escalate}, Global: global}

	case "bug":
		p := upAction{Key: "Enter", Label: "Comment + close", Mnemonic: -1, Kind: upActionDrawerComment, Endpoint: upEpComments, Method: "POST", AutoAdvance: true, Primary: true, Help: "Comment then close (the comment is the confirm)."}
		openMR := upAction{Key: "o", Label: "Open MR", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "File / open the fix MR (lands with the MR endpoint)."}
		return upFooter{Primary: &p, Secondaries: []upAction{openMR, reassign, snooze, escalate}, Global: global}

	case "epic":
		p := upAction{Key: "Enter", Label: "Pick child", Mnemonic: -1, Kind: upActionChatbox, Deferred: true, Primary: true, Help: "Drop into the next actionable child (lands with the child-picker)."}
		wrap := upAction{Key: "w", Label: "Wrap & close", Mnemonic: 0, Kind: upActionPost, Endpoint: upEpClose, Method: "POST", AutoAdvance: true, Help: "Close the epic once its children are done."}
		addChild := upAction{Key: "a", Label: "Add child", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Add a child task (lands with the child-picker)."}
		return upFooter{Primary: &p, Secondaries: []upAction{comment, wrap, addChild, snooze}, Global: global}

	case "agent":
		p := upAction{Key: "Enter", Label: "Nudge", Mnemonic: -1, Kind: upActionChatbox, Deferred: true, Primary: true, Help: "Send a nudge to the live agent (lands with the agent verbs)."}
		sendKey := upAction{Key: "y", Label: "Send-key", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Send a named key to the terminal."}
		tail := upAction{Key: "t", Label: "Catch-up tail", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Read the terminal tail."}
		stop := upAction{Key: "x", Label: "Stop", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Stop the agent."}
		return upFooter{Primary: &p, Secondaries: []upAction{sendKey, tail, stop, comment}, Global: global}

	case "decision":
		// Resolve is live (endpoint exists). When the decision carries no
		// parsed options the primary still resolves (free-text response), but
		// the label drops the "[recommended]" hint the chip row supplies.
		label := "Resolve"
		if hasDecisionOptions {
			label = "Resolve: recommended"
		}
		p := upAction{Key: "Enter", Label: label, Mnemonic: -1, Kind: upActionPost, Endpoint: upEpDecResolve, Method: "POST", AutoAdvance: true, Primary: true, Help: "Resolve with the recommended option, or pick another below."}
		dismiss := upAction{Key: "d", Label: "Dismiss", Mnemonic: 0, Kind: upActionPost, Endpoint: upEpDecDismiss, Method: "POST", AutoAdvance: true, Help: "Dismiss the decision with a reason."}
		return upFooter{Primary: &p, Secondaries: []upAction{dismiss, comment, snooze}, Global: global}

	case "escalation":
		// Acknowledge/Resolve go through the escalation approve/reject verbs
		// which the /up surface does not yet proxy (PRD §10 "verify / NEW").
		p := upAction{Key: "Enter", Label: "Acknowledge", Mnemonic: -1, Kind: upActionChatbox, Deferred: true, Primary: true, Help: "Acknowledge and route (lands with the escalation verbs)."}
		resolve := upAction{Key: "v", Label: "Resolve", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Resolve in the linked review surface."}
		return upFooter{Primary: &p, Secondaries: []upAction{resolve, reassign, comment, snooze}, Global: global}

	case "molecule", "formula":
		p := upAction{Key: "Enter", Label: "Apply", Mnemonic: -1, Kind: upActionChatbox, Deferred: true, Primary: true, Help: "Run the next pending step (lands with the apply endpoint)."}
		inspect := upAction{Key: "i", Label: "Inspect", Mnemonic: 0, Kind: upActionOpen, Help: "Inspect the plan on the full bead view."}
		return upFooter{Primary: &p, Secondaries: []upAction{inspect, comment, snooze}, Global: global}

	case "schedule", "trigger":
		p := upAction{Key: "Enter", Label: "Toggle enabled", Mnemonic: -1, Kind: upActionChatbox, Deferred: true, Primary: true, Help: "Enable / disable this trigger (lands with the toggle proxy)."}
		editCron := upAction{Key: "n", Label: "Edit cron", Mnemonic: -1, Kind: upActionOpen, Help: "Edit the cron on the full bead view."}
		runNow := upAction{Key: "u", Label: "Run now", Mnemonic: 0, Kind: upActionChatbox, Deferred: true, Help: "Fire this trigger now (lands with the run-now endpoint)."}
		return upFooter{Primary: &p, Secondaries: []upAction{editCron, runNow, comment}, Global: global}

	case "message", "squawk", "doc", "bundle":
		// Read-only (§5.3): no primary. Enter is bound to skip client-side.
		return upFooter{Primary: nil, Secondaries: nil, Global: global, ReadOnly: true}

	default:
		// Unknown type: a generic Close primary so the footer is never empty.
		p := upAction{Key: "Enter", Label: "Close", Mnemonic: -1, Kind: upActionPost, Endpoint: upEpClose, Method: "POST", AutoAdvance: true, Primary: true, Help: "Close this bead and advance."}
		return upFooter{Primary: &p, Secondaries: []upAction{comment, reassign, snooze}, Global: global}
	}
}
