package heraldry

import (
	"bytes"
	"sync"
	"testing"
)

func TestKey(t *testing.T) {
	a, _ := Parse("🎲🛡️🖼️")
	b, _ := Parse("slate 🎲 . \U0001F6E1 . 🖼") // no VS16, the six-token form: the same arms
	k := Key(a, FormatPNG, 120, Day)
	if len(k) != 64 || Key(b, FormatPNG, 120, Day) != k {
		t.Errorf("Key is not a stable sha256 of the respelled arms: %q", k)
	}
	c, _ := Parse("🖼️🛡️🎲") // the same emoji in another order
	for _, other := range []string{
		Key(a, FormatPNG, 80, Day),
		Key(a, FormatPNG, 120, Night),
		Key(a, FormatSVG, 0, Day),
		Key(c, FormatPNG, 120, Day),
	} {
		if other == k {
			t.Error("Key ignores an input that changes the picture")
		}
	}
	if Key(a, FormatSVG, 0, Day) != Key(a, FormatSVG, 120, Day) {
		t.Error("an SVG's key depends on a height it does not have")
	}
}

func TestCache(t *testing.T) {
	c := NewCache(2)
	p, err := c.PNG(goldenArms[0].stored, 80, Day)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := Parse(goldenArms[0].stored)
	want, _ := a.PNG(80, Day)
	if p.ContentType != "image/png" || p.Key != Key(a, FormatPNG, 80, Day) || !bytes.Equal(p.Bytes, want) {
		t.Errorf("PNG picture: %s %s, %d bytes", p.ContentType, p.Key, len(p.Bytes))
	}
	if got, ok := c.Get(p.Key); !ok || !bytes.Equal(got.Bytes, p.Bytes) || got.Arms.PlainName() != a.PlainName() {
		t.Error("Get does not serve the picture by its key")
	}
	s, err := c.SVG(goldenArms[0].stored, Night)
	if err != nil || s.ContentType != "image/svg+xml" || !bytes.Equal(s.Bytes, a.SVG(Night, SVGOptions{})) {
		t.Errorf("SVG picture: %v %s", err, s.ContentType)
	}
	// A third picture evicts the least recently used, p (s is newer).
	if _, err := c.PNG(goldenArms[1].stored, 80, Day); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Errorf("cache holds %d, want 2", c.Len())
	}
	if _, ok := c.Get(p.Key); ok {
		t.Error("the least recently used picture was not evicted")
	}
	if _, ok := c.Get(s.Key); !ok {
		t.Error("a recent picture was evicted")
	}
	for _, bad := range []string{"", "teal . . 🛡️ . .", "🛡️🛡️🎲"} {
		if _, err := c.PNG(bad, 80, Day); err == nil {
			t.Errorf("PNG(%q) succeeded", bad)
		}
	}
	if _, err := c.PNG(goldenArms[0].stored, MaxHeight+1, Day); err == nil {
		t.Error("an oversize height was drawn")
	}
}

func TestCacheConcurrent(t *testing.T) {
	c := NewCache(8)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.PNG(goldenArms[i%3].stored, 48, Theme(i%2)); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if c.Len() != 6 {
		t.Errorf("cache holds %d distinct pictures, want 6", c.Len())
	}
}
