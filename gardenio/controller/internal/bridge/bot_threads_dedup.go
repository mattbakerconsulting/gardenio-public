package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// threadReplyDedupWindow is how long an identical (channel, thread_ts, text)
// thread reply is collapsed after one is posted. Five minutes comfortably
// covers the observed pod-churn burst pattern (ka-0000002080: 3 bursts of 4
// identical "interrupted" breadcrumbs, ~90s apart, all inside ~4.5 min) while
// staying short enough that a genuinely repeated message eventually goes
// through.
const threadReplyDedupWindow = 5 * time.Minute

// threadReplyDeduper collapses identical thread replies that arrive within
// threadReplyDedupWindow of each other (ka-0000002080). It is the bridge-side
// backstop for controller-side idempotency races: N pod incarnations being
// torn down near-simultaneously each believe they are the first to post an
// "interrupted" breadcrumb (their bead-field dedup marker is a non-atomic
// read-modify-write across separate processes), and all N POSTs land here
// within a second. The bridge is the single process every reply funnels
// through, so a reservation taken under one lock is the only dedup that
// actually holds under that concurrency.
//
// Reservation semantics: begin() claims the (channel, thread_ts, text) slot
// BEFORE the Slack API call, so concurrent identical requests collapse even
// while the winning post is still in flight. commit() records the winner's
// Slack timestamp; abort() releases the slot after a failed post so a retry
// is not wrongly suppressed.
type threadReplyDeduper struct {
	mu      sync.Mutex
	window  time.Duration
	now     func() time.Time // injectable for tests
	entries map[string]*threadReplyEntry
}

// threadReplyEntry is one reserved/posted reply slot.
type threadReplyEntry struct {
	at time.Time // when the slot was reserved
	ts string    // Slack timestamp of the winning post ("" while in flight)
}

func newThreadReplyDeduper(window time.Duration) *threadReplyDeduper {
	return &threadReplyDeduper{
		window:  window,
		now:     time.Now,
		entries: make(map[string]*threadReplyEntry),
	}
}

// threadReplyKey identifies a reply for dedup purposes. The text is hashed so
// the map's memory stays bounded by the number of distinct replies per window,
// not by message size.
func threadReplyKey(channel, threadTS, text string) string {
	sum := sha256.Sum256([]byte(text))
	return channel + "\x00" + threadTS + "\x00" + hex.EncodeToString(sum[:])
}

// begin reserves the (channel, thread_ts, text) slot. If an identical reply
// was already reserved or posted inside the window it reports dup=true along
// with the earlier post's Slack timestamp (empty if that post is still in
// flight), and the caller must NOT post. Otherwise the slot is reserved and
// the caller must follow up with commit (post succeeded) or abort (post
// failed).
func (d *threadReplyDeduper) begin(channel, threadTS, text string) (key string, dup bool, prevTS string) {
	key = threadReplyKey(channel, threadTS, text)
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	// Lazy prune: expired slots are dropped on each reservation attempt so the
	// map stays bounded without a background sweeper.
	for k, e := range d.entries {
		if now.Sub(e.at) > d.window {
			delete(d.entries, k)
		}
	}
	if e, ok := d.entries[key]; ok {
		return key, true, e.ts
	}
	d.entries[key] = &threadReplyEntry{at: now}
	return key, false, ""
}

// commit records the Slack timestamp of the winning post on its reserved slot
// so later deduped callers can return it.
func (d *threadReplyDeduper) commit(key, ts string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.entries[key]; ok {
		e.ts = ts
	}
}

// abort releases a reserved slot after a failed Slack post so a subsequent
// retry of the same reply is not suppressed.
func (d *threadReplyDeduper) abort(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
}
