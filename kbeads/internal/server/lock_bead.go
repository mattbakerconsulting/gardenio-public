package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"gardenio/kbeads/internal/model"
)

// Lock beads (gd-SlLPKtUGWZ). A bead labelled `lock` is a mutex that runs
// take by setting themselves as its assignee (`kd claim`) and release by
// clearing it (`kd unclaim`); the merge desk's pick and train locks are the
// first. Before this guard a run whose claim was refused fell back to
// `kd update --assignee` and overwrote a live holder (gd-NaCpTTc0Da,
// gd-auRiQZi7pB), so the rule lives here, on every update path:
//
//   - a run may take a free lock, retake its own, or release its own;
//   - it may not take or release a lock whose holder is a live agent;
//   - a holder with no active agent bead, or one in a gone agent_state, is
//     dead, and its lock can be taken;
//   - a signed-in human (OIDC) may clear or take a lock regardless, as the
//     break-glass for a holder the liveness check misjudges.
//
// The holder read and the write are compare-and-set: updateBead re-reads the
// assignee under a row lock inside its write transaction, so of two runs that
// both saw the lock free, the second is refused rather than clobbering.

// lockBeadLabel marks a bead as a lock.
const lockBeadLabel = "lock"

// lockHolderGoneStates are the agent_state values of an agent that is no
// longer running, whatever its bead's status says.
var lockHolderGoneStates = map[string]bool{
	"done": true, "failed": true, "killed": true, "expired": true,
	"retired": true, "resume_failed": true,
}

func isLockBead(b *model.Bead) bool {
	return b != nil && slices.Contains(b.Labels, lockBeadLabel)
}

// lockHeldError refuses a take or release of a lock another live agent holds.
type lockHeldError struct {
	id     string
	holder string
	// raced is set when the holder took the lock between this update's read
	// and its write.
	raced bool
}

func (e *lockHeldError) Error() string {
	if e.raced {
		return fmt.Sprintf("lock %s was just taken by %s: another run won the race for it. "+
			"Leave the work it guards to that run and retry on your next run.", e.id, e.holder)
	}
	return fmt.Sprintf("lock %s is held by %s, a live agent. Leave the work it guards to that run "+
		"and retry on your next run; kd update --assignee and kd claim --force are refused the same way. "+
		"A signed-in operator can clear it with kd unclaim %s.", e.id, e.holder, e.id)
}

// lockChange reports whether the patch moves the lock's holder: a lock bead
// whose assignee the patch changes.
func lockChange(bead *model.Bead, in *updateBeadInput) bool {
	return isLockBead(bead) && in.Assignee != nil && *in.Assignee != bead.Assignee
}

// lockTakeRefusal returns the refusal for this patch on bead, or nil when it
// may go ahead. bead is the stored row, read before the patch is applied.
func (s *BeadsServer) lockTakeRefusal(ctx context.Context, bead *model.Bead, in *updateBeadInput) error {
	if !lockChange(bead, in) || bead.Assignee == "" || bead.Assignee == actorFromContext(ctx) {
		return nil
	}
	if authMethodFromContext(ctx) == authMethodOIDC {
		return nil
	}
	live, err := s.lockHolderLive(ctx, bead.Assignee)
	if err != nil {
		// Fail closed: a lookup fault is not evidence the holder is gone.
		return fmt.Errorf("checking whether lock %s holder %s is live: %w", bead.ID, bead.Assignee, err)
	}
	if live {
		return &lockHeldError{id: bead.ID, holder: bead.Assignee}
	}
	return nil
}

// lockHolderLive reports whether holder names an agent that is still running:
// an agent bead in an active status whose agent_state is not a gone state.
func (s *BeadsServer) lockHolderLive(ctx context.Context, holder string) (bool, error) {
	agent, err := s.store.FindAgentByName(ctx, holder, activeAgentStatuses)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && agent == nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	state, _, _ := exactFieldString(agent.Fields, "agent_state")
	return !lockHolderGoneStates[state], nil
}
