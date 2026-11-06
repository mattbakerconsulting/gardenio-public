package main

import (
	"context"
	"testing"
)

// TestThreadProducedMRs is the table test for the /threads "Produced MRs"
// aggregator (gb-ux follow-up ka-0000003947). It exercises the produces-edge
// walk over a resolved member set and the projection onto the shared
// wrapUpMRView shape: dedup across multiple producers, skipping non-MR and
// URL-less targets, member-then-discovery ordering, and the empty cases.
func TestThreadProducedMRs(t *testing.T) {
	// build wires a fresh threadFake and returns it; each case mutates it in
	// place before calling threadProducedMRs, so the fixtures stay legible.
	cases := []struct {
		name      string
		members   []string
		setup     func(f *threadFake)
		wantURLs  []string // expected wrapUpMRView.URL values, in order
		wantState map[string]string
		wantIID   map[string]string
	}{
		{
			name:    "no members yields nil",
			members: nil,
			setup:   func(f *threadFake) {},
		},
		{
			name:    "member with no produces edge yields nil",
			members: []string{"gd-task1"},
			setup: func(f *threadFake) {
				f.addChild("gd-task1", "a task", "gd-wish", "gd-wish", ts("2026-06-20T10:00:00Z"), true)
			},
		},
		{
			name:    "single produced MR is projected onto the pill shape",
			members: []string{"gd-wish", "gd-task1"},
			setup: func(f *threadFake) {
				f.addMRBead("gd-mr1", map[string]string{
					"mr_url":             "https://gitlab.com/org/repo/-/merge_requests/42",
					"mr_iid":             "42",
					"mr_title":           "feat: the thing",
					"mr_state":           "opened",
					"mr_pipeline_status": "success",
				})
				f.addProduces("gd-task1", "gd-mr1")
			},
			wantURLs:  []string{"https://gitlab.com/org/repo/-/merge_requests/42"},
			wantState: map[string]string{"https://gitlab.com/org/repo/-/merge_requests/42": "opened"},
			wantIID:   map[string]string{"https://gitlab.com/org/repo/-/merge_requests/42": "42"},
		},
		{
			name:    "one MR produced by two members is de-duplicated",
			members: []string{"gd-task1", "gd-task2"},
			setup: func(f *threadFake) {
				f.addMRBead("gd-mr1", map[string]string{
					"mr_url":   "https://gitlab.com/org/repo/-/merge_requests/7",
					"mr_iid":   "7",
					"mr_state": "merged",
				})
				// The same MR bead is the produces-target of both task beads
				// (the multi-producer pattern UpsertMergeRequestBead preserves).
				f.addProduces("gd-task1", "gd-mr1")
				f.addProduces("gd-task2", "gd-mr1")
			},
			wantURLs: []string{"https://gitlab.com/org/repo/-/merge_requests/7"},
		},
		{
			name:    "multiple MRs preserve member-then-discovery order",
			members: []string{"gd-taskA", "gd-taskB"},
			setup: func(f *threadFake) {
				f.addMRBead("gd-mrB", map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/2", "mr_iid": "2"})
				f.addMRBead("gd-mrA", map[string]string{"mr_url": "https://gitlab.com/org/repo/-/merge_requests/1", "mr_iid": "1"})
				// taskA (earlier member) produces mrA; taskB produces mrB. The
				// result must follow member order (A before B), NOT bead-id or
				// map order.
				f.addProduces("gd-taskA", "gd-mrA")
				f.addProduces("gd-taskB", "gd-mrB")
			},
			wantURLs: []string{
				"https://gitlab.com/org/repo/-/merge_requests/1",
				"https://gitlab.com/org/repo/-/merge_requests/2",
			},
		},
		{
			name:    "non-MR produces target is skipped",
			members: []string{"gd-task1"},
			setup: func(f *threadFake) {
				// A produces edge to a plain task bead (e.g. a report) is not an
				// MR — the produces edge is authoritative for the walk but the
				// target must resolve to a merge-request bead to earn a pill.
				f.addChild("gd-report", "a report", "gd-task1", "gd-wish", ts("2026-06-20T10:00:00Z"), false)
				f.addProduces("gd-task1", "gd-report")
			},
		},
		{
			name:    "MR bead with no mr_url is skipped (would dead-link)",
			members: []string{"gd-task1"},
			setup: func(f *threadFake) {
				f.addMRBead("gd-mr-nourl", map[string]string{"mr_iid": "9", "mr_state": "opened"})
				f.addProduces("gd-task1", "gd-mr-nourl")
			},
		},
		{
			name:    "produces target the daemon did not return is skipped",
			members: []string{"gd-task1"},
			setup: func(f *threadFake) {
				// Edge points at gd-gone but no bead is seeded (deleted /
				// cross-fleet) — BatchGetBeads omits it, so no pill.
				f.addProduces("gd-task1", "gd-gone")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newThreadFake()
			tc.setup(f)

			got, err := threadProducedMRs(context.Background(), f, tc.members)
			if err != nil {
				t.Fatalf("threadProducedMRs: %v", err)
			}
			if len(got) != len(tc.wantURLs) {
				t.Fatalf("got %d MRs %+v, want %d %v", len(got), got, len(tc.wantURLs), tc.wantURLs)
			}
			for i, want := range tc.wantURLs {
				if got[i].URL != want {
					t.Errorf("MR[%d].URL = %q, want %q", i, got[i].URL, want)
				}
				if tc.wantState != nil {
					if s := tc.wantState[want]; s != "" && got[i].State != s {
						t.Errorf("MR[%d].State = %q, want %q", i, got[i].State, s)
					}
				}
				if tc.wantIID != nil {
					if iid := tc.wantIID[want]; iid != "" && got[i].IID != iid {
						t.Errorf("MR[%d].IID = %q, want %q", i, got[i].IID, iid)
					}
				}
			}
		})
	}
}

// TestThreadProducedMRs_DepErrorPropagates asserts a forward-dep transport
// failure surfaces as an error so the handler can degrade (omit the section)
// rather than silently rendering an empty list as "nothing shipped".
func TestThreadProducedMRs_DepErrorPropagates(t *testing.T) {
	f := newThreadFake()
	f.depErr = errThreadTest
	_, err := threadProducedMRs(context.Background(), f, []string{"gd-task1"})
	if err == nil {
		t.Fatal("expected forward-dep error to propagate, got nil")
	}
}

// TestThreadProducedMRs_BatchErrorPropagates asserts a batch-fetch failure of
// the produced candidates surfaces as an error (same degrade-not-lie contract).
func TestThreadProducedMRs_BatchErrorPropagates(t *testing.T) {
	f := newThreadFake()
	f.addProduces("gd-task1", "gd-mr1")
	f.batchErr = errThreadTest
	_, err := threadProducedMRs(context.Background(), f, []string{"gd-task1"})
	if err == nil {
		t.Fatal("expected batch-fetch error to propagate, got nil")
	}
}
