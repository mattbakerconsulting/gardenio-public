package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// laneStub stands up a stub daemon for the gate-list ownership tests. It:
//
//   - records the query string of every GET /v1/beads *list* call (the calls
//     that carry the ownership predicate) into `listQueries`, so a test can
//     assert what reached the WIRE — a predicate that never makes it into the
//     URL is a silent no-op the daemon would ignore, returning every lane's
//     gates;
//   - answers a GET /v1/beads/<agentID> *single-bead* lookup (the agent bead
//     the caller resolves its own seat_key off) with the fields in `agentBead`,
//     so --mine can resolve a durable seat exactly as it does in a live pod.
//
// Every list call returns an empty result set; these tests assert on the query,
// not the rows.
type laneStub struct {
	listQueries []string
	agentID     string
	agentBead   map[string]any
}

func newLaneStub(t *testing.T, s *laneStub) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && s.agentID != "" && r.URL.Path == "/v1/beads/"+s.agentID {
			w.WriteHeader(200)
			json.NewEncoder(w).Encode(s.agentBead)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/v1/beads" {
			s.listQueries = append(s.listQueries, r.URL.RawQuery)
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	t.Cleanup(srv.Close)

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })
}

// anyQueryContains reports whether any recorded list query contains sub.
func (s *laneStub) anyQueryContains(sub string) bool {
	for _, q := range s.listQueries {
		if strings.Contains(q, sub) {
			return true
		}
	}
	return false
}

// captureListQuery is the legacy single-query helper kept for the default-path
// tests, which issue exactly one list call.
func captureListQuery(t *testing.T) *string {
	t.Helper()
	captured := new(string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/beads") {
			*captured = r.URL.RawQuery
		}
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"beads": []any{}, "total": 0})
	}))
	t.Cleanup(srv.Close)

	oldDaemon := daemon
	daemon, _ = beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	t.Cleanup(func() { daemon = oldDaemon })

	return captured
}

// setActor pins the global actor for one test.
func setActor(t *testing.T, name string) {
	t.Helper()
	old := actor
	actor = name
	t.Cleanup(func() { actor = old })
}

// TestDecisionList_MineScopesToDurableSeat is the acceptance test for
// ka-0000005138: a gardener running under one instance name must return
// decisions its SEAT filed under OTHER instance names. --mine resolves the
// caller's seat_key off its agent bead and filters owner_seat == that seat —
// which is stable across respawn — so the created_by instance-name churn that
// broke the old filter no longer hides a lane's own decisions.
func TestDecisionList_MineScopesToDurableSeat(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-live")
	// The live instance is -nhdq; the decision was filed as -7gxg. created_by
	// no longer matches, but owner_seat does.
	setActor(t, "gardenio-lane-nhdq")

	stub := &laneStub{
		agentID: "gd-agent-live",
		agentBead: map[string]any{
			"id": "gd-agent-live", "type": "agent", "status": "open",
			"fields": map[string]string{"seat_key": "gardenio/crew/my-lane"},
		},
	}
	newLaneStub(t, stub)

	cmd := decisionListCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("mine", "true")
	t.Cleanup(func() { cmd.Flags().Set("mine", "false") })

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("gb decision list --mine returned an error: %v", err)
	}
	// The durable-seat predicate must reach the daemon (URL-encoded as
	// field=owner_seat%3D<seat>). This is the predicate that survives respawn.
	if !stub.anyQueryContains("owner_seat") {
		t.Errorf("--mine did not send the durable owner_seat predicate.\nqueries: %v", stub.listQueries)
	}
	// The created_by fallback is unioned in for pre-owner-stamp rows.
	if !stub.anyQueryContains("created_by=gardenio-lane-nhdq") {
		t.Errorf("--mine did not union the created_by fallback.\nqueries: %v", stub.listQueries)
	}
}

// TestDecisionList_MineCreatedByFallbackWhenNoSeat proves the fallback: when the
// caller's agent bead carries no seat_key (or none is resolvable), --mine still
// scopes to created_by == actor, preserving the pre-owner-stamp behavior so a
// row this instance filed before the stamp existed still resolves.
func TestDecisionList_MineCreatedByFallbackWhenNoSeat(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-noseat")
	setActor(t, "gardenio-gardener-x")

	stub := &laneStub{
		agentID: "gd-agent-noseat",
		agentBead: map[string]any{
			"id": "gd-agent-noseat", "type": "agent", "status": "open",
			"fields": map[string]string{}, // no seat_key
		},
	}
	newLaneStub(t, stub)

	cmd := decisionListCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("mine", "true")
	t.Cleanup(func() { cmd.Flags().Set("mine", "false") })

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("gb decision list --mine returned an error: %v", err)
	}
	if !stub.anyQueryContains("created_by=gardenio-gardener-x") {
		t.Errorf("--mine did not fall back to created_by.\nqueries: %v", stub.listQueries)
	}
	if stub.anyQueryContains("owner_seat") {
		t.Errorf("no seat resolved; --mine must not send an owner_seat predicate.\nqueries: %v", stub.listQueries)
	}
}

// TestDecisionList_LaneInspectsAnotherSeat proves --lane <seat> filters on the
// given seat's owner_seat and, since you are not looking at your own instance's
// rows, adds NO created_by fallback.
func TestDecisionList_LaneInspectsAnotherSeat(t *testing.T) {
	setActor(t, "gardenio-gardener-x")
	stub := &laneStub{}
	newLaneStub(t, stub)

	cmd := decisionListCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("lane", "gardenio/crew/other-lane")
	t.Cleanup(func() { cmd.Flags().Set("lane", "") })

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("gb decision list --lane returned an error: %v", err)
	}
	if !stub.anyQueryContains("owner_seat") {
		t.Errorf("--lane did not send an owner_seat predicate.\nqueries: %v", stub.listQueries)
	}
	if stub.anyQueryContains("created_by=") {
		t.Errorf("--lane inspects another lane; it must not add a created_by fallback.\nqueries: %v", stub.listQueries)
	}
}

// TestList_MineAndLaneMutuallyExclusive proves the two ownership flags cannot be
// combined.
func TestList_MineAndLaneMutuallyExclusive(t *testing.T) {
	setActor(t, "gardenio-gardener-x")
	stub := &laneStub{}
	newLaneStub(t, stub)

	cmd := decisionListCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("mine", "true")
	cmd.Flags().Set("lane", "gardenio/crew/other")
	t.Cleanup(func() {
		cmd.Flags().Set("mine", "false")
		cmd.Flags().Set("lane", "")
	})

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected --mine + --lane to error, got nil")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Errorf("error should explain the flags are exclusive, got: %v", err)
	}
	if len(stub.listQueries) != 0 {
		t.Errorf("must not query the daemon on a flag conflict.\nqueries: %v", stub.listQueries)
	}
}

// TestEscalationList_MineScopesOwnership is the escalation half. Escalations
// carry no assignee (gb escalation create stamps none) — measured 2026-08-15:
// 0 of 40 open escalations carried an assignee, 40 of 40 carried created_by.
// Escalation create does not yet stamp owner_seat, so a --mine listing resolves
// on the created_by fallback today; --mine must therefore filter created_by
// (never assignee) so it does not return an empty list for every caller.
func TestEscalationList_MineScopesOwnership(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-esc")
	setActor(t, "gardenio-gardener-x")

	stub := &laneStub{
		agentID: "gd-agent-esc",
		agentBead: map[string]any{
			"id": "gd-agent-esc", "type": "agent", "status": "open",
			"fields": map[string]string{"seat_key": "gardenio/crew/esc-lane"},
		},
	}
	newLaneStub(t, stub)

	cmd := escalationListCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("mine", "true")
	cmd.Flags().Set("limit", "5") // bounded path; avoids the page-walk
	t.Cleanup(func() {
		cmd.Flags().Set("mine", "false")
		cmd.Flags().Set("limit", "0")
	})

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("gb escalation list --mine returned an error: %v", err)
	}
	if !stub.anyQueryContains("created_by=gardenio-gardener-x") {
		t.Errorf("--mine did not send the created_by fallback.\nqueries: %v", stub.listQueries)
	}
	if stub.anyQueryContains("assignee=") {
		t.Errorf("escalations carry no assignee; --mine must not filter on it.\nqueries: %v", stub.listQueries)
	}
}

// TestList_WithoutMineSendsNoOwnershipFilter guards the default path: omitting
// --mine/--lane must not smuggle an ownership predicate into the query, or the
// unfiltered "show me every open gate" listing silently becomes a personal one.
func TestList_WithoutMineSendsNoOwnershipFilter(t *testing.T) {
	setActor(t, "gardenio-gardener-x")

	for _, tc := range []struct {
		name string
		run  func(t *testing.T) *string
	}{
		{"decision", func(t *testing.T) *string {
			got := captureListQuery(t)
			cmd := decisionListCmd
			cmd.SetContext(context.Background())
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("gb decision list: %v", err)
			}
			return got
		}},
		{"escalation", func(t *testing.T) *string {
			got := captureListQuery(t)
			cmd := escalationListCmd
			cmd.SetContext(context.Background())
			cmd.Flags().Set("limit", "5")
			t.Cleanup(func() { cmd.Flags().Set("limit", "0") })
			if err := cmd.RunE(cmd, nil); err != nil {
				t.Fatalf("gb escalation list: %v", err)
			}
			return got
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.run(t)
			if strings.Contains(*got, "created_by=") {
				t.Errorf("default list must not filter by created_by.\nquery: %s", *got)
			}
			if strings.Contains(*got, "owner_seat") {
				t.Errorf("default list must not filter by owner_seat.\nquery: %s", *got)
			}
		})
	}
}

// TestMine_ErrorsWhenNeitherSeatNorActor covers the guard that keeps --mine
// honest. When NEITHER a durable seat NOR a trustworthy actor is available
// (defaultActor() falls back to the literal "unknown", and no agent bead / no
// seat_key is resolvable), --mine must fail loud rather than filter on
// created_by=unknown and return whichever beads some unrelated caller filed
// under that name — a wrong answer that looks like a right one, in the exact
// double-dispatch check a gardener uses to decide whether to spawn crew.
func TestMine_ErrorsWhenNeitherSeatNorActor(t *testing.T) {
	// No KD_AGENT_ID → no seat resolvable.
	t.Setenv("KD_AGENT_ID", "")
	for _, actorVal := range []string{"unknown", ""} {
		t.Run("actor="+actorVal, func(t *testing.T) {
			setActor(t, actorVal)
			stub := &laneStub{}
			newLaneStub(t, stub)

			cmd := decisionListCmd
			cmd.SetContext(context.Background())
			cmd.Flags().Set("mine", "true")
			t.Cleanup(func() { cmd.Flags().Set("mine", "false") })

			err := cmd.RunE(cmd, nil)
			if err == nil {
				t.Fatal("expected --mine to error when neither seat nor actor is resolvable, got nil")
			}
			if !strings.Contains(err.Error(), "KD_ACTOR") {
				t.Errorf("error should tell the caller how to fix it, got: %v", err)
			}
			if len(stub.listQueries) != 0 {
				t.Errorf("must not query the daemon with no ownership signal.\nqueries: %v", stub.listQueries)
			}
		})
	}
}

// TestMine_SeatAloneSufficesWhenActorUnknown proves the seat rescues a caller
// whose actor is unresolved: when the agent bead yields a seat_key but the
// actor is "unknown", --mine proceeds on the seat alone (no created_by
// fallback, since created_by=unknown is untrustworthy) rather than erroring.
func TestMine_SeatAloneSufficesWhenActorUnknown(t *testing.T) {
	t.Setenv("KD_AGENT_ID", "gd-agent-seatonly")
	setActor(t, "unknown")

	stub := &laneStub{
		agentID: "gd-agent-seatonly",
		agentBead: map[string]any{
			"id": "gd-agent-seatonly", "type": "agent", "status": "open",
			"fields": map[string]string{"seat_key": "gardenio/crew/seatonly"},
		},
	}
	newLaneStub(t, stub)

	cmd := decisionListCmd
	cmd.SetContext(context.Background())
	cmd.Flags().Set("mine", "true")
	t.Cleanup(func() { cmd.Flags().Set("mine", "false") })

	if err := cmd.RunE(cmd, nil); err != nil {
		t.Fatalf("--mine should proceed on the seat alone, got error: %v", err)
	}
	if !stub.anyQueryContains("owner_seat") {
		t.Errorf("--mine did not send the owner_seat predicate.\nqueries: %v", stub.listQueries)
	}
	if stub.anyQueryContains("created_by=unknown") {
		t.Errorf("--mine must not fall back to the untrustworthy created_by=unknown.\nqueries: %v", stub.listQueries)
	}
}
