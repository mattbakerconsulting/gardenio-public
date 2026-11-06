package bridge

// Unit tests for jiraReplyDeduper — the reservation-semantics dedup that
// collapses identical Jira reply comments within a window (ka-0000007243,
// Slack threadReplyDeduper parity). The endpoint-level behavior (a retry that
// posts once, a failed post that releases its slot) is covered in
// jira_reply_test.go; these tests pin the deduper primitive directly.

import (
	"sync"
	"testing"
	"time"
)

func TestJiraReplyDeduper_CollapsesIdenticalInWindow(t *testing.T) {
	d := newJiraReplyDeduper(5 * time.Minute)
	current := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return current }

	key, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello")
	if dup {
		t.Fatalf("first begin must not be a dup")
	}
	d.commit(key)

	// Inside the window: an identical reply is collapsed.
	current = current.Add(4 * time.Minute)
	if _, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello"); !dup {
		t.Fatalf("inside window: identical reply must be collapsed (dup=true)")
	}
}

func TestJiraReplyDeduper_WindowExpiry(t *testing.T) {
	d := newJiraReplyDeduper(5 * time.Minute)
	current := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return current }

	key, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello")
	if dup {
		t.Fatalf("first begin must not be a dup")
	}
	d.commit(key)

	// commit() refreshes the reservation timestamp to the landing time, so the
	// window is measured from the post. 4 minutes later: still collapsed.
	current = current.Add(4 * time.Minute)
	if _, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello"); !dup {
		t.Fatalf("inside window: dup=false, want the reply collapsed")
	}

	// Past the window (measured from the reservation): the identical reply is
	// allowed through again so a genuinely repeated message is never muted
	// forever.
	current = current.Add(2 * time.Minute)
	if _, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello"); dup {
		t.Fatalf("past window: identical reply must be allowed to post again")
	}
}

func TestJiraReplyDeduper_DistinctKeysNotCollapsed(t *testing.T) {
	d := newJiraReplyDeduper(5 * time.Minute)

	base := func() { d.begin("crew-1", "PE-42", CommentInternal, "hello") }
	base()

	// Each of these differs in exactly one key component and must NOT collapse.
	cases := []struct {
		name             string
		agent, key, text string
		vis              CommentVisibility
	}{
		{"different-text", "crew-1", "PE-42", "goodbye", CommentInternal},
		{"different-agent", "crew-2", "PE-42", "hello", CommentInternal},
		{"different-ticket", "crew-1", "PE-99", "hello", CommentInternal},
		{"different-visibility", "crew-1", "PE-42", "hello", CommentPublic},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, dup := d.begin(tc.agent, tc.key, tc.vis, tc.text); dup {
				t.Fatalf("%s must not collapse with the base reservation", tc.name)
			}
		})
	}
}

// TestJiraReplyDeduper_AbortReleasesSlot verifies a failed post's abort frees
// the slot so a legitimate retry — which classifyReplyPostError invites — is
// not suppressed.
func TestJiraReplyDeduper_AbortReleasesSlot(t *testing.T) {
	d := newJiraReplyDeduper(5 * time.Minute)

	key, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello")
	if dup {
		t.Fatalf("first begin must not be a dup")
	}
	d.abort(key) // simulate a failed Jira post

	if _, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello"); dup {
		t.Fatalf("after abort, an identical retry must be allowed (not suppressed)")
	}
}

// TestJiraReplyDeduper_ConcurrentBeginReservesOnce is the load-bearing race
// test: N goroutines racing on the same reply must yield exactly one non-dup
// winner (the one that gets to post), all others collapsed.
func TestJiraReplyDeduper_ConcurrentBeginReservesOnce(t *testing.T) {
	d := newJiraReplyDeduper(5 * time.Minute)

	const n = 64
	var wg sync.WaitGroup
	var mu sync.Mutex
	winners := 0
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			if _, dup := d.begin("crew-1", "PE-42", CommentInternal, "hello"); !dup {
				mu.Lock()
				winners++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if winners != 1 {
		t.Fatalf("exactly one goroutine must win the reservation, got %d", winners)
	}
}

// TestJiraReplyDeduper_LazyPruneBoundsMap verifies expired entries are dropped
// on subsequent reservations so the map does not grow without bound.
func TestJiraReplyDeduper_LazyPruneBoundsMap(t *testing.T) {
	d := newJiraReplyDeduper(5 * time.Minute)
	current := time.Unix(1_700_000_000, 0)
	d.now = func() time.Time { return current }

	for i := range 100 {
		key, _ := d.begin("crew-1", "PE-42", CommentInternal, string(rune('a'+i%26))+string(rune('0'+i/26)))
		d.commit(key)
	}

	current = current.Add(6 * time.Minute)
	d.begin("crew-1", "PE-42", CommentInternal, "fresh")

	d.mu.Lock()
	got := len(d.entries)
	d.mu.Unlock()
	if got != 1 {
		t.Fatalf("expected lazy prune to drop all 100 expired entries (1 fresh left), got %d", got)
	}
}
