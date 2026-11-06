package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// A daemon fault is NOT evidence of "no cross-project conflict". Failing open
// here let an agent claim another project's bead whenever the daemon hiccuped.
func TestCheckProjectMatch_FetchErrorFailsClosed(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{GetErr: errors.New("daemon down")}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := checkProjectMatch(context.Background(), "gd-other")
	if err == nil {
		t.Fatal("a GetBead failure must fail closed, got nil")
	}
	if !strings.Contains(err.Error(), "daemon down") {
		t.Errorf("the underlying error must be wrapped, got %v", err)
	}
	if !strings.Contains(err.Error(), "--force") {
		t.Errorf("the error must name the override, got %v", err)
	}
}

func TestCheckProjectMatch_NoProjectSkipsWithWarning(t *testing.T) {
	t.Setenv("KD_PROJECT", "")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := checkProjectMatch(context.Background(), "gd-any"); err != nil {
		t.Fatalf("no configured project must skip the check, got %v", err)
	}
	if len(mc.GetBeadCalls) != 0 {
		t.Errorf("the skip must not fetch the bead at all, got %v", mc.GetBeadCalls)
	}
}

func TestCheckProjectMatch_MismatchStillErrors(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{Beads: map[string]*model.Bead{
		"gd-gas": {ID: "gd-gas", Labels: []string{"project:gascity"}},
	}}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	err := checkProjectMatch(context.Background(), "gd-gas")
	if err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Errorf("a genuine cross-project claim must still be refused, got %v", err)
	}
}

func TestCheckProjectMatch_SameProjectAllowed(t *testing.T) {
	t.Setenv("KD_PROJECT", "gardenio")
	t.Setenv("BOAT_PROJECT", "")
	t.Setenv("BEADS_AGENT_NAME", "")

	mc := &mockBeadsClient{Beads: map[string]*model.Bead{
		"gd-ours": {ID: "gd-ours", Labels: []string{"project:gardenio"}},
	}}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	if err := checkProjectMatch(context.Background(), "gd-ours"); err != nil {
		t.Errorf("a same-project claim must be allowed, got %v", err)
	}
}
