package bridge

import (
	"encoding/json"
	"sync"
	"time"
)

// Slack redelivers the same Events API event under a stable event_id when it
// thinks a prior delivery was not acknowledged in time, and the Agents &
// Assistants surface can fan a single user utterance out as BOTH a message
// event and an app_mention event. Neither the parsed slackevents.EventsAPIEvent
// nor the message/app_mention inner events dedup on event_id, so a redelivered
// or double-fanned event re-ran the whole mention path — re-nudging the bound
// agent and, when that agent was busy/wedged, posting another identical
// ":warning: Created task for agent but nudge failed …" reply. The user sees N
// copies of the warning for a SINGLE message they sent (ka-0000005413).
//
// slackEventDedup is a bounded, TTL'd seen-set keyed on Slack's event_id. The
// first time an event_id is seen it returns true (process it); every repeat
// within the TTL returns false (drop it). This is process-local — the bridge is
// a single replica (helm replicaCount: 1), so cross-replica coordination is not
// needed; a restart re-opens the window, which is harmless because a duplicate
// after a restart is rare and the downstream message-bead create is already
// idempotent on channel+ts.

// slackEventDedupTTL is how long an event_id is remembered. Slack's redelivery
// retries span ~30 minutes (three attempts at 1s / 1min / 5min plus jitter), so
// a generous window comfortably covers the retry horizon while staying small
// enough that the seen-set never holds more than a few thousand ids.
const slackEventDedupTTL = 30 * time.Minute

// slackEventDedupMax bounds the seen-set so a burst of distinct events cannot
// grow it without limit between cleanup passes. When exceeded, MarkSeen evicts
// the oldest half (approximate; iteration order) so the set stays bounded even
// if the periodic Cleanup is starved.
const slackEventDedupMax = 4096

// slackEventDedup tracks recently-seen Slack event_ids to suppress duplicate
// processing of redelivered / double-fanned Events API events.
type slackEventDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time // event_id → first-seen time
}

func newSlackEventDedup() *slackEventDedup {
	return &slackEventDedup{seen: make(map[string]time.Time)}
}

// MarkSeen returns true if this is the FIRST time eventID has been seen within
// the TTL window (caller should process the event), or false if it is a
// duplicate (caller should drop it). An empty eventID is always processed —
// without a stable id we cannot dedup, and dropping would lose the event.
func (d *slackEventDedup) MarkSeen(eventID string) bool {
	if eventID == "" {
		return true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if first, ok := d.seen[eventID]; ok && now.Sub(first) < slackEventDedupTTL {
		return false
	}
	// Approximate bound: if the set has grown past the cap (periodic Cleanup
	// starved by a burst), drop enough of the oldest-by-iteration entries to
	// make room. Iteration order is random, so this is not exact-LRU, but it
	// keeps memory bounded and any wrongly-evicted id only risks re-processing
	// a duplicate — which the idempotent message-bead create already tolerates.
	if len(d.seen) >= slackEventDedupMax {
		toDrop := len(d.seen) / 2
		for k := range d.seen {
			delete(d.seen, k)
			toDrop--
			if toDrop <= 0 {
				break
			}
		}
	}
	d.seen[eventID] = now
	return true
}

// Cleanup removes entries older than the TTL to bound memory. Called
// periodically by the bot's Run loop, alongside the concierge debouncer.
func (d *slackEventDedup) Cleanup() {
	cutoff := time.Now().Add(-slackEventDedupTTL)
	d.mu.Lock()
	defer d.mu.Unlock()
	for k, v := range d.seen {
		if v.Before(cutoff) {
			delete(d.seen, k)
		}
	}
}

// eventIDFromRequestPayload extracts Slack's stable event_id from the raw
// socket-mode request payload. The parsed slackevents.EventsAPIEvent drops
// event_id, but the outer EventsAPICallbackEvent carries it, so we pull it from
// the raw payload JSON. Returns "" when the payload is absent or unparseable
// (the caller then falls back to always-process, never dropping the event).
func eventIDFromRequestPayload(payload json.RawMessage) string {
	if len(payload) == 0 {
		return ""
	}
	var envelope struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return ""
	}
	return envelope.EventID
}
