package heraldry

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"
)

// Version is the drawing's version. It is part of every Key, so a change to
// the drawing (geometry, keyline, artwork) gives every picture a new address
// and no browser or Slack file cache serves the old one. Bump it with the
// goldens. 2 is the run drawn as a row (the shield went).
const Version = "2"

// Format is a picture's encoding.
type Format string

const (
	FormatPNG Format = "png"
	FormatSVG Format = "svg"
)

// ContentType is the format's media type.
func (f Format) ContentType() string {
	if f == FormatSVG {
		return "image/svg+xml"
	}
	return "image/png"
}

// Picture is one rendered picture and its content address.
type Picture struct {
	// Key is the content address (Key): stable for the same arms, format,
	// height and theme, so it serves as a URL path segment and an ETag.
	Key string
	// Format and ContentType say how Bytes are encoded.
	Format      Format
	ContentType string
	// Bytes are shared with the cache; callers must not modify them.
	Bytes []byte
	// Arms is what was drawn; PlainName gives the picture's alt text.
	Arms Arms
}

// Key is the content address of a picture: the lower-case hex SHA-256 of the
// drawing's Version, the run (as Canonical respells it, so "⚙" and "⚙️",
// and a six-token form and its run, share a key), the format, the height (0
// for SVG) and the theme, NUL-separated.
func Key(a Arms, f Format, h int, th Theme) string {
	if f == FormatSVG {
		h = 0
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "heraldry/%s\x00%s\x00%s\x00%d\x00%s", Version, a.Canonical(), f, h, th))
	return hex.EncodeToString(sum[:])
}

// Cache renders pictures through a bounded, in-memory, content-addressed
// cache: the first request for a (arms, format, height, theme) draws it, and
// later ones, by the same inputs or by Key, are served from memory until it
// is evicted (least recently used first). It is safe for concurrent use.
//
// A server that puts Key in a URL (/arms/<key>.png) answers from Get, and on
// a miss (a restart, an eviction) needs the inputs again, so its URL should
// carry them too.
type Cache struct {
	mu      sync.Mutex
	max     int
	order   *list.List // front is most recent; values are *Picture
	entries map[string]*list.Element
}

// NewCache holds up to n pictures; n < 1 is 1.
func NewCache(n int) *Cache {
	return &Cache{max: max(n, 1), order: list.New(), entries: map[string]*list.Element{}}
}

// PNG is the arms in a stored form, h device pixels high, in theme th.
func (c *Cache) PNG(stored string, h int, th Theme) (Picture, error) {
	return c.picture(stored, FormatPNG, h, th)
}

// SVG is the arms in a stored form as a stand-alone SVG in theme th.
func (c *Cache) SVG(stored string, th Theme) (Picture, error) {
	return c.picture(stored, FormatSVG, 0, th)
}

// Get is a picture drawn earlier, by its Key.
func (c *Cache) Get(key string) (Picture, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.entries[key]
	if !ok {
		return Picture{}, false
	}
	c.order.MoveToFront(el)
	return *el.Value.(*Picture), true
}

func (c *Cache) picture(stored string, f Format, h int, th Theme) (Picture, error) {
	a, err := Parse(stored)
	if err != nil {
		return Picture{}, err
	}
	key := Key(a, f, h, th)
	if p, ok := c.Get(key); ok {
		return p, nil
	}
	p := Picture{Key: key, Format: f, ContentType: f.ContentType(), Arms: a}
	if f == FormatSVG {
		p.Bytes = a.SVG(th, SVGOptions{})
	} else if p.Bytes, err = a.PNG(h, th); err != nil {
		return Picture{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.entries[key]; ok { // drawn concurrently; keep the first
		c.order.MoveToFront(el)
		return *el.Value.(*Picture), nil
	}
	c.entries[key] = c.order.PushFront(&p)
	for c.order.Len() > c.max {
		old := c.order.Back()
		c.order.Remove(old)
		delete(c.entries, old.Value.(*Picture).Key)
	}
	return p, nil
}
