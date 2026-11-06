package server

import (
	"sync"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
)

// beads_mr_dedup_test.go — daemon-enforced dedup of type=merge-request beads on
// fields.mr_url (ka-0000002762). These are the unit-level contract tests over
// the mock store; the migration-041 index is exercised in the real-Postgres
// integration suite.

const testMRURL = "https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/807"

// TestCreateMergeRequest_DedupsOnMRURL is the core lookup-or-return contract: a
// second create carrying the same fields.mr_url returns the FIRST bead, inserts
// no duplicate, and records no second create event.
func TestCreateMergeRequest_DedupsOnMRURL(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	first, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR !807",
		Type:   "merge-request",
		Labels: []string{"project:gardenio"},
		Fields: []byte(`{"mr_url":"` + testMRURL + `","mr_state":"opened"}`),
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if n := countBeadsOfType(ms, "merge-request"); n != 1 {
		t.Fatalf("after first create: got %d merge-request beads, want 1", n)
	}
	eventsAfterFirst := len(ms.events)
	if eventsAfterFirst == 0 {
		t.Fatalf("first create recorded no events; expected a create event")
	}

	// Second create for the same mr_url — e.g. a second bridge replica's webhook
	// — must return the existing bead, not a duplicate.
	second, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR !807 (re-poll)",
		Type:   "merge-request",
		Labels: []string{"project:gardenio"},
		Fields: []byte(`{"mr_url":"` + testMRURL + `","mr_state":"merged"}`),
	})
	if err != nil {
		t.Fatalf("second create (dedup): %v", err)
	}
	if second.Bead.Id != first.Bead.Id {
		t.Errorf("dedup returned a new bead: first=%q second=%q", first.Bead.Id, second.Bead.Id)
	}
	if n := countBeadsOfType(ms, "merge-request"); n != 1 {
		t.Errorf("after dedup: got %d merge-request beads, want 1 (no duplicate)", n)
	}
	if len(ms.events) != eventsAfterFirst {
		t.Errorf("dedup recorded %d new event(s), want 0 (events: %d→%d)",
			len(ms.events)-eventsAfterFirst, eventsAfterFirst, len(ms.events))
	}
	// The returned bead reflects the ORIGINAL (first) bead — the daemon returns
	// it verbatim; state reconciliation is the bridge's UpdateBeadFields job, not
	// createBead's.
	if second.Bead.Title != "MR !807" {
		t.Errorf("dedup returned title=%q, want the original bead's title", second.Bead.Title)
	}
}

// TestCreateMergeRequest_DistinctURLsCreateSeparately verifies the dedup is
// keyed strictly on mr_url — two different MRs each get their own bead.
func TestCreateMergeRequest_DistinctURLsCreateSeparately(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR !807",
		Type:   "merge-request",
		Fields: []byte(`{"mr_url":"` + testMRURL + `"}`),
	}); err != nil {
		t.Fatalf("create A: %v", err)
	}
	if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR !808",
		Type:   "merge-request",
		Fields: []byte(`{"mr_url":"https://gitlab.com/Acme/Platform/monorepo/-/merge_requests/808"}`),
	}); err != nil {
		t.Fatalf("create B: %v", err)
	}
	if n := countBeadsOfType(ms, "merge-request"); n != 2 {
		t.Errorf("distinct URLs: got %d merge-request beads, want 2", n)
	}
}

// TestCreateMergeRequest_NoMRURLStillCreates verifies a merge-request bead with
// no mr_url is not swallowed by the dedup guard (degenerate but must not hang or
// collapse onto an unrelated bead).
func TestCreateMergeRequest_NoMRURLStillCreates(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "MR bead, no url yet",
		Type:   "merge-request",
		Fields: []byte(`{"mr_state":"opened"}`),
	}); err != nil {
		t.Fatalf("create 1: %v", err)
	}
	if _, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
		Title:  "another MR bead, no url",
		Type:   "merge-request",
		Fields: []byte(`{"mr_state":"opened"}`),
	}); err != nil {
		t.Fatalf("create 2: %v", err)
	}
	// Both create — with no mr_url there is nothing to dedup on.
	if n := countBeadsOfType(ms, "merge-request"); n != 2 {
		t.Errorf("no-mr_url beads: got %d, want 2 (dedup must not collapse them)", n)
	}
}

// TestCreateMergeRequest_ConcurrentDoubleCreateYieldsOne is the bead's Verify
// clause for HALF-1: a forced concurrent double-upsert of the same mr_url yields
// exactly ONE bead. The per-mr_url mutex acquired in createBead serialises the
// two racers' lookup+insert windows so the second sees the first's committed
// bead and returns it.
func TestCreateMergeRequest_ConcurrentDoubleCreateYieldsOne(t *testing.T) {
	srv, ms, ctx := testCtx(t)

	const n = 8
	var wg sync.WaitGroup
	ids := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := srv.CreateBead(ctx, &beadsv1.CreateBeadRequest{
				Title:  "MR !807 racer",
				Type:   "merge-request",
				Fields: []byte(`{"mr_url":"` + testMRURL + `"}`),
			})
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = resp.Bead.Id
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	if got := countBeadsOfType(ms, "merge-request"); got != 1 {
		t.Fatalf("concurrent double-create produced %d merge-request beads, want exactly 1", got)
	}
	// Every racer must have observed the single canonical id.
	first := ids[0]
	for i, id := range ids {
		if id != first {
			t.Errorf("racer %d got id=%q, want the single canonical id %q", i, id, first)
		}
	}
}
