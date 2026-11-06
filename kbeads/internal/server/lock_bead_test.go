package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Lock beads (gd-SlLPKtUGWZ): a run may not take or clear a lock another live
// agent holds, by kd claim or by the kd update fallback; a dead or absent
// holder's lock can be taken; the take is compare-and-set.

const lockID = "gd-lock1"

// deskCtx is a merge-desk run as agent pods authenticate: its own per-agent
// fleet token (agent-gd-agent-<name>). The merge desk runs as the reviewer
// role, whose all-types ownership_grants row lets it update the lock bead it
// does not own (seedLock seeds that role and desk-b; seedDeskAgent gives a run
// the role). It used to be the shared static token with X-Actor, retired in
// gd-ke6PC7bRto.
func deskCtx(name string) context.Context {
	return withAuthMethod(agentFleetCtx("gd-agent-"+name, name), authMethodStatic)
}

// seedLock adds a lock bead held by holder ("" for free).
func seedLock(ms *mockStore, holder string) {
	st := model.StatusOpen
	if holder != "" {
		st = model.StatusInProgress
	}
	ms.beads[lockID] = &model.Bead{
		ID: lockID, Kind: model.KindIssue, Type: model.TypeTask, Title: "merge-desk train lock",
		Status: st, Assignee: holder, CreatedBy: "operator@example.com",
	}
	ms.labels[lockID] = []string{lockBeadLabel, "merge-desk:lock", "project:gardenio"}
	makeBypassRoleBead(ms, "gd-role-reviewer", "reviewer")
	// desk-b is the run that tries to take a held lock in these tests; it
	// needs its agent bead for the reviewer role's bypass (deskCtx).
	seedDeskAgent(ms, "desk-b", model.StatusInProgress, "working")
}

// seedDeskAgent adds the agent bead for a desk run in the given state.
func seedDeskAgent(ms *mockStore, name string, status model.Status, agentState string) {
	f, _ := json.Marshal(map[string]any{"agent": name, "agent_state": agentState, "role": "reviewer"})
	ms.beads["gd-agent-"+name] = &model.Bead{
		ID: "gd-agent-" + name, Kind: model.KindConfig, Type: "agent", Title: name,
		Status: status, Fields: f,
	}
}

func take(name string) updateBeadInput {
	ip := string(model.StatusInProgress)
	return updateBeadInput{Assignee: &name, Status: &ip}
}

func release() updateBeadInput {
	empty, open := "", string(model.StatusOpen)
	return updateBeadInput{Assignee: &empty, Status: &open}
}

func requireLockHeld(t *testing.T, err error, holder string) {
	t.Helper()
	var held *lockHeldError
	if !errors.As(err, &held) {
		t.Fatalf("err = %v, want a *lockHeldError", err)
	}
	if held.holder != holder {
		t.Fatalf("refusal names holder %q, want %q", held.holder, holder)
	}
}

func TestLockBead_LiveHolderRefusesSecondTaker(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   updateBeadInput
	}{
		{"kd claim", take("desk-b")},
		{"kd update --assignee only", func() updateBeadInput { n := "desk-b"; return updateBeadInput{Assignee: &n} }()},
		{"release by another run", release()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedLock(ms, "desk-a")
			seedDeskAgent(ms, "desk-a", model.StatusInProgress, "working")

			_, err := s.updateBead(deskCtx("desk-b"), lockID, tc.in)
			requireLockHeld(t, err, "desk-a")
			if got := ms.beads[lockID].Assignee; got != "desk-a" {
				t.Fatalf("assignee = %q after a refused take, want desk-a", got)
			}
		})
	}
}

func TestLockBead_DeadHolderCanBeTaken(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(ms *mockStore)
	}{
		{"agent_state done", func(ms *mockStore) { seedDeskAgent(ms, "desk-a", model.StatusInProgress, "done") }},
		{"agent_state failed", func(ms *mockStore) { seedDeskAgent(ms, "desk-a", model.StatusInProgress, "failed") }},
		{"agent bead closed", func(ms *mockStore) { seedDeskAgent(ms, "desk-a", model.StatusClosed, "working") }},
		{"no agent bead", func(*mockStore) {}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedLock(ms, "desk-a")
			tc.seed(ms)

			got, err := s.updateBead(deskCtx("desk-b"), lockID, take("desk-b"))
			if err != nil {
				t.Fatalf("taking a dead holder's lock must pass: %v", err)
			}
			if got.Assignee != "desk-b" || ms.beads[lockID].Assignee != "desk-b" {
				t.Fatalf("assignee = %q (stored %q), want desk-b", got.Assignee, ms.beads[lockID].Assignee)
			}
		})
	}
}

func TestLockBead_FreeLockTakeAndOwnRelease(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLock(ms, "")
	seedDeskAgent(ms, "desk-a", model.StatusInProgress, "working")

	if _, err := s.updateBead(deskCtx("desk-a"), lockID, take("desk-a")); err != nil {
		t.Fatalf("taking a free lock: %v", err)
	}
	if _, err := s.updateBead(deskCtx("desk-a"), lockID, release()); err != nil {
		t.Fatalf("the holder releasing its own lock: %v", err)
	}
	if got := ms.beads[lockID].Assignee; got != "" {
		t.Fatalf("assignee = %q after release, want empty", got)
	}
}

// Two runs both read the lock free; the other committed first. The row-locked
// re-read in the write transaction sees it and refuses this one.
func TestLockBead_TakeLosesRaceIsRefused(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLock(ms, "")
	ms.lockBeadAssigneeHook = func(id string) {
		ms.beadsMu.Lock()
		defer ms.beadsMu.Unlock()
		won := *ms.beads[id]
		won.Assignee, won.Status = "desk-a", model.StatusInProgress
		ms.beads[id] = &won
	}

	_, err := s.updateBead(deskCtx("desk-b"), lockID, take("desk-b"))
	requireLockHeld(t, err, "desk-a")
	var held *lockHeldError
	if errors.As(err, &held) && !held.raced {
		t.Fatalf("refusal should report the lost race: %v", err)
	}
	if got := ms.beads[lockID].Assignee; got != "desk-a" {
		t.Fatalf("assignee = %q, want the race winner desk-a", got)
	}
}

func TestLockBead_OperatorMayClearLiveHolder(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLock(ms, "desk-a")
	seedDeskAgent(ms, "desk-a", model.StatusInProgress, "working")

	ctx := humanCtx("operator@example.com", []string{RoleAdmin})
	if _, err := s.updateBead(ctx, lockID, release()); err != nil {
		t.Fatalf("a signed-in operator clearing a live holder's lock: %v", err)
	}
	if got := ms.beads[lockID].Assignee; got != "" {
		t.Fatalf("assignee = %q, want empty", got)
	}
}

func TestLockBead_PlainTaskUnaffected(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLock(ms, "desk-a")
	ms.labels[lockID] = []string{"project:gardenio"}
	seedDeskAgent(ms, "desk-a", model.StatusInProgress, "working")

	if _, err := s.updateBead(deskCtx("desk-b"), lockID, take("desk-b")); err != nil {
		t.Fatalf("a bead without the lock label keeps the old behaviour: %v", err)
	}
}

func TestLockBead_HolderLookupFailureFailsClosed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedLock(ms, "desk-a")
	ms.findAgentByNameErr = errors.New("db down")

	if _, err := s.updateBead(deskCtx("desk-b"), lockID, take("desk-b")); err == nil {
		t.Fatal("a failed holder lookup must refuse the take")
	}
	if got := ms.beads[lockID].Assignee; got != "desk-a" {
		t.Fatalf("assignee = %q, want desk-a", got)
	}
}
