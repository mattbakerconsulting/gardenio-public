package decisionview

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// countingReader is fakeReader counting its reads.
type countingReader struct {
	fakeReader
	mu    sync.Mutex
	reads int
}

func (c *countingReader) GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error) {
	c.mu.Lock()
	c.reads++
	c.mu.Unlock()
	return c.fakeReader.GetBead(ctx, id)
}

func evidenceFixture() (*beadsapi.BeadDetail, *countingReader) {
	d := &beadsapi.BeadDetail{ID: "gd-d", Fields: map[string]string{"requesting_task": "gd-t"}}
	r := &countingReader{fakeReader: fakeReader{
		beads:    map[string]*beadsapi.BeadDetail{"gd-t": {ID: "gd-t", Title: "Grade the Porch"}},
		comments: map[string][]beadsapi.Comment{"gd-t": {{Text: "![phone, dark](https://v/image/ui/phone.png)"}}},
	}}
	return d, r
}

func TestEvidenceImagesCarrySourceTitle(t *testing.T) {
	d, r := evidenceFixture()
	got := LoadEvidence(context.Background(), r, d, 0)
	if len(got) != 1 || got[0].Source != "gd-t" || got[0].SourceTitle != "Grade the Porch" {
		t.Errorf("an image names its bead and that bead's title: %+v", got)
	}
}

func TestEvidenceCache(t *testing.T) {
	d, r := evidenceFixture()
	c := NewEvidenceCache(time.Minute)
	now := time.Unix(1_000_000, 0)
	c.now = func() time.Time { return now }
	ctx := context.Background()

	if got := c.Load(ctx, r, d, 0); len(got) != 1 {
		t.Fatalf("first load reads: %+v", got)
	}
	c.Load(ctx, r, d, 0)
	if r.reads != 1 {
		t.Errorf("a second render inside the TTL reads nothing: %d reads", r.reads)
	}
	c.Load(beadsapi.WithUserBearer(ctx, "another-viewer"), r, d, 0)
	if r.reads != 2 {
		t.Errorf("another caller's bearer is its own entry, never the first viewer's images: %d reads", r.reads)
	}
	moved := &beadsapi.BeadDetail{ID: "gd-d", Fields: map[string]string{"requesting_task": "gd-t", "evidence": "gd-t2"}}
	c.Load(ctx, r, moved, 0)
	if r.reads != 4 {
		t.Errorf("a decision naming new evidence is read again at once (gd-t2, gd-t): %d reads", r.reads)
	}
	now = now.Add(time.Minute)
	c.Load(ctx, r, d, 0)
	if r.reads != 5 {
		t.Errorf("past the TTL it reads again: %d reads", r.reads)
	}
	var nilCache *EvidenceCache
	if got := nilCache.Load(ctx, r, d, 0); len(got) != 1 || r.reads != 6 {
		t.Errorf("a nil cache reads every time: %+v, %d reads", got, r.reads)
	}
	if c.Load(ctx, r, &beadsapi.BeadDetail{ID: "gd-x"}, 0) != nil || r.reads != 6 {
		t.Error("a decision with no evidence beads reads nothing")
	}
}

func TestEvidenceCacheLoadAll(t *testing.T) {
	d, r := evidenceFixture()
	other := &beadsapi.BeadDetail{ID: "gd-o"}
	got := NewEvidenceCache(0).LoadAll(context.Background(), r, []*beadsapi.BeadDetail{d, nil, other}, 0)
	if len(got) != 1 || len(got["gd-d"]) != 1 {
		t.Errorf("keyed by decision, none for a decision without evidence: %+v", got)
	}
}

func TestEvidenceStrip(t *testing.T) {
	imgs := []Image{
		{URL: "https://viewer.example/image/ui/a.png", Alt: `phone <b>"dark"</b>`, Source: "gd-t", SourceTitle: "Grade & ship"},
		{URL: "https://elsewhere.example/b.png", Alt: "not proxied", Source: "gd-t"},
		{URL: "https://viewer.example/image/ui/c.png", Alt: "c.png", Source: "gd-e"},
	}
	h := string(EvidenceStrip("gd-d", imgs, StripOptions{Base: "/b", Slot: "mb", Href: func(id string) string { return "/b/garden?bead=" + id }}))
	for _, want := range []string{
		`data-decision-evidence="gd-d"`,
		`Screenshots <span class="dec-ev-n">2</span>`,
		`src="/b/image/ui/a.png"`,
		`popovertarget="dec-ev-mb-gd-d-0"`, `id="dec-ev-mb-gd-d-0" popover`,
		`from <a href="/b/garden?bead=gd-t">Grade &amp; ship</a>`,
		`from <a href="/b/garden?bead=gd-e">gd-e</a>`,
		`alt="phone &lt;b&gt;&#34;dark&#34;&lt;/b&gt;"`,
		`popovertargetaction="hide"`,
	} {
		if !strings.Contains(h, want) {
			t.Errorf("strip lacks %q:\n%s", want, h)
		}
	}
	if strings.Contains(h, "elsewhere.example") || strings.Contains(h, "<b>") {
		t.Errorf("a picture the proxy does not serve is left out, and alt text is escaped:\n%s", h)
	}
	if EvidenceStrip("gd-d", imgs[1:2], StripOptions{}) != "" || EvidenceStrip("gd-d", nil, StripOptions{}) != "" {
		t.Error("no picture the proxy serves: no strip")
	}
	if h := string(EvidenceStrip("gd-d", imgs[:1], StripOptions{})); strings.Contains(h, "dec-ev-from") || !strings.Contains(h, `id="dec-ev-ev-gd-d-0"`) {
		t.Errorf("no Href links no source; the default slot is ev:\n%s", h)
	}
}

func TestEvidenceBySource(t *testing.T) {
	got := EvidenceBySource([]Image{{Source: "a", SourceTitle: "A"}, {Source: "b"}, {Source: "a"}})
	if len(got) != 2 || got[0] != (SourceCount{ID: "a", Title: "A", N: 2}) || got[1] != (SourceCount{ID: "b", N: 1}) {
		t.Errorf("counted per source in first-seen order: %+v", got)
	}
}
