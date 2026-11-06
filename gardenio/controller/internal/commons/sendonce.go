package commons

import (
	"context"
	"regexp"
	"sync"
	"time"
)

// A say or reply form may carry a nonce, one per message the person writes
// (the workbench's optimistic send, web/commons2.js). A Retry of a send that
// was only slow posts it again: the message the first attempt wrote answers
// it, and nothing is written twice (gd-JpYV1FWtnZ). The message bead keeps
// the nonce, so the stream's copy of it settles the pending row it was sent
// from. The memory is this process's: one beads-viewer serves the Commons.

const (
	// fieldNonce is a message's client nonce.
	fieldNonce = "nonce"
	// sendKeep is how long a written message answers its nonce.
	sendKeep = 10 * time.Minute
)

// nonceOK is what a nonce may be; anything else is taken as none.
var nonceOK = regexp.MustCompile(`^[A-Za-z0-9-]{8,64}$`)

// sendOnce is each nonce's attempt, by poster.
type sendOnce struct {
	mu sync.Mutex
	m  map[string]*attempt
}

// attempt is the one write under a key: done closes when it ends, m then
// being what it wrote.
type attempt struct {
	done chan struct{}
	m    Message
	at   time.Time
}

// sendKey is the key of who's send with nonce, "" when it has none.
func sendKey(who, nonce string) string {
	if who == "" || !nonceOK.MatchString(nonce) {
		return ""
	}
	return who + "\x00" + nonce
}

// claim holds key for this attempt (again false; end it with end), or
// returns the message an earlier attempt under key wrote (again true). An
// attempt still writing is waited for; when it writes nothing, key is held
// for this one.
func (s *sendOnce) claim(ctx context.Context, key string, now time.Time) (m Message, again bool, err error) {
	for {
		s.mu.Lock()
		if s.m == nil {
			s.m = map[string]*attempt{}
		}
		for k, a := range s.m {
			if a.m.ID != "" && now.Sub(a.at) > sendKeep {
				delete(s.m, k)
			}
		}
		a, ok := s.m[key]
		if !ok {
			s.m[key] = &attempt{done: make(chan struct{}), at: now}
			s.mu.Unlock()
			return Message{}, false, nil
		}
		s.mu.Unlock()
		select {
		case <-a.done:
			if a.m.ID != "" {
				return a.m, true, nil
			}
		case <-ctx.Done():
			return Message{}, false, ctx.Err()
		}
	}
}

// end ends the attempt holding key: m is what it wrote, or a zero Message
// when it wrote nothing, which frees key for the next attempt.
func (s *sendOnce) end(key string, m Message, now time.Time) {
	s.mu.Lock()
	a := s.m[key]
	if m.ID == "" {
		delete(s.m, key)
	} else {
		a.m, a.at = m, now
	}
	s.mu.Unlock()
	close(a.done)
}
