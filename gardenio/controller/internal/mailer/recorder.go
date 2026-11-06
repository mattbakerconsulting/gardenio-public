package mailer

import (
	"context"
	"sync"
)

// Recorder is the mailer fake: it validates each message the way SES would
// and keeps it in memory instead of sending it. It is safe for concurrent use.
type Recorder struct {
	mu   sync.Mutex
	sent []Message
	err  error
}

// NewRecorder returns an empty Recorder.
func NewRecorder() *Recorder { return &Recorder{} }

// Send validates m and records it. When FailWith has set an error, Send
// returns that error and records nothing, the way a failed SES call delivers
// nothing.
func (r *Recorder) Send(_ context.Context, m Message) error {
	if err := Validate(m); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	m.To = append([]string(nil), m.To...)
	r.sent = append(r.sent, m)
	return nil
}

// FailWith makes every later Send return err; nil restores success.
func (r *Recorder) FailWith(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}

// Sent returns a copy of every message recorded so far, oldest first.
func (r *Recorder) Sent() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.sent...)
}

// SentKind returns the recorded messages of one Kind, oldest first.
func (r *Recorder) SentKind(k Kind) []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []Message
	for _, m := range r.sent {
		if m.Kind == k {
			out = append(out, m)
		}
	}
	return out
}

// Reset forgets every recorded message.
func (r *Recorder) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sent = nil
}
