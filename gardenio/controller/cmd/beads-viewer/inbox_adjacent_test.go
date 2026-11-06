package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestInbox_AdjacentBuckets_RendersWhenStreamSparse exercises the ka-0000002575
// behavior: when the mine-scope rendered stream is <= inboxAdjacentBucketsThreshold,
// the inbox inline-renders preview cards for pending Decisions and Escalations
// below the primary stream and above the empty-state filler.
func TestInbox_AdjacentBuckets_RendersWhenStreamSparse(t *testing.T) {
	humanID := int64(100)
	now := time.Now()

	// One intake row in the mine-scope stream — well below the threshold.
	// Plus one pending decision (global, not scoped) so the Decisions card
	// has something to surface.
	beads := map[string]*beadsapi.BeadDetail{
		"gd-int1": {
			ID: "gd-int1", Title: "lone-intake", Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID, UpdatedAt: now,
		},
		"gd-dec1": {
			ID: "gd-dec1", Title: "pending-decision-one", Type: "decision", Status: "open",
			OperatedByID: &humanID, UpdatedAt: now.Add(-5 * time.Minute),
			CreatedAt: now.Add(-1 * time.Hour),
			Labels:    []string{"project:gardenio"},
			Fields:    map[string]string{"prompt": "pick something", "requested_by": "alice"},
		},
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads:           beads,
		deps:            map[string][]beadsapi.Dependency{},
		escalationsResp: makeFakeEscalations(now, 2),
	}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `class="inbox-adjacent"`) {
		t.Errorf("expected adjacent-buckets section when stream is sparse\n%s", body)
	}
	for _, want := range []string{
		`id="inbox-adjacent-decisions-heading"`,
		`id="inbox-adjacent-escalations-heading"`,
		`id="inbox-adjacent-mentions-heading"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("expected adjacent-buckets card %q in body", want)
		}
	}
	// Pending decision must surface in the Decisions card.
	if !strings.Contains(body, "pending-decision-one") {
		t.Errorf("expected decision row to render inline")
	}
	// Escalation row(s) must surface in the Escalations card.
	if !strings.Contains(body, "gd-esc1") {
		t.Errorf("expected escalation row gd-esc1 to render inline")
	}
}

// TestInbox_AdjacentBuckets_SuppressedWhenStreamPopulated confirms the inverse:
// when the rendered stream count exceeds the threshold the adjacent-buckets
// section is suppressed so we do not waste vertical space on a busy inbox.
func TestInbox_AdjacentBuckets_SuppressedWhenStreamPopulated(t *testing.T) {
	humanID := int64(100)
	now := time.Now()

	// Four intake rows — above the <= 3 threshold.
	beads := map[string]*beadsapi.BeadDetail{}
	for i, id := range []string{"gd-i1", "gd-i2", "gd-i3", "gd-i4"} {
		beads[id] = &beadsapi.BeadDetail{
			ID: id, Title: "intake-" + id, Type: "task", Status: "open",
			Category: "intake", OperatedByID: &humanID,
			UpdatedAt: now.Add(-time.Duration(i) * time.Minute),
		}
	}
	daemon := &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
		beads:           beads,
		deps:            map[string][]beadsapi.Dependency{},
		escalationsResp: makeFakeEscalations(now, 2),
	}}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}
	mux := mkInboxMux(t, daemon, lookup)

	req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if strings.Contains(body, `class="inbox-adjacent"`) {
		t.Errorf("did not expect adjacent-buckets section when stream count > threshold\n%s", body)
	}
}

// TestInbox_AdjacentBuckets_BoundaryThreshold pins the inclusive boundary so a
// future tweak to inboxAdjacentBucketsThreshold has to update this test too.
// At exactly the threshold the section renders; one above it does not.
func TestInbox_AdjacentBuckets_BoundaryThreshold(t *testing.T) {
	humanID := int64(100)
	now := time.Now()

	mk := func(count int) *inboxFilteringDaemon {
		bs := map[string]*beadsapi.BeadDetail{}
		for i := range count {
			id := "gd-bnd" + string(rune('a'+i))
			bs[id] = &beadsapi.BeadDetail{
				ID: id, Title: "boundary-" + id, Type: "task", Status: "open",
				Category: "intake", OperatedByID: &humanID, UpdatedAt: now,
			}
		}
		return &inboxFilteringDaemon{fakeDaemon: &fakeDaemon{
			beads: bs, deps: map[string][]beadsapi.Dependency{},
		}}
	}
	lookup := &inboxFakeLookup{rows: map[string]struct {
		id    int64
		kind  string
		found bool
	}{
		"alice@example.com": {id: humanID, kind: "human", found: true},
	}}

	for _, tc := range []struct {
		name   string
		count  int
		expect bool
	}{
		{"at threshold renders", inboxAdjacentBucketsThreshold, true},
		{"one above suppresses", inboxAdjacentBucketsThreshold + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := mkInboxMux(t, mk(tc.count), lookup)
			req := inboxRequest(http.MethodGet, "/inbox", "alice@example.com")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200, got %d", w.Code)
			}
			has := strings.Contains(w.Body.String(), `class="inbox-adjacent"`)
			if has != tc.expect {
				t.Errorf("adjacent section present=%v want=%v", has, tc.expect)
			}
		})
	}
}

// TestInbox_AdjacentBuckets_CapDecisionRows guards the preview cap so a deep
// pending-decision queue does not blow out the inline section.
func TestInbox_AdjacentBuckets_CapDecisionRows(t *testing.T) {
	long := make([]decisionRowView, 0, 10)
	for range 10 {
		long = append(long, decisionRowView{ID: "gd-d", Title: "x"})
	}
	got := capDecisionRows(long, inboxAdjacentBucketsPreviewCap)
	if len(got) != inboxAdjacentBucketsPreviewCap {
		t.Fatalf("cap = %d, want %d", len(got), inboxAdjacentBucketsPreviewCap)
	}
	if got := capDecisionRows(nil, 3); got != nil {
		t.Errorf("cap on nil should return nil, got %v", got)
	}
	short := []decisionRowView{{ID: "gd-d"}}
	if got := capDecisionRows(short, 5); len(got) != 1 {
		t.Errorf("short slice passthrough broken: len=%d", len(got))
	}
}

func makeFakeEscalations(now time.Time, n int) []beadsapi.EscalationDetail {
	out := make([]beadsapi.EscalationDetail, 0, n)
	for i := range n {
		id := "gd-esc" + string(rune('1'+i))
		out = append(out, beadsapi.EscalationDetail{
			Issue: &beadsapi.BeadDetail{
				ID:        id,
				Title:     "escalation-" + id,
				Status:    "open",
				CreatedAt: now.Add(-time.Duration(i+1) * time.Hour),
				UpdatedAt: now.Add(-time.Duration(i) * time.Minute),
				Fields:    map[string]string{"operation_type": "secret-rotate", "audience": "owner"},
			},
		})
	}
	return out
}
