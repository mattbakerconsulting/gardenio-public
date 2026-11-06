package conversation

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

// InboundKind is what a human did on a surface.
type InboundKind string

const (
	InboundCompose InboundKind = "compose" // a new top-level post (a mention, in Slack)
	InboundReply   InboundKind = "reply"   // a reply in a thread
	InboundAction  InboundKind = "action"  // a button or select on a card
	InboundSubmit  InboundKind = "submit"  // a modal submission
	InboundReact   InboundKind = "react"   // a reaction added
	InboundJoin    InboundKind = "join"    // the bot or a person joined a channel
)

// ReactionDone is the reaction that marks a wish done: the Commons' Done
// button, and Slack's ⏹️ (black_square_for_stop) on a thread card, which the
// Slack adapter names so. Every other Slack reaction stays the Slack
// adapter's (reaction rules), never an Inbound (design section 5.2, slice 4b).
const ReactionDone = "done"

// Inbound is one surface event, normalised. Every surface adapter turns its
// own events into this value, and one dispatcher runs the handlers that exist
// today against it (design section 3.4).
type Inbound struct {
	Kind     InboundKind
	Surface  string
	Land     string    // project
	Channel  string    // channel bead id
	Thread   string    // thread head bead id ("" for a top-level post)
	Message  string    // the message bead id the human's words were written to
	Actor    Principal // oidc:<email> (Slack: resolved through the account link)
	Text     string
	Files    []File
	ActionID string // Action/Submit: today's Slack action_id / callback_id
	Value    string
	Values   map[string]string // Submit, keyed on Field.ID
	Reaction string
	Trigger  Trigger
	// At is the surface ref of the message the event happened on (the card a
	// button was clicked on, the message a reaction was added to), so a
	// handler can update or reply to it without a lookup.
	At Ref
	// Roots are the thread root's copies the adapter already knows (a Slack
	// reply: its channel and thread_ts), so a handler still keyed on surface
	// refs needs no lookup. Thread bindings move to the head id in slice 4d.
	Roots []Ref
	// Spawns is a Commons reply's leave to start or wake an agent with its
	// words: set by the inbound call once it has checked the sender's land
	// role, never by the watchers or the sweep, which have no caller. Without
	// it a Commons reply only reaches a live agent.
	Spawns bool
	// Raw is the surface's own event, for a handler that still answers in
	// that surface's own terms (Slack: the slack.InteractionCallback of a
	// button or a modal, whose modals and ephemerals Slack shapes). Nil from
	// the Commons.
	Raw any
}

// Root returns the thread root's copy on the named surface, when the adapter
// supplied one.
func (in Inbound) Root(surface string) (Ref, bool) {
	for _, r := range in.Roots {
		if r.Surface == surface && !r.IsZero() {
			return r, true
		}
	}
	return Ref{}, false
}

// Errors Dispatch returns.
var (
	ErrNoHandler    = errors.New("conversation: no handler for this inbound")
	ErrNoThread     = errors.New("conversation: reply names no thread")
	ErrNoAction     = errors.New("conversation: action or submission names no action id")
	ErrNoReaction   = errors.New("conversation: reaction names no reaction")
	ErrHandlerPanic = errors.New("conversation: inbound handler panicked")
)

// Validate reports whether in carries what its kind needs to be dispatched.
func (in Inbound) Validate() error {
	if in.Kind == "" || in.Surface == "" {
		return fmt.Errorf("conversation: inbound needs a kind and a surface: %w", ErrNoHandler)
	}
	if in.Kind == InboundReply && in.Thread == "" && len(in.Roots) == 0 {
		return ErrNoThread
	}
	if (in.Kind == InboundAction || in.Kind == InboundSubmit) && in.ActionID == "" {
		return ErrNoAction
	}
	if in.Kind == InboundReact && in.Reaction == "" {
		return ErrNoReaction
	}
	return nil
}

// Answer is what a handler gives back to the surface the Inbound came from.
// Ephemerals and modals are never beads (C5): the Slack surface sends them
// itself, the Commons returns them in the response to its inbound call.
type Answer struct {
	Ephemeral []Message
	Modal     *Modal
}

// Handler runs one kind of Inbound.
type Handler func(ctx context.Context, in Inbound) (Answer, error)

// Dispatcher runs the handler registered for an Inbound's kind. It is the one
// entry point every surface adapter calls, and the panic boundary for the
// handlers under it: a panic becomes ErrHandlerPanic, never a crashed bridge.
type Dispatcher struct {
	mu       sync.RWMutex
	handlers map[InboundKind]Handler
	onPanic  func(in Inbound, v any, stack []byte)
}

// NewDispatcher returns an empty dispatcher. onPanic, when non-nil, is told
// about every recovered handler panic (the bridge logs and counts it).
func NewDispatcher(onPanic func(in Inbound, v any, stack []byte)) *Dispatcher {
	return &Dispatcher{handlers: map[InboundKind]Handler{}, onPanic: onPanic}
}

// Handle registers h for kind, replacing any earlier handler.
func (d *Dispatcher) Handle(kind InboundKind, h Handler) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.handlers[kind] = h
}

// Dispatch validates in and runs the handler for its kind.
func (d *Dispatcher) Dispatch(ctx context.Context, in Inbound) (a Answer, err error) {
	if err := in.Validate(); err != nil {
		return Answer{}, err
	}
	d.mu.RLock()
	h := d.handlers[in.Kind]
	d.mu.RUnlock()
	if h == nil {
		return Answer{}, fmt.Errorf("%w: %s", ErrNoHandler, in.Kind)
	}
	defer func() {
		if v := recover(); v != nil {
			if d.onPanic != nil {
				d.onPanic(in, v, debug.Stack())
			}
			a, err = Answer{}, fmt.Errorf("%w: %s: %v", ErrHandlerPanic, in.Kind, v)
		}
	}()
	return h(ctx, in)
}
