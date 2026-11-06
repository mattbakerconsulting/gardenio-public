package commons

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

func TestLastLine(t *testing.T) {
	long := strings.Repeat("word ", 20)
	for name, c := range map[string]struct {
		m    Message
		want string
	}{
		"first line":   {Message{Text: "  \n\thello   there \nmore"}, "hello there"},
		"cut":          {Message{Text: long}, string([]rune(strings.TrimSpace(long))[:lastLineMax-1]) + "…"},
		"files only":   {Message{Files: []File{{Name: "farm.png"}, {Name: "b.pdf"}}}, "farm.png"},
		"nothing said": {Message{}, ""},
	} {
		if got := lastLine(c.m); got != c.want {
			t.Errorf("%s: lastLine = %q, want %q", name, got, c.want)
		}
	}
}

// countingReader counts the per-channel reads (channel_id filtered lists) it
// is asked for.
type countingReader struct {
	Reader
	reads atomic.Int32
}

func (c *countingReader) ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if q.Fields[fieldChannelID] != "" {
		c.reads.Add(1)
	}
	return c.Reader.ListBeadsFiltered(ctx, q)
}

// TestFanOutStopsWhenTheBudgetIsSpent: once the request's read budget has run
// out (or its caller left), the unread counts send no more per-channel reads;
// the rows left go without a badge (gd-WUojuuXdbc).
func TestFanOutStopsWhenTheBudgetIsSpent(t *testing.T) {
	d := commonstest.TwoLands(t0)
	lands, err := readLands(t.Context(), d, adminScope)
	if err != nil || len(lands) == 0 {
		t.Fatalf("readLands = %v, %v", lands, err)
	}
	rd := &countingReader{Reader: d}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if u := readUnread(ctx, rd, lands, nil, ""); len(u) != 0 {
		t.Errorf("readUnread on a spent context counted %v", u)
	}
	if n := rd.reads.Load(); n != 0 {
		t.Errorf("%d per-channel reads sent on a spent context, want 0", n)
	}
}

// TestByReadOrder: a fan-out reads every channel but a DM first, in display
// order, then the DMs newest first, so a spent budget leaves the oldest DMs
// blank (gd-WUojuuXdbc).
func TestByReadOrder(t *testing.T) {
	chs := []Channel{
		{Name: "dm-old", Kind: "dm", Created: t0.Add(-48 * time.Hour)},
		{Name: "general", Kind: "general", Created: t0.Add(-72 * time.Hour)},
		{Name: "dm-new", Kind: "dm", Created: t0},
		{Name: "machines", Kind: kindMachines},
		{Name: "dm-mid", Kind: "dm", Created: t0.Add(-time.Hour)},
		{Name: "crew", Kind: "crew"},
	}
	slices.SortStableFunc(chs, byReadOrder)
	var got []string
	for _, ch := range chs {
		got = append(got, ch.Name)
	}
	want := []string{"general", "machines", "crew", "dm-new", "dm-mid", "dm-old"}
	if !slices.Equal(got, want) {
		t.Errorf("read order = %v, want %v", got, want)
	}
}
