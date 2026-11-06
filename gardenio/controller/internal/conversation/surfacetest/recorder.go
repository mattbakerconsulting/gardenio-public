// Package surfacetest holds the test doubles for conversation.Surface: a
// Recorder fake, a twin of the slackmock fixture's Web API, and the contract
// every Surface implementation must pass (RunContract).
package surfacetest

import (
	"context"
	"fmt"
	"sync"

	"gardenio/controller/internal/conversation"
)

// Call is one recorded Surface call.
type Call struct {
	Op        string // Post | Reply | Update | Delete | React | DM | Ephemeral | OpenModal
	Channel   conversation.ChannelRef
	At        conversation.Ref // thread root (Reply) or target (Update/Delete/React)
	Message   conversation.Message
	Broadcast bool
	Reaction  string
	On        bool
	To        conversation.Principal
	Trigger   conversation.Trigger
	Modal     conversation.Modal
	Ref       conversation.Ref // what the call returned
}

// Shown is the state of one message the Recorder holds, as a surface would
// show it.
type Shown struct {
	Ref       conversation.Ref
	ThreadTS  string // root ts when the message is a reply
	Text      string
	Kind      conversation.Kind
	SubjectID string
	Deleted   bool
	Reactions map[string]bool
}

// Recorder is an in-memory conversation.Surface. It records every call, keeps
// what each message would show, and can be told to fail an operation.
type Recorder struct {
	name string

	mu    sync.Mutex
	seq   int
	calls []Call
	shown map[conversation.Ref]*Shown
	fail  map[string]error
}

var _ conversation.Surface = (*Recorder)(nil)

// NewRecorder returns a Recorder that answers to name ("slack", "commons", or
// anything else for a test-only surface).
func NewRecorder(name string) *Recorder {
	return &Recorder{name: name, shown: map[conversation.Ref]*Shown{}, fail: map[string]error{}}
}

// Fail makes every later call of op return err (nil clears it).
func (r *Recorder) Fail(op string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		delete(r.fail, op)
		return
	}
	r.fail[op] = err
}

// Calls returns a copy of the recorded calls.
func (r *Recorder) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// CallsOf returns the recorded calls of op.
func (r *Recorder) CallsOf(op string) []Call {
	var out []Call
	for _, c := range r.Calls() {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// Lookup returns what the message at ref shows.
func (r *Recorder) Lookup(ref conversation.Ref) (Shown, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.shown[ref]
	if !ok {
		return Shown{}, false
	}
	out := *s
	out.Reactions = map[string]bool{}
	for k, v := range s.Reactions {
		out.Reactions[k] = v
	}
	return out, true
}

// Name implements conversation.Surface.
func (r *Recorder) Name() string { return r.name }

func (r *Recorder) record(c Call) error {
	r.calls = append(r.calls, c)
	return r.fail[c.Op]
}

// mint shows a new message. A "commons" Recorder names a copy of a recorded
// message by its bead id, as the Commons does; every other copy gets a
// Slack-style ts.
func (r *Recorder) mint(channel, threadTS string, m conversation.Message) conversation.Ref {
	r.seq++
	ref := conversation.Ref{Surface: r.name, Channel: channel, TS: fmt.Sprintf("1700000000.%06d", r.seq)}
	if r.name == conversation.SurfaceCommons && m.BeadID != "" {
		ref.TS = m.BeadID
	}
	r.shown[ref] = &Shown{Ref: ref, ThreadTS: threadTS, Text: m.Text, Kind: m.KindOrText(), SubjectID: m.SubjectID, Reactions: map[string]bool{}}
	return ref
}

func (r *Recorder) live(at conversation.Ref) (*Shown, error) {
	if at.IsZero() {
		return nil, conversation.ErrNoRef
	}
	s, ok := r.shown[at]
	if !ok || s.Deleted {
		return nil, fmt.Errorf("%s: message %s/%s not found", r.name, at.Channel, at.TS)
	}
	return s, nil
}

// Post implements conversation.Surface.
func (r *Recorder) Post(_ context.Context, ch conversation.ChannelRef, m conversation.Message) (conversation.Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := Call{Op: "Post", Channel: ch, Message: m}
	if err := r.record(c); err != nil {
		return conversation.Ref{}, err
	}
	if m.Empty() {
		return conversation.Ref{}, conversation.ErrEmptyMessage
	}
	id := ch.For(r.name)
	if id == "" {
		return conversation.Ref{}, conversation.ErrNoChannel
	}
	ref := r.mint(id, "", m)
	r.calls[len(r.calls)-1].Ref = ref
	return ref, nil
}

// Reply implements conversation.Surface.
func (r *Recorder) Reply(_ context.Context, thread conversation.Ref, m conversation.Message, broadcast bool) (conversation.Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "Reply", At: thread, Message: m, Broadcast: broadcast}); err != nil {
		return conversation.Ref{}, err
	}
	if m.Empty() {
		return conversation.Ref{}, conversation.ErrEmptyMessage
	}
	if thread.IsZero() {
		return conversation.Ref{}, conversation.ErrNoRef
	}
	ref := r.mint(thread.Channel, thread.TS, m)
	r.calls[len(r.calls)-1].Ref = ref
	return ref, nil
}

// Update implements conversation.Surface.
func (r *Recorder) Update(_ context.Context, at conversation.Ref, m conversation.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "Update", At: at, Message: m}); err != nil {
		return err
	}
	if m.Empty() {
		return conversation.ErrEmptyMessage
	}
	s, err := r.live(at)
	if err != nil {
		return err
	}
	s.Text, s.Kind, s.SubjectID = m.Text, m.KindOrText(), m.SubjectID
	return nil
}

// Delete implements conversation.Surface.
func (r *Recorder) Delete(_ context.Context, at conversation.Ref) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "Delete", At: at}); err != nil {
		return err
	}
	s, err := r.live(at)
	if err != nil {
		return err
	}
	s.Deleted = true
	return nil
}

// React implements conversation.Surface.
func (r *Recorder) React(_ context.Context, at conversation.Ref, reaction string, on bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "React", At: at, Reaction: reaction, On: on}); err != nil {
		return err
	}
	s, err := r.live(at)
	if err != nil {
		return err
	}
	if reaction == "" {
		return fmt.Errorf("%s: empty reaction", r.name)
	}
	if on {
		s.Reactions[reaction] = true
	} else {
		delete(s.Reactions, reaction)
	}
	return nil
}

// DM implements conversation.Surface. The DM channel of a principal is
// "dm:<principal id>".
func (r *Recorder) DM(_ context.Context, to conversation.Principal, m conversation.Message) (conversation.Ref, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "DM", To: to, Message: m}); err != nil {
		return conversation.Ref{}, err
	}
	if m.Empty() {
		return conversation.Ref{}, conversation.ErrEmptyMessage
	}
	if to.ID == "" {
		return conversation.Ref{}, conversation.ErrNoRecipient
	}
	ref := r.mint("dm:"+to.ID, "", m)
	r.calls[len(r.calls)-1].Ref = ref
	return ref, nil
}

// Ephemeral implements conversation.Surface.
func (r *Recorder) Ephemeral(_ context.Context, t conversation.Trigger, to conversation.Principal, m conversation.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "Ephemeral", Trigger: t, To: to, Message: m}); err != nil {
		return err
	}
	if m.Empty() {
		return conversation.ErrEmptyMessage
	}
	if t.Surface != r.name || t.Channel == "" {
		return conversation.ErrNoTrigger
	}
	if to.ID == "" {
		return conversation.ErrNoRecipient
	}
	return nil
}

// OpenModal implements conversation.Surface.
func (r *Recorder) OpenModal(_ context.Context, t conversation.Trigger, v conversation.Modal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.record(Call{Op: "OpenModal", Trigger: t, Modal: v}); err != nil {
		return err
	}
	if t.Surface != r.name {
		return conversation.ErrNoTrigger
	}
	return v.Validate()
}
