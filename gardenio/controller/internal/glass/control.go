package glass

// Who has the keyboard (spec §3.2 GL-9, §6 step 6; gd-mWooPMTNwA). Attach is
// an explicit act: a person claims a session's keyboard, every other watcher
// of the session is told who holds it ("attached as <them>"), and only the
// holder's typed input, named keys and resizes go on. The claim is the
// garden's own: coop's PTY takes any write, and coopmux checks only the tier.
//
// A claim is a lease. The holder renews it with every frame they send, and
// their Glass renews it while it is open; one nobody renews (a closed laptop,
// a lost tab) lapses on its own and every watcher is told. Letting go (Esc
// Esc, a blur timeout, closing the Glass) ends it at once. Another person may
// take a held keyboard only by saying so (take): the one it was taken from is
// told who has it now.
//
// Claims live in the relay, beside the watchers they are announced to, so a
// watcher who joins while a claim is held hears of it with its first events.
// Like the relay's upstreams they are in-process: beads-viewer runs as one
// replica, and a restart lets every keyboard go.

import (
	"encoding/json"
	"errors"
	"time"
)

// EventControl is who holds a session's keyboard, sent to every watcher when
// it changes and to a new watcher when one is held: a Control.
const EventControl = "control"

// DefaultLease is how long a claim lasts without a renewal.
const DefaultLease = 90 * time.Second

// Why a claim changed hands, the Control's Reason.
const (
	ControlClaimed  = "claimed"
	ControlTaken    = "taken"
	ControlReleased = "released"
	ControlExpired  = "expired"
)

// ErrHeld is a claim refused because another person holds the keyboard.
var ErrHeld = errors.New("glass: another person has the keyboard")

// Control is a session's keyboard as watchers are told it: Holder is the
// person attached, "" when nobody is.
type Control struct {
	Holder string    `json:"holder,omitempty"`
	Since  time.Time `json:"since,omitzero"`
	// Until is when the claim lapses unless it is renewed.
	Until time.Time `json:"until,omitzero"`
	// Reason is why it last changed: claimed, taken, released or expired.
	Reason string `json:"reason,omitempty"`
	// From is who held it before, on a taken or a let-go keyboard.
	From string `json:"from,omitempty"`
}

// claim is a held keyboard and the timer that lapses it.
type claim struct {
	c     Control
	timer *time.Timer
}

func (r *Relay) lease() time.Duration {
	if r.Lease > 0 {
		return r.Lease
	}
	return DefaultLease
}

// Holder is who has session's keyboard now; ok is false when nobody does.
func (r *Relay) Holder(session string) (c Control, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cl := r.controls[session]; cl != nil {
		return cl.c, true
	}
	return Control{}, false
}

// Claim gives operator session's keyboard. A claim by the holder renews it.
// One held by another person is refused with ErrHeld and the claim as it is,
// unless take is set: then it is taken from them.
func (r *Relay) Claim(session, operator string, take bool) (Control, error) {
	if operator == "" {
		return Control{}, errors.New("glass: a claim needs a person")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	cl := r.controls[session]
	if cl != nil && cl.c.Holder == operator {
		r.renewLocked(session, cl)
		return cl.c, nil
	}
	if cl != nil && !take {
		return cl.c, ErrHeld
	}
	now := time.Now()
	c := Control{Holder: operator, Since: now, Until: now.Add(r.lease()), Reason: ControlClaimed}
	if cl != nil {
		cl.timer.Stop()
		c.Reason, c.From = ControlTaken, cl.c.Holder
	}
	cl = &claim{c: c}
	r.controls[session] = cl
	cl.timer = time.AfterFunc(r.lease(), func() { r.lapse(session, cl) })
	r.announceLocked(session, c)
	return c, nil
}

// Renew extends operator's claim on session's keyboard; false when they do
// not hold it (it lapsed, was let go, or was taken).
func (r *Relay) Renew(session, operator string) (Control, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cl := r.controls[session]
	if cl == nil || operator == "" || cl.c.Holder != operator {
		return Control{}, false
	}
	r.renewLocked(session, cl)
	return cl.c, true
}

func (r *Relay) renewLocked(session string, cl *claim) {
	cl.c.Until = time.Now().Add(r.lease())
	cl.timer.Reset(r.lease())
}

// Release lets go of operator's claim on session's keyboard; false when they
// did not hold it.
func (r *Relay) Release(session, operator string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cl := r.controls[session]
	if cl == nil || operator == "" || cl.c.Holder != operator {
		return false
	}
	cl.timer.Stop()
	delete(r.controls, session)
	r.announceLocked(session, Control{Reason: ControlReleased, From: operator})
	return true
}

// lapse ends a claim nobody renewed, unless it has since changed hands.
func (r *Relay) lapse(session string, cl *claim) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.controls[session] != cl || time.Now().Before(cl.c.Until) {
		return // let go, taken, or renewed while the timer fired
	}
	delete(r.controls, session)
	r.announceLocked(session, Control{Reason: ControlExpired, From: cl.c.Holder})
}

// announceLocked tells every watcher of session who has its keyboard. Called
// with r.mu held; it takes the feed's lock after, the order Watch takes them.
func (r *Relay) announceLocked(session string, c Control) {
	f := r.feeds[session]
	if f == nil {
		return
	}
	ev := controlEvent(c)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.control = nil
	if c.Holder != "" {
		f.control = ev.Data
	}
	for w := range f.watchers {
		if !w.offer(ev) {
			w.reset(append(f.resyncEvents(), ev)...)
		}
	}
}

func controlEvent(c Control) Event {
	b, _ := json.Marshal(c)
	return Event{Kind: EventControl, ID: -1, Data: b}
}
