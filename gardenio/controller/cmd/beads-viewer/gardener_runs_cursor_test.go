package main

// Unit tests for the stateless (schedule_key, id) cursor pagination behind
// /gardener/{id}/runs (design doc §7/§8, task ka-0000006972).

import (
	"fmt"
	"testing"
)

// mkIterView builds a gardenerIterationView with just the fields the cursor
// pager reads (ID + ScheduleKey). The pager never touches the embedded
// agentRowView, so a bare view is sufficient.
func mkIterView(id, scheduleKey string) gardenerIterationView {
	return gardenerIterationView{
		agentRowView: agentRowView{ID: id},
		ScheduleKey:  scheduleKey,
	}
}

// newestFirstThread builds n cards in newest-first order (index 0 == newest)
// with monotonically-decreasing schedule_keys, mirroring the ordered life-thread
// the pager consumes. Card i has id "gd-r{i}" and key "2026-06-02T{NN}" where NN
// decreases as i grows, so card 0 (newest) has the lexically-largest key.
func newestFirstThread(n int) []gardenerIterationView {
	out := make([]gardenerIterationView, 0, n)
	for i := range n {
		// minute slot counts DOWN so newest (i=0) sorts highest.
		key := fmt.Sprintf("2026-06-02T%02d:%02d", (n-i)/60, (n-i)%60)
		out = append(out, mkIterView("gd-r"+itoa(i), key))
	}
	return out
}

func TestRunsCursor_EncodeDecodeRoundTrip(t *testing.T) {
	cases := []runsCursor{
		{Key: "2026-06-02T07:11", ID: "gd-abc123"},
		{Key: "", ID: "gd-legacy"},                  // legacy iteration with no schedule_key
		{Key: "2026-06-02T07:11", ID: "gd-x/y+z=="}, // id with base64-ish chars
	}
	for _, c := range cases {
		tok := c.encode()
		got, ok := decodeRunsCursor(tok)
		if !ok {
			t.Fatalf("decode(%q) failed for cursor %+v", tok, c)
		}
		if got != c {
			t.Fatalf("roundtrip mismatch: encoded %+v, decoded %+v (tok=%q)", c, got, tok)
		}
	}
}

func TestDecodeRunsCursor_JunkDegradesToNoCursor(t *testing.T) {
	// Empty, non-base64, and well-formed-but-id-less tokens must all decode to
	// ok=false so the handler treats them as "first tail page", never a 500.
	bad := []string{
		"",
		"   ",
		"!!!not base64!!!",
		runsCursor{Key: "2026-06-02T07:11", ID: ""}.encode(), // id-less
	}
	for _, tok := range bad {
		if _, ok := decodeRunsCursor(tok); ok {
			t.Errorf("decodeRunsCursor(%q) = ok, want degraded (false)", tok)
		}
	}
}

// TestRunsPage_FirstPageStartsAtEagerBoundary verifies that a nil cursor (the
// first /runs reveal) resumes exactly at gardenerEagerRuns — the cards the eager
// page already showed are skipped, with no gap and no repeat.
func TestRunsPage_FirstPageStartsAtEagerBoundary(t *testing.T) {
	total := gardenerEagerRuns + gardenerRunsPageSize + 3
	iters := newestFirstThread(total)

	page, next := runsPage(iters, nil)
	if len(page) != gardenerRunsPageSize {
		t.Fatalf("first page size = %d, want %d", len(page), gardenerRunsPageSize)
	}
	// The first tail card is exactly the one after the eager head.
	if page[0].ID != iters[gardenerEagerRuns].ID {
		t.Fatalf("first tail card = %s, want %s (eager boundary)", page[0].ID, iters[gardenerEagerRuns].ID)
	}
	if next == nil {
		t.Fatalf("expected a continuation cursor (more tail remains)")
	}
	// The continuation cursor points at the last card of this page.
	wantLast := iters[gardenerEagerRuns+gardenerRunsPageSize-1]
	if next.ID != wantLast.ID || next.Key != wantLast.ScheduleKey {
		t.Fatalf("next cursor = %+v, want {Key:%q ID:%q}", *next, wantLast.ScheduleKey, wantLast.ID)
	}
}

// TestRunsPage_WalkExhaustsTailExactlyOnce walks the whole tail page by page and
// asserts every card beyond the eager head appears exactly once, in order, with
// the chain terminating (next==nil) precisely when the last card is served.
func TestRunsPage_WalkExhaustsTailExactlyOnce(t *testing.T) {
	for _, total := range []int{
		0, 1,
		gardenerEagerRuns,                            // no tail
		gardenerEagerRuns + 1,                        // 1-card tail
		gardenerEagerRuns + gardenerRunsPageSize,     // exactly one full page
		gardenerEagerRuns + gardenerRunsPageSize + 1, // one full page + 1
		167, // prod max
	} {
		t.Run(fmt.Sprintf("total=%d", total), func(t *testing.T) {
			iters := newestFirstThread(total)

			var seen []string
			var cur *runsCursor
			for hops := 0; ; hops++ {
				if hops > total+10 {
					t.Fatalf("cursor walk did not terminate (total=%d)", total)
				}
				page, next := runsPage(iters, cur)
				for _, c := range page {
					seen = append(seen, c.ID)
				}
				if next == nil {
					break
				}
				if len(page) != gardenerRunsPageSize {
					t.Fatalf("non-final page size = %d, want %d", len(page), gardenerRunsPageSize)
				}
				cur = next
			}

			// The tail is everything beyond the eager head, in order, once each.
			wantStart := min(gardenerEagerRuns, total)
			want := iters[wantStart:]
			if len(seen) != len(want) {
				t.Fatalf("walked %d tail cards, want %d (total=%d)", len(seen), len(want), total)
			}
			for i := range want {
				if seen[i] != want[i].ID {
					t.Fatalf("tail card %d = %s, want %s", i, seen[i], want[i].ID)
				}
			}
		})
	}
}

// TestRunsPage_PrunedAnchorResync verifies the (schedule_key, id) resync path:
// when the cursor's anchor card has been pruned between reveals, the next page
// resumes at the first card strictly OLDER than the cursor — no skip, no repeat,
// no crash.
func TestRunsPage_PrunedAnchorResync(t *testing.T) {
	iters := newestFirstThread(gardenerEagerRuns + 10)

	// Take a cursor at a card in the tail, then simulate that card (and a couple
	// around it) being pruned from a subsequent fetch.
	anchorIdx := gardenerEagerRuns + 4
	anchor := iters[anchorIdx]
	cur := &runsCursor{Key: anchor.ScheduleKey, ID: anchor.ID}

	// Rebuild the thread without the anchor card (pruned).
	pruned := make([]gardenerIterationView, 0, len(iters)-1)
	for i, c := range iters {
		if i == anchorIdx {
			continue
		}
		pruned = append(pruned, c)
	}

	page, _ := runsPage(pruned, cur)
	if len(page) == 0 {
		t.Fatalf("expected a resync page after pruned anchor, got empty")
	}
	// The first resumed card must be the next-older card (the one that followed
	// the pruned anchor in the original order), and must NOT be any card at or
	// newer than the cursor position.
	wantFirst := iters[anchorIdx+1]
	if page[0].ID != wantFirst.ID {
		t.Fatalf("resync first card = %s, want %s (first card older than pruned anchor)", page[0].ID, wantFirst.ID)
	}
	for _, c := range page {
		if !runsOlder(c, *cur) {
			t.Errorf("resync page contains card %s not strictly older than cursor", c.ID)
		}
	}
}

// TestRunsPage_LegacyEmptyScheduleKey verifies paging still works when no
// iteration carries a schedule_key (legacy beads): the cursor falls back to the
// id tie-breaker and exact positional matching, so the walk still exhausts the
// tail exactly once.
func TestRunsPage_LegacyEmptyScheduleKey(t *testing.T) {
	total := gardenerEagerRuns + gardenerRunsPageSize + 2
	iters := make([]gardenerIterationView, 0, total)
	for i := range total {
		iters = append(iters, mkIterView("gd-leg"+itoa(i), "")) // empty schedule_key
	}

	var seen []string
	var cur *runsCursor
	for hops := 0; ; hops++ {
		if hops > total {
			t.Fatalf("legacy cursor walk did not terminate")
		}
		page, next := runsPage(iters, cur)
		for _, c := range page {
			seen = append(seen, c.ID)
		}
		if next == nil {
			break
		}
		cur = next
	}
	if len(seen) != total-gardenerEagerRuns {
		t.Fatalf("legacy walk saw %d cards, want %d", len(seen), total-gardenerEagerRuns)
	}
}

// TestRunsStartIndex_NoTailWhenWithinEagerHead asserts the boundary case: a
// thread that fits entirely within the eager head yields an empty first tail
// page (start == len), so the lazy section renders nothing.
func TestRunsStartIndex_NoTailWhenWithinEagerHead(t *testing.T) {
	iters := newestFirstThread(gardenerEagerRuns - 1)
	page, next := runsPage(iters, nil)
	if len(page) != 0 || next != nil {
		t.Fatalf("expected empty tail for short thread, got page=%d next=%v", len(page), next)
	}
}
