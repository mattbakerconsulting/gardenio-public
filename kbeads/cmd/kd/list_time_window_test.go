package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

// resetListTimeWindowFlags clears the time-window flags this file mutates so a
// previous subtest's setting doesn't leak into the next one (Cobra carries flag
// values across RunE calls within one process). Also registered as a Cleanup so
// tests OUTSIDE this file never see a leftover window value.
func resetListTimeWindowFlags(t *testing.T) {
	t.Helper()
	reset := func() {
		names := map[string]bool{
			"created-after": true, "created-before": true,
			"updated-after": true, "updated-before": true,
			"closed-after": true, "closed-before": true,
		}
		listCmd.Flags().VisitAll(func(f *pflag.Flag) {
			if names[f.Name] {
				f.Changed = false
				_ = f.Value.Set(f.DefValue)
			}
		})
	}
	reset()
	t.Cleanup(reset)
}

// TestList_TimeWindowFlags pins that each --*-after/--*-before flag lands on
// the matching ListBeadsRequest field, accepting both RFC3339 and duration
// spellings (ka-0000008195).
func TestList_TimeWindowFlags(t *testing.T) {
	cases := []struct {
		flag  string
		field func(r *mockBeadsClient) *time.Time
	}{
		{"created-after", func(r *mockBeadsClient) *time.Time { return r.ListBeadsCalls[0].CreatedAfter }},
		{"created-before", func(r *mockBeadsClient) *time.Time { return r.ListBeadsCalls[0].CreatedBefore }},
		{"updated-after", func(r *mockBeadsClient) *time.Time { return r.ListBeadsCalls[0].UpdatedAfter }},
		{"updated-before", func(r *mockBeadsClient) *time.Time { return r.ListBeadsCalls[0].UpdatedBefore }},
		{"closed-after", func(r *mockBeadsClient) *time.Time { return r.ListBeadsCalls[0].ClosedAfter }},
		{"closed-before", func(r *mockBeadsClient) *time.Time { return r.ListBeadsCalls[0].ClosedBefore }},
	}
	for _, tc := range cases {
		t.Run(tc.flag+"_rfc3339", func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()
			resetListTimeWindowFlags(t)

			want := "2026-07-18T09:00:00Z"
			if err := listCmd.Flags().Set(tc.flag, want); err != nil {
				t.Fatalf("setting --%s: %v", tc.flag, err)
			}
			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})
			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := tc.field(mc)
			if got == nil || got.Format(time.RFC3339) != want {
				t.Errorf("--%s=%s → %v, want that timestamp", tc.flag, want, got)
			}
		})
		t.Run(tc.flag+"_duration", func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()
			resetListTimeWindowFlags(t)

			if err := listCmd.Flags().Set(tc.flag, "24h"); err != nil {
				t.Fatalf("setting --%s: %v", tc.flag, err)
			}
			before := time.Now().Add(-24 * time.Hour)
			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})
			after := time.Now().Add(-24 * time.Hour)
			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := tc.field(mc)
			if got == nil || got.Before(before) || got.After(after) {
				t.Errorf("--%s=24h → %v, want ~now-24h [%v, %v]", tc.flag, got, before, after)
			}
		})
	}
}

// TestList_TimeWindowUnset guards that omitted flags never leak a zero time
// into the request — a bare `kd list` stays windowless.
func TestList_TimeWindowUnset(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	resetListTimeWindowFlags(t)

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})
	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	for name, v := range map[string]*time.Time{
		"CreatedAfter": req.CreatedAfter, "CreatedBefore": req.CreatedBefore,
		"UpdatedAfter": req.UpdatedAfter, "UpdatedBefore": req.UpdatedBefore,
		"ClosedAfter": req.ClosedAfter, "ClosedBefore": req.ClosedBefore,
	} {
		if v != nil {
			t.Errorf("%s = %v, want nil when flag unset", name, v)
		}
	}
}

// TestList_TimeWindowMalformedFailsLoud pins that a malformed cutoff aborts
// before any request is sent — a CLI typo must not silently widen the result
// set (contrast the server, which silently ignores unparseable query params
// from arbitrary HTTP callers).
func TestList_TimeWindowMalformedFailsLoud(t *testing.T) {
	mc := newMockClient()
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()
	resetListTimeWindowFlags(t)

	if err := listCmd.Flags().Set("closed-after", "yesterday-ish"); err != nil {
		t.Fatalf("setting --closed-after: %v", err)
	}
	err := listCmd.RunE(listCmd, nil)
	if err == nil {
		t.Fatal("malformed --closed-after must error, got nil")
	}
	if !strings.Contains(err.Error(), "--closed-after") {
		t.Errorf("error %q should name the offending flag", err)
	}
	if len(mc.ListBeadsCalls) != 0 {
		t.Errorf("no request should be sent on malformed cutoff, got %d calls", len(mc.ListBeadsCalls))
	}
}
