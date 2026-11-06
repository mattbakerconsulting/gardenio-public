package commons

import (
	"context"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Typing (Commons III, spec sections 2.6 and 7.3 step 4.4) is who is writing
// in a channel or a thread right now. Like presence it is in-memory state,
// never a bead, and it lives on Presence: a person may signal typing only on
// a channel where they hold an event stream (Present), and the change rides
// the same hub, cut per subscriber by land and never replayed:
//
//	event: typing
//	data: {"id":"<channel id>","channel":"<channel id>","thread":"<root id>","land":"<land>"}
//
// thread is absent for the channel's own composer. An event is sent only when
// someone starts or stops typing there, never per keystroke; the client
// refetches GET /garden/c/{land}/{channel}/typing[?thread=<root id>].
//
// The composer signals with POST /garden/c/{land}/{channel}/typing (form:
// thread, and stop=1 when it is emptied). One signal keeps its sender typing
// for TypingFor. Signals are rate-limited per principal, which covers every
// connection it holds. A principal on a spot has two composers, as the pane
// does: the channel's and one thread's (the drawer). Each gets at most one
// signal through per TypingGap, however many tabs are open, and a faster one
// answers 429 with Retry-After. A signal in another thread moves the thread
// line there, inside the same limit: keying the limit on the thread id would
// let a client that names a fresh id per request through every time, each one
// a typing event to every subscriber in the land. Sending the message stops
// it too (write.go).

const (
	// typingFor is the default TypingFor: a composer re-signals every 3 s
	// while its text changes, so a pause longer than this ends the line.
	typingFor = 6 * time.Second
	// typingGap is the default TypingGap.
	typingGap = 2 * time.Second
	// typingShown caps the names one typing line says.
	typingShown = 3
	// typingFormMax caps the typing form.
	typingFormMax = 1 << 10
	// typingNameMax cuts one name in the line, so a long one cannot push
	// "is typing" off the end (the rubric's F1).
	typingNameMax = 20
)

// typingKey is one of a principal's two composers on a spot: the channel's,
// or (InThread) whichever thread it last typed in.
type typingKey struct {
	presenceKey
	InThread bool
}

func typingAt(who string, at Spot, thread string) typingKey {
	return typingKey{presenceKey{Who: who, Spot: at}, thread != ""}
}

// typist is one principal typing at one spot, in its channel or one thread.
type typist struct {
	thread string    // "" for the channel's own composer
	until  time.Time // typing until then; zero once stopped
	last   time.Time // the last signal let through, for the rate limit
	timer  *time.Timer
}

func (p *Presence) typingTimes() (forDur, gap time.Duration) {
	forDur, gap = p.TypingFor, p.TypingGap
	if forDur <= 0 {
		forDur = typingFor
	}
	if gap <= 0 {
		gap = typingGap
	}
	return forDur, gap
}

// Type records that who is typing at the spot, in thread ("" for the
// channel's own composer). The first signal publishes; later ones only extend
// it, and one in another thread moves the thread line there, publishing both.
// ok is false, with how long to wait, when the signal comes sooner than
// TypingGap after the last one let through from the same composer: the
// channel's, or any thread's. Nil-safe.
func (p *Presence) Type(who string, at Spot, thread string) (ok bool, retry time.Duration) {
	if p == nil || who == "" || at.Land == "" || at.Channel == "" {
		return true, 0
	}
	forDur, gap := p.typingTimes()
	k := typingAt(who, at, thread)
	now := p.now()
	p.mu.Lock()
	t := p.typing[k]
	if t != nil && now.Sub(t.last) < gap {
		p.mu.Unlock()
		return false, gap - now.Sub(t.last)
	}
	typing := t != nil && !t.until.IsZero() && now.Before(t.until)
	moved, from := typing && t.thread != thread, ""
	if t == nil {
		t = &typist{timer: time.AfterFunc(forDur, func() { p.expire(k) })}
		p.typing[k] = t
	} else {
		from = t.thread
		t.timer.Reset(forDur)
	}
	t.thread, t.last, t.until = thread, now, now.Add(forDur)
	p.mu.Unlock()
	if moved {
		p.publishTyping(at, from)
	}
	if !typing || moved {
		p.publishTyping(at, thread)
	}
	return true, 0
}

// StopTyping ends who's typing at the spot and thread, when it was: the
// composer was emptied or its message sent. The rate limit still counts the
// last signal, so stopping and starting again is no faster. Nil-safe.
func (p *Presence) StopTyping(who string, at Spot, thread string) {
	if p == nil {
		return
	}
	_, gap := p.typingTimes()
	p.mu.Lock()
	t := p.typing[typingAt(who, at, thread)]
	if t == nil || t.until.IsZero() || t.thread != thread {
		p.mu.Unlock()
		return
	}
	t.until = time.Time{}
	t.timer.Reset(max(0, gap-p.now().Sub(t.last)))
	p.mu.Unlock()
	p.publishTyping(at, thread)
}

// expire forgets a typist once its line has run out and the rate limit no
// longer needs its last signal, publishing when it was still typing.
func (p *Presence) expire(k typingKey) {
	_, gap := p.typingTimes()
	p.mu.Lock()
	t := p.typing[k]
	if t == nil {
		p.mu.Unlock()
		return
	}
	due := t.until
	if due.IsZero() {
		due = t.last.Add(gap)
	}
	if now := p.now(); now.Before(due) {
		t.timer.Reset(due.Sub(now)) // extended since this timer was set
		p.mu.Unlock()
		return
	}
	typing, thread := !t.until.IsZero(), t.thread
	delete(p.typing, k)
	p.mu.Unlock()
	if typing {
		p.publishTyping(k.Spot, thread)
	}
}

func (p *Presence) publishTyping(at Spot, thread string) {
	if p.publish == nil {
		return
	}
	p.publish(Invalidation{Kind: "typing", ID: at.Channel, Channel: at.Channel, Thread: thread, Land: at.Land, At: p.now()})
}

// Typing returns who is typing at the spot and thread, sorted by name.
// Nil-safe.
func (p *Presence) Typing(at Spot, thread string) []string {
	if p == nil {
		return nil
	}
	now := p.now()
	var out []string
	p.mu.Lock()
	for k, t := range p.typing {
		if k.Spot == at && t.thread == thread && now.Before(t.until) {
			out = append(out, k.Who)
		}
	}
	p.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return authorName(out[i]) < authorName(out[j]) })
	return out
}

// typingThread reads the thread a typing request names: "" for the channel,
// else a bead id. ok is false for something that cannot be one.
func typingThread(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if len(s) > 200 || strings.ContainsAny(s, " \t\r\n/?#") {
		return "", false
	}
	return s, true
}

// typingSignal serves POST /garden/c/{land}/{channel}/typing: the viewer is
// typing in the channel (or, with thread, in that thread), or stopped (stop=1).
// It answers 204; 409 when the viewer holds no stream on the channel, 429 past
// the rate limit.
func (h *Handler) typingSignal(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) || !h.writable(w, r) {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, typingFormMax)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	thread, ok := typingThread(r.PostForm.Get("thread"))
	if !ok {
		http.Error(w, "bad thread id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	action := ActionSay
	if thread != "" {
		action = ActionReply
	}
	if !h.may(r, ch, action) {
		h.refuse(w, ch, action)
		return
	}
	who, at := h.viewer(r), Spot{Land: ch.Land, Channel: ch.ID}
	if r.PostForm.Get("stop") != "" {
		h.Presence.StopTyping(who, at, thread)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if who == "" || !h.Presence.Present(who, at) {
		http.Error(w, "Open #"+ch.Name+" to say you are typing there.", http.StatusConflict)
		return
	}
	if ok, retry := h.Presence.Type(who, at, thread); !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int((retry+time.Second-1)/time.Second)))
		http.Error(w, "Too many typing signals.", http.StatusTooManyRequests)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// typingLine serves GET /garden/c/{land}/{channel}/typing[?thread=<root id>]:
// who else is typing there, as the line above the composer says it.
func (h *Handler) typingLine(w http.ResponseWriter, r *http.Request) {
	if !h.ready(w) {
		return
	}
	thread, ok := typingThread(r.URL.Query().Get("thread"))
	if !ok {
		http.Error(w, "bad thread id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	ch, err := readChannel(ctx, h.Beads, h.scope(r), r.PathValue("land"), r.PathValue("channel"))
	if err != nil {
		h.fail(w, err)
		return
	}
	h.write(w, "typing", struct {
		Channel Channel
		Thread  string
		Line    string
	}{ch, thread, h.typingOf(r, ch, thread)})
}

// typingOf is the line of who other than the viewer is typing in ch (in
// thread, when set): the route's answer, and what the stream's typing event
// carries.
func (h *Handler) typingOf(r *http.Request, ch Channel, thread string) string {
	self := h.viewer(r)
	var names []string
	for _, who := range h.Presence.Typing(Spot{Land: ch.Land, Channel: ch.ID}, thread) {
		if who != self {
			names = append(names, clipRunes(authorName(who), typingNameMax))
		}
	}
	return typingWords(names)
}

// typingWords says who is typing: "ann is typing…", "ann and bo are
// typing…", "ann, bo and cy are typing…", then "Several people are typing…".
func typingWords(names []string) string {
	switch n := len(names); {
	case n == 0:
		return ""
	case n == 1:
		return names[0] + " is typing…"
	case n > typingShown:
		return "Several people are typing…"
	default:
		return strings.Join(names[:n-1], ", ") + " and " + names[n-1] + " are typing…"
	}
}

func (h *Handler) viewer(r *http.Request) string {
	if h.Viewer == nil {
		return ""
	}
	return h.Viewer(r)
}

// The typing fragment is the slot's content: empty when nobody else types, so
// the slot (and its :empty rule) folds away.
var _ = template.Must(fragments.New("typing").Parse(
	`<p class="cm-typing" data-commons="typing" data-land="{{.Channel.Land}}" data-channel="{{.Channel.ID}}"{{if .Thread}} data-thread="{{.Thread}}"{{end}}>{{.Line}}</p>
`))
