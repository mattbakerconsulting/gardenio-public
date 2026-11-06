package captioner

import (
	"container/list"
	"time"
)

// lru is the caption cache: at most size entries, each good for ttl, keyed by the SHA-256 of the prepared command.
// A cached "" is a reply that was dropped, so the same command is not asked again. Not safe for concurrent use: the
// Captioner's lock guards it.
type lru struct {
	size  int
	ttl   time.Duration
	order *list.List // front is the newest
	items map[string]*list.Element
}

type lruEntry struct {
	key     string
	caption string
	at      time.Time
}

func newLRU(size int, ttl time.Duration) *lru {
	return &lru{size: max(size, 1), ttl: ttl, order: list.New(), items: map[string]*list.Element{}}
}

func (c *lru) get(key string, now time.Time) (string, bool) {
	el, ok := c.items[key]
	if !ok {
		return "", false
	}
	e := el.Value.(*lruEntry)
	if now.Sub(e.at) >= c.ttl {
		c.order.Remove(el)
		delete(c.items, key)
		return "", false
	}
	c.order.MoveToFront(el)
	return e.caption, true
}

func (c *lru) put(key, caption string, now time.Time) {
	if el, ok := c.items[key]; ok {
		el.Value = &lruEntry{key: key, caption: caption, at: now}
		c.order.MoveToFront(el)
		return
	}
	c.items[key] = c.order.PushFront(&lruEntry{key: key, caption: caption, at: now})
	for c.order.Len() > c.size {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.items, old.Value.(*lruEntry).key)
	}
}
