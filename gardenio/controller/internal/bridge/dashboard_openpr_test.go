package bridge

import (
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/prhealth"
)

// The dashboard carries the PR sweeper's open-PR line under the summary
// (plan gd-3F4W22h2nT step 10, gd-Pt8ztuasTE), and a new record changes the
// hash so the pinned message is updated.
func TestRenderBlocks_OpenPRLine(t *testing.T) {
	d := &Dashboard{cfg: DashboardConfig{}}
	blocks, hash := d.renderBlocks(Garden, nil, nil, accountCostData{})
	if strings.Contains(blocksToText(blocks), "open PRs") {
		t.Fatal("no sweeper record yet: want no open-PR line")
	}

	h, err := prhealth.Parse(`{"at":"2026-10-30T14:17:00Z","open":22,"desk_queue":3,"orphans":9,` +
		`"oldest_orphan_hours":124.5,"oldest_open_hours":130,"train":"green","train_pr":1570}`)
	if err != nil {
		t.Fatal(err)
	}
	d.openPRs = h
	blocks, withPRs := d.renderBlocks(Garden, nil, nil, accountCostData{})
	want := "22 open PRs · 3 in the desk queue · 9 orphaned (oldest 5.2d) · train green #1570 · as of 14:17 UTC"
	ctx, ok := blocks[2].(*slack.ContextBlock)
	if !ok {
		t.Fatalf("block 2 = %T, want the open-PR context block under the summary", blocks[2])
	}
	text := ctx.ContextElements.Elements[0].(*slack.TextBlockObject).Text
	if !strings.Contains(text, want) || !strings.HasPrefix(text, Garden.Semantic(SemReview)) {
		t.Errorf("open-PR line = %q, want the review glyph and %q", text, want)
	}
	if withPRs == hash {
		t.Error("a sweeper record did not change the dashboard hash")
	}

	h2 := *h
	h2.Orphans = 2
	d.openPRs = &h2
	if _, next := d.renderBlocks(Garden, nil, nil, accountCostData{}); next == withPRs {
		t.Error("a new sweeper record did not change the dashboard hash")
	}
}
