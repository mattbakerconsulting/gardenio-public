package main

import (
	"context"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// stubConfigLister is an in-memory ListAllBeadsFiltered stub for buildRefCorpus.
type stubConfigLister struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (s *stubConfigLister) ListAllBeadsFiltered(_ context.Context, _ beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &beadsapi.ListBeadsResult{Beads: s.beads, Total: len(s.beads)}, nil
}

// TestBuildRefCorpus_OnlyInjectedCategories verifies the reference corpus is
// assembled ONLY from the claude-instructions + nudge-prompts config beads —
// other config beads (type:/view:/context:) are the daemon schema, not injected
// prose, and must not pollute the redundancy reference.
func TestBuildRefCorpus_OnlyInjectedCategories(t *testing.T) {
	lister := &stubConfigLister{beads: []*beadsapi.BeadDetail{
		{ID: "gd-1", Title: "claude-instructions", Description: "INSTRUCTIONS_BODY_ALPHA"},
		{ID: "gd-2", Title: "nudge-prompts", Description: "NUDGE_BODY_BETA"},
		{ID: "gd-3", Title: "type:agent", Description: "SCHEMA_BODY_GAMMA"},
		{ID: "gd-4", Title: "context", Description: "CONTEXT_BODY_DELTA"},
		nil, // nil bead must be skipped, not panic
	}}

	got := buildRefCorpus(context.Background(), lister)
	if !strings.Contains(got, "INSTRUCTIONS_BODY_ALPHA") {
		t.Error("ref corpus missing claude-instructions body")
	}
	if !strings.Contains(got, "NUDGE_BODY_BETA") {
		t.Error("ref corpus missing nudge-prompts body")
	}
	if strings.Contains(got, "SCHEMA_BODY_GAMMA") {
		t.Error("ref corpus wrongly included type:agent schema bead")
	}
	if strings.Contains(got, "CONTEXT_BODY_DELTA") {
		t.Error("ref corpus wrongly included context config bead")
	}
}

// TestBuildRefCorpus_FailOpen verifies a fetch error yields "" (skip the
// redundancy check) rather than a partial/garbage corpus.
func TestBuildRefCorpus_FailOpen(t *testing.T) {
	lister := &stubConfigLister{err: context.DeadlineExceeded}
	if got := buildRefCorpus(context.Background(), lister); got != "" {
		t.Errorf("buildRefCorpus on fetch error = %q, want empty (fail-open)", got)
	}
}
