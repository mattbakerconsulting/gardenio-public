package main

import (
	"testing"

	"gardenio/kbeads/internal/client"
	"gardenio/kbeads/internal/model"
	"github.com/spf13/pflag"
)

// TestList_MentionsFlag locks in that `kd list --mentions=<id>` round-trips
// the target bead id into ListBeadsRequest.Mentions and leaves MentionsType
// nil (no relationship filter when only --mentions is set).
func TestList_MentionsFlag(t *testing.T) {
	mc := newMockClient()
	mc.ListBeadsResult = []*model.Bead{{ID: "gd-AAA", Title: "x", Type: "task"}}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	resetListMentionFlags(t)
	if err := listCmd.Flags().Set("mentions", "gd-Target001"); err != nil {
		t.Fatalf("setting --mentions: %v", err)
	}

	captureStdout(t, func() {
		if err := listCmd.RunE(listCmd, nil); err != nil {
			t.Fatalf("RunE: %v", err)
		}
	})

	if len(mc.ListBeadsCalls) != 1 {
		t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
	}
	req := mc.ListBeadsCalls[0]
	if req.Mentions != "gd-Target001" {
		t.Errorf("Mentions = %q, want gd-Target001", req.Mentions)
	}
	if req.MentionsType != nil {
		t.Errorf("MentionsType = %v, want nil (only --mentions was set)", req.MentionsType)
	}
}

// TestList_MentionsTypeFlag locks in the three-state semantics of
// --mentions-type (matching `kd refs --type`):
//   - flag unset       → nil
//   - --mentions-type= → "" (untyped only)
//   - --mentions-type=blocks → "blocks"
//   - --mentions-type=mentions → "" (alias for untyped)
func TestList_MentionsTypeFlag(t *testing.T) {
	cases := []struct {
		name    string
		setFlag bool
		flagVal string
		want    *string
	}{
		{"unset", false, "", nil},
		{"empty", true, "", strPtr("")},
		{"verb", true, "blocks", strPtr("blocks")},
		{"mentions_alias", true, "mentions", strPtr("")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mc := newMockClient()
			origClient := beadsClient
			beadsClient = mc
			defer func() { beadsClient = origClient }()

			resetListMentionFlags(t)
			if err := listCmd.Flags().Set("mentions", "gd-Target001"); err != nil {
				t.Fatalf("setting --mentions: %v", err)
			}
			if tc.setFlag {
				if err := listCmd.Flags().Set("mentions-type", tc.flagVal); err != nil {
					t.Fatalf("setting --mentions-type: %v", err)
				}
			}

			captureStdout(t, func() {
				if err := listCmd.RunE(listCmd, nil); err != nil {
					t.Fatalf("RunE: %v", err)
				}
			})

			if len(mc.ListBeadsCalls) != 1 {
				t.Fatalf("expected 1 ListBeads call, got %d", len(mc.ListBeadsCalls))
			}
			got := mc.ListBeadsCalls[0].MentionsType
			if !ptrEq(got, tc.want) {
				t.Errorf("MentionsType = %v, want %v", derefOrNil(got), derefOrNil(tc.want))
			}
		})
	}
}

func ptrEq(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func derefOrNil(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// resetListMentionFlags clears the listCmd flags this file mutates so a
// previous subtest's setting doesn't leak into the next one. Cobra carries
// the Changed bit and the value across RunE calls within one process; both
// must be reset for the three-state --mentions-type test to behave.
func resetListMentionFlags(t *testing.T) {
	t.Helper()
	listCmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Name == "mentions" || f.Name == "mentions-type" {
			f.Changed = false
			_ = f.Value.Set(f.DefValue)
		}
	})
}

var _ = (*client.ListBeadsRequest)(nil)
