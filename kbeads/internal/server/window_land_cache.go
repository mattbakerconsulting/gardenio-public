package server

import (
	"context"
	"sync"
	"time"
)

// windowLandViewTTL is how long a built land view is served before the next
// read builds it again. The viewer serves a view for its own windowViewTTL
// (20 s) without asking, so a land view this old is no staler than what a
// visitor already sees.
const windowLandViewTTL = 15 * time.Second

// windowLandViewBuildTimeout bounds one land view build. The build runs on a
// context of its own, so a reader that gives up leaves it running for the
// next reader (gd-FC7tIACC5f).
const windowLandViewBuildTimeout = 60 * time.Second

// windowLandViewCache holds each land's view: one build per key at a time,
// whichever windows read it, and its result for windowLandViewTTL. A failed
// build is not kept. The zero value is ready to use.
type windowLandViewCache struct {
	mu      sync.Mutex
	entries map[string]*windowLandViewBuild
	// now is time.Now when nil.
	now func() time.Time
}

// windowLandViewBuild is one build. done is closed once view, err and at are
// set; they never change after.
type windowLandViewBuild struct {
	done chan struct{}
	view *windowLandView
	err  error
	at   time.Time
}

// get returns key's view: the last build's when it ended within
// windowLandViewTTL without error, else the one in flight, else a new one
// that build makes. ctx bounds only the wait; the view is shared, so a
// caller copies it before changing it.
func (c *windowLandViewCache) get(ctx context.Context, key string, build func(context.Context) (*windowLandView, error)) (*windowLandView, error) {
	c.mu.Lock()
	b := c.entries[key]
	if b == nil || c.stale(b) {
		b = &windowLandViewBuild{done: make(chan struct{})}
		if c.entries == nil {
			c.entries = map[string]*windowLandViewBuild{}
		}
		c.entries[key] = b
		bctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), windowLandViewBuildTimeout)
		go func() {
			defer cancel()
			v, err := build(bctx)
			c.mu.Lock()
			b.view, b.err, b.at = v, err, c.clock()
			c.mu.Unlock()
			close(b.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-b.done:
		return b.view, b.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// stale reports whether b has ended and cannot be served again. c.mu is held.
func (c *windowLandViewCache) stale(b *windowLandViewBuild) bool {
	select {
	case <-b.done:
		return b.err != nil || c.clock().Sub(b.at) >= windowLandViewTTL
	default:
		return false
	}
}

func (c *windowLandViewCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}
