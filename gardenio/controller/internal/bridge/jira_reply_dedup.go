package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

// jiraReplyDedupWindow is how long an identical (agent, jira_key, visibility,
// text) reply is collapsed after one is posted. It mirrors the slack-bridge's
// threadReplyDedupWindow (bot_threads_dedup.go): five minutes comfortably
// covers the observed retry / pod-churn burst pattern while staying short
// enough that a genuinely repeated reply eventually goes through.
const jiraReplyDedupWindow = 5 * time.Minute

// jiraReplyDeduper collapses identical Jira reply comments that arrive within
// jiraReplyDedupWindow of each other. It is the Jira-side port of the
// slack-bridge's threadReplyDeduper (ka-0000002080, ka-0000007243).
//
// Why it is needed: POST /api/jira/reply posts a comment on a possibly
// customer-facing ticket, and classifyReplyPostError deliberately tells agents
// to RETRY on a transient (5xx / 429 / transport) failure. If the bridge's
// post to Jira actually lands but its 200 to the caller is lost (a network
// blip between the bridge and the gb CLI), or N pod incarnations each believe
// they are the first to reply, the retry / re-invocation posts a DUPLICATE
// comment. The bridge is the single process every reply funnels through, so a
// reservation taken under one lock is the only dedup that holds under that
// concurrency.
//
// Reservation semantics (identical to the Slack deduper): begin() claims the
// (agent, jira_key, visibility, text) slot BEFORE the Jira API call, so
// concurrent identical requests collapse even while the winning post is still
// in flight. Only the caller that gets dup=false owns the slot and must follow
// up: commit() on a landed post so it survives the window, or abort() on a
// failed post so a legitimate retry is not suppressed.
type jiraReplyDeduper struct {
	mu      sync.Mutex
	window  time.Duration
	now     func() time.Time // injectable for tests
	entries map[string]*jiraReplyEntry
}

// jiraReplyEntry is one reserved/posted reply slot.
type jiraReplyEntry struct {
	at time.Time // when the slot was reserved
}

func newJiraReplyDeduper(window time.Duration) *jiraReplyDeduper {
	return &jiraReplyDeduper{
		window:  window,
		now:     time.Now,
		entries: make(map[string]*jiraReplyEntry),
	}
}

// jiraReplyKey identifies a reply for dedup purposes. The text is hashed so the
// map's memory stays bounded by the number of distinct replies per window, not
// by message size. Visibility is part of the key so an internal note and a
// public comment with the same text are never collapsed into one.
func jiraReplyKey(agent, jiraKey string, vis CommentVisibility, text string) string {
	sum := sha256.Sum256([]byte(text))
	return agent + "\x00" + jiraKey + "\x00" + string(vis) + "\x00" + hex.EncodeToString(sum[:])
}

// begin reserves the (agent, jira_key, visibility, text) slot. If an identical
// reply was already reserved or posted inside the window it reports dup=true
// and the caller must NOT post. Otherwise the slot is reserved and the caller
// must follow up with commit (post succeeded) or abort (post failed).
func (d *jiraReplyDeduper) begin(agent, jiraKey string, vis CommentVisibility, text string) (key string, dup bool) {
	key = jiraReplyKey(agent, jiraKey, vis, text)
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
	if _, ok := d.entries[key]; ok {
		return key, true
	}
	d.entries[key] = &jiraReplyEntry{at: now}
	return key, false
}

// commit confirms the reserved slot as a landed post. The reservation already
// holds the slot for the window; commit refreshes its timestamp so the dedup
// window is measured from when the post actually landed.
func (d *jiraReplyDeduper) commit(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if e, ok := d.entries[key]; ok {
		e.at = d.now()
	}
}

// abort releases a reserved slot after a failed Jira post so a subsequent retry
// of the same reply is not suppressed.
func (d *jiraReplyDeduper) abort(key string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.entries, key)
}
