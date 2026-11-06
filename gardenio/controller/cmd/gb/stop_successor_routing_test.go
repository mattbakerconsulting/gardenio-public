package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// predSlackRouting is a predecessor carrying every Slack routing field plus the
// wish pointer, as a scheduled thread agent spawned by a gardener would.
var predSlackRouting = map[string]string{
	"slack_spawn_channel":                            "C0SPAWN",
	beadsapi.FieldSlackSpawnChannelInheritedFromRole: "plan-gardener",
	"schedule_slack_channel":                         "C0SCHED",
	"slack_thread_channel":                           "C0THREAD",
	"slack_thread_ts":                                "1792008122.869589",
	beadsapi.FieldThreadID:                           "gd-wish-1",
	beadsapi.FieldSlackThreadBeadID:                  "gd-wish-1",
}

// TestMintSuccessorAgent_CarriesSlackRouting is the gd-jOHtHdkdIt guard: each
// routing field the predecessor set lands on the successor with the same value,
// so the successor posts where its predecessor did.
func TestMintSuccessorAgent_CarriesSlackRouting(t *testing.T) {
	for k := range predSlackRouting {
		t.Run(k, func(t *testing.T) {
			mts := newLineageMintTest(t, lineagePred("gd-root-route", map[string]string{k: predSlackRouting[k]}), 0)
			if _, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, ""); err != nil {
				t.Fatalf("mintSuccessorAgent: %v", err)
			}
			if got := mts.createdFrom[k]; got != predSlackRouting[k] {
				t.Errorf("successor %s = %v, want %q", k, got, predSlackRouting[k])
			}
		})
	}
	t.Run("all", func(t *testing.T) {
		mts := newLineageMintTest(t, lineagePred("gd-root-route", predSlackRouting), 0)
		if _, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, ""); err != nil {
			t.Fatalf("mintSuccessorAgent: %v", err)
		}
		for k, want := range predSlackRouting {
			if got := mts.createdFrom[k]; got != want {
				t.Errorf("successor %s = %v, want %q", k, got, want)
			}
		}
	})
}

// TestMintSuccessorAgent_NoSlackRoutingGainsNone: a predecessor with no Slack
// fields yields a successor with none, so it routes by channel_roles / project
// primary exactly as its predecessor did.
func TestMintSuccessorAgent_NoSlackRoutingGainsNone(t *testing.T) {
	mts := newLineageMintTest(t, lineagePred("gd-root-plain", nil), 0)
	if _, err := mintSuccessorAgent(context.Background(), "gd-pred-loop", &WrapUp{Successor: SuccessorNew}, ""); err != nil {
		t.Fatalf("mintSuccessorAgent: %v", err)
	}
	for k := range mts.createdFrom {
		if strings.HasPrefix(k, "slack_") || strings.Contains(k, "slack_channel") || k == beadsapi.FieldThreadID {
			t.Errorf("successor gained routing field %s=%v from a predecessor with none", k, mts.createdFrom[k])
		}
	}
}
