package main

import (
	"context"
	"slices"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// The seed asks kbeads to drop bookkeeping bead types server-side so the
// 4000-event raw scan budget is spent on displayable events (gd-Mfr8MRXbYG).
func TestCollectFeedSeed_SendsExcludeTypes(t *testing.T) {
	fake := newFake()
	var got []string
	fake.listGlobalEventsFn = func(req beadsapi.ListGlobalEventsRequest) (*beadsapi.ListGlobalEventsResponse, error) {
		got = req.ExcludeTypes
		return &beadsapi.ListGlobalEventsResponse{}, nil
	}
	srv := NewServer(fake, testLogger(), "", "", "", &fakeScreenClient{})
	srv.feedSeedCardsAndUsers(context.Background(), feedBackfillLimit)

	for _, want := range []string{"trigger", "agent", "account", "session"} {
		if !slices.Contains(got, want) {
			t.Errorf("ExcludeTypes %v is missing %q", got, want)
		}
	}
}

// Safety net for the exclusion list: a type excluded server-side must be one
// classifyFeedEvent drops anyway (otherwise the seed would lose cards), and
// every displayable family must stay out of the list.
func TestFeedSeedExcludeTypes_MatchesClassifier(t *testing.T) {
	for _, typ := range feedSeedExcludeTypes {
		for _, topic := range []string{"beads.bead.created", "beads.bead.updated", "beads.bead.closed"} {
			data := feedEventData(t, "gd-x", typ, "t", "open")
			if _, _, _, ok := classifyFeedEvent(topic, data); ok {
				t.Errorf("type %q is excluded server-side but classifyFeedEvent(%s) displays it", typ, topic)
			}
		}
	}
	for _, typ := range []string{"task", "bug", "feature", "epic", "decision", "merge-request", "message", "mail"} {
		if slices.Contains(feedSeedExcludeTypes, typ) {
			t.Errorf("displayable type %q must not be excluded from the seed", typ)
		}
	}
}
