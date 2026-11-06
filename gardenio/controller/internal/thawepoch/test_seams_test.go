package thawepoch

import (
	"fmt"
	"strings"
	"time"
)

// Err returns a non-nil error describing the fail-closed reason, or nil when
// Released. Convenient for callers that want to fail a thaw with one check.
func (r GateResult) Err() error {
	if r.Released {
		return nil
	}
	return fmt.Errorf("thawepoch: rehydration not released for epoch %d after %s; failed consumers: %s",
		r.Epoch, r.Elapsed.Round(time.Millisecond), strings.Join(r.FailedConsumers, ", "))
}

// Notify is an optional eager nudge: an inotify front-end (coop's notify crate,
// or a Go fsnotify watcher if wired later) can call Notify to make the watcher
// re-check the marker immediately instead of waiting for the next poll tick.
// Safe to call before Run; nudges are coalesced.
func (w *Watcher) Notify() {
	if w.notify == nil {
		return
	}
	select {
	case w.notify <- struct{}{}:
	default: // a nudge is already pending; coalesce
	}
}
