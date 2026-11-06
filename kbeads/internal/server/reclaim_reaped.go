package server

import (
	"context"
	"encoding/json"
	"strings"

	"gardenio/kbeads/internal/model"
)

// reapedAssigneeField is beadsapi.ReapedAssigneeField: the gone agent whose
// assignee a stale-reaper or stop-gate reopen, park or half-reap clear removed
// (gardenio controller statusreporter/stalereaper.go, cmd/gb/hook_beadclose.go).
const reapedAssigneeField = "reaped_assignee"

// assigneeWasReapedAgent reports whether b is unassigned and its
// reaped_assignee names an agent with a closed bead: the state
// assigneeIsGoneAgent admitted a moment before the reap cleared the assignee.
// It keeps the controller's reclaim grant (row 5) alive across the clear, so
// the controller can claim a reaped bug/feature/chore/epic for the crew sent
// back to it (gd-8Irb2vDFf7).
//
// It does not require the name to have no live bead, as assigneeIsGoneAgent
// does: a resume crew minted under the dead crew's name is live by the time
// its task is claimed for it. A human (email), a name with no closed agent
// bead, or a lookup error is refused.
func (s *BeadsServer) assigneeWasReapedAgent(ctx context.Context, b *model.Bead) bool {
	if b.Assignee != "" || len(b.Fields) == 0 {
		return false
	}
	var fields map[string]any
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		return false
	}
	name, _ := fields[reapedAssigneeField].(string)
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "@") {
		return false
	}
	closed, err := s.store.FindAgentByName(ctx, name, []model.Status{model.StatusClosed})
	return err == nil && closed != nil
}
